// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/reader"
)

// policy is the owner policy with one admin.
func policy() *access.OwnerPolicy { return &access.OwnerPolicy{Admins: []string{root}} }

// request is the envelope of one question as a subject.
func request(subject, action string, res authz.Resource) authz.Request {
	req := authz.Request{Subject: subject, Action: action, Resource: res, Claims: map[string]any{}}
	req.Issuer, req.Sub, _ = authz.SplitSubject(subject)
	return req
}

// mine is the filter that narrows a read to one subject's own.
func mine(subject string) *authz.Filter { return &authz.Filter{Owners: []string{subject}} }

// The rows of the owner policy, one question each. alice asks unless the
// row says otherwise, and root is the admin.
func TestTheOwnerPolicy(t *testing.T) {
	parseOf := func(owner string) authz.Resource { return access.Parse{ID: "prs_1", Owner: owner}.Resource() }
	fileOf := func(owner string) authz.Resource { return access.File{ID: "fil_1", Owner: owner, Size: 10}.Resource() }
	cases := []struct {
		name, subject, action string
		res                   authz.Resource
		allow                 bool
		reason                string
		filter                *authz.Filter
	}{
		// A subject and what it owns.
		{"reads its own parse", alice, authorizer.ActionParseRead, parseOf(alice), true, "", nil},
		{"cancels its own parse", alice, authorizer.ActionParseCancel, parseOf(alice), true, "", nil},
		{"deletes its own parse", alice, authorizer.ActionParseDelete, parseOf(alice), true, "", nil},
		{"retries its own parse", alice, authorizer.ActionParseCreate, parseOf(alice), true, "", nil},
		{"reads its own file", alice, authorizer.ActionFileRead, fileOf(alice), true, "", nil},
		{"deletes its own file", alice, authorizer.ActionFileDelete, fileOf(alice), true, "", nil},

		// A subject and what another owns.
		{"reads another's parse", alice, authorizer.ActionParseRead, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"cancels another's parse", alice, authorizer.ActionParseCancel, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"deletes another's parse", alice, authorizer.ActionParseDelete, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"retries another's parse", alice, authorizer.ActionParseCreate, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"reads another's file", alice, authorizer.ActionFileRead, fileOf(bob), false, authz.ReasonNotOwner, nil},
		{"deletes another's file", alice, authorizer.ActionFileDelete, fileOf(bob), false, authz.ReasonNotOwner, nil},

		// A create: in the caller's own name, or in none.
		{"submits in its context", alice, authorizer.ActionParseCreate, access.Parse{}.Resource(), true, "", nil},
		{"submits in its own name", alice, authorizer.ActionParseCreate, access.Parse{Owner: alice}.Resource(), true, "", nil},
		{"submits in another's name", alice, authorizer.ActionParseCreate, access.Parse{Owner: bob}.Resource(), false, authz.ReasonNotOwner, nil},
		{"uploads in its context", alice, authorizer.ActionFileCreate, access.File{}.Resource(), true, "", nil},
		{"uploads in another's name", alice, authorizer.ActionFileCreate, access.File{Owner: bob}.Resource(), false, authz.ReasonNotOwner, nil},

		// An object with an id and no owner is one that does not exist.
		{"reads a parse nobody owns", alice, authorizer.ActionParseRead, parseOf(""), false, authz.ReasonNotOwner, nil},
		{"retries a parse nobody owns", alice, authorizer.ActionParseCreate, parseOf(""), false, authz.ReasonNotOwner, nil},
		{"deletes a file nobody owns", alice, authorizer.ActionFileDelete, fileOf(""), false, authz.ReasonNotOwner, nil},

		// Reads that range over owners are narrowed to the subject's own.
		{"lists parses", alice, authorizer.ActionParseList, access.Parses(), true, "", mine(alice)},
		{"reads usage, naming nothing", alice, authorizer.ActionUsageRead, access.Usage("", ""), true, "", mine(alice)},
		{"reads its own usage", alice, authorizer.ActionUsageRead, access.Usage(alice, ""), true, "", nil},
		{"reads its own group's usage", alice, authorizer.ActionUsageRead, access.Usage(alice, alice), true, "", nil},
		{"reads another's usage", alice, authorizer.ActionUsageRead, access.Usage(bob, ""), false, authz.ReasonNotOwner, nil},
		{"reads another group's usage", alice, authorizer.ActionUsageRead, access.Usage("", bob), false, authz.ReasonNotOwner, nil},
		{"reads the queue, naming nothing", alice, authorizer.ActionQueueRead, access.Queue(""), true, "", mine(alice)},
		{"reads its own group's queue", alice, authorizer.ActionQueueRead, access.Queue(alice), true, "", nil},
		{"reads another group's queue", alice, authorizer.ActionQueueRead, access.Queue(bob), false, authz.ReasonNotOwner, nil},
		{"lists the readers", alice, authorizer.ActionReaderList, access.Readers(), true, "", nil},

		// An admin reads every owner's and changes nothing but its own.
		{"an admin reads another's parse", root, authorizer.ActionParseRead, parseOf(bob), true, "", nil},
		{"an admin reads another's file", root, authorizer.ActionFileRead, fileOf(bob), true, "", nil},
		{"an admin lists every parse", root, authorizer.ActionParseList, access.Parses(), true, "", nil},
		{"an admin reads all usage", root, authorizer.ActionUsageRead, access.Usage("", ""), true, "", nil},
		{"an admin reads another's usage", root, authorizer.ActionUsageRead, access.Usage(bob, ""), true, "", nil},
		{"an admin reads the whole queue", root, authorizer.ActionQueueRead, access.Queue(""), true, "", nil},
		{"an admin reads another group's queue", root, authorizer.ActionQueueRead, access.Queue(bob), true, "", nil},
		{"an admin cancels another's parse", root, authorizer.ActionParseCancel, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"an admin deletes another's parse", root, authorizer.ActionParseDelete, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"an admin retries another's parse", root, authorizer.ActionParseCreate, parseOf(bob), false, authz.ReasonNotOwner, nil},
		{"an admin deletes another's file", root, authorizer.ActionFileDelete, fileOf(bob), false, authz.ReasonNotOwner, nil},
		{"an admin submits in another's name", root, authorizer.ActionParseCreate, access.Parse{Owner: bob}.Resource(), false, authz.ReasonNotOwner, nil},
		{"an admin cancels its own parse", root, authorizer.ActionParseCancel, parseOf(root), true, "", nil},

		// No subject, and questions the vocabulary does not have.
		{"nobody lists the readers", "", authorizer.ActionReaderList, access.Readers(), false, authz.ReasonAnonymous, nil},
		{"nobody submits", "", authorizer.ActionParseCreate, access.Parse{}.Resource(), false, authz.ReasonAnonymous, nil},
		{"an action outside the vocabulary", alice, "parse.update", parseOf(alice), false, access.ReasonUnknownAction, nil},
		{"an action about another kind", alice, authorizer.ActionParseRead, fileOf(alice), false, access.ReasonUnknownAction, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := policy().Authorize(t.Context(), request(c.subject, c.action, c.res))
			if err != nil {
				t.Fatalf("the policy gave no decision: %v", err)
			}
			if d.Allow != c.allow || d.Reason != c.reason || !reflect.DeepEqual(d.Filter, c.filter) {
				t.Errorf("got allow=%v reason=%q filter=%+v, want allow=%v reason=%q filter=%+v",
					d.Allow, d.Reason, d.Filter, c.allow, c.reason, c.filter)
			}
			// The owner policy names no limit: the defaults are in force.
			if d.Limits != nil {
				t.Errorf("the policy names limits: %s", d.Limits)
			}
		})
	}
}

// The probe id is denied for every subject and every action, by the owner
// policy and by the stub authorizer: an allow would be an authorizer that
// does not read the request.
func TestTheProbeIsDeniedForEverySubjectAndAction(t *testing.T) {
	s := endpoint(t)
	client, err := access.NewClient(access.ClientOptions{URL: s.URL(), Token: reader.NewCredential(s.Token())})
	if err != nil {
		t.Fatal(err)
	}
	deciders := map[string]authz.Authorizer{"the owner policy": policy(), "the stub authorizer": client}
	for name, decider := range deciders {
		for _, a := range authorizer.Vocabulary().Actions {
			for _, subject := range []string{"", alice, bob, root} {
				req := authz.Probe(a.Name, a.Kind)
				req.Subject = subject
				req.Issuer, req.Sub, _ = authz.SplitSubject(subject)
				d, err := decider.Authorize(t.Context(), req)
				if err != nil {
					t.Fatalf("%s gave the probe no decision as %q for %s: %v", name, subject, a.Name, err)
				}
				if d.Allow || d.Reason == "" {
					t.Errorf("%s answered the probe as %q for %s with %+v", name, subject, a.Name, d)
				}
			}
		}
		if err := access.Check(t.Context(), decider); err != nil {
			t.Errorf("%s fails the start-up check: %v", name, err)
		}
	}
}

// The owner policy answers the same contract as an operator's endpoint,
// for every row of the vocabulary, served through the shared scaffold.
func TestOwnerPolicyConformance(t *testing.T) {
	const token = "conformance-bearer"
	srv := httptest.NewServer(server.New(server.Options{
		Bearer: token, Vocabulary: authorizer.Vocabulary(), Decider: policy(),
	}))
	defer srv.Close()
	conformance.Run(t, srv.URL, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithPageActions(),
		conformance.WithSubjects(alice, bob))
}

// The same suite against the policy's own answer. The scaffold above
// narrows by the grants whatever its decider returned, so that run passes
// whether or not the policy narrows anything. lectiod reaches the policy
// through no scaffold: this endpoint is the contract's wire and the bearer
// and nothing else, so what is checked is the narrowing the policy does.
func TestOwnerPolicyNarrowsByTheGrants(t *testing.T) {
	const token = "conformance-bearer"
	srv := httptest.NewServer(bare(policy(), token))
	defer srv.Close()
	conformance.Run(t, srv.URL, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithPageActions(),
		conformance.WithSubjects(alice, bob))
}

// bare serves one decider over the contract's wire: the bearer, the 400 an
// action outside the vocabulary answers, and the decision as the decider
// gave it.
func bare(d *access.OwnerPolicy, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req authz.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || !authorizer.Known(req.Action) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		decision, err := d.Decide(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"allow": decision.Allow, "reason": decision.Reason, "filter": decision.Filter,
		})
	})
}

// A token that carries grants is narrower than its holder: the policy's
// allow stands only where a grant names the action and the resource.
func TestTheGrantsOfATokenNarrowTheOwnersReach(t *testing.T) {
	grant := func(action, id string) map[string]any {
		return map[string]any{
			"token_use": authz.TokenUsePAT,
			"authorization_details": []any{map[string]any{
				"type": authz.GrantType, "actions": []any{authorizer.Core + ":" + action},
				"datatypes": []any{authorizer.KindParse}, "identifier": id,
			}},
		}
	}
	own := access.Parse{ID: "prs_1", Owner: alice}.Resource()
	cases := []struct {
		name, action string
		claims       map[string]any
		allow        bool
		reason       string
	}{
		{"the granted action on the granted parse", authorizer.ActionParseRead, grant(authorizer.ActionParseRead, "prs_1"), true, ""},
		{"the granted action on every parse", authorizer.ActionParseRead, grant(authorizer.ActionParseRead, ""), true, ""},
		{"another action on the granted parse", authorizer.ActionParseDelete, grant(authorizer.ActionParseRead, "prs_1"), false, authz.ReasonGrant},
		{"the granted action on another parse", authorizer.ActionParseRead, grant(authorizer.ActionParseRead, "prs_2"), false, authz.ReasonGrant},
		{"a key that carries no grant", authorizer.ActionParseRead, map[string]any{"token_use": authz.TokenUsePAT}, false, authz.ReasonGrant},
		{"grants nobody can read", authorizer.ActionParseRead, map[string]any{"token_use": authz.TokenUsePAT, "authorization_details": "all"}, false, authz.ReasonGrant},
		{"a token of a person, with no grants", authorizer.ActionParseDelete, map[string]any{}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := request(alice, c.action, own)
			req.Claims = c.claims
			d, err := policy().Authorize(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if d.Allow != c.allow || d.Reason != c.reason {
				t.Errorf("got %+v, want allow=%v reason=%q", d, c.allow, c.reason)
			}
		})
	}
	// A grant is no authority: one that names another's parse reaches
	// nothing the holder could not reach.
	req := request(alice, authorizer.ActionParseRead, access.Parse{ID: "prs_9", Owner: bob}.Resource())
	req.Claims = grant(authorizer.ActionParseRead, "prs_9")
	if d, _ := policy().Authorize(t.Context(), req); d.Allow || d.Reason != authz.ReasonNotOwner {
		t.Errorf("a grant on another's parse gives %+v", d)
	}
}

// Under the owner policy, through the interface a handler asks: a subject
// cannot read, cancel or delete another subject's parse and its list is
// narrowed to its own, an admin can read it and cannot change it, and what
// a request is held to is the server's defaults with the owner as group.
func TestASubjectAndAnotherSubjectsParse(t *testing.T) {
	az := access.NewAuthorizer(policy(), configured())
	stored := access.Parse{ID: "prs_1", Owner: alice, Class: "interactive"}.Resource()
	ask := func(subject, action string, res authz.Resource) access.Decision {
		t.Helper()
		d, err := az.Authorize(t.Context(), caller(subject), access.Question{Action: action, Resource: res})
		if err != nil {
			t.Fatalf("%s as %s: %v", action, subject, err)
		}
		return d
	}
	for _, action := range []string{authorizer.ActionParseRead, authorizer.ActionParseCancel, authorizer.ActionParseDelete} {
		if d := ask(alice, action, stored); !d.Allow {
			t.Errorf("the owner is denied %s: %+v", action, d)
		}
		d := ask(bob, action, stored)
		if d.Allow {
			t.Errorf("another subject is allowed %s", action)
		}
		if fault.CodeOf(d.Err()) != fault.Forbidden {
			t.Errorf("the refusal of %s is %v", action, d.Err())
		}
	}
	if d := ask(bob, authorizer.ActionParseList, access.Parses()); !d.Allow || !reflect.DeepEqual(d.Filter, mine(bob)) {
		t.Errorf("another subject's list is %+v, want it narrowed to its own", d)
	}
	if d := ask(root, authorizer.ActionParseRead, stored); !d.Allow {
		t.Errorf("an admin is denied the read: %+v", d)
	}
	if d := ask(root, authorizer.ActionParseList, access.Parses()); !d.Allow || d.Filter != nil {
		t.Errorf("an admin's list is %+v, want it narrowed by nothing", d)
	}
	for _, action := range []string{authorizer.ActionParseCancel, authorizer.ActionParseDelete} {
		if d := ask(root, action, stored); d.Allow {
			t.Errorf("an admin is allowed %s on another's parse", action)
		}
	}

	// The limits are the configured defaults, and the group is the owner.
	d := ask(bob, authorizer.ActionParseCreate, access.Parse{Class: "batch"}.Resource())
	want := configured()
	want.Owner, want.Group = bob, bob
	if !d.Allow || !reflect.DeepEqual(d.Limits, want) {
		t.Errorf("a submit is held to %+v, want %+v", d.Limits, want)
	}
}
