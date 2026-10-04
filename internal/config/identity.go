// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"net/url"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/reader"
)

// DefaultOIDCAudience is the audience a token must be addressed to when
// LECTIO_OIDC_AUDIENCE names none.
const DefaultOIDCAudience = "lectio"

// readIdentity reads the settings of specs/012-identity-and-authorization.md:
// whose tokens are accepted, and who decides what a caller may do. It
// reads and checks each variable on its own. Which of them a server needs
// depends on how it runs, and is decided where the identity is built.
func (s *Settings) readIdentity(getenv func(string) string) []error {
	var errs []error

	for _, entry := range list(getenv("LECTIO_OIDC_ISSUERS")) {
		issuer := strings.TrimRight(entry, "/")
		switch {
		case !absolute(issuer):
			errs = append(errs, errors.New("LECTIO_OIDC_ISSUERS has an entry that is not an absolute http or https URL"))
		case slices.Contains(s.OIDCIssuers, issuer):
			errs = append(errs, errors.New("LECTIO_OIDC_ISSUERS lists an issuer twice"))
		default:
			s.OIDCIssuers = append(s.OIDCIssuers, issuer)
		}
	}

	for _, audience := range list(getenv("LECTIO_OIDC_AUDIENCE")) {
		if slices.Contains(s.OIDCAudiences, audience) {
			errs = append(errs, errors.New("LECTIO_OIDC_AUDIENCE lists an audience twice"))
			continue
		}
		s.OIDCAudiences = append(s.OIDCAudiences, audience)
	}
	if len(s.OIDCAudiences) == 0 {
		s.OIDCAudiences = []string{DefaultOIDCAudience}
	}

	s.AuthorizerURL = strings.TrimSpace(getenv("LECTIO_AUTHORIZER_URL"))
	if token := strings.TrimSpace(getenv("LECTIO_AUTHORIZER_TOKEN")); token != "" {
		s.AuthorizerToken = reader.NewCredential(token)
	}
	if s.AuthorizerURL != "" {
		if !absolute(s.AuthorizerURL) {
			errs = append(errs, errors.New("LECTIO_AUTHORIZER_URL is not an absolute http or https URL"))
		}
		if s.AuthorizerToken.IsZero() {
			errs = append(errs, errors.New("LECTIO_AUTHORIZER_TOKEN is not set while LECTIO_AUTHORIZER_URL is: the authorizer requires a bearer"))
		}
	}

	for _, subject := range list(getenv("LECTIO_ADMIN_SUBJECTS")) {
		// A subject is the issuer and the sub joined by a pipe. A bare
		// sub would match no verified caller, so it is refused here and
		// does not sit in the list doing nothing.
		if issuer, sub, ok := authz.SplitSubject(subject); !ok || issuer == "" || sub == "" {
			errs = append(errs, errors.New("LECTIO_ADMIN_SUBJECTS has an entry that is not of the form <issuer>|<sub>"))
			continue
		}
		s.AdminSubjects = append(s.AdminSubjects, subject)
	}
	return errs
}

// list reads a comma-separated variable, trimming each entry and dropping
// the empty ones.
func list(raw string) []string {
	var out []string
	for entry := range strings.SplitSeq(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// absolute reports whether raw is an http or https URL with a host.
func absolute(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
