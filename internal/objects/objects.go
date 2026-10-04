// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package objects is the shape of what a parse writes to the object store,
// and the read and the write of each: the manifest prepare leaves, a page's
// stored result, the document index, what an extraction keeps and
// produces, and a figure's description. A worker writes them and the API
// reads them, so the shapes are defined once, here, or in the package that
// computes them. Where each is kept is
// specs/002-object-model.md; the keys are in internal/blob.
package objects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/blob"
	"latere.ai/x/lectio/internal/figures"
	"latere.ai/x/lectio/internal/parse"
)

// contentType is the type every object of this package is stored with.
const contentType = "application/json"

// FirstReading is the revision of a page's result that was read once. A
// page that is read again within its parse would raise it; nothing does yet
// (specs/002-object-model.md, Open).
const FirstReading = 1

// Manifest is what prepare found out about a file, as the parse's row keeps
// it: the manifest of internal/parse, where the working copy is, and the
// lease token of the prepare task that wrote it.
type Manifest struct {
	parse.Manifest

	// Work is the object key the pages are read from: the source snapshot
	// itself when nothing had to be opened or converted, and a copy under
	// the parse's prefix otherwise.
	Work string `json:"work"`

	// Token is the lease token of the prepare task whose settle was
	// accepted. The pages of a native format are stored under keys that
	// carry it.
	Token int64 `json:"token"`
}

// Page is a page's result as it is stored. The API serves the page inside
// it; the revision and the image's key are the store's own.
type Page struct {
	// Revision counts the readings of the page within its parse, from
	// FirstReading.
	Revision int `json:"revision"`

	// Image is the object key of the image the reader saw. It is empty for
	// a page that was not read from an image.
	Image string `json:"image,omitempty"`

	Page document.Page `json:"page"`
}

// Summary is what a page task says of its result when it settles. The task
// store keeps it on the task's row, so a list of a running parse's pages is
// answered from rows and reads no object.
type Summary struct {
	Blocks    int                 `json:"blocks"`
	Source    document.PageSource `json:"source,omitempty"`
	Reused    bool                `json:"reused,omitempty"`
	Truncated bool                `json:"truncated,omitempty"`
}

// Entry is where one page of a document is stored.
type Entry struct {
	Number   int    `json:"number"`
	Key      string `json:"key,omitempty"`
	Revision int    `json:"revision,omitempty"`
}

// Index is the document index as it is stored: the document the API serves,
// and for each of its pages the key of the result that won.
type Index struct {
	document.Document

	// Keys lists the pages of the document in its order. A page with no key
	// has no stored result: the parse ended without reading it.
	Keys []Entry `json:"keys"`
}

// Put writes a value as JSON. It is the write of every shape with nothing
// to add: an extraction's input, its progress and its result
// (internal/extract), and a figure's description (internal/figures).
func Put(ctx context.Context, store blob.Store, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("objects: encoding %s: %w", key, err)
	}
	return store.Put(ctx, key, raw, contentType)
}

// Get reads a value stored as JSON, the read of every shape Put writes. A
// key that holds nothing is blob.ErrNotFound.
func Get(ctx context.Context, store blob.Store, key string, into any) error {
	raw, _, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("objects: %s does not hold what was written there: %w", key, err)
	}
	return nil
}

// PutPage stores a page's result.
func PutPage(ctx context.Context, store blob.Store, key string, p Page) error {
	return Put(ctx, store, key, p)
}

// GetPage reads a page's result. A page read from the store always has its
// list of blocks, empty when it has none.
func GetPage(ctx context.Context, store blob.Store, key string) (Page, error) {
	var p Page
	if err := Get(ctx, store, key, &p); err != nil {
		return Page{}, err
	}
	if p.Page.Blocks == nil {
		p.Page.Blocks = []document.Block{}
	}
	return p, nil
}

// PutIndex stores a document index.
func PutIndex(ctx context.Context, store blob.Store, key string, idx Index) error {
	return Put(ctx, store, key, idx)
}

// GetIndex reads a document index.
func GetIndex(ctx context.Context, store blob.Store, key string) (Index, error) {
	var idx Index
	if err := Get(ctx, store, key, &idx); err != nil {
		return Index{}, err
	}
	return idx, nil
}

// Skipped is a page the parse ended without reading: canceled or out of
// time. Nothing went wrong with the page, so it has no error and no blocks.
func Skipped(n int) document.Page {
	return document.Page{Number: n, State: document.PageSkipped, Blocks: []document.Block{}}
}

// Failed is a page that could not be read, as its task's row says why.
func Failed(n, attempts int, code, detail string) document.Page {
	return document.Page{
		Number: n, State: document.PageFailed, Source: document.SourceReader, Attempts: attempts,
		Blocks: []document.Block{}, Error: &document.Error{Code: code, Detail: detail},
	}
}

// reads is how many page results GetPages reads at once.
const reads = 8

// GetPages reads the page results under keys, several at once, and returns
// them in the order of the keys. It fails with the first key that could not
// be read.
func GetPages(ctx context.Context, store blob.Store, keys []string) ([]Page, error) {
	out := make([]Page, len(keys))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		limit = make(chan struct{}, reads)
	)
	for i, key := range keys {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			page, err := GetPage(ctx, store, key)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && first == nil {
				first = fmt.Errorf("%s: %w", key, err)
			}
			out[i] = page
		})
	}
	wg.Wait()
	return out, first
}

// Describe writes onto the figure blocks of pages the descriptions stored
// under keys, which are by the ref of each figure's block. It is how a
// figure that was described is read with its description, wherever its
// page is read: the page's stored result is as its reader left it, and the
// description is an object of its own. A description that is not there any
// more leaves its figure as it was.
func Describe(ctx context.Context, store blob.Store, pages []document.Page, keys map[string]string) error {
	type target struct {
		block *document.Block
		key   string
	}
	var targets []target
	for p := range pages {
		copied := false
		for i := range pages[p].Blocks {
			key, has := keys[pages[p].Blocks[i].Ref]
			if !has || key == "" || pages[p].Blocks[i].Kind != document.KindFigure {
				continue
			}
			// The blocks may be shared with whoever read the page: the page
			// is given a list of its own before one of them is written.
			if !copied {
				pages[p].Blocks, copied = slices.Clone(pages[p].Blocks), true
			}
			targets = append(targets, target{block: &pages[p].Blocks[i], key: key})
		}
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		limit = make(chan struct{}, reads)
	)
	for _, t := range targets {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			var d figures.Description
			err := Get(ctx, store, t.key, &d)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, blob.ErrNotFound):
			case err != nil && first == nil:
				first = fmt.Errorf("%s: %w", t.key, err)
			case err == nil:
				d.Onto(t.block)
			}
		})
	}
	wg.Wait()
	return first
}
