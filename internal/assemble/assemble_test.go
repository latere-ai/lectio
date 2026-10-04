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
// runs from page 2 onto page 3. The readers labeled the title a header on
// one page and the page number on one page; the rest came back as text.
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
		// "Page n of 4" is a page number on every page, the one its reader
		// labeled and the three it did not. A page number is never a footer
		// and never repeats: each says something else.
		if foot.Kind != document.KindPageNumber || foot.Repeated {
			t.Errorf("page %d page number: %+v", p.Number, foot)
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

// TestADocumentKeepsTheTitleOfItsFirstTitledPage: a reader sees one page
// and calls the line each slide of a deck opens with a title. The document
// has the title of its first page that holds one, both lines of it when the
// reader split it in 2, and a title on a later page is a heading: it keeps
// its text and its place, and the outline puts it below the title. A page
// before the title that has none changes nothing, and a title that runs
// from page to page is furniture before this rule looks.
func TestADocumentKeepsTheTitleOfItsFirstTitledPage(t *testing.T) {
	pages := []document.Page{
		page(1, text(document.KindText, "Cover note")),
		page(2, text(document.KindTitle, "Harbor works"), text(document.KindTitle, "Autumn plan"), text(document.KindText, "body")),
		page(3, document.Block{Kind: document.KindTitle, Text: "Work packages", Level: 1}, text(document.KindText, "body")),
		page(4, text(document.KindTitle, "Budget by quarter"), document.Block{Kind: document.KindHeading, Text: "Detail", Level: 3}),
	}
	doc := Document("p", pages)
	var kinds []document.Kind
	for _, p := range pages {
		for _, b := range p.Blocks {
			if b.Kind == document.KindTitle || b.Kind == document.KindHeading {
				kinds = append(kinds, b.Kind)
			}
		}
	}
	want := []document.Kind{document.KindTitle, document.KindTitle, document.KindHeading, document.KindHeading, document.KindHeading}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("the titles and headings are %v, want %v", kinds, want)
	}
	if pages[2].Blocks[0].Text != "Work packages" || pages[2].Blocks[0].Ref != "3.1" {
		t.Errorf("the block that became a heading is %+v; it keeps its text and its place", pages[2].Blocks[0])
	}
	levels := map[string]int{}
	for _, h := range doc.Outline {
		levels[h.Text] = h.Level
	}
	if levels["Harbor works"] != 1 || levels["Work packages"] != 2 || levels["Budget by quarter"] != 2 || levels["Detail"] != 3 {
		t.Errorf("the outline is %+v; a later page's title is a section under the document's", doc.Outline)
	}

	// A format that carries its own structure names its own titles: each
	// sheet of a workbook keeps the title its sheet gave it.
	sheets := []document.Page{
		page(1, text(document.KindTitle, "Rainfall")),
		page(2, text(document.KindTitle, "Stations")),
	}
	for i := range sheets {
		sheets[i].Source = document.SourceNative
	}
	Document("p", sheets)
	if sheets[1].Blocks[0].Kind != document.KindTitle {
		t.Errorf("a native page's title became a %s; nothing was guessed there", sheets[1].Blocks[0].Kind)
	}

	// A document whose every page opens with the same line: the line is a
	// running header on each, and none of them is left a title or a heading.
	deck := []document.Page{
		page(1, text(document.KindTitle, "Quarterly review"), text(document.KindText, "one")),
		page(2, text(document.KindTitle, "Quarterly review"), text(document.KindText, "two")),
		page(3, text(document.KindTitle, "Quarterly review"), text(document.KindText, "three")),
	}
	Document("p", deck)
	for _, p := range deck {
		if k := p.Blocks[0].Kind; k != document.KindPageHeader && k != document.KindTitle {
			t.Errorf("page %d opens with a %s; a line that runs from page to page is furniture or the title, never a heading", p.Number, k)
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
		"<!-- page 1 -->\n\nACME Annual Report\n\n# Annual Report\n\nIntro.\n\n<!-- page 2 -->\n",
		"<!-- page 2 -->\n\n## Results\n\n| r0c0 | r0c1 |\n| --- | --- |\n| r1c0 | r1c1 |\n| r2c0 | r2c1 |\n",
		"<!-- page 4 -->\n\n## Outlook\n\nClosing.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown lacks:\n%s\ngot:\n%s", want, got)
		}
	}
	if strings.Count(got, "ACME") != 1 || strings.Contains(got, "Page ") {
		t.Errorf("a running header is printed once and a page number is not printed:\n%s", got)
	}

	out.Reset()
	if err := Markdown(&out, pages, doc.Outline, View{Repeated: RepeatedKeep, Pages: []int{2, 3}, Tables: TablesHTML}); err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if strings.Count(got, "ACME") != 2 || strings.Count(got, "Page ") != 2 || strings.Contains(got, "Outlook") || !strings.Contains(got, "<table>…</table>") || strings.Contains(got, "<!--") {
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
		"figure":               {document.Block{Kind: document.KindFigure, Description: "A bar\nchart."}, "", "*[Figure: A bar chart.]*"},
		"figure, undescribed":  {text(document.KindFigure, ""), "", ""},
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
	if want := "ACME Annual Report\n\nAnnual Report\n\nIntro.\n\nOutlook\n\nClosing.\n"; out.String() != want {
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

	// A chunk holds what the document says: the running header and the
	// page numbers are in none.
	byPage := rows(Chunks(pages, ByPage, 6000))
	if len(byPage) != 4 || byPage[0].id != "c1" || !reflect.DeepEqual(byPage[1].pages, []int{2}) || !reflect.DeepEqual(byPage[1].blocks, []string{"2.2", "2.3"}) {
		t.Fatalf("by page: %+v", byPage)
	}
	if byPage[0].text != "Annual Report\n\nIntro." {
		t.Fatalf("a page's furniture is left out of its chunk: %+v", byPage[0])
	}

	bySection := rows(Chunks(pages, BySection, 6000))
	want := []row{
		{"c1", "Annual Report\n\nIntro.", []int{1}, []string{"1.2", "1.3"}},
		{"c2", "Results\n\ncells\n\ncells\n\nAfter the table.", []int{2, 3}, []string{"2.2", "2.3", "3.2", "3.3"}},
		{"c3", "Outlook\n\nClosing.", []int{4}, []string{"4.2", "4.3"}},
	}
	if !reflect.DeepEqual(bySection, want) {
		t.Fatalf("by section:\n got %+v\nwant %+v", bySection, want)
	}

	// Every block of content is in exactly one chunk, and no chunk is past
	// the bound unless it is one block or a heading with the block under it.
	small := Chunks(pages, BySection, 30)
	seen := map[string]int{}
	for _, c := range small {
		for _, ref := range c.Blocks {
			seen[ref]++
		}
		if len(c.Blocks) > 2 && len(c.Text) > 30 {
			t.Errorf("chunk %s is %d characters over %d blocks", c.ID, len(c.Text), len(c.Blocks))
		}
	}
	if len(seen) != 8 {
		t.Fatalf("8 blocks are content, chunks cover %d: %v", len(seen), seen)
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
	// Each count stops at a different write: the first page's marker, the
	// blank line before a block, a block, and, at the eighth and ninth
	// write, the blank line before the second page's marker and the marker.
	for n := range 10 {
		if err := Markdown(&failAfter{n}, pages, doc.Outline, View{PageBreaks: true}); err == nil {
			t.Errorf("Markdown must report a failed write (after %d)", n)
		}
	}
	for n := range 3 {
		if err := Text(&failAfter{n}, pages, View{}); err == nil {
			t.Errorf("Text must report a failed write (after %d)", n)
		}
	}
}
