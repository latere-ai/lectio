// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package text

import (
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/lectio/reader"
)

// The bounds lines and figures are read by. A length that goes with the
// type is in sizes: multiples of the size of the type it is measured
// beside.
const (
	// lineDrift is how far, in sizes, the baselines of 2 words may differ
	// for the words to stand on one line. Type set on one line shares its
	// baseline to a fraction of a point; a raised or lowered character is
	// a third of a size off. Raised, a raised mark and its line become one
	// line with the mark out of place.
	lineDrift = 0.25

	// smallType is the largest size, as a share of the size of the line
	// beside it, at which a word off that line's baseline is a raised or
	// lowered mark of the line, and not a line of its own.
	smallType = 0.85

	// columnGap is the gap, in sizes, that parts one line into 2 pieces of
	// text. A space between words is a quarter to a third of a size, and
	// a justified line stretches it to about one. Lowered, a loose line of
	// justified prose is cut in 2 and its page declined; raised, 2 cells
	// set close become one sentence.
	columnGap = 2.0

	// bulletSize is the largest a painted mark may be, in sizes, and
	// bulletReach the farthest it may stand before a line's first word,
	// for the mark to be the bullet of a list item.
	bulletSize  = 0.8
	bulletReach = 3.0

	// clusterGap is how far apart, as a share of the page's width, 2
	// painted shapes may lie and be parts of one figure. The bars of a
	// chart stand a bar's width apart. Raised, 2 figures side by side
	// become one; lowered, one chart becomes several.
	clusterGap = 0.1

	// minFigure is the least share of the page a painted region covers to
	// be reported as a figure. Under it the region is an ornament, a mark
	// or an icon, and is left out. Raised, small figures are dropped
	// without a trace; lowered, ornaments are reported as figures.
	minFigure = 0.002

	// maxFigures is the largest share of the page that figures may cover
	// for the page to be read from its text. A page past it is mostly
	// picture: a scan with text laid over it, a full-page chart, a
	// photograph. What matters on such a page is what a model sees.
	// Raised, pages of pictures are returned as an empty figure and a few
	// words; lowered, a page of text with one large chart costs a call.
	maxFigures = 0.4

	// maxFigureText is the largest share of a figure's area that the
	// words inside it may cover. The labels of a chart cover a tenth of
	// it; a frame around a paragraph is half text. Past the bound the
	// region is text with something painted around it, which this reader
	// cannot tell from a figure, and the page is declined.
	maxFigureText = 0.3
)

// page is one page being read.
type page struct {
	t     *reader.PageText
	words []reader.Word
	// taken marks the words that a table or a figure holds, and bullet
	// the words a painted bullet stands before.
	taken, bullet []bool

	// What the page's upright rectangles are: lines of ruling, and areas.
	rules, fills []rect
	// separators are the lines that belong to no table, frames the
	// lattices of one cell, and tableBoxes where the tables lie.
	separators, frames, tableBoxes []rect
	// spent marks the drawings that were bullets.
	spent []bool

	// pieces are the tables and figures, each a finished region.
	pieces []*piece
}

// piece is one thing that takes a place in the reading order: a line of
// text, or a table or a figure.
type piece struct {
	box  rect
	line *line
	raw  *reader.Raw
}

// line is the words of one line of one column, from left to right.
type line struct {
	words    []int
	box      rect
	baseline float64
	size     float64
	bold     bool
	// marked says a bullet stands before the line.
	marked bool
}

// read builds the regions of a page from its text, or says why the page
// is declined.
func read(in reader.Page) ([]reader.Raw, error) {
	t := in.Text
	if t.Partial {
		return nil, decline("the page holds more text or more drawing than is read of one page")
	}
	words, err := usable(t.Words)
	if err != nil {
		return nil, err
	}
	if err := seen(in, words); err != nil {
		return nil, err
	}
	p := &page{t: t, words: words, taken: make([]bool, len(words)), bullet: make([]bool, len(words)), spent: make([]bool, len(t.Drawings))}
	if err := p.tables(); err != nil {
		return nil, err
	}
	p.bullets()
	if err := p.figures(); err != nil {
		return nil, err
	}
	rows, err := p.rows()
	if err != nil {
		return nil, err
	}
	pieces, err := p.flow(rows)
	if err != nil {
		return nil, err
	}
	blocks := p.paragraphs(pieces)
	p.label(blocks)
	return p.raws(blocks), nil
}

// inTable reports whether a rectangle's middle lies in a table.
func (p *page) inTable(r rect) bool {
	x, y := middle(r)
	for _, box := range p.tableBoxes {
		if holds(grow(box, snap), x, y) {
			return true
		}
	}
	return false
}

// bullets finds the small painted marks that stand before a word, which
// is how some files draw the bullet of a list item.
func (p *page) bullets() {
	for d, mark := range p.t.Drawings {
		_, y := middle(mark)
		best, reach := -1, math.Inf(1)
		for i, w := range p.words {
			gap := w.Box.X0 - mark.X1
			if p.taken[i] || y < w.Box.Y0 || y > w.Box.Y1 || gap < -snap || gap >= reach {
				continue
			}
			best, reach = i, gap
		}
		if best < 0 {
			continue
		}
		size := p.words[best].Size
		if mark.Width() <= bulletSize*size && mark.Height() <= bulletSize*size && reach <= bulletReach*size {
			p.bullet[best], p.spent[d] = true, true
		}
	}
}

// figures finds what the page paints that is neither text nor a table,
// and reports each region of it as a figure whose text is the words
// inside it.
func (p *page) figures() error {
	// What a figure is made of: a drawing, and a painted area that holds
	// no word, as a bar of a chart does and a background does not. A
	// drawing inside a table is what a cell holds in place of words, a
	// mark or a picture, and a table of this reader has words alone.
	var parts []rect
	for d, r := range p.t.Drawings {
		switch {
		case p.spent[d]:
		case p.inTable(r):
			return decline("a drawing of the page lies over a table")
		default:
			parts = append(parts, r)
		}
	}
	var backgrounds []rect
	for _, f := range p.fills {
		if p.inTable(f) {
			continue
		}
		empty := true
		for i, w := range p.words {
			x, y := middle(w.Box)
			empty = empty && (p.taken[i] || !holds(f, x, y))
		}
		if empty {
			parts = append(parts, f)
		} else {
			backgrounds = append(backgrounds, f)
		}
	}

	reach := clusterGap * p.t.Width
	var regions []rect
	for _, set := range groups(len(parts), func(i, j int) bool { return touches(grow(parts[i], reach), parts[j]) }) {
		region := parts[set[0]]
		for _, i := range set[1:] {
			region = around(region, parts[i])
		}
		// A frame or a background around the parts is the figure's own
		// edge: the smallest one that holds them all.
		edge, found := rect{}, false
		for _, outer := range append(slices.Clone(p.frames), backgrounds...) {
			if within(grow(outer, snap), region) && (!found || area(outer) < area(edge)) {
				edge, found = outer, true
			}
		}
		if found {
			region = edge
		}
		regions = append(regions, region)
	}

	whole, total := p.t.Width*p.t.Height, 0.0
	var figures []*piece
	for _, set := range groups(len(regions), func(i, j int) bool { return touches(regions[i], regions[j]) }) {
		region := regions[set[0]]
		for _, i := range set[1:] {
			region = around(region, regions[i])
		}
		if area(region) < minFigure*whole {
			continue
		}
		for _, box := range p.tableBoxes {
			if touches(region, box) {
				return decline("a drawing of the page lies over a table")
			}
		}
		total += area(region)
		figures = append(figures, &piece{box: region, raw: &reader.Raw{Label: "figure"}})
	}
	if total > maxFigures*whole {
		return decline("the page is mostly drawing or image, and its text covers little of it")
	}
	for _, f := range figures {
		var inside []int
		lettered := 0.0
		for i, w := range p.words {
			if x, y := middle(w.Box); !p.taken[i] && holds(f.box, x, y) {
				inside = append(inside, i)
				lettered += area(w.Box)
			}
		}
		if lettered > maxFigureText*area(f.box) {
			return decline("a drawn region of the page holds more text than a figure does")
		}
		for _, i := range inside {
			p.taken[i] = true
		}
		f.raw.Text = p.textOf(inside)
		p.pieces = append(p.pieces, f)
	}
	// A line inside a figure is part of it, and separates nothing.
	p.separators = slices.DeleteFunc(p.separators, func(s rect) bool {
		x, y := middle(s)
		return slices.ContainsFunc(figures, func(f *piece) bool { return holds(f.box, x, y) })
	})
	return nil
}

// linesOf puts words into lines: the words that stand on one baseline,
// from left to right, with a raised or lowered mark in the line it is a
// mark of.
func (p *page) linesOf(words []int) []*line {
	sorted := slices.Clone(words)
	slices.SortStableFunc(sorted, func(a, b int) int {
		wa, wb := p.words[a], p.words[b]
		switch {
		case wa.Baseline != wb.Baseline:
			return sign(wa.Baseline - wb.Baseline)
		}
		return sign(wa.Box.X0 - wb.Box.X0)
	})
	var lines []*line
	for _, i := range sorted {
		w := p.words[i]
		if n := len(lines); n > 0 && w.Baseline-lines[n-1].baseline <= lineDrift*math.Min(w.Size, lines[n-1].size) {
			lines[n-1].words = append(lines[n-1].words, i)
			continue
		}
		lines = append(lines, &line{words: []int{i}, baseline: w.Baseline, size: w.Size})
	}
	for _, l := range lines {
		p.measure(l)
	}

	// A line of small type whose baseline lies inside a line of larger
	// type, beside one of its words, is a mark of that line.
	for i := 0; i < len(lines); i++ {
		small := lines[i]
		for _, host := range lines {
			if host == small || small.size > smallType*host.size ||
				small.baseline < host.box.Y0 || small.baseline > host.box.Y1+lineDrift*host.size || !p.beside(small, host) {
				continue
			}
			host.words = append(host.words, small.words...)
			p.measure(host)
			lines = slices.Delete(lines, i, i+1)
			i--
			break
		}
	}
	return lines
}

// beside reports whether a small line begins next to a word of a host
// line, with less than a space between them.
func (p *page) beside(small, host *line) bool {
	for _, i := range host.words {
		w := p.words[i]
		if small.box.X0 <= w.Box.X1+host.size/2 && small.box.X1 >= w.Box.X0-host.size/2 {
			return true
		}
	}
	return false
}

// measure sets what a line is from its words: their order from left to
// right, the box around them, the size most of its characters are set in,
// and whether all of it is bold.
func (p *page) measure(l *line) {
	slices.SortStableFunc(l.words, func(a, b int) int { return sign(p.words[a].Box.X0 - p.words[b].Box.X0) })
	sizes := map[float64]int{}
	l.bold = true
	for n, i := range l.words {
		w := p.words[i]
		if n == 0 {
			l.box = w.Box
		}
		l.box = around(l.box, w.Box)
		l.bold = l.bold && w.Bold
		sizes[w.Size] += utf8.RuneCountInString(w.Text)
	}
	l.size = most(sizes)
	l.marked = p.bullet[l.words[0]]
}

// most is the size the most characters are set in, the larger of 2 that
// hold as many.
func most(sizes map[float64]int) float64 {
	best, count := 0.0, -1
	for size, n := range sizes {
		if n > count || (n == count && size > best) {
			best, count = size, n
		}
	}
	return best
}

func sign(v float64) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	}
	return 0
}

// lineText is the words of a line with a space between each 2.
func (p *page) lineText(l *line) string {
	texts := make([]string, len(l.words))
	for i, at := range l.words {
		texts[i] = p.words[at].Text
	}
	return strings.Join(texts, " ")
}

// joinLines joins the lines of one paragraph. A line that ends in a soft
// hyphen continues in the next with the hyphen gone. A line that ends in
// a hyphen after a letter, where the next begins with a small letter,
// continues with the hyphen kept and no space: whether the hyphen is the
// word's own or the line break's is not known, and dropping it would be a
// guess.
func joinLines(lines []string) string {
	var out strings.Builder
	for i, text := range lines {
		if i == 0 {
			out.WriteString(text)
			continue
		}
		joined := out.String()
		last, size := utf8.DecodeLastRuneInString(joined)
		first, _ := utf8.DecodeRuneInString(text)
		before, _ := utf8.DecodeLastRuneInString(joined[:len(joined)-size])
		switch {
		case last == '\u00ad':
			out.Reset()
			out.WriteString(joined[:len(joined)-size])
		case last == '-' && unicode.IsLetter(before) && unicode.IsLower(first):
		default:
			out.WriteByte(' ')
		}
		out.WriteString(text)
	}
	return out.String()
}

// lonePrivate reports whether a word is one character of the private
// range.
func lonePrivate(word string) bool {
	r, size := utf8.DecodeRuneInString(word)
	return size == len(word) && private(r)
}

// row is the pieces that begin at one height of the page, from left to
// right: the parts of one line of type, or one table or figure.
type row struct {
	pieces []*piece
	top    float64
}

// rows puts what is left of the page's words into lines, cuts each line
// where a gap parts it, and returns the lines with the tables and the
// figures, from the top of the page down.
func (p *page) rows() ([]*row, error) {
	var free []int
	for i := range p.words {
		if !p.taken[i] {
			free = append(free, i)
		}
	}
	var rows []*row
	for _, l := range p.linesOf(free) {
		r := &row{top: l.box.Y0}
		start := 0
		for i := 1; i <= len(l.words); i++ {
			if i < len(l.words) {
				a, b := p.words[l.words[i-1]], p.words[l.words[i]]
				reach := columnGap
				// The mark of a list item stands off from the item's
				// first word, and is no piece of its own.
				if i-1 == start && marker.MatchString(a.Text) {
					reach = math.Max(columnGap, bulletReach)
				}
				if b.Box.X0-a.Box.X1 <= reach*math.Max(a.Size, b.Size) {
					continue
				}
			}
			part := &line{words: l.words[start:i], baseline: l.baseline}
			// One character of the private range before a line is the
			// bullet a symbol font draws. Anywhere else it is a character
			// that means nothing.
			drawn := len(part.words) > 1 && lonePrivate(p.words[part.words[0]].Text)
			if drawn {
				part.words = part.words[1:]
			}
			for _, at := range part.words {
				if lonePrivate(p.words[at].Text) {
					return nil, decline("a character of the page has no Unicode mapping")
				}
			}
			p.measure(part)
			part.marked = part.marked || drawn
			r.pieces = append(r.pieces, &piece{box: part.box, line: part})
			start = i
		}
		rows = append(rows, r)
	}
	for _, pc := range p.pieces {
		rows = append(rows, &row{top: pc.box.Y0, pieces: []*piece{pc}})
	}
	slices.SortStableFunc(rows, func(a, b *row) int { return sign(a.top - b.top) })
	return rows, nil
}
