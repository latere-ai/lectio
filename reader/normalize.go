// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reader

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"latere.ai/x/lectio/document"
)

// Raw is one region as a model or an engine returned it, before it is
// brought onto the object model. An adapter decodes its wire format into
// Raws and calls Normalize, so every adapter's blocks mean the same thing.
type Raw struct {
	// Label is the engine's own name for the kind of region.
	Label string

	// Text is the region's content. For a table it may be the table's HTML.
	Text string

	// Box is [x0, y0, x1, y1] on the engine's grid. Any other length means
	// the engine gave no position.
	Box []float64

	// Order is the engine's reading order. Zero means the position in the
	// reply is the order.
	Order int

	// Level is a heading's depth, when the engine gave one.
	Level int

	// HTML is a table's markup when the engine returned it apart from Text.
	HTML string

	// Description is what the engine says a figure shows, in its own
	// words. It is kept apart from Text, which is transcription.
	Description string
}

// Grid is the coordinate range an engine's boxes are in: the image's size
// in pixels, or a fixed range such as 1000 by 1000.
type Grid struct{ Width, Height float64 }

// Normalize brings raw regions onto the object model. For each region it
// maps the engine's label onto the closed set of kinds, scales the box to a
// fraction of the page and repairs it, reads a table's structure from its
// markup, and records what it changed in the block's flags. Regions with no
// content are dropped. The blocks come back in reading order with Order
// dense from 1 and Ref empty.
func Normalize(raws []Raw, grid Grid) []document.Block {
	blocks := make([]document.Block, 0, len(raws))
	for i, raw := range raws {
		b := document.Block{Text: strings.TrimSpace(raw.Text), Order: raw.Order, Level: min(max(raw.Level, 0), 6)}
		if b.Order <= 0 {
			b.Order = i + 1
		}

		kind, known := KindOf(raw.Label)
		b.Kind = kind
		if !known {
			b.Flags = append(b.Flags, document.FlagKindCoerced)
		}
		switch kind {
		case document.KindTitle, document.KindHeading:
			// An engine that writes text as Markdown puts a heading's
			// depth in its marks. The marks give the level when the
			// engine named none, and are not part of the text.
			if marks, rest, ok := headingMarks(b.Text); ok {
				b.Text = rest
				if b.Level == 0 {
					b.Level = marks
				}
			}
		case document.KindFormula:
			b.Level = 0
			b.Text = mathBody(b.Text)
		case document.KindFigure:
			b.Level = 0
			// Only a figure has a description: for any other kind the
			// engine's own words about a region are not kept.
			b.Description = strings.TrimSpace(raw.Description)
		default:
			b.Level = 0
		}

		if box, repaired, ok := scale(raw.Box, grid); ok {
			b.Box = &box
			if repaired {
				b.Flags = append(b.Flags, document.FlagBoxClamped)
			}
		} else if len(raw.Box) == 4 {
			// The engine gave a position and nothing usable is left of it.
			b.Flags = append(b.Flags, document.FlagBoxClamped)
		}

		if kind == document.KindTable {
			markup := raw.HTML
			if markup == "" && strings.Contains(strings.ToLower(raw.Text), "<table") {
				markup = raw.Text
			}
			if table, ok := TableFromHTML(markup); ok {
				b.Table = table
				b.Text = tableText(table)
			}
		}

		if b.Text == "" && b.Table == nil && kind != document.KindFigure && kind != document.KindSignature && kind != document.KindBarcode {
			continue
		}
		blocks = append(blocks, b)
	}

	document.Number(0, blocks)
	for i := range blocks {
		blocks[i].Ref = ""
	}
	return blocks
}

// headingMarks reads the Markdown marks a heading's text begins with: one
// to six number signs and a space. It returns how many there are and the
// text after them.
func headingMarks(text string) (marks int, rest string, ok bool) {
	rest = strings.TrimLeft(text, "#")
	marks = len(text) - len(rest)
	if marks < 1 || marks > 6 || !strings.HasPrefix(rest, " ") {
		return 0, text, false
	}
	return marks, strings.TrimSpace(rest), true
}

// mathBody returns a formula without the delimiters an engine wrapped it
// in. The text of a formula block is the formula; a rendering adds the
// delimiters its format needs.
func mathBody(text string) string {
	for _, pair := range [][2]string{{"$$", "$$"}, {`\[`, `\]`}, {`\(`, `\)`}, {"$", "$"}} {
		if len(text) >= len(pair[0])+len(pair[1]) && strings.HasPrefix(text, pair[0]) && strings.HasSuffix(text, pair[1]) {
			return strings.TrimSpace(text[len(pair[0]) : len(text)-len(pair[1])])
		}
	}
	return text
}

// scale turns an engine's box into a fraction of the page.
func scale(raw []float64, grid Grid) (box document.Box, repaired, ok bool) {
	if len(raw) != 4 || grid.Width <= 0 || grid.Height <= 0 {
		return box, false, false
	}
	box = document.Box{raw[0] / grid.Width, raw[1] / grid.Height, raw[2] / grid.Width, raw[3] / grid.Height}
	return box.Normalize()
}

// labels maps the names engines use onto the closed set of kinds. Engines
// agree on what a page holds and not on what to call it, so the mapping is
// here, once, and not in each adapter.
var labels = map[string]document.Kind{
	"title": document.KindTitle,

	"heading": document.KindHeading, "section_header": document.KindHeading, "sectionheader": document.KindHeading,
	"subtitle": document.KindHeading, "subheading": document.KindHeading,

	"text": document.KindText, "paragraph": document.KindText, "plain_text": document.KindText,
	"body": document.KindText, "narrative_text": document.KindText, "narrativetext": document.KindText,
	"uncategorized_text": document.KindText, "uncategorizedtext": document.KindText,
	"aside_text": document.KindText, "references": document.KindText, "comment": document.KindText,

	"list_item": document.KindListItem, "list": document.KindListItem, "listitem": document.KindListItem,

	"table": document.KindTable,

	"figure": document.KindFigure, "picture": document.KindFigure, "image": document.KindFigure,
	"chart": document.KindFigure, "diagram": document.KindFigure,

	"formula": document.KindFormula, "equation": document.KindFormula, "math": document.KindFormula,

	"form": document.KindForm,

	"key_value": document.KindKeyValue, "key_value_region": document.KindKeyValue, "key_value_pair": document.KindKeyValue,

	"caption": document.KindCaption, "table_caption": document.KindCaption,
	"figure_caption": document.KindCaption, "figurecaption": document.KindCaption,
	"formula_caption": document.KindCaption,

	"footnote": document.KindFootnote,

	"page_header": document.KindPageHeader, "header": document.KindPageHeader, "running_header": document.KindPageHeader,

	"page_footer": document.KindPageFooter, "footer": document.KindPageFooter, "running_footer": document.KindPageFooter,

	"page_number": document.KindPageNumber, "pagenumber": document.KindPageNumber,

	"signature": document.KindSignature,

	"barcode": document.KindBarcode, "qr_code": document.KindBarcode,

	"code": document.KindCode, "code_block": document.KindCode, "code_snippet": document.KindCode,
	"codesnippet": document.KindCode,
}

// KindOf maps an engine's label onto the closed set of kinds. Case, hyphens
// and spaces do not matter: "Section-header" and "section header" are both a
// heading. A label nobody knows is text, and ok is false.
func KindOf(label string) (kind document.Kind, ok bool) {
	key := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(label)))
	// Some engines prefix every layout label; the word after it is the label.
	key = strings.TrimPrefix(key, "layout_")
	if kind, ok = labels[key]; ok {
		return kind, true
	}
	return document.KindText, false
}

// TableFromHTML reads a table's structure from its markup: the cells with
// their row, column, spans, and text, and the table's size. ok is false
// when the markup holds no cell.
func TableFromHTML(markup string) (table *document.Table, ok bool) {
	if strings.TrimSpace(markup) == "" {
		return nil, false
	}
	table = &document.Table{}
	taken := map[[2]int]bool{}
	row, col := -1, 0
	var cell *document.Cell
	var text strings.Builder

	closeCell := func() {
		if cell == nil {
			return
		}
		cell.Text = strings.Join(strings.Fields(text.String()), " ")
		table.Cells = append(table.Cells, *cell)
		cell = nil
		text.Reset()
	}

	z := html.NewTokenizer(strings.NewReader(markup))
	for {
		switch z.Next() {
		case html.ErrorToken:
			closeCell()
			table.HTML = tableHTML(table)
			return table, len(table.Cells) > 0
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "tr":
				closeCell()
				row, col = row+1, 0
			case "td", "th":
				closeCell()
				if row < 0 {
					row = 0
				}
				for taken[[2]int{row, col}] {
					col++
				}
				rowSpan, colSpan := spans(z, hasAttr)
				cell = &document.Cell{Row: row, Col: col, Header: string(name) == "th"}
				if rowSpan > 1 {
					cell.RowSpan = rowSpan
				}
				if colSpan > 1 {
					cell.ColSpan = colSpan
				}
				for r := range rowSpan {
					for c := range colSpan {
						taken[[2]int{row + r, col + c}] = true
					}
				}
				table.Rows = max(table.Rows, row+rowSpan)
				table.Cols = max(table.Cols, col+colSpan)
				col += colSpan
			case "br":
				text.WriteByte(' ')
			case "sup", "sub":
				// A raised or lowered run is part of the value: n squared
				// is not n2. Plain text marks it the way plain-text math
				// does, with a caret or an underscore.
				if cell != nil {
					text.WriteString(map[string]string{"sup": "^", "sub": "_"}[string(name)])
				}
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "td" || string(name) == "th" {
				closeCell()
			}
		case html.TextToken:
			if cell != nil {
				text.Write(z.Text())
			}
		}
	}
}

// spans reads rowspan and colspan from the tag the tokenizer is on. A span
// that is missing, not a number, or below 1 is 1; one beyond a bound is the
// bound, so a hostile span cannot make the table arbitrarily large.
func spans(z *html.Tokenizer, hasAttr bool) (rowSpan, colSpan int) {
	const bound = 1000
	rowSpan, colSpan = 1, 1
	for hasAttr {
		var key, val []byte
		key, val, hasAttr = z.TagAttr()
		n, err := strconv.Atoi(strings.TrimSpace(string(val)))
		if err != nil || n < 1 {
			continue
		}
		switch string(key) {
		case "rowspan":
			rowSpan = min(n, bound)
		case "colspan":
			colSpan = min(n, bound)
		}
	}
	return rowSpan, colSpan
}

// tableHTML writes a table's markup from its cells. The markup a reader
// returned is not kept: it came from a model that read a file somebody
// else wrote, and whatever that file led it to write, a script, a handler,
// a link, would be served to whoever renders the result. Markup written
// from the cells holds a table and nothing else.
func tableHTML(t *document.Table) string {
	var b strings.Builder
	b.WriteString("<table>")
	row := -1
	for _, c := range t.Cells {
		if c.Row != row {
			if row >= 0 {
				b.WriteString("</tr>")
			}
			b.WriteString("<tr>")
			row = c.Row
		}
		tag := "td"
		if c.Header {
			tag = "th"
		}
		b.WriteString("<" + tag)
		if c.RowSpan > 1 {
			b.WriteString(` rowspan="` + strconv.Itoa(c.RowSpan) + `"`)
		}
		if c.ColSpan > 1 {
			b.WriteString(` colspan="` + strconv.Itoa(c.ColSpan) + `"`)
		}
		b.WriteString(">" + html.EscapeString(c.Text) + "</" + tag + ">")
	}
	if row >= 0 {
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
	return b.String()
}

// tableText is a table as plain text: cells joined by " | " and rows by a
// newline, which is what a text search and a text rendering read.
func tableText(t *document.Table) string {
	rows := make([][]string, t.Rows)
	for _, c := range t.Cells {
		rows[c.Row] = append(rows[c.Row], c.Text)
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			lines = append(lines, strings.Join(r, " | "))
		}
	}
	return strings.Join(lines, "\n")
}

// Check decides whether a reader's result is usable. It returns nil, or an
// Error of class Invalid saying why not: the page has content and the reply
// has no blocks, or the reply ran to its output limit with one line making
// up most of it, which is what a model that has fallen into a loop
// produces. A reply that ended by itself is not a loop however much it
// repeats: a timesheet repeats. blank says the caller already knows the
// page is empty.
func Check(result Result, blank bool) error {
	if len(result.Blocks) == 0 {
		if blank {
			return nil
		}
		return Errorf(Invalid, "the reply holds no blocks for a page that is not blank")
	}
	if !result.Truncated {
		return nil
	}
	const longReply, share = 20, 0.5
	counts, total, top := map[string]int{}, 0, 0
	for _, b := range result.Blocks {
		for line := range strings.SplitSeq(b.Text, "\n") {
			line = strings.TrimSpace(line)
			if len(line) < 8 {
				continue
			}
			total++
			counts[line]++
			top = max(top, counts[line])
		}
	}
	if total >= longReply && float64(top) > share*float64(total) {
		return Errorf(Invalid, "one line makes up %d of the reply's %d lines", top, total)
	}
	return nil
}
