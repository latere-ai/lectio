// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package access is lectiod's side of
// specs/012-identity-and-authorization.md: who a caller is, and who
// decides what that caller may do.
//
// Who is an Authenticator. A bearer is verified against one of the issuers
// LECTIO_OIDC_ISSUERS lists, through latere.ai/x/pkg/authkit/jwt, and
// becomes a Caller: the rendered subject, its two halves apart, and every
// claim of the token verbatim. No claim is read for meaning here: an
// issuer's organization or group claim means something to the authorizer
// that reads it and nothing to Lectio. A development server takes one
// static token through the same interface.
//
// What is an Authorizer. One question per request, in the vocabulary of
// latere.ai/x/lectio/authorizer, goes to the endpoint LECTIO_AUTHORIZER_URL
// names through the client of latere.ai/x/pkg/authz, which caches, retries
// and fails closed by that contract's rules. With no endpoint configured
// the owner policy of this package answers. A handler cannot tell which is
// in force.
//
// A handler is written in four steps: authenticate, build the resource
// from the request and the stored object, ask, and act within the
// decision. Routes says which action each route of the contract asks and
// which fields its resource carries.
package access

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lectio/authorizer"
	"latere.ai/x/lectio/internal/fault"
)

// Caller is a verified bearer. Subject is the one string an owner field,
// an event and an authorizer request carry: the issuer and the sub joined
// by a pipe. Claims is every claim of the token, verbatim. It is handed to
// the authorizer as it came and read by nothing in Lectio.
type Caller struct {
	Subject string
	Issuer  string
	Sub     string
	Claims  map[string]any
}

// Authenticator turns a request's bearer into a caller.
type Authenticator interface {
	// Authenticate verifies the request's bearer. A request that carries
	// none fails with fault.MissingToken, and one whose bearer is refused
	// with fault.InvalidToken.
	Authenticate(r *http.Request) (Caller, error)
}

// Authorizer answers one question about one caller.
type Authorizer interface {
	// Authorize asks whether the caller may do the question's action to
	// its resource. A deny is a Decision and not an error. An error is a
	// question that got no decision, and the request fails closed with
	// it: fault.AuthorizerUnavailable when the authorizer did not answer
	// or answered what cannot be read, and fault.CapabilityUnsupported
	// when its allow carries a limit this server does not know.
	Authorize(ctx context.Context, caller Caller, q Question) (Decision, error)
}

// Question is what a handler asks: the action its route is given in
// Routes, the resource built from the request and the stored object, and
// what the authorizer learns about the request itself.
type Question struct {
	Action   string
	Resource authz.Resource
	Request  authz.Caller
}

// Ask builds the question a request asks.
func Ask(r *http.Request, action string, resource authz.Resource) Question {
	return Question{Action: action, Resource: resource, Request: RequestInfo(r)}
}

// RequestInfo is what the authorizer is told about the request: its id
// when the request carries one, the peer address as the server sees it,
// and the user agent. The id is for correlation alone. The address and
// the user agent are inputs a policy may decide from.
func RequestInfo(r *http.Request) authz.Caller {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// An address with no port is already the host.
		host = r.RemoteAddr
	}
	return authz.Caller{ID: r.Header.Get("X-Request-Id"), IP: host, UserAgent: r.UserAgent()}
}

// Decision is the answer to a question.
type Decision struct {
	// Allow says whether the caller may. Nothing else is set on a deny
	// but Reason.
	Allow bool
	// Reason is the authorizer's own word for its answer. On a deny it is
	// the developer detail of the refusal and never the sentence a person
	// reads.
	Reason string
	// Limits are what the request is held to: the server's defaults with
	// what the allow named laid over them. Owner and Group are always
	// set. They are read from the allow of parse.create and of
	// file.create, and are the defaults for every other action. On a
	// question about a stored object, Owner is that object's owner.
	Limits authorizer.Limits
	// Filter narrows a list to the owners and labels the caller may see.
	// Nil narrows nothing. It is set on parse.list, and on a usage or
	// queue read that names no owner or group.
	Filter *authz.Filter
}

// Err is nil for an allow and the refusal of a deny: fault.Forbidden with
// the authorizer's reason as its detail.
func (d Decision) Err() error {
	return d.Or(fault.New(fault.Forbidden, "the authorizer denied the request: %s", d.Reason))
}

// Or is nil for an allow and refused for a deny. A read of a stored object
// passes the error it answers a missing object with, so that a refused
// object and a missing one are one answer and an id cannot be probed for
// what somebody else owns.
func (d Decision) Or(refused error) error {
	if d.Allow {
		return nil
	}
	return refused
}

// asker asks one authz.Authorizer, the shared client over the operator's
// endpoint or the owner policy, and reads its answer into Lectio's codes
// and limits. The cache, the retry and the timeout are the client's.
// Nothing here decides.
type asker struct {
	inner    authz.Authorizer
	defaults authorizer.Limits

	// fileRetention is the Retention of the defaults for an upload, when
	// it is set: a file and a parse are kept for different times.
	fileRetention time.Duration
	// unenforced are the wire names of the limits this server cannot hold.
	unenforced []string
}

// Option says what the server behind an authorizer holds a request to.
type Option func(*asker)

// FileRetention sets how long a file is kept when its allow names no
// shorter time. The Retention of the defaults is a parse's.
func FileRetention(d time.Duration) Option {
	return func(a *asker) { a.fileRetention = d }
}

// Unenforced names members of the limits object, by their wire names, that
// the server cannot hold a request to. An allow in which one of them would
// change the limits in force is refused with capability_unsupported: a
// limit the server is handed and cannot enforce is never passed in
// silence. A member that changes nothing, such as a cap of zero or a
// ceiling above the server's own, is no limit and is not refused.
func Unenforced(members ...string) Option {
	return func(a *asker) { a.unenforced = append(a.unenforced, members...) }
}

// NewAuthorizer wraps whichever authorizer a server runs. defaults are
// the server's configured limits, which an allow lays its own over.
func NewAuthorizer(inner authz.Authorizer, defaults authorizer.Limits, opts ...Option) Authorizer {
	a := &asker{inner: inner, defaults: defaults}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *asker) Authorize(ctx context.Context, caller Caller, q Question) (Decision, error) {
	// A question outside the vocabulary, or about another kind than its
	// action acts on, is a mistake in a handler. It is refused before it
	// is sent, so the mistake cannot be answered with an allow.
	if kind := authorizer.Kind(q.Action); kind == "" || kind != q.Resource.Kind {
		return Decision{}, fault.New(fault.Internal, "the action %q does not act on the kind %q", q.Action, q.Resource.Kind)
	}
	d, err := a.inner.Authorize(ctx, envelope(caller, q))
	if err != nil {
		// The detail is fixed: the error underneath may name the
		// endpoint, and is for the log and not for the caller.
		return Decision{}, fault.Wrap(fault.AuthorizerUnavailable, err, "the question %s got no decision", q.Action)
	}
	if !d.Allow {
		return Decision{Reason: reasonOf(d)}, nil
	}
	limits, err := a.limits(caller, q, d)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Allow: true, Reason: d.Reason, Limits: limits, Filter: d.Filter}, nil
}

// limits are the limits in force after an allow. The defaults name the
// owner the request asked for, or the caller when it asked for none, and
// the group is the owner unless the allow names one.
func (a *asker) limits(caller Caller, q Question, d authz.Decision) (authorizer.Limits, error) {
	defaults := a.defaults
	defaults.Owner = q.Resource.String(authorizer.FieldOwner)
	if defaults.Owner == "" {
		defaults.Owner = caller.Subject
	}
	if q.Action == authorizer.ActionFileCreate && a.fileRetention > 0 {
		defaults.Retention = a.fileRetention
	}

	var named authorizer.WireLimits
	if q.Action == authorizer.ActionParseCreate || q.Action == authorizer.ActionFileCreate {
		var err error
		if named, err = authorizer.DecodeLimits(d); err != nil {
			if unknown, ok := errors.AsType[*authorizer.UnknownLimit](err); ok {
				return authorizer.Limits{}, fault.Wrap(fault.CapabilityUnsupported, err,
					"the allow of %s carries a limit this server does not enforce: %s", q.Action, strings.Join(unknown.Members, ", "))
			}
			// A ceiling the server cannot read is not one it can hold, so
			// the answer is no decision at all.
			return authorizer.Limits{}, fault.Wrap(fault.AuthorizerUnavailable, err, "the allow of %s carries limits that cannot be read", q.Action)
		}
	}
	if q.Resource.ID != "" {
		// A question about a stored parse moves it to nobody: a retry, a
		// figure run and an extraction stay its owner's, whatever owner
		// the allow names.
		named.Owner = nil
	}
	if q.Action == authorizer.ActionFileCreate {
		// A file joins no queue and reads no page, so an upload is held
		// to these four and to nothing else the allow names.
		named = authorizer.WireLimits{
			Owner: named.Owner, Group: named.Group,
			MaxFileBytes: named.MaxFileBytes, RetentionSeconds: named.RetentionSeconds,
		}
	}
	limits := named.Over(defaults)
	var lost []string
	for _, member := range a.unenforced {
		if sets(limits, without(named, member).Over(defaults)) {
			lost = append(lost, member)
		}
	}
	if len(lost) > 0 {
		return authorizer.Limits{}, fault.New(fault.CapabilityUnsupported,
			"the allow of %s carries a limit this server does not enforce: %s", q.Action, strings.Join(lost, ", "))
	}
	if limits.Group == "" {
		limits.Group = limits.Owner
	}
	return limits, nil
}

// sets reports whether a holds a limit that b does not: a member that is
// set in a and differs in b. A member that is zero in a holds nothing, so
// an allow that lifts a cap sets none.
func sets(a, b authorizer.Limits) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		if !va.Field(i).IsZero() && !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			return true
		}
	}
	return false
}

// without is the limits object with one member not named. A name the
// object does not have changes nothing.
func without(w authorizer.WireLimits, member string) authorizer.WireLimits {
	v := reflect.ValueOf(&w).Elem()
	for i := range v.NumField() {
		if name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ","); name == member {
			v.Field(i).SetZero()
		}
	}
	return w
}

// envelope is what one call carries: the caller's subject and its claims
// verbatim, the action, the resource, and what is known of the request.
func envelope(c Caller, q Question) authz.Request {
	claims := c.Claims
	if claims == nil {
		claims = map[string]any{}
	}
	return authz.Request{
		Subject: c.Subject, Issuer: c.Issuer, Sub: c.Sub, Claims: claims,
		Action: q.Action, Resource: q.Resource, Request: q.Request,
	}
}

// reasonOf is the authorizer's reason, or a word when it named none, so a
// refusal always says something.
func reasonOf(d authz.Decision) string {
	if d.Reason == "" {
		return "no reason given"
	}
	return d.Reason
}

// Check sends the probe every authorizer denies and reports one that
// answered nothing or allowed it. An allow is an endpoint that does not
// read the request.
func Check(ctx context.Context, a authz.Authorizer) error {
	return authz.Check(ctx, a, authorizer.ActionParseRead, authorizer.KindParse)
}
