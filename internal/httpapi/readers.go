// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/fault"
)

// Readers: the readers a deployment reads with and the order the routing
// policy tries them in, configured and stored through the API
// (specs/008-readers.md, specs/019-readers-at-runtime.md).

// ReaderAdmin stores readers and the routing policy and puts the set they
// make in force: *readerset.Manager. Each write is checked against the set
// it would make, and refused whole when that set does not build.
type ReaderAdmin interface {
	PutReader(ctx context.Context, name string, spec []byte) (created bool, err error)
	DeleteReader(ctx context.Context, name string) error
	PutPolicy(ctx context.Context, spec []byte) error
	DeletePolicy(ctx context.Context) error
	// Check reads what is stored again when it changed since this process
	// last read it.
	Check(ctx context.Context) error
}

// readerView is a reader as the API describes it.
type readerView struct {
	Name    string          `json:"name"`
	Accepts []string        `json:"accepts"`
	Image   imageView       `json:"image"`
	Boxes   bool            `json:"boxes"`
	Kinds   []document.Kind `json:"kinds,omitempty"`
	Version string          `json:"version,omitempty"`
	Default bool            `json:"default,omitempty"`
	// TextLayer says the reader reads a page from the text its file carries
	// and calls no model, so a console keeps it out of a picker of models
	// while the routing policy still tries it first.
	TextLayer bool `json:"text_layer,omitempty"`
	// Source is configured or added; Adapter and Model are what its spec
	// names; Describes and Extracts say what it does besides reading
	// pages; Spec is an added reader's spec as it was stored.
	Source    string          `json:"source"`
	Adapter   string          `json:"adapter,omitempty"`
	Model     string          `json:"model,omitempty"`
	Describes bool            `json:"describes"`
	Extracts  bool            `json:"extracts"`
	Spec      json.RawMessage `json:"spec,omitempty"`
}

type imageView struct {
	DPI      int    `json:"dpi,omitempty"`
	LongEdge int    `json:"long_edge,omitempty"`
	Format   string `json:"format,omitempty"`
}

// chainView is one of the policy's chains.
type chainView struct {
	Chain []string `json:"chain"`
}

// policyView is the routing policy in force.
type policyView struct {
	Read     chainView `json:"read"`
	Describe chainView `json:"describe"`
	Extract  chainView `json:"extract"`
	Source   string    `json:"source"`
}

// readers is the set in force: Live's when it is set, and otherwise the one
// the members name. A request takes it once, so it sees one set.
func (s *Server) readers() *config.Readers {
	if s.Live != nil {
		return s.Live.Current()
	}
	return &config.Readers{Readers: s.Readers, Chain: s.Chain}
}

// fresh is the set in force once what is stored was checked for a change:
// a request that names a reader, or is given one, is held to the readers as
// they are now, and not as this process read them up to a poll ago, so a
// reader just removed is not named by new work. A check that fails is
// logged, and the set this process holds answers.
func (s *Server) fresh(ctx context.Context) *config.Readers {
	if s.Manage != nil {
		if err := s.Manage.Check(ctx); err != nil {
			s.log().WarnContext(ctx, "the stored readers were not checked for a change; the set this process holds answers", "error", err)
		}
	}
	return s.readers()
}

// viewReader is one reader of the set as the API describes it, and false
// for a name the set does not hold.
func viewReader(set *config.Readers, name string, withSpec bool) (readerView, bool) {
	rd, ok := set.Readers[name]
	if !ok {
		return readerView{}, false
	}
	d := rd.Describe()
	v := readerView{
		Name: name, Accepts: d.Accepts, Boxes: d.Boxes, Kinds: d.Kinds, Version: d.Version,
		Image:     imageView{DPI: d.Image.DPI, LongEdge: d.Image.LongEdge, Format: d.Image.Format},
		Default:   len(set.Chain) > 0 && set.Chain[0] == name,
		TextLayer: d.Text && d.Local,
		Source:    set.Sources[name], Adapter: set.Adapters[name], Model: set.Models[name],
		Describes: set.Describers[name] != nil, Extracts: set.Extractors[name] != nil,
	}
	if v.Source == "" {
		v.Source = config.SourceConfigured
	}
	if spec := set.Specs[name]; withSpec && len(spec) > 0 {
		v.Spec = json.RawMessage(spec)
	}
	return v, true
}

// viewPolicy is the set's routing policy.
func viewPolicy(set *config.Readers) policyView {
	chain := func(names []string) chainView { return chainView{Chain: append([]string{}, names...)} }
	v := policyView{Read: chain(set.Chain), Describe: chain(set.DescribeChain), Extract: chain(set.ExtractChain), Source: set.PolicySource}
	if v.Source == "" {
		v.Source = config.SourceConfigured
	}
	return v
}

// listReaders lists the readers in force under their names, the routing
// policy's in its order ahead of the others, which follow by name.
func (s *Server) listReaders(w http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Readers()); err != nil {
		return err
	}
	set := s.readers()
	out := struct {
		Readers []readerView `json:"readers"`
	}{Readers: []readerView{}}
	names := slices.Clone(set.Chain)
	for _, name := range slices.Sorted(maps.Keys(set.Readers)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if v, ok := viewReader(set, name, false); ok {
			out.Readers = append(out.Readers, v)
		}
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

// getReader reads one reader, with its spec when it was stored.
func (s *Server) getReader(w http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Readers()); err != nil {
		return err
	}
	name := r.PathValue("name")
	v, ok := viewReader(s.readers(), name, true)
	if !ok {
		return fault.New(fault.NotFound, "no reader is named %q", name)
	}
	httpjson.Write(w, http.StatusOK, v)
	return nil
}

// manage is the store of readers, or not_implemented where there is none.
func (s *Server) manage() (ReaderAdmin, error) {
	if s.Manage == nil {
		return nil, fault.New(fault.NotImplemented, "this server stores no reader: readers are its configuration's alone")
	}
	return s.Manage, nil
}

// specBody reads a body that is one JSON object, as it was sent: the spec
// of a document, which the set checks member by member when it is built.
func specBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return nil, fault.New(fault.InvalidRequest, "the body is over %d bytes or stopped before its end", maxBody)
	}
	var members map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&members); err != nil || members == nil {
		return nil, fault.New(fault.InvalidRequest, "the body is not a JSON object")
	}
	if dec.More() {
		return nil, fault.New(fault.InvalidRequest, "the body holds more than one JSON value")
	}
	return bytes.TrimSpace(raw), nil
}

// putReader stores a reader, or replaces a stored one, and answers it as
// the set it made has it.
func (s *Server) putReader(w http.ResponseWriter, r *http.Request, c call) error {
	name := r.PathValue("name")
	if _, err := s.allowed(r, c, access.Reader(name)); err != nil {
		return err
	}
	m, err := s.manage()
	if err != nil {
		return err
	}
	spec, err := specBody(w, r)
	if err != nil {
		return err
	}
	created, err := m.PutReader(r.Context(), name, spec)
	if err != nil {
		return err
	}
	v, ok := viewReader(s.readers(), name, true)
	if !ok {
		// The set in force did not take the reader: a process that stores
		// one also builds it, so this is ours.
		return fault.New(fault.Internal, "the reader %s was stored and is not in force", name)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpjson.Write(w, status, v)
	return nil
}

// deleteReader removes a stored reader that nothing names.
func (s *Server) deleteReader(w http.ResponseWriter, r *http.Request, c call) error {
	name := r.PathValue("name")
	if _, err := s.allowed(r, c, access.Reader(name)); err != nil {
		return err
	}
	m, err := s.manage()
	if err != nil {
		return err
	}
	if err := m.DeleteReader(r.Context(), name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// getPolicy reads the routing policy in force.
func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Readers()); err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewPolicy(s.readers()))
	return nil
}

// putPolicy sets the routing policy in the place of the configured one.
func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Reader(access.PolicyName)); err != nil {
		return err
	}
	m, err := s.manage()
	if err != nil {
		return err
	}
	spec, err := specBody(w, r)
	if err != nil {
		return err
	}
	if err := m.PutPolicy(r.Context(), spec); err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewPolicy(s.readers()))
	return nil
}

// deletePolicy puts the configured routing policy back.
func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request, c call) error {
	if _, err := s.allowed(r, c, access.Reader(access.PolicyName)); err != nil {
		return err
	}
	m, err := s.manage()
	if err != nil {
		return err
	}
	if err := m.DeletePolicy(r.Context()); err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewPolicy(s.readers()))
	return nil
}

// Roles a request asks of a reader, for fallback.
const (
	rolePages   = "pages"
	roleFigures = "figures"
	roleFields  = "fields"
)

// fallback is the reader a request that names none is held to for a role:
// the allow's default reader when it does that role, and empty when it
// does not, which leaves the routing policy's order
// (specs/019-readers-at-runtime.md). A default the set does not hold is
// reader_not_found on the member the request would have named: the
// authorizer holds a default the deployment no longer offers.
func (s *Server) fallback(ctx context.Context, def, role, member string) (string, error) {
	if def == "" {
		return "", nil
	}
	set := s.fresh(ctx)
	if set.Readers[def] == nil {
		return "", fault.Wrap(fault.ReaderNotFound, field(member), "the default reader %q the caller is given is not configured", def)
	}
	switch {
	case role == roleFigures && set.Describers[def] == nil, role == roleFields && set.Extractors[def] == nil:
		return "", nil
	}
	return def, nil
}
