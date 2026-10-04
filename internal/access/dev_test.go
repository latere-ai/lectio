// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/reader"
)

func TestTheDevelopmentTokenStandsForOneSubject(t *testing.T) {
	var auth access.Authenticator
	auth, err := access.NewStaticToken(reader.NewCredential("dev-secret"), access.DevSubject)
	if err != nil {
		t.Fatal(err)
	}
	c, err := auth.Authenticate(bearing(t, "dev-secret"))
	if err != nil {
		t.Fatalf("the token was refused: %v", err)
	}
	// The subject names no issuer, so no verified token can be it.
	if c.Subject != "dev" || c.Sub != "dev" || c.Issuer != "" || c.Claims == nil || len(c.Claims) != 0 {
		t.Errorf("the caller is %+v", c)
	}
	// The scheme is matched without regard to case.
	lower := bearing(t, "")
	lower.Header.Set("Authorization", "bearer dev-secret")
	if _, err := auth.Authenticate(lower); err != nil {
		t.Errorf("a lower-case scheme was refused: %v", err)
	}
	// One request's claims are not another's.
	c.Claims["changed"] = true
	if again, _ := auth.Authenticate(bearing(t, "dev-secret")); len(again.Claims) != 0 {
		t.Errorf("a second caller's claims are %v", again.Claims)
	}
}

func TestWhatTheDevelopmentTokenRefuses(t *testing.T) {
	auth, err := access.NewStaticToken(reader.NewCredential("dev-secret"), access.DevSubject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(bearing(t, "")); fault.CodeOf(err) != fault.MissingToken {
		t.Errorf("no bearer: %v, want missing_token", err)
	}
	for _, token := range []string{"dev-secre", "dev-secret-and-more", "another"} {
		_, err := auth.Authenticate(bearing(t, token))
		if fault.CodeOf(err) != fault.InvalidToken {
			t.Errorf("%q: %v, want invalid_token", token, err)
		}
		if err != nil && (strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "dev-secret")) {
			t.Errorf("the error repeats a token: %v", err)
		}
	}
	if shown := fmt.Sprintf("%+v %#v", auth, auth); strings.Contains(shown, "dev-secret") {
		t.Errorf("the authenticator prints its token: %s", shown)
	}
}

// An empty token would equal the bearer of a request that sends none.
func TestADevelopmentTokenIsNeverEmpty(t *testing.T) {
	if auth, err := access.NewStaticToken(reader.Credential{}, access.DevSubject); err == nil {
		t.Errorf("built %+v with an empty token", auth)
	}
	if auth, err := access.NewStaticToken(reader.NewCredential("t"), ""); err == nil {
		t.Errorf("built %+v for no subject", auth)
	}
}

// The development caller is decided about like any other: under the owner
// policy it owns what it creates and nothing else.
func TestTheDevelopmentCallerUnderTheOwnerPolicy(t *testing.T) {
	auth, err := access.NewStaticToken(reader.NewCredential("dev"), access.DevSubject)
	if err != nil {
		t.Fatal(err)
	}
	c, err := auth.Authenticate(bearing(t, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	az := access.NewAuthorizer(&access.OwnerPolicy{}, configured())
	d, err := az.Authorize(t.Context(), c, submit(""))
	if err != nil || !d.Allow {
		t.Fatalf("a submit: %+v, %v", d, err)
	}
	want := configured()
	want.Owner, want.Group = "dev", "dev"
	if !reflect.DeepEqual(d.Limits, want) {
		t.Errorf("held to %+v, want %+v", d.Limits, want)
	}
	own := access.Question{Action: authorizer.ActionParseRead, Resource: access.Parse{ID: "prs_1", Owner: "dev"}.Resource()}
	if d, err := az.Authorize(t.Context(), c, own); err != nil || !d.Allow {
		t.Errorf("its own parse: %+v, %v", d, err)
	}
	if d, err := az.Authorize(t.Context(), c, reading("prs_2", alice)); err != nil || d.Allow {
		t.Errorf("another's parse: %+v, %v", d, err)
	}
}
