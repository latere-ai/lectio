// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"html"
	"math"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lectio/reader"
)

// The bounds ruling is read by. A table is read only where ruling says
// where every cell begins and ends. A table set with white space alone,
// or with lines under its rows and none between its columns, has cells a
// reader would have to guess, and is not read here.
const (
	// maxRule is the thickest, in points, a painted rectangle may be and
	// count as a line of ruling. A hairline is a quarter of a point and a
	// heavy rule 2; a bar of a chart and a cell's background are thicker.
	// Raised, a thin bar is taken for a line and a chart for a table.
	maxRule = 3.0

	// snap is how far apart, in points, 2 positions may lie and be the
	// same line of a table. Borders that meet are drawn a line's
	// thickness apart, so it is half of maxRule. Raised, 2 lines of a
	// table set tight become one; lowered, one line drawn as 2 strokes
	// becomes 2.
	snap = 1.5

	// edgeCover is the share of a cell's side that ruling must cover for
	// the side to be ruled. A side with no line under it joins the 2
	// cells it lies between. Lowered, a dash of ruling splits a merged
	// cell; raised, a line that stops short of a corner joins 2 cells.
	edgeCover = 0.9

	// maxRules bounds the lines of ruling on one page, maxCells the
	// squares one grid of lines makes, and maxShapes the areas and
	// drawings beside them. Finding which lines touch, and which shapes
	// make one figure, compares every pair, so the bounds keep a page
	// that paints 50,000 strokes from costing a worker minutes. A ledger
	// page ruled cell by cell paints a few thousand lines, and a page
	// that paints thousands of shapes is a drawing with words on it.
	// Raised, such a page costs seconds before it is declined or read.
	maxRules  = 4000
	maxCells  = 20000
	maxShapes = 2000
)

type rect = reader.Rect

func area(r rect) float64 { return math.Max(r.Width(), 0) * math.Max(r.Height(), 0) }

// grow is a rectangle with a margin on every side.
func grow(r rect, by float64) rect {
	return rect{X0: r.X0 - by, Y0: r.Y0 - by, X1: r.X1 + by, Y1: r.Y1 + by}
}

// meet is the part 2 rectangles share. Its width or height is negative
// when they share none.
func meet(a, b rect) rect {
	return rect{X0: math.Max(a.X0, b.X0), Y0: math.Max(a.Y0, b.Y0), X1: math.Min(a.X1, b.X1), Y1: math.Min(a.Y1, b.Y1)}
}

// touches reports whether 2 rectangles share a point.
func touches(a, b rect) bool {
	m := meet(a, b)
	return m.X0 <= m.X1 && m.Y0 <= m.Y1
}

// holds reports whether a point lies in a rectangle.
func holds(r rect, x, y float64) bool { return x >= r.X0 && x <= r.X1 && y >= r.Y0 && y <= r.Y1 }

// within reports whether inner lies in outer.
func within(outer, inner rect) bool {
	return inner.X0 >= outer.X0 && inner.Y0 >= outer.Y0 && inner.X1 <= outer.X1 && inner.Y1 <= outer.Y1
}

// around is the rectangle around 2 rectangles.
func around(a, b rect) rect {
	return rect{X0: math.Min(a.X0, b.X0), Y0: math.Min(a.Y0, b.Y0), X1: math.Max(a.X1, b.X1), Y1: math.Max(a.Y1, b.Y1)}
}

func middle(r rect) (x, y float64) { return (r.X0 + r.X1) / 2, (r.Y0 + r.Y1) / 2 }

// sets sorts items into sets of items that were joined, directly or
// through others.
type sets []int

func newSets(n int) sets {
	s := make(sets, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func (s sets) find(i int) int {
	for s[i] != i {
		s[i] = s[s[i]]
		i = s[i]
	}
	return i
}

func (s sets) join(i, j int) { s[s.find(j)] = s.find(i) }

// lists returns the sets, each listing its items in order, in the order
// of their first item.
func (s sets) lists() [][]int {
	at := map[int]int{}
	var out [][]int
	for i := range s {
		root := s.find(i)
		k, seen := at[root]
		if !seen {
			k, at[root] = len(out), len(out)
			out = append(out, nil)
		}
		out[k] = append(out[k], i)
	}
	return out
}

// groups sorts items into sets by asking of every 2 whether they are
// joined.
func groups(n int, joined func(i, j int) bool) [][]int {
	s := newSets(n)
	for i := range n {
		for j := i + 1; j < n; j++ {
			if joined(i, j) {
				s.join(i, j)
			}
		}
	}
	return s.lists()
}

// span is a stretch along one axis.
type span struct{ lo, hi float64 }

// axis reduces positions to the lines they lie on: positions within snap
// of each other are one line, at their mean.
func axis(at []float64) []float64 {
	slices.Sort(at)
	var out []float64
	for i := 0; i < len(at); {
		j, sum := i, 0.0
		for j < len(at) && at[j]-at[i] <= snap {
			sum += at[j]
			j++
		}
		out = append(out, sum/float64(j-i))
		i = j
	}
	return out
}

// covered is how much of the stretch from lo to hi the spans cover. The
// spans are sorted and do not overlap.
func covered(spans []span, lo, hi float64) float64 {
	total := 0.0
	for _, s := range spans {
		total += math.Max(0, math.Min(s.hi, hi)-math.Max(s.lo, lo))
	}
	return total
}

// joinSpans sorts spans and joins the ones that overlap or lie within snap
// of each other.
func joinSpans(spans []span) []span {
	slices.SortFunc(spans, func(a, b span) int {
		switch {
		case a.lo < b.lo:
			return -1
		case a.lo > b.lo:
			return 1
		}
		return 0
	})
	var out []span
	for _, s := range spans {
		if n := len(out); n > 0 && s.lo <= out[n-1].hi+snap {
			out[n-1].hi = math.Max(out[n-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	return out
}

// cell is one cell of a ruled table: the rows and columns of the grid it
// covers, and where it lies.
type cell struct {
	row, col, rows, cols int
	box                  rect
	words                []int
}

// lattice is ruling that closes: a grid of lines whose outer frame is
// whole, and the cells the lines inside it leave.
type lattice struct {
	box   rect
	cells []*cell
	// grid is how many rows and columns the lines make before any are
	// joined.
	rows, cols int
}

// close reads a set of touching lines as a lattice. ok is false for lines
// that make no grid: fewer than 2 across or fewer than 2 down, which is a
// separator, an underline or a mark in a margin. An error is ruling that
// makes a grid and does not close it, which this reader cannot read as a
// table and cannot take for anything else.
func closeLattice(lines []rect) (l *lattice, ok bool, err error) {
	var xs, ys []float64
	for _, r := range lines {
		x, y := middle(r)
		if r.Width() >= r.Height() {
			ys = append(ys, y)
		} else {
			xs = append(xs, x)
		}
	}
	xs, ys = axis(xs), axis(ys)
	if len(xs) < 2 || len(ys) < 2 {
		return nil, false, nil
	}
	// What each line of the grid has of ruling along it.
	across, down := make([][]span, len(ys)), make([][]span, len(xs))
	for _, r := range lines {
		x, y := middle(r)
		if r.Width() >= r.Height() {
			i := nearest(ys, y)
			across[i] = append(across[i], span{r.X0, r.X1})
		} else {
			i := nearest(xs, x)
			down[i] = append(down[i], span{r.Y0, r.Y1})
		}
	}
	for i := range across {
		across[i] = joinSpans(across[i])
	}
	for i := range down {
		down[i] = joinSpans(down[i])
	}
	rows, cols := len(ys)-1, len(xs)-1
	if rows*cols > maxCells {
		return nil, false, decline("the page draws more ruling than is read of one page")
	}
	// A line that runs on past the grid's last line leaves a cell open on
	// that side.
	grid := grow(rect{X0: xs[0], Y0: ys[0], X1: xs[cols], Y1: ys[rows]}, snap)
	for _, r := range lines {
		if !within(grid, r) {
			return nil, false, decline("ruling on the page does not close into a table")
		}
	}
	// ruledBelow reports a line under row r in column c, counting the top
	// of the grid as under row -1; ruledRight a line right of column c in
	// row r.
	ruledBelow := func(r, c int) bool {
		return covered(across[r+1], xs[c], xs[c+1]) >= edgeCover*(xs[c+1]-xs[c])
	}
	ruledRight := func(r, c int) bool {
		return covered(down[c+1], ys[r], ys[r+1]) >= edgeCover*(ys[r+1]-ys[r])
	}
	for c := range cols {
		if !ruledBelow(-1, c) || !ruledBelow(rows-1, c) {
			return nil, false, decline("ruling on the page does not close into a table")
		}
	}
	for r := range rows {
		if !ruledRight(r, -1) || !ruledRight(r, cols-1) {
			return nil, false, decline("ruling on the page does not close into a table")
		}
	}

	// 2 squares of the grid with no line between them are one cell.
	joined := newSets(rows * cols)
	for r := range rows {
		for c := range cols {
			if c+1 < cols && !ruledRight(r, c) {
				joined.join(r*cols+c, r*cols+c+1)
			}
			if r+1 < rows && !ruledBelow(r, c) {
				joined.join(r*cols+c, (r+1)*cols+c)
			}
		}
	}
	l = &lattice{box: rect{X0: xs[0], Y0: ys[0], X1: xs[cols], Y1: ys[rows]}, rows: rows, cols: cols}
	for _, squares := range joined.lists() {
		c := &cell{row: rows, col: cols}
		bottom, right := 0, 0
		for _, s := range squares {
			c.row, c.col = min(c.row, s/cols), min(c.col, s%cols)
			bottom, right = max(bottom, s/cols), max(right, s%cols)
		}
		c.rows, c.cols = bottom-c.row+1, right-c.col+1
		// Squares that join into a shape with a corner cut out are no
		// cell.
		if c.rows*c.cols != len(squares) {
			return nil, false, decline("ruling on the page does not close into a table")
		}
		c.box = rect{X0: xs[c.col], Y0: ys[c.row], X1: xs[c.col+c.cols], Y1: ys[c.row+c.rows]}
		l.cells = append(l.cells, c)
	}

	// A line of the grid that no cell begins at divides nothing: a stub of
	// ruling put it there. The rows and columns are counted without it.
	startsRow, startsCol := make([]int, rows+1), make([]int, cols+1)
	for _, c := range l.cells {
		startsRow[c.row+1], startsCol[c.col+1] = 1, 1
	}
	for i := 1; i <= rows; i++ {
		startsRow[i] += startsRow[i-1]
	}
	for i := 1; i <= cols; i++ {
		startsCol[i] += startsCol[i-1]
	}
	for _, c := range l.cells {
		c.rows, c.row = startsRow[c.row+c.rows]-startsRow[c.row], startsRow[c.row]
		c.cols, c.col = startsCol[c.col+c.cols]-startsCol[c.col], startsCol[c.col]
	}
	l.rows, l.cols = startsRow[rows], startsCol[cols]
	return l, true, nil
}

// nearest is the index of the line a position lies on.
func nearest(lines []float64, at float64) int {
	best := 0
	for i, v := range lines {
		if math.Abs(v-at) < math.Abs(lines[best]-at) {
			best = i
		}
	}
	return best
}

// tables finds the ruling of a page and reads each lattice of more than
// one cell as a table: its words are taken out of the page's flow and
// into its cells. A lattice of one cell is a frame, and a line that
// belongs to no lattice is a separator.
func (p *page) tables() error {
	for _, r := range p.t.Rects {
		if math.Min(r.Width(), r.Height()) <= maxRule {
			p.rules = append(p.rules, r)
		} else {
			p.fills = append(p.fills, r)
		}
	}
	switch {
	case len(p.rules) > maxRules:
		return decline("the page draws more ruling than is read of one page")
	case len(p.fills)+len(p.t.Drawings) > maxShapes:
		return decline("the page is mostly drawing or image, and its text covers little of it")
	}
	for _, set := range groups(len(p.rules), func(i, j int) bool { return touches(grow(p.rules[i], snap), p.rules[j]) }) {
		lines := make([]rect, len(set))
		for i, at := range set {
			lines[i] = p.rules[at]
		}
		l, ok, err := closeLattice(lines)
		switch {
		case err != nil:
			return err
		case !ok:
			p.separators = append(p.separators, lines...)
		case len(l.cells) == 1:
			p.frames = append(p.frames, l.box)
		default:
			if err := p.table(l); err != nil {
				return err
			}
		}
	}
	return nil
}

// table takes the words inside a lattice into its cells and adds the table
// to the page's pieces.
func (p *page) table(l *lattice) error {
	inside := grow(l.box, snap)
	for i, w := range p.words {
		x, y := middle(w.Box)
		if p.taken[i] || !holds(inside, x, y) {
			continue
		}
		var home *cell
		for _, c := range l.cells {
			if holds(c.box, x, y) {
				home = c
				break
			}
		}
		// A word that lies across a line of the table belongs to no one
		// cell.
		if home == nil || w.Box.X0 < home.box.X0-snap || w.Box.X1 > home.box.X1+snap {
			return decline("a word lies across the ruling of a table")
		}
		home.words = append(home.words, i)
		p.taken[i] = true
	}
	slices.SortFunc(l.cells, func(a, b *cell) int {
		if a.row != b.row {
			return a.row - b.row
		}
		return a.col - b.col
	})

	// The leading rows that are set apart from the rest, in bold or on a
	// background, are the table's header.
	marked := make([]bool, l.rows)
	for r := range marked {
		marked[r] = p.headerRow(l, r)
	}
	head := 0
	for head < l.rows && marked[head] {
		head++
	}
	if head == l.rows {
		head = 0
	}

	var markup strings.Builder
	markup.WriteString("<table>")
	row := -1
	for _, c := range l.cells {
		if c.row != row {
			if row >= 0 {
				markup.WriteString("</tr>")
			}
			markup.WriteString("<tr>")
			row = c.row
		}
		tag := "td"
		if c.row < head {
			tag = "th"
		}
		markup.WriteString("<" + tag)
		if c.rows > 1 {
			markup.WriteString(` rowspan="` + strconv.Itoa(c.rows) + `"`)
		}
		if c.cols > 1 {
			markup.WriteString(` colspan="` + strconv.Itoa(c.cols) + `"`)
		}
		markup.WriteString(">" + html.EscapeString(p.textOf(c.words)) + "</" + tag + ">")
	}
	markup.WriteString("</tr></table>")
	p.pieces = append(p.pieces, &piece{box: l.box, raw: &reader.Raw{Label: "table", HTML: markup.String()}})
	p.tableBoxes = append(p.tableBoxes, l.box)
	return nil
}

// headerRow reports whether a row of a table is set apart: every cell that
// begins in it holds words and all of them bold, or every such cell lies
// on a painted background.
func (p *page) headerRow(l *lattice, r int) bool {
	cells, bold, backed := 0, true, true
	for _, c := range l.cells {
		if c.row != r {
			continue
		}
		cells++
		bold = bold && len(c.words) > 0
		for _, i := range c.words {
			bold = bold && p.words[i].Bold
		}
		on := false
		for _, f := range p.fills {
			on = on || area(meet(f, c.box)) >= edgeCover*area(c.box)
		}
		backed = backed && on
	}
	return cells > 0 && (bold || backed)
}

// textOf is the text of some words of the page as it is read: line by
// line, each line from left to right, the lines joined as the lines of a
// paragraph are.
func (p *page) textOf(words []int) string {
	var lines []string
	for _, l := range p.linesOf(words) {
		lines = append(lines, p.lineText(l))
	}
	return joinLines(lines)
}
