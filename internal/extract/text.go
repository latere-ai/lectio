// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package extract is what runs an extraction, less the calls and the
// storage: the document as the text a model is given, the check of a
// caller's schema and the validator of a reply, the cut of a long document
// into windows and the merge of their replies, and the steps an extraction
// takes from one call to the next. It holds no state between 2 calls and
// reaches nothing, so whoever schedules an extraction, one call at a time
// across worker processes, runs it through these functions and keeps what
// they return. The design is specs/011-structured-extraction.md.
package extract

import (
	"regexp"
	"strings"

	"latere.ai/x/lectio/document"
)

// Piece is one block of a document as an extraction reads it.
type Piece struct {
	// Ref is the block's address, which is what a citation names.
	Ref string
	// Text is the block's content: its text, or for a table its markup.
	Text string
	// Section says the block is a title or a heading, where a section of
	// the document begins.
	Section bool
}

// fence matches the tags the extraction prompt fences the document and the
// schema with, in any letter case.
var fence = regexp.MustCompile(`(?i)<(/?(?:document|schema))>`)

// Pieces is the document as an extraction reads it: the blocks of the pages
// that were read, in reading order. Page furniture, a running header, a
// running footer and a page number, is left out wherever it stands: it is
// not part of what the document says, and a model that reads a running
// header answers a question about the title with it. What is not read is
// in no window, so no citation can name it. A table is its markup, which keeps merged
// cells. A figure is the words printed in it and never its description,
// which is a reader's own prose and not something the document says. A
// block with nothing printed in it is left out.
//
// A block that prints one of the tags the prompt fences the document with
// has the tag's opening bracket written as an entity, so no text of a file
// closes the fence it is inside.
func Pieces(pages []document.Page) []Piece {
	var out []Piece
	for _, page := range pages {
		if page.State != document.PageSucceeded {
			continue
		}
		for _, b := range page.Blocks {
			if furniture(b.Kind) {
				continue
			}
			text := b.Text
			if b.Kind == document.KindTable && b.Table != nil && b.Table.HTML != "" {
				text = b.Table.HTML
			}
			if text = strings.TrimSpace(text); text == "" {
				continue
			}
			out = append(out, Piece{
				Ref: b.Ref, Text: fence.ReplaceAllString(text, "&lt;$1>"),
				Section: b.Kind == document.KindTitle || b.Kind == document.KindHeading,
			})
		}
	}
	return out
}

// line is a piece as the model reads it: its ref in brackets, then its
// content. The ref marks where the block begins, and the block may run over
// several lines.
func line(ref, text string) string { return "[" + ref + "] " + text }

// furniture reports whether a block of this kind is page furniture, which
// an extraction neither reads nor lets a citation point at.
func furniture(k document.Kind) bool {
	return k == document.KindPageHeader || k == document.KindPageFooter || k == document.KindPageNumber
}
