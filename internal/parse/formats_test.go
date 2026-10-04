// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package parse

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/testfixtures"
)

// wantPage is what a page of an office fixture must hold.
type wantPage struct {
	// kinds is how many blocks of each kind the page holds.
	kinds map[document.Kind]int
	// headings are the page's titles and headings in order, as
	// "<kind><level>:<text>".
	headings []string
	// rows and cols are the shape of the page's table, when it has one,
	// and cells are cells that table must hold exactly as given.
	rows, cols int
	cells      []document.Cell
	// texts are blocks the page must hold, as "<kind>:<text>".
	texts []string
}

// TestOfficeFormatsAreReadFromTheirOwnStructure is the quality gate for the
// formats read with no model: each fixture goes through Prepare as an
// upload does, and must come out with the pages, the blocks of each kind,
// the table shape, the spanned cells and the texts written down here.
func TestOfficeFormatsAreReadFromTheirOwnStructure(t *testing.T) {
	rainfall := wantPage{
		kinds:    map[document.Kind]int{document.KindTitle: 1, document.KindTable: 1},
		headings: []string{"title1:Rainfall"},
		rows:     7, cols: 4,
		cells: []document.Cell{
			{Row: 0, Col: 0, ColSpan: 4, Text: "Rainfall by week"},
			{Row: 1, Col: 2, Text: "Rain (mm)"},
			{Row: 2, Col: 1, Text: "2026-03-02"}, {Row: 2, Col: 2, Text: "12.5"}, {Row: 2, Col: 3, Text: "28.6%"},
			{Row: 3, Col: 2, Text: "0"}, {Row: 4, Col: 1, Text: "2026-03-16"},
			{Row: 5, Col: 0, ColSpan: 2, Text: "Total"}, {Row: 5, Col: 2, Text: "43.7"}, {Row: 5, Col: 3, Text: "100%"},
			{Row: 6, Col: 1, Text: "TRUE"}, {Row: 6, Col: 2, Text: "Total of March"}, {Row: 6, Col: 3, Text: ""},
		},
	}
	stations := wantPage{
		kinds:    map[document.Kind]int{document.KindTitle: 1, document.KindTable: 1},
		headings: []string{"title1:Stations"},
		rows:     3, cols: 2,
		cells: []document.Cell{{Row: 0, Col: 1, Text: "Altitude (m)"}, {Row: 1, Col: 0, Text: "Ridge North"}, {Row: 2, Col: 1, Text: "612.5"}},
	}
	spare := wantPage{kinds: map[document.Kind]int{document.KindTitle: 1}, headings: []string{"title1:Spare"}}

	report := wantPage{
		kinds: map[document.Kind]int{
			document.KindTitle: 1, document.KindHeading: 3, document.KindText: 4, document.KindListItem: 3,
			document.KindTable: 1, document.KindFigure: 1, document.KindCaption: 1, document.KindFootnote: 1,
		},
		headings: []string{"title1:Field station report", "heading1:Summary", "heading2:Readings", "heading2:Notes"},
		rows:     4, cols: 4,
		cells: []document.Cell{
			{Row: 0, Col: 0, Header: true, Text: "Week"},
			{Row: 0, Col: 1, ColSpan: 2, Header: true, Text: "Rain (mm), measured and corrected"},
			{Row: 1, Col: 3, RowSpan: 2, Text: "Team A"},
			{Row: 2, Col: 2, Text: "0.0"},
			{Row: 3, Col: 2, Text: "30.8"}, {Row: 3, Col: 3, Text: "Team B"},
		},
		texts: []string{
			"text:The station logged rain on 14 days. See the method notes for how the gauges were read.",
			"text:Station:\tRidge North\nPeriod:\tMarch",
			"list_item:Record the level to the millimeter.",
			"figure:",
			"caption:Figure 1. The gauge at Ridge North.",
			"text:The float was replaced in March.",
			"footnote:Gauges were read at 08:00 local time.",
		},
	}

	for _, tc := range []struct {
		fixture   string
		mediaType string
		pages     []wantPage
	}{
		// The generated document, and the same document as an office suite
		// wrote it back out: other part names, other styles, the same
		// content.
		{testfixtures.ReportDOCX, detect.MIMEDOCX, []wantPage{report}},
		{testfixtures.ReportSuiteDOCX, detect.MIMEDOCX, []wantPage{report}},
		// A document written by a word processor: one heading and 7
		// paragraphs, 3 of them in quotation styles.
		{testfixtures.DOCX, detect.MIMEDOCX, []wantPage{{
			kinds:    map[document.Kind]int{document.KindHeading: 1, document.KindText: 7},
			headings: []string{"heading2:Some block quotes, in different ways"},
			texts:    []string{"text:This is called the Intense Quote style.", "text:And back to the normal style."},
		}}},
		{testfixtures.LedgerXLSX, detect.MIMEXLSX, []wantPage{rainfall, stations, spare}},
		{testfixtures.LedgerXLSM, detect.MIMEXLSM, []wantPage{rainfall, stations, spare}},
		{testfixtures.LedgerSuiteXLSX, detect.MIMEXLSX, []wantPage{rainfall, stations, spare}},
		// A workbook written by a spreadsheet program: 3 sheets, the
		// first with 4 strings and the others empty.
		{testfixtures.XLSX, detect.MIMEXLSX, []wantPage{
			{
				kinds: map[document.Kind]int{document.KindTitle: 1, document.KindTable: 1}, headings: []string{"title1:Tabelle1"},
				rows: 2, cols: 2, cells: []document.Cell{{Row: 0, Col: 0, Text: "Foo"}, {Row: 1, Col: 1, Text: "Quuk"}},
			},
			{kinds: map[document.Kind]int{document.KindTitle: 1}, headings: []string{"title1:Tabelle2"}},
			{kinds: map[document.Kind]int{document.KindTitle: 1}, headings: []string{"title1:Tabelle3"}},
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			// The content alone decides the type: the name says nothing.
			got, err := pipeline().Prepare(context.Background(), testfixtures.Read(t, tc.fixture), named("upload"), "")
			if err != nil {
				t.Fatal(err)
			}
			if got.Manifest.MediaType != tc.mediaType || got.Manifest.Source != document.SourceNative || got.Manifest.PagesTotal != len(tc.pages) || len(got.Native) != len(tc.pages) {
				t.Fatalf("manifest = %+v with %d pages, want %s and %d pages", got.Manifest, len(got.Native), tc.mediaType, len(tc.pages))
			}
			for i, want := range tc.pages {
				checkPage(t, got.Native[i], i+1, want)
			}
		})
	}

	// A selection takes sheets as it takes pages.
	second, err := pipeline().Prepare(context.Background(), testfixtures.Read(t, testfixtures.LedgerXLSX), named("ledger.xlsx"), "2-")
	if err != nil || !slices.Equal(second.Manifest.Selected, []int{2, 3}) || len(second.Native) != 2 || second.Native[0].Number != 2 {
		t.Fatalf("the second sheet on: %+v, %v", second.Manifest, err)
	}
	checkPage(t, second.Native[0], 2, stations)
}

func checkPage(t *testing.T, p document.Page, number int, want wantPage) {
	t.Helper()
	if p.Number != number || p.State != document.PageSucceeded || p.Source != document.SourceNative || p.Reader != "" || p.Usage == nil || p.Usage.Pages != 1 {
		t.Fatalf("page %d = %+v", number, p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}

	kinds := map[document.Kind]int{}
	var headings, texts []string
	var table *document.Table
	for _, b := range p.Blocks {
		if b.Box != nil {
			t.Errorf("page %d: block %s of a native page has a box", number, b.Ref)
		}
		kinds[b.Kind]++
		switch b.Kind {
		case document.KindTitle, document.KindHeading:
			headings = append(headings, fmt.Sprintf("%s%d:%s", b.Kind, b.Level, b.Text))
		case document.KindTable:
			table = b.Table
		}
		texts = append(texts, fmt.Sprintf("%s:%s", b.Kind, b.Text))
	}
	if !maps.Equal(kinds, want.kinds) {
		t.Errorf("page %d: blocks by kind = %v, want %v", number, kinds, want.kinds)
	}
	if !reflect.DeepEqual(headings, want.headings) {
		t.Errorf("page %d: headings = %q, want %q", number, headings, want.headings)
	}
	for _, text := range want.texts {
		if !slices.Contains(texts, text) {
			t.Errorf("page %d: no block %q among %q", number, text, texts)
		}
	}
	if want.rows == 0 {
		return
	}
	if table == nil || table.Rows != want.rows || table.Cols != want.cols {
		t.Fatalf("page %d: table = %+v, want %d rows by %d columns", number, table, want.rows, want.cols)
	}
	for _, cell := range want.cells {
		if !slices.Contains(table.Cells, cell) {
			t.Errorf("page %d: the table has no cell %+v", number, cell)
		}
	}
}
