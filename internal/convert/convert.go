// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package convert is conversion from both ends: the client a pipeline
// converts through, and the sidecar that holds the office suite and does
// the converting. They meet in one HTTP call, and nothing else of one is
// known to the other. The design is specs/009-intake.md.
//
// The call is a POST of the file to Path. Content-Type is the file's media
// type and Accept the media type wanted. The answer is 200 with the
// conversion as its body and the wanted type as its Content-Type, or a
// status that says why not, with a body of {"error": {"code", "detail"}}:
//
//	415 unsupported_media_type  the converter does not convert that pair
//	413 file_too_large          the file, or its conversion, is over the limit
//	422 document_corrupt        the suite could not convert the file within its time and its memory
//	500 internal                the converter itself failed
//
// The sidecar converts one file at a time, so a call may wait for the one
// before it. It needs no credential and takes none.
//
// The package imports the standard library and nothing that reaches a
// network of its own, so the sidecar built from it holds only what it
// needs. A pipeline that wants its calls traced passes the client a
// transport that does so.
package convert

import (
	"mime"
	"strings"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
)

// Path is the one route of the sidecar.
const Path = "/v1/convert"

// conversion is how the suite is asked for one conversion. The filter that
// reads the file is named outright: left to choose, the suite goes by what
// the bytes look like to it, and a file sent as one format would be opened
// as whatever else it was written to pass for.
type conversion struct {
	ext    string // the extension the file is given in the scratch directory
	reads  string // the suite's filter that reads the file
	out    string // the extension of the conversion
	writes string // the suite's filter that writes it
}

// conversions are the conversions the sidecar does, by the media type of
// the file and the one wanted: a legacy word-processing file into the
// current format, and presentations and rich text into PDF.
var conversions = map[[2]string]conversion{
	{detect.MIMEDOC, detect.MIMEDOCX}:    {"doc", "MS Word 97", "docx", "MS Word 2007 XML"},
	{detect.MIMEPPTX, detect.MIMEPDF}:    {"pptx", "Impress MS PowerPoint 2007 XML", "pdf", "impress_pdf_Export"},
	{detect.MIMEPPT, detect.MIMEPDF}:     {"ppt", "MS PowerPoint 97", "pdf", "impress_pdf_Export"},
	{detect.MIMEODP, detect.MIMEPDF}:     {"odp", "impress8", "pdf", "impress_pdf_Export"},
	{detect.MIMEKeynote, detect.MIMEPDF}: {"key", "Apple Keynote", "pdf", "impress_pdf_Export"},
	{detect.MIMERTF, detect.MIMEPDF}:     {"rtf", "Rich Text Format", "pdf", "writer_pdf_Export"},
	{detect.MIMEODT, detect.MIMEPDF}:     {"odt", "writer8", "pdf", "writer_pdf_Export"},
}

// Converts reports whether the sidecar converts one media type into another.
func Converts(from, to string) bool {
	_, ok := conversions[[2]string{mediaType(from), mediaType(to)}]
	return ok
}

// mediaType is a header's media type without its parameters, in lower
// case, and the empty string for a header that is not one media type.
func mediaType(header string) string {
	t, _, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return strings.ToLower(t)
}

// problem is the body of an answer that is not a conversion.
type problem struct {
	Error struct {
		Code   fault.Code `json:"code"`
		Detail string     `json:"detail"`
	} `json:"error"`
}
