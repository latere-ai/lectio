// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package native reads formats that carry their own structure, with no
// model: the page's blocks come from the file itself. A native page's
// blocks have no box, because the format says what a block is and not where
// it sits on a page.
package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"strconv"
	"strings"
	"unicode/utf8"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

// The media types this package reads.
const (
	TypeText     = "text/plain"
	TypeMarkdown = "text/markdown"
	TypeCSV      = "text/csv"
	TypeDOCX     = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	TypeXLSX     = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	TypeXLSM     = "application/vnd.ms-excel.sheet.macroenabled.12"
)

// Reads reports whether this package reads a media type.
func Reads(mediaType string) bool {
	switch mediaType {
	case TypeText, TypeMarkdown, TypeCSV, TypeDOCX, TypeXLSX, TypeXLSM:
		return true
	}
	return false
}

// Pages reads a file into pages. Text, Markdown and a word-processing
// document are one page of blocks; a delimited table is one page holding
// one table; a workbook is one page per sheet. maxPages is the most pages a
// document may have, zero for no limit: a workbook that lists more sheets
// is refused before any sheet is read.
func Pages(ctx context.Context, data []byte, mediaType string, maxPages int) ([]document.Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch mediaType {
	case TypeDOCX:
		return readDOCX(ctx, data, limits)
	case TypeXLSX, TypeXLSM:
		// A workbook with macros is read as one without: its cells are
		// the same parts, and the macros are a part nothing here opens.
		return readXLSX(ctx, data, maxPages, limits)
	}
	if !utf8.Valid(data) {
		return nil, fault.New(fault.DocumentCorrupt, "the file is not UTF-8 text")
	}
	var blocks []document.Block
	switch mediaType {
	case TypeText:
		blocks = paragraphs(string(data))
	case TypeMarkdown:
		blocks = markdown(string(data))
	case TypeCSV:
		table, err := delimited(data)
		if err != nil {
			return nil, err
		}
		blocks = table
	default:
		return nil, fault.New(fault.UnsupportedMediaType, "%s is not read natively", mediaType)
	}
	return []document.Page{{
		Number: 1, State: document.PageSucceeded, Source: document.SourceNative,
		Blocks: document.Number(1, blocks),
		Usage:  &document.Usage{Pages: 1},
	}}, nil
}

// paragraphs splits plain text at blank lines. Each run of lines is a block
// of text.
func paragraphs(s string) []document.Block {
	var out []document.Block
	for part := range strings.SplitSeq(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if text := strings.TrimSpace(part); text != "" {
			out = append(out, document.Block{Kind: document.KindText, Text: text})
		}
	}
	return out
}

// markdown reads the block structure Markdown states outright: headings by
// their marks, list items, fenced code, and paragraphs. It is not a full
// Markdown parser; what it does not recognize is text, which loses no
// content.
func markdown(s string) []document.Block {
	var out []document.Block
	var para, code []string
	inCode := false

	flush := func() {
		if text := strings.TrimSpace(strings.Join(para, "\n")); text != "" {
			out = append(out, document.Block{Kind: document.KindText, Text: text})
		}
		para = nil
	}

	lines := bufio.NewScanner(strings.NewReader(s))
	lines.Buffer(make([]byte, 0, 64<<10), len(s)+1)
	for lines.Scan() {
		line := strings.TrimRight(lines.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			if inCode {
				out = append(out, document.Block{Kind: document.KindCode, Text: strings.Join(code, "\n")})
				code = nil
			} else {
				flush()
			}
			inCode = !inCode
		case inCode:
			code = append(code, line)
		case trimmed == "":
			flush()
		case headingLevel(trimmed) > 0:
			flush()
			level := headingLevel(trimmed)
			kind := document.KindHeading
			if level == 1 {
				kind = document.KindTitle
			}
			out = append(out, document.Block{Kind: kind, Level: level, Text: strings.TrimSpace(strings.TrimRight(trimmed[level:], "#"))})
		case listItem(trimmed) != "":
			flush()
			out = append(out, document.Block{Kind: document.KindListItem, Text: listItem(trimmed)})
		default:
			para = append(para, trimmed)
		}
	}
	if inCode {
		// A fence that never closes still holds its lines.
		out = append(out, document.Block{Kind: document.KindCode, Text: strings.Join(code, "\n")})
	}
	flush()
	return out
}

// headingLevel returns 1 to 6 for a line that opens with that many number
// signs and a space, and 0 otherwise.
func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n == len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

// listItem returns the text of a list item, or the empty string for a line
// that is not one. A list item opens with a dash, an asterisk or a plus and
// a space, or with a number, a period and a space.
func listItem(line string) string {
	for _, mark := range []string{"- ", "* ", "+ "} {
		if rest, ok := strings.CutPrefix(line, mark); ok {
			return strings.TrimSpace(rest)
		}
	}
	number, rest, ok := strings.Cut(line, ". ")
	if !ok {
		return ""
	}
	if _, err := strconv.Atoi(number); err != nil {
		return ""
	}
	return strings.TrimSpace(rest)
}

// delimited reads a delimited table into one table block.
func delimited(data []byte) ([]document.Block, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, fault.Wrap(fault.DocumentCorrupt, err, "the table could not be read")
	}
	if len(records) == 0 {
		return nil, nil
	}
	var cells []document.Cell
	cols := 0
	for row, record := range records {
		cols = max(cols, len(record))
		for col, value := range record {
			cells = append(cells, document.Cell{Row: row, Col: col, Text: value})
		}
	}
	return []document.Block{tableOf(len(records), cols, cells)}, nil
}
