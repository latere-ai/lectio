// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/durable"
	"latere.ai/x/lectio/internal/extract"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/objects"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
	"latere.ai/x/lectio/internal/testservers"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// TestWorkOnAnEndedParseSaysWhatItsStoresDoNotAnswer: an extraction and a
// figure run are read from the task store and the object store, and what
// either fails is an error the handlers answer as internal, never a field
// or a description made of nothing. The tasks are settled by the case
// itself, with no worker, so each row is in the state the case needs.
func TestWorkOnAnEndedParseSaysWhatItsStoresDoNotAnswer(t *testing.T) {
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable backend did not run: %v", err)
	}
	ctx := context.Background()
	dsn := testservers.Database(t, srv)
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	settings := tasks.Settings{
		Pools:     []tasks.Pool{{Reader: "stub", MaxInFlight: 8}},
		ReadChain: []string{"stub"}, ExtractChain: []string{"stub"}, DescribeChain: []string{"stub"},
	}
	st, err := postgres.Open(ctx, dsn, postgres.Options{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	objs := &failing{Store: blob.NewMemory()}
	b := &durable.Backend{
		Store: st, Objects: objs, Poll: time.Millisecond,
		Readers:    map[string]reader.Reader{"stub": &stub.Reader{}},
		Describers: map[string]reader.Describer{"stub": &stub.Describer{}}, DescribeChain: []string{"gone", "stub"},
		Extractors: map[string]reader.Extractor{"stub": stub.Extractor{}}, ExtractChain: []string{"stub"},
	}
	failed := func(what string, err error) {
		t.Helper()
		if err == nil || fault.CodeOf(err) != fault.Internal {
			t.Errorf("%s: %v", what, err)
		}
	}
	worker, err := st.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// held is what the worker says it still runs.
	var held []tasks.Held
	exchange := func(free int, settles ...tasks.Settle) []tasks.Claim {
		t.Helper()
		reply, err := st.Exchange(ctx, worker, tasks.Request{Settles: settles, Held: held, Free: free, Idle: true, Kinds: tasks.Kinds})
		if err != nil || len(reply.Refused) != 0 {
			t.Fatalf("the exchange: %+v, %v", reply, err)
		}
		return reply.Claims
	}
	done := func(c tasks.Claim) tasks.Settle {
		return tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done}
	}

	// A parse of one page that holds 2 figures, read from an image.
	p := store.Parse{ID: "prs_a", Owner: "alice", CreatedAt: time.Now()}
	if _, _, err := b.Submit(ctx, p, store.Admission{}, "", ""); err != nil {
		t.Fatal(err)
	}
	prepared := done(exchange(1)[0])
	prepared.Prepare = &tasks.Prepared{
		Manifest: json.RawMessage(`{"media_type":"image/png","pages_total":1,"selected":[1],"source":"reader","work":"sources/x","token":1}`), Pages: []int{1},
	}
	read := done(exchange(1, prepared)[0])
	read.Output = blob.PageKey("prs_a", 1, 1)
	box := &document.Box{0.1, 0.1, 0.5, 0.5}
	page := objects.Page{Revision: 1, Image: "parses/prs_a/pages/1.1.png", Page: document.Page{
		Number: 1, State: document.PageSucceeded, Blocks: document.Number(1, []document.Block{
			{Kind: document.KindFigure, Order: 1, Box: box}, {Kind: document.KindFigure, Order: 2, Box: box},
		}),
	}}
	if err := objects.PutPage(ctx, objs, read.Output, page); err != nil {
		t.Fatal(err)
	}
	assembled := done(exchange(1, read)[0])
	assembled.Output = blob.IndexKey("prs_a", 1)
	assembled.Assemble = &tasks.Assembled{Index: assembled.Output}
	if err := objects.PutIndex(ctx, objs, assembled.Output, objects.Index{Keys: []objects.Entry{{Number: 1, Key: read.Output}}}); err != nil {
		t.Fatal(err)
	}
	exchange(0, assembled)
	current := func() store.Parse {
		t.Helper()
		got, err := b.Parse(ctx, "prs_a")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// A run: the parse's file is gone, so nothing names its descriptions,
	// and the describer the chain names that nobody configured is passed
	// over. One figure is described, and the other's description is an
	// object that is none.
	if err := b.Figures(ctx, current(), run.FigureOptions{}); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	claims := exchange(2)
	if len(claims) != 2 || claims[0].Context.Figure.Page != read.Output {
		t.Fatalf("the run's figures were claimed as %+v", claims)
	}
	first, second := done(claims[0]), done(claims[1])
	first.Output, second.Output = blob.FigureKey("prs_a", "1.1", 1), blob.FigureKey("prs_a", "1.2", 1)
	if err := objects.Put(ctx, objs, first.Output, figures.Description{Description: "A chart."}); err != nil {
		t.Fatal(err)
	}
	held = []tasks.Held{{Parse: claims[1].Parse, Task: claims[1].Task, Token: claims[1].Token}}
	exchange(0, first)
	held = nil
	if got, ok, err := b.Page(ctx, current(), 1); err != nil || !ok || got.Blocks[0].Description != "A chart." || got.Blocks[1].Description != "" {
		t.Fatalf("the page with one figure described: %+v, %t, %v", got, ok, err)
	}
	if err := objs.Put(ctx, second.Output, []byte("["), "application/json"); err != nil {
		t.Fatal(err)
	}
	exchange(0, second)
	_, _, err = b.Page(ctx, current(), 1)
	failed("a page whose figure's description is no description", err)
	_, err = b.Pages(ctx, current())
	failed("the pages of such a parse", err)
	// A request for a run reads no description, so one that is none does
	// not fail it: with both figures described it finds nothing to do.
	if err := b.Figures(ctx, current(), run.FigureOptions{}); err != nil {
		t.Fatalf("a run over such a parse: %v", err)
	}
	b.WaitFigures(ctx, "prs_a", time.Minute)
	if got, started, err := b.FigureRun(ctx, "prs_a"); err != nil || !started || got.State != store.RunSucceeded || got.Total != 0 {
		t.Fatalf("a run with nothing to describe: %+v, %t, %v", got, started, err)
	}
	if got := current(); got.Described != 2 {
		t.Fatalf("the parse counts %d described figures", got.Described)
	}

	// An extraction that was filled and whose object is gone, and one
	// whose row holds no summary.
	if _, err := b.CreateField(ctx, current(), store.FieldRequest{Name: "invoice", Schema: []byte(`{"type":"object"}`)}); err != nil {
		t.Fatalf("asking an extraction: %v", err)
	}
	if _, err := b.CreateField(ctx, current(), store.FieldRequest{Name: "odd", Schema: []byte(`{"type":"object"}`), Extractor: "stub"}); err != nil {
		t.Fatalf("asking an extraction of a named extractor: %v", err)
	}
	if _, err := b.CreateField(ctx, current(), store.FieldRequest{Name: "raw", Schema: []byte(`{`)}); fault.CodeOf(err) != fault.InvalidSchema {
		t.Fatalf("a schema that is no JSON: %v", err)
	}
	extracting := exchange(2)
	filled, odd := done(extracting[0]), done(extracting[1])
	filled.Output, filled.Result = blob.FieldKey("prs_a", "invoice", 1), json.RawMessage(`{"model":"m","attempts":1,"windows":1}`)
	odd.Output, odd.Result = blob.FieldKey("prs_a", "odd", 1), json.RawMessage(`"no summary"`)
	exchange(0, filled, odd)
	_, _, err = b.Field(ctx, current(), "invoice")
	failed("an extraction whose object is gone", err)
	_, _, err = b.Field(ctx, current(), "odd")
	failed("an extraction whose row holds no summary", err)
	_, err = b.Fields(ctx, current())
	failed("the extractions of such a parse", err)
	if err := objects.Put(ctx, objs, filled.Output, extract.Result{Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := b.Field(ctx, current(), "invoice"); err != nil || !ok || got.State != store.FieldSucceeded || string(got.Data) != `{}` || got.Model != "m" {
		t.Fatalf("the extraction once its object is there: %+v, %t, %v", got, ok, err)
	}
	if _, ok, err := b.Field(ctx, current(), "other"); ok || err != nil {
		t.Fatalf("an extraction nobody asked: %t, %v", ok, err)
	}

	// The object store stops answering.
	if err := objects.Put(ctx, objs, second.Output, figures.Description{Description: "A photo."}); err != nil {
		t.Fatal(err)
	}
	objs.down = true
	failed("a run with the object store down", b.Figures(ctx, current(), run.FigureOptions{}))
	_, _, err = b.Field(ctx, current(), "invoice")
	failed("an extraction with the object store down", err)
	objs.down = false

	// The task store stops answering.
	ended := current()
	st.Close()
	failed("a run with the task store down", b.Figures(ctx, ended, run.FigureOptions{Redo: true}))
	_, _, err = b.FigureRun(ctx, "prs_a")
	failed("reading a run with the task store down", err)
	b.WaitFigures(ctx, "prs_a", time.Minute)
	_, err = b.CreateField(ctx, ended, store.FieldRequest{Name: "late", Schema: []byte(`{"type":"object"}`)})
	failed("asking an extraction with the task store down", err)
	_, _, err = b.Field(ctx, ended, "invoice")
	failed("reading an extraction with the task store down", err)
	_, err = b.Fields(ctx, ended)
	failed("listing the extractions with the task store down", err)
	_, err = b.Pages(ctx, ended)
	failed("the pages of a parse with described figures, with the task store down", err)
	_, _, err = b.Document(ctx, ended)
	failed("the document of a parse with extractions, with the task store down", err)
	unindexed := ended
	unindexed.IndexKey, unindexed.Manifest = "", nil
	_, _, err = b.Document(ctx, unindexed)
	failed("a document made from rows, with the task store down", err)
}

// reads is an object store that counts what is read from it, by what the
// key holds.
type reads struct {
	blob.Store
	pages, descriptions atomic.Int64
}

func (r *reads) Get(ctx context.Context, key string) ([]byte, string, error) {
	switch {
	case strings.Contains(key, "/figures/"):
		r.descriptions.Add(1)
	case strings.Contains(key, "/pages/"):
		r.pages.Add(1)
	}
	return r.Store.Get(ctx, key)
}

// TestAFigureRunTakesAsManyFiguresAsARunDescribes: a run over pages that
// hold more figures than one run describes is refused with too_many_pages
// and starts nothing. The request stops reading pages once it has passed
// the bound, and reads no description to find which figures have one. A
// figure an earlier run described is not counted again, so a parse with
// more figures than one run takes is described a range of pages at a time.
func TestAFigureRunTakesAsManyFiguresAsARunDescribes(t *testing.T) {
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the durable backend did not run: %v", err)
	}
	ctx := context.Background()
	dsn := testservers.Database(t, srv)
	if err := postgres.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	st, err := postgres.Open(ctx, dsn, postgres.Options{Settings: tasks.Settings{
		Pools: []tasks.Pool{{Reader: "stub", MaxInFlight: 64}}, ReadChain: []string{"stub"}, DescribeChain: []string{"stub"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	objs := &reads{Store: blob.NewMemory()}
	b := &durable.Backend{
		Store: st, Objects: objs, Poll: time.Millisecond,
		Readers:    map[string]reader.Reader{"stub": &stub.Reader{}},
		Describers: map[string]reader.Describer{"stub": &stub.Describer{}}, DescribeChain: []string{"stub"},
	}
	worker, err := st.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(free int, settles ...tasks.Settle) []tasks.Claim {
		t.Helper()
		reply, err := st.Exchange(ctx, worker, tasks.Request{Settles: settles, Free: free, Idle: true, Kinds: tasks.Kinds})
		if err != nil || len(reply.Refused) != 0 {
			t.Fatalf("the exchange: %+v, %v", reply, err)
		}
		return reply.Claims
	}
	done := func(c tasks.Claim) tasks.Settle {
		return tasks.Settle{Parse: c.Parse, Task: c.Task, Token: c.Token, Outcome: tasks.Done}
	}

	// A parse of 40 pages read from images: 600 figures on each of the
	// first 2, and 1 on each of the others.
	const total = 40
	if _, _, err := b.Submit(ctx, store.Parse{ID: "prs_a", Owner: "alice", CreatedAt: time.Now()}, store.Admission{}, "", ""); err != nil {
		t.Fatal(err)
	}
	numbers := make([]int, total)
	for i := range numbers {
		numbers[i] = i + 1
	}
	selected, err := json.Marshal(numbers)
	if err != nil {
		t.Fatal(err)
	}
	prepared := done(exchange(1)[0])
	prepared.Prepare = &tasks.Prepared{
		Manifest: json.RawMessage(`{"media_type":"image/png","pages_total":40,"selected":` + string(selected) + `,"source":"reader","work":"sources/x","token":1}`),
		Pages:    numbers,
	}
	box := &document.Box{0.1, 0.1, 0.5, 0.5}
	var settles []tasks.Settle
	index := objects.Index{}
	for _, c := range exchange(total, prepared) {
		n, ok := tasks.PageOf(c.Task)
		if !ok {
			t.Fatalf("claimed %s where the pages were queued", c.Task)
		}
		blocks := make([]document.Block, 1)
		if n <= 2 {
			blocks = make([]document.Block, 600)
		}
		for i := range blocks {
			blocks[i] = document.Block{Kind: document.KindFigure, Order: i + 1, Box: box}
		}
		read := done(c)
		read.Output = blob.PageKey("prs_a", n, 1)
		page := objects.Page{Revision: 1, Image: "parses/prs_a/pages/x.png", Page: document.Page{
			Number: n, State: document.PageSucceeded, Blocks: document.Number(n, blocks),
		}}
		if err := objects.PutPage(ctx, objs, read.Output, page); err != nil {
			t.Fatal(err)
		}
		settles = append(settles, read)
		index.Keys = append(index.Keys, objects.Entry{Number: n, Key: read.Output})
	}
	slices.SortFunc(index.Keys, func(a, b objects.Entry) int { return a.Number - b.Number })
	assembled := done(exchange(1, settles...)[0])
	assembled.Output = blob.IndexKey("prs_a", 1)
	assembled.Assemble = &tasks.Assembled{Index: assembled.Output}
	if err := objects.PutIndex(ctx, objs, assembled.Output, index); err != nil {
		t.Fatal(err)
	}
	exchange(0, assembled)
	current := func() store.Parse {
		t.Helper()
		got, err := b.Parse(ctx, "prs_a")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The pages after the second hold 38 figures: a run over them is
	// taken, and its figures are described.
	if err := b.Figures(ctx, current(), run.FigureOptions{Pages: numbers[2:]}); err != nil {
		t.Fatalf("a run over 38 figures: %v", err)
	}
	settles = nil
	for _, c := range exchange(64) {
		ref, _ := tasks.FigureOf(c.Task)
		s := done(c)
		s.Output = blob.FigureKey("prs_a", ref, c.Token)
		settles = append(settles, s)
	}
	exchange(0, settles...)
	if got, started, err := b.FigureRun(ctx, "prs_a"); err != nil || !started || got.State != store.RunSucceeded || got.Done != 38 {
		t.Fatalf("the run over 38 figures: %+v, %t, %v", got, started, err)
	}

	// A run over every page finds 1,200 figures to describe in the first
	// batch of pages it reads, and is refused there: it has read 16 pages
	// of the 40, and no description.
	objs.pages.Store(0)
	objs.descriptions.Store(0)
	err = b.Figures(ctx, current(), run.FigureOptions{})
	if fault.CodeOf(err) != fault.TooManyPages {
		t.Fatalf("a run over 1,200 figures: %v", err)
	}
	if objs.pages.Load() != 16 || objs.descriptions.Load() != 0 {
		t.Fatalf("the refused run read %d pages and %d descriptions", objs.pages.Load(), objs.descriptions.Load())
	}
	if got, _, err := b.FigureRun(ctx, "prs_a"); err != nil || got.State != store.RunSucceeded || got.Total != 38 {
		t.Fatalf("a run that was refused changed the run to %+v, %v", got, err)
	}

	// A run over the first page and the pages that are described takes the
	// 600 figures that have no description, and counts no other.
	if err := b.Figures(ctx, current(), run.FigureOptions{Pages: append([]int{1}, numbers[2:]...)}); err != nil {
		t.Fatalf("a run over 600 figures and 38 that are described: %v", err)
	}
	if got, _, err := b.FigureRun(ctx, "prs_a"); err != nil || got.State != store.RunRunning || got.Total != 600 {
		t.Fatalf("the run over the first page: %+v, %v", got, err)
	}
}
