// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package quality_test holds Lectio to its quality corpus: the files of
// internal/testfixtures whose content is known block by block. Two tests
// run them.
//
// The gate, in gate_test.go, runs on every push and calls no model. A file
// read from its own structure goes through the pipeline and is scored
// against its truth, which it must match exactly. For a file a model
// reads, the gate holds the pipeline to what the pipeline alone decides:
// what the file is, how many pages it has, which are selected, the image a
// reader is given, which pages are blank, and what assembly makes of pages
// that were read without a mistake.
//
// The live run, in live_test.go, is opt-in: it reads the same corpus with
// a configured reader through the server as it ships, scores every file,
// and writes a report. docs/quality.md describes both.
package quality_test

import (
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/quality"
	"latere.ai/x/lectio/internal/testfixtures"
)

// file is one file of the corpus: where it is, what it is scored against,
// and what the pipeline is to make of it before any page is read.
type file struct {
	// name is the name the file is uploaded under, and fixture where it is
	// in internal/testfixtures.
	name, fixture string
	// format says what the file is, for a report.
	format string
	// class decides the bars the file's scores are held to.
	class quality.Class

	// truth is the fixture that holds the file's truth, and pages the pages
	// of that truth the file holds, in order; nil is every page.
	truth string
	pages []int

	// detected is the media type the file is detected as, and working the
	// one its pages are read from: another one when the file is converted
	// first.
	detected, working string
	// source says who produces the blocks: a reader, or the format itself.
	source document.PageSource
	// blank lists the pages with nothing on them.
	blank []int
	// width and height are the size in pixels of the image a reader is
	// given of page 1 when it asks for 160 dpi: an image file's own size,
	// and for a PDF its page at that resolution, each side rounded up. Both
	// are 0 for a file whose pages a conversion decides.
	width, height int
}

// corpus is every file of the quality corpus.
var corpus = []file{
	{
		name: "plain.txt", fixture: testfixtures.PlainTXT, format: "plain text", class: quality.Exact,
		truth: testfixtures.PlainTruth, detected: detect.MIMETXT, working: detect.MIMETXT, source: document.SourceNative,
	},
	{
		name: "notes.md", fixture: testfixtures.NotesMD, format: "Markdown", class: quality.Exact,
		truth: testfixtures.NotesTruth, detected: detect.MIMEMarkdown, working: detect.MIMEMarkdown, source: document.SourceNative,
	},
	{
		name: "readings.csv", fixture: testfixtures.ReadingsCSV, format: "CSV", class: quality.Exact,
		truth: testfixtures.ReadingsTruth, detected: detect.MIMECSV, working: detect.MIMECSV, source: document.SourceNative,
	},
	{
		name: "report.docx", fixture: testfixtures.ReportDOCX, format: "Word document", class: quality.Exact,
		truth: testfixtures.ReportTruth, detected: detect.MIMEDOCX, working: detect.MIMEDOCX, source: document.SourceNative,
	},
	{
		name: "ledger.xlsx", fixture: testfixtures.LedgerXLSX, format: "workbook, 3 sheets", class: quality.Exact,
		truth: testfixtures.LedgerTruth, detected: detect.MIMEXLSX, working: detect.MIMEXLSX, source: document.SourceNative,
	},
	{
		name: "report.doc", fixture: testfixtures.ReportDOC, format: "Word 97 document, converted", class: quality.Converted,
		truth: testfixtures.ReportTruth, detected: detect.MIMEDOC, working: detect.MIMEDOCX, source: document.SourceNative,
	},
	{
		name: "slides.pptx", fixture: testfixtures.SlidesPPTX, format: "presentation, converted", class: quality.Converted,
		truth: testfixtures.SlidesTruth, detected: detect.MIMEPPTX, working: detect.MIMEPDF, source: document.SourceReader,
	},
	{
		name: "memo.rtf", fixture: testfixtures.MemoRTF, format: "rich text, converted", class: quality.Converted,
		truth: testfixtures.MemoTruth, detected: detect.MIMERTF, working: detect.MIMEPDF, source: document.SourceReader,
	},
	{
		name: "survey.pdf", fixture: testfixtures.SurveyPDF, format: "typeset PDF", class: quality.Typeset,
		truth: testfixtures.SurveyTruth, detected: detect.MIMEPDF, working: detect.MIMEPDF, source: document.SourceReader,
		blank: []int{4}, width: 1323, height: 1871,
	},
	{
		name: "survey-scan.pdf", fixture: testfixtures.SurveyScanPDF, format: "scanned PDF", class: quality.Scan,
		truth: testfixtures.SurveyTruth, detected: detect.MIMEPDF, working: detect.MIMEPDF, source: document.SourceReader,
		blank: []int{4}, width: 1323, height: 1872,
	},
	{
		name: "survey-1.png", fixture: testfixtures.SurveyPNG, format: "PNG of a page", class: quality.Scan,
		truth: testfixtures.SurveyTruth, pages: []int{1}, detected: detect.MIMEPNG, working: detect.MIMEPNG, source: document.SourceReader,
		width: 992, height: 1404,
	},
	{
		name: "survey-1.jpg", fixture: testfixtures.SurveyJPEG, format: "JPEG of a page", class: quality.Scan,
		truth: testfixtures.SurveyTruth, pages: []int{1}, detected: detect.MIMEJPEG, working: detect.MIMEJPEG, source: document.SourceReader,
		width: 992, height: 1404,
	},
	{
		name: "survey-2-4.tiff", fixture: testfixtures.SurveyTIFF, format: "TIFF of 3 frames", class: quality.Scan,
		truth: testfixtures.SurveyTruth, pages: []int{2, 3, 4}, detected: detect.MIMETIFF, working: detect.MIMETIFF, source: document.SourceReader,
		blank: []int{3}, width: 992, height: 1404,
	},
}

// truthOf reads the truth of a corpus file: the pages the file holds.
func truthOf(t testing.TB, f file) quality.Truth {
	t.Helper()
	truth, err := quality.Parse(testfixtures.Read(t, f.truth))
	if err != nil {
		t.Fatalf("%s: %v", f.truth, err)
	}
	if f.pages != nil {
		truth = truth.Select(f.pages...)
	}
	return truth
}
