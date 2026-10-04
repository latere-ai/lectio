// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package run

import (
	"context"
	"slices"
	"sync"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/reader"
)

// FigureOptions say which figures of a parse a run describes.
type FigureOptions struct {
	// Pages limits the run to figures on these pages. Nil is every page.
	Pages []int

	// Describer names the one describer to use. Empty takes the routing
	// policy's chain.
	Describer string

	// Redo describes a figure again that already has a description, and
	// takes none from an earlier run.
	Redo bool
}

// Figures starts a run that describes the figures of a parse: every block
// of kind figure on the selected pages that has a box on a page that has
// an image, and that has no description yet. Each figure is one job in the
// queue the pages go through, so describing shares the workers with
// reading and takes its owner's turn.
//
// The parse must have ended, so that no page is rewritten under the run.
// A run is refused while an earlier one of the same parse is in flight,
// and when no describer is configured or the one named is not.
func (r *Runner) Figures(p store.Parse, opt FigureOptions) (store.FigureRun, error) {
	if !p.Terminal() {
		return store.FigureRun{}, fault.New(fault.NotTerminal, "parse %s has not ended", p.ID)
	}
	chain := r.DescribeChain
	if opt.Describer != "" {
		chain = []string{opt.Describer}
	}
	chain = slices.DeleteFunc(slices.Clone(chain), func(name string) bool { return r.Describers[name] == nil })
	if len(chain) == 0 {
		if opt.Describer != "" {
			return store.FigureRun{}, fault.New(fault.ReaderNotFound, "no describer is named %q", opt.Describer)
		}
		return store.FigureRun{}, fault.New(fault.ReaderNotFound, "no describer is configured")
	}

	var found []figures.Figure
	for _, page := range r.Store.Pages(p.ID) {
		if opt.Pages != nil && !slices.Contains(opt.Pages, page.Number) {
			continue
		}
		if _, ok := r.Store.Image(p.ID, page.Number); !ok {
			// A page that was not read from an image has nothing to cut a
			// figure from.
			continue
		}
		found = append(found, figures.Of(page, opt.Redo)...)
	}
	if err := figures.Bounded(len(found)); err != nil {
		return store.FigureRun{}, err
	}

	run := store.FigureRun{State: store.RunRunning, Total: len(found), StartedAt: time.Now().UTC()}
	if err := r.Store.StartFigureRun(p.ID, run); err != nil {
		return store.FigureRun{}, err
	}

	ctx, cancel := context.WithCancel(r.base)
	h := &handle{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	r.describing[p.ID] = h
	r.mu.Unlock()

	r.wg.Go(func() {
		defer close(h.done)
		defer cancel()
		var all sync.WaitGroup
		r.mu.Lock()
		// Once the runner has stopped no worker takes a job, so none is
		// queued and the run ends with what it has.
		if r.base.Err() == nil {
			for i, f := range found {
				all.Add(1)
				j := &job{ctx: ctx, parse: p, seq: i, wg: &all}
				j.do = func() { r.describe(j, f, chain, opt.Redo) }
				r.queue.push(j)
			}
			r.cond.Broadcast()
		}
		r.mu.Unlock()
		all.Wait()

		now := time.Now().UTC()
		r.Store.UpdateFigureRun(p.ID, func(run *store.FigureRun) {
			run.FinishedAt, run.State = &now, store.RunSucceeded
			// A run failed when it described nothing it set out to. One
			// that lost some figures and described others succeeded, and
			// says which it lost.
			if run.Total > 0 && run.Done == 0 {
				run.State = store.RunFailed
			}
		})
		r.mu.Lock()
		delete(r.describing, p.ID)
		r.mu.Unlock()
	})
	return run, nil
}

// FiguresDone returns a channel that is closed when the run describing a
// parse's figures has ended. With no run in flight it is already closed.
func (r *Runner) FiguresDone(id string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.describing[id]; ok {
		return h.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

// describe does the work of one figure: it cuts the figure from its page's
// image, has a describer describe it, and writes what came back onto the
// figure's block.
func (r *Runner) describe(j *job, f figures.Figure, chain []string, redo bool) {
	if j.ctx.Err() != nil {
		return
	}
	lost := func(why *document.Error) {
		r.Store.UpdateFigureRun(j.parse.ID, func(run *store.FigureRun) {
			run.Failed++
			if run.Failures == nil {
				run.Failures = map[string]document.Error{}
			}
			run.Failures[f.Ref] = *why
		})
	}

	// A figure this owner already had described, from the same bytes with
	// the same describers, is taken and not described again.
	key := figures.Key(j.parse.ContentSHA, f, j.parse.Languages, chain, r.Describers)
	if !redo {
		if res, ok := r.Store.Figure(j.parse.Owner, key); ok {
			r.write(j.parse.ID, f.Ref, res)
			r.Store.UpdateFigureRun(j.parse.ID, func(run *store.FigureRun) { run.Done++; run.Reused++ })
			return
		}
	}

	page, ok := r.Store.Image(j.parse.ID, f.Page)
	if !ok {
		lost(&document.Error{Code: string(fault.FigureUnreadable), Detail: "the figure's page has no image"})
		return
	}
	cut, err := render.Crop(page, f.Box)
	if err != nil {
		lost(&document.Error{Code: string(fault.CodeOf(err)), Detail: fault.DetailOf(err)})
		return
	}
	req := reader.FigureRequest{
		Data: cut.Data, MediaType: cut.MediaType, Width: cut.Width, Height: cut.Height,
		Caption: f.Caption, Languages: j.parse.Languages,
	}
	if r.Credential != nil {
		req.Credential = r.Credential(j.parse.Owner)
	}

	var res reader.FigureResult
	_, failure, gone := r.try(j.ctx, figureUnit, len(chain),
		func(int) bool { return true },
		func(at int) (err error) {
			res, err = r.Describers[chain[at]].DescribeFigure(j.ctx, req)
			return err
		})
	switch {
	case gone:
		// The runner stopped: what came back is dropped.
	case failure != nil:
		lost(failure)
	default:
		r.write(j.parse.ID, f.Ref, res)
		r.Store.KeepFigure(j.parse.Owner, key, res)
		r.Store.UpdateFigureRun(j.parse.ID, func(run *store.FigureRun) {
			run.Done++
			run.Usage = run.Usage.Add(res.Usage)
			delete(run.Failures, f.Ref)
		})
		r.update(j.parse.ID, func(p *store.Parse) { p.Usage = p.Usage.Add(res.Usage) })
	}
}

// write puts a description onto a figure's block.
func (r *Runner) write(parseID, ref string, res reader.FigureResult) {
	r.Store.UpdateBlock(parseID, ref, figures.Kept(res).Onto)
}
