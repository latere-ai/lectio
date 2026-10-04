// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package quality scores what a parse returned against a truth: the content
// of a file whose every block is known, because whoever made the file wrote
// it down. A score is a number and not an impression, so a bar can be put on
// it and a change that lowers it is seen.
//
// A truth lists, page by page, the blocks in reading order: each block's
// kind, its text, a table's cells, and its box where the file's layout is
// known. 5 measures compare the pages a parse returned with it:
//
//   - CER, the character error rate, is the fewest insertions, deletions and
//     substitutions of one character that turn the found transcription of a
//     page into the truth's, summed over the pages and divided by the
//     truth's characters.
//   - Kinds is the share of the truth's blocks that a found block of the
//     same kind matches.
//   - Cells is the share of the truth's table cells that the matching table
//     holds at the same row and column, with the same spans and the same
//     text.
//   - Order is the share of pairs of neighboring truth blocks, both matched,
//     whose found blocks come in the same order.
//   - Boxes is the share of the truth's blocks with a box whose matching
//     block has a box that overlaps it by at least half, as intersection
//     over union.
//
// A page's transcription is the text of its blocks in reading order joined
// by spaces, a table as its cells row by row, with every run of whitespace
// as one space. Figures and formulas are left out of it: the words printed
// in a figure have no reading order, and a formula's notation has no single
// spelling. Nothing else is folded: case, punctuation and every other
// character count as they are.
//
// A truth block and a found block of the same page match when at least half
// of their characters agree, each block in one match at most and the
// closest pairs first. A figure and a formula have no text to compare, and
// match a found block of the same kind: the one whose box overlaps theirs
// most when both have one, the first one left otherwise.
package quality

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/lectio/document"
)

const (
	// matchSimilarity is how alike the texts of a truth block and a found
	// block must be for the two to match: 1 less the edits between them
	// over the longer one's length. Two different paragraphs of prose agree
	// in about a quarter of their characters, and a paragraph read with a
	// few errors in nearly all of them, so half tells the two apart.
	matchSimilarity = 0.5

	// boxOverlap is the least intersection over union of a truth box and a
	// found box for the found one to count as right. It is the threshold
	// layout detection is commonly scored at.
	boxOverlap = 0.5

	// maxNotes bounds the notes one score carries.
	maxNotes = 40
)

// Scores is how a parse of one file compares with its truth.
type Scores struct {
	// Pages and Blocks are the truth's; Found and Matched count the blocks
	// the parse returned and the truth blocks one of them matches.
	Pages   int `json:"pages"`
	Blocks  int `json:"blocks"`
	Found   int `json:"found_blocks"`
	Matched int `json:"matched_blocks"`

	// Characters is the length of the truth's transcription and Edits the
	// distance of the found one from it; CER is their ratio.
	Characters int     `json:"characters"`
	Edits      int     `json:"edits"`
	CER        float64 `json:"cer"`

	KindsRight int     `json:"kinds_right"`
	Kinds      float64 `json:"kinds"`

	// Cells is nil for a truth with no table.
	CellsTotal int      `json:"cells_total"`
	CellsRight int      `json:"cells_right"`
	Cells      *float64 `json:"cells"`

	Pairs      int     `json:"order_pairs"`
	PairsRight int     `json:"order_pairs_right"`
	Order      float64 `json:"order"`

	// Boxes is nil for a truth with no box.
	BoxesTotal int      `json:"boxes_total"`
	BoxesRight int      `json:"boxes_right"`
	Boxes      *float64 `json:"boxes"`

	// Notes say what was not right, block by block, for whoever reads a
	// miss: at most maxNotes of them.
	Notes []string `json:"notes,omitempty"`
}

// Score compares the pages a parse returned with a truth. The truth's pages
// are numbered from 1 in order, and a found page is compared with the truth
// page of its number. A truth page with no found page is all deletions, and
// a found page the truth does not have is all insertions.
func Score(truth Truth, found []document.Page) Scores {
	s := Scores{Pages: len(truth.Pages)}
	byNumber := map[int][]document.Block{}
	var extra []int
	for _, p := range found {
		byNumber[p.Number] = p.Blocks
		if p.Number < 1 || p.Number > len(truth.Pages) {
			extra = append(extra, p.Number)
		}
	}
	for i, p := range truth.Pages {
		s.page(i+1, p.Blocks, byNumber[i+1])
	}
	slices.Sort(extra)
	for _, n := range extra {
		s.page(n, nil, byNumber[n])
	}

	s.CER = float64(s.Edits) / float64(max(s.Characters, 1))
	s.Kinds, s.Order = share(s.KindsRight, s.Blocks), share(s.PairsRight, s.Pairs)
	if s.CellsTotal > 0 {
		v := share(s.CellsRight, s.CellsTotal)
		s.Cells = &v
	}
	if s.BoxesTotal > 0 {
		v := share(s.BoxesRight, s.BoxesTotal)
		s.Boxes = &v
	}
	return s
}

// share is a ratio that is 1 when there was nothing to get wrong.
func share(right, total int) float64 {
	if total == 0 {
		return 1
	}
	return float64(right) / float64(total)
}

// item is one block as the measures see it: its kind, its text with
// whitespace collapsed, its box and its cells.
type item struct {
	kind  document.Kind
	text  []rune
	box   *document.Box
	cells []document.Cell
}

// texted reports whether a block's text is compared. A figure's and a
// formula's are not.
func texted(k document.Kind) bool {
	return k != document.KindFigure && k != document.KindFormula
}

// squeeze writes every run of whitespace as one space and drops it at the
// ends, which is the one thing the measures fold.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// cellText is a table as its transcription: the cells row by row.
func cellText(cells []document.Cell) string {
	sorted := slices.Clone(cells)
	slices.SortStableFunc(sorted, func(a, b document.Cell) int {
		return cmp.Or(cmp.Compare(a.Row, b.Row), cmp.Compare(a.Col, b.Col))
	})
	texts := make([]string, 0, len(sorted))
	for _, c := range sorted {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, " ")
}

func truthItems(blocks []Block) []item {
	out := make([]item, 0, len(blocks))
	for _, b := range blocks {
		text := b.Text
		if b.Kind == document.KindTable {
			text = cellText(b.Cells)
		}
		out = append(out, item{kind: b.Kind, text: []rune(squeeze(text)), box: b.Box, cells: b.Cells})
	}
	return out
}

func foundItems(blocks []document.Block) []item {
	out := make([]item, 0, len(blocks))
	for _, b := range blocks {
		it := item{kind: b.Kind, box: b.Box}
		text := b.Text
		// A table with cells is compared by its cells, so the marks a
		// block's text puts between them do not count as characters.
		if b.Table != nil && len(b.Table.Cells) > 0 {
			it.cells, text = b.Table.Cells, cellText(b.Table.Cells)
		}
		it.text = []rune(squeeze(text))
		out = append(out, it)
	}
	return out
}

// transcription joins the compared texts of a page's blocks, in order.
func transcription(items []item) []rune {
	var out []rune
	for _, it := range items {
		if !texted(it.kind) || len(it.text) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, ' ')
		}
		out = append(out, it.text...)
	}
	return out
}

// page adds one page's comparison to the scores.
func (s *Scores) page(number int, truthBlocks []Block, foundBlocks []document.Block) {
	truth, found := truthItems(truthBlocks), foundItems(foundBlocks)
	s.Blocks += len(truth)
	s.Found += len(found)

	want, got := transcription(truth), transcription(found)
	s.Characters += len(want)
	s.Edits += distance(want, got)

	matches := match(truth, found)
	previous := -1 // the match of the truth block before this one
	for i, t := range truth {
		ref := document.Ref(number, i+1)
		j := matches[i]
		if t.kind == document.KindTable {
			s.CellsTotal += len(t.cells)
		}
		if t.box != nil {
			s.BoxesTotal++
		}
		if j < 0 {
			s.note("%s %s is not found: %q", ref, t.kind, clip(t.text))
			previous = -1
			continue
		}
		f := found[j]
		s.Matched++
		if f.kind == t.kind {
			s.KindsRight++
		} else {
			s.note("%s %s is found as %s: %q", ref, t.kind, f.kind, clip(t.text))
		}
		if i > 0 && previous >= 0 {
			s.Pairs++
			if previous < j {
				s.PairsRight++
			} else {
				s.note("%s is found before the block it follows", ref)
			}
		}
		previous = j
		if t.kind == document.KindTable {
			s.cells(ref, t.cells, f.cells)
		}
		if t.box != nil {
			if overlap := iou(t.box, f.box); overlap >= boxOverlap {
				s.BoxesRight++
			} else {
				s.note("%s %s: its box overlaps the found one by %.2f", ref, t.kind, overlap)
			}
		}
	}
}

// cells counts the truth's cells that a found table holds as they are.
func (s *Scores) cells(ref string, truth, found []document.Cell) {
	at := map[[2]int]document.Cell{}
	for _, c := range found {
		at[[2]int{c.Row, c.Col}] = c
	}
	for _, want := range truth {
		got, ok := at[[2]int{want.Row, want.Col}]
		switch {
		case !ok:
			s.note("%s cell %d,%d %q is not found", ref, want.Row, want.Col, want.Text)
		case span(got.RowSpan) != span(want.RowSpan) || span(got.ColSpan) != span(want.ColSpan):
			s.note("%s cell %d,%d spans %d by %d and is found spanning %d by %d", ref, want.Row, want.Col,
				span(want.RowSpan), span(want.ColSpan), span(got.RowSpan), span(got.ColSpan))
		case squeeze(got.Text) != squeeze(want.Text):
			s.note("%s cell %d,%d %q is found as %q", ref, want.Row, want.Col, want.Text, got.Text)
		default:
			s.CellsRight++
		}
	}
}

// span reads a cell's span, where 0 and 1 both say the cell does not span.
func span(n int) int { return max(n, 1) }

func (s *Scores) note(format string, args ...any) {
	if len(s.Notes) < maxNotes {
		s.Notes = append(s.Notes, fmt.Sprintf(format, args...))
	}
}

// clip is the start of a text, for a note.
func clip(text []rune) string {
	const most = 60
	if len(text) > most {
		return string(text[:most]) + "..."
	}
	return string(text)
}

// match pairs each truth block with at most one found block and returns,
// for every truth block, the index of its found block or -1.
func match(truth, found []item) []int {
	out := make([]int, len(truth))
	for i := range out {
		out[i] = -1
	}
	taken := make([]bool, len(found))

	type pair struct {
		t, f  int
		alike float64
	}
	var pairs []pair
	for i, t := range truth {
		if !texted(t.kind) {
			continue
		}
		for j, f := range found {
			if !texted(f.kind) {
				continue
			}
			// The edits between two texts are at least the difference of
			// their lengths, so a pair too unequal in length is skipped
			// before its distance is computed.
			short, long := min(len(t.text), len(f.text)), max(len(t.text), len(f.text))
			if long == 0 || float64(short) < matchSimilarity*float64(long) {
				continue
			}
			if alike := 1 - float64(distance(t.text, f.text))/float64(long); alike >= matchSimilarity {
				pairs = append(pairs, pair{i, j, alike})
			}
		}
	}
	// The closest pairs first; among equally close ones, the pair nearest
	// to the same place in the reading order, so that two blocks with the
	// same text match the found blocks in their order.
	slices.SortStableFunc(pairs, func(a, b pair) int {
		return cmp.Or(cmp.Compare(b.alike, a.alike), cmp.Compare(abs(a.t-a.f), abs(b.t-b.f)), cmp.Compare(a.t, b.t), cmp.Compare(a.f, b.f))
	})
	for _, p := range pairs {
		if out[p.t] < 0 && !taken[p.f] {
			out[p.t], taken[p.f] = p.f, true
		}
	}

	for i, t := range truth {
		if texted(t.kind) {
			continue
		}
		best, bestOverlap := -1, -1.0
		for j, f := range found {
			if taken[j] || f.kind != t.kind {
				continue
			}
			if overlap := iou(t.box, f.box); overlap > bestOverlap {
				best, bestOverlap = j, overlap
			}
		}
		if best >= 0 {
			out[i], taken[best] = best, true
		}
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// iou is the intersection of two boxes over their union, 0 when either is
// missing or the two do not meet.
func iou(a, b *document.Box) float64 {
	if a == nil || b == nil {
		return 0
	}
	w := min(a[2], b[2]) - max(a[0], b[0])
	h := min(a[3], b[3]) - max(a[1], b[1])
	if w <= 0 || h <= 0 {
		return 0
	}
	// Two boxes that meet in an area each have one, so the union is not 0.
	inter := w * h
	return inter / ((a[2]-a[0])*(a[3]-a[1]) + (b[2]-b[0])*(b[3]-b[1]) - inter)
}

// distance is the edit distance of two texts: the fewest insertions,
// deletions and substitutions of one character that turn one into the
// other. It keeps 2 rows of the table, so its memory is the shorter text's
// length whatever the longer one's.
func distance(a, b []rune) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}
	previous, current := make([]int, len(b)+1), make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
