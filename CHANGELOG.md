# Changelog

What changed for whoever runs `lectiod`, calls its API, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- Added: `lectiod` reads `LECTIO_OIDC_ISSUERS`, `LECTIO_OIDC_AUDIENCE`
  (default `lectio`), `LECTIO_AUTHORIZER_URL`, `LECTIO_AUTHORIZER_TOKEN`
  and `LECTIO_ADMIN_SUBJECTS`, and refuses to start when one is not
  well formed, naming the variable. Nothing uses them yet: the
  development server still takes its one static token.
- Added: identity and authorization as a library the server does not
  call yet (`internal/access`). A bearer is verified against the listed
  issuers and becomes a subject, `<issuer>|<sub>`, with every claim
  handed on unread. One question per request goes to the authorizer,
  which may answer with limits and with whose a new parse or file is;
  an authorizer that does not answer refuses the request with
  `authorizer_unavailable`. With no authorizer the owner policy
  decides: a caller acts on its own, and the admin subjects read
  everyone's and change nothing.
- Added: the package `authorizer`, what an authorization endpoint for
  Lectio is written against: the 11 actions `lectiod` asks, the resource
  kind and the fields of each, and the limits an allow may carry, with
  the JSON form an endpoint renders and the function that reads it. The
  server does not ask an authorizer yet.
- Added: an image of the server. `Dockerfile` builds `lectiod` alone on
  a distroless base: no shell, no package manager, no office suite, run
  as user 65532. It is 37 MiB, and it runs with a read-only root file
  system and no capability.
- Added: `deploy/`, Kustomize manifests for a cluster. `deploy/base`
  holds one Deployment for the API and one for the workers, the API's
  Service, a network policy for each role and a disruption budget. It
  names no namespace, registry or host: an overlay carries the settings
  and the Reader and Policy documents as 2 ConfigMaps, and the
  credentials are Secrets applied by hand. `deploy/README.md` lists
  every name and key. The manifests are written for the durable server,
  with its roles, its internal listener and its probes, which this
  build does not have yet.
- Added: `deploy/components/converter`, which an overlay adds to run the
  conversion sidecar in a cluster: Pods of its own, under a policy that
  admits the workers' calls and refuses every connection the sidecar
  opens. An installation without it refuses the formats that need
  conversion, as before.
- Added: `deploy/examples/compose.yaml`, the stack on one machine:
  Postgres, an object store with its bucket, `lectiod` in the role
  `all`, and the sidecar on a socket with no network. It needs the
  durable server too. `docs/running.md` says how to start it and run a
  parse against it.
- Added: a release publishes 2 images, `ghcr.io/<owner>/lectiod:<tag>`
  and `ghcr.io/<owner>/lectio-convert:<tag>`, for linux/amd64 and
  linux/arm64, each signed and with an attested bill of materials and
  provenance, and attaches `deploy-<tag>.tar.gz`: the deploy tree with
  both images pinned to the release by digest.
- Changed: the sidecar's image stamps its build, so `lectio-convert
  version` in an image names the version it was built from. It printed
  `dev` before.
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
