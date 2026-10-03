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

// para is a paragraph of one run, with the given properties.
func para(props, text string) string {
	if props != "" {
		props = `<w:pPr>` + props + `</w:pPr>`
	}
	return `<w:p>` + props + `<w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

// documentRels names the parts beside the main one.
func documentRels(kinds ...string) entry {
	var b strings.Builder
	for i, kind := range kinds {
		b.WriteString(`<Relationship Id="rId` + itoa(i+1) + `" Type="` + relType + kind + `" Target="` + kind + `.xml"/>`)
	}
	return entry{"word/_rels/document.xml.rels", `<Relationships ` + relsNS + `>` + b.String() + `</Relationships>`}
}

// docx reads a word-processing document through the package's entry point
// and checks what holds for every native page.
func docxPage(t *testing.T, data []byte) document.Page {
	t.Helper()
	pages, err := Pages(context.Background(), data, TypeDOCX, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatalf("%d pages", len(pages))
	}
	p := pages[0]
	if p.Number != 1 || p.State != document.PageSucceeded || p.Source != document.SourceNative || p.Usage.Pages != 1 {
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
	return p
}

func TestAParagraphIsWhatItsStyleSays(t *testing.T) {
	styles := `<w:styles ` + wordNS + `>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Standard"><w:name w:val="Normal"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Titel"><w:name w:val="Title"/><w:basedOn w:val="Standard"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="berschrift1"><w:name w:val="heading 1"/><w:basedOn w:val="Standard"/><w:pPr><w:outlineLvl w:val="0"/><w:rPr><w:b/></w:rPr></w:pPr><w:rPr><w:b/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Section"><w:name w:val="Section"/><w:basedOn w:val="berschrift1"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Deep"><w:name w:val="Heading 9"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Sub"><w:name w:val="Subtitle"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Legend"><w:name w:val="caption"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Bullet"><w:name w:val="List Bullet"/><w:pPr><w:numPr><w:numId w:val="3"/></w:numPr></w:pPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Outlined"><w:name w:val="Appendix"/><w:pPr><w:outlineLvl w:val="2"/></w:pPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Body"><w:name w:val="Body Text"/><w:pPr><w:outlineLvl w:val="9"/><w:pPrChange><w:pPr><w:outlineLvl w:val="0"/></w:pPr></w:pPrChange></w:pPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="LoopA"><w:name w:val="Loop A"/><w:basedOn w:val="LoopB"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="LoopB"><w:name w:val="Loop B"/><w:basedOn w:val="LoopA"/></w:style>` +
		`<w:style w:type="character" w:styleId="Heading2"><w:name w:val="heading 2"/><w:rPr><w:b/></w:rPr></w:style>` +
		`<w:style w:type="table" w:styleId="Grid"><w:name w:val="Table Grid"/><w:tblPr/><w:trPr/><w:tcPr/><w:tblStylePr w:type="firstRow"/></w:style>` +
		`</w:styles>`
	numbered := `<w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>`
	body := para(`<w:pStyle w:val="Titel"/>`, "Annual report") +
		para(`<w:pStyle w:val="Sub"/>`, "For the board") +
		para(`<w:pStyle w:val="berschrift1"/>`+numbered, "A numbered heading") +
		para(`<w:pStyle w:val="Section"/>`, "A style based on a heading") +
		para(`<w:pStyle w:val="Deep"/>`, "The deepest heading") +
		para(`<w:pStyle w:val="Outlined"/>`, "An outline level from a style") +
		para(`<w:outlineLvl w:val="1"/>`, "An outline level of its own") +
		para(`<w:pStyle w:val="Body"/>`, "Body text") +
		para(`<w:pStyle w:val="Legend"/>`, "A caption") +
		para(numbered, "A list item") +
		para(`<w:pStyle w:val="Bullet"/>`, "A list item by its style") +
		para(`<w:pStyle w:val="Bullet"/><w:numPr><w:numId w:val="0"/></w:numPr>`, "Taken out of the list") +
		para(`<w:pStyle w:val="LoopA"/>`, "A style based on itself") +
		para(`<w:pStyle w:val="Heading2"/>`, "A character style is no paragraph style, and the id says heading 2") +
		para(`<w:pStyle w:val="Heading3"/><w:rPr><w:del w:id="1"/></w:rPr><w:sectPr><w:pStyle w:val="Titel"/></w:sectPr>`, "A style nothing declares") +
		para(`<w:pStyle w:val="Heading12"/>`, "Not a heading level") +
		`<w:p><w:pPr><w:pStyle w:val="Titel"/></w:pPr></w:p>` + // an empty paragraph is no block
		`<w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr>`
	got := shape(docxPage(t, docxOf(t, body, documentRels("styles"), entry{"word/styles.xml", styles})))
	want := []string{
		"title1:Annual report",
		"heading2:For the board",
		"heading1:A numbered heading",
		"heading1:A style based on a heading",
		"heading6:The deepest heading",
		"heading3:An outline level from a style",
		"heading2:An outline level of its own",
		"text:Body text",
		"caption:A caption",
		"list_item:A list item",
		"list_item:A list item by its style",
		"text:Taken out of the list",
		"text:A style based on itself",
		"heading2:A character style is no paragraph style, and the id says heading 2",
		"heading3:A style nothing declares",
		"text:Not a heading level",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks:\n got %q\nwant %q", got, want)
	}

	// With no styles part, the built-in styles are known by their ids.
	bare := shape(docxPage(t, docxOf(t, para(`<w:pStyle w:val="Title"/>`, "T")+para(`<w:pStyle w:val="Heading1"/>`, "H")+para("", "P"))))
	if want := []string{"title1:T", "heading1:H", "text:P"}; !reflect.DeepEqual(bare, want) {
		t.Fatalf("blocks = %q, want %q", bare, want)
	}

	// A document may declare only so many styles.
	few := limits
	few.formats = 2
	if _, err := readDOCX(context.Background(), docxOf(t, "", documentRels("styles"), entry{"word/styles.xml", styles}), few); fault.CodeOf(err) != fault.FileTooLarge {
		t.Fatalf("styles over the bound: %v", err)
	}
}

func TestTheTextOfAParagraph(t *testing.T) {
	body := `<w:p>` +
		`<w:r><w:rPr><w:b/></w:rPr><w:t xml:space="preserve">Name:</w:t><w:tab/><w:t>value</w:t><w:br/><w:t>next line</w:t><w:cr/><w:t>third</w:t><w:br w:type="page"/><w:t>-same</w:t><w:noBreakHyphen/><w:t>word</w:t></w:r>` +
		`</w:p>` +
		`<w:p>` +
		`<w:r><w:t xml:space="preserve">See </w:t></w:r>` +
		`<w:hyperlink r:id="rId9" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:r><w:rPr><w:rStyle w:val="Hyperlink"/></w:rPr><w:t>the site</w:t></w:r></w:hyperlink>` +
		`<w:r><w:t xml:space="preserve">, page </w:t></w:r>` +
		`<w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText> PAGEREF _Toc1 \h </w:instrText></w:r><w:r><w:fldChar w:fldCharType="separate"/></w:r><w:r><w:t>12</w:t></w:r><w:r><w:fldChar w:fldCharType="end"/></w:r>` +
		`<w:fldSimple w:instr=" DATE "><w:r><w:t xml:space="preserve"> of 2026</w:t></w:r></w:fldSimple>` +
		`<w:smartTag><w:r><w:t>.</w:t></w:r></w:smartTag>` +
		`</w:p>` +
		// Tracked changes read as accepted.
		`<w:p><w:r><w:t xml:space="preserve">The fee is </w:t></w:r>` +
		`<w:del w:id="1"><w:r><w:delText>100</w:delText></w:r></w:del><w:ins w:id="2"><w:r><w:t>120</w:t></w:r></w:ins>` +
		`<w:moveFrom w:id="3"><w:r><w:t> moved away</w:t></w:r></w:moveFrom><w:moveTo w:id="4"><w:r><w:t xml:space="preserve"> euros</w:t></w:r></w:moveTo>` +
		`<w:r><w:rPr><w:rPrChange w:id="5"><w:rPr><w:vertAlign w:val="superscript"/></w:rPr></w:rPrChange></w:rPr><w:t>.</w:t></w:r></w:p>` +
		// A phonetic guide is not the text it sits over.
		`<w:p><w:r><w:ruby><w:rt><w:r><w:t>とうきょう</w:t></w:r></w:rt><w:rubyBase><w:r><w:t>東京</w:t></w:r></w:rubyBase></w:ruby></w:r></w:p>` +
		// Mathematics keeps its characters.
		`<w:p><m:oMath xmlns:m="http://schemas.openxmlformats.org/officeDocument/2006/math"><m:r><m:t>a+b</m:t></m:r></m:oMath></w:p>` +
		// Wrappers are read through, wherever they are.
		`<w:sdt><w:sdtPr><w:alias w:val="Field"/></w:sdtPr><w:sdtContent>` + para("", "Inside a content control") + `</w:sdtContent></w:sdt>` +
		`<w:customXml w:element="note">` + para("", "Inside custom markup") + `</w:customXml>` +
		// A raised run outside a table is plain text.
		`<w:p><w:r><w:t>1</w:t></w:r><w:r><w:rPr><w:vertAlign w:val="superscript"/></w:rPr><w:t>st</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t xml:space="preserve">   </w:t></w:r></w:p>`
	got := shape(docxPage(t, docxOf(t, body)))
	want := []string{
		"text:Name:\tvalue\nnext line\nthird-same-word",
		"text:See the site, page 12 of 2026.",
		"text:The fee is 120 euros.",
		"text:東京",
		"text:a+b",
		"text:Inside a content control",
		"text:Inside custom markup",
		"text:1st",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks:\n got %q\nwant %q", got, want)
	}

	if p := docxPage(t, docxOf(t, `<w:bookmarkStart w:id="0" w:name="a"/>`)); len(p.Blocks) != 0 {
		t.Fatalf("an empty document is one page with no blocks: %+v", p.Blocks)
	}
	// What is beside the body is passed over.
	beside := pack(t, entry{"word/document.xml", `<w:document ` + wordNS + `><w:background w:color="FFFFFF"><w:p><w:r><w:t>not the body</w:t></w:r></w:p></w:background><w:body>` + para("", "the body") + `</w:body></w:document>`})
	if got := shape(docxPage(t, beside)); !reflect.DeepEqual(got, []string{"text:the body"}) {
		t.Fatalf("blocks = %q", got)
	}
}

// cellOf is a table cell with the given properties and content.
func cellOf(props, content string) string {
	if props != "" {
		props = `<w:tcPr>` + props + `</w:tcPr>`
	}
	return `<w:tc>` + props + content + `</w:tc>`
}

func TestATableKeepsItsCellsSpansAndHeaderRows(t *testing.T) {
	picture := `<w:p><w:r><w:drawing><a:blip xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" r:embed="rId5" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"/></w:drawing></w:r></w:p>`
	nested := `<w:tbl><w:tblGrid><w:gridCol/><w:gridCol/></w:tblGrid><w:tr>` + cellOf("", para("", "in")) + cellOf("", para("", "side")) + `</w:tr></w:tbl>`
	table := `<w:tbl>` +
		`<w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblLook w:firstRow="1"/></w:tblPr>` +
		`<w:tblGrid><w:gridCol w:w="100"/><w:gridCol w:w="100"/><w:gridCol w:w="100"/><w:tblGridChange w:id="1"><w:tblGrid><w:gridCol/><w:gridCol/><w:gridCol/><w:gridCol/></w:tblGrid></w:tblGridChange></w:tblGrid>` +
		// A header row, its first cell over two columns.
		`<w:tr><w:trPr><w:tblHeader/></w:trPr>` +
		cellOf(`<w:gridSpan w:val="2"/>`, para("", "Region")) + cellOf("", para("", "Total")) + `</w:tr>` +
		// A cell merged down two rows, and a raised run in a cell.
		`<w:tr><w:tblPrEx><w:jc w:val="left"/></w:tblPrEx>` +
		cellOf(`<w:vMerge w:val="restart"/>`, para("", "North")+para("", "and east")) +
		cellOf("", `<w:p><w:r><w:t>km</w:t></w:r><w:r><w:rPr><w:vertAlign w:val="superscript"/></w:rPr><w:t>2</w:t></w:r><w:r><w:rPr><w:vertAlign w:val="subscript"/></w:rPr><w:t>i</w:t><w:t>j</w:t></w:r><w:r><w:rPr><w:vertAlign w:val="baseline"/></w:rPr><w:t>.</w:t></w:r></w:p>`) +
		cellOf("", para("", "1,200")) + `</w:tr>` +
		`<w:tr>` + cellOf(`<w:vMerge/>`, `<w:p/>`) + cellOf(`<w:tcPrChange w:id="2"><w:tcPr><w:gridSpan w:val="2"/></w:tcPr></w:tcPrChange>`, picture+para("", "a &lt; b")) + cellOf("", nested) + `</w:tr>` +
		// A row a tracked change deleted is not a row.
		`<w:tr><w:trPr><w:del w:id="3"/></w:trPr>` + cellOf("", para("", "deleted")) + cellOf("", para("", "row")) + cellOf("", para("", "gone")) + `</w:tr>` +
		// A row that starts one column in, inside a wrapper, with a
		// continuation that has nothing above it to continue, a header
		// mark that is switched off, and deleted text in a cell.
		`<w:sdt><w:sdtContent><w:tr><w:trPr><w:gridBefore w:val="1"/><w:tblHeader w:val="0"/><w:trPrChange w:id="4"><w:trPr><w:tblHeader/></w:trPr></w:trPrChange></w:trPr>` +
		cellOf(`<w:vMerge w:val="continue"/>`, para("", "alone")) + cellOf("", `<w:p><w:del w:id="5"><w:r><w:delText>struck</w:delText></w:r></w:del><w:r><w:t>kept</w:t></w:r></w:p>`) + `</w:tr></w:sdtContent></w:sdt>` +
		`</w:tbl>`
	p := docxPage(t, docxOf(t, para("", "Before.")+table+para("", "After.")))
	if got := shape(p); len(got) != 4 || got[0] != "text:Before." || got[2] != "figure:" || got[3] != "text:After." {
		t.Fatalf("blocks = %q", got)
	}
	b := p.Blocks[1]
	if b.Kind != document.KindTable || b.Table.Rows != 4 || b.Table.Cols != 3 {
		t.Fatalf("table = %+v", b.Table)
	}
	want := []document.Cell{
		{Row: 0, Col: 0, ColSpan: 2, Header: true, Text: "Region"}, {Row: 0, Col: 2, Header: true, Text: "Total"},
		{Row: 1, Col: 0, RowSpan: 2, Text: "North\nand east"}, {Row: 1, Col: 1, Text: "km^2_ij."}, {Row: 1, Col: 2, Text: "1,200"},
		{Row: 2, Col: 1, Text: "a < b"}, {Row: 2, Col: 2, Text: "in | side"},
		{Row: 3, Col: 1, Text: "alone"}, {Row: 3, Col: 2, Text: "kept"},
	}
	if !reflect.DeepEqual(b.Table.Cells, want) {
		t.Fatalf("cells:\n got %+v\nwant %+v", b.Table.Cells, want)
	}
	if want := "Region | Total\nNorth\nand east | km^2_ij. | 1,200\na < b | in | side\nalone | kept"; b.Text != want {
		t.Fatalf("text = %q, want %q", b.Text, want)
	}
	if want := `<table><tr><th colspan="2">Region</th><th>Total</th></tr>` +
		"<tr><td rowspan=\"2\">North\nand east</td><td>km^2_ij.</td><td>1,200</td></tr>" +
		`<tr><td>a &lt; b</td><td>in | side</td></tr><tr><td>alone</td><td>kept</td></tr></table>`; b.Table.HTML != want {
		t.Fatalf("markup = %s", b.Table.HTML)
	}

	// A span is held to its bound, text in a continued cell is kept, and a
	// table with no cell is no block.
	wide := `<w:tbl><w:tr>` + cellOf(`<w:gridSpan w:val="2000000000"/><w:vMerge w:val="restart"/>`, para("", "wide")) + `</w:tr>` +
		`<w:tr>` + cellOf(`<w:gridSpan w:val="-4"/><w:vMerge/>`, para("", "more")) + `</w:tr>` +
		`<w:tr><w:trPr><w:gridBefore w:val="-1"/></w:trPr>` + cellOf(`<w:vMerge/>`, "") + `</w:tr></w:tbl><w:tbl><w:tblPr/></w:tbl>`
	p = docxPage(t, docxOf(t, wide))
	if len(p.Blocks) != 1 || p.Blocks[0].Table.Cols != maxSpan || p.Blocks[0].Table.Rows != 3 {
		t.Fatalf("blocks = %+v", p.Blocks)
	}
	if got, want := p.Blocks[0].Table.Cells, []document.Cell{{Row: 0, Col: 0, RowSpan: 3, ColSpan: maxSpan, Text: "wide\nmore"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cells = %+v, want %+v", got, want)
	}

	// The cells of a file's tables are held to a bound together, counted
	// as rows times columns.
	few := limits
	few.cells = 2 * maxSpan
	if _, err := readDOCX(context.Background(), docxOf(t, wide), few); fault.CodeOf(err) != fault.FileTooLarge {
		t.Fatalf("a table of three rows by a thousand columns against a bound of two thousand cells: %v", err)
	}
}

func TestFiguresTextBoxesAndNotes(t *testing.T) {
	const drawingNS = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006" xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office" xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape"`
	body := `<w:p ` + drawingNS + `><w:r><w:t>A picture:</w:t></w:r>` +
		`<w:r><w:drawing><a:graphic><a:graphicData><a:blip r:embed="rId4"/></a:graphicData></a:graphic></w:drawing></w:r></w:p>` +
		// One drawing written twice, for a reader of the older markup.
		`<w:p ` + drawingNS + `><w:r><mc:AlternateContent><mc:Choice Requires="wps"><w:drawing><wps:wsp><wps:txbx><w:txbxContent>` +
		para(`<w:pStyle w:val="Heading2"/>`, "In a text box") + `</w:txbxContent></wps:txbx></wps:wsp><mc:AlternateContent><mc:Fallback><a:blip/></mc:Fallback></mc:AlternateContent></w:drawing></mc:Choice>` +
		`<mc:Fallback><w:pict><v:textbox><w:txbxContent>` + para("", "In a text box") + `</w:txbxContent></v:textbox></w:pict></mc:Fallback></mc:AlternateContent></w:r></w:p>` +
		// A picture in the older markup, an embedded object, and a shape
		// that shows nothing.
		`<w:p ` + drawingNS + `><w:r><w:pict><v:shape><v:imagedata r:id="rId5"/></v:shape></w:pict></w:r>` +
		`<w:r><w:object><v:shape><v:imagedata r:id="rId6"/></v:shape><o:OLEObject r:id="rId7"/></w:object></w:r>` +
		`<w:r><w:pict><v:line/></w:pict></w:r></w:p>` +
		// Notes, cited out of order and one of them twice.
		`<w:p><w:r><w:t>Claims</w:t></w:r><w:r><w:footnoteReference w:id="3"/></w:r><w:r><w:t xml:space="preserve"> and sources</w:t></w:r>` +
		`<w:r><w:endnoteReference w:id="1"/></w:r><w:r><w:footnoteReference w:id="2"/></w:r><w:r><w:footnoteReference w:id="3"/></w:r><w:r><w:footnoteReference w:id="9"/></w:r></w:p>` +
		`<mc:AlternateContent xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"><mc:Choice Requires="w14">` + para("", "The current markup.") + `</mc:Choice><mc:Fallback>` + para("", "The older markup.") + `</mc:Fallback></mc:AlternateContent>`
	footnotes := `<w:footnotes ` + wordNS + `>` +
		`<w:footnote w:type="separator" w:id="-1"><w:p><w:r><w:separator/></w:r></w:p></w:footnote>` +
		`<w:footnote w:type="continuationSeparator" w:id="0"><w:p><w:r><w:t>not a note</w:t></w:r></w:p></w:footnote>` +
		`<w:footnote w:id="1">` + para("", "A note nothing cites.") + `</w:footnote>` +
		`<w:footnote w:id="2"><w:p><w:r><w:footnoteRef/></w:r><w:r><w:t xml:space="preserve"> Second, in two</w:t></w:r></w:p>` + para("", "paragraphs.") + `</w:footnote>` +
		`<w:footnote w:type="normal" w:id="3">` + para("", "Third.") + `<w:p ` + drawingNS + `><w:r><w:pict><v:imagedata/></w:pict></w:r></w:p></w:footnote>` +
		`</w:footnotes>`
	endnotes := `<w:endnotes ` + wordNS + `><w:endnote w:id="1">` + para("", "An endnote.") + `</w:endnote></w:endnotes>`
	p := docxPage(t, docxOf(t, body, documentRels("footnotes", "endnotes"), entry{"word/footnotes.xml", footnotes}, entry{"word/endnotes.xml", endnotes}))
	want := []string{
		"text:A picture:", "figure:",
		"heading2:In a text box",
		"figure:", "figure:",
		"text:Claims and sources",
		"text:The current markup.",
		"footnote:Third.", "footnote:Second, in two\nparagraphs.", "footnote:An endnote.",
	}
	if got := shape(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks:\n got %q\nwant %q", got, want)
	}

	// The notes a body cites are held to the bound on blocks.
	var cites strings.Builder
	for i := range 12 {
		cites.WriteString(`<w:r><w:footnoteReference w:id="` + itoa(i) + `"/></w:r>`)
	}
	few := limits
	few.blocks = 10
	if _, err := readDOCX(context.Background(), docxOf(t, `<w:p>`+cites.String()+`</w:p>`), few); fault.CodeOf(err) != fault.FileTooLarge {
		t.Fatalf("citations over the bound: %v", err)
	}
	// A body within the bound whose notes take it over is refused too.
	few.blocks = 3
	over := docxOf(t, para("", "a")+`<w:p><w:r><w:t>b</w:t><w:footnoteReference w:id="2"/><w:footnoteReference w:id="3"/></w:r></w:p>`, documentRels("footnotes"), entry{"word/footnotes.xml", footnotes})
	if _, err := readDOCX(context.Background(), over, few); fault.CodeOf(err) != fault.FileTooLarge {
		t.Fatalf("blocks over the bound once the notes are added: %v", err)
	}
}

// Every reader of a part passes on an error from the part it reads.
func TestADamagedPartFailsWhereverItIsRead(t *testing.T) {
	broken := `<w:x ` + wordNS + `><w:style w:type="paragraph"><w:pPr><w:broken></w:pPr></w:style><w:footnote w:id="2"><w:p><w:r><w:t>cut`
	for name, data := range map[string][]byte{
		"the relationships":          pack(t, entry{"_rels/.rels", `<Relationships>`}, entry{"word/document.xml", "<a/>"}),
		"the document relationships": docxOf(t, para("", "x"), entry{"word/_rels/document.xml.rels", `<Relationships><Relationship`}),
		"the relationships, empty":   docxOf(t, para("", "x"), entry{"word/_rels/document.xml.rels", ``}),
		"the styles":                 docxOf(t, para("", "x"), documentRels("styles"), entry{"word/styles.xml", broken}),
		"the styles, empty":          docxOf(t, para("", "x"), documentRels("styles"), entry{"word/styles.xml", ""}),
		"the styles, a style cut":    docxOf(t, para("", "x"), documentRels("styles"), entry{"word/styles.xml", `<w:styles ` + wordNS + `><w:style w:type="paragraph"><w:name`}),
		"the styles, a skip cut":     docxOf(t, para("", "x"), documentRels("styles"), entry{"word/styles.xml", `<w:styles ` + wordNS + `><w:style w:type="table"><w:name`}),
		"the styles, properties cut": docxOf(t, para("", "x"), documentRels("styles"), entry{"word/styles.xml", `<w:styles ` + wordNS + `><w:style w:type="paragraph"><w:rPr><w:b`}),
		"the notes":                  docxOf(t, `<w:p><w:r><w:footnoteReference w:id="2"/></w:r></w:p>`, documentRels("footnotes"), entry{"word/footnotes.xml", broken}),
		"the notes, empty":           docxOf(t, `<w:p><w:r><w:footnoteReference w:id="2"/></w:r></w:p>`, documentRels("footnotes"), entry{"word/footnotes.xml", ""}),
		"the notes, a skip cut":      docxOf(t, `<w:p><w:r><w:footnoteReference w:id="2"/></w:r></w:p>`, documentRels("footnotes"), entry{"word/footnotes.xml", `<w:footnotes ` + wordNS + `><w:footnote w:id="7"><w:p>`}),
		"the notes, cut at the root": docxOf(t, `<w:p><w:r><w:footnoteReference w:id="2"/></w:r></w:p>`, documentRels("footnotes"), entry{"word/footnotes.xml", `<w:footnotes ` + wordNS + `>`}),
		"a paragraph":                docxOf(t, `<w:p><w:r><w:t>cut</w:p>`),
		"paragraph properties":       docxOf(t, `<w:p><w:pPr><w:rPr><w:b></w:pPr></w:p>`),
		"paragraph properties, cut":  docxOf(t, `<w:p><w:pPr><w:jc></w:p>`),
		"run properties":             docxOf(t, `<w:p><w:r><w:rPr><w:b></w:r></w:p>`),
		"deleted text":               docxOf(t, `<w:p><w:del><w:r></w:del></w:p>`),
		"a drawing":                  docxOf(t, `<w:p><w:r><w:drawing><a></w:drawing></w:r></w:p>`),
		"a text box":                 docxOf(t, `<w:p><w:r><w:drawing><w:txbxContent><w:p></w:txbxContent></w:drawing></w:r></w:p>`),
		"a fallback in a drawing":    docxOf(t, `<w:p><w:r><w:drawing><mc:Fallback xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"><a></mc:Fallback></w:drawing></w:r></w:p>`),
		"a table":                    docxOf(t, `<w:tbl><w:tr></w:tbl>`),
		"table properties":           docxOf(t, `<w:tbl><w:tblPr><w:a></w:tblPr></w:tbl>`),
		"a table, cut":               docxOf(t, `<w:tbl><w:tblGrid></w:tbl>`),
		"a row":                      docxOf(t, `<w:tbl><w:tr><w:tc></w:tr></w:tbl>`),
		"row properties":             docxOf(t, `<w:tbl><w:tr><w:trPr><w:trPrChange><w:a></w:trPrChange></w:trPr></w:tr></w:tbl>`),
		"cell properties":            docxOf(t, `<w:tbl><w:tr><w:tc><w:tcPr><w:a></w:tcPr></w:tc></w:tr></w:tbl>`),
		"changed cell properties":    docxOf(t, `<w:tbl><w:tr><w:tc><w:tcPr><w:tcPrChange><w:a></w:tcPrChange></w:tcPr></w:tc></w:tr></w:tbl>`),
		"a section":                  docxOf(t, `<w:sectPr><w:a></w:sectPr>`),
		"what is beside the body":    pack(t, entry{"word/document.xml", `<w:document ` + wordNS + `><w:background><w:a></w:background></w:document>`}),
		"the body, cut":              pack(t, entry{"word/document.xml", `<w:document ` + wordNS + `><w:body>`}),
		"the document, cut":          pack(t, entry{"word/document.xml", `<w:document ` + wordNS + `>`}),
		"after the document":         pack(t, entry{"word/document.xml", `<w:document ` + wordNS + `></w:document><w:a>`}),
	} {
		if _, err := readDOCX(context.Background(), data, limits); fault.CodeOf(err) != fault.DocumentCorrupt {
			t.Errorf("%s: err = %v, want %s", name, err, fault.DocumentCorrupt)
		}
	}
}
