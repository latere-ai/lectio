// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command lectiod serves the Lectio API and runs its parses.
//
// It runs in two modes. With LECTIO_DEV=true it is one process with files
// and results in memory, pages read by an in-process runner, and one token:
// the whole parsing path and the whole API with nothing that survives a
// restart, for a laptop and for tests. Without it, it is the durable server
// of specs/004-durable-tasks.md and specs/016-distribution.md: its work is
// rows in Postgres, its bytes are in an object store, and LECTIO_ROLE says
// whether the process serves the API, runs the tasks, or does both.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/convert"
	"latere.ai/x/lectio/internal/fetch"
	"latere.ai/x/lectio/internal/httpapi"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/version"
	"latere.ai/x/lectio/reader"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := serve(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lectiod:", err)
		os.Exit(1)
	}
}

// serve runs the server until ctx ends. out takes what the command prints
// and logs takes its log. ready, when it is not nil, is sent the address
// the server listens on once it does.
func serve(ctx context.Context, args []string, getenv func(string) string, out, logs io.Writer, ready chan<- string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			_, err := fmt.Fprintln(out, version.String())
			return err
		}
		return fmt.Errorf("unknown argument %q; lectiod is configured by its environment", args[0])
	}

	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	if !s.Dev && s.DatabaseURL == "" {
		return errors.New("LECTIO_DATABASE_URL is not set. The durable server keeps its work in a database; LECTIO_DEV=true runs without one and keeps nothing")
	}

	readers := config.Stub()
	if s.ConfigPath != "" {
		if readers, err = config.Load(s.ConfigPath); err != nil {
			return err
		}
	} else {
		log.WarnContext(ctx, "LECTIO_CONFIG is not set: pages are read by the stub reader, which calls no model and describes the page it was given")
	}

	limits := pages.Limits{MaxBytes: s.MaxFileBytes, MaxPages: s.MaxPages}
	pipeline := &parse.Pipeline{Limits: limits, Renderer: render.NewPages()}
	if s.ConverterURL != "" {
		// A conversion is what the pages are then read from, so it is held
		// to the size a file is.
		converter, err := convert.New(convert.Config{URL: s.ConverterURL, MaxBytes: s.MaxFileBytes, Trace: otel.Transport})
		if err != nil {
			return fmt.Errorf("LECTIO_CONVERTER_URL: %w", err)
		}
		pipeline.Converter = converter
	} else {
		log.InfoContext(ctx, "LECTIO_CONVERTER_URL is not set: a format that needs conversion is refused")
	}
	if !s.Dev {
		return serveDurable(ctx, s, readers, pipeline, log, ready)
	}
	who, err := identity(ctx, s, log)
	if err != nil {
		return err
	}
	for _, what := range append(readers.RunnerUnapplied, readers.Unapplied...) {
		log.WarnContext(ctx, "the configuration sets what this build does not apply", "setting", what)
	}
	log.WarnContext(ctx, "LECTIO_DEV: files, parses and results are kept in memory and are lost when the process stops")
	st := store.NewMemory()
	runner := &run.Runner{
		Store: st, Pipeline: pipeline,
		Readers: readers.Readers, Chain: readers.Chain, Workers: s.Workers, Attempts: s.Attempts,
		Describers: readers.Describers, DescribeChain: readers.DescribeChain,
		Credential: func(string) reader.Credential { return s.ModelKey },
	}
	api := &httpapi.Server{
		Backend: &httpapi.Memory{Store: st, Runner: runner}, Auth: who.Authenticator, Authz: who.Authorizer,
		Readers: readers.Readers, Chain: readers.Chain, Limits: limits,
		Fetcher:  &fetch.Fetcher{MaxBytes: s.MaxFileBytes, Allow: s.FetchAllow},
		BasePath: s.BasePath, MaxDeadline: s.MaxDeadline, Log: log,
	}

	if err := checked(ctx, who, log); err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	// The runner outlives the listener: a request that is being answered
	// may still be waiting on a parse.
	work, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	runner.Start(work)
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()

	log.InfoContext(ctx, "listening", "addr", ln.Addr().String(), "base_path", s.BasePath, "readers", readers.Chain, "version", version.Version)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	select {
	case err = <-failed:
	case <-ctx.Done():
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.Grace)
		err = srv.Shutdown(grace)
		cancel()
	}
	stopWork()
	runner.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	log.InfoContext(ctx, "stopped")
	return err
}

// startCheck bounds what a server asks of its issuers and its authorizer
// before it listens.
const startCheck = 15 * time.Second

// identity builds who is calling and who decides from the settings, and
// logs which of the modes is in force. A server that is not a development
// one and lists no issuer is refused here, before anything is opened.
func identity(ctx context.Context, s config.Settings, log *slog.Logger) (*access.Access, error) {
	who, err := access.New(s)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "identity", "callers", who.Identity, "decisions", who.Authorization)
	if who.Identity == access.IdentityDevelopment {
		log.WarnContext(ctx, "no issuer is configured: the one caller is the holder of LECTIO_DEV_TOKEN")
	}
	return who, nil
}

// checked asks the issuers and the authorizer what a server wants known
// before it listens. An issuer that does not answer is named and the
// server starts: its keys are read when its first token arrives. An
// authorizer that does not answer is named and the server starts, since
// every request then fails closed. One that allows the probe does not read
// what it is asked, and the server is refused.
func checked(ctx context.Context, who *access.Access, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, startCheck)
	defer cancel()
	if err := who.Warm(ctx); err != nil {
		log.WarnContext(ctx, "an issuer's keys could not be read at start; they are read when its first token arrives", "error", err)
	}
	if who.Authorization != access.AuthorizationEndpoint {
		return nil
	}
	switch err := who.Check(ctx); {
	case errors.Is(err, authz.ErrProbeAllowed):
		return errors.New("LECTIO_AUTHORIZER_URL names an endpoint that allowed the probe every authorizer denies: it does not read what it is asked")
	case err != nil:
		log.WarnContext(ctx, "the authorizer did not answer the probe at start; requests fail closed until it does", "error", err)
	}
	return nil
}
