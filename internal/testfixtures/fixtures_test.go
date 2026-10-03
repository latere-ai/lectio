// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testfixtures

import (
	"slices"
	"testing"
)

// named is every fixture constant. A constant missing here fails
// TestEveryEmbeddedFileIsNamed as soon as its file exists.
var named = []string{
	MinimalPDF, MultipagePDF, DOCX, XLSX, PPTX, JPEG, PNG, MultiTIFF,
	CSV, TXT, HTML, XML, Markdown, WrappedPDF, WrappedXML,
	DOC, PPT, RTF, Keynote,
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
