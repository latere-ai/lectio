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
	"slices"
	"strconv"
	"strings"
	"unicode"

	"latere.ai/x/lectio/document"
)

// maxRunningText is the longest text the header and footer pass looks at.
// A running header is a line, not a paragraph.
const maxRunningText = 200

// Document assembles pages into a document. It marks the running headers
// and footers on the pages it is given, in place, and returns the index:
// the pages without their blocks, the spans, the outline, and usage.
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
	doc.Spans = spans(pages)
	doc.Outline = outline(pages)
	for _, p := range pages {
		doc.Pages = append(doc.Pages, p.Summary())
	}
	return doc
}

// running makes the labeling of running headers and footers consistent. A
// reader is asked to label them and does so unevenly, so the pass looks at
// the first and the last block of every page: a text that appears at the
// same edge on at least half the pages, and on two at least, is a running
// header or footer. Its blocks take that kind, and every occurrence after
// the first is marked repeated. Nothing is deleted.
func running(pages []document.Page) {
	type edge struct {
		page  int // index into pages
		block int // index into the page's blocks
	}
	tops, bottoms := map[string][]edge{}, map[string][]edge{}
	counted := 0
	for i, p := range pages {
		if p.State != document.PageSucceeded || len(p.Blocks) == 0 {
			continue
		}
		counted++
		if key, ok := runningKey(p.Blocks[0]); ok {
			tops[key] = append(tops[key], edge{i, 0})
		}
		if last := len(p.Blocks) - 1; last > 0 {
			if key, ok := runningKey(p.Blocks[last]); ok {
				bottoms[key] = append(bottoms[key], edge{i, last})
			}
		}
	}
	threshold := max(2, (counted+1)/2)
	mark := func(found map[string][]edge, kind document.Kind) {
		for _, edges := range found {
			if len(edges) < threshold {
				continue
			}
			for n, e := range edges {
				b := &pages[e.page].Blocks[e.block]
				b.Kind, b.Level, b.Repeated = kind, 0, n > 0
			}
		}
	}
	mark(tops, document.KindPageHeader)
	mark(bottoms, document.KindPageFooter)
}

// runningKey is the form in which two running lines are compared: lower
// case, whitespace collapsed, and every run of digits replaced by one
// placeholder, so "Page 3 of 40" and "Page 4 of 40" are the same line.
func runningKey(b document.Block) (string, bool) {
	if !b.Kind.Textual() || b.Text == "" || len(b.Text) > maxRunningText {
		return "", false
	}
	var key strings.Builder
	inDigits := false
	for _, r := range strings.Join(strings.Fields(strings.ToLower(b.Text)), " ") {
		if unicode.IsDigit(r) {
			if !inDigits {
				key.WriteByte('#')
			}
			inDigits = true
			continue
		}
		inDigits = false
		key.WriteRune(r)
	}
	return key.String(), true
}

// furniture reports whether a block is page furniture and not content: the
// kinds a table continuing across pages reaches past.
func furniture(k document.Kind) bool {
	return k == document.KindPageHeader || k == document.KindPageFooter || k == document.KindPageNumber
}

// spans joins tables that continue across pages. When a page's last content
// block is a table and the next page's first content block is a table with
// the same number of columns, the two are parts of one table. Column count
// is the only test. The per-page tables are not changed; a span adds the
// join.
func spans(pages []document.Page) []document.Span {
	var out []document.Span
	var open *document.Span
	closeOpen := func() {
		if open != nil && len(open.Parts) > 1 {
			open.ID = "s" + strconv.Itoa(len(out)+1)
			out = append(out, *open)
		}
		open = nil
	}
	for i, p := range pages {
		first, last := contentEdges(p)
		joins := open != nil && i > 0 && pages[i-1].Number+1 == p.Number &&
			first != nil && first.Table != nil && first.Table.Cols == open.Cols
		if joins {
			open.Parts = append(open.Parts, first.Ref)
			open.Rows += first.Table.Rows
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
		}
	}
	closeOpen()
	return out
}

// contentEdges returns a page's first and last blocks that are content.
// Both are nil for a page that did not succeed or holds only furniture.
func contentEdges(p document.Page) (first, last *document.Block) {
	if p.State != document.PageSucceeded {
		return nil, nil
	}
	for i := range p.Blocks {
		if furniture(p.Blocks[i].Kind) {
			continue
		}
		if first == nil {
			first = &p.Blocks[i]
		}
		last = &p.Blocks[i]
	}
	return first, last
}

// outline lists the headings in page order. A reader's levels are per page
// and drift, so they are brought onto one scale for the document: the
// distinct levels seen become 1, 2, 3 in order. A title with no level is at
// the top, and a heading with none is below every title.
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

// headingLevel is a heading's level before the document-wide scale: the
// reader's, or 1 for a title and 2 for a heading the reader gave none.
func headingLevel(b document.Block) int {
	switch {
	case b.Level > 0:
		return b.Level
	case b.Kind == document.KindTitle:
		return 1
	}
	return 2
}
