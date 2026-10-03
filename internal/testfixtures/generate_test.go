// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testfixtures

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update writes the generated fixtures:
//
//	go test ./internal/testfixtures -run TestGeneratedFixturesAreCurrent -update
var update = flag.Bool("update", false, "write the fixtures this package generates")

// part is one part of a generated package.
type part struct{ name, body string }

// generated are the fixtures this file builds, each from the parts written
// out below. They are packages a word processor and a spreadsheet program
// open, written by hand so that every construct the native readers handle
// is in a file whose content is known: nothing in them comes from a third
// party, and no name in them is a person's.
var generated = map[string]func() []part{
	ReportDOCX: reportDOCX,
	LedgerXLSX: func() []part { return ledger(false) },
	LedgerXLSM: func() []part { return ledger(true) },
}

// TestGeneratedFixturesAreCurrent keeps each generated file equal to what
// builds it: the same parts, in the same order, with the same content. The
// parts are compared and not the bytes, since a compressor may write the
// same part differently from one toolchain to the next.
func TestGeneratedFixturesAreCurrent(t *testing.T) {
	for name, build := range generated {
		want := build()
		if *update {
			if err := os.WriteFile(filepath.FromSlash(name), pack(t, want), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got := unpack(t, Read(t, name))
		if len(got) != len(want) {
			t.Errorf("%s holds %d parts and its generator writes %d; run this test with -update", name, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: part %d, %s, is not what its generator writes; run this test with -update", name, i, want[i].name)
			}
		}
	}
}

func pack(t *testing.T, parts []part) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.Create(p.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, p.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unpack(t *testing.T, data []byte) []part {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var out []part
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.Close(); err != nil {
			t.Fatal(err)
		}
		out = append(out, part{f.Name, string(body)})
	}
	return out
}

const (
	xmlHead  = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	relsOpen = xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`
	relBase  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
	wordNS   = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	sheetNS  = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
)

// gauge is an 8 by 8 gray checkerboard as a PNG, written out so that the
// fixture does not depend on an encoder.
func gauge() string {
	png, err := hex.DecodeString("89504e470d0a1a0a0000000d4948445200000008000000080800000000e164e157000000134944415478da6370f80f810c50da81812c11009c1327e17fba17e30000000049454e44ae426082")
	if err != nil {
		panic(err)
	}
	return string(png)
}

// reportDOCX is a short report: a title, headings by style name and by
// outline level, paragraphs with a link, a tab and a line break, a list, a
// table with a header row and cells merged across and down, a picture with
// its caption, a tracked change, and a footnote.
func reportDOCX() []part {
	p := func(props, runs string) string {
		if props != "" {
			props = `<w:pPr>` + props + `</w:pPr>`
		}
		return `<w:p>` + props + runs + `</w:p>`
	}
	r := func(text string) string { return `<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r>` }
	styled := func(style, text string) string { return p(`<w:pStyle w:val="`+style+`"/>`, r(text)) }
	item := func(text string) string {
		return p(`<w:pStyle w:val="ListParagraph"/><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>`, r(text))
	}
	cell := func(props, text string) string {
		return `<w:tc><w:tcPr><w:tcW w:w="2200" w:type="dxa"/>` + props + `</w:tcPr>` + p("", r(text)) + `</w:tc>`
	}
	row := func(props string, cells ...string) string {
		if props != "" {
			props = `<w:trPr>` + props + `</w:trPr>`
		}
		return `<w:tr>` + props + strings.Join(cells, "") + `</w:tr>`
	}

	table := `<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:w="0" w:type="auto"/></w:tblPr>` +
		`<w:tblGrid><w:gridCol w:w="2200"/><w:gridCol w:w="2200"/><w:gridCol w:w="2200"/><w:gridCol w:w="2200"/></w:tblGrid>` +
		row(`<w:tblHeader/>`, cell("", "Week"), cell(`<w:gridSpan w:val="2"/>`, "Rain (mm), measured and corrected"), cell("", "Observer")) +
		row("", cell("", "1"), cell("", "12.5"), cell("", "12.9"), cell(`<w:vMerge w:val="restart"/>`, "Team A")) +
		row("", cell("", "2"), cell("", "0.0"), cell("", "0.0"), `<w:tc><w:tcPr><w:tcW w:w="2200" w:type="dxa"/><w:vMerge/></w:tcPr><w:p/></w:tc>`) +
		row("", cell("", "3"), cell("", "31.2"), cell("", "30.8"), cell("", "Team B")) +
		`</w:tbl>`

	picture := `<w:r><w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" distT="0" distB="0" distL="0" distR="0">` +
		`<wp:extent cx="762000" cy="762000"/><wp:docPr id="1" name="Picture 1"/>` +
		`<a:graphic xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture">` +
		`<pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:nvPicPr><pic:cNvPr id="1" name="gauge.png"/><pic:cNvPicPr/></pic:nvPicPr>` +
		`<pic:blipFill><a:blip r:embed="rId4"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>` +
		`<pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="762000" cy="762000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic>` +
		`</a:graphicData></a:graphic></wp:inline></w:drawing></w:r>`

	body := styled("Title", "Field station report") +
		styled("Heading1", "Summary") +
		p("", r("The station logged rain on 14 days. See ")+
			`<w:hyperlink r:id="rId5"><w:r><w:rPr><w:rStyle w:val="Hyperlink"/></w:rPr><w:t>the method notes</w:t></w:r></w:hyperlink>`+
			r(" for how the gauges were read.")+`<w:r><w:rPr><w:rStyle w:val="FootnoteReference"/></w:rPr><w:footnoteReference w:id="2"/></w:r>`) +
		p("", `<w:r><w:t>Station:</w:t><w:tab/><w:t>Ridge North</w:t><w:br/><w:t>Period:</w:t><w:tab/><w:t>March</w:t></w:r>`) +
		item("Empty the gauge at 08:00.") + item("Record the level to the millimeter.") + item("Reset the float.") +
		styled("Heading2", "Readings") +
		table +
		p("", picture) +
		styled("Caption", "Figure 1. The gauge at Ridge North.") +
		p("", r("The float was replaced in ")+
			`<w:del w:id="10" w:author="Reviewer" w:date="2026-03-20T09:00:00Z"><w:r><w:delText>February</w:delText></w:r></w:del>`+
			`<w:ins w:id="11" w:author="Reviewer" w:date="2026-03-20T09:00:00Z"><w:r><w:t>March</w:t></w:r></w:ins>`+r(".")) +
		styled("Annex", "Notes") +
		p("", r("Readings are provisional.")) +
		`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr>`

	style := func(id, name, props string) string {
		return `<w:style w:type="paragraph" w:styleId="` + id + `"><w:name w:val="` + name + `"/><w:basedOn w:val="Normal"/>` + props + `</w:style>`
	}
	styles := xmlHead + `<w:styles ` + wordNS + `>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		style("Title", "Title", `<w:rPr><w:sz w:val="56"/></w:rPr>`) +
		style("Heading1", "heading 1", `<w:pPr><w:keepNext/><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:sz w:val="32"/></w:rPr>`) +
		style("Heading2", "heading 2", `<w:pPr><w:keepNext/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:sz w:val="26"/></w:rPr>`) +
		// A heading by its outline level alone: its name says nothing.
		style("Annex", "Annex", `<w:pPr><w:keepNext/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/></w:rPr>`) +
		style("Caption", "caption", `<w:rPr><w:i/></w:rPr>`) +
		style("ListParagraph", "List Paragraph", `<w:pPr><w:ind w:left="720"/></w:pPr>`) +
		style("FootnoteText", "footnote text", `<w:rPr><w:sz w:val="20"/></w:rPr>`) +
		`<w:style w:type="character" w:styleId="Hyperlink"><w:name w:val="Hyperlink"/><w:rPr><w:u w:val="single"/></w:rPr></w:style>` +
		`<w:style w:type="character" w:styleId="FootnoteReference"><w:name w:val="footnote reference"/><w:rPr><w:vertAlign w:val="superscript"/></w:rPr></w:style>` +
		`<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:tblPr><w:tblBorders>` +
		`<w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:left w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
		`<w:bottom w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:right w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
		`<w:insideH w:val="single" w:sz="4" w:space="0" w:color="auto"/><w:insideV w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
		`</w:tblBorders></w:tblPr></w:style>` +
		`</w:styles>`

	numbering := xmlHead + `<w:numbering ` + wordNS + `>` +
		`<w:abstractNum w:abstractNumId="0"><w:multiLevelType w:val="hybridMultilevel"/><w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="decimal"/><w:lvlText w:val="%1."/><w:lvlJc w:val="left"/><w:pPr><w:ind w:left="720" w:hanging="360"/></w:pPr></w:lvl></w:abstractNum>` +
		`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num></w:numbering>`

	footnotes := xmlHead + `<w:footnotes ` + wordNS + `>` +
		`<w:footnote w:type="separator" w:id="-1"><w:p><w:r><w:separator/></w:r></w:p></w:footnote>` +
		`<w:footnote w:type="continuationSeparator" w:id="0"><w:p><w:r><w:continuationSeparator/></w:r></w:p></w:footnote>` +
		`<w:footnote w:id="2"><w:p><w:pPr><w:pStyle w:val="FootnoteText"/></w:pPr><w:r><w:rPr><w:rStyle w:val="FootnoteReference"/></w:rPr><w:footnoteRef/></w:r>` +
		`<w:r><w:t xml:space="preserve"> Gauges were read at 08:00 local time.</w:t></w:r></w:p></w:footnote>` +
		`</w:footnotes>`

	return []part{
		{"[Content_Types].xml", xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/>` +
			`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
			`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
			`<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>` +
			`<Override PartName="/word/footnotes.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footnotes+xml"/>` +
			`</Types>`},
		{"_rels/.rels", relsOpen + `<Relationship Id="rId1" Type="` + relBase + `officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", xmlHead + `<w:document ` + wordNS + `><w:body>` + body + `</w:body></w:document>`},
		{"word/_rels/document.xml.rels", relsOpen +
			`<Relationship Id="rId1" Type="` + relBase + `styles" Target="styles.xml"/>` +
			`<Relationship Id="rId2" Type="` + relBase + `numbering" Target="numbering.xml"/>` +
			`<Relationship Id="rId3" Type="` + relBase + `footnotes" Target="footnotes.xml"/>` +
			`<Relationship Id="rId4" Type="` + relBase + `image" Target="media/gauge.png"/>` +
			`<Relationship Id="rId5" Type="` + relBase + `hyperlink" Target="https://example.org/method" TargetMode="External"/>` +
			`</Relationships>`},
		{"word/styles.xml", styles},
		{"word/numbering.xml", numbering},
		{"word/footnotes.xml", footnotes},
		{"word/media/gauge.png", gauge()},
	}
}

// ledger is a workbook of three sheets: one with a merged title, a header
// row, dates, numbers, percentages, a boolean, and formulas with their
// stored values; a hidden one with inline strings; and an empty one. With
// macros it also holds the part a macro project is stored in, as a
// placeholder: no macro is in it.
func ledger(macros bool) []part {
	rainfall := xmlHead + `<worksheet ` + sheetNS + `><dimension ref="A1:D7"/>` +
		`<sheetViews><sheetView tabSelected="1" workbookViewId="0"/></sheetViews><sheetFormatPr defaultRowHeight="15"/>` +
		`<cols><col min="1" max="4" width="14" customWidth="1"/></cols><sheetData>` +
		`<row r="1"><c r="A1" t="s"><v>0</v></c></row>` +
		`<row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2" t="s"><v>2</v></c><c r="C2" t="s"><v>3</v></c><c r="D2" t="s"><v>4</v></c></row>` +
		`<row r="3"><c r="A3"><v>1</v></c><c r="B3" s="1"><v>46083</v></c><c r="C3"><v>12.5</v></c><c r="D3" s="2"><v>0.28599999999999998</v></c></row>` +
		`<row r="4"><c r="A4"><v>2</v></c><c r="B4" s="1"><v>46090</v></c><c r="C4"><v>0</v></c><c r="D4" s="2"><v>0</v></c></row>` +
		`<row r="5"><c r="A5"><v>3</v></c><c r="B5" s="1"><v>46097</v></c><c r="C5"><v>31.2</v></c><c r="D5" s="2"><v>0.71399999999999997</v></c></row>` +
		`<row r="6"><c r="A6" t="s"><v>5</v></c><c r="C6"><f>SUM(C3:C5)</f><v>43.7</v></c><c r="D6" s="2"><f>SUM(D3:D5)</f><v>1</v></c></row>` +
		`<row r="7"><c r="A7" t="s"><v>6</v></c><c r="B7" t="b"><v>1</v></c><c r="C7" t="str"><f>A6&amp;" of March"</f><v>Total of March</v></c></row>` +
		`</sheetData><mergeCells count="2"><mergeCell ref="A1:D1"/><mergeCell ref="A6:B6"/></mergeCells>` +
		`<pageMargins left="0.7" right="0.7" top="0.75" bottom="0.75" header="0.3" footer="0.3"/></worksheet>`
	stations := xmlHead + `<worksheet ` + sheetNS + `><dimension ref="A1:B3"/><sheetData>` +
		`<row r="1"><c r="A1" t="inlineStr"><is><t>Station</t></is></c><c r="B1" t="inlineStr"><is><t>Altitude (m)</t></is></c></row>` +
		`<row r="2"><c r="A2" t="inlineStr"><is><t>Ridge North</t></is></c><c r="B2"><v>1840</v></c></row>` +
		`<row r="3"><c r="A3" t="inlineStr"><is><t>Valley</t></is></c><c r="B3"><v>612.5</v></c></row>` +
		`</sheetData></worksheet>`
	empty := xmlHead + `<worksheet ` + sheetNS + `><dimension ref="A1"/><sheetData/></worksheet>`

	shared := []string{"Rainfall by week", "Week", "Start", "Rain (mm)", "Share", "Total", "Complete"}
	var items strings.Builder
	for _, s := range shared {
		items.WriteString(`<si><t>` + s + `</t></si>`)
	}

	main := "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
	extra := ""
	if macros {
		main = "application/vnd.ms-excel.sheet.macroEnabled.main+xml"
		extra = `<Default Extension="bin" ContentType="application/vnd.ms-office.vbaProject"/>`
	}
	parts := []part{
		{"[Content_Types].xml", xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` + extra +
			`<Override PartName="/xl/workbook.xml" ContentType="` + main + `"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet2.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet3.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>` +
			`</Types>`},
		{"_rels/.rels", relsOpen + `<Relationship Id="rId1" Type="` + relBase + `officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", xmlHead + `<workbook ` + sheetNS + `><workbookPr/><bookViews><workbookView xWindow="0" yWindow="0" windowWidth="16000" windowHeight="9000"/></bookViews><sheets>` +
			`<sheet name="Rainfall" sheetId="1" r:id="rId1"/><sheet name="Stations" sheetId="2" state="hidden" r:id="rId2"/><sheet name="Spare" sheetId="3" r:id="rId3"/>` +
			`</sheets></workbook>`},
	}
	rels := `<Relationship Id="rId1" Type="` + relBase + `worksheet" Target="worksheets/sheet1.xml"/>` +
		`<Relationship Id="rId2" Type="` + relBase + `worksheet" Target="worksheets/sheet2.xml"/>` +
		`<Relationship Id="rId3" Type="` + relBase + `worksheet" Target="worksheets/sheet3.xml"/>` +
		`<Relationship Id="rId4" Type="` + relBase + `styles" Target="styles.xml"/>` +
		`<Relationship Id="rId5" Type="` + relBase + `sharedStrings" Target="sharedStrings.xml"/>`
	if macros {
		// The detector knows a workbook with macros by this part's name,
		// which therefore comes early in the package.
		parts = append(parts, part{"xl/vbaProject.bin", "a placeholder where a macro project is stored; it holds no macro"})
		rels += `<Relationship Id="rId6" Type="http://schemas.microsoft.com/office/2006/relationships/vbaProject" Target="vbaProject.bin"/>`
	}
	return append(parts,
		part{"xl/_rels/workbook.xml.rels", relsOpen + rels + `</Relationships>`},
		part{"xl/worksheets/sheet1.xml", rainfall},
		part{"xl/worksheets/sheet2.xml", stations},
		part{"xl/worksheets/sheet3.xml", empty},
		part{"xl/styles.xml", xmlHead + `<styleSheet ` + sheetNS + `>` +
			`<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>` +
			`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
			`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
			`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
			`<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
			`<xf numFmtId="14" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
			`<xf numFmtId="10" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs>` +
			`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`},
		part{"xl/sharedStrings.xml", xmlHead + `<sst ` + sheetNS + ` count="7" uniqueCount="7">` + items.String() + `</sst>`},
	)
}
