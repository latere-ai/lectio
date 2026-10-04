// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality_test

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/quality"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

// engine renders and counts PDFs for every test of the package. Loading it
// compiles the engine, which is done once.
var engine = render.NewPages()

// converter stands in for the conversion sidecar: it answers a conversion
// with a fixed file of the type wanted and records what it was asked. It is
// the fake of internal/parse's tests, toPDF, and of the converter
// cmd/lectiod's tests stand up beside the server: the conversion is not the
// office suite's, so what a test can hold is that the file was sent for
// conversion as the right pair of types and that the pages are then read
// from what came back. The suite itself converts the same files in the
// live run.
type converter struct {
	answers map[string][]byte // by the media type wanted
	asked   []string
}

func (c *converter) Convert(_ context.Context, _ []byte, from, to string) ([]byte, error) {
	c.asked = append(c.asked, from+" to "+to)
	return c.answers[to], nil
}

// pipeline is the pipeline a server runs, with the fake converter.
func pipeline(t *testing.T) (*parse.Pipeline, *converter) {
	t.Helper()
	c := &converter{answers: map[string][]byte{
		detect.MIMEDOCX: testfixtures.Read(t, testfixtures.ReportDOCX),
		detect.MIMEPDF:  testfixtures.Read(t, testfixtures.SurveyPDF),
	}}
	return &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: engine, Converter: c}, c
}

// prepared takes a corpus file through Prepare as an upload of its name is
// taken, and holds the manifest to what the corpus says the file is.
func prepared(t *testing.T, f file, selection string) (*parse.Pipeline, parse.Prepared) {
	t.Helper()
	p, c := pipeline(t)
	got, err := p.Prepare(context.Background(), testfixtures.Read(t, f.fixture), detect.DeclaredType{FileName: f.name}, selection)
	if err != nil {
		t.Fatalf("%s: %v", f.name, err)
	}
	if got.Manifest.MediaType != f.working || got.Manifest.Source != f.source {
		t.Fatalf("%s: manifest %+v, want %s read by %s", f.name, got.Manifest, f.working, f.source)
	}
	var want []string
	if f.detected != f.working {
		want = []string{f.detected + " to " + f.working}
	}
	if !slices.Equal(c.asked, want) {
		t.Fatalf("%s: the converter was asked %q, want %q", f.name, c.asked, want)
	}
	return p, got
}

// TestAFileReadFromItselfMatchesItsTruth is the bar for the formats read
// with no model: each goes through Prepare and assembly as a parse takes
// it, and what comes out is the file's truth, with no character, kind,
// cell or order wrong. A legacy Word file is one of them once it is
// converted, and here the conversion is the fake's.
func TestAFileReadFromItselfMatchesItsTruth(t *testing.T) {
	exact, _ := quality.BarsOf(quality.Exact)
	for _, f := range corpus {
		if f.source != document.SourceNative {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			truth := truthOf(t, f)
			_, got := prepared(t, f, "")
			if got.Manifest.PagesTotal != len(truth.Pages) || len(got.Native) != len(truth.Pages) {
				t.Fatalf("%d pages, %d read, want %d", got.Manifest.PagesTotal, len(got.Native), len(truth.Pages))
			}
			doc := assemble.Document("gate", got.Native)
			scores := quality.Score(truth, got.Native)
			if misses := exact.Misses(scores); len(misses) > 0 || scores.Matched != scores.Blocks || scores.Found != scores.Blocks {
				t.Errorf("misses %v; %d of %d blocks found among %d\n%s", misses, scores.Matched, scores.Blocks, scores.Found, strings.Join(scores.Notes, "\n"))
			}
			if scores.Boxes != nil {
				t.Error("a file read from itself has no box to score")
			}
			for _, p := range got.Native {
				for _, b := range p.Blocks {
					if b.Box != nil {
						t.Errorf("block %s of a native page has a box", b.Ref)
					}
				}
			}
			if len(doc.Spans) != 0 || !reflect.DeepEqual(doc.Outline, truth.Outline) {
				t.Errorf("spans %v, outline %+v, want no span and %+v", doc.Spans, doc.Outline, truth.Outline)
			}
			t.Logf("%d pages, %d blocks, %d characters, %d cells: CER %.4f", scores.Pages, scores.Blocks, scores.Characters, scores.CellsTotal, scores.CER)
		})
	}

	// A selection takes sheets as it takes pages.
	ledger := corpus[slices.IndexFunc(corpus, func(f file) bool { return f.name == "ledger.xlsx" })]
	_, second := prepared(t, ledger, "2-")
	if !slices.Equal(second.Manifest.Selected, []int{2, 3}) || len(second.Native) != 2 {
		t.Fatalf("the second sheet on: %+v", second.Manifest)
	}
	renumbered := []document.Page{second.Native[0], second.Native[1]}
	renumbered[0].Number, renumbered[1].Number = 1, 2
	if scores := quality.Score(truthOf(t, ledger).Select(2, 3), renumbered); scores.CER != 0 || scores.Cells == nil || *scores.Cells != 1 {
		t.Fatalf("the selected sheets: %+v", scores)
	}
}

// TestHTMLAndXMLAreNotRead says why the corpus holds neither: a parse of
// one is refused. When they are read, this fails, and each then needs a
// file and a truth in the corpus.
func TestHTMLAndXMLAreNotRead(t *testing.T) {
	p, _ := pipeline(t)
	for _, fixture := range []string{testfixtures.HTML, testfixtures.XML, testfixtures.SurveyHTML} {
		if _, err := p.Prepare(context.Background(), testfixtures.Read(t, fixture), detect.DeclaredType{FileName: fixture}, ""); fault.CodeOf(err) != fault.UnsupportedMediaType {
			t.Errorf("%s: %v", fixture, err)
		}
	}
}

// full is a reader's description that asks for a page at 160 dpi as a PNG:
// what a reader configured with no long edge is given.
var full = reader.Description{Name: "full", Accepts: []string{detect.MIMEPNG, detect.MIMEJPEG}, Image: reader.ImageSpec{DPI: 160, Format: "png"}}

// TestAFileAModelReadsIsPreparedAndRendered holds the pipeline to its own
// part of the files a reader reads, with the stub in the reader's place:
// the type the file is detected as and what it is converted to, the count
// of its pages, a selection of them, the image each page is rendered to,
// and the blank pages, which cost no call and hold no block.
func TestAFileAModelReadsIsPreparedAndRendered(t *testing.T) {
	for _, f := range corpus {
		if f.source != document.SourceReader {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			truth := truthOf(t, f)
			p, got := prepared(t, f, "")
			total := len(truth.Pages)
			blank := f.blank
			if f.detected != f.working {
				// The fake's conversion is the survey, whatever was sent.
				total, blank = 4, []int{4}
			}
			all := make([]int, total)
			for i := range all {
				all[i] = i + 1
			}
			if got.Manifest.PagesTotal != total || !slices.Equal(got.Manifest.Selected, all) || len(got.Native) != 0 {
				t.Fatalf("manifest %+v, want %d pages", got.Manifest, total)
			}

			stubbed := &stub.Reader{}
			read := make([]document.Page, 0, total)
			for _, n := range all {
				page, err := p.ReadPage(context.Background(), got.Manifest, got.Working, n, stubbed, parse.PageOptions{})
				if err != nil {
					t.Fatalf("page %d: %v", n, err)
				}
				if slices.Contains(blank, n) {
					if !page.Image.Blank || len(page.Page.Blocks) != 0 || stubbed.Calls(n) != 0 || page.Page.State != document.PageSucceeded || page.Page.Reader != "" {
						t.Errorf("page %d is blank: blank %v, %d blocks, %d calls, reader %q", n, page.Image.Blank, len(page.Page.Blocks), stubbed.Calls(n), page.Page.Reader)
					}
				} else if page.Image.Blank || len(page.Page.Blocks) != 3 || stubbed.Calls(n) != 1 || !strings.Contains(page.Page.Blocks[1].Text, stub.Digest(page.Image.Data)) {
					t.Errorf("page %d: blank %v, %d calls, blocks %+v", n, page.Image.Blank, stubbed.Calls(n), page.Page.Blocks)
				}
				if err := page.Page.Validate(); err != nil {
					t.Errorf("page %d: %v", n, err)
				}
				read = append(read, page.Page)
			}
			// The stub's pages assemble into a document that lists every
			// page, the blank ones with no block.
			doc := assemble.Document("gate", read)
			if len(doc.Pages) != total || doc.Usage.Pages != total || len(doc.Outline) != total-len(blank) {
				t.Errorf("document %+v", doc)
			}

			// The image a reader is given: an image file as it is, a page
			// of a PDF at the reader's resolution.
			if f.width > 0 {
				img, err := p.Renderer.Render(context.Background(), got.Working, got.Manifest.MediaType, 1, full)
				if err != nil {
					t.Fatal(err)
				}
				if img.Width != f.width || img.Height != f.height || img.Blank {
					t.Errorf("page 1 renders to %d by %d, blank %v; want %d by %d", img.Width, img.Height, img.Blank, f.width, f.height)
				}
				// A PNG or a JPEG the reader accepts is what it is given,
				// byte for byte; a page of a PDF and a frame of a TIFF are
				// encoded for it.
				passed := f.detected == detect.MIMEPNG || f.detected == detect.MIMEJPEG
				if want := map[bool]string{true: f.detected, false: detect.MIMEPNG}[passed]; img.MediaType != want || passed != bytes.Equal(img.Data, got.Working) {
					t.Errorf("page 1 is given as %s, the file's own bytes: %v", img.MediaType, bytes.Equal(img.Data, got.Working))
				}
			}

			// A selection names pages of the file and is clamped to it.
			last := []int{total}
			_, some := prepared(t, f, strconv.Itoa(total)+"-9")
			if !slices.Equal(some.Manifest.Selected, last) || some.Manifest.PagesTotal != total {
				t.Errorf("the last page on: %+v", some.Manifest)
			}
			if _, err := p.Prepare(context.Background(), testfixtures.Read(t, f.fixture), detect.DeclaredType{FileName: f.name}, strconv.Itoa(total+1)); fault.CodeOf(err) != fault.InvalidPages {
				t.Errorf("a page past the end: %v", err)
			}
		})
	}
}

// replay is a reader that makes no mistake: for each page it returns the
// blocks the truth holds for it. With it in the reader's place, a file goes
// through the pipeline as with a model, and what comes out is known.
type replay struct {
	pages []document.Page
	calls int
}

func (r *replay) Describe() reader.Description {
	return reader.Description{Name: "replay", Accepts: []string{detect.MIMEPNG}, Image: reader.ImageSpec{DPI: 72, Format: "png"}, Boxes: true, Version: "replay"}
}

func (r *replay) ReadPage(_ context.Context, page reader.Page) (reader.Result, error) {
	r.calls++
	blocks := slices.Clone(r.pages[page.Number-1].Blocks)
	for i := range blocks {
		blocks[i].Ref = "" // the caller numbers the page
	}
	return reader.Result{Blocks: blocks, Model: "replay", Usage: document.Usage{Pages: 1}}, nil
}

// TestAReadingWithNoMistakeScoresPerfectThroughThePipeline takes each file
// of the survey through the pipeline with a reader that returns the truth,
// and assembles what it read. Nothing between the reader and the document
// may cost a point: every measure is at its best, the table that continues
// is one span, the outline is the truth's, and the running header is
// printed once.
func TestAReadingWithNoMistakeScoresPerfectThroughThePipeline(t *testing.T) {
	exact, _ := quality.BarsOf(quality.Exact)
	for _, f := range corpus {
		if f.truth != testfixtures.SurveyTruth {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			truth := truthOf(t, f)
			p, got := prepared(t, f, "")
			r := &replay{pages: truth.Document()}
			read := make([]document.Page, 0, len(truth.Pages))
			for _, n := range got.Manifest.Selected {
				page, err := p.ReadPage(context.Background(), got.Manifest, got.Working, n, r, parse.PageOptions{})
				if err != nil {
					t.Fatalf("page %d: %v", n, err)
				}
				read = append(read, page.Page)
			}
			if want := len(truth.Pages) - len(f.blank); r.calls != want {
				t.Errorf("%d pages were read, want %d: a blank page costs no call", r.calls, want)
			}
			doc := assemble.Document("gate", read)
			scores := quality.Score(truth, read)
			if misses := exact.Misses(scores); len(misses) > 0 || scores.Boxes == nil || scores.Matched != scores.Blocks {
				t.Errorf("misses %v, scores %+v\n%s", misses, scores, strings.Join(scores.Notes, "\n"))
			}
			if !reflect.DeepEqual(doc.Spans, truth.Spans) {
				t.Errorf("spans %+v, want %+v", doc.Spans, truth.Spans)
			}
			if !reflect.DeepEqual(doc.Outline, truth.Outline) {
				t.Errorf("outline %+v, want %+v", doc.Outline, truth.Outline)
			}
		})
	}
}

// TestAssemblyFindsWhatTheSurveyHolds runs assembly over the survey as a
// reader would return it that labels no furniture: the running header and
// the page numbers come back as text. Assembly is to find them by what
// they say and where they lie, mark every header after the first as
// repeated, join the table that continues, and build the outline from the
// printed section numbers, so that the pages hold the truth's kinds again.
func TestAssemblyFindsWhatTheSurveyHolds(t *testing.T) {
	truth, err := quality.Parse(testfixtures.Read(t, testfixtures.SurveyTruth))
	if err != nil {
		t.Fatal(err)
	}
	read := truth.Document()
	furniture := 0
	for i := range read {
		for j := range read[i].Blocks {
			b := &read[i].Blocks[j]
			if b.Kind == document.KindPageHeader || b.Kind == document.KindPageNumber {
				b.Kind = document.KindText
				furniture++
			}
			// A reader sees one page: every heading is at the top for it.
			if b.Kind == document.KindHeading {
				b.Level = 1
			}
		}
	}
	if furniture != 6 {
		t.Fatalf("the survey holds %d blocks of furniture, and this test expects 6", furniture)
	}

	doc := assemble.Document("gate", read)
	if scores := quality.Score(truth, read); scores.Kinds != 1 || scores.CER != 0 {
		t.Errorf("after assembly: %+v\n%s", scores, strings.Join(scores.Notes, "\n"))
	}
	if !reflect.DeepEqual(doc.Spans, truth.Spans) || len(truth.Spans) != 1 {
		t.Errorf("spans %+v, want %+v", doc.Spans, truth.Spans)
	}
	if !reflect.DeepEqual(doc.Outline, truth.Outline) || len(truth.Outline) != 7 {
		t.Errorf("outline %+v, want %+v", doc.Outline, truth.Outline)
	}
	var headers, repeated int
	for _, p := range read {
		for _, b := range p.Blocks {
			if b.Kind == document.KindPageHeader {
				headers++
				if b.Repeated {
					repeated++
				}
			}
		}
	}
	if headers != 3 || repeated != 2 {
		t.Errorf("%d running headers, %d marked repeated; want 3 and 2", headers, repeated)
	}
	if len(doc.Pages) != 4 || doc.Pages[3].Blocks != 0 || doc.Pages[3].State != document.PageSucceeded {
		t.Errorf("pages %+v", doc.Pages)
	}

	// The Markdown of the result prints the header once and no page number.
	var printed strings.Builder
	if err := assemble.Markdown(&printed, read, doc.Outline, assemble.View{}); err != nil {
		t.Fatal(err)
	}
	md := printed.String()
	if strings.Count(md, "Tide gauge survey, third quarter") != 1 || strings.Contains(md, "\n2\n") || !strings.Contains(md, "### 2.1 Weekly record") || !strings.Contains(md, "| A, north mole | 212.4 | 61.8 | +38 | 99.1% |") {
		t.Errorf("the document as Markdown:\n%s", md)
	}

	// Assembling again changes nothing.
	again := assemble.Document("gate", read)
	if !reflect.DeepEqual(again, doc) {
		t.Error("a second assembly gives another document")
	}
}

// TestTheBarsAreStatedOnceInTheDocumentation keeps the table of bars a
// person reads equal to the bars a run is judged by.
func TestTheBarsAreStatedOnceInTheDocumentation(t *testing.T) {
	doc, err := os.ReadFile("../../docs/quality.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), quality.BarsTable()) {
		t.Errorf("docs/quality.md does not hold the table of bars as it is:\n%s", quality.BarsTable())
	}
	// Every file of the corpus is listed, with the class its bars come from.
	for _, f := range corpus {
		if _, ok := quality.BarsOf(f.class); !ok {
			t.Errorf("%s: class %q has no bars", f.name, f.class)
		}
		if !strings.Contains(string(doc), "| `"+f.name+"` | "+f.format+" | `"+string(f.class)+"` |") {
			t.Errorf("docs/quality.md does not list %s as %s of class %s", f.name, f.format, f.class)
		}
	}
}
