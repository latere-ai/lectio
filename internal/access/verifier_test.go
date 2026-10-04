// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authkit/conformance"
	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
)

// audience is the audience the gate's identity block declares for this
// repository, and the default of LECTIO_OIDC_AUDIENCE.
const audience = "lectio"

// verifying builds the verifier lectiod runs, over the stub issuers.
func verifying(t testing.TB, audiences []string, issuers ...*issuertest.Server) *access.Verifier {
	t.Helper()
	var urls []string
	for _, iss := range issuers {
		urls = append(urls, iss.URL())
	}
	v, err := access.NewVerifier(access.VerifierOptions{Issuers: urls, Audiences: audiences})
	if err != nil {
		t.Fatalf("the verifier would not build: %v", err)
	}
	return v
}

// bearing is a request that carries the token as its bearer.
func bearing(t testing.TB, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/parses", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// The verifier passes the shared conformance suite: it admits its own
// audience and no other, refuses a token that names nobody, and calls the
// issuer for nothing but its discovery document and its key set. The
// authenticator under test is the validator lectiod runs.
func TestVerifierConformance(t *testing.T) {
	conformance.Run(t, conformance.Service{
		Audience: audience,
		New: func(tb testing.TB, issuerURL, _ string) authkit.Authenticator {
			tb.Helper()
			v, err := access.NewVerifier(access.VerifierOptions{Issuers: []string{issuerURL}, Audiences: []string{audience}})
			if err != nil {
				tb.Fatalf("the verifier would not build: %v", err)
			}
			return v.Authenticator()
		},
	})
}

func TestAVerifiedTokenBecomesACaller(t *testing.T) {
	iss := issuertest.New(t)
	v := verifying(t, []string{audience}, iss)
	token := iss.Mint(issuertest.Claims{
		Sub: "alice", Aud: issuertest.StringList{audience},
		// Claims Lectio reads nothing of. They reach the authorizer as
		// the issuer wrote them.
		Extra: map[string]any{"tenant": "acme", "groups": []any{"readers", "writers"}},
	})
	c, err := v.Authenticate(bearing(t, token))
	if err != nil {
		t.Fatalf("a token for %q was refused: %v", audience, err)
	}
	// The subject is rendered once, as the issuer and the sub joined.
	if want := iss.URL() + "|alice"; c.Subject != want || c.Issuer != iss.URL() || c.Sub != "alice" {
		t.Errorf("the caller is %q, %q, %q, want the subject %q", c.Subject, c.Issuer, c.Sub, want)
	}
	if c.Subject != authz.Subject(iss.URL(), "alice") {
		t.Errorf("the subject %q is not the shared contract's rendering", c.Subject)
	}
	if c.Claims["tenant"] != "acme" || !slices.Equal(c.Claims["groups"].([]any), []any{"readers", "writers"}) {
		t.Errorf("the claims are %v", c.Claims)
	}
	for _, registered := range []string{"iss", "sub", "aud", "exp"} {
		if _, ok := c.Claims[registered]; !ok {
			t.Errorf("the claims lack %q: %v", registered, c.Claims)
		}
	}
}

func TestWhatTheVerifierRefuses(t *testing.T) {
	iss, other := issuertest.New(t), issuertest.New(t)
	v := verifying(t, []string{audience}, iss)
	now := time.Now()
	good := iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}})
	// The signature is the last segment. Its first character is changed:
	// the last one carries bits a decoder drops.
	at := strings.LastIndexByte(good, '.') + 1
	forged := good[:at] + map[bool]string{true: "A", false: "B"}[good[at] != 'A'] + good[at+1:]

	cases := []struct {
		name, token, reason string
	}{
		{"another audience", iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"another-service"}}), "audience"},
		{"no audience", iss.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"aud"}}), "audience"},
		{"expired past the skew", iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}, Exp: now.Add(-5 * time.Minute).Unix()}), "expired"},
		{"not valid yet, past the skew", iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}, Nbf: now.Add(5 * time.Minute).Unix()}), "nbf"},
		{"an issuer that is not listed", other.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}}), "issuer"},
		{"a signature that does not check out", forged, "signature"},
		{"not a token", "not-a-token", "malformed"},
		{"no subject", iss.Mint(issuertest.Claims{Aud: issuertest.StringList{audience}, Omit: []string{"sub"}}), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := v.Authenticate(bearing(t, c.token))
			if fault.CodeOf(err) != fault.InvalidToken {
				t.Fatalf("got the caller %+v and %v, want invalid_token", got, err)
			}
			if detail := fault.DetailOf(err); !strings.Contains(detail, c.reason) {
				t.Errorf("the detail %q does not carry the reason %q", detail, c.reason)
			}
			// A credential's value is never part of what is said about it.
			if strings.Contains(err.Error(), c.token) {
				t.Errorf("the error repeats the token: %v", err)
			}
		})
	}
}

func TestARequestWithNoBearer(t *testing.T) {
	iss := issuertest.New(t)
	v := verifying(t, []string{audience}, iss)
	basic := bearing(t, "")
	basic.Header.Set("Authorization", "Basic YWxpY2U6c2VjcmV0")
	empty := bearing(t, "")
	empty.Header.Set("Authorization", "Bearer ")
	for name, r := range map[string]*http.Request{"no header": bearing(t, ""), "another scheme": basic, "an empty bearer": empty} {
		if _, err := v.Authenticate(r); fault.CodeOf(err) != fault.MissingToken {
			t.Errorf("%s: got %v, want missing_token", name, err)
		}
	}
}

// exp and nbf are read with a skew: a token is accepted until ClockSkew
// past its exp and from ClockSkew before its nbf.
func TestTheSkewOnExpAndNbf(t *testing.T) {
	iss := issuertest.New(t)
	v := verifying(t, []string{audience}, iss)
	now := time.Now()
	within := access.ClockSkew / 3
	for name, claims := range map[string]issuertest.Claims{
		"expired within the skew":       {Sub: "alice", Aud: issuertest.StringList{audience}, Exp: now.Add(-within).Unix()},
		"not valid yet within the skew": {Sub: "alice", Aud: issuertest.StringList{audience}, Nbf: now.Add(within).Unix()},
	} {
		if _, err := v.Verify(iss.Mint(claims)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// Two issuers that agree on a sub are two subjects, and each issuer
// answers for its own tokens alone.
func TestTwoIssuersAreTwoSubjects(t *testing.T) {
	one, two := issuertest.New(t), issuertest.New(t)
	v := verifying(t, []string{audience}, one, two)
	if got := v.Issuers(); !slices.Equal(got, []string{one.URL(), two.URL()}) {
		t.Fatalf("the verifier lists %v", got)
	}
	var subjects []string
	for _, iss := range []*issuertest.Server{one, two} {
		c, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}}))
		if err != nil {
			t.Fatalf("a token of %s was refused: %v", iss.URL(), err)
		}
		if c.Issuer != iss.URL() {
			t.Errorf("the caller's issuer is %q, want %q", c.Issuer, iss.URL())
		}
		subjects = append(subjects, c.Subject)
	}
	if subjects[0] == subjects[1] {
		t.Errorf("two issuers gave one subject: %q", subjects[0])
	}
	// A caller's copy of the list is its own.
	v.Issuers()[0] = "changed"
	if v.Issuers()[0] != one.URL() {
		t.Error("a caller changed the verifier's issuers")
	}
}

// The audience is a list: a token addressed to any entry is accepted, and
// the first entry is the primary one.
func TestAnyListedAudienceIsAccepted(t *testing.T) {
	iss := issuertest.New(t)
	v := verifying(t, []string{"parsing", audience}, iss)
	if v.Audience() != "parsing" {
		t.Errorf("the primary audience is %q, want the first entry", v.Audience())
	}
	for _, aud := range []string{"parsing", audience} {
		if _, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{aud}})); err != nil {
			t.Errorf("a token for %q was refused: %v", aud, err)
		}
	}
	if _, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"another-service"}})); err == nil {
		t.Error("a token for another service was accepted")
	}
}

// An issuer written with a trailing slash is the same issuer, and the
// subject is rendered without it.
func TestAnIssuerWithATrailingSlash(t *testing.T) {
	iss := issuertest.New(t)
	v, err := access.NewVerifier(access.VerifierOptions{Issuers: []string{iss.URL() + "/"}, Audiences: []string{audience}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}}))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if c.Subject != iss.URL()+"|alice" {
		t.Errorf("the subject is %q", c.Subject)
	}
}

// A personal access token carries the grants its holder chose. The
// verifier admits it and hands the grants on with every other claim: they
// are applied where the decision is made.
func TestATokenWithGrantsIsAdmittedWithItsGrants(t *testing.T) {
	iss := issuertest.New(t)
	v := verifying(t, []string{audience}, iss)
	grants := []any{map[string]any{"type": authz.GrantType, "actions": []any{"lectio:parse.read"}, "datatypes": []any{"Parse"}}}
	c, err := v.Verify(iss.Mint(issuertest.Claims{
		Sub: "alice", Aud: issuertest.StringList{audience},
		Extra: map[string]any{"token_use": authz.TokenUsePAT, "authorization_details": grants},
	}))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	parsed, err := authz.ParseGrants(c.Claims)
	if err != nil || len(parsed) != 1 || !slices.Equal(parsed[0].Actions, []string{"lectio:parse.read"}) {
		t.Errorf("the grants arrive as %+v, %v", parsed, err)
	}
}

func TestWarmReadsEachIssuersKeys(t *testing.T) {
	iss := issuertest.New(t)
	v := verifying(t, []string{audience}, iss)
	if err := v.Warm(t.Context()); err != nil {
		t.Fatalf("warming a reachable issuer: %v", err)
	}
	// Warm, the verifier reads a token with no further fetch.
	iss.ResetRequests()
	if _, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}})); err != nil {
		t.Fatal(err)
	}
	if got := iss.Requests(); len(got) != 0 {
		t.Errorf("a warm verifier called the issuer: %v", got)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	down, err := access.NewVerifier(access.VerifierOptions{Issuers: []string{gone.URL}, Audiences: []string{audience}})
	if err != nil {
		t.Fatal(err)
	}
	if err := down.Warm(t.Context()); err == nil {
		t.Error("warming an issuer that does not answer reports nothing")
	}
}

func TestAVerifierNeedsAnIssuerAndAnAudience(t *testing.T) {
	cases := map[string]access.VerifierOptions{
		"no issuer":             {Audiences: []string{audience}},
		"no audience":           {Issuers: []string{issuer}},
		"an empty audience":     {Issuers: []string{issuer}, Audiences: []string{audience, ""}},
		"an empty issuer":       {Issuers: []string{"/"}, Audiences: []string{audience}},
		"an issuer twice":       {Issuers: []string{issuer, issuer + "/"}, Audiences: []string{audience}},
		"no issuer, no nothing": {},
	}
	for name, o := range cases {
		if v, err := access.NewVerifier(o); err == nil {
			t.Errorf("%s: built %+v", name, v)
		} else if !strings.Contains(err.Error(), "LECTIO_OIDC_") {
			t.Errorf("%s: the error names no variable: %v", name, err)
		}
	}
}
