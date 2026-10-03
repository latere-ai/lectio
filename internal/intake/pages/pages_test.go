// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package pages

import (
	"bytes"
	"compress/zlib"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/tiffx"
	"latere.ai/x/lectio/internal/testfixtures"
)

func TestDefaultLimits(t *testing.T) {
	want := Limits{MaxBytes: 256 << 20, MaxPages: 3000}
	if got := DefaultLimits(); got != want {
		t.Errorf("DefaultLimits() = %+v, want %+v", got, want)
	}
}

func TestSelect(t *testing.T) {
	tests := []struct {
		name  string
		expr  string
		total int
		want  []int
		err   bool
	}{
		{"empty all", "", 3, []int{1, 2, 3}, false},
		{"blank all", "  ", 2, []int{1, 2}, false},
		{"pages and a range", "1-3,7", 10, []int{1, 2, 3, 7}, false},
		{"normalize and dedup", "3,1-2,2", 10, []int{1, 2, 3}, false},
		{"spaces around parts", " 2 , 4 - 5 ", 10, []int{2, 4, 5}, false},
		{"range clamped to the document", "1-100", 12, intsTo(12), false},
		{"page past the end dropped", "2,99", 10, []int{2}, false},
		{"single page", "5", 10, []int{5}, false},
		{"last page", "10", 10, []int{10}, false},
		{"nothing within the document", "50-60", 12, nil, true},
		{"page past the end alone", "13", 12, nil, true},
		{"open range", "5-", 8, []int{5, 6, 7, 8}, false},
		{"page and open range", "1,7-", 8, []int{1, 7, 8}, false},
		{"open range from the last page", "8-", 8, []int{8}, false},
		{"open range with spaces", " 6 - ", 8, []int{6, 7, 8}, false},
		{"open range past the end beside a page", "2,9-", 8, []int{2}, false},
		{"open range past the end alone", "9-", 8, nil, true},
		{"malformed reversed", "5-1", 10, nil, true},
		{"malformed text", "abc", 10, nil, true},
		{"malformed range bound", "3-x", 10, nil, true},
		{"malformed lone dash", "-", 10, nil, true},
		{"malformed range without a start", "-3", 10, nil, true},
		{"malformed double dash", "3--", 10, nil, true},
		{"malformed empty part", "1,,3", 10, nil, true},
		{"malformed trailing comma", "1,", 10, nil, true},
		{"zero page", "0", 10, nil, true},
		{"zero range bound", "0-3", 10, nil, true},
		{"zero open range", "0-", 10, nil, true},
		{"document without pages", "1", 0, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Select(tt.expr, tt.total)
			if tt.err {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				if code := fault.CodeOf(err); code != fault.InvalidPages {
					t.Errorf("code = %q, want %q", code, fault.InvalidPages)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSelectHugeUpperBoundReturnsPromptly guards against a range whose upper
// bound is enormous. Without the clamp to the document the loop runs for
// billions of iterations, or at the largest int overflows its counter and
// never ends. With it the answer is the same as for a range that fits.
func TestSelectHugeUpperBoundReturnsPromptly(t *testing.T) {
	type result struct {
		pages []int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		got, err := Select("1-9223372036854775807", 3)
		done <- result{got, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("unexpected error: %v", r.err)
		}
		if !slices.Equal(r.pages, []int{1, 2, 3}) {
			t.Errorf("got %v, want [1 2 3]", r.pages)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Select did not return within 2s: unbounded loop")
	}
}

func intsTo(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

func TestEnforceMax(t *testing.T) {
	lim := Limits{MaxPages: 100}
	if err := EnforceMax(100, lim); err != nil {
		t.Errorf("a document at the limit should pass: %v", err)
	}
	err := EnforceMax(101, lim)
	if err == nil {
		t.Fatal("a document over the limit should fail")
	}
	if code := fault.CodeOf(err); code != fault.TooManyPages {
		t.Errorf("code = %q, want %q", code, fault.TooManyPages)
	}
	if got, want := fault.DetailOf(err), "101 pages, limit 100"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	// A zero limit means none.
	if err := EnforceMax(1_000_000, Limits{}); err != nil {
		t.Errorf("a zero limit should not bound the count: %v", err)
	}
}

func TestCountPDFFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		want    int
	}{
		{testfixtures.MinimalPDF, 1},
		{testfixtures.MultipagePDF, 3},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			n, err := CountPDF(testfixtures.Read(t, c.fixture))
			if err != nil {
				t.Fatalf("CountPDF: %v", err)
			}
			if n != c.want {
				t.Errorf("pages = %d, want %d", n, c.want)
			}
		})
	}
}

func TestCountTIFF(t *testing.T) {
	n, err := CountTIFF(testfixtures.Read(t, testfixtures.MultiTIFF))
	if err != nil {
		t.Fatalf("CountTIFF: %v", err)
	}
	if n != 3 {
		t.Errorf("pages = %d, want 3", n)
	}
}

func TestCountTIFFCorrupt(t *testing.T) {
	for _, in := range [][]byte{
		{},
		[]byte("XX\x00\x00"),         // bad byte order
		{'I', 'I', 0, 0, 0, 0, 0, 0}, // bad magic
	} {
		_, err := CountTIFF(in)
		if err == nil {
			t.Errorf("want an error for %q", in)
			continue
		}
		if code := fault.CodeOf(err); code != fault.DocumentCorrupt {
			t.Errorf("code for %q = %q, want %q", in, code, fault.DocumentCorrupt)
		}
		if !errors.Is(err, tiffx.ErrCorrupt) {
			t.Errorf("err for %q = %v, want tiffx.ErrCorrupt underneath", in, err)
		}
	}
}

// objStmPDF wraps a page tree of seven pages in a Flate-compressed object
// stream, the layout in which the tree is invisible to a scan of the file
// body. head is the stream's dictionary and its stream keyword, and rest is
// what the file holds between the object stream and its end.
//
// The payload is the page tree and the leaf pages it counts, as an object
// stream carries the whole object set. That redundancy is also what keeps the
// tree out of the clear: Flate emits a stored block when coding a short input
// with little redundancy would grow it.
func objStmPDF(t *testing.T, head, rest string) []byte {
	t.Helper()
	inner := "<< /Type /Pages /Count 7 /Kids [2 0 R] >>\n" +
		strings.Repeat("<< /Type /Page /Parent 1 0 R /MediaBox [0 0 612 792] >>\n", 7)
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	if _, err := w.Write([]byte(inner)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n1 0 obj\n" + head)
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n" + rest + "%%EOF\n")
	// If the tree ever appears in the clear, the count no longer depends on
	// inflation.
	if bytes.Contains(b.Bytes(), []byte("/Pages")) {
		t.Fatal("the page tree is readable outside the object stream")
	}
	return b.Bytes()
}

// TestCountPDFReadsCompressedPageTree covers the object stream path: the page
// tree lives only inside a Flate stream, so the count is reachable only after
// inflating it, wherever the stream sits in the file and whichever order its
// dictionary lists the keys in.
func TestCountPDFReadsCompressedPageTree(t *testing.T) {
	const (
		typeFirst   = "<< /Type /ObjStm /Filter /FlateDecode /N 8 /First 0 >>\nstream\n"
		filterFirst = "<</Filter/FlateDecode/First 0/N 8/Type/ObjStm>>stream\r\n"
		nestedFirst = "<< /DecodeParms << /Columns 1 >> /Filter /FlateDecode /N 8 /First 0 /Type /ObjStm >>\nstream\n"
	)
	// A dictionary whose filter lies further from /ObjStm than the window the
	// dictionary is read in.
	longDict := "<< /Type /ObjStm /Pad (" + strings.Repeat("x", 2*pdfDictScanWindow) + ") /Filter /FlateDecode >>\nstream\n"
	// An uncompressed content stream longer than the span the stream keyword
	// is searched in, so the object stream does not reach the end of the
	// file within it.
	later := "2 0 obj\n<< /Length 5000 >>\nstream\n" + strings.Repeat("q Q ", 1250) + "\nendstream\nendobj\n"
	if len(later) <= pdfStreamKeywordSpan {
		t.Fatalf("the later object is %d bytes, want more than %d", len(later), pdfStreamKeywordSpan)
	}
	for _, tc := range []struct{ name, head, rest string }{
		{"stream at the end of the file", typeFirst, ""},
		{"stream far from the end of the file", typeFirst, later},
		{"filter before type", filterFirst, ""},
		{"filter before type, far from the end of the file", filterFirst, later},
		{"nested dictionary before type", nestedFirst, later},
		{"filter far after type", longDict, later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := CountPDF(objStmPDF(t, tc.head, tc.rest))
			if err != nil {
				t.Fatalf("CountPDF: %v", err)
			}
			if n != 7 {
				t.Errorf("pages = %d, want 7", n)
			}
		})
	}
}

// TestCountPDFInflatesEveryObjectStream covers a file with two object
// streams, each more than the search span from the end: the page tree is in
// the first and the count has to come from it.
func TestCountPDFInflatesEveryObjectStream(t *testing.T) {
	var other bytes.Buffer
	w := zlib.NewWriter(&other)
	if _, err := w.Write([]byte(strings.Repeat("<< /Type /Font /Subtype /Type1 >>\n", 8))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rest := "2 0 obj\n<< /Type /ObjStm /Filter /FlateDecode /N 8 /First 0 >>\nstream\n" + other.String() +
		"\nendstream\nendobj\n3 0 obj\n(" + strings.Repeat("x", 2*pdfStreamKeywordSpan) + ")\nendobj\n"
	b := objStmPDF(t, "<< /Type /ObjStm /Filter /FlateDecode /N 8 /First 0 >>\nstream\n", rest)
	n, err := CountPDF(b)
	if err != nil {
		t.Fatalf("CountPDF: %v", err)
	}
	if n != 7 {
		t.Errorf("pages = %d, want 7", n)
	}
}

// TestCountPDFRootCountWins covers the tree: intermediate nodes carry partial
// counts and unrelated dictionaries carry a /Count of their own, so the
// document total is the largest count on a page tree node.
func TestCountPDFRootCountWins(t *testing.T) {
	b := []byte("%PDF-1.4\n" +
		"1 0 obj\n<< /Type /Outlines /Count 99 >>\nendobj\n" +
		"2 0 obj\n<< /Type /Pages /Count 5 /Kids [3 0 R 4 0 R] >>\nendobj\n" +
		"3 0 obj\n<< /Type /Pages /Count 2 >>\nendobj\n" +
		"4 0 obj\n<< /Type /Pages /Count 3 >>\nendobj\n%%EOF\n")
	n, err := CountPDF(b)
	if err != nil {
		t.Fatalf("CountPDF: %v", err)
	}
	if n != 5 {
		t.Errorf("pages = %d, want 5 (the root count, not the outline's 99)", n)
	}
}

// TestCountPDFReadsTheMarkersOwnDictionary covers nesting around the marker:
// a dictionary nested before it and one nested after it both belong to the
// page tree node, and the /Count that follows them is still the node's.
func TestCountPDFReadsTheMarkersOwnDictionary(t *testing.T) {
	b := []byte("%PDF-1.4\n" +
		"1 0 obj\n<< /Resources << /Font << >> >> /Type /Pages /MediaBox [0 0 1 1] " +
		"/Rotate << /A 1 >> /Count 6 >>\nendobj\n" +
		"2 0 obj\n<< /Type /Outlines /Count 99 >>\nendobj\n%%EOF\n")
	n, err := CountPDF(b)
	if err != nil {
		t.Fatalf("CountPDF: %v", err)
	}
	if n != 6 {
		t.Errorf("pages = %d, want 6", n)
	}
}

// TestCountPDFFallsBackToPageNodes covers a page tree with no usable /Count:
// the page nodes themselves are counted, and /Pages must not be mistaken for
// a page.
func TestCountPDFFallsBackToPageNodes(t *testing.T) {
	b := []byte("%PDF-1.4\n" +
		"1 0 obj\n<< /Type /Pages /Kids [2 0 R 3 0 R] >>\nendobj\n" +
		"2 0 obj\n<< /Type /Page /Parent 1 0 R >>\nendobj\n" +
		"3 0 obj\n<< /Type /Page /Parent 1 0 R >>\nendobj\n%%EOF\n")
	n, err := CountPDF(b)
	if err != nil {
		t.Fatalf("CountPDF: %v", err)
	}
	if n != 2 {
		t.Errorf("pages = %d, want 2", n)
	}
}

// TestCountPDFRejectsNonPDF covers the header guard and a PDF that carries no
// page tree at all.
func TestCountPDFRejectsNonPDF(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"text", []byte("not a pdf")},
		{"wrong magic", []byte("GIF89a and then some")},
		{"header only", []byte("%PDF-1.7\n%%EOF\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := CountPDF(tc.in)
			if err == nil {
				t.Fatalf("CountPDF = %d, want an error", n)
			}
			if code := fault.CodeOf(err); code != fault.DocumentCorrupt {
				t.Errorf("code = %q, want %q", code, fault.DocumentCorrupt)
			}
		})
	}
}

// TestCountPDFSurvivesDeepNesting pins the reason the count is a byte scan. A
// parser that follows the object graph recurses once per level of nesting,
// and nesting this deep exhausts the goroutine stack, which ends the process
// with no panic to recover. The scan has no recursion to run away, so it
// must return an answer promptly.
func TestCountPDFSurvivesDeepNesting(t *testing.T) {
	const depth = 500_000
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n1 0 obj\n")
	b.WriteString(strings.Repeat("[", depth))
	b.WriteString(strings.Repeat("]", depth))
	b.WriteString("\nendobj\n2 0 obj\n<< /Type /Pages /Count 4 >>\nendobj\n%%EOF\n")

	done := make(chan struct{})
	var n int
	var err error
	go func() {
		defer close(done)
		n, err = CountPDF(b.Bytes())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("CountPDF did not return on a deeply nested document")
	}
	if err != nil {
		t.Fatalf("CountPDF: %v", err)
	}
	if n != 4 {
		t.Errorf("pages = %d, want 4", n)
	}
}

// TestCountPDFIgnoresUndecodableStreams covers the object streams that are
// skipped: one that is not Flate-encoded, one with no endstream, one that
// does not inflate, and one that inflates to nothing. Each is passed over
// and the count comes from the rest of the file.
func TestCountPDFIgnoresUndecodableStreams(t *testing.T) {
	var empty bytes.Buffer
	if err := zlib.NewWriter(&empty).Close(); err != nil {
		t.Fatal(err)
	}
	tail := "\n3 0 obj\n<< /Type /Pages /Count 2 >>\nendobj\n%%EOF\n"
	for _, tc := range []struct{ name, body string }{
		{"not flate", "%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /LZWDecode >>\nstream\nxx\nendstream\nendobj"},
		{"no stream keyword", "%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nendobj"},
		{"truncated", "%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nstream\nnot-zlib-bytes"},
		{"garbage payload", "%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nstream\n\x78\x9c!!!!\nendstream\nendobj"},
		{"not zlib", "%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nstream\nplain\nendstream\nendobj"},
		{"inflates to nothing", "%PDF-1.5\n1 0 obj\n<< /Type /ObjStm /Filter /FlateDecode >>\nstream\n" + empty.String() + "\nendstream\nendobj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := CountPDF([]byte(tc.body + tail))
			if err != nil {
				t.Fatalf("CountPDF: %v", err)
			}
			if n != 2 {
				t.Errorf("pages = %d, want 2", n)
			}
		})
	}
}
