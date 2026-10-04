// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testfixtures

import "embed"

// corpusFiles holds the quality corpus: files whose content is known, each with
// the truth it is scored against. Everything in it was written for this
// repository, and README.md says how each file was made.
//
//go:embed quality
var corpusFiles embed.FS

// The quality corpus, by path. A truth is the JSON internal/quality reads.
const (
	// The survey: 3 typeset pages and a blank one, with headings, lists, a
	// formula, a figure, a table with merged cells and a table that
	// continues across 2 pages. SurveyHTML is its source.
	SurveyHTML  = "quality/survey.html"
	SurveyPDF   = "quality/survey.pdf"
	SurveyTruth = "quality/survey.truth.json"
	// The survey's pages as images: every page in a PDF with no text, the
	// first page as a PNG and as a JPEG, and the others as frames of a TIFF.
	SurveyScanPDF = "quality/survey-scan.pdf"
	SurveyPNG     = "quality/survey-1.png"
	SurveyJPEG    = "quality/survey-1.jpg"
	SurveyTIFF    = "quality/survey-2-4.tiff"

	PlainTXT      = "quality/plain.txt"
	PlainTruth    = "quality/plain.truth.json"
	NotesMD       = "quality/notes.md"
	NotesTruth    = "quality/notes.truth.json"
	ReadingsCSV   = "quality/readings.csv"
	ReadingsTruth = "quality/readings.truth.json"

	// The truths of ReportDOCX and LedgerXLSX, and ReportDOCX as a Word 97
	// file, which holds the same report.
	ReportTruth = "quality/report.truth.json"
	LedgerTruth = "quality/ledger.truth.json"
	ReportDOC   = "quality/report.doc"

	SlidesPPTX  = "quality/slides.pptx" // generated: 3 slides, with a list and a table
	SlidesTruth = "quality/slides.truth.json"
	MemoRTF     = "quality/memo.rtf" // written by hand: headings, a list and a table
	MemoTruth   = "quality/memo.truth.json"
)
