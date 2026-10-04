// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testfixtures

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/quality"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/reader"
)

// corpus is every file of the quality corpus. A file missing here fails
// TestQualityCorpusIsCurrent as soon as it exists.
var corpus = []string{
	SurveyHTML, SurveyPDF, SurveyTruth, SurveyScanPDF, SurveyPNG, SurveyJPEG, SurveyTIFF,
	PlainTXT, PlainTruth, NotesMD, NotesTruth, ReadingsCSV, ReadingsTruth,
	ReportTruth, LedgerTruth, ReportDOC, SlidesPPTX, SlidesTruth, MemoRTF, MemoTruth,
}

// scanDPI is the resolution the survey's pages are rendered at for the
// files that hold them as images: what a scanner set to save space gives.
const scanDPI = 120

// TestQualityCorpusIsCurrent keeps the quality corpus equal to what makes
// it: every file is named, every truth holds together, the survey's truth
// was written from the source that is committed beside it, and the
// presentation holds the parts its generator writes.
//
// With -update it writes the generated files again: the presentation, and
// from survey.pdf the scanned PDF, the PNG, the JPEG and the TIFF. With
// CHROME naming a headless Chromium it first prints survey.html to
// survey.pdf and writes survey.truth.json from what the page's own script
// measured:
//
//	CHROME=/path/to/chrome-headless-shell go test ./internal/testfixtures -run TestQualityCorpusIsCurrent -update
func TestQualityCorpusIsCurrent(t *testing.T) {
	if *update {
		if chrome := os.Getenv("CHROME"); chrome != "" {
			writeSurvey(t, chrome)
		}
		writeImages(t)
		if err := os.WriteFile(filepath.FromSlash(SlidesPPTX), pack(t, slidesPPTX()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	entries, err := corpusFiles.ReadDir("quality")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(corpus) {
		t.Errorf("%d files are in the corpus, %d are named", len(entries), len(corpus))
	}
	for _, e := range entries {
		if path := "quality/" + e.Name(); !slices.Contains(corpus, path) {
			t.Errorf("%s is in the corpus and no constant names it", path)
		}
	}
	for _, name := range corpus {
		data := Read(t, name)
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
		if strings.HasSuffix(name, ".truth.json") {
			if _, err := quality.Parse(data); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}

	// The survey's truth says which source it was measured from.
	truth, err := quality.Parse(Read(t, SurveyTruth))
	if err != nil {
		t.Fatal(err)
	}
	if want := digest(Read(t, SurveyHTML)); truth.Source != want {
		t.Errorf("%s was written from another %s; print the survey again with CHROME set and -update", SurveyTruth, SurveyHTML)
	}

	got, want := unpack(t, Read(t, SlidesPPTX)), slidesPPTX()
	if len(got) != len(want) {
		t.Fatalf("%s holds %d parts and its generator writes %d; run this test with -update", SlidesPPTX, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: part %d, %s, is not what its generator writes; run this test with -update", SlidesPPTX, i, want[i].name)
		}
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeSurvey prints survey.html to survey.pdf with a headless Chromium,
// and writes survey.truth.json from the truth the page's script left on its
// root element.
func writeSurvey(t *testing.T, chrome string) {
	t.Helper()
	source, err := filepath.Abs(filepath.FromSlash(SurveyHTML))
	if err != nil {
		t.Fatal(err)
	}
	printed := filepath.Join(t.TempDir(), "survey.pdf")
	run := func(args ...string) []byte {
		cmd := exec.CommandContext(context.Background(), chrome, append(args, "file://"+source)...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", chrome, err)
		}
		return out
	}
	// Tags describe the document's structure to a screen reader; they add
	// a third to the file and no reader of pages sees them.
	run("--headless", "--no-pdf-header-footer", "--disable-pdf-tagging", "--print-to-pdf="+printed)
	pdf, err := os.ReadFile(printed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.FromSlash(SurveyPDF), pdf, 0o600); err != nil {
		t.Fatal(err)
	}

	dom, err := html.Parse(bytes.NewReader(run("--headless", "--dump-dom")))
	if err != nil {
		t.Fatal(err)
	}
	var measured string
	for n := range dom.Descendants() {
		if n.Type != html.ElementNode || n.Data != "html" {
			continue
		}
		for _, a := range n.Attr {
			if a.Key == "data-truth" {
				measured = a.Val
			}
		}
	}
	truth, err := quality.Parse([]byte(measured))
	if err != nil {
		t.Fatalf("the page left no truth on its root element: %v", err)
	}
	page, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	truth.Source = digest(page)
	if err := os.WriteFile(filepath.FromSlash(SurveyTruth), encodeTruth(t, truth), 0o600); err != nil {
		t.Fatal(err)
	}
}

// encodeTruth writes a truth with one block on a line, so the file reads
// as the page does and a change to one block is a change to one line.
func encodeTruth(t *testing.T, truth quality.Truth) []byte {
	t.Helper()
	line := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	list := func(b *strings.Builder, indent string, items []string) {
		for i, item := range items {
			b.WriteString(indent + item)
			if i < len(items)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
	}
	var b strings.Builder
	b.WriteString("{\n  \"source\": " + line(truth.Source) + ",\n  \"pages\": [\n")
	for i, p := range truth.Pages {
		blocks := make([]string, 0, len(p.Blocks))
		for _, block := range p.Blocks {
			blocks = append(blocks, line(block))
		}
		if len(blocks) == 0 {
			b.WriteString("    {\"blocks\": []}")
		} else {
			b.WriteString("    {\"blocks\": [\n")
			list(&b, "      ", blocks)
			b.WriteString("    ]}")
		}
		if i < len(truth.Pages)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  ],\n  \"spans\": [\n")
	spans := make([]string, 0, len(truth.Spans))
	for _, s := range truth.Spans {
		spans = append(spans, line(s))
	}
	list(&b, "    ", spans)
	b.WriteString("  ],\n  \"outline\": [\n")
	outline := make([]string, 0, len(truth.Outline))
	for _, h := range truth.Outline {
		outline = append(outline, line(h))
	}
	list(&b, "    ", outline)
	b.WriteString("  ]\n}\n")
	return []byte(b.String())
}

// writeImages writes the files that hold the survey's pages as images,
// each from survey.pdf as it is on disk, rendered by the engine a parse
// renders with: a scan is a picture of the page, in the grays a scanner
// keeps.
func writeImages(t *testing.T) {
	t.Helper()
	pdf, err := os.ReadFile(filepath.FromSlash(SurveyPDF))
	if err != nil {
		t.Fatal(err)
	}
	engine := render.NewPages()
	total, err := engine.CountPDF(context.Background(), pdf)
	if err != nil {
		t.Fatal(err)
	}
	pages := make([]*image.Gray, 0, total)
	for n := 1; n <= total; n++ {
		img, err := engine.Render(context.Background(), pdf, detect.MIMEPDF, n, reader.Description{
			Accepts: []string{detect.MIMEPNG}, Image: reader.ImageSpec{DPI: scanDPI, Format: "png"},
		})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := png.Decode(bytes.NewReader(img.Data))
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, grays(decoded))
	}

	jpegs := make([][]byte, 0, len(pages))
	for _, p := range pages {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, p, &jpeg.Options{Quality: 60}); err != nil {
			t.Fatal(err)
		}
		jpegs = append(jpegs, buf.Bytes())
	}
	var first bytes.Buffer
	paletted := image.NewPaletted(pages[0].Bounds(), grayLevels())
	draw.Draw(paletted, paletted.Bounds(), pages[0], image.Point{}, draw.Src)
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&first, paletted); err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{
		SurveyScanPDF: scannedPDF(pages, jpegs),
		SurveyPNG:     first.Bytes(),
		SurveyJPEG:    jpegs[0],
		SurveyTIFF:    framesTIFF(t, pages[1:]),
	} {
		if err := os.WriteFile(filepath.FromSlash(name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// grayLevels are the 16 grays a page image is kept in.
func grayLevels() color.Palette {
	levels := make(color.Palette, 16)
	for i := range levels {
		levels[i] = color.Gray{Y: uint8(i * 17)}
	}
	return levels
}

// grays turns a rendered page into 16 levels of gray, which keeps the edge
// of a letter and a quarter of the bytes.
func grays(src image.Image) *image.Gray {
	b := src.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := range b.Dy() {
		for x := range b.Dx() {
			level := (int(color.GrayModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y) + 8) / 17
			out.SetGray(x, y, color.Gray{Y: uint8(level * 17)})
		}
	}
	return out
}

// scannedPDF wraps page images in a PDF: each page is one JPEG that fills
// an A4 sheet, and the file holds no text.
func scannedPDF(pages []*image.Gray, jpegs [][]byte) []byte {
	var out bytes.Buffer
	var offsets []int
	object := func(body string, stream []byte) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\n", len(offsets), body)
		if stream != nil {
			out.WriteString("stream\n")
			out.Write(stream)
			out.WriteString("\nendstream\n")
		}
		out.WriteString("endobj\n")
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	kids := make([]string, 0, len(pages))
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+3*i))
	}
	object("<< /Type /Catalog /Pages 2 0 R >>", nil)
	object(fmt.Sprintf("<< /Type /Pages /Kids [ %s ] /Count %d >>", strings.Join(kids, " "), len(pages)), nil)
	for i, p := range pages {
		first := 3 + 3*i
		object(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>", first+1, first+2), nil)
		object(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>",
			p.Bounds().Dx(), p.Bounds().Dy(), len(jpegs[i])), jpegs[i])
		draw := "q 595 0 0 842 0 0 cm /Im0 Do Q"
		object(fmt.Sprintf("<< /Length %d >>", len(draw)), []byte(draw))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, at := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", at)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

// framesTIFF writes pages as the frames of one TIFF: 8 bits of gray,
// deflated, one strip and one directory per frame.
func framesTIFF(t *testing.T, pages []*image.Gray) []byte {
	t.Helper()
	le := binary.LittleEndian
	out := []byte{'I', 'I', 42, 0, 0, 0, 0, 0}
	link := 4 // where the offset of the next directory is written
	for _, p := range pages {
		var strip bytes.Buffer
		z, err := zlib.NewWriterLevel(&strip, zlib.BestCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := z.Write(p.Pix); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		at := len(out)
		out = append(out, strip.Bytes()...)
		if len(out)%2 == 1 {
			out = append(out, 0) // a directory begins on a word
		}
		le.PutUint32(out[link:], uint32(len(out)))

		const short, long = 3, 4
		entries := []struct {
			tag, kind uint16
			value     uint32
		}{
			{256, long, uint32(p.Bounds().Dx())}, // width
			{257, long, uint32(p.Bounds().Dy())}, // height
			{258, short, 8},                      // bits per sample
			{259, short, 8},                      // compression: deflate
			{262, short, 1},                      // photometric: black is 0
			{273, long, uint32(at)},              // strip offset
			{277, short, 1},                      // samples per pixel
			{278, long, uint32(p.Bounds().Dy())}, // rows per strip
			{279, long, uint32(strip.Len())},     // strip byte count
		}
		out = le.AppendUint16(out, uint16(len(entries)))
		for _, e := range entries {
			out = le.AppendUint16(out, e.tag)
			out = le.AppendUint16(out, e.kind)
			out = le.AppendUint32(out, 1)
			out = le.AppendUint32(out, e.value)
		}
		link = len(out)
		out = le.AppendUint32(out, 0)
	}
	return out
}

// slidesPPTX is a presentation of 3 slides: a title with a line under it,
// a heading with a list of 4 items, and a heading with a table and a line
// of text. Every shape is placed outright and no placeholder of the layout
// is used, so the slides read the same in any program that opens them.
func slidesPPTX() []part {
	const (
		slideNS = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`
		types   = "application/vnd.openxmlformats-officedocument.presentationml."
		tree    = `<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="0" cy="0"/><a:chOff x="0" y="0"/><a:chExt cx="0" cy="0"/></a:xfrm></p:grpSpPr>`
		black   = `<a:solidFill><a:srgbClr val="000000"/></a:solidFill>`
	)
	run := func(size int, bold bool, text string) string {
		b := ""
		if bold {
			b = ` b="1"`
		}
		return fmt.Sprintf(`<a:r><a:rPr lang="en-US" sz="%d"%s>%s<a:latin typeface="Arial"/></a:rPr><a:t>%s</a:t></a:r>`, size, b, black, text)
	}
	id := 1
	// box is a text box at a place on the slide, in English Metric Units,
	// holding the paragraphs given.
	box := func(name string, y, height int, paragraphs ...string) string {
		id++
		return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr txBox="1"/><p:nvPr/></p:nvSpPr>`+
			`<p:spPr><a:xfrm><a:off x="685800" y="%d"/><a:ext cx="7772400" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom><a:noFill/></p:spPr>`+
			`<p:txBody><a:bodyPr wrap="square" rtlCol="0"><a:noAutofit/></a:bodyPr><a:lstStyle/>%s</p:txBody></p:sp>`, id, name, y, height, strings.Join(paragraphs, ""))
	}
	para := func(runs string) string { return `<a:p>` + runs + `</a:p>` }
	item := func(text string) string {
		return `<a:p><a:pPr marL="342900" indent="-342900"><a:spcAft><a:spcPts val="1200"/></a:spcAft><a:buFont typeface="Arial"/><a:buChar char="&#8226;"/></a:pPr>` + run(2400, false, text) + `</a:p>`
	}
	line := func(side string) string {
		return `<a:` + side + ` w="12700">` + black + `</a:` + side + `>`
	}
	cell := func(bold bool, text string) string {
		return `<a:tc><a:txBody><a:bodyPr/><a:lstStyle/>` + para(run(1800, bold, text)) + `</a:txBody><a:tcPr>` +
			line("lnL") + line("lnR") + line("lnT") + line("lnB") + `<a:noFill/></a:tcPr></a:tc>`
	}
	row := func(bold bool, texts ...string) string {
		cells := ""
		for _, text := range texts {
			cells += cell(bold, text)
		}
		return `<a:tr h="457200">` + cells + `</a:tr>`
	}
	table := func(y int, rows ...string) string {
		id++
		return fmt.Sprintf(`<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="%d" name="Table"/><p:cNvGraphicFramePr><a:graphicFrameLocks noGrp="1"/></p:cNvGraphicFramePr><p:nvPr/></p:nvGraphicFramePr>`+
			`<p:xfrm><a:off x="685800" y="%d"/><a:ext cx="7772400" cy="%d"/></p:xfrm>`+
			`<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblPr firstRow="1"/>`+
			`<a:tblGrid><a:gridCol w="2590800"/><a:gridCol w="2590800"/><a:gridCol w="2590800"/></a:tblGrid>%s</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`,
			id, y, 457200*len(rows), strings.Join(rows, ""))
	}
	// slide is one slide of the shapes given. Its shapes were numbered
	// while they were built, from 2, and the next slide's begin there again.
	slide := func(shapes ...string) string {
		id = 1
		return xmlHead + `<p:sld ` + slideNS + `><p:cSld><p:spTree>` + tree + strings.Join(shapes, "") + `</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sld>`
	}
	slides := []string{
		slide(
			box("Title", 2057400, 1143000, para(run(4000, true, "Harbor works, autumn plan"))),
			box("Subtitle", 3429000, 914400, para(run(2000, false, "Sample slides written for a parsing test. Every name and number is invented."))),
		),
		slide(
			box("Heading", 457200, 914400, para(run(3200, true, "Work packages"))),
			box("List", 1600200, 3657600,
				item("Dredge the fairway to nine meters."), item("Replace the fenders at berth four."),
				item("Renew the lights on the north mole."), item("Survey the lock gate before the first frost.")),
		),
		slide(
			box("Heading", 457200, 914400, para(run(3200, true, "Budget by quarter"))),
			table(1600200, row(true, "Package", "Third quarter", "Fourth quarter"), row(false, "Dredging", "240", "310"), row(false, "Fenders", "85", "40")),
			box("Note", 3429000, 685800, para(run(2000, false, "All amounts are in thousands."))),
		),
	}

	fill := `<a:solidFill><a:schemeClr val="phClr"/></a:solidFill>`
	stroke := `<a:ln w="9525">` + fill + `</a:ln>`
	effect := `<a:effectStyle><a:effectLst/></a:effectStyle>`
	accents := ""
	for i, c := range []string{"4A6FA5", "A5674A", "6FA54A", "A54A6F", "4AA59B", "9B4AA5"} {
		accents += fmt.Sprintf(`<a:accent%d><a:srgbClr val="%s"/></a:accent%d>`, i+1, c, i+1)
	}
	font := `<a:latin typeface="Arial"/><a:ea typeface=""/><a:cs typeface=""/>`
	theme := xmlHead + `<a:theme xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" name="Plain"><a:themeElements>` +
		`<a:clrScheme name="Plain"><a:dk1><a:srgbClr val="000000"/></a:dk1><a:lt1><a:srgbClr val="FFFFFF"/></a:lt1>` +
		`<a:dk2><a:srgbClr val="333333"/></a:dk2><a:lt2><a:srgbClr val="EEEEEE"/></a:lt2>` + accents +
		`<a:hlink><a:srgbClr val="0000FF"/></a:hlink><a:folHlink><a:srgbClr val="800080"/></a:folHlink></a:clrScheme>` +
		`<a:fontScheme name="Plain"><a:majorFont>` + font + `</a:majorFont><a:minorFont>` + font + `</a:minorFont></a:fontScheme>` +
		`<a:fmtScheme name="Plain"><a:fillStyleLst>` + fill + fill + fill + `</a:fillStyleLst><a:lnStyleLst>` + stroke + stroke + stroke + `</a:lnStyleLst>` +
		`<a:effectStyleLst>` + effect + effect + effect + `</a:effectStyleLst><a:bgFillStyleLst>` + fill + fill + fill + `</a:bgFillStyleLst></a:fmtScheme>` +
		`</a:themeElements></a:theme>`

	overrides, slideRels, slideIDs := "", "", ""
	parts := []part{{}, {"_rels/.rels", relsOpen + `<Relationship Id="rId1" Type="` + relBase + `officeDocument" Target="ppt/presentation.xml"/></Relationships>`}, {}, {}}
	for i, body := range slides {
		n := i + 1
		overrides += fmt.Sprintf(`<Override PartName="/ppt/slides/slide%d.xml" ContentType="%sslide+xml"/>`, n, types)
		slideRels += fmt.Sprintf(`<Relationship Id="rId%d" Type="%sslide" Target="slides/slide%d.xml"/>`, n+2, relBase, n)
		slideIDs += fmt.Sprintf(`<p:sldId id="%d" r:id="rId%d"/>`, 255+n, n+2)
		parts = append(parts,
			part{fmt.Sprintf("ppt/slides/slide%d.xml", n), body},
			part{fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", n), relsOpen + `<Relationship Id="rId1" Type="` + relBase + `slideLayout" Target="../slideLayouts/slideLayout1.xml"/></Relationships>`},
		)
	}
	parts[0] = part{"[Content_Types].xml", xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/ppt/presentation.xml" ContentType="` + types + `presentation.main+xml"/>` +
		`<Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="` + types + `slideMaster+xml"/>` +
		`<Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="` + types + `slideLayout+xml"/>` +
		`<Override PartName="/ppt/theme/theme1.xml" ContentType="application/vnd.openxmlformats-officedocument.theme+xml"/>` +
		overrides + `</Types>`}
	parts[2] = part{"ppt/presentation.xml", xmlHead + `<p:presentation ` + slideNS + `>` +
		`<p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst><p:sldIdLst>` + slideIDs + `</p:sldIdLst>` +
		`<p:sldSz cx="9144000" cy="6858000" type="screen4x3"/><p:notesSz cx="6858000" cy="9144000"/></p:presentation>`}
	parts[3] = part{"ppt/_rels/presentation.xml.rels", relsOpen +
		`<Relationship Id="rId1" Type="` + relBase + `slideMaster" Target="slideMasters/slideMaster1.xml"/>` +
		`<Relationship Id="rId2" Type="` + relBase + `theme" Target="theme/theme1.xml"/>` + slideRels + `</Relationships>`}
	return append(parts,
		part{"ppt/slideMasters/slideMaster1.xml", xmlHead + `<p:sldMaster ` + slideNS + `><p:cSld><p:bg><p:bgRef idx="1001"><a:schemeClr val="bg1"/></p:bgRef></p:bg><p:spTree>` + tree + `</p:spTree></p:cSld>` +
			`<p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>` +
			`<p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst></p:sldMaster>`},
		part{"ppt/slideMasters/_rels/slideMaster1.xml.rels", relsOpen +
			`<Relationship Id="rId1" Type="` + relBase + `slideLayout" Target="../slideLayouts/slideLayout1.xml"/>` +
			`<Relationship Id="rId2" Type="` + relBase + `theme" Target="../theme/theme1.xml"/></Relationships>`},
		part{"ppt/slideLayouts/slideLayout1.xml", xmlHead + `<p:sldLayout ` + slideNS + ` type="blank" preserve="1"><p:cSld name="Blank"><p:spTree>` + tree + `</p:spTree></p:cSld><p:clrMapOvr><a:masterClrMapping/></p:clrMapOvr></p:sldLayout>`},
		part{"ppt/slideLayouts/_rels/slideLayout1.xml.rels", relsOpen + `<Relationship Id="rId1" Type="` + relBase + `slideMaster" Target="../slideMasters/slideMaster1.xml"/></Relationships>`},
		part{"ppt/theme/theme1.xml", theme},
	)
}
