# Changelog

What changed for whoever runs `lectiod`, calls its API, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- Added: a key per tenant, in the durable server. `LECTIO_KEYS=endpoint`
  reads the pages of each group with a key an endpoint of yours issued
  for that group, so a gateway attributes each tenant's model spend to
  it and bounds the spend by the tenant's budget. `static`, the default,
  keeps one key from `LECTIO_MODEL_KEY` for every group. A worker sends
  `POST LECTIO_KEYS_URL` with the bearer `LECTIO_KEYS_TOKEN` and
  `{"group", "owner", "parse"}`, once per group, takes `{"key",
  "expires_at"}`, holds the key in memory alone, and asks again 1 minute
  before it expires, so a key has to be good for more than 1 minute when
  it is issued. A `402` fails the group's pages with `budget_exhausted`
  and a `403` with `reader_unavailable`. Any other answer, and no
  answer, fails nothing: the group's pages wait in the queue, spend no
  attempt, and are read once the endpoint answers, which a worker tries
  after 1 second and then after twice as long each time, up to 30
  seconds; the worker stays ready meanwhile. With a key per group a rate
  limit of the model endpoint pauses the calls of the group whose key
  was limited, and no other group's. Only a process that runs tasks
  reads `LECTIO_KEYS_URL` and `LECTIO_KEYS_TOKEN`: the API reads
  `LECTIO_KEYS` alone and starts without them. A worker, and a process
  in both roles, does not start with `endpoint` and no address or no
  bearer, with `endpoint` and a `LECTIO_MODEL_KEY`, or with an address
  or a bearer while the source is `static`, and names the variable; a
  development server does not start with `endpoint`. In the deploy
  tree, `LECTIO_KEYS` is a key of the ConfigMap `lectiod`, and the
  worker Deployment reads the 2 others from the optional Secret
  `lectiod-keys`, which `deploy/bootstrap/secrets.example.yaml`
  declares.

## v0.1.1 - 2026-10-04

- Fixed: a release publishes its images. The pipeline of `v0.1.0` stopped
  before it tagged them: the first bill of materials is written into a
  directory the job had not made yet. That tag has no image under its
  name, no deploy archive and no release page; the first release that
  carries them is the one after it.

## v0.1.0 - 2026-10-04

- Changed: `lectiod` verifies its callers and asks who decides. The
  durable server no longer takes the token of `LECTIO_DEV_TOKEN`: a
  process that serves the API needs `LECTIO_OIDC_ISSUERS`, verifies each
  bearer against them for the audience of `LECTIO_OIDC_AUDIENCE`, and is
  refused at start without one, naming the variable. A worker needs
  none. With `LECTIO_AUTHORIZER_URL` and `LECTIO_AUTHORIZER_TOKEN` every
  request is asked of that endpoint; without them the owner policy
  decides, with `LECTIO_ADMIN_SUBJECTS` as the subjects that read every
  owner's. `LECTIO_DEV=true` keeps its one static token, for the
  subject `dev`, unless it lists issuers. The server logs the mode at
  start, names an issuer or an endpoint that does not answer and starts,
  and is refused an endpoint that allows the probe every authorizer
  denies.
- Changed: every route asks its action before it acts, the planned ones
  included. A parse or a file the caller may not act on is answered
  as one that is not there (`404`), a create, a list or a read of the
  readers the caller may not make is `403 forbidden` with the
  authorizer's reason in `details.reason`, and a request the authorizer
  gave no decision on is `503 authorizer_unavailable`, which is
  retryable. An owner is now the verified subject, `<issuer>|<sub>`, or
  the owner the authorizer's allow names.
- Added: the limits of an allow are held. A new file or parse is
  recorded under the allow's `owner`; a parse joins its `group` and
  `project` with their weights and the group's `max_running` and
  `max_queued` (`429 queue_full`); `max_priority` bounds a submit's
  priority (`400 invalid_request`), `classes` its class (`403
  forbidden`), `readers` the reader it pins (`403
  reader_not_permitted`); `max_file_bytes` holds an upload and the file
  a submit names (`413 file_too_large`); `max_pages` fails a parse that
  selects more with `too_many_pages`; and a list is narrowed to the
  owners and labels of the allow's `filter`. A limit a server cannot
  hold is `422 capability_unsupported`: a development server refuses
  `max_running`, `max_queued`, `pages_per_day` and `retention_seconds`.
- Changed: with no bound configured, a submit's `priority` is 0 or is
  refused. `LECTIO_GROUP_DEFAULTS=max_priority=10` admits priorities
  from -10 to 10 under the owner policy and where an allow names none.
- Added: `LECTIO_GROUP_DEFAULTS`, a list of `name=value` over `weight`,
  `max_running`, `max_queued`, `max_priority` and `pages_per_day`: what
  a group takes when the allow of its parse names none.
- Added: pages per day, in the durable server. An allow's
  `pages_per_day` is its group's budget for a day in UTC. A parse's
  pages are reserved when they are counted: a parse the day does not
  hold fails with `budget_exhausted` before a page is read, a submit of
  a group with nothing left is `402 budget_exhausted`, and the pages a
  parse reserved and did not read are given back when it ends.
- Added: retention, in the durable server. A file is kept for
  `LECTIO_FILE_RETENTION` (default `24h`) after its last upload and
  after the end of the last parse that read it, and a parse with
  everything it wrote for `LECTIO_PARSE_RETENTION` (default `720h`)
  after it ended; an allow's `retention_seconds` shortens either. A
  file's `expires_at` is in its API view. Every worker runs the sweep,
  one of them per `LECTIO_SWEEP_INTERVAL`: objects are removed before
  rows, and a delete that stopped halfway is finished by a later sweep.
  Files and parses stored before this version have no retention and are
  kept until they are deleted.
- Changed: the schema is at version 4. The API applies migration
  `000004_limits` at start; a worker of this version waits for it.
- Changed: `api/openapi.yaml` lists the codes `authorizer_unavailable`
  and `capability_unsupported`, `503` among the statuses any operation
  may answer, `422` on `POST /files`, and `402` on `POST /parses`.
- Added: the durable server. Without `LECTIO_DEV`, `lectiod` keeps
  parses and their tasks in Postgres and bytes in an S3 bucket, and
  `LECTIO_ROLE` says whether a process serves the API (`api`), runs the
  tasks (`worker`), or does both (`all`, the default). The API applies
  the schema at start and holds no parse in memory, so it can be
  restarted while parses run. A worker that is stopped gives its pages
  back at once; one that is killed loses them to the other workers
  after `LECTIO_TASK_LEASE`, and no page is recorded twice. It describes
  no figures yet (`501 not_implemented`), and serves every other built
  route.
- Added: `/livez`, `/readyz` and `/version` on `LECTIO_INTERNAL_ADDR`
  (default `:8081`) in the durable server. A process is ready when the
  database answers and, for a worker, the task store answered it within
  a third of its lease.
- Added: the bucket's settings, `LECTIO_BUCKET`, `LECTIO_BUCKET_PREFIX`,
  `LECTIO_S3_ENDPOINT`, `LECTIO_S3_REGION`, `LECTIO_S3_ACCESS_KEY`,
  `LECTIO_S3_SECRET_KEY` and `LECTIO_S3_PATH_STYLE`, and the worker's and
  the queue's: `LECTIO_TASK_LEASE`, `LECTIO_TASK_EXPIRIES`,
  `LECTIO_SWEEP_INTERVAL`, `LECTIO_WORKER_FLUSH`, `LECTIO_WORKER_POLL`,
  `LECTIO_POOL_RECOVERY`, `LECTIO_POOL_RESUME` and `LECTIO_CACHE_BYTES`.
  `LECTIO_KEYS` takes `static` and nothing else.
- Changed: in the durable server a Reader's `maxInFlight` and `cost`
  are applied. `maxInFlight` bounds the reader's calls in flight across
  every worker, 8 when a document names none. `cost` is a whole number:
  a fraction is refused when the documents are loaded.
- Changed: a page waits for a reader that is only busy. It goes to the
  next reader of the Policy's chain only when the first cannot be
  called at all, its key paused by a rate limit or its breaker open, so
  load alone never sends pages to a costlier reader.
- Changed: `POST /parses/{parse}/figures` lists `501` among its answers
  in `api/openapi.yaml`.
- Added: `lectiod` reads `LECTIO_OIDC_ISSUERS`, `LECTIO_OIDC_AUDIENCE`
  (default `lectio`), `LECTIO_AUTHORIZER_URL`, `LECTIO_AUTHORIZER_TOKEN`
  and `LECTIO_ADMIN_SUBJECTS`, and refuses to start when one is not
  well formed, naming the variable.
- Added: identity and authorization (`internal/access`). A bearer is
  verified against the listed
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
  the JSON form an endpoint renders and the function that reads it.
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
  with its roles, its internal listener and its probes.
- Added: `deploy/components/converter`, which an overlay adds to run the
  conversion sidecar in a cluster: Pods of its own, under a policy that
  admits the workers' calls and refuses every connection the sidecar
  opens. An installation without it refuses the formats that need
  conversion, as before.
- Added: `deploy/examples/compose.yaml`, the stack on one machine:
  Postgres, an object store with its bucket, `lectiod` in the role
  `all`, and the sidecar on a socket with no network. It needs an
  identity provider of yours, named by `LECTIO_OIDC_ISSUERS`, which the
  server does not sign callers in through yet. `docs/running.md` says how to start it and run
  a parse against it.
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
- Added: a quality corpus and its bars. 13 files, each with the truth
  it is scored against: plain text, Markdown, CSV, a Word document, a
  workbook, a Word 97 file, a presentation, rich text, a typeset PDF,
  the same pages as a scanned PDF, a PNG, a JPEG and a TIFF. 5 measures
  compare a parse with a truth: character error rate, kinds, table
  cells, reading order and boxes. The gate holds the files read with no
  model to their truths exactly. `make live-quality` reads the corpus
  with the reader `LECTIO_LIVE_CONFIG` names, through the durable
  server, writes `report.md` and `report.json`, and fails when a file
  is under a bar. [`docs/quality.md`](docs/quality.md) has the corpus,
  the measures, the bars and the settings.
- Added: `make live-convert`, an opt-in test that converts a fixture of
  every converted format through a running sidecar.
- Fixed: a list item no longer keeps the bullet or the number an engine
  or a model wrote before it. Its text is the item alone, as a list item
  read from a Word document already was.
