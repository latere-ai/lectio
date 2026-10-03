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
page numbers and running headers and footers, which tables continue
onto the next page, and the outline of headings. Every pass is deterministic and calls no
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
what the pass needs: the edge blocks and the furniture of each page
for the furniture pass, the edge tables of adjacent pages for the
table pass, and headings for the outline. It never holds all blocks of
a long document.

### Page furniture

A reader is asked to label running headers, footers and page numbers,
and does so unevenly. The pass makes the labeling consistent. It errs
toward content: a block hidden as furniture is lost to whoever reads a
rendering, and a line of furniture left in the text is only noise. A
block becomes furniture on strong evidence and on nothing else. The
pass has three steps.

1. **Page numbers.** A page's first or last block of kind `text`,
   `caption` or `footnote` whose whole text is a page number becomes
   `page_number`. So does a block a reader labeled `page_header` or
   `page_footer` that holds a page number and nothing else, wherever it
   is on the page. A page number is a number of at most four digits,
   alone or after a word for page (`page`, `seite`, `pagina`, `página`,
   `p.`), with an optional total, inside optional dashes, brackets or a
   closing dot: "3", "Page 3", "3 / 40", "Page 3 of 40", "- 3 -",
   "[3]". A `title` or a `heading` is never relabeled by this step, and
   a block a reader labeled `page_number` keeps that kind.
2. **Running lines.** For each page take the first and the last block
   that is not a page number, when its kind is textual and its text is
   at most 200 characters. Compare the texts after lowering case and
   collapsing whitespace, and change nothing else. A text that appears
   at the same edge on at least `max(2, ceil(pages / 2))` pages is a
   running header or footer, where `pages` counts the pages that
   succeeded and hold a block. Its blocks get kind `page_header` or
   `page_footer` and lose any heading level. A block a reader already
   labeled as furniture keeps its kind.
3. **Repeats.** Among all blocks of kind `page_header` and
   `page_footer`, the ones a reader labeled and the ones step 2 found,
   every occurrence after the first gets `repeated: true`. Here every
   run of digits counts as one placeholder, so "Report, page 3" repeats
   "Report, page 2".

Two rules keep content out of the furniture.

- **Digits are compared as they are in step 2.** Lines that differ by
  a number are different content: "Balance carried forward 1,234.56"
  at the foot of each page of a statement, "Invoice No. 1042" at the
  head of each invoice of a batch, "Step 3" at the head of a slide.
  Digits are folded only in step 3, among blocks that are furniture
  already, where folding can hide nothing else.
- **Position counts when a reader gives one.** A block with a box
  takes part in steps 1 and 2 only when the box lies in the top 12
  percent of the page, for the first block, or the bottom 12 percent,
  for the last. A block with no box, which is every block of a native
  page, has no position to judge by and takes part. A block a reader
  labeled as furniture is taken wherever it sits.

Nothing is deleted. The blocks stay on their pages with their refs. A
caller who disagrees with the pass still has every block, and
`repeated=keep` prints every one. The pass is idempotent: assembling
pages that were assembled changes nothing.

### Tables across pages

When a page's last content block is a table and the next page's first
content block is a table with the same number of columns, the two may
be parts of one table. The column count alone is not enough: a page
that ends with a two-column table of totals and a page that begins
with a two-column table of bank details would be joined. One more sign
is needed, either of:

- the second table's first row repeats the first row of the table's
  first part, compared cell by cell after lowering case and collapsing
  whitespace. A first row with no text is no sign;
- both tables have boxes whose left edges, and whose right edges,
  agree within 3 percent of the page's width.

A caption between the page's edge and the table that contains
"continued" or "cont.", in any case, is passed over, so "Table 3
(continued)" above a table does not hide it. Any other caption before
the second table stands for a table of its own, and nothing is joined.

Runs of two or more are recorded as a Span ([[002-object-model]]) with
the parts' refs, the total rows and the columns. A table that fills
its page keeps the run open; anything after it on the page ends the
run there. Content blocks exclude headers, footers and page numbers,
which is why this pass runs after the previous one. The per-page
tables are not changed.

A table that continues with no repeated header on a page whose reader
gave no boxes is not joined. Joining by comparing the columns' own
positions, or by asking a model, is left out until these rules' errors
are measured on real documents.

### Outline

Headings in page order form the outline: `[{ref, level, text, page}]`.
A reader sees one page, so the depth it gives a heading is a guess made
without the rest of the document, and it drifts: the same reader put
"1 Introduction" at level 1 on one page and "4 Why Self-Attention" at
level 2 on another. A printed section number does not drift. Each
heading first gets a level from what the document says:

| Heading | Level |
|---|---|
| a `title` | 1, whatever its reader said |
| a `heading` that begins with a section number | 1 plus the number's depth: "3 Model" is 2, "3.2 Attention" is 3, "3.2.1 Scaled" is 4 |
| any other `heading` | its reader's level, and at least 2; 2 when its reader gave none |

A section number is digits in groups of at most three joined by dots
("1", "2.3", "3.2.1"), a capital letter followed by such groups
("A.1"), or one capital letter or a Roman numeral with a closing dot
("A.", "IV."); a closing dot is optional on the first two. The number
must be followed by a space and text. A letter with no dot is a word
("A Study"), and four digits are a year. Levels stop at 6.

The distinct levels seen are then mapped to 1, 2, 3 in order. So an
outline has no gap, and a document with no title has its top sections
at level 1. The Markdown view prints each heading at its level in the
outline.

### Views

A view is how a document is rendered when it is read ([[003-api]]). It
is made from the stored page results at that moment and is not stored,
so no option of a submit decides it and no stored copy can be the
wrong one.

| Option | Values | Effect |
|---|---|---|
| `pages` | a selection | limits the rendering to these pages |
| `tables` | `auto`, `markdown`, `html` | how a table is written in Markdown |
| `repeated` | `once`, `keep`, `drop` | `once` prints the first occurrence of a running header or footer, `keep` prints every block, and `drop` prints no header and no footer. A page number is printed only under `keep` |
| `page_breaks` | on or off | marks where each page begins in Markdown, as a comment carrying the page number |

- **Markdown**: headings by their level in the outline, list items,
  tables as Markdown when no cell spans and as HTML when one does
  (`auto`), formulas in math delimiters, code fenced, a caption
  emphasized.
- **Text**: the same reading order as text only, a blank line between
  blocks.

A figure has two things to print, and a view keeps them apart
([[002-object-model]]). Its `description` is what a reader says the
figure shows: the reader's own prose, not the document's. Its `text` is
the words printed inside it. Markdown prints the description as
`*[Figure: <description>]*` and the words after it as text; the text
view prints `[Figure: <description>]` and then the words. A figure
with neither prints nothing.

A page that did not succeed prints nothing in either view, and a block
with nothing to print leaves no blank line.

### Chunks

Chunks are a view too, cut when they are read:

- by `page`: one chunk per page.
- by `section`: a chunk begins at each title or heading and runs to
  the next.

Either way a chunk longer than the caller's `max_chars`, whose default
is `LECTIO_CHUNK_MAX_CHARS` (6,000), is split at block boundaries. The
bound counts characters, not bytes, so a text in a script of several
bytes per character is cut where a caller expects. Each chunk lists
the pages and block refs it covers, so a retrieval hit can be shown on
the page.

A chunk is cited as what the document says, so it holds transcription
and nothing else. Headers, footers and page numbers are left out, and
so is a figure's description; the words printed in a figure are kept.

A heading belongs to what follows it. A chunk that would hold only
headings is not cut off: the next heading or block joins it, so
"Chapter 2" directly above "2.1 Overview" begins one chunk, and a
heading is not split from the block under it because the two together
pass the bound. Only a run of headings that is itself past the bound
is cut.

A block is not split, with one exception. A table longer than the
bound is cut between rows into parts. Each part begins with the
table's header row, which is the rows whose cells are marked as
headers or, when none is, the first row, and each part names the
table's block ref, so a hit in row 300 still says what its columns
are. A row is never cut. Any other single block longer than the bound
is a chunk of its own.

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

- The furniture pass with its three steps, the table pass with its two
  signs and the continued caption, the outline with levels from
  printed section numbers, each deterministic, and the document index.
- The Markdown and text views with every option above, a figure's
  description kept apart from its text, and chunks by page and by
  section with a heading kept with its section and a long table cut by
  its rows.

Remaining:

- Streaming. The passes take every page of the parse at once, which
  the in-process runner can afford and a 3,000-page document on a
  worker cannot.
- The repeated header row of a later part of a span is not marked, so
  a rendering prints it on every part.
- A section chunk ends at the next heading of any level, not at the
  next heading of the same or a higher level.
- `LECTIO_CHUNK_MAX_CHARS`, the server's default for `max_chars`, is
  not read; the default is fixed at 6,000.
- No view reads a span: a table that continues is printed part by
  part.
- Page numbers in Roman numerals, and words for page in languages
  other than the five listed, are not recognized. They stay content.
- A heading with no printed number has only its reader's level. In a
  document that numbers nothing, the outline is as good as the reader.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Assembling the same page results twice writes byte-identical objects | a determinism test |
| A 40-page report with a running title and "Page n of 40", none labeled by a reader, yields 40 header blocks with 39 marked repeated and 40 page numbers; the Markdown prints the title once and no page number | `TestAFortyPageReport` |
| A six-page statement whose pages each end with "Balance carried forward" and a different amount keeps all six as text, and the Markdown prints all six. The same holds for a batch whose pages each begin with a heading "Invoice No." and a different number, which all stay in the outline, and for slides titled "Step 1", "Step 2" | `TestContentThatDiffersByItsNumbersIsNotARunningLine` |
| "3", "Page 3 of 40", "3 / 40", "- 3 -" and their like as a page's first or last block become `page_number`; "1,234.56", "Total 3", a heading "3" and a number in the middle of a page do not. A reader's `page_number` is never rewritten | `TestAPageNumberIsFoundByWhatItSays` |
| A page number is printed only under `repeated=keep`, and is in no chunk | `TestAPageNumberIsPrintedOnlyWhenEverythingIsKept` |
| A line that repeats on every page becomes a running header or footer when its box lies in the top or bottom 12 percent of the page, and stays text when it lies elsewhere | `TestARunningLineSitsAtThePagesEdge` |
| A header a reader labeled keeps its kind, and "Report 2024 · 2" repeats "Report 2024 · 1". Assembling assembled pages changes nothing | `TestAReadersFurnitureIsKeptAndItsRepeatsAreFound`, `TestAssemblyIsDeterministic` |
| A table spanning three pages yields one span of three parts. A two-column table of totals at the end of a page and a two-column table of bank details at the start of the next yield none. A second table under "Table 3 (continued)" joins; one under "Table 4: Spare parts" does not. A second table with no repeated header joins when both boxes share their left and right edges | `TestDocumentJoinsTablesAcrossPages`, `TestATableContinuesOnlyWhenMoreThanItsColumnsAgree` |
| A document whose only heading levels are 2 and 4 has an outline with levels 1 and 2 | `TestOutlineBringsLevelsOntoOneScale` |
| Headings read page by page as "1 Introduction" at 1, "3.1" at 2, "3.3" at 3 and "4 Why Self-Attention" at 2 come out with 3.3 beside 3.1 and 4 beside 1, under the title | `TestAHeadingsDepthComesFromItsPrintedNumber` |
| `section` chunks cover every block of content exactly once; a chunk past the bound is one block, or a heading with the block under it | `TestChunks` |
| Ten characters of two bytes each, twice, are one chunk in a bound of 25. "Chapter 2" directly above "2.1 Overview" begins one chunk | `TestChunksCountCharactersAndKeepAHeadingWithItsSection` |
| A table of nine rows past the bound is cut into parts that each begin with its header row, name its ref and are within the bound; every row is in exactly one part | `TestALongTableIsChunkedByItsRows` |
| A figure's description is printed as `*[Figure: ...]*` in Markdown and `[Figure: ...]` in text, apart from the words printed in it, and is in no chunk | `TestAFiguresDescriptionIsKeptApartFromItsText` |
| Each value of each view option changes the rendering as stated and reads no page again | a table test over one assembled fixture |
| Assembling a 3,000-page document keeps worker heap under 64 MiB | a memory test |
| After one page is retried and succeeds, `assemble` runs again and the document reflects the page | an end-to-end test |
