// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/internal/access"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/id"
	"latere.ai/x/lectio/internal/store"
)

// Sets: a stored definition of an extraction, made before any parse is
// read, and asked of each parse that joins it (specs/018-sets.md). The body
// of a set is the body of a request for an extraction, and is checked as one
// is.

// setView is one set as the API returns it.
type setView struct {
	ID           string          `json:"id"`
	Owner        string          `json:"owner,omitempty"`
	Name         string          `json:"name"`
	Schema       json.RawMessage `json:"schema"`
	Instructions string          `json:"instructions,omitempty"`
	Citations    bool            `json:"citations"`
	Extractor    string          `json:"extractor,omitempty"`
	Version      int             `json:"version"`
	Parses       store.SetCounts `json:"parses"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

func viewSet(s store.Set) setView {
	return setView{
		ID: s.ID, Owner: s.Owner, Name: s.Name, Schema: s.Schema, Instructions: s.Instructions,
		Citations: s.Citations, Extractor: s.Extractor, Version: s.Version, Parses: s.Parses,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

// memberView is one parse of a set with its extraction under the set's
// name, each as its own read answers it.
type memberView struct {
	Parse parseView `json:"parse"`
	Field fieldView `json:"field"`
}

// set returns the stored set the request's path names, once the caller may
// do the route's action to it. A set the caller may not act on is answered
// exactly as one that is not there, as a parse is.
func (s *Server) set(r *http.Request, c call) (store.Set, access.Decision, error) {
	set, err := s.Backend.Set(r.Context(), r.PathValue("set"))
	if err != nil {
		return store.Set{}, access.Decision{}, err
	}
	d, err := s.ask(r, c, access.Set{ID: set.ID, Owner: set.Owner, Name: set.Name}.Resource())
	if err == nil {
		err = d.Or(fault.New(fault.SetNotFound, "no set %s", set.ID))
	}
	if err != nil {
		return store.Set{}, access.Decision{}, err
	}
	return set, d, nil
}

// pageOf reads the limit and the cursor of a list.
func pageOf(r *http.Request) (limit int, cursor string, err error) {
	limit = defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			return 0, "", invalid("limit", "limit is from 1 to %d", maxLimit)
		}
		limit = n
	}
	return limit, r.URL.Query().Get("cursor"), nil
}

// createSet makes a set in the owner the allow names: its definition is
// checked as an extraction's request is, and nothing is asked of any parse.
func (s *Server) createSet(w http.ResponseWriter, r *http.Request, c call) error {
	var req fieldRequest
	if _, err := decode(w, r, &req); err != nil {
		return err
	}
	d, err := s.allowed(r, c, access.Set{Name: req.Name}.Resource())
	if err != nil {
		return err
	}
	asked, err := s.checkField(r.Context(), req, d, true)
	if err != nil {
		return err
	}
	made, err := s.Backend.CreateSet(r.Context(), store.Set{
		ID: s.IDs.New(id.Set), Owner: d.Limits.Owner, Name: asked.Name,
		Schema: asked.Schema, Instructions: asked.Instructions, Citations: asked.Citations, Extractor: asked.Extractor,
	})
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, viewSet(made))
	return nil
}

// listSets lists the sets the caller may see, newest first, of a name when
// one is given. The allow's filter narrows the owners; a set carries no
// label, so a filter that narrows by labels leaves none.
func (s *Server) listSets(w http.ResponseWriter, r *http.Request, c call) error {
	limit, cursor, err := pageOf(r)
	if err != nil {
		return err
	}
	d, err := s.allowed(r, c, access.Sets())
	if err != nil {
		return err
	}
	out := struct {
		Sets       []setView `json:"sets"`
		NextCursor string    `json:"next_cursor,omitempty"`
	}{Sets: []setView{}}
	var owners []string
	none := false
	if by := d.Filter; by != nil {
		if by.Owners != nil {
			owners = slices.Clone(by.Owners)
			none = len(owners) == 0
		}
		none = none || len(by.Labels) > 0
	}
	if !none {
		found, more, err := s.Backend.Sets(r.Context(), owners, r.URL.Query().Get("name"), cursor, limit)
		if err != nil {
			return err
		}
		for _, set := range found {
			out.Sets = append(out.Sets, viewSet(set))
		}
		if more {
			out.NextCursor = found[len(found)-1].ID
		}
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

func (s *Server) getSet(w http.ResponseWriter, r *http.Request, c call) error {
	set, _, err := s.set(r, c)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewSet(set))
	return nil
}

// updateSet replaces a set's definition, which asks every parse of the set
// again with it. The name is the set's; a body that names another is
// refused.
func (s *Server) updateSet(w http.ResponseWriter, r *http.Request, c call) error {
	set, d, err := s.set(r, c)
	if err != nil {
		return err
	}
	var req fieldRequest
	if _, err := decode(w, r, &req); err != nil {
		return err
	}
	if req.Name != "" && req.Name != set.Name {
		return invalid("name", "name is the set's, %s, or left out", set.Name)
	}
	req.Name = set.Name
	asked, err := s.checkField(r.Context(), req, d, true)
	if err != nil {
		return err
	}
	set.Schema, set.Instructions, set.Citations, set.Extractor = asked.Schema, asked.Instructions, asked.Citations, asked.Extractor
	changed, err := s.Backend.UpdateSet(r.Context(), set)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewSet(changed))
	return nil
}

// deleteSet removes a set. Its parses, and their extractions under its
// name, stay.
func (s *Server) deleteSet(w http.ResponseWriter, r *http.Request, c call) error {
	set, _, err := s.set(r, c)
	if err != nil {
		return err
	}
	if err := s.Backend.DeleteSet(r.Context(), set.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// listSetParses lists the parses of a set, newest first, each with its
// extraction under the set's name: the set's table in one read.
func (s *Server) listSetParses(w http.ResponseWriter, r *http.Request, c call) error {
	limit, cursor, err := pageOf(r)
	if err != nil {
		return err
	}
	set, _, err := s.set(r, c)
	if err != nil {
		return err
	}
	found, more, err := s.Backend.SetParses(r.Context(), set, cursor, limit)
	if err != nil {
		return err
	}
	out := struct {
		Parses     []memberView `json:"parses"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}{Parses: make([]memberView, len(found))}
	for i, m := range found {
		out.Parses[i] = memberView{Parse: viewParse(m.Parse), Field: viewField(m.Field)}
	}
	if more {
		out.NextCursor = found[len(found)-1].Parse.ID
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

// joinSet adds a parse of the set's owner to the set. It asks about the set
// alone: a parse of another owner is answered as one that is not there. The
// answer is the parse's extraction under the set's name, 202 when it was
// asked and 200 when the parse held one the set's definition filled.
func (s *Server) joinSet(w http.ResponseWriter, r *http.Request, c call) error {
	set, d, err := s.set(r, c)
	if err != nil {
		return err
	}
	if set.Extractor != "" {
		s.fresh(r.Context())
	}
	if l := d.Limits.Readers; set.Extractor != "" && len(l) > 0 && !slices.Contains(l, set.Extractor) {
		return fault.New(fault.ReaderNotPermitted, "the set's extractor %s is not among the readers the caller may pin", set.Extractor)
	}
	p, err := s.Backend.Parse(r.Context(), r.PathValue("parse"))
	if err != nil {
		return err
	}
	if p.Owner != set.Owner {
		// Answered as Store.Parse answers a parse that is not there, so a
		// set of one's own tells nothing of another owner's parses.
		return fault.New(fault.ParseNotFound, "no parse %s", p.ID)
	}
	f, asked, err := s.Backend.JoinSet(r.Context(), set, p)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if asked {
		status = http.StatusAccepted
	}
	httpjson.Write(w, status, joinView{fieldView: viewField(f), Asked: asked})
	return nil
}

// joinView is a join's answer: the parse's extraction under the set's name,
// and whether the join asked it, which the status says as well.
type joinView struct {
	fieldView
	Asked bool `json:"asked"`
}

// leaveSet takes a parse out of a set. Its extraction under the set's name
// stays on the parse, of no set, and is the answer.
func (s *Server) leaveSet(w http.ResponseWriter, r *http.Request, c call) error {
	set, _, err := s.set(r, c)
	if err != nil {
		return err
	}
	p, err := s.Backend.Parse(r.Context(), r.PathValue("parse"))
	if err != nil {
		return err
	}
	if p.Owner != set.Owner {
		// Answered as joinSet answers it.
		return fault.New(fault.ParseNotFound, "no parse %s", p.ID)
	}
	f, err := s.Backend.LeaveSet(r.Context(), set, p)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, viewField(f))
	return nil
}
