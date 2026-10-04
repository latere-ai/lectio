// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/lectio/internal/fault"
)

// ClockSkew is how far an issuer's clock may differ from this server's: a
// token is read until this long past its exp and from this long before its
// nbf.
const ClockSkew = 30 * time.Second

// fetchTimeout bounds one read of an issuer's discovery document or key
// set.
const fetchTimeout = 10 * time.Second

// VerifierOptions configures a Verifier. Issuers and Audiences are
// LECTIO_OIDC_ISSUERS and LECTIO_OIDC_AUDIENCE.
type VerifierOptions struct {
	// Issuers are the issuers whose tokens are accepted. Each one's key
	// set is discovered from the issuer itself.
	Issuers []string
	// Audiences are the audiences a token may be addressed to. The first
	// is the primary one.
	Audiences []string
	// HTTP reads the discovery documents and the key sets. Nil takes a
	// client that traces its calls and trusts the system's roots.
	HTTP *http.Client
	// Now is the clock. Nil takes time.Now.
	Now func() time.Time
}

// Verifier verifies a bearer against the listed issuers. Each issuer
// answers for its own tokens alone: the token's iss selects the key set
// its signature is checked against.
//
// Nothing here calls an issuer for anything but its discovery document
// and its key set. The shared verifier caches each set, refreshes it when
// a token names a key it does not hold, and serves the cached set while a
// refresh fails.
type Verifier struct {
	issuers   []string
	audiences []string
	validator *jwt.Validator
}

// NewVerifier builds the verifier. It reads nothing from the network:
// Warm does, for a server that wants an unreachable issuer known at start.
func NewVerifier(o VerifierOptions) (*Verifier, error) {
	if len(o.Issuers) == 0 {
		return nil, errors.New("LECTIO_OIDC_ISSUERS names no issuer")
	}
	if len(o.Audiences) == 0 || slices.Contains(o.Audiences, "") {
		// A verifier with no audience would accept a token addressed to
		// any other service of the same issuer.
		return nil, errors.New("LECTIO_OIDC_AUDIENCE names no audience")
	}
	v := &Verifier{audiences: slices.Clone(o.Audiences)}
	for _, raw := range o.Issuers {
		issuer := strings.TrimRight(raw, "/")
		if issuer == "" || slices.Contains(v.issuers, issuer) {
			return nil, errors.New("LECTIO_OIDC_ISSUERS has an empty entry or lists an issuer twice")
		}
		v.issuers = append(v.issuers, issuer)
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout, Transport: otel.Transport(nil)}
	}
	v.validator = jwt.New(jwt.Config{
		Issuers:    v.issuers,
		Audiences:  v.audiences,
		HTTPClient: client,
		ClockSkew:  ClockSkew,
		Now:        o.Now,
		// A personal access token carries the grants its holder chose, and
		// the shared verifier refuses one unless whoever reads the token
		// promises to apply them. The promise is kept where the decision
		// is made: the owner policy narrows its answer by the grants, and
		// an authorizer built on the shared scaffold does the same.
		ReadsGrants: true,
	})
	return v, nil
}

// Issuers lists the issuers this verifier accepts, in configured order.
func (v *Verifier) Issuers() []string { return slices.Clone(v.issuers) }

// Audience is the primary audience, the first one configured.
func (v *Verifier) Audience() string { return v.audiences[0] }

// Warm reads every issuer's key set once, so the first request does not
// pay for the fetch. The error names each issuer that did not answer. The
// verifier works either way and reads the set again when a token of that
// issuer arrives.
func (v *Verifier) Warm(ctx context.Context) error { return v.validator.Warm(ctx) }

// Authenticator is the same validator behind the interface the shared
// conformance suite drives. It is the validator lectiod runs, not a second
// one built for a test.
func (v *Verifier) Authenticator() *jwt.Authenticator { return jwt.NewAuthenticator(v.validator) }

// Authenticate reads the bearer of a request and verifies it. There is no
// anonymous access.
func (v *Verifier) Authenticate(r *http.Request) (Caller, error) {
	raw, ok := bearer.FromRequest(r)
	if !ok || raw == "" {
		return Caller{}, missingToken()
	}
	return v.Verify(raw)
}

// Verify verifies one bearer: its signature against the key set of the
// issuer it names, iss among the listed issuers, aud among the audiences,
// and exp and nbf within the skew. A token that carries iat is also held
// to the shared verifier's age bound.
func (v *Verifier) Verify(raw string) (Caller, error) {
	verified, err := v.validator.Validate(raw)
	if err != nil {
		// The detail is the reason word every service on the shared
		// verifier writes, and never a part of the token.
		reason := string(jwt.ReasonOf(err))
		if reason == "" {
			reason = "refused"
		}
		return Caller{}, fault.Wrap(fault.InvalidToken, err, "the bearer token was refused: %s", reason)
	}
	// Every claim, as the issuer wrote it, for the authorizer to read. The
	// token is verified, so this decodes what the signature covers.
	var claims map[string]any
	if err := jwt.DecodePayload(raw, &claims); err != nil {
		return Caller{}, fault.Wrap(fault.InvalidToken, err, "the bearer token was refused: malformed")
	}
	issuer := strings.TrimRight(verified.Iss, "/")
	return Caller{Subject: authz.Subject(issuer, verified.Sub), Issuer: issuer, Sub: verified.Sub, Claims: claims}, nil
}

// missingToken is the refusal of a request that carries no bearer.
func missingToken() error {
	return fault.New(fault.MissingToken, "the request carries no bearer token")
}
