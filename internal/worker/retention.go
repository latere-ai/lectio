// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"time"

	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/store/postgres"
)

// The retention sweep of specs/014-sources-and-retention.md. Any worker
// runs it. The store says when it is due, so across a fleet it runs once
// per sweep interval however many workers ask.
//
// A parse that has expired is removed in 3 steps: the work on it is
// stopped, then its objects are removed, then its rows. With its
// extractions and its figures dropped first, none of them is claimed after
// the objects were listed, and one that still runs has its settle refused
// and its worker removes what it wrote. A worker that stops between the
// steps leaves a row, which the next sweep lists again, and never an object
// that no row names. A file is removed object first and row second.

// Retainer is the part of the task store the retention sweep uses.
type Retainer interface {
	// Expired claims the sweep when it is due and returns what it is to
	// remove: the parses whose retention has ended, and the files whose
	// delete has begun.
	Expired(ctx context.Context, limit int) (postgres.Expired, error)
	// CloseParse stops the work on an expired parse, before its objects
	// are removed. It reports false for a parse that is no longer one to
	// remove.
	CloseParse(ctx context.Context, parseID string) (expired bool, err error)
	// ExpireParse removes the rows of an expired parse, once its objects
	// are gone.
	ExpireParse(ctx context.Context, parseID string) (removed bool, err error)
	// ForgetFile removes the row of a file whose delete began, once its
	// object is gone.
	ForgetFile(ctx context.Context, fileID string) error
}

const (
	// DefaultSweep is how often a worker asks whether the retention sweep
	// is due.
	DefaultSweep = 30 * time.Second

	// sweepBatch is how many parses and how many files one sweep removes.
	// What is left is removed by the sweeps after it.
	sweepBatch = 200
)

// retain runs the retention sweep until ctx ends.
func (w *Worker) retain(ctx context.Context) {
	every := w.Sweep
	if every <= 0 {
		every = DefaultSweep
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.sweep(ctx)
		}
	}
}

// sweep removes what has expired, once: for each parse its work, every
// object under its prefix and then its rows, and for each file its object
// and then its row. What fails is logged and left for the next sweep, which finds the
// row still there.
func (w *Worker) sweep(ctx context.Context) {
	due, err := w.Retention.Expired(ctx, sweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			w.Log.WarnContext(ctx, "the retention sweep could not ask what has expired", "error", err)
		}
		return
	}
	var parses, files int
	for _, id := range due.Parses {
		if ctx.Err() != nil {
			return
		}
		if err := w.expireParse(ctx, id); err != nil {
			w.Log.WarnContext(ctx, "an expired parse was not removed and is swept again", "parse", id, "error", err)
			continue
		}
		parses++
	}
	for _, f := range due.Files {
		if ctx.Err() != nil {
			return
		}
		if err := w.Objects.Delete(ctx, f.Key); err != nil {
			w.Log.WarnContext(ctx, "an expired file's object was not removed and is swept again", "file", f.ID, "error", err)
			continue
		}
		if err := w.Retention.ForgetFile(ctx, f.ID); err != nil {
			w.Log.WarnContext(ctx, "an expired file's row was not removed and is swept again", "file", f.ID, "error", err)
			continue
		}
		files++
	}
	if parses+files > 0 {
		w.Log.InfoContext(ctx, "the retention sweep removed what expired", "parses", parses, "files", files)
	}
}

// expireParse stops the work on a parse, removes every object the parse
// wrote, and then its rows.
func (w *Worker) expireParse(ctx context.Context, id string) error {
	expired, err := w.Retention.CloseParse(ctx, id)
	if err != nil || !expired {
		return err
	}
	keys, err := w.Objects.List(ctx, blob.ParsePrefix(id))
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := w.Objects.Delete(ctx, key); err != nil {
			return err
		}
	}
	_, err = w.Retention.ExpireParse(ctx, id)
	return err
}
