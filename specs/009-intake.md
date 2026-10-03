---
title: "Intake: detect the type, unwrap, convert, extract natively, count and render pages"
status: drafted
track: core
depends_on:
  - specs/002-object-model.md
  - specs/005-parse-graph.md
affects: [internal/intake/, internal/render/]
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
| count and select | count pages, apply the `pages` selection, enforce the limits |
| write | the working copy, the manifest, the native page results, the tasks |

Limits, each overridable downward by the allow
([[013-limits-and-usage]]): `LECTIO_MAX_FILE_BYTES` 256 MiB,
`LECTIO_MAX_PAGES` 3,000.

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
type Renderer interface {
    Open(path string) (Doc, error)      // the local working copy
}
type Doc interface {
    Pages() int
    Size(page int) (width, height float64, rotation int)
    Render(page int, dpi int, longEdge int) (image.Image, error)
    Close() error
}
```

The working copy is fetched once per worker into a cache directory
bounded by `LECTIO_CACHE_BYTES` (default 2 GiB, least recently used
out first) and opened from disk, so a page task's memory is one page
image regardless of the document's size.

### Blank pages

A rendered page whose pixels are uniform within a tolerance is written
as a succeeded page with no blocks and no model call.

## Not in this spec

Reading a PDF's text layer to verify or replace a transcription;
legacy spreadsheets (`.xls`); audio and video; archives; e-mail
messages with attachments; recovering a damaged PDF. Optical cleanup
of scans. Each is a later spec.

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
