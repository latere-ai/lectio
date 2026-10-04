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
