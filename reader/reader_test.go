// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
)

// A key must be reachable through Reveal and nowhere else: not through
// printing, not through an error that wraps it, not through a log line,
// and not through a stored result.
func TestCredentialNeverLeaves(t *testing.T) {
	const key = "sk-live-do-not-print"
	c := NewCredential(key)

	if c.Reveal() != key {
		t.Fatal("Reveal must return the key")
	}
	if c.IsZero() || !(Credential{}).IsZero() {
		t.Fatal("IsZero must tell a set key from an unset one")
	}

	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("calling", "credential", c, "page", Page{Number: 1, Credential: c})
	stored, err := json.Marshal(struct {
		C Credential
		P *Credential
	}{c, &c})
	if err != nil {
		t.Fatal(err)
	}

	for name, out := range map[string]string{
		"%v":      fmt.Sprintf("%v", c),
		"%s":      fmt.Sprintf("key %s", c),
		"%+v":     fmt.Sprintf("%+v", Page{Credential: c}),
		"%#v":     fmt.Sprintf("%#v", c),
		"error":   fmt.Errorf("call with %v failed: %w", c, io.EOF).Error(),
		"log":     log.String(),
		"json":    string(stored),
		"request": fmt.Sprint(ExtractRequest{Credential: c}),
	} {
		if strings.Contains(out, key) {
			t.Errorf("the key leaked through %s: %s", name, out)
		}
	}
}

func TestClassNames(t *testing.T) {
	want := map[Class]string{Retryable: "retryable", Invalid: "invalid", RateLimited: "rate_limited", Budget: "budget", Permanent: "permanent", Class(99): "class(99)"}
	for class, name := range want {
		if got := class.String(); got != name {
			t.Errorf("Class(%d).String() = %q, want %q", int(class), got, name)
		}
	}
}

func TestErrorCarriesItsClassThroughWrapping(t *testing.T) {
	inner := &Error{Class: RateLimited, RetryAfter: 7 * time.Second, Detail: "slow down", Err: io.ErrUnexpectedEOF}
	wrapped := fmt.Errorf("page 3: %w", inner)

	if got := ClassOf(wrapped); got != RateLimited {
		t.Fatalf("ClassOf = %v", got)
	}
	if got := RetryAfterOf(wrapped); got != 7*time.Second {
		t.Fatalf("RetryAfterOf = %v", got)
	}
	if !errors.Is(wrapped, io.ErrUnexpectedEOF) {
		t.Fatal("the cause must stay reachable")
	}
	if got, want := inner.Error(), "rate_limited: slow down: unexpected EOF"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got, want := Errorf(Permanent, "status %d", 400).Error(), "permanent: status 400"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	plain := errors.New("connection reset")
	if ClassOf(plain) != Retryable || RetryAfterOf(plain) != 0 {
		t.Fatal("an unclassified error is retryable and names no wait")
	}
	if e := FromTransport(plain); e.Class != Retryable || !errors.Is(e, plain) {
		t.Fatalf("FromTransport = %+v", e)
	}
}

func TestFromStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header string
		body   string
		class  Class
		wait   time.Duration
	}{
		{"rate limit with a wait", 429, "7", "", RateLimited, 7 * time.Second},
		{"rate limit without one", 429, "", "", RateLimited, 0},
		{"unavailable that says when", 503, "2.5", "", RateLimited, 2500 * time.Millisecond},
		{"unavailable that does not", 503, "", "", Retryable, 0},
		{"server error", 500, "", "", Retryable, 0},
		{"gateway timeout", 504, "", "", Retryable, 0},
		{"request timeout", 408, "", "", Retryable, 0},
		{"conflict", 409, "", "", Retryable, 0},
		{"payment required", 402, "", "", Budget, 0},
		{"a budget refusal behind another status", 429, "", `{"error":{"code":"budget_exhausted"}}`, Budget, 0},
		{"a quota refusal", 403, "", `{"error":{"type":"insufficient_quota"}}`, Budget, 0},
		{"bad request", 400, "", "", Permanent, 0},
		{"unauthorized", 401, "", "", Permanent, 0},
		{"a date is not read", 429, "Wed, 21 Oct 2026 07:28:00 GMT", "", RateLimited, 0},
		{"a negative wait is none", 429, "-3", "", RateLimited, 0},
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Retry-After", tc.header)
		}
		e := FromStatus(tc.status, h, []byte(tc.body))
		if e.Class != tc.class || e.RetryAfter != tc.wait || e.Status != tc.status {
			t.Errorf("%s: FromStatus(%d) = %v after %v, want %v after %v", tc.name, tc.status, e.Class, e.RetryAfter, tc.class, tc.wait)
		}
		if strings.Contains(e.Error(), tc.body) && tc.body != "" {
			t.Errorf("%s: the body must not be kept in the error", tc.name)
		}
	}
}

func TestKindOf(t *testing.T) {
	for label, want := range map[string]document.Kind{
		"Title": document.KindTitle, "Section-header": document.KindHeading, "section header": document.KindHeading,
		"Text": document.KindText, "NarrativeText": document.KindText, "List-item": document.KindListItem,
		"Table": document.KindTable, "Picture": document.KindFigure, "Formula": document.KindFormula,
		"Caption": document.KindCaption, "Footnote": document.KindFootnote, "Page-header": document.KindPageHeader,
		"Page-footer": document.KindPageFooter, "page_number": document.KindPageNumber, "Code": document.KindCode,
		"key-value region": document.KindKeyValue, "QR code": document.KindBarcode,
		"LAYOUT_SECTION_HEADER": document.KindHeading, "LAYOUT_FIGURE": document.KindFigure,
		"FigureCaption": document.KindCaption, "CodeSnippet": document.KindCode, "equation": document.KindFormula,
	} {
		got, ok := KindOf(label)
		if got != want || !ok {
			t.Errorf("KindOf(%q) = %q, %v; want %q", label, got, ok, want)
		}
	}
	if got, ok := KindOf("watermark"); got != document.KindText || ok {
		t.Errorf("an unknown label is text and not ok, got %q, %v", got, ok)
	}
}

func TestNormalize(t *testing.T) {
	raws := []Raw{
		{Label: "Text", Text: " second ", Box: []float64{100, 300, 900, 400}, Order: 2},
		{Label: "Title", Text: "First", Box: []float64{100, 100, 900, 200}, Order: 1, Level: 1},
		{Label: "watermark", Text: "draft", Box: []float64{900, 100, 100, 200}},
		{Label: "Text", Text: "   "},
		{Label: "Picture", Text: ""},
		{Label: "Text", Text: "off the page", Box: []float64{1200, 100, 1300, 200}},
		{Label: "Text", Text: "no position", Box: []float64{1, 2}, Level: 3},
	}
	got := Normalize(raws, Grid{1000, 1000})

	type row struct {
		kind  document.Kind
		text  string
		box   string
		level int
		flags string
	}
	var rows []row
	for i, b := range got {
		if b.Order != i+1 || b.Ref != "" {
			t.Errorf("block %d: order %d, ref %q; want dense order and no ref", i, b.Order, b.Ref)
		}
		box := "none"
		if b.Box != nil {
			box = fmt.Sprint(*b.Box)
		}
		rows = append(rows, row{b.Kind, b.Text, box, b.Level, fmt.Sprint(b.Flags)})
	}
	want := []row{
		{document.KindTitle, "First", "[0.1 0.1 0.9 0.2]", 1, "[]"},
		{document.KindText, "second", "[0.1 0.3 0.9 0.4]", 0, "[]"},
		{document.KindText, "draft", "[0.1 0.1 0.9 0.2]", 0, "[kind_coerced box_clamped]"},
		{document.KindFigure, "", "none", 0, "[]"},
		{document.KindText, "off the page", "none", 0, "[box_clamped]"},
		{document.KindText, "no position", "none", 0, "[]"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("Normalize:\n got %v\nwant %v", rows, want)
	}

	if got := Normalize([]Raw{{Label: "Text", Text: "x", Box: []float64{1, 2, 3, 4}}}, Grid{}); got[0].Box != nil {
		t.Fatal("a box on a grid with no size is no box")
	}
}

func TestNormalizeReadsATable(t *testing.T) {
	markup := `<table><tr><th colspan="2">Revenue</th></tr><tr><td>Q1</td><td>10<br>million</td></tr></table>`
	for name, raw := range map[string]Raw{
		"markup in the text":   {Label: "Table", Text: markup},
		"markup apart from it": {Label: "Table", Text: "ignored", HTML: markup},
	} {
		got := Normalize([]Raw{raw}, Grid{1000, 1000})
		if len(got) != 1 || got[0].Table == nil {
			t.Fatalf("%s: no table: %+v", name, got)
		}
		tb := got[0].Table
		if tb.Rows != 2 || tb.Cols != 2 || len(tb.Cells) != 3 || tb.HTML != markup {
			t.Fatalf("%s: table = %+v", name, tb)
		}
		if tb.Cells[0].ColSpan != 2 || tb.Cells[2].Text != "10 million" || tb.Cells[2].Row != 1 || tb.Cells[2].Col != 1 {
			t.Fatalf("%s: cells = %+v", name, tb.Cells)
		}
		if want := "Revenue\nQ1 | 10 million"; got[0].Text != want {
			t.Fatalf("%s: text = %q, want %q", name, got[0].Text, want)
		}
	}

	plain := Normalize([]Raw{{Label: "Table", Text: "a | b"}}, Grid{1000, 1000})
	if plain[0].Table != nil || plain[0].Text != "a | b" {
		t.Fatalf("a table without markup keeps its text and has no structure: %+v", plain[0])
	}
}

func TestTableFromHTML(t *testing.T) {
	// A cell that spans rows pushes the next row's cells to the right.
	table, ok := TableFromHTML(`<table>
	  <tr><td rowspan="2">A</td><td>B</td></tr>
	  <tr><td>C</td></tr>
	  <tr><td colspan="0">D</td><td rowspan="x">E</td><td colspan="99999">F</td></tr>
	</table>`)
	if !ok {
		t.Fatal("no table")
	}
	at := map[string][2]int{}
	for _, c := range table.Cells {
		at[c.Text] = [2]int{c.Row, c.Col}
	}
	want := map[string][2]int{"A": {0, 0}, "B": {0, 1}, "C": {1, 1}, "D": {2, 0}, "E": {2, 1}, "F": {2, 2}}
	if !reflect.DeepEqual(at, want) {
		t.Fatalf("cells at %v, want %v", at, want)
	}
	if table.Rows != 3 || table.Cols != 1002 {
		t.Fatalf("size = %d by %d; a span is bounded at 1000", table.Rows, table.Cols)
	}

	if _, ok := TableFromHTML(""); ok {
		t.Fatal("no markup is no table")
	}
	if _, ok := TableFromHTML("<p>not a table</p>"); ok {
		t.Fatal("markup without a cell is no table")
	}
	bare, ok := TableFromHTML("<td>lonely</td>")
	if !ok || bare.Rows != 1 || bare.Cells[0].Text != "lonely" {
		t.Fatalf("a cell outside a row is in row 0: %+v", bare)
	}
}

func TestCheck(t *testing.T) {
	if err := Check(Result{}, true); err != nil {
		t.Fatalf("a blank page with no blocks is fine: %v", err)
	}
	if err := Check(Result{}, false); ClassOf(err) != Invalid {
		t.Fatalf("no blocks for a page with content is invalid, got %v", err)
	}

	good := Result{Blocks: []document.Block{{Text: strings.Repeat("a different line each time\nand another one here\n", 12)}}}
	if err := Check(good, false); err != nil {
		t.Fatalf("two alternating lines are not a loop: %v", err)
	}

	loop := Result{Blocks: []document.Block{{Text: "Heading of the page"}, {Text: strings.Repeat("the same sentence again\n", 40)}}}
	err := Check(loop, false)
	if ClassOf(err) != Invalid || !strings.Contains(err.Error(), "40 of the reply's 41 lines") {
		t.Fatalf("a reply that repeats one line is invalid, got %v", err)
	}

	short := Result{Blocks: []document.Block{{Text: strings.Repeat("the same sentence again\n", 5)}}}
	if err := Check(short, false); err != nil {
		t.Fatalf("a short reply is not judged: %v", err)
	}
}

// An engine that writes Markdown puts a heading's depth in its marks and
// wraps a formula in delimiters. Neither is part of the block's text.
func TestNormalizeReadsWhatAnEngineWritesAsMarkdown(t *testing.T) {
	for name, tc := range map[string]struct {
		raw   Raw
		text  string
		level int
	}{
		"a heading's marks give its level":      {Raw{Label: "Section-header", Text: "### 3.2.1 Scaled Dot-Product Attention"}, "3.2.1 Scaled Dot-Product Attention", 3},
		"a level the engine named is kept":      {Raw{Label: "Section-header", Text: "## Results", Level: 4}, "Results", 4},
		"a title with one mark":                 {Raw{Label: "Title", Text: "# Attention Is All You Need"}, "Attention Is All You Need", 1},
		"a heading with no mark":                {Raw{Label: "Section-header", Text: "4 Why Self-Attention"}, "4 Why Self-Attention", 0},
		"a number sign that is not a mark":      {Raw{Label: "Section-header", Text: "#hashtag"}, "#hashtag", 0},
		"seven signs are not a heading's marks": {Raw{Label: "Section-header", Text: "####### deep"}, "####### deep", 0},
		"text keeps its number signs":           {Raw{Label: "Text", Text: "## not a heading"}, "## not a heading", 0},
		"a formula in display delimiters":       {Raw{Label: "Formula", Text: "$$\nE = mc^2 \\quad (1)\n$$"}, "E = mc^2 \\quad (1)", 0},
		"a formula in bracket delimiters":       {Raw{Label: "Formula", Text: `\[ a + b \]`}, "a + b", 0},
		"a formula in inline delimiters":        {Raw{Label: "Formula", Text: "$x_i$", Level: 2}, "x_i", 0},
		"a formula with none":                   {Raw{Label: "Formula", Text: "a^2 + b^2"}, "a^2 + b^2", 0},
		"a lone delimiter is the formula":       {Raw{Label: "Formula", Text: "$"}, "$", 0},
	} {
		got := Normalize([]Raw{tc.raw}, Grid{1000, 1000})
		if len(got) != 1 || got[0].Text != tc.text || got[0].Level != tc.level {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestATableCellKeepsWhatIsRaisedOrLowered(t *testing.T) {
	table, ok := TableFromHTML(`<table><tr><td>O(n<sup>2</sup> · d)</td><td>O(log<sub>k</sub>(n))</td></tr></table>`)
	if !ok || len(table.Cells) != 2 || table.Cells[0].Text != "O(n^2 · d)" || table.Cells[1].Text != "O(log_k(n))" {
		t.Fatalf("cells: %+v", table)
	}
	// Outside a cell the marks are not text.
	if table, ok := TableFromHTML(`<table><sup>x</sup><tr><td>a</td></tr></table>`); !ok || table.Cells[0].Text != "a" {
		t.Fatalf("a mark outside a cell: %+v", table)
	}
}
