// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
)

// The subjects the tests ask as, rendered the way a verified token is.
const (
	issuer = "https://issuer.example"
	alice  = issuer + "|alice"
	bob    = issuer + "|bob"
	root   = issuer + "|root"
)

// caller is a verified caller with the subject and no claim.
func caller(subject string) access.Caller {
	iss, sub, _ := authz.SplitSubject(subject)
	return access.Caller{Subject: subject, Issuer: iss, Sub: sub, Claims: map[string]any{}}
}

// answers is an authorizer that gives one answer and keeps what it was
// asked.
type answers struct {
	decision authz.Decision
	err      error
	asked    []authz.Request
}

func (a *answers) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	a.asked = append(a.asked, req)
	return a.decision, a.err
}

// allowing is an authorizer that allows with the limits object raw.
func allowing(raw string) *answers {
	d := authz.Decision{Allow: true}
	if raw != "" {
		d.Limits = json.RawMessage(raw)
	}
	return &answers{decision: d}
}

// configured are a server's limits, each member set so a change shows.
func configured() authorizer.Limits {
	return authorizer.Limits{
		Weight: 1, ProjectWeight: 1, MaxRunning: 16, MaxQueued: 200, MaxPriority: 10,
		MaxFileBytes: 256 << 20, MaxPages: 3000, Retention: 720 * time.Hour,
	}
}

func submit(owner string) access.Question {
	return access.Question{Action: authorizer.ActionParseCreate, Resource: access.Parse{Owner: owner, Class: "interactive"}.Resource()}
}

func TestTheEnvelopeCarriesTheCallerAndTheQuestion(t *testing.T) {
	inner := allowing("")
	az := access.NewAuthorizer(inner, configured())
	c := caller(alice)
	// A claim Lectio reads nothing of, to be handed on as it came.
	c.Claims = map[string]any{"tenant": "acme", "groups": []any{"a", "b"}, "exp": float64(1790000000)}
	q := access.Question{
		Action:   authorizer.ActionParseRead,
		Resource: access.Parse{ID: "prs_1", Owner: alice, Class: "batch", Priority: 2}.Resource(),
		Request:  authz.Caller{ID: "req-1", IP: "203.0.113.4", UserAgent: "test"},
	}
	if _, err := az.Authorize(t.Context(), c, q); err != nil {
		t.Fatal(err)
	}
	if len(inner.asked) != 1 {
		t.Fatalf("the authorizer was asked %d times", len(inner.asked))
	}
	got := inner.asked[0]
	if got.Subject != alice || got.Issuer != issuer || got.Sub != "alice" {
		t.Errorf("the envelope names %q, %q, %q", got.Subject, got.Issuer, got.Sub)
	}
	if !reflect.DeepEqual(got.Claims, c.Claims) {
		t.Errorf("the claims arrive as %v, want %v verbatim", got.Claims, c.Claims)
	}
	if got.Action != q.Action || !reflect.DeepEqual(got.Resource, q.Resource) || got.Request != q.Request {
		t.Errorf("the question arrives as %+v", got)
	}
	if got.Workload != nil {
		t.Errorf("the envelope carries a workload: %v", got.Workload)
	}
}

// A caller with no claims sends an empty object and not null, which an
// endpoint reads as a request with no claims and not as a malformed one.
func TestACallerWithNoClaimsSendsAnEmptyObject(t *testing.T) {
	inner := allowing("")
	az := access.NewAuthorizer(inner, configured())
	if _, err := az.Authorize(t.Context(), access.Caller{Subject: "dev", Sub: "dev"}, submit("")); err != nil {
		t.Fatal(err)
	}
	if claims := inner.asked[0].Claims; claims == nil || len(claims) != 0 {
		t.Errorf("the claims are %v, want an empty object", claims)
	}
}

func TestADenyIsADecision(t *testing.T) {
	az := access.NewAuthorizer(&answers{decision: authz.Decision{Reason: "not_owner"}}, configured())
	d, err := az.Authorize(t.Context(), caller(alice), submit(""))
	if err != nil {
		t.Fatalf("a deny is an error: %v", err)
	}
	if d.Allow || d.Reason != "not_owner" || !reflect.DeepEqual(d.Limits, authorizer.Limits{}) || d.Filter != nil {
		t.Errorf("the deny is %+v", d)
	}
	refusal := d.Err()
	if fault.CodeOf(refusal) != fault.Forbidden || !strings.Contains(fault.DetailOf(refusal), "not_owner") {
		t.Errorf("the refusal is %v", refusal)
	}
	// A read of a stored object answers a refused one as a missing one.
	missing := fault.New(fault.ParseNotFound, "no parse has this id")
	if got := d.Or(missing); got != error(missing) {
		t.Errorf("Or gives %v, want the error it was passed", got)
	}
}

func TestADenyWithNoReasonStillSaysSomething(t *testing.T) {
	az := access.NewAuthorizer(&answers{}, configured())
	d, err := az.Authorize(t.Context(), caller(alice), submit(""))
	if err != nil || d.Allow || d.Reason == "" {
		t.Errorf("got %+v, %v", d, err)
	}
}

func TestAnAllowIsNoRefusal(t *testing.T) {
	az := access.NewAuthorizer(allowing(""), configured())
	d, err := az.Authorize(t.Context(), caller(alice), submit(""))
	if err != nil || !d.Allow {
		t.Fatalf("got %+v, %v", d, err)
	}
	if d.Err() != nil || d.Or(errors.New("refused")) != nil {
		t.Errorf("an allow refuses: %v", d.Err())
	}
}

// The owner a create is recorded under: the one the allow names, else the
// one the request named, else the caller. The group is the owner unless
// the allow names one. No claim is read for either.
func TestWhoseACreateIs(t *testing.T) {
	cases := []struct {
		name, requested, limits string
		owner, group            string
	}{
		{"nothing named", "", "", alice, alice},
		{"the request names an owner", "org:acme", "", "org:acme", "org:acme"},
		{"the allow names the owner", "", `{"owner": "org:acme"}`, "org:acme", "org:acme"},
		{"the allow's owner wins over the request's", "org:other", `{"owner": "org:acme"}`, "org:acme", "org:acme"},
		{"the allow names a group", "", `{"group": "team-1"}`, alice, "team-1"},
		{"the allow names both", "", `{"owner": "org:acme", "group": "acme"}`, "org:acme", "acme"},
		{"an empty owner on the allow names none", "", `{"owner": ""}`, alice, alice},
	}
	for _, c := range cases {
		for _, action := range []string{authorizer.ActionParseCreate, authorizer.ActionFileCreate} {
			t.Run(c.name+"/"+action, func(t *testing.T) {
				az := access.NewAuthorizer(allowing(c.limits), configured())
				q := submit(c.requested)
				if action == authorizer.ActionFileCreate {
					q = access.Question{Action: action, Resource: access.File{Owner: c.requested}.Resource()}
				}
				d, err := az.Authorize(t.Context(), caller(alice), q)
				if err != nil {
					t.Fatal(err)
				}
				if d.Limits.Owner != c.owner || d.Limits.Group != c.group {
					t.Errorf("recorded under %q in group %q, want %q in %q", d.Limits.Owner, d.Limits.Group, c.owner, c.group)
				}
			})
		}
	}
}

// Each member of the limits, through the question a submit asks: an allow
// that carries it changes what the request is held to, and one that does
// not leaves the server's default.
func TestEachLimitOfASubmit(t *testing.T) {
	base := configured()
	base.Owner, base.Group = alice, alice
	cases := map[string]func(*authorizer.Limits){
		`{"owner": "org:acme"}`:             func(l *authorizer.Limits) { l.Owner, l.Group = "org:acme", "org:acme" },
		`{"group": "acme"}`:                 func(l *authorizer.Limits) { l.Group = "acme" },
		`{"weight": 8}`:                     func(l *authorizer.Limits) { l.Weight = 8 },
		`{"project": "reports"}`:            func(l *authorizer.Limits) { l.Project = "reports" },
		`{"project_weight": 3}`:             func(l *authorizer.Limits) { l.ProjectWeight = 3 },
		`{"max_running": 2}`:                func(l *authorizer.Limits) { l.MaxRunning = 2 },
		`{"max_queued": 0}`:                 func(l *authorizer.Limits) { l.MaxQueued = 0 },
		`{"max_priority": 1}`:               func(l *authorizer.Limits) { l.MaxPriority = 1 },
		`{"classes": ["batch"]}`:            func(l *authorizer.Limits) { l.Classes = []string{"batch"} },
		`{"readers": ["small"]}`:            func(l *authorizer.Limits) { l.Readers = []string{"small"} },
		`{"max_file_bytes": 1024}`:          func(l *authorizer.Limits) { l.MaxFileBytes = 1024 },
		`{"max_pages": 10}`:                 func(l *authorizer.Limits) { l.MaxPages = 10 },
		`{"pages_per_day": 500}`:            func(l *authorizer.Limits) { l.PagesPerDay = 500 },
		`{"retention_seconds": 60}`:         func(l *authorizer.Limits) { l.Retention = time.Minute },
		`{"max_file_bytes": 1000000000000}`: nil, // above the server's: an allow never raises it
		`{}`:                                nil,
		``:                                  nil,
		`{"weight": null}`:                  nil,
		`{"max_pages": 5000}`:               nil,
		`{"retention_seconds": 1000000000}`: nil,
	}
	named := 0
	for raw, change := range cases {
		t.Run(raw, func(t *testing.T) {
			az := access.NewAuthorizer(allowing(raw), configured())
			d, err := az.Authorize(t.Context(), caller(alice), submit(""))
			if err != nil {
				t.Fatal(err)
			}
			want := base
			if change != nil {
				change(&want)
			}
			if !reflect.DeepEqual(d.Limits, want) {
				t.Errorf("held to %+v, want %+v", d.Limits, want)
			}
		})
		if change != nil {
			named++
		}
	}
	if members := reflect.TypeFor[authorizer.Limits]().NumField(); named != members {
		t.Errorf("%d cases change a member, and Limits has %d", named, members)
	}
}

// An upload joins no queue and reads no page: it is held to the owner,
// the group, the file size and the retention, whatever else the allow
// names.
func TestAnUploadIsHeldToFourMembers(t *testing.T) {
	raw := `{"owner": "org:acme", "group": "acme", "weight": 8, "max_queued": 1, "max_priority": 0, "classes": ["batch"],
		"max_file_bytes": 1024, "max_pages": 10, "pages_per_day": 5, "retention_seconds": 60}`
	az := access.NewAuthorizer(allowing(raw), configured())
	q := access.Question{Action: authorizer.ActionFileCreate, Resource: access.File{MediaType: "application/pdf"}.Resource()}
	d, err := az.Authorize(t.Context(), caller(alice), q)
	if err != nil {
		t.Fatal(err)
	}
	want := configured()
	want.Owner, want.Group, want.MaxFileBytes, want.Retention = "org:acme", "acme", 1024, time.Minute
	if !reflect.DeepEqual(d.Limits, want) {
		t.Errorf("held to %+v, want %+v", d.Limits, want)
	}
}

// The limits of an allow of any other action are not read: a read is held
// to the defaults, under the stored object's owner, whatever the answer
// carries.
func TestTheLimitsOfAReadAreNotRead(t *testing.T) {
	for _, raw := range []string{`{"max_pages": 10, "owner": "org:acme"}`, `{"max_pages": -1}`, `{"burst": 1}`, `[1]`} {
		az := access.NewAuthorizer(allowing(raw), configured())
		q := access.Question{Action: authorizer.ActionParseRead, Resource: access.Parse{ID: "prs_1", Owner: bob}.Resource()}
		d, err := az.Authorize(t.Context(), caller(alice), q)
		if err != nil {
			t.Fatalf("limits %s: %v", raw, err)
		}
		want := configured()
		want.Owner, want.Group = bob, bob
		if !reflect.DeepEqual(d.Limits, want) {
			t.Errorf("limits %s: held to %+v, want %+v", raw, d.Limits, want)
		}
	}
}

func TestTheFilterOfAListIsHandedOn(t *testing.T) {
	filter := &authz.Filter{Owners: []string{alice, "org:acme"}, Labels: map[string]string{"team": "a"}}
	az := access.NewAuthorizer(&answers{decision: authz.Decision{Allow: true, Reason: "member", Filter: filter}}, configured())
	d, err := az.Authorize(t.Context(), caller(alice), access.Question{Action: authorizer.ActionParseList, Resource: access.Parses()})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow || d.Reason != "member" || !reflect.DeepEqual(d.Filter, filter) {
		t.Errorf("the decision is %+v", d)
	}
}

// A question that got no decision fails closed, and what the caller is
// told names neither the endpoint nor what went wrong inside it.
func TestNoDecisionIsUnavailable(t *testing.T) {
	cause := &authz.Unavailable{URL: "https://plane.example/authz/lectio", Status: http.StatusBadGateway}
	az := access.NewAuthorizer(&answers{decision: authz.Decision{Allow: true}, err: cause}, configured())
	d, err := az.Authorize(t.Context(), caller(alice), submit(""))
	if fault.CodeOf(err) != fault.AuthorizerUnavailable {
		t.Fatalf("got %+v, %v, want authorizer_unavailable", d, err)
	}
	if d.Allow {
		t.Error("an allow came back with the error")
	}
	if detail := fault.DetailOf(err); strings.Contains(detail, "plane.example") || strings.Contains(detail, "502") {
		t.Errorf("the detail a caller reads is %q", detail)
	}
	if !errors.Is(err, error(cause)) {
		t.Error("the cause is not kept for the log")
	}
}

// Limits the server cannot read are no decision. A limit it does not know
// is a refusal of its own, and names the member.
func TestLimitsTheServerCannotHold(t *testing.T) {
	cases := []struct {
		name, raw string
		code      fault.Code
		detail    string
	}{
		{"a figure below zero", `{"max_pages": -1}`, fault.AuthorizerUnavailable, "cannot be read"},
		{"a weight above the most", `{"weight": 1001}`, fault.AuthorizerUnavailable, "cannot be read"},
		{"not an object", `[1]`, fault.AuthorizerUnavailable, "cannot be read"},
		{"a member this version does not know", `{"tokens_per_day": 5, "max_pages": 10}`, fault.CapabilityUnsupported, "tokens_per_day"},
	}
	for _, c := range cases {
		for _, action := range []string{authorizer.ActionParseCreate, authorizer.ActionFileCreate} {
			t.Run(c.name+"/"+action, func(t *testing.T) {
				az := access.NewAuthorizer(allowing(c.raw), configured())
				q := submit("")
				if action == authorizer.ActionFileCreate {
					q = access.Question{Action: action, Resource: access.File{}.Resource()}
				}
				d, err := az.Authorize(t.Context(), caller(alice), q)
				if fault.CodeOf(err) != c.code || d.Allow {
					t.Fatalf("got %+v, %v, want %s", d, err, c.code)
				}
				if !strings.Contains(fault.DetailOf(err), c.detail) {
					t.Errorf("the detail %q does not say %q", fault.DetailOf(err), c.detail)
				}
			})
		}
	}
}

// A question outside the vocabulary, or about another kind than its
// action acts on, is a mistake in a handler: it is refused and never
// sent, so it cannot be answered with an allow.
func TestAQuestionTheVocabularyDoesNotHaveIsNeverSent(t *testing.T) {
	cases := []access.Question{
		{Action: "parse.update", Resource: access.Parse{ID: "prs_1", Owner: alice}.Resource()},
		{Action: authorizer.ActionParseRead, Resource: access.File{ID: "fil_1", Owner: alice}.Resource()},
		{Action: "", Resource: access.Parses()},
	}
	for _, q := range cases {
		inner := allowing("")
		az := access.NewAuthorizer(inner, configured())
		d, err := az.Authorize(t.Context(), caller(alice), q)
		if err == nil || fault.CodeOf(err) != fault.Internal || d.Allow {
			t.Errorf("%q on %q: got %+v, %v", q.Action, q.Resource.Kind, d, err)
		}
		if len(inner.asked) != 0 {
			t.Errorf("%q on %q was sent", q.Action, q.Resource.Kind)
		}
	}
}

func TestAskBuildsTheQuestionOfARequest(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/parses", nil)
	r.RemoteAddr = "203.0.113.4:51234"
	r.Header.Set("X-Request-Id", "req-7")
	r.Header.Set("User-Agent", "client/1.0")
	q := access.Ask(r, authorizer.ActionParseList, access.Parses())
	want := authz.Caller{ID: "req-7", IP: "203.0.113.4", UserAgent: "client/1.0"}
	if q.Action != authorizer.ActionParseList || q.Resource.Kind != authorizer.KindParse || q.Request != want {
		t.Errorf("the question is %+v", q)
	}

	// An address with no port is the host as it stands, and IPv6 loses
	// its brackets with its port.
	r.RemoteAddr = "203.0.113.4"
	if got := access.RequestInfo(r).IP; got != "203.0.113.4" {
		t.Errorf("an address with no port reads as %q", got)
	}
	r.RemoteAddr = "[2001:db8::1]:443"
	if got := access.RequestInfo(r).IP; got != "2001:db8::1" {
		t.Errorf("an IPv6 address reads as %q", got)
	}
}

func TestCheckReadsTheProbesAnswer(t *testing.T) {
	denies := &answers{decision: authz.Decision{Reason: "probe"}}
	if err := access.Check(t.Context(), denies); err != nil {
		t.Errorf("an authorizer that denies the probe: %v", err)
	}
	probe := denies.asked[0]
	if probe.Resource.ID != authz.ProbeID || probe.Subject != "" || !slices.Contains(authorizer.Actions(), probe.Action) {
		t.Errorf("the probe is %+v", probe)
	}
	if err := access.Check(t.Context(), allowing("")); !errors.Is(err, authz.ErrProbeAllowed) {
		t.Errorf("an authorizer that allows the probe: %v", err)
	}
	down := &answers{err: &authz.Unavailable{Status: http.StatusServiceUnavailable}}
	if err := access.Check(t.Context(), down); err == nil {
		t.Error("an authorizer that does not answer passes the check")
	}
}
