// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/reader"
)

// endpoint starts the stub authorizer of the shared contract, told
// Lectio's vocabulary.
func endpoint(t *testing.T) *stub.Server {
	t.Helper()
	return stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
}

// asking builds the client lectiod runs against url, behind the
// interface a handler depends on. now is the clock its cache runs on.
func asking(t *testing.T, url, token string, now func() time.Time) access.Authorizer {
	t.Helper()
	client, err := access.NewClient(access.ClientOptions{URL: url, Token: reader.NewCredential(token), Now: now})
	if err != nil {
		t.Fatalf("the client would not build: %v", err)
	}
	return access.NewAuthorizer(client, configured())
}

// reading is the question a read of a stored parse asks.
func reading(id, owner string) access.Question {
	return access.Question{Action: authorizer.ActionParseRead, Resource: access.Parse{ID: id, Owner: owner}.Resource()}
}

// The endpoint lectiod asks answers the shared contract for every row of
// Lectio's vocabulary: the probe is denied, a wrong bearer is refused, an
// action outside the table is a 400, a token's grants narrow an allow,
// and a decision has the contract's shape.
func TestAuthorizerConformance(t *testing.T) {
	s := endpoint(t)
	conformance.Run(t, s.URL(), s.Token(),
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithPageActions(),
		conformance.WithSubjects(alice, bob))
}

// The client reaches the endpoint with the bearer it was given and reads
// its answers as decisions: the probe, which every authorizer denies, is
// the check a server runs at start.
func TestTheClientAsksTheEndpointItIsPointedAt(t *testing.T) {
	s := endpoint(t)
	client, err := access.NewClient(access.ClientOptions{URL: s.URL(), Token: reader.NewCredential(s.Token())})
	if err != nil {
		t.Fatal(err)
	}
	if client.URL() != s.URL() {
		t.Errorf("the client asks %q", client.URL())
	}
	if err := access.Check(t.Context(), client); err != nil {
		t.Fatalf("the endpoint did not deny the probe: %v", err)
	}
	// A bearer the endpoint does not know is no decision.
	wrong := asking(t, s.URL(), "another-token", nil)
	if _, err := wrong.Authorize(t.Context(), caller(alice), reading("prs_1", alice)); fault.CodeOf(err) != fault.AuthorizerUnavailable {
		t.Errorf("with a wrong bearer: %v, want authorizer_unavailable", err)
	}
}

func TestAClientNeedsAURL(t *testing.T) {
	if _, err := access.NewClient(access.ClientOptions{Token: reader.NewCredential("t")}); err == nil {
		t.Error("a client with no URL was built")
	}
}

// What the endpoint receives is the caller, the question and Lectio's own
// bearer, and what comes back is the limits and the filter of the answer.
func TestOneQuestionEndToEnd(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: alice, Action: authorizer.ActionParseCreate, Limits: map[string]any{
		"owner": "org:acme", "group": "acme", "weight": 4, "max_queued": 50, "max_pages": 100, "retention_seconds": 3600,
	}})
	az := asking(t, s.URL(), s.Token(), nil)
	c := caller(alice)
	c.Claims = map[string]any{"tenant": "acme"}
	q := access.Question{
		Action:   authorizer.ActionParseCreate,
		Resource: access.Parse{Class: "batch", Priority: 1, Labels: map[string]string{"batch": "q3"}}.Resource(),
		Request:  authz.Caller{ID: "req-1", IP: "203.0.113.4", UserAgent: "test"},
	}
	d, err := az.Authorize(t.Context(), c, q)
	if err != nil {
		t.Fatal(err)
	}
	want := configured()
	want.Owner, want.Group, want.Weight, want.MaxQueued, want.MaxPages, want.Retention = "org:acme", "acme", 4, 50, 100, time.Hour
	if !d.Allow || !reflect.DeepEqual(d.Limits, want) {
		t.Errorf("the decision is %+v, want the limits %+v", d, want)
	}

	seen := s.Requests()
	if len(seen) != 1 {
		t.Fatalf("the endpoint saw %d requests", len(seen))
	}
	got := seen[0]
	if got.Subject != alice || got.Issuer != issuer || got.Sub != "alice" || got.Claims["tenant"] != "acme" {
		t.Errorf("the endpoint saw the caller %q, %q, %q with %v", got.Subject, got.Issuer, got.Sub, got.Claims)
	}
	if got.Action != authorizer.ActionParseCreate || got.Resource.Kind != authorizer.KindParse || got.Resource.ID != "" ||
		got.Resource.String("class") != "batch" || got.Resource.Int("priority") != 1 || got.Request != q.Request {
		t.Errorf("the endpoint saw the question %+v", got)
	}
}

// The contract's cache rules, through the client lectiod runs: an allow is
// held for its ttl, a deny briefly, and an answer that is no decision
// never.
func TestWhatTheClientRemembers(t *testing.T) {
	s := endpoint(t)
	s.Allow(stub.Rule{Subject: alice, Action: authorizer.ActionParseRead, Resource: "prs_1", TTL: 120})
	s.Deny(stub.Rule{Subject: bob, Action: authorizer.ActionParseRead, Resource: "prs_1"}, "not_owner")
	clock := time.Now()
	az := asking(t, s.URL(), s.Token(), func() time.Time { return clock })
	ask := func(subject string) access.Decision {
		t.Helper()
		d, err := az.Authorize(t.Context(), caller(subject), reading("prs_1", alice))
		if err != nil {
			t.Fatalf("asking as %s: %v", subject, err)
		}
		return d
	}
	asked := func() int { return len(s.Requests()) }

	// An allow is asked once and held for the ttl its answer names.
	for range 3 {
		if !ask(alice).Allow {
			t.Fatal("the owner was denied")
		}
	}
	if asked() != 1 {
		t.Fatalf("3 asks of one allowed question reached the endpoint %d times", asked())
	}
	clock = clock.Add(119 * time.Second)
	ask(alice)
	if asked() != 1 {
		t.Errorf("the allow was asked again inside its ttl")
	}
	clock = clock.Add(2 * time.Second)
	ask(alice)
	if asked() != 2 {
		t.Errorf("the allow was not asked again past its ttl: %d requests", asked())
	}

	// A deny is held for the contract's short while and no longer.
	for range 3 {
		if d := ask(bob); d.Allow || d.Reason != "not_owner" {
			t.Fatalf("another subject got %+v", d)
		}
	}
	if asked() != 3 {
		t.Fatalf("3 asks of one denied question reached the endpoint %d times", asked()-2)
	}
	clock = clock.Add(authz.DenyTTL + time.Second)
	ask(bob)
	if asked() != 4 {
		t.Errorf("the deny was not asked again after %s", authz.DenyTTL)
	}

	// An answer that is no decision is never held: each ask is sent, and
	// the allow that follows the outage is read at once.
	s.Fail(http.StatusServiceUnavailable)
	for range 2 {
		if _, err := az.Authorize(t.Context(), caller(alice), reading("prs_2", alice)); fault.CodeOf(err) != fault.AuthorizerUnavailable {
			t.Fatalf("during the outage: %v", err)
		}
	}
	if asked() != 6 {
		t.Errorf("2 asks during an outage reached the endpoint %d times", asked()-4)
	}
	s.Resume()
	if d, err := az.Authorize(t.Context(), caller(alice), reading("prs_2", alice)); err != nil || !d.Allow {
		t.Errorf("after the outage: %+v, %v", d, err)
	}
}

// A create names no id, so its answer is never held: every submit is
// asked, and limits changed at the authorizer hold from the next one.
func TestACreateIsAlwaysAsked(t *testing.T) {
	s := endpoint(t)
	az := asking(t, s.URL(), s.Token(), nil)
	for range 3 {
		if _, err := az.Authorize(t.Context(), caller(alice), submit("")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Requests()); got != 3 {
		t.Errorf("3 submits reached the endpoint %d times", got)
	}
}

// Anything but a well-formed answer is no decision, and the request fails
// closed with authorizer_unavailable: another status, a body that is not
// JSON, a body with no allow, and no answer within the deadline.
func TestTheClientFailsClosed(t *testing.T) {
	outages := map[string]func(*stub.Server){
		"a 500":                func(s *stub.Server) { s.Fail(http.StatusInternalServerError) },
		"a 403":                func(s *stub.Server) { s.Fail(http.StatusForbidden) },
		"a body not JSON":      func(s *stub.Server) { s.FailBody(stub.BodyMalformed) },
		"a body with no allow": func(s *stub.Server) { s.FailBody(stub.BodyNoAllow) },
		"no answer":            func(s *stub.Server) { s.Hang() },
	}
	for name, outage := range outages {
		t.Run(name, func(t *testing.T) {
			s := endpoint(t)
			az := asking(t, s.URL(), s.Token(), nil)
			outage(s)
			// The request's own deadline bounds the wait on an endpoint
			// that never answers.
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			d, err := az.Authorize(ctx, caller(alice), reading("prs_1", alice))
			if fault.CodeOf(err) != fault.AuthorizerUnavailable || d.Allow {
				t.Fatalf("got %+v, %v, want authorizer_unavailable", d, err)
			}
			// What a caller reads names neither the endpoint nor its bearer.
			if detail := fault.DetailOf(err); strings.Contains(detail, s.URL()) || strings.Contains(err.Error(), s.Token()) {
				t.Errorf("the refusal says too much: %q, %v", detail, err)
			}
		})
	}
}

// A call whose connection failed before any answer is tried once more, so
// a connection the endpoint closed between two requests costs no request
// its decision.
func TestOneRetryOnAConnectionFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Close the connection with no response line.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			if err := conn.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"allow": true}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	az := asking(t, srv.URL, "t", nil)
	d, err := az.Authorize(t.Context(), caller(alice), reading("prs_1", alice))
	if err != nil || !d.Allow {
		t.Fatalf("got %+v, %v, want the allow of the second attempt", d, err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("the endpoint was called %d times, want 2", got)
	}

	// A second failure is not tried a third time.
	calls.Store(0)
	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer always.Close()
	az = asking(t, always.URL, "t", nil)
	if _, err := az.Authorize(t.Context(), caller(alice), reading("prs_1", alice)); fault.CodeOf(err) != fault.AuthorizerUnavailable {
		t.Fatalf("got %v, want authorizer_unavailable", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("an endpoint that always fails was called %d times, want 2", got)
	}
}

// The token is a credential: the client's options print a placeholder.
func TestTheClientsOptionsDoNotPrintTheToken(t *testing.T) {
	o := access.ClientOptions{URL: "https://plane.example/authz", Token: reader.NewCredential("az-secret-token")}
	for _, shown := range []string{fmt.Sprint(o), fmt.Sprintf("%+v", o), fmt.Sprintf("%#v", o)} {
		if strings.Contains(shown, "az-secret-token") {
			t.Errorf("the options print the token: %s", shown)
		}
	}
}
