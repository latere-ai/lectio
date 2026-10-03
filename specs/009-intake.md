---
title: "Intake: detect the type, unwrap, convert, extract natively, count and render pages"
status: drafted
track: core
depends_on:
  - specs/002-object-model.md
  - specs/005-parse-graph.md
affects: [internal/intake/, internal/native/, internal/render/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Intake

## Overview

Intake is everything that happens to a file before a model sees a
page. It decides what the file is, turns it into something with pages,
reads the pages that need no model, and produces the image of each
page that does. It runs in two places: the `prepare` task does the
document-wide steps once, and each page task renders its own page
([[005-parse-graph]]).

## Current state

The earlier service's intake is the most complete part of it and is
carried over nearly whole: type detection by content, unwrapping of
signed containers, conversion of office formats through LibreOffice,
native extraction for word-processing documents, spreadsheets, CSV,
text, HTML, XML and Markdown, page counting, page-range selection, and
the decision between native and model reading. Those packages are pure
Go with fixture tests. Two things change. They held the whole file in
memory and now read from a local working copy. And PDF pages were
rendered by a separate service in another language, which this spec
replaces with rendering inside the worker.

## Design

### Steps in `prepare`

| Step | What it does |
|---|---|
| snapshot | a URL source is fetched to the object store ([[014-sources-and-retention]]); an uploaded file is already there |
| detect | identify the media type from leading bytes, the package structure of zipped office formats, and container signatures; fall back to the file name, then to the declared type |
| unwrap | open a signed `.p7m` container to the document inside, up to three levels; the signature is not verified |
| convert | legacy word-processing files to the current format; presentations and rich text to PDF |
| extract natively | for formats that carry their structure, produce the pages' blocks now |
| count and select | count pages, enforce the limits, apply the `pages` selection |
| write | the working copy, the manifest, the native page results, the tasks |

Limits, each overridable downward by the allow
([[013-limits-and-usage]]): `LECTIO_MAX_FILE_BYTES` 256 MiB,
`LECTIO_MAX_PAGES` 3,000. The page limit is on the document, counted
before the selection, so selecting three pages of a document over the
limit is still refused with `too_many_pages`.

### Selecting pages

A selection is ranges and numbers separated by commas, counted from 1,
such as `1-3,7`. An open range such as `5-` runs to the last page. A
range needs its start, so `-3` is malformed. The selection is clamped
to the document: a page past the end is dropped, and a range that runs
past the end stops at the last page. Its syntax can be checked with no
document, which is what a submit does ([[003-api]]); whether it names
any page of the document is known once the pages are counted, and a
selection that names none is `invalid_pages` on the parse.

### What reads each format

| Input | Path | Page source |
|---|---|---|
| PDF | rendered page by page | `reader` |
| JPEG, PNG, WebP | one page | `reader` |
| TIFF | one page per frame | `reader` |
| `.docx` | native: one page of blocks, tables kept as tables | `native` |
| `.xlsx`, `.xlsm`, `.csv` | native: one page per sheet, each a table | `native` |
| text, Markdown, HTML, XML | native | `native` |
| `.doc` | converted to `.docx`, then native | `native` |
| `.pptx`, `.ppt`, `.rtf`, `.key`, `.odt`, `.odp` | converted to PDF, then rendered | `reader` |
| `.p7m` | unwrapped, then as the document inside | either |
| anything else | refused with `unsupported_media_type` | |

A `native` page task copies blocks that `prepare` already produced and
calls no model. Its blocks have `box: null` ([[002-object-model]]).

A PDF is always read by a reader in the first version, including one
with a text layer. Reading the text layer directly would be cheaper
and exact for the text, and gives no layout, no tables and no reading
order across columns, which are what a caller comes for. Using the
text layer to check or correct the reader's transcription is noted
under Not in this spec.

### Conversion

Conversion runs LibreOffice as a child process, one at a time per
worker by default, in a private scratch directory with a private
profile and an empty environment, with a time limit. It is optional
at build and at run: the default image contains no office suite and
refuses the formats that need one with `unsupported_media_type`; the
`-office` image contains it ([[016-distribution]]). A worker advertises
whether it can convert, and `prepare` for a file that needs conversion
is claimed only by a worker that can, through a `needs` column the
claim query filters on.

### Rendering a page

Each page task renders the one page it was given.

- PDF: by PDFium compiled to WebAssembly and run in-process on a
  WebAssembly runtime written in Go. That keeps the worker one static
  binary with no cgo and no second service to deploy, scale and keep
  warm. This choice is to be confirmed by a spike before the spec is
  validated: render the fixture set, compare against a native PDFium
  build, and measure time and memory per page. The interface below is
  what the rest of the system depends on, so if the spike fails, the
  fallback is a rendering sidecar behind the same interface and
  nothing else changes.
- TIFF and other images: decoded and re-encoded in process.

```go
package render

type Renderer interface {
    // Render returns page n, counted from 1, of a file of the given media
    // type, prepared the way the reader's description asks.
    Render(ctx context.Context, data []byte, mediaType string, n int, want reader.Description) (Image, error)
}

type Image struct {
    Data          []byte
    MediaType     string
    Width, Height int  // pixels
    Blank         bool // every sampled pixel has the same color
}
```

One call renders one page, at the reader's resolution, within its long
edge, in a format it accepts ([[008-readers]]), so a caller never
holds more than one page image for the work it is doing. The image the
caller stores is the image the reader saw. An image that already fits
what the reader asks is passed through byte for byte; one that does
not is scaled down or re-encoded.

The interface takes the file's bytes because the first implementation
reads images, which are small. The PDF renderer opens the working copy
from disk: the copy is fetched once per worker into a cache directory
bounded by `LECTIO_CACHE_BYTES` (default 2 GiB, least recently used
out first), so a page task's memory is one page image regardless of
the document's size, and the interface changes to take the local path
when that renderer lands.

### Blank pages

A rendered page whose pixels are uniform within a tolerance is written
as a succeeded page with no blocks and no model call. The check
samples a grid of points across the image and does not read every
pixel.

## Not in this spec

Reading a PDF's text layer to verify or replace a transcription;
legacy spreadsheets (`.xls`); audio and video; archives; e-mail
messages with attachments; recovering a damaged PDF. Optical cleanup
of scans. Each is a later spec.

## Implementation status

Built:

- `internal/intake/detect`: type detection by content, package
  structure and container signature, then file name, then declared
  type, and the route each type takes.
- `internal/intake/unwrap`: a DER-encoded signed container opened to
  the document inside, up to three levels, the signature not verified.
- `internal/intake/pages`: page counting for PDF and TIFF, the limits,
  the selection with open ranges and clamping, and the syntax check a
  submit uses.
- `internal/intake/tiffx`: the frames of a TIFF, counted and decoded
  one at a time.
- `internal/native`: plain text and Markdown as one page of blocks,
  and CSV as one page holding one table.
- `internal/render`: the interface above and a renderer for PNG, JPEG
  and each frame of a TIFF, with scaling, re-encoding and the blank
  check.

Remaining:

- PDF rendering. A PDF is detected, counted and selected, and then
  every page fails with `unsupported_media_type`, because no renderer
  for it is built. The PDFium plan above stands and is the first thing
  to build.
- Native reading of `.docx`, `.xlsx`, `.xlsm`, HTML and XML. They are
  detected and accepted as uploads, and a parse of one fails with
  `unsupported_media_type`.
- Conversion. The step takes a converter and none is built, so the
  formats that need one are refused.
- The working copy on disk and its cache. The steps hold the file in
  memory.

Known issues in the code that was carried over, each to be fixed with
a test:

- A PDF's page count is read from its `/Count` and trusted. A file
  that lies about it is not caught until a page fails to render.
- A signed container in BER encoding fails to open. Only DER is read.
- A signed container in PEM form is detected as text.
- `.xls` is routed as a native spreadsheet, though it is not in this
  spec. WebP, `.odt` and `.odp` are in the format table and are not
  detected.
- The walk over a TIFF's directories has no cap on their number.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Every row of the format table has a real fixture that is detected, routed and paged as stated, including a misnamed file and a `.p7m` nested twice | fixture tests |
| `prepare` on a 256 MiB PDF keeps worker heap under 64 MiB | a memory test |
| A page task renders page n of a 3,000-page PDF without rendering any other page | a test instrumenting the renderer |
| The in-process renderer's output for the fixture set differs from a native PDFium render by less than a stated pixel tolerance, and renders an A4 page at 160 dpi in under 300 ms on the reference runner | the spike, kept as a benchmark |
| A worker without the office suite never claims a `prepare` that needs conversion, and a deployment with no such worker fails the parse with `unsupported_media_type` instead of leaving it queued | a dispatch test |
| A conversion that exceeds its time limit is killed with its children and leaves no file in the scratch directory | a test with a stub converter |
| A blank page costs no reader call | a test with a counting stub reader |
