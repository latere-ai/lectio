// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reader

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Class says what a caller should do about a failed call. Every error a
// Reader or an Extractor returns carries one, so the code that schedules
// work never looks at a status code or a vendor's error body.
type Class int

// The classes of a failed call.
const (
	// Retryable: the same call may succeed later. A network error, a
	// timeout, a 5xx. It spends an attempt.
	Retryable Class = iota + 1

	// Invalid: the model answered and the answer is not usable: it does not
	// parse, does not match the reply schema, or loops. It spends an
	// attempt, and repeated invalid replies are what send a page to the
	// next reader.
	Invalid

	// RateLimited: the endpoint is describing its own capacity. The call
	// waits and spends no attempt. RetryAfter says how long, when the
	// endpoint said.
	RateLimited

	// Budget: the endpoint refuses because the key's budget is spent. The
	// reader is healthy and the caller is out of funds, so this is final
	// for the parse and says nothing about the reader.
	Budget

	// Permanent: this page will never be read by this reader: the image is
	// too large for it, or in a form it does not take. It is not retried.
	Permanent

	// Refused: the model, or a filter in front of it, declined the content
	// of this page. The reader is healthy and another may read the page,
	// so the page goes to the next reader in the chain and is not tried
	// again here.
	Refused

	// Misconfigured: the endpoint rejected the request itself: a parameter
	// it does not accept, a model it does not have, a key it does not
	// know. No page succeeds on this reader until its configuration
	// changes, so the failure is the reader's and not the page's.
	Misconfigured
)

func (c Class) String() string {
	switch c {
	case Retryable:
		return "retryable"
	case Invalid:
		return "invalid"
	case RateLimited:
		return "rate_limited"
	case Budget:
		return "budget"
	case Permanent:
		return "permanent"
	case Refused:
		return "refused"
	case Misconfigured:
		return "misconfigured"
	}
	return "class(" + strconv.Itoa(int(c)) + ")"
}

// Error is a failed call to a reader or an extractor.
type Error struct {
	Class Class

	// RetryAfter is how long the endpoint asked the caller to wait. Zero
	// when it did not say.
	RetryAfter time.Duration

	// Status is the HTTP status behind the error, zero when there was none.
	Status int

	// Detail is one sentence for a developer. It never holds page content
	// or a credential.
	Detail string

	// Err is the error underneath, when there is one.
	Err error
}

func (e *Error) Error() string {
	s := e.Class.String() + ": " + e.Detail
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the error underneath.
func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an Error of a class.
func Errorf(class Class, format string, args ...any) *Error {
	return &Error{Class: class, Detail: fmt.Sprintf(format, args...)}
}

// ClassOf reports the class of the first *Error in err's chain. An error
// that carries none is Retryable: an adapter that failed without saying why
// has most likely met a transport problem, and a wrong guess costs one
// attempt and not a page.
func ClassOf(err error) Class {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Class
	}
	return Retryable
}

// RetryAfterOf reports how long the endpoint asked the caller to wait.
func RetryAfterOf(err error) time.Duration {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.RetryAfter
	}
	return 0
}

// FromTransport classifies an error from sending a request: the connection
// failed, the context ended, the body could not be read.
func FromTransport(err error) *Error {
	return &Error{Class: Retryable, Detail: "the request did not complete", Err: err}
}

// FromStatus classifies an HTTP response that is not a success. It is what
// every adapter over HTTP calls, so one endpoint's 429 means the same thing
// as another's. body is the response body, read for the words a gateway
// uses to refuse a spent budget; it is not kept.
func FromStatus(status int, header http.Header, body []byte) *Error {
	e := &Error{Status: status, RetryAfter: retryAfter(header), Detail: "the endpoint answered " + strconv.Itoa(status)}
	switch {
	case status == http.StatusPaymentRequired || mentionsBudget(body):
		e.Class = Budget
	case status == http.StatusTooManyRequests:
		e.Class = RateLimited
	case status == http.StatusServiceUnavailable && e.RetryAfter > 0:
		// An endpoint that says when to come back is describing its
		// capacity, not failing.
		e.Class = RateLimited
	case status == http.StatusRequestTimeout || status == http.StatusConflict || status >= 500:
		e.Class = Retryable
	case status == http.StatusRequestEntityTooLarge || status == http.StatusUnsupportedMediaType:
		// The page as it was sent: too large, or in a form the endpoint
		// does not take.
		e.Class = Permanent
	default:
		// Any other 4xx is about the request and not about the page: a
		// parameter, the model's name, the key, the address.
		e.Class = Misconfigured
	}
	return e
}

// retryAfter reads a Retry-After header given in seconds. A date form is
// not read: no model endpoint sends one, and a clock comparison across
// machines is not something to act on.
func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(header.Get("Retry-After")), 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// budgetWords are the codes gateways and providers put in the body of a
// refusal for a spent budget. A body is searched only up to a bound, since
// an error body that long is not an error body.
var budgetWords = []string{"budget_exhausted", "spend_exceeded", "insufficient_quota", "billing_hard_limit"}

func mentionsBudget(body []byte) bool {
	text := strings.ToLower(string(body[:min(len(body), 4096)]))
	for _, w := range budgetWords {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}
