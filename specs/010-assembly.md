---
title: "Assembly: from page results to one document, with running headers, tables across pages, an outline, and the views a result is read in"
status: drafted
track: core
depends_on:
  - specs/002-object-model.md
  - specs/005-parse-graph.md
affects: [internal/assemble/, document/]
effort: medium
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Assembly

## Overview

A reader sees one page at a time, so nothing it returns knows about
the page before or after. Assembly is the step that does. It reads the
page results of a parse and writes the document: which blocks are
running headers and footers, which tables continue onto the next page,
and the outline of headings. Every pass is deterministic and calls no
model, so assembling the same pages twice gives the same document, and
re-assembling after a retry of one page costs nothing but the pass.
The same package holds the views a document is read in, Markdown,
text and chunks. A view is chosen by whoever reads the result and is
made when it is read, so another view needs no new parse.

## Current state

The earlier service had a formatter that rendered a layout to Markdown
or plain text and staged large output to the object store. That
rendering is carried over. Header and footer detection, joining tables
across pages, the outline and the chunks are new here.

## Design

`assemble` streams page results in page order and holds, per pass, only
what the pass needs: the first and last block of each page for the
header pass, the edge tables of adjacent pages for the table pass, and
headings for the outline. It never holds all blocks of a long document.

### Running headers and footers

A reader is asked to label running headers, footers and page numbers,
and does so unevenly. The pass makes the labeling consistent.

1. For each page take the first and the last block, when its kind is
   textual and its text is at most 200 characters.
2. Normalize the text: lower case, whitespace collapsed, every run of
   digits replaced by one placeholder, so "Page 3 of 40" and "Page 4 of
   40" are equal.
3. A normalized text that appears at the same edge on at least
   `max(2, ceil(pages / 2))` pages is a running header or footer,
   where `pages` counts the pages that succeeded and hold a block. Its
   blocks get kind `page_header` or `page_footer`, lose any heading
   level, and after the first occurrence get `repeated: true`.

Nothing is deleted. The blocks stay on their pages with their refs;
the renderings print a repeated block once. A caller who disagrees
with the pass still has every block.

### Tables across pages

When a page's last content block is a table and the next page's first
content block is a table with the same number of columns, the two are
parts of one table. Runs of two or more are recorded as a Span
([[002-object-model]]) with the parts' refs, the total rows and the
columns. A table that fills its page keeps the run open; anything
after it on the page ends the run there. A repeated header row on a
later part, equal to the first part's header row, is marked so that
renderings print it once. Content blocks exclude headers, footers and
page numbers, which is why this pass runs after the previous one. The
per-page tables are not changed.

Column count is the only test in the first version. Joining by
comparing column positions, or by asking a model, is left out until
this rule's errors are measured on real documents.

### Outline

Headings in page order form the outline: `[{ref, level, text, page}]`.
A reader's levels are per page and drift, so levels are normalized over
the document: the distinct levels seen are mapped to 1, 2, 3 in order.
A title its reader gave no level counts as 1 and a heading with none
as 2 before the mapping.

### Views

A view is how a document is rendered when it is read ([[003-api]]). It
is made from the stored page results at that moment and is not stored,
so no option of a submit decides it and no stored copy can be the
wrong one.

| Option | Values | Effect |
|---|---|---|
| `pages` | a selection | limits the rendering to these pages |
| `tables` | `auto`, `markdown`, `html` | how a table is written in Markdown |
| `repeated` | `once`, `keep`, `drop` | `once` prints the first occurrence of a running header or footer, `keep` prints every one, and `drop` prints none and leaves out page numbers too |
| `page_breaks` | on or off | marks where each page begins in Markdown, as a comment carrying the page number |

- **Markdown**: headings by their level in the outline, list items,
  tables as Markdown when no cell spans and as HTML when one does
  (`auto`), formulas in math delimiters, code fenced, a figure as its
  description in an emphasized line, a caption emphasized.
- **Text**: the same reading order as text only, a blank line between
  blocks.

A page that did not succeed prints nothing in either.

### Chunks

Chunks are a view too, cut when they are read:

- by `page`: one chunk per page.
- by `section`: a chunk begins at each title or heading and runs to
  the next.

Either way a chunk longer than the caller's `max_chars`, whose default
is `LECTIO_CHUNK_MAX_CHARS` (6,000), is split at block boundaries, repeated headers and footers are
left out, and a block is never split, so a single block longer than
the bound is a chunk of its own. Each chunk lists the pages and block
refs it covers, so a retrieval hit can be shown on the page.

### The document index

`document.json` holds the page list with each page's state, the spans,
the outline, the usage totals, and the names of the renderings and
fields present ([[002-object-model]]). It does not repeat the blocks;
those are read by page. `GET /parses/{parse}/document?format=json`
returns the index and nothing more, so the reply for a long document
is small and the server never builds the whole document in memory.

### Failed pages

A failed page is listed with its error and contributes nothing to any
pass. A table is not joined across a failed page, nor across a page
that was not selected.

## Not in this spec

Reading order across columns within a page, which is the reader's.
Merging tables by content. De-hyphenation and joining a paragraph
split by a page break. Language detection.

## Implementation status

Built, in `internal/assemble`:

- The header and footer pass, the table pass and the outline, each
  deterministic, and the document index.
- The Markdown and text views with every option above, and chunks by
  page and by section.

Remaining:

- Streaming. The passes take every page of the parse at once, which
  the in-process runner can afford and a 3,000-page document on a
  worker cannot.
- The repeated header row of a later part of a span is not marked.
- A section chunk ends at the next heading of any level, not at the
  next heading of the same or a higher level.
- `LECTIO_CHUNK_MAX_CHARS`, the server's default for `max_chars`, is
  not read; the default is fixed at 6,000.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Assembling the same page results twice writes byte-identical objects | a determinism test |
| A 40-page fixture with a running title and "Page n of 40" yields 40 header blocks and 40 footer blocks, 39 of each marked repeated, and the Markdown prints each once | a fixture test |
| A fixture table spanning three pages yields one span of three parts with the header row printed once; two unrelated tables with different column counts on adjacent pages yield none | a fixture test |
| A document whose only heading levels are 2 and 4 has an outline with levels 1 and 2 | a unit test |
| `section` chunks cover every non-repeated block exactly once and none exceeds the size limit unless it is one block | a property test over fixtures |
| Each value of each view option changes the rendering as stated and reads no page again | a table test over one assembled fixture |
| Assembling a 3,000-page document keeps worker heap under 64 MiB | a memory test |
| After one page is retried and succeeds, `assemble` runs again and the document reflects the page | an end-to-end test |
