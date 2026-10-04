// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

const sheetNS = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

// tab is one sheet of a workbook a test builds: its name, the kind of its
// part, and the part.
type tab struct{ name, kind, part string }

// worksheet is a sheet whose part holds the given markup.
func worksheet(name, inner string) tab {
	return tab{name, "worksheet", `<worksheet ` + sheetNS + `>` + inner + `</worksheet>`}
}

// rowsOf wraps rows into the element that holds a sheet's cells.
func rowsOf(rows string) string { return `<sheetData>` + rows + `</sheetData>` }

// workbookOf builds a workbook. Its relationships always name a shared
// strings part and a styles part; a test adds either as a further entry.
func workbookOf(t testing.TB, tabs []tab, rest ...entry) []byte {
	t.Helper()
	var sheets, rels strings.Builder
	entries := []entry{rootRels("xl/workbook.xml")}
	for i, s := range tabs {
		n := itoa(i + 1)
		sheets.WriteString(`<sheet name="` + s.name + `" sheetId="` + n + `" r:id="rId` + n + `"/>`)
		rels.WriteString(`<Relationship Id="rId` + n + `" Type="` + relType + s.kind + `" Target="worksheets/sheet` + n + `.xml"/>`)
		if s.part != "" {
			entries = append(entries, entry{"xl/worksheets/sheet" + n + ".xml", s.part})
		}
	}
	entries = append(entries,
		entry{"xl/workbook.xml", `<workbook ` + sheetNS + `><sheets>` + sheets.String() + `</sheets></workbook>`},
		entry{"xl/_rels/workbook.xml.rels", `<Relationships ` + relsNS + `>` + rels.String() +
			`<Relationship Id="rIdStrings" Type="` + relType + `sharedStrings" Target="sharedStrings.xml"/>` +
			`<Relationship Id="rIdStyles" Type="` + relType + `styles" Target="/xl/styles.xml"/>` +
			`<Relationship Id="rIdLink" Type="` + relType + `externalLink" Target="file:///srv/other.xlsx" TargetMode="External"/></Relationships>`},
	)
	return pack(t, append(entries, rest...)...)
}

// sharedOf is a shared strings part holding the given strings.
func sharedOf(items ...string) entry {
	var b strings.Builder
	for _, s := range items {
		b.WriteString(`<si><t>` + s + `</t></si>`)
	}
	return entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + ` count="` + itoa(len(items)) + `" uniqueCount="` + itoa(len(items)) + `">` + b.String() + `</sst>`}
}

// workbookPages reads a workbook through the package's entry point and
// checks what holds for every native page.
func workbookPages(t *testing.T, data []byte) []document.Page {
	t.Helper()
	pages, err := Pages(context.Background(), data, TypeXLSX, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pages {
		if p.Number != i+1 || p.State != document.PageSucceeded || p.Source != document.SourceNative || p.Usage.Pages != 1 {
			t.Fatalf("page = %+v", p)
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		for _, b := range p.Blocks {
			if b.Box != nil {
				t.Fatalf("a native block has no box: %+v", b)
			}
		}
	}
	return pages
}

const ledgerStyles = `<styleSheet ` + sheetNS + `>` +
	`<numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd\ hh:mm:ss"/><numFmt numFmtId="x" formatCode="0%"/></numFmts>` +
	`<cellStyleXfs count="2"><xf numFmtId="14"/><xf numFmtId="14"/></cellStyleXfs>` +
	`<cellXfs count="5"><xf numFmtId="0"><alignment><xf numFmtId="14"/></alignment></xf><xf numFmtId="14"/><xf numFmtId="10"/><xf numFmtId="164"/><xf numFmtId="20"/><xf/></cellXfs>` +
	`</styleSheet>`

func TestAWorkbookIsOnePagePerSheet(t *testing.T) {
	ledger := worksheet("Ledger", `<dimension ref="B2:E7"/><sheetViews><sheetView workbookViewId="0"/></sheetViews>`+rowsOf(
		`<row r="2"><c r="B2" t="s"><v>0</v></c></row>`+
			`<row r="3"><c r="B3" t="s"><v>1</v></c><c r="C3" t="s"><v> 2 </v></c><c r="D3" t="inlineStr"><is><t>Share</t></is></c>`+
			`<c r="E3" t="inlineStr"><is><r><rPr><b/></rPr><t>Pa</t></r><r><t>id</t></r><rPh sb="0" eb="1"><t>ペイド</t></rPh><phoneticPr fontId="1"/></is></c></row>`+
			`<row r="4"><c r="B4" t="s"><v>3</v></c><c r="C4" s="1"><v>46027</v></c><c r="D4" s="2"><v>0.125</v></c><c r="E4" t="b"><v>1</v></c></row>`+
			`<row r="5"><c r="B5" t="s"/><c r="C5" s="3"><v>46027.75</v></c><c r="D5"><v>2.2999999999999998</v></c><c r="E5" t="b"><v>0</v></c></row>`+
			`<row r="6"><c r="B6" t="s"><v>4</v></c><c r="C6" t="str"><f>B4&amp;"!"</f><v>North!</v></c><c r="D6" s="5"><f>SUM(D4:D5)</f><v>2.425</v></c><c r="E6" t="e"><v>#DIV/0!</v></c></row>`+
			`<row r="7"><c r="B7"><f>NOW()</f></c><c r="C7" t="d"><v>2026-01-05T00:00:00Z</v></c><c r="D7" s="4"><v>0.5</v></c><c r="E7" s="9"><v>7</v></c></row>`,
	)+`<mergeCells count="4"><mergeCell ref="B2:E2"/><mergeCell ref="B4:B5"/><mergeCell ref="C7"/><mergeCell ref="G9:H12"/><mergeCell ref="not a range"/></mergeCells>`)
	// A hidden sheet is a sheet. Its rows and cells give no references,
	// so each follows the one before it.
	hidden := tab{"Hidden", "worksheet", `<worksheet ` + sheetNS + `>` + rowsOf(
		`<row><c t="inlineStr"><is><t>a</t></is></c><c><v>1</v></c></row><row><c><v>2</v></c></row>`) + `</worksheet>`}
	hidden.name = `Hidden" state="hidden`
	data := workbookOf(t, []tab{
		ledger, hidden, worksheet("Empty", rowsOf("")), {"Chart", "chartsheet", `<chartsheet ` + sheetNS + `/>`},
		{"Dialog", "dialogsheet", `<dialogsheet ` + sheetNS + `/>`}, worksheet("", rowsOf(`<row r="1"><c r="A1"><v>1</v></c></row>`)),
	}, sharedOf("Quarterly ledger", "Region", "Booked", "North", "South"), entry{"xl/styles.xml", ledgerStyles})

	pages := workbookPages(t, data)
	var shapes [][]string
	for _, p := range pages {
		shapes = append(shapes, shape(p))
	}
	want := [][]string{
		{"title1:Ledger", "table:Quarterly ledger\nRegion | Booked | Share | Paid\nNorth | 2026-01-05 | 12.5% | TRUE\n2026-01-05T18:00:00 | 2.3 | FALSE\nSouth | North! | 2.425 | #DIV/0!\n | 2026-01-05T00:00:00Z | 12:00 | 7"},
		{"title1:Hidden", "table:a | 1\n2 | "},
		{"title1:Empty"},
		{"title1:Chart", "figure:"},
		{"title1:Dialog"},
		{"table:1"},
	}
	if !reflect.DeepEqual(shapes, want) {
		t.Fatalf("pages:\n got %q\nwant %q", shapes, want)
	}

	table := pages[0].Blocks[1].Table
	if table.Rows != 6 || table.Cols != 4 || len(table.Cells) != 20 {
		t.Fatalf("table = %d by %d, %d cells", table.Rows, table.Cols, len(table.Cells))
	}
	if got, want := table.Cells[0], (document.Cell{Row: 0, Col: 0, ColSpan: 4, Text: "Quarterly ledger"}); got != want {
		t.Fatalf("the merged title = %+v, want %+v", got, want)
	}
	if got, want := table.Cells[5], (document.Cell{Row: 2, Col: 0, RowSpan: 2, Text: "North"}); got != want {
		t.Fatalf("the cell merged down = %+v, want %+v", got, want)
	}
	if got, want := table.Cells[9], (document.Cell{Row: 3, Col: 1, Text: "2026-01-05T18:00:00"}); got != want {
		t.Fatalf("the cell after a merged one = %+v, want %+v", got, want)
	}
	if !strings.HasPrefix(table.HTML, `<table><tr><td colspan="4">Quarterly ledger</td></tr><tr><td>Region</td>`) ||
		!strings.Contains(table.HTML, `<tr><td rowspan="2">North</td><td>2026-01-05</td><td>12.5%</td><td>TRUE</td></tr><tr><td>2026-01-05T18:00:00</td>`) {
		t.Fatalf("markup = %s", table.HTML)
	}

	// A workbook with macros is read as one without.
	macros, err := Pages(context.Background(), data, TypeXLSM, 0)
	if err != nil || !reflect.DeepEqual(macros, pages) {
		t.Fatalf("the same workbook as one with macros: %v", err)
	}
}

func TestASheetIsReadWhateverOrderItsCellsAreIn(t *testing.T) {
	// Cells out of order, 2 for one position, fixed references, and a
	// workbook that counts its days from 1904.
	sheet := worksheet("Mixed", rowsOf(
		`<row r="3"><c r="$C$3"><v>33</v></c><c r="B3"><v>32</v></c></row>`+
			`<row r="2"><c r="B2" s="1"><v>1</v></c><c r="B2"><v>again</v></c><c r="d2"><v>24</v></c></row>`))
	data := workbookOf(t, []tab{sheet}, entry{"xl/styles.xml", ledgerStyles})
	data = pack(t, append(unpack(t, data, "xl/workbook.xml"), entry{"xl/workbook.xml",
		`<workbook ` + sheetNS + `><workbookPr date1904="1"/><sheets><sheet name="Mixed" sheetId="1" r:id="rId1"/></sheets></workbook>`})...)
	pages := workbookPages(t, data)
	if got, want := pages[0].Blocks[1].Text, "1904-01-02 |  | 24\n32 | 33 | "; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}

	// The main part is found by the package's relationships, and by its
	// usual name when they name none.
	moved := pack(t, rootRels("book/main.xml"),
		entry{"book/main.xml", `<workbook ` + sheetNS + `><workbookPr date1904="0"/><sheets><sheet name="Moved" r:id="rId1"/></sheets></workbook>`},
		entry{"book/_rels/main.xml.rels", `<Relationships ` + relsNS + `><Relationship Id="rId1" Type="` + relType + `worksheet" Target="../sheets/one.xml"/></Relationships>`},
		entry{"sheets/one.xml", `<worksheet ` + sheetNS + `>` + rowsOf(`<row><c s="1"><v>2</v></c></row>`) + `</worksheet>`},
	)
	if got := shape(workbookPages(t, moved)[0]); !reflect.DeepEqual(got, []string{"title1:Moved", "table:2"}) {
		t.Fatalf("blocks = %q", got)
	}
	unnamed := pack(t, unpack(t, workbookOf(t, []tab{worksheet("One", rowsOf(`<row><c><v>5</v></c></row>`))}), "_rels/.rels")...)
	if got := shape(workbookPages(t, unnamed)[0]); !reflect.DeepEqual(got, []string{"title1:One", "table:5"}) {
		t.Fatalf("blocks = %q", got)
	}
}

// unpack returns the entries of a package a test built, without the named
// ones, so a test can replace or remove a part.
func unpack(t testing.TB, data []byte, without ...string) []entry {
	t.Helper()
	p, err := open(context.Background(), data, limits)
	if err != nil {
		t.Fatal(err)
	}
	var out []entry
	for name, f := range p.parts {
		skip := false
		for _, w := range without {
			skip = skip || strings.EqualFold(name, w)
		}
		if skip {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := rc.Read(buf)
			body.Write(buf[:n])
			if err != nil {
				break
			}
		}
		_ = rc.Close()
		out = append(out, entry{f.Name, body.String()})
	}
	return out
}

// A sheet says how large it is, and a workbook how many strings it holds.
// Neither statement is allocated for: each is checked, and the cells and
// strings are what the bytes hold.
func TestWhatASheetDeclaresIsNeverAllocatedFor(t *testing.T) {
	one := rowsOf(`<row r="1"><c r="A1"><v>1</v></c></row>`)
	for name, tc := range map[string]struct {
		data []byte
		code fault.Code
		says string
	}{
		"a dimension of every cell a sheet can have": {
			workbookOf(t, []tab{worksheet("S", `<dimension ref="A1:XFD1048576"/>`+one)}), fault.FileTooLarge, "declares 17179869184 cells"},
		"2 cells at opposite corners": {
			workbookOf(t, []tab{worksheet("S", rowsOf(`<row r="1"><c r="A1"><v>1</v></c></row><row r="1048576"><c r="XFD1048576"><v>2</v></c></row>`))}), fault.FileTooLarge, "cells"},
		"more shared strings than the part can hold": {
			workbookOf(t, []tab{worksheet("S", one)}, entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + ` count="2" uniqueCount="4000000000"><si><t>a</t></si><si><t>b</t></si></sst>`}),
			fault.DocumentCorrupt, "declares 4000000000 strings"},
		"one long string named by many cells": {
			workbookOf(t, []tab{worksheet("S", rowsOf(`<row r="1">`+strings.Repeat(`<c t="s"><v>0</v></c>`, 80)+`</row>`))}, sharedOf(strings.Repeat("long ", 100_000))),
			fault.FileTooLarge, "bytes of text"},
	} {
		var err error
		used := allocated(func() { _, err = Pages(context.Background(), tc.data, TypeXLSX, 0) })
		if fault.CodeOf(err) != tc.code || !strings.Contains(fault.DetailOf(err), tc.says) {
			t.Errorf("%s: %v; want %s saying %q", name, err, tc.code, tc.says)
		}
		// The one long string is 500 KB and is read once; nothing else
		// here is larger than its file.
		if used > 16<<20 {
			t.Errorf("%s: refusing it allocated %d bytes", name, used)
		}
	}

	// A count the part could hold is not checked further: producers get it
	// wrong, and nothing depends on it.
	sloppy := workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c t="s"><v>1</v></c></row>`))},
		entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + ` uniqueCount="7"><si><t>a</t></si><si><t>b</t></si></sst>`})
	if got := workbookPages(t, sloppy)[0].Blocks[1].Text; got != "b" {
		t.Fatalf("text = %q", got)
	}
	// A dimension smaller than the sheet is as little use: the cells decide.
	under := workbookOf(t, []tab{worksheet("S", `<dimension ref="A1"/>`+rowsOf(`<row r="1"><c r="A1"><v>1</v></c><c r="C1"><v>3</v></c></row>`))})
	if got := workbookPages(t, under)[0].Blocks[1].Text; got != "1 |  | 3" {
		t.Fatalf("text = %q", got)
	}
}

// A workbook over the limit on pages is refused for the sheets it lists,
// before any is opened: the sheets here have no parts to open.
func TestAWorkbookOverThePageLimitIsRefusedBeforeASheetIsRead(t *testing.T) {
	tabs := make([]tab, 5)
	for i := range tabs {
		tabs[i] = tab{name: "S" + itoa(i), kind: "worksheet"}
	}
	data := workbookOf(t, tabs)
	if _, err := Pages(context.Background(), data, TypeXLSX, 4); fault.CodeOf(err) != fault.TooManyPages {
		t.Fatalf("5 sheets against a limit of 4: %v", err)
	}
	if _, err := Pages(context.Background(), data, TypeXLSX, 5); fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Fatalf("5 sheets with no parts, within the limit: %v", err)
	}
	// With no limit on pages, a workbook holds no more sheets than a
	// package may hold entries.
	few := limits
	few.entries = 4
	if _, err := readXLSX(context.Background(), data, 0, few); fault.CodeOf(err) != fault.FileTooLarge && fault.CodeOf(err) != fault.TooManyPages {
		t.Fatalf("5 sheets against a bound of 4 entries: %v", err)
	}
}

func TestAWorkbookIsHeldToItsBounds(t *testing.T) {
	cell := func(ref string) string { return rowsOf(`<row><c r="` + ref + `"><v>1</v></c></row>`) }
	small := limits
	small.cells, small.formats = 6, 3
	for name, tc := range map[string]struct {
		data []byte
		b    bounds
		code fault.Code
	}{
		"a row past the last":             {workbookOf(t, []tab{worksheet("S", rowsOf(`<row r="1048577"><c><v>1</v></c></row>`))}), limits, fault.DocumentCorrupt},
		"a row that is no number":         {workbookOf(t, []tab{worksheet("S", rowsOf(`<row r="first"><c><v>1</v></c></row>`))}), limits, fault.DocumentCorrupt},
		"a column past the last":          {workbookOf(t, []tab{worksheet("S", cell("XFE1"))}), limits, fault.DocumentCorrupt},
		"a reference with no row":         {workbookOf(t, []tab{worksheet("S", cell("AB"))}), limits, fault.DocumentCorrupt},
		"a reference with no column":      {workbookOf(t, []tab{worksheet("S", cell("12"))}), limits, fault.DocumentCorrupt},
		"a reference past the last row":   {workbookOf(t, []tab{worksheet("S", cell("A1048577"))}), limits, fault.DocumentCorrupt},
		"a reference that is no cell":     {workbookOf(t, []tab{worksheet("S", cell("A1!"))}), limits, fault.DocumentCorrupt},
		"a cell outside every row":        {workbookOf(t, []tab{worksheet("S", `<sheetData><c><v>1</v></c></sheetData>`)}), limits, fault.DocumentCorrupt},
		"cells past the last column":      {workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c r="XFD1"><v>1</v></c><c><v>2</v></c></row>`))}), limits, fault.DocumentCorrupt},
		"merged ranges that overlap":      {workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c r="A1"><v>1</v></c><c r="C3"><v>1</v></c></row>`)+`<mergeCells><mergeCell ref="A1:B2"/><mergeCell ref="B2:C3"/></mergeCells>`)}), limits, fault.DocumentCorrupt},
		"a shared string that is not one": {workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c t="s"><v>5</v></c></row>`))}, sharedOf("only")), limits, fault.DocumentCorrupt},
		"a shared string with no number":  {workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c t="s"><v>x</v></c></row>`))}, sharedOf("only")), limits, fault.DocumentCorrupt},
		"a sheet with no relationship":    {pack(t, entry{"xl/workbook.xml", `<workbook ` + sheetNS + `><sheets><sheet name="S" r:id="rId7"/></sheets></workbook>`}), limits, fault.DocumentCorrupt},
		"a workbook with no sheet":        {pack(t, entry{"xl/workbook.xml", `<workbook ` + sheetNS + `><sheets/></workbook>`}), limits, fault.DocumentCorrupt},
		"a part that is no workbook":      {pack(t, entry{"xl/workbook.xml", `<w:document ` + wordNS + `/>`}), limits, fault.DocumentCorrupt},
		"a package with no workbook":      {pack(t, entry{"xl/other.xml", `<a/>`}), limits, fault.DocumentCorrupt},
		"a file that is no package":       {[]byte("PK\x03\x04"), limits, fault.DocumentCorrupt},
		"cells over the bound":            {workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c r="A1"><v>1</v></c><c r="G1"><v>1</v></c></row>`))}), small, fault.FileTooLarge},
		"values over the bound":           {workbookOf(t, []tab{worksheet("S", rowsOf(`<row>`+strings.Repeat(`<c><v>1</v></c>`, 7)+`</row>`))}), small, fault.FileTooLarge},
		"cells over the bound together":   {workbookOf(t, []tab{worksheet("A", rowsOf(`<row>`+strings.Repeat(`<c><v>1</v></c>`, 4)+`</row>`)), worksheet("B", rowsOf(`<row>`+strings.Repeat(`<c><v>1</v></c>`, 4)+`</row>`))}), small, fault.FileTooLarge},
		"shared strings over the bound":   {workbookOf(t, []tab{worksheet("S", cell("A1"))}, sharedOf("a", "b", "c", "d", "e", "f", "g")), small, fault.FileTooLarge},
		"formats over the bound":          {workbookOf(t, []tab{worksheet("S", cell("A1"))}, entry{"xl/styles.xml", ledgerStyles}), small, fault.FileTooLarge},
		"declared formats over the bound": {workbookOf(t, []tab{worksheet("S", cell("A1"))}, entry{"xl/styles.xml", `<styleSheet ` + sheetNS + `><numFmts>` + strings.Repeat(`<numFmt numFmtId="1"/>`, 2) + `<numFmt numFmtId="2"/><numFmt numFmtId="3"/><numFmt numFmtId="4"/></numFmts></styleSheet>`}), small, fault.FileTooLarge},
	} {
		if _, err := readXLSX(context.Background(), tc.data, 0, tc.b); fault.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
}

// Every reader of a part passes on an error from the part it reads.
func TestADamagedWorkbookPartFailsWhereverItIsRead(t *testing.T) {
	good := worksheet("S", rowsOf(`<row><c t="s"><v>0</v></c></row>`))
	for name, data := range map[string][]byte{
		"the relationships":          pack(t, entry{"_rels/.rels", `<Relationships>`}, entry{"xl/workbook.xml", "<a/>"}),
		"the workbook":               pack(t, entry{"xl/workbook.xml", `<workbook ` + sheetNS + `><sheets>`}),
		"the workbook, empty":        pack(t, entry{"xl/workbook.xml", ``}),
		"the workbook relationships": pack(t, entry{"xl/workbook.xml", `<workbook ` + sheetNS + `><sheets><sheet name="S" r:id="rId1"/></sheets></workbook>`}, entry{"xl/_rels/workbook.xml.rels", `<Relationships>`}),
		"the shared strings":         workbookOf(t, []tab{good}, entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + `><si><t>cut`}),
		"the shared strings, empty":  workbookOf(t, []tab{good}, entry{"xl/sharedStrings.xml", ``}),
		"the shared strings, cut":    workbookOf(t, []tab{good}, entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + `>`}),
		"a phonetic guide":           workbookOf(t, []tab{good}, entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + `><si><rPh><t></rPh></si></sst>`}),
		"a string item":              workbookOf(t, []tab{good}, entry{"xl/sharedStrings.xml", `<sst ` + sheetNS + `><si><r></si></sst>`}),
		"the styles":                 workbookOf(t, []tab{good}, sharedOf("a"), entry{"xl/styles.xml", `<styleSheet ` + sheetNS + `><cellXfs><xf>`}),
		"the styles, empty":          workbookOf(t, []tab{good}, sharedOf("a"), entry{"xl/styles.xml", ``}),
		"the styles, cut":            workbookOf(t, []tab{good}, sharedOf("a"), entry{"xl/styles.xml", `<styleSheet ` + sheetNS + `><fonts>`}),
		"a sheet":                    workbookOf(t, []tab{{"S", "worksheet", `<worksheet ` + sheetNS + `><sheetData><row>`}}),
		"a sheet, empty":             workbookOf(t, []tab{{"S", "worksheet", ``}}),
		"a cell":                     workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c><v>1</c></row>`))}),
		"a cell's value":             workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c><v><a></v></c></row>`))}),
		"a cell's formula":           workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c><f><a></f></c></row>`))}),
		"a cell's inline string":     workbookOf(t, []tab{worksheet("S", rowsOf(`<row><c t="inlineStr"><is><t><a></t></is></c></row>`))}),
		"after the sheet":            workbookOf(t, []tab{{"S", "worksheet", `<worksheet ` + sheetNS + `/><a>`}}),
	} {
		if _, err := readXLSX(context.Background(), data, 0, limits); fault.CodeOf(err) != fault.DocumentCorrupt {
			t.Errorf("%s: err = %v, want %s", name, err, fault.DocumentCorrupt)
		}
	}
}
