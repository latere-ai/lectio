// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package testfixtures embeds the real document files the intake tests read:
// one file per format intake accepts, and two signed containers. Embedding
// them keeps those tests hermetic, with no network access and no path that
// depends on the working directory. README.md records where each file comes
// from.
package testfixtures

import (
	"embed"
	"testing"
)

//go:embed files
var files embed.FS

// The embedded fixtures, by path.
const (
	MinimalPDF   = "files/minimal.pdf"   // one page, page tree in an object stream
	MultipagePDF = "files/multipage.pdf" // three pages, page tree in the clear
	TextPDF      = "files/text.pdf"      // two letter pages: a line of text and a bar, then nothing
	DOCX         = "files/sample.docx"
	XLSX         = "files/sample.xlsx"
	PPTX         = "files/sample.pptx"
	JPEG         = "files/sample.jpg"
	PNG          = "files/sample.png"
	MultiTIFF    = "files/multipage.tiff" // three frames: 10x10, 10x10, 20x20
	CSV          = "files/sample.csv"
	TXT          = "files/sample.txt"
	HTML         = "files/sample.html"
	XML          = "files/sample.xml"
	Markdown     = "files/sample.md"
	WrappedPDF   = "files/wrapped-pdf.p7m" // DER SignedData around MinimalPDF
	WrappedXML   = "files/wrapped-xml.p7m" // DER SignedData around an XML invoice
	DOC          = "files/sample.doc"      // Word 97 binary
	PPT          = "files/sample.ppt"      // PowerPoint 97 binary
	RTF          = "files/sample.rtf"
	Keynote      = "files/sample.key" // Keynote package in the IWA layout
)

// Read returns the bytes of the fixture at name, and fails the test when no
// such fixture is embedded.
func Read(t testing.TB, name string) []byte {
	t.Helper()
	b, err := files.ReadFile(name)
	if err != nil {
		t.Fatalf("testfixtures: read %q: %v", name, err)
	}
	return b
}
