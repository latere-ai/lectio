// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package authorizer is the vocabulary an authorization endpoint for
// Lectio is written against: the actions lectiod asks, the resource kind
// each one acts on, the fields a resource carries, and the limits an allow
// may carry. Import it to write the endpoint LECTIO_AUTHORIZER_URL points
// at, in Go, without keeping a copy of the strings.
//
// The envelope on the wire is latere.ai/x/pkg/authz's, and this package
// declares none of it: lectiod posts a request with the caller's subject,
// its claims, an action and a resource, and reads back an allow or a deny,
// all of it that package's types. Vocabulary is the whole action table as
// authz.Vocabulary, which the client refuses an unknown action against,
// latere.ai/x/pkg/authz/server validates against, and
// latere.ai/x/pkg/authz/conformance drives its cases from:
//
//	conformance.Run(t, url, token, conformance.WithVocabulary(authorizer.Vocabulary()))
//
// Every action answers a decision, parse.list included: its allow may
// carry a filter that narrows the list to owners and labels. Lectio names
// no action whose answer is a page of its own shape.
//
// A subject is the issuer and the sub joined, "https://issuer.example|alice",
// and the claims are the token's, verbatim. Lectio reads none of them: an
// endpoint reads a plan, a team or an organization from whichever claim its
// issuer stamps.
//
// The resource carries its members flat beside its kind and id, under the
// names of the Field constants; Fields lists the members each action may
// send. A member Lectio does not know for a request is absent, never
// empty, so an endpoint can tell a parse with no pinned reader from one
// whose reader is the empty string.
//
// An allow may carry limits. WireLimits is the limits object as the answer
// renders it, so an endpoint writes the type lectiod decodes:
//
//	limits := authorizer.WireLimits{Group: &group, MaxQueued: &hundred}
//
// A member the endpoint does not set is left out, and the server's
// configured default stays in force. DecodeLimits is the reading half,
// lectiod's own, and is here so an endpoint's tests can hold their answers
// to it: a member this version does not know and a figure out of range are
// both errors, and lectiod refuses the request the answer was for.
//
// The promise, as for every public package of this module: additive within
// a module major. An action string never changes and never disappears, a
// resource kind stays the kind it is, and a limits member keeps its wire
// name and its meaning. A new action is a new row in the table of
// specs/012-identity-and-authorization.md first and a constant here second.
package authorizer
