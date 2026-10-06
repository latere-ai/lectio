# Changelog

What changed for whoever runs `lectiod`, calls its API, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- Added: `GET /readers` marks a reader that reads a page from the text
  its file carries and calls no model with `text_layer: true`, so a
  console can keep it out of a picker of models while the routing
  policy still tries it first.

- Fixed: an extraction no longer reads page furniture. Blocks of kind
  `page_header`, `page_footer` and `page_number` are left out of the
  text a model is given and out of what a citation may name, so a
  running header is not taken for the document's title.

## v0.4.0 - 2026-10-04

- Added: extraction with a schema, in the durable server.
  `POST /parses/{parse}/fields` takes a `name`, a JSON Schema of draft
  2020-12 whose root is an object, and optional `instructions`, and
  answers `202` with the extraction, `pending`. A text model fills an
  object in the shape of the schema from the document's text, and each
  value cites the blocks it was read from; `GET
  /parses/{parse}/fields/{name}?resolve=true` returns each citation
  with its page and its box. A field returns the `schema` and the
  `instructions` it was asked with, from its request on, so a client can
  show what it holds before it is filled. A `uniqueItems` check of a
  long list that holds lists or objects is priced as if every item met
  every other, so a reply built for its items to hash alike is not
  checked, and an honest list of more than about 500 objects held to
  `uniqueItems` costs too much to check and fails its field. A value
  the document does not state is
  left out and never made up. An object that does not satisfy the
  schema is sent back to the model with what was wrong, up to 2 times,
  and the extraction then fails with `schema_not_satisfied`; its parse
  is not affected. A schema that does not compile, is over 64 KiB,
  nests deeper than 16 levels or refers to anything but a schema inside
  itself is `400 invalid_schema`. A schema uses the keywords of draft
  2020-12 that the contract lists, and one that uses another is refused
  with the keyword named: among those not taken are `$id`, `$anchor`,
  `$dynamicRef`, `dependencies`, `definitions`, `unevaluatedProperties`
  and a caller's own keywords. The list exists because a validator
  keeps nothing of what it applied, so what holding an object to a
  schema costs has to be counted before it is done, and a count has a
  price for the keywords it knows. A schema that applies more than 256
  subschemas to one value, applies itself without end, or holds
  patterns that compile to more than 16,384 steps together is refused,
  and so is a number of more than 32 characters. A reply is counted
  before it is checked: one that would take more than 1,048,576 units
  of work, or whose check does not end in 5 seconds, fails the
  extraction with `schema_not_satisfied`, and its worker goes on. A
  document longer than the extractor's input is read in windows, at
  most 32, and the replies are merged. An extraction that has made a
  call stays with its extractor: it waits while that one is paused, and
  moves down the chain only when the extractor declines it or its
  replies are not usable. An
  extraction may be asked while its parse runs and waits for the parse
  to end. A parse holds at most 64 extractions. A development server
  answers `501` for the 3 routes.
- Added: describing figures, in the durable server. `POST
  /parses/{parse}/figures` answered `501` there and now starts a run,
  as it does in a development server: each figure is one task, queued
  with the pages of every parse in the parse's group, and its
  description is on the figure's block wherever the block is read. A
  figure that was described from the same bytes by the same describers
  is taken from that description by a later run of the same owner. A
  run describes at most 1,000 figures, in both servers: a request whose
  pages hold more is `413 too_many_pages`, and the parse is described
  a range of pages at a time.
- Added: an extraction and a figure are read with the key of their
  parse's group, as a page is. With `LECTIO_KEYS=endpoint`, a `402` from
  the key endpoint fails them with `budget_exhausted`, a `403` with
  `reader_not_permitted`, and an endpoint that does not answer leaves
  them waiting. A rate limit pauses the group's key and spends no
  attempt. Every call is in the meters of `GET /usage`, under its
  group, its owner and its reader.
- Added: a Reader document of the `chat` adapter gives an extractor
  under the reader's name, as it gives a describer, and takes
  `maxInput`, the most text one extraction call is given, in bytes;
  400,000 unless set. The Policy's `extract.chain` is applied: it was
  read and reported as not applied. With no `extract` chain an
  extraction runs only when its request names an `extractor`, and with
  no `describe` chain a run only when its request names a `describer`.
- Changed: `DELETE /parses/{parse}` and the end of a parse's retention
  stop the work on the parse first, then remove its objects, then its
  rows, so an extraction or a figure that runs while its parse is
  removed leaves no object behind: its settle is refused, and its
  worker removes what it wrote. A delete that stops halfway is
  finished by the retention sweep.
- Changed: a worker names the kinds of task it runs in every exchange
  and is handed no other. A worker of `v0.3.0` names none and is handed
  `prepare`, `page` and `assemble`, so one that is running when the
  schema is migrated goes on reading pages, and the extractions and the
  figure runs wait for the first worker of this release.
- Upgrading from `v0.3.0`: roll the API first, which applies the sixth
  migration, and the workers right after it. Once the schema is
  migrated a `v0.3.0` Pod that starts or restarts does not come up,
  since a process refuses a schema at another version than its own; the
  ones already running go on until they are replaced. Rolling back to
  `v0.3.0` needs the migration's down file applied first. The migration
  waits up to 10 seconds for its locks and fails with nothing applied
  when a transaction holds one of its tables for longer;
  `docs/running.md` has the repair.
- Changed: `POST /parses/{parse}/retry` is `409 conflict` while an
  extraction or a figure of the parse is queued or running. A retry
  writes again the pages that work reads.
- Changed: `GET /parses/{parse}/fields/{name}` lists `400` for a
  `resolve` that is not `true` or `false`, and a citation of a field is
  a ref, or with `resolve=true` an object of `ref`, `page` and `box`.
- Operators: the durable server's schema moves to version 6 when the
  API starts. Roll the workers with the API: a worker of an earlier
  version fails an extraction or a figure it is handed with `internal`.
  An extraction and a figure run have as long as `LECTIO_MAX_DEADLINE`
  from when they are queued.

## v0.3.0 - 2026-10-04

- Added: `POST /parses/{parse}/retry`, in the durable server. It reads
  again the pages of a parse that failed and no other: the answer is
  `202` with the parse `running` again, the pages that were read stay
  readable, and the parse is assembled again when the retried pages
  have settled. It is for a parse whose pages all settled and of which
  at least 1 failed. A parse that has not ended is `409 not_terminal`;
  one with no failed page, and one that was canceled, ran out of time
  or could not be prepared, is `409 conflict`. The parse stays in its
  group: a group at `max_queued` is `429 queue_full`, and one whose day
  does not hold the failed pages is `402 budget_exhausted`. The parse
  has as long from the retry as its submit gave it. A parse that ended
  with a failed page before this version cannot be retried and is
  `409 conflict`: submit its file again.
- Added: `GET /parses/{parse}/events`, in the durable server: server-sent
  events of a parse until it ends. `page` is sent once for each page
  that is read or fails, and `progress` and `state` as they change.
  Each event has an id that only grows, and `Last-Event-ID` resumes
  after it with nothing sent twice, at any replica and after a restart
  of the API: the events are read from stored rows. The server polls a
  parse once a second, sends a comment line after 15 seconds of
  silence, and ends a stream it has held for 5 minutes, so a client
  connects again with the last id it saw. A page that was read before
  this version was installed has no event.
- Added: `GET /usage`, in the durable server: the pages read, the model
  calls made for them and their input and output tokens, summed by
  `group`, `owner` or `reader` over hours or days in UTC. The meters
  are written as pages settle, a failed call counted as the call it
  was, and start empty at this version. A caller reads its own usage,
  a subject of `LECTIO_ADMIN_SUBJECTS` everyone's, and an authorizer is
  asked `usage.read` with the `owner` and the `group` the request
  names.
- Added: `GET /queue`, in the durable server: each group with its
  weight, its bounds, its parses that have not ended and its queued and
  running tasks per class, the same for each of its projects, and each
  reader's pool with its calls in flight, its breaker and the keys a
  rate limit paused. A caller sees its own group, a subject of
  `LECTIO_ADMIN_SUBJECTS` every group that holds work, and an
  authorizer is asked `queue.read` with the `group` the request names.
- Fixed: a parse that fails for its pages says what they failed with.
  When every failed page carries the same code the parse carries it
  too, so a parse whose pages were all refused for budget fails with
  `budget_exhausted` and not with `page_unreadable`. Pages that failed
  with several codes still fail the parse with `page_unreadable`. In
  the durable server.
- Changed: `api/openapi.yaml` no longer marks those 4 routes planned
  and describes their answers. `GET /usage` loses `by=model`, which no
  server could answer, takes `owner` and `group`, and answers `400` for
  a parameter that is not valid; `GET /queue` takes `group`; `POST
  /parses/{parse}/retry` lists `402`; and `GET /parses/{parse}/events`
  names its `Last-Event-ID` header. Each of the 4 still lists `501`,
  which a development server answers: they are built over the task
  store.
- Changed: the schema is at version 5. The API applies migration
  `000005_routes` at start; a worker of this version waits for it. In
  the durable server a parse that ends with a failed page now keeps the
  task rows of all its pages, which a retry needs, until the parse is
  deleted.
- Changed: a page whose group the key endpoint refuses with `403` fails
  with `reader_not_permitted`, where it failed with `reader_unavailable`.
  The group may not read with the operator's keys, which no retry mends,
  and a client can now tell that from a reader that is down. A parse whose
  pages all failed this way fails with the same code.
- Changed: a document has one title. A reader sees one page and calls the
  line each slide of a deck opens with a `title`; assembly now keeps the
  titles of the first page that holds one and makes a `title` on a later
  page a `heading`, with its text, place and ref unchanged and its level
  below the title's in the outline. A parse that has ended is not
  assembled again, so its pages keep the kinds they have.
- Added: a reader that reads a PDF page from the text the file carries,
  with no model call. A Reader document with `adapter: text` names no
  endpoint, no model and no key; named first in a Policy's
  `read.chain`, it reads every page it can read without guessing and
  the reader after it reads the rest. It builds paragraphs, headings by
  the size and the weight of their type, list items, tables whose
  ruling closes every cell, and a figure where the page paints one,
  each with its box. It declines, at the cost of one call to the next
  reader, a scan with or without a recognition layer, a page that is
  mostly picture, a table set without ruling, text side by side that is
  not columns of prose, text set at an angle, a word that lies under a
  bar painted over it or is drawn in the paper's color, and text whose
  characters have no Unicode or do not read as text. A parse that names a `text`
  reader gets it alone and fails the pages it declines with
  `page_unreadable`. Changed in the contract: a page's `source`, and
  its entry in the document, has a third value, `text_layer`, beside
  `reader` and `native`. Such a page names its `reader` and no `model`,
  and its `usage` is one page and no token; it counts as one page
  against a group's pages for a day. A client that switches on `source`
  needs the new case. `GET /usage` counts such a page as a page and no
  call, and counts no call for a page the reader declines. A worker
  asks the key source for no key before such a reader reads. In the Go
  packages: `reader.Description` has `Text` and `Local`, `reader.Page`
  has `Text`, `reader.Result` has `TextLayer`, and
  `document.SourceTextLayer` is the new source. A `text` reader's
  `maxInFlight` is 64 unless its document sets it. On the typeset file
  of the quality corpus the reader alone is within the bars a model is
  held to: 0.55% character error, 97.4% of kinds, every table cell.

## v0.2.0 - 2026-10-04

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
