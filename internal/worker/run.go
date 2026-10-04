// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/keys"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/reader"
)

// assembleReads is how many page results assemble reads at once.
const assembleReads = 8

// run does the work of one claimed task and returns how the attempt ended.
// What it returns is not sent when ctx has ended: the task was taken away,
// or the worker is stopping.
func (w *Worker) run(ctx context.Context, c tasks.Claim) tasks.Settle {
	s := tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token}
	switch c.Kind {
	case tasks.Prepare:
		w.prepare(ctx, c, &s)
	case tasks.Page:
		w.page(ctx, c, &s)
	case tasks.Assemble:
		w.assemble(ctx, c, &s)
	default:
		permanent(&s, fault.Internal, "a worker runs no task of kind %q", c.Kind)
	}
	return s
}

// permanent ends an attempt as a failure no other attempt changes.
func permanent(s *tasks.Settle, code fault.Code, format string, args ...any) {
	s.Outcome = tasks.Permanent
	s.Error = &tasks.Error{Code: string(code), Detail: fmt.Sprintf(format, args...)}
}

// retryable ends an attempt as a failure another attempt may not repeat.
func retryable(s *tasks.Settle, code fault.Code, format string, args ...any) {
	s.Outcome = tasks.Retryable
	s.Error = &tasks.Error{Code: string(code), Detail: fmt.Sprintf(format, args...)}
}

// stored ends an attempt for an error of the object store or of the task
// store: neither says anything about the file or the reader, so the task is
// tried again. A key that holds nothing where a file was promised is the
// file's own failure.
func stored(s *tasks.Settle, what string, err error) {
	if errors.Is(err, blob.ErrNotFound) {
		permanent(s, fault.FileNotFound, "%s is not in the object store", what)
		return
	}
	retryable(s, fault.Internal, "%s could not be read or written", what)
}

// prepare runs intake once for the parse: it tells what the file is, opens
// and converts what needs it, counts and selects the pages, and writes the
// working copy and, for a format that carries its own structure, the pages.
func (w *Worker) prepare(ctx context.Context, c tasks.Claim, s *tasks.Settle) {
	file := c.Context.File
	if file == nil {
		permanent(s, fault.FileNotFound, "the parse names no file")
		return
	}
	data, err := w.cache.get(ctx, w.Objects, file.Key)
	if err != nil {
		stored(s, "the file", err)
		return
	}
	prepared, err := w.Pipeline.Prepare(ctx, data, detect.DeclaredType{MIME: file.MediaType, FileName: file.Name}, c.Context.Pages)
	if err != nil {
		if f, ok := errors.AsType[*fault.Error](err); ok {
			permanent(s, f.Code, "%s", f.Detail)
			return
		}
		retryable(s, fault.Internal, "the file could not be prepared")
		return
	}

	m := objects.Manifest{Manifest: prepared.Manifest, Work: file.Key, Token: c.Token}
	if !same(prepared.Working, data) {
		// The file was opened or converted: the pages are read from what
		// came out, kept under the parse's prefix and this task's token.
		m.Work = blob.WorkKey(c.Parse, c.Token, m.MediaType)
		if err := w.Objects.Put(ctx, m.Work, prepared.Working, m.MediaType); err != nil {
			stored(s, "the working copy", err)
			return
		}
	}
	for _, page := range prepared.Native {
		// A format that carries its own structure has nothing left to read:
		// its pages are written here, under this task's token.
		res := objects.Page{Revision: objects.FirstReading, Page: page}
		if err := objects.PutPage(ctx, w.Objects, blob.PageKey(c.Parse, page.Number, c.Token), res); err != nil {
			stored(s, "a page", err)
			return
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		permanent(s, fault.Internal, "the manifest does not encode")
		return
	}
	pages := m.Selected
	if pages == nil {
		pages = []int{}
	}
	s.Outcome = tasks.Done
	s.Prepare = &tasks.Prepared{Manifest: raw, Pages: pages, Native: m.Source == document.SourceNative}
}

// same reports whether two slices are the same bytes in memory, which is
// what Prepare returns for a file it neither opened nor converted.
func same(a, b []byte) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// page reads one page: it takes the result of the same read when an earlier
// parse kept one, and otherwise resolves the key of the page's group,
// renders the page, has the reader it was claimed for read it with that key,
// and writes the image and the result under this task's token. What it does
// about a failed call follows from the class of the reader's error
// (specs/005-parse-graph.md) and from nothing else.
func (w *Worker) page(ctx context.Context, c tasks.Claim, s *tasks.Settle) {
	n, ok := tasks.PageOf(c.Task)
	var m objects.Manifest
	if err := json.Unmarshal(c.Context.Manifest, &m); err != nil || !ok {
		permanent(s, fault.Internal, "the task carries no page or no manifest")
		return
	}
	if c.Context.Reuse != "" && w.reuse(ctx, c, n, s) {
		return
	}
	rd := w.Readers[c.Reader]
	if rd == nil {
		// This process has no such reader: another reader may take the page.
		s.Outcome = tasks.Next
		s.Error = &tasks.Error{Code: string(fault.ReaderUnavailable), Detail: "the reader the page was claimed for is not configured"}
		return
	}
	// The key is resolved before anything is fetched or rendered: a page
	// that has none yet goes back to the queue having cost nothing.
	opt := parse.PageOptions{Languages: c.Context.Languages}
	if w.Keys != nil {
		key, err := w.Keys.Key(ctx, c.Group, c.Context.Owner, c.Parse)
		if err != nil {
			keyless(s, err)
			return
		}
		opt.Credential = key
	}
	working, err := w.cache.get(ctx, w.Objects, m.Work)
	if err != nil {
		stored(s, "the working copy", err)
		return
	}

	got, err := w.Pipeline.ReadPage(ctx, m.Manifest, working, n, rd, opt)
	if err != nil {
		w.failed(c, s, err)
		return
	}
	// A page with nothing on it is written with no call.
	if !got.Image.Blank {
		s.Usage, s.Units, s.Health = tasks.Usage{Calls: 1}, w.cost(c.Reader), tasks.Healthy
		if u := got.Page.Usage; u != nil {
			s.Usage.InputTokens, s.Usage.OutputTokens = u.InputTokens, u.OutputTokens
		}
	}
	got.Page.Attempts = c.Attempt + 1
	res := objects.Page{Revision: objects.FirstReading, Page: got.Page}
	if len(got.Image.Data) > 0 {
		res.Image = blob.ImageKey(c.Parse, n, c.Token, got.Image.MediaType)
		if err := w.Objects.Put(ctx, res.Image, got.Image.Data, got.Image.MediaType); err != nil {
			stored(s, "the page's image", err)
			return
		}
	}
	w.wrote(ctx, c, s, n, res)
}

// wrote stores a page's result under the task's token and ends the attempt
// as done, with the key and what a list of pages says of the page.
func (w *Worker) wrote(ctx context.Context, c tasks.Claim, s *tasks.Settle, n int, res objects.Page) {
	key := blob.PageKey(c.Parse, n, c.Token)
	if err := objects.PutPage(ctx, w.Objects, key, res); err != nil {
		stored(s, "the page's result", err)
		return
	}
	summary, err := json.Marshal(objects.Summary{
		Blocks: len(res.Page.Blocks), Source: res.Page.Source, Reused: res.Page.Reused, Truncated: res.Page.Truncated,
	})
	if err != nil {
		permanent(s, fault.Internal, "the page's summary does not encode")
		return
	}
	s.Outcome, s.Output, s.Result = tasks.Done, key, summary
}

// reuse takes the result an earlier parse of the same owner kept for the
// same read: the result and its image are copied under this task's own
// keys, so the parse depends on no object of another, and no model is
// called. It reports false when the result is no longer there, and the page
// is then read.
func (w *Worker) reuse(ctx context.Context, c tasks.Claim, n int, s *tasks.Settle) bool {
	kept, err := objects.GetPage(ctx, w.Objects, c.Context.Reuse)
	if errors.Is(err, blob.ErrNotFound) {
		return false
	}
	if err != nil {
		stored(s, "the result kept for the page", err)
		return true
	}
	page := kept.Page
	page.Number, page.Reused, page.Usage = n, true, &document.Usage{Pages: 1}
	page.Blocks = document.Number(n, page.Blocks)
	res := objects.Page{Revision: objects.FirstReading, Page: page}
	if kept.Image != "" {
		data, mediaType, err := w.Objects.Get(ctx, kept.Image)
		switch {
		case errors.Is(err, blob.ErrNotFound):
			// The result outlived its image: the page is taken without one.
		case err != nil:
			stored(s, "the image kept for the page", err)
			return true
		default:
			res.Image = blob.ImageKey(c.Parse, n, c.Token, mediaType)
			if err := w.Objects.Put(ctx, res.Image, data, mediaType); err != nil {
				stored(s, "the page's image", err)
				return true
			}
		}
	}
	w.wrote(ctx, c, s, n, res)
	return true
}

// keyless ends a page's attempt for which the key source gave no key
// (specs/013-limits-and-usage.md). No reader was called, so no call is
// recorded and nothing is said about a reader's health.
//
// A refusal is about the group and stands whatever is tried, on every
// reader, so the page fails at once and does not move down the chain: a
// group with no budget as the reader's own budget refusal does, and a group
// that is issued no key as a page no reader can be called for.
//
// Anything else is a source that cannot say yet. The attempt ends as a
// wait, which spends no attempt and pauses the group's key scope, so the
// group's pages stay unclaimed until the source is asked again. A source
// that names no wait leaves it to the store's own pause.
func keyless(s *tasks.Settle, err error) {
	switch {
	case errors.Is(err, keys.ErrBudget):
		permanent(s, fault.BudgetExhausted, "the group has no budget left to read with")
	case errors.Is(err, keys.ErrForbidden):
		permanent(s, fault.ReaderNotPermitted, "the group is issued no key to read with")
	default:
		s.Outcome, s.RetryAfter = tasks.Wait, keys.RetryAfterOf(err)
	}
}

// cost is what one call to a reader is charged.
func (w *Worker) cost(name string) int {
	return max(1, w.Costs[name])
}

// failed ends a page's attempt for an error of ReadPage. A failure of the
// file itself, a page that cannot be rendered, is not the reader's: no
// other attempt or reader changes it. Every other error is the reader's and
// is read by its class; one that carries none is taken for a failure that
// may pass, as the reader package says.
func (w *Worker) failed(c tasks.Claim, s *tasks.Settle, err error) {
	if f, ok := errors.AsType[*fault.Error](err); ok {
		permanent(s, f.Code, "%s", f.Detail)
		return
	}
	// The reader was called, and what the call spent is recorded whatever
	// it returned.
	s.Usage, s.Units = tasks.Usage{Calls: 1}, w.cost(c.Reader)
	switch reader.ClassOf(err) {
	case reader.RateLimited:
		// Waiting for capacity is not failing: it spends no attempt.
		s.Outcome, s.RetryAfter = tasks.Wait, reader.RetryAfterOf(err)
	case reader.Budget:
		// The reader is healthy and the key is out of funds.
		permanent(s, fault.BudgetExhausted, "the key's budget is spent")
	case reader.Permanent:
		permanent(s, fault.PageUnreadable, "the reader cannot take the page as it is")
	case reader.Refused:
		// The reader is healthy and declined this page. Another may take
		// it; the same one would decline again.
		s.Outcome = tasks.Next
		s.Error = &tasks.Error{Code: string(fault.PageUnreadable), Detail: "the model declined the page"}
	case reader.Misconfigured:
		// The endpoint rejected the request itself, as it will every time:
		// the failure is the reader's and counts against it.
		s.Outcome, s.Health = tasks.Next, tasks.Unhealthy
		s.Error = &tasks.Error{Code: string(fault.ReaderUnavailable), Detail: "the reader's endpoint rejected the request; its configuration needs to change"}
	case reader.Invalid:
		// The reader answered, so the reply says nothing about its health.
		retryable(s, fault.PageUnreadable, "the reader's replies were not usable")
		s.Invalid = true
	default:
		retryable(s, fault.ReaderUnavailable, "the reader could not be reached")
		s.Health = tasks.Unhealthy
	}
}

// assemble builds the document of a parse whose pages have all settled: it
// reads every page's result by the key its task recorded, runs the passes of
// internal/assemble, writes the pages those passes changed and the pages
// that failed under its own token, and writes the index that lists the key
// of every page.
func (w *Worker) assemble(ctx context.Context, c tasks.Claim, s *tasks.Settle) {
	var m objects.Manifest
	if len(c.Context.Manifest) > 0 {
		if err := json.Unmarshal(c.Context.Manifest, &m); err != nil {
			permanent(s, fault.Internal, "the manifest does not decode")
			return
		}
	}
	rows, err := w.Store.Tasks(ctx, c.Parse)
	if err != nil {
		retryable(s, fault.Internal, "the parse's tasks could not be read")
		return
	}
	pages, err := w.results(ctx, c.Parse, m, rows)
	if err != nil {
		stored(s, "a page's result", err)
		return
	}

	// The passes mark blocks on the pages in place. A page they changed is
	// written again; one they left alone stays where its task wrote it.
	before := make([][]byte, len(pages))
	read := make([]document.Page, len(pages))
	for i, p := range pages {
		read[i] = p.stored.Page
		if before[i], err = json.Marshal(read[i]); err != nil {
			permanent(s, fault.Internal, "a page does not encode")
			return
		}
	}
	doc := assemble.Document(c.Parse, read)
	doc.Renderings = []string{"markdown", "text"}
	idx := objects.Index{Document: doc, Keys: make([]objects.Entry, len(pages))}
	for i, p := range pages {
		p.stored.Page = read[i]
		after, err := json.Marshal(read[i])
		if err != nil {
			permanent(s, fault.Internal, "a page does not encode")
			return
		}
		if p.key == "" || !bytes.Equal(before[i], after) {
			p.key = blob.AssembledPageKey(c.Parse, read[i].Number, c.Token)
			if err := objects.PutPage(ctx, w.Objects, p.key, p.stored); err != nil {
				stored(s, "a page's result", err)
				return
			}
		}
		idx.Keys[i] = objects.Entry{Number: read[i].Number, Key: p.key, Revision: p.stored.Revision}
	}
	key := blob.IndexKey(c.Parse, c.Token)
	if err := objects.PutIndex(ctx, w.Objects, key, idx); err != nil {
		stored(s, "the document index", err)
		return
	}
	s.Outcome, s.Output, s.Assemble = tasks.Done, key, &tasks.Assembled{Index: key}
}

// result is a page as assemble holds it: the stored result and the key it
// was read from, empty for a page that has no stored result yet.
type result struct {
	stored objects.Page
	key    string
}

// results reads the result of every selected page, in the order of the
// selection: a native page from where prepare wrote it, a page that was read
// from the key its task recorded, and a page that failed from its task's
// error.
func (w *Worker) results(ctx context.Context, parseID string, m objects.Manifest, rows []postgres.Task) ([]result, error) {
	byPage := map[int]postgres.Task{}
	for _, row := range rows {
		if n, ok := tasks.PageOf(row.ID); ok {
			byPage[n] = row
		}
	}
	out := make([]result, len(m.Selected))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		limit = make(chan struct{}, assembleReads)
	)
	for i, n := range m.Selected {
		key := ""
		row, has := byPage[n]
		switch {
		case m.Source == document.SourceNative:
			key = blob.PageKey(parseID, n, m.Token)
		case has && row.State == tasks.Succeeded:
			key = row.Output
		case has && row.State == tasks.Failed && row.Error != nil:
			out[i] = result{stored: objects.Page{Revision: objects.FirstReading, Page: objects.Failed(n, row.Attempt, row.Error.Code, row.Error.Detail)}}
			continue
		default:
			out[i] = result{stored: objects.Page{Revision: objects.FirstReading, Page: objects.Skipped(n)}}
			continue
		}
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			page, err := objects.GetPage(ctx, w.Objects, key)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && first == nil {
				first = fmt.Errorf("page %d: %w", n, err)
			}
			out[i] = result{stored: page, key: key}
		})
	}
	wg.Wait()
	return out, first
}
