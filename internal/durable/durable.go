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
//
// An extraction and a run that describes figures are tasks of a parse that
// has ended (specs/003-api.md, specs/011-structured-extraction.md). The
// backend queues them and reads what they wrote: a field's row and its
// object, and a figure's description, which is an object of its own and is
// written onto the figure's block whenever the block is read.
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
	"latere.ai/x/lectio/internal/extract"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
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

	// Describers and Extractors are what says what a figure shows and what
	// fills a schema, by the names of the readers they are configured
	// with, and DescribeChain and ExtractChain the routing policy's order
	// for each, as the workers have them. The API calls none of them: it
	// reads which exist, and the version a describer names itself with.
	Describers    map[string]reader.Describer
	DescribeChain []string
	Extractors    map[string]reader.Extractor
	ExtractChain  []string

	// MaxDeadline is the deadline of a parse submitted with none, and how
	// long an extraction and a figure run have, so nothing waits without
	// bound. Zero takes one hour.
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
	deadline := b.deadline()
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

// deadline is the longest work waits when its request names no time: a
// parse submitted with none, an extraction, and a figure run.
func (b *Backend) deadline() time.Duration {
	if b.MaxDeadline <= 0 {
		return time.Hour
	}
	return b.MaxDeadline
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
		IndexKey: row.Index, Fields: row.Fields, Described: row.Described,
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

// DeleteParse removes an owner's parse that has ended, in 3 steps: the
// work on it is stopped, every object under its prefix is removed, and
// then its rows. With its extractions and its figures dropped first no
// task of it is claimed after the objects were listed and every later
// settle of one is refused, so a task that still runs leaves nothing that
// a row names, and its worker removes what it wrote. With the rows removed
// last, a delete that stops halfway leaves a parse whose retention has
// ended, which the retention sweep removes, and never an object nothing
// names.
func (b *Backend) DeleteParse(ctx context.Context, owner, id string) error {
	row, err := b.Store.ParseOf(ctx, owner, id)
	if err != nil {
		return err
	}
	if !row.Terminal() {
		return fault.New(fault.NotTerminal, "parse %s has not ended", id)
	}
	if err := b.Store.DeleteParse(ctx, owner, id); err != nil {
		return err
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
	// The sweep may have removed the rows in between: they are gone either
	// way.
	_, err = b.Store.ExpireParse(ctx, id)
	return err
}

// Wait reads the parse's row until it has ended. There is no bus between
// the processes: whichever worker ends the parse, the row says so.
func (b *Backend) Wait(ctx context.Context, owner, id string, d time.Duration) {
	b.until(ctx, d, func() bool {
		row, err := b.Store.ParseOf(ctx, owner, id)
		return err != nil || row.Terminal()
	})
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

// Page returns one page of a parse, its figures with the descriptions they
// were given. ok is false for a page that has no result yet.
func (b *Backend) Page(ctx context.Context, p store.Parse, n int) (document.Page, bool, error) {
	stored, ok, err := b.stored(ctx, p, n)
	if err != nil || !ok {
		return document.Page{}, false, err
	}
	pages := []document.Page{stored.Page}
	if err := b.describe(ctx, p, pages); err != nil {
		return document.Page{}, false, err
	}
	return pages[0], true, nil
}

// describe writes onto the figures of pages the descriptions a run gave
// them. A page's stored result is as its reader left it, and a description
// is an object of its own, so every read of a block goes through here. A
// parse none of whose figures was described costs no statement.
func (b *Backend) describe(ctx context.Context, p store.Parse, pages []document.Page) error {
	if p.Described == 0 {
		return nil
	}
	got, started, err := b.Store.Figures(ctx, p.ID)
	if err != nil || !started {
		return err
	}
	keys := map[string]string{}
	for _, f := range got.Figures {
		keys[f.Ref] = f.Output
	}
	if err := objects.Describe(ctx, b.Objects, pages, keys); err != nil {
		return fmt.Errorf("durable: reading a figure's description: %w", err)
	}
	return nil
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

// Pages returns every page of a parse that has a result, with its blocks,
// its figures with the descriptions they were given. For a parse that
// ended without assemble, the running headers and footers are marked here,
// as assemble would have marked them.
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
	return out, b.describe(ctx, p, out)
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
		idx.Fields, err = b.filled(ctx, p)
		return idx.Document, err == nil, err
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
	doc.Fields, err = b.filled(ctx, p)
	return doc, err == nil, err
}

// filled names the extractions of a parse that were filled, which a
// document lists beside its renderings. A parse nobody asked an extraction
// of costs no statement.
func (b *Backend) filled(ctx context.Context, p store.Parse) ([]string, error) {
	if p.Fields == 0 {
		return nil, nil
	}
	rows, err := b.Store.Fields(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, row := range rows {
		if row.State == postgres.FieldSucceeded {
			names = append(names, row.Name)
		}
	}
	return names, nil
}

// Usage reads the meter the task store writes as tasks settle.
func (b *Backend) Usage(ctx context.Context, q store.UsageQuery) ([]store.UsageSum, error) {
	sums, err := b.Store.Usage(ctx, postgres.UsageQuery(q))
	if err != nil {
		return nil, err
	}
	out := make([]store.UsageSum, len(sums))
	for i, sum := range sums {
		out[i] = store.UsageSum(sum)
	}
	return out, nil
}

// Queue reads the queue from the counters the task store keeps for the
// claim, and the pools from their rows.
func (b *Backend) Queue(ctx context.Context, groups []string) (store.Queue, error) {
	got, err := b.Store.Queue(ctx, groups)
	if err != nil {
		return store.Queue{}, err
	}
	out := store.Queue{Groups: make([]store.QueueGroup, len(got.Groups)), Pools: make([]store.QueuePool, len(got.Pools))}
	for i, g := range got.Groups {
		out.Groups[i] = store.QueueGroup{
			Group: g.Group, Weight: g.Weight, MaxRunning: g.MaxRunning, MaxQueued: g.MaxQueued, Parses: g.Parses,
			Classes: queueClasses(g.Classes), Projects: make([]store.QueueProject, len(g.Projects)),
		}
		for j, p := range g.Projects {
			out.Groups[i].Projects[j] = store.QueueProject{Project: p.Project, Weight: p.Weight, Parses: p.Parses, Classes: queueClasses(p.Classes)}
		}
	}
	for i, p := range got.Pools {
		out.Pools[i] = store.QueuePool{
			Reader: p.Reader, MaxInFlight: p.MaxInFlight, InFlight: p.InFlight, Breaker: p.Breaker,
			Scopes: make([]store.QueueScope, len(p.Scopes)),
		}
		for j, sc := range p.Scopes {
			out.Pools[i].Scopes[j] = store.QueueScope(sc)
		}
	}
	return out, nil
}

// queueClasses names the classes of a queue as the API names them.
func queueClasses(classes []postgres.ClassQueue) []store.QueueClass {
	out := make([]store.QueueClass, len(classes))
	for i, c := range classes {
		out[i] = store.QueueClass{Class: c.Class.String(), Queued: c.Queued, Running: c.Running}
	}
	return out
}

// Figures starts a run that describes figures of a parse that has ended:
// every figure block on the selected pages that has a box, sits on a page
// read from an image, and has no description yet, or has one when the run
// describes again. The run and a task per figure are written in one
// transaction, so when this returns the figures are queued with the pages
// of every parse, in the parse's group. A run is refused while an earlier
// one of the parse is in flight, for a parse that has not ended, and when
// no describer is configured or the one named is not.
func (b *Backend) Figures(ctx context.Context, p store.Parse, opt run.FigureOptions) error {
	if !p.Terminal() {
		return fault.New(fault.NotTerminal, "parse %s has not ended", p.ID)
	}
	chain := b.DescribeChain
	if opt.Describer != "" {
		chain = []string{opt.Describer}
	}
	chain = slices.DeleteFunc(slices.Clone(chain), func(name string) bool { return b.Describers[name] == nil })
	if len(chain) == 0 {
		if opt.Describer != "" {
			return fault.New(fault.ReaderNotFound, "no describer is named %q", opt.Describer)
		}
		return fault.New(fault.ReaderNotFound, "no describer is configured")
	}

	at, err := b.locate(ctx, p, 0)
	if err != nil {
		return err
	}
	// Only the pages the run takes are read: a run over 2 pages of a long
	// document reads 2 objects.
	at = slices.DeleteFunc(at, func(l located) bool {
		return l.key == "" || (opt.Pages != nil && !slices.Contains(opt.Pages, l.n))
	})
	// A figure an earlier run described is known by the key of its
	// description, which the run's rows hold: no description is read to
	// find which figures have one.
	kept := map[string]string{}
	if p.Described > 0 && !opt.Redo {
		got, started, err := b.Store.Figures(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, f := range got.Figures {
			if started && f.Output != "" {
				kept[f.Ref] = f.Output
			}
		}
	}
	// The file's bytes name a description so that it is made once. A
	// file that is gone names none, and its figures are described.
	sha := ""
	if file, err := b.Store.File(ctx, p.File); err == nil {
		sha = file.SHA256
	} else if fault.CodeOf(err) != fault.FileNotFound {
		return err
	}
	start := postgres.FigureStart{Parse: p.ID, Pin: opt.Describer, Redo: opt.Redo, Deadline: b.deadline()}
	// The pages are read a batch at a time, and no further once the run
	// holds more figures than it may: a run that is refused has read the
	// pages up to the one that passed the bound, and not the document.
	for len(at) > 0 {
		batch := at[:min(len(at), figureBatch)]
		at = at[len(batch):]
		stored, err := b.read(ctx, batch)
		if err != nil {
			return err
		}
		for i, s := range stored {
			// A page that was not read from an image has nothing to cut a
			// figure from.
			if s.Image == "" {
				continue
			}
			for _, f := range figures.Of(s.Page, opt.Redo) {
				if kept[f.Ref] != "" {
					continue
				}
				start.Figures = append(start.Figures, postgres.FigureAsk{
					Ref: f.Ref, Page: f.Page, PageKey: batch[i].key, FigureKey: figures.Key(sha, f, p.Languages, chain, b.Describers),
				})
			}
		}
		if err := figures.Bounded(len(start.Figures)); err != nil {
			return err
		}
	}
	return b.Store.StartFigures(ctx, start)
}

// figureBatch is how many pages a request that starts a run reads at a
// time while it finds the run's figures.
const figureBatch = 16

// FigureRun returns the run of a parse that describes its figures, when
// one was started: what the task store counted of it, and why each figure
// it lost was lost.
func (b *Backend) FigureRun(ctx context.Context, parseID string) (store.FigureRun, bool, error) {
	got, started, err := b.Store.Figures(ctx, parseID)
	if err != nil || !started {
		return store.FigureRun{}, false, err
	}
	out := store.FigureRun{
		State: got.Run.State, Total: got.Run.Total, Done: got.Run.Done, Failed: got.Run.Failed, Reused: got.Run.Reused,
		Usage:     document.Usage{InputTokens: got.Run.InputTokens, OutputTokens: got.Run.OutputTokens},
		StartedAt: got.Run.StartedAt, FinishedAt: got.Run.FinishedAt,
	}
	for _, f := range got.Figures {
		if f.State == postgres.FigureFailed && f.Error != nil {
			if out.Failures == nil {
				out.Failures = map[string]document.Error{}
			}
			out.Failures[f.Ref] = document.Error{Code: f.Error.Code, Detail: f.Error.Detail}
		}
	}
	return out, true, nil
}

// WaitFigures reads the run's row until the run has ended. There is no bus
// between the processes: whichever worker describes the last figure, the
// row says so.
func (b *Backend) WaitFigures(ctx context.Context, parseID string, d time.Duration) {
	b.until(ctx, d, func() bool {
		got, started, err := b.Store.Figures(ctx, parseID)
		return err != nil || !started || got.Run.State != postgres.RunRunning
	})
}

// until reads a condition at the poll interval until it holds, d has
// passed, or ctx ends.
func (b *Backend) until(ctx context.Context, d time.Duration, ended func() bool) {
	poll := b.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for !ended() {
		select {
		case <-tick.C:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// CreateField asks an extraction of a parse: its row, and its task at once
// when the parse has ended. One asked of a parse that runs waits for the
// parse to end. The extractor is the one the request names, or the first
// of the routing policy's extract chain that has room when its task is
// claimed.
func (b *Backend) CreateField(ctx context.Context, p store.Parse, f store.FieldRequest) (store.Field, error) {
	if err := b.extractorFor(f); err != nil {
		return store.Field{}, err
	}
	asked, err := fieldRequest(p, f, b.deadline())
	if err != nil {
		return store.Field{}, err
	}
	if err := b.Store.CreateField(ctx, asked); err != nil {
		return store.Field{}, err
	}
	return store.Field{Name: f.Name, State: store.FieldPending, Schema: f.Schema, Instructions: f.Instructions}, nil
}

// extractorFor refuses a request that names an extractor that is not
// configured, or names none when the policy's extract chain holds none.
func (b *Backend) extractorFor(f store.FieldRequest) error {
	switch {
	case f.Extractor != "" && b.Extractors[f.Extractor] == nil:
		return fault.New(fault.ReaderNotFound, "no extractor is named %q", f.Extractor)
	case f.Extractor == "" && !slices.ContainsFunc(b.ExtractChain, func(name string) bool { return b.Extractors[name] != nil }):
		return fault.New(fault.ReaderNotFound, "no extractor is configured")
	}
	return nil
}

// AskField asks an extraction of a parse again under its name, after it has
// ended, or as a new one when the parse has none of the name. The result the
// asking replaced is removed: nothing names it any longer. A removal that
// fails leaves the object to the parse's own removal, and is logged.
func (b *Backend) AskField(ctx context.Context, p store.Parse, f store.FieldRequest) (store.Field, error) {
	if err := b.extractorFor(f); err != nil {
		return store.Field{}, err
	}
	asked, err := fieldRequest(p, f, b.deadline())
	if err != nil {
		return store.Field{}, err
	}
	replaced, err := b.Store.AskFieldAgain(ctx, asked)
	if errors.Is(err, postgres.ErrNoField) {
		return b.CreateField(ctx, p, f)
	}
	if err != nil {
		return store.Field{}, err
	}
	if replaced != "" {
		if err := b.Objects.Delete(ctx, replaced); err != nil {
			b.log().WarnContext(ctx, "the result an asking replaced was not removed", "parse", p.ID, "field", f.Name, "err", err)
		}
	}
	return store.Field{Name: f.Name, State: store.FieldPending, Schema: f.Schema, Instructions: f.Instructions}, nil
}

// fieldRequest is an extraction's request as the store keeps it.
func fieldRequest(p store.Parse, f store.FieldRequest, deadline time.Duration) (postgres.FieldRequest, error) {
	request, err := json.Marshal(tasks.Field{Schema: f.Schema, Instructions: f.Instructions, Citations: f.Citations})
	if err != nil {
		return postgres.FieldRequest{}, fault.New(fault.InvalidSchema, "the schema is not JSON")
	}
	return postgres.FieldRequest{Parse: p.ID, Name: f.Name, Request: string(request), Pin: f.Extractor, Deadline: deadline}, nil
}

// Field returns one extraction of a parse: its row, and for one that was
// filled its object and its citations from the object store.
func (b *Backend) Field(ctx context.Context, p store.Parse, name string) (store.Field, bool, error) {
	row, ok, err := b.Store.Field(ctx, p.ID, name)
	if err != nil || !ok {
		return store.Field{}, false, err
	}
	f, err := b.field(ctx, row)
	return f, err == nil, err
}

// Fields returns the extractions of a parse, by name, each as Field
// returns it.
func (b *Backend) Fields(ctx context.Context, p store.Parse) ([]store.Field, error) {
	rows, err := b.Store.Fields(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]store.Field, len(rows))
	for i, row := range rows {
		if out[i], err = b.field(ctx, row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// field is an extraction's row as the API's field, with its result read
// from the object store when it has one.
func (b *Backend) field(ctx context.Context, row postgres.Field) (store.Field, error) {
	f := store.Field{
		Name: row.Name, State: row.State,
		Usage: document.Usage{InputTokens: row.InputTokens, OutputTokens: row.OutputTokens},
	}
	if row.Error != nil {
		f.Error = &document.Error{Code: row.Error.Code, Detail: row.Error.Detail}
	}
	var asked tasks.Field
	if err := json.Unmarshal([]byte(row.Request), &asked); err != nil {
		return store.Field{}, fmt.Errorf("durable: the row of the extraction %s of %s holds no request: %w", row.Name, row.Parse, err)
	}
	f.Schema, f.Instructions = asked.Schema, asked.Instructions
	var said extract.Summary
	if len(row.Result) > 0 {
		if err := json.Unmarshal(row.Result, &said); err != nil {
			return store.Field{}, fmt.Errorf("durable: the row of the extraction %s of %s holds no summary: %w", row.Name, row.Parse, err)
		}
		f.Model, f.Constrained, f.Attempts, f.Windows = said.Model, said.Constrained, said.Attempts, said.Windows
	}
	if row.State == postgres.FieldSucceeded && row.Output != "" {
		var result extract.Result
		if err := objects.Get(ctx, b.Objects, row.Output, &result); err != nil {
			return store.Field{}, fmt.Errorf("durable: reading the extraction %s of %s: %w", row.Name, row.Parse, err)
		}
		f.Data, f.Citations = result.Data, result.Citations
	}
	return f, nil
}
