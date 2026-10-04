// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestIdentitySettingsHaveDefaults(t *testing.T) {
	s, err := FromEnv(env())
	if err != nil {
		t.Fatal(err)
	}
	if s.OIDCIssuers != nil || !slices.Equal(s.OIDCAudiences, []string{DefaultOIDCAudience}) ||
		s.AuthorizerURL != "" || !s.AuthorizerToken.IsZero() || s.AdminSubjects != nil {
		t.Fatalf("defaults: %+v", s)
	}
}

func TestIdentitySettingsAreReadFromTheEnvironment(t *testing.T) {
	s, err := FromEnv(env(
		"LECTIO_OIDC_ISSUERS", " https://issuer.example/ , ,http://127.0.0.1:9000/realms/dev",
		"LECTIO_OIDC_AUDIENCE", " parsing , lectio ",
		"LECTIO_AUTHORIZER_URL", " https://plane.example/authz/lectio ",
		"LECTIO_AUTHORIZER_TOKEN", " az-token ",
		"LECTIO_ADMIN_SUBJECTS", " https://issuer.example|root , ,https://issuer.example|ops",
	))
	if err != nil {
		t.Fatal(err)
	}
	// An issuer is kept without its trailing slash, so one issuer written
	// two ways is one issuer. The first audience is the primary one.
	if want := []string{"https://issuer.example", "http://127.0.0.1:9000/realms/dev"}; !slices.Equal(s.OIDCIssuers, want) {
		t.Errorf("issuers: %v, want %v", s.OIDCIssuers, want)
	}
	if want := []string{"parsing", "lectio"}; !slices.Equal(s.OIDCAudiences, want) {
		t.Errorf("audiences: %v, want %v", s.OIDCAudiences, want)
	}
	if s.AuthorizerURL != "https://plane.example/authz/lectio" || s.AuthorizerToken.Reveal() != "az-token" {
		t.Errorf("authorizer: %q", s.AuthorizerURL)
	}
	if want := []string{"https://issuer.example|root", "https://issuer.example|ops"}; !slices.Equal(s.AdminSubjects, want) {
		t.Errorf("admin subjects: %v, want %v", s.AdminSubjects, want)
	}
}

// The token is a credential: it shows as a placeholder wherever the
// settings are printed.
func TestTheAuthorizerTokenIsNeverPrinted(t *testing.T) {
	s, err := FromEnv(env("LECTIO_AUTHORIZER_URL", "https://plane.example/authz", "LECTIO_AUTHORIZER_TOKEN", "az-secret-token"))
	if err != nil {
		t.Fatal(err)
	}
	for _, shown := range []string{fmt.Sprint(s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		if strings.Contains(shown, "az-secret-token") {
			t.Fatalf("the settings print the token: %s", shown)
		}
	}
}

func TestAnIdentitySettingThatIsWrongNamesItsVariableAndNotItsValue(t *testing.T) {
	cases := []struct {
		name, variable string
		pairs          []string
	}{
		{"an issuer that is no URL", "LECTIO_OIDC_ISSUERS", []string{"LECTIO_OIDC_ISSUERS", "sk-secret-issuer"}},
		{"an issuer with no host", "LECTIO_OIDC_ISSUERS", []string{"LECTIO_OIDC_ISSUERS", "https:///sk-secret"}},
		{"an issuer of another scheme", "LECTIO_OIDC_ISSUERS", []string{"LECTIO_OIDC_ISSUERS", "ftp://sk-secret.example"}},
		{"an issuer that does not parse", "LECTIO_OIDC_ISSUERS", []string{"LECTIO_OIDC_ISSUERS", "https://sk-secret.example/%zz"}},
		{"an issuer listed twice", "LECTIO_OIDC_ISSUERS", []string{"LECTIO_OIDC_ISSUERS", "https://sk-secret.example,https://sk-secret.example/"}},
		{"an audience listed twice", "LECTIO_OIDC_AUDIENCE", []string{"LECTIO_OIDC_AUDIENCE", "sk-secret,sk-secret"}},
		{"an authorizer that is no URL", "LECTIO_AUTHORIZER_URL", []string{"LECTIO_AUTHORIZER_URL", "sk-secret-url", "LECTIO_AUTHORIZER_TOKEN", "t"}},
		{"an authorizer with no token", "LECTIO_AUTHORIZER_TOKEN", []string{"LECTIO_AUTHORIZER_URL", "https://sk-secret.example/authz"}},
		{"an admin that is a bare sub", "LECTIO_ADMIN_SUBJECTS", []string{"LECTIO_ADMIN_SUBJECTS", "sk-secret-root"}},
		{"an admin with no issuer", "LECTIO_ADMIN_SUBJECTS", []string{"LECTIO_ADMIN_SUBJECTS", "|sk-secret-root"}},
		{"an admin with no sub", "LECTIO_ADMIN_SUBJECTS", []string{"LECTIO_ADMIN_SUBJECTS", "https://sk-secret.example|"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := FromEnv(env(c.pairs...))
			if err == nil {
				t.Fatalf("no error; settings: %+v", s)
			}
			if !strings.Contains(err.Error(), c.variable) {
				t.Errorf("the error does not name %s: %v", c.variable, err)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Errorf("the error repeats a value: %v", err)
			}
		})
	}
}

// What is wrong is left out of the settings, so nothing built from them
// trusts an issuer or an admin that was refused.
func TestARefusedEntryIsNotKept(t *testing.T) {
	s, err := FromEnv(env(
		"LECTIO_OIDC_ISSUERS", "https://issuer.example,not-a-url",
		"LECTIO_ADMIN_SUBJECTS", "https://issuer.example|root,bare",
	))
	if err == nil {
		t.Fatal("no error")
	}
	if !slices.Equal(s.OIDCIssuers, []string{"https://issuer.example"}) || !slices.Equal(s.AdminSubjects, []string{"https://issuer.example|root"}) {
		t.Errorf("kept issuers %v and admins %v", s.OIDCIssuers, s.AdminSubjects)
	}
}

// A token with no URL is not an error: the owner policy decides, and the
// token is read and unused.
func TestAnAuthorizerTokenWithNoURL(t *testing.T) {
	s, err := FromEnv(env("LECTIO_AUTHORIZER_TOKEN", "az-token"))
	if err != nil {
		t.Fatal(err)
	}
	if s.AuthorizerURL != "" || s.AuthorizerToken.Reveal() != "az-token" {
		t.Errorf("settings: %+v", s)
	}
}
