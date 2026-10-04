// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

// entry is one part of a package a test builds.
type entry struct{ name, body string }

// pack writes entries into a ZIP, deflated, in the order given.
func pack(t testing.TB, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// declaring writes one entry whose directory record declares size bytes,
// whatever the entry holds. body is stored as the entry's deflated stream.
func declaring(t testing.TB, name string, size uint64, body []byte, rest ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: name, Method: zip.Deflate,
		CompressedSize64: uint64(len(body)), UncompressedSize64: size,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	for _, e := range rest {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const (
	wordNS  = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	relsNS  = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
	relType = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
)

// rootRels names the main part of a package.
func rootRels(target string) entry {
	return entry{"_rels/.rels", `<Relationships ` + relsNS + `><Relationship Id="rId1" Type="` + relType + `officeDocument" Target="` + target + `"/></Relationships>`}
}

// docxOf is a word-processing document whose body holds the given markup,
// with any further parts.
func docxOf(t testing.TB, body string, rest ...entry) []byte {
	t.Helper()
	entries := []entry{
		rootRels("word/document.xml"),
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document ` + wordNS + `><w:body>` + body + `</w:body></w:document>`},
	}
	return pack(t, append(entries, rest...)...)
}

// refused reads a word-processing document under bounds and returns the
// code it was refused with, failing the test when it was read.
func refused(t *testing.T, data []byte, b bounds) (fault.Code, string) {
	t.Helper()
	pages, err := readDOCX(context.Background(), data, b)
	if err == nil {
		t.Fatalf("the document was read: %d blocks", len(pages[0].Blocks))
	}
	return fault.CodeOf(err), fault.DetailOf(err)
}

// allocated returns the bytes allocated while do runs.
func allocated(do func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	do()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A part that declares more than a part may inflate to is refused for what
// it declares. Its stream is not valid, so a reader that inflated it would
// report a corrupt file: the code says nothing was inflated.
func TestAPartThatDeclaresMoreThanItsBoundIsRefusedBeforeItIsInflated(t *testing.T) {
	bomb := declaring(t, "word/document.xml", 1<<40, []byte("not a deflate stream"))
	if code, detail := refused(t, bomb, limits); code != fault.FileTooLarge || !strings.Contains(detail, "main document part") {
		t.Fatalf("a part that declares a terabyte: %s, %q", code, detail)
	}
	// The detail names the part by its role and never by a name from the file.
	renamed := declaring(t, "word/secret-name.xml", 1<<40, []byte("x"), rootRels("word/secret-name.xml"))
	if code, detail := refused(t, renamed, limits); code != fault.FileTooLarge || strings.Contains(detail, "secret") {
		t.Fatalf("a part named by the file: %s, %q", code, detail)
	}

	// Parts that are each within the bound are held to it together.
	small := limits
	small.partBytes, small.inflatedBytes = 4096, 6000
	padding := strings.Repeat(" ", 3000)
	together := docxOf(t, `<w:p><w:r><w:t>text</w:t></w:r></w:p>`+padding,
		entry{"word/_rels/document.xml.rels", `<Relationships ` + relsNS + `><Relationship Id="rId1" Type="` + relType + `styles" Target="styles.xml"/></Relationships>`},
		entry{"word/styles.xml", `<w:styles ` + wordNS + `>` + padding + `</w:styles>`},
	)
	if code, _ := refused(t, together, small); code != fault.FileTooLarge {
		t.Fatalf("parts over the bound together: %s", code)
	}
	if _, err := readDOCX(context.Background(), together, limits); err != nil {
		t.Fatalf("the same document under the real bounds: %v", err)
	}
}

// A part that inflates past what its directory record declares is refused
// while it is read, so the declared size bounds what is inflated.
func TestAPartThatInflatesPastWhatItDeclaresIsRefused(t *testing.T) {
	// 32 MiB of paragraphs, deflated, behind a record that declares 512 bytes.
	body := `<w:document ` + wordNS + `><w:body>` + strings.Repeat(`<w:p><w:r><w:t>again</w:t></w:r></w:p>`, 1<<20) + `</w:body></w:document>`
	var deflated bytes.Buffer
	zw := zip.NewWriter(&deflated)
	w, err := zw.Create("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	// The deflated stream of the one entry: after its local header, before
	// its data descriptor.
	raw := deflated.Bytes()
	start := 30 + int(binary.LittleEndian.Uint16(raw[26:])) + int(binary.LittleEndian.Uint16(raw[28:]))
	end := bytes.LastIndex(raw, []byte("PK\x07\x08"))
	lying := declaring(t, "word/document.xml", 512, raw[start:end])

	var code fault.Code
	used := allocated(func() { code, _ = refused(t, lying, limits) })
	if code != fault.DocumentCorrupt {
		t.Fatalf("a part that inflates past what it declares: %s", code)
	}
	if used > 8<<20 {
		t.Fatalf("refusing it allocated %d bytes: the part was inflated past what it declares", used)
	}
}

// A package may list only so many entries, and its directory is read only
// so far: a directory of 500,000 entries is refused without holding
// them.
func TestAPackageOfTooManyEntriesIsRefused(t *testing.T) {
	entries := make([]entry, limits.entries+1)
	for i := range entries {
		entries[i].name = "word/media/" + strings.Repeat("0", i%7) + string(rune('a'+i%26)) + "-" + itoa(i)
	}
	if code, detail := refused(t, pack(t, entries...), limits); code != fault.FileTooLarge || !strings.Contains(detail, "entries") {
		t.Fatalf("one entry over the bound: %s, %q", code, detail)
	}

	// A directory alone, with no entry behind any record: the reader needs
	// nothing else to list a package.
	const count = 500_000
	record := make([]byte, 46+1)
	copy(record, "PK\x01\x02")
	binary.LittleEndian.PutUint16(record[28:], 1) // the length of the name
	record[46] = 'a'
	directory := bytes.Repeat(record, count)
	end := make([]byte, 22)
	copy(end, "PK\x05\x06")
	binary.LittleEndian.PutUint16(end[8:], uint16(count%65536))
	binary.LittleEndian.PutUint16(end[10:], uint16(count%65536))
	binary.LittleEndian.PutUint32(end[12:], uint32(len(directory)))
	file := append(directory, end...)

	var code fault.Code
	var detail string
	used := allocated(func() { code, detail = refused(t, file, limits) })
	if code != fault.FileTooLarge || !strings.Contains(detail, "directory") {
		t.Fatalf("a directory of %d entries: %s, %q", count, code, detail)
	}
	if used > 64<<20 {
		t.Fatalf("refusing it allocated %d bytes: the directory was read whole", used)
	}
}

func itoa(n int) string {
	var digits []byte
	for ; n > 0 || len(digits) == 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

func TestMarkupIsHeldToItsBounds(t *testing.T) {
	deep := strings.Repeat("<w:sdt>", limits.depth) + strings.Repeat("</w:sdt>", limits.depth)
	wide := `<w:p ` + strings.Repeat(`a="" `, limits.runBytes/5+1) + `/>`
	long := `<w:p><w:r><w:t>` + strings.Repeat("a", limits.runBytes+1) + `</w:t></w:r></w:p>`
	few := limits
	few.tokens = 1000
	crowded := limits
	crowded.blocks = 10

	for name, tc := range map[string]struct {
		data []byte
		b    bounds
		code fault.Code
		says string
	}{
		"elements nested past the bound": {docxOf(t, deep), limits, fault.DocumentCorrupt, "deep"},
		"a start tag past the bound":     {docxOf(t, wide), limits, fault.DocumentCorrupt, "tag"},
		"a run of text past the bound":   {docxOf(t, long), limits, fault.DocumentCorrupt, "run of text"},
		"tokens past the bound":          {docxOf(t, strings.Repeat(`<w:p/>`, 600)), few, fault.FileTooLarge, "tokens"},
		"blocks past the bound":          {docxOf(t, strings.Repeat(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`, 11)), crowded, fault.FileTooLarge, "blocks"},
		"a part with no markup":          {pack(t, entry{"word/document.xml", ""}), limits, fault.DocumentCorrupt, "no markup"},
		"a part that is not markup":      {pack(t, entry{"word/document.xml", "<w:document"}), limits, fault.DocumentCorrupt, "markup"},
		"a part of another vocabulary":   {pack(t, entry{"word/document.xml", "<workbook/>"}), limits, fault.DocumentCorrupt, "not a word-processing document"},
		"a file that is no package":      {[]byte("PK\x03\x04 and nothing else"), limits, fault.DocumentCorrupt, "not a readable package"},
		"a package with no main part":    {pack(t, entry{"word/other.xml", "<a/>"}), limits, fault.DocumentCorrupt, "no main document part"},
	} {
		code, detail := refused(t, tc.data, tc.b)
		if code != tc.code || !strings.Contains(detail, tc.says) {
			t.Errorf("%s: %s, %q; want %s saying %q", name, code, detail, tc.code, tc.says)
		}
	}

	// At the bound, a document is read.
	atDepth := strings.Repeat("<w:sdt>", limits.depth-5) + `<w:p><w:r><w:t>deep</w:t></w:r></w:p>` + strings.Repeat("</w:sdt>", limits.depth-5)
	pages, err := readDOCX(context.Background(), docxOf(t, atDepth), limits)
	if err != nil || len(pages[0].Blocks) != 1 || pages[0].Blocks[0].Text != "deep" {
		t.Fatalf("a document nested to the bound: %v", err)
	}
}

// The format forbids a document type, and the decoder defines no entity
// from one: a part that declares an entity is refused, and nothing the
// entity names is read.
func TestAPartThatDeclaresAnEntityIsRefused(t *testing.T) {
	paragraph := func(text string) string {
		return `<w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`
	}
	for name, document := range map[string]string{
		"an external entity": `<!DOCTYPE w:document [<!ENTITY xxe SYSTEM "file:///etc/hostname">]><w:document ` + wordNS + `>` + paragraph("&xxe;"),
		"an entity that expands": `<!DOCTYPE w:document [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;"><!ENTITY c "&b;&b;&b;&b;&b;&b;&b;&b;&b;&b;">]>` +
			`<w:document ` + wordNS + `>` + paragraph("&c;&c;&c;&c;&c;&c;&c;&c;&c;&c;"),
		"an external subset":                `<!DOCTYPE w:document SYSTEM "http://203.0.113.7/lectio.dtd"><w:document ` + wordNS + `>` + paragraph("text"),
		"an entity nothing declares":        `<w:document ` + wordNS + `>` + paragraph("&xxe;"),
		"a document type after the content": `<w:document ` + wordNS + `>` + paragraph("text") + `<!DOCTYPE late>`,
	} {
		if code, _ := refused(t, pack(t, entry{"word/document.xml", document}), limits); code != fault.DocumentCorrupt {
			t.Errorf("%s: %s, want %s", name, code, fault.DocumentCorrupt)
		}
	}
}

// A relationship reaches a part of the same package or nothing: a target
// that says it is external, and one whose path climbs out of the package,
// are not followed, whatever the archive holds under that name.
func TestARelationshipThatLeavesThePackageIsNotFollowed(t *testing.T) {
	body := `<w:p><w:pPr><w:pStyle w:val="Outside"/></w:pPr><w:r><w:t>Body.</w:t></w:r><w:r><w:footnoteReference w:id="2"/></w:r></w:p>`
	note := `<w:footnotes ` + wordNS + `><w:footnote w:id="2"><w:p><w:r><w:t>NOT IN THE PACKAGE</w:t></w:r></w:p></w:footnote></w:footnotes>`
	styles := `<w:styles ` + wordNS + `><w:style w:type="paragraph" w:styleId="Outside"><w:name w:val="heading 1"/></w:style></w:styles>`
	rels := func(footnotes, styles string) entry {
		return entry{"word/_rels/document.xml.rels", `<Relationships ` + relsNS + `>` +
			`<Relationship Id="rId1" Type="` + relType + `footnotes" ` + footnotes + `/>` +
			`<Relationship Id="rId2" Type="` + relType + `styles" ` + styles + `/>` +
			`<Relationship Id="rId3" Type="` + relType + `image" Target="http://203.0.113.7/pixel.png" TargetMode="External"/>` +
			`</Relationships>`}
	}

	outside := docxOf(t, body,
		rels(`Target="../../outside/footnotes.xml"`, `Target="file:///etc/styles.xml" TargetMode="External"`),
		entry{"../outside/footnotes.xml", note}, entry{"outside/footnotes.xml", note},
		entry{"file:/etc/styles.xml", styles}, entry{"word/file:/etc/styles.xml", styles},
	)
	pages, err := readDOCX(context.Background(), outside, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := shape(pages[0]); len(got) != 1 || got[0] != "text:Body." {
		t.Fatalf("blocks = %q; a part outside the package was read", got)
	}

	// The same parts inside the package are followed, by a relative path
	// and by one from the package's root.
	inside := docxOf(t, body,
		rels(`Target="footnotes.xml"`, `Target="/word/../word/styles.xml"`),
		entry{"word/footnotes.xml", note}, entry{"word/styles.xml", styles},
	)
	pages, err = readDOCX(context.Background(), inside, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := shape(pages[0]); len(got) != 2 || got[0] != "heading1:Body." || got[1] != "footnote:NOT IN THE PACKAGE" {
		t.Fatalf("blocks = %q", got)
	}

	for target, want := range map[string]string{
		"styles.xml": "word/styles.xml", "./media/../styles.xml": "word/styles.xml", "/word/styles.xml": "word/styles.xml",
		"../customXml/item1.xml": "customXml/item1.xml",
		"":                       "", "..": "", "../..": "", "../../etc/passwd": "", "/../etc/passwd": "", "/": "", "//etc/passwd": "",
	} {
		got, inside := resolve("word/", target)
		if got != want || inside != (want != "") {
			t.Errorf("resolve(%q) = %q, %v; want %q", target, got, inside, want)
		}
	}
}

func TestAPackageIsReadWhateverItsEntriesAreCalled(t *testing.T) {
	// Part names compare without case, the first entry of a name is the
	// part, and an entry whose name leaves the package is passed over.
	data := pack(t,
		entry{"../../outside.txt", "not a part"},
		rootRels("WORD/Document.xml"),
		entry{"Word/document.XML", `<w:document ` + wordNS + `><w:body><w:p><w:r><w:t>first</w:t></w:r></w:p></w:body></w:document>`},
		entry{"word/document.xml", `<w:document ` + wordNS + `><w:body><w:p><w:r><w:t>second</w:t></w:r></w:p></w:body></w:document>`},
	)
	pages, err := readDOCX(context.Background(), data, limits)
	if err != nil || len(pages[0].Blocks) != 1 || pages[0].Blocks[0].Text != "first" {
		t.Fatalf("pages = %+v, %v", pages, err)
	}

	// An entry stored with a method nothing reads, and one whose checksum
	// does not match its bytes, are corrupt.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "word/document.xml", Method: 99, CompressedSize64: 1, UncompressedSize64: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if code, detail := refused(t, buf.Bytes(), limits); code != fault.DocumentCorrupt || !strings.Contains(detail, "could not be opened") {
		t.Fatalf("an unknown method: %s, %q", code, detail)
	}

	body := []byte(`<w:document ` + wordNS + `><w:body/></w:document>`)
	buf.Reset()
	zw = zip.NewWriter(&buf)
	w, err = zw.CreateRaw(&zip.FileHeader{Name: "word/document.xml", Method: zip.Store, CRC32: crc32.ChecksumIEEE(body) + 1,
		CompressedSize64: uint64(len(body)), UncompressedSize64: uint64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if code, _ := refused(t, buf.Bytes(), limits); code != fault.DocumentCorrupt {
		t.Fatalf("a checksum that does not match: %s", code)
	}
}

func TestAReadStopsWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Enough tokens to reach the point where the context is looked at.
	data := docxOf(t, strings.Repeat(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`, 2000))
	if _, err := readDOCX(ctx, data, limits); err == nil || fault.CodeOf(err) != fault.Internal || ctx.Err() == nil {
		t.Fatalf("a read under a context that ended: %v", err)
	}
}

func TestTableOf(t *testing.T) {
	b := tableOf(3, 3, []document.Cell{
		{Row: 0, Col: 0, ColSpan: 2, Header: true, Text: ""},
		{Row: 0, Col: 2, RowSpan: 3, Text: "a<b"},
		{Row: 2, Col: 0, Text: "x"}, {Row: 2, Col: 1, Text: "y"},
	})
	if want := `<table><tr><th colspan="2"></th><td rowspan="3">a&lt;b</td></tr><tr></tr><tr><td>x</td><td>y</td></tr></table>`; b.Table.HTML != want {
		t.Fatalf("markup = %s", b.Table.HTML)
	}
	if want := " | a<b\nx | y"; b.Text != want || b.Kind != document.KindTable || b.Table.Rows != 3 || b.Table.Cols != 3 {
		t.Fatalf("text = %q", b.Text)
	}
}

// The bound is on the bytes between one opening bracket and the next: a
// tag and the text after it, together.
func TestRunsBoundsTheBytesBetweenBrackets(t *testing.T) {
	for input, within := range map[string]bool{
		"<a>abc</a>": true, "abc<a>": true, "abcdefg<a>": false, "<a>abcdef": false, "<a b='cdefg'>": false,
	} {
		r := &runs{r: strings.NewReader(input), max: 6}
		if _, err := r.Read(make([]byte, 64)); (err == nil) != within {
			t.Errorf("%q: err = %v, want within the bound: %v", input, err, within)
		}
	}
}
