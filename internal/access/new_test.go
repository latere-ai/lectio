// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/fault"
)

// settings reads a server's settings from the variables given, the way
// lectiod reads its environment.
func settings(t *testing.T, pairs ...string) config.Settings {
	t.Helper()
	env := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		env[pairs[i]] = pairs[i+1]
	}
	s, err := config.FromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("the settings: %v", err)
	}
	return s
}

// Which settings select which mode. Who is calling and who decides are
// chosen apart: the issuers choose the first and the authorizer's URL the
// second.
func TestTheSettingsSelectTheMode(t *testing.T) {
	cases := []struct {
		name                    string
		pairs                   []string
		identity, authorization string
	}{
		{"a verifier and an authorizer",
			[]string{"LECTIO_OIDC_ISSUERS", issuer, "LECTIO_AUTHORIZER_URL", "https://plane.example/authz", "LECTIO_AUTHORIZER_TOKEN", "t"},
			access.IdentityOIDC, access.AuthorizationEndpoint},
		{"a verifier and the owner policy",
			[]string{"LECTIO_OIDC_ISSUERS", issuer, "LECTIO_ADMIN_SUBJECTS", root},
			access.IdentityOIDC, access.AuthorizationOwnerPolicy},
		{"a development server",
			[]string{"LECTIO_DEV", "true"},
			access.IdentityDevelopment, access.AuthorizationOwnerPolicy},
		{"a development server with issuers verifies",
			[]string{"LECTIO_DEV", "true", "LECTIO_OIDC_ISSUERS", issuer},
			access.IdentityOIDC, access.AuthorizationOwnerPolicy},
		{"a development server with an authorizer asks it",
			[]string{"LECTIO_DEV", "true", "LECTIO_AUTHORIZER_URL", "https://plane.example/authz", "LECTIO_AUTHORIZER_TOKEN", "t"},
			access.IdentityDevelopment, access.AuthorizationEndpoint},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := access.New(settings(t, c.pairs...))
			if err != nil {
				t.Fatal(err)
			}
			if a.Identity != c.identity || a.Authorization != c.authorization {
				t.Errorf("the mode is %q and %q, want %q and %q", a.Identity, a.Authorization, c.identity, c.authorization)
			}
			if a.Authenticator == nil || a.Authorizer == nil {
				t.Errorf("a handler has nothing to depend on: %+v", a)
			}
		})
	}
	// The phrase lectiod logs at start is the spec's.
	if access.AuthorizationOwnerPolicy != "owner policy" {
		t.Errorf("the owner policy is logged as %q", access.AuthorizationOwnerPolicy)
	}
}

// A server that is not a development one and lists no issuer is refused,
// and the refusal names the variable: there is no anonymous access.
func TestAServerWithNoIssuerIsRefused(t *testing.T) {
	a, err := access.New(settings(t))
	if err == nil {
		t.Fatalf("built %+v", a)
	}
	if !strings.Contains(err.Error(), "LECTIO_OIDC_ISSUERS") || !strings.Contains(err.Error(), "LECTIO_DEV") {
		t.Errorf("the refusal does not name what to set: %v", err)
	}
}

// Settings that were not read from the environment are still checked.
func TestSettingsBuiltByHandAreChecked(t *testing.T) {
	for name, s := range map[string]config.Settings{
		"issuers and no audience":        {OIDCIssuers: []string{issuer}},
		"a development server, no token": {Dev: true},
	} {
		if a, err := access.New(s); err == nil {
			t.Errorf("%s: built %+v", name, a)
		}
	}
}

func TestTheDefaultsAreTheServersLimits(t *testing.T) {
	s := settings(t, "LECTIO_MAX_FILE_BYTES", "1024", "LECTIO_MAX_PAGES", "10")
	if got, want := access.Defaults(s), (authorizer.Limits{MaxFileBytes: 1024, MaxPages: 10}); !reflect.DeepEqual(got, want) {
		t.Errorf("the defaults are %+v, want %+v", got, want)
	}
}

// One server, a verifier and an authorizer: a token of the issuer becomes
// a caller, the endpoint is asked with the caller's claims as they came,
// and the allow's limits lie over the server's own.
func TestAVerifierAndAnAuthorizerEndToEnd(t *testing.T) {
	iss := issuertest.New(t)
	s := endpoint(t)
	s.Allow(stub.Rule{Action: authorizer.ActionParseCreate, Limits: map[string]any{"owner": "org:acme", "group": "acme", "max_file_bytes": 1024}})
	configured := settings(t,
		"LECTIO_OIDC_ISSUERS", iss.URL(), "LECTIO_AUTHORIZER_URL", s.URL(), "LECTIO_AUTHORIZER_TOKEN", s.Token(),
		"LECTIO_MAX_PAGES", "500",
	)
	a, err := access.New(configured)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Warm(t.Context()); err != nil {
		t.Fatalf("warming: %v", err)
	}
	if err := a.Check(t.Context()); err != nil {
		t.Fatalf("the start-up check: %v", err)
	}
	s.ClearRequests()

	token := iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}, Extra: map[string]any{"tenant": "acme"}})
	r := bearing(t, token)
	c, err := a.Authenticator.Authenticate(r)
	if err != nil {
		t.Fatalf("the token was refused: %v", err)
	}
	d, err := a.Authorizer.Authorize(t.Context(), c, access.Ask(r, authorizer.ActionParseCreate, access.Parse{Class: "interactive"}.Resource()))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow || d.Limits.Owner != "org:acme" || d.Limits.Group != "acme" || d.Limits.MaxFileBytes != 1024 || d.Limits.MaxPages != 500 {
		t.Errorf("the decision is %+v", d)
	}
	seen := s.Requests()
	if len(seen) != 1 || seen[0].Subject != iss.URL()+"|alice" || seen[0].Claims["tenant"] != "acme" || seen[0].Claims["sub"] != "alice" {
		t.Errorf("the endpoint saw %+v", seen)
	}

	// The endpoint goes away: the next question fails closed.
	s.Fail(503)
	if _, err := a.Authorizer.Authorize(t.Context(), c, access.Ask(r, authorizer.ActionParseCreate, access.Parse{}.Resource())); fault.CodeOf(err) != fault.AuthorizerUnavailable {
		t.Errorf("with the endpoint down: %v", err)
	}
	// A server that starts while the endpoint is down learns it from the
	// check. One that was already running holds the probe's deny for the
	// contract's short while, as it holds any deny.
	starting, err := access.New(configured)
	if err != nil {
		t.Fatal(err)
	}
	if err := starting.Check(t.Context()); err == nil {
		t.Error("the start-up check passes an endpoint that is down")
	}
	// Nothing printed of the server's identity shows the endpoint's bearer.
	if shown := fmt.Sprintf("%+v %#v", a, a); strings.Contains(shown, s.Token()) {
		t.Errorf("the identity prints the authorizer's token: %s", shown)
	}
}

// One server, a verifier and the owner policy: the subjects of
// LECTIO_ADMIN_SUBJECTS read what another owns, and nobody else does.
func TestAVerifierAndTheOwnerPolicyEndToEnd(t *testing.T) {
	iss := issuertest.New(t)
	admin := iss.URL() + "|root"
	a, err := access.New(settings(t, "LECTIO_OIDC_ISSUERS", iss.URL(), "LECTIO_ADMIN_SUBJECTS", admin))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Check(t.Context()); err != nil {
		t.Fatalf("the owner policy fails the start-up check: %v", err)
	}
	stored := access.Parse{ID: "prs_1", Owner: iss.URL() + "|alice"}.Resource()
	for sub, allowed := range map[string]bool{"alice": true, "bob": false, "root": true} {
		r := bearing(t, iss.Mint(issuertest.Claims{Sub: sub, Aud: issuertest.StringList{audience}}))
		c, err := a.Authenticator.Authenticate(r)
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		d, err := a.Authorizer.Authorize(t.Context(), c, access.Ask(r, authorizer.ActionParseRead, stored))
		if err != nil || d.Allow != allowed {
			t.Errorf("%s reading alice's parse: %+v, %v, want allow=%v", sub, d, err, allowed)
		}
	}
}

// A development server: the one static token, the owner policy, and no
// issuer to warm.
func TestADevelopmentServerEndToEnd(t *testing.T) {
	a, err := access.New(settings(t, "LECTIO_DEV", "true", "LECTIO_DEV_TOKEN", "t0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Warm(t.Context()); err != nil {
		t.Errorf("warming a server with no issuer: %v", err)
	}
	if _, err := a.Authenticator.Authenticate(bearing(t, "dev")); fault.CodeOf(err) != fault.InvalidToken {
		t.Errorf("another token: %v, want invalid_token", err)
	}
	r := bearing(t, "t0")
	c, err := a.Authenticator.Authenticate(r)
	if err != nil || c.Subject != access.DevSubject {
		t.Fatalf("the development token: %+v, %v", c, err)
	}
	d, err := a.Authorizer.Authorize(t.Context(), c, access.Ask(r, authorizer.ActionFileCreate, access.File{MediaType: "application/pdf"}.Resource()))
	if err != nil || !d.Allow || d.Limits.Owner != "dev" || d.Limits.Group != "dev" || d.Limits.MaxFileBytes != 256<<20 {
		t.Errorf("an upload: %+v, %v", d, err)
	}
}
