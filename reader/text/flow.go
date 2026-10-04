// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"math"
	"regexp"
	"slices"
	"unicode/utf8"

	"latere.ai/x/lectio/reader"
)

// The bounds the order of reading and the kinds of blocks are found by.
const (
	// minGutter is the narrowest strip of clear paper, in points, that
	// parts 2 pieces of text standing side by side down a run of lines.
	// The gutter of a page set in columns is 12 points or more. Lowered,
	// the chance alignment of spaces in 2 lines counts as a gutter.
	minGutter = 6.0

	// sideBySide is the share of the lower of 2 pieces that must lie at
	// the height of the other for the 2 to stand side by side, and not
	// one above the other. Lines set tight overlap by a tenth.
	sideBySide = 0.3

	// A side of a page read as a column of prose holds at least
	// minColumnLines lines, at least minFullLines of them reaching
	// fullLine of the widest, with minColumnWords words to a line on
	// average. Cells of a table set without ruling fail one of the 3:
	// they are few, or ragged, or a word or 2 each. Each bound lowered
	// reads more tables as columns, which puts every cell of a column
	// before the next column's; each raised declines more pages of real
	// columns.
	minColumnLines = 4
	minFullLines   = 0.5
	fullLine       = 0.85
	minColumnWords = 3.0

	// maxStray is how far, in line pitches of its column, a line that
	// stands past the end of the other column may lie from the line
	// before it and still be the column running on. Farther, it may be a
	// line under both columns, which is read after them, so the page is
	// declined.
	maxStray = 1.5

	// maxLeading is the largest distance between 2 baselines, in sizes,
	// at which the second line continues the first one's paragraph, and
	// pitchSlack how much a paragraph's later lines may exceed the pitch
	// of its first 2. Print is set at 1.2 to 1.5 sizes, and a paragraph
	// break adds half a line or more.
	maxLeading = 1.9
	pitchSlack = 1.25

	// indent is how far, in sizes, a line may begin from where its
	// paragraph's lines begin and still belong to it. A paragraph's first
	// line is indented by one size or more.
	indent = 1.0

	// headingScale is how much larger than the page's body type a block's
	// type must be to be a heading by its size alone. A block in bold at
	// the body's size or more, of at most headingLines lines, is one by
	// its weight. titleScale is the size at which the largest heading of
	// a page is its title.
	headingScale = 1.15
	headingLines = 3
	titleScale   = 1.6

	// captionReach is how far, in sizes, a caption may stand from the
	// table or the figure it names.
	captionReach = 2.0
)

// bounds is every bound the reader judges by, for its version.
var bounds = [...]float64{
	minLetters, mojibakePairs, maxVowelless, vowelSample,
	maxRule, snap, edgeCover, maxRules, maxCells, maxShapes,
	lineDrift, smallType, columnGap, bulletSize, bulletReach, clusterGap, minFigure, maxFigures, maxFigureText,
	minGutter, sideBySide, minColumnLines, minFullLines, fullLine, minColumnWords, maxStray,
	maxLeading, pitchSlack, indent, headingScale, headingLines, titleScale, captionReach,
}

// marker is the mark of a list item as a word of its own: a bullet, or a
// number of up to 3 digits closed by a period or a parenthesis.
var marker = regexp.MustCompile(`^(?:[-*+\x{2022}\x{00B7}\x{25E6}\x{25AA}\x{2023}\x{2013}]|\(?[0-9]{1,3}[.)]|\p{Co})$`)

// named is how a caption begins: a word and a number, as in "Table 2".
var named = regexp.MustCompile(`^\p{L}{2,}\.?\s?[0-9]+`)

// noted is how a footnote begins: its mark, then its text.
var noted = regexp.MustCompile(`^(?:[0-9\x{00B9}\x{00B2}\x{00B3}\x{2070}-\x{2079}]{1,3}|[*\x{2020}\x{2021}\x{00A7}])\s?\S`)

// block is one region of the page in the making: lines of text that
// belong together, or a table or a figure.
type block struct {
	label string
	level int
	lines []*line
	raw   *reader.Raw
	box   rect

	size   float64
	bold   bool
	marked bool
	// pitch is the distance between the baselines of its first 2 lines.
	pitch float64
}

// flow puts the pieces of rows in the order they are read in. Rows that
// stand one above the other are read downward. Rows that hold pieces side
// by side are read as columns, left before right, when each side reads as
// prose; one line whose parts stand apart is read from left to right; and
// anything else side by side is declined.
func (p *page) flow(rows []*row) ([]*piece, error) {
	var out []*piece
	for len(rows) > 0 {
		n, gutter, split := band(rows)
		head := rows[:n]
		rows = rows[n:]
		if !split || n == 1 {
			for _, r := range head {
				out = append(out, r.pieces...)
			}
			continue
		}
		left, right := divide(head, gutter)
		if !p.prose(left) || !p.prose(right) {
			return nil, decline("text stands side by side on the page, and is neither columns of prose nor the cells of a ruled table")
		}
		if stray(left, right) {
			return nil, decline("a line beside the page's columns may belong under them")
		}
		for _, side := range [][]*row{left, right} {
			pieces, err := p.flow(side)
			if err != nil {
				return nil, err
			}
			out = append(out, pieces...)
		}
	}
	return out, nil
}

// abreast reports whether 2 pieces stand side by side.
func abreast(a, b rect) bool {
	return math.Min(a.Y1, b.Y1)-math.Max(a.Y0, b.Y0) > sideBySide*math.Min(a.Height(), b.Height())
}

// band is the longest run of rows from the first that share their
// gutters: every 2 pieces in it that stand side by side have a strip of
// clear paper between them that no piece of the run enters. It returns
// how many rows the run holds and, when pieces stand side by side in it,
// the widest such strip.
func band(rows []*row) (n int, gutter span, split bool) {
	clear := []span{{math.Inf(-1), math.Inf(1)}}
	var strips []span
	for ; n < len(rows); n++ {
		next := clear
		for _, pc := range rows[n].pieces {
			next = without(next, pc.box.X0, pc.box.X1)
		}
		more, ok := between(rows[:n], rows[n])
		all := append(slices.Clip(strips), more...)
		if n > 0 && (!ok || !parted(next, all)) {
			break
		}
		clear, strips = next, all
	}
	for _, s := range strips {
		for _, c := range clear {
			if g := (span{math.Max(s.lo, c.lo), math.Min(s.hi, c.hi)}); g.hi-g.lo >= minGutter && (!split || g.hi-g.lo > gutter.hi-gutter.lo) {
				gutter, split = g, true
			}
		}
	}
	return n, gutter, split
}

// without takes the stretch from lo to hi out of spans.
func without(spans []span, lo, hi float64) []span {
	out := make([]span, 0, len(spans)+1)
	for _, s := range spans {
		if hi <= s.lo || lo >= s.hi {
			out = append(out, s)
			continue
		}
		if lo > s.lo {
			out = append(out, span{s.lo, lo})
		}
		if hi < s.hi {
			out = append(out, span{hi, s.hi})
		}
	}
	return out
}

// between lists the strips between the pieces of a row, and between each
// of them and every earlier piece it stands beside. ok is false when a
// piece of the row lies over an earlier one.
func between(earlier []*row, r *row) (strips []span, ok bool) {
	for i := 1; i < len(r.pieces); i++ {
		strips = append(strips, span{r.pieces[i-1].box.X1, r.pieces[i].box.X0})
	}
	for _, e := range earlier {
		for _, a := range e.pieces {
			for _, b := range r.pieces {
				switch {
				case !abreast(a.box, b.box):
				case a.box.X1 <= b.box.X0:
					strips = append(strips, span{a.box.X1, b.box.X0})
				case b.box.X1 <= a.box.X0:
					strips = append(strips, span{b.box.X1, a.box.X0})
				default:
					return nil, false
				}
			}
		}
	}
	return strips, true
}

// parted reports whether every strip still holds a stretch of clear paper
// as wide as a gutter.
func parted(clear, strips []span) bool {
	for _, s := range strips {
		if !slices.ContainsFunc(clear, func(c span) bool { return math.Min(s.hi, c.hi)-math.Max(s.lo, c.lo) >= minGutter }) {
			return false
		}
	}
	return true
}

// divide parts the rows of a band at a gutter into the rows left of it
// and the rows right of it.
func divide(rows []*row, gutter span) (left, right []*row) {
	for _, r := range rows {
		l, rt := &row{top: math.Inf(1)}, &row{top: math.Inf(1)}
		for _, pc := range r.pieces {
			side := l
			if pc.box.X0 >= gutter.hi {
				side = rt
			}
			side.pieces, side.top = append(side.pieces, pc), math.Min(side.top, pc.box.Y0)
		}
		if len(l.pieces) > 0 {
			left = append(left, l)
		}
		if len(rt.pieces) > 0 {
			right = append(right, rt)
		}
	}
	return left, right
}

// prose reports whether one side of a gutter reads as a column of prose:
// enough lines, most of them as wide as the column, with words enough to
// be sentences. A side that holds no line of text, only a table or a
// figure, stands beside the prose and passes.
func (p *page) prose(side []*row) bool {
	var widths []float64
	words, widest := 0, 0.0
	for _, r := range side {
		for _, pc := range r.pieces {
			if pc.line != nil {
				widths = append(widths, pc.box.Width())
				words += len(pc.line.words)
				widest = math.Max(widest, pc.box.Width())
			}
		}
	}
	if len(widths) == 0 {
		return true
	}
	full := 0
	for _, w := range widths {
		if w >= fullLine*widest {
			full++
		}
	}
	lines := float64(len(widths))
	return len(widths) >= minColumnLines && float64(full) >= minFullLines*lines && float64(words) >= minColumnWords*lines
}

// stray reports a line that the order of columns may put in the wrong
// place: one on the left that begins below the end of the right side, or
// one on the right that ends above the start of the left side, which lies
// farther from the line next to it in its column than maxStray of the
// column's pitch.
func stray(left, right []*row) bool {
	l, r := extent(left), extent(right)
	for i := 1; i < len(left); i++ {
		if left[i].top >= r.Y1 {
			if left[i].top-left[i-1].top > maxStray*pitch(left) {
				return true
			}
			break
		}
	}
	for i := len(right) - 2; i >= 0; i-- {
		if extent(right[i:i+1]).Y1 <= l.Y0 {
			return right[i+1].top-right[i].top > maxStray*pitch(right)
		}
	}
	return false
}

// extent is the box around the pieces of rows.
func extent(rows []*row) rect {
	box := rows[0].pieces[0].box
	for _, r := range rows {
		for _, pc := range r.pieces {
			box = around(box, pc.box)
		}
	}
	return box
}

// pitch is the distance from one row of a side to the next, as the middle
// one of all such distances.
func pitch(side []*row) float64 {
	steps := make([]float64, 0, len(side))
	for i := 1; i < len(side); i++ {
		steps = append(steps, side[i].top-side[i-1].top)
	}
	slices.Sort(steps)
	return steps[len(steps)/2]
}

// paragraphs joins pieces, in the order they are read in, into blocks. A
// table and a figure are blocks of their own, and a line continues the
// block before it when it is set the same way, close under it, and
// begins where the block's lines begin.
func (p *page) paragraphs(pieces []*piece) []*block {
	var out []*block
	var open *block
	for _, pc := range pieces {
		if pc.raw != nil {
			out, open = append(out, &block{raw: pc.raw, box: pc.box, label: pc.raw.Label}), nil
			continue
		}
		l := pc.line
		if open != nil && p.continues(open, l) {
			open.lines, open.box = append(open.lines, l), around(open.box, l.box)
			continue
		}
		open = &block{lines: []*line{l}, box: l.box, size: l.size, bold: l.bold, marked: l.marked || p.numbered(l)}
		out = append(out, open)
	}
	return out
}

// numbered reports whether a line begins with the mark of a list item.
func (p *page) numbered(l *line) bool {
	return len(l.words) > 1 && marker.MatchString(p.words[l.words[0]].Text)
}

// continues reports whether a line belongs to the block before it.
func (p *page) continues(b *block, l *line) bool {
	first, last := b.lines[0], b.lines[len(b.lines)-1]
	pitch := l.baseline - last.baseline
	switch {
	case l.marked || p.numbered(l):
		return false
	case math.Abs(l.size-b.size) > 0.02*b.size || l.bold != b.bold:
		return false
	case pitch <= 0:
		return false
	case len(b.lines) == 1 && pitch > maxLeading*b.size:
		return false
	case len(b.lines) > 1 && pitch > pitchSlack*b.pitch:
		return false
	}
	off := l.box.X0 - first.box.X0
	switch {
	case b.marked:
		// The lines of a list item hang under its text, right of its
		// mark.
		if off < -indent*b.size || off > bulletReach*b.size {
			return false
		}
	case len(b.lines) == 1:
		// A paragraph's first line may be indented; its second may not be
		// indented against the first.
		if off > indent*b.size {
			return false
		}
	default:
		if math.Abs(l.box.X0-b.lines[1].box.X0) > indent*b.size {
			return false
		}
	}
	if len(b.lines) == 1 {
		b.pitch = pitch
	}
	return true
}

// text is a block's lines as its text.
func (p *page) text(b *block) string {
	lines := make([]string, len(b.lines))
	for i, l := range b.lines {
		lines[i] = p.lineText(l)
	}
	return joinLines(lines)
}

// label gives each block of text its kind. The page's body type is the
// size most of its characters are set in. A block set larger, or in bold
// where the body is not, is a heading, and the largest heading a title
// when it is alone at its size and well above the body. A block that
// begins with a mark is a list item. A block of small type under a
// separator that begins with a note's mark is a footnote, and a block
// that names a table or a figure it stands by is a caption. Everything
// else is text: a running header and a page number too, which are told
// from the pages around them by assembly.
func (p *page) label(blocks []*block) {
	sizes := map[float64]int{}
	chars, bold := 0, 0
	for _, b := range blocks {
		for _, l := range b.lines {
			n := utf8.RuneCountInString(p.lineText(l))
			sizes[l.size] += n
			chars += n
			if l.bold {
				bold += n
			}
		}
	}
	body, bodyBold := most(sizes), 2*bold > chars

	var heads []float64
	for _, b := range blocks {
		if b.raw != nil {
			continue
		}
		words := p.text(b)
		drawn := b.lines[0].marked
		switch {
		case !drawn && (b.size >= headingScale*body || (b.bold && !bodyBold && b.size >= body && len(b.lines) <= headingLines)):
			b.label = "heading"
			if !slices.Contains(heads, b.size) {
				heads = append(heads, b.size)
			}
		case b.marked:
			b.label = "list_item"
		case b.size < body && p.footnote(b, words):
			b.label = "footnote"
		case len(b.lines) <= headingLines && named.MatchString(words) && p.captions(b):
			b.label = "caption"
		default:
			b.label = "text"
		}
	}
	slices.SortFunc(heads, func(a, b float64) int { return sign(b - a) })
	titled := false
	for _, b := range blocks {
		if b.label != "heading" {
			continue
		}
		b.level = min(slices.Index(heads, b.size)+1, 6)
		alone := !slices.ContainsFunc(blocks, func(o *block) bool { return o != b && o.label == "heading" && o.size == b.size })
		if b.level == 1 && !titled && alone && b.size >= titleScale*body {
			b.label, titled = "title", true
		}
	}
}

// footnote reports whether a block of small type is a note: it begins
// with a note's mark, and it stands in the lower half of the page under a
// separator that begins where it does.
func (p *page) footnote(b *block, words string) bool {
	if !noted.MatchString(words) {
		return false
	}
	return slices.ContainsFunc(p.separators, func(s rect) bool {
		return s.Width() > s.Height() && s.Y0 >= p.t.Height/2 && b.box.Y0 >= s.Y0 && math.Abs(b.box.X0-s.X0) <= captionReach*b.size
	})
}

// captions reports whether a block stands directly above or under a table
// or a figure.
func (p *page) captions(b *block) bool {
	return slices.ContainsFunc(p.pieces, func(pc *piece) bool {
		if b.box.X1 <= pc.box.X0 || b.box.X0 >= pc.box.X1 {
			return false
		}
		gap := math.Max(pc.box.Y0-b.box.Y1, b.box.Y0-pc.box.Y1)
		return gap >= -snap && gap <= captionReach*b.size
	})
}

// raws writes the blocks as the regions a reader's result is made of, in
// reading order, with each box in points.
func (p *page) raws(blocks []*block) []reader.Raw {
	out := make([]reader.Raw, 0, len(blocks))
	for i, b := range blocks {
		raw := reader.Raw{Label: b.label, Level: b.level}
		if b.raw != nil {
			raw = *b.raw
		} else {
			raw.Text = p.text(b)
		}
		raw.Order, raw.Box = i+1, []float64{b.box.X0, b.box.Y0, b.box.X1, b.box.Y1}
		out = append(out, raw)
	}
	return out
}
