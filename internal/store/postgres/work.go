// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/tasks"
)

// Work on a parse that has ended: an extraction, and a run that describes
// its figures (specs/003-api.md, specs/011-structured-extraction.md). Each
// is a task of the parse, dispatched and admitted as a page is, and what it
// produces is recorded on a row of its own.

// MaxFields is the most extractions one parse holds. An extraction reads no
// page, so no budget of pages bounds how many a caller asks for; this does.
const MaxFields = 64

const (
	fieldCreateSQL   = `SELECT lectio_field_create($1)`
	fieldCreateAtSQL = `SELECT lectio_field_create($1, $2)`
	fieldSQL         = `SELECT lectio_field($1, $2)`
	fieldsSQL        = `SELECT lectio_fields($1)`

	figuresStartSQL   = `SELECT lectio_figures_start($1)`
	figuresStartAtSQL = `SELECT lectio_figures_start($1, $2)`
	figuresSQL        = `SELECT lectio_figures($1)`
)

// The states of an extraction. It is pending from its request until its
// task ends, whether it waits for its parse to end, for its turn, or for a
// model.
const (
	FieldPending   = "pending"
	FieldSucceeded = "succeeded"
	FieldFailed    = "failed"
)

// FieldRequest is an extraction to queue.
type FieldRequest struct {
	Parse string `json:"parse"`
	// Name identifies the extraction within its parse.
	Name string `json:"name"`

	// Request is what the model is asked, a tasks.Field as one JSON
	// document. It is kept and handed to the task as text, so a schema
	// reaches the model with its members in the order its caller wrote
	// them.
	Request string `json:"request"`

	// Pin is the extractor the request named, when it named one. The task
	// waits for that extractor and is never run by another.
	Pin string `json:"pin,omitempty"`

	// Deadline is how long the extraction has from when its task is queued,
	// which is the request for a parse that has ended and the parse's end
	// for one that has not. Nothing waits without bound, so it is required.
	Deadline time.Duration `json:"-"`
}

// fieldRequest is a FieldRequest as lectio_field_create reads it.
type fieldRequest struct {
	FieldRequest
	DeadlineMS int64 `json:"deadline_ms"`
	MaxFields  int   `json:"max_fields"`
}

// CreateField writes an extraction of a parse and, when the parse has
// ended, queues its task in the same transaction. One asked while the parse
// runs has no task until the parse ends. A parse that is not there is
// refused with parse_not_found, a name the parse already has with conflict,
// and a parse that holds MaxFields extractions with conflict too.
func (s *Store) CreateField(ctx context.Context, f FieldRequest) error {
	switch {
	case f.Parse == "" || f.Name == "" || f.Request == "":
		return fault.New(fault.InvalidRequest, "an extraction has a parse, a name and a request")
	case f.Deadline < time.Millisecond:
		return fault.New(fault.InvalidRequest, "an extraction has a deadline")
	}
	doc, err := json.Marshal(fieldRequest{FieldRequest: f, DeadlineMS: f.Deadline.Milliseconds(), MaxFields: MaxFields})
	if err != nil {
		return fmt.Errorf("store: encoding the extraction %s of %s: %w", f.Name, f.Parse, err)
	}
	answer, err := s.text(ctx, fieldCreateSQL, fieldCreateAtSQL, string(doc))
	switch {
	case err != nil:
		return fmt.Errorf("store: writing the extraction %s of %s: %w", f.Name, f.Parse, err)
	case answer == "missing":
		return fault.New(fault.ParseNotFound, "no parse %s", f.Parse)
	case answer == "conflict":
		return fault.New(fault.Conflict, "parse %s already has an extraction named %s", f.Parse, f.Name)
	case answer == "full":
		return fault.New(fault.Conflict, "parse %s holds %d extractions, which is as many as a parse may", f.Parse, MaxFields)
	}
	return nil
}

// Field is one extraction as the store holds it. The result's bytes are in
// the object store under Output.
type Field struct {
	Parse string `json:"parse_id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Pin   string `json:"pin"`

	// Request is what the extraction was asked with, the tasks.Field its
	// request wrote, as the text it was kept as.
	Request string `json:"request"`

	// DeadlineAt is when the extraction fails for time. It is nil while the
	// extraction waits for its parse to end: its time runs from when its
	// task is queued.
	DeadlineAt *time.Time `json:"deadline_at"`

	// Output is the object key of the result, for an extraction that
	// succeeded. Result is what its task said of how it was filled, kept as
	// it came, and Error why it failed.
	Output string          `json:"output"`
	Result json.RawMessage `json:"result"`
	Error  *tasks.Error    `json:"error"`

	// Calls and the tokens are what every claim of its task used, the calls
	// that failed or were told to wait included.
	Calls        int   `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Field returns one extraction of a parse. ok is false when the parse has
// none of the name.
func (s *Store) Field(ctx context.Context, parseID, name string) (f Field, ok bool, err error) {
	var out *Field
	if err := s.decode(ctx, &out, fieldSQL, "", parseID, name); err != nil {
		return Field{}, false, fmt.Errorf("store: reading the extraction %s of %s: %w", name, parseID, err)
	}
	if out == nil {
		return Field{}, false, nil
	}
	return *out, true, nil
}

// Fields returns the extractions of a parse, by name.
func (s *Store) Fields(ctx context.Context, parseID string) ([]Field, error) {
	var out []Field
	if err := s.decode(ctx, &out, fieldsSQL, "", parseID); err != nil {
		return nil, fmt.Errorf("store: reading the extractions of %s: %w", parseID, err)
	}
	return out, nil
}

// The states of a run that describes a parse's figures, and of one figure
// in the last run that took it.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"

	FigurePending   = "pending"
	FigureSucceeded = "succeeded"
	FigureFailed    = "failed"
)

// FigureAsk is one figure a run sets out to describe.
type FigureAsk struct {
	// Ref is the ref of the figure's block, and Page the page it is on.
	Ref  string `json:"ref"`
	Page int    `json:"page"`
	// PageKey is the object key of the stored result of the figure's page.
	PageKey string `json:"page_key"`
	// FigureKey names what describing the figure means, so that a
	// description of it is kept and taken by a later run of the same owner.
	// Empty keeps and takes nothing.
	FigureKey string `json:"figure_key,omitempty"`
}

// FigureStart is a run to begin.
type FigureStart struct {
	Parse string `json:"parse"`
	// Pin is the describer the request named, when it named one.
	Pin string `json:"pin,omitempty"`
	// Redo describes again a figure that has a description, and takes none
	// an earlier run kept.
	Redo bool `json:"redo"`
	// Deadline is how long the run has. The figures it has not described by
	// then are lost to it.
	Deadline time.Duration `json:"-"`
	// Figures are the figures to describe, in the order they are queued in.
	Figures []FigureAsk `json:"figures"`
}

// figureStart is a FigureStart as lectio_figures_start reads it.
type figureStart struct {
	FigureStart
	DeadlineMS int64 `json:"deadline_ms"`
}

// StartFigures begins a run that describes figures of a parse that has
// ended: the run, a row per figure and a task per figure, in one
// transaction. A parse that has not ended is refused with not_terminal, one
// whose earlier run is in flight with conflict, and a run of more figures
// than figures.MaxRun with too_many_pages. A run of no figure has ended
// when it is written.
func (s *Store) StartFigures(ctx context.Context, r FigureStart) error {
	switch {
	case r.Parse == "":
		return fault.New(fault.InvalidRequest, "a run has a parse")
	case r.Deadline < time.Millisecond:
		return fault.New(fault.InvalidRequest, "a run has a deadline")
	}
	if r.Figures == nil {
		r.Figures = []FigureAsk{}
	}
	// The statement writes a row and a task per figure under the lock the
	// exchange takes, so the bound on a run is held before it is sent.
	if err := figures.Bounded(len(r.Figures)); err != nil {
		return err
	}
	doc, err := json.Marshal(figureStart{FigureStart: r, DeadlineMS: r.Deadline.Milliseconds()})
	if err != nil {
		return fmt.Errorf("store: encoding the figure run of %s: %w", r.Parse, err)
	}
	answer, err := s.text(ctx, figuresStartSQL, figuresStartAtSQL, string(doc))
	switch {
	case err != nil:
		return fmt.Errorf("store: starting the figure run of %s: %w", r.Parse, err)
	case answer == "missing":
		return fault.New(fault.ParseNotFound, "no parse %s", r.Parse)
	case answer == "not_terminal":
		return fault.New(fault.NotTerminal, "parse %s has not ended", r.Parse)
	case answer == "conflict":
		return fault.New(fault.Conflict, "the figures of parse %s are being described", r.Parse)
	}
	return nil
}

// FigureRun is the run of a parse that describes its figures.
type FigureRun struct {
	State string `json:"state"`
	Redo  bool   `json:"redo"`

	// Total is how many figures the run set out to describe, and Open how
	// many of them have a task that has not ended. Done and Failed count
	// the ones that ended, and Reused those of Done that were taken from an
	// earlier description.
	Total  int `json:"total"`
	Open   int `json:"open"`
	Done   int `json:"done"`
	Failed int `json:"failed"`
	Reused int `json:"reused"`

	// Calls and the tokens are what the run's tasks used, the calls that
	// failed or were told to wait included.
	Calls        int   `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	StartedAt  time.Time  `json:"started_at"`
	DeadlineAt time.Time  `json:"deadline_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Figure is one figure a run took: how the last run that took it ended for
// it, and where its description is. The description's bytes are in the
// object store under Output, which a figure keeps from an earlier run when
// a later one lost it.
type Figure struct {
	Ref     string       `json:"ref"`
	Page    int          `json:"page"`
	PageKey string       `json:"page_key"`
	State   string       `json:"state"`
	Output  string       `json:"output"`
	Error   *tasks.Error `json:"error"`
}

// Figures is the run of a parse and the figures a run took, of one instant.
type Figures struct {
	Run     FigureRun `json:"run"`
	Figures []Figure  `json:"figures"`
}

// Figures returns the run of a parse that describes its figures, with the
// figures a run took, in page order. started is false for a parse no run
// was started for.
func (s *Store) Figures(ctx context.Context, parseID string) (out Figures, started bool, err error) {
	var got *Figures
	if err := s.decode(ctx, &got, figuresSQL, "", parseID); err != nil {
		return Figures{}, false, fmt.Errorf("store: reading the figures of %s: %w", parseID, err)
	}
	if got == nil {
		return Figures{}, false, nil
	}
	return *got, true, nil
}
