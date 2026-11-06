# Security

For an operator deciding how to run Lectio and a builder deciding what to
send it: what Lectio guarantees, what it sees and keeps, what it does not
defend, and what an operator keeps safe. Reporting a vulnerability, and
the test that holds each guarantee, are in [SECURITY.md](../SECURITY.md).

## What Lectio guarantees

**Every request is verified and asked about.** Each one carries a token
from an issuer the operator listed, and nothing acts before the
authorizer, or the owner policy, has decided. The routes that take no
token are the contract at `/v1/openapi.yaml` and `/healthz`. Under the
owner policy, another caller's file or parse is not found.

**No decision is a refusal.** A deny, or an authorizer that does not
answer, is answered before anything is done, and `lectiod` does not start
against an authorizer that allows the reserved probe.

**An allow cannot widen what the server bounds.** A file's size, a
parse's pages and the retention an allow carries can be smaller than the
server's own and never larger. The caps on a group's queue and day are
the authorizer's to set either way. A limit the server cannot hold, such
as a cap on the queue in the development server, refuses the request.

**A fetch reaches public addresses only.** A source URL is fetched over
`https`, from an address checked when the connection is made and again
after each redirect, unless the operator listed its host in
`LECTIO_FETCH_ALLOW`. The fetch carries no trace header, and a failed
fetch tells the caller neither the address nor the query.

**A hostile file is bounded.** A PDF page renders in an engine held to
512 MiB and 30 seconds, with no file system. An image is refused from its
header past 40 megapixels, a TIFF past 100,000 pages. An office package
is refused for what it declares before it is inflated, and its markup is
held to bounds on depth, tokens and tag size. A part that declares an
entity, and a relationship that leaves its package, are refused.

**Conversion is kept apart.** Presentations, rich text and older office
formats are converted in the `lectio-convert` sidecar, whose suite runs
with nothing of the sidecar's environment, macros and links off, and is
killed with its children at its time limit. The shipped manifests give
the sidecar no network out and no credential.

**A model key stays out of what Lectio writes.** A key is in no answer,
error, log line, row or object. With a key endpoint, only the workers
read the endpoint's address and bearer.

## What Lectio sees

| What | Where it goes |
|---|---|
| A file's bytes | the bucket, until the file's retention passes; the conversion sidecar, for a format it converts |
| A file's name, media type, size and digest, and its owner | Postgres, with the file |
| Each page's image, by default at 160 dpi and at most 2048 pixels on its long edge (the `layout` adapter: 200 dpi, no bound on the edge; both set per reader); a page read from a PDF's own words sends none | the model endpoint of the reader that reads it, and the bucket |
| A figure cut from its page, when figures are asked for | the model endpoint of the reader that describes it |
| A document's text, for an extraction, in windows of 400,000 bytes by default and at most 32 | the model endpoint of the reader that extracts |
| Each page's text, blocks, tables, the document and its fields | the bucket and Postgres, until the parse's retention passes |
| Which reader and which model read each page | the parse's record, read through the API |
| Each action a caller takes | the authorizer, as [the question](authorizer.md#the-question): the caller's subject and every claim of its token, the action, the object's fields, including a parse's labels and origin as the caller wrote them, and the request's id, peer address and user agent; never a file's bytes, its stored name, its digest, a source URL or a page's content |
| A tenant's group, owner and parse, and the models the parse may use | the key endpoint, with `LECTIO_KEYS=endpoint` ([Whose key reads a page](readers.md#whose-key-reads-a-page)) |
| One line per request and task: ids, outcomes and the text of an error, which can name an address of your infrastructure | the log on standard error, and over OTLP with `OTEL_EXPORTER_OTLP_ENDPOINT` set ([Configuration](configuration.md#observability)) |

## How long it keeps things

- **A file** is kept for `LECTIO_FILE_RETENTION`, 24 hours by default,
  from its last upload and past the end of the last parse that read it.
- **A parse**, with its pages, images, document and fields, is kept for
  `LECTIO_PARSE_RETENTION`, 30 days by default, from its end.
- An authorizer may shorten either for a caller and never lengthens
  them ([Limits an allow carries](authorizer.md#limits-an-allow-carries)).
- **The retention sweep** runs on the workers every
  `LECTIO_SWEEP_INTERVAL`, 30 seconds by default, and removes at most
  200 parses and 200 files each time, objects first and rows after. A
  delete that stopped halfway is finished by a later sweep.
- **An idempotency key** is kept for 24 hours on the durable server.
- **The usage meter**, the pages read, model calls and tokens counted
  by the hour, is never removed.
- **The development server** keeps everything in its process until it
  stops, and nothing after.

## Deleting

- **A parse** can be deleted once it has ended. The delete removes every
  object the parse wrote and its rows.
- **A file** can be deleted once no running parse reads it; until then
  the delete answers `409 not_terminal`. It removes the file's stored
  copy. The parses that read it keep their page text, page images,
  document, fields and figures until each is deleted or its retention
  passes, so delete those too to remove a file's content.

## For a builder

- A transcription or an extracted value is a model's reading of a file
  someone else wrote. A file can hold text written to steer the model.
  Treat what comes back as the file's claim, and check a value before
  acting on it.
- Each value of an extraction cites the blocks it was read from, and
  each page names the reader and model that read it, so a value can be
  traced back to its place on the page.
- Pages go to the model endpoint your operator configured. Ask them
  which model reads your pages and what that endpoint keeps.
- Errors to branch on, and the bounds a request is held to, are in the
  [API guide](api.md#errors).

## What Lectio does not defend

[SECURITY.md](../SECURITY.md#what-is-not-defended) is the full list.
The limits an operator plans around:

- **Text in a file that steers a model.** The prompts tell the model
  that a file's text is content, and Lectio checks the shape of the
  answer, not its truth.
- **What the model endpoint keeps.** Each page goes there; its terms are
  the endpoint's.
- **What a deleted file leaves** in the parses that read it.
- **A conversion sidecar run with a network.** The shipped manifests take
  it away; an installation of its own must too.
- **A development server on a public address.** It listens on every
  interface with the token `dev` by default.
- **An authorizer that answers wrongly,** or an issuer that signs a token
  for the wrong subject. Lectio follows the decision it is given.
- **A compromised host,** and the services you configure: the model
  endpoint, the bucket, Postgres, the issuer and the key endpoint.

## For an operator

| Secret | What its holder can do |
|---|---|
| `LECTIO_MODEL_KEY` | call the model endpoint, billed to you |
| `LECTIO_KEYS_TOKEN` | ask your key endpoint for any group's key |
| `LECTIO_AUTHORIZER_TOKEN` | call your authorizer as `lectiod` |
| `LECTIO_DATABASE_URL`, `LECTIO_DATABASE_POOL_URL` | read and change every parse, task, page and result |
| `LECTIO_S3_ACCESS_KEY`, `LECTIO_S3_SECRET_KEY` | read and remove every file, page image and document in the bucket |
| `LECTIO_DEV_TOKEN` | call a development server that lists no issuer, as the subject `dev` |
| `OTEL_EXPORTER_OTLP_HEADERS`, when it carries a token | send telemetry as your installation |

Beside the secrets:

- Run the development server for one person on one machine. Bind it to
  `127.0.0.1` with `LECTIO_ADDR`, since its default is every interface.
- Put TLS in front of `lectiod`, which serves plain HTTP, and keep the
  internal listener, `LECTIO_INTERNAL_ADDR`, inside the cluster.
- `LECTIO_OIDC_ISSUERS` and `LECTIO_AUTHORIZER_URL` accept `http`. List
  only `https` ones outside a test.
- List in `LECTIO_FETCH_ALLOW` only hosts every caller may read through
  Lectio, such as your own object store.
- Each subject in `LECTIO_ADMIN_SUBJECTS` reads every owner's files and
  parses under the owner policy.
- `LECTIO_READER_ENDPOINTS` bounds where a reader stored through the API
  may send pages. List only endpoints you trust with your callers' files.
- Run `lectio-convert` with no network, as
  [Install](install.md#the-conversion-sidecar) says.
- With `OTEL_EXPORTER_OTLP_ENDPOINT` set, the logs, traces and metrics
  leave the host for that endpoint.
