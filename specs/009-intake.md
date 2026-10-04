---
title: "Intake: detect the type, unwrap, convert, extract natively, count and render pages, and hand over a page's own text"
status: validated
track: core
depends_on:
  - specs/002-object-model.md
  - specs/005-parse-graph.md
affects: [internal/intake/, internal/native/, internal/render/]
effort: large
created: 2026-10-03
updated: 2026-10-04
author: changkun
---

# Intake

## Overview

Intake is everything that happens to a file before a reader sees a
page. It decides what the file is, turns it into something with pages,
reads the pages that need no reader, and produces what a reader is
given of each page that does: its image, and for a page of a PDF the
text the file carries for it. It runs in 2 places: the `prepare`
task does the document-wide steps once, and each page task renders its
own page ([[005-parse-graph]]).

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
| PDF | rendered page by page, each page with the text the file carries for it | `reader`, or `text_layer` |
| JPEG, PNG, WebP | one page | `reader` |
| TIFF | one page per frame | `reader` |
| `.docx` | native: one page of blocks, tables kept as tables | `native` |
| `.xlsx`, `.xlsm`, `.csv` | native: one page per sheet, each a table | `native` |
| text, Markdown, HTML, XML | native | `native` |
| `.doc` | converted to `.docx`, then native | `native` |
| `.pptx`, `.ppt`, `.rtf`, `.key`, `.odt`, `.odp` | converted to PDF, then as a PDF | `reader`, or `text_layer` |
| `.p7m` | unwrapped, then as the document inside | either |
| `.xls` | refused with `unsupported_media_type`, with a detail that says to save the workbook as `.xlsx` | |
| anything else | refused with `unsupported_media_type` | |

A `native` page task copies blocks that `prepare` already produced and
calls no model. Its blocks have `box: null` ([[002-object-model]]).

A page of a PDF is read by a reader of the routing policy's chain
([[008-readers]]). Which one is the chain's to say, page by page: a
chain that begins with a reader of the `text` adapter reads a page from
the text the file carries for it, with no model call, and the page's
source is `text_layer`; a page that reader declines, a scan or a page
that is mostly a figure, goes to the next reader and its source is
`reader`. The file's manifest says `reader` either way: it says that
the pages are read one by one, and each page says by what. Intake's
part is to hand the page's own text over, under bounds, which is
[A page's own text](#a-pages-own-text) below.

### Reading a zipped office package

A `.docx`, `.xlsx` or `.xlsm` file is a ZIP of XML parts, and the
worker reads it itself, with the standard library and no office suite.
Such a file is small and says how large it becomes: its directory says
how many parts it holds and how far each inflates, its markup says how
deep it nests, and a sheet says how many cells it has. Each statement is
checked against a bound before anything is allocated for it, and the
reading is held to the same bound, because a statement can be false.

| Bound | Value | Past it |
|---|---|---|
| bytes read to list the package's entries | 4 MiB | `file_too_large` |
| entries | 10,000 | `file_too_large` |
| one part, by the size its entry declares | 64 MiB | `file_too_large` |
| all parts that are read, by the sizes their entries declare | 128 MiB | `file_too_large` |
| a part that inflates past the size its entry declares | | `document_corrupt` |
| elements open at once | 256 | `document_corrupt` |
| tokens of markup, over all parts that are read | 33,554,432 | `file_too_large` |
| bytes from one opening angle bracket to the next: one tag, or one run of text | 1 MiB | `document_corrupt` |
| a document type declaration | none | `document_corrupt` |
| blocks on one page | 500,000 | `file_too_large` |
| cells over all tables of a file, as rows times columns | 1,048,576 | `file_too_large` |
| cells a sheet declares in its dimension | the same | `file_too_large` |
| bytes of cell text over all sheets | 32 MiB | `file_too_large` |
| shared strings a workbook declares | what its part's bytes can hold | `document_corrupt` |
| sheets | `LECTIO_MAX_PAGES`, and never more than the entries | `too_many_pages` |
| paragraph styles, cell formats | 65,536 | `file_too_large` |

A size is `file_too_large` and a file that contradicts itself or its
format is `document_corrupt`, as for an image and a PDF. 4 points make
the bounds hold:

- The directory is read through a budget. The archive reader holds
  every entry it lists, at several times the entry's bytes, so a
  directory of 500,000 entries is refused once the budget is
  spent and is never held whole.
- A part is refused for the size its entry declares, before a byte is
  inflated, and a part that inflates past that size is refused as it is
  read. So what is declared bounds what is inflated.
- A part is read as a stream of tokens and never held whole. The
  decoder defines no entity from a document type and fetches nothing one
  names; a part that declares one is refused outright, since the format
  forbids it. A start tag is decoded whole, one value per attribute,
  which is why the distance from one bracket to the next is bounded.
- Nothing is read through a relationship but a part of the same
  package. A target that says it is external, and one whose path climbs
  out of the package, are passed over. An error's detail names a part
  by its role, never by a name the file supplied.

Every part is read to its end, where the archive checks its bytes
against their checksum.

A `.docx` is one page of blocks in document order:

- A paragraph is a `title`, a `heading` with its level, a `caption`, a
  `list_item` or `text`. The paragraph's style decides, by the name the
  styles part gives it and not by its id, which a localized producer
  translates; then the outline level the paragraph or its style sets;
  then whether it is in a list. A style inherits from the style it is
  based on. A numbered heading is a heading.
- A table is a `table` block with its cells: a cell that spans columns
  (`gridSpan`) or rows (`vMerge`) carries the span, and the cells of a
  row marked to repeat as a header (`tblHeader`) are header cells. A
  table inside a cell is read into the cell's text. A raised or lowered
  run in a cell is marked with `^` or `_` ([[002-object-model]]).
- A picture, a chart, a diagram or an embedded object is a `figure`
  block with no text. Its bytes are not extracted, so it has no image
  and cannot be described. The paragraphs of a text box follow the
  paragraph the box is anchored in.
- A footnote or an endnote the body refers to is a `footnote` block at
  the end of the page, in the order of the references.
- Text is the text of the runs: the text of a link, a tab as a tab, a
  line break as a newline, the result of a field and not its code.
  Tracked changes are read as accepted: what was inserted is text and
  what was deleted is not.

Not read: headers and footers, comments, the numbers and bullets a list
shows, text in a drawing shape that is not a text box, symbol-font
characters, and the picture inside a table cell beyond the fact of it. A
document formatted by hand, with large bold lines in place of heading
styles, has no headings to read.

A workbook is one page per sheet, in the workbook's order: a `title`
block with the sheet's name, and the sheet's cells as one `table`.

- Every sheet the workbook lists is a page, a hidden one included. What
  a sheet holds is content whether or not its tab is shown, and a result
  that left it out would say nothing was there. The sheets are counted
  from the workbook part, so a workbook over the limit on pages is
  refused before a sheet is opened.
- The table is the rectangle around the cells that hold a value, with
  every position in it present, so a row reads in columns whatever the
  sheet left empty. A merged range is one cell with its span. The size a
  sheet declares for itself, and the number of strings a workbook
  declares, are checked and used for nothing: no memory is taken for
  either, and the cells and strings are what the bytes hold.
- A cell's text is a shared or an inline string, `TRUE` or `FALSE`, an
  error as the sheet shows it, or a number. A formula is never
  evaluated: the cell holds the value the workbook stored beside it, and
  no text when it stored none.
- A number is written as the shortest decimal that is the same number,
  which is `2.3` for a stored `2.2999999999999998`. Its format is read
  for 2 things only. A date or time format makes it an ISO 8601 date,
  time, or both, with seconds only when the format shows them. A
  percent format makes it the number times 100 with a percent sign.
  Separators, currency signs, rounding and padding a format would add
  are not applied.
- A cell that names a shared string costs a few bytes in the file and
  the whole string in the result, which is what the bound on cell text
  is for.
- A sheet that is one chart is a page with its title and a `figure`.

Not read from a workbook: charts, pictures and pivot tables on a sheet,
comments, data validation, conditional formats, defined names, the
header rows of a structured table, and the macros of an `.xlsm`, whose
part is never opened. The locale-dependent built-in formats 27 to 36 and
50 to 58 are read as dates, and 32 to 35 as times.

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

The sidecar is `lectio-convert` ([[016-distribution]]), and the call is
one HTTP request: a POST of the file to `/v1/convert`, with the file's
media type as `Content-Type` and the type wanted as `Accept`. It
converts 7 pairs and no other: `.doc` to `.docx`, and `.pptx`,
`.ppt`, `.odp`, `.key`, `.rtf` and `.odt` to PDF. The answer is the
conversion, or a status with a body of `{"error": {"code", "detail"}}`:

| Answer | Meaning | The parse fails with |
|---|---|---|
| `200` | the conversion, as the type asked for | |
| `415` | the sidecar does not convert that pair | `unsupported_media_type` |
| `413` | the file, or its conversion, is over `LECTIO_MAX_FILE_BYTES` | `file_too_large` |
| `422` | the suite did not convert the file within its time and its memory | `document_corrupt` |
| any other status, no answer, or none within 5 minutes | the sidecar failed | `internal` |

The first 3 are the file's, as a page that does not render within
its bounds is. The last is not the file's and not its caller's. The
client follows no redirect, so a sidecar cannot send a file on, and it
holds a conversion to the size a file is held to. `prepare` then reads
the conversion as it reads any file, so a conversion that is not the
type it was asked for fails as a corrupt file does.

What the sidecar does for the 4 rules, and what it leaves to whoever
runs it:

- **Network.** The sidecar cannot take the network from itself. It is
  run in a container with no network, listening on a socket in a
  directory it shares with the worker
  (`LECTIO_CONVERT_ADDR=unix:/run/lectio/convert.sock`, and the worker's
  `LECTIO_CONVERTER_URL=unix:///run/lectio/convert.sock`), which leaves
  it no route to any address, the worker's included. Where containers
  of one pod share a network, it is a pod of its own under a policy
  that denies it all egress and admits the workers' calls.
- **Profile.** Each conversion gets a profile of its own, written
  before the suite starts: macro execution disabled, the macro security
  level at its highest, links out of a document blocked, and links not
  updated on load. The filter that reads the file is named for its
  type, so the suite does not choose one from what the bytes look like.
- **Credentials.** The sidecar holds none and is given none. The suite
  is started with an environment of 2 variables, its home and its
  directory for temporary files, both inside the conversion's scratch
  directory. The scratch directory holds the file, the profile and
  everything the suite writes, and is removed when the conversion ends,
  however it ends, with the socket the suite leaves outside it when it
  is killed.
- **Time and memory.** One conversion runs at a time, and a call that
  waits holds a connection and no part of its file. The suite runs in a
  process group of its own. Past `LECTIO_CONVERT_TIMEOUT` (2 minutes)
  the whole group is killed. `LECTIO_CONVERT_MEMORY_BYTES` (4 GiB)
  bounds the address space of the suite and of everything it starts:
  the sidecar's own program sets the limit on itself and then becomes
  the suite, so the system enforces it, an allocation past it fails,
  and the suite ends. That is a bound on address space. The bound on
  the memory a container holds is the container's own limit, which the
  deployment sets.

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

### A page's own text

A typeset PDF holds the text of each page: every character with the
Unicode it maps to, where it is drawn and in what font. The engine that
renders a page reads that too, with no model and no bitmap. A reader
that asks for it in its description is handed it beside the page's
image ([[008-readers]]).

```go
package parse

// A renderer that holds a PDF engine is one.
type PDFTexter interface {
    TextPDF(ctx context.Context, data []byte, n int) (reader.PageText, error)
}
```

```go
package reader

type PageText struct {
    Width, Height float64 // the page's size in points, as it is shown
    Words    []Word       // in the order the file draws them
    Rects    []Rect       // the upright rectangles the page paints
    Drawings []Rect       // the boxes around everything else it paints
    Partial  bool         // the page holds more than is read of one page
}

type Word struct {
    Text         string  // as the file maps it to Unicode; U+FFFD where it maps to nothing
    Box          Rect    // across: its glyphs; down: the line of type, ascent to descent
    Baseline     float64
    Size         float64 // of its type, in points, as drawn
    Bold, Italic bool    // what the font's name and flags say
    Hidden       bool    // drawn with no fill and no stroke
    Turned       bool    // does not run from left to right along the page
    Unmapped     int     // characters the file maps to no Unicode character
}

type Rect struct{ X0, Y0, X1, Y1 float64 }
```

What each member is, and why a reader needs it:

- **One coordinate space.** Every position is in points from the top
  left of the page as it is shown, after the page's rotation and its
  crop. The engine is asked where 3 points of the page's own space land
  on the page it draws, and the map is taken from its answers, so a
  position lies where the page's image shows it, to a hundredth of a
  point. A position over the page's size is a block's box
  ([[002-object-model]]).
- **Words.** A word is the characters between 2 pieces of white space.
  The engine adds a space where the file leaves a gap and writes none,
  and a character that is not drawn beside the one before it begins a
  word of its own. A word's box spans its glyphs across the page and the
  line of type down it, so the words of one line share their extent
  whatever letters they hold. Its size is the size it is drawn at: the
  size the font is set at times what the matrix it is placed with makes
  of it. A word the page's crop leaves outside is not handed over.
- **Type.** Bold and italic come from the font's name and the flags of
  its descriptor. A font's weight as a number is not read: a file that
  states none has one derived from the width of the font's stems, which
  puts a regular serif face above a bold sans one.
- **What cannot be read as text.** `Unmapped` counts the characters a
  font gives no Unicode for, or the engine reports as mapped wrongly.
  `Hidden` marks text that is in the file and not on the page, which is
  what a recognition pass lays under a scan. `Turned` marks text set at
  an angle.
- **What the page paints beside its text.** A path made of upright lines
  alone is reported as rectangles: a filled rectangle as it is, a
  stroked line as a rectangle as thick as its stroke, a frame as its 4
  sides. That is what ruling and backgrounds are made of, and a reader
  finds a table's cells in them. Everything else, an image, a shading, a
  curve, a slanted line, a filled shape that is no rectangle, a path of
  more than 16 segments, is reported as the box around it: a reader sees
  from those how much of the page is picture. A fill or a stroke in the
  paper's color, and a path that only cuts a clip, paint nothing and are
  left out. Shapes inside a form are placed by the form's own matrix.

The text is read by an engine instance of its own, under every bound a
render is held to. 4 more bound what is copied out of the engine and
how many calls are made into it:

| Bound | Value | Past it |
|---|---|---|
| characters read of one page | 50,000 | the reading stops and the page is `Partial` |
| painted objects visited on one page, inside forms too | 50,000 | the walk stops and the page is `Partial` |
| forms inside forms | 8 deep | the form is one drawing |
| segments read of one path | 16 | the path is one drawing |
| memory, time | as a render: 512 MiB, 30 seconds | `document_corrupt` |

A dense page of print holds under 10,000 characters and paints a few
thousand objects. A page of 50,000 characters is read in under a second
and one of 50,000 objects in under 3. A page that is `Partial` was
written to be drawn many times over; a reader that needs the whole page
declines it. A page whose text the engine cannot get through within its
memory and its time is not a failed page, since it rendered: the
pipeline hands the reader no text, a reader that needs it declines the
page, and one that reads the image reads it.

Not read: text an annotation or a form field shows, which the renderer
does not draw either; whether a clip or a shape painted over a word
hides it; and the document's structure tree, which says what a tagged
PDF's author meant each run of text to be.

### Blank pages

A rendered page whose pixels are uniform within a tolerance is written
as a succeeded page with no blocks and no model call. The check reads
every pixel. A sample of points misses a page that holds one short
line, and a page taken for blank is never shown to a reader, so what
it held would be lost without a trace. A page with content ends the
scan at its first mark.

## Not in this spec

A PDF page's own text as exact text handed to a model that reads the
page's image, so that the model transcribes nothing and only says what
each run of text is. The text is handed to any reader that asks for
it; no reader that calls a model asks yet
([[017-agent-driven-parsing]]).

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
  CSV as one page holding one table, `.docx` as one page of blocks, and
  `.xlsx` and `.xlsm` as one page per sheet, each read from the package
  under the bounds above.
- `internal/convert`, `cmd/lectio-convert` and `deploy/converter`: the
  client a pipeline converts through, the sidecar that answers it, and
  the sidecar's image, with the suite, as a user that is not root. Run
  by hand, with no network, on a socket: all 7 conversions through
  LibreOffice 25.2, a conversion past its time limit killed with
  nothing left running or on disk, and a suite under a memory limit too
  small for it refused. `make live-convert` repeats the conversions
  against a running sidecar.
- `internal/render`: the interface above; a renderer for PNG, JPEG and
  each frame of a TIFF, with the bound on decoded pixels, scaling,
  re-encoding and the blank check; and the PDF renderer with every
  bound of the table above, which also counts a PDF's pages for
  `prepare`.
- A page's own text: `TextPDF` of the PDF renderer, with its 4 bounds,
  and `ReadPage` of `internal/parse` handing it to a reader that asks.

Remaining:

- A comparison of the PDF renderer's output against a native build of
  the engine, on a fixture set, kept as a benchmark.
- Native reading of HTML and XML. They are detected and accepted as
  uploads, and a parse of one fails with `unsupported_media_type`.
- The bytes of a picture in a `.docx`. A figure of a native page has no
  image.
- The denial of the network to the sidecar, as something the
  repository ships and tests. The image and the sidecar are built, and
  run with no network when they are told to; the manifests that tell
  them to (`deploy/`, [[016-distribution]]) are not, and no test in the
  gate runs a container. The test with a counting server is not built
  either: run by hand with a network, 3 documents that name an
  outside resource were converted by this suite without one fetch, with
  the profile and without it, so the profile's effect has not been
  observed.
- The memory limit under a real suite in the gate. The gate proves the
  limit is set on the suite's process; that the system then enforces it
  was checked by hand in the image.
- Detecting a legacy office file by its content. A `.doc` and a `.ppt`
  are compound files, told apart by their name or declared type.
- The working copy on disk and its cache. The steps hold the file in
  memory.

Known issues in the code that was carried over, each to be fixed with
a test:

- A signed container in BER encoding fails to open. Only DER is read.
- A signed container in PEM form is detected as text.
- WebP is in the format table and is not detected.
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
| A package whose entry declares more than a part may inflate to is refused with `file_too_large` before it is inflated, and one whose entry inflates past what it declares is refused with `document_corrupt` within 8 MiB of allocation | `TestAPartThatDeclaresMoreThanItsBoundIsRefusedBeforeItIsInflated`, `TestAPartThatInflatesPastWhatItDeclaresIsRefused` |
| A package of more than 10,000 entries is refused, and a directory of 500,000 entries is refused within 64 MiB of allocation | `TestAPackageOfTooManyEntriesIsRefused` |
| Markup nested past 256 elements, a tag of over 1 MiB, and tokens past their bound are each refused | `TestMarkupIsHeldToItsBounds` |
| A part that declares a document type or an entity is refused, and nothing an entity names is read | `TestAPartThatDeclaresAnEntityIsRefused` |
| A relationship whose target is outside the package is not followed, whatever the archive holds under that name | `TestARelationshipThatLeavesThePackageIsNotFollowed` |
| A `.docx` keeps its headings with their levels, its list items, its tables with spans and header rows, its footnotes, and a figure for each picture | `TestAParagraphIsWhatItsStyleSays`, `TestATableKeepsItsCellsSpansAndHeaderRows`, `TestFiguresTextBoxesAndNotes` |
| A sheet that declares every cell a sheet can have, 2 cells at opposite corners of one, a count of shared strings the part cannot hold, and one long string named by many cells are each refused within 16 MiB of allocation | `TestWhatASheetDeclaresIsNeverAllocatedFor` |
| A workbook that lists more sheets than the limit on pages is refused with `too_many_pages` before a sheet is opened | `TestAWorkbookOverThePageLimitIsRefusedBeforeASheetIsRead` |
| A workbook is one page per sheet with its merged cells as spans, its dates in ISO 8601, and its formulas as their stored values | `TestAWorkbookIsOnePagePerSheet`, `TestRender` |
| Each office format read natively has a generated fixture and one an office suite wrote back out, and each comes out of `prepare` with the stated pages, blocks of each kind, table shape, spanned cells and texts | `TestOfficeFormatsAreReadFromTheirOwnStructure` |
| A converter has no route to any address, the worker's own included, and a document that names an external resource converts without fetching it | a test with a converter in a container and a counting server |
| A deployment with no converter fails a parse that needs one with `unsupported_media_type` at `prepare`, and one with a converter reads the conversion's pages | `TestTheDevServerConvertsThroughAConverter` |
| A conversion that exceeds its time limit is killed with its children and leaves no file in the scratch directory, and the suite runs under the memory limit it is given | `TestAConversionPastItsTimeLimitIsKilledWithItsChildren`, `TestTheSuiteRunsUnderTheMemoryLimit` |
| The suite is started with the filter for the file's type, a profile that turns macros and links off, and an environment that holds nothing of the sidecar's | `TestTheSuiteIsGivenNothingOfTheSidecar` |
| The client maps each answer of the sidecar to its code, follows no redirect, does not wait past its time limit, and refuses a conversion over the size limit | `TestTheClientMapsWhatTheSidecarAnswers`, `TestTheClientDoesNotWaitPastItsTimeLimit` |
| One conversion runs at a time, and a call that stops waiting was never started | `TestOneConversionRunsAtATime` |
| Every type the detector routes to conversion is one the sidecar converts, and a real suite converts the fixture of each | `TestEveryConvertedTypeHasAConversion`; `TestLiveConverter`, run by hand |
| A blank page costs no reader call | a test with a counting stub reader |
| A word of a page's own text comes with its place, its baseline, the size it is drawn at and what its font says of it, and text that is hidden, turned or mapped to no Unicode character says so | `TestAPagesWordsComeWithTheirPlaceAndTheirType`, `TestCharactersWithNoMappingAreCountedAndWordsOffThePageAreLeftOut` |
| Every mark on a page's rendering lies in a word, a rectangle or a drawing of its text, and each of those has a mark in it, for a page turned by each quarter, cropped, and with a box that does not begin at 0 | `TestAPositionLiesWhereTheRenderingShowsIt` |
| A filled rectangle, a stroked line and a frame are rectangles; a curve, a triangle, an image and a path of more than 16 segments are drawings; a fill in the paper's color and a clip are nothing; a shape in a form lies where the form puts it | `TestWhatAPagePaintsIsRectanglesAndDrawings`, `TestFormsAreWalkedToADepth` |
| A file of under 4 kilobytes whose page holds 200,000 characters, and one of under 8 whose page paints 60,600 objects, each come back `Partial` with no more than the bound read | `TestAPageOfMoreCharactersThanTheBoundIsPartial`, `TestAPageOfMoreObjectsThanTheBoundIsPartial`, with the bounds set to 600 and 150 |
| The text of the page of under 2 kilobytes that asks the engine for gigabytes is refused with `document_corrupt`, allocates under 128 MiB on the heap, returns within a second of its context ending or its time running out, and the engine reads the next page | `TestThePageWrittenToExhaustTheEngineHasItsTextRefusedWithinTheBounds` |
| A reader that asks for a page's own text is handed it beside the image; one that does not ask, a format that carries none, and a page whose text could not be read within the bounds get none | `TestAReaderThatAsksIsHandedThePagesOwnText` |
