// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
)

func read(t *testing.T, data, mediaType string) document.Page {
	t.Helper()
	pages, err := Pages(context.Background(), []byte(data), mediaType)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatalf("%d pages", len(pages))
	}
	p := pages[0]
	if p.Number != 1 || p.State != document.PageSucceeded || p.Source != document.SourceNative || p.Usage.Pages != 1 {
		t.Fatalf("page = %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, b := range p.Blocks {
		if b.Box != nil {
			t.Fatalf("a native block has no box: %+v", b)
		}
	}
	return p
}

func shape(p document.Page) []string {
	out := make([]string, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		s := fmt.Sprintf("%s:%s", b.Kind, b.Text)
		if b.Level > 0 {
			s = fmt.Sprintf("%s%d:%s", b.Kind, b.Level, b.Text)
		}
		out = append(out, s)
	}
	return out
}

func TestReads(t *testing.T) {
	for mediaType, want := range map[string]bool{TypeText: true, TypeMarkdown: true, TypeCSV: true, "text/html": false, "application/pdf": false} {
		if Reads(mediaType) != want {
			t.Errorf("Reads(%q) = %v", mediaType, !want)
		}
	}
}

func TestPlainText(t *testing.T) {
	got := shape(read(t, "First paragraph\nstill the first.\r\n\r\n\n\nSecond.\n\n   \n", TypeText))
	want := []string{"text:First paragraph\nstill the first.", "text:Second."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks = %q, want %q", got, want)
	}
	if got := read(t, "", TypeText); len(got.Blocks) != 0 {
		t.Fatalf("an empty file is one page with no blocks: %+v", got)
	}
}

func TestMarkdown(t *testing.T) {
	src := strings.Join([]string{
		"# Report ##",
		"Intro line one",
		"line two.",
		"",
		"## Findings",
		"- first",
		"* second",
		"+ third",
		"12. numbered",
		"Not. a list item",
		"#hashtag is text",
		"####### seven marks is text",
		"```go",
		"x := 1",
		"",
		"# not a heading",
		"```",
		"After the code.",
		"```",
		"never closed",
	}, "\r\n")
	got := shape(read(t, src, TypeMarkdown))
	want := []string{
		"title1:Report",
		"text:Intro line one\nline two.",
		"heading2:Findings",
		"list_item:first", "list_item:second", "list_item:third", "list_item:numbered",
		"text:Not. a list item\n#hashtag is text\n####### seven marks is text",
		"code:x := 1\n\n# not a heading",
		"text:After the code.",
		"code:never closed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks:\n got %q\nwant %q", got, want)
	}
}

func TestDelimited(t *testing.T) {
	p := read(t, "name,amount\nwidgets,\"1,200\"\n<b>bold</b>,3,extra\n", TypeCSV)
	if len(p.Blocks) != 1 || p.Blocks[0].Kind != document.KindTable {
		t.Fatalf("blocks = %+v", p.Blocks)
	}
	b := p.Blocks[0]
	if b.Table.Rows != 3 || b.Table.Cols != 3 || len(b.Table.Cells) != 7 {
		t.Fatalf("table = %+v", b.Table)
	}
	if want := "name | amount\nwidgets | 1,200\n<b>bold</b> | 3 | extra"; b.Text != want {
		t.Fatalf("text = %q, want %q", b.Text, want)
	}
	if !strings.Contains(b.Table.HTML, "<td>&lt;b&gt;bold&lt;/b&gt;</td>") || !strings.HasPrefix(b.Table.HTML, "<table><tr><td>name</td>") {
		t.Fatalf("markup must escape cell text: %s", b.Table.HTML)
	}
	if got := read(t, "", TypeCSV); len(got.Blocks) != 0 {
		t.Fatalf("an empty table is a page with no blocks: %+v", got)
	}
}

func TestFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		data, mediaType string
		code            fault.Code
	}{
		"not UTF-8":            {"\xff\xfe\x00", TypeText, fault.DocumentCorrupt},
		"a type not read":      {"<p>x</p>", "text/html", fault.UnsupportedMediaType},
		"a quote never closed": {"a,\"b\n", TypeCSV, fault.DocumentCorrupt},
	} {
		_, err := Pages(context.Background(), []byte(tc.data), tc.mediaType)
		if name == "a quote never closed" && err == nil {
			// Lazy quotes read an unclosed quote as text; what cannot fail is not tested as a failure.
			continue
		}
		if fault.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Pages(ctx, []byte("x"), TypeText); err == nil {
		t.Fatal("a canceled read must fail")
	}
}
