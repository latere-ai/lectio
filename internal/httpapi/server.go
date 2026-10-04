// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package httpapi serves the HTTP API that api/openapi.yaml describes. The
// contract is that file; this package is held to it by a test that walks
// both. A route the contract marks planned is routed, authenticated, asks
// its action, and answers 501, so a client written against the contract
// finds every address from the first version on. The design is
// specs/003-api.md.
//
// Every route is written the same way
// (specs/012-identity-and-authorization.md): the caller is authenticated,
// the resource is built from the request and from the stored object, the
// route's action is asked, and the handler acts within the decision. Which
// action a route asks is the row of access.Routes for it, and a test walks
// the contract with an authorizer that records what it is asked.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/api"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/fetch"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/reader"
)

// Server holds what the handlers depend on.
type Server struct {
	// Backend is where files, parses and results are kept and how a parse
	// is run.
	Backend Backend

	// Auth says who is calling and Authz what that caller may do
	// (specs/012-identity-and-authorization.md). A handler cannot tell what
	// is behind either: a verifier or a development token, an operator's
	// endpoint or the owner policy.
	Auth  access.Authenticator
	Authz access.Authorizer

	// Readers and Chain are the configured readers and the order the
	// routing policy tries them in, as whoever runs the pages has them.
	Readers map[string]reader.Reader
	Chain   []string

	// Limits bound one file for every caller. An allow lowers them for its
	// caller and never raises them. Fetcher gets a file from a URL; nil
	// refuses URL sources.
	Limits  pages.Limits
	Fetcher *fetch.Fetcher

	// FileRetention is how long a file that a submit fetched from a URL is
	// kept, unless the submit's allow names a shorter time. Zero keeps it.
	// An upload's retention is its own allow's.
	FileRetention time.Duration

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

// call is one request being answered: who is calling, and the action its
// route asks before it acts. The action is the row of access.Routes for
// the route, so no handler names one.
type call struct {
	caller access.Caller
	action string
}

// handler is one operation. An error it returns is written as the error
// envelope.
type handler func(w http.ResponseWriter, r *http.Request, c call) error

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
	return []Route{
		{"POST", "/files", false, s.createFile},
		{"GET", "/files/{file}", false, s.getFile},
		{"DELETE", "/files/{file}", false, s.deleteFile},
		{"POST", "/parses", false, s.createParse},
		{"GET", "/parses", false, s.listParses},
		{"GET", "/parses/{parse}", false, s.getParse},
		{"DELETE", "/parses/{parse}", false, s.deleteParse},
		{"POST", "/parses/{parse}/cancel", false, s.cancelParse},
		{"POST", "/parses/{parse}/retry", true, s.plannedOnParse},
		{"GET", "/parses/{parse}/events", true, s.plannedOnParse},
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
		{"POST", "/parses/{parse}/fields", true, s.plannedOnParse},
		{"GET", "/parses/{parse}/fields", true, s.plannedOnParse},
		{"GET", "/parses/{parse}/fields/{name}", true, s.plannedOnParse},
		{"GET", "/readers", false, s.listReaders},
		{"GET", "/usage", true, s.plannedUsage},
		{"GET", "/queue", true, s.plannedQueue},
	}
}

// errPlanned is the answer of an operation the contract has and the server
// does not build yet.
func errPlanned() error {
	return fault.New(fault.NotImplemented, "this operation is part of the contract and is not built yet")
}

// plannedOnParse answers a planned operation on a parse. It asks as the
// built operation will, so what a caller may not do is refused today and
// not on the day the operation is built.
func (s *Server) plannedOnParse(_ http.ResponseWriter, r *http.Request, c call) error {
	if _, _, err := s.parse(r, c); err != nil {
		return err
	}
	return errPlanned()
}

// plannedUsage answers the planned read of the meters. The read names no
// owner and no group yet, so the question carries neither.
func (s *Server) plannedUsage(_ http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Usage("", "")); err != nil {
		return err
	}
	return errPlanned()
}

// plannedQueue answers the planned read of the queue.
func (s *Server) plannedQueue(_ http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Queue("")); err != nil {
		return err
	}
	return errPlanned()
}

// ask puts the route's question about a resource to the authorizer. An
// error is a question that got no decision, and the request fails closed.
func (s *Server) ask(r *http.Request, c call, resource authz.Resource) (access.Decision, error) {
	return s.Authz.Authorize(r.Context(), c.caller, access.Ask(r, c.action, resource))
}

// allowed asks and refuses a deny with forbidden: the answer of a create,
// of a list, and of a read that is about no stored object.
func (s *Server) allowed(r *http.Request, c call, resource authz.Resource) (access.Decision, error) {
	d, err := s.ask(r, c, resource)
	if err != nil {
		return access.Decision{}, err
	}
	return d, d.Err()
}

// parse returns the stored parse the request's path names, once the
// caller may do the route's action to it. The parse is read by its id
// alone, since its owner is part of the question. A parse the caller may
// not act on is answered exactly as one that is not there, so an id cannot
// be probed for what somebody else owns.
func (s *Server) parse(r *http.Request, c call) (store.Parse, access.Decision, error) {
	p, err := s.Backend.Parse(r.Context(), r.PathValue("parse"))
	if err != nil {
		return store.Parse{}, access.Decision{}, err
	}
	d, err := s.ask(r, c, parseResource(p))
	if err == nil {
		err = d.Or(fault.New(fault.ParseNotFound, "no parse %s", p.ID))
	}
	if err != nil {
		return store.Parse{}, access.Decision{}, err
	}
	return p, d, nil
}

// parseResource is a stored parse as a question carries it.
func parseResource(p store.Parse) authz.Resource {
	res := access.Parse{
		ID: p.ID, Owner: p.Owner, Class: p.Class, Priority: p.Priority, Reader: p.Reader, Labels: p.Labels,
		Pages: p.PagesTotal,
	}
	if p.Origin != nil {
		res.Origin = access.Origin{Store: p.Origin.Store, Path: p.Origin.Path, Version: p.Origin.Version}
	}
	if p.Manifest != nil {
		// The selection is known once the pages were counted.
		res.Pages = len(p.Manifest.Selected)
	}
	return res.Resource()
}

// file returns the stored file the request's path names, once the caller
// may do the route's action to it, as parse does for a parse.
func (s *Server) file(r *http.Request, c call) (store.File, error) {
	f, err := s.Backend.File(r.Context(), r.PathValue("file"))
	if err != nil {
		return store.File{}, err
	}
	d, err := s.ask(r, c, access.File{ID: f.ID, Owner: f.Owner, Size: f.Size, MediaType: f.MediaType}.Resource())
	if err == nil {
		err = d.Or(fault.New(fault.FileNotFound, "no file %s", f.ID))
	}
	if err != nil {
		return store.File{}, err
	}
	return f, nil
}

// Handler returns the API. Below the base path every operation needs a
// caller and asks its action; the contract itself and the health check do
// neither.
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
		// A route with no row asks the empty action, which the authorizer
		// refuses before it is sent: the request fails closed.
		row, _ := access.RouteOf(rt.Method, rt.Path)
		mux.HandleFunc(rt.Method+" "+base+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			caller, err := s.Auth.Authenticate(r)
			if err == nil {
				err = rt.handle(w, r, call{caller: caller, action: row.Action})
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
	fault.InvalidRequest:        {http.StatusBadRequest, "The request is not valid.", false},
	fault.UnknownField:          {http.StatusBadRequest, "The request has a field that is not recognized.", false},
	fault.InvalidPages:          {http.StatusBadRequest, "The page selection is not valid.", false},
	fault.InvalidSchema:         {http.StatusBadRequest, "The schema is not valid.", false},
	fault.ReaderNotFound:        {http.StatusBadRequest, "There is no reader with that name.", false},
	fault.MissingToken:          {http.StatusUnauthorized, "Sign in to continue.", false},
	fault.InvalidToken:          {http.StatusUnauthorized, "The sign-in is not valid or has expired.", false},
	fault.BudgetExhausted:       {http.StatusPaymentRequired, "The budget for reading pages is spent.", false},
	fault.Forbidden:             {http.StatusForbidden, "You do not have access to this.", false},
	fault.ReaderNotPermitted:    {http.StatusForbidden, "You do not have access to this reader.", false},
	fault.NotFound:              {http.StatusNotFound, "There is nothing at this address.", false},
	fault.FileNotFound:          {http.StatusNotFound, "The file was not found.", false},
	fault.ParseNotFound:         {http.StatusNotFound, "The parse was not found.", false},
	fault.PageNotFound:          {http.StatusNotFound, "The page was not found.", false},
	fault.BlockNotFound:         {http.StatusNotFound, "The block was not found.", false},
	fault.MethodNotAllowed:      {http.StatusMethodNotAllowed, "This address does not take that method.", false},
	fault.PageNotReady:          {http.StatusConflict, "The page has not been read yet.", true},
	fault.DocumentNotReady:      {http.StatusConflict, "The document is not ready yet.", true},
	fault.Conflict:              {http.StatusConflict, "The request conflicts with what is already there.", false},
	fault.IdempotencyConflict:   {http.StatusConflict, "The idempotency key was already used with a different request.", false},
	fault.NotTerminal:           {http.StatusConflict, "The parse has not ended yet.", false},
	fault.AlreadyTerminal:       {http.StatusConflict, "The parse has already ended.", false},
	fault.FileTooLarge:          {http.StatusRequestEntityTooLarge, "The file is too large.", false},
	fault.TooManyPages:          {http.StatusRequestEntityTooLarge, "The file has too many pages.", false},
	fault.UnsupportedMediaType:  {http.StatusUnsupportedMediaType, "This type of file cannot be parsed.", false},
	fault.SourceUnreachable:     {http.StatusUnprocessableEntity, "The file could not be fetched from its address.", true},
	fault.RateLimited:           {http.StatusTooManyRequests, "Too many requests. Try again shortly.", true},
	fault.QueueFull:             {http.StatusTooManyRequests, "Too much work is waiting. Try again shortly.", true},
	fault.NotImplemented:        {http.StatusNotImplemented, "This is not available yet.", false},
	fault.CapabilityUnsupported: {http.StatusUnprocessableEntity, "This server does not support a limit that applies to this request.", false},
	fault.AuthorizerUnavailable: {http.StatusServiceUnavailable, "Access could not be checked. Try again shortly.", true},
	fault.Internal:              {http.StatusInternalServerError, "Something went wrong on our side.", true},
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
	switch code {
	case fault.Internal:
		s.log().ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	case fault.AuthorizerUnavailable:
		// The error underneath may name the endpoint. It is logged, and
		// the caller is told the fixed detail alone.
		s.log().WarnContext(r.Context(), "the authorizer gave no decision", "method", r.Method, "path", r.URL.Path, "error", err)
		details["reason"] = fault.DetailOf(err)
	default:
		if reason := fault.DetailOf(err); reason != "" {
			details["reason"] = reason
		}
	}
	if f, ok := errors.AsType[field](err); ok {
		details["field"] = string(f)
	}
	httpjson.WriteError(w, p.status, httpjson.Error{Code: string(code), Message: p.message, Details: details})
}
