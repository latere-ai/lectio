// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"errors"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/reader"
)

// How a server tells who is calling.
const (
	// IdentityOIDC verifies a token against the listed issuers.
	IdentityOIDC = "oidc"
	// IdentityDevelopment takes the one static token of a development
	// server.
	IdentityDevelopment = "development token"
)

// Who decides what a caller may do.
const (
	// AuthorizationEndpoint asks the endpoint LECTIO_AUTHORIZER_URL names.
	AuthorizationEndpoint = "authorizer"
	// AuthorizationOwnerPolicy decides in process. It is the phrase
	// lectiod logs at start.
	AuthorizationOwnerPolicy = "owner policy"
)

// Access is what a server's handlers depend on, and which of the modes is
// in force behind each interface.
type Access struct {
	Authenticator Authenticator
	Authorizer    Authorizer

	// Identity and Authorization name the mode behind each, for the line
	// a server logs at start.
	Identity      string
	Authorization string

	verifier *Verifier
	decider  authz.Authorizer
}

// New builds the identity of a server from its settings.
//
// Who is calling: with LECTIO_OIDC_ISSUERS set, a verifier over those
// issuers and LECTIO_OIDC_AUDIENCE, whether or not the server is a
// development one. With none set, a development server takes its one
// static token, and any other server is refused: there is no anonymous
// access.
//
// Who decides: with LECTIO_AUTHORIZER_URL set, that endpoint, asked with
// LECTIO_AUTHORIZER_TOKEN. With none, the owner policy, with
// LECTIO_ADMIN_SUBJECTS as its admins.
func New(s config.Settings) (*Access, error) {
	a := &Access{}

	switch {
	case len(s.OIDCIssuers) > 0:
		v, err := NewVerifier(VerifierOptions{Issuers: s.OIDCIssuers, Audiences: s.OIDCAudiences})
		if err != nil {
			return nil, err
		}
		a.verifier, a.Authenticator, a.Identity = v, v, IdentityOIDC
	case s.Dev:
		token, err := NewStaticToken(reader.NewCredential(s.DevToken), DevSubject)
		if err != nil {
			return nil, err
		}
		a.Authenticator, a.Identity = token, IdentityDevelopment
	default:
		return nil, errors.New("LECTIO_OIDC_ISSUERS is not set. A server verifies its callers against an issuer; LECTIO_DEV=true takes one static token and no issuer")
	}

	if s.AuthorizerURL != "" {
		client, err := NewClient(ClientOptions{URL: s.AuthorizerURL, Token: s.AuthorizerToken})
		if err != nil {
			return nil, err
		}
		a.decider, a.Authorization = client, AuthorizationEndpoint
	} else {
		a.decider, a.Authorization = &OwnerPolicy{Admins: s.AdminSubjects}, AuthorizationOwnerPolicy
	}
	// A default the server cannot hold would be passed in silence on every
	// request, so it is refused here, at start.
	lost := unenforced(s)
	if g := s.GroupDefaults; len(lost) > 0 && (g.MaxRunning > 0 || g.MaxQueued > 0 || g.PagesPerDay > 0) {
		return nil, errors.New("LECTIO_GROUP_DEFAULTS sets max_running, max_queued or pages_per_day, which a development server does not hold: it has one queue for every caller and no group")
	}
	a.Authorizer = NewAuthorizer(a.decider, Defaults(s), FileRetention(s.FileRetention), Unenforced(lost...))
	return a, nil
}

// unenforced are the limits the server these settings start cannot hold a
// request to, by their wire names. An allow that sets one is refused with
// capability_unsupported. The durable server holds every member of the
// limits. A development server keeps everything in its process and has one
// queue for every caller: it has no group to bound and no day to budget,
// and nothing it holds expires.
func unenforced(s config.Settings) []string {
	if s.Dev {
		return []string{"max_running", "max_queued", "pages_per_day", "retention_seconds"}
	}
	return nil
}

// Defaults are the limits a server is configured with, which an allow
// lays its own over and the owner policy applies as they are: the bounds
// on a file and on a parse, what a group takes when its allow names none,
// and how long a parse is kept. A file's retention is laid under the allow
// of an upload.
func Defaults(s config.Settings) authorizer.Limits {
	g := s.GroupDefaults
	return authorizer.Limits{
		Weight: g.Weight, MaxRunning: g.MaxRunning, MaxQueued: g.MaxQueued, MaxPriority: g.MaxPriority, PagesPerDay: g.PagesPerDay,
		MaxFileBytes: s.MaxFileBytes, MaxPages: s.MaxPages, Retention: s.ParseRetention,
	}
}

// Warm reads every issuer's key set once, so the first request does not
// pay for the fetch, and names each issuer that did not answer. A server
// that takes the development token has nothing to read.
func (a *Access) Warm(ctx context.Context) error {
	if a.verifier == nil {
		return nil
	}
	return a.verifier.Warm(ctx)
}

// Check sends the probe every authorizer denies. It reports an endpoint
// that did not answer and one that allowed the probe, which is an
// endpoint that does not read the request.
func (a *Access) Check(ctx context.Context) error { return Check(ctx, a.decider) }
