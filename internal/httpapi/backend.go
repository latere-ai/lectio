// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
)

// Backend is what the handlers serve from: where files and parses are kept,
// how a parse is started and stopped, and where its results are read. The
// handlers hold the contract, its checks and its shapes, and are the same
// over every backend. Two exist: Memory, the development server's, and
// internal/durable, over Postgres and an object store.
//
// A backend decides nothing about who may do what. A handler reads a file
// or a parse by its id alone, asks whether its caller may act on it, and
// then acts in the name of the stored owner: every method that takes an
// owner is given the owner the object is recorded under, and serves that
// owner alone.
type Backend interface {
	// PutFile stores a file whose bytes are in f.Data. The same bytes are
	// one file per owner: when the owner already has them, that file is
	// returned and created is false.
	PutFile(ctx context.Context, f store.File) (stored store.File, created bool, err error)
	// File returns a file, without its bytes, whoever owns it.
	File(ctx context.Context, id string) (store.File, error)
	// DeleteFile removes an owner's file. One that a parse which has not
	// ended reads is refused with not_terminal.
	DeleteFile(ctx context.Context, owner, id string) error

	// Submit stores a parse and starts it. With an idempotency key it is
	// safe to repeat: the same key with the same body digest returns the
	// parse the first call made, and with another it is refused with
	// idempotency_conflict. a is what the parse is admitted with.
	Submit(ctx context.Context, p store.Parse, a store.Admission, key, digest string) (stored store.Parse, created bool, err error)
	// Parse returns a parse as it stands, whoever owns it.
	Parse(ctx context.Context, id string) (store.Parse, error)
	// Parses returns the parses of the owners that match, newest first: at
	// most limit, after the parse whose id is after. Nil owners is every
	// owner's, and an empty list is nobody's. more says others follow.
	Parses(ctx context.Context, owners []string, f store.Filter, after string, limit int) (out []store.Parse, more bool, err error)
	// Cancel stops an owner's parse and returns it. One that has ended is
	// refused with already_terminal.
	Cancel(ctx context.Context, owner, id string) (store.Parse, error)
	// Retry queues again the pages of an owner's parse that failed and
	// returns the parse as that leaves it: running, with only those pages
	// open. One that has not ended is refused with not_terminal, one with
	// no failed page to read again with conflict, and one whose group has
	// no room for it with queue_full or budget_exhausted.
	Retry(ctx context.Context, owner, id string) (store.Parse, error)
	// DeleteParse removes an owner's parse and everything it wrote. One
	// that has not ended is refused with not_terminal.
	DeleteParse(ctx context.Context, owner, id string) error
	// Wait returns when the parse has ended, when d has passed, or when ctx
	// ends, whichever is first.
	Wait(ctx context.Context, owner, id string, d time.Duration)

	// Events returns a parse as it stands, with the count of its changes,
	// and the pages of it that settled after the change after, oldest
	// first and at most limit of them, all as of one instant.
	Events(ctx context.Context, id string, after int64, limit int) (store.Events, error)

	// Summaries lists the pages of a parse that have a result, in page
	// order, each without its blocks.
	Summaries(ctx context.Context, p store.Parse) ([]document.PageSummary, error)
	// Page returns one page of a parse. ok is false for a page that has no
	// result yet.
	Page(ctx context.Context, p store.Parse, n int) (page document.Page, ok bool, err error)
	// Pages returns every page of a parse that has a result, with its
	// blocks, in page order.
	Pages(ctx context.Context, p store.Parse) ([]document.Page, error)
	// Image returns the image of a page that a reader saw. ok is false for
	// a page that has none.
	Image(ctx context.Context, p store.Parse, n int) (img render.Image, ok bool, err error)
	// Document returns a parse's document index. ok is false until the
	// parse has one, which every parse that ended does.
	Document(ctx context.Context, p store.Parse) (doc document.Document, ok bool, err error)

	// Usage reads the meters: what was read and what it cost, summed by a
	// key over fixed intervals, in the order of the intervals and then of
	// the keys.
	Usage(ctx context.Context, q store.UsageQuery) ([]store.UsageSum, error)

	// Queue returns the queue as it stands: the groups with their
	// projects, and the readers' pools. Nil groups is every group that
	// holds a parse that has not ended, and a list is those groups, with
	// the pools as those groups see them.
	Queue(ctx context.Context, groups []string) (store.Queue, error)

	// Figures starts a run that describes the figures of a parse, and
	// FigureRun returns the run of a parse when one was started.
	// WaitFigures returns when the run has ended, when d has passed, or
	// when ctx ends.
	Figures(ctx context.Context, p store.Parse, opt run.FigureOptions) error
	FigureRun(ctx context.Context, parseID string) (run store.FigureRun, started bool, err error)
	WaitFigures(ctx context.Context, parseID string, d time.Duration)

	// CreateField asks an extraction of a parse and returns it as that
	// leaves it: pending. A name the parse already has is refused with
	// conflict, and an extractor that is not configured, or none when the
	// policy names none, with reader_not_found.
	CreateField(ctx context.Context, p store.Parse, f store.FieldRequest) (store.Field, error)
	// AskField asks an extraction of a parse again under its name, or for
	// the first time when the parse has none of the name, and returns it
	// pending. One that has not ended is refused with conflict, and an
	// extractor as CreateField refuses it.
	AskField(ctx context.Context, p store.Parse, f store.FieldRequest) (store.Field, error)
	// Field returns one extraction of a parse. ok is false when the parse
	// has none of the name.
	Field(ctx context.Context, p store.Parse, name string) (f store.Field, ok bool, err error)
	// Fields returns the extractions of a parse, by name.
	Fields(ctx context.Context, p store.Parse) ([]store.Field, error)
}

// Memory is the Backend of a development server: everything is kept in the
// process by a memory store, and parses are run by an in-process runner.
// Nothing survives a restart, so nothing expires either. Of an Admission
// it holds the most pages a parse may select. It has one queue for every
// owner and no group: a server over it refuses an allow that sets a bound
// of a group, a budget of pages or a retention, and does not pass it in
// silence (access.New).
type Memory struct {
	Store  *store.Memory
	Runner *run.Runner
}

// PutFile stores the file in the process.
func (m *Memory) PutFile(_ context.Context, f store.File) (store.File, bool, error) {
	stored, created := m.Store.PutFile(f)
	return stored, created, nil
}

// File returns a file whoever owns it.
func (m *Memory) File(_ context.Context, id string) (store.File, error) {
	return m.Store.FileByID(id)
}

// DeleteFile removes an owner's file.
func (m *Memory) DeleteFile(_ context.Context, owner, id string) error {
	return m.Store.DeleteFile(owner, id)
}

// Submit stores the parse and hands it to the runner. A parse outlives the
// request that submitted it, so it runs under the runner's context.
func (m *Memory) Submit(_ context.Context, p store.Parse, a store.Admission, key, digest string) (store.Parse, bool, error) {
	p.MaxPages = a.MaxPages
	stored, created, err := m.Store.CreateParse(p, key, digest)
	if err != nil {
		return store.Parse{}, false, err
	}
	if created {
		m.Runner.Submit(stored) //nolint:contextcheck
	}
	return stored, created, nil
}

// Parse returns a parse whoever owns it.
func (m *Memory) Parse(_ context.Context, id string) (store.Parse, error) {
	return m.Store.ParseByID(id)
}

// Parses lists the parses of the owners.
func (m *Memory) Parses(_ context.Context, owners []string, f store.Filter, after string, limit int) ([]store.Parse, bool, error) {
	out, more := m.Store.ListParses(owners, f, after, limit)
	return out, more, nil
}

// Cancel marks the parse and stops the runner's work on it.
func (m *Memory) Cancel(_ context.Context, owner, id string) (store.Parse, error) {
	p, err := m.Store.Parse(owner, id)
	if err != nil {
		return store.Parse{}, err
	}
	if p.Terminal() {
		return store.Parse{}, fault.New(fault.AlreadyTerminal, "parse %s is %s", p.ID, p.State)
	}
	if p, err = m.Store.UpdateParse(p.ID, func(p *store.Parse) { p.CancelRequested = true }); err != nil {
		return store.Parse{}, err
	}
	m.Runner.Cancel(p.ID)
	return p, nil
}

// undurable is the answer of an operation that is built over the task
// store, which a development server does not have.
func undurable(what string) error {
	return fault.New(fault.NotImplemented, "a development server keeps no task store, and %s is built over one", what)
}

// Retry is not built in a development server: its runner drives a parse
// once, from its file to its document, and keeps nothing of one that ended
// to queue again.
func (m *Memory) Retry(context.Context, string, string) (store.Parse, error) {
	return store.Parse{}, undurable("a retry")
}

// Events is not built in a development server: the events of a parse are
// read from the rows of the task store, which number its changes.
func (m *Memory) Events(context.Context, string, int64, int) (store.Events, error) {
	return store.Events{}, undurable("the event stream")
}

// Usage is not built in a development server: the meters are written by
// the task store as tasks settle.
func (m *Memory) Usage(context.Context, store.UsageQuery) ([]store.UsageSum, error) {
	return nil, undurable("the meter")
}

// Queue is not built in a development server: its runner has one queue
// for every owner, no group and no pool.
func (m *Memory) Queue(context.Context, []string) (store.Queue, error) {
	return store.Queue{}, undurable("the view of the queue")
}

// CreateField is not built in a development server: an extraction is a
// task of the task store, which holds what it has between 2 of its calls.
func (m *Memory) CreateField(context.Context, store.Parse, store.FieldRequest) (store.Field, error) {
	return store.Field{}, undurable("an extraction")
}

// AskField is not built in a development server, as CreateField is not.
func (m *Memory) AskField(context.Context, store.Parse, store.FieldRequest) (store.Field, error) {
	return store.Field{}, undurable("an extraction")
}

// Field is not built in a development server, as CreateField is not.
func (m *Memory) Field(context.Context, store.Parse, string) (store.Field, bool, error) {
	return store.Field{}, false, undurable("an extraction")
}

// Fields is not built in a development server, as CreateField is not.
func (m *Memory) Fields(context.Context, store.Parse) ([]store.Field, error) {
	return nil, undurable("an extraction")
}

// DeleteParse removes an owner's parse and what it wrote.
func (m *Memory) DeleteParse(_ context.Context, owner, id string) error {
	return m.Store.DeleteParse(owner, id)
}

// Wait waits on the runner, which knows when a parse it runs has ended.
func (m *Memory) Wait(ctx context.Context, _, id string, d time.Duration) {
	await(ctx, m.Runner.Done(id), d)
}

// await returns when done is closed, d has passed, or ctx ends.
func await(ctx context.Context, done <-chan struct{}, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Summaries lists the pages the store holds for the parse.
func (m *Memory) Summaries(_ context.Context, p store.Parse) ([]document.PageSummary, error) {
	read := m.Store.Pages(p.ID)
	out := make([]document.PageSummary, len(read))
	for i, page := range read {
		out[i] = page.Summary()
	}
	return out, nil
}

// Page returns one page the store holds.
func (m *Memory) Page(_ context.Context, p store.Parse, n int) (document.Page, bool, error) {
	page, ok := m.Store.Page(p.ID, n)
	return page, ok, nil
}

// Pages returns the pages the store holds for the parse.
func (m *Memory) Pages(_ context.Context, p store.Parse) ([]document.Page, error) {
	return m.Store.Pages(p.ID), nil
}

// Image returns the image the store holds for a page.
func (m *Memory) Image(_ context.Context, p store.Parse, n int) (render.Image, bool, error) {
	img, ok := m.Store.Image(p.ID, n)
	return img, ok && len(img.Data) > 0, nil
}

// Document returns the document the runner assembled.
func (m *Memory) Document(_ context.Context, p store.Parse) (document.Document, bool, error) {
	doc, ok := m.Store.Document(p.ID)
	return doc, ok, nil
}

// Figures starts a run of the runner. A run outlives the request that
// started it, so it runs under the runner's context.
func (m *Memory) Figures(_ context.Context, p store.Parse, opt run.FigureOptions) error {
	_, err := m.Runner.Figures(p, opt) //nolint:contextcheck
	return err
}

// FigureRun returns the run the store holds for the parse.
func (m *Memory) FigureRun(_ context.Context, parseID string) (store.FigureRun, bool, error) {
	current, started := m.Store.FigureRun(parseID)
	return current, started, nil
}

// WaitFigures waits on the runner.
func (m *Memory) WaitFigures(ctx context.Context, parseID string, d time.Duration) {
	await(ctx, m.Runner.FiguresDone(parseID), d)
}
