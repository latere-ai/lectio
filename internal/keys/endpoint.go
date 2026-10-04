// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/lectio/reader"
)

// What the endpoint source runs with. Each value is stated here and named
// by specs/013-limits-and-usage.md.
const (
	// Margin is how long before its expiry a key is last handed to a call.
	// A gateway checks a key when a call arrives, so the margin covers what
	// lies between handing the key over and that arrival: rendering the
	// page, and the difference between this clock and the one that set the
	// expiry. An answer whose key expires within the margin is unusable and
	// is treated as no key.
	Margin = time.Minute

	// Timeout bounds one request to the endpoint.
	Timeout = 10 * time.Second

	// RefusalTTL is how long a refusal is answered from memory. It is short
	// so that a group whose budget was raised, or which was admitted, reads
	// again soon, and long enough that the queued pages of a refused group
	// fail on one request and not on one each.
	RefusalTTL = 5 * time.Second

	// RetryMin is how long the source waits before it asks again after an
	// answer that issued no key and was no refusal. The wait doubles with
	// each such answer in a row for the group and stops at RetryMax, which
	// also bounds a wait the endpoint names in Retry-After.
	RetryMin = time.Second
	RetryMax = 30 * time.Second

	// maxAnswerBytes bounds what is read of an answer: a key and a time.
	maxAnswerBytes = 64 << 10
)

// Options configures the source that asks an operator's endpoint.
type Options struct {
	// URL and Token are LECTIO_KEYS_URL and LECTIO_KEYS_TOKEN: where a
	// group's key is asked, and the bearer the endpoint requires. Both are
	// checked where they are read, in internal/config.
	URL   string
	Token reader.Credential

	// HTTP sends the requests. Nil takes a client that traces its calls and
	// trusts the system's roots.
	HTTP *http.Client

	// Now is the clock the memory runs on. Nil takes time.Now.
	Now func() time.Time

	// Log takes one line per answer that issued no key. Nil takes
	// slog.Default.
	Log *slog.Logger
}

// Endpoint is the source that asks an operator's endpoint for the key of
// each group and holds the answer in memory:
//
//   - a key, until Margin before it expires;
//   - a refusal, for RefusalTTL;
//   - anything else, until the wait before the next request has passed.
//
// One request per group is in flight at a time. Calls for the group that
// arrive meanwhile wait for its answer and make no request of their own.
type Endpoint struct {
	url     string
	token   reader.Credential
	http    *http.Client
	now     func() time.Time
	log     *slog.Logger
	timeout time.Duration

	mu     sync.Mutex
	groups map[string]*entry
	// swept is when entries that stand no longer were last dropped.
	swept time.Time
}

// entry is what the source holds of one group: the last answer, until when
// it stands, and the request in flight when there is one.
type entry struct {
	key reader.Credential
	// err is nil beside a key, and otherwise a refusal or an *Unavailable.
	err   error
	until time.Time
	// failures counts the answers in a row that issued no key and were no
	// refusal. It sets the wait before the next request.
	failures int
	// flight is closed when the request in flight has its answer, and nil
	// when none is in flight.
	flight chan struct{}
}

// NewEndpoint builds the source. It sends nothing: a group's key is asked
// when its first page is read.
func NewEndpoint(o Options) *Endpoint {
	e := &Endpoint{
		url: o.URL, token: o.Token, http: o.HTTP, now: o.Now, log: o.Log,
		timeout: Timeout, groups: map[string]*entry{},
	}
	if e.http == nil {
		// The timeout bounds a request through its context, so the client
		// sets none of its own.
		e.http = &http.Client{Transport: otel.Transport(nil)}
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	return e
}

// String is what an Endpoint prints as. The bearer and the keys it holds
// are members a formatter would otherwise reach, so it names its kind and
// nothing it holds.
func (e *Endpoint) String() string { return "keys.Endpoint" }

// GoString is String, for the verb that prints a value as Go source.
func (e *Endpoint) GoString() string { return e.String() }

// Key returns the key of a group: the one in memory while it stands, and
// otherwise the answer of a request, which every call for the group that
// arrives while it is in flight shares. A call whose context ends while it
// waits returns at once and leaves the request to the others.
func (e *Endpoint) Key(ctx context.Context, group, owner, parse string) (reader.Credential, error) {
	for {
		e.mu.Lock()
		g := e.groups[group]
		if g == nil {
			g = &entry{}
			e.groups[group] = g
		}
		if now := e.now(); now.Before(g.until) {
			key, err := g.answer(now)
			e.mu.Unlock()
			return key, err
		}
		if g.flight == nil {
			g.flight = make(chan struct{})
			// The request outlives the call that started it: the others
			// that wait for its answer are not ended by that call's end.
			go e.ask(context.WithoutCancel(ctx), g, group, owner, parse)
		}
		flight := g.flight
		e.mu.Unlock()

		select {
		case <-flight:
		case <-ctx.Done():
			return reader.Credential{}, &Unavailable{Reason: "the call ended while the group's key was asked for", Err: ctx.Err()}
		}
	}
}

// answer is what the entry holds, for a call at now. A wait is reported with
// what is left of it.
func (g *entry) answer(now time.Time) (reader.Credential, error) {
	if u, ok := errors.AsType[*Unavailable](g.err); ok {
		return reader.Credential{}, &Unavailable{RetryAfter: g.until.Sub(now), Reason: u.Reason, Err: u.Err}
	}
	return g.key, g.err
}

// ask makes the one request in flight for a group and records its answer.
func (e *Endpoint) ask(ctx context.Context, g *entry, group, owner, parse string) {
	r := e.request(ctx, group, owner, parse)

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if r.down == nil && r.refusal == nil && !now.Before(r.expires.Add(-Margin)) {
		r.down = &Unavailable{Reason: "the key the endpoint issued is expired or expires within the margin a key is used in"}
	}
	switch {
	case r.down != nil:
		g.failures++
		wait := min(RetryMax, RetryMin<<min(g.failures-1, 5))
		if r.down.RetryAfter > 0 {
			wait = min(r.down.RetryAfter, RetryMax)
		}
		g.key, g.err, g.until = reader.Credential{}, r.down, now.Add(wait)
		e.log.WarnContext(ctx, "the key endpoint issued no key; the group's pages wait", "group", group,
			"error", r.down, "answers_in_a_row", g.failures, "asked_again_in", wait)
	case r.refusal != nil:
		g.key, g.err, g.until, g.failures = reader.Credential{}, r.refusal, now.Add(RefusalTTL), 0
		e.log.InfoContext(ctx, "the key endpoint refused the group a key", "group", group, "error", r.refusal)
	default:
		g.key, g.err, g.until, g.failures = r.key, nil, r.expires.Add(-Margin), 0
	}
	close(g.flight)
	g.flight = nil
	e.forget(now)
}

// forget drops what is held of groups whose answer stopped standing more
// than RetryMax ago, so the memory follows the groups that are being read
// and not every group that ever was. It runs at most once per RetryMax. An
// entry is kept that long past its answer because its count of failures is
// what the next wait is computed from.
func (e *Endpoint) forget(now time.Time) {
	if now.Sub(e.swept) < RetryMax {
		return
	}
	e.swept = now
	for group, g := range e.groups {
		if g.flight == nil && now.Sub(g.until) > RetryMax {
			delete(e.groups, group)
		}
	}
}

// question is the body of a request: whose key is asked for.
type question struct {
	Group string `json:"group"`
	Owner string `json:"owner"`
	Parse string `json:"parse"`
}

// issued is the body of a 200.
type issued struct {
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expires_at"`
}

// reply is what one request came to: a key and its expiry, a refusal, or
// neither.
type reply struct {
	key     reader.Credential
	expires time.Time
	// refusal is ErrBudget or ErrForbidden.
	refusal error
	// down is set when neither a key nor a refusal came.
	down *Unavailable
}

// request asks the endpoint once. No error it reports holds a byte of the
// answer: an answer that is not the one the contract names may still hold a
// key.
func (e *Endpoint) request(ctx context.Context, group, owner, parse string) reply {
	down := func(reason string, err error) reply { return reply{down: &Unavailable{Reason: reason, Err: err}} }
	body, err := json.Marshal(question{Group: group, Owner: owner, Parse: parse})
	if err != nil {
		return down("the request to the key endpoint could not be encoded", err)
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return down("the request to the key endpoint could not be built", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return down("the request to the key endpoint did not complete", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return down("the answer of the key endpoint could not be read", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusPaymentRequired:
		return reply{refusal: ErrBudget}
	case http.StatusForbidden:
		return reply{refusal: ErrForbidden}
	default:
		// A wrong bearer, a rate limit and a failure of the endpoint are the
		// operator's to mend and say nothing about the group.
		return reply{down: &Unavailable{
			RetryAfter: retryAfter(resp.Header),
			Reason:     "the key endpoint answered " + strconv.Itoa(resp.StatusCode),
		}}
	}
	var a issued
	switch err := json.Unmarshal(raw, &a); {
	case err != nil:
		return down("the answer of the key endpoint is not a key and an expiry in RFC 3339", nil)
	case a.Key == "":
		return down("the answer of the key endpoint names no key", nil)
	case a.ExpiresAt.IsZero():
		return down("the answer of the key endpoint names no expiry", nil)
	}
	return reply{key: reader.NewCredential(a.Key), expires: a.ExpiresAt}
}

// retryAfter reads a Retry-After header given in whole seconds. A date is
// not read: it would compare this clock with the endpoint's.
func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
