// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
)

func box(x0, y0, x1, y1 float64) *document.Box { return &document.Box{x0, y0, x1, y1} }

// near compares two shares.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// sample is a truth of 2 pages that holds every kind of thing the measures
// look at: prose, a table with a merged cell, a figure and a formula, with
// a box on every block.
func sample() Truth {
	return Truth{
		Pages: []Page{
			{Blocks: []Block{
				{Kind: document.KindPageHeader, Text: "Tide survey", Box: box(0.1, 0.02, 0.4, 0.05)},
				{Kind: document.KindTitle, Level: 1, Text: "Tide gauge survey", Box: box(0.1, 0.1, 0.7, 0.14)},
				{Kind: document.KindText, Text: "Four gauges along the harbor wall were read every ten minutes.", Box: box(0.1, 0.2, 0.9, 0.3)},
				{Kind: document.KindFormula, Text: "r(t) = h(t) - p(t)", Box: box(0.4, 0.32, 0.6, 0.35)},
				{Kind: document.KindTable, Rows: 3, Cols: 3, Box: box(0.1, 0.4, 0.9, 0.6), Cells: []document.Cell{
					{Row: 0, Col: 0, RowSpan: 2, Header: true, Text: "Station"},
					{Row: 0, Col: 1, ColSpan: 2, Header: true, Text: "Level (cm)"},
					{Row: 1, Col: 1, Header: true, Text: "Mean"}, {Row: 1, Col: 2, Header: true, Text: "Largest"},
					{Row: 2, Col: 0, Text: "A"}, {Row: 2, Col: 1, Text: "212.4"}, {Row: 2, Col: 2, Text: "250"},
				}},
				{Kind: document.KindFigure, Box: box(0.1, 0.65, 0.9, 0.85)},
				{Kind: document.KindPageNumber, Text: "1", Box: box(0.49, 0.95, 0.51, 0.97)},
			}},
			{Blocks: []Block{
				{Kind: document.KindHeading, Level: 2, Text: "2 Results", Box: box(0.1, 0.1, 0.3, 0.13)},
				{Kind: document.KindListItem, Text: "A reading is dropped on a fault.", Box: box(0.12, 0.2, 0.6, 0.22)},
				{Kind: document.KindListItem, Text: "A week counts when it is complete.", Box: box(0.12, 0.23, 0.6, 0.25)},
			}},
		},
	}
}

// read returns the sample as a reader that made no mistake returns it, for
// a case to change.
func read() []document.Page { return sample().Document() }

func TestAReadingWithNoMistakeScoresPerfect(t *testing.T) {
	got := Score(sample(), read())
	if got.Pages != 2 || got.Blocks != 10 || got.Found != 10 || got.Matched != 10 {
		t.Fatalf("counts = %+v", got)
	}
	if got.Edits != 0 || got.CER != 0 || got.Kinds != 1 || got.Order != 1 || got.Cells == nil || *got.Cells != 1 || got.Boxes == nil || *got.Boxes != 1 {
		t.Fatalf("scores = %+v", got)
	}
	if got.CellsTotal != 7 || got.BoxesTotal != 10 || got.Pairs != 8 || len(got.Notes) != 0 {
		t.Fatalf("totals = %+v", got)
	}
	// The transcription leaves the figure and the formula out, and counts a
	// table by its cells, not by the marks a block's text puts between them.
	want := len("Tide survey Tide gauge survey Four gauges along the harbor wall were read every ten minutes. Station Level (cm) Mean Largest A 212.4 250 1") +
		len("2 Results A reading is dropped on a fault. A week counts when it is complete.")
	if got.Characters != want {
		t.Fatalf("the truth has %d characters, want %d", got.Characters, want)
	}
	if misses := bars[Exact].Misses(got); len(misses) != 0 {
		t.Fatalf("a perfect reading misses %v", misses)
	}
}

func TestCERCountsEditsOverTheTruthsCharacters(t *testing.T) {
	truth := Truth{Pages: []Page{{Blocks: []Block{{Kind: document.KindText, Text: "0123456789"}}}}}
	page := func(text string) []document.Page {
		return Truth{Pages: []Page{{Blocks: []Block{{Kind: document.KindText, Text: text}}}}}.Document()
	}
	for name, tc := range map[string]struct {
		found string
		edits int
	}{
		"the same text":                {"0123456789", 0},
		"one character replaced":       {"0123x56789", 1},
		"one character lost":           {"012356789", 1},
		"2 characters added":           {"01234ab56789", 2},
		"whitespace is one space":      {"  0123456789 \n", 0},
		"case counts":                  {"0123456789"[:9] + "A", 1},
		"a character of several bytes": {"012345678é", 1},
	} {
		got := Score(truth, page(tc.found))
		if got.Edits != tc.edits || !near(got.CER, float64(tc.edits)/10) {
			t.Errorf("%s: %d edits, CER %v, want %d", name, got.Edits, got.CER, tc.edits)
		}
	}

	// Whitespace inside a text is folded on both sides.
	spaced := Truth{Pages: []Page{{Blocks: []Block{{Kind: document.KindText, Text: "one\ttwo\n three"}}}}}
	if got := Score(spaced, page("one two three")); got.Edits != 0 || got.Characters != len("one two three") {
		t.Fatalf("whitespace: %+v", got)
	}
}

func TestAFigureAndAFormulaAreNotTranscription(t *testing.T) {
	found := read()
	// The reader writes the formula its own way and reads words in the
	// figure: neither is an error of transcription.
	found[0].Blocks[3].Text = `r(t)=h(t)-p(t)`
	found[0].Blocks[5].Text = "Largest residual 0 20 40 60"
	got := Score(sample(), found)
	if got.Edits != 0 || got.Kinds != 1 || got.Matched != 10 {
		t.Fatalf("scores = %+v", got)
	}

	// A formula the reader took for text is found by nobody: its kind is
	// wrong, and what it wrote is text the truth's transcription lacks.
	found = read()
	found[0].Blocks[3].Kind = document.KindText
	got = Score(sample(), found)
	if got.KindsRight != 9 || got.Matched != 9 || got.Edits != len("r(t) = h(t) - p(t) ") {
		t.Fatalf("a formula read as text: %+v", got)
	}
	if !strings.Contains(strings.Join(got.Notes, "\n"), "1.4 formula is not found") {
		t.Fatalf("notes = %q", got.Notes)
	}
}

func TestKindsAreTheBlocksFoundWithTheirKind(t *testing.T) {
	found := read()
	found[0].Blocks[1].Kind = document.KindHeading // the title, read as a heading
	found[1].Blocks[1].Text = "something else entirely, nothing like the truth"
	got := Score(sample(), found)
	if got.KindsRight != 8 || !near(got.Kinds, 0.8) || got.Matched != 9 {
		t.Fatalf("kinds = %+v", got)
	}
	notes := strings.Join(got.Notes, "\n")
	for _, want := range []string{`1.2 title is found as heading: "Tide gauge survey"`, `2.2 list_item is not found: "A reading is dropped on a fault."`} {
		if !strings.Contains(notes, want) {
			t.Errorf("no note %q in %q", want, got.Notes)
		}
	}

	// A text read with a few errors is still the block.
	found = read()
	found[0].Blocks[2].Text = "Four gauges along the harbour wall were read evry ten minutes"
	if got := Score(sample(), found); got.Matched != 10 || got.Kinds != 1 || got.Edits != 3 {
		t.Fatalf("a text with 3 errors: %+v", got)
	}
}

func TestCellsAreRightByPlaceSpansAndText(t *testing.T) {
	change := func(edit func(*document.Table)) Scores {
		found := read()
		edit(found[0].Blocks[4].Table)
		return Score(sample(), found)
	}
	for name, tc := range map[string]struct {
		edit  func(*document.Table)
		right int
		note  string
	}{
		"a wrong text":            {func(tb *document.Table) { tb.Cells[5].Text = "212.9" }, 6, `1.5 cell 2,1 "212.4" is found as "212.9"`},
		"a span that is lost":     {func(tb *document.Table) { tb.Cells[1].ColSpan = 0 }, 6, "1.5 cell 0,1 spans 1 by 2 and is found spanning 1 by 1"},
		"a cell that is missing":  {func(tb *document.Table) { tb.Cells = tb.Cells[:6] }, 6, `1.5 cell 2,2 "250" is not found`},
		"a cell in another place": {func(tb *document.Table) { tb.Cells[4].Col = 1; tb.Cells[5].Col = 0 }, 5, ""},
		"whitespace in a cell":    {func(tb *document.Table) { tb.Cells[1].Text = "Level  (cm) " }, 7, ""},
		"a span of 1 written out": {func(tb *document.Table) { tb.Cells[4].RowSpan, tb.Cells[4].ColSpan = 1, 1 }, 7, ""},
		"a header that is not":    {func(tb *document.Table) { tb.Cells[0].Header = false }, 7, ""},
	} {
		got := change(tc.edit)
		if got.CellsRight != tc.right || got.CellsTotal != 7 || !near(*got.Cells, float64(tc.right)/7) {
			t.Errorf("%s: %d of %d cells right", name, got.CellsRight, got.CellsTotal)
		}
		if tc.note != "" && !strings.Contains(strings.Join(got.Notes, "\n"), tc.note) {
			t.Errorf("%s: no note %q in %q", name, tc.note, got.Notes)
		}
	}

	// A table the reader returned as text has no cell to be right.
	found := read()
	found[0].Blocks[4].Table = nil
	found[0].Blocks[4].Text = "Station Level (cm) Mean Largest A 212.4 250"
	if got := Score(sample(), found); got.CellsRight != 0 || *got.Cells != 0 || got.Kinds != 1 || got.Edits != 0 {
		t.Fatalf("a table with no cells: %+v", got)
	}
	// A table nobody found has none either.
	found = read()
	found[0].Blocks = append(found[0].Blocks[:4:4], found[0].Blocks[5:]...)
	if got := Score(sample(), found); got.CellsRight != 0 || got.Matched != 9 {
		t.Fatalf("a table that is missing: %+v", got)
	}
	// A truth with no table has no measure of cells.
	if got := Score(sample().Select(2), sample().Select(2).Document()); got.Cells != nil || got.CellsTotal != 0 {
		t.Fatalf("a truth with no table: %+v", got)
	}
}

func TestOrderIsTheNeighborsThatStayInOrder(t *testing.T) {
	found := read()
	// The two list items come back swapped.
	blocks := found[1].Blocks
	blocks[1], blocks[2] = blocks[2], blocks[1]
	got := Score(sample(), found)
	if got.Pairs != 8 || got.PairsRight != 7 || !near(got.Order, 7.0/8) || got.Kinds != 1 {
		t.Fatalf("order = %+v", got)
	}
	if !strings.Contains(strings.Join(got.Notes, "\n"), "2.3 is found before the block it follows") {
		t.Fatalf("notes = %q", got.Notes)
	}
	// The swap is 2 texts out of place for the transcription.
	if got.Edits == 0 {
		t.Fatal("a swap costs no edit")
	}

	// A pair with a block nobody found is no pair.
	found = read()
	found[1].Blocks[1].Text = "nothing like it at all, a different line"
	if got := Score(sample(), found); got.Pairs != 6 || got.Order != 1 {
		t.Fatalf("a pair with a missing block: %+v", got)
	}
	// One block is no pair, and no pair is nothing out of order.
	one := Truth{Pages: []Page{{Blocks: []Block{{Kind: document.KindText, Text: "alone"}}}}}
	if got := Score(one, one.Document()); got.Pairs != 0 || got.Order != 1 {
		t.Fatalf("one block: %+v", got)
	}
}

func TestBoxesAreTheOnesThatOverlapByHalf(t *testing.T) {
	found := read()
	found[0].Blocks[2].Box = box(0.1, 0.2, 0.9, 0.26) // the upper 6 tenths: 0.6
	found[0].Blocks[1].Box = box(0.1, 0.1, 0.3, 0.14) // a third of the width: 0.33
	found[0].Blocks[0].Box = nil                      // no box at all
	found[1].Blocks[0].Box = box(0.5, 0.5, 0.6, 0.6)  // somewhere else
	got := Score(sample(), found)
	if got.BoxesTotal != 10 || got.BoxesRight != 7 || !near(*got.Boxes, 0.7) {
		t.Fatalf("boxes = %d of %d", got.BoxesRight, got.BoxesTotal)
	}
	if !strings.Contains(strings.Join(got.Notes, "\n"), "1.2 title: its box overlaps the found one by 0.33") {
		t.Fatalf("notes = %q", got.Notes)
	}

	// A truth with no box has no measure of boxes, whatever the reader gave.
	bare := sample()
	for i := range bare.Pages {
		for j := range bare.Pages[i].Blocks {
			bare.Pages[i].Blocks[j].Box = nil
		}
	}
	if got := Score(bare, read()); got.Boxes != nil || got.BoxesTotal != 0 || got.Kinds != 1 {
		t.Fatalf("a truth with no box: %+v", got)
	}

	for name, tc := range map[string]struct {
		a, b *document.Box
		want float64
	}{
		"the same box":     {box(0, 0, 1, 1), box(0, 0, 1, 1), 1},
		"half of it":       {box(0, 0, 1, 1), box(0, 0, 0.5, 1), 0.5},
		"side by side":     {box(0, 0, 0.5, 1), box(0.5, 0, 1, 1), 0},
		"one above":        {box(0, 0, 1, 0.4), box(0, 0.6, 1, 1), 0},
		"no box":           {box(0, 0, 1, 1), nil, 0},
		"a box of no area": {box(0.2, 0.2, 0.2, 0.2), box(0.2, 0.2, 0.2, 0.2), 0},
		"a quarter each":   {box(0, 0, 0.5, 0.5), box(0.25, 0.25, 0.75, 0.75), 0.0625 / 0.4375},
	} {
		if got := iou(tc.a, tc.b); !near(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestBlocksMatchOnceAndInOrder(t *testing.T) {
	// 3 rows that read the same match the 3 found blocks in their order,
	// so none is out of order.
	same := Truth{Pages: []Page{{Blocks: []Block{
		{Kind: document.KindText, Text: "Balance carried forward"},
		{Kind: document.KindText, Text: "Balance carried forward"},
		{Kind: document.KindText, Text: "Balance carried forward"},
	}}}}
	if got := Score(same, same.Document()); got.Matched != 3 || got.Order != 1 || got.Edits != 0 {
		t.Fatalf("blocks with the same text: %+v", got)
	}

	// A reader that returns one block twice has it matched once.
	found := read()
	found[1].Blocks = append(found[1].Blocks, found[1].Blocks[2])
	if got := Score(sample(), found); got.Matched != 10 || got.Found != 11 || got.Edits == 0 {
		t.Fatalf("a block returned twice: %+v", got)
	}

	// Of 2 figures, each matches the found figure that lies where it does,
	// whatever the order they come back in.
	figures := Truth{Pages: []Page{{Blocks: []Block{
		{Kind: document.KindFigure, Box: box(0.1, 0.1, 0.9, 0.4)},
		{Kind: document.KindFigure, Box: box(0.1, 0.5, 0.9, 0.8)},
	}}}}
	swapped := figures.Document()
	swapped[0].Blocks[0], swapped[0].Blocks[1] = swapped[0].Blocks[1], swapped[0].Blocks[0]
	if got := Score(figures, swapped); got.Matched != 2 || got.BoxesRight != 2 || got.PairsRight != 0 {
		t.Fatalf("2 figures: %+v", got)
	}
	// A figure with no figure left to match is not found.
	if got := Score(figures, figures.Select(3).Document()); got.Matched != 0 || got.Kinds != 0 {
		t.Fatalf("figures against an empty page: %+v", got)
	}
}

func TestPagesAreComparedByNumber(t *testing.T) {
	// A page the parse did not return is lost whole.
	got := Score(sample(), read()[:1])
	if got.Matched != 7 || got.Found != 7 || got.Edits != len("2 Results A reading is dropped on a fault. A week counts when it is complete.") {
		t.Fatalf("a missing page: %+v", got)
	}
	// A page the truth does not have is text that should not be there.
	more := append(read(), document.Page{Number: 3, Blocks: []document.Block{{Kind: document.KindText, Text: "extra"}}})
	got = Score(sample(), more)
	if got.Edits != len("extra") || got.Found != 11 || got.Blocks != 10 || got.Pages != 2 {
		t.Fatalf("an extra page: %+v", got)
	}
	// A blank truth page that comes back with text has a rate over nothing.
	blank := Truth{Pages: []Page{{}}}
	got = Score(blank, []document.Page{{Number: 1, Blocks: []document.Block{{Kind: document.KindText, Text: "ghost"}}}})
	if got.Characters != 0 || got.CER != 5 || got.Kinds != 1 || got.Order != 1 {
		t.Fatalf("text on a blank page: %+v", got)
	}
	if got := Score(blank, []document.Page{{Number: 1}}); got.CER != 0 || len(bars[Exact].Misses(got)) != 0 {
		t.Fatalf("a blank page read as blank: %+v", got)
	}
}

func TestNotesAreBounded(t *testing.T) {
	var truth Truth
	var blocks []Block
	for i := range 3 * maxNotes {
		blocks = append(blocks, Block{Kind: document.KindText, Text: fmt.Sprintf("line number %04d of a long page, long enough to be cut where a note quotes it", i)})
	}
	truth.Pages = []Page{{Blocks: blocks}}
	got := Score(truth, nil)
	if len(got.Notes) != maxNotes || got.Matched != 0 || got.CER != 1 {
		t.Fatalf("%d notes, %+v", len(got.Notes), got)
	}
	if !strings.HasSuffix(got.Notes[0], `..."`) {
		t.Fatalf("a long text is cut in a note: %q", got.Notes[0])
	}
}

func TestDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"abc", "", 3}, {"", "abc", 3}, {"kitten", "sitting", 3},
		{"flaw", "lawn", 2}, {"abc", "abc", 0}, {"ab", "ba", 2}, {"été", "ete", 2},
	} {
		if got := distance([]rune(tc.a), []rune(tc.b)); got != tc.want {
			t.Errorf("distance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseRefusesATruthThatDoesNotHoldTogether(t *testing.T) {
	raw, err := json.Marshal(sample())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Parse(raw); err != nil || len(got.Pages) != 2 || got.Pages[0].Blocks[4].Cells[1].ColSpan != 2 {
		t.Fatalf("the sample: %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		edit func(*Truth)
		want string
	}{
		"an unknown kind":           {func(tr *Truth) { tr.Pages[0].Blocks[0].Kind = "banner" }, `block 1.1: unknown kind "banner"`},
		"a box off the page":        {func(tr *Truth) { tr.Pages[0].Blocks[1].Box = box(0.5, 0.5, 1.5, 0.6) }, "block 1.2: its box"},
		"cells on a paragraph":      {func(tr *Truth) { tr.Pages[0].Blocks[2].Cells = []document.Cell{{Text: "x"}} }, "block 1.3: it is text and holds cells"},
		"a table with no cell":      {func(tr *Truth) { tr.Pages[0].Blocks[4].Cells = nil }, "block 1.5: it is a table with no cell"},
		"a cell outside its table":  {func(tr *Truth) { tr.Pages[0].Blocks[4].Cols = 2 }, "block 1.5: cell 0,1 lies outside its table"},
		"a span of no table":        {func(tr *Truth) { tr.Spans = []document.Span{{ID: "s1", Parts: []string{"1.5", "2.1"}}} }, "span s1 names 2.1"},
		"a span of no block":        {func(tr *Truth) { tr.Spans = []document.Span{{ID: "s1", Parts: []string{"9.1"}}} }, "span s1 names 9.1"},
		"a heading that is none":    {func(tr *Truth) { tr.Outline = []document.Heading{{Ref: "1.3", Text: "x"}} }, "the outline names 1.3"},
		"a heading by another text": {func(tr *Truth) { tr.Outline = []document.Heading{{Ref: "2.1", Text: "3 Results"}} }, "the outline names 2.1"},
		"a ref that is none":        {func(tr *Truth) { tr.Outline = []document.Heading{{Ref: "first", Text: "x"}} }, "the outline names first"},
	} {
		tr := sample()
		tc.edit(&tr)
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}

	for name, raw := range map[string]string{
		"no JSON":           `{"pages": [`,
		"an unknown member": `{"pages": [], "confidence": 1}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "the truth is not read") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSelectKeepsWhatItsPagesStillHold(t *testing.T) {
	truth := sample()
	truth.Source = "abc"
	truth.Pages = append(truth.Pages, Page{Blocks: []Block{
		{Kind: document.KindTable, Rows: 1, Cols: 1, Cells: []document.Cell{{Text: "A"}}},
	}})
	truth.Spans = []document.Span{{ID: "s1", Parts: []string{"1.5", "3.1"}, Rows: 4, Cols: 3}}
	truth.Outline = []document.Heading{
		{Ref: "1.2", Level: 1, Text: "Tide gauge survey", Page: 1},
		{Ref: "2.1", Level: 2, Text: "2 Results", Page: 2},
	}

	// The whole truth, in another order: every ref moves with its page.
	got := truth.Select(3, 1)
	if len(got.Pages) != 2 || got.Source != "abc" || got.Pages[0].Blocks[0].Kind != document.KindTable || got.Pages[1].Blocks[1].Text != "Tide gauge survey" {
		t.Fatalf("pages = %+v", got.Pages)
	}
	if len(got.Spans) != 1 || got.Spans[0].Parts[0] != "2.5" || got.Spans[0].Parts[1] != "1.1" || got.Spans[0].Rows != 4 {
		t.Fatalf("spans = %+v", got.Spans)
	}
	if len(got.Outline) != 1 || got.Outline[0] != (document.Heading{Ref: "2.2", Level: 1, Text: "Tide gauge survey", Page: 2}) {
		t.Fatalf("outline = %+v", got.Outline)
	}

	// A span that loses a part is no span, and a page the truth does not
	// have is a blank one.
	got = truth.Select(2, 3, 7)
	if len(got.Pages) != 3 || len(got.Pages[2].Blocks) != 0 || len(got.Spans) != 0 || len(got.Outline) != 1 || got.Outline[0].Ref != "1.1" {
		t.Fatalf("a part of the truth: %+v", got)
	}
}

func TestDocumentIsTheTruthAsAReaderWouldReturnIt(t *testing.T) {
	pages := read()
	for _, p := range pages {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		if p.State != document.PageSucceeded || p.Usage == nil {
			t.Fatalf("page = %+v", p)
		}
	}
	table := pages[0].Blocks[4]
	if table.Ref != "1.5" || table.Table.Rows != 3 || len(table.Table.Cells) != 7 || table.Text != "Station | Level (cm)\nMean | Largest\nA | 212.4 | 250" {
		t.Fatalf("table = %+v", table)
	}
	if title := pages[0].Blocks[1]; title.Level != 1 || title.Box == nil || *title.Box != *box(0.1, 0.1, 0.7, 0.14) {
		t.Fatalf("title = %+v", title)
	}
	// The pages are a copy: changing them does not change the truth.
	truth := sample()
	pages = truth.Document()
	pages[0].Blocks[1].Box[0] = 0.5
	pages[0].Blocks[4].Table.Cells[0].Text = "changed"
	if truth.Pages[0].Blocks[1].Box[0] != 0.1 || truth.Pages[0].Blocks[4].Cells[0].Text != "Station" {
		t.Fatal("the truth changed with its document")
	}
}
