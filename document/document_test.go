// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package document

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestKindsAreAClosedSet(t *testing.T) {
	all := Kinds()
	if len(all) != 17 {
		t.Fatalf("the spec lists 17 kinds, Kinds() has %d", len(all))
	}
	seen := map[Kind]bool{}
	for _, k := range all {
		if !k.Valid() {
			t.Errorf("%q is listed and not valid", k)
		}
		if seen[k] {
			t.Errorf("%q is listed twice", k)
		}
		seen[k] = true
	}
	all[0] = "changed"
	if Kinds()[0] != KindTitle {
		t.Fatal("Kinds must return a copy")
	}
	if Kind("paragraph").Valid() {
		t.Fatal(`"paragraph" is outside the set`)
	}
}

func TestCoerceKind(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  Kind
		ok    bool
	}{
		{"table", KindTable, true},
		{" Heading ", KindHeading, true},
		{"PAGE_FOOTER", KindPageFooter, true},
		{"paragraph", KindText, false},
		{"", KindText, false},
	} {
		got, ok := CoerceKind(tc.label)
		if got != tc.want || ok != tc.ok {
			t.Errorf("CoerceKind(%q) = %q, %v; want %q, %v", tc.label, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTextualKinds(t *testing.T) {
	for _, k := range []Kind{KindTitle, KindHeading, KindText, KindListItem, KindCaption, KindFootnote, KindPageHeader, KindPageFooter, KindPageNumber} {
		if !k.Textual() {
			t.Errorf("%q is running text", k)
		}
	}
	for _, k := range []Kind{KindTable, KindFigure, KindFormula, KindForm, KindKeyValue, KindSignature, KindBarcode, KindCode} {
		if k.Textual() {
			t.Errorf("%q is not running text", k)
		}
	}
}

func TestBoxValid(t *testing.T) {
	for _, tc := range []struct {
		name string
		box  Box
		want bool
	}{
		{"inside", Box{0.1, 0.2, 0.9, 0.8}, true},
		{"whole page", Box{0, 0, 1, 1}, true},
		{"negative", Box{-0.1, 0, 1, 1}, false},
		{"past the edge", Box{0, 0, 1.2, 1}, false},
		{"inverted x", Box{0.9, 0, 0.1, 1}, false},
		{"inverted y", Box{0, 0.9, 1, 0.1}, false},
		{"no width", Box{0.5, 0, 0.5, 1}, false},
	} {
		if got := tc.box.Valid(); got != tc.want {
			t.Errorf("%s: Valid() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBoxNormalize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in, out Box
		changed bool
		ok      bool
	}{
		{"untouched", Box{0.1, 0.2, 0.9, 0.8}, Box{0.1, 0.2, 0.9, 0.8}, false, true},
		{"swapped corners", Box{0.9, 0.8, 0.1, 0.2}, Box{0.1, 0.2, 0.9, 0.8}, true, true},
		{"clamped", Box{-0.2, 0.1, 1.4, 0.9}, Box{0, 0.1, 1, 0.9}, true, true},
		{"no area after clamping", Box{1.2, 0.1, 1.6, 0.9}, Box{1, 0.1, 1, 0.9}, true, false},
	} {
		out, changed, ok := tc.in.Normalize()
		if out != tc.out || changed != tc.changed || ok != tc.ok {
			t.Errorf("%s: Normalize() = %v, %v, %v; want %v, %v, %v", tc.name, out, changed, ok, tc.out, tc.changed, tc.ok)
		}
	}
}

func TestRefRoundTrips(t *testing.T) {
	if got := Ref(3, 12); got != "3.12" {
		t.Fatalf("Ref(3, 12) = %q", got)
	}
	page, order, err := ParseRef("3.12")
	if err != nil || page != 3 || order != 12 {
		t.Fatalf("ParseRef = %d, %d, %v", page, order, err)
	}
	for _, bad := range []string{"", "3", "3.", ".4", "a.b", "0.1", "1.0", "-1.2", "1.2.3"} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) must fail", bad)
		}
	}
}

func TestNumberSortsRenumbersAndAddresses(t *testing.T) {
	blocks := []Block{
		{Kind: KindText, Order: 30, Text: "third"},
		{Kind: KindTitle, Order: 2, Text: "first"},
		{Kind: KindText, Order: 30, Text: "fourth"},
		{Kind: KindText, Order: 7, Text: "second"},
	}
	got := Number(5, blocks)
	var texts, refs []string
	for i, b := range got {
		texts = append(texts, b.Text)
		refs = append(refs, b.Ref)
		if b.Order != i+1 {
			t.Errorf("block %d has order %d", i, b.Order)
		}
	}
	if want := []string{"first", "second", "third", "fourth"}; !reflect.DeepEqual(texts, want) {
		t.Fatalf("order = %v, want %v (equal orders keep the reader's sequence)", texts, want)
	}
	if want := []string{"5.1", "5.2", "5.3", "5.4"}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
}

func page(blocks ...Block) Page {
	return Page{Number: 2, Width: 595, Height: 842, State: PageSucceeded, Source: SourceReader, Blocks: Number(2, blocks)}
}

func TestPageValidate(t *testing.T) {
	box := Box{0.1, 0.1, 0.9, 0.2}
	good := page(Block{Kind: KindTitle, Box: &box, Text: "Report", Level: 1}, Block{Kind: KindTable, Table: &Table{Rows: 1, Cols: 1}})
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid page: %v", err)
	}

	bad := Box{0.9, 0.1, 0.1, 0.2}
	for _, tc := range []struct {
		name   string
		mutate func(*Page)
		want   string
	}{
		{"page number", func(p *Page) { p.Number = 0 }, "below 1"},
		{"blocks on a failed page", func(p *Page) { p.State = PageFailed }, "holds 2 blocks"},
		{"unknown kind", func(p *Page) { p.Blocks[0].Kind = "paragraph" }, "unknown kind"},
		{"order", func(p *Page) { p.Blocks[1].Order = 5 }, "has order 5"},
		{"ref", func(p *Page) { p.Blocks[1].Ref = "9.9" }, `has ref "9.9"`},
		{"box", func(p *Page) { p.Blocks[0].Box = &bad }, "box outside the page"},
		{"level", func(p *Page) { p.Blocks[0].Level = 7 }, "has level 7"},
		{"table on text", func(p *Page) { p.Blocks[0].Table = &Table{} }, "holds a table"},
	} {
		p := page(Block{Kind: KindTitle, Box: &box, Text: "Report", Level: 1}, Block{Kind: KindTable, Table: &Table{Rows: 1, Cols: 1}})
		tc.mutate(&p)
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestPageFind(t *testing.T) {
	p := page(Block{Kind: KindText, Text: "a"}, Block{Kind: KindText, Text: "b"})
	b, err := p.Find("2.2")
	if err != nil || b.Text != "b" {
		t.Fatalf("Find(2.2) = %+v, %v", b, err)
	}
	for _, ref := range []string{"2.3", "3.1"} {
		if _, err := p.Find(ref); !errors.Is(err, ErrNoBlock) {
			t.Errorf("Find(%q) = %v, want ErrNoBlock", ref, err)
		}
	}
	if _, err := p.Find("nope"); err == nil || errors.Is(err, ErrNoBlock) {
		t.Errorf("Find of a malformed ref must say the ref is malformed, got %v", err)
	}
}

func TestPageSummary(t *testing.T) {
	p := Page{Number: 4, State: PageFailed, Source: SourceReader, Error: &Error{Code: "page_unreadable"}}
	got := p.Summary()
	want := PageSummary{Number: 4, State: PageFailed, Source: SourceReader, Blocks: 0, Error: &Error{Code: "page_unreadable"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Summary() = %+v, want %+v", got, want)
	}
}

func TestUsageAdd(t *testing.T) {
	got := Usage{Pages: 1, InputTokens: 10, OutputTokens: 4, Cost: "0.01", Currency: "USD"}.Add(Usage{Pages: 2, InputTokens: 5, OutputTokens: 1, Cost: "0.02"})
	want := Usage{Pages: 3, InputTokens: 15, OutputTokens: 5, Cost: "0.01", Currency: "USD"}
	if got != want {
		t.Fatalf("Add = %+v, want %+v (costs are not summed here)", got, want)
	}
}

// The wire shape is part of the contract: a block with no position says
// "box": null, and a native page says so.
func TestWireShape(t *testing.T) {
	box := Box{0.08, 0.31, 0.92, 0.58}
	p := Page{
		Number: 3, Width: 595, Height: 842, State: PageSucceeded, Source: SourceReader,
		Reader: "default", Model: "some-model", Attempts: 1,
		Blocks: Number(3, []Block{
			{Kind: KindTable, Box: &box, Text: "Quarter | Revenue", Table: &Table{Rows: 2, Cols: 2, HTML: "<table></table>", Cells: []Cell{{Row: 0, Col: 0, Text: "Quarter"}}}, Flags: []Flag{FlagBoxClamped}},
			{Kind: KindText, Text: "no position"},
		}),
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"ref":"3.1"`, `"kind":"table"`, `"box":[0.08,0.31,0.92,0.58]`, `"flags":["box_clamped"]`,
		`"ref":"3.2"`, `"box":null`, `"source":"reader"`, `"state":"succeeded"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire form lacks %s:\n%s", want, raw)
		}
	}
	var back Page
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, p) {
		t.Fatalf("a page must survive JSON:\n got %+v\nwant %+v", back, p)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
}
