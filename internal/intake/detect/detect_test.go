// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package detect

import (
	"strings"
	"testing"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/testfixtures"
)

func TestDetectFixturesContentFirst(t *testing.T) {
	tests := []struct {
		fixture   string
		wantMIME  string
		wantClass RouteClass
	}{
		{testfixtures.MinimalPDF, MIMEPDF, ClassReader},
		{testfixtures.MultipagePDF, MIMEPDF, ClassReader},
		{testfixtures.JPEG, MIMEJPEG, ClassReader},
		{testfixtures.PNG, MIMEPNG, ClassReader},
		{testfixtures.MultiTIFF, MIMETIFF, ClassReader},
		{testfixtures.DOCX, MIMEDOCX, ClassNativeDOCX},
		{testfixtures.XLSX, MIMEXLSX, ClassNativeSpreadsheet},
		{testfixtures.PPTX, MIMEPPTX, ClassConvertToPDF},
		{testfixtures.RTF, MIMERTF, ClassConvertToPDF},
		{testfixtures.Keynote, MIMEKeynote, ClassConvertToPDF},
		{testfixtures.ODT, MIMEODT, ClassConvertToPDF},
		{testfixtures.ODP, MIMEODP, ClassConvertToPDF},
		{testfixtures.ReportDOCX, MIMEDOCX, ClassNativeDOCX},
		{testfixtures.ReportSuiteDOCX, MIMEDOCX, ClassNativeDOCX},
		{testfixtures.LedgerXLSX, MIMEXLSX, ClassNativeSpreadsheet},
		{testfixtures.LedgerSuiteXLSX, MIMEXLSX, ClassNativeSpreadsheet},
		{testfixtures.LedgerXLSM, MIMEXLSM, ClassNativeSpreadsheet},
		{testfixtures.WrappedPDF, MIMEPKCS7MIME, ClassUnwrapP7M},
		{testfixtures.WrappedXML, MIMEPKCS7MIME, ClassUnwrapP7M},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			b := testfixtures.Read(t, tt.fixture)
			// The content alone: no file name and no declared type.
			mime, err := Detect(b, DeclaredType{})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if mime != tt.wantMIME {
				t.Fatalf("mime = %q, want %q", mime, tt.wantMIME)
			}
			class, ok := ClassOf(mime)
			if !ok || class != tt.wantClass {
				t.Errorf("class = %q (ok=%v), want %q", class, ok, tt.wantClass)
			}
		})
	}
}

// TestDetectContentBeatsNameAndDeclaredType covers the misnamed and the
// mislabeled file: what the bytes are wins over what the submitter called
// them.
func TestDetectContentBeatsNameAndDeclaredType(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		declared DeclaredType
		want     string
	}{
		{"pdf named as text", testfixtures.MinimalPDF, DeclaredType{FileName: "scan.txt", MIME: "text/plain"}, MIMEPDF},
		{"docx named as pdf", testfixtures.DOCX, DeclaredType{FileName: "report.pdf", MIME: "application/pdf"}, MIMEDOCX},
		{"png declared as jpeg", testfixtures.PNG, DeclaredType{MIME: "image/jpeg"}, MIMEPNG},
		{"signed container named as xml", testfixtures.WrappedXML, DeclaredType{FileName: "invoice.xml"}, MIMEPKCS7MIME},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mime, err := Detect(testfixtures.Read(t, tt.fixture), tt.declared)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if mime != tt.want {
				t.Errorf("mime = %q, want %q", mime, tt.want)
			}
		})
	}
}

func TestDetectExtensionFallback(t *testing.T) {
	cases := []struct {
		fixture  string
		filename string
		want     string
	}{
		// CSV, plain text and Markdown all sniff as plain text; the extension
		// tells them apart.
		{testfixtures.CSV, "data.csv", MIMECSV},
		{testfixtures.TXT, "notes.txt", MIMETXT},
		{testfixtures.Markdown, "README.md", MIMEMarkdown},
		{testfixtures.Markdown, "README.MD", MIMEMarkdown},
		// The legacy binary office formats are OLE2 containers, which the
		// sniffer does not open; the extension names them.
		{testfixtures.DOC, "memo.doc", MIMEDOC},
		{testfixtures.PPT, "deck.ppt", MIMEPPT},
	}
	for _, c := range cases {
		t.Run(c.filename, func(t *testing.T) {
			b := testfixtures.Read(t, c.fixture)
			mime, err := Detect(b, DeclaredType{FileName: c.filename})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if mime != c.want {
				t.Errorf("mime = %q, want %q", mime, c.want)
			}
		})
	}
}

// TestDetectPlainTextKeepsANonTextExtensionOut covers the limit of the
// refinement: text content is refined only to a text format, so a text file
// named like a binary format stays plain text.
func TestDetectPlainTextKeepsANonTextExtensionOut(t *testing.T) {
	mime, err := Detect(testfixtures.Read(t, testfixtures.TXT), DeclaredType{FileName: "notes.pdf"})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if mime != MIMETXT {
		t.Errorf("mime = %q, want %q", mime, MIMETXT)
	}
}

func TestDetectDeclaredFallback(t *testing.T) {
	// Bytes that sniff to nothing, a name with no known extension, and a
	// declared type that is supported, parameters and case aside.
	binary := []byte("\x00\x01\x02\x03random binary")
	for _, declared := range []DeclaredType{
		{MIME: "application/pdf"},
		{MIME: "Application/PDF; qs=0.9", FileName: "download"},
		{MIME: "application/pdf", FileName: "archive.bin"},
	} {
		mime, err := Detect(binary, declared)
		if err != nil || mime != MIMEPDF {
			t.Errorf("Detect(%+v) = %q, %v; want %q, nil", declared, mime, err, MIMEPDF)
		}
	}
}

func TestDetectUnsupported(t *testing.T) {
	tests := []struct {
		name     string
		peek     []byte
		declared DeclaredType
	}{
		{"unidentifiable bytes and no hints", []byte("\x00\x01\x02\x03\x04\x05\x06\x07garbage"), DeclaredType{}},
		{"too short to sniff and no hints", []byte("%P"), DeclaredType{}},
		{"empty and no hints", nil, DeclaredType{}},
		// A generic binary type is never an answer.
		{"declared generic binary", []byte("\x00\x01\x02\x03random"), DeclaredType{MIME: MIMEOctet}},
		{"declared an unsupported type", []byte("\x00\x01\x02\x03random"), DeclaredType{MIME: "video/mp4", FileName: "clip.mp4"}},
		// An open document template begins like a document and is none.
		{"an open document template", append(zipHead("mimetype"+MIMEODT+"-template"), zipHead("content.xml")...), DeclaredType{}},
		// A ZIP that is no supported office package is refused, not guessed.
		{"plain zip", zipHead("random/file.txt"), DeclaredType{}},
		{"plain zip declared as zip", zipHead("random/file.txt"), DeclaredType{MIME: "application/zip", FileName: "bundle.zip"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mime, err := Detect(tt.peek, tt.declared)
			if err == nil {
				t.Fatalf("Detect = %q, want an error", mime)
			}
			if got := fault.CodeOf(err); got != fault.UnsupportedMediaType {
				t.Errorf("code = %q, want %q", got, fault.UnsupportedMediaType)
			}
		})
	}
}

// A legacy spreadsheet is known and not read. It is refused where it is
// detected, with a detail that says what to do, whether its name or its
// declared type names it.
func TestDetectRefusesALegacySpreadsheet(t *testing.T) {
	// The first bytes of a compound file, which is all the content says.
	compound := []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1\x00\x00\x00\x00\x00\x00\x00\x00")
	for _, declared := range []DeclaredType{
		{FileName: "ledger.xls"},
		{FileName: "LEDGER.XLS", MIME: "application/pdf"},
		{MIME: "application/vnd.ms-excel"},
	} {
		mime, err := Detect(compound, declared)
		if err == nil {
			t.Fatalf("Detect(%+v) = %q, want a refusal", declared, mime)
		}
		if got := fault.CodeOf(err); got != fault.UnsupportedMediaType {
			t.Errorf("Detect(%+v): code = %q, want %q", declared, got, fault.UnsupportedMediaType)
		}
		if detail := fault.DetailOf(err); !strings.Contains(detail, ".xls") || !strings.Contains(detail, ".xlsx") {
			t.Errorf("Detect(%+v): the detail does not say what to do: %q", declared, detail)
		}
	}
	if class, ok := ClassOf(MIMEXLS); ok {
		t.Errorf("a legacy spreadsheet has the route %q, and nothing reads it", class)
	}
}

// HTML and XML are known and not read. Each is refused where it is
// detected, with a detail that names the format: by its content whatever
// it is called, and by its name or its declared type when the content says
// only text or nothing. Neither has a route.
func TestDetectRefusesHTMLAndXML(t *testing.T) {
	html, xml := testfixtures.Read(t, testfixtures.HTML), testfixtures.Read(t, testfixtures.XML)
	for name, tc := range map[string]struct {
		peek     []byte
		declared DeclaredType
		format   string
	}{
		"an html page":                         {html, DeclaredType{}, "HTML"},
		"an html page named as csv":            {html, DeclaredType{FileName: "table.csv"}, "HTML"},
		"an html page declared as text":        {html, DeclaredType{MIME: "text/plain"}, "HTML"},
		"html markup alone":                    {[]byte("<html><body>x</body></html>"), DeclaredType{}, "HTML"},
		"text named as html":                   {[]byte("just some words"), DeclaredType{FileName: "page.htm"}, "HTML"},
		"bytes declared as html":               {[]byte("\x00\x01\x02\x03random"), DeclaredType{MIME: "text/html; charset=utf-8"}, "HTML"},
		"an xml file":                          {xml, DeclaredType{}, "XML"},
		"an xml file named as text":            {xml, DeclaredType{FileName: "data.txt"}, "XML"},
		"an xml declaration after white space": {[]byte("\n  <?xml version=\"1.0\"?><a/>"), DeclaredType{}, "XML"},
		"text named as xml":                    {[]byte("just some words"), DeclaredType{FileName: "notes.xml"}, "XML"},
	} {
		t.Run(name, func(t *testing.T) {
			mime, err := Detect(tc.peek, tc.declared)
			if err == nil {
				t.Fatalf("Detect = %q, want a refusal", mime)
			}
			if got := fault.CodeOf(err); got != fault.UnsupportedMediaType {
				t.Errorf("code = %q, want %q", got, fault.UnsupportedMediaType)
			}
			if detail := fault.DetailOf(err); !strings.Contains(detail, tc.format) {
				t.Errorf("the detail does not name %s: %q", tc.format, detail)
			}
		})
	}
	for _, mime := range []string{MIMEHTML, MIMEXMLText} {
		if class, ok := ClassOf(mime); ok {
			t.Errorf("%s has the route %q, and nothing reads it", mime, class)
		}
	}
}

// zipHead is the head of a ZIP whose first local file header names part: the
// signature, the 26 bytes of fixed fields, and the name.
func zipHead(part string) []byte {
	b := append([]byte("PK\x03\x04"), make([]byte, 26)...)
	return append(b, part...)
}

// TestDetectZippedPackagesByPartName covers each part name the ZIP probe
// knows, on a head that holds nothing else.
func TestDetectZippedPackagesByPartName(t *testing.T) {
	tests := []struct {
		parts []string
		want  string
	}{
		{[]string{"word/document.xml"}, MIMEDOCX},
		{[]string{"ppt/presentation.xml"}, MIMEPPTX},
		{[]string{"ppt/slides/slide1.xml"}, MIMEPPTX},
		{[]string{"xl/workbook.xml"}, MIMEXLSX},
		{[]string{"xl/worksheets/sheet1.xml"}, MIMEXLSX},
		{[]string{"xl/workbook.xml", "xl/vbaProject.bin"}, MIMEXLSM},
		{[]string{"index.apxl"}, MIMEKeynote},
		{[]string{"Index/Document.iwa"}, MIMEKeynote},
		{[]string{"mimetype" + MIMEODT, "content.xml"}, MIMEODT},
		{[]string{"mimetype" + MIMEODP, "content.xml"}, MIMEODP},
	}
	for _, tt := range tests {
		t.Run(tt.parts[0], func(t *testing.T) {
			var b []byte
			for _, p := range tt.parts {
				b = append(b, zipHead(p)...)
			}
			mime, err := Detect(b, DeclaredType{})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if mime != tt.want {
				t.Errorf("mime = %q, want %q", mime, tt.want)
			}
		})
	}
}

// TestDetectAnUnknownZipFallsBackToItsName covers a package whose part names
// lie past the head: the content says only ZIP, so the extension decides.
func TestDetectAnUnknownZipFallsBackToItsName(t *testing.T) {
	for name, want := range map[string]string{"report.docx": MIMEDOCX, "letter.odt": MIMEODT, "deck.ODP": MIMEODP} {
		mime, err := Detect(zipHead("[Content_Types].xml"), DeclaredType{FileName: name})
		if err != nil {
			t.Fatalf("Detect(%s): %v", name, err)
		}
		if mime != want {
			t.Errorf("Detect(%s) = %q, want %q", name, mime, want)
		}
	}
}

func TestDetectTextByContent(t *testing.T) {
	tests := []struct {
		name string
		peek string
		want string
	}{
		{"plain text", "just some words", MIMETXT},
		{"big-endian tiff", "MM\x00*\x00\x00\x00\x08", MIMETIFF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mime, err := Detect([]byte(tt.peek), DeclaredType{})
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if mime != tt.want {
				t.Errorf("mime = %q, want %q", mime, tt.want)
			}
		})
	}
}

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"text/html; charset=utf-8": MIMEHTML,
		" Text/HTML ":              MIMEHTML,
		"application/xml":          MIMEXMLText,
		"text/xml":                 MIMEXMLText,
		"TEXT/RTF":                 MIMERTF,
		"image/jpg":                MIMEJPEG,
		"image/pjpeg":              MIMEJPEG,
		"image/x-png":              MIMEPNG,
		"image/tif":                MIMETIFF,
		"image/x-tiff":             MIMETIFF,
		"application/vnd.ms-word":  MIMEDOC,
		"application/word":         MIMEDOC,
		"video/mp4":                "video/mp4",
		"":                         "",
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassOf(t *testing.T) {
	if _, ok := ClassOf("application/zip"); ok {
		t.Error("application/zip should be unsupported")
	}
	// An alias and its parameters resolve to the canonical type's class.
	if class, ok := ClassOf("image/jpg; q=1"); !ok || class != ClassReader {
		t.Errorf("ClassOf(image/jpg; q=1) = %q, %v; want %q, true", class, ok, ClassReader)
	}
}

// TestEveryConvertedTypeHasAFixture keeps the set of types that need
// conversion equal to the set with a fixture, so a type cannot be routed to
// conversion with no real file to prove the conversion on.
func TestEveryConvertedTypeHasAFixture(t *testing.T) {
	withFixture := map[string]string{
		MIMEDOC:     testfixtures.DOC,
		MIMEPPT:     testfixtures.PPT,
		MIMEPPTX:    testfixtures.PPTX,
		MIMERTF:     testfixtures.RTF,
		MIMEKeynote: testfixtures.Keynote,
		MIMEODT:     testfixtures.ODT,
		MIMEODP:     testfixtures.ODP,
	}
	converted := 0
	for mime, class := range classByMIME {
		if class != ClassConvertDOCToDOCX && class != ClassConvertToPDF {
			continue
		}
		converted++
		if _, ok := withFixture[mime]; !ok {
			t.Errorf("%s needs conversion and has no fixture", mime)
		}
	}
	if converted != len(withFixture) {
		t.Errorf("%d types need conversion, %d have fixtures; keep the two sets equal", converted, len(withFixture))
	}
}
