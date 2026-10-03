// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package run runs parses inside one process. It is the development
// runner: it calls the same steps a durable control plane will call, in the
// same shape, one page at a time, and keeps nothing that survives the
// process. A restart loses every parse that had not ended.
//
// What it already holds to is the shape of the work. A parse is prepared
// once and then read page by page. Pages of every running parse wait in one
// queue ordered by class, then priority, then position in their parse, so
// interactive work goes first and two parses advance together. A page that
// fails is retried alone, a rate limit is waited out without spending an
// attempt, and a parse that is canceled stops between pages and abandons
// the page being read. What it does not hold to, and says so here, is
// durability, fairness between tenants, and a bound on a batch backlog's
// wait. Those are specs 004, 006 and 007.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/reader"
)

// maxWaits bounds how often one page waits out a rate limit before the
// reader is given up on. A durable runner bounds the wait by the parse's
// deadline; a parse here may have none.
const maxWaits = 20

// Runner runs parses.
type Runner struct {
	Store    *store.Memory
	Pipeline *parse.Pipeline

	// Readers are the configured readers by name. Chain is the routing
	// policy's order for a parse that pins none: the first reads, and the
	// next is tried once for a page the first cannot read.
	Readers map[string]reader.Reader
	Chain   []string

	// Credential returns the key an owner's pages are read with. Nil means
	// every call is made without one.
	Credential func(owner string) reader.Credential

	// Workers is how many pages are read at once, across every parse.
	// Attempts is how often one page is tried. Zero takes 4 and 3.
	Workers  int
	Attempts int

	// Backoff is how long a page waits before its next attempt. Nil takes a
	// doubling delay from 200 ms to 5 s.
	Backoff func(attempt int) time.Duration

	mu      sync.Mutex
	cond    *sync.Cond
	queue   queue
	base    context.Context
	running map[string]*handle
	wg      sync.WaitGroup
}

// handle is what the runner keeps for a parse that has not ended.
type handle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// job is one page waiting to be read.
type job struct {
	ctx      context.Context
	parse    store.Parse
	manifest parse.Manifest
	working  []byte
	chain    []string
	page     int
	seq      int
	wg       *sync.WaitGroup
}

// jobs is one owner's waiting pages in one class, kept sorted with the
// page to dispatch next at the end, so taking it is a truncation.
type jobs []*job

// later orders two jobs of one owner: negative when a is dispatched after
// b. The higher priority goes first, then the earlier position in its
// parse, so an owner's parses advance together and a short one finishes
// early, then the older parse.
func later(a, b *job) int {
	if a.parse.Priority != b.parse.Priority {
		return a.parse.Priority - b.parse.Priority
	}
	if a.seq != b.seq {
		return b.seq - a.seq
	}
	return strings.Compare(b.parse.ID, a.parse.ID)
}

// lane is the waiting pages of one class: each owner's own queue, and the
// order the owners are served in.
type lane struct {
	owners []string
	jobs   map[string]jobs
}

// interactiveShare is how many interactive pages are dispatched for one
// batch page when both classes have work.
const interactiveShare = 4

// queue is every waiting page. A page is chosen in three steps: the class,
// the owner, then that owner's next page. Interactive pages go ahead of
// batch pages without starving them: one page in five is a batch page
// when both wait. Within a class the owners take turns, one page each, so
// nothing an owner queues, however much or at whatever priority, moves it
// ahead of another owner. Priority orders an owner's own pages and
// nothing else.
//
// This is the shape of the fair queue with every weight at one. It is not
// the fair queue: it keeps no account of what each owner was served.
type queue struct {
	interactive, batch lane
	size               int
	// streak counts interactive pages dispatched since the last batch page.
	streak int
}

func (q *queue) len() int { return q.size }

func (q *queue) lane(class string) *lane {
	if class == store.ClassBatch {
		return &q.batch
	}
	return &q.interactive
}

// push puts a job at its place in its owner's order.
func (q *queue) push(j *job) {
	l := q.lane(j.parse.Class)
	if l.jobs == nil {
		l.jobs = map[string]jobs{}
	}
	own, waiting := l.jobs[j.parse.Owner]
	if !waiting {
		// An owner with nothing waiting joins the end of the turn order.
		l.owners = append(l.owners, j.parse.Owner)
	}
	at, _ := slices.BinarySearchFunc(own, j, later)
	l.jobs[j.parse.Owner] = slices.Insert(own, at, j)
	q.size++
}

// pop takes the job to dispatch next. The queue is not empty.
func (q *queue) pop() *job {
	l := &q.interactive
	switch {
	case len(q.batch.owners) == 0:
		// No batch page waits, so none is owed a turn.
		q.streak = 0
	case len(l.owners) == 0 || q.streak >= interactiveShare:
		l, q.streak = &q.batch, 0
	default:
		q.streak++
	}

	owner := l.owners[0]
	own := l.jobs[owner]
	last := len(own) - 1
	j := own[last]
	own[last] = nil
	l.owners = l.owners[1:]
	if last == 0 {
		delete(l.jobs, owner)
	} else {
		// The owner has more waiting: it goes to the back of the turns.
		l.jobs[owner] = own[:last]
		l.owners = append(l.owners, owner)
	}
	q.size--
	return j
}

// Start starts the workers. They stop, and running parses are abandoned,
// when ctx ends. Start is called once, before any Submit.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	r.cond = sync.NewCond(&r.mu)
	r.base = ctx
	r.running = map[string]*handle{}
	workers := r.Workers
	if workers <= 0 {
		workers = 4
	}
	r.mu.Unlock()

	for range workers {
		r.wg.Go(r.work)
	}
	// A worker waiting for a page is woken when the runner stops.
	r.wg.Go(func() {
		<-ctx.Done()
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
}

// Wait blocks until the workers and every running parse have stopped,
// which they do once the context given to Start ends.
func (r *Runner) Wait() { r.wg.Wait() }

// Submit starts a stored parse.
func (r *Runner) Submit(p store.Parse) {
	var ctx context.Context
	var cancel context.CancelFunc
	if p.DeadlineAt != nil {
		ctx, cancel = context.WithDeadline(r.base, *p.DeadlineAt)
	} else {
		ctx, cancel = context.WithCancel(r.base)
	}
	h := &handle{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	r.running[p.ID] = h
	r.mu.Unlock()

	r.wg.Go(func() {
		defer close(h.done)
		defer cancel()
		r.drive(ctx, p)
		r.mu.Lock()
		delete(r.running, p.ID)
		r.mu.Unlock()
	})
}

// Cancel stops a parse. It reports whether the parse was running here.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	h, ok := r.running[id]
	r.mu.Unlock()
	if ok {
		h.cancel()
	}
	return ok
}

// Done returns a channel that is closed when the parse has ended. For a
// parse that is not running here the channel is already closed.
func (r *Runner) Done(id string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.running[id]; ok {
		return h.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

// drive runs one parse from its file to its document.
func (r *Runner) drive(ctx context.Context, p store.Parse) {
	now := time.Now().UTC()
	r.update(p.ID, func(p *store.Parse) { p.State, p.Stage, p.StartedAt = store.StateRunning, store.StagePreparing, &now })

	file, err := r.Store.File(p.Owner, p.File)
	if err != nil {
		r.assemble(p.ID, nil)
		r.finish(ctx, p.ID, err)
		return
	}
	prepared, err := r.Pipeline.Prepare(ctx, file.Data, detect.DeclaredType{MIME: file.MediaType, FileName: file.Name}, p.Pages)
	if err != nil {
		r.assemble(p.ID, nil)
		r.finish(ctx, p.ID, err)
		return
	}
	r.update(p.ID, func(p *store.Parse) {
		p.Manifest, p.PagesTotal, p.Stage = &prepared.Manifest, len(prepared.Manifest.Selected), store.StageReading
	})

	if prepared.Manifest.Source == document.SourceNative {
		for _, page := range prepared.Native {
			r.Store.PutPage(p.ID, page, nil)
			r.update(p.ID, func(p *store.Parse) { p.PagesDone++ })
		}
	} else {
		chain := r.Chain
		if p.Reader != "" {
			// A parse that named its reader has a chain of one.
			chain = []string{p.Reader}
		}
		if len(chain) == 0 || r.Readers[chain[0]] == nil {
			r.assemble(p.ID, prepared.Manifest.Selected)
			r.finish(ctx, p.ID, fault.New(fault.ReaderUnavailable, "no reader is configured to read the pages"))
			return
		}
		var pages sync.WaitGroup
		r.mu.Lock()
		// Once the runner has stopped no worker takes a page, so none is
		// queued: the parse ends as canceled below.
		if r.base.Err() == nil {
			for i, n := range prepared.Manifest.Selected {
				pages.Add(1)
				r.queue.push(&job{ctx: ctx, parse: p, manifest: prepared.Manifest, working: prepared.Working, chain: chain, page: n, seq: i, wg: &pages})
			}
			r.cond.Broadcast()
		}
		r.mu.Unlock()
		pages.Wait()
	}
	r.assemble(p.ID, prepared.Manifest.Selected)
	r.finish(ctx, p.ID, nil)
}

// assemble writes a parse's document from the pages that were read. It
// runs however the parse ends: a parse that was canceled, ran out of time,
// or lost pages still has every page it read, and a caller reads them as a
// document and not only one by one. A page of selected that was never
// read is recorded as skipped, so the document accounts for every page
// the parse was asked for.
func (r *Runner) assemble(id string, selected []int) {
	r.update(id, func(p *store.Parse) { p.Stage = store.StageAssembling })
	for _, n := range selected {
		if _, read := r.Store.Page(id, n); !read {
			r.Store.PutPage(id, document.Page{Number: n, State: document.PageSkipped, Blocks: []document.Block{}}, nil)
		}
	}
	read := r.Store.Pages(id)
	for i := range read {
		// Assembly edits blocks in place, and a stored page may be being
		// served: it works on a copy, which then replaces the stored one.
		read[i].Blocks = slices.Clone(read[i].Blocks)
	}
	doc := assemble.Document(id, read)
	doc.Renderings = []string{"markdown", "text"}
	for _, page := range read {
		// Assembly marks running headers and footers on the pages.
		r.Store.PutPage(id, page, nil)
	}
	r.Store.PutDocument(doc)
	r.update(id, func(p *store.Parse) { p.Usage = doc.Usage })
}

// finish gives a parse its terminal state. err is why it failed outright,
// or nil when it ran to its end, in which case the state follows from the
// context and from how many pages failed.
func (r *Runner) finish(ctx context.Context, id string, err error) {
	now := time.Now().UTC()
	r.update(id, func(p *store.Parse) {
		p.FinishedAt, p.Stage = &now, store.StageDone
		switch {
		case err != nil && ctx.Err() == nil:
			p.State = store.StateFailed
			p.Error = &document.Error{Code: string(fault.CodeOf(err)), Detail: fault.DetailOf(err)}
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			p.State = store.StateFailed
			p.Error = &document.Error{Code: string(fault.DeadlineExceeded), Detail: "the parse did not end by its deadline"}
		case ctx.Err() != nil:
			p.State = store.StateCanceled
		case p.PagesFailed > p.AllowFailedPages:
			p.State = store.StateFailed
			p.Error = &document.Error{Code: string(fault.PageUnreadable), Detail: fmt.Sprintf("%d of %d pages could not be read", p.PagesFailed, p.PagesTotal)}
		default:
			p.State = store.StateSucceeded
		}
	})
}

// update changes a parse. A parse that was deleted meanwhile is left alone.
func (r *Runner) update(id string, change func(*store.Parse)) {
	_, _ = r.Store.UpdateParse(id, change)
}

// work is one worker: it takes the next page in dispatch order and reads
// it, until the runner stops.
func (r *Runner) work() {
	for {
		r.mu.Lock()
		for r.queue.len() == 0 && r.base.Err() == nil {
			r.cond.Wait()
		}
		if r.base.Err() != nil {
			// The runner stopped: release every parse waiting on a page.
			for r.queue.len() > 0 {
				r.queue.pop().wg.Done()
			}
			r.mu.Unlock()
			return
		}
		j := r.queue.pop()
		r.mu.Unlock()
		r.read(j)
		j.wg.Done()
	}
}

// read does the work of one page and records how it ended. A page of a
// parse that was canceled is left unread.
func (r *Runner) read(j *job) {
	if j.ctx.Err() != nil {
		return
	}
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	opt := parse.PageOptions{Languages: j.parse.Languages}
	if r.Credential != nil {
		opt.Credential = r.Credential(j.parse.Owner)
	}

	// A page an earlier parse of this owner read whole, from the same
	// bytes with the same readers, is taken and not read again.
	key := r.readKey(j)
	if j.parse.Reuse {
		if page, img, ok := r.Store.Read(j.parse.Owner, key); ok {
			page.Number, page.Reused, page.Usage = j.page, true, &document.Usage{Pages: 1}
			page.Blocks = document.Number(j.page, page.Blocks)
			r.Store.PutPage(j.parse.ID, page, img)
			r.update(j.parse.ID, func(p *store.Parse) { p.PagesDone++; p.PagesReused++ })
			return
		}
	}

	// at is the reader in the chain the page is with. It moves down the
	// chain when a reader cannot be the one to read this page.
	at, attempt, invalid, waits, escalated := 0, 0, 0, 0, false
	next := func() bool {
		for at+1 < len(j.chain) {
			if at++; r.Readers[j.chain[at]] != nil {
				attempt, invalid = 0, 0
				return true
			}
		}
		return false
	}
	for {
		attempt++
		got, err := r.Pipeline.ReadPage(j.ctx, j.manifest, j.working, j.page, r.Readers[j.chain[at]], opt)
		if j.ctx.Err() != nil {
			// The parse ended while the page was being read: whatever came
			// back is dropped, so nothing is written after a cancel.
			return
		}
		if err == nil {
			got.Page.Attempts = attempt
			r.Store.PutPage(j.parse.ID, got.Page, &got.Image)
			r.Store.KeepRead(j.parse.Owner, key, got.Page, &got.Image)
			r.update(j.parse.ID, func(p *store.Parse) { p.PagesDone++ })
			return
		}
		// A failure of the file itself is not the reader's: no other
		// attempt or reader changes it.
		if f, ok := errors.AsType[*fault.Error](err); ok {
			r.fail(j, attempt, f.Code, f.Detail)
			return
		}

		switch reader.ClassOf(err) {
		case reader.RateLimited:
			// Waiting for capacity is not failing: it spends no attempt.
			attempt--
			if waits++; waits > maxWaits {
				r.fail(j, attempt, fault.ReaderUnavailable, "the reader stayed rate limited")
				return
			}
			if !sleep(j.ctx, max(reader.RetryAfterOf(err), r.backoff(waits))) {
				return
			}
			continue
		case reader.Budget:
			r.fail(j, attempt, fault.BudgetExhausted, "the key's budget is spent")
			return
		case reader.Permanent:
			r.fail(j, attempt, fault.PageUnreadable, "the reader cannot take the page as it is")
			return
		case reader.Refused:
			// The reader is healthy and declined this page. Another may
			// read it; the same one will decline again.
			if next() {
				continue
			}
			r.fail(j, attempt, fault.PageUnreadable, "the model declined to read the page")
			return
		case reader.Misconfigured:
			// The endpoint rejected the request itself, as it will for
			// every page. The failure is the reader's and not the page's.
			if next() {
				continue
			}
			r.fail(j, attempt, fault.ReaderUnavailable, "the reader's endpoint rejected the request; its configuration needs to change")
			return
		case reader.Invalid:
			// Two unusable replies send the page to the next reader, once.
			if invalid++; invalid == 2 && !escalated && next() {
				escalated = true
				continue
			}
		}
		if attempt >= attempts {
			code, detail := fault.ReaderUnavailable, "the reader could not be reached"
			if reader.ClassOf(err) == reader.Invalid {
				code, detail = fault.PageUnreadable, "the reader's replies were not usable"
			}
			r.fail(j, attempt, code, detail)
			return
		}
		if !sleep(j.ctx, r.backoff(attempt)) {
			return
		}
	}
}

// readKey names what reading this page means: the file's bytes, the page,
// the languages hinted, and every reader that may come to read it, each by
// the version it describes itself with. Two reads with the same key give
// the same result, so the second need not happen. A reader that names no
// version makes no such promise, and then there is no key.
func (r *Runner) readKey(j *job) string {
	if j.parse.ContentSHA == "" {
		return ""
	}
	parts := []string{j.parse.ContentSHA, strconv.Itoa(j.page), strings.Join(j.parse.Languages, ",")}
	for _, name := range j.chain {
		rd := r.Readers[name]
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

// fail records a page that could not be read.
func (r *Runner) fail(j *job, attempts int, code fault.Code, detail string) {
	r.Store.PutPage(j.parse.ID, document.Page{
		Number: j.page, State: document.PageFailed, Source: document.SourceReader, Attempts: attempts,
		Blocks: []document.Block{}, Error: &document.Error{Code: string(code), Detail: detail},
	}, nil)
	r.update(j.parse.ID, func(p *store.Parse) { p.PagesFailed++ })
}

func (r *Runner) backoff(attempt int) time.Duration {
	if r.Backoff != nil {
		return r.Backoff(attempt)
	}
	return min(200*time.Millisecond<<min(attempt-1, 5), 5*time.Second)
}

// sleep waits for d, or until ctx ends. It reports whether it waited the
// whole time.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
