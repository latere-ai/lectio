// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package pages answers three questions about a document before any page is
// read: how many pages it has, which of them a selection such as "1-3,7"
// names, and whether it is within the limits on one document. It counts the
// pages of a PDF and the frames of a TIFF from the file's own structure,
// without rendering anything.
package pages

import (
	"bytes"
	"compress/zlib"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/tiffx"
)

// Limits are the hard bounds on one document. EnforceMax applies MaxPages.
// MaxBytes is applied where the file's bytes are received, since nothing in
// this package sees them arrive.
type Limits struct {
	MaxBytes int64 // largest source file, in bytes
	MaxPages int   // most pages a document may have, counted before selection
}

// DefaultLimits are the limits that apply when none are configured: 256 MiB
// and 3,000 pages.
func DefaultLimits() Limits {
	return Limits{MaxBytes: 256 << 20, MaxPages: 3000}
}

// Select parses a page selection against a document of total pages and
// returns the 1-based pages it names, sorted and without duplicates. The
// selection is ranges and numbers separated by commas, such as "1-3,7". An
// open range, such as "5-", runs to the last page. An empty selection means
// every page.
//
// The selection is clamped to the document: a page past the end is dropped, a
// range that runs past the end stops at the last page, and a range that
// starts past the end names nothing. A selection that is malformed, or that
// names no page of the document, fails with fault.InvalidPages. A range needs
// its start, so "-3" is malformed.
func Select(expr string, total int) ([]int, error) {
	if total <= 0 {
		return nil, fault.New(fault.InvalidPages, "the document has no pages to select from")
	}
	if strings.TrimSpace(expr) == "" {
		all := make([]int, total)
		for i := range all {
			all[i] = i + 1
		}
		return all, nil
	}

	set := map[int]bool{}
	for part := range strings.SplitSeq(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, malformed(expr)
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil || a < 1 {
				return nil, malformed(expr)
			}
			// An open range has no upper bound and runs to the last page.
			b := total
			if hi = strings.TrimSpace(hi); hi != "" {
				b, err = strconv.Atoi(hi)
				if err != nil || b < a {
					return nil, malformed(expr)
				}
			}
			// The upper bound stops at total. Pages past the end are dropped
			// either way, and without the clamp a range such as
			// "1-9223372036854775807" would run the loop for as long as the
			// number is large, or overflow the counter and never end.
			last := min(b, total)
			for p := a; p <= last; p++ {
				set[p] = true
			}
		} else {
			p, err := strconv.Atoi(part)
			if err != nil || p < 1 {
				return nil, malformed(expr)
			}
			if p <= total {
				set[p] = true
			}
		}
	}
	if len(set) == 0 {
		return nil, fault.New(fault.InvalidPages,
			"pages %q selects none of the document's %d pages", expr, total)
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// Check reports whether a selection is well formed, with no document to
// apply it to: what a submit can know before the file was counted. A
// selection that passes may still name no page of the document, which
// Select reports.
func Check(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	for part := range strings.SplitSeq(expr, ",") {
		lo, hi, ranged := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || a < 1 {
			return malformed(expr)
		}
		if hi = strings.TrimSpace(hi); ranged && hi != "" {
			if b, err := strconv.Atoi(hi); err != nil || b < a {
				return malformed(expr)
			}
		}
	}
	return nil
}

func malformed(expr string) error {
	return fault.New(fault.InvalidPages, "pages %q is malformed", expr)
}

// EnforceMax checks a document's page count against lim.MaxPages and fails
// with fault.TooManyPages when the count is over it. A MaxPages of zero means
// no limit. The count is the document's total and not the size of a
// selection, so selecting one page does not get a document past the limit.
func EnforceMax(total int, lim Limits) error {
	if lim.MaxPages > 0 && total > lim.MaxPages {
		return fault.New(fault.TooManyPages, "%d pages, limit %d", total, lim.MaxPages)
	}
	return nil
}

// The bounds on the work CountPDF does. The scan is linear in the bytes it is
// allowed to see, so a cap on inflated output caps the whole operation.
const (
	maxPDFObjectStreams  = 4096
	maxPDFInflatedBytes  = 64 << 20
	pdfDictScanWindow    = 512
	pdfStreamKeywordSpan = 4096
)

var (
	pdfPagesNode = regexp.MustCompile(`/Type\s*/Pages`)
	// A page node is /Type /Page with no name character after it, which is
	// what separates it from the /Type /Pages tree node.
	pdfPageNode  = regexp.MustCompile(`/Type\s*/Page($|[^a-zA-Z])`)
	pdfCountKey  = regexp.MustCompile(`/Count\s+(\d+)`)
	pdfObjStmKey = regexp.MustCompile(`/ObjStm`)
)

// CountPDF returns the number of pages in a PDF, read from its page tree.
//
// The count comes from the document's own structure: the root /Type /Pages
// node carries /Count, and each page is a /Type /Page node. From PDF 1.5 on
// those nodes usually sit in Flate-compressed object streams, so those are
// inflated before the scan.
//
// The count is what the file's bytes say and no more. A file can be
// written so that they say more or fewer pages than a viewer shows: an
// object nothing refers to, a count placed where the scan does not look. A
// caller that renders the pages takes the count from the engine that
// renders them, and uses this one only where it has no engine.
//
// The counts are read off a bounded scan of the bytes; the object graph is
// not parsed. Following indirect references means walking that graph
// recursively, and a document can nest arrays and dictionaries deeply enough
// to exhaust the goroutine stack. Stack exhaustion ends the process and no
// recover catches it, so one crafted file would take a worker down. A linear
// scan over a capped number of bytes cannot fail that way.
//
// A PDF whose page tree cannot be read fails with fault.DocumentCorrupt.
func CountPDF(b []byte) (int, error) {
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		return 0, fault.New(fault.DocumentCorrupt, "the file does not start with a PDF header")
	}

	// Object streams hold the page tree in compressed form: scan the file
	// body and everything the object streams expand to.
	regions := append([][]byte{b}, inflatePDFObjectStreams(b)...)

	// The root node's /Count is the document total. Intermediate nodes carry
	// partial counts, so the largest wins. /Count also appears on unrelated
	// dictionaries, outlines for one, so only the dictionary around each
	// /Type /Pages marker is read.
	best := 0
	for _, region := range regions {
		for _, loc := range pdfPagesNode.FindAllIndex(region, -1) {
			for _, m := range pdfCountKey.FindAllSubmatch(pdfEnclosingDict(region, loc[0]), -1) {
				if n, err := strconv.Atoi(string(m[1])); err == nil && n > best {
					best = n
				}
			}
		}
	}

	// No usable /Count: count the page nodes themselves.
	if best == 0 {
		for _, region := range regions {
			best += len(pdfPageNode.FindAllIndex(region, -1))
		}
	}

	if best < 1 {
		return 0, fault.New(fault.DocumentCorrupt, "the PDF page tree names no pages")
	}
	return best, nil
}

// pdfEnclosingDict returns the << ... >> dictionary around the byte at index
// at, so a key read near a marker belongs to the marker's object and not to
// the dictionary next to it in the file. Depth is a counter and not
// recursion, and the walk stops at pdfDictScanWindow bytes either way, so a
// document that nests dictionaries without bound costs a bounded scan and
// yields a truncated window.
func pdfEnclosingDict(region []byte, at int) []byte {
	lo := max(at-pdfDictScanWindow, 0)
	start := lo
	for i, depth := at, 0; i > lo; i-- {
		if region[i] == '>' && i > 0 && region[i-1] == '>' {
			depth++
			i--
			continue
		}
		if region[i] == '<' && i > 0 && region[i-1] == '<' {
			if depth == 0 {
				start = i - 1
				break
			}
			depth--
			i--
		}
	}

	hi := min(at+pdfDictScanWindow, len(region))
	end := hi
	for i, depth := at, 0; i < hi-1; i++ {
		if region[i] == '<' && region[i+1] == '<' {
			depth++
			i++
			continue
		}
		if region[i] == '>' && region[i+1] == '>' {
			if depth == 0 {
				end = i + 2
				break
			}
			depth--
			i++
		}
	}
	if start >= end {
		return nil
	}
	return region[start:end]
}

// inflatePDFObjectStreams returns the decompressed contents of the document's
// Flate-encoded object streams, which is where a PDF from version 1.5 on
// keeps its page tree. A stream that is not Flate-encoded, is truncated, or
// would take the run past its caps is skipped: a missing region can only
// lower the count, never crash.
func inflatePDFObjectStreams(b []byte) [][]byte {
	var out [][]byte
	budget := maxPDFInflatedBytes
	// read is where the last stream that was found ends. Streams lie one
	// after another in a file, so a marker before it sits inside a stream
	// body that was already searched: it is that body's bytes and not a
	// header. Skipping it keeps the searches for the end of a stream from
	// overlapping, so together they read the file at most once however the
	// file was written.
	read := 0

	for _, loc := range pdfObjStmKey.FindAllIndex(b, -1) {
		if len(out) >= maxPDFObjectStreams || budget <= 0 {
			break
		}
		if loc[0] < read {
			continue
		}
		// The stream body starts at the stream keyword after the dictionary
		// that named /ObjStm.
		tail := b[loc[1]:min(loc[1]+pdfStreamKeywordSpan, len(b))]
		before, after, ok := bytes.Cut(tail, []byte("stream"))
		if !ok {
			continue
		}
		// The filter is a key of that dictionary, and a writer may put it
		// before the /ObjStm name or after it. The dictionary covers both
		// orders; the bytes up to the stream keyword cover a dictionary
		// longer than the window pdfEnclosingDict reads.
		flate := []byte("FlateDecode")
		if !bytes.Contains(pdfEnclosingDict(b, loc[0]), flate) && !bytes.Contains(before, flate) {
			continue
		}
		body := bytes.TrimLeft(after, "\r\n")
		// body is the end of tail, and tail is a window that can stop short
		// of the end of the file, so the body's offset in b is counted from
		// where tail ends. The dictionary's /Length is an indirect reference
		// often enough that it cannot be relied on: the body ends at the
		// endstream keyword.
		abs := loc[1] + len(tail) - len(body)
		endIdx := bytes.Index(b[abs:], []byte("endstream"))
		if endIdx < 0 {
			// No stream from here to the end of the file is closed, so no
			// later header's is either.
			break
		}
		read = abs + endIdx

		zr, err := zlib.NewReader(bytes.NewReader(b[abs : abs+endIdx]))
		if err != nil {
			continue
		}
		// A stream that fails partway still yields the prefix it inflated,
		// and that prefix is worth scanning, so neither error ends the count.
		data, _ := io.ReadAll(io.LimitReader(zr, int64(budget)))
		_ = zr.Close()
		if len(data) == 0 {
			continue
		}
		budget -= len(data)
		out = append(out, data)
	}
	return out
}

// CountTIFF returns the number of pages in a TIFF, one per frame. A
// single-frame TIFF has one page. Input that is not a readable TIFF fails
// with fault.DocumentCorrupt.
func CountTIFF(b []byte) (int, error) {
	return tiffx.CountFrames(b)
}
