// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/assemble"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/store"
)

// The bounds of a chunk's size, in characters.
const (
	minChunk     = 200
	maxChunk     = 100000
	defaultChunk = 6000
)

// oneOf reads a query parameter that takes one of a few values. The first
// is the default.
func oneOf(r *http.Request, name string, values ...string) (string, error) {
	got := r.URL.Query().Get(name)
	if got == "" {
		return values[0], nil
	}
	if !slices.Contains(values, got) {
		return "", invalid(name, "%s is one of %v", name, values)
	}
	return got, nil
}

// assembled returns a parse, its document, and its pages, once the parse
// has assembled them.
func (s *Server) assembled(r *http.Request, owner string) (store.Parse, document.Document, []document.Page, error) {
	p, err := s.Store.Parse(owner, r.PathValue("parse"))
	if err != nil {
		return p, document.Document{}, nil, err
	}
	doc, ok := s.Store.Document(p.ID)
	if !ok {
		return p, doc, nil, fault.New(fault.DocumentNotReady, "parse %s is %s and has not assembled its pages", p.ID, p.State)
	}
	return p, doc, s.Store.Pages(p.ID), nil
}

// getDocument serves the document: its index as JSON, or its content as
// Markdown or text. The rendering is made now, from the stored blocks, by
// the view the caller asked for.
func (s *Server) getDocument(w http.ResponseWriter, r *http.Request, owner string) error {
	format, err := oneOf(r, "format", "json", "markdown", "text")
	if err != nil {
		return err
	}
	view := assemble.View{}
	if view.Tables, err = oneOf(r, "tables", assemble.TablesAuto, assemble.TablesMarkdown, assemble.TablesHTML); err != nil {
		return err
	}
	if view.Repeated, err = oneOf(r, "repeated", assemble.RepeatedOnce, assemble.RepeatedKeep, assemble.RepeatedDrop); err != nil {
		return err
	}
	if raw := r.URL.Query().Get("page_breaks"); raw != "" {
		if view.PageBreaks, err = strconv.ParseBool(raw); err != nil {
			return invalid("page_breaks", "page_breaks is true or false")
		}
	}

	p, doc, read, err := s.assembled(r, owner)
	if err != nil {
		return err
	}
	if format == "json" {
		httpjson.Write(w, http.StatusOK, doc)
		return nil
	}
	if selection := r.URL.Query().Get("pages"); selection != "" {
		if view.Pages, err = pages.Select(selection, p.Manifest.PagesTotal); err != nil {
			return err
		}
	}

	// The rendering is built before the first byte is sent, so a failure
	// is still an error response and not half a document.
	var out bytes.Buffer
	contentType := "text/plain; charset=utf-8"
	if format == "markdown" {
		contentType = "text/markdown; charset=utf-8"
		err = assemble.Markdown(&out, read, doc.Outline, view)
	} else {
		err = assemble.Text(&out, read, view)
	}
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(out.Bytes())
	return nil
}

// listPages lists the pages the parse reads, each with its state. A page
// that was not read yet is pending.
func (s *Server) listPages(w http.ResponseWriter, r *http.Request, owner string) error {
	p, err := s.Store.Parse(owner, r.PathValue("parse"))
	if err != nil {
		return err
	}
	read := map[int]document.PageSummary{}
	for _, page := range s.Store.Pages(p.ID) {
		read[page.Number] = page.Summary()
	}
	out := struct {
		Pages []document.PageSummary `json:"pages"`
	}{Pages: []document.PageSummary{}}
	if p.Manifest != nil {
		for _, n := range p.Manifest.Selected {
			summary, ok := read[n]
			if !ok {
				summary = document.PageSummary{Number: n, State: document.PagePending}
			}
			out.Pages = append(out.Pages, summary)
		}
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}

// page returns one page of a parse the caller owns. A page the parse does
// not read is not found; one it has not read yet is not ready.
func (s *Server) page(owner, parseID string, n int) (document.Page, error) {
	p, err := s.Store.Parse(owner, parseID)
	if err != nil {
		return document.Page{}, err
	}
	if p.Manifest != nil && !slices.Contains(p.Manifest.Selected, n) {
		return document.Page{}, fault.New(fault.PageNotFound, "parse %s reads no page %d", p.ID, n)
	}
	page, ok := s.Store.Page(p.ID, n)
	if !ok {
		if p.Terminal() {
			// The parse ended before it knew its pages, so there is none.
			return document.Page{}, fault.New(fault.PageNotFound, "parse %s ended with no page %d", p.ID, n)
		}
		return document.Page{}, fault.New(fault.PageNotReady, "page %d of parse %s has not been read yet", n, p.ID)
	}
	return page, nil
}

func pageNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("page"))
	if err != nil || n < 1 {
		return 0, invalid("page", "a page is a number from 1")
	}
	return n, nil
}

func (s *Server) getPage(w http.ResponseWriter, r *http.Request, owner string) error {
	n, err := pageNumber(r)
	if err != nil {
		return err
	}
	page, err := s.page(owner, r.PathValue("parse"), n)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, page)
	return nil
}

func (s *Server) getPageImage(w http.ResponseWriter, r *http.Request, owner string) error {
	n, err := pageNumber(r)
	if err != nil {
		return err
	}
	parseID := r.PathValue("parse")
	if _, err := s.page(owner, parseID, n); err != nil {
		return err
	}
	img, ok := s.Store.Image(parseID, n)
	if !ok || len(img.Data) == 0 {
		return fault.New(fault.PageNotFound, "page %d was not read from an image, so it has none", n)
	}
	w.Header().Set("Content-Type", img.MediaType)
	_, _ = w.Write(img.Data)
	return nil
}

func (s *Server) getBlock(w http.ResponseWriter, r *http.Request, owner string) error {
	ref := r.PathValue("ref")
	n, _, err := document.ParseRef(ref)
	if err != nil {
		return invalid("ref", "a block's ref is <page>.<order>")
	}
	page, err := s.page(owner, r.PathValue("parse"), n)
	if err != nil {
		return err
	}
	block, err := page.Find(ref)
	if err != nil {
		return fault.New(fault.BlockNotFound, "page %d holds no block %s", n, ref)
	}
	httpjson.Write(w, http.StatusOK, block)
	return nil
}

// listBlocks serves the blocks of every page read so far, one JSON object
// per line, in page and reading order. It is the bulk read: a caller that
// wants all of a long document asks once and not once per page. It answers
// while the parse runs, with what has been read.
func (s *Server) listBlocks(w http.ResponseWriter, r *http.Request, owner string) error {
	p, err := s.Store.Parse(owner, r.PathValue("parse"))
	if err != nil {
		return err
	}
	var only []int
	if selection := r.URL.Query().Get("pages"); selection != "" {
		if p.Manifest == nil {
			return fault.New(fault.DocumentNotReady, "parse %s has not counted its pages yet", p.ID)
		}
		if only, err = pages.Select(selection, p.Manifest.PagesTotal); err != nil {
			return err
		}
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, page := range s.Store.Pages(p.ID) {
		if only != nil && !slices.Contains(only, page.Number) {
			continue
		}
		for _, block := range page.Blocks {
			if err := enc.Encode(block); err != nil {
				return err
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(out.Bytes())
	return nil
}

// listChunks serves the document cut into chunks, one JSON object per
// line. The cut is made now, so another strategy or size is another read.
func (s *Server) listChunks(w http.ResponseWriter, r *http.Request, owner string) error {
	by, err := oneOf(r, "by", assemble.BySection, assemble.ByPage)
	if err != nil {
		return err
	}
	size := defaultChunk
	if raw := r.URL.Query().Get("max_chars"); raw != "" {
		if size, err = strconv.Atoi(raw); err != nil || size < minChunk || size > maxChunk {
			return invalid("max_chars", "max_chars is from %d to %d", minChunk, maxChunk)
		}
	}
	_, _, read, err := s.assembled(r, owner)
	if err != nil {
		return err
	}

	// Encoded before the first byte is sent, as a document is, so a
	// failure is an error response and not half a list.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, chunk := range assemble.Chunks(read, by, size) {
		if err := enc.Encode(chunk); err != nil {
			return err
		}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(out.Bytes())
	return nil
}

// readerView is a reader as the API describes it.
type readerView struct {
	Name    string          `json:"name"`
	Accepts []string        `json:"accepts"`
	Image   imageView       `json:"image"`
	Boxes   bool            `json:"boxes"`
	Kinds   []document.Kind `json:"kinds,omitempty"`
	Version string          `json:"version,omitempty"`
	Default bool            `json:"default,omitempty"`
}

type imageView struct {
	DPI      int    `json:"dpi,omitempty"`
	LongEdge int    `json:"long_edge,omitempty"`
	Format   string `json:"format,omitempty"`
}

// listReaders lists the configured readers under their configured names,
// the one the routing policy tries first ahead of the others.
func (s *Server) listReaders(w http.ResponseWriter, _ *http.Request, _ string) error {
	out := struct {
		Readers []readerView `json:"readers"`
	}{Readers: []readerView{}}
	names := slices.Clone(s.Chain)
	for _, name := range slices.Sorted(maps.Keys(s.Readers)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for i, name := range names {
		rd, ok := s.Readers[name]
		if !ok {
			continue
		}
		d := rd.Describe()
		out.Readers = append(out.Readers, readerView{
			Name: name, Accepts: d.Accepts, Boxes: d.Boxes, Kinds: d.Kinds, Version: d.Version,
			Image:   imageView{DPI: d.Image.DPI, LongEdge: d.Image.LongEdge, Format: d.Image.Format},
			Default: i == 0 && len(s.Chain) > 0,
		})
	}
	httpjson.Write(w, http.StatusOK, out)
	return nil
}
