// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"latere.ai/x/lectio/internal/fault"
)

// Sets: a stored definition of an extraction, asked of each parse that
// joins it (specs/018-sets.md). A parse is in a set when it holds an
// extraction under the set's name that the set asked.

// MaxSetParses is the most parses one set holds. A change of a set's
// definition touches every parse in it in one transaction, under the lock
// every exchange takes, and the bound keeps that transaction short.
const MaxSetParses = 10_000

const (
	setCreateSQL   = `SELECT lectio_set_create($1)`
	setCreateAtSQL = `SELECT lectio_set_create($1, $2)`
	setSQL         = `SELECT lectio_set($1)`
	setsSQL        = `SELECT lectio_sets($1, $2, $3, $4)`
	setParsesSQL   = `SELECT lectio_set_parses($1, $2, $3)`
	setJoinSQL     = `SELECT lectio_set_join($1)`
	setJoinAtSQL   = `SELECT lectio_set_join($1, $2)`
	setUpdateSQL   = `SELECT lectio_set_update($1)`
	setUpdateAtSQL = `SELECT lectio_set_update($1, $2)`
	setDeleteSQL   = `SELECT lectio_set_delete($1)`
	setLeaveSQL    = `SELECT lectio_set_leave($1, $2)`
	setLeaveAtSQL  = `SELECT lectio_set_leave($1, $2, $3)`
)

// SetRequest is a set to write, or the new definition of one.
type SetRequest struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`

	// Request is the definition: what each parse is asked, a tasks.Field as
	// one JSON document, kept as an extraction's request is kept so that an
	// extraction asked with it holds the same bytes.
	Request string `json:"request"`

	// Pin is the extractor the definition names, when it names one.
	Pin string `json:"pin,omitempty"`
}

// Set is one set as the store holds it.
type Set struct {
	ID      string `json:"set_id"`
	Owner   string `json:"owner"`
	Name    string `json:"name"`
	Request string `json:"request"`
	Pin     string `json:"pin"`
	Version int    `json:"version"`

	// Parses is how many parses are in the set, and where their extractions
	// stand.
	Parses SetCounts `json:"parses"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SetCounts are the parses of a set by the state of their extraction.
type SetCounts struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// Member is one parse of a set with its extraction under the set's name.
type Member struct {
	Parse Parse `json:"parse"`
	Field Field `json:"field"`
}

// CreateSet writes a set. A name the owner already has is refused with
// conflict.
func (s *Store) CreateSet(ctx context.Context, r SetRequest) (Set, error) {
	if r.ID == "" || r.Owner == "" || r.Name == "" || r.Request == "" {
		return Set{}, fault.New(fault.InvalidRequest, "a set has an id, an owner, a name and a request")
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return Set{}, fmt.Errorf("store: encoding the set %s: %w", r.Name, err)
	}
	var answer struct {
		Answer string `json:"answer"`
		Set    Set    `json:"set"`
	}
	if err := s.decode(ctx, &answer, setCreateSQL, setCreateAtSQL, string(doc)); err != nil {
		return Set{}, fmt.Errorf("store: writing the set %s: %w", r.Name, err)
	}
	switch answer.Answer {
	case "created":
		return answer.Set, nil
	case "conflict":
		return Set{}, fault.New(fault.Conflict, "the owner already has a set named %s", r.Name)
	}
	return Set{}, fmt.Errorf("store: writing the set %s answered %q", r.Name, answer.Answer)
}

// Set returns a set whoever owns it, or set_not_found.
func (s *Store) Set(ctx context.Context, id string) (Set, error) {
	var out *Set
	if err := s.decode(ctx, &out, setSQL, "", id); err != nil {
		return Set{}, fmt.Errorf("store: reading the set %s: %w", id, err)
	}
	if out == nil {
		return Set{}, fault.New(fault.SetNotFound, "no set %s", id)
	}
	return *out, nil
}

// Sets returns the sets of the owners, newest first: at most limit, after
// the set whose id is after. Nil owners is every owner's, and an empty list
// is nobody's. An empty name matches every set. more says others follow.
func (s *Store) Sets(ctx context.Context, owners []string, name, after string, limit int) (out []Set, more bool, err error) {
	// A nil list encodes as the JSON null, which the statement reads as
	// every owner's.
	scope, err := json.Marshal(owners)
	if err != nil {
		return nil, false, fmt.Errorf("store: encoding the owners of a list: %w", err)
	}
	// One row past the limit says whether a next page exists.
	if err := s.decode(ctx, &out, setsSQL, "", string(scope), name, after, limit+1); err != nil {
		return nil, false, fmt.Errorf("store: listing sets: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// SetParses returns the parses of a set, newest first, each with its
// extraction under the set's name: at most limit, after the parse whose id
// is after. more says others follow.
func (s *Store) SetParses(ctx context.Context, setID, after string, limit int) (out []Member, more bool, err error) {
	if err := s.decode(ctx, &out, setParsesSQL, "", setID, after, limit+1); err != nil {
		return nil, false, fmt.Errorf("store: listing the parses of the set %s: %w", setID, err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// SetJoin is a parse to add to a set.
type SetJoin struct {
	Set   string `json:"set"`
	Parse string `json:"parse"`

	// Deadline is how long an extraction the join asks has from when its
	// task is queued, as FieldRequest's.
	Deadline time.Duration `json:"-"`
}

// setJoin is a SetJoin as lectio_set_join reads it.
type setJoin struct {
	SetJoin
	DeadlineMS int64 `json:"deadline_ms"`
	MaxFields  int   `json:"max_fields"`
	MaxParses  int   `json:"max_parses"`
}

// JoinSet adds a parse to a set, which asks the set's extraction of it
// unless the parse holds one the set's definition, equal as a JSON value,
// already filled. asked
// says whether an extraction was asked; replaced is the object key of the
// result an asking replaced, empty when there was none, for the caller to
// remove. A set that is not there is refused with set_not_found; a parse
// that is not there, or is another owner's, with parse_not_found; and with
// conflict a parse whose extraction of the name has not ended, a parse that
// holds MaxFields extractions, and a set that holds MaxSetParses parses.
func (s *Store) JoinSet(ctx context.Context, j SetJoin) (asked bool, replaced string, err error) {
	if j.Set == "" || j.Parse == "" {
		return false, "", fault.New(fault.InvalidRequest, "a join has a set and a parse")
	}
	if j.Deadline < time.Millisecond {
		return false, "", fault.New(fault.InvalidRequest, "a join has a deadline")
	}
	doc, err := json.Marshal(setJoin{SetJoin: j, DeadlineMS: j.Deadline.Milliseconds(), MaxFields: MaxFields, MaxParses: MaxSetParses})
	if err != nil {
		return false, "", fmt.Errorf("store: encoding the join of %s to %s: %w", j.Parse, j.Set, err)
	}
	var answer struct {
		Answer string  `json:"answer"`
		Output *string `json:"output"`
	}
	if err := s.decode(ctx, &answer, setJoinSQL, setJoinAtSQL, string(doc)); err != nil {
		return false, "", fmt.Errorf("store: adding %s to the set %s: %w", j.Parse, j.Set, err)
	}
	switch answer.Answer {
	case "no_set":
		return false, "", fault.New(fault.SetNotFound, "no set %s", j.Set)
	case "missing":
		// As Parse answers a parse that is not there: a parse of another
		// owner is not told apart from one that is not there.
		return false, "", fault.New(fault.ParseNotFound, "no parse %s", j.Parse)
	case "busy":
		return false, "", fault.New(fault.Conflict, "the extraction of parse %s under the set's name has not ended", j.Parse)
	case "full":
		return false, "", fault.New(fault.Conflict, "parse %s holds %d extractions, which is as many as a parse may", j.Parse, MaxFields)
	case "full_set":
		return false, "", fault.New(fault.Conflict, "set %s holds %d parses, which is as many as a set may", j.Set, MaxSetParses)
	case "joined":
		return false, "", nil
	case "asked":
		if answer.Output != nil {
			return true, *answer.Output, nil
		}
		return true, "", nil
	}
	return false, "", fmt.Errorf("store: adding %s to the set %s answered %q", j.Parse, j.Set, answer.Answer)
}

// setUpdate is a new definition as lectio_set_update reads it.
type setUpdate struct {
	Set        string `json:"set"`
	Request    string `json:"request"`
	Pin        string `json:"pin,omitempty"`
	DeadlineMS int64  `json:"deadline_ms"`
}

// UpdateSet replaces a set's definition and asks every extraction of the set
// again with it, in one transaction: one whose parse is still read has its
// request replaced, and one that has ended is asked again. A parse that is
// being removed is left as it is. The definition the set holds, byte for
// byte and with the same extractor, changes nothing; one that orders the
// members otherwise is a change. It returns the
// set as that leaves it and the object keys of the results the askings
// replaced, for the caller to remove. A set that is not there is refused
// with set_not_found, and a set with an extraction whose task is queued or
// running with conflict, which changes nothing.
func (s *Store) UpdateSet(ctx context.Context, r SetRequest, deadline time.Duration) (set Set, replaced []string, err error) {
	switch {
	case r.ID == "" || r.Request == "":
		return Set{}, nil, fault.New(fault.InvalidRequest, "a change of a set has the set and a request")
	case deadline < time.Millisecond:
		return Set{}, nil, fault.New(fault.InvalidRequest, "a change of a set has a deadline")
	}
	doc, err := json.Marshal(setUpdate{Set: r.ID, Request: r.Request, Pin: r.Pin, DeadlineMS: deadline.Milliseconds()})
	if err != nil {
		return Set{}, nil, fmt.Errorf("store: encoding the change of the set %s: %w", r.ID, err)
	}
	var answer struct {
		Answer  string   `json:"answer"`
		Set     Set      `json:"set"`
		Outputs []string `json:"outputs"`
	}
	if err := s.decode(ctx, &answer, setUpdateSQL, setUpdateAtSQL, string(doc)); err != nil {
		return Set{}, nil, fmt.Errorf("store: changing the set %s: %w", r.ID, err)
	}
	switch answer.Answer {
	case "no_set":
		return Set{}, nil, fault.New(fault.SetNotFound, "no set %s", r.ID)
	case "busy":
		return Set{}, nil, fault.New(fault.Conflict, "an extraction of the set %s is queued or running", r.ID)
	case "updated":
		return answer.Set, answer.Outputs, nil
	}
	return Set{}, nil, fmt.Errorf("store: changing the set %s answered %q", r.ID, answer.Answer)
}

// LeaveSet takes a parse out of a set. Its extraction under the set's name
// stays on the parse, with its values, as an extraction of no set. A set
// that is not there is refused with set_not_found; a parse that is not
// there, is another owner's or holds no extraction of the set with
// parse_not_found; and a parse whose extraction of the set has not ended
// with conflict.
func (s *Store) LeaveSet(ctx context.Context, set, parse string) error {
	answer, err := s.text(ctx, setLeaveSQL, setLeaveAtSQL, set, parse)
	if err != nil {
		return fmt.Errorf("store: taking %s out of the set %s: %w", parse, set, err)
	}
	switch answer {
	case "no_set":
		return fault.New(fault.SetNotFound, "no set %s", set)
	case "missing":
		// As JoinSet answers it: a parse of another owner is not told apart
		// from one that is not there.
		return fault.New(fault.ParseNotFound, "no parse %s", parse)
	case "not_member":
		return fault.New(fault.ParseNotFound, "parse %s is not in the set %s", parse, set)
	case "busy":
		return fault.New(fault.Conflict, "the extraction of parse %s under the set's name has not ended", parse)
	case "left":
		return nil
	}
	return fmt.Errorf("store: taking %s out of the set %s answered %q", parse, set, answer)
}

// DeleteSet removes a set. Its extractions stay on their parses as
// extractions of no set. A set that is not there is refused with
// set_not_found.
func (s *Store) DeleteSet(ctx context.Context, id string) error {
	answer, err := s.text(ctx, setDeleteSQL, "", id)
	switch {
	case err != nil:
		return fmt.Errorf("store: deleting the set %s: %w", id, err)
	case answer == "missing":
		return fault.New(fault.SetNotFound, "no set %s", id)
	}
	return nil
}
