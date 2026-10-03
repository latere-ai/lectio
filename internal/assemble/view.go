// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package assemble

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"latere.ai/x/lectio/document"
)

// View is how a document is rendered when it is read. It is chosen by the
// reader of a result and never by the submit, so another view needs no new
// parse.
type View struct {
	// Pages limits the rendering to these page numbers. Nil is every page.
	Pages []int

	// Tables is how a table is written in Markdown: "markdown", "html", or
	// "auto", which writes Markdown unless a cell spans.
	Tables string

	// Repeated is what happens to a page's furniture: "once" prints the
	// first occurrence of a running header or footer, "keep" prints every
	// block, and "drop" prints no header and no footer. A page number is
	// printed only under "keep": it says nothing about the document.
	Repeated string

	// PageBreaks marks where each page begins in Markdown.
	PageBreaks bool
}

// The values of View.Tables and View.Repeated.
const (
	TablesAuto     = "auto"
	TablesMarkdown = "markdown"
	TablesHTML     = "html"

	RepeatedOnce = "once"
	RepeatedKeep = "keep"
	RepeatedDrop = "drop"
)

// shown reports whether a block is printed under the view.
func (v View) shown(b document.Block) bool {
	switch v.Repeated {
	case RepeatedKeep:
		return true
	case RepeatedDrop:
		return !furniture(b.Kind)
	}
	return !b.Repeated && b.Kind != document.KindPageNumber
}

// selected returns the pages the view covers, in order, that succeeded.
func (v View) selected(pages []document.Page) []document.Page {
	out := make([]document.Page, 0, len(pages))
	for _, p := range pages {
		if p.State == document.PageSucceeded && (v.Pages == nil || slices.Contains(v.Pages, p.Number)) {
			out = append(out, p)
		}
	}
	return out
}

// Markdown writes the pages as Markdown in reading order: headings by
// level, lists, tables, formulas in math delimiters, and a figure as what
// its reader says it shows, marked as such, followed by the words printed
// in it. outline gives each heading its level in the document; a heading
// it does not hold takes the level its own text and reader give it. A
// block with nothing to print prints nothing.
func Markdown(w io.Writer, pages []document.Page, outline []document.Heading, v View) error {
	levels := make(map[string]int, len(outline))
	for _, h := range outline {
		levels[h.Ref] = h.Level
	}
	first := true
	for _, p := range v.selected(pages) {
		if v.PageBreaks {
			if !first {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "<!-- page %d -->\n", p.Number); err != nil {
				return err
			}
			first = false
		}
		for _, b := range p.Blocks {
			if !v.shown(b) {
				continue
			}
			out := markdownBlock(b, levels, v.Tables)
			if out == "" {
				continue
			}
			if !first {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			first = false
			if _, err := io.WriteString(w, out+"\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

func markdownBlock(b document.Block, levels map[string]int, tables string) string {
	switch b.Kind {
	case document.KindTitle, document.KindHeading:
		level, ok := levels[b.Ref]
		if !ok {
			level = headingLevel(b)
		}
		return strings.Repeat("#", min(max(level, 1), 6)) + " " + oneLine(b.Text)
	case document.KindListItem:
		return "- " + oneLine(b.Text)
	case document.KindTable:
		return markdownTable(b, tables)
	case document.KindFormula:
		return "$$\n" + b.Text + "\n$$"
	case document.KindCode:
		return "```\n" + b.Text + "\n```"
	case document.KindFigure:
		// The description is the reader's own prose, so it is set apart
		// from the words printed in the figure, which are transcription.
		if b.Description == "" {
			return b.Text
		}
		out := "*[Figure: " + oneLine(b.Description) + "]*"
		if b.Text != "" {
			out += "\n\n" + b.Text
		}
		return out
	case document.KindCaption:
		return "*" + oneLine(b.Text) + "*"
	}
	return b.Text
}

// markdownTable writes a table. A table with a cell that spans cannot be
// said in Markdown, so under "auto" it is written as its HTML; a table with
// no structure is written as the text its reader gave.
func markdownTable(b document.Block, tables string) string {
	t := b.Table
	if t == nil || len(t.Cells) == 0 {
		return b.Text
	}
	spans := slices.ContainsFunc(t.Cells, func(c document.Cell) bool { return c.RowSpan > 1 || c.ColSpan > 1 })
	if t.HTML != "" && (tables == TablesHTML || (tables != TablesMarkdown && spans)) {
		return t.HTML
	}

	grid := make([][]string, t.Rows)
	for i := range grid {
		grid[i] = make([]string, t.Cols)
	}
	for _, c := range t.Cells {
		if c.Row < t.Rows && c.Col < t.Cols {
			grid[c.Row][c.Col] = strings.ReplaceAll(oneLine(c.Text), "|", `\|`)
		}
	}
	var out strings.Builder
	for i, row := range grid {
		out.WriteString("| ")
		out.WriteString(strings.Join(row, " | "))
		out.WriteString(" |\n")
		if i == 0 {
			out.WriteString("|")
			out.WriteString(strings.Repeat(" --- |", t.Cols))
			out.WriteString("\n")
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// oneLine folds a text onto one line, for the constructs that end at a
// newline.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Text writes the pages as plain text in reading order, a blank line
// between blocks. A figure's description is printed in brackets, so it is
// not taken for the document's own words.
func Text(w io.Writer, pages []document.Page, v View) error {
	first := true
	for _, p := range v.selected(pages) {
		for _, b := range p.Blocks {
			out := plain(b)
			if !v.shown(b) || out == "" {
				continue
			}
			if !first {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			first = false
			if _, err := io.WriteString(w, out+"\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// plain is a block as the text view prints it: its text, and for a figure
// what its reader says it shows, in brackets, ahead of the words printed
// in it.
func plain(b document.Block) string {
	if b.Kind != document.KindFigure || b.Description == "" {
		return b.Text
	}
	out := "[Figure: " + oneLine(b.Description) + "]"
	if b.Text != "" {
		out += "\n\n" + b.Text
	}
	return out
}

// The strategies Chunks cuts by.
const (
	ByPage    = "page"
	BySection = "section"
)

// Chunks cuts the pages into runs of blocks for retrieval. By page, a chunk
// is a page. By section, a chunk begins at each title or heading and runs
// to the next. Either way a chunk longer than maxChars, counted in
// characters, is split at block boundaries, and each chunk lists the pages
// and the blocks it covers so a hit can be shown on the page.
//
// A chunk is cited as what the document says, so it holds transcription
// and nothing else: page furniture is left out, and so is what a reader
// says a figure shows.
//
// A heading belongs to what follows it. A chunk that would hold only
// headings is not cut off: the next heading or the next block joins it,
// so "Chapter 2" directly above "2.1 Overview" is the start of one chunk.
// Only a run of headings that is itself past maxChars, a table of
// contents read as headings, is cut.
//
// A block is not split, with one exception. A table longer than maxChars
// is cut between rows into parts that each begin with the table's header
// row and each name the table's block, so a hit in row 300 still says
// what its columns are. Any other single block longer than maxChars is a
// chunk of its own.
func Chunks(pages []document.Page, by string, maxChars int) []document.Chunk {
	var out []document.Chunk
	var cur document.Chunk
	var text strings.Builder
	size := 0        // characters in text
	headings := true // cur holds nothing but headings

	flush := func() {
		if len(cur.Blocks) == 0 {
			return
		}
		cur.ID = "c" + strconv.Itoa(len(out)+1)
		cur.Text = text.String()
		out = append(out, cur)
		cur = document.Chunk{}
		text.Reset()
		size, headings = 0, true
	}
	add := func(page int, ref, s string) {
		if text.Len() > 0 {
			text.WriteString("\n\n")
			size += 2
		}
		text.WriteString(s)
		size += utf8.RuneCountInString(s)
		if !slices.Contains(cur.Blocks, ref) {
			cur.Blocks = append(cur.Blocks, ref)
		}
		if !slices.Contains(cur.Pages, page) {
			cur.Pages = append(cur.Pages, page)
		}
	}

	for _, p := range (View{}).selected(pages) {
		if by == ByPage {
			flush()
		}
		for _, b := range p.Blocks {
			if furniture(b.Kind) || b.Text == "" {
				continue
			}
			length := utf8.RuneCountInString(b.Text)
			if parts := tableParts(b, maxChars); len(parts) > 1 {
				for i, part := range parts {
					if i > 0 || !headings {
						flush()
					}
					add(p.Number, b.Ref, part)
					headings = false
				}
				flush()
				continue
			}
			heading := b.Kind == document.KindTitle || b.Kind == document.KindHeading
			if (!headings || size > maxChars) && ((by == BySection && heading) || size+2+length > maxChars) {
				flush()
			}
			add(p.Number, b.Ref, b.Text)
			headings = headings && heading
		}
	}
	flush()
	return out
}

// tableParts cuts a table that is longer than maxChars into parts between
// its rows. Each part begins with the header row, which is the rows whose
// cells are marked as headers or, when none is, the first row, and holds
// as many of the other rows as fit. A row is not cut, so a part may be
// longer than maxChars when one row is. The result is nil for a block that
// is not a table with cells, and has one part for a table that fits or has
// no row beside its header.
func tableParts(b document.Block, maxChars int) []string {
	if b.Table == nil || len(b.Table.Cells) == 0 || utf8.RuneCountInString(b.Text) <= maxChars {
		return nil
	}
	cells := slices.Clone(b.Table.Cells)
	slices.SortStableFunc(cells, func(a, c document.Cell) int {
		return cmp.Or(cmp.Compare(a.Row, c.Row), cmp.Compare(a.Col, c.Col))
	})
	type row struct {
		line   string
		header bool
	}
	var rows []row
	marked := false
	for i, at := 0, -1; i < len(cells); i++ {
		c := cells[i]
		if c.Row != at {
			at = c.Row
			rows = append(rows, row{line: c.Text})
		} else {
			rows[len(rows)-1].line += " | " + c.Text
		}
		rows[len(rows)-1].header = rows[len(rows)-1].header || c.Header
		marked = marked || c.Header
	}
	if !marked {
		rows[0].header = true
	}

	var head, body []string
	for _, r := range rows {
		if r.header {
			head = append(head, r.line)
		} else {
			body = append(body, r.line)
		}
	}
	header := strings.Join(head, "\n")
	if len(body) == 0 {
		return []string{header}
	}

	var parts []string
	var part strings.Builder
	size, held := 0, 0
	for _, line := range body {
		length := utf8.RuneCountInString(line) + 1
		if held > 0 && size+length > maxChars {
			parts = append(parts, part.String())
			held = 0
		}
		if held == 0 {
			part.Reset()
			part.WriteString(header)
			size = utf8.RuneCountInString(header)
		}
		part.WriteString("\n" + line)
		size += length
		held++
	}
	return append(parts, part.String())
}
