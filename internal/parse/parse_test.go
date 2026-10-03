// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package parse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/stub"
)

func pipeline() *Pipeline {
	return &Pipeline{Limits: pages.DefaultLimits(), Renderer: render.Images{}}
}

func named(name string) detect.DeclaredType { return detect.DeclaredType{FileName: name} }

// sheet is a white PNG, with a dark bar across it unless it is to be blank.
func sheet(t *testing.T, w, h int, blank bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{255, 255, 255, 255}
			if !blank && y > h/3 && y < 2*h/3 {
				c = color.RGBA{0, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// toPDF is a converter that answers every request with a fixed PDF.
type toPDF struct {
	out      []byte
	from, to string
	err      error
}

func (c *toPDF) Convert(_ context.Context, _ []byte, from, to string) ([]byte, error) {
	c.from, c.to = from, to
	return c.out, c.err
}

func TestPrepareAFileReadByAReader(t *testing.T) {
	for name, tc := range map[string]struct {
		fixture   string
		selection string
		mediaType string
		total     int
		selected  []int
	}{
		"an image is one page":  {testfixtures.PNG, "", detect.MIMEPNG, 1, []int{1}},
		"a TIFF, every frame":   {testfixtures.MultiTIFF, "", detect.MIMETIFF, 3, []int{1, 2, 3}},
		"a TIFF, some frames":   {testfixtures.MultiTIFF, "2-9", detect.MIMETIFF, 3, []int{2, 3}},
		"a PDF":                 {testfixtures.MultipagePDF, "1,3", detect.MIMEPDF, 3, []int{1, 3}},
		"a signed PDF, wrapped": {testfixtures.WrappedPDF, "", detect.MIMEPDF, 1, []int{1}},
	} {
		got, err := pipeline().Prepare(context.Background(), testfixtures.Read(t, tc.fixture), named(tc.fixture), tc.selection)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		want := Manifest{MediaType: tc.mediaType, PagesTotal: tc.total, Selected: tc.selected, Source: document.SourceReader}
		if !reflect.DeepEqual(got.Manifest, want) || len(got.Native) != 0 || len(got.Working) == 0 {
			t.Errorf("%s: manifest = %+v, want %+v", name, got.Manifest, want)
		}
	}

	// The working copy of a container is what was inside it.
	wrapped, err := pipeline().Prepare(context.Background(), testfixtures.Read(t, testfixtures.WrappedPDF), named("invoice.p7m"), "")
	if err != nil || !bytes.Equal(wrapped.Working, testfixtures.Read(t, testfixtures.MinimalPDF)) {
		t.Fatalf("the working copy of a signed PDF is the PDF: %v", err)
	}
}

func TestPrepareAFileThatCarriesItsOwnStructure(t *testing.T) {
	got, err := pipeline().Prepare(context.Background(), testfixtures.Read(t, testfixtures.Markdown), named("notes.md"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.Source != document.SourceNative || got.Manifest.MediaType != detect.MIMEMarkdown || got.Manifest.PagesTotal != 1 || len(got.Native) != 1 {
		t.Fatalf("prepared = %+v", got.Manifest)
	}
	if p := got.Native[0]; p.State != document.PageSucceeded || p.Source != document.SourceNative || len(p.Blocks) == 0 {
		t.Fatalf("native page = %+v", p)
	}

	csv, err := pipeline().Prepare(context.Background(), testfixtures.Read(t, testfixtures.CSV), named("table.csv"), "1")
	if err != nil || csv.Native[0].Blocks[0].Kind != document.KindTable {
		t.Fatalf("a delimited table is one page with one table: %+v, %v", csv.Manifest, err)
	}
}

func TestPrepareConverts(t *testing.T) {
	conv := &toPDF{out: testfixtures.Read(t, testfixtures.MultipagePDF)}
	p := pipeline()
	p.Converter = conv

	got, err := p.Prepare(context.Background(), testfixtures.Read(t, testfixtures.PPTX), named("deck.pptx"), "")
	if err != nil {
		t.Fatal(err)
	}
	if conv.from != detect.MIMEPPTX || conv.to != detect.MIMEPDF || got.Manifest.MediaType != detect.MIMEPDF || got.Manifest.PagesTotal != 3 {
		t.Fatalf("converted %s to %s; manifest %+v", conv.from, conv.to, got.Manifest)
	}
	if !bytes.Equal(got.Working, conv.out) {
		t.Fatal("the working copy of a converted file is the conversion")
	}

	// A legacy word-processing file converts to a format this build does
	// not read yet, and says so.
	_, err = p.Prepare(context.Background(), testfixtures.Read(t, testfixtures.DOC), named("letter.doc"), "")
	if fault.CodeOf(err) != fault.UnsupportedMediaType || conv.to != detect.MIMEDOCX {
		t.Fatalf("a converted word-processing file: %v (converted to %s)", err, conv.to)
	}

	conv.err = fault.New(fault.DocumentCorrupt, "the converter gave up")
	if _, err := p.Prepare(context.Background(), testfixtures.Read(t, testfixtures.PPTX), named("deck.pptx"), ""); fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Fatalf("a failed conversion: %v", err)
	}
}

func TestPrepareRefuses(t *testing.T) {
	small := pipeline()
	small.Limits = pages.Limits{MaxBytes: 16, MaxPages: 2}
	twoPages := pipeline()
	twoPages.Limits.MaxPages = 2

	for name, tc := range map[string]struct {
		p         *Pipeline
		data      []byte
		declared  detect.DeclaredType
		selection string
		code      fault.Code
	}{
		"a file over the size limit":   {small, testfixtures.Read(t, testfixtures.PNG), named("a.png"), "", fault.FileTooLarge},
		"a file of no known type":      {pipeline(), []byte{0, 1, 2, 3}, named("blob.bin"), "", fault.UnsupportedMediaType},
		"a format needing a converter": {pipeline(), testfixtures.Read(t, testfixtures.PPTX), named("deck.pptx"), "", fault.UnsupportedMediaType},
		"a recognized format not read": {pipeline(), testfixtures.Read(t, testfixtures.DOCX), named("a.docx"), "", fault.UnsupportedMediaType},
		"too many pages":               {twoPages, testfixtures.Read(t, testfixtures.MultiTIFF), named("a.tiff"), "", fault.TooManyPages},
		"a selection naming no page":   {pipeline(), testfixtures.Read(t, testfixtures.MultiTIFF), named("a.tiff"), "7", fault.InvalidPages},
		"a malformed selection":        {pipeline(), testfixtures.Read(t, testfixtures.Markdown), named("a.md"), "x", fault.InvalidPages},
		"a container that is not one":  {pipeline(), []byte("0\x80 not DER"), detect.DeclaredType{FileName: "a.p7m"}, "", fault.DocumentCorrupt},
		"a PDF that is not one":        {pipeline(), []byte("%PDF-1.7\nnothing here"), named("a.pdf"), "", fault.DocumentCorrupt},
		"a TIFF that is not one":       {pipeline(), []byte("II*\x00\xff\xff\xff\xff"), named("a.tiff"), "", fault.DocumentCorrupt},
		"text that is not UTF-8":       {pipeline(), []byte("caf\xe9 \xff\xfe plain"), named("a.txt"), "", fault.DocumentCorrupt},
	} {
		_, err := tc.p.Prepare(context.Background(), tc.data, tc.declared, tc.selection)
		if fault.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}

	// A native file is held to the page limit too.
	none := pipeline()
	none.Limits.MaxPages = 0
	if _, err := none.Prepare(context.Background(), testfixtures.Read(t, testfixtures.TXT), named("a.txt"), ""); err != nil {
		t.Fatalf("no page limit: %v", err)
	}
}

func TestReadPage(t *testing.T) {
	p := pipeline()
	prepared, err := p.Prepare(context.Background(), sheet(t, 200, 100, false), named("scan.png"), "")
	if err != nil {
		t.Fatal(err)
	}
	r := &stub.Reader{}

	got, err := p.ReadPage(context.Background(), prepared.Manifest, prepared.Working, 1, r, PageOptions{Languages: []string{"en"}, Credential: reader.NewCredential("k")})
	if err != nil {
		t.Fatal(err)
	}
	page := got.Page
	if page.Number != 1 || page.State != document.PageSucceeded || page.Source != document.SourceReader || page.Reader != stub.Name || page.Model != stub.Name {
		t.Fatalf("page = %+v", page)
	}
	if page.Width != 200 || page.Height != 100 || got.Image.MediaType != "image/png" || len(got.Image.Data) == 0 {
		t.Fatalf("page size %vx%v, image %s", page.Width, page.Height, got.Image.MediaType)
	}
	if len(page.Blocks) != 3 || page.Blocks[0].Ref != "1.1" || page.Blocks[2].Ref != "1.3" || page.Usage.Pages != 1 {
		t.Fatalf("blocks = %+v", page.Blocks)
	}
	if !strings.Contains(page.Blocks[1].Text, stub.Digest(got.Image.Data)) {
		t.Fatal("the image returned is the image the reader was given")
	}
	if err := page.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReadPageSkipsABlankPage(t *testing.T) {
	r := &stub.Reader{}
	got, err := pipeline().ReadPage(context.Background(), Manifest{MediaType: detect.MIMEPNG}, sheet(t, 40, 40, true), 1, r, PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Calls(1) != 0 || got.Page.State != document.PageSucceeded || len(got.Page.Blocks) != 0 || got.Page.Blocks == nil || got.Page.Reader != "" || got.Page.Usage.Pages != 1 {
		t.Fatalf("a blank page costs no call and is a succeeded page with no blocks: calls %d, %+v", r.Calls(1), got.Page)
	}
}

// answers is a reader that returns a fixed result.
type answers struct {
	result reader.Result
	err    error
}

func (a answers) Describe() reader.Description {
	return reader.Description{Name: "fixed", Accepts: []string{"image/png"}, Image: reader.ImageSpec{Format: "png"}}
}

func (a answers) ReadPage(context.Context, reader.Page) (reader.Result, error) {
	return a.result, a.err
}

func TestReadPageFailures(t *testing.T) {
	m := Manifest{MediaType: detect.MIMEPNG}
	p := pipeline()
	page := sheet(t, 40, 40, false)

	limited := reader.Errorf(reader.RateLimited, "slow down")
	if _, err := p.ReadPage(context.Background(), m, page, 1, answers{err: limited}, PageOptions{}); !errors.Is(err, limited) {
		t.Fatalf("the reader's error comes back as it classified it: %v", err)
	}
	if _, err := p.ReadPage(context.Background(), m, page, 1, answers{}, PageOptions{}); reader.ClassOf(err) != reader.Invalid {
		t.Fatalf("no blocks for a page with content: %v", err)
	}
	bad := answers{result: reader.Result{Blocks: []document.Block{{Kind: "nonsense", Order: 1, Text: "x"}}}}
	if _, err := p.ReadPage(context.Background(), m, page, 1, bad, PageOptions{}); reader.ClassOf(err) != reader.Invalid {
		t.Fatalf("blocks that do not fit the model: %v", err)
	}
	if _, err := p.ReadPage(context.Background(), Manifest{MediaType: detect.MIMEPDF}, []byte("%PDF"), 1, &stub.Reader{}, PageOptions{}); fault.CodeOf(err) != fault.UnsupportedMediaType {
		t.Fatalf("a page that cannot be rendered: %v", err)
	}
}

// pdfOf writes objects, numbered from 1, as a PDF with a correct
// cross-reference table. The first object is the catalog.
func pdfOf(objects ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, at := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", at)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// engines renders and counts PDFs for the tests below. Loading it compiles
// the engine, which is done once.
var engines = render.NewPages()

// A count read off a PDF's bytes can be made wrong in both directions. A
// parse goes by the count of the engine that renders the pages.
func TestPrepareCountsAPDFsPagesWithTheEngineThatRendersThem(t *testing.T) {
	// A page tree of two levels, 80 nodes of 2 pages. The root writes its
	// kids before its count, which puts the count past where a scan of the
	// bytes looks, so the scan reports a node's count: 2 pages of 160.
	const nodes, each = 80, 2
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	var kids strings.Builder
	for i := range nodes {
		node := len(objects) + 1
		fmt.Fprintf(&kids, " %d 0 R", node)
		objects = append(objects, fmt.Sprintf("<< /Type /Pages /Parent 2 0 R /Count %d /Kids [ %d 0 R %d 0 R ] >>", each, node+1, node+2))
		for range each {
			objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] >>", node))
		}
		_ = i
	}
	objects[1] = "<< /Type /Pages /Kids [" + kids.String() + " ] /Count " + fmt.Sprint(nodes*each) + " >>"
	deep := pdfOf(objects...)

	// A tree of 3 pages, and an object nothing refers to that claims 40.
	orphan := pdfOf(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 3 0 R 4 0 R 5 0 R ] /Count 3 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Pages /Kids [ ] /Count 40 >>",
	)

	for name, tc := range map[string]struct {
		file           []byte
		scanned, pages int
	}{
		"a count the scan does not reach":        {deep, each, nodes * each},
		"a count on an object nothing refers to": {orphan, 40, 3},
	} {
		if scanned, err := pages.CountPDF(tc.file); err != nil || scanned != tc.scanned {
			t.Fatalf("%s: the bytes say %d, %v; this test expects them to say %d", name, scanned, err, tc.scanned)
		}
		p := &Pipeline{Limits: pages.DefaultLimits(), Renderer: engines}
		got, err := p.Prepare(context.Background(), tc.file, named("file.pdf"), "")
		if err != nil || got.Manifest.PagesTotal != tc.pages || len(got.Manifest.Selected) != tc.pages {
			t.Errorf("%s: %d pages, %v; the file has %d", name, got.Manifest.PagesTotal, err, tc.pages)
		}
		// Without an engine the scan is all there is.
		got, err = pipeline().Prepare(context.Background(), tc.file, named("file.pdf"), "")
		if err != nil || got.Manifest.PagesTotal != tc.scanned {
			t.Errorf("%s, with no engine: %d pages, %v", name, got.Manifest.PagesTotal, err)
		}
	}

	// The limit on pages is applied to the engine's count.
	two := &Pipeline{Limits: pages.Limits{MaxPages: 2}, Renderer: engines}
	if _, err := two.Prepare(context.Background(), orphan, named("file.pdf"), ""); fault.CodeOf(err) != fault.TooManyPages {
		t.Errorf("three pages against a limit of two: %v", err)
	}
	// A PDF the engine cannot open is refused, whatever its bytes claim.
	claims := []byte("%PDF-1.4\n<< /Type /Pages /Count 5 >>\n")
	if _, err := (&Pipeline{Renderer: engines}).Prepare(context.Background(), claims, named("file.pdf"), ""); fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Errorf("bytes that claim five pages and hold no document: %v", err)
	}
}
