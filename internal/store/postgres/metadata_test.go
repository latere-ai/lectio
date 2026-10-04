// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/store/postgres/migrations"
	"latere.ai/x/lectio/internal/tasks"
)

// What the API serves from and a worker runs a task with: files, the
// members of a parse its caller chose, the reads kept for reuse, and the
// part of a parse a claim carries. Each case runs on a direct connection, in
// exec mode and behind a transaction-mode pooler, like every store case.

// file is a file row of an owner for bytes named by a digest.
func file(owner, id, sha string) postgres.File {
	return postgres.File{
		ID: id, Owner: owner, Name: "report.pdf", Size: 1200, SHA256: sha,
		MediaType: "application/pdf", Key: "sources/" + owner + "/" + sha + "/" + id,
	}
}

// stored writes a file's row and fails the case when it was there before.
func (h *harness) stored(f postgres.File) postgres.File {
	h.t.Helper()
	got, created, err := h.store.InsertFile(context.Background(), f)
	if err != nil || !created {
		h.t.Fatalf("writing %s: created %t, %v", f.ID, created, err)
	}
	return got
}

// TestTheSameBytesAreOneFilePerOwner: an owner has one file per digest,
// however many uploads carry the bytes and however many of them arrive at
// once; another owner's upload of the same bytes is a file of its own, and
// neither reads the other's.
func TestTheSameBytesAreOneFilePerOwner(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		first := h.stored(file("alice", "fil_1", "aa"))
		if first.CreatedAt.IsZero() || first.Key != "sources/alice/aa/fil_1" || first.Size != 1200 {
			t.Fatalf("a stored file is %+v", first)
		}
		again, created, err := h.store.InsertFile(ctx, file("alice", "fil_2", "aa"))
		if err != nil || created || again.ID != "fil_1" {
			t.Fatalf("the same bytes again: %+v, created %t, %v", again, created, err)
		}
		if other := h.stored(file("bob", "fil_3", "aa")); other.ID != "fil_3" {
			t.Fatalf("another owner's upload of the same bytes is %+v", other)
		}
		if got, err := h.store.File(ctx, "alice", "fil_1"); err != nil || got.SHA256 != "aa" || got.MediaType != "application/pdf" {
			t.Fatalf("reading a file: %+v, %v", got, err)
		}
		if _, err := h.store.File(ctx, "bob", "fil_1"); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("another owner read the file: %v", err)
		}
		if got, ok, err := h.store.FileByContent(ctx, "alice", "aa"); err != nil || !ok || got.ID != "fil_1" {
			t.Fatalf("a file by its content: %+v, %t, %v", got, ok, err)
		}
		if _, ok, err := h.store.FileByContent(ctx, "alice", "bb"); err != nil || ok {
			t.Fatalf("a file for bytes the owner does not have: %t, %v", ok, err)
		}

		// 8 uploads of one new digest at once write one row.
		var made atomic.Int64
		var wg sync.WaitGroup
		ids := make([]string, 8)
		for i := range ids {
			wg.Go(func() {
				got, created, err := h.store.InsertFile(ctx, file("alice", fmt.Sprintf("fil_c%d", i), "cc"))
				if err != nil {
					t.Errorf("an upload at once: %v", err)
					return
				}
				if created {
					made.Add(1)
				}
				ids[i] = got.ID
			})
		}
		wg.Wait()
		if made.Load() != 1 || strings.Count(strings.Join(ids, ","), ids[0]) != len(ids) {
			t.Fatalf("8 uploads of the same bytes made %d files: %v", made.Load(), ids)
		}
	})
}

// TestAFileAParseReadsIsNotDeleted: a file that a parse which has not ended
// reads is refused. Once deleted it is gone for every caller, a submit that
// names it included, before its object and its row are, and the same bytes
// uploaded again are a new file.
func TestAFileAParseReadsIsNotDeleted(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.stored(file("alice", "fil_1", "aa"))
		h.submit(postgres.Submission{Parse: "prs_a", Owner: "alice", File: "fil_1"})
		if _, err := h.store.DeleteFile(ctx, "alice", "fil_1"); fault.CodeOf(err) != fault.NotTerminal {
			t.Fatalf("deleting a file a running parse reads: %v", err)
		}
		if _, err := h.store.DeleteFile(ctx, "bob", "fil_1"); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("deleting another owner's file: %v", err)
		}
		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatal(err)
		}
		key, err := h.store.DeleteFile(ctx, "alice", "fil_1")
		if err != nil || key != "sources/alice/aa/fil_1" {
			t.Fatalf("deleting the file: %q, %v", key, err)
		}
		if _, err := h.store.File(ctx, "alice", "fil_1"); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("a file whose delete began was read: %v", err)
		}
		if _, err := h.store.DeleteFile(ctx, "alice", "fil_1"); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("deleting it twice: %v", err)
		}
		if _, _, err := h.store.Submit(ctx, filled(postgres.Submission{Parse: "prs_b", Owner: "alice", File: "fil_1"})); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("a submit that names a deleted file: %v", err)
		}
		if _, _, err := h.store.Submit(ctx, filled(postgres.Submission{Parse: "prs_c", Owner: "bob", File: "fil_1"})); fault.CodeOf(err) != fault.FileNotFound {
			t.Fatalf("a submit that names another owner's file: %v", err)
		}
		// The row of the delete that began does not stand in the way of the
		// same bytes.
		if again := h.stored(file("alice", "fil_2", "aa")); again.ID != "fil_2" {
			t.Fatalf("the same bytes after a delete are %+v", again)
		}
		if err := h.store.ForgetFile(ctx, "fil_1"); err != nil {
			t.Fatal(err)
		}
		if err := h.store.ForgetFile(ctx, "fil_2"); err != nil {
			t.Fatal(err)
		}
		if n := value[int64](h, `SELECT count(*) FROM files`); n != 1 {
			t.Fatalf("%d file rows are left, want the one that was not deleted", n)
		}
	})
}

// TestAParseKeepsWhatItsCallerChose: the members of a submit that the
// control plane does not read are kept as they came and returned with the
// parse, to its owner and to no one else.
func TestAParseKeepsWhatItsCallerChose(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.stored(file("alice", "fil_1", "aa"))
		h.submit(postgres.Submission{
			Parse: "prs_a", Owner: "alice", File: "fil_1", Pin: stub, Priority: 3, AllowFailedPages: 2, Class: tasks.Batch,
			Options: postgres.ParseOptions{Pages: "1-3,7", Languages: []string{"de", "en"}, Reuse: true},
			Labels:  map[string]string{"batch": "2026-10"},
			Origin:  &postgres.Origin{Store: "example", Path: "reports/q3.pdf", Version: "4"},
		})
		p, err := h.store.ParseOf(ctx, "alice", "prs_a")
		if err != nil {
			t.Fatal(err)
		}
		if p.File != "fil_1" || p.Options.Pages != "1-3,7" || strings.Join(p.Options.Languages, ",") != "de,en" || !p.Options.Reuse ||
			p.Labels["batch"] != "2026-10" || p.Origin == nil || *p.Origin != (postgres.Origin{Store: "example", Path: "reports/q3.pdf", Version: "4"}) ||
			p.Pin != stub || p.Priority != 3 || p.AllowFailedPages != 2 || p.Class != tasks.Batch || p.Terminal() {
			t.Fatalf("the parse its owner reads is %+v, origin %+v", p, p.Origin)
		}
		if _, err := h.store.ParseOf(ctx, "bob", "prs_a"); fault.CodeOf(err) != fault.ParseNotFound {
			t.Fatalf("another owner read the parse: %v", err)
		}
		// A parse submitted with none of them has none.
		h.submit(postgres.Submission{Parse: "prs_b", Owner: "alice"})
		if p := h.parse("prs_b"); p.File != "" || p.Origin != nil || len(p.Labels) != 0 || p.Options.Reuse || p.PagesReused != 0 {
			t.Fatalf("a parse submitted with nothing chosen is %+v", p)
		}
	})
}

// TestASubmitIsSafeToRepeatWithAKey: for 24 hours a second submit of the
// owner with the same idempotency key answers the parse the first made, and
// is refused when its body differs. Two submits with one key at once make
// one parse, in one group or in two.
func TestASubmitIsSafeToRepeatWithAKey(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		keyed := func(id, owner, key, digest string) postgres.Submission {
			return filled(postgres.Submission{Parse: id, Owner: owner, IdempotencyKey: key, BodyDigest: digest})
		}
		if id, created, err := h.store.Submit(ctx, keyed("prs_1", "alice", "k1", "d1")); err != nil || !created || id != "prs_1" {
			t.Fatalf("the first submit: %q, %t, %v", id, created, err)
		}
		if id, created, err := h.store.Submit(ctx, keyed("prs_2", "alice", "k1", "d1")); err != nil || created || id != "prs_1" {
			t.Fatalf("the same key and body again: %q, %t, %v", id, created, err)
		}
		if _, _, err := h.store.Submit(ctx, keyed("prs_3", "alice", "k1", "d2")); fault.CodeOf(err) != fault.IdempotencyConflict {
			t.Fatalf("the same key with another body: %v", err)
		}
		if id, created, err := h.store.Submit(ctx, keyed("prs_4", "bob", "k1", "d9")); err != nil || !created || id != "prs_4" {
			t.Fatalf("another owner's submit with the same key: %q, %t, %v", id, created, err)
		}
		if id, created, err := h.store.Submit(ctx, keyed("prs_1", "alice", "", "")); err != nil || created || id != "prs_1" {
			t.Fatalf("a submit repeated under its parse's id: %q, %t, %v", id, created, err)
		}

		// One key, 6 submits at once, each into a group of its own so that
		// no lock on a group's row orders them: one parse.
		var wg sync.WaitGroup
		ids := make([]string, 6)
		var made atomic.Int64
		for i := range ids {
			wg.Go(func() {
				sub := keyed(fmt.Sprintf("prs_c%d", i), "carol", "k2", "d1")
				sub.Group = fmt.Sprintf("team-%d", i)
				id, created, err := h.store.Submit(ctx, sub)
				if err != nil {
					t.Errorf("a submit at once: %v", err)
					return
				}
				if created {
					made.Add(1)
				}
				ids[i] = id
			})
		}
		wg.Wait()
		if made.Load() != 1 || strings.Count(strings.Join(ids, ","), ids[0]) != len(ids) {
			t.Fatalf("6 submits with one key made %d parses: %v", made.Load(), ids)
		}
		if n := value[int64](h, `SELECT count(*) FROM parses WHERE owner = 'carol'`); n != 1 {
			t.Fatalf("carol has %d parses, want 1", n)
		}

		// The key is held for 24 hours from its parse's submit.
		h.advance(24*time.Hour - time.Second)
		if id, created, err := h.store.Submit(ctx, keyed("prs_5", "alice", "k1", "d1")); err != nil || created || id != "prs_1" {
			t.Fatalf("inside the 24 hours: %q, %t, %v", id, created, err)
		}
		h.advance(2 * time.Second)
		if id, created, err := h.store.Submit(ctx, keyed("prs_6", "alice", "k1", "d2")); err != nil || !created || id != "prs_6" {
			t.Fatalf("after the 24 hours: %q, %t, %v", id, created, err)
		}
	})
}

// TestParsesAreListedForTheirOwner: an owner's parses, newest first, a page
// at a time, narrowed by state, file, origin path and labels, and never
// another owner's.
func TestParsesAreListedForTheirOwner(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.stored(file("alice", "fil_1", "aa"))
		h.submit(postgres.Submission{Parse: "prs_1", Owner: "alice", File: "fil_1", Labels: map[string]string{"batch": "oct", "kind": "invoice"}, Origin: &postgres.Origin{Path: "a/1.png"}})
		h.submit(postgres.Submission{Parse: "prs_2", Owner: "alice", Labels: map[string]string{"batch": "oct"}})
		h.submit(postgres.Submission{Parse: "prs_3", Owner: "alice", File: "fil_1"})
		h.submit(postgres.Submission{Parse: "prs_4", Owner: "bob"})
		if err := h.store.Cancel(ctx, "prs_2"); err != nil {
			t.Fatal(err)
		}
		listed := func(f postgres.Filter, after string, limit int) (string, bool) {
			t.Helper()
			got, more, err := h.store.Parses(ctx, "alice", f, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, len(got))
			for i, p := range got {
				ids[i] = p.ID
			}
			return strings.Join(ids, " "), more
		}
		for name, tc := range map[string]struct {
			filter postgres.Filter
			after  string
			limit  int
			want   string
			more   bool
		}{
			"all":             {postgres.Filter{}, "", 50, "prs_3 prs_2 prs_1", false},
			"the first page":  {postgres.Filter{}, "", 2, "prs_3 prs_2", true},
			"the next page":   {postgres.Filter{}, "prs_2", 2, "prs_1", false},
			"a whole page":    {postgres.Filter{}, "", 3, "prs_3 prs_2 prs_1", false},
			"by state":        {postgres.Filter{State: "canceled"}, "", 50, "prs_2", false},
			"by file":         {postgres.Filter{File: "fil_1"}, "", 50, "prs_3 prs_1", false},
			"by origin path":  {postgres.Filter{OriginPath: "a/1.png"}, "", 50, "prs_1", false},
			"by a label":      {postgres.Filter{Labels: map[string]string{"batch": "oct"}}, "", 50, "prs_2 prs_1", false},
			"by two labels":   {postgres.Filter{Labels: map[string]string{"batch": "oct", "kind": "invoice"}}, "", 50, "prs_1", false},
			"by another":      {postgres.Filter{Labels: map[string]string{"batch": "nov"}}, "", 50, "", false},
			"by all together": {postgres.Filter{State: "queued", File: "fil_1", OriginPath: "a/1.png", Labels: map[string]string{"kind": "invoice"}}, "", 50, "prs_1", false},
		} {
			if got, more := listed(tc.filter, tc.after, tc.limit); got != tc.want || more != tc.more {
				t.Errorf("%s: %q, more %t, want %q, more %t", name, got, more, tc.want, tc.more)
			}
		}
		if got, _, err := h.store.Parses(ctx, "carol", postgres.Filter{}, "", 50); err != nil || len(got) != 0 {
			t.Fatalf("an owner with no parse lists %d, %v", len(got), err)
		}
	})
}

// TestDeleteParseRemovesItsRows: a parse that has not ended is refused. The
// delete of one that has takes its task rows and the reads kept from it.
func TestDeleteParseRemovesItsRows(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Owner: "alice", ReadBase: "base"}, 2)
		w.settle(done(w.claim(1, 1)[0]))
		if err := h.store.DeleteParse(ctx, "alice", "prs_a"); fault.CodeOf(err) != fault.NotTerminal {
			t.Fatalf("deleting a parse that has not ended: %v", err)
		}
		if err := h.store.DeleteParse(ctx, "bob", "prs_a"); fault.CodeOf(err) != fault.ParseNotFound {
			t.Fatalf("deleting another owner's parse: %v", err)
		}
		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatal(err)
		}
		if n := value[int64](h, `SELECT count(*) FROM reads`); n != 1 {
			t.Fatalf("%d reads are kept before the delete, want 1", n)
		}
		if err := h.store.DeleteParse(ctx, "alice", "prs_a"); err != nil {
			t.Fatal(err)
		}
		if n := value[int64](h, `SELECT (SELECT count(*) FROM parses) + (SELECT count(*) FROM tasks) + (SELECT count(*) FROM reads)`); n != 0 {
			t.Fatalf("%d rows of the parse are left", n)
		}
		if err := h.store.DeleteParse(ctx, "alice", "prs_a"); fault.CodeOf(err) != fault.ParseNotFound {
			t.Fatalf("deleting it twice: %v", err)
		}
	})
}

// manifest is a manifest as a worker's prepare writes one.
const manifest = `{"media_type":"application/pdf","pages_total":9,"selected":[1,2,3],"source":"reader","work":"sources/alice/aa/fil_1","token":1}`

// TestAClaimCarriesWhatItsTaskNeeds: a worker runs a task from its claim
// alone. Prepare is told the file and the selection, a page the manifest
// without its list of pages and the languages, and assemble the manifest
// whole.
func TestAClaimCarriesWhatItsTaskNeeds(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.stored(file("alice", "fil_1", "aa"))
		h.submit(postgres.Submission{
			Parse: "prs_a", Owner: "alice", File: "fil_1",
			Options: postgres.ParseOptions{Pages: "1-3", Languages: []string{"de"}},
		})
		prepare := w.claim(1, 1)[0]
		if c := prepare.Context; c.Owner != "alice" || c.File == nil || *c.File != (tasks.Source{Key: "sources/alice/aa/fil_1", Name: "report.pdf", MediaType: "application/pdf"}) ||
			c.Pages != "1-3" || c.Languages != nil || c.Manifest != nil || c.Reuse != "" {
			t.Fatalf("the context of prepare is %+v, file %+v", c, c.File)
		}
		settle := done(prepare)
		settle.Prepare = &tasks.Prepared{Manifest: json.RawMessage(manifest), Pages: []int{1, 2, 3}}
		w.settle(settle)

		claims := w.claim(3, 3)
		for _, c := range claims {
			var m map[string]any
			if err := json.Unmarshal(c.Context.Manifest, &m); err != nil {
				t.Fatalf("the manifest of %s: %v", c.Task, err)
			}
			if _, listed := m["selected"]; listed || m["media_type"] != "application/pdf" || m["work"] != "sources/alice/aa/fil_1" || m["token"] != 1.0 ||
				strings.Join(c.Context.Languages, ",") != "de" || c.Context.File != nil || c.Context.Pages != "" || c.Context.Owner != "alice" {
				t.Fatalf("the context of %s is %+v, manifest %v", c.Task, c.Context, m)
			}
		}
		w.settle(done(claims[0]), done(claims[1]), done(claims[2]))
		assemble := w.claim(1, 1)[0]
		var m struct {
			Selected []int `json:"selected"`
		}
		if err := json.Unmarshal(assemble.Context.Manifest, &m); err != nil || len(m.Selected) != 3 || assemble.Context.Languages != nil {
			t.Fatalf("the context of assemble is %+v, selected %v, %v", assemble.Context, m.Selected, err)
		}
	})
}

// read is a settle of a page that was read, with what the page says of its
// result.
func read(c tasks.Claim, result string) tasks.Settle {
	s := done(c)
	s.Result = json.RawMessage(result)
	return s
}

// TestAPageReadWholeIsKeptAndTaken: a page that was read whole is kept
// under what was read, per owner. A later parse of the same owner that would
// do the same read is told where the result is, when it takes earlier reads,
// and counts the page as reused. A page that failed or whose reply was cut
// is not kept, and neither is anything of a parse whose readers promise
// nothing about their results.
func TestAPageReadWholeIsKeptAndTaken(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		w := h.worker()
		reuse := postgres.ParseOptions{Reuse: true}
		h.reading(w, postgres.Submission{Parse: "prs_a", Owner: "alice", ReadBase: "base", Options: reuse, AllowFailedPages: 1}, 4)
		first := w.claim(4, 4)
		for _, c := range first {
			if c.Context.Reuse != "" {
				t.Fatalf("a page nobody read before is told of a kept read: %+v", c)
			}
		}
		w.settle(read(first[0], `{"blocks":3}`), read(first[1], `{"blocks":2,"truncated":true}`),
			ended(first[2], tasks.Permanent, "page_unreadable"), read(first[3], `{"blocks":0}`))
		if got := h.task("prs_a", "page-1"); string(got.Result) != `{"blocks": 3}` {
			t.Fatalf("the row of a page that was read holds the result %s", got.Result)
		}
		if _, ok, err := h.store.Task(context.Background(), "prs_a", "page-9"); err != nil || ok {
			t.Fatalf("a task that is not there: %t, %v", ok, err)
		}
		assembled := func() {
			t.Helper()
			w.settle(done(w.claim(1, 1)[0]))
		}
		assembled()
		kept := value[string](h, `SELECT string_agg(read_key || '=' || output, ',' ORDER BY read_key) FROM reads WHERE owner = 'alice'`)
		if want := "base:1=" + done(first[0]).Output + ",base:4=" + done(first[3]).Output; kept != want {
			t.Fatalf("the reads kept are %q, want %q", kept, want)
		}

		// The same read by the same owner: the pages kept are taken.
		h.reading(w, postgres.Submission{Parse: "prs_b", Owner: "alice", ReadBase: "base", Options: reuse}, 4)
		second := w.claim(4, 4)
		for i, want := range []string{done(first[0]).Output, "", "", done(first[3]).Output} {
			if second[i].Context.Reuse != want {
				t.Fatalf("page %d of the second parse is told %q, want %q", i+1, second[i].Context.Reuse, want)
			}
		}
		w.settle(read(second[0], `{"blocks":3,"reused":true}`), read(second[1], `{"blocks":2}`), read(second[2], `{"blocks":1}`), read(second[3], `{"blocks":0,"reused":true}`))
		if p := h.parse("prs_b"); p.PagesReused != 2 || p.PagesDone != 4 {
			t.Fatalf("the second parse counts %+v", p)
		}
		assembled()
		// A page taken from a read stays kept where it was, and the pages
		// the second parse read itself are kept from it.
		kept = value[string](h, `SELECT string_agg(read_key || '=' || parse_id, ',' ORDER BY read_key) FROM reads WHERE owner = 'alice'`)
		if kept != "base:1=prs_a,base:2=prs_b,base:3=prs_b,base:4=prs_a" {
			t.Fatalf("after the second parse the reads kept are %q", kept)
		}

		// A parse that takes no earlier read is told of none, and what it
		// reads replaces what was kept. Another owner, another base, and a
		// parse with no base are told of none either.
		h.reading(w, postgres.Submission{Parse: "prs_c", Owner: "alice", ReadBase: "base"}, 1)
		c := w.claim(1, 1)[0]
		if c.Context.Reuse != "" {
			t.Fatalf("a parse that takes no earlier read is told %q", c.Context.Reuse)
		}
		w.settle(read(c, `{"blocks":3}`))
		assembled()
		if id := value[string](h, `SELECT parse_id FROM reads WHERE read_key = 'base:1'`); id != "prs_c" {
			t.Fatalf("the read of page 1 is kept from %s, want the parse that read it last", id)
		}
		for _, sub := range []postgres.Submission{
			{Parse: "prs_d", Owner: "bob", ReadBase: "base", Options: reuse},
			{Parse: "prs_e", Owner: "alice", ReadBase: "other", Options: reuse},
			{Parse: "prs_f", Owner: "alice", Options: reuse},
		} {
			h.reading(w, sub, 1)
			c := w.claim(1, 1)[0]
			if c.Context.Reuse != "" {
				t.Fatalf("%s is told of the read %q", sub.Parse, c.Context.Reuse)
			}
			w.settle(done(c))
			assembled()
		}
		if n := value[int64](h, `SELECT count(*) FROM reads WHERE parse_id = 'prs_f'`); n != 0 {
			t.Fatalf("a parse with no base kept %d reads", n)
		}
	})
}

// TestTheDatabaseAnswers: the readiness probe's question.
func TestTheDatabaseAnswers(t *testing.T) {
	h := open(t, server(t), modes[0], defaults())
	if err := h.store.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEveryMigrationGoesDownAndUpAgain: each migration's down file leaves
// the schema the one before it wrote, so the store opens and runs a parse
// after the whole schema went down and up again.
func TestEveryMigrationGoesDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	dsn := database(t, server(t))
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	url, err := postgres.MigrationURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		t.Fatal(err)
	}
	highest, err := migrations.Highest()
	if err != nil {
		t.Fatal(err)
	}
	// One step at a time, down to nothing, so each down file is run against
	// the schema it is written for.
	for range highest {
		if err := m.Steps(-1); err != nil {
			t.Fatalf("a migration did not go down: %v", err)
		}
	}
	if err := m.Up(); err != nil {
		t.Fatalf("the migrations did not go up again: %v", err)
	}
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Fatalf("closing the migrator: %v, %v", srcErr, dbErr)
	}

	store, err := postgres.Open(ctx, dsn, postgres.Options{Settings: defaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := &harness{t: t, store: store, now: epoch}
	store.SetClock(h.now)
	w := h.worker()
	h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)
	w.settle(done(w.claim(1, 1)[0]))
	w.settle(done(w.claim(1, 1)[0]))
	if p := h.parse("prs_a"); p.State != "succeeded" {
		t.Fatalf("after the schema went down and up a parse ended %+v", p)
	}
}

// TestAClaimItsWorkerNeverHeardOfReturnsToTheQueue: a task the store handed
// out in a reply that was lost on its way is leased to a worker that does
// not know it. The worker's next exchange names what it holds, and the task
// it does not name goes back to the queue with no counter changed, for any
// worker to take under a new token.
func TestAClaimItsWorkerNeverHeardOfReturnsToTheQueue(t *testing.T) {
	everywhere(t, defaults(), func(t *testing.T, h *harness) {
		w, other := h.worker(), h.worker()
		h.reading(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 3)
		claims := w.claim(2, 2)
		delete(w.held, tasks.Ref{Parse: claims[1].Parse, Task: claims[1].Task})

		if reply := w.exchange(0); len(reply.Lost) != 0 || len(reply.Refused) != 0 {
			t.Fatalf("the exchange that names one of two claims answered %+v", reply)
		}
		got := h.task("prs_a", "page-2")
		if got.State != tasks.Queued || got.Attempt != 0 || got.Expiries != 0 || got.Calling || got.Reader != "" || got.LeaseOwner != "" {
			t.Fatalf("the claim the worker did not name is %+v", got)
		}
		if got := h.task("prs_a", "page-1"); got.State != tasks.Leased || got.LeaseOwner != w.id {
			t.Fatalf("the claim the worker named is %+v", got)
		}

		c := other.claim(1, 1)[0]
		if c.Task != "page-2" || c.Token != 2 || c.Alone {
			t.Fatalf("the task that returned was claimed as %+v", c)
		}
		// Had the first worker run it after all, its settle is under a token
		// that is no longer the task's.
		if reply := w.exchange(0, done(claims[1])); len(reply.Refused) != 1 {
			t.Fatalf("a settle under the token of a reply that was lost was not refused: %+v", reply)
		}
		other.settle(done(c))
	})
}
