# Quality

How well Lectio reads a file is measured, not judged by eye. The
repository holds a small corpus of files whose content is known block by
block, 5 measures that compare what a parse returned with that content,
and a bar for each measure. One test holds the parts that need no model
to those bars on every push. One command reads the same corpus with the
reader you configure and writes the numbers down.

The bars are bars for this corpus. They say that a change made reading
worse, or that a reader is not fit for clean pages. They are no claim
about any other document.

## The corpus

The files are in `internal/testfixtures`, each beside its truth: the
pages of the file and, on each page, the blocks in reading order with
their kind, their text, the cells of a table, and the box of a block
where the layout is known. Every file was written for this repository
and every name and number in it is invented.
[`internal/testfixtures/README.md`](../internal/testfixtures/README.md)
says how each file was made and how to make it again.

| File | What it is | Class | Pages | Read by |
|---|---|---|---:|---|
| `plain.txt` | plain text | `exact` | 1 | the file itself |
| `notes.md` | Markdown | `exact` | 1 | the file itself |
| `readings.csv` | CSV | `exact` | 1 | the file itself |
| `report.docx` | Word document | `exact` | 1 | the file itself |
| `ledger.xlsx` | workbook, 3 sheets | `exact` | 3 | the file itself |
| `report.doc` | Word 97 document, converted | `converted` | 1 | the converter, then the conversion itself |
| `slides.pptx` | presentation, converted | `converted` | 3 | the converter, then a reader |
| `memo.rtf` | rich text, converted | `converted` | 1 | the converter, then a reader |
| `survey.pdf` | typeset PDF | `typeset` | 4 | a reader |
| `survey-scan.pdf` | scanned PDF | `scan` | 4 | a reader |
| `survey-1.png` | PNG of a page | `scan` | 1 | a reader |
| `survey-1.jpg` | JPEG of a page | `scan` | 1 | a reader |
| `survey-2-4.tiff` | TIFF of 3 frames | `scan` | 3 | a reader |

The survey is the file the others are measured around: 3 typeset pages
and a blank one, with a running header, page numbers, numbered headings,
lists, a formula, a figure with its caption, a footnote, a table with
merged cells, and a table that continues onto the next page under a
repeated header row. Its truth has a box for every block. The scanned
PDF holds the same pages as images with no text, the PNG and the JPEG
hold page 1, and the TIFF holds pages 2 to 4, so the same truth scores
all 5.

HTML and XML are not in the corpus: a parse of either is refused today.

## The measures

Each measure compares the pages a parse returned with the truth, page by
page.

| Measure | What it is |
|---|---|
| CER | the character error rate: the fewest insertions, deletions and substitutions of one character that turn the found text of a page into the truth's, summed over the pages and divided by the truth's characters |
| Kinds | the share of the truth's blocks that a found block of the same kind matches |
| Cells | the share of the truth's table cells that the matching table holds at the same row and column, with the same spans and the same text |
| Order | the share of pairs of neighboring truth blocks, both matched, whose found blocks come in the same order |
| Boxes | the share of the truth's blocks with a box whose matching block has a box that overlaps it by at least half, as intersection over union |

The text of a page is the text of its blocks in reading order, a table
as its cells row by row, with every run of whitespace as one space.
Figures and formulas are left out of it: the words printed in a figure
have no reading order, and a formula can be written in more than one
way. Nothing else is folded. Case, punctuation and every other character
count as they are.

A truth block and a found block of the same page match when at least
half of their characters agree, each block in one match at most, the
closest pairs first. A figure and a formula match a found block of the
same kind. A block nobody matches counts against Kinds, and what it held
counts against CER.

Cells has no value for a file with no table, and Boxes none for a file
whose truth has no box, which is every file but the survey.

## The bars

A file's class decides its bars. CER may be at most its bar, and every
other measure at least its bar.

| Class | CER at most | Kinds at least | Cells at least | Order at least | Boxes at least |
|---|---:|---:|---:|---:|---:|
| `exact` | 0% | 100% | 100% | 100% | 100% |
| `typeset` | 2% | 90% | 95% | 95% | 90% |
| `scan` | 5% | 90% | 95% | 95% | 85% |
| `converted` | 2% | 90% | 95% | 95% | 90% |

- `exact` is a file read from its own structure. Nothing stands between
  the file and the result, so nothing may differ.
- `typeset` is a page rendered from its source with no loss. The bars
  are what a competent page-reading model reaches on a clean page.
- `scan` is an image of a page at 120 dpi in 16 grays, which is less
  than a reader is given of a typeset page, so more errors are allowed.
- `converted` is a file an office suite converts first. Its pages are
  clean, so it is held to the typeset bars.

## On every push

`go tool lateregate`, the gate every push runs, includes the tests of
`test/quality`. They call no model:

- Each `exact` file goes through the pipeline and assembly as a parse
  takes it, and must match its truth with no character, kind, cell or
  order wrong. `report.doc` is among them, with a converter that answers
  with `report.docx`.
- Each file a reader reads is taken through the pipeline with the stub
  reader: the type it is detected as and what it is converted to, the
  count of its pages, a selection of them, the size of the image a
  reader is given, and the blank pages, which cost no call and hold no
  block. The converter of these tests is a fake that answers with a
  fixed file, so they hold that a file was sent for conversion as the
  right pair of types, and not what an office suite makes of it.
- Each file of the survey is read by a reader that returns the truth,
  and what comes out must score perfect on every measure: nothing
  between a reader and the document may cost a point.
- Assembly runs over the survey as a reader would return it that labeled
  no header and no page number. It must find the running header on 3
  pages and mark 2 of them repeated, find the page numbers, join the
  table that continues into one span, and build the outline from the
  printed section numbers.

## With a model

`make live-quality` reads the corpus with a reader of your own, through
the server as it ships: it builds `lectiod`, starts Postgres and an
S3-compatible object store in containers, runs the durable server in
the role `all` against them, uploads and parses each file over the API,
and scores what the API returns. It calls a model, so it is never part
of the gate.

```sh
LECTIO_LIVE_CONFIG=reader.yaml \
LECTIO_LIVE_CONVERTER=http://127.0.0.1:8090 \
LECTIO_LIVE_OUT=out/quality \
  make live-quality
```

| Variable | Meaning |
|---|---|
| `LECTIO_LIVE_CONFIG` | a file or a directory of Reader and Policy documents, as `LECTIO_CONFIG` takes them. Required |
| `LECTIO_MODEL_KEY` | the key the reader's endpoint takes. It is handed to the server and is written to no file and no log |
| `LECTIO_LIVE_CONVERTER` | the address of a running conversion sidecar, as `LECTIO_CONVERTER_URL` takes it. Without it the 3 `converted` files are left out and the report says so |
| `LECTIO_LIVE_OUT` | the directory the results are written to. `out/quality` when it is not set |
| `LECTIO_LIVE_FILES` | file names of the corpus, separated by commas, to run only those |
| `LECTIO_LIVE_SERVER` | `durable`, the default, or `dev` for the development server, which needs no container runtime and keeps everything in memory |
| `LECTIO_LIVE_LECTIOD` | a `lectiod` binary to run in place of the one built from the tree |

The run needs a container runtime for the durable server. It stands up
an issuer of its own and calls the server with a token of it, so it
needs no account anywhere. The model reads 14 pages: the 2 blank pages of the 2 PDFs and the blank frame of
the TIFF cost no call, and the files read from themselves cost none.

It writes, under the output directory:

- `report.md`: one row per file with its pages, blocks, the 5 measures,
  seconds per page and tokens, then the bars, then what each file missed
  and a note for every block that was not right. Seconds per page is the
  time of the parse over the pages a reader was called for, with 2 pages
  read at once.
- `report.json`: the same numbers.
- one directory per file with each page as the API returned it, the
  image the reader was given of it, and the document as Markdown, so a
  miss can be looked at.
- `server.log`: the server's log.

The command fails when a file misses a bar or a parse does not succeed.
A miss is a finding and not a verdict: look at the page and its image
before deciding whether the reader, the truth or a measure is at fault.

To start a sidecar for the `converted` files on one machine:

```sh
podman build -f deploy/converter/Dockerfile -t lectio-convert .
podman run -d -p 127.0.0.1:8090:8090 lectio-convert
```
