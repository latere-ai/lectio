// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/convert"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
)

// TestMain lets the test binary stand in for both programs the sidecar
// starts: its own under the limit argument, which is the command's real
// main, and the office suite, which here writes a fixed conversion.
func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == convert.LimitArg:
		main()
	case slices.Contains(os.Args, "--convert-to"):
		outdir := os.Args[slices.Index(os.Args, "--outdir")+1]
		if err := os.WriteFile(filepath.Join(outdir, "input.pdf"), []byte("%PDF-1.7 from the suite"), 0o600); err != nil {
			os.Exit(70)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func env(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(name string) string { return m[name] }
}

// buffer is a log the test reads while the sidecar writes it.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// started runs the sidecar with the test binary as its suite and returns
// the address it listens on, its log, and a stop that ends it.
func started(t *testing.T, pairs ...string) (addr string, logs *buffer, stop func() error) {
	t.Helper()
	logs = &buffer{}
	// A limit no address space reaches: the stand-in for the suite is
	// built with the race detector, which maps far more than a suite.
	pairs = append([]string{"LECTIO_CONVERT_SUITE", os.Args[0], "LECTIO_CONVERT_MEMORY_BYTES", "4611686018427387904"}, pairs...)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() { done <- serve(ctx, nil, env(pairs...), io.Discard, logs, ready) }()
	select {
	case addr = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("the sidecar did not start: %v", err)
	}
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() { cancel(); err = <-done })
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return addr, logs, stop
}

func converter(t *testing.T, url string) *convert.Client {
	t.Helper()
	c, err := convert.New(convert.Config{URL: url, MaxBytes: 1 << 20, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheSidecarConvertsOverTheNetwork(t *testing.T) {
	addr, logs, stop := started(t, "LECTIO_CONVERT_ADDR", "127.0.0.1:0", "LECTIO_CONVERT_TIMEOUT", "30s", "LECTIO_MAX_FILE_BYTES", "64")
	c := converter(t, "http://"+addr)
	out, err := c.Convert(context.Background(), []byte("a deck"), detect.MIMEPPTX, detect.MIMEPDF)
	if err != nil || string(out) != "%PDF-1.7 from the suite" {
		t.Fatalf("Convert = %q, %v", out, err)
	}
	if _, err := c.Convert(context.Background(), bytes.Repeat([]byte("x"), 65), detect.MIMEPPTX, detect.MIMEPDF); fault.CodeOf(err) != fault.FileTooLarge {
		t.Fatalf("a file over LECTIO_MAX_FILE_BYTES: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("a stop that was asked for is not a failure: %v", err)
	}
	log := logs.String()
	for _, want := range []string{`"msg":"listening"`, `"timeout":"30s"`, `"msg":"converted"`, `"msg":"stopped"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not say %s:\n%s", want, log)
		}
	}
}

// A sidecar with no network listens on a socket, and takes the place of a
// socket an earlier one left behind.
func TestTheSidecarConvertsOverASocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "lc")
	if err == nil && len(dir) > 80 {
		// A socket's path is bounded at about 100 bytes.
		if err = os.RemoveAll(dir); err == nil {
			dir, err = os.MkdirTemp("/tmp", "lc")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(dir, "convert.sock")
	if err := os.WriteFile(socket, []byte("left behind"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _ = started(t, "LECTIO_CONVERT_ADDR", "unix:"+socket)
	out, err := converter(t, "unix://"+socket).Convert(context.Background(), []byte("a deck"), detect.MIMEODP, detect.MIMEPDF)
	if err != nil || string(out) != "%PDF-1.7 from the suite" {
		t.Fatalf("Convert = %q, %v", out, err)
	}

	// A path that cannot be cleared is not listened on.
	if err := os.MkdirAll(filepath.Join(dir, "taken", "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = serve(context.Background(), nil, env("LECTIO_CONVERT_SUITE", os.Args[0], "LECTIO_CONVERT_ADDR", "unix:"+filepath.Join(dir, "taken")), io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("a socket path that is a directory: %v", err)
	}
}

func TestTheSidecarDoesNotStartOnWhatItCannotRun(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	for name, tc := range map[string]struct {
		args   []string
		getenv func(string) string
		want   string
	}{
		"a suite that is not there": {nil, env("LECTIO_CONVERT_SUITE", "/nonexistent/sk-secret-suite"), "LECTIO_CONVERT_SUITE"},
		"an address that is taken":  {nil, env("LECTIO_CONVERT_SUITE", os.Args[0], "LECTIO_CONVERT_ADDR", taken.Addr().String()), "address already in use"},
		"an argument":               {[]string{"serve"}, env(), `unknown argument "serve"`},
		"settings that do not parse": {nil, env(
			"LECTIO_CONVERT_TIMEOUT", "sk-secret-1", "LECTIO_CONVERT_MEMORY_BYTES", "sk-secret-2", "LECTIO_MAX_FILE_BYTES", "0",
		), "LECTIO_CONVERT_TIMEOUT"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := serve(ctx, tc.args, tc.getenv, io.Discard, io.Discard, nil)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "sk-secret") {
			t.Errorf("%s: the error repeats a value: %v", name, err)
		}
	}

	_, err = fromEnv(env("LECTIO_CONVERT_TIMEOUT", "0s", "LECTIO_CONVERT_MEMORY_BYTES", "0", "LECTIO_MAX_FILE_BYTES", "many"))
	for _, name := range []string{"LECTIO_CONVERT_TIMEOUT", "LECTIO_CONVERT_MEMORY_BYTES", "LECTIO_MAX_FILE_BYTES"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	s, err := fromEnv(env())
	if err != nil || s.addr != ":8090" || s.suite != "soffice" || s.timeout != 2*time.Minute || s.memory != 4<<30 || s.maxBytes != 256<<20 {
		t.Fatalf("defaults: %+v, %v", s, err)
	}

	var out bytes.Buffer
	if err := serve(context.Background(), []string{"--version"}, env(), &out, io.Discard, nil); err != nil || !strings.HasPrefix(out.String(), "lectio-convert ") {
		t.Fatalf("version: %q, %v", out.String(), err)
	}
}

func TestMainRunsTheCommand(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"lectio-convert", "version"}
	main()
}
