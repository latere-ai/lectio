// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// withDescribers are the settings of a case about work on an ended parse:
// the one reader is the policy's extractor and its describer too.
func withDescribers() tasks.Settings {
	s := defaults()
	s.DescribeChain = []string{stub}
	return s
}

// through runs a submitted parse of n pages to its end: its prepare, its
// pages and its assemble, by one worker, while nothing else can be claimed.
func (h *harness) through(w *worker, parse string, n int) {
	h.t.Helper()
	c := w.claim(1, 1)[0]
	if c.Parse != parse || c.Kind != tasks.Prepare {
		h.t.Fatalf("claimed %s/%s where %s/prepare was the only task", c.Parse, c.Task, parse)
	}
	w.settle(prepared(c, n))
	var settles []tasks.Settle
	for _, c := range w.claim(n, n) {
		settles = append(settles, done(c))
	}
	w.settle(settles...)
	w.settle(done(w.claim(1, 1)[0]))
	if p := h.parse(parse); p.State != "succeeded" {
		h.t.Fatalf("%s ended %s", parse, p.State)
	}
}

// asked is the request of the extractions of this suite: a schema with one
// member, written with its members in an order a JSON value would not keep.
const asked = `{"schema":{"type":"object","properties":{"total":{"type":"number"},"a":{"const":null}}},"citations":true}`

// field asks an extraction of a parse, pinned to an extractor when pin
// names one, with an hour to end in.
func (h *harness) field(parse, name, pin string) {
	h.t.Helper()
	err := h.store.CreateField(context.Background(), postgres.FieldRequest{
		Parse: parse, Name: name, Request: asked, Pin: pin, Deadline: time.Hour,
	})
	if err != nil {
		h.t.Fatalf("asking the extraction %s of %s: %v", name, parse, err)
	}
}

// read returns one extraction of a parse.
func (h *harness) read(parse, name string) postgres.Field {
	h.t.Helper()
	f, ok, err := h.store.Field(context.Background(), parse, name)
	if err != nil || !ok {
		h.t.Fatalf("reading the extraction %s of %s: found %t, %v", name, parse, ok, err)
	}
	return f
}

// figures starts a run over the figures of the refs, each on the page its
// ref names, with an hour to end in.
func (h *harness) figures(parse, pin string, redo bool, refs ...string) {
	h.t.Helper()
	run := postgres.FigureStart{Parse: parse, Pin: pin, Redo: redo, Deadline: time.Hour}
	for _, ref := range refs {
		page, _, _ := strings.Cut(ref, ".")
		n, err := strconv.Atoi(page)
		if err != nil {
			h.t.Fatalf("the ref %q names no page", ref)
		}
		run.Figures = append(run.Figures, postgres.FigureAsk{
			Ref: ref, Page: n, PageKey: "parses/" + parse + "/pages/" + page + ".1.json", FigureKey: "what-" + ref + "-shows",
		})
	}
	if err := h.store.StartFigures(context.Background(), run); err != nil {
		h.t.Fatalf("starting the figure run of %s: %v", parse, err)
	}
}

// run returns the figure run of a parse and the figures it took.
func (h *harness) run(parse string) postgres.Figures {
	h.t.Helper()
	got, started, err := h.store.Figures(context.Background(), parse)
	if err != nil || !started {
		h.t.Fatalf("reading the figure run of %s: started %t, %v", parse, started, err)
	}
	return got
}

// refused fails the case unless err carries the code.
func refused(t testing.TB, what string, err error, code fault.Code) {
	t.Helper()
	if fault.CodeOf(err) != code || err == nil {
		t.Fatalf("%s: %v, want %s", what, err, code)
	}
}

// readThrough queues a parse of n pages and runs it to its end.
func (h *harness) readThrough(w *worker, sub postgres.Submission, n int) {
	h.t.Helper()
	h.submit(sub)
	h.through(w, sub.Parse, n)
}

// step is a settle of an extraction that made one call and has another to
// make: it names the key of what it has so far.
func step(c tasks.Claim) tasks.Settle {
	s := ended(c, tasks.Continue, "")
	s.Output = "parses/" + c.Parse + "/fields/progress." + strconv.FormatInt(c.Token, 10) + ".json"
	s.Usage, s.Health = tasks.Usage{Calls: 1, InputTokens: 1000, OutputTokens: 50}, tasks.Healthy
	return s
}

// filledBy is a settle of an extraction's last call: the key of the result and
// what the field says of how it was filled.
func filledBy(c tasks.Claim) tasks.Settle {
	s := tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done, Units: 1, Health: tasks.Healthy}
	s.Output = "parses/" + c.Parse + "/fields/result." + strconv.FormatInt(c.Token, 10) + ".json"
	s.Result = []byte(`{"model":"m","constrained":false,"attempts":1,"windows":2}`)
	s.Usage = tasks.Usage{Calls: 1, InputTokens: 1000, OutputTokens: 50}
	return s
}

// described is a settle of a figure a describer described.
func described(c tasks.Claim) tasks.Settle {
	s := tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done, Units: 1, Health: tasks.Healthy}
	s.Output = "parses/" + c.Parse + "/figures/" + c.Task + "." + strconv.FormatInt(c.Token, 10) + ".json"
	s.Usage = tasks.Usage{Calls: 1, InputTokens: 300, OutputTokens: 30}
	return s
}

// metered reads the meter's rows of a kind as "reader:pages/calls/in/out".
func (h *harness) metered(kind string) string {
	h.t.Helper()
	return value[string](h, `SELECT coalesce(string_agg(reader || ':' || pages || '/' || calls || '/' || input_tokens || '/' || output_tokens, ' '
	                                  ORDER BY reader), '') FROM (
	                           SELECT reader, sum(pages) AS pages, sum(calls) AS calls, sum(input_tokens) AS input_tokens,
	                                  sum(output_tokens) AS output_tokens FROM usage WHERE kind = $1 GROUP BY reader) u`, kind)
}

// TestAnExtractionIsATaskOfItsParseOneCallAClaim: an extraction asked of a
// parse that has ended is a task of that parse at once, in its group and
// ahead of the group's pages. Its claim is admitted for the policy's
// extractor, takes a slot and a charge, and carries what was asked byte for
// byte, the document index and nothing of an earlier claim. A claim that
// has another call to make settles with the key of what it has and returns
// to the queue with no attempt spent; the next claim carries that key under
// the token after. The last settle fills the field, with what every claim
// used, and removes the task. Each settle meters its call under the kind
// and the extractor, and none moves the parse.
func TestAnExtractionIsATaskOfItsParseOneCallAClaim(t *testing.T) {
	everywhere(t, withDescribers(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice", Priority: 0}, 2)
		before := h.parse("prs_a")

		h.field("prs_a", "invoice", "")
		f := h.read("prs_a", "invoice")
		if f.State != postgres.FieldPending || f.DeadlineAt == nil || !f.DeadlineAt.Equal(h.now.Add(time.Hour)) || f.Output != "" {
			t.Fatalf("an extraction that was just asked is %+v", f)
		}
		row := h.task("prs_a", "extract-invoice")
		if row.Kind != tasks.Extract || row.State != tasks.Queued || row.Lane != "extract" || row.Seq != 0 || row.Group != "acme" {
			t.Fatalf("its task is %+v", row)
		}

		first := w.claim(1, 1)[0]
		if first.Kind != tasks.Extract || first.Reader != stub || first.Token != 1 || first.Context.Request != asked ||
			first.Context.Index != before.Index || first.Context.Progress != "" || first.Context.Owner != "alice" {
			t.Fatalf("the first claim is %+v", first)
		}
		if row = h.task("prs_a", "extract-invoice"); !row.Calling || row.Charged != 1 {
			t.Fatalf("the claimed task holds %+v", row)
		}
		w.settle(step(first))
		row = h.task("prs_a", "extract-invoice")
		if row.State != tasks.Queued || row.Attempt != 0 || row.Calling || row.Output != step(first).Output || !row.AvailableAt.Equal(h.now) {
			t.Fatalf("between 2 calls the task is %+v", row)
		}
		if f = h.read("prs_a", "invoice"); f.State != postgres.FieldPending {
			t.Fatalf("between 2 calls the extraction is %+v", f)
		}

		second := w.claim(1, 1)[0]
		if second.Token != 2 || second.Context.Progress != step(first).Output || second.Attempt != 0 {
			t.Fatalf("the second claim is %+v", second)
		}
		w.settle(filledBy(second))
		f = h.read("prs_a", "invoice")
		if f.State != postgres.FieldSucceeded || f.Output != filledBy(second).Output || f.Error != nil || f.FinishedAt == nil ||
			f.Calls != 2 || f.InputTokens != 2000 || f.OutputTokens != 100 || !strings.Contains(string(f.Result), `"windows": 2`) {
			t.Fatalf("the filled extraction is %+v, result %s", f, f.Result)
		}
		if rows := h.states("prs_a"); rows != "" {
			t.Fatalf("the task rows left are %s", rows)
		}
		if got := h.metered("extract"); got != "stub:0/2/2000/100" {
			t.Fatalf("the meter of extractions reads %s", got)
		}
		after := h.parse("prs_a")
		if after.State != before.State || after.Events != before.Events || after.Index != before.Index || after.PagesDone != before.PagesDone ||
			after.Calls != before.Calls+2 || after.Fields != 1 {
			t.Fatalf("the extraction moved its parse from %+v to %+v", before, after)
		}

		// The list holds every extraction of the parse, by name.
		h.field("prs_a", "contract", "")
		all, err := h.store.Fields(context.Background(), "prs_a")
		if err != nil || len(all) != 2 || all[0].Name != "contract" || all[1].Name != "invoice" {
			t.Fatalf("the extractions of the parse are %+v, %v", all, err)
		}
		if _, ok, err := h.store.Field(context.Background(), "prs_a", "other"); ok || err != nil {
			t.Fatalf("an extraction nobody asked is found: %t, %v", ok, err)
		}
		w.settle(filledBy(w.claim(1, 1)[0]))
	})
}

// TestAnExtractionAskedWhileItsParseRunsWaitsForItsEnd: an extraction asked
// of a parse that has not ended has no task and no deadline. Its task is
// queued in the transaction that ends the parse, whichever way the parse
// ends: by its assemble, by a cancel or by its deadline. Its time runs from
// there.
func TestAnExtractionAskedWhileItsParseRunsWaitsForItsEnd(t *testing.T) {
	logic(t, withDescribers(), func(t *testing.T, h *harness) {
		w := h.worker()
		for _, id := range []string{"prs_assembled", "prs_canceled", "prs_late"} {
			h.submit(postgres.Submission{Parse: id, Group: id, Deadline: 30 * time.Minute})
			h.field(id, "invoice", "")
			if f := h.read(id, "invoice"); f.State != postgres.FieldPending || f.DeadlineAt != nil {
				t.Fatalf("an extraction of a running parse is %+v", f)
			}
		}
		if n := value[int](h, `SELECT count(*) FROM tasks WHERE kind = 'extract'`); n != 0 {
			t.Fatalf("%d extractions have a task while their parses run", n)
		}

		// queued fails the case unless the extraction of a parse has a task
		// that waits, and time that runs from when its parse ended.
		queued := func(id string, ended time.Time) {
			t.Helper()
			f, row := h.read(id, "invoice"), h.task(id, "extract-invoice")
			if f.DeadlineAt == nil || !f.DeadlineAt.Equal(ended.Add(time.Hour)) || row.State != tasks.Queued || row.Kind != tasks.Extract {
				t.Fatalf("after %s ended at %v its extraction is %+v with the task %+v", id, ended, f, row)
			}
		}

		h.advance(time.Minute)
		if err := h.store.Cancel(context.Background(), "prs_canceled"); err != nil {
			t.Fatal(err)
		}
		queued("prs_canceled", h.now)

		// The parse that ends by its assemble: its prepare, a page, and the
		// assemble are the only tasks of it.
		for _, c := range w.claim(8, 3) {
			switch {
			case c.Parse == "prs_assembled" && c.Kind == tasks.Prepare:
				w.settle(prepared(c, 1))
			case c.Parse == "prs_canceled" && c.Kind == tasks.Extract:
				w.settle(filledBy(c))
			case c.Parse != "prs_late" || c.Kind != tasks.Prepare:
				t.Fatalf("claimed %s/%s where 2 prepares and an extraction waited", c.Parse, c.Task)
			}
		}
		w.settle(done(w.claim(1, 1)[0]))
		w.settle(done(w.claim(1, 1)[0]))
		queued("prs_assembled", h.now)

		// The third parse runs out of time: the sweep that fails it queues
		// what waited for it.
		h.advance(time.Hour)
		w.exchange(0)
		if p := h.parse("prs_late"); p.State != "failed" || p.Error.Code != "deadline_exceeded" {
			t.Fatalf("the late parse is %+v", p)
		}
		queued("prs_late", h.now)
		for _, c := range w.exchange(8).Claims {
			w.settle(filledBy(c))
		}
	})
}

// TestWhatAnExtractionIsRefusedFor: a parse that is not there, a name the
// parse has, a parse that holds as many extractions as it may, and a
// request with no name, no request or no deadline.
func TestWhatAnExtractionIsRefusedFor(t *testing.T) {
	logic(t, withDescribers(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.submit(postgres.Submission{Parse: "prs_a", Group: "acme"})
		ask := func(parse, name string) error {
			return h.store.CreateField(ctx, postgres.FieldRequest{Parse: parse, Name: name, Request: asked, Deadline: time.Hour})
		}
		refused(t, "a parse that is not there", ask("prs_gone", "invoice"), fault.ParseNotFound)
		h.field("prs_a", "invoice", "")
		refused(t, "a name the parse has", ask("prs_a", "invoice"), fault.Conflict)
		for i := 1; i < postgres.MaxFields; i++ {
			h.field("prs_a", "f"+strconv.Itoa(i), "")
		}
		refused(t, "one extraction more than a parse may hold", ask("prs_a", "last"), fault.Conflict)
		if p := h.parse("prs_a"); p.Fields != postgres.MaxFields {
			t.Fatalf("the parse counts %d extractions", p.Fields)
		}
		for name, req := range map[string]postgres.FieldRequest{
			"no name":     {Parse: "prs_a", Request: asked, Deadline: time.Hour},
			"no request":  {Parse: "prs_a", Name: "x", Deadline: time.Hour},
			"no parse":    {Name: "x", Request: asked, Deadline: time.Hour},
			"no deadline": {Parse: "prs_a", Name: "x", Request: asked},
		} {
			refused(t, name, h.store.CreateField(ctx, req), fault.InvalidRequest)
		}
		if err := h.store.Cancel(ctx, "prs_a"); err != nil {
			t.Fatal(err)
		}
		// A parse whose retention has ended is being removed.
		h.exec(`UPDATE parses SET expires_at = $1 WHERE parse_id = 'prs_a'`, h.now)
		refused(t, "a parse past its retention", ask("prs_a", "late"), fault.ParseNotFound)
		refused(t, "a figure run of a parse past its retention",
			h.store.StartFigures(ctx, postgres.FigureStart{Parse: "prs_a", Deadline: time.Hour}), fault.ParseNotFound)
		h.exec(`UPDATE parses SET expires_at = NULL WHERE parse_id = 'prs_a'`)
		h.exec(`SELECT lectio_drop('prs_a', 'extract', NULL)`)
	})
}

// extractors is settings with 2 extractors in the extract chain and one
// reader, so an extraction can move down a chain of its own.
func extractors() tasks.Settings {
	s := readers(tasks.Pool{Reader: stub, MaxInFlight: 100}, tasks.Pool{Reader: "text", MaxInFlight: 100, Cost: 2},
		tasks.Pool{Reader: "strong", MaxInFlight: 100, Cost: 5})
	s.ReadChain, s.ExtractChain, s.DescribeChain = []string{stub}, []string{"text", "strong"}, []string{"strong"}
	return s
}

// TestAnExtractionFollowsTheClassOfItsExtractorsError: an extraction's
// attempt ends as a page's does. A failure that may pass spends an attempt
// and waits a backoff, and at the bound of attempts the field fails with
// the error of its last attempt. A step that was answered gives the next
// call attempts of its own. A rate limit spends none and pauses the key
// scope. An extractor that cannot be the one moves the task down the
// policy's extract chain, where it is charged that extractor's cost, and an
// extraction that named its extractor stays with it. A failure that is the
// field's own fails it at once, with what its task said.
func TestAnExtractionFollowsTheClassOfItsExtractorsError(t *testing.T) {
	settings := extractors()
	settings.Attempts = 2
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)

		// A failure that may pass, then a step, then 2 failures in a row.
		h.field("prs_a", "retried", "")
		c := w.claim(1, 1)[0]
		if c.Reader != "text" {
			t.Fatalf("the extraction was claimed for %s, want the first of the extract chain", c.Reader)
		}
		w.settle(unhealthy(c))
		if row := h.task("prs_a", "extract-retried"); row.Attempt != 1 || row.State != tasks.Queued || !row.AvailableAt.After(h.now) {
			t.Fatalf("after a failure that may pass the task is %+v", row)
		}
		h.advance(5 * time.Second)
		c = w.claim(1, 1)[0]
		w.settle(step(c))
		if row := h.task("prs_a", "extract-retried"); row.Attempt != 0 {
			t.Fatalf("after a step the task has spent %d attempts", row.Attempt)
		}
		for range 2 {
			h.advance(5 * time.Second)
			w.settle(unhealthy(w.claim(1, 1)[0]))
		}
		if f := h.read("prs_a", "retried"); f.State != postgres.FieldFailed || f.Error.Code != "reader_unavailable" || f.Calls != 1 {
			t.Fatalf("at the bound of attempts the extraction is %+v, error %+v", f, f.Error)
		}

		// A rate limit: no attempt, and the task waits out the pause of its
		// key scope.
		h.field("prs_a", "limited", "")
		c = w.claim(1, 1)[0]
		w.settle(limited(c, 7*time.Second))
		if row := h.task("prs_a", "extract-limited"); row.Attempt != 0 || !row.AvailableAt.Equal(h.now.Add(7*time.Second)) {
			t.Fatalf("after a rate limit the task is %+v", row)
		}
		w.claim(1, 0)
		h.advance(8 * time.Second)
		c = w.claim(1, 1)[0]
		if c.Reader != "text" || c.Attempt != 0 {
			t.Fatalf("after the pause the task was claimed as %+v", c)
		}
		// Its extractor declines it: it goes to the next of the chain, which
		// costs more, and that one declines it too. No extractor is left.
		w.settle(declined(c, "schema_not_satisfied"))
		c = w.claim(1, 1)[0]
		if row := h.task("prs_a", "extract-limited"); c.Reader != "strong" || row.Charged != 5 || row.Lane != "extract@1" {
			t.Fatalf("a declined extraction was claimed for %s as %+v", c.Reader, row)
		}
		w.settle(declined(c, "schema_not_satisfied"))
		if f := h.read("prs_a", "limited"); f.State != postgres.FieldFailed || f.Error.Code != "schema_not_satisfied" {
			t.Fatalf("with no extractor left the extraction is %+v", f)
		}

		// One that named its extractor waits for it and never moves.
		h.field("prs_a", "pinned", "strong")
		c = w.claim(1, 1)[0]
		if c.Reader != "strong" || c.Pin != "strong" || h.task("prs_a", "extract-pinned").Lane != "extract:strong" {
			t.Fatalf("a pinned extraction was claimed as %+v", c)
		}
		w.settle(declined(c, "schema_not_satisfied"))
		if f := h.read("prs_a", "pinned"); f.State != postgres.FieldFailed {
			t.Fatalf("a pinned extraction its extractor declined is %+v", f)
		}

		// A failure of the field's own: at once, with what the task said.
		h.field("prs_a", "unsatisfied", "")
		c = w.claim(1, 1)[0]
		bad := ended(c, tasks.Permanent, "schema_not_satisfied")
		bad.Result = []byte(`{"attempts":3,"windows":1}`)
		w.settle(bad)
		f := h.read("prs_a", "unsatisfied")
		if f.State != postgres.FieldFailed || f.Error.Code != "schema_not_satisfied" || f.Output != "" || !strings.Contains(string(f.Result), `"attempts": 3`) {
			t.Fatalf("an extraction that did not satisfy its schema is %+v, result %s", f, f.Result)
		}
		if p := h.parse("prs_a"); p.State != "succeeded" {
			t.Fatalf("a failed extraction moved its parse to %s", p.State)
		}
	})
}

// TestAnExtractionStaysWithTheExtractorThatBeganIt: an extraction that has
// made a call was cut for the extractor that made it, and another would
// start it over and be paid for the same windows again. So when that
// extractor is paused between 2 claims the extraction waits for it, while
// one that has made no call yet is taken by the next of the chain. It is
// claimed for its own extractor when the pause ends, with no pin a caller
// set. It leaves the extractor only by the outcome that moves a task down
// the chain, starts over with the one after, stays with that one, and has
// no extractor to return to.
func TestAnExtractionStaysWithTheExtractorThatBeganIt(t *testing.T) {
	logic(t, extractors(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)
		h.field("prs_a", "long", "")
		h.field("prs_a", "other", "")
		claims := w.claim(2, 2)
		long, other := claims[0], claims[1]
		if long.Task != "extract-long" || long.Reader != "text" || other.Reader != "text" {
			t.Fatalf("the extractions were claimed as %s", names(claims))
		}
		// One makes its first call. The other is told to wait, which pauses
		// the extractor for 7 seconds.
		w.settle(step(long), limited(other, 7*time.Second))
		if row := h.task("prs_a", "extract-long"); !row.Stuck || row.Pin != "text" || row.Lane != "extract:text" || row.Output != step(long).Output {
			t.Fatalf("after its first call the extraction is %+v", row)
		}

		// While the extractor is paused, an extraction that has made no call
		// is taken by the next of the chain, and the one that has waits.
		h.field("prs_a", "fresh", "")
		fresh := w.claim(3, 1)[0]
		if fresh.Task != "extract-fresh" || fresh.Reader != "strong" {
			t.Fatalf("with the first extractor paused, %s was claimed for %s", fresh.Task, fresh.Reader)
		}
		h.advance(6 * time.Second)
		w.claim(3, 0)

		// The pause ends: it is claimed for its own extractor, with what it
		// had so far, and is told of no pin.
		h.advance(2 * time.Second)
		claims = w.claim(3, 2)
		long = claims[0]
		if long.Task != "extract-long" || long.Reader != "text" || long.Pin != "" || long.Context.Progress == "" || claims[1].Task != "extract-other" {
			t.Fatalf("after the pause the claims are %s, the first %+v", names(claims), long)
		}

		// The extractor declines it: it moves to the one after and is its
		// own no more, until that one has made a call.
		w.settle(declined(long, "schema_not_satisfied"))
		if row := h.task("prs_a", "extract-long"); row.Stuck || row.Pin != "" || row.Lane != "extract@1" {
			t.Fatalf("an extraction its extractor declined is %+v", row)
		}
		long = w.claim(1, 1)[0]
		if long.Reader != "strong" {
			t.Fatalf("it was claimed for %s", long.Reader)
		}
		w.settle(step(long))
		if row := h.task("prs_a", "extract-long"); !row.Stuck || row.Pin != "strong" || row.Lane != "extract:strong" {
			t.Fatalf("after a call of the second extractor the extraction is %+v", row)
		}
		// No extractor is left after the last: declined there, it fails.
		long = w.claim(1, 1)[0]
		w.settle(declined(long, "schema_not_satisfied"))
		if f := h.read("prs_a", "long"); f.State != postgres.FieldFailed || f.Error.Code != "schema_not_satisfied" || f.Calls != 2 {
			t.Fatalf("with no extractor left the extraction is %+v, error %+v", f, f.Error)
		}

		// 2 replies that were not usable move it once, as they move a page,
		// and it leaves its extractor then too.
		w.settle(done(claims[1]), done(fresh))
		h.field("prs_a", "garbled", "")
		c := w.claim(1, 1)[0]
		w.settle(step(c))
		for range 2 {
			h.advance(time.Minute)
			c = w.claim(1, 1)[0]
			bad := ended(c, tasks.Retryable, "schema_not_satisfied")
			bad.Invalid = true
			w.settle(bad)
		}
		if row := h.task("prs_a", "extract-garbled"); row.Stuck || row.Pin != "" || row.Lane != "extract@1" {
			t.Fatalf("after 2 replies that were not usable the extraction is %+v", row)
		}

		// One that named its extractor is pinned by its caller, and stays so.
		h.field("prs_a", "pinned", "text")
		h.advance(time.Minute)
		for _, c := range w.claim(2, 2) {
			if c.Task == "extract-pinned" {
				if c.Pin != "text" {
					t.Fatalf("a pinned extraction is told of the pin %q", c.Pin)
				}
				w.settle(step(c))
				continue
			}
			w.settle(done(c))
		}
		if row := h.task("prs_a", "extract-pinned"); row.Stuck || row.Pin != "text" {
			t.Fatalf("after a call a pinned extraction is %+v", row)
		}
		h.consistent()
	})
}

// TestAWorkerOfTheReleaseBeforeRunsBesideThisOne: a fleet is rolled one
// process at a time, so a worker that knows 3 kinds of task exchanges with
// a store that holds 5. Such a worker names no kind, and is handed no
// extraction and no figure, which it would fail: it runs the parses, and
// the extractions and the figures wait for a worker that names them. A
// worker is handed the tasks that call no model only when it runs both
// kinds of them.
func TestAWorkerOfTheReleaseBeforeRunsBesideThisOne(t *testing.T) {
	logic(t, extractors(), func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)
		h.field("prs_a", "invoice", "")
		h.figures("prs_a", "", false, "1.2", "1.3")

		before := h.worker()
		before.earlier = true
		before.claim(4, 0)
		for _, task := range []string{"extract-invoice", "figure-1.2", "figure-1.3"} {
			if row := h.task("prs_a", task); row.State != tasks.Queued || row.LeaseOwner != "" || row.LeasedAt != nil || row.Attempt != 0 {
				t.Fatalf("a worker of the release before touched %+v", row)
			}
		}

		// It runs a parse from its prepare to its end, past the extraction
		// and the figures that are ahead of the pages in the queue.
		h.submit(postgres.Submission{Parse: "prs_b", Group: "acme"})
		h.through(before, "prs_b", 2)

		// A worker that runs one of the 2 kinds that call no model is not
		// handed their lane, and is handed the extraction it names.
		h.submit(postgres.Submission{Parse: "prs_c", Group: "acme"})
		partial := h.worker()
		reply := partial.raw(tasks.Request{Free: 4, Kinds: []tasks.Kind{tasks.Prepare, tasks.Extract}})
		if len(reply.Claims) != 1 || reply.Claims[0].Task != "extract-invoice" {
			t.Fatalf("a worker that runs prepare and extract claimed %s", names(reply.Claims))
		}

		// A worker of this release takes what is left: the prepare, and the
		// 2 figures.
		got := map[tasks.Kind]int{}
		for _, c := range w.claim(4, 3) {
			got[c.Kind]++
		}
		if got[tasks.Prepare] != 1 || got[tasks.Figure] != 2 {
			t.Fatalf("a worker of this release claimed %v", got)
		}
		h.consistent()
	})
}

// TestAWorkerThatDiesMidExtractionLosesOneCall: an extraction whose worker
// dies between 2 of its calls returns to the queue with one expiry and what
// it had so far: the worker that takes it next is handed the key the last
// step left, and makes only the calls that were not made. One whose workers
// die at every claim fails with internal at the bound, and its field says
// so.
func TestAWorkerThatDiesMidExtractionLosesOneCall(t *testing.T) {
	settings := withDescribers()
	settings.Expiries = 2
	logic(t, settings, func(t *testing.T, h *harness) {
		// reap lets a live worker find a dead one: a lease passes while it
		// keeps exchanging.
		reap := func(live *worker) {
			for range 5 {
				h.advance(third - time.Second)
				live.exchange(0)
			}
		}
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)
		h.field("prs_a", "invoice", "")
		doomed := h.worker()
		first := doomed.claim(1, 1)[0]
		doomed.settle(step(first))
		doomed.claim(1, 1)

		// The worker dies in its second call: another returns the task.
		reap(w)
		row := h.task("prs_a", "extract-invoice")
		if row.State != tasks.Queued || row.Expiries != 1 || row.Attempt != 0 || row.Output != step(first).Output {
			t.Fatalf("after its worker died the task is %+v", row)
		}
		next := w.raw(tasks.Request{Free: 1}).Claims
		if len(next) != 1 || !next[0].Alone || next[0].Context.Progress != step(first).Output || next[0].Token != 3 {
			t.Fatalf("the worker that took it over was handed %+v", next)
		}

		// That worker dies too: the task is the cause, and its field fails.
		survivor := h.worker()
		reap(survivor)
		f := h.read("prs_a", "invoice")
		if f.State != postgres.FieldFailed || f.Error.Code != "internal" || f.Calls != 1 {
			t.Fatalf("an extraction that ended 2 workers is %+v, error %+v", f, f.Error)
		}
		if rows := h.states("prs_a"); rows != "" {
			t.Fatalf("the task rows left are %s", rows)
		}
	})
}

// TestWorkOutOfTimeIsGivenUp: nothing on an ended parse waits without
// bound. An extraction whose task has not ended by its deadline fails with
// deadline_exceeded, keeps what its steps used, and its task is dropped: a
// worker that still runs it is told it lost it, and its settle is refused.
// The figures a run has not described by its deadline are lost to it, and
// the run ends with the ones it described.
func TestWorkOutOfTimeIsGivenUp(t *testing.T) {
	settings := withDescribers()
	settings.SweepInterval = time.Second
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_a", Group: "acme"}, 1)
		h.field("prs_a", "running", "")
		c := w.claim(1, 1)[0]
		w.settle(step(c))
		held := w.claim(1, 1)[0]
		h.figures("prs_a", "", false, "1.2", "1.3")
		for _, c := range w.claim(8, 2) {
			if c.Task == "figure-1.2" {
				w.settle(described(c))
			}
		}
		// One more waits for a reader that is paused, and is never claimed.
		h.field("prs_a", "waiting", "")
		h.exec(`INSERT INTO pool_scopes (reader, scope, ceiling, paused_until) VALUES ($1, '', 1, $2)`, stub, h.now.Add(24*time.Hour))
		w.claim(1, 0)

		// The sweep gives the work up, and the worker's next exchange tells
		// it what it lost: the extraction and the figure it still ran.
		h.advance(61 * time.Minute)
		w.exchange(0)
		if reply := w.exchange(0); len(reply.Lost) != 2 {
			t.Fatalf("the worker was told it lost %v, want the extraction and the figure it held", reply.Lost)
		}
		for _, name := range []string{"running", "waiting"} {
			if f := h.read("prs_a", name); f.State != postgres.FieldFailed || f.Error.Code != "deadline_exceeded" || f.FinishedAt == nil {
				t.Fatalf("the extraction %s is %+v after its deadline", name, f)
			}
		}
		if f := h.read("prs_a", "running"); f.Calls != 1 || f.InputTokens != 1000 {
			t.Fatalf("the extraction that had made a call kept %+v of it", f)
		}
		got := h.run("prs_a")
		if got.Run.State != postgres.RunSucceeded || got.Run.Done != 1 || got.Run.Failed != 1 || got.Run.Open != 0 || got.Run.FinishedAt == nil {
			t.Fatalf("the run is %+v after its deadline", got.Run)
		}
		if lost := got.Figures[1]; lost.Ref != "1.3" || lost.State != postgres.FigureFailed || lost.Error.Code != "reader_unavailable" {
			t.Fatalf("the figure the run did not reach is %+v", lost)
		}
		if rows := h.states("prs_a"); rows != "" {
			t.Fatalf("the task rows left are %s", rows)
		}
		if refused := w.raw(tasks.Request{}, filledBy(held)).Refused; len(refused) != 1 {
			t.Fatalf("the settle of a task that was given up was accepted: %v", refused)
		}
	})
}

// TestAFigureRunIsOneTaskPerFigure: a run writes a row and a task per
// figure, in the order the figures come in, in the parse's group. A claim
// is admitted for the policy's describer and carries where the figure's
// page is stored and the languages hinted. A figure that was described
// holds the key of its description, its parse counts it, and the
// description is kept for the owner. A figure a describer cannot take says
// why. The run counts what ended, ends when nothing is open, and succeeded
// when it described any figure it set out to.
func TestAFigureRunIsOneTaskPerFigure(t *testing.T) {
	everywhere(t, withDescribers(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		sub := postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice", Options: postgres.ParseOptions{Languages: []string{"de"}}}
		h.readThrough(w, sub, 2)
		if _, started, err := h.store.Figures(ctx, "prs_a"); started || err != nil {
			t.Fatalf("a parse nobody asked about has a run: %t, %v", started, err)
		}

		h.figures("prs_a", "", false, "1.2", "2.1", "2.4")
		got := h.run("prs_a")
		if got.Run.State != postgres.RunRunning || got.Run.Total != 3 || got.Run.Open != 3 || !got.Run.StartedAt.Equal(h.now) || len(got.Figures) != 3 {
			t.Fatalf("a run that just began is %+v", got)
		}
		// The tokens of a run's tasks start at the run's number times 2^32.
		if rows := h.states("prs_a"); rows != "figure-1.2:queued/0/4294967296 figure-2.1:queued/0/4294967296 figure-2.4:queued/0/4294967296" {
			t.Fatalf("the run's tasks are %s", rows)
		}
		refused(t, "a second run while one is in flight",
			h.store.StartFigures(ctx, postgres.FigureStart{Parse: "prs_a", Deadline: time.Hour}), fault.Conflict)

		claims := w.claim(8, 3)
		first := claims[0]
		if first.Task != "figure-1.2" || first.Kind != tasks.Figure || first.Reader != stub || first.Context.Figure == nil ||
			first.Context.Figure.Page != "parses/prs_a/pages/1.1.json" || first.Context.Figure.Reuse != "" ||
			len(first.Context.Languages) != 1 || h.task("prs_a", "figure-1.2").Lane != "figure" {
			t.Fatalf("the first figure was claimed as %+v, figure %+v", first, first.Context.Figure)
		}
		w.settle(described(claims[0]), ended(claims[1], tasks.Permanent, "figure_unreadable"))
		got = h.run("prs_a")
		if got.Run.State != postgres.RunRunning || got.Run.Open != 1 || got.Run.Done != 1 || got.Run.Failed != 1 || got.Run.Calls != 1 || got.Run.InputTokens != 300 {
			t.Fatalf("with one figure open the run is %+v", got.Run)
		}
		if one, lost := got.Figures[0], got.Figures[1]; one.State != postgres.FigureSucceeded || one.Output != described(claims[0]).Output ||
			lost.State != postgres.FigureFailed || lost.Error.Code != "figure_unreadable" || lost.Output != "" {
			t.Fatalf("the figures are %+v", got.Figures)
		}
		w.settle(described(claims[2]))
		got = h.run("prs_a")
		if got.Run.State != postgres.RunSucceeded || got.Run.Open != 0 || got.Run.Done != 2 || got.Run.FinishedAt == nil || got.Run.Reused != 0 {
			t.Fatalf("the ended run is %+v", got.Run)
		}
		if p := h.parse("prs_a"); p.Described != 2 || p.State != "succeeded" || p.Calls != 2 {
			t.Fatalf("the parse holds %+v", p)
		}
		if rows := h.states("prs_a"); rows != "" {
			t.Fatalf("the task rows left are %s", rows)
		}
		if got := h.metered("figure"); got != "stub:0/2/600/60" {
			t.Fatalf("the meter of figures reads %s", got)
		}

		// A later parse of the same owner over the same figure is handed
		// the description that was kept, and counts it as taken. A run that
		// describes again is handed none.
		h.readThrough(w, postgres.Submission{Parse: "prs_b", Group: "acme", Owner: "alice"}, 1)
		h.figures("prs_b", "", false, "1.2")
		c := w.claim(1, 1)[0]
		if c.Context.Figure.Reuse != described(claims[0]).Output {
			t.Fatalf("the same figure of a later parse was handed %+v", c.Context.Figure)
		}
		taken := described(c)
		taken.Usage, taken.Result = tasks.Usage{}, []byte(`{"reused":true}`)
		w.settle(taken)
		if run := h.run("prs_b").Run; run.Done != 1 || run.Reused != 1 || run.Calls != 0 {
			t.Fatalf("a run that took a description is %+v", run)
		}
		h.figures("prs_b", stub, true, "1.2")
		c = w.claim(1, 1)[0]
		if c.Context.Figure.Reuse != "" || c.Pin != stub || h.task("prs_b", "figure-1.2").Lane != "figure:stub" {
			t.Fatalf("a run that describes again was handed %+v as %+v", c.Context.Figure, c)
		}
		// The task of a figure is written again by each run, and what it
		// writes is under its token: no token of a run is one of the run
		// before, so no key is.
		if taken.Token != 1<<32+1 || c.Token != 2<<32+1 {
			t.Fatalf("the figure was claimed under the token %d by the first run and %d by the second", taken.Token, c.Token)
		}
		// A figure that could not be described again keeps what it had.
		w.settle(ended(c, tasks.Permanent, "figure_unreadable"))
		again := h.run("prs_b")
		if again.Run.State != postgres.RunFailed || !again.Run.Redo || again.Figures[0].Output != taken.Output || again.Figures[0].State != postgres.FigureFailed {
			t.Fatalf("a run that described nothing is %+v", again)
		}
		if p := h.parse("prs_b"); p.Described != 1 {
			t.Fatalf("the parse counts %d described figures", p.Described)
		}

		// A later run says what it lost itself, and not what a run before
		// it lost: the figure the first run of the first parse could not
		// describe has no row once a run that does not take it began, and
		// the one that kept its description is described.
		h.figures("prs_a", "", false, "1.9")
		w.settle(described(w.claim(1, 1)[0]))
		h.figures("prs_b", "", false)
		if later := h.run("prs_a"); len(later.Figures) != 3 || later.Figures[2].Ref != "2.4" || later.Run.Failed != 0 {
			t.Fatalf("after a later run the figures of the first parse are %+v", later)
		}
		if kept := h.run("prs_b").Figures; len(kept) != 1 || kept[0].State != postgres.FigureSucceeded || kept[0].Error != nil || kept[0].Output != taken.Output {
			t.Fatalf("after a later run the figure that kept its description is %+v", kept)
		}
	})
}

// TestWhatAFigureRunIsRefusedFor: a parse that is not there, one that has
// not ended, and a request with no parse or no deadline. A run of no figure
// has ended when it is written, and a later run replaces it.
func TestWhatAFigureRunIsRefusedFor(t *testing.T) {
	logic(t, withDescribers(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.submit(postgres.Submission{Parse: "prs_running", Group: "acme"})
		refused(t, "a parse that is not there", h.store.StartFigures(ctx, postgres.FigureStart{Parse: "prs_gone", Deadline: time.Hour}), fault.ParseNotFound)
		refused(t, "a parse that has not ended", h.store.StartFigures(ctx, postgres.FigureStart{Parse: "prs_running", Deadline: time.Hour}), fault.NotTerminal)
		refused(t, "no parse", h.store.StartFigures(ctx, postgres.FigureStart{Deadline: time.Hour}), fault.InvalidRequest)
		refused(t, "no deadline", h.store.StartFigures(ctx, postgres.FigureStart{Parse: "prs_running"}), fault.InvalidRequest)

		h.through(w, "prs_running", 1)
		h.figures("prs_running", "", false)
		empty := h.run("prs_running").Run
		if empty.State != postgres.RunSucceeded || empty.Total != 0 || empty.FinishedAt == nil || !empty.FinishedAt.Equal(h.now) {
			t.Fatalf("a run of no figure is %+v", empty)
		}
		h.advance(time.Minute)
		h.figures("prs_running", "", false, "1.1")
		if run := h.run("prs_running").Run; run.State != postgres.RunRunning || run.Total != 1 || !run.StartedAt.Equal(h.now) || run.FinishedAt != nil {
			t.Fatalf("the run that replaced it is %+v", run)
		}
		w.settle(described(w.claim(1, 1)[0]))

		// A run describes at most figures.MaxRun figures: 1 more is refused
		// with nothing written, and as many is 1 row and 1 task each.
		h.readThrough(w, postgres.Submission{Parse: "prs_many", Group: "acme"}, 1)
		refs := make([]string, figures.MaxRun+1)
		for i := range refs {
			refs[i] = "1." + strconv.Itoa(i+1)
		}
		many := postgres.FigureStart{Parse: "prs_many", Deadline: time.Hour}
		for _, ref := range refs {
			many.Figures = append(many.Figures, postgres.FigureAsk{Ref: ref, Page: 1, PageKey: "parses/prs_many/pages/1.1.json"})
		}
		refused(t, "a run of 1 figure more than a run describes", h.store.StartFigures(ctx, many), fault.TooManyPages)
		if _, started, err := h.store.Figures(ctx, "prs_many"); started || err != nil {
			t.Fatalf("a run that was refused was started: %t, %v", started, err)
		}
		h.figures("prs_many", "", false, refs[:figures.MaxRun]...)
		if got := h.run("prs_many"); got.Run.Total != figures.MaxRun || got.Run.Open != figures.MaxRun || len(got.Figures) != figures.MaxRun || len(h.tasks("prs_many")) != figures.MaxRun {
			t.Fatalf("a run of %d figures holds %d figures and %d tasks: %+v", figures.MaxRun, len(got.Figures), len(h.tasks("prs_many")), got.Run)
		}
		h.consistent()
	})
}

// TestARetryWaitsForTheWorkOnItsParse: a retry writes the pages and the
// index of a parse again, so it is refused while an extraction or a figure
// of the parse is queued or running, and is accepted once that has ended.
// An extraction asked after the retry waits for the parse to end again.
func TestARetryWaitsForTheWorkOnItsParse(t *testing.T) {
	logic(t, withDescribers(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		h.failing(w, postgres.Submission{Parse: "prs_a", Group: "acme", Owner: "alice", AllowFailedPages: 1}, 2, "page_unreadable", 2)
		h.field("prs_a", "invoice", "")
		refused(t, "a retry while an extraction waits", h.store.Retry(ctx, "alice", "prs_a"), fault.Conflict)
		c := w.claim(1, 1)[0]
		refused(t, "a retry while an extraction runs", h.store.Retry(ctx, "alice", "prs_a"), fault.Conflict)
		w.settle(filledBy(c))
		h.figures("prs_a", "", false, "1.2")
		refused(t, "a retry while a figure waits", h.store.Retry(ctx, "alice", "prs_a"), fault.Conflict)
		w.settle(described(w.claim(1, 1)[0]))

		if err := h.store.Retry(ctx, "alice", "prs_a"); err != nil {
			t.Fatalf("a retry once the work has ended: %v", err)
		}
		h.field("prs_a", "later", "")
		if n := value[int](h, `SELECT count(*) FROM tasks WHERE parse_id = 'prs_a' AND kind = 'extract'`); n != 0 {
			t.Fatal("an extraction asked of a retried parse has a task before the parse ended")
		}
		w.settle(done(w.claim(1, 1)[0]))
		w.settle(done(w.claim(1, 1)[0]))
		// The parse ended with every page read: its page rows are gone, the
		// extraction's task is there, and what was filled and described
		// before the retry is as it was.
		if rows := h.states("prs_a"); rows != "extract-later:queued/0/0" {
			t.Fatalf("after the retried parse ended its rows are %s", rows)
		}
		if f, figs := h.read("prs_a", "invoice"), h.run("prs_a"); f.State != postgres.FieldSucceeded || figs.Figures[0].Output == "" {
			t.Fatalf("the retry changed what was there: %+v, %+v", f, figs)
		}
		w.settle(filledBy(w.claim(1, 1)[0]))
	})
}

// TestADeleteDropsTheWorkOnItsParse: a delete of a parse, and the end of
// its retention, drop its extractions and its figures that are queued or
// running before the parse's objects are listed, and its rows go after.
// From the first step on nothing of the parse is claimed, nothing new is
// asked of it, and a worker that still ran one of its tasks is told it lost
// it and has its settle refused, whether its call returned before the
// objects were listed or after. The counters of queued and running tasks
// stay a count of the rows.
func TestADeleteDropsTheWorkOnItsParse(t *testing.T) {
	logic(t, withDescribers(), func(t *testing.T, h *harness) {
		ctx := context.Background()
		w := h.worker()
		for _, id := range []string{"prs_deleted", "prs_expired"} {
			h.readThrough(w, postgres.Submission{Parse: id, Group: "acme", Owner: "alice", Retention: time.Hour}, 1)
		}
		for _, id := range []string{"prs_deleted", "prs_expired"} {
			h.field(id, "running", "")
		}
		held := w.claim(2, 2)
		for _, id := range []string{"prs_deleted", "prs_expired"} {
			h.field(id, "waiting", "")
			h.figures(id, "", false, "1.2")
		}

		// A parse within its retention is not closed, and one that is not
		// there is not.
		for _, id := range []string{"prs_expired", "prs_none"} {
			if expired, err := h.store.CloseParse(ctx, id); err != nil || expired {
				t.Fatalf("closing %s: %t, %v", id, expired, err)
			}
		}
		if rows := h.states("prs_expired"); !strings.Contains(rows, "extract-running:leased") || !strings.Contains(rows, "figure-1.2:queued") {
			t.Fatalf("a parse within its retention lost work: %s", rows)
		}

		// The first step of each: the work is dropped and the rows stay.
		if err := h.store.DeleteParse(ctx, "alice", "prs_deleted"); err != nil {
			t.Fatalf("deleting a parse with work on it: %v", err)
		}
		h.advance(2 * time.Hour)
		if expired, err := h.store.CloseParse(ctx, "prs_expired"); err != nil || !expired {
			t.Fatalf("closing a parse whose retention has ended: %t, %v", expired, err)
		}
		for _, id := range []string{"prs_deleted", "prs_expired"} {
			if rows := h.states(id); strings.Contains(rows, "extract-") || strings.Contains(rows, "figure-") {
				t.Fatalf("after the first step %s holds the tasks %s", id, rows)
			}
			if p := h.parse(id); p.State != "succeeded" || p.ExpiresAt.After(h.now) {
				t.Fatalf("after the first step %s is %s and expires at %v", id, p.State, p.ExpiresAt)
			}
			// Nothing new is asked of it, and nothing of it is claimed.
			err := h.store.CreateField(ctx, postgres.FieldRequest{Parse: id, Name: "late", Request: asked, Deadline: time.Hour})
			refused(t, "an extraction of "+id, err, fault.ParseNotFound)
			refused(t, "a figure run of "+id, h.store.StartFigures(ctx, postgres.FigureStart{Parse: id, Deadline: time.Hour}), fault.ParseNotFound)
		}
		h.consistent()

		// The objects are listed and removed here. One worker's call
		// returns before that and one after: both settles are refused, each
		// when it is sent, and both tasks are lost.
		reply := w.raw(tasks.Request{}, step(held[0]))
		if len(reply.Refused) != 1 || reply.Refused[0].Parse != held[0].Parse || len(reply.Lost) != 1 || reply.Lost[0].Parse != held[1].Parse || len(reply.Claims) != 0 {
			t.Fatalf("between the steps the worker's settle was refused %v and it lost %v", reply.Refused, reply.Lost)
		}

		for _, id := range []string{"prs_deleted", "prs_expired"} {
			if removed, err := h.store.ExpireParse(ctx, id); err != nil || !removed {
				t.Fatalf("removing the rows of %s: %t, %v", id, removed, err)
			}
		}
		if removed, err := h.store.ExpireParse(ctx, "prs_expired"); err != nil || removed {
			t.Fatalf("expiring it again: %t, %v", removed, err)
		}
		for _, table := range []string{"parses", "tasks", "fields", "figures", "figure_runs"} {
			if n := value[int](h, `SELECT count(*) FROM `+table); n != 0 {
				t.Fatalf("%d rows of %s are left", n, table)
			}
		}
		if reply := w.raw(tasks.Request{}, filledBy(held[1])); len(reply.Refused) != 1 {
			t.Fatalf("after the rows went the worker's settle was refused %v", reply.Refused)
		}
		w.claim(4, 0)
		refused(t, "a delete of a parse that is not there", h.store.DeleteParse(ctx, "alice", "prs_deleted"), fault.ParseNotFound)
		h.submit(postgres.Submission{Parse: "prs_running", Group: "acme", Owner: "alice"})
		refused(t, "a delete of a parse that has not ended", h.store.DeleteParse(ctx, "alice", "prs_running"), fault.NotTerminal)
	})
}

// TestAnExtractionHoldsASlotOnlyWhileItCalls: an extraction over 6 windows
// is 6 claims. With a pool of one call in flight, it holds the slot while
// each claim runs and none between 2 of them, so a page of another group
// that waits for the same reader is read between its calls and neither ever
// runs beside the other. Each call is charged the reader's cost.
func TestAnExtractionHoldsASlotOnlyWhileItCalls(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "only", MaxInFlight: 1, Cost: 3})
	settings.ExtractChain = []string{"only"}
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_asked", Group: "asking"}, 1)
		h.reading(w, postgres.Submission{Parse: "prs_read", Group: "reading"}, 6)
		h.field("prs_asked", "invoice", "")

		inFlight := `SELECT count(*) FROM tasks WHERE state = 'leased' AND calling AND reader = 'only'`
		calls := map[tasks.Kind]int{}
		for len(h.tasks("prs_read"))+len(h.tasks("prs_asked")) > 0 {
			claims := w.exchange(4).Claims
			if len(claims) == 0 {
				t.Fatalf("nothing was claimed while %s and %s wait", h.states("prs_read"), h.states("prs_asked"))
			}
			// The assemble of the parse that is read calls no model, and may
			// be claimed beside the one task that does.
			c := claims[0]
			for _, other := range claims[1:] {
				if c.Reader != "" && other.Reader != "" {
					t.Fatalf("2 calls were admitted at once to a pool of 1: %s", names(claims))
				}
				if other.Reader != "" {
					c, other = other, c
				}
				w.settle(done(other))
			}
			if n := value[int](h, inFlight); n != 1 && c.Reader != "" || n != 0 && c.Reader == "" {
				t.Fatalf("%d calls are in flight after %s was claimed", n, names(claims))
			}
			calls[c.Kind]++
			switch {
			case c.Kind == tasks.Extract && calls[tasks.Extract] < 6:
				if charged := h.task(c.Parse, c.Task).Charged; charged != 3 {
					t.Fatalf("a call of the extraction is charged %d, want the reader's cost", charged)
				}
				s := step(c)
				s.Units = 3
				w.settle(s)
			case c.Kind == tasks.Extract:
				w.settle(filledBy(c))
			default:
				w.settle(done(c))
			}
			if n := value[int](h, inFlight); n != 0 {
				t.Fatalf("%d slots are held between 2 calls", n)
			}
		}
		// 6 calls of the extraction, 6 pages and the assemble after them.
		if calls[tasks.Extract] != 6 || calls[tasks.Page] != 6 {
			t.Fatalf("the calls were %v", calls)
		}
		if f := h.read("prs_asked", "invoice"); f.State != postgres.FieldSucceeded || f.Calls != 6 {
			t.Fatalf("the extraction is %+v", f)
		}
	})
}

// TestExtractionsOfOneGroupDoNotDelayAnothersPages: a group that queues 40
// extractions and a group that queues 40 pages, of equal weight and with
// readers of equal cost, are served in turn while both wait: of every 10
// claims, 5 are the pages'. An extraction is charged as a page is, so
// queuing extractions buys a group no more than its share.
func TestExtractionsOfOneGroupDoNotDelayAnothersPages(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "pages", MaxInFlight: 100}, tasks.Pool{Reader: "text", MaxInFlight: 100})
	settings.ReadChain, settings.ExtractChain = []string{"pages"}, []string{"text"}
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_asked", Group: "asking"}, 1)
		for i := range 40 {
			h.field("prs_asked", "f"+strconv.Itoa(i), "")
		}
		h.reading(w, postgres.Submission{Parse: "prs_read", Group: "reading"}, 40)

		for round := range 6 {
			claims := w.claim(10, 10)
			pages := 0
			settles := make([]tasks.Settle, len(claims))
			for i, c := range claims {
				if settles[i] = filledBy(c); c.Kind == tasks.Page {
					pages++
					settles[i] = done(c)
				}
			}
			if pages != 5 {
				t.Fatalf("round %d claimed %d pages of 10 tasks: %s", round, pages, names(claims))
			}
			w.settle(settles...)
		}
		h.exec(`SELECT lectio_drop('prs_asked', 'extract', NULL)`)
	})
}

// TestExtractionsNeverExceedTheirReadersPool: with a pool of 3 calls in
// flight for the extractor, a worker with 10 free slots is handed 3
// extractions and no more, however many wait, and the pages of another
// group take the slots the extractions cannot.
func TestExtractionsNeverExceedTheirReadersPool(t *testing.T) {
	settings := readers(tasks.Pool{Reader: "pages", MaxInFlight: 100}, tasks.Pool{Reader: "text", MaxInFlight: 3})
	settings.ReadChain, settings.ExtractChain = []string{"pages"}, []string{"text"}
	logic(t, settings, func(t *testing.T, h *harness) {
		w := h.worker()
		h.readThrough(w, postgres.Submission{Parse: "prs_asked", Group: "asking"}, 1)
		for i := range 40 {
			h.field("prs_asked", "f"+strconv.Itoa(i), "")
		}
		h.reading(w, postgres.Submission{Parse: "prs_read", Group: "reading"}, 40)

		inFlight := `SELECT count(*) FROM tasks WHERE state = 'leased' AND calling AND reader = 'text'`
		var held []tasks.Claim
		for range 4 {
			settles := make([]tasks.Settle, len(held))
			for i, c := range held {
				if settles[i] = filledBy(c); c.Kind == tasks.Page {
					settles[i] = done(c)
				}
			}
			held = w.exchange(10, settles...).Claims
			if got := by(held)["text"]; len(held) != 10 || got != 3 || value[int](h, inFlight) != 3 {
				t.Fatalf("an exchange claimed %d extractions of %d tasks with %d in flight: %s", got, len(held), value[int](h, inFlight), names(held))
			}
		}
		for _, c := range held {
			if c.Kind == tasks.Page {
				w.settle(done(c))
			}
		}
		h.exec(`SELECT lectio_drop('prs_asked', 'extract', NULL)`)
		h.exec(`UPDATE fields SET state = 'failed'`)
	})
}
