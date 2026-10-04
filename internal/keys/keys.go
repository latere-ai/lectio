// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package keys resolves the key a page is read with. The key a reader call
// is made with decides who the model endpoint charges, so a worker resolves
// it per group through one interface: one key from configuration for every
// group, or a key an operator's endpoint issued for the group, which a
// gateway attributes to that tenant and bounds by its budget. The design is
// specs/013-limits-and-usage.md.
//
// A key is held in memory and nowhere else. It travels as a
// reader.Credential, which prints as a placeholder, and no error and no log
// line of this package holds a key or the bearer the endpoint is asked with.
package keys

import (
	"context"
	"errors"
	"time"

	"latere.ai/x/lectio/reader"
)

// Source resolves the key a group's pages are read with.
type Source interface {
	// Key returns the key for a group. owner and parse say whose parse the
	// key is asked for, for a source that records it. An error is one of
	// the 2 refusals below, which no wait changes, or it means the source
	// cannot say yet: RetryAfterOf then reports when it is worth asking
	// again.
	Key(ctx context.Context, group, owner, parse string) (reader.Credential, error)
}

// The refusals of a source. Each is about the group and stands until the
// operator changes something, so a page that meets one fails at once.
var (
	// ErrBudget is a group with no budget left to read with.
	ErrBudget = errors.New("keys: the group has no budget left")
	// ErrForbidden is a group that may not read with this operator's keys.
	ErrForbidden = errors.New("keys: the group is issued no key")
)

// Unavailable is a source that issued no key and may issue one later: its
// endpoint did not answer, answered with something other than a key or a
// refusal, or the call ended while the key was asked for. It says nothing
// about the group, so a page that meets it waits and does not fail.
type Unavailable struct {
	// RetryAfter is how long until the source asks its endpoint again.
	// Until then it answers from memory, with what is left of the wait.
	RetryAfter time.Duration

	// Reason is one sentence for a developer. It never holds a key, the
	// bearer, or a byte of the endpoint's answer.
	Reason string

	// Err is the error underneath, when there is one.
	Err error
}

func (u *Unavailable) Error() string {
	if u.Err != nil {
		return "keys: " + u.Reason + ": " + u.Err.Error()
	}
	return "keys: " + u.Reason
}

// Unwrap returns the error underneath.
func (u *Unavailable) Unwrap() error { return u.Err }

// RetryAfterOf reports how long to wait before a source is asked again for
// the group err was returned for. It is zero for an error that names no
// wait, and the caller then takes a wait of its own.
func RetryAfterOf(err error) time.Duration {
	if u, ok := errors.AsType[*Unavailable](err); ok {
		return u.RetryAfter
	}
	return 0
}

// Static is the source of one key from configuration: every group's pages
// are read with it, and the operator pays. The member is exported so that a
// Static prints as the placeholder its Credential prints as.
type Static struct {
	Credential reader.Credential
}

// Key returns the one key, whatever the group.
func (s Static) Key(context.Context, string, string, string) (reader.Credential, error) {
	return s.Credential, nil
}
