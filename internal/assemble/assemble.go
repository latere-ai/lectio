// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package assemble makes one document out of pages. A reader sees one page
// at a time, so nothing it returns knows about the page before or after;
// this package does. It finds the running headers and footers, joins the
// tables that continue across pages, and builds the outline. Every pass is
// deterministic and calls no model: assembling the same pages twice gives
// the same document. The design is specs/010-assembly.md.
package assemble

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/lectio/document"
)

const (
	// maxRunningText is the longest text, in characters, the header and
	// footer pass looks at. A running header is a line, not a paragraph.
	maxRunningText = 200

	// edgeBand is the share of a page's height, from its top or from its
	// bottom, that a running header, a running footer or a page number
	// lies in.
	edgeBand = 0.12

	// edgeTolerance is how far the left and the right edge of two tables
	// may differ, as a share of the page's width, for the two to be parts
	// of one table.
	edgeTolerance = 0.03
)

// Document assembles pages into a document. It marks the running headers,
// footers and page numbers on the pages it is given and leaves the document
// one title, in place, and returns the index: the pages without their
// blocks, the spans, the outline, and usage.
//
// pages must be in page order. A page that did not succeed is listed and
// takes part in no pass, and a table is not joined across one.
func Document(parse string, pages []document.Page) document.Document {
	doc := document.Document{Parse: parse, Pages: make([]document.PageSummary, 0, len(pages))}
	for _, p := range pages {
		if p.Usage != nil {
			doc.Usage = doc.Usage.Add(*p.Usage)
		}
	}
	running(pages)
	oneTitle(pages)
	doc.Spans = spans(pages)
	doc.Outline = outline(pages)
	for _, p := range pages {
		doc.Pages = append(doc.Pages, p.Summary())
	}
	return doc
}

// running finds the furniture of the pages: page numbers, and the headers
// and footers that run from page to page. A reader is asked to label them
// and does so unevenly, so the pass makes the labeling consistent. It errs
// toward content. A block that is hidden as furniture is lost to whoever
// reads a rendering, and a line of furniture left in the text is only
// noise, so a block becomes furniture on strong evidence and on nothing
// else. Nothing is deleted: blocks are marked and stay where they are.
//
// The pass has three steps.
//
//  1. A page's first or last block whose whole text is a page number, such
//     as "3" or "Page 3 of 40", is a page number.
//  2. A text that repeats, letter for letter, at the same edge on at least
//     half the pages, and on two at least, is a running header or footer.
//     The texts are compared with their digits as they are: lines that
//     differ by a number, a balance carried forward or an invoice's number,
//     are different content.
//  3. Among the headers and footers, the ones a reader labeled and the
//     ones step 2 found, every occurrence after the first is marked
//     repeated. Here digits are folded, so "Report, page 3" repeats
//     "Report, page 2": these blocks are furniture already, and folding
//     can hide nothing else.
func running(pages []document.Page) {
	type edge struct {
		page  int // index into pages
		block int // index into the page's blocks
	}
	tops, bottoms := map[string][]edge{}, map[string][]edge{}
	counted := 0
	for i := range pages {
		p := &pages[i]
		if p.State != document.PageSucceeded || len(p.Blocks) == 0 {
			continue
		}
		counted++
		pageNumbers(p)

		// The edges are looked for inside the page numbers: a footer line
		// above a page number is still the bottom of the page.
		first, last := 0, len(p.Blocks)-1
		for first <= last && p.Blocks[first].Kind == document.KindPageNumber {
			first++
		}
		for last > first && p.Blocks[last].Kind == document.KindPageNumber {
			last--
		}
		if first > last {
			continue
		}
		if key, ok := runningKey(p.Blocks[first], true); ok {
			tops[key] = append(tops[key], edge{i, first})
		}
		if last > first {
			if key, ok := runningKey(p.Blocks[last], false); ok {
				bottoms[key] = append(bottoms[key], edge{i, last})
			}
		}
	}

	threshold := max(2, (counted+1)/2)
	label := func(found map[string][]edge, kind document.Kind) {
		for _, edges := range found {
			if len(edges) < threshold {
				continue
			}
			for _, e := range edges {
				if b := &pages[e.page].Blocks[e.block]; !furniture(b.Kind) {
					b.Kind, b.Level = kind, 0
				}
			}
		}
	}
	label(tops, document.KindPageHeader)
	label(bottoms, document.KindPageFooter)

	seen := map[string]bool{}
	for i := range pages {
		if pages[i].State != document.PageSucceeded {
			continue
		}
		for j := range pages[i].Blocks {
			b := &pages[i].Blocks[j]
			if b.Kind != document.KindPageHeader && b.Kind != document.KindPageFooter {
				continue
			}
			key := string(b.Kind) + "\x00" + foldDigits(fold(b.Text))
			b.Repeated = seen[key]
			seen[key] = true
		}
	}
}

// pageNumber matches a text that is a page number and nothing else: a
// number of at most four digits, alone or after a word for page, with an
// optional total ("3 / 40", "Page 3 of 40"), inside optional dashes,
// brackets or a closing dot ("- 3 -", "[3]", "3.").
var pageNumber = regexp.MustCompile(`(?i)^[\s\-\x{2013}\x{2014}\[(]*(?:(?:page|seite|pagina|página|p\.?)\s*)?\d{1,4}(?:\s*(?:/|of|von|de|di)\s*\d{1,4})?[\s\-\x{2013}\x{2014}\])\.]*$`)

// pageNumbers relabels the blocks of a page that are page numbers: the
// first or the last block, when its whole text is one and it does not sit
// away from the page's edge, and any block a reader called a header or a
// footer that holds a page number and nothing else. A title or a heading
// is left alone, whatever it says, and so is a number in the middle of a
// page.
func pageNumbers(p *document.Page) {
	last := len(p.Blocks) - 1
	for i := range p.Blocks {
		b := &p.Blocks[i]
		switch b.Kind {
		case document.KindText, document.KindCaption, document.KindFootnote:
			if (i != 0 && i != last) || !atEdge(*b, i == 0, i == last) {
				continue
			}
		case document.KindPageHeader, document.KindPageFooter:
		default:
			continue
		}
		if pageNumber.MatchString(b.Text) {
			b.Kind, b.Level = document.KindPageNumber, 0
		}
	}
}

// atEdge reports whether a block lies in the band at the top of its page,
// when top is set, or at the bottom, when bottom is. A block with no box
// has no position to judge by and passes.
func atEdge(b document.Block, top, bottom bool) bool {
	if b.Box == nil {
		return true
	}
	return (top && b.Box[3] <= edgeBand) || (bottom && b.Box[1] >= 1-edgeBand)
}

// runningKey is the form in which two lines at a page's edge are compared:
// lower case with whitespace collapsed, and nothing else changed. ok is
// false for a block that cannot be a running line: one that is not text,
// is empty or longer than a line, or that a reader placed away from the
// edge. A block a reader already labeled a header or a footer is taken
// wherever it sits.
func runningKey(b document.Block, top bool) (key string, ok bool) {
	if !b.Kind.Textual() || b.Text == "" || utf8.RuneCountInString(b.Text) > maxRunningText {
		return "", false
	}
	if !furniture(b.Kind) && !atEdge(b, top, !top) {
		return "", false
	}
	return fold(b.Text), true
}

// fold lowers a text's case and collapses its whitespace.
func fold(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// foldDigits replaces every run of digits by one placeholder, so "page 3
// of 40" and "page 4 of 40" are the same line.
func foldDigits(s string) string {
	var out strings.Builder
	inDigits := false
	for _, r := range s {
		if unicode.IsDigit(r) {
			if !inDigits {
				out.WriteByte('#')
			}
			inDigits = true
			continue
		}
		inDigits = false
		out.WriteRune(r)
	}
	return out.String()
}

// furniture reports whether a block is page furniture and not content: the
// kinds a table continuing across pages reaches past, and that a chunk
// leaves out.
func furniture(k document.Kind) bool {
	return k == document.KindPageHeader || k == document.KindPageFooter || k == document.KindPageNumber
}

// spans joins tables that continue across pages. When a page's last content
// block is a table and the next page's first content block is a table with
// the same number of columns, the two may be parts of one table. The
// column count alone is not enough: two unrelated tables of two columns
// would be joined. One more sign is needed: the second table begins with
// the first one's header row, or the two lie between the same left and
// right edges on their pages. The per-page tables are not changed; a span
// adds the join.
func spans(pages []document.Page) []document.Span {
	var out []document.Span
	var open *document.Span
	var header string        // the first row of the open span's first part
	var prev *document.Block // the open span's latest part
	closeOpen := func() {
		if open != nil && len(open.Parts) > 1 {
			open.ID = "s" + strconv.Itoa(len(out)+1)
			out = append(out, *open)
		}
		open = nil
	}
	for i, p := range pages {
		first, last := tableEdges(p)
		joins := open != nil && i > 0 && pages[i-1].Number+1 == p.Number &&
			first != nil && first.Table != nil && first.Table.Cols == open.Cols &&
			continues(prev, first, header)
		if joins {
			open.Parts = append(open.Parts, first.Ref)
			open.Rows += first.Table.Rows
			prev = first
			// A table that fills the page keeps the span open; anything
			// after it on the page ends the span here.
			if last != first {
				closeOpen()
			}
		} else {
			closeOpen()
		}
		if open == nil && last != nil && last.Table != nil && last.Table.Cols > 0 {
			open = &document.Span{Parts: []string{last.Ref}, Rows: last.Table.Rows, Cols: last.Table.Cols}
			header, prev = firstRow(last.Table), last
		}
	}
	closeOpen()
	return out
}

// continues reports whether next carries on the table that prev is the
// latest part of, given that their column counts agree: next begins with
// the table's header row, or both have a place on their page and it is the
// same from left to right.
func continues(prev, next *document.Block, header string) bool {
	if header != "" && firstRow(next.Table) == header {
		return true
	}
	if prev.Box == nil || next.Box == nil {
		return false
	}
	return math.Abs(prev.Box[0]-next.Box[0]) <= edgeTolerance && math.Abs(prev.Box[2]-next.Box[2]) <= edgeTolerance
}

// firstRow is a table's first row in the form two rows are compared in:
// its cells in column order, folded. It is empty for a row with no text.
func firstRow(t *document.Table) string {
	var cells []document.Cell
	for _, c := range t.Cells {
		if c.Row == 0 {
			cells = append(cells, c)
		}
	}
	slices.SortStableFunc(cells, func(a, b document.Cell) int { return cmp.Compare(a.Col, b.Col) })
	texts := make([]string, 0, len(cells))
	empty := true
	for _, c := range cells {
		texts = append(texts, fold(c.Text))
		empty = empty && texts[len(texts)-1] == ""
	}
	if empty {
		return ""
	}
	return strings.Join(texts, "\x1f")
}

// tableEdges returns a page's first and last blocks that are content, which
// is where a table that continues from the page before, or onto the next,
// has to be. A caption that says the table is continued is passed over, so
// "Table 3 (continued)" above a table does not hide it; any other caption
// stands for a table of its own and is content. Both results are nil for a
// page that did not succeed or holds only furniture.
func tableEdges(p document.Page) (first, last *document.Block) {
	if p.State != document.PageSucceeded {
		return nil, nil
	}
	var content []*document.Block
	for i := range p.Blocks {
		if !furniture(p.Blocks[i].Kind) {
			content = append(content, &p.Blocks[i])
		}
	}
	if len(content) > 1 && continued(content[0]) {
		content = content[1:]
	}
	if len(content) > 1 && continued(content[len(content)-1]) {
		content = content[:len(content)-1]
	}
	if len(content) == 0 {
		return nil, nil
	}
	return content[0], content[len(content)-1]
}

// continued reports whether a block is a caption saying that a table goes
// on from, or onto, another page.
func continued(b *document.Block) bool {
	if b.Kind != document.KindCaption {
		return false
	}
	text := strings.ToLower(b.Text)
	return strings.Contains(text, "continued") || strings.Contains(text, "cont.")
}

// oneTitle leaves a document the title of its first page that has one. A
// reader sees one page, so on a page that opens with a large line it cannot
// know whether the document already has a title: the first slide of a deck
// and each slide after it look alike, and so do the first page of a report
// and the first page of each of its parts. The titles of the first page that
// holds one are the document's. A block a reader called a title on a later
// page becomes a heading and keeps its text, its place and its level, which
// the outline then puts on the document's scale below the title. A title
// that running already made furniture is no longer a title and is not
// counted.
//
// The rule is for pages a reader read. A format that carries its own
// structure says which of its parts are titles, a workbook's sheets for
// one, and nothing was guessed there, so such a page keeps its titles and
// stands for no title of a read page either.
func oneTitle(pages []document.Page) {
	titled := false
	for i := range pages {
		if pages[i].Source == document.SourceNative {
			continue
		}
		found := false
		for j := range pages[i].Blocks {
			b := &pages[i].Blocks[j]
			if b.Kind != document.KindTitle {
				continue
			}
			if titled {
				b.Kind = document.KindHeading
				continue
			}
			found = true
		}
		titled = titled || found
	}
}

// outline lists the headings in page order with their levels on one scale
// for the document. A reader sees one page, so the depth it gives a heading
// is a guess made without the rest of the document, and it drifts from
// page to page. Each heading first gets a level from what the document
// itself says (headingLevel), and the distinct levels seen then become 1,
// 2, 3 in order, so an outline has no gap and, with no title, its top
// sections are at the top.
func outline(pages []document.Page) []document.Heading {
	var out []document.Heading
	for _, p := range pages {
		for _, b := range p.Blocks {
			if b.Kind != document.KindTitle && b.Kind != document.KindHeading {
				continue
			}
			out = append(out, document.Heading{Ref: b.Ref, Level: headingLevel(b), Text: b.Text, Page: p.Number})
		}
	}
	seen := make([]int, 0, 6)
	for _, h := range out {
		if !slices.Contains(seen, h.Level) {
			seen = append(seen, h.Level)
		}
	}
	slices.SortFunc(seen, cmp.Compare[int])
	for i := range out {
		out[i].Level = slices.Index(seen, out[i].Level) + 1
	}
	return out
}

// headingLevel is a heading's level before the document-wide scale. A
// title is 1. A heading that begins with a printed section number is one
// below its number's depth, so "3 Model" is 2 and "3.2.1 Attention" is 4,
// whatever its reader said: the number is printed in the document and the
// reader's level is not. Any other heading keeps its reader's level, and is
// never above 2, so it does not rank with a title.
func headingLevel(b document.Block) int {
	if b.Kind == document.KindTitle {
		return 1
	}
	if depth := numberDepth(b.Text); depth > 0 {
		return min(1+depth, 6)
	}
	return min(max(b.Level, 2), 6)
}

// sectionNumber matches the printed number a heading begins with: "1",
// "2.3", "3.2.1", each with an optional closing dot; a letter followed by
// numbers, "A.1"; or one letter or a Roman numeral with a closing dot,
// "A." and "IV.". A letter or a numeral with no dot is a word ("A Study"),
// and a number of four digits is a year.
var sectionNumber = regexp.MustCompile(`^(?:(\d{1,3}(?:\.\d{1,3})*)\.?|([A-Z](?:\.\d{1,3})+)\.?|(?:[A-Z]|[IVXLC]{1,7})\.)\s+\S`)

// numberDepth is how deep a heading's printed section number is: 1 for
// "3", 2 for "3.2", 3 for "3.2.1". It is 0 for a heading with none.
func numberDepth(text string) int {
	m := sectionNumber.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	return strings.Count(m[1]+m[2], ".") + 1
}
