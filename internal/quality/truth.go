// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"latere.ai/x/lectio/document"
)

// Truth is the content of a corpus file, as whoever made the file wrote it
// down. Its pages are numbered from 1 in the order they are listed.
type Truth struct {
	// Source is the SHA-256 of the source a truth was generated from, when
	// it was generated, so a test can tell that the two still belong
	// together.
	Source string `json:"source,omitempty"`

	Pages []Page `json:"pages"`

	// Spans are the tables that continue from one page to the next, and
	// Outline the headings with their level in the document, as assembly is
	// to find them. A span's parts and a heading's ref address the blocks
	// above as "<page>.<order>", both counted from 1.
	Spans   []document.Span    `json:"spans,omitempty"`
	Outline []document.Heading `json:"outline,omitempty"`
}

// Page is one page of a truth: its blocks in reading order. A page with no
// block is a blank page.
type Page struct {
	Blocks []Block `json:"blocks"`
}

// Block is one block of a truth.
type Block struct {
	Kind document.Kind `json:"kind"`
	// Level is the depth a title or a heading has on its page.
	Level int `json:"level,omitempty"`
	// Text is what the block prints, as plain text. A table has none: its
	// cells are its text. A figure's and a formula's are kept for a person
	// to read and are not compared.
	Text string `json:"text,omitempty"`
	// Box is where the block lies on its page, as a block's box is, when
	// the file's layout is known.
	Box *document.Box `json:"box,omitempty"`
	// Rows, Cols and Cells are a table's, every cell listed, an empty one
	// too.
	Rows  int             `json:"rows,omitempty"`
	Cols  int             `json:"cols,omitempty"`
	Cells []document.Cell `json:"cells,omitempty"`
}

// Parse reads a truth and refuses one that does not hold together: an
// unknown member, a kind outside the set, a table with no cell or a cell
// outside its table, cells on a block that is not a table, a box off the
// page, or a span or a heading that addresses no block.
func Parse(data []byte) (Truth, error) {
	var t Truth
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Truth{}, fmt.Errorf("the truth is not read: %w", err)
	}
	for i, p := range t.Pages {
		for j, b := range p.Blocks {
			if err := b.check(); err != nil {
				return Truth{}, fmt.Errorf("block %s: %w", document.Ref(i+1, j+1), err)
			}
		}
	}
	for _, s := range t.Spans {
		for _, part := range s.Parts {
			if b, ok := t.block(part); !ok || b.Kind != document.KindTable {
				return Truth{}, fmt.Errorf("span %s names %s, which is no table of the truth", s.ID, part)
			}
		}
	}
	for _, h := range t.Outline {
		b, ok := t.block(h.Ref)
		if !ok || (b.Kind != document.KindTitle && b.Kind != document.KindHeading) || b.Text != h.Text {
			return Truth{}, fmt.Errorf("the outline names %s %q, which is no heading of the truth", h.Ref, h.Text)
		}
	}
	return t, nil
}

func (b Block) check() error {
	switch {
	case !b.Kind.Valid():
		return fmt.Errorf("unknown kind %q", b.Kind)
	case b.Box != nil && !b.Box.Valid():
		return fmt.Errorf("its box lies outside the page or has no area")
	case b.Kind != document.KindTable && (len(b.Cells) > 0 || b.Rows > 0 || b.Cols > 0):
		return fmt.Errorf("it is %s and holds cells", b.Kind)
	case b.Kind == document.KindTable && len(b.Cells) == 0:
		return fmt.Errorf("it is a table with no cell")
	}
	for _, c := range b.Cells {
		if c.Row < 0 || c.Col < 0 || c.Row+span(c.RowSpan) > b.Rows || c.Col+span(c.ColSpan) > b.Cols {
			return fmt.Errorf("cell %d,%d lies outside its table of %d rows by %d columns", c.Row, c.Col, b.Rows, b.Cols)
		}
	}
	return nil
}

// block returns the block a ref addresses.
func (t Truth) block(ref string) (Block, bool) {
	page, order, err := document.ParseRef(ref)
	if err != nil || page > len(t.Pages) || order > len(t.Pages[page-1].Blocks) {
		return Block{}, false
	}
	return t.Pages[page-1].Blocks[order-1], true
}

// Select returns the truth of a file that holds only some of the pages, in
// the order given: the pages renumbered from 1, and the spans and the
// outline kept where every block they address is still there. A span that
// loses a part is dropped, since what is left of it is one table or a join
// the truth never stated. numbers counts from 1, and a number the truth
// does not have selects a blank page.
func (t Truth) Select(numbers ...int) Truth {
	out := Truth{Source: t.Source, Pages: make([]Page, 0, len(numbers))}
	renumbered := map[int]int{}
	for i, n := range numbers {
		page := Page{}
		if n >= 1 && n <= len(t.Pages) {
			page = t.Pages[n-1]
			renumbered[n] = i + 1
		}
		out.Pages = append(out.Pages, page)
	}
	move := func(ref string) (string, bool) {
		page, order, err := document.ParseRef(ref)
		if err != nil || renumbered[page] == 0 {
			return "", false
		}
		return document.Ref(renumbered[page], order), true
	}
	for _, s := range t.Spans {
		kept := document.Span{ID: s.ID, Rows: s.Rows, Cols: s.Cols}
		for _, part := range s.Parts {
			if ref, ok := move(part); ok {
				kept.Parts = append(kept.Parts, ref)
			}
		}
		if len(kept.Parts) == len(s.Parts) {
			out.Spans = append(out.Spans, kept)
		}
	}
	for _, h := range t.Outline {
		if ref, ok := move(h.Ref); ok {
			page, _, _ := document.ParseRef(ref)
			out.Outline = append(out.Outline, document.Heading{Ref: ref, Level: h.Level, Text: h.Text, Page: page})
		}
	}
	return out
}

// Document returns the truth as the pages a reader that made no mistake
// would return: every block numbered, with its kind, level, text and box,
// and a table with its cells and its text as a block's text is written.
// It is what the measures score as perfect, and what assembly is run over
// to see that it finds the truth's spans and outline.
func (t Truth) Document() []document.Page {
	pages := make([]document.Page, 0, len(t.Pages))
	for i, p := range t.Pages {
		blocks := make([]document.Block, 0, len(p.Blocks))
		for j, b := range p.Blocks {
			block := document.Block{Kind: b.Kind, Order: j + 1, Level: b.Level, Text: b.Text}
			if b.Box != nil {
				box := *b.Box
				block.Box = &box
			}
			if b.Kind == document.KindTable {
				block.Table = &document.Table{Rows: b.Rows, Cols: b.Cols, Cells: append([]document.Cell(nil), b.Cells...)}
				block.Text = tableText(b)
			}
			blocks = append(blocks, block)
		}
		pages = append(pages, document.Page{
			Number: i + 1, State: document.PageSucceeded, Source: document.SourceReader,
			Blocks: document.Number(i+1, blocks), Usage: &document.Usage{Pages: 1},
		})
	}
	return pages
}

// tableText is a table as a block's text: the cells of a row joined by
// " | " and the rows by a newline.
func tableText(b Block) string {
	rows := make([][]string, b.Rows)
	for _, c := range b.Cells {
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
