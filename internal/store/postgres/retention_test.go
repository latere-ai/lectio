// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
)

// How long a file and a parse are kept, and what the retention sweep is
// told to remove (specs/014-sources-and-retention.md).

// sweep asks what has expired, after the sweep interval has passed since
// the last time it was asked.
func (h *harness) sweep() postgres.Expired {
	h.t.Helper()
	h.advance(time.Minute)
	due, err := h.store.Expired(context.Background(), 100)
	if err != nil {
		h.t.Fatalf("the retention sweep: %v", err)
	}
	if !due.Due {
		h.t.Fatal("the sweep was not due a minute after the last one")
	}
	return due
}

// kept writes a file's row that is kept for a retention.
func (h *harness) kept(owner, id, sha string, retention time.Duration) postgres.File {
	h.t.Helper()
	f := file(owner, id, sha)
	f.Retention = retention
	return h.stored(f)
}

// finished runs a parse to its end: its prepare selects no page, so its
// assemble follows at once.
func (h *harness) finished(w *worker, sub postgres.Submission) postgres.Parse {
	h.t.Helper()
	h.submit(sub)
	w.settle(done(w.claim(1, 1)[0]))
	w.settle(done(w.claim(1, 1)[0]))
	p := h.parse(sub.Parse)
	if p.State != "succeeded" {
		h.t.Fatalf("%s ended %s", sub.Parse, p.State)
	}
	return p
}

// ids are the ids of listed files, sorted.
func ids(files []postgres.ExpiredFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.ID
	}
	slices.Sort(out)
	return out
}

// TestAParseIsKeptForItsRetentionAndThenListed: a parse's retention runs
// from its end, however it ended. Before it has passed the sweep lists
// nothing and the parse cannot be expired; after it the parse is listed
// until its rows are removed, with its tasks and the reads kept from it. A
// parse with no retention is kept, and so is one that has not ended.
func TestAParseIsKeptForItsRetentionAndThenListed(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		done := h.finished(w, postgres.Submission{Parse: "prs_done", Group: "acme", Retention: time.Hour})
		if done.ExpiresAt == nil || !done.ExpiresAt.Equal(done.FinishedAt.Add(time.Hour)) {
			t.Fatalf("a parse that ended at %v expires at %v", done.FinishedAt, done.ExpiresAt)
		}
		h.submit(postgres.Submission{Parse: "prs_canceled", Group: "acme", Retention: time.Hour})
		if err := h.store.Cancel(ctx, "prs_canceled"); err != nil {
			t.Fatal(err)
		}
		if p := h.parse("prs_canceled"); p.ExpiresAt == nil || !p.ExpiresAt.Equal(h.now.Add(time.Hour)) {
			t.Fatalf("a canceled parse expires at %v", p.ExpiresAt)
		}
		if p := h.finished(w, postgres.Submission{Parse: "prs_kept", Group: "acme"}); p.ExpiresAt != nil {
			t.Fatalf("a parse with no retention expires at %v", p.ExpiresAt)
		}
		// A parse that has not ended: its prepare is claimed and held.
		h.submit(postgres.Submission{Parse: "prs_open", Group: "acme", Retention: time.Hour, Deadline: 100 * time.Hour})
		w.claim(1, 1)

		if due := h.sweep(); len(due.Parses) != 0 || len(due.Files) != 0 {
			t.Fatalf("within the retention the sweep lists %+v", due)
		}
		if removed, err := h.store.ExpireParse(ctx, "prs_done"); err != nil || removed {
			t.Fatalf("a parse within its retention was expired: %t, %v", removed, err)
		}
		// The sweep is one process's at a time: asked again within the
		// interval, it is not due and lists nothing.
		if again, err := h.store.Expired(ctx, 100); err != nil || again.Due || len(again.Parses) != 0 {
			t.Fatalf("a second sweep within the interval: %+v, %v", again, err)
		}

		h.advance(time.Hour)
		due := h.sweep()
		if !slices.Equal(due.Parses, []string{"prs_canceled", "prs_done"}) {
			t.Fatalf("after the retention the sweep lists %v", due.Parses)
		}
		// Listed and not removed: a sweep that stopped before it removed
		// the rows finds them again.
		if again := h.sweep(); !slices.Equal(again.Parses, due.Parses) {
			t.Fatalf("an interrupted sweep is followed by one that lists %v", again.Parses)
		}
		if one, err := h.store.Expired(ctx, 1); err != nil || one.Due {
			t.Fatalf("asked at once again: %+v, %v", one, err)
		}
		for _, id := range due.Parses {
			if removed, err := h.store.ExpireParse(ctx, id); err != nil || !removed {
				t.Fatalf("expiring %s: %t, %v", id, removed, err)
			}
			if _, err := h.store.Parse(ctx, id); fault.CodeOf(err) != fault.ParseNotFound {
				t.Fatalf("%s outlived its expiry: %v", id, err)
			}
			if n := value[int](h, `SELECT count(*) FROM tasks WHERE parse_id = $1`, id); n != 0 {
				t.Fatalf("%d task rows of %s outlived it", n, id)
			}
		}
		if removed, err := h.store.ExpireParse(ctx, "prs_done"); err != nil || removed {
			t.Fatalf("expiring a parse twice: %t, %v", removed, err)
		}
		if due := h.sweep(); len(due.Parses) != 0 {
			t.Fatalf("after the rows are gone the sweep lists %v", due.Parses)
		}
		for _, id := range []string{"prs_kept", "prs_open"} {
			if _, err := h.store.Parse(ctx, id); err != nil {
				t.Fatalf("%s was removed: %v", id, err)
			}
		}
		// A batch is bounded, and the oldest goes first.
		for _, id := range []string{"prs_1", "prs_2", "prs_3"} {
			h.finished(w, postgres.Submission{Parse: id, Group: "acme", Retention: time.Minute})
			h.advance(time.Second)
		}
		h.advance(time.Hour)
		if two, err := h.store.Expired(ctx, 2); err != nil || !slices.Equal(two.Parses, []string{"prs_1", "prs_2"}) {
			t.Fatalf("a batch of 2 lists %v, %v", two.Parses, err)
		}
	})
}

// TestAFileIsKeptForItsRetentionPastItsLastParse: a file is kept for its
// retention from its last upload, and for its retention past the end of
// the last parse that read it. A file a parse still reads is kept however
// old it is. When its time is over the sweep begins its delete: the file
// is gone for every caller, and its row stays until its object is.
func TestAFileIsKeptForItsRetentionPastItsLastParse(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		idle := h.kept("alice", "fil_idle", "aa", 24*time.Hour)
		if idle.ExpiresAt == nil || !idle.ExpiresAt.Equal(h.now.Add(24*time.Hour)) {
			t.Fatalf("a file uploaded at %v expires at %v", h.now, idle.ExpiresAt)
		}
		h.kept("alice", "fil_read", "bb", 24*time.Hour)
		h.kept("alice", "fil_busy", "cc", 24*time.Hour)
		if forever := h.kept("alice", "fil_forever", "dd", 0); forever.ExpiresAt != nil {
			t.Fatalf("a file with no retention expires at %v", forever.ExpiresAt)
		}
		// A parse reads fil_busy and does not end: its prepare is claimed
		// and held.
		h.submit(postgres.Submission{Parse: "prs_busy", Owner: "alice", File: "fil_busy", Deadline: 1000 * time.Hour})
		w.claim(1, 1)

		// 20 hours on, a parse reads fil_read and ends: the file is kept for
		// 24 hours from there.
		h.advance(20 * time.Hour)
		h.submit(postgres.Submission{Parse: "prs_read", Owner: "alice", File: "fil_read"})
		w.settle(done(w.claim(1, 1)[0]))
		w.settle(done(w.claim(1, 1)[0]))
		ended := h.parse("prs_read")
		if got, err := h.store.File(ctx, "fil_read"); err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(ended.FinishedAt.Add(24*time.Hour)) {
			t.Fatalf("a file whose last parse ended at %v expires at %+v, %v", ended.FinishedAt, got.ExpiresAt, err)
		}

		// 25 hours after the uploads only the file nobody read has expired.
		h.advance(5 * time.Hour)
		due := h.sweep()
		if !slices.Equal(ids(due.Files), []string{"fil_idle"}) || due.Files[0].Key != idle.Key {
			t.Fatalf("25 hours after the uploads the sweep lists %+v", due.Files)
		}
		if _, err := h.store.File(ctx, "fil_idle"); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("a file whose delete began was read: %v", err)
		}
		if _, _, err := h.store.Submit(ctx, filled(postgres.Submission{Parse: "prs_late", Owner: "alice", File: "fil_idle"})); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("a parse was submitted for a file whose delete began: %v", err)
		}
		// The sweep stopped before it removed the object: the row is still
		// there and is listed again, until the row is forgotten.
		if again := h.sweep(); !slices.Equal(ids(again.Files), []string{"fil_idle"}) {
			t.Fatalf("an interrupted sweep is followed by one that lists %+v", again.Files)
		}
		if err := h.store.ForgetFile(ctx, "fil_idle"); err != nil {
			t.Fatal(err)
		}
		if due := h.sweep(); len(due.Files) != 0 {
			t.Fatalf("after the row is gone the sweep lists %+v", due.Files)
		}
		// The owner uploads the same bytes again, and has a new file.
		if again := h.kept("alice", "fil_again", "aa", time.Hour); again.ID != "fil_again" {
			t.Fatalf("an upload of bytes whose file expired is %+v", again)
		}

		// 24 hours past the end of its parse, the file that was read goes.
		// The one a parse still reads stays, 45 hours after its upload.
		h.advance(20 * time.Hour)
		if due := h.sweep(); !slices.Equal(ids(due.Files), []string{"fil_again", "fil_read"}) {
			t.Fatalf("24 hours after its parse ended the sweep lists %+v", due.Files)
		}
		if _, err := h.store.File(ctx, "fil_busy"); err != nil {
			t.Fatalf("a file a parse still reads was removed: %v", err)
		}
		// Its parse ends, and the file is kept for its retention from there.
		if err := h.store.Cancel(ctx, "prs_busy"); err != nil {
			t.Fatal(err)
		}
		if due := h.sweep(); slices.Contains(ids(due.Files), "fil_busy") {
			t.Fatal("a file was removed at the end of the parse that read it")
		}
		h.advance(24 * time.Hour)
		if due := h.sweep(); !slices.Contains(ids(due.Files), "fil_busy") {
			t.Fatalf("24 hours after its parse was canceled the sweep lists %+v", due.Files)
		}
		if _, err := h.store.File(ctx, "fil_forever"); err != nil {
			t.Fatalf("a file with no retention was removed: %v", err)
		}
	})
}

// TestAnUploadOfTheSameBytesKeepsTheFile: an upload of bytes the owner has
// is a use of the file. It is kept for that upload's retention from then,
// whether the upload found the file by its content or met it in the insert.
func TestAnUploadOfTheSameBytesKeepsTheFile(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.kept("alice", "fil_1", "aa", 24*time.Hour)
		h.advance(23 * time.Hour)

		second := file("alice", "fil_2", "aa")
		second.Retention = 12 * time.Hour
		got, created, err := h.store.InsertFile(ctx, second)
		if err != nil || created || got.ID != "fil_1" || got.Key != "sources/alice/aa/fil_1" {
			t.Fatalf("the same bytes again: %+v, created %t, %v", got, created, err)
		}
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(h.now.Add(12*time.Hour)) {
			t.Fatalf("after the second upload the file expires at %v, want 12 hours from %v", got.ExpiresAt, h.now)
		}
		h.advance(2 * time.Hour)
		if due := h.sweep(); len(due.Files) != 0 {
			t.Fatalf("25 hours after the first upload and 2 after the second, the sweep lists %+v", due.Files)
		}

		if err := h.store.KeepFile(ctx, "fil_1", 48*time.Hour); err != nil {
			t.Fatal(err)
		}
		if got, err := h.store.File(ctx, "fil_1"); err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(h.now.Add(48*time.Hour)) {
			t.Fatalf("a file kept for 48 hours expires at %+v, %v", got.ExpiresAt, err)
		}
		if err := h.store.KeepFile(ctx, "fil_1", 0); err != nil {
			t.Fatal(err)
		}
		if got, err := h.store.File(ctx, "fil_1"); err != nil || got.ExpiresAt != nil {
			t.Fatalf("a file kept with no retention expires at %+v, %v", got.ExpiresAt, err)
		}
	})
}

// TestADeleteThatStoppedHalfwayIsFinishedByTheSweep: a delete a request
// began marks the row and removes the object and the row after. One that
// stopped after the mark leaves a row that names its object, and the sweep
// lists it once the sweep interval has passed, so nothing is left behind
// for good. A delete that is still under way is not listed.
func TestADeleteThatStoppedHalfwayIsFinishedByTheSweep(t *testing.T) {
	logic(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		stuck := h.kept("alice", "fil_stuck", "aa", 0)
		key, err := h.store.DeleteFile(ctx, "alice", "fil_stuck")
		if err != nil || key != stuck.Key {
			t.Fatalf("beginning the delete: %q, %v", key, err)
		}
		// The first sweep is due at once: a delete that began this instant
		// is still the request's own.
		due, err := h.store.Expired(ctx, 100)
		if err != nil || !due.Due || len(due.Files) != 0 {
			t.Fatalf("a delete under way was listed: %+v, %v", due, err)
		}
		if due := h.sweep(); !slices.Equal(ids(due.Files), []string{"fil_stuck"}) || due.Files[0].Key != stuck.Key {
			t.Fatalf("a delete that stopped halfway is listed as %+v", due.Files)
		}
	})
}
