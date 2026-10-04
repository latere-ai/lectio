// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package objects_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/objects"
)

// TestAPageAndAnIndexAreReadAsTheyWereWritten: a stored page keeps its
// revision and its image's key around the page the API serves, an index
// keeps the key of every page beside the document, and a page read back
// always has its list of blocks.
func TestAPageAndAnIndexAreReadAsTheyWereWritten(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMemory()
	page := objects.Page{Revision: objects.FirstReading, Image: "parses/p/pages/1.1.png", Page: document.Page{Number: 1, State: document.PageSucceeded}}
	if err := objects.PutPage(ctx, store, "parses/p/pages/1.1.json", page); err != nil {
		t.Fatal(err)
	}
	got, err := objects.GetPage(ctx, store, "parses/p/pages/1.1.json")
	if err != nil || got.Revision != 1 || got.Image != page.Image || got.Page.Number != 1 || got.Page.Blocks == nil || len(got.Page.Blocks) != 0 {
		t.Fatalf("the page read back is %+v, %v", got, err)
	}
	raw, contentType, err := store.Get(ctx, "parses/p/pages/1.1.json")
	if err != nil || contentType != "application/json" || !strings.Contains(string(raw), `"revision":1`) {
		t.Fatalf("the stored page is %s of %q, %v", raw, contentType, err)
	}

	idx := objects.Index{
		Parse: "p", Pages: []document.PageSummary{{Number: 1, State: document.PageSucceeded}},
		Keys: []objects.Entry{{Number: 1, Key: "parses/p/pages/1.1.json", Revision: 1}},
	}
	if err := objects.PutIndex(ctx, store, "parses/p/document.2.json", idx); err != nil {
		t.Fatal(err)
	}
	back, err := objects.GetIndex(ctx, store, "parses/p/document.2.json")
	if err != nil || back.Parse != "p" || len(back.Pages) != 1 || len(back.Keys) != 1 || back.Keys[0] != idx.Keys[0] {
		t.Fatalf("the index read back is %+v, %v", back, err)
	}

	// What is not there is not found, and what is there and is no page is
	// an error that names the key.
	if _, err := objects.GetPage(ctx, store, "parses/p/pages/9.1.json"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a page that is not there: %v", err)
	}
	if _, err := objects.GetIndex(ctx, store, "parses/p/document.9.json"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("an index that is not there: %v", err)
	}
	if err := store.Put(ctx, "parses/p/pages/2.1.json", []byte("{"), "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.GetPage(ctx, store, "parses/p/pages/2.1.json"); err == nil || !strings.Contains(err.Error(), "parses/p/pages/2.1.json") {
		t.Fatalf("an object that is no page: %v", err)
	}

	pages, err := objects.GetPages(ctx, store, []string{"parses/p/pages/1.1.json", "parses/p/pages/1.1.json"})
	if err != nil || len(pages) != 2 || pages[1].Page.Number != 1 {
		t.Fatalf("2 pages read at once: %+v, %v", pages, err)
	}
	if _, err := objects.GetPages(ctx, store, []string{"parses/p/pages/1.1.json", "parses/p/pages/9.1.json"}); !errors.Is(err, blob.ErrNotFound) || !strings.Contains(err.Error(), "9.1.json") {
		t.Fatalf("pages of which one is not there: %v", err)
	}
	if skipped := objects.Skipped(4); skipped.State != document.PageSkipped || skipped.Error != nil || skipped.Blocks == nil {
		t.Fatalf("a skipped page is %+v", skipped)
	}
	if failed := objects.Failed(5, 3, "page_unreadable", "no"); failed.State != document.PageFailed || failed.Attempts != 3 || failed.Error.Code != "page_unreadable" {
		t.Fatalf("a failed page is %+v", failed)
	}
}

// TestADescriptionIsReadOntoItsFigure: a figure's description is an object
// of its own, by the ref of the figure's block, and a page is read with the
// descriptions of its figures written onto them. The page's own blocks are
// not written to: whoever else holds them sees them as they were. A
// description that is gone, a ref with no key, and a ref that is no
// figure's leave their blocks as they were, and an object that is no
// description is an error that names its key.
func TestADescriptionIsReadOntoItsFigure(t *testing.T) {
	ctx := context.Background()
	store := blob.NewMemory()
	kept := figures.Description{Type: "chart", Description: "Revenue by quarter.", Labels: []string{"Q1", "Q2"}, Model: "m"}
	if err := objects.Put(ctx, store, "parses/p/figures/1.2.3.json", kept); err != nil {
		t.Fatal(err)
	}
	var back figures.Description
	if err := objects.Get(ctx, store, "parses/p/figures/1.2.3.json", &back); err != nil || back.Description != kept.Description || len(back.Labels) != 2 {
		t.Fatalf("the description read back is %+v, %v", back, err)
	}
	if err := objects.Put(ctx, store, "parses/p/unencodable.json", func() {}); err == nil || !strings.Contains(err.Error(), "unencodable.json") {
		t.Fatalf("a value that does not encode: %v", err)
	}

	shared := []document.Block{
		{Ref: "1.1", Kind: document.KindText, Text: "A paragraph."},
		{Ref: "1.2", Kind: document.KindFigure},
		{Ref: "1.3", Kind: document.KindFigure, Text: "printed"},
		{Ref: "1.4", Kind: document.KindFigure},
	}
	pages := []document.Page{{Number: 1, Blocks: shared}, {Number: 2, Blocks: []document.Block{{Ref: "2.1", Kind: document.KindFigure}}}}
	keys := map[string]string{
		"1.1": "parses/p/figures/1.2.3.json", // no figure
		"1.2": "parses/p/figures/1.2.3.json",
		"1.3": "parses/p/figures/gone.json", // not there any more
		"1.4": "",                           // lost by its run
		"9.9": "parses/p/figures/1.2.3.json",
	}
	if err := objects.Describe(ctx, store, pages, keys); err != nil {
		t.Fatal(err)
	}
	got := pages[0].Blocks
	if got[1].Description != kept.Description || got[1].Figure == nil || got[1].Figure.Type != "chart" || got[1].Text != "Q1\nQ2" {
		t.Fatalf("the described figure is %+v", got[1])
	}
	if got[0].Description != "" || got[2].Description != "" || got[2].Text != "printed" || got[3].Description != "" || pages[1].Blocks[0].Description != "" {
		t.Fatalf("blocks with no description to read were written: %+v", got)
	}
	if shared[1].Description != "" {
		t.Fatal("the page's own blocks were written to")
	}

	if err := store.Put(ctx, "parses/p/figures/bad.json", []byte("["), "application/json"); err != nil {
		t.Fatal(err)
	}
	if err := objects.Describe(ctx, store, pages, map[string]string{"2.1": "parses/p/figures/bad.json"}); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("an object that is no description: %v", err)
	}
}
