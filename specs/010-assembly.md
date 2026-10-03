---
title: "Assembly: from page results to one document, with running headers, tables across pages, an outline, renderings and chunks"
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
the outline of headings, the text renderings, and the chunks. Every
pass is deterministic and calls no model, so assembling the same pages
twice gives the same document, and re-assembling after a retry of one
page costs nothing but the pass.

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
   `max(2, ceil(pages / 2))` pages is a running header or footer. Its
   blocks get kind `page_header` or `page_footer` and, after the first
   occurrence, `repeated: true`.

Nothing is deleted. The blocks stay on their pages with their refs;
the renderings print a repeated block once. A caller who disagrees
with the pass still has every block.

### Tables across pages

When a page's last content block is a table and the next page's first
content block is a table with the same number of columns, the two are
parts of one table. Runs of two or more are recorded as a Span
([[002-object-model]]) with the parts' refs, the total rows and the
columns. A repeated header row on a later part, equal to the first
part's header row, is marked so that renderings print it once.
Content blocks exclude headers, footers and page numbers, which is why
this pass runs after the previous one. The per-page tables are not
changed.

Column count is the only test in the first version. Joining by
comparing column positions, or by asking a model, is left out until
this rule's errors are measured on real documents.

### Outline

Headings in page order form the outline: `[{ref, level, text, page}]`.
A reader's levels are per page and drift, so levels are normalized over
the document: the distinct levels seen are mapped to 1, 2, 3 in order,
and a title on the first page is level 1.

### Renderings

`output.text` selects one:

- `markdown`: headings by level, lists, tables as Markdown when no
  cell spans and as HTML when one does, formulas in math delimiters,
  figures as their description in a captioned line, page breaks as a
  comment carrying the page number.
- `plain`: the same reading order as text only.
- `none`: no rendering is written.

A rendering is written to the object store and streamed by the API; it
is never placed inside a JSON reply.

### Chunks

`output.chunks` selects one:

- `page`: one chunk per page.
- `section`: a chunk begins at each heading and runs to the next
  heading of the same or a higher level; a chunk over
  `LECTIO_CHUNK_MAX_CHARS` (default 6,000) is split at block
  boundaries.
- `none`: no chunks.

Each chunk lists the pages and block refs it covers, so a retrieval
hit can be shown on the page.

### The document index

`document.json` holds the page list with each page's state, the spans,
the outline, the usage totals, and the names of the renderings and
fields present. It does not repeat the blocks; those are read by page.
`GET /parses/{parse}/document?format=json` streams the index followed
by the pages, so a client that wants one object gets one and the
server still never builds it in memory.

### Failed pages

A failed page is listed with its error and contributes nothing to any
pass. A table is not joined across a failed page.

## Not in this spec

Reading order across columns within a page, which is the reader's.
Merging tables by content. De-hyphenation and joining a paragraph
split by a page break. Language detection.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Assembling the same page results twice writes byte-identical objects | a determinism test |
| A 40-page fixture with a running title and "Page n of 40" yields 40 header blocks and 40 footer blocks, 39 of each marked repeated, and the Markdown prints each once | a fixture test |
| A fixture table spanning three pages yields one span of three parts with the header row printed once; two unrelated tables with different column counts on adjacent pages yield none | a fixture test |
| A document whose only heading levels are 2 and 4 has an outline with levels 1 and 2 | a unit test |
| `section` chunks cover every non-repeated block exactly once and none exceeds the size limit | a property test over fixtures |
| Assembling a 3,000-page document keeps worker heap under 64 MiB | a memory test |
| After one page is retried and succeeds, `assemble` runs again and the document reflects the page | an end-to-end test |
