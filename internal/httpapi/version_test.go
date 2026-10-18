// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"testing"

	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/version"
)

// TestEveryAnswerNamesTheRelease is the first criterion of
// specs/020-release-version-header.md: a routed answer, every kind of
// refusal the handler writes, the contract and the health check each carry
// the release of the server that wrote them.
func TestEveryAnswerNamesTheRelease(t *testing.T) {
	e := serve(t, func(s *Server, _ *run.Runner) { s.Version = "v9.8.7" })
	for name, c := range map[string]struct {
		e            *env
		method, path string
		body         any
		status       int
	}{
		"a routed answer":           {e, "GET", "/readers", nil, http.StatusOK},
		"a caller with no token":    {e.as(""), "GET", "/readers", nil, http.StatusUnauthorized},
		"a request it cannot take":  {e, "POST", "/parses", `{"source":{}}`, http.StatusBadRequest},
		"an object that is missing": {e, "GET", "/parses/prs_00000000000000000000000000", nil, http.StatusNotFound},
		"an address not routed":     {e, "GET", "/nothing-here", nil, http.StatusNotFound},
		"a method not routed":       {e, "DELETE", "/readers", nil, http.StatusMethodNotAllowed},
		"the contract":              {e.as(""), "GET", "/openapi.yaml", nil, http.StatusOK},
	} {
		r := c.e.do(c.method, c.path, c.body)
		if r.status != c.status {
			t.Errorf("%s: %s %s answered %d, want %d: %s", name, c.method, c.path, r.status, c.status, r.body)
		}
		if got := r.header.Get(HeaderVersion); got != "v9.8.7" {
			t.Errorf("%s: %s %s answered %s %q", name, c.method, c.path, HeaderVersion, got)
		}
	}

	// The health check sits outside the base path.
	resp, err := http.Get(e.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get(HeaderVersion); resp.StatusCode != http.StatusOK || got != "v9.8.7" {
		t.Errorf("GET /healthz answered %d with %s %q", resp.StatusCode, HeaderVersion, got)
	}
}

// TestAServerWithNoVersionNamesTheStampedOne is the second criterion: the
// value is the linker's unless a caller of the package sets another, so the
// header and the internal /version agree.
func TestAServerWithNoVersionNamesTheStampedOne(t *testing.T) {
	e := serve(t, nil)
	if got := e.do("GET", "/readers", nil).header.Get(HeaderVersion); got != version.Version {
		t.Errorf("a server with no Version answered %s %q, want %q", HeaderVersion, got, version.Version)
	}
}
