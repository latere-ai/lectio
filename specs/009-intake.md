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

The first draft of this spec carried three things over without
weighing them, and each let a small crafted file mislead or exhaust a
worker. A PDF's page count was read off the file's bytes and trusted.
An image was decoded at whatever size its header declared. And
conversion ran an office suite inside the worker, with the worker's
network, scheduled through the task queue. The sections below replace
all three.

## Design

### Steps in `prepare`

| Step | What it does |
|---|---|
| snapshot | a URL source is fetched to the object store ([[014-sources-and-retention]]); an uploaded file is already there |
| detect | identify the media type from leading bytes, the package structure of zipped office formats, and container signatures; fall back to the file name, then to the declared type |
| unwrap | open a signed `.p7m` container to the document inside, up to three levels; the signature is not verified |
| convert | legacy word-processing files to the current format; presentations and rich text to PDF |
| extract natively | for formats that carry their structure, produce the pages' blocks now |
| count and select | count pages, a PDF's by the engine that renders it; enforce the limits; apply the `pages` selection |
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

### Counting pages

A PDF's page count comes from the engine that renders its pages. The
engine walks the page tree the way a viewer does, and the pages are
then rendered by the same engine, so the count and the pages cannot
disagree. A count read off the file's bytes can be made wrong in both
directions: an object nothing refers to can claim forty pages in a
file of three, and a count written where a scan of the bytes does not
look turns a file of 160 pages into one of 2. The first becomes
thousands of page tasks that fail, the second a parse that succeeds
with most of the document missing. The scan of the bytes
(`pages.CountPDF`) stands only where a pipeline has no engine, and its
searches are kept from overlapping, so it reads a file at most once
however the file was written.

A TIFF has one page per image file directory. The walk over the
directories stops at 100,000 and refuses the file with
`too_many_pages`: a directory takes six bytes, so a small file can
declare millions.

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

Conversion runs a large third-party program, an office suite, on a
file a caller chose. An office document can name resources outside
itself, an image by its address or a linked file, and the suite
fetches them while it converts. Inside the worker that is a way around
the address check of [[014-sources-and-retention]]: the file, and not
the caller, decides where the worker connects, and the worker holds
the model credential and sits on the private network. So a converter
is held to four rules, whatever implements it:

- It has no network: no route out, by a network namespace or by a
  policy that denies it all traffic.
- Its profile turns off the loading of external resources and the
  running of macros, as a second line behind the first.
- It sees none of the worker's credentials: an empty environment, a
  private scratch directory and profile, no mounted secret.
- It runs under a limit on time and a limit on memory, and is killed
  with its children when it passes either.

Conversion sits behind an interface, as rendering does, and the task
queue knows nothing about it:

```go
package parse

type Converter interface {
    // Convert returns data, of media type from, as media type to.
    Convert(ctx context.Context, data []byte, from, to string) ([]byte, error)
}
```

The implementation is a sidecar: a small service beside the worker
that holds the office suite and answers this one call, in a container
that is given no network. `prepare` calls it like any other step. A
worker with no converter configured refuses the formats that need one
with `unsupported_media_type`.

The first draft scheduled conversion instead: a second server image
that carried the suite, workers that advertised whether they could
convert, and a column the claim query filtered on. That put one
optional dependency into the image set, the task row and the dispatch
query, and it ran the suite in the process that holds the model
credential.

### Rendering a page

Each page task renders the one page it was given.

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
    Blank         bool // every pixel has the same color
}
```

One call renders one page, at the reader's resolution, within its long
edge, in a format it accepts ([[008-readers]]), so a caller never
holds more than one page image for the work it is doing. The image the
caller stores is the image the reader saw. An image that already fits
what the reader asks is passed through byte for byte; one that does
not is scaled down or re-encoded.

**A PDF** is rendered by PDFium compiled to WebAssembly and run
in-process on a WebAssembly runtime written in Go. The worker stays
one static binary with no cgo and no second service to deploy. A PDF
is a program for the engine that draws it, and a caller chose the
file, so the engine is given nothing and bounded in everything:

| Bound | Value | Why |
|---|---|---|
| file system | none | Left to its default, the binding gives the engine the host's whole file system. |
| network, environment | none | The module is given neither. |
| memory per instance | 512 MiB | A file of under two kilobytes, forms that draw forms six levels deep, asked an unbounded engine for about 12 GiB. The bound is the page's bitmap (40 megapixels at four bytes, 160 MB) and room to parse the page and decode its fonts and images. |
| time per page | 30 seconds | A page of a real document renders in milliseconds. |
| bitmap | 40 megapixels | A page may declare any size; past the bound the resolution is lowered until the page fits. |
| instances | 4 at once | Each holds its own memory. |

Three points of the design make the bounds hold:

- An instance renders one page and is discarded. A page that breaks
  the engine, or exhausts it, cannot leave anything behind for the
  next.
- An instance's memory is mapped from the operating system and not
  taken from the Go heap: the bound is reserved as address space and
  made usable as the engine grows into it. Growing copies nothing, and
  the memory goes back when the instance is discarded. On the Go heap
  the same hostile page left several times the bound behind as
  garbage.
- A render does not outlive its context. Each of the engine's slots
  has its own runtime, and ending a slot's context ends the call
  running on it from the inside; the caller returns at once and the
  slot comes back with a fresh engine. The slot is never closed from
  outside while a call runs on it, which would free memory under
  running code.

The engine reads the file through a callback, the blocks it needs and
no more, so opening a 256 MiB file to render one page copies nothing
into the engine. A page that fails to load or to render within the
bounds is `document_corrupt`, and its detail says it is damaged or
needs more memory than a render is given. A PDF that opens only with a
password is `unsupported_media_type`.

On a current laptop a letter page at 160 dpi renders in about 20 ms,
instance start included. Comparing the output against a native
build of the engine on a fixture set is still to do.

**An image** is decoded and, when the reader's description asks, scaled
or re-encoded. An image file states its dimensions in its first bytes
and decoding allocates for what they state: a PNG of 165 KB that
declares 12,000 by 12,000 pixels asks for about 900 MiB. The header is
read first and an image over 40 megapixels is refused with
`file_too_large` before a pixel is decoded. A TIFF frame is held to
the same bound, and is read out of its file through two patches, the
header's first-directory offset and the frame's next-directory
pointer, so taking one frame copies no file.

The renderer takes the file's bytes. In the in-process runner that is
the file as the store holds it. In a durable worker the working copy
is fetched once per worker into a cache directory bounded by
`LECTIO_CACHE_BYTES` (default 2 GiB, least recently used out first)
and mapped, so a page task's memory is still one page image whatever
the document's size.

### Blank pages

A rendered page whose pixels are uniform within a tolerance is written
as a succeeded page with no blocks and no model call. The check reads
every pixel. A sample of points misses a page that holds one short
line, and a page taken for blank is never shown to a reader, so what
it held would be lost without a trace. A page with content ends the
scan at its first mark.

## Not in this spec

A PDF page's own text. Most PDFs carry it, the engine that renders a
page can hand it over with the position of every character, and
reading it costs no model call. Using it, as a reader of its own tried
before any model and as exact text handed to a model, is the largest
cost lever this design has and is the subject of
[[017-agent-driven-parsing]]. This spec still sends every PDF page to a
reader as an image.

Legacy spreadsheets (`.xls`); audio and video; archives; e-mail
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
- `internal/intake/tiffx`: the frames of a TIFF, counted up to the
  bound on directories and decoded one at a time with no copy of the
  file.
- `internal/native`: plain text and Markdown as one page of blocks,
  and CSV as one page holding one table.
- `internal/render`: the interface above; a renderer for PNG, JPEG and
  each frame of a TIFF, with the bound on decoded pixels, scaling,
  re-encoding and the blank check; and the PDF renderer with every
  bound of the table above, which also counts a PDF's pages for
  `prepare`.

Remaining:

- A comparison of the PDF renderer's output against a native build of
  the engine, on a fixture set, kept as a benchmark.
- Native reading of `.docx`, `.xlsx`, `.xlsm`, HTML and XML. They are
  detected and accepted as uploads, and a parse of one fails with
  `unsupported_media_type`.
- Conversion. The step takes a converter and none is built, so the
  formats that need one are refused. The sidecar and its isolation are
  to build together: a converter with a network is not an
  intermediate step.
- The working copy on disk and its cache. The steps hold the file in
  memory.

Known issues in the code that was carried over, each to be fixed with
a test:

- A signed container in BER encoding fails to open. Only DER is read.
- A signed container in PEM form is detected as text.
- `.xls` is routed as a native spreadsheet, though it is not in this
  spec. WebP, `.odt` and `.odp` are in the format table and are not
  detected.
- `prepare` holds the file in memory, and the scan of a PDF's bytes,
  where no engine counts, holds up to 64 MiB of inflated object
  streams beside it.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Every row of the format table has a real fixture that is detected, routed and paged as stated, including a misnamed file and a `.p7m` nested twice | fixture tests |
| `prepare` on a 256 MiB PDF keeps worker heap under 64 MiB, the file mapped from the working copy and not held | a memory test |
| A page task renders page n of a 3,000-page PDF without rendering any other page | a test instrumenting the renderer |
| The in-process renderer's output for the fixture set differs from a native PDFium render by less than a stated pixel tolerance, and renders an A4 page at 160 dpi in under 300 ms on the reference runner | a benchmark |
| A PDF of under two kilobytes that asks the engine for gigabytes is refused with `document_corrupt`, allocates under 128 MiB on the heap while it is refused, and the engine renders the next file | `TestAPageWrittenToExhaustTheEngineFailsWithinItsBounds` |
| A render returns within a second of its context ending, and a page that outlives the time a page is given is `document_corrupt` | `TestARenderStopsWhenItsTimeIsUp` |
| The engine cannot open a file of the host by its path | `TestTheEngineSeesNoFileOfTheHost` |
| A PDF whose bytes claim more pages than its tree holds, and one whose bytes claim fewer, are both prepared with the engine's count | `TestPrepareCountsAPDFsPagesWithTheEngineThatRendersThem` |
| Counting a 2 MiB file of unclosed object streams from its bytes takes under a second | `TestCountPDFIsNotQuadraticInUnclosedStreams` |
| An image file of a few dozen bytes that declares 900 megapixels is refused with `file_too_large` and allocates nothing for its pixels | `TestAnImageThatDeclaresAHugeSizeIsRefusedFromItsHeader` |
| A TIFF of more than 100,000 directories is refused with `too_many_pages`, and reading one frame of a 16 MiB file allocates no copy of it | `TestCountFramesStopsAtABoundOnDirectories`, `TestFramePNGDoesNotCopyTheFile` |
| A page that holds one short word is not blank | `TestAPageWithOneSmallMarkIsNotBlank` |
| A converter has no route to any address, the worker's own included, and a document that names an external resource converts without fetching it | a test with a converter in a container and a counting server |
| A deployment with no converter fails a parse that needs one with `unsupported_media_type` at `prepare` | a pipeline test |
| A conversion that exceeds its time or its memory limit is killed with its children and leaves no file in the scratch directory | a test with a stub converter |
| A blank page costs no reader call | a test with a counting stub reader |
