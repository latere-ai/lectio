// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	pdfium "github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/structs"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/reader"
)

// The bounds on what is read of one page's own text. The engine's memory
// and its time bound what a page can make the engine do. These bound what
// is copied out of the engine onto the heap, and how many calls are made
// into it, for a page the engine itself got through.
const (
	// defaultTextChars bounds the characters read of one page when the
	// renderer names no other bound. A dense page of print holds under
	// 10,000, and a page of the bound is read in under a second. A page
	// past it is one that draws its text many times over, and what is
	// read of it is marked partial.
	defaultTextChars = 50_000

	// defaultTextObjects bounds the painted objects visited on one page,
	// the ones inside a form included. A page of a real document paints a
	// few thousand, and a page of the bound is walked in under 3 seconds.
	// Past the bound the walk stops and the page is marked partial.
	defaultTextObjects = 50_000

	// maxFormDepth bounds how deep forms inside forms are walked. A form
	// deeper than this is reported as one drawing.
	maxFormDepth = 8

	// maxSegments bounds the segments read of one path. A line has 2 and
	// a rectangle 5; a path of more is a shape, and is reported as a
	// drawing with no segment read.
	maxSegments = 16

	// viewScale is how many device units a point is given when the engine
	// is asked where a point of the page lands: its answer is in whole
	// units, so a position is exact to a hundredth of a point.
	viewScale = 100
)

// TextPDF returns what page n of a PDF holds besides its image: its words
// with their positions and their type, and where it paints anything else
// (reader.PageText). It calls no model and renders no bitmap.
//
// The page is read by an engine instance of its own, under the memory and
// the time a render is held to, and what is copied out is bounded: a page
// that holds more than the bounds comes back partial, and one the engine
// cannot get through within its memory and its time fails with
// fault.DocumentCorrupt.
func (p *PDF) TextPDF(ctx context.Context, data []byte, n int) (reader.PageText, error) {
	if err := ctx.Err(); err != nil {
		return reader.PageText{}, err
	}
	out, err := onInstance(ctx, p, func(instance pdfium.Pdfium) (reader.PageText, error) {
		doc, err := open(instance, data)
		if err != nil {
			return reader.PageText{}, err
		}
		defer func() { _, _ = instance.FPDF_CloseDocument(doc) }()

		counted, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
		if err != nil || n < 1 || n > counted.PageCount {
			return reader.PageText{}, fault.New(fault.InvalidPages, "the PDF has no page %d", n)
		}
		e := &extraction{instance: instance, chars: p.TextChars, budget: p.TextObjects}
		if e.chars <= 0 {
			e.chars = defaultTextChars
		}
		if e.budget <= 0 {
			e.budget = defaultTextObjects
		}
		if err := guarded(func() { e.read(doc.Document, n-1) }); err != nil {
			return reader.PageText{}, fault.Wrap(fault.DocumentCorrupt, err, "the text of page %d could not be read: the page is damaged, or it needs more memory than a page is given", n)
		}
		return e.out, nil
	})
	if errors.Is(err, errSlow) {
		return reader.PageText{}, fault.New(fault.DocumentCorrupt, "the text of page %d was not read in the time a page is given", n)
	}
	return out, err
}

// failure is an engine call that failed while a page was read.
type failure struct{ err error }

// must returns what an engine call returned. The calls that read a page
// take nothing a file can make invalid: a handle the engine gave and an
// index under a count it gave. Such a call fails when the engine itself
// has: it was stopped, or it is out of the memory an instance is given.
// The engine does not say which, and either ends the reading of the page,
// so the failure is raised and guarded turns it back into an error.
func must[T any](v T, err error) T {
	if err != nil {
		panic(failure{err})
	}
	return v
}

// guarded runs the reading of a page and returns the engine call that
// failed in it, if one did.
func guarded(read func()) (err error) {
	defer func() {
		switch r := recover().(type) {
		case nil:
		case failure:
			err = r.err
		default:
			panic(r)
		}
	}()
	read()
	return nil
}

// TextPDF returns what page n of a PDF holds besides its image.
func (r *Pages) TextPDF(ctx context.Context, data []byte, n int) (reader.PageText, error) {
	return r.PDF.TextPDF(ctx, data, n)
}

// matrix is an affine map of the plane, as PDF writes one: a point (x, y)
// goes to (a*x + c*y + e, b*x + d*y + f).
type matrix struct{ a, b, c, d, e, f float64 }

func (m matrix) apply(x, y float64) (float64, float64) {
	return m.a*x + m.c*y + m.e, m.b*x + m.d*y + m.f
}

// then is the map that applies m first and next after it.
func (m matrix) then(next matrix) matrix {
	return matrix{
		a: next.a*m.a + next.c*m.b, b: next.b*m.a + next.d*m.b,
		c: next.a*m.c + next.c*m.d, d: next.b*m.c + next.d*m.d,
		e: next.a*m.e + next.c*m.f + next.e, f: next.b*m.e + next.d*m.f + next.f,
	}
}

// scale is how much the map stretches a length, as the root of the area it
// gives a unit square.
func (m matrix) scale() float64 { return math.Sqrt(math.Abs(m.a*m.d - m.b*m.c)) }

func matrixOf(m structs.FPDF_FS_MATRIX) matrix {
	return matrix{a: float64(m.A), b: float64(m.B), c: float64(m.C), d: float64(m.D), e: float64(m.E), f: float64(m.F)}
}

// box is the upright rectangle around the corners (x0, y0) and (x1, y1)
// once m has moved them, and around the 2 other corners of the rectangle
// they span, which a rotation moves elsewhere.
func (m matrix) box(x0, y0, x1, y1 float64) reader.Rect {
	out := reader.Rect{X0: math.Inf(1), Y0: math.Inf(1), X1: math.Inf(-1), Y1: math.Inf(-1)}
	for _, corner := range [4][2]float64{{x0, y0}, {x1, y0}, {x0, y1}, {x1, y1}} {
		x, y := m.apply(corner[0], corner[1])
		out.X0, out.Y0 = math.Min(out.X0, x), math.Min(out.Y0, y)
		out.X1, out.Y1 = math.Max(out.X1, x), math.Max(out.Y1, y)
	}
	return out
}

// extraction reads one page of an open document.
type extraction struct {
	instance pdfium.Pdfium
	page     requests.Page

	// view takes a position in the page's own space, where the file
	// places things, to points from the top left of the page as it is
	// shown.
	view matrix

	// chars is how many characters may be read, and budget how many more
	// painted objects may be visited.
	chars, budget int

	out reader.PageText
}

// read reads the page at an index of a document into out.
func (e *extraction) read(doc references.FPDF_DOCUMENT, index int) {
	in := e.instance
	// The page is loaded once and held, so that no call below loads it
	// again.
	loaded := must(in.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc, Index: index}))
	defer func() { _, _ = in.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: loaded.Page}) }()
	e.page = requests.Page{ByReference: &loaded.Page}

	size := must(in.GetPageSize(&requests.GetPageSize{Page: e.page}))
	e.out.Width, e.out.Height = size.Width, size.Height
	e.findView()
	e.words()
	e.painted()
}

// findView asks the engine where 3 points of the page's own space land on
// the page as it is shown, and takes the map from its answers. The engine
// knows the page's rotation and which of its boxes is shown, and draws
// the page by the same rule, so a position read here lies where the
// rendering of the page shows it.
func (e *extraction) findView() {
	// The device is the page at viewScale units a point, held under the
	// range the engine counts device units in.
	per := math.Min(viewScale, 1e9/math.Max(e.out.Width, e.out.Height))
	w, h := max(1, int(math.Round(e.out.Width*per))), max(1, int(math.Round(e.out.Height*per)))
	const span = 1000
	var at [3][2]float64
	for i, p := range [3][2]float64{{0, 0}, {span, 0}, {0, span}} {
		got := must(e.instance.FPDF_PageToDevice(&requests.FPDF_PageToDevice{
			Page: e.page, SizeX: w, SizeY: h, Rotate: enums.FPDF_PAGE_ROTATION_NONE, PageX: p[0], PageY: p[1],
		}))
		at[i] = [2]float64{float64(got.DeviceX) * e.out.Width / float64(w), float64(got.DeviceY) * e.out.Height / float64(h)}
	}
	e.view = matrix{
		a: (at[1][0] - at[0][0]) / span, b: (at[1][1] - at[0][1]) / span,
		c: (at[2][0] - at[0][0]) / span, d: (at[2][1] - at[0][1]) / span,
		e: at[0][0], f: at[0][1],
	}
}

// word is a word being put together from its characters.
type word struct {
	reader.Word
	text strings.Builder
	// In the page's own space: where the word's first character stands,
	// the direction the word runs in, how far along it the word reaches
	// so far, and the size of its type.
	x, y, ux, uy, reach, own float64
}

// along is how far a point lies from the word's start in the direction
// the word runs in, and across how far to the side of it.
func (w *word) along(x, y float64) float64 { return (x-w.x)*w.ux + (y-w.y)*w.uy }

func (w *word) across(x, y float64) float64 { return math.Abs((x-w.x)*w.uy - (y-w.y)*w.ux) }

// continues reports whether a glyph that begins at (x, y) is drawn beside
// the word's last one: on the word's line, not back before its start, and
// not after a gap a space would not fill.
func (w *word) continues(x, y float64) bool {
	return w.across(x, y) <= 0.8*w.own && w.along(x, y) >= -0.2*w.own && w.along(x, y) <= w.reach+w.own
}

// words reads the page's characters and puts them together into words. A
// word ends at white space, which the engine adds where the file leaves a
// gap and writes none, and where the next character is not drawn beside
// the last one, whether or not the engine put a space between.
func (e *extraction) words() {
	in := e.instance
	text := must(in.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: e.page})).TextPage
	defer func() { _, _ = in.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: text}) }()

	chars := must(in.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: text})).Count
	if chars > e.chars {
		chars, e.out.Partial = e.chars, true
	}

	var w *word
	flush := func() {
		if w == nil {
			return
		}
		// A word the page's crop leaves outside is in the file and not on
		// the page.
		if _, shown := e.onPage(w.Box); shown {
			w.Text = w.text.String()
			e.out.Words = append(e.out.Words, w.Word)
		}
		w = nil
	}
	for i := range chars {
		r := rune(must(in.FPDFText_GetUnicode(&requests.FPDFText_GetUnicode{TextPage: text, Index: i})).Unicode)
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		// The glyph's box. One with no extent, which a mark that takes no
		// room has, lies where the glyph stands.
		glyph := must(in.FPDFText_GetCharBox(&requests.FPDFText_GetCharBox{TextPage: text, Index: i}))
		if w != nil && !w.continues(glyph.Left, glyph.Bottom) {
			flush()
		}
		if w == nil {
			w = e.begin(text, i)
		}
		if r == 0 || r == unicode.ReplacementChar || !utf8.ValidRune(r) ||
			must(in.FPDFText_HasUnicodeMapError(&requests.FPDFText_HasUnicodeMapError{TextPage: text, Index: i})).HasUnicodeMapError {
			r = unicode.ReplacementChar
			w.Unmapped++
		}
		w.text.WriteRune(r)
		w.reach = math.Max(w.reach, math.Max(w.along(glyph.Left, glyph.Bottom), w.along(glyph.Right, glyph.Top)))
		w.Box = union(w.Box, e.view.box(glyph.Left, glyph.Bottom, glyph.Right, glyph.Top))
	}
	flush()
}

// union is the rectangle around 2 rectangles.
func union(a, b reader.Rect) reader.Rect {
	return reader.Rect{X0: math.Min(a.X0, b.X0), Y0: math.Min(a.Y0, b.Y0), X1: math.Max(a.X1, b.X1), Y1: math.Max(a.Y1, b.Y1)}
}

// begin starts a word at character i. What is the same for a word's
// characters is read once, from its first.
func (e *extraction) begin(text references.FPDF_TEXTPAGE, i int) *word {
	in := e.instance
	size := must(in.FPDFText_GetFontSize(&requests.FPDFText_GetFontSize{TextPage: text, Index: i})).FontSize
	placed := matrixOf(must(in.FPDFText_GetMatrix(&requests.FPDFText_GetMatrix{TextPage: text, Index: i})).Matrix)
	origin := must(in.FPDFText_GetCharOrigin(&requests.FPDFText_GetCharOrigin{TextPage: text, Index: i}))
	line := must(in.FPDFText_GetLooseCharBox(&requests.FPDFText_GetLooseCharBox{TextPage: text, Index: i})).Rect

	w := &word{x: origin.X, y: origin.Y, ux: 1}
	_, w.Baseline = e.view.apply(origin.X, origin.Y)
	// The size a font is set at is in the text's own space. The matrix
	// the text is placed with says how large that is on the page, and
	// which way the text runs.
	w.own = size * placed.scale()
	w.Size = w.own * e.view.scale()
	if long := math.Hypot(placed.a, placed.b); long > 0 {
		w.ux, w.uy = placed.a/long, placed.b/long
	}
	// The direction the word runs in on the page as it is shown.
	dx, dy := e.view.a*w.ux+e.view.c*w.uy, e.view.b*w.ux+e.view.d*w.uy
	w.Turned = dx <= 0 || math.Abs(dy) > 0.05*dx

	// The line of type the word stands in, from the font's ascent to its
	// descent: every word of a line begins with the same extent down the
	// page, whatever letters it holds.
	w.Box = e.view.box(float64(line.Left), float64(line.Bottom), float64(line.Right), float64(line.Top))

	// A font the engine cannot name says nothing of its type.
	if font, err := in.FPDFText_GetFontInfo(&requests.FPDFText_GetFontInfo{TextPage: text, Index: i}); err == nil {
		w.Bold, w.Italic = typeOf(font.FontName, font.Flags)
	}
	// A character that belongs to no text object is one the engine put
	// there, and is drawn like the text around it.
	if object, err := in.FPDFText_GetTextObject(&requests.FPDFText_GetTextObject{TextPage: text, Index: i}); err == nil {
		mode := must(in.FPDFTextObj_GetTextRenderMode(&requests.FPDFTextObj_GetTextRenderMode{PageObject: object.TextObject})).TextRenderMode
		w.Hidden = mode == enums.FPDF_TEXTRENDERMODE_INVISIBLE || mode == enums.FPDF_TEXTRENDERMODE_CLIP
	}
	return w
}

// The flags of a font's descriptor that say how it is drawn.
const (
	flagItalic    = 1 << 6
	flagForceBold = 1 << 18
)

// typeOf says whether a font is bold and whether it is italic, from its
// name and its descriptor's flags. A font's weight as a number is not
// read: a file that states none has one derived from the width of the
// font's stems, which puts a regular serif face above a bold sans one.
func typeOf(name string, flags int) (bold, italic bool) {
	// A font embedded in part carries a tag of 6 letters before its name.
	if _, rest, tagged := strings.Cut(name, "+"); tagged {
		name = rest
	}
	name = strings.ToLower(name)
	for _, mark := range []string{"bold", "black", "heavy"} {
		bold = bold || strings.Contains(name, mark)
	}
	for _, mark := range []string{"italic", "oblique"} {
		italic = italic || strings.Contains(name, mark)
	}
	// The faces of the Computer Modern family name their weight and their
	// slant in letters after "cm": cmbx is bold, cmti and cmsl are slanted.
	if face, found := strings.CutPrefix(name, "cm"); found {
		bold = bold || strings.HasPrefix(face, "b")
		face = strings.TrimPrefix(face, "bx")
		italic = italic || strings.HasPrefix(face, "ti") || strings.HasPrefix(face, "sl")
	}
	return bold || flags&flagForceBold != 0, italic || flags&flagItalic != 0
}

// painted walks what the page paints that is not text.
func (e *extraction) painted() {
	count := must(e.instance.FPDFPage_CountObjects(&requests.FPDFPage_CountObjects{Page: e.page})).Count
	for i := 0; i < count && !e.out.Partial; i++ {
		e.object(must(e.instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: e.page, Index: i})).PageObject, e.view, 0)
	}
}

// object reports one painted object. to takes the space the object is
// placed in, the page's own or that of the form it is inside, to the page
// as it is shown.
func (e *extraction) object(object references.FPDF_PAGEOBJECT, to matrix, depth int) {
	if e.budget--; e.budget < 0 {
		e.out.Partial = true
		return
	}
	in := e.instance
	switch must(in.FPDFPageObj_GetType(&requests.FPDFPageObj_GetType{PageObject: object})).Type {
	case enums.FPDF_PAGEOBJ_TEXT:
	case enums.FPDF_PAGEOBJ_PATH:
		e.path(object, to)
	case enums.FPDF_PAGEOBJ_FORM:
		if depth >= maxFormDepth {
			e.drawing(object, to)
			return
		}
		inside := matrixOf(must(in.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{PageObject: object})).Matrix).then(to)
		count := must(in.FPDFFormObj_CountObjects(&requests.FPDFFormObj_CountObjects{PageObject: object})).Count
		for i := 0; i < count && !e.out.Partial; i++ {
			e.object(must(in.FPDFFormObj_GetObject(&requests.FPDFFormObj_GetObject{PageObject: object, Index: uint64(i)})).PageObject, inside, depth+1)
		}
	default:
		// An image, a shading, and a kind of object the engine has no
		// name for.
		e.drawing(object, to)
	}
}

// drawing reports an object as the box around it.
func (e *extraction) drawing(object references.FPDF_PAGEOBJECT, to matrix) {
	bounds := must(e.instance.FPDFPageObj_GetBounds(&requests.FPDFPageObj_GetBounds{PageObject: object}))
	box := to.box(float64(bounds.Left), float64(bounds.Bottom), float64(bounds.Right), float64(bounds.Top))
	if box, ok := e.onPage(box); ok {
		e.out.Drawings = append(e.out.Drawings, box)
	}
}

// onPage cuts a rectangle to the page. ok is false when nothing of it is
// on the page.
func (e *extraction) onPage(r reader.Rect) (reader.Rect, bool) {
	r.X0, r.Y0 = math.Max(r.X0, 0), math.Max(r.Y0, 0)
	r.X1, r.Y1 = math.Min(r.X1, e.out.Width), math.Min(r.Y1, e.out.Height)
	return r, r.X0 <= r.X1 && r.Y0 <= r.Y1 && (r.X0 < r.X1 || r.Y0 < r.Y1)
}

// white reports whether a color shows nothing on paper: it is white, or
// it is fully transparent.
func white(c structs.FPDF_COLOR) bool {
	const near = 250
	return c.A == 0 || (c.R >= near && c.G >= near && c.B >= near)
}

// path reports a path: as upright rectangles when it is made of upright
// lines alone, a filled rectangle as it is and a stroked line as a
// rectangle as thick as its stroke, and as a drawing otherwise.
func (e *extraction) path(object references.FPDF_PAGEOBJECT, to matrix) {
	in := e.instance
	mode := must(in.FPDFPath_GetDrawMode(&requests.FPDFPath_GetDrawMode{PageObject: object}))
	// A fill or a stroke in the paper's color shows nothing. A color the
	// engine cannot state, as that of a pattern, is taken to show.
	fill, noFill := in.FPDFPageObj_GetFillColor(&requests.FPDFPageObj_GetFillColor{PageObject: object})
	stroke, noStroke := in.FPDFPageObj_GetStrokeColor(&requests.FPDFPageObj_GetStrokeColor{PageObject: object})
	filled := mode.FillMode != enums.FPDF_FILLMODE_NONE && (noFill != nil || !white(fill.FillColor))
	stroked := mode.Stroke && (noStroke != nil || !white(stroke.StrokeColor))
	if !filled && !stroked {
		// A path that paints nothing: one a clip is cut with, or one drawn
		// in the paper's color.
		return
	}
	count := must(in.FPDFPath_CountSegments(&requests.FPDFPath_CountSegments{PageObject: object})).Count
	if count > maxSegments {
		e.drawing(object, to)
		return
	}
	onto := matrixOf(must(in.FPDFPageObj_GetMatrix(&requests.FPDFPageObj_GetMatrix{PageObject: object})).Matrix).then(to)

	// The path's runs of lines, each from a move to the next one.
	var runs [][][2]float64
	for i := range count {
		segment := must(in.FPDFPath_GetPathSegment(&requests.FPDFPath_GetPathSegment{PageObject: object, Index: i})).PathSegment
		point := must(in.FPDFPathSegment_GetPoint(&requests.FPDFPathSegment_GetPoint{PathSegment: segment}))
		x, y := onto.apply(float64(point.X), float64(point.Y))
		switch kind := must(in.FPDFPathSegment_GetType(&requests.FPDFPathSegment_GetType{PathSegment: segment})).Type; {
		case kind == enums.FPDF_SEGMENT_MOVETO:
			runs = append(runs, [][2]float64{{x, y}})
		case kind == enums.FPDF_SEGMENT_LINETO && len(runs) > 0:
			runs[len(runs)-1] = append(runs[len(runs)-1], [2]float64{x, y})
		default:
			// A curve, or a line that begins nowhere.
			e.drawing(object, to)
			return
		}
		// A run that is closed ends where it began.
		if run := runs[len(runs)-1]; must(in.FPDFPathSegment_GetClose(&requests.FPDFPathSegment_GetClose{PathSegment: segment})).IsClose && run[0] != run[len(run)-1] {
			runs[len(runs)-1] = append(run, run[0])
		}
	}

	width := 0.0
	if stroked {
		width = float64(must(in.FPDFPageObj_GetStrokeWidth(&requests.FPDFPageObj_GetStrokeWidth{PageObject: object})).StrokeWidth) * onto.scale()
	}
	var rects []reader.Rect
	for _, run := range runs {
		for j := 1; j < len(run); j++ {
			if !upright(run[j-1], run[j]) {
				e.drawing(object, to)
				return
			}
		}
		if filled {
			// Of the shapes upright lines close, a rectangle alone is
			// reported as one.
			area, ok := rectangle(run)
			if !ok {
				e.drawing(object, to)
				return
			}
			rects = append(rects, area)
		}
		for j := 1; stroked && j < len(run); j++ {
			a, b := run[j-1], run[j]
			rects = append(rects, reader.Rect{
				X0: math.Min(a[0], b[0]) - width/2, Y0: math.Min(a[1], b[1]) - width/2,
				X1: math.Max(a[0], b[0]) + width/2, Y1: math.Max(a[1], b[1]) + width/2,
			})
		}
	}
	for _, r := range rects {
		if r, ok := e.onPage(r); ok {
			e.out.Rects = append(e.out.Rects, r)
		}
	}
}

// straight is how far, in points, the 2 ends of a line may differ in the
// direction the line does not run in for the line to be upright. A file
// writes positions with a few decimals, and a rotation of the page adds
// its own rounding.
const straight = 0.05

// upright reports whether the line from a to b runs along the page or
// down it.
func upright(a, b [2]float64) bool {
	return math.Abs(a[0]-b[0]) <= straight || math.Abs(a[1]-b[1]) <= straight
}

// rectangle reports the rectangle a run of upright lines closes: 4 lines
// around an area, with the run back at its start. ok is false for any
// other shape.
func rectangle(run [][2]float64) (r reader.Rect, ok bool) {
	// A run that was not closed is filled as if it were.
	if run[0] != run[len(run)-1] {
		run = append(slices.Clip(run), run[0])
	}
	// 4 upright lines that come back to their start go around a rectangle
	// when each corner lies across from the one 2 lines on.
	across := func(a, b [2]float64) bool {
		return math.Abs(a[0]-b[0]) > straight && math.Abs(a[1]-b[1]) > straight
	}
	if len(run) != 5 || !across(run[0], run[2]) || !across(run[1], run[3]) {
		return r, false
	}
	return reader.Rect{
		X0: math.Min(run[0][0], run[2][0]), Y0: math.Min(run[0][1], run[2][1]),
		X1: math.Max(run[0][0], run[2][0]), Y1: math.Max(run[0][1], run[2][1]),
	}, true
}
