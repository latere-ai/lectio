// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// collector is an OTLP/HTTP receiver that counts what each signal's path
// was sent.
type collector struct {
	mu    sync.Mutex
	paths map[string]int
}

func newCollector(t *testing.T) (*collector, string) {
	t.Helper()
	c := &collector{paths: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.paths[r.URL.Path]++
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func (c *collector) got(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[path]
}

// traceOf answers the trace a GET of url carried back, which it does only
// when a span recorded the request.
func traceOf(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get("X-Trace-Id")
}

// TestTheServerExportsItsTelemetry: with an endpoint set, every request of
// the API but the probe is a span, and the stop flushes the spans and the
// request metrics to the endpoint.
func TestTheServerExportsItsTelemetry(t *testing.T) {
	col, endpoint := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	base, _, stop := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_DEV_TOKEN", "t0"))

	if id := traceOf(t, base+"/v1/parses"); id == "" {
		t.Error("GET /v1/parses carried no trace id: the API records no span")
	}
	if id := traceOf(t, base+"/healthz"); id != "" {
		t.Errorf("GET /healthz carried trace %s: the probe is recorded", id)
	}

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	for _, path := range []string{"/v1/traces", "/v1/metrics"} {
		if col.got(path) == 0 {
			t.Errorf("nothing was sent to %s; the collector got %v", path, col.paths)
		}
	}
}

// TestTheServerExportsNothingWithoutAnEndpoint: without the endpoint the API
// answers as before and carries no trace.
func TestTheServerExportsNothingWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	base, _, stop := started(t, env("LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_DEV_TOKEN", "t0"))
	if id := traceOf(t, base+"/v1/parses"); id != "" {
		t.Errorf("GET /v1/parses carried trace %s with no endpoint set", id)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
