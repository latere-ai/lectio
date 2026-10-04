// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package parse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/text"
)

// asks is a reader that says whether it asks for a page's own text, and
// keeps the page it was handed.
type asks struct {
	text bool
	got  reader.Page
}

func (a *asks) Describe() reader.Description {
	return reader.Description{Name: "asks", Accepts: []string{"image/png"}, Image: reader.ImageSpec{DPI: 36, Format: "png"}, Text: a.text}
}

func (a *asks) ReadPage(_ context.Context, page reader.Page) (reader.Result, error) {
	a.got = page
	return reader.Result{Blocks: []document.Block{{Kind: document.KindText, Order: 1, Text: "read"}}}, nil
}

// imagesOnly renders pages and reads no text of any.
type imagesOnly struct{ render.Renderer }

// noText renders pages, and fails with err when a page's text is asked
// for.
type noText struct {
	render.Renderer
	err error
}

func (n noText) TextPDF(context.Context, []byte, int) (reader.PageText, error) {
	return reader.PageText{}, n.err
}

// A reader that asks for a page's own text is handed it beside the image,
// for a page of a file that carries its text. Every other reader, every
// other format, and a renderer that reads no text leave it out.
func TestAReaderThatAsksIsHandedThePagesOwnText(t *testing.T) {
	log := Manifest{MediaType: detect.MIMEPDF}
	pdf := testfixtures.Logbook(1)
	whole := &Pipeline{Limits: pages.DefaultLimits(), Renderer: engines}

	asked := &asks{text: true}
	if _, err := whole.ReadPage(context.Background(), log, pdf, 1, asked, PageOptions{}); err != nil {
		t.Fatal(err)
	}
	own := asked.got.Text
	if own == nil || own.Width != 612 || own.Height != 792 || len(own.Words) < 60 || own.Words[0].Text != "Entry" || !own.Words[0].Bold || own.Partial {
		t.Fatalf("the page's own text: %+v", own)
	}
	if len(asked.got.Data) == 0 || asked.got.MediaType != "image/png" || asked.got.Width != 306 {
		t.Errorf("the page's image is handed over too: %d bytes of %s, %d wide", len(asked.got.Data), asked.got.MediaType, asked.got.Width)
	}

	for name, tc := range map[string]struct {
		p    *Pipeline
		m    Manifest
		file []byte
		r    *asks
	}{
		"a reader that does not ask":     {whole, log, pdf, &asks{}},
		"a format that carries no text":  {whole, Manifest{MediaType: detect.MIMEPNG}, sheet(t, 40, 40, false), &asks{text: true}},
		"a renderer that reads no text":  {&Pipeline{Renderer: imagesOnly{engines}}, log, pdf, &asks{text: true}},
		"text the engine could not read": {&Pipeline{Renderer: noText{engines, fault.New(fault.DocumentCorrupt, "the text of page 1 was not read in the time a page is given")}}, log, pdf, &asks{text: true}},
	} {
		if _, err := tc.p.ReadPage(context.Background(), tc.m, tc.file, 1, tc.r, PageOptions{}); err != nil || tc.r.got.Text != nil || len(tc.r.got.Data) == 0 {
			t.Errorf("%s: %v, text %v", name, err, tc.r.got.Text)
		}
	}

	// A failure that is not the page's own is the read's: the engine did
	// not start, or the read was canceled while the text was asked for.
	down := errors.New("the engine did not start")
	failing := &Pipeline{Renderer: noText{engines, down}}
	if _, err := failing.ReadPage(context.Background(), log, pdf, 1, &asks{text: true}, PageOptions{}); !errors.Is(err, down) {
		t.Errorf("an engine that did not start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := &Pipeline{Renderer: noText{canceling{engines, cancel}, fault.New(fault.DocumentCorrupt, "the PDF engine stopped while reading the file")}}
	if _, err := stopped.ReadPage(ctx, log, pdf, 1, &asks{text: true}, PageOptions{}); fault.CodeOf(err) != fault.DocumentCorrupt || ctx.Err() == nil {
		t.Errorf("a read canceled while the text was asked for: %v", err)
	}
}

// canceling renders a page and then cancels the read it was rendered for.
type canceling struct {
	render.Renderer
	cancel context.CancelFunc
}

func (c canceling) Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (render.Image, error) {
	img, err := c.Renderer.Render(ctx, data, mediaType, n, want)
	c.cancel()
	return img, err
}

// A page the text reader reads says so: its source is the file's text
// layer, it names its reader and no model, and it counts one page and no
// token. A page that holds no text is declined, for the next reader, and
// a page with nothing on it is read by no reader at all.
func TestAPageReadFromItsOwnTextSaysSo(t *testing.T) {
	own, err := text.New(text.Config{Name: "own", Image: reader.ImageSpec{DPI: 36}})
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{Limits: pages.DefaultLimits(), Renderer: engines}
	log := testfixtures.Logbook(2, 2)
	prepared, err := p.Prepare(context.Background(), log, named("log.pdf"), "")
	if err != nil || prepared.Manifest.Source != document.SourceReader || prepared.Manifest.PagesTotal != 2 {
		t.Fatalf("the logbook is prepared as a file a reader reads: %+v, %v", prepared.Manifest, err)
	}

	got, err := p.ReadPage(context.Background(), prepared.Manifest, prepared.Working, 1, own, PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	page := got.Page
	if page.Source != document.SourceTextLayer || page.Reader != "own" || page.Model != "" || page.Truncated || *page.Usage != (document.Usage{Pages: 1}) || page.State != document.PageSucceeded {
		t.Fatalf("the page: %+v", page)
	}
	heading, paragraphs := testfixtures.LogbookEntry(1)
	want := []struct {
		kind document.Kind
		text string
	}{{document.KindHeading, heading}, {document.KindText, paragraphs[0]}, {document.KindText, paragraphs[1]}, {document.KindText, "1"}}
	if len(page.Blocks) != len(want) {
		t.Fatalf("%d blocks: %+v", len(page.Blocks), page.Blocks)
	}
	for i, w := range want {
		if b := page.Blocks[i]; b.Kind != w.kind || b.Text != w.text || b.Ref != document.Ref(1, i+1) || b.Box == nil {
			t.Errorf("block %d is %s %q at %v, want %s %q", i+1, b.Kind, b.Text, b.Box, w.kind, w.text)
		}
	}
	// The result holds the page's image, as a page a model read does.
	if got.Image.Width != 306 || got.Image.Height != 396 || got.Image.MediaType != "image/png" || page.Width != 306 {
		t.Errorf("the page's image: %d by %d, %s", got.Image.Width, got.Image.Height, got.Image.MediaType)
	}
	if err := page.Validate(); err != nil {
		t.Error(err)
	}

	// The page that is a picture is declined, with the class a chain moves
	// down on.
	_, err = p.ReadPage(context.Background(), prepared.Manifest, prepared.Working, 2, own, PageOptions{})
	if reader.ClassOf(err) != reader.Refused || !strings.Contains(err.Error(), "no text") {
		t.Errorf("a page that is a picture: %v", err)
	}
	// So is a page of a format that carries no text of its own.
	if _, err := p.ReadPage(context.Background(), Manifest{MediaType: detect.MIMEPNG}, sheet(t, 40, 40, false), 1, own, PageOptions{}); reader.ClassOf(err) != reader.Refused {
		t.Errorf("an image: %v", err)
	}
	// A page with nothing on it is not declined: it is read by no reader.
	blank, err := p.ReadPage(context.Background(), Manifest{MediaType: detect.MIMEPDF}, testfixtures.Read(t, testfixtures.TextPDF), 2, own, PageOptions{})
	if err != nil || !blank.Image.Blank || blank.Page.Reader != "" || len(blank.Page.Blocks) != 0 || blank.Page.Source != document.SourceReader {
		t.Errorf("a page with nothing on it: %+v, %v", blank.Page, err)
	}
}
