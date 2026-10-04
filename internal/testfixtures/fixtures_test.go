// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testfixtures

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// named is every fixture constant. A constant missing here fails
// TestEveryEmbeddedFileIsNamed as soon as its file exists.
var named = []string{
	MinimalPDF, MultipagePDF, TextPDF, DOCX, XLSX, PPTX, JPEG, PNG, MultiTIFF,
	CSV, TXT, HTML, XML, Markdown, WrappedPDF, WrappedXML,
	DOC, PPT, RTF, Keynote, ODT, ODP,
	ReportDOCX, LedgerXLSX, LedgerXLSM, ReportSuiteDOCX, LedgerSuiteXLSX,
}

func TestReadReturnsEveryNamedFixture(t *testing.T) {
	for _, name := range named {
		if b := Read(t, name); len(b) == 0 {
			t.Errorf("fixture %q is empty", name)
		}
	}
}

// TestEveryEmbeddedFileIsNamed keeps the directory and the constants equal:
// a file no constant names is a file no test reads.
func TestEveryEmbeddedFileIsNamed(t *testing.T) {
	entries, err := files.ReadDir("files")
	if err != nil {
		t.Fatalf("read the embedded directory: %v", err)
	}
	if len(entries) != len(named) {
		t.Errorf("%d files are embedded, %d are named", len(entries), len(named))
	}
	for _, e := range entries {
		if path := "files/" + e.Name(); !slices.Contains(named, path) {
			t.Errorf("%s is embedded and no constant names it", path)
		}
	}
}

// failRecorder is a testing.TB whose Fatalf records the call and returns, so
// a test can observe that Read fails the test it was given.
type failRecorder struct {
	testing.TB
	failed bool
}

func (r *failRecorder) Helper() {}

func (r *failRecorder) Fatalf(string, ...any) { r.failed = true }

func TestReadFailsTheTestForAnUnknownFixture(t *testing.T) {
	rec := &failRecorder{TB: t}
	if b := Read(rec, "files/absent.bin"); b != nil {
		t.Errorf("Read returned %d bytes for a fixture that does not exist", len(b))
	}
	if !rec.failed {
		t.Error("Read did not fail the test for a fixture that does not exist")
	}
}

// A logbook is a PDF of the pages asked for, each holding the entry
// LogbookEntry says it holds, but for the pages that are pictures.
func TestALogbookHoldsAnEntryToAPage(t *testing.T) {
	pdf := Logbook(3, 2)
	if !bytes.HasPrefix(pdf, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Fatalf("a logbook is a PDF: %q ... %q", pdf[:9], pdf[len(pdf)-6:])
	}
	heading, paragraphs := LogbookEntry(3)
	if heading != "Entry 3 of the harbor log" || len(paragraphs) != 2 || !strings.Contains(paragraphs[0], "Reading 3 stands") {
		t.Fatalf("entry 3: %q, %q", heading, paragraphs)
	}
	for _, want := range []string{"/Count 3", "(Entry 1 of the harbor log) Tj", "(Entry 3 of the harbor log) Tj", "/Im Do", "(3) Tj"} {
		if !bytes.Contains(pdf, []byte(want)) {
			t.Errorf("the file lacks %s", want)
		}
	}
	if bytes.Contains(pdf, []byte("(Entry 2 of the harbor log)")) {
		t.Error("the page that is a picture holds its entry's text")
	}
	// The table of the file's objects names where each begins.
	at := bytes.LastIndex(pdf, []byte("startxref\n"))
	var xref int
	if _, err := fmt.Sscanf(string(pdf[at:]), "startxref\n%d", &xref); err != nil || !bytes.HasPrefix(pdf[xref:], []byte("xref\n0 12\n")) {
		t.Fatalf("the table of objects: %v, %q", err, pdf[xref:xref+12])
	}
	for n, line := range strings.Split(string(pdf[xref:at]), "\n")[2:13] {
		var offset int
		if _, err := fmt.Sscanf(line, "%010d 00000 n", &offset); n > 0 && (err != nil || !bytes.HasPrefix(pdf[offset:], fmt.Appendf(nil, "%d 0 obj\n", n))) {
			t.Errorf("object %d does not begin at %d: %v", n, offset, err)
		}
	}
	if lines := wrapped("one two three four", 9); !slices.Equal(lines, []string{"one two", "three", "four"}) {
		t.Errorf("a paragraph is broken at its spaces: %q", lines)
	}
}
