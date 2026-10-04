# Changelog

What changed for whoever runs `lectiod`, calls its API, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- Added: the package `authorizer`, what an authorization endpoint for
  Lectio is written against: the 11 actions `lectiod` asks, the resource
  kind and the fields of each, and the limits an allow may carry, with
  the JSON form an endpoint renders and the function that reads it. The
  server does not ask an authorizer yet.
- Added: the durable task store over Postgres, as a package
  (`internal/store/postgres`) the server does not open yet. A parse and
  its tasks are rows; a worker reaches the database through one
  statement that renews its lease, settles, and claims the next tasks
  in the fair order of tenants, projects and classes, only where a
  reader has room. `lectiod` still runs with `LECTIO_DEV=true` alone.
- Added: `LECTIO_DATABASE_POOL_URL`, the endpoint the store's pool
  opens, for an installation with a transaction-mode pooler in front of
  its database. Without it the pool opens `LECTIO_DATABASE_URL`, which
  migrations always use.
- Added: the repository scaffold. The object model (`document`); the
  interfaces a page-reading model and a field-extracting model sit behind
  (`reader`), with adapters for an OpenAI-compatible chat endpoint, for a
  layout engine behind HTTP, and a stub that calls nothing; intake for
  images, text, Markdown and CSV; assembly with Markdown, text and chunk
  views chosen when a result is read; and the HTTP contract in
  `api/openapi.yaml`.
- Added: `lectiod` with `LECTIO_DEV=true`, a development server that
  serves the whole contract in one process and keeps nothing. Routes the
  contract marks planned answer `501 not_implemented`.
- Added: PDF pages are rendered in process, by PDFium compiled to
  WebAssembly: no C library, no second service. The engine sees no host
  file and is bounded in memory and time.
- Added: `GET /parses/{parse}/blocks`, every block of the pages read so
  far in one answer.
- Added: a parse that is canceled, runs out of time, or loses pages
  still serves its document. A page it never read has the state
  `skipped`.
- Added: on a block, `description`, what a reader says a figure shows,
  kept apart from `text`; on a table cell, `header`; on a page,
  `truncated` and `reused`; on a reader, `version`.
- Changed: `reuse` works by page. Every submit makes a parse of its own;
  a page already read whole from the same bytes by the same readers is
  taken and no model is called. A parse no longer answers with another
  parse and `reused: true`; it reports `progress.pages_reused`.
- Changed: a chat reader sends no `temperature` unless one is
  configured, and sends its output bound as `max_completion_tokens`
  unless `outputLimitParam: max_tokens` is set.
- Added: a Reader document takes `boxes` (the order and space a model is
  asked for positions in), `temperature`, `outputLimitParam` and `cost`.
  `requestsPerMinute` is refused.
- Changed: `priority` orders a caller's own pages. Callers take turns,
  and nothing one queues moves it ahead of another.
- Added: `make live`, an opt-in test that parses a real file with a
  configured reader.
- Security: a source URL reaches no trace; a table's markup is written
  from its cells and never passed through from a model; a page is
  skipped as blank only when every pixel is one color. `SECURITY.md`
  lists each control with its test and what is not promised.
- Added: `GET /parses/{parse}/blocks/{ref}/image`, a block as it looks
  on its page: the page image cut to the block's box.
- Added: `POST` and `GET /parses/{parse}/figures`. A request against a
  parse that has ended has each figure cut from its page and described
  by a model: the block gains `description`, `figure.type`, and the
  words printed in the figure. No page is read again, and a figure
  described once is not described twice.
- Added: the Policy document takes `describe.chain`, naming the Reader
  documents that describe figures. A `chat` reader can; a `layout`
  reader cannot.
- Added: `make live` with `LECTIO_LIVE_DESCRIBE=1` also describes the
  figures of the file it parses.
- Changed: every operation's `summary` in `api/openapi.yaml` is a short
  label with no closing period, and the sentence it replaced opens the
  operation's description, so a reference renders the document as it is.
- Changed: a legacy spreadsheet (`.xls`) is refused when it is uploaded,
  with `unsupported_media_type` and a detail that says to save the
  workbook as `.xlsx`. It was accepted before and failed when parsed.
- Added: a Word document (`.docx`) is read from the file itself, with
  no model and no office suite: one page of blocks with headings and
  their levels, list items, tables with merged cells and header rows,
  footnotes, and a figure where a picture is. Tracked changes are read
  as accepted. A parse of one failed with `unsupported_media_type`
  before.
- Security: a zipped office document is read under bounds on what it
  declares and on what it inflates to, so a small crafted file cannot
  exhaust the server: it is refused with `file_too_large` or
  `document_corrupt`.
- Added: an Excel workbook (`.xlsx`, `.xlsm`) is read from the file
  itself: one page per sheet, hidden sheets included, each with the
  sheet's name as its title and its cells as one table. Merged cells
  keep their spans, a date reads as an ISO 8601 date, a percentage as a
  percentage, and a formula as the value the workbook stored for it;
  no formula is evaluated and no macro is opened.
- Added: `lectio-convert`, a sidecar that converts what the server does
  not read itself: `.doc` to `.docx`, and `.pptx`, `.ppt`, `.odp`,
  `.key`, `.rtf` and `.odt` to PDF. `deploy/converter/Dockerfile` builds
  its image. It holds no credential, runs its office suite with an
  empty environment in a scratch directory under a limit on time and on
  memory, and is meant to be run with no network: give it a socket on a
  volume with `LECTIO_CONVERT_ADDR=unix:/path`.
- Added: `LECTIO_CONVERTER_URL`, where the server reaches the sidecar,
  as `http://host:port` or `unix:///path/to/socket`. With none set the
  formats that need conversion are refused with
  `unsupported_media_type`, as before.
- Added: `.odt` and `.odp` files are detected.
- Added: `make live-convert`, an opt-in test that converts a fixture of
  every converted format through a running sidecar.
