// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package durable is the API's backend over the durable control plane: rows
// in Postgres, bytes in an object store, and parses run by worker processes
// that claim their tasks from the task store. It holds no parse in memory,
// so any replica of the API answers any request and a restart changes
// nothing a caller sees. The design is specs/001-architecture.md,
// specs/002-object-model.md and specs/004-durable-tasks.md.
//
// A page is found through its task's row while its parse runs, and through
// the document index, which lists the key of every page, once assemble has
// written one. A parse that ended without assemble, because it was
// canceled, ran out of time or could not start, has no index: its document
// is made from its task rows when it is read.
package durable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
)

// DefaultPoll is how often a held submit looks whether its parse has ended.
const DefaultPoll = 100 * time.Millisecond

// Backend serves the API from the task store and the object store.
type Backend struct {
	Store   *postgres.Store
	Objects blob.Store

	// Readers and Chain are the configured readers and the routing
	// policy's order, as the workers have them. They name what a read of a
	// page means, which is what a kept page is found under.
	Readers map[string]reader.Reader
	Chain   []string

	// MaxDeadline is the deadline of a parse submitted with none, so
	// nothing waits without bound. Zero takes one hour.
	MaxDeadline time.Duration

	// Poll is how often a held submit reads its parse. Zero takes
	// DefaultPoll.
	Poll time.Duration

	// Log takes what a caller is not told. Nil takes slog.Default.
	Log *slog.Logger
}

func (b *Backend) log() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

// PutFile writes the bytes to the object store and then the file's row. The
// same bytes are one file per owner: when the owner has them, nothing is
// stored, and the file is kept for the retention of this upload from now.
// An upload that lost to another of the same bytes at the same instant
// removes the object it wrote.
func (b *Backend) PutFile(ctx context.Context, f store.File) (store.File, bool, error) {
	if have, ok, err := b.Store.FileByContent(ctx, f.Owner, f.SHA256); err != nil {
		return store.File{}, false, err
	} else if ok {
		if err := b.Store.KeepFile(ctx, have.ID, f.Retention); err != nil {
			return store.File{}, false, err
		}
		kept, err := b.Store.File(ctx, have.ID)
		if err != nil {
			return store.File{}, false, err
		}
		return viewFile(kept), false, nil
	}
	key := blob.SourceKey(f.Owner, f.SHA256, f.ID)
	if err := b.Objects.Put(ctx, key, f.Data, f.MediaType); err != nil {
		return store.File{}, false, fmt.Errorf("durable: storing the file: %w", err)
	}
	row, created, err := b.Store.InsertFile(ctx, postgres.File{
		ID: f.ID, Owner: f.Owner, Name: f.Name, Size: f.Size, SHA256: f.SHA256, MediaType: f.MediaType, Key: key,
		Retention: f.Retention,
	})
	if err != nil || !created {
		// The row was not written, so nothing names the object.
		if rmErr := b.Objects.Delete(ctx, key); rmErr != nil {
			b.log().WarnContext(ctx, "an object nothing names was left behind", "key", key, "error", rmErr)
		}
	}
	if err != nil {
		return store.File{}, false, err
	}
	return viewFile(row), created, nil
}

func viewFile(f postgres.File) store.File {
	return store.File{
		ID: f.ID, Owner: f.Owner, Name: f.Name, MediaType: f.MediaType, SHA256: f.SHA256, Size: f.Size,
		CreatedAt: f.CreatedAt, ExpiresAt: f.ExpiresAt,
	}
}

// File returns a file whoever owns it.
func (b *Backend) File(ctx context.Context, id string) (store.File, error) {
	f, err := b.Store.File(ctx, id)
	if err != nil {
		return store.File{}, err
	}
	return viewFile(f), nil
}

// DeleteFile removes an owner's file: the row is marked, so the file is
// gone for every caller, then the object is removed and then the row. A
// delete that stops halfway leaves a marked row that still names its
// object, and never an object nothing names.
func (b *Backend) DeleteFile(ctx context.Context, owner, id string) error {
	key, err := b.Store.DeleteFile(ctx, owner, id)
	if err != nil {
		return err
	}
	if err := b.Objects.Delete(ctx, key); err != nil {
		return fmt.Errorf("durable: removing the file's object: %w", err)
	}
	return b.Store.ForgetFile(ctx, id)
}

// Submit writes the parse and its first task in one transaction. The
// admission's group, project and bounds are the fair queue's, its pages for
// a day the group's budget, and its ceiling on pages and its retention the
// parse's own. The parse's deadline is the caller's or the longest one
// allowed.
func (b *Backend) Submit(ctx context.Context, p store.Parse, a store.Admission, key, digest string) (store.Parse, bool, error) {
	deadline := b.MaxDeadline
	if deadline <= 0 {
		deadline = time.Hour
	}
	if p.DeadlineAt != nil {
		deadline = p.DeadlineAt.Sub(p.CreatedAt)
	}
	sub := postgres.Submission{
		Parse: p.ID, Owner: p.Owner,
		Group: a.Group, Project: a.Project, Weight: a.Weight, ProjectWeight: a.ProjectWeight,
		MaxRunning: a.MaxRunning, MaxQueued: a.MaxQueued, MaxPriority: a.MaxPriority,
		PagesPerDay: a.PagesPerDay, MaxPages: a.MaxPages, Retention: a.Retention,
		Priority: p.Priority, Pin: p.Reader, AllowFailedPages: p.AllowFailedPages, Deadline: deadline,
		File:    p.File,
		Options: postgres.ParseOptions{Pages: p.Pages, Languages: p.Languages, Reuse: p.Reuse},
		Labels:  p.Labels, IdempotencyKey: key, BodyDigest: digest, ReadBase: b.readBase(p),
	}
	if p.Class == store.ClassBatch {
		sub.Class = tasks.Batch
	}
	if p.Origin != nil {
		sub.Origin = &postgres.Origin{Store: p.Origin.Store, Path: p.Origin.Path, Version: p.Origin.Version}
	}
	id, created, err := b.Store.Submit(ctx, sub)
	if err != nil {
		return store.Parse{}, false, err
	}
	stored, err := b.Parse(ctx, id)
	return stored, created, err
}

// readBase names what reading a page of the parse means, less the page: the
// file's bytes, the languages hinted, and every reader that may come to read
// a page, each by the version it describes itself with. Two parses with the
// same base read the same, so a page one of them read whole need not be
// read by the other. A reader that names no version promises nothing about
// its results, and then there is no base.
func (b *Backend) readBase(p store.Parse) string {
	if p.ContentSHA == "" {
		return ""
	}
	chain := b.Chain
	if p.Reader != "" {
		chain = []string{p.Reader}
	}
	parts := []string{p.ContentSHA, strings.Join(p.Languages, ",")}
	for _, name := range chain {
		rd := b.Readers[name]
		if rd == nil {
			continue
		}
		version := rd.Describe().Version
		if version == "" {
			return ""
		}
		parts = append(parts, name+"="+version)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Parse returns a parse as its row stands, whoever owns it.
func (b *Backend) Parse(ctx context.Context, id string) (store.Parse, error) {
	row, err := b.Store.Parse(ctx, id)
	if err != nil {
		return store.Parse{}, err
	}
	return view(row), nil
}

// view is a parse's row as the API's parse. The stage is derived from where
// the graph of the parse stands.
func view(row postgres.Parse) store.Parse {
	p := store.Parse{
		ID: row.ID, Owner: row.Owner, State: row.State,
		File: row.File, Pages: row.Options.Pages, Reader: row.Pin, Languages: row.Options.Languages,
		Class: row.Class.String(), Priority: row.Priority, AllowFailedPages: row.AllowFailedPages,
		Reuse:      row.Options.Reuse,
		PagesTotal: row.PagesTotal, PagesDone: row.PagesDone, PagesFailed: row.PagesFailed, PagesReused: row.PagesReused,
		Usage:     document.Usage{Pages: row.PagesDone, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens},
		CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt, DeadlineAt: &row.DeadlineAt,
		IndexKey: row.Index,
	}
	if len(row.Labels) > 0 {
		p.Labels = row.Labels
	}
	if row.Origin != nil {
		p.Origin = &store.Origin{Store: row.Origin.Store, Path: row.Origin.Path, Version: row.Origin.Version}
	}
	if row.Error != nil {
		p.Error = &document.Error{Code: row.Error.Code, Detail: row.Error.Detail}
	}
	var m objects.Manifest
	if len(row.Manifest) > 0 && json.Unmarshal(row.Manifest, &m) == nil && m.MediaType != "" {
		p.Manifest, p.ManifestToken = &m.Manifest, m.Token
	}
	switch {
	case row.Terminal():
		p.Stage = store.StageDone
	case row.State == store.StateQueued:
		p.Stage = store.StageQueued
	case p.Manifest == nil:
		p.Stage = store.StagePreparing
	case row.PagesOpen > 0:
		p.Stage = store.StageReading
	default:
		p.Stage = store.StageAssembling
	}
	return p
}

// Parses lists the parses of the owners.
func (b *Backend) Parses(ctx context.Context, owners []string, f store.Filter, after string, limit int) ([]store.Parse, bool, error) {
	rows, more, err := b.Store.Parses(ctx, owners, postgres.Filter{State: f.State, File: f.File, OriginPath: f.OriginPath, Labels: f.Labels}, after, limit)
	if err != nil {
		return nil, false, err
	}
	out := make([]store.Parse, len(rows))
	for i, row := range rows {
		out[i] = view(row)
	}
	return out, more, nil
}

// Cancel stops an owner's parse in the request's own transaction: the parse
// and its tasks that had not settled are canceled when it returns.
func (b *Backend) Cancel(ctx context.Context, owner, id string) (store.Parse, error) {
	if _, err := b.Store.ParseOf(ctx, owner, id); err != nil {
		return store.Parse{}, err
	}
	if err := b.Store.Cancel(ctx, id); err != nil {
		return store.Parse{}, err
	}
	return b.Parse(ctx, id)
}

// Retry queues again the pages of an owner's parse that failed, in one
// transaction: the parse is running again when it returns, and its workers
// read those pages and no other.
func (b *Backend) Retry(ctx context.Context, owner, id string) (store.Parse, error) {
	if err := b.Store.Retry(ctx, owner, id); err != nil {
		return store.Parse{}, err
	}
	return b.Parse(ctx, id)
}

// Events returns what a stream of a parse's events is told: the parse's
// row and the rows of its pages that settled after a change, read in one
// statement. Nothing is kept between 2 calls, so any replica answers any
// stream, and one that started after another stopped answers the same.
func (b *Backend) Events(ctx context.Context, id string, after int64, limit int) (store.Events, error) {
	got, err := b.Store.Events(ctx, id, after, limit)
	if err != nil {
		return store.Events{}, err
	}
	out := store.Events{Parse: view(got.Parse), Seq: got.Parse.Events, Pages: make([]store.PageEvent, len(got.Pages))}
	for i, page := range got.Pages {
		out.Pages[i] = store.PageEvent{Seq: page.Event, Page: page.Page, State: string(page.State)}
		if page.Error != nil {
			out.Pages[i].Error = &document.Error{Code: page.Error.Code, Detail: page.Error.Detail}
		}
	}
	return out, nil
}

// DeleteParse removes an owner's parse that has ended: every object under
// its prefix first, then its rows, so a delete that stops halfway leaves a
// row to delete again and never an object nothing names.
func (b *Backend) DeleteParse(ctx context.Context, owner, id string) error {
	row, err := b.Store.ParseOf(ctx, owner, id)
	if err != nil {
		return err
	}
	if !row.Terminal() {
		return fault.New(fault.NotTerminal, "parse %s has not ended", id)
	}
	keys, err := b.Objects.List(ctx, blob.ParsePrefix(id))
	if err != nil {
		return fmt.Errorf("durable: listing the objects of %s: %w", id, err)
	}
	for _, key := range keys {
		if err := b.Objects.Delete(ctx, key); err != nil {
			return fmt.Errorf("durable: removing an object of %s: %w", id, err)
		}
	}
	return b.Store.DeleteParse(ctx, owner, id)
}

// Wait reads the parse's row until it has ended. There is no bus between
// the processes: whichever worker ends the parse, the row says so.
func (b *Backend) Wait(ctx context.Context, owner, id string, d time.Duration) {
	poll := b.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if row, err := b.Store.ParseOf(ctx, owner, id); err != nil || row.Terminal() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// located is where a page of a parse is: the key of its stored result, or
// the page itself when it has none, because it failed or was skipped.
type located struct {
	n    int
	key  string
	page *document.Page
}

// locate finds the pages of a parse that have a result, in page order:
// through the document index when the parse has one, through the manifest
// for a format prepare wrote the pages of, and through the task rows
// otherwise. only, when it is above 0, limits the answer to that page.
func (b *Backend) locate(ctx context.Context, p store.Parse, only int) ([]located, error) {
	var out []located
	keep := func(l located) {
		if only == 0 || l.n == only {
			out = append(out, l)
		}
	}
	switch {
	case p.IndexKey != "":
		idx, err := objects.GetIndex(ctx, b.Objects, p.IndexKey)
		if err != nil {
			return nil, fmt.Errorf("durable: reading the index of %s: %w", p.ID, err)
		}
		for _, entry := range idx.Keys {
			if entry.Key == "" {
				skipped := objects.Skipped(entry.Number)
				keep(located{n: entry.Number, page: &skipped})
				continue
			}
			keep(located{n: entry.Number, key: entry.Key})
		}
	case p.Manifest == nil:
		// The parse has not counted its pages, or ended before it could.
	case p.Manifest.Source == document.SourceNative:
		for _, n := range p.Manifest.Selected {
			keep(located{n: n, key: blob.PageKey(p.ID, n, p.ManifestToken)})
		}
	case only > 0:
		row, ok, err := b.Store.Task(ctx, p.ID, tasks.PageID(only))
		if err != nil {
			return nil, err
		}
		if l, has := fromRow(row, p.Terminal()); ok && has {
			out = append(out, l)
		}
	default:
		rows, err := b.Store.Tasks(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if l, has := fromRow(row, p.Terminal()); has {
				out = append(out, l)
			}
		}
		slices.SortFunc(out, func(x, y located) int { return x.n - y.n })
	}
	return out, nil
}

// fromRow reads where a page is from its task's row. A page whose task has
// not settled has no result yet, unless its parse has ended: it was then not
// reached, and is recorded as skipped.
func fromRow(row postgres.Task, ended bool) (located, bool) {
	n, ok := tasks.PageOf(row.ID)
	if !ok {
		return located{}, false
	}
	switch {
	case row.State == tasks.Succeeded:
		return located{n: n, key: row.Output}, true
	case row.State == tasks.Failed && row.Error != nil:
		failed := objects.Failed(n, row.Attempt, row.Error.Code, row.Error.Detail)
		return located{n: n, page: &failed}, true
	case ended:
		skipped := objects.Skipped(n)
		return located{n: n, page: &skipped}, true
	}
	return located{}, false
}

// read fetches the results of located pages, in their order.
func (b *Backend) read(ctx context.Context, at []located) ([]objects.Page, error) {
	var keys []string
	for _, l := range at {
		if l.page == nil {
			keys = append(keys, l.key)
		}
	}
	stored, err := objects.GetPages(ctx, b.Objects, keys)
	if err != nil {
		return nil, fmt.Errorf("durable: reading a page: %w", err)
	}
	out := make([]objects.Page, 0, len(at))
	for _, l := range at {
		if l.page != nil {
			out = append(out, objects.Page{Revision: objects.FirstReading, Page: *l.page})
			continue
		}
		out, stored = append(out, stored[0]), stored[1:]
	}
	return out, nil
}

// Summaries lists the pages of a parse that have a result. While the parse
// runs they are answered from the task rows, each of which holds what its
// page said of its result, so no object is read.
func (b *Backend) Summaries(ctx context.Context, p store.Parse) ([]document.PageSummary, error) {
	if p.IndexKey == "" && p.Manifest != nil && p.Manifest.Source != document.SourceNative && !p.Terminal() {
		rows, err := b.Store.Tasks(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		var out []document.PageSummary
		for _, row := range rows {
			l, has := fromRow(row, false)
			switch {
			case !has:
			case l.page != nil:
				out = append(out, l.page.Summary())
			default:
				var said objects.Summary
				if err := json.Unmarshal(row.Result, &said); err != nil && len(row.Result) > 0 {
					return nil, fmt.Errorf("durable: the row of page %d of %s holds no summary: %w", l.n, p.ID, err)
				}
				out = append(out, document.PageSummary{Number: l.n, State: document.PageSucceeded, Source: said.Source, Blocks: said.Blocks})
			}
		}
		slices.SortFunc(out, func(x, y document.PageSummary) int { return x.Number - y.Number })
		return out, nil
	}
	pages, err := b.Pages(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]document.PageSummary, len(pages))
	for i, page := range pages {
		out[i] = page.Summary()
	}
	return out, nil
}

// Page returns one page of a parse. ok is false for a page that has no
// result yet.
func (b *Backend) Page(ctx context.Context, p store.Parse, n int) (document.Page, bool, error) {
	stored, ok, err := b.stored(ctx, p, n)
	return stored.Page, ok, err
}

// stored returns one page's result as it is kept.
func (b *Backend) stored(ctx context.Context, p store.Parse, n int) (objects.Page, bool, error) {
	at, err := b.locate(ctx, p, n)
	if err != nil || len(at) == 0 {
		return objects.Page{}, false, err
	}
	stored, err := b.read(ctx, at)
	if err != nil {
		return objects.Page{}, false, err
	}
	return stored[0], true, nil
}

// Pages returns every page of a parse that has a result, with its blocks.
// For a parse that ended without assemble, the running headers and footers
// are marked here, as assemble would have marked them.
func (b *Backend) Pages(ctx context.Context, p store.Parse) ([]document.Page, error) {
	at, err := b.locate(ctx, p, 0)
	if err != nil {
		return nil, err
	}
	stored, err := b.read(ctx, at)
	if err != nil {
		return nil, err
	}
	out := make([]document.Page, len(stored))
	for i, s := range stored {
		out[i] = s.Page
	}
	if p.IndexKey == "" && p.Terminal() {
		assemble.Document(p.ID, out)
	}
	return out, nil
}

// Image returns the image of a page that a reader saw.
func (b *Backend) Image(ctx context.Context, p store.Parse, n int) (render.Image, bool, error) {
	stored, ok, err := b.stored(ctx, p, n)
	if err != nil || !ok || stored.Image == "" {
		return render.Image{}, false, err
	}
	data, mediaType, err := b.Objects.Get(ctx, stored.Image)
	switch {
	case errors.Is(err, blob.ErrNotFound):
		return render.Image{}, false, nil
	case err != nil:
		return render.Image{}, false, fmt.Errorf("durable: reading a page's image: %w", err)
	}
	return render.Image{Data: data, MediaType: mediaType}, len(data) > 0, nil
}

// Document returns a parse's document index: the one assemble wrote, or,
// for a parse that ended without assemble, one made from what it read.
func (b *Backend) Document(ctx context.Context, p store.Parse) (document.Document, bool, error) {
	if p.IndexKey != "" {
		idx, err := objects.GetIndex(ctx, b.Objects, p.IndexKey)
		if err != nil {
			return document.Document{}, false, fmt.Errorf("durable: reading the index of %s: %w", p.ID, err)
		}
		return idx.Document, true, nil
	}
	if !p.Terminal() {
		return document.Document{}, false, nil
	}
	at, err := b.locate(ctx, p, 0)
	if err != nil {
		return document.Document{}, false, err
	}
	stored, err := b.read(ctx, at)
	if err != nil {
		return document.Document{}, false, err
	}
	pages := make([]document.Page, len(stored))
	for i, s := range stored {
		pages[i] = s.Page
	}
	doc := assemble.Document(p.ID, pages)
	doc.Renderings = []string{"markdown", "text"}
	return doc, true, nil
}

// Figures is not built over the durable control plane: describing figures
// is a task of its own there, and that task does not exist yet.
func (b *Backend) Figures(context.Context, store.Parse, run.FigureOptions) error {
	return fault.New(fault.NotImplemented, "describing figures is not built in the durable server yet")
}

// FigureRun reports no run: none can be started.
func (b *Backend) FigureRun(context.Context, string) (store.FigureRun, bool, error) {
	return store.FigureRun{}, false, nil
}

// WaitFigures returns at once: there is no run to wait for.
func (b *Backend) WaitFigures(context.Context, string, time.Duration) {}
