// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package assemble

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

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

	// Repeated is what happens to running headers and footers: "once"
	// prints the first occurrence, "keep" prints every one, and "drop"
	// prints none and leaves out page numbers too.
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
	return !b.Repeated
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
// level, lists, tables, formulas in math delimiters, figures as their
// description. levels maps a heading's ref to its level in the document's
// outline; a heading it does not hold keeps the level its reader gave.
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
			if !first {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			first = false
			if _, err := io.WriteString(w, markdownBlock(b, levels, v.Tables)+"\n"); err != nil {
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
		if b.Text == "" {
			return "*Figure.*"
		}
		return "*Figure: " + oneLine(b.Text) + "*"
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
// between blocks.
func Text(w io.Writer, pages []document.Page, v View) error {
	first := true
	for _, p := range v.selected(pages) {
		for _, b := range p.Blocks {
			if !v.shown(b) || b.Text == "" {
				continue
			}
			if !first {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			first = false
			if _, err := io.WriteString(w, b.Text+"\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// The strategies Chunks cuts by.
const (
	ByPage    = "page"
	BySection = "section"
)

// Chunks cuts the pages into runs of blocks for retrieval. By page, a chunk
// is a page. By section, a chunk begins at each title or heading and runs
// to the next. Either way a chunk longer than maxChars is split at block
// boundaries, repeated headers and footers are left out, and each chunk
// lists the pages and the blocks it covers so a hit can be shown on the
// page. A block is never split, so a single block longer than maxChars is a
// chunk of its own.
func Chunks(pages []document.Page, by string, maxChars int) []document.Chunk {
	var out []document.Chunk
	var cur document.Chunk
	var text strings.Builder

	flush := func() {
		if len(cur.Blocks) == 0 {
			return
		}
		cur.ID = "c" + strconv.Itoa(len(out)+1)
		cur.Text = text.String()
		out = append(out, cur)
		cur = document.Chunk{}
		text.Reset()
	}

	view := View{Repeated: RepeatedOnce}
	for _, p := range view.selected(pages) {
		if by == ByPage {
			flush()
		}
		for _, b := range p.Blocks {
			if !view.shown(b) || b.Text == "" {
				continue
			}
			heading := b.Kind == document.KindTitle || b.Kind == document.KindHeading
			if (by == BySection && heading) || (text.Len() > 0 && text.Len()+len(b.Text)+2 > maxChars) {
				flush()
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(b.Text)
			cur.Blocks = append(cur.Blocks, b.Ref)
			if !slices.Contains(cur.Pages, p.Number) {
				cur.Pages = append(cur.Pages, p.Number)
			}
		}
	}
	flush()
	return out
}
