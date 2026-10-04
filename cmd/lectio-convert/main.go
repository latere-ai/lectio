// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command lectio-convert is the conversion sidecar: a small service that
// holds an office suite and answers one call, a file in and its conversion
// out (internal/convert). It converts legacy word-processing files into the
// current format, and presentations and rich text into PDF.
//
// It runs a large program on a file somebody else wrote, so it is built to
// hold nothing worth taking: no credential, no client of any other service,
// and a suite that is started with an empty environment in a scratch
// directory of its own, under a limit on time and one on memory. What it
// cannot do for itself is go without a network. Whoever runs it gives it
// none: a container with no network and a socket on a volume it shares
// with the server, or a policy that denies it all traffic but the
// server's calls. specs/009-intake.md says why.
//
// It is configured by its environment:
//
//	LECTIO_CONVERT_ADDR          where it listens: host:port, or unix:/path/to/socket (default :8090)
//	LECTIO_CONVERT_SUITE         the suite's program (default soffice, found on PATH)
//	LECTIO_CONVERT_TIMEOUT       the time one conversion is given (default 2m)
//	LECTIO_CONVERT_MEMORY_BYTES  the address space the suite may map (default 4 GiB)
//	LECTIO_MAX_FILE_BYTES        the largest file taken and the largest conversion returned (default 256 MiB)
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
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/lectio/internal/convert"
	"latere.ai/x/lectio/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == convert.LimitArg {
		// Under this argument the program is not the sidecar. It bounds
		// its own memory and becomes the suite, and comes back only when
		// that failed.
		fmt.Fprintln(os.Stderr, "lectio-convert:", convert.Limit(os.Args[2:]))
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := serve(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lectio-convert:", err)
		os.Exit(1)
	}
}

// settings are what the environment configures.
type settings struct {
	addr     string
	suite    string
	timeout  time.Duration
	memory   uint64
	maxBytes int64
}

// fromEnv reads the settings. A value that does not parse is an error that
// names its variable and never the value.
func fromEnv(getenv func(string) string) (settings, error) {
	s := settings{addr: ":8090", suite: "soffice", timeout: 2 * time.Minute, memory: 4 << 30, maxBytes: 256 << 20}
	var errs []error
	if v := strings.TrimSpace(getenv("LECTIO_CONVERT_ADDR")); v != "" {
		s.addr = v
	}
	if v := strings.TrimSpace(getenv("LECTIO_CONVERT_SUITE")); v != "" {
		s.suite = v
	}
	if v := strings.TrimSpace(getenv("LECTIO_CONVERT_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, errors.New("LECTIO_CONVERT_TIMEOUT is not a duration above zero, such as 2m"))
		}
		s.timeout = d
	}
	if v := strings.TrimSpace(getenv("LECTIO_CONVERT_MEMORY_BYTES")); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			errs = append(errs, errors.New("LECTIO_CONVERT_MEMORY_BYTES is not a number above zero"))
		}
		s.memory = n
	}
	if v := strings.TrimSpace(getenv("LECTIO_MAX_FILE_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			errs = append(errs, errors.New("LECTIO_MAX_FILE_BYTES is not a number above zero"))
		}
		s.maxBytes = n
	}
	return s, errors.Join(errs...)
}

// serve runs the sidecar until ctx ends. out takes what the command prints
// and logs takes its log. ready, when it is not nil, is sent the address
// the sidecar listens on once it does.
func serve(ctx context.Context, args []string, getenv func(string) string, out, logs io.Writer, ready chan<- string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			_, err := fmt.Fprintf(out, "lectio-convert %s (%s, %s)\n", version.Version, version.Commit, version.Date)
			return err
		}
		return fmt.Errorf("unknown argument %q; lectio-convert is configured by its environment", args[0])
	}
	s, err := fromEnv(getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	// Both programs are found once, at start: a sidecar that cannot run
	// its suite says so before it takes a file.
	suite, err := exec.LookPath(s.suite)
	if err != nil {
		return errors.New("LECTIO_CONVERT_SUITE names no program that can be run here")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("the sidecar cannot find its own program: %w", err)
	}
	sidecar := &convert.Sidecar{Suite: suite, Self: self, Timeout: s.timeout, MemoryBytes: s.memory, MaxBytes: s.maxBytes, Log: log}

	network, address := "tcp", s.addr
	if path, ok := strings.CutPrefix(s.addr, "unix:"); ok {
		network, address = "unix", path
		// A socket left by a sidecar that was killed is in the way of
		// this one. Nothing else lives at the path a sidecar is given.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the socket's path is taken: %w", err)
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, network, address)
	if err != nil {
		return err
	}

	// A conversion outlives the listener by at most the time it is given,
	// and its suite is killed when the sidecar stops.
	work, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWork()
	srv := &http.Server{
		Handler: sidecar.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return work },
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()

	log.InfoContext(ctx, "listening", "addr", ln.Addr().String(), "suite", suite, "timeout", s.timeout.String(), "memory_bytes", s.memory, "version", version.Version)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	select {
	case err = <-failed:
	case <-ctx.Done():
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
		err = srv.Shutdown(grace)
		cancel()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	log.InfoContext(ctx, "stopped")
	return err
}
