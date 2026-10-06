// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/extract"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store"
)

// maxInstructions is the most characters a caller's instructions hold.
const maxInstructions = 4000

// fieldName is what names an extraction: lower case, at most 63
// characters, beginning with a letter. It is a segment of an address and
// of an object's key, so it holds neither a slash nor a dot.
var fieldName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// fieldRequest is the body of a request for an extraction.
type fieldRequest struct {
	Name         string          `json:"name"`
	Schema       json.RawMessage `json:"schema"`
	Instructions string          `json:"instructions"`
	Citations    *bool           `json:"citations"`
	Extractor    string          `json:"extractor"`
}

// citationView is a citation with where its block is: the page, and the
// block's box on it when the block has a place.
type citationView struct {
	Ref  string        `json:"ref"`
	Page int           `json:"page"`
	Box  *document.Box `json:"box"`
}

// fieldView is one extraction as the API returns it. A citation is the ref
// of a block, or, when the read asked for it, the ref with its page and
// its box.
type fieldView struct {
	Name         string           `json:"name"`
	State        string           `json:"state"`
	Schema       json.RawMessage  `json:"schema,omitempty"`
	Instructions string           `json:"instructions,omitempty"`
	Data         json.RawMessage  `json:"data,omitempty"`
	Citations    map[string][]any `json:"citations,omitempty"`
	Model        string           `json:"model,omitempty"`
	Constrained  bool             `json:"constrained"`
	Attempts     int              `json:"attempts,omitempty"`
	Windows      int              `json:"windows,omitempty"`
	Usage        *document.Usage  `json:"usage,omitempty"`
	Error        *document.Error  `json:"error,omitempty"`
}

// viewField is a field as the API returns it, each citation a ref.
func viewField(f store.Field) fieldView {
	out := fieldView{
		Name: f.Name, State: f.State, Schema: f.Schema, Instructions: f.Instructions, Data: f.Data, Model: f.Model, Constrained: f.Constrained,
		Attempts: f.Attempts, Windows: f.Windows, Error: f.Error,
	}
	if f.Usage != (document.Usage{}) {
		out.Usage = &f.Usage
	}
	if len(f.Citations) > 0 {
		out.Citations = make(map[string][]any, len(f.Citations))
		for pointer, refs := range f.Citations {
			for _, ref := range refs {
				out.Citations[pointer] = append(out.Citations[pointer], ref)
			}
		}
	}
	return out
}

// createField asks an extraction of a parse: an object in the shape of a
// schema, filled from the document. It is a request against a parse and no
// option of a submit, so a new question needs no new parse and reads no
// page again. The schema is checked here, before anything is queued. The
// work is the parse's owner's and runs in the parse's group; what the
// caller's allow holds it to is the extractors the caller may name.
func (s *Server) createField(w http.ResponseWriter, r *http.Request, c call) error {
	p, d, err := s.parse(r, c)
	if err != nil {
		return err
	}
	var req fieldRequest
	if _, err := decode(w, r, &req); err != nil {
		return err
	}
	asked, err := s.checkField(req, d)
	if err != nil {
		return err
	}
	f, err := s.Backend.CreateField(r.Context(), p, asked)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, viewField(f))
	return nil
}

// checkField checks a request for an extraction, before anything is queued:
// its name, its schema, its instructions, and the extractor it names against
// what the caller may pin.
func (s *Server) checkField(req fieldRequest, d access.Decision) (store.FieldRequest, error) {
	switch {
	case !fieldName.MatchString(req.Name):
		return store.FieldRequest{}, invalid("name", "name is lower case letters, digits, underscores and hyphens, at most 63, beginning with a letter")
	case len(req.Schema) == 0:
		return store.FieldRequest{}, invalid("schema", "schema is required")
	case utf8.RuneCountInString(req.Instructions) > maxInstructions:
		return store.FieldRequest{}, invalid("instructions", "instructions holds at most %d characters", maxInstructions)
	}
	if _, err := extract.Compile(req.Schema); err != nil {
		return store.FieldRequest{}, fault.Wrap(fault.InvalidSchema, field("schema"), "%s", fault.DetailOf(err))
	}
	if l := d.Limits.Readers; req.Extractor != "" && len(l) > 0 && !slices.Contains(l, req.Extractor) {
		return store.FieldRequest{}, fault.Wrap(fault.ReaderNotPermitted, field("extractor"), "the extractor %s is not among the readers the caller may pin", req.Extractor)
	}
	return store.FieldRequest{
		Name: req.Name, Schema: req.Schema, Instructions: req.Instructions,
		Citations: req.Citations == nil || *req.Citations, Extractor: req.Extractor,
	}, nil
}

// askField asks an extraction of a parse again under its name: with another
// schema, or with the same one after it failed. The name is the path's; a
// body that names another is refused. An extraction that has not ended is
// refused with conflict, since its task may be running, and a name the parse
// has none of is asked as a new extraction, as POST asks one. The checks are
// those of a new extraction.
func (s *Server) askField(w http.ResponseWriter, r *http.Request, c call) error {
	p, d, err := s.parse(r, c)
	if err != nil {
		return err
	}
	var req fieldRequest
	if _, err := decode(w, r, &req); err != nil {
		return err
	}
	name := r.PathValue("name")
	switch {
	case !fieldName.MatchString(name):
		return fault.New(fault.NotFound, "no extraction is named %q", name)
	case req.Name != "" && req.Name != name:
		return invalid("name", "name is the path's, %s, or left out", name)
	}
	req.Name = name
	f, err := s.checkField(req, d)
	if err != nil {
		return err
	}
	asked, err := s.Backend.AskField(r.Context(), p, f)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusAccepted, viewField(asked))
	return nil
}

// listFields lists the extractions of a parse, by name.
func (s *Server) listFields(w http.ResponseWriter, r *http.Request, c call) error {
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	fields, err := s.Backend.Fields(r.Context(), p)
	if err != nil {
		return err
	}
	out := struct {
		Fields []fieldView `json:"fields"`
	}{Fields: make([]fieldView, len(fields))}
	for i, f := range fields {
		out.Fields[i] = viewField(f)
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

// getField reads one extraction. With resolve set, each citation comes
// with the page its block is on and the block's box, so a caller draws the
// source of a value without a read of the block.
func (s *Server) getField(w http.ResponseWriter, r *http.Request, c call) error {
	resolve := false
	if raw := r.URL.Query().Get("resolve"); raw != "" {
		var err error
		if resolve, err = strconv.ParseBool(raw); err != nil {
			return invalid("resolve", "resolve is true or false")
		}
	}
	p, _, err := s.parse(r, c)
	if err != nil {
		return err
	}
	f, ok, err := s.Backend.Field(r.Context(), p, r.PathValue("name"))
	if err != nil {
		return err
	}
	if !ok {
		return fault.New(fault.NotFound, "parse %s has no extraction named %s", p.ID, r.PathValue("name"))
	}
	out := viewField(f)
	if resolve {
		// A page is read once, however many values cite it.
		pages := map[int]document.Page{}
		for pointer, refs := range f.Citations {
			for i, ref := range refs {
				n, _, err := document.ParseRef(ref)
				if err != nil {
					continue
				}
				page, read := pages[n]
				if !read {
					if page, _, err = s.Backend.Page(r.Context(), p, n); err != nil {
						return err
					}
					pages[n] = page
				}
				resolved := citationView{Ref: ref, Page: n}
				if block, err := page.Find(ref); err == nil {
					resolved.Box = block.Box
				}
				out.Citations[pointer][i] = resolved
			}
		}
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}
