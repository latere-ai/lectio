// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/run"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// illustrated reads every page as a paragraph, a figure with a box, and
// the figure's caption.
type illustrated struct{}

func (illustrated) Describe() reader.Description {
	return reader.Description{Name: "illustrated", Accepts: []string{"image/png"}, Image: reader.ImageSpec{DPI: 72, Format: "png"}, Boxes: true, Version: "v1"}
}

func (illustrated) ReadPage(context.Context, reader.Page) (reader.Result, error) {
	return reader.Result{Model: "layout", Usage: document.Usage{Pages: 1}, Blocks: reader.Normalize([]reader.Raw{
		{Label: "text", Text: "A paragraph.", Box: []float64{100, 50, 900, 150}},
		{Label: "figure", Box: []float64{250, 200, 750, 800}},
		{Label: "caption", Text: "Figure 1: what it is.", Box: []float64{250, 820, 750, 880}},
	}, reader.Grid{Width: 1000, Height: 1000})}, nil
}

// figured is a server whose pages hold a figure each and whose describer
// is the one given.
func figured(t *testing.T, d reader.Describer) *env {
	t.Helper()
	return serve(t, func(_ *Server, r *run.Runner) {
		r.Readers, r.Chain = map[string]reader.Reader{"layout": illustrated{}}, []string{"layout"}
		if d != nil {
			r.Describers, r.DescribeChain = map[string]reader.Describer{"vision": d}, []string{"vision"}
		}
	})
}

func TestABlocksImageIsItsRegionOfThePage(t *testing.T) {
	e := figured(t, nil)
	pid := e.parsed(e.upload("scan.png", sheet(t)), "")["id"].(string)

	// The figure's box covers the middle half of the page's width and six
	// tenths of its height, and the page is 60 by 40.
	got := e.do("GET", "/parses/"+pid+"/blocks/1.2/image", nil)
	if got.status != http.StatusOK || got.header.Get("Content-Type") != "image/png" {
		t.Fatalf("a block's image: %d %s", got.status, got.body)
	}
	img, err := png.Decode(bytes.NewReader(got.body))
	if err != nil || img.Bounds().Dx() != 30 || img.Bounds().Dy() != 24 {
		t.Fatalf("the image is %v, %v", img.Bounds(), err)
	}

	for path, want := range map[string]struct {
		status int
		code   string
	}{
		"/blocks/1.9/image":   {http.StatusNotFound, "block_not_found"},
		"/blocks/7.1/image":   {http.StatusNotFound, "page_not_found"},
		"/blocks/first/image": {http.StatusBadRequest, "invalid_request"},
	} {
		if got := e.do("GET", "/parses/"+pid+path, nil); got.status != want.status || got.code(t) != want.code {
			t.Errorf("%s: %d %s", path, got.status, got.body)
		}
	}
	if got := e.as("bob-token").do("GET", "/parses/"+pid+"/blocks/1.2/image", nil); got.status != http.StatusNotFound {
		t.Errorf("another caller's block: %d %s", got.status, got.body)
	}

	// A page read from the file itself has no image, and its blocks have
	// no position, so there is nothing to cut.
	native := e.parsed(e.upload("sample.csv", testfixtures.Read(t, testfixtures.CSV)), "")["id"].(string)
	if got := e.do("GET", "/parses/"+native+"/blocks/1.1/image", nil); got.status != http.StatusNotFound || got.code(t) != "block_not_found" {
		t.Fatalf("a native block's image: %d %s", got.status, got.body)
	}
}

func TestFiguresAreDescribedOnRequest(t *testing.T) {
	d := &stub.Describer{}
	e := figured(t, d)
	pid := e.parsed(e.upload("scan.png", sheet(t)), "")["id"].(string)

	// Before anyone asks, the figure is listed with where it is and no
	// description, and there is no run.
	before := e.do("GET", "/parses/"+pid+"/figures", nil)
	figs := before.json(t)["figures"].([]any)
	if before.status != http.StatusOK || before.json(t)["run"] != nil || len(figs) != 1 || at(figs[0], "ref") != "1.2" || at(figs[0], "page") != 1.0 || at(figs[0], "description") != nil {
		t.Fatalf("figures before a run: %d %s", before.status, before.body)
	}

	got := e.do("POST", "/parses/"+pid+"/figures", nil, "Prefer", "wait=20")
	if got.status != http.StatusOK || got.header.Get("Preference-Applied") != "wait=20" {
		t.Fatalf("describe: %d %s", got.status, got.body)
	}
	run := got.json(t)["run"].(map[string]any)
	if run["state"] != "succeeded" || run["total"] != 1.0 || run["done"] != 1.0 || run["failed"] != 0.0 || run["finished_at"] == nil || at(run, "usage", "output_tokens") != 1.0 {
		t.Fatalf("run: %v", run)
	}
	fig := got.json(t)["figures"].([]any)[0]
	// The figure was cut from the page and described alone, with its
	// caption as context.
	if desc, _ := at(fig, "description").(string); !strings.Contains(desc, "A figure of 30 by 24 pixels") || !strings.HasSuffix(desc, "Its caption reads: Figure 1: what it is.") {
		t.Fatalf("description: %v", fig)
	}
	if at(fig, "figure", "type") != "other" || at(fig, "figure", "model") != "stub" || at(fig, "text") == "" || at(fig, "error") != nil {
		t.Fatalf("figure: %v", fig)
	}

	// The description is on the block, wherever the block is read.
	block := e.do("GET", "/parses/"+pid+"/blocks/1.2", nil).json(t)
	if block["description"] != at(fig, "description") || at(block, "figure", "type") != "other" {
		t.Fatalf("the block: %v", block)
	}
	if md := e.do("GET", "/parses/"+pid+"/document?format=markdown", nil); !strings.Contains(string(md.body), "*[Figure: A figure of 30 by 24 pixels") {
		t.Fatalf("the document: %q", md.body)
	}
	// What the run consumed is counted on the parse.
	if p := e.do("GET", "/parses/"+pid, nil).json(t); at(p, "usage", "output_tokens") == nil {
		t.Fatalf("the parse's usage: %v", p["usage"])
	}

	// A second request finds nothing left to describe; asked to describe
	// again, it calls the describer.
	again := e.do("POST", "/parses/"+pid+"/figures", `{}`, "Prefer", "wait=20")
	if again.status != http.StatusOK || at(again.json(t), "run", "total") != 0.0 || d.Calls() != 1 {
		t.Fatalf("a second run: %d %s, %d calls", again.status, again.body, d.Calls())
	}
	redo := e.do("POST", "/parses/"+pid+"/figures", `{"redo":true,"pages":"1","describer":"vision"}`, "Prefer", "wait=20")
	if redo.status != http.StatusOK || at(redo.json(t), "run", "done") != 1.0 || d.Calls() != 2 {
		t.Fatalf("described again: %d %s, %d calls", redo.status, redo.body, d.Calls())
	}

	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"a member the contract does not name": {`{"model":"x"}`, 400, "unknown_field"},
		"a malformed selection":               {`{"pages":"3-1"}`, 400, "invalid_pages"},
		"a selection naming no page":          {`{"pages":"9"}`, 400, "invalid_pages"},
		"a describer that is not configured":  {`{"describer":"other"}`, 400, "reader_not_found"},
		"a body that is not JSON":             {`pages=1`, 400, "invalid_request"},
	} {
		if got := e.do("POST", "/parses/"+pid+"/figures", tc.body); got.status != tc.status || got.code(t) != tc.code {
			t.Errorf("%s: %d %s", name, got.status, got.body)
		}
	}
	for _, method := range []string{"POST", "GET"} {
		if got := e.as("bob-token").do(method, "/parses/"+pid+"/figures", nil); got.status != http.StatusNotFound || got.code(t) != "parse_not_found" {
			t.Errorf("%s as another caller: %d %s", method, got.status, got.body)
		}
	}

	// A parse that failed before it knew its pages has none to select from.
	failed := e.do("POST", "/parses", `{"source":{"file":"`+e.upload("scan2.png", append(sheet(t), 0))+`"},"pages":"9"}`, "Prefer", "wait=20").json(t)
	if got := e.do("POST", "/parses/"+failed["id"].(string)+"/figures", `{"pages":"1"}`); failed["state"] != "failed" || got.status != http.StatusBadRequest || got.code(t) != "invalid_pages" {
		t.Errorf("a parse with no pages: %v, then %d %s", failed["state"], got.status, got.body)
	}

	// With no describer configured there is nothing to ask.
	bare := figured(t, nil)
	other := bare.parsed(bare.upload("scan.png", sheet(t)), "")["id"].(string)
	if got := bare.do("POST", "/parses/"+other+"/figures", nil); got.status != http.StatusBadRequest || got.code(t) != "reader_not_found" {
		t.Fatalf("no describer: %d %s", got.status, got.body)
	}
}

func TestAFigureRunIsOneAtATimeAndSaysWhatItLost(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d := &stub.Describer{Fail: func(reader.FigureRequest) error {
		once.Do(func() { close(reached) })
		<-release
		return reader.Errorf(reader.Permanent, "no")
	}}
	e := figured(t, d)
	pid := e.parsed(e.upload("scan.png", sheet(t)), "")["id"].(string)

	started := e.do("POST", "/parses/"+pid+"/figures", nil)
	if started.status != http.StatusAccepted || at(started.json(t), "run", "state") != "running" || at(started.json(t), "run", "total") != 1.0 {
		t.Fatalf("started: %d %s", started.status, started.body)
	}
	<-reached
	if got := e.do("POST", "/parses/"+pid+"/figures", nil); got.status != http.StatusConflict || got.code(t) != "conflict" {
		t.Fatalf("a second run while one is in flight: %d %s", got.status, got.body)
	}
	close(release)
	<-e.server.Runner.FiguresDone(pid)

	// The run described nothing it set out to, so it failed, and the
	// figure says why.
	got := e.do("GET", "/parses/"+pid+"/figures", nil).json(t)
	fig := got["figures"].([]any)[0]
	if at(got, "run", "state") != "failed" || at(got, "run", "failed") != 1.0 || at(fig, "error", "code") != "figure_unreadable" || at(fig, "description") != nil {
		t.Fatalf("after the run: %v", got)
	}

	// A parse that has not ended cannot have its figures described.
	g, running, hold := gated(t, 1)
	sub := g.do("POST", "/parses", `{"source":{"file":"`+g.upload("scan.png", sheet(t))+`"}}`)
	<-running
	if got := g.do("POST", "/parses/"+sub.json(t)["id"].(string)+"/figures", nil); got.status != http.StatusConflict || got.code(t) != "not_terminal" {
		t.Fatalf("figures of a running parse: %d %s", got.status, got.body)
	}
	close(hold)
	g.ended(sub.json(t)["id"].(string))
}
