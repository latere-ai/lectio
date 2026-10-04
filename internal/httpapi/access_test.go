// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"sync"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
)

// tokens is a test's Authenticator: a table from bearer to subject. The
// subjects name no issuer, so the cases read as alice and bob.
type tokens map[string]string

func (t tokens) Authenticate(r *http.Request) (access.Caller, error) {
	raw, ok := bearer.FromRequest(r)
	if !ok || raw == "" {
		return access.Caller{}, fault.New(fault.MissingToken, "the request carries no bearer token")
	}
	subject, ok := t[raw]
	if !ok {
		return access.Caller{}, fault.New(fault.InvalidToken, "the bearer token is not known")
	}
	return access.Caller{Subject: subject, Sub: subject, Claims: map[string]any{}}, nil
}

// callers are the callers of the cases: 2 subjects, and one the owner
// policy of the cases lets read every owner's.
var callers = tokens{"alice-token": "alice", "bob-token": "bob", "root-token": "root"}

// configured are the limits the server of the cases is configured with.
// The bound on a priority is set, so that a case may name one.
func configured() authorizer.Limits {
	return authorizer.Limits{MaxPriority: 10}
}

// ownerPolicy is what decides in a case that sets no authorizer of its
// own: the owner policy, with root as its admin.
func ownerPolicy() access.Authorizer {
	return access.NewAuthorizer(&access.OwnerPolicy{Admins: []string{"root"}}, configured())
}

// recorder is an authorizer that keeps every question it is asked and
// answers each with what a case told it to: an allow with no limits and no
// filter unless the case says otherwise.
type recorder struct {
	mu    sync.Mutex
	asked []authz.Request
	// answer decides one request. Nil allows.
	answer func(authz.Request) (authz.Decision, error)
}

func (r *recorder) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, req)
	if r.answer != nil {
		return r.answer(req)
	}
	return authz.Decision{Allow: true}, nil
}

// take returns the questions asked since the last take.
func (r *recorder) take() []authz.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.asked
	r.asked = nil
	return out
}

// allowing answers every request with an allow that carries the limits
// object raw, and the filter when it is not nil.
func allowing(raw string, filter *authz.Filter) func(authz.Request) (authz.Decision, error) {
	return func(authz.Request) (authz.Decision, error) {
		d := authz.Decision{Allow: true, Filter: filter}
		if raw != "" {
			d.Limits = []byte(raw)
		}
		return d, nil
	}
}
