// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package assemble

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
)

// boxed places a block on its page: x0, y0, x1, y1 as fractions.
func boxed(b document.Block, x0, y0, x1, y1 float64) document.Block {
	b.Box = &document.Box{x0, y0, x1, y1}
	return b
}

func markdown(t *testing.T, pages []document.Page, v View) string {
	t.Helper()
	doc := Document("p", pages)
	var out bytes.Buffer
	if err := Markdown(&out, pages, doc.Outline, v); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Lines that differ only by their numbers are different content. Folding
// every run of digits into one made a statement's balances, a batch's
// invoice numbers and a deck's step titles look like one running line, and
// all but the first were hidden.
func TestContentThatDiffersByItsNumbersIsNotARunningLine(t *testing.T) {
	var statement, batch, deck []document.Page
	for n := 1; n <= 6; n++ {
		statement = append(statement, page(n,
			text(document.KindText, fmt.Sprintf("Transactions of week %d follow", n)),
			text(document.KindText, "Rows."),
			text(document.KindText, fmt.Sprintf("Balance carried forward %d,234.56", n))))
		batch = append(batch, page(n,
			document.Block{Kind: document.KindHeading, Text: fmt.Sprintf("Invoice No. 104%d", n), Level: 1},
			text(document.KindText, "Lines.")))
		deck = append(deck, page(n,
			text(document.KindTitle, fmt.Sprintf("Step %d", n)),
			text(document.KindText, "Do this.")))
	}

	out := markdown(t, statement, View{})
	for n, p := range statement {
		last := p.Blocks[len(p.Blocks)-1]
		if last.Kind != document.KindText || last.Repeated {
			t.Errorf("statement page %d: the balance became %s, repeated %v", p.Number, last.Kind, last.Repeated)
		}
		if want := fmt.Sprintf("Balance carried forward %d,234.56", n+1); !strings.Contains(out, want) {
			t.Errorf("the Markdown lost %q", want)
		}
	}

	doc := Document("p", batch)
	for _, p := range batch {
		if first := p.Blocks[0]; first.Kind != document.KindHeading || first.Level != 1 || first.Repeated {
			t.Errorf("batch page %d: the invoice heading became %+v", p.Number, first)
		}
	}
	if len(doc.Outline) != 6 {
		t.Errorf("the outline holds %d of 6 invoices: %+v", len(doc.Outline), doc.Outline)
	}

	// A deck's pages each open with a line a reader called a title. None
	// is furniture: the first is the document's title, and each later one
	// is a heading under it, printed in full.
	out = markdown(t, deck, View{})
	for n, p := range deck {
		first, kind, mark := p.Blocks[0], document.KindHeading, "## "
		if n == 0 {
			kind, mark = document.KindTitle, "# "
		}
		if first.Kind != kind || first.Repeated {
			t.Errorf("deck page %d: the line it opens with became %+v, want a %s that is not repeated", p.Number, first, kind)
		}
		if want := fmt.Sprintf("%sStep %d", mark, n+1); !strings.Contains(out, want) {
			t.Errorf("the Markdown lost %q", want)
		}
	}
}

func TestAPageNumberIsFoundByWhatItSays(t *testing.T) {
	for _, expr := range []string{"3", "Page 3", "page 3 of 40", "3 / 40", "3/40", "- 3 -", "[3]", "(3)", "p. 3", "Seite 3 von 40", "Página 3 de 40", "pagina 3 di 40", "3."} {
		for name, at := range map[string]func(document.Block) []document.Block{
			"last":  func(b document.Block) []document.Block { return []document.Block{text(document.KindText, "Body."), b} },
			"first": func(b document.Block) []document.Block { return []document.Block{b, text(document.KindText, "Body.")} },
			"only":  func(b document.Block) []document.Block { return []document.Block{b} },
		} {
			pages := []document.Page{page(1, at(text(document.KindText, expr))...)}
			Document("p", pages)
			found := false
			for _, b := range pages[0].Blocks {
				found = found || (b.Text == expr && b.Kind == document.KindPageNumber)
			}
			if !found {
				t.Errorf("%q as the %s block is a page number: %+v", expr, name, pages[0].Blocks)
			}
		}
	}

	// What only looks like one stays content.
	for _, body := range []string{"1,234.56", "12345", "2024 was a good year", "Page break", "Total 3", "3 apples", "Chapter 3", "p. 3 and following"} {
		pages := []document.Page{page(1, text(document.KindText, "Body."), text(document.KindText, body))}
		Document("p", pages)
		if got := pages[0].Blocks[1].Kind; got != document.KindText {
			t.Errorf("%q became %s", body, got)
		}
	}
	// A number in the middle of a page, or one a reader called a heading, is
	// not a page number, and a number that sits away from the page's edge is
	// not one either.
	pages := []document.Page{page(1,
		document.Block{Kind: document.KindHeading, Text: "3"},
		text(document.KindText, "Body."),
		text(document.KindText, "7"),
		text(document.KindText, "More."),
		boxed(text(document.KindText, "9"), 0.4, 0.5, 0.6, 0.55))}
	Document("p", pages)
	for i, want := range []document.Kind{document.KindHeading, document.KindText, document.KindText, document.KindText, document.KindText} {
		if got := pages[0].Blocks[i].Kind; got != want {
			t.Errorf("block %d is %s, want %s", i+1, got, want)
		}
	}

	// A reader's own label for a page number is kept, and a header or a
	// footer that is only a page number is one, wherever it is on the page.
	labeled := []document.Page{
		page(1, text(document.KindText, "Body."), text(document.KindPageNumber, "Page 1 of 3")),
		page(2, text(document.KindText, "Body."), text(document.KindPageFooter, "2")),
		page(3, text(document.KindText, "Body."), text(document.KindPageHeader, "- 3 -"), text(document.KindFootnote, "1 See the appendix.")),
	}
	Document("p", labeled)
	for _, p := range labeled {
		if number := p.Blocks[1]; number.Kind != document.KindPageNumber || number.Repeated {
			t.Errorf("page %d: %+v", p.Number, number)
		}
	}
}

// The report every other pass is measured on: forty pages, each under the
// same running title and above "Page n of 40", none of them labeled by a
// reader.
func TestAFortyPageReport(t *testing.T) {
	var pages []document.Page
	for n := 1; n <= 40; n++ {
		pages = append(pages, page(n,
			text(document.KindText, "ACME Annual Report"),
			text(document.KindText, fmt.Sprintf("The text of page %d.", n)),
			text(document.KindText, fmt.Sprintf("Page %d of 40", n))))
	}
	out := markdown(t, pages, View{})
	headers, repeated, numbers := 0, 0, 0
	for _, p := range pages {
		if p.Blocks[0].Kind == document.KindPageHeader {
			headers++
		}
		if p.Blocks[0].Repeated {
			repeated++
		}
		if p.Blocks[2].Kind == document.KindPageNumber && !p.Blocks[2].Repeated {
			numbers++
		}
	}
	if headers != 40 || repeated != 39 || numbers != 40 {
		t.Fatalf("%d headers, %d of them repeated, %d page numbers", headers, repeated, numbers)
	}
	if strings.Count(out, "ACME Annual Report") != 1 || strings.Contains(out, "of 40") || strings.Count(out, "The text of page") != 40 {
		t.Fatalf("the title once, no page number, every page's text:\n%s", out[:200])
	}
}

// A page number says nothing about the document, so a rendering leaves it
// out unless the caller asks for every block.
func TestAPageNumberIsPrintedOnlyWhenEverythingIsKept(t *testing.T) {
	pages := []document.Page{
		page(1, text(document.KindText, "First."), text(document.KindPageFooter, "5")),
		page(2, text(document.KindText, "Second."), text(document.KindText, "6")),
	}
	for repeated, want := range map[string]string{
		"":           "First.\n\nSecond.\n",
		RepeatedOnce: "First.\n\nSecond.\n",
		RepeatedDrop: "First.\n\nSecond.\n",
		RepeatedKeep: "First.\n\n5\n\nSecond.\n\n6\n",
	} {
		if got := markdown(t, pages, View{Repeated: repeated}); got != want {
			t.Errorf("repeated=%q:\n got %q\nwant %q", repeated, got, want)
		}
	}
	var out bytes.Buffer
	if err := Text(&out, pages, View{}); err != nil || out.String() != "First.\n\nSecond.\n" {
		t.Errorf("text: %q, %v", out.String(), err)
	}
	if chunks := Chunks(pages, ByPage, 100); len(chunks) != 2 || chunks[0].Text != "First." || chunks[1].Text != "Second." {
		t.Errorf("chunks: %+v", chunks)
	}
}

// A line that repeats is a running header or footer only where one sits:
// when a reader gives positions, in the band at the page's top or bottom.
func TestARunningLineSitsAtThePagesEdge(t *testing.T) {
	doc := func(y0, y1 float64) []document.Page {
		var pages []document.Page
		for n := 1; n <= 4; n++ {
			pages = append(pages, page(n,
				boxed(text(document.KindText, "Confidential"), 0.1, y0, 0.9, y1),
				boxed(text(document.KindText, fmt.Sprintf("Body of page %d.", n)), 0.1, 0.3, 0.9, 0.6),
				boxed(text(document.KindText, "Internal use only"), 0.1, 1-y1, 0.9, 1-y0)))
		}
		Document("p", pages)
		return pages
	}

	for _, p := range doc(0.03, 0.06) {
		head, foot := p.Blocks[0], p.Blocks[2]
		if head.Kind != document.KindPageHeader || foot.Kind != document.KindPageFooter || head.Repeated != (p.Number > 1) || foot.Repeated != (p.Number > 1) {
			t.Errorf("at the edge, page %d: %+v, %+v", p.Number, head, foot)
		}
	}
	for _, p := range doc(0.2, 0.25) {
		if head, foot := p.Blocks[0], p.Blocks[2]; head.Kind != document.KindText || foot.Kind != document.KindText || head.Repeated || foot.Repeated {
			t.Errorf("away from the edge, page %d: %+v, %+v", p.Number, head, foot)
		}
	}
}

// What a reader labeled as furniture stays furniture, and its repeats are
// found even when a page number is part of the line.
func TestAReadersFurnitureIsKeptAndItsRepeatsAreFound(t *testing.T) {
	var pages []document.Page
	for n := 1; n <= 3; n++ {
		pages = append(pages, page(n,
			text(document.KindPageHeader, fmt.Sprintf("ACME Report 2024 · %d", n)),
			text(document.KindText, fmt.Sprintf("Body %d.", n)),
			text(document.KindPageFooter, "acme.example"),
			text(document.KindPageNumber, fmt.Sprintf("%d", n))))
	}
	out := markdown(t, pages, View{})
	for _, p := range pages {
		head, foot, num := p.Blocks[0], p.Blocks[2], p.Blocks[3]
		if head.Kind != document.KindPageHeader || foot.Kind != document.KindPageFooter || num.Kind != document.KindPageNumber {
			t.Errorf("page %d: kinds %s, %s, %s", p.Number, head.Kind, foot.Kind, num.Kind)
		}
		if head.Repeated != (p.Number > 1) || foot.Repeated != (p.Number > 1) || num.Repeated {
			t.Errorf("page %d: repeated %v, %v, %v", p.Number, head.Repeated, foot.Repeated, num.Repeated)
		}
	}
	if want := "ACME Report 2024 · 1\n\nBody 1.\n\nacme.example\n\nBody 2.\n\nBody 3.\n"; out != want {
		t.Errorf("markdown:\n got %q\nwant %q", out, want)
	}

	// A footer line above the page number is still the page's bottom edge.
	var lined []document.Page
	for n := 1; n <= 3; n++ {
		lined = append(lined, page(n, text(document.KindText, fmt.Sprintf("Body %d.", n)), text(document.KindText, "Printed on recycled paper"), text(document.KindText, fmt.Sprintf("%d", n))))
	}
	Document("p", lined)
	for _, p := range lined {
		if foot := p.Blocks[1]; foot.Kind != document.KindPageFooter || foot.Repeated != (p.Number > 1) {
			t.Errorf("page %d: the line above the page number: %+v", p.Number, foot)
		}
	}

	// Assembling again changes nothing, whatever a first pass relabeled.
	mixed := func() []document.Page {
		return []document.Page{
			page(1, text(document.KindText, "Report 2023"), text(document.KindText, "a")),
			page(2, text(document.KindText, "Report 2023"), text(document.KindText, "b")),
			page(3, text(document.KindText, "Report 2023"), text(document.KindText, "c")),
			page(4, text(document.KindPageHeader, "Report 2024"), text(document.KindText, "d")),
		}
	}
	once, twice := mixed(), mixed()
	Document("p", once)
	Document("p", twice)
	Document("p", twice)
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("a second assembly changed the pages:\n once %+v\ntwice %+v", once, twice)
	}
}

// rows is a table with the given rows; header marks the first row's cells.
func rows(header bool, lines ...string) document.Block {
	t := &document.Table{Rows: len(lines)}
	for r, line := range lines {
		cells := strings.Split(line, "|")
		t.Cols = max(t.Cols, len(cells))
		for c, cell := range cells {
			t.Cells = append(t.Cells, document.Cell{Row: r, Col: c, Text: strings.TrimSpace(cell), Header: header && r == 0})
		}
	}
	return document.Block{Kind: document.KindTable, Table: t, Text: strings.Join(lines, "\n")}
}

// Two tables with the same number of columns are not one table. A join
// needs a second sign: the header row again, or the same place on the page.
func TestATableContinuesOnlyWhenMoreThanItsColumnsAgree(t *testing.T) {
	totals := rows(false, "Subtotal | 100.00", "Total | 119.00")
	bank := rows(false, "IBAN | DE00 0000", "BIC | ABCDEFGH")
	items := rows(true, "Item | Qty | Price", "Bolt | 4 | 1.00")
	more := rows(true, "Item | Qty | Price", "Nut | 9 | 0.50")
	headless := rows(false, "Washer | 2 | 0.10", "Screw | 7 | 0.20")

	for name, tc := range map[string]struct {
		pages []document.Page
		want  []string
	}{
		"two unrelated tables of two columns": {
			[]document.Page{page(1, text(document.KindText, "a"), totals), page(2, bank, text(document.KindText, "z"))},
			nil,
		},
		"unrelated, and in different places": {
			[]document.Page{page(1, boxed(totals, 0.5, 0.8, 0.9, 0.9)), page(2, boxed(bank, 0.1, 0.1, 0.5, 0.2))},
			nil,
		},
		"the header row again": {
			[]document.Page{page(1, text(document.KindText, "a"), items), page(2, more, text(document.KindText, "z"))},
			[]string{"1.2,2.1"},
		},
		"no header again, the same place on the page": {
			[]document.Page{page(1, boxed(items, 0.1, 0.5, 0.9, 0.95)), page(2, boxed(headless, 0.11, 0.05, 0.89, 0.3))},
			[]string{"1.1,2.1"},
		},
		"no header again, and a place only one of them has": {
			[]document.Page{page(1, boxed(items, 0.1, 0.5, 0.9, 0.95)), page(2, headless)},
			nil,
		},
		"a caption that says continued": {
			[]document.Page{page(1, items), page(2, text(document.KindCaption, "Table 3 (continued)"), more)},
			[]string{"1.1,2.2"},
		},
		"a caption that says cont.": {
			[]document.Page{page(1, items, text(document.KindCaption, "Cont. on the next page")), page(2, more)},
			[]string{"1.1,2.1"},
		},
		"a caption that names another table": {
			[]document.Page{page(1, items), page(2, text(document.KindCaption, "Table 4: Spare parts"), more)},
			nil,
		},
		"first rows with nothing in them are no header": {
			[]document.Page{page(1, rows(false, " | ", "a | b")), page(2, rows(false, " | ", "c | d"))},
			nil,
		},
		"a page that holds only a caption": {
			[]document.Page{page(1, items), page(2, text(document.KindCaption, "Table 3 (continued)"))},
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

// A reader sees one page, so the depth it gives a heading is a guess made
// without the rest of the document. A printed section number is not.
func TestAHeadingsDepthComesFromItsPrintedNumber(t *testing.T) {
	// The headings of a real paper, with the levels a model gave them page
	// by page.
	read := []struct {
		kind  document.Kind
		text  string
		level int
		want  int
	}{
		{document.KindTitle, "Attention Is All You Need", 2, 1},
		{document.KindHeading, "Abstract", 2, 2},
		{document.KindHeading, "1 Introduction", 1, 2},
		{document.KindHeading, "2 Background", 1, 2},
		{document.KindHeading, "3 Model Architecture", 1, 2},
		{document.KindHeading, "3.1 Encoder and Decoder Stacks", 2, 3},
		{document.KindHeading, "3.2 Attention", 2, 3},
		{document.KindHeading, "3.2.1 Scaled Dot-Product Attention", 3, 4},
		{document.KindHeading, "3.3 Position-wise Feed-Forward Networks", 3, 3},
		{document.KindHeading, "3.5 Positional Encoding", 2, 3},
		{document.KindHeading, "4 Why Self-Attention", 2, 2},
	}
	var blocks []document.Block
	for _, h := range read {
		blocks = append(blocks, document.Block{Kind: h.kind, Text: h.text, Level: h.level})
	}
	pages := []document.Page{page(1, blocks...)}
	doc := Document("p", pages)
	for i, h := range doc.Outline {
		if h.Level != read[i].want {
			t.Errorf("%q is at level %d, want %d", h.Text, h.Level, read[i].want)
		}
	}
	if out := markdown(t, pages, View{}); !strings.Contains(out, "\n### 3.3 Position-wise") || !strings.Contains(out, "\n## 4 Why Self-Attention") {
		t.Errorf("the Markdown prints the outline's levels:\n%s", out)
	}

	for text, want := range map[string]int{
		"1 Introduction": 1, "2.3 Results": 2, "3.2.1 Method": 3, "4. Discussion": 1, "4.1. Limits": 2,
		"A.1 Proofs": 2, "B.2.3 Data": 3, "A. Appendix": 1, "IV. Experiments": 1,
		"Introduction": 0, "A Study of Tables": 0, "2024 Annual Report": 0, "3rd Quarter": 0, "1": 0, "1.": 0, "IBM. Notes": 0,
	} {
		if got := numberDepth(text); got != want {
			t.Errorf("numberDepth(%q) = %d, want %d", text, got, want)
		}
	}

	// With no title, the top numbered sections are the top of the outline.
	untitled := Document("p", []document.Page{page(1,
		document.Block{Kind: document.KindHeading, Text: "1 Scope", Level: 3},
		document.Block{Kind: document.KindHeading, Text: "1.1 Terms", Level: 3},
		document.Block{Kind: document.KindHeading, Text: "Notes", Level: 1})}).Outline
	if untitled[0].Level != 1 || untitled[1].Level != 2 || untitled[2].Level != 1 {
		t.Errorf("an outline with no title: %+v", untitled)
	}
}

func TestChunksCountCharactersAndKeepAHeadingWithItsSection(t *testing.T) {
	// Twice ten characters of two bytes each fit in 25 characters.
	wide := []document.Page{page(1, text(document.KindText, strings.Repeat("ä", 10)), text(document.KindText, strings.Repeat("ö", 10)))}
	if got := Chunks(wide, ByPage, 25); len(got) != 1 {
		t.Errorf("22 characters in a bound of 25 are %d chunks", len(got))
	}

	// A heading followed at once by another is not a chunk of its own.
	nested := []document.Page{page(1,
		text(document.KindTitle, "Handbook"),
		document.Block{Kind: document.KindHeading, Text: "Chapter 2"},
		document.Block{Kind: document.KindHeading, Text: "2.1 Overview"},
		text(document.KindText, "The overview."),
		document.Block{Kind: document.KindHeading, Text: "2.2 Detail"},
		text(document.KindText, "The detail."),
		document.Block{Kind: document.KindHeading, Text: "Chapter 3"})}
	var got []string
	for _, c := range Chunks(nested, BySection, 6000) {
		got = append(got, c.Text)
	}
	want := []string{"Handbook\n\nChapter 2\n\n2.1 Overview\n\nThe overview.", "2.2 Detail\n\nThe detail.", "Chapter 3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("by section:\n got %q\nwant %q", got, want)
	}
	// A run of headings that is itself past the bound is cut, so a table of
	// contents read as headings is not one chunk of any length.
	var contents []document.Block
	for n := 1; n <= 5; n++ {
		contents = append(contents, document.Block{Kind: document.KindHeading, Text: fmt.Sprintf("Section %02d", n)})
	}
	if got := Chunks([]document.Page{page(1, contents...)}, BySection, 15); len(got) != 3 || got[0].Text != "Section 01\n\nSection 02" || got[2].Text != "Section 05" {
		t.Errorf("a run of headings past the bound: %+v", got)
	}
	// Nor is a heading cut from the text under it because the two are long.
	tight := []document.Page{page(1, document.Block{Kind: document.KindHeading, Text: "A heading of thirty characters"}, text(document.KindText, "Its text."), text(document.KindText, "More text."))}
	if got := Chunks(tight, ByPage, 32); len(got) != 2 || got[0].Text != "A heading of thirty characters\n\nIts text." {
		t.Errorf("a heading stays with what follows it: %+v", got)
	}
}

func TestALongTableIsChunkedByItsRows(t *testing.T) {
	lines := []string{"Day | Start | End"}
	for d := 1; d <= 9; d++ {
		lines = append(lines, fmt.Sprintf("Day %d | 08:00 | 17:00", d))
	}
	sheet := rows(true, lines...)
	pages := []document.Page{page(1, document.Block{Kind: document.KindHeading, Text: "Timesheet"}, sheet, text(document.KindText, "Signed."))}

	// The header row and two rows are 61 characters, and a third row would
	// pass 70: nine rows are five parts.
	got := Chunks(pages, BySection, 70)
	if len(got) != 6 {
		t.Fatalf("a table of %d characters in a bound of 70 is %d chunks: %+v", len(sheet.Text), len(got), got)
	}
	if want := "Timesheet\n\nDay | Start | End\nDay 1 | 08:00 | 17:00\nDay 2 | 08:00 | 17:00"; got[0].Text != want {
		t.Errorf("the first part, under its heading:\n got %q\nwant %q", got[0].Text, want)
	}
	seen := 0
	for i, c := range got[:5] {
		table := c.Text
		if i == 0 {
			table = strings.TrimPrefix(table, "Timesheet\n\n")
		}
		if !strings.HasPrefix(table, "Day | Start | End\n") || !slices.Contains(c.Blocks, "1.2") || len([]rune(table)) > 70 {
			t.Errorf("part %d begins with the header row, names the table and is within the bound: %+v", i+1, c)
		}
		seen += strings.Count(table, "08:00")
	}
	if seen != 9 || got[5].Text != "Signed." || got[5].ID != "c6" || !reflect.DeepEqual(got[5].Blocks, []string{"1.3"}) {
		t.Errorf("every row is in one part, and the text after the table is its own chunk: %d rows, %+v", seen, got[5])
	}

	// Without header cells the first row is the header; a row longer than
	// the bound is a part of its own; a table of one row is not split.
	plain := rows(false, "Name | Note", "A | "+strings.Repeat("x", 80), "B | short")
	parts := Chunks([]document.Page{page(1, plain)}, ByPage, 40)
	if len(parts) != 2 || !strings.HasPrefix(parts[0].Text, "Name | Note\nA | xxx") || parts[1].Text != "Name | Note\nB | short" {
		t.Errorf("first row as the header: %+v", parts)
	}
	one := rows(true, strings.Repeat("h", 50)+" | h")
	if parts := Chunks([]document.Page{page(1, one)}, ByPage, 20); len(parts) != 1 || !strings.HasPrefix(parts[0].Text, "hhh") {
		t.Errorf("a table of one row: %+v", parts)
	}
	// A table with no structure is one block, as any other.
	flat := text(document.KindTable, strings.Repeat("a | b\n", 20))
	if parts := Chunks([]document.Page{page(1, flat)}, ByPage, 20); len(parts) != 1 {
		t.Errorf("a table with no cells: %d chunks", len(parts))
	}
}

// A figure's description is the reader's prose and its text is what is
// printed inside it. A rendering shows both and says which is which; a
// chunk, which is cited as what the document says, holds only the text.
func TestAFiguresDescriptionIsKeptApartFromItsText(t *testing.T) {
	described := document.Block{Kind: document.KindFigure, Description: "A bar chart of\nrevenue by quarter.", Text: "Q1 Q2 Q3 Q4"}
	onlyDescribed := document.Block{Kind: document.KindFigure, Description: "A photograph of a bridge."}
	onlyWords := document.Block{Kind: document.KindFigure, Text: "EXIT"}
	empty := document.Block{Kind: document.KindFigure}
	pages := []document.Page{page(1, text(document.KindText, "Before."), described, onlyDescribed, onlyWords, empty, text(document.KindText, "After."))}

	if got, want := markdown(t, pages, View{}), "Before.\n\n*[Figure: A bar chart of revenue by quarter.]*\n\nQ1 Q2 Q3 Q4\n\n*[Figure: A photograph of a bridge.]*\n\nEXIT\n\nAfter.\n"; got != want {
		t.Errorf("markdown:\n got %q\nwant %q", got, want)
	}
	var out bytes.Buffer
	if err := Text(&out, pages, View{}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Before.\n\n[Figure: A bar chart of revenue by quarter.]\n\nQ1 Q2 Q3 Q4\n\n[Figure: A photograph of a bridge.]\n\nEXIT\n\nAfter.\n"; got != want {
		t.Errorf("text:\n got %q\nwant %q", got, want)
	}
	chunks := Chunks(pages, ByPage, 6000)
	if len(chunks) != 1 || chunks[0].Text != "Before.\n\nQ1 Q2 Q3 Q4\n\nEXIT\n\nAfter." || !reflect.DeepEqual(chunks[0].Blocks, []string{"1.1", "1.2", "1.4", "1.6"}) {
		t.Errorf("chunks: %+v", chunks)
	}
}
