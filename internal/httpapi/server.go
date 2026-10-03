// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package httpapi serves the HTTP API that api/openapi.yaml describes. The
// contract is that file; this package is held to it by a test that walks
// both. A route the contract marks planned is routed, authenticated, and
// answers 501, so a client written against the contract finds every address
// from the first version on. The design is specs/003-api.md.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/api"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/fetch"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/reader"
)

// Authenticator says who is calling. The owner it returns is the only
// scope a request has: a caller reads and changes its own files and parses
// and no one else's.
type Authenticator interface {
	// Authenticate returns the caller's owner id. It fails with
	// fault.MissingToken or fault.InvalidToken.
	Authenticate(r *http.Request) (owner string, err error)
}

// Tokens is an Authenticator over a fixed table: bearer token to owner.
type Tokens map[string]string

// Authenticate looks the request's bearer token up in the table.
func (t Tokens) Authenticate(r *http.Request) (string, error) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if token = strings.TrimSpace(token); !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", fault.New(fault.MissingToken, "the request carries no bearer token")
	}
	owner, ok := t[token]
	if !ok {
		return "", fault.New(fault.InvalidToken, "the bearer token is not known")
	}
	return owner, nil
}

// Server holds what the handlers depend on.
type Server struct {
	Store  *store.Memory
	Runner *run.Runner
	Auth   Authenticator

	// Readers and Chain are the configured readers and the order the
	// routing policy tries them in, as the runner has them.
	Readers map[string]reader.Reader
	Chain   []string

	// Limits bound one file. Fetcher gets a file from a URL; nil refuses
	// URL sources.
	Limits  pages.Limits
	Fetcher *fetch.Fetcher

	// BasePath is where the API is mounted. Empty takes "/v1".
	BasePath string

	// MaxDeadline is the longest a parse may be given. Zero takes one hour.
	MaxDeadline time.Duration

	// IDs makes identifiers, Now is the clock, and Log takes what a caller
	// is not told. Nil takes a new generator, time.Now, and slog.Default.
	IDs *id.Generator
	Now func() time.Time
	Log *slog.Logger
}

// handler is one operation. owner is the authenticated caller. An error it
// returns is written as the error envelope.
type handler func(w http.ResponseWriter, r *http.Request, owner string) error

// Route is one operation of the contract: its method, its path below the
// base path in the contract's own spelling, and whether it is built.
type Route struct {
	Method  string
	Path    string
	Planned bool

	handle handler
}

// Routes lists every operation the server routes, in the contract's order.
func (s *Server) Routes() []Route {
	planned := func(http.ResponseWriter, *http.Request, string) error {
		return fault.New(fault.NotImplemented, "this operation is part of the contract and is not built yet")
	}
	return []Route{
		{"POST", "/files", false, s.createFile},
		{"GET", "/files/{file}", false, s.getFile},
		{"DELETE", "/files/{file}", false, s.deleteFile},
		{"POST", "/parses", false, s.createParse},
		{"GET", "/parses", false, s.listParses},
		{"GET", "/parses/{parse}", false, s.getParse},
		{"DELETE", "/parses/{parse}", false, s.deleteParse},
		{"POST", "/parses/{parse}/cancel", false, s.cancelParse},
		{"POST", "/parses/{parse}/retry", true, planned},
		{"GET", "/parses/{parse}/events", true, planned},
		{"GET", "/parses/{parse}/document", false, s.getDocument},
		{"GET", "/parses/{parse}/pages", false, s.listPages},
		{"GET", "/parses/{parse}/pages/{page}", false, s.getPage},
		{"GET", "/parses/{parse}/pages/{page}/image", false, s.getPageImage},
		{"GET", "/parses/{parse}/blocks", false, s.listBlocks},
		{"GET", "/parses/{parse}/blocks/{ref}", false, s.getBlock},
		{"GET", "/parses/{parse}/blocks/{ref}/image", false, s.getBlockImage},
		{"POST", "/parses/{parse}/figures", false, s.createFigures},
		{"GET", "/parses/{parse}/figures", false, s.listFigures},
		{"GET", "/parses/{parse}/chunks", false, s.listChunks},
		{"POST", "/parses/{parse}/fields", true, planned},
		{"GET", "/parses/{parse}/fields", true, planned},
		{"GET", "/parses/{parse}/fields/{name}", true, planned},
		{"GET", "/readers", false, s.listReaders},
		{"GET", "/usage", true, planned},
		{"GET", "/queue", true, planned},
	}
}

// Handler returns the API. Below the base path every operation needs a
// caller; the contract itself and the health check do not.
func (s *Server) Handler() http.Handler {
	if s.IDs == nil {
		s.IDs = &id.Generator{}
	}
	base := strings.TrimRight(s.BasePath, "/")
	if s.BasePath == "" {
		base = "/v1"
	}

	mux := http.NewServeMux()
	for _, rt := range s.Routes() {
		mux.HandleFunc(rt.Method+" "+base+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			owner, err := s.Auth.Authenticate(r)
			if err == nil {
				err = rt.handle(w, r, owner)
			}
			if err != nil {
				s.fail(w, r, err)
			}
		})
	}
	mux.HandleFunc("GET "+base+"/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPI)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		// Nothing is routed here. The mux knows whether the path exists
		// under another method; it is asked, and the answer is written in
		// the API's own shape and not the mux's plain text.
		var probe probe
		mux.ServeHTTP(&probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.Header().Get("Allow"))
			s.fail(w, r, fault.New(fault.MethodNotAllowed, "%s is not a method of this address", r.Method))
			return
		}
		s.fail(w, r, fault.New(fault.NotFound, "no operation is routed at this address"))
	})
}

// probe is a response that keeps only its status and headers.
type probe struct {
	header http.Header
	status int
}

func (p *probe) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}
func (p *probe) Write(b []byte) (int, error) { return len(b), nil }
func (p *probe) WriteHeader(status int)      { p.status = status }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// problem is how a code is answered: the status, the one sentence a person
// reads, and whether the same request may succeed later.
type problem struct {
	status    int
	message   string
	retryable bool
}

// problems is every code the API answers with. The contract's list of
// codes and this table are held equal by a test.
var problems = map[fault.Code]problem{
	fault.InvalidRequest:       {http.StatusBadRequest, "The request is not valid.", false},
	fault.UnknownField:         {http.StatusBadRequest, "The request has a field that is not recognized.", false},
	fault.InvalidPages:         {http.StatusBadRequest, "The page selection is not valid.", false},
	fault.InvalidSchema:        {http.StatusBadRequest, "The schema is not valid.", false},
	fault.ReaderNotFound:       {http.StatusBadRequest, "There is no reader with that name.", false},
	fault.MissingToken:         {http.StatusUnauthorized, "Sign in to continue.", false},
	fault.InvalidToken:         {http.StatusUnauthorized, "The sign-in is not valid or has expired.", false},
	fault.BudgetExhausted:      {http.StatusPaymentRequired, "The budget for reading pages is spent.", false},
	fault.Forbidden:            {http.StatusForbidden, "You do not have access to this.", false},
	fault.ReaderNotPermitted:   {http.StatusForbidden, "You do not have access to this reader.", false},
	fault.NotFound:             {http.StatusNotFound, "There is nothing at this address.", false},
	fault.FileNotFound:         {http.StatusNotFound, "The file was not found.", false},
	fault.ParseNotFound:        {http.StatusNotFound, "The parse was not found.", false},
	fault.PageNotFound:         {http.StatusNotFound, "The page was not found.", false},
	fault.BlockNotFound:        {http.StatusNotFound, "The block was not found.", false},
	fault.MethodNotAllowed:     {http.StatusMethodNotAllowed, "This address does not take that method.", false},
	fault.PageNotReady:         {http.StatusConflict, "The page has not been read yet.", true},
	fault.DocumentNotReady:     {http.StatusConflict, "The document is not ready yet.", true},
	fault.Conflict:             {http.StatusConflict, "The request conflicts with what is already there.", false},
	fault.IdempotencyConflict:  {http.StatusConflict, "The idempotency key was already used with a different request.", false},
	fault.NotTerminal:          {http.StatusConflict, "The parse has not ended yet.", false},
	fault.AlreadyTerminal:      {http.StatusConflict, "The parse has already ended.", false},
	fault.FileTooLarge:         {http.StatusRequestEntityTooLarge, "The file is too large.", false},
	fault.TooManyPages:         {http.StatusRequestEntityTooLarge, "The file has too many pages.", false},
	fault.UnsupportedMediaType: {http.StatusUnsupportedMediaType, "This type of file cannot be parsed.", false},
	fault.SourceUnreachable:    {http.StatusUnprocessableEntity, "The file could not be fetched from its address.", true},
	fault.RateLimited:          {http.StatusTooManyRequests, "Too many requests. Try again shortly.", true},
	fault.QueueFull:            {http.StatusTooManyRequests, "Too much work is waiting. Try again shortly.", true},
	fault.NotImplemented:       {http.StatusNotImplemented, "This is not available yet.", false},
	fault.Internal:             {http.StatusInternalServerError, "Something went wrong on our side.", true},
}

// field names the request field an error is about. It rides inside a
// fault.Error and comes out as the envelope's details.field.
type field string

func (f field) Error() string { return "field " + string(f) }

// invalid is an invalid_request about one field.
func invalid(name, format string, args ...any) error {
	return fault.Wrap(fault.InvalidRequest, field(name), format, args...)
}

// fail writes an error as the envelope. A code with no entry in the table,
// and an error that carries no code, is internal: its text is logged and
// the caller is told only that it happened.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := fault.CodeOf(err)
	p, ok := problems[code]
	if !ok {
		code, p = fault.Internal, problems[fault.Internal]
	}
	details := map[string]any{"retryable": p.retryable}
	if code == fault.Internal {
		s.log().ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	} else if reason := fault.DetailOf(err); reason != "" {
		details["reason"] = reason
	}
	if f, ok := errors.AsType[field](err); ok {
		details["field"] = string(f)
	}
	httpjson.WriteError(w, p.status, httpjson.Error{Code: string(code), Message: p.message, Details: details})
}
