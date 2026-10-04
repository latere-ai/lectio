// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package run

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/store"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// painted renders every page as a real image, 200 by 100, so a region of
// it can be cut out.
type painted struct{}

var canvas = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := range 100 {
		for x := range 200 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

func (painted) Render(context.Context, []byte, string, int, reader.Description) (render.Image, error) {
	return render.Image{Data: canvas, MediaType: "image/png", Width: 200, Height: 100}, nil
}

// illustrated reads every page as a paragraph, a figure with a box, and
// the figure's caption. On page 2 the figure already has text.
type illustrated struct{ version string }

func (r illustrated) Describe() reader.Description {
	return reader.Description{Name: "illustrated", Accepts: []string{"image/png"}, Image: reader.ImageSpec{DPI: 72, Format: "png"}, Boxes: true, Version: r.version}
}

func (illustrated) ReadPage(_ context.Context, page reader.Page) (reader.Result, error) {
	fig := reader.Raw{Label: "figure", Box: []float64{250, 200, 750, 800}}
	if page.Number == 2 {
		fig.Text = "printed inside"
	}
	return reader.Result{Model: "layout", Usage: document.Usage{Pages: 1}, Blocks: reader.Normalize([]reader.Raw{
		{Label: "text", Text: "A paragraph.", Box: []float64{100, 50, 900, 150}},
		fig,
		{Label: "caption", Text: "Figure " + string(rune('0'+page.Number)) + ": what it is.", Box: []float64{250, 820, 750, 880}},
	}, reader.Grid{Width: 1000, Height: 1000})}, nil
}

// illustratedRunner is a running runner whose pages hold a figure each.
func illustratedRunner(t *testing.T, describers map[string]reader.Describer, chain ...string) *Runner {
	t.Helper()
	return start(t, &Runner{
		Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: painted{}},
		Readers:  map[string]reader.Reader{"layout": illustrated{version: "v1"}}, Chain: []string{"layout"},
		Describers: describers, DescribeChain: chain,
		Credential: func(owner string) reader.Credential { return reader.NewCredential("key of " + owner) },
	})
}

// described starts a run over a parse's figures and returns it as it ended.
func described(t *testing.T, r *Runner, id string, opt FigureOptions) store.FigureRun {
	t.Helper()
	p, err := r.Store.Parse("alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Figures(p, opt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.FiguresDone(id):
	case <-time.After(10 * time.Second):
		t.Fatalf("the figures of %s were not described", id)
	}
	run, ok := r.Store.FigureRun(id)
	if !ok {
		t.Fatal("no run")
	}
	return run
}

func figureOf(t *testing.T, r *Runner, id string, n int) document.Block {
	t.Helper()
	page, ok := r.Store.Page(id, n)
	if !ok {
		t.Fatalf("no page %d", n)
	}
	return page.Blocks[1]
}

func TestAParsesFiguresAreDescribed(t *testing.T) {
	var mu sync.Mutex
	var seen []reader.FigureRequest
	d := &stub.Describer{Fail: func(req reader.FigureRequest) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, req)
		return nil
	}}
	r := illustratedRunner(t, map[string]reader.Describer{"vision": d}, "vision")
	p := run(t, r, store.Parse{ID: "prs_1", ContentSHA: "sha", Languages: []string{"de"}}, testfixtures.MultiTIFF)
	if p.State != store.StateSucceeded || figureOf(t, r, "prs_1", 1).Description != "" {
		t.Fatalf("a parse describes no figure by itself: %+v", p)
	}

	got := described(t, r, "prs_1", FigureOptions{})
	if got.State != store.RunSucceeded || got.Total != 3 || got.Done != 3 || got.Failed != 0 || got.Reused != 0 || got.FinishedAt == nil || got.Usage.OutputTokens != 3 {
		t.Fatalf("run = %+v", got)
	}
	if d.Calls() != 3 {
		t.Fatalf("one call per figure: %d", d.Calls())
	}

	// The describer was given the figure alone, cut from its page: the box
	// covers half the page's width and six tenths of its height.
	for _, req := range seen {
		if req.Width != 100 || req.Height != 60 || req.MediaType != "image/png" || !strings.HasPrefix(req.Caption, "Figure ") ||
			req.Languages[0] != "de" || req.Credential.Reveal() != "key of alice" {
			t.Fatalf("the describer was given %+v", req)
		}
	}

	// What came back is on the figure's block: what it shows, what kind of
	// figure it is and who said so. The labels printed in it are its text
	// when the page's reader gave it none, and a text the reader gave is kept.
	one, two := figureOf(t, r, "prs_1", 1), figureOf(t, r, "prs_1", 2)
	if !strings.Contains(one.Description, "A figure of 100 by 60 pixels") || !strings.HasSuffix(one.Description, "Its caption reads: Figure 1: what it is.") {
		t.Fatalf("description = %q", one.Description)
	}
	if one.Figure == nil || one.Figure.Type != reader.FigureOther || one.Figure.Model != stub.Name || !strings.HasSuffix(one.Text, " bytes") {
		t.Fatalf("figure = %+v, text %q", one.Figure, one.Text)
	}
	if two.Text != "printed inside" || two.Description == "" {
		t.Fatalf("a figure the reader gave text keeps it: %+v", two)
	}
	page, _ := r.Store.Page("prs_1", 1)
	if err := page.Validate(); err != nil {
		t.Fatalf("the page still fits the object model: %v", err)
	}
	// What the run consumed is counted on the parse.
	if after, _ := r.Store.Parse("alice", "prs_1"); after.Usage.OutputTokens != p.Usage.OutputTokens+3 {
		t.Fatalf("usage: %+v after %+v", after.Usage, p.Usage)
	}

	// A second run finds nothing left to describe. Asked to describe again,
	// it calls the describer for every figure.
	if again := described(t, r, "prs_1", FigureOptions{}); again.Total != 0 || again.State != store.RunSucceeded || d.Calls() != 3 {
		t.Fatalf("a second run: %+v, %d calls", again, d.Calls())
	}
	if redo := described(t, r, "prs_1", FigureOptions{Redo: true, Pages: []int{2}}); redo.Total != 1 || redo.Done != 1 || redo.Reused != 0 || d.Calls() != 4 {
		t.Fatalf("described again: %+v, %d calls", redo, d.Calls())
	}

	// Another parse of the same bytes takes its figures from the first:
	// the same figure is not described twice.
	run(t, r, store.Parse{ID: "prs_2", ContentSHA: "sha", Languages: []string{"de"}, Reuse: true}, testfixtures.MultiTIFF)
	if taken := described(t, r, "prs_2", FigureOptions{}); taken.Total != 3 || taken.Done != 3 || taken.Reused != 3 || d.Calls() != 4 {
		t.Fatalf("figures of the same bytes: %+v, %d calls", taken, d.Calls())
	}
	if got := figureOf(t, r, "prs_2", 3); got.Description == "" || got.Figure == nil {
		t.Fatalf("a figure taken from an earlier description: %+v", got)
	}
	// A parse with no digest of its bytes, and other language hints, are
	// other work.
	run(t, r, store.Parse{ID: "prs_3"}, testfixtures.MultiTIFF)
	if fresh := described(t, r, "prs_3", FigureOptions{Pages: []int{1}}); fresh.Total != 1 || fresh.Reused != 0 || d.Calls() != 5 {
		t.Fatalf("a parse with no digest: %+v, %d calls", fresh, d.Calls())
	}
}

// crowded reads the first page as 1,001 figures and every other page as 1.
type crowded struct{ illustrated }

func (crowded) ReadPage(_ context.Context, page reader.Page) (reader.Result, error) {
	n := 1
	if page.Number == 1 {
		n = figures.MaxRun + 1
	}
	raw := make([]reader.Raw, n)
	for i := range raw {
		raw[i] = reader.Raw{Label: "figure", Box: []float64{250, 200, 750, 800}}
	}
	return reader.Result{Model: "layout", Usage: document.Usage{Pages: 1}, Blocks: reader.Normalize(raw, reader.Grid{Width: 1000, Height: 1000})}, nil
}

// TestARunOfMoreFiguresThanARunDescribesIsRefused: a run over pages that
// hold more figures than one run describes is refused before anything is
// queued, and a run over fewer of the parse's pages is taken.
func TestARunOfMoreFiguresThanARunDescribesIsRefused(t *testing.T) {
	r := start(t, &Runner{
		Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: painted{}},
		Readers:  map[string]reader.Reader{"layout": crowded{}}, Chain: []string{"layout"},
		Describers: map[string]reader.Describer{"vision": &stub.Describer{}}, DescribeChain: []string{"vision"},
	})
	p := run(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
	if _, err := r.Figures(p, FigureOptions{}); fault.CodeOf(err) != fault.TooManyPages {
		t.Fatalf("a run over %d figures: %v", figures.MaxRun+3, err)
	}
	if _, started := r.Store.FigureRun("prs_1"); started {
		t.Fatal("a run that was refused was started")
	}
	if got := described(t, r, "prs_1", FigureOptions{Pages: []int{2, 3}}); got.Total != 2 || got.Done != 2 {
		t.Fatalf("a run over the pages after the first: %+v", got)
	}
}

func TestAFigureRunIsRefusedWhenItCannotStart(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := &stub.Describer{Fail: func(reader.FigureRequest) error {
		once.Do(func() { close(reached) })
		<-release
		return nil
	}}
	r := illustratedRunner(t, map[string]reader.Describer{"vision": slow}, "vision", "gone")
	p := run(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)

	if _, err := r.Figures(store.Parse{ID: "prs_x", State: store.StateRunning}, FigureOptions{}); fault.CodeOf(err) != fault.NotTerminal {
		t.Fatalf("a parse that has not ended: %v", err)
	}
	if _, err := r.Figures(p, FigureOptions{Describer: "other"}); fault.CodeOf(err) != fault.ReaderNotFound {
		t.Fatalf("a describer that is not configured: %v", err)
	}
	none := illustratedRunner(t, nil)
	q := run(t, none, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
	if _, err := none.Figures(q, FigureOptions{}); fault.CodeOf(err) != fault.ReaderNotFound {
		t.Fatalf("no describer at all: %v", err)
	}

	// While a run is in flight a second one is refused.
	started, err := r.Figures(p, FigureOptions{})
	if err != nil || started.State != store.RunRunning || started.Total != 3 {
		t.Fatalf("started: %+v, %v", started, err)
	}
	<-reached
	if _, err := r.Figures(p, FigureOptions{}); fault.CodeOf(err) != fault.Conflict {
		t.Fatalf("a second run while one is in flight: %v", err)
	}
	close(release)
	<-r.FiguresDone("prs_1")
	if got, _ := r.Store.FigureRun("prs_1"); got.State != store.RunSucceeded || got.Done != 3 {
		t.Fatalf("run = %+v", got)
	}
	// With no run in flight the channel is closed already.
	<-r.FiguresDone("prs_none")
}

func TestAFigureThatCannotBeDescribedIsCounted(t *testing.T) {
	second := func(err error) *stub.Describer {
		// Fails the figure of page 2, by its caption.
		return &stub.Describer{Fail: func(req reader.FigureRequest) error {
			if strings.HasPrefix(req.Caption, "Figure 2") {
				return err
			}
			return nil
		}}
	}
	for name, tc := range map[string]struct {
		chain  []*stub.Describer
		failed int
		code   string
		calls  []int
	}{
		"declined, and the next describes it":   {chain: []*stub.Describer{second(reader.Errorf(reader.Refused, "no")), {}}, calls: []int{3, 1}},
		"declined, with no other describer":     {chain: []*stub.Describer{second(reader.Errorf(reader.Refused, "no"))}, failed: 1, code: "figure_unreadable", calls: []int{3}},
		"a describer whose request is rejected": {chain: []*stub.Describer{second(reader.Errorf(reader.Misconfigured, "400"))}, failed: 1, code: "reader_unavailable", calls: []int{3}},
		"a figure the describer cannot take":    {chain: []*stub.Describer{second(reader.Errorf(reader.Permanent, "no"))}, failed: 1, code: "figure_unreadable", calls: []int{3}},
		"a spent budget":                        {chain: []*stub.Describer{second(reader.Errorf(reader.Budget, "spent"))}, failed: 1, code: "budget_exhausted", calls: []int{3}},
		"a failure that passes":                 {chain: []*stub.Describer{second(reader.Errorf(reader.Retryable, "reset"))}, failed: 1, code: "reader_unavailable", calls: []int{5}},
		"replies that are not usable":           {chain: []*stub.Describer{second(reader.Errorf(reader.Invalid, "prose"))}, failed: 1, code: "figure_unreadable", calls: []int{5}},
	} {
		t.Run(name, func(t *testing.T) {
			describers, chain := map[string]reader.Describer{}, []string{}
			for i, d := range tc.chain {
				name := string(rune('a' + i))
				describers[name], chain = d, append(chain, name)
			}
			r := illustratedRunner(t, describers, chain...)
			run(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
			got := described(t, r, "prs_1", FigureOptions{})

			// The run ends and succeeds with what it described: one figure
			// lost does not fail the others.
			if got.State != store.RunSucceeded || got.Done != 3-tc.failed || got.Failed != tc.failed {
				t.Fatalf("run = %+v", got)
			}
			why, lost := got.Failures["2.2"]
			if lost != (tc.failed == 1) || why.Code != tc.code || (lost && why.Detail == "") {
				t.Fatalf("failures = %+v", got.Failures)
			}
			if two := figureOf(t, r, "prs_1", 2); (two.Description == "") != (tc.failed == 1) {
				t.Fatalf("page 2's figure: %+v", two)
			}
			for i, d := range tc.chain {
				if d.Calls() != tc.calls[i] {
					t.Errorf("describer %d was called %d times, want %d", i, d.Calls(), tc.calls[i])
				}
			}
		})
	}

	// A run that described nothing it set out to has failed.
	every := &stub.Describer{Fail: func(reader.FigureRequest) error { return reader.Errorf(reader.Permanent, "no") }}
	r := illustratedRunner(t, map[string]reader.Describer{"vision": every}, "vision")
	run(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
	if got := described(t, r, "prs_1", FigureOptions{}); got.State != store.RunFailed || got.Failed != 3 || len(got.Failures) != 3 {
		t.Fatalf("run = %+v", got)
	}
	// Describing again what failed clears the failure it had.
	every.Fail = nil
	if got := described(t, r, "prs_1", FigureOptions{}); got.State != store.RunSucceeded || got.Done != 3 || len(got.Failures) != 0 {
		t.Fatalf("a later run = %+v", got)
	}
}

func TestOnlyAFigureThatCanBeCutOutIsDescribed(t *testing.T) {
	d := &stub.Describer{}

	// A page read from the file itself has no image to cut from.
	native := illustratedRunner(t, map[string]reader.Describer{"vision": d}, "vision")
	run(t, native, store.Parse{ID: "prs_1"}, testfixtures.CSV)
	if got := described(t, native, "prs_1", FigureOptions{}); got.Total != 0 || got.State != store.RunSucceeded {
		t.Fatalf("a native page: %+v", got)
	}

	// A page whose stored image cannot be decoded: the figure is lost with
	// the reason, and no describer is called.
	broken := start(t, &Runner{
		Readers: map[string]reader.Reader{"layout": illustrated{}}, Chain: []string{"layout"},
		Describers: map[string]reader.Describer{"vision": d}, DescribeChain: []string{"vision"},
	})
	run(t, broken, store.Parse{ID: "prs_1"}, testfixtures.PNG)
	got := described(t, broken, "prs_1", FigureOptions{})
	if got.State != store.RunFailed || got.Failures["1.2"].Code != string(fault.DocumentCorrupt) || d.Calls() != 0 {
		t.Fatalf("a page image that does not decode: %+v, %d calls", got, d.Calls())
	}

	// A describer that names no version promises nothing: its figures are
	// described again by the next parse of the same bytes.
	unversioned := illustratedRunner(t, map[string]reader.Describer{"vision": noDescriberVersion{d}}, "vision")
	run(t, unversioned, store.Parse{ID: "prs_1", ContentSHA: "sha"}, testfixtures.PNG)
	described(t, unversioned, "prs_1", FigureOptions{})
	run(t, unversioned, store.Parse{ID: "prs_2", ContentSHA: "sha"}, testfixtures.PNG)
	if again := described(t, unversioned, "prs_2", FigureOptions{}); again.Reused != 0 || d.Calls() != 2 {
		t.Fatalf("figures of a describer with no version: %+v, %d calls", again, d.Calls())
	}
}

// noDescriberVersion is a describer that describes itself without a version.
type noDescriberVersion struct{ reader.Describer }

func (n noDescriberVersion) Describe() reader.DescriberDescription {
	d := n.Describer.Describe()
	d.Version = ""
	return d
}

func TestStoppingTheRunnerEndsAFigureRun(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d := &stub.Describer{Fail: func(reader.FigureRequest) error {
		once.Do(func() { close(reached) })
		<-release
		return nil
	}}
	r := &Runner{
		Store: store.NewMemory(), Pipeline: &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: painted{}},
		Readers: map[string]reader.Reader{"layout": illustrated{}}, Chain: []string{"layout"},
		Describers: map[string]reader.Describer{"vision": d}, DescribeChain: []string{"vision"}, Workers: 1,
	}
	ctx, stop := context.WithCancel(context.Background())
	r.Start(ctx)
	p := run(t, r, store.Parse{ID: "prs_1"}, testfixtures.MultiTIFF)
	if _, err := r.Figures(p, FigureOptions{}); err != nil {
		t.Fatal(err)
	}
	<-reached
	stop()
	close(release)
	r.Wait()

	// The figure that was being described is dropped and the others were
	// never taken: the run ended having described nothing.
	got, _ := r.Store.FigureRun("prs_1")
	if got.State != store.RunFailed || got.Done != 0 || got.FinishedAt == nil || figureOf(t, r, "prs_1", 1).Description != "" {
		t.Fatalf("run = %+v", got)
	}
	// A run asked of a stopped runner ends at once.
	if _, err := r.Figures(p, FigureOptions{}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if got, _ := r.Store.FigureRun("prs_1"); got.State != store.RunFailed || got.Total != 3 {
		t.Fatalf("a run on a stopped runner = %+v", got)
	}
}
