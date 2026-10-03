// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package fault is the set of reasons work on a file fails. Every package
// under internal returns a *fault.Error for a failure a caller can act on,
// and the API maps its code to a status. The codes are part of the HTTP
// contract (specs/003-api.md): adding one is a change to that spec.
package fault

import (
	"errors"
	"fmt"
)

// Code names one reason. A caller branches on it and never on the detail.
type Code string

// The reasons a request is refused before any work.
const (
	InvalidRequest       Code = "invalid_request"
	UnknownField         Code = "unknown_field"
	InvalidPages         Code = "invalid_pages"
	InvalidSchema        Code = "invalid_schema"
	UnsupportedMediaType Code = "unsupported_media_type"
	FileTooLarge         Code = "file_too_large"
	TooManyPages         Code = "too_many_pages"
	SourceUnreachable    Code = "source_unreachable"
	MissingToken         Code = "missing_token"
	InvalidToken         Code = "invalid_token"
	Forbidden            Code = "forbidden"
	ReaderNotFound       Code = "reader_not_found"
	FileNotFound         Code = "file_not_found"
	ParseNotFound        Code = "parse_not_found"
	PageNotFound         Code = "page_not_found"
	PageNotReady         Code = "page_not_ready"
	BlockNotFound        Code = "block_not_found"
	DocumentNotReady     Code = "document_not_ready"
	Conflict             Code = "conflict"
	IdempotencyConflict  Code = "idempotency_conflict"
	NotTerminal          Code = "not_terminal"
	AlreadyTerminal      Code = "already_terminal"
	RateLimited          Code = "rate_limited"
	QueueFull            Code = "queue_full"
	NotImplemented       Code = "not_implemented"
	ReaderNotPermitted   Code = "reader_not_permitted"
	NotFound             Code = "not_found"
	MethodNotAllowed     Code = "method_not_allowed"
)

// The reasons work that was accepted fails. These land on the parse or on a
// page, not on the request that submitted it.
const (
	DocumentCorrupt    Code = "document_corrupt"
	PageUnreadable     Code = "page_unreadable"
	FigureUnreadable   Code = "figure_unreadable"
	ReaderUnavailable  Code = "reader_unavailable"
	BudgetExhausted    Code = "budget_exhausted"
	DeadlineExceeded   Code = "deadline_exceeded"
	SchemaNotSatisfied Code = "schema_not_satisfied"
	Internal           Code = "internal"
)

// Error is one failure: a code a caller branches on, a sentence for a
// developer, and the error underneath when there is one. The sentence never
// holds file content, a file name, or a credential.
type Error struct {
	Code   Code
	Detail string
	Err    error
}

// New builds an Error whose detail is formatted like fmt.Sprintf.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Wrap builds an Error that keeps err underneath, so errors.Is and errors.As
// still reach it.
func Wrap(code Code, err error, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...), Err: err}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Code) + ": " + e.Detail + ": " + e.Err.Error()
	}
	return string(e.Code) + ": " + e.Detail
}

// Unwrap returns the error underneath.
func (e *Error) Unwrap() error { return e.Err }

// CodeOf reports the code of the first *Error in err's chain. An error that
// carries none is Internal: a failure nobody classified is not the caller's.
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return Internal
}

// DetailOf reports the detail of the first *Error in err's chain, or the
// empty string. An unclassified error's text is not returned: it may hold
// what a detail must not.
func DetailOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Detail
	}
	return ""
}
