// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/otel"
	"latere.ai/x/pkg/retry"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/durable"
	"latere.ai/x/lectio/internal/fetch"
	"latere.ai/x/lectio/internal/httpapi"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/version"
	"latere.ai/x/lectio/internal/worker"
	"latere.ai/x/lectio/reader"
)

// probeTimeout bounds one readiness check.
const probeTimeout = 2 * time.Second

// wrapReaders, when a test of this package sets it, replaces the configured
// readers before the durable server starts, so a process the test runs
// reads pages with a reader that waits, or blocks, as the test needs. It is
// nil in a build.
var wrapReaders func(config.Readers) config.Readers

// serveDurable runs the durable server in the role LECTIO_ROLE names, until
// ctx ends. The API role applies the schema over the direct connection,
// serves the contract from the task store and the object store, and holds
// no parse in memory. The worker role runs the tasks and serves no public
// route. Both serve their probes on the internal listener. ready, when it
// is not nil, is sent the address of the API, or of the probes for a
// process that serves no API.
func serveDurable(ctx context.Context, s config.Settings, readers config.Readers, pipeline *parse.Pipeline, log *slog.Logger, ready chan<- string) error {
	serves, works := s.Role != config.RoleWorker, s.Role != config.RoleAPI
	if wrapReaders != nil {
		readers = wrapReaders(readers)
	}
	for _, what := range readers.Unapplied {
		log.WarnContext(ctx, "the configuration sets what this build does not apply", "setting", what)
	}
	// Who is calling is the API's question. A worker acts on tasks and for
	// no caller, verifies no token and asks nobody.
	var who *access.Access
	if serves {
		var err error
		if who, err = identity(ctx, s, log); err != nil {
			return err
		}
	}

	objects, err := blob.NewS3(blob.S3Config{
		Endpoint: s.S3Endpoint, Region: s.S3Region, Bucket: s.Bucket, Prefix: s.BucketPrefix,
		AccessKey: s.S3AccessKey, SecretKey: s.S3SecretKey.Reveal(), PathStyle: s.S3PathStyle,
		HTTPClient: &http.Client{Transport: otel.Transport(nil)},
	})
	if err != nil {
		return err
	}

	// The schema is the API's to apply, over the direct connection: the
	// migrator holds a session lock while it runs, so of several replicas
	// that start at once one applies it and the others find it applied.
	if serves {
		if err := postgres.Migrate(ctx, s.DatabaseURL); err != nil {
			return err
		}
	}
	open := func(ctx context.Context) (*postgres.Store, error) {
		return postgres.Open(ctx, s.ServingURL(), postgres.Options{Settings: s.Queue(readers)})
	}
	st, err := open(ctx)
	if err != nil && !serves {
		// A worker that starts before the API of a new version has applied
		// the schema waits for it and does not give up at once.
		log.WarnContext(ctx, "the task store did not open; the worker waits for the schema", "error", err)
		err = retry.Do(ctx, retry.Policy{MaxAttempts: 60, Base: 250 * time.Millisecond, Max: 2 * time.Second}, func(ctx context.Context) error {
			st, err = open(ctx)
			return err
		})
	}
	if err != nil {
		return err
	}
	defer st.Close()

	checks := []health.Check{{Name: "database", Run: st.Ping}}
	var w *worker.Worker
	if works {
		costs := map[string]int{}
		for _, p := range readers.Pools {
			costs[p.Reader] = p.Cost
		}
		w = &worker.Worker{
			Store: st, Objects: objects, Pipeline: pipeline, Readers: readers.Readers, Costs: costs,
			Credential: func(string, string, string) reader.Credential { return s.ModelKey },
			Slots:      s.Workers, Lease: s.Lease, Flush: s.Flush, Poll: s.Poll, Grace: s.Grace,
			CacheBytes: s.CacheBytes, Retention: st, Sweep: s.SweepInterval, Log: log,
		}
		checks = append(checks, health.Check{Name: "worker", Run: func(context.Context) error { return w.Ready() }})
	}

	internal, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.InternalAddr)
	if err != nil {
		return fmt.Errorf("LECTIO_INTERNAL_ADDR: %w", err)
	}
	probes := &http.Server{
		Handler: health.Handler(health.Options{
			Ready: health.Checks(checks...), Timeout: probeTimeout,
			Version: version.Version, Commit: version.Commit, BuildTime: version.Date,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	failed := make(chan error, 3)
	go func() { failed <- probes.Serve(internal) }()
	addr := internal.Addr().String()

	var api *http.Server
	if serves {
		limits := pages.Limits{MaxBytes: s.MaxFileBytes, MaxPages: s.MaxPages}
		handlers := &httpapi.Server{
			Backend: &durable.Backend{
				Store: st, Objects: objects, Readers: readers.Readers, Chain: readers.Chain,
				MaxDeadline: s.MaxDeadline, Log: log,
			},
			Auth: who.Authenticator, Authz: who.Authorizer,
			Readers: readers.Readers, Chain: readers.Chain, Limits: limits,
			Fetcher:       &fetch.Fetcher{MaxBytes: s.MaxFileBytes, Allow: s.FetchAllow},
			FileRetention: s.FileRetention,
			BasePath:      s.BasePath, MaxDeadline: s.MaxDeadline, Log: log,
		}
		if err := checked(ctx, who, log); err != nil {
			return errors.Join(err, probes.Close())
		}
		public, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
		if err != nil {
			return errors.Join(err, probes.Close())
		}
		api = &http.Server{Handler: handlers.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { failed <- api.Serve(public) }()
		addr = public.Addr().String()
	}
	// worked is nil for a process that runs no task, so nothing is ever
	// received from it.
	var worked chan error
	if works {
		worked = make(chan error, 1)
		go func() { worked <- w.Run(ctx) }()
	}

	log.InfoContext(ctx, "listening", "role", s.Role, "addr", addr, "internal", internal.Addr().String(),
		"base_path", s.BasePath, "readers", readers.Chain, "version", version.Version)
	if ready != nil {
		ready <- addr
	}

	select {
	case err = <-failed:
	case err = <-worked:
		// The worker stopped by itself: its last exchange failed.
	case <-ctx.Done():
	}
	// The API stops accepting and lets open requests finish, and the worker
	// lets its tasks finish, each within the grace period; the probes
	// answer until both are done.
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.Grace+5*time.Second)
	defer cancel()
	if api != nil {
		err = errors.Join(err, api.Shutdown(grace))
	}
	if works && ctx.Err() != nil {
		err = errors.Join(err, <-worked)
	}
	err = errors.Join(err, probes.Shutdown(grace))
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	log.InfoContext(ctx, "stopped", "role", s.Role)
	return err
}
