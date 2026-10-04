// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/reader"
)

// sheet is a page's own text in the making: a letter page that a test
// sets type and paints shapes on, the way a file would.
type sheet struct{ reader.PageText }

func letter() *sheet { return &sheet{reader.PageText{Width: 612, Height: 792}} }

// set is how a line of a test is set.
type set struct {
	size                 float64
	bold, hidden, turned bool
}

// type sets a line of words from x on a baseline and returns where it
// ends. A character is half its size wide, a space a quarter, and the
// line of type runs from 0.8 of a size above the baseline to 0.2 below.
func (s *sheet) line(x, baseline float64, as set, text string) float64 {
	for word := range strings.FieldsSeq(text) {
		width := 0.5 * as.size * float64(utf8.RuneCountInString(word))
		s.Words = append(s.Words, reader.Word{
			Text: word, Baseline: baseline, Size: as.size, Bold: as.bold, Hidden: as.hidden, Turned: as.turned,
			Box: reader.Rect{X0: x, Y0: baseline - 0.8*as.size, X1: x + width, Y1: baseline + 0.2*as.size},
		})
		x += width + 0.25*as.size
	}
	return x - 0.25*as.size
}

// prose sets lines of body type one under the other, 14 points apart, and
// returns the baseline after the last.
func (s *sheet) prose(x, baseline float64, lines ...string) float64 {
	for _, text := range lines {
		s.line(x, baseline, body, text)
		baseline += 14
	}
	return baseline
}

// rule paints a line half a point thick.
func (s *sheet) rule(x0, y0, x1, y1 float64) {
	s.Rects = append(s.Rects, reader.Rect{X0: x0 - 0.25, Y0: y0 - 0.25, X1: x1 + 0.25, Y1: y1 + 0.25})
}

// grid rules every line of a table whose lines stand at xs and ys.
func (s *sheet) grid(xs, ys []float64) {
	for _, y := range ys {
		s.rule(xs[0], y, xs[len(xs)-1], y)
	}
	for _, x := range xs {
		s.rule(x, ys[0], x, ys[len(ys)-1])
	}
}

var (
	body  = set{size: 10}
	heavy = set{size: 10, bold: true}
	small = set{size: 8}
)

func mustNew(t *testing.T) *Reader {
	t.Helper()
	r, err := New(Config{Name: "text"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// readSheet reads a sheet and fails the test when the reader declines it.
func readSheet(t *testing.T, s *sheet) []document.Block {
	t.Helper()
	res, err := mustNew(t).ReadPage(context.Background(), reader.Page{Number: 1, Text: &s.PageText})
	if err != nil {
		t.Fatalf("the page was not read: %v", err)
	}
	if !res.TextLayer || res.Model != "" || res.Truncated || res.Usage != (document.Usage{Pages: 1}) {
		t.Fatalf("a page read from its text says so, names no model and counts no token: %+v", res)
	}
	for i, b := range res.Blocks {
		if b.Order != i+1 || b.Ref != "" || b.Box == nil || !b.Box.Valid() {
			t.Fatalf("block %d: order %d, ref %q, box %v", i, b.Order, b.Ref, b.Box)
		}
	}
	return res.Blocks
}

// declined reads a sheet and returns why the reader declined it. It fails
// the test when the reader read the page, and when the error is of
// another class or says anything the page holds.
func declined(t *testing.T, name string, text *reader.PageText) string {
	t.Helper()
	res, err := mustNew(t).ReadPage(context.Background(), reader.Page{Number: 1, Text: text})
	if err == nil {
		t.Fatalf("%s: the page was read: %s", name, show(res.Blocks))
	}
	e, ok := errors.AsType[*reader.Error](err)
	if !ok || e.Class != reader.Refused || e.Detail == "" {
		t.Fatalf("%s: the page is declined with %v, want the class refused", name, err)
	}
	return e.Detail
}

// show writes blocks one to a line, as kind, level and text.
func show(blocks []document.Block) string {
	var b strings.Builder
	for _, block := range blocks {
		fmt.Fprintf(&b, "\n%s", block.Kind)
		if block.Level > 0 {
			fmt.Fprintf(&b, " %d", block.Level)
		}
		fmt.Fprintf(&b, ": %s", block.Text)
	}
	return b.String()
}

func expect(t *testing.T, got []document.Block, want ...string) {
	t.Helper()
	if shown := show(got); shown != "\n"+strings.Join(want, "\n") {
		t.Fatalf("the page reads:%s\n\nwant:\n%s", shown, strings.Join(want, "\n"))
	}
}

// A page of running text is read into its blocks in the order they are
// read in: a title and headings by the size and the weight of their type,
// paragraphs by the space between them, and list items by their marks.
func TestAPageOfProseIsReadIntoItsBlocks(t *testing.T) {
	s := letter()
	s.line(72, 40, small, "A running header")
	s.line(72, 100, set{size: 24, bold: true}, "The harbor survey")
	y := s.prose(72, 140,
		"The first paragraph runs over 3 lines of body type, each set close",
		"under the one before it and beginning where it begins, so that the",
		"lines are one block of text.")
	y = s.prose(72, y+8,
		"A second paragraph follows after a gap. A line that ends in a hy-",
		"phen runs on in the next, and a line that ends in a soft hy\u00ad",
		"phen loses it. A line that ends in a dash -",
		"and one that ends in a number 3-",
		"4 keep their space.")
	s.line(72, y+12, set{size: 14, bold: true}, "1 Method")
	y = s.prose(72, y+34, "Body text under the heading.")
	s.line(72, y+10, heavy, "1.1 A heading by its weight alone")
	y = s.prose(72, y+26, "More body text, then a list:")
	// A bullet a font draws, one the file paints, a number, and a mark of
	// a symbol font, which has no Unicode of its own.
	s.line(84, y+4, body, "\u2022 An item with a bullet that")
	s.line(93, y+18, body, "runs over 2 lines.")
	s.Drawings = append(s.Drawings, reader.Rect{X0: 86, Y0: y + 27, X1: 89, Y1: y + 30})
	s.line(93, y+32, body, "An item whose bullet the file paints.")
	s.line(84, y+46, body, "2. A numbered item.")
	s.line(84, y+60, body, "\uf0b7 An item of a symbol font.")
	// A line that begins where the item's mark does, and stands farther
	// under the item than the item stands under the one before it, is no
	// line of the item.
	s.line(84, y+78, body, "A paragraph set where the marks are.")
	s.prose(72, y+100, "A paragraph after the list")
	s.line(96, y+128, body, "An indented line begins a paragraph of its own,")
	s.line(72, y+142, body, "and its second line begins at the margin.")
	s.line(303, 760, small, "7")

	got := readSheet(t, s)
	expect(t, got,
		"text: A running header",
		"title 1: The harbor survey",
		"text: The first paragraph runs over 3 lines of body type, each set close under the one before it and beginning where it begins, so that the lines are one block of text.",
		"text: A second paragraph follows after a gap. A line that ends in a hy-phen runs on in the next, and a line that ends in a soft hyphen loses it. A line that ends in a dash - and one that ends in a number 3- 4 keep their space.",
		"heading 2: 1 Method",
		"text: Body text under the heading.",
		"heading 3: 1.1 A heading by its weight alone",
		"text: More body text, then a list:",
		"list_item: An item with a bullet that runs over 2 lines.",
		"list_item: An item whose bullet the file paints.",
		"list_item: A numbered item.",
		"list_item: An item of a symbol font.",
		"text: A paragraph set where the marks are.",
		"text: A paragraph after the list",
		"text: An indented line begins a paragraph of its own, and its second line begins at the margin.",
		"text: 7",
	)
	// A block's box is the box around its lines, as shares of the page.
	title := got[1].Box
	if want := (document.Box{72.0 / 612, (100 - 0.8*24) / 792, (72 + 0.5*24*15 + 0.25*24*2) / 612, (100 + 0.2*24) / 792}); !closeBox(*title, want) {
		t.Errorf("the title lies at %v, want %v", *title, want)
	}
	first := got[2].Box
	if first[1] >= first[3] || !near(first[1], (140-8)/792.0) || !near(first[3], (168+2)/792.0) || !near(first[0], 72/612.0) {
		t.Errorf("the first paragraph lies at %v", *first)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func closeBox(a, b document.Box) bool {
	for i := range a {
		if !near(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Where no space parts 2 paragraphs, the indent of a first line does, and
// a line that does not begin under a list item's text ends the item.
func TestAnIndentBeginsAParagraph(t *testing.T) {
	s := letter()
	y := s.prose(72, 100, "A paragraph of 2 lines is set flush left,", "and it ends here.")
	s.line(90, y, body, "The next begins with an indent,")
	s.line(72, y+14, body, "at the pitch of the lines before it.")
	s.line(72, y+50, body, "One line,")
	s.line(90, y+64, body, "then one indented against it.")
	s.line(72, y+100, body, "3. An item of a list")
	s.line(86, y+114, body, "that hangs under its text,")
	s.line(40, y+128, body, "and a line left of it,")
	s.line(84, y+164, body, "4. Another item,")
	s.line(200, y+178, body, "and a line far right of it.")
	expect(t, readSheet(t, s),
		"text: A paragraph of 2 lines is set flush left, and it ends here.",
		"text: The next begins with an indent, at the pitch of the lines before it.",
		"text: One line,",
		"text: then one indented against it.",
		"list_item: An item of a list that hangs under its text,",
		"text: and a line left of it,",
		"list_item: Another item,",
		"text: and a line far right of it.",
	)
}

// A page whose body is bold has no heading by weight, a block of many
// lines in bold is text, 2 blocks at the page's largest size are headings
// and no title, and a mark raised or lowered beside a line belongs to it.
func TestHeadingsAndMarks(t *testing.T) {
	s := letter()
	s.line(72, 100, set{size: 18}, "Part one")
	s.prose(72, 130, "Body text of the first part.")
	s.line(72, 180, set{size: 18}, "Part two")
	y := 210.0
	for _, text := range []string{"A warning set in bold", "that runs over 4 lines", "is a paragraph of text", "and no heading."} {
		s.line(72, y, heavy, text)
		y += 14
	}
	// A note's mark, raised and small, and a lowered index.
	end := s.line(72, 300, body, "A sentence with a note")
	s.line(end, 296, set{size: 6}, "1")
	end = s.line(end+12, 300, body, "and the water H")
	s.line(end, 302, set{size: 6}, "2")
	s.line(end+4, 300, body, "O.")
	expect(t, readSheet(t, s),
		"heading 1: Part one",
		"text: Body text of the first part.",
		"heading 1: Part two",
		"text: A warning set in bold that runs over 4 lines is a paragraph of text and no heading.",
		"text: A sentence with a note 1 and the water H 2 O.",
	)

	all := letter()
	all.line(72, 100, heavy, "Every line of this page is bold,")
	all.line(72, 114, heavy, "so none of them is a heading.")
	expect(t, readSheet(t, all), "text: Every line of this page is bold, so none of them is a heading.")
}

// table sets a ruled table on a sheet: the lines at xs and ys, and the
// text of each cell, row by row, at the top left of its cell.
func (s *sheet) table(xs, ys []float64, as set, rows ...[]string) {
	s.grid(xs, ys)
	for r, row := range rows {
		for c, text := range row {
			s.line(xs[c]+4, ys[r]+12, as, text)
		}
	}
}

// A table is read where ruling closes every cell: its cells by the lines
// between them, a cell that spans where a line is missing, and its header
// by the weight of its type or the background under it.
func TestARuledTableIsReadCellByCell(t *testing.T) {
	s := letter()
	s.prose(72, 100, "A paragraph above the table.")
	s.line(72, 124, small, "Table 3. Levels by station.")
	xs, ys := []float64{72, 200, 300, 400}, []float64{130, 150, 170, 190, 210}
	// The frame, every line across, and the lines down: the first header
	// cell spans 2 rows and the second 2 columns, so the line under the
	// first and the line between the 2 columns of the second are missing.
	for _, y := range ys {
		x0 := xs[0]
		if y == ys[1] {
			x0 = xs[1]
		}
		s.rule(x0, y, xs[3], y)
	}
	s.rule(xs[0], ys[0], xs[0], ys[4])
	s.rule(xs[1], ys[0], xs[1], ys[4])
	s.rule(xs[2], ys[1], xs[2], ys[4])
	s.rule(xs[3], ys[0], xs[3], ys[4])
	s.line(76, 152, heavy, "Station")
	s.line(204, 142, heavy, "Water level")
	s.line(204, 162, heavy, "Mean")
	s.line(304, 162, heavy, "Spread")
	for r, row := range [][]string{{"A, north <mole>", "212.4", "61.8"}, {"B, fairway", "", "63.2"}} {
		for c, text := range row {
			s.line(xs[c]+4, ys[r+2]+12, body, text)
		}
	}
	// A cell of 2 lines.
	s.line(204, ys[3]+9, small, "not")
	s.line(204, ys[3]+18, small, "read")
	s.prose(72, 240, "A paragraph under the table.")

	got := readSheet(t, s)
	expect(t, got,
		"text: A paragraph above the table.",
		"caption: Table 3. Levels by station.",
		"table: Station | Water level\nMean | Spread\nA, north <mole> | 212.4 | 61.8\nB, fairway | not read | 63.2",
		"text: A paragraph under the table.",
	)
	table := got[2].Table
	if table == nil || table.Rows != 4 || table.Cols != 3 || len(table.Cells) != 10 {
		t.Fatalf("the table: %+v", table)
	}
	want := []document.Cell{
		{Row: 0, Col: 0, RowSpan: 2, Header: true, Text: "Station"},
		{Row: 0, Col: 1, ColSpan: 2, Header: true, Text: "Water level"},
		{Row: 1, Col: 1, Header: true, Text: "Mean"},
		{Row: 1, Col: 2, Header: true, Text: "Spread"},
		{Row: 2, Col: 0, Text: "A, north <mole>"},
	}
	for i, w := range want {
		if table.Cells[i] != w {
			t.Errorf("cell %d is %+v, want %+v", i, table.Cells[i], w)
		}
	}
	if !strings.Contains(table.HTML, "<td>A, north &lt;mole&gt;</td>") {
		t.Errorf("a cell's text is written as text: %s", table.HTML)
	}
	if box := *got[2].Box; !closeBox(box, document.Box{72.0 / 612, 130.0 / 792, 400.0 / 612, 210.0 / 792}) {
		t.Errorf("the table lies at %v", box)
	}

	// A header by its background, where every cell of the first row lies
	// on a painted area and no cell of the others does.
	shaded := letter()
	shaded.table([]float64{72, 200, 300}, []float64{100, 120, 140, 160}, body, []string{"Week", "Level"}, []string{"1", "211.8"}, []string{"2", "212.1"})
	shaded.Rects = append(shaded.Rects, reader.Rect{X0: 72, Y0: 100, X1: 200, Y1: 120}, reader.Rect{X0: 200, Y0: 100, X1: 300, Y1: 120})
	cells := readSheet(t, shaded)[0].Table.Cells
	if len(cells) != 6 || !cells[0].Header || !cells[1].Header || cells[2].Header {
		t.Errorf("a header by its background: %+v", cells)
	}

	// A table whose every row is bold has no header, and a frame around a
	// paragraph is no table.
	plain := letter()
	plain.table([]float64{72, 200, 300}, []float64{100, 120, 140}, heavy, []string{"a", "b"}, []string{"c", "d"})
	plain.grid([]float64{72, 400}, []float64{200, 240})
	plain.prose(80, 215, "A paragraph in a frame is a paragraph.")
	got = readSheet(t, plain)
	expect(t, got, "table: a | b\nc | d", "text: A paragraph in a frame is a paragraph.")
	if got[0].Table.Cells[0].Header {
		t.Error("a table that is all bold has a header")
	}

	// A table whose every cell is ruled by itself, side by side, as some
	// files draw one: the pieces of a line are one line.
	pieces := letter()
	for r := range 2 {
		for c := range 2 {
			x, y := 72+100*float64(c), 100+20*float64(r)
			pieces.grid([]float64{x, x + 100}, []float64{y, y + 20})
			pieces.line(x+4, y+12, body, fmt.Sprintf("cell %d%d", r, c))
		}
	}
	// 2 words set at one place are both kept.
	pieces.line(76, 112, body, "twice")
	expect(t, readSheet(t, pieces), "table: cell twice 00 | cell 01\ncell 10 | cell 11")

	// A stub of a line that reaches no cell's side divides nothing: the
	// row it would begin is not counted.
	tall := letter()
	tall.rule(72, 100, 200, 100)
	tall.rule(72, 140, 200, 140)
	tall.rule(72, 100, 72, 140)
	tall.rule(136, 100, 136, 140)
	tall.rule(200, 100, 200, 140)
	tall.rule(72, 120, 72.5, 120)
	tall.line(76, 112, body, "left")
	tall.line(140, 112, body, "right")
	if table := readSheet(t, tall)[0].Table; table.Rows != 1 || table.Cols != 2 {
		t.Errorf("2 cells side by side: %+v", table)
	}
}

// Ruling that does not close, a word across a line, and more lines than
// are read of a page are each declined: the cells would be a guess.
func TestRulingThatIsNoTableIsDeclined(t *testing.T) {
	for name, tc := range map[string]struct {
		build func(*sheet)
		why   string
	}{
		"a grid with one side open": {func(s *sheet) {
			s.rule(72, 100, 300, 100)
			s.rule(72, 140, 300, 140)
			s.rule(72, 100, 72, 140)
			s.rule(186, 100, 186, 140)
			s.line(76, 112, body, "open on the right")
		}, "does not close into a table"},
		"a grid with no top": {func(s *sheet) {
			s.rule(72, 140, 300, 140)
			s.rule(72, 120, 300, 120)
			s.rule(72, 100, 72, 140)
			s.rule(300, 100, 300, 140)
			s.line(76, 112, body, "no line above")
		}, "does not close into a table"},
		"a grid whose top stops short": {func(s *sheet) {
			s.rule(72, 100, 186, 100)
			s.rule(72, 140, 300, 140)
			s.rule(72, 100, 72, 140)
			s.rule(186, 100, 186, 140)
			s.rule(300, 120, 300, 140)
			s.rule(186, 120, 300, 120)
			s.line(76, 112, body, "a corner cut out")
		}, "does not close into a table"},
		"a grid whose side stops short": {func(s *sheet) {
			s.rule(72, 100, 300, 100)
			s.rule(72, 140, 300, 140)
			s.rule(72, 100, 72, 140)
			s.rule(186, 100, 186, 140)
			s.rule(300, 100, 300, 118)
			s.line(76, 112, body, "a side half ruled")
		}, "does not close into a table"},
		"cells that join around a corner": {func(s *sheet) {
			// A grid of 2 by 2 whose lines inside stop short, so that 3 of
			// its squares join and the fourth is cut off.
			s.grid([]float64{72, 300}, []float64{100, 180})
			s.rule(186, 140, 300, 140)
			s.rule(186, 140, 186, 180)
			s.rule(72, 140, 80, 140)
			s.rule(186, 100, 186, 104)
			s.line(76, 112, body, "an L")
		}, "does not close into a table"},
		"a word across a line": {func(s *sheet) {
			s.table([]float64{72, 120, 300}, []float64{100, 120, 140}, body, []string{"Unwrappable"}, []string{"b"})
		}, "lies across the ruling"},
		"more ruling than a page holds": {func(s *sheet) {
			for i := range maxRules + 1 {
				s.rule(72, float64(i%700)+20, 73, float64(i%700)+20)
			}
			s.line(100, 112, body, "words")
		}, "more ruling than is read"},
		"a grid of more squares than a page holds": {func(s *sheet) {
			xs, ys := make([]float64, 202), make([]float64, 102)
			for i := range xs {
				xs[i] = 20 + 2.8*float64(i)
			}
			for i := range ys {
				ys[i] = 20 + 7*float64(i)
			}
			s.grid(xs, ys)
			s.line(100, 780, body, "words")
		}, "more ruling than is read"},
	} {
		s := letter()
		tc.build(s)
		if why := declined(t, name, &s.PageText); !strings.Contains(why, tc.why) {
			t.Errorf("%s: declined because %q", name, why)
		}
	}
}

// chart paints a framed chart on a sheet: 4 bars and their labels.
func (s *sheet) chart(x0, y0, x1, y1 float64) {
	s.grid([]float64{x0, x1}, []float64{y0, y1})
	s.line(x0+8, y0+14, small, "Largest residual")
	for i := range 4 {
		x := x0 + 40 + float64(i)*90
		s.Rects = append(s.Rects, reader.Rect{X0: x, Y0: y1 - 20 - float64(10*i+20), X1: x + 50, Y1: y1 - 20})
		s.line(x+8, y1-6, small, fmt.Sprintf("Station %c", 'A'+i))
	}
	s.rule(x0+30, y1-20, x1-10, y1-20) // the axis: a line that separates nothing
}

// What a page paints beside its text is reported as a figure, with the
// words inside it as its text, and a caption that names it beside it.
func TestWhatAPagePaintsIsAFigure(t *testing.T) {
	s := letter()
	y := s.prose(72, 100, "A paragraph above the figure, wide enough to", "be the page's body text.")
	s.chart(72, y+6, 540, y+126)
	s.line(72, y+140, small, "Figure 1. Largest residual at each station.")
	// An ornament smaller than a figure, and a curve next to a second
	// one, which no frame holds.
	s.Drawings = append(s.Drawings, reader.Rect{X0: 500, Y0: 20, X1: 520, Y1: 40})
	s.Drawings = append(s.Drawings, reader.Rect{X0: 100, Y0: 500, X1: 200, Y1: 560}, reader.Rect{X0: 220, Y0: 500, X1: 300, Y1: 580})
	s.line(120, 530, small, "peak")
	s.prose(72, 620, "A paragraph under both figures.")
	// A note under a separator, in small type, with its mark.
	s.rule(72, 700, 200, 700)
	s.line(72, 712, small, "\u00b9 A note at the foot of the page.")
	s.line(72, 724, small, "It runs over 2 lines.")

	got := readSheet(t, s)
	expect(t, got,
		"text: A paragraph above the figure, wide enough to be the page's body text.",
		"figure: Largest residual Station A Station B Station C Station D",
		"caption: Figure 1. Largest residual at each station.",
		"figure: peak",
		"text: A paragraph under both figures.",
		"footnote: \u00b9 A note at the foot of the page. It runs over 2 lines.",
	)
	if box := *got[1].Box; !closeBox(box, document.Box{72.0 / 612, (y + 6) / 792, 540.0 / 612, (y + 126) / 792}) {
		t.Errorf("the framed figure lies at %v", box)
	}
	if box := *got[3].Box; !closeBox(box, document.Box{100.0 / 612, 500.0 / 792, 300.0 / 612, 580.0 / 792}) {
		t.Errorf("the figure of 2 drawings lies at %v", box)
	}

	// 2 backgrounds that overlap, each under a drawing and a label, are
	// one figure.
	joined := letter()
	joined.prose(72, 100, "Body text above.")
	joined.Rects = append(joined.Rects, reader.Rect{X0: 72, Y0: 200, X1: 300, Y1: 300}, reader.Rect{X0: 250, Y0: 250, X1: 500, Y1: 400})
	joined.Drawings = append(joined.Drawings, reader.Rect{X0: 100, Y0: 220, X1: 140, Y1: 260}, reader.Rect{X0: 400, Y0: 330, X1: 480, Y1: 390})
	joined.line(80, 214, small, "Inflow")
	joined.line(330, 394, small, "Outflow")
	got = readSheet(t, joined)
	expect(t, got, "text: Body text above.", "figure: Inflow Outflow")
	if box := *got[1].Box; !closeBox(box, document.Box{72.0 / 612, 200.0 / 792, 500.0 / 612, 400.0 / 792}) {
		t.Errorf("the figure on 2 backgrounds lies at %v", box)
	}

	// A figure on a background: the area holds words, so it is no part of
	// the figure, and it is the figure's edge.
	backed := letter()
	backed.prose(72, 100, "Body text above.")
	backed.Rects = append(backed.Rects, reader.Rect{X0: 72, Y0: 200, X1: 400, Y1: 400})
	backed.Drawings = append(backed.Drawings, reader.Rect{X0: 100, Y0: 240, X1: 300, Y1: 380})
	backed.line(80, 214, small, "Depth")
	got = readSheet(t, backed)
	expect(t, got, "text: Body text above.", "figure: Depth")
	if box := *got[1].Box; !closeBox(box, document.Box{72.0 / 612, 200.0 / 792, 400.0 / 612, 400.0 / 792}) {
		t.Errorf("the figure on a background lies at %v", box)
	}

	// Small type under a separator that begins with no note's mark, small
	// type with a mark and no separator, and a line that names no table
	// beside one are text.
	plain := letter()
	plain.prose(72, 72, "Body text, in 3 lines, with characters enough to be", "the type that most of the characters of the page", "are set in.")
	plain.table([]float64{72, 200, 300}, []float64{130, 150, 170}, body, []string{"a", "b"}, []string{"c", "d"})
	plain.line(72, 182, small, "Source: the harbor office.")
	plain.line(72, 300, small, "1 Small type that begins with a number.")
	plain.rule(72, 700, 200, 700)
	plain.line(72, 712, small, "Printed on both sides.")
	plain.line(300, 740, small, "2 of 9")
	expect(t, readSheet(t, plain),
		"text: Body text, in 3 lines, with characters enough to be the type that most of the characters of the page are set in.",
		"table: a | b\nc | d",
		"text: Source: the harbor office.",
		"text: 1 Small type that begins with a number.",
		"text: Printed on both sides.",
		"text: 2 of 9",
	)

	// A line that names a table and stands under none is text.
	off := letter()
	off.prose(72, 60, "Body text, in 2 lines, with characters enough", "to be the body.")
	off.table([]float64{320, 430, 540}, []float64{100, 120, 140}, body, []string{"a", "b"}, []string{"c", "d"})
	off.line(72, 150, small, "Table 9.")
	expect(t, readSheet(t, off), "text: Body text, in 2 lines, with characters enough to be the body.", "table: a | b\nc | d", "text: Table 9.")
}

// A page that is mostly picture, a painted region that holds more text
// than a figure does, and a drawing over a table are declined.
func TestAPageThatIsMostlyDrawingIsDeclined(t *testing.T) {
	scan := letter()
	scan.Drawings = append(scan.Drawings, reader.Rect{X0: 0, Y0: 0, X1: 612, Y1: 792})
	scan.prose(72, 100, "A few words that a recognition pass laid over a scan of a page.")
	if why := declined(t, "a scan with text over it", &scan.PageText); !strings.Contains(why, "mostly drawing or image") {
		t.Errorf("a scan: %q", why)
	}

	framed := letter()
	framed.grid([]float64{72, 400}, []float64{100, 160})
	framed.Drawings = append(framed.Drawings, reader.Rect{X0: 380, Y0: 104, X1: 396, Y1: 156})
	y := 114.0
	for range 4 {
		framed.line(76, y, body, "A frame around lines of text and one drawing, which may be a")
		y += 12
	}
	if why := declined(t, "text in a frame with a drawing", &framed.PageText); !strings.Contains(why, "more text than a figure") {
		t.Errorf("framed text: %q", why)
	}

	shapes := letter()
	shapes.prose(72, 100, "A map of 2,001 strokes with a few words on it.")
	for i := range maxShapes + 1 {
		shapes.Drawings = append(shapes.Drawings, reader.Rect{X0: 100, Y0: 200 + float64(i%400), X1: 101, Y1: 201 + float64(i%400)})
	}
	if why := declined(t, "more shapes than a page of text holds", &shapes.PageText); !strings.Contains(why, "mostly drawing or image") {
		t.Errorf("a page of shapes: %q", why)
	}

	// A drawing that reaches into a table, and a mark drawn inside a cell
	// in place of a word.
	for name, drawing := range map[string]reader.Rect{
		"a drawing over a table":     {X0: 250, Y0: 110, X1: 420, Y1: 200},
		"a mark drawn inside a cell": {X0: 240, Y0: 124, X1: 248, Y1: 132},
	} {
		over := letter()
		over.table([]float64{72, 200, 300}, []float64{100, 120, 140}, body, []string{"a", "b"}, []string{"c", ""})
		over.Drawings = append(over.Drawings, drawing)
		if why := declined(t, name, &over.PageText); !strings.Contains(why, "over a table") {
			t.Errorf("%s: %q", name, why)
		}
	}
}

// column sets lines of prose one under the other from x, and returns the
// sheet.
func (s *sheet) column(x, baseline, pitch float64, lines int, text string) {
	for range lines {
		s.line(x, baseline, body, text)
		baseline += pitch
	}
}

const (
	leftLine  = "the left column holds lines of prose that fill"
	rightLine = "the right column holds as many lines of prose"
)

// A page set in columns is read column by column, whether or not the
// lines of the columns stand at the same heights, with what stands above
// the columns before them.
func TestColumnsOfProseAreReadOneAfterTheOther(t *testing.T) {
	for name, shift := range map[string]float64{"lines at the same heights": 0, "lines at other heights": 5} {
		s := letter()
		s.line(72, 80, set{size: 14, bold: true}, "A heading across both columns of the page here")
		s.column(72, 120, 14, 6, leftLine)
		s.column(320, 120+shift, 14, 6, rightLine)
		got := readSheet(t, s)
		expect(t, got,
			"heading 1: A heading across both columns of the page here",
			"text: "+strings.TrimSpace(strings.Repeat(leftLine+" ", 6)),
			"text: "+strings.TrimSpace(strings.Repeat(rightLine+" ", 6)),
		)
		if got[1].Box[2] >= got[2].Box[0] {
			t.Errorf("%s: the columns lie at %v and %v", name, *got[1].Box, *got[2].Box)
		}
	}

	// 3 columns, with a figure standing in the last beside the prose, and
	// the left column running on under the others.
	s := letter()
	s.column(40, 100, 14, 8, "one column of three")
	s.column(230, 100, 14, 5, "the second of three")
	s.column(420, 100, 14, 4, "the last, of 4 lines")
	s.Drawings = append(s.Drawings, reader.Rect{X0: 420, Y0: 160, X1: 560, Y1: 200})
	expect(t, readSheet(t, s),
		"text: "+strings.TrimSpace(strings.Repeat("one column of three ", 8)),
		"text: "+strings.TrimSpace(strings.Repeat("the second of three ", 5)),
		"text: "+strings.TrimSpace(strings.Repeat("the last, of 4 lines ", 4)),
		"figure: ",
	)

	// A figure that stands beside a column of prose is read after it.
	beside := letter()
	beside.column(72, 100, 14, 6, leftLine)
	beside.Drawings = append(beside.Drawings, reader.Rect{X0: 340, Y0: 95, X1: 540, Y1: 180})
	expect(t, readSheet(t, beside), "text: "+strings.TrimSpace(strings.Repeat(leftLine+" ", 6)), "figure: ")

	// One line whose 2 parts stand apart is read from left to right, and
	// lines that only stand at 2 margins, none beside another, are read
	// downward.
	apart := letter()
	apart.line(72, 60, small, "Harbor survey")
	apart.line(480, 60, small, "page 12")
	apart.prose(72, 100, "A paragraph of body text that runs across the whole width of the page, from margin to margin,", "in 2 lines.")
	apart.line(400, 160, body, "Signed on the right,")
	apart.line(72, 190, body, "and dated on the left.")
	expect(t, readSheet(t, apart),
		"text: Harbor survey",
		"text: page 12",
		"text: A paragraph of body text that runs across the whole width of the page, from margin to margin, in 2 lines.",
		"text: Signed on the right,",
		"text: and dated on the left.",
	)
}

// Text that stands side by side and is not columns of prose is the cells
// of a table set without ruling, or cannot be told from one, and is
// declined: a few short lines, ragged ones, or a word or 2 each. So is a
// line that the order of columns would put between them.
func TestTextSideBySideThatIsNoProseIsDeclined(t *testing.T) {
	const aside = "neither columns of prose nor the cells of a ruled table"
	for name, tc := range map[string]struct {
		build func(*sheet)
		why   string
	}{
		"3 rows of cells": {func(s *sheet) {
			for i, row := range [][2]string{{"Station A, north mole", "212.4 centimeters above datum"}, {"Station B", "214.9"}, {"Station C, lock gate", "211.2 centimeters"}} {
				s.line(72, 100+14*float64(i), body, row[0])
				s.line(320, 100+14*float64(i), body, row[1])
			}
		}, aside},
		"a column of names beside prose": {func(s *sheet) {
			for i, name := range []string{"A, north mole of the harbor", "B", "C, lock", "D, south quay", "E", "F, fairway"} {
				s.line(72, 100+14*float64(i), body, name)
			}
			s.column(320, 100, 14, 6, rightLine)
		}, aside},
		"columns of numbers": {func(s *sheet) {
			s.column(72, 100, 14, 6, "212.4")
			s.column(320, 100, 14, 6, "61.8")
		}, aside},
		"a line under both columns": {func(s *sheet) {
			s.column(72, 100, 14, 6, leftLine)
			s.column(320, 100, 14, 6, rightLine)
			s.line(72, 230, body, "See the appendix.")
		}, "may belong under them"},
		"a line above the left column, on the right": {func(s *sheet) {
			s.line(320, 60, body, "A date at the right margin")
			s.column(72, 100, 14, 6, leftLine)
			s.column(320, 100, 14, 6, rightLine)
		}, "may belong under them"},
		"a mark far beside a line": {func(s *sheet) {
			// Small type level with a line and beside none of its words
			// is no mark of the line.
			s.prose(72, 100, "A line of body type,", "and a second.")
			s.line(400, 98, set{size: 6}, "a")
		}, aside},
		"a column of numbers beside the second of 2 columns": {func(s *sheet) {
			// The wide gutter parts 2 sides that each read as prose. The
			// narrow one, inside the right side, parts prose from numbers.
			s.column(72, 100, 14, 6, leftLine)
			s.column(360, 100, 14, 6, "the middle column has prose too")
			s.column(534, 100, 14, 6, "212.4")
		}, aside},
		"a line over another": {func(s *sheet) {
			s.column(72, 100, 14, 5, leftLine)
			s.column(320, 100, 14, 5, rightLine)
			s.line(200, 131, body, "struck across the columns of the page, over their lines")
		}, aside},
	} {
		s := letter()
		tc.build(s)
		if why := declined(t, name, &s.PageText); !strings.Contains(why, tc.why) {
			t.Errorf("%s: declined because %q", name, why)
		}
	}

	// A column that runs on under the other at its own pitch is the
	// column, and is read.
	s := letter()
	s.column(72, 100, 14, 9, leftLine)
	s.column(320, 100, 14, 6, rightLine)
	if got := readSheet(t, s); len(got) != 2 {
		t.Errorf("a column longer than the other: %s", show(got))
	}
}

// A page whose text is not there, is not whole, is not drawn, or is not
// usable as text is declined, each with its reason and nothing of what
// the page holds.
func TestAPageWhoseTextCannotBeReadIsDeclined(t *testing.T) {
	const sentence = "The gauges were read every ten minutes for thirteen weeks."
	page := func(build func(*sheet)) *reader.PageText {
		s := letter()
		build(s)
		return &s.PageText
	}
	for name, tc := range map[string]struct {
		text *reader.PageText
		why  string
	}{
		"a format that carries no text": {nil, "carries no text of its own"},
		"a page with nothing on it":     {page(func(*sheet) {}), "no text that is drawn"},
		"a page that is not whole": {page(func(s *sheet) {
			s.prose(72, 100, sentence)
			s.Partial = true
		}), "more text or more drawing than is read"},
		"text that is in the file and not on the page": {page(func(s *sheet) {
			s.line(72, 100, set{size: 10, hidden: true}, sentence)
			s.Words = append(s.Words, reader.Word{})
		}), "no text that is drawn"},
		"a word drawn at an angle": {page(func(s *sheet) {
			s.prose(72, 100, sentence)
			s.line(300, 400, set{size: 40, turned: true}, "DRAFT")
		}), "drawn at an angle"},
		"a character with no mapping": {page(func(s *sheet) {
			s.prose(72, 100, sentence)
			s.Words[2].Unmapped = 1
		}), "no Unicode mapping"},
		"a control character":            {page(func(s *sheet) { s.prose(72, 100, sentence, "of\u0001ce") }), "no Unicode mapping"},
		"a replacement character":        {page(func(s *sheet) { s.prose(72, 100, sentence, "of\ufffdce") }), "no Unicode mapping"},
		"a character Unicode leaves out": {page(func(s *sheet) { s.prose(72, 100, sentence, "of\ufffece") }), "no Unicode mapping"},
		"private characters in a word":   {page(func(s *sheet) { s.prose(72, 100, sentence, "\uf041\uf042\uf043") }), "no Unicode mapping"},
		"a private character in a line":  {page(func(s *sheet) { s.prose(72, 100, "The gauges \uf0b7 were read.") }), "no Unicode mapping"},
		"a private character alone":      {page(func(s *sheet) { s.prose(72, 100, sentence, "\uf0b7") }), "no Unicode mapping"},
		"text decoded twice": {page(func(s *sheet) {
			s.prose(72, 100, "Les relev\u00c3\u00a9s de la mar\u00c3\u00a9e \u00e2\u20ac\u201c troisi\u00c3\u00a8me trimestre")
		}), "decoded twice"},
		"glyphs that map to symbols": {page(func(s *sheet) { s.prose(72, 100, "!\"#$ %&'( )*+, -./: ;<=> ?@[\\ ]^_` {|}~ ab") }), "not mostly letters"},
		"glyphs that map to the wrong letters": {page(func(s *sheet) {
			s.prose(72, 100, strings.Repeat("Wkh jdxjhv zhuh uhdg hyhub whq plqxwhv iru wklubhhq zhhnv. ", 3))
		}), "do not read as words"},
	} {
		if why := declined(t, name, tc.text); !strings.Contains(why, tc.why) {
			t.Errorf("%s: declined because %q", name, why)
		}
		// The reason is a fixed sentence: it holds nothing the page says.
		if why := declined(t, name, tc.text); strings.Contains(why, "gauges") || strings.Contains(why, "DRAFT") {
			t.Errorf("%s: the reason holds the page's text: %q", name, why)
		}
	}

	// What is not declined: abbreviations and short words say nothing of
	// a page's letters, one pair that a double decoding leaves is a
	// quotation, and a page in another script has no word of the Latin
	// alphabet to judge.
	for name, lines := range map[string][]string{
		"abbreviations":    {"HTTP, SMTP, DNS, NTP, TLS and SSH are spelled in capitals; by, my, try and why are short.", "PDF XML SQL CSV RTF GPS LED USB CPU RAM ROM SSD"},
		"one such pair":    {"A text decoded twice writes \u00c3\u00a9 where an accented e was meant."},
		"another script":   {"\u6f6e\u4f4d\u8ba1 \u6bcf\u5341\u5206\u949f \u8bfb\u6570\u4e00\u6b21", "\u7b2c\u4e09\u5b63\u5ea6 \u5171\u5341\u4e09\u5468"},
		"a page of digits": {"212.4 214.9 211.2 213.6"},
	} {
		s := letter()
		s.prose(72, 100, lines...)
		if got := readSheet(t, s); len(got) != 1 {
			t.Errorf("%s: %s", name, show(got))
		}
	}

	// A reader whose call was canceled says so as a failure that may
	// pass, and declines nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := letter()
	s.prose(72, 100, sentence)
	if _, err := mustNew(t).ReadPage(ctx, reader.Page{Text: &s.PageText}); reader.ClassOf(err) != reader.Retryable || !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled read: %v", err)
	}
}

// The reader asks for the page's own text, describes the image a result
// holds, and names in its version everything that changes what it
// returns.
func TestTheReaderDescribesItself(t *testing.T) {
	r := mustNew(t)
	d := r.Describe()
	if d.Name != "text" || !d.Text || !d.Boxes || d.Version == "" || d.Image != (reader.ImageSpec{DPI: 160, LongEdge: 2048, Format: "png"}) || d.Accepts[0] != "image/png" || len(d.Kinds) != 8 {
		t.Fatalf("the description: %+v", d)
	}
	same, err := New(Config{Name: "another name", Image: reader.ImageSpec{DPI: 160, LongEdge: 2048, Format: "png"}})
	if err != nil || same.Describe().Version != d.Version {
		t.Errorf("2 readers that differ in name alone: %v, %q and %q", err, same.Describe().Version, d.Version)
	}
	versions := map[string]bool{d.Version: true}
	for _, image := range []reader.ImageSpec{{DPI: 72}, {LongEdge: 1024}, {Format: "jpeg"}} {
		other, err := New(Config{Name: "text", Image: image})
		if err != nil || versions[other.Describe().Version] {
			t.Errorf("a reader with image %+v: %v, version %q", image, err, other.Describe().Version)
			continue
		}
		versions[other.Describe().Version] = true
	}
	if jpeg, _ := New(Config{Name: "text", Image: reader.ImageSpec{Format: "jpeg"}}); jpeg.Describe().Accepts[0] != "image/jpeg" {
		t.Error("a reader of JPEG images takes them first")
	}
	// The description is the caller's to change: the reader's own kinds
	// stay as they are.
	d.Kinds[0] = document.KindBarcode
	if r.Describe().Kinds[0] != document.KindTitle {
		t.Error("a description shares its kinds with the reader")
	}
	for name, cfg := range map[string]Config{
		"no name":         {},
		"an image format": {Name: "text", Image: reader.ImageSpec{Format: "webp"}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// Every bound a page is judged by is in the version.
	if len(bounds) != 34 {
		t.Errorf("%d bounds are in the version; a bound added to the reader is added to them", len(bounds))
	}
}
