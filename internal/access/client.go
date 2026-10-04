// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package access

import (
	"net/http"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/reader"
)

// ClientOptions configures the client that asks an operator's endpoint.
// URL and Token are LECTIO_AUTHORIZER_URL and LECTIO_AUTHORIZER_TOKEN.
type ClientOptions struct {
	URL   string
	Token reader.Credential
	// HTTP sends the calls. Nil takes a client that traces its calls and
	// trusts the system's roots: a certificate authority of the
	// operator's own is added there, and no setting of Lectio names one.
	HTTP *http.Client
	// Now is the clock the cache runs on. Nil takes time.Now.
	Now func() time.Time
}

// NewClient builds the authorizer client. It sends nothing. The rules are
// the shared contract's and are not restated here: an allow is cached for
// its ttl, a deny briefly, and an answer that is no decision never; a call
// whose connection failed is tried once more; and anything but a
// well-formed answer is no decision. The vocabulary rides along, so an
// action outside Lectio's table costs no round trip.
func NewClient(o ClientOptions) (*authz.Client, error) {
	client := o.HTTP
	if client == nil {
		// The contract's timeout bounds a call through the request's
		// context, so the client sets none of its own.
		client = &http.Client{Transport: otel.Transport(nil)}
	}
	return authz.NewClient(authz.Options{
		URL: o.URL, Token: o.Token.Reveal(), HTTP: client, Now: o.Now,
		Vocabulary: authorizer.Vocabulary(),
	})
}
