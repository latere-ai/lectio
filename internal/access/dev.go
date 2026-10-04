// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"

	"latere.ai/x/pkg/bearer"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/reader"
)

// DevSubject is the subject the development token stands for. It names no
// issuer, so it can never be the subject of a verified token.
const DevSubject = "dev"

// StaticToken is the Authenticator of a development server: one token
// that stands for one subject. It verifies nothing and keeps nothing, and
// exists so that a server with no issuer still has a caller to scope by.
//
// It holds a digest of the token and not the token: printing a value that
// has the token in an unexported field would show it, since no formatter
// asks such a field how it wants to be shown.
type StaticToken struct {
	digest  [sha256.Size]byte
	subject string
}

// NewStaticToken accepts token as the bearer of subject. An empty token is
// refused: it would be equal to the bearer of a request that sends none.
func NewStaticToken(token reader.Credential, subject string) (*StaticToken, error) {
	if token.IsZero() {
		return nil, errors.New("LECTIO_DEV_TOKEN is empty")
	}
	if subject == "" {
		return nil, errors.New("the development token stands for no subject")
	}
	return &StaticToken{digest: sha256.Sum256([]byte(token.Reveal())), subject: subject}, nil
}

// Authenticate compares the request's bearer with the token. Both are
// digested first, so the comparison takes the same time whatever the
// bearer's length and wherever it first differs.
func (s *StaticToken) Authenticate(r *http.Request) (Caller, error) {
	raw, ok := bearer.FromRequest(r)
	if !ok || raw == "" {
		return Caller{}, missingToken()
	}
	if got := sha256.Sum256([]byte(raw)); subtle.ConstantTimeCompare(got[:], s.digest[:]) != 1 {
		return Caller{}, fault.New(fault.InvalidToken, "the bearer token is not known")
	}
	// A fresh map each time: a caller's claims are the request's own.
	return Caller{Subject: s.subject, Sub: s.subject, Claims: map[string]any{}}, nil
}

// String says what the authenticator is and shows no part of its token.
func (s StaticToken) String() string { return "the development token of " + s.subject }

// GoString is String, so %#v shows no digest either.
func (s StaticToken) GoString() string { return s.String() }
