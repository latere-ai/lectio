// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package assemble

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
)

func text(kind document.Kind, s string) document.Block {
	return document.Block{Kind: kind, Text: s}
}

func table(rows, cols int) document.Block {
	t := &document.Table{Rows: rows, Cols: cols, HTML: "<table>…</table>"}
	for r := range rows {
		for c := range cols {
			t.Cells = append(t.Cells, document.Cell{Row: r, Col: c, Text: fmt.Sprintf("r%dc%d", r, c)})
		}
	}
	return document.Block{Kind: document.KindTable, Table: t, Text: "cells"}
}

func page(n int, blocks ...document.Block) document.Page {
	return document.Page{Number: n, State: document.PageSucceeded, Source: document.SourceReader, Blocks: document.Number(n, blocks), Usage: &document.Usage{Pages: 1, InputTokens: 10, OutputTokens: 2}}
}

func failed(n int) document.Page {
	return document.Page{Number: n, State: document.PageFailed, Error: &document.Error{Code: "page_unreadable"}}
}

// A 4-page report with a running title, "Page n of 4", and a table that
// runs from page 2 onto page 3.
func report() []document.Page {
	return []document.Page{
		page(1, text(document.KindText, "ACME Annual Report"), text(document.KindTitle, "Annual Report"), text(document.KindText, "Intro."), text(document.KindText, "Page 1 of 4")),
		page(2, text(document.KindText, "ACME  annual report"), text(document.KindHeading, "Results"), table(3, 2), text(document.KindPageNumber, "Page 2 of 4")),
		page(3, text(document.KindPageHeader, "ACME Annual Report"), table(5, 2), text(document.KindText, "After the table."), text(document.KindText, "Page 3 of 4")),
		page(4, text(document.KindText, "ACME Annual Report"), text(document.KindHeading, "Outlook"), text(document.KindText, "Closing."), text(document.KindText, "Page 4 of 4")),
	}
}

func TestDocumentFindsRunningHeadersAndFooters(t *testing.T) {
	pages := report()
	doc := Document("prs_1", pages)

	for i, p := range pages {
		head, foot := p.Blocks[0], p.Blocks[len(p.Blocks)-1]
		if head.Kind != document.KindPageHeader || head.Repeated != (i > 0) {
			t.Errorf("page %d header: %+v", p.Number, head)
		}
		// Page 2's footer was labeled a page number by its reader; the pass
		// looks at textual kinds, and a page number is one.
		if foot.Kind != document.KindPageFooter || foot.Repeated != (i > 0) {
			t.Errorf("page %d footer: %+v", p.Number, foot)
		}
	}
	if doc.Parse != "prs_1" || len(doc.Pages) != 4 || doc.Pages[1].Blocks != 4 {
		t.Fatalf("index = %+v", doc)
	}
	if doc.Usage != (document.Usage{Pages: 4, InputTokens: 40, OutputTokens: 8}) {
		t.Fatalf("usage = %+v", doc.Usage)
	}
}

func TestRunningNeedsHalfThePages(t *testing.T) {
	pages := []document.Page{
		page(1, text(document.KindText, "Same top"), text(document.KindText, "a")),
		page(2, text(document.KindText, "Same top"), text(document.KindText, "b")),
		page(3, text(document.KindText, "Other"), text(document.KindText, "c")),
		page(4, text(document.KindText, "Another"), text(document.KindText, "d")),
		page(5, text(document.KindText, "Yet another"), text(document.KindText, "e")),
		page(6, text(document.KindTable, "not text"), text(document.KindText, strings.Repeat("long ", 60))),
		page(7), failed(8),
	}
	Document("prs_1", pages)
	if pages[0].Blocks[0].Kind != document.KindText {
		t.Fatal("a line on 2 of 6 pages is not a running header")
	}

	two := []document.Page{page(1, text(document.KindText, "Only block")), page(2, text(document.KindText, "Only block"))}
	Document("prs_1", two)
	if two[0].Blocks[0].Kind != document.KindPageHeader || two[1].Blocks[0].Kind != document.KindPageHeader || !two[1].Blocks[0].Repeated {
		t.Fatalf("a page's only block is its top edge, and 2 of 2 pages is enough: %+v", two)
	}
}

func TestDocumentJoinsTablesAcrossPages(t *testing.T) {
	doc := Document("prs_1", report())
	want := []document.Span{{ID: "s1", Parts: []string{"2.3", "3.2"}, Rows: 8, Cols: 2}}
	if !reflect.DeepEqual(doc.Spans, want) {
		t.Fatalf("spans = %+v, want %+v", doc.Spans, want)
	}

	for name, tc := range map[string]struct {
		pages []document.Page
		want  []string
	}{
		"three pages of one table": {
			[]document.Page{page(1, text(document.KindText, "a"), table(2, 3)), page(2, table(9, 3)), page(3, table(1, 3), text(document.KindText, "z"))},
			[]string{"1.2,2.1,3.1"},
		},
		"different column counts": {
			[]document.Page{page(1, table(2, 3)), page(2, table(2, 4))},
			nil,
		},
		"a failed page between": {
			[]document.Page{page(1, table(2, 3)), failed(2), page(3, table(2, 3))},
			nil,
		},
		"pages that are not adjacent": {
			[]document.Page{page(1, table(2, 3)), page(3, table(2, 3))},
			nil,
		},
		"text between the tables": {
			[]document.Page{page(1, table(2, 3)), page(2, text(document.KindText, "x"), table(2, 3))},
			nil,
		},
		"two spans": {
			[]document.Page{page(1, table(2, 3)), page(2, table(2, 3), text(document.KindText, "x"), table(1, 5)), page(3, table(4, 5))},
			[]string{"1.1,2.1", "2.3,3.1"},
		},
		"a table with no structure": {
			[]document.Page{page(1, text(document.KindTable, "a | b")), page(2, text(document.KindTable, "c | d"))},
			nil,
		},
	} {
		var got []string
		for _, s := range Document("p", tc.pages).Spans {
			got = append(got, strings.Join(s.Parts, ","))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: spans = %v, want %v", name, got, tc.want)
		}
	}
}

func TestOutlineBringsLevelsOntoOneScale(t *testing.T) {
	pages := []document.Page{
		page(1, document.Block{Kind: document.KindHeading, Text: "A", Level: 2}, document.Block{Kind: document.KindHeading, Text: "B", Level: 4}),
		page(2, document.Block{Kind: document.KindHeading, Text: "C", Level: 2}, text(document.KindText, "body")),
	}
	got := Document("p", pages).Outline
	want := []document.Heading{{Ref: "1.1", Level: 1, Text: "A", Page: 1}, {Ref: "1.2", Level: 2, Text: "B", Page: 1}, {Ref: "2.1", Level: 1, Text: "C", Page: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outline = %+v, want %+v", got, want)
	}

	unleveled := Document("p", []document.Page{page(1, text(document.KindHeading, "Section"), text(document.KindTitle, "Title"))}).Outline
	if unleveled[0].Level != 2 || unleveled[1].Level != 1 {
		t.Fatalf("a title with no level is above a heading with none: %+v", unleveled)
	}
	if got := Document("p", []document.Page{page(1, text(document.KindText, "x"))}).Outline; got != nil {
		t.Fatalf("no headings, no outline: %+v", got)
	}
}

func TestAssemblyIsDeterministic(t *testing.T) {
	a, b := report(), report()
	if !reflect.DeepEqual(Document("p", a), Document("p", b)) || !reflect.DeepEqual(a, b) {
		t.Fatal("assembling the same pages twice must give the same document")
	}
	again := Document("p", a)
	if !reflect.DeepEqual(again, Document("p", b)) {
		t.Fatal("assembling assembled pages must change nothing")
	}
}

func TestMarkdown(t *testing.T) {
	pages := report()
	doc := Document("p", pages)

	var out bytes.Buffer
	if err := Markdown(&out, pages, doc.Outline, View{PageBreaks: true}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"<!-- page 1 -->\n\nACME Annual Report\n\n# Annual Report\n\nIntro.\n\nPage 1 of 4\n",
		"<!-- page 2 -->\n\n## Results\n\n| r0c0 | r0c1 |\n| --- | --- |\n| r1c0 | r1c1 |\n| r2c0 | r2c1 |\n",
		"<!-- page 4 -->\n\n## Outlook\n\nClosing.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown lacks:\n%s\ngot:\n%s", want, got)
		}
	}
	if strings.Count(got, "ACME") != 1 || strings.Count(got, "Page ") != 1 {
		t.Errorf("a running header and footer are printed once:\n%s", got)
	}

	out.Reset()
	if err := Markdown(&out, pages, doc.Outline, View{Repeated: RepeatedKeep, Pages: []int{2, 3}, Tables: TablesHTML}); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if strings.Count(got, "ACME") != 2 || strings.Contains(got, "Outlook") || !strings.Contains(got, "<table>…</table>") || strings.Contains(got, "<!--") {
		t.Errorf("kept, limited to pages 2 and 3, tables as HTML:\n%s", got)
	}

	out.Reset()
	if err := Markdown(&out, pages, nil, View{Repeated: RepeatedDrop, Pages: []int{4}}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "## Outlook\n\nClosing.\n" {
		t.Errorf("dropped, and without an outline a heading keeps its own level:\n%q", got)
	}
}

func TestMarkdownBlocks(t *testing.T) {
	spanning := table(2, 2)
	spanning.Table.Cells[0].ColSpan = 2
	bare := table(1, 1)
	bare.Table.HTML = ""
	bare.Table.Cells[0].Text = "a|b\nc"

	for name, tc := range map[string]struct {
		block  document.Block
		tables string
		want   string
	}{
		"list item":            {text(document.KindListItem, "first\n item"), "", "- first item"},
		"formula":              {text(document.KindFormula, "E = mc^2"), "", "$$\nE = mc^2\n$$"},
		"code":                 {text(document.KindCode, "x := 1"), "", "```\nx := 1\n```"},
		"figure":               {text(document.KindFigure, "A bar chart."), "", "*Figure: A bar chart.*"},
		"figure, undescribed":  {text(document.KindFigure, ""), "", "*Figure.*"},
		"caption":              {text(document.KindCaption, "Table 1"), "", "*Table 1*"},
		"deep heading":         {document.Block{Kind: document.KindHeading, Text: "Deep", Level: 9}, "", "###### Deep"},
		"spanning, auto":       {spanning, TablesAuto, "<table>…</table>"},
		"spanning, markdown":   {spanning, TablesMarkdown, "| r0c0 | r0c1 |\n| --- | --- |\n| r1c0 | r1c1 |"},
		"no markup, html":      {bare, TablesHTML, "| a\\|b c |\n| --- |"},
		"no structure":         {text(document.KindTable, "a | b"), "", "a | b"},
		"cells past the table": {document.Block{Kind: document.KindTable, Table: &document.Table{Rows: 1, Cols: 1, Cells: []document.Cell{{Row: 0, Col: 0, Text: "in"}, {Row: 3, Col: 3, Text: "out"}}}}, "", "| in |\n| --- |"},
	} {
		if got := markdownBlock(tc.block, nil, tc.tables); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

func TestText(t *testing.T) {
	pages := report()
	Document("p", pages)
	pages = append(pages, failed(5), page(6, text(document.KindFigure, "")))

	var out bytes.Buffer
	if err := Text(&out, pages, View{Pages: []int{1, 4, 5, 6}}); err != nil {
		t.Fatal(err)
	}
	if want := "ACME Annual Report\n\nAnnual Report\n\nIntro.\n\nPage 1 of 4\n\nOutlook\n\nClosing.\n"; out.String() != want {
		t.Fatalf("text:\n got %q\nwant %q", out.String(), want)
	}
}

func TestChunks(t *testing.T) {
	pages := report()
	Document("p", pages)

	type row struct {
		id, text string
		pages    []int
		blocks   []string
	}
	rows := func(cs []document.Chunk) (out []row) {
		for _, c := range cs {
			out = append(out, row{c.ID, c.Text, c.Pages, c.Blocks})
		}
		return out
	}

	byPage := rows(Chunks(pages, ByPage, 6000))
	if len(byPage) != 4 || byPage[0].id != "c1" || !reflect.DeepEqual(byPage[1].pages, []int{2}) || !reflect.DeepEqual(byPage[1].blocks, []string{"2.2", "2.3"}) {
		t.Fatalf("by page: %+v", byPage)
	}
	if !strings.HasPrefix(byPage[0].text, "ACME Annual Report\n\nAnnual Report") || strings.Contains(byPage[1].text, "ACME") {
		t.Fatalf("a repeated header is in the first chunk and no other: %+v", byPage)
	}

	bySection := rows(Chunks(pages, BySection, 6000))
	want := []row{
		{"c1", "ACME Annual Report", []int{1}, []string{"1.1"}},
		{"c2", "Annual Report\n\nIntro.\n\nPage 1 of 4", []int{1}, []string{"1.2", "1.3", "1.4"}},
		{"c3", "Results\n\ncells\n\ncells\n\nAfter the table.", []int{2, 3}, []string{"2.2", "2.3", "3.2", "3.3"}},
		{"c4", "Outlook\n\nClosing.", []int{4}, []string{"4.2", "4.3"}},
	}
	if !reflect.DeepEqual(bySection, want) {
		t.Fatalf("by section:\n got %+v\nwant %+v", bySection, want)
	}

	// Every block that is shown is in exactly one chunk, and no chunk of
	// more than one block is past the bound.
	small := Chunks(pages, BySection, 30)
	seen := map[string]int{}
	for _, c := range small {
		for _, ref := range c.Blocks {
			seen[ref]++
		}
		if len(c.Blocks) > 1 && len(c.Text) > 30 {
			t.Errorf("chunk %s is %d characters over %d blocks", c.ID, len(c.Text), len(c.Blocks))
		}
	}
	if len(seen) != 10 {
		t.Fatalf("10 blocks are shown, chunks cover %d: %v", len(seen), seen)
	}
	for ref, n := range seen {
		if n != 1 {
			t.Errorf("block %s is in %d chunks", ref, n)
		}
	}

	if got := Chunks(nil, ByPage, 100); got != nil {
		t.Fatalf("no pages, no chunks: %+v", got)
	}
}

// failAfter is a writer that fails once it has taken n writes.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("the reader went away")
	}
	f.n--
	return len(p), nil
}

func TestViewsReportAWriterThatFails(t *testing.T) {
	pages := report()
	doc := Document("p", pages)
	// Each count stops at a different write: a page marker, the blank line
	// before it, a block, the blank line before a block.
	for n := range 6 {
		if err := Markdown(&failAfter{n}, pages, doc.Outline, View{PageBreaks: true}); err == nil {
			t.Errorf("Markdown must report a failed write (after %d)", n)
		}
	}
	for n := range 3 {
		if err := Text(&failAfter{n}, pages, View{}); err == nil {
			t.Errorf("Text must report a failed write (after %d)", n)
		}
	}
	if err := Markdown(&failAfter{16}, pages, doc.Outline, View{PageBreaks: true}); err == nil {
		t.Error("Markdown must report a failed write at the second page's marker")
	}
}
