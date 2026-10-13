// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/run"
)

// admin is a ReaderAdmin in memory over the stub configuration: what it
// stores is built and put in force at once, as the durable server's is.
type admin struct {
	mu     sync.Mutex
	file   config.File
	live   *config.Live
	added  []config.Added
	policy []byte
}

func (a *admin) put(added []config.Added, policy []byte) error {
	set, err := a.file.Build(added, policy)
	if err != nil {
		return fault.New(fault.InvalidRequest, "%v", err)
	}
	a.added, a.policy = added, policy
	a.live.Set(set)
	return nil
}

func (a *admin) PutReader(_ context.Context, name string, spec []byte) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.file.CheckReader(name, spec); err != nil {
		return false, fault.New(fault.InvalidRequest, "%v", err)
	}
	created := !slices.ContainsFunc(a.added, func(x config.Added) bool { return x.Name == name })
	next := slices.DeleteFunc(slices.Clone(a.added), func(x config.Added) bool { return x.Name == name })
	return created, a.put(append(next, config.Added{Name: name, Spec: spec}), a.policy)
}

func (a *admin) DeleteReader(_ context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.ContainsFunc(a.added, func(x config.Added) bool { return x.Name == name }) {
		return fault.New(fault.NotFound, "no stored reader is named %s", name)
	}
	return a.put(slices.DeleteFunc(slices.Clone(a.added), func(x config.Added) bool { return x.Name == name }), a.policy)
}

func (a *admin) PutPolicy(_ context.Context, spec []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.file.CheckPolicy(a.added, spec); err != nil {
		return fault.New(fault.InvalidRequest, "%v", err)
	}
	return a.put(a.added, spec)
}

func (a *admin) DeletePolicy(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.put(a.added, nil)
}

func (a *admin) Check(context.Context) error { return nil }

// TestReadersAreChangedThroughTheAPIAsTheContractSays: an admin stores a
// reader, reads it, sets the routing policy, puts the configured one back
// and removes the reader, each answered as the contract says; a caller who
// is no admin changes neither, and a server that stores no reader says it
// does not.
func TestReadersAreChangedThroughTheAPIAsTheContractSays(t *testing.T) {
	e := serve(t, func(s *Server, _ *run.Runner) {
		set, err := config.StubFile().Build(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		live := config.NewLive(set)
		s.Live, s.Manage = live, &admin{file: config.StubFile(), live: live}
	})
	root := e.as("root-token")
	if got := root.do("PUT", "/readers/own", `{"adapter":"text"}`); got.status != http.StatusCreated || got.json(t)["source"] != "added" {
		t.Fatalf("storing a reader: %d %s", got.status, got.body)
	}
	if got := root.do("PUT", "/readers/own", `{"adapter":"text"}`); got.status != http.StatusOK {
		t.Fatalf("replacing a reader: %d %s", got.status, got.body)
	}
	for _, bad := range []string{`[]`, `{"adapter":"text"} {}`, `{"adapter":"stub"}`} {
		if got := root.do("PUT", "/readers/own", bad); got.status != http.StatusBadRequest || got.code(t) != "invalid_request" {
			t.Fatalf("storing %s: %d %s", bad, got.status, got.body)
		}
	}
	if got := e.do("GET", "/readers/own", nil); got.status != http.StatusOK || got.json(t)["spec"] == nil || got.json(t)["adapter"] != "text" {
		t.Fatalf("reading the stored reader: %d %s", got.status, got.body)
	}
	if got := e.do("GET", "/readers/gone", nil); got.status != http.StatusNotFound || got.code(t) != "not_found" {
		t.Fatalf("reading a reader that is not there: %d %s", got.status, got.body)
	}
	policy := `{"read":{"chain":["own","stub"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`
	for _, rt := range []struct{ method, path, body string }{{"PUT", "/policy", policy}, {"DELETE", "/policy", ""}, {"PUT", "/readers/other", `{"adapter":"text"}`}, {"DELETE", "/readers/own", ""}} {
		if got := e.do(rt.method, rt.path, rt.body); got.status != http.StatusForbidden {
			t.Fatalf("%s %s as a caller who is no admin: %d %s", rt.method, rt.path, got.status, got.body)
		}
	}
	if got := root.do("PUT", "/policy", policy); got.status != http.StatusOK || got.json(t)["source"] != "set" {
		t.Fatalf("setting the policy: %d %s", got.status, got.body)
	}
	if got := root.do("PUT", "/policy", `{"read":{"chain":["gone"]}}`); got.status != http.StatusBadRequest {
		t.Fatalf("a policy naming no reader: %d %s", got.status, got.body)
	}
	if got := e.do("GET", "/policy", nil); got.status != http.StatusOK || got.json(t)["source"] != "set" {
		t.Fatalf("reading the policy: %d %s", got.status, got.body)
	}
	if got := root.do("DELETE", "/policy", nil); got.status != http.StatusOK || got.json(t)["source"] != "configured" {
		t.Fatalf("putting the configured policy back: %d %s", got.status, got.body)
	}
	if got := root.do("DELETE", "/readers/own", nil); got.status != http.StatusNoContent {
		t.Fatalf("removing the reader: %d %s", got.status, got.body)
	}
	if got := root.do("DELETE", "/readers/own", nil); got.status != http.StatusNotFound {
		t.Fatalf("removing it again: %d %s", got.status, got.body)
	}

	// A server that stores no reader.
	plain := serve(t, nil).as("root-token")
	for _, rt := range []struct{ method, path, body string }{{"PUT", "/policy", policy}, {"DELETE", "/policy", ""}, {"PUT", "/readers/own", `{"adapter":"text"}`}, {"DELETE", "/readers/own", ""}} {
		if got := plain.do(rt.method, rt.path, rt.body); got.status != http.StatusNotImplemented || got.code(t) != "not_implemented" {
			t.Fatalf("%s %s on a server that stores no reader: %d %s", rt.method, rt.path, got.status, got.body)
		}
	}
}
