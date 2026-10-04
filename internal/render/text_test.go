// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium/structs"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/testfixtures"
	"latere.ai/x/lectio/reader"
)

// typeset is a one-page PDF that draws content with 3 of the standard
// fonts: F1 upright, F2 bold, F3 slanted. box is the page's MediaBox, page
// what else its dictionary holds, and resources what else its resources
// hold. more are further objects, numbered from 8.
func typeset(box, page, resources, content string, more ...string) []byte {
	return pdfOf(append([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [%s] %s /Contents 4 0 R /Resources << /Font << /F1 5 0 R /F2 6 0 R /F3 7 0 R >> %s >> >>", box, page, resources),
		streamOf("", content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Oblique >>",
	}, more...)...)
}

const letter = "0 0 612 792"

func textOf(t *testing.T, pdf []byte, n int) reader.PageText {
	t.Helper()
	got, err := engine.TextPDF(context.Background(), pdf, n)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func near(a, b, by float64) bool { return math.Abs(a-b) <= by }

func nearRect(a, b reader.Rect, by float64) bool {
	return near(a.X0, b.X0, by) && near(a.Y0, b.Y0, by) && near(a.X1, b.X1, by) && near(a.Y1, b.Y1, by)
}

// A page's words come with their text, the place each is drawn at, from
// the top left of the page in points, and what its type says of it.
func TestAPagesWordsComeWithTheirPlaceAndTheirType(t *testing.T) {
	got := textOf(t, typeset(letter, "", "", strings.Join([]string{
		"BT /F1 12 Tf 72 720 Td (Plain words here) Tj ET",
		"BT /F2 24 Tf 72 680 Td (Heavy) Tj ET",
		"BT /F3 10 Tf 72 660 Td (Slanted) Tj ET",
		"BT /F1 10 Tf 2 0 0 2 72 600 Tm (Doubled) Tj ET",
		"BT /F1 10 Tf 3 Tr 72 560 Td (Unseen) Tj 0 Tr ET",
		"BT /F1 10 Tf 0 1 -1 0 300 400 Tm (Upward) Tj ET",
		"BT /F1 10 Tf 72 300 Td (far) Tj 200 0 Td (apart) Tj ET",
	}, "\n")), 1)
	if got.Width != 612 || got.Height != 792 || got.Partial || len(got.Rects) != 0 || len(got.Drawings) != 0 {
		t.Fatalf("the page: %+v", got)
	}
	type word struct {
		text                         string
		x, baseline, size            float64
		bold, italic, hidden, turned bool
	}
	want := []word{
		{text: "Plain", x: 72, baseline: 72, size: 12},
		{text: "words", x: 102, baseline: 72, size: 12},
		{text: "here", x: 138, baseline: 72, size: 12},
		{text: "Heavy", x: 72, baseline: 112, size: 24, bold: true},
		{text: "Slanted", x: 72, baseline: 132, size: 10, italic: true},
		{text: "Doubled", x: 72, baseline: 192, size: 20},
		{text: "Unseen", x: 72, baseline: 232, size: 10, hidden: true},
		{text: "Upward", x: 300, baseline: 392, size: 10, turned: true},
		{text: "far", x: 72, baseline: 492, size: 10},
		{text: "apart", x: 272, baseline: 492, size: 10},
	}
	if len(got.Words) != len(want) {
		t.Fatalf("%d words, want %d: %+v", len(got.Words), len(want), got.Words)
	}
	for i, w := range want {
		g := got.Words[i]
		if g.Text != w.text || g.Bold != w.bold || g.Italic != w.italic || g.Hidden != w.hidden || g.Turned != w.turned || g.Unmapped != 0 || !near(g.Size, w.size, 0.01) {
			t.Errorf("word %d: %+v, want %+v", i, g, w)
		}
		if w.turned {
			continue
		}
		// A word begins within a point of where its text was placed,
		// stands on the baseline it was placed on, and its box spans the
		// line of type around that baseline.
		if !near(g.Box.X0, w.x, 1.5) || !near(g.Baseline, w.baseline, 0.01) || g.Box.Y0 >= g.Baseline || g.Box.Y1 <= g.Baseline || g.Box.X1 <= g.Box.X0 || g.Box.Height() < 0.9*w.size || g.Box.Height() > 1.6*w.size {
			t.Errorf("word %d %q lies at %+v on %.1f, want it from %.1f on %.1f", i, g.Text, g.Box, g.Baseline, w.x, w.baseline)
		}
	}
	// The words of one line share the extent of their line.
	if a, b := got.Words[0].Box, got.Words[2].Box; a.Y0 != b.Y0 || a.Y1 != b.Y1 {
		t.Errorf("2 words of one line span %+v and %+v", a, b)
	}

	// The fixture of 2 letter pages holds a line of text and a bar, then
	// nothing.
	pdf := testfixtures.Read(t, testfixtures.TextPDF)
	first, second := textOf(t, pdf, 1), textOf(t, pdf, 2)
	if len(first.Words) == 0 || len(first.Rects) != 1 || !nearRect(first.Rects[0], reader.Rect{X0: 72, Y0: 168, X1: 540, Y1: 192}, 0.5) {
		t.Errorf("the first page: %+v", first)
	}
	if len(second.Words) != 0 || len(second.Rects) != 0 || len(second.Drawings) != 0 || second.Partial || second.Width != 612 {
		t.Errorf("the page with nothing on it: %+v", second)
	}
	if again, err := (&Pages{PDF: engine}).TextPDF(context.Background(), pdf, 1); err != nil || len(again.Words) != len(first.Words) {
		t.Errorf("through the renderer of every format: %d words, %v", len(again.Words), err)
	}
}

// A character the file maps to no Unicode character is counted in its
// word and stands in it as U+FFFD, and a word the page's crop leaves
// outside is not handed over.
func TestCharactersWithNoMappingAreCountedAndWordsOffThePageAreLeftOut(t *testing.T) {
	onePage := func(content string, fonts ...string) []byte {
		return pdfOf(append([]string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
			streamOf("", content),
		}, fonts...)...)
	}
	for name, tc := range map[string]struct {
		pdf      []byte
		text     string
		unmapped int
	}{
		// An encoding that names glyphs no list of glyph names knows.
		"glyph names of the font's own": {onePage("BT /F1 12 Tf 72 700 Td (ABC) Tj ET",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Type /Encoding /Differences [65 /g001 /g002 /g003] >> >>"), "\ufffd\ufffd\ufffd", 3},
		// A table to Unicode that maps 2 of 3 codes to nothing usable.
		"a table that maps to nothing": {onePage("BT /F1 12 Tf 72 700 Td (ABC) Tj ET",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /ToUnicode 6 0 R >>",
			streamOf("", "/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CMapName /X def /CMapType 2 def 1 begincodespacerange <00> <FF> endcodespacerange 2 beginbfchar <41> <FFFD> <42> <0000> endbfchar endcmap CMapName currentdict /CMap defineresource pop end end")), "\ufffd\ufffdC", 2},
		// A font of drawn glyphs that says nothing of what they are.
		"glyphs drawn in the file": {onePage("BT /F1 12 Tf 72 700 Td (ab) Tj ET",
			"<< /Type /Font /Subtype /Type3 /FontBBox [0 0 1000 1000] /FontMatrix [0.001 0 0 0.001 0 0] /CharProcs << /sq 6 0 R >> /Encoding << /Type /Encoding /Differences [97 /sq /sq] >> /FirstChar 97 /LastChar 98 /Widths [1000 1000] >>",
			streamOf("", "1000 0 d0 0 0 800 800 re f")), "\ufffd\ufffd", 2},
	} {
		got := textOf(t, tc.pdf, 1)
		if len(got.Words) != 1 || got.Words[0].Unmapped != tc.unmapped || got.Words[0].Text != tc.text {
			t.Errorf("%s: %+v", name, got.Words)
		}
	}

	cropped := textOf(t, typeset(letter, "/CropBox [100 100 400 400]", "", "BT /F1 12 Tf 150 300 Td (shown) Tj 0 250 Td (cut) Tj ET"), 1)
	if len(cropped.Words) != 1 || cropped.Words[0].Text != "shown" || cropped.Width != 300 || !near(cropped.Words[0].Box.X0, 50, 1.5) || !near(cropped.Words[0].Baseline, 100, 0.01) {
		t.Errorf("a page cropped around one of its 2 words: %+v", cropped)
	}
	// A page too small for a device unit still has its words placed.
	if speck := textOf(t, typeset("0 0 0.001 0.001", "", "", "BT /F1 12 Tf 150 300 Td (far) Tj ET"), 1); len(speck.Words) != 0 || speck.Partial {
		t.Errorf("a page of a thousandth of a point: %+v", speck)
	}
}

// ink is where a page's rendering is not paper, at one pixel to a point.
func ink(t *testing.T, pdf []byte) (dark [][2]int, width, height int) {
	t.Helper()
	got, err := engine.Render(context.Background(), pdf, detect.MIMEPDF, 1, describe(72, 0, "png"))
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	for y := range got.Height {
		for x := range got.Width {
			if r, g, b, _ := img.At(x, y).RGBA(); r < 0xc000 || g < 0xc000 || b < 0xc000 {
				dark = append(dark, [2]int{x, y})
			}
		}
	}
	return dark, got.Width, got.Height
}

// lines up holds what a page's text says to what its rendering shows:
// every mark on the rendering lies in a word, a rectangle or a drawing,
// and each of those has a mark in it.
func linesUp(t *testing.T, name string, pdf []byte) reader.PageText {
	t.Helper()
	got := textOf(t, pdf, 1)
	dark, width, height := ink(t, pdf)
	if !near(got.Width, float64(width), 1) || !near(got.Height, float64(height), 1) {
		t.Errorf("%s: the page is %.1f by %.1f points, and renders to %d by %d at a pixel a point", name, got.Width, got.Height, width, height)
	}
	var boxes []reader.Rect
	for _, w := range got.Words {
		boxes = append(boxes, w.Box)
	}
	boxes = append(append(boxes, got.Rects...), got.Drawings...)
	if len(boxes) == 0 || len(dark) == 0 {
		t.Fatalf("%s: %d boxes and %d marks", name, len(boxes), len(dark))
	}
	marked := make([]bool, len(boxes))
	for _, at := range dark {
		x, y := float64(at[0])+0.5, float64(at[1])+0.5
		in := false
		for i, b := range boxes {
			if x >= b.X0-1.5 && x <= b.X1+1.5 && y >= b.Y0-1.5 && y <= b.Y1+1.5 {
				in, marked[i] = true, true
			}
		}
		if !in {
			t.Errorf("%s: the rendering has a mark at %d, %d, and no word or shape lies there: %+v", name, at[0], at[1], boxes)
			break
		}
	}
	for i, has := range marked {
		if !has {
			t.Errorf("%s: %+v has no mark of the rendering in it", name, boxes[i])
		}
	}
	return got
}

// What a page's text says of a position holds for the page as it is
// rendered, whatever the page's rotation and whichever of its boxes is
// shown.
func TestAPositionLiesWhereTheRenderingShowsIt(t *testing.T) {
	const content = "BT /F1 40 Tf 100 600 Td (Mark) Tj ET 0 0 0 rg 300 200 80 30 re f"
	for name, page := range map[string]string{
		"as it is":             "",
		"turned a quarter":     "/Rotate 90",
		"upside down":          "/Rotate 180",
		"turned 3 quarters":    "/Rotate 270",
		"cropped":              "/CropBox [40 60 500 700]",
		"cropped and turned":   "/CropBox [40 60 500 700] /Rotate 90",
		"a box not at 0":       "",
		"a box not at 0, back": "/Rotate 270",
	} {
		box := letter
		if strings.HasPrefix(name, "a box not at 0") {
			box = "-50 -80 562 712"
		}
		got := linesUp(t, name, typeset(box, page, "", content))
		if len(got.Words) != 1 || len(got.Rects) != 1 || got.Words[0].Text != "Mark" {
			t.Errorf("%s: %+v", name, got)
			continue
		}
		// The word runs along the page only where the page is not turned.
		if turned := strings.Contains(page, "/Rotate"); got.Words[0].Turned != turned {
			t.Errorf("%s: the word is turned: %v", name, got.Words[0].Turned)
		}
	}

	// A page turned a quarter whose text is set to be read once the page
	// is shown: the text runs up the page's own space, and along the page
	// as it is shown.
	got := linesUp(t, "text set for the turned page", typeset(letter, "/Rotate 90", "", "BT /F1 40 Tf 0 1 -1 0 300 100 Tm (Mark) Tj ET"))
	if len(got.Words) != 1 || got.Words[0].Turned || got.Width != 792 || !near(got.Words[0].Size, 40, 0.01) || !near(got.Words[0].Box.X0, 100, 3) || !near(got.Words[0].Baseline, 300, 0.5) {
		t.Errorf("text set for the turned page: %+v", got)
	}
}

// What a page paints beside its text comes as upright rectangles where it
// is made of upright lines, and as the box around it otherwise.
func TestWhatAPagePaintsIsRectanglesAndDrawings(t *testing.T) {
	image := "<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 1 >>\nstream\n\x00\nendstream"
	form := streamOf("/Type /XObject /Subtype /Form /BBox [0 0 100 100] /Matrix [2 0 0 2 0 0]", "0 0 0 rg 0 0 20 10 re f")
	zigzag := "0 0 0 rg 40 40 m"
	for i := range 10 {
		zigzag += fmt.Sprintf(" %d %d l %d %d l", 40+i*4, 44+i*4, 44+i*4, 44+i*4)
	}
	got := linesUp(t, "shapes", typeset(letter, "", "/XObject << /Im1 8 0 R /Fm1 9 0 R >>", strings.Join([]string{
		"1 1 1 rg 0 0 612 792 re f",                                                // a fill in the paper's color: nothing
		"1 1 1 RG 5 w 20 20 m 600 20 l S",                                          // a stroke in the paper's color: nothing
		"0.5 g 100 700 200 50 re f",                                                // a filled rectangle
		"0 0 0 RG 2 w 100 600 m 300 600 l S",                                       // a line, as thick as its stroke
		"0 0 0 RG 1 w 100 500 100 40 re S",                                         // a frame: its 4 sides
		"0 0 0 rg 500 500 m 560 500 l 560 540 l 500 540 l f",                       // a rectangle that was not closed
		"0 0 0 rg 0 0 0 RG 1 w 400 600 60 30 re B",                                 // filled and framed
		"0 0 0 rg 100 400 m 200 450 l 150 380 l f",                                 // a triangle
		"0 0 0 RG 1 w 100 300 m 150 350 200 350 250 300 c S",                       // a curve
		"0 0 0 rg 400 400 m 460 400 l 460 420 l 430 420 l 430 460 l 400 460 l h f", // upright lines around no rectangle
		zigzag + " f",                      // more lines than are read of a path
		"q 300 250 60 20 re W n Q",         // a path a clip is cut with: nothing
		"q 50 0 0 40 400 700 cm /Im1 Do Q", // an image
		"q 1 0 0 1 300 100 cm /Fm1 Do Q",   // a form, placed and scaled
		"0 0 0 rg 700 700 50 50 re f",      // off the page: nothing
		"0 0 0 rg 590 100 80 20 re f",      // half off the page: cut to it
	}, "\n"), image, form))

	wantRects := []reader.Rect{
		{X0: 100, Y0: 42, X1: 300, Y1: 92},
		{X0: 99, Y0: 191, X1: 301, Y1: 193},
		{X0: 99.5, Y0: 291.5, X1: 200.5, Y1: 292.5}, {X0: 199.5, Y0: 251.5, X1: 200.5, Y1: 292.5},
		{X0: 99.5, Y0: 251.5, X1: 200.5, Y1: 252.5}, {X0: 99.5, Y0: 251.5, X1: 100.5, Y1: 292.5},
		{X0: 500, Y0: 252, X1: 560, Y1: 292},
		{X0: 400, Y0: 162, X1: 460, Y1: 192},
		{X0: 399.5, Y0: 191.5, X1: 460.5, Y1: 192.5}, {X0: 459.5, Y0: 161.5, X1: 460.5, Y1: 192.5},
		{X0: 399.5, Y0: 161.5, X1: 460.5, Y1: 162.5}, {X0: 399.5, Y0: 161.5, X1: 400.5, Y1: 192.5},
		{X0: 300, Y0: 672, X1: 340, Y1: 692}, // the form's rectangle, twice its size, where the form was placed
		{X0: 590, Y0: 672, X1: 612, Y1: 692},
	}
	if len(got.Rects) != len(wantRects) {
		t.Fatalf("%d rectangles, want %d: %+v", len(got.Rects), len(wantRects), got.Rects)
	}
	for i, want := range wantRects {
		if !nearRect(got.Rects[i], want, 0.05) {
			t.Errorf("rectangle %d is %+v, want %+v", i, got.Rects[i], want)
		}
	}
	wantDrawings := []reader.Rect{
		{X0: 100, Y0: 342, X1: 200, Y1: 412},
		{X0: 100, Y0: 442, X1: 250, Y1: 492},
		{X0: 400, Y0: 332, X1: 460, Y1: 392},
		{X0: 40, Y0: 712, X1: 80, Y1: 752},
		{X0: 400, Y0: 52, X1: 450, Y1: 92},
	}
	if len(got.Drawings) != len(wantDrawings) {
		t.Fatalf("%d drawings, want %d: %+v", len(got.Drawings), len(wantDrawings), got.Drawings)
	}
	for i, want := range wantDrawings {
		// The box around a stroked curve takes its stroke and its control
		// points in.
		if !nearRect(got.Drawings[i], want, 13) {
			t.Errorf("drawing %d is %+v, want %+v", i, got.Drawings[i], want)
		}
	}
	if len(got.Words) != 0 || got.Partial {
		t.Errorf("the page holds no text: %+v", got.Words)
	}
}

// formsInForms is a page that draws a form that draws a form, depth deep,
// the last of which fills one square.
func formsInForms(depth int) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /XObject << /F 5 0 R >> >> >>",
		streamOf("", "/F Do"),
	}
	for d := 1; d <= depth; d++ {
		if d == depth {
			objects = append(objects, streamOf("/Type /XObject /Subtype /Form /BBox [0 0 612 792]", "0 0 0 rg 100 100 50 50 re f"))
			break
		}
		objects = append(objects, streamOf(fmt.Sprintf("/Type /XObject /Subtype /Form /BBox [0 0 612 792] /Resources << /XObject << /F %d 0 R >> >>", 5+d), "/F Do"))
	}
	return pdfOf(objects...)
}

// A form inside a form is walked to a depth, and past it is one drawing.
func TestFormsAreWalkedToADepth(t *testing.T) {
	square := reader.Rect{X0: 100, Y0: 642, X1: 150, Y1: 692}
	within := textOf(t, formsInForms(maxFormDepth), 1)
	if len(within.Rects) != 1 || len(within.Drawings) != 0 || !nearRect(within.Rects[0], square, 0.05) {
		t.Errorf("%d forms deep: %+v", maxFormDepth, within)
	}
	past := textOf(t, formsInForms(maxFormDepth+2), 1)
	if len(past.Rects) != 0 || len(past.Drawings) != 1 || !nearRect(past.Drawings[0], square, 0.05) || past.Partial {
		t.Errorf("%d forms deep: %+v", maxFormDepth+2, past)
	}
}

// repeated is a page that draws one form many times. The form's content
// is repeated too, so a file of a few kilobytes holds as much as a test
// needs.
func repeated(form string, times int) []byte {
	return pdfOf(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /XObject << /F 5 0 R >> >> >>",
		streamOf("", strings.Repeat("/F Do\n", times)),
		streamOf("/Type /XObject /Subtype /Form /BBox [0 0 612 792] /Resources << /Font << /F1 6 0 R >> >>", form),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
}

// A page that holds more characters than are read of one page comes back
// partial, with no more than the bound copied out of the engine. The
// bounds of these 2 tests are set low: a call into the engine costs 100
// times its price under the race detector, and what is proven is that the
// reading stops at its bound, whatever the bound is.
func TestAPageOfMoreCharactersThanTheBoundIsPartial(t *testing.T) {
	engine.TextChars = 600
	defer func() { engine.TextChars = 0 }()
	// 500 characters to a form, drawn 400 times: 200,000 characters in a
	// file of under 2 kilobytes.
	form := "BT /F1 1 Tf 72 700 Td (" + strings.Repeat("abcd ", 100) + ") Tj ET"
	pdf := repeated(form, 400)
	if len(pdf) > 4096 {
		t.Fatalf("the file is %d bytes", len(pdf))
	}
	got := textOf(t, pdf, 1)
	chars := 0
	for _, w := range got.Words {
		chars += len(w.Text) + 1
	}
	if !got.Partial || chars > 600 || chars < 500 {
		t.Fatalf("partial %v, %d characters in %d words; the bound is 600", got.Partial, chars, len(got.Words))
	}
	// A page under the bound is whole.
	if whole := textOf(t, repeated(form, 1), 1); whole.Partial || len(whole.Words) != 100 {
		t.Errorf("500 characters: partial %v, %d words", whole.Partial, len(whole.Words))
	}
	if defaultTextChars != 50_000 || defaultTextObjects != 50_000 {
		t.Error("the bounds a page's text is read under are 50,000 characters and 50,000 objects")
	}
}

// A page that paints more objects than are visited of one page comes back
// partial, and the walk stops at the bound, inside a form too.
func TestAPageOfMoreObjectsThanTheBoundIsPartial(t *testing.T) {
	engine.TextObjects = 150
	defer func() { engine.TextObjects = 0 }()
	// 100 squares to a form, drawn 600 times: 60,600 objects.
	form := "0 0 0 rg\n" + strings.Repeat("9 9 1 1 re f\n", 100)
	pdf := repeated(form, 600)
	if len(pdf) > 8192 {
		t.Fatalf("the file is %d bytes", len(pdf))
	}
	got := textOf(t, pdf, 1)
	// The 150 objects are 2 forms and 148 squares.
	if !got.Partial || len(got.Rects) != 148 {
		t.Fatalf("partial %v, %d rectangles; the bound is 150 objects", got.Partial, len(got.Rects))
	}
	if whole := textOf(t, repeated(form, 1), 1); whole.Partial || len(whole.Rects) != 100 {
		t.Errorf("101 objects: partial %v, %d rectangles", whole.Partial, len(whole.Rects))
	}
}

// A page written to exhaust the engine fails within the engine's bounds
// when its text is asked for, as it does when it is rendered, and the
// engine reads the next page as if nothing had happened.
func TestThePageWrittenToExhaustTheEngineHasItsTextRefusedWithinTheBounds(t *testing.T) {
	normal := testfixtures.Read(t, testfixtures.TextPDF)
	textOf(t, normal, 1)
	bomb := nestedForms(6)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := engine.TextPDF(context.Background(), bomb, 1)
	runtime.ReadMemStats(&after)
	if fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Fatalf("the page's text was not refused: %v", err)
	}
	const bound = 128 << 20
	if grew := after.TotalAlloc - before.TotalAlloc; grew > bound {
		t.Fatalf("refusing the page allocated %d MiB on the heap, over %d MiB", grew>>20, bound>>20)
	}
	if got := textOf(t, normal, 1); len(got.Words) == 0 {
		t.Fatal("the engine after the refused page reads no text")
	}

	// The reading does not outlive its context, nor the time a page is
	// given.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := engine.TextPDF(ctx, bomb, 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > time.Second {
		t.Fatalf("a reading whose context ended returned after %s with %v", time.Since(began), err)
	}
	engine.Timeout = 100 * time.Millisecond
	defer func() { engine.Timeout = 0 }()
	began = time.Now()
	_, err = engine.TextPDF(context.Background(), bomb, 1)
	if fault.CodeOf(err) != fault.DocumentCorrupt || !strings.Contains(fault.DetailOf(err), "in the time a page is given") || time.Since(began) > time.Second {
		t.Fatalf("a reading past its time returned after %s with %v", time.Since(began), err)
	}
	engine.Timeout = 0
	if got := textOf(t, normal, 1); len(got.Words) == 0 {
		t.Fatal("the engine after 2 stopped readings reads no text")
	}
}

func TestAPagesTextThatCannotBeReadSaysWhy(t *testing.T) {
	pdf := testfixtures.Read(t, testfixtures.TextPDF)
	for name, tc := range map[string]struct {
		data []byte
		page int
		want fault.Code
	}{
		"a page past the end":   {pdf, 3, fault.InvalidPages},
		"page 0":                {pdf, 0, fault.InvalidPages},
		"bytes that are no PDF": {[]byte("%PDF-1.4\nnothing follows"), 1, fault.DocumentCorrupt},
	} {
		if _, err := engine.TextPDF(context.Background(), tc.data, tc.page); fault.CodeOf(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.TextPDF(ctx, pdf, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("a reading that was canceled: %v", err)
	}

	// An engine call that fails ends the reading with its error, and a
	// failure that is not the engine's is not taken for one.
	lost := errors.New("the engine is gone")
	if err := guarded(func() { must(0, lost) }); !errors.Is(err, lost) {
		t.Errorf("a failed call: %v", err)
	}
	if err := guarded(func() { must(1, nil) }); err != nil {
		t.Errorf("a call that did not fail: %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a panic that is no failed call was swallowed")
			}
		}()
		_ = guarded(func() { panic("a bug") })
	}()
}

func TestAFontSaysItsTypeInItsNameAndItsFlags(t *testing.T) {
	for name, want := range map[string][2]bool{
		"Helvetica":                {false, false},
		"ABCDEF+Helvetica-Bold":    {true, false},
		"Times-BoldItalic":         {true, true},
		"Arial-Black":              {true, false},
		"SomeFace-HeavyOblique":    {true, true},
		"TimesNewRomanPS-ItalicMT": {false, true},
		"CMR10":                    {false, false},
		"CMBX12":                   {true, false},
		"CMTI10":                   {false, true},
		"CMSL10":                   {false, true},
		"CMBXTI10":                 {true, true},
		"XYZABC+CMBXSL10":          {true, true},
	} {
		if bold, italic := typeOf(name, 0); bold != want[0] || italic != want[1] {
			t.Errorf("%s: bold %v, italic %v", name, bold, italic)
		}
	}
	if bold, italic := typeOf("F1", flagForceBold|flagItalic); !bold || !italic {
		t.Error("a font whose name says nothing and whose flags say bold and italic")
	}
}

func TestAMatrixMovesAPointAndABox(t *testing.T) {
	turn := matrix{a: 0, b: 1, c: -1, d: 0, e: 10, f: 20} // a quarter turn, then a move
	if x, y := turn.apply(3, 4); x != 6 || y != 23 {
		t.Errorf("a point: %v, %v", x, y)
	}
	double := matrix{a: 2, d: 2}
	if x, y := double.then(turn).apply(3, 4); x != 2 || y != 26 {
		t.Errorf("doubled, then turned: %v, %v", x, y)
	}
	if got := turn.box(0, 0, 2, 1); got != (reader.Rect{X0: 9, Y0: 20, X1: 10, Y1: 22}) {
		t.Errorf("a box: %+v", got)
	}
	if s := (matrix{a: 3, d: 3}).scale(); s != 3 {
		t.Errorf("scale: %v", s)
	}
	if _, ok := rectangle([][2]float64{{0, 0}, {4, 0}, {4, 3}, {2, 3}, {2, 1}, {0, 1}, {0, 0}}); ok {
		t.Error("6 upright lines are no rectangle")
	}
	if _, ok := rectangle([][2]float64{{0, 0}, {4, 0}, {4, 3}, {4, 0}, {0, 0}}); ok {
		t.Error("lines that go back over themselves are no rectangle")
	}
	if !white(colorOf(255, 255, 255, 255)) || !white(colorOf(0, 0, 0, 0)) || white(colorOf(10, 10, 10, 255)) {
		t.Error("white is the paper's color, and what is fully transparent")
	}
}

func colorOf(r, g, b, a uint) structs.FPDF_COLOR { return structs.FPDF_COLOR{R: r, G: g, B: b, A: a} }
