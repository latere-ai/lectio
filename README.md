# Lectio

**A control plane that turns a file into a structured document.** Lectio
takes a PDF, an office document, a spreadsheet, or a scan and returns
its content as addressable objects: pages, the blocks on each page with
their kind and position, tables with their cells, and fields shaped by a
schema the caller supplies, each pointing at the blocks it was read
from. Pages are read by a vision-language model behind one interface,
so the model is a line of configuration.

What Lectio owns is the work around the model call. A parse is a graph
of small tasks kept in Postgres: one task prepares the file, one reads
each page, and the rest assemble the document. A worker that dies
loses nothing but its lease. A page that was read is never read again.
Tenants share the workers and the model's rate limit by weight, with an
interactive class ahead of a batch class, and no tenant can queue its
way past another.

> **Status: first release.** The specs in [`specs/`](specs/README.md)
> are the design, and each says what of it is built. Two servers run.
> The development server is the whole parsing path in one process with
> nothing durable. The durable server keeps parses and their tasks in
> Postgres and bytes in an S3 bucket, with the API and the workers as
> separate processes: a worker that is killed loses its lease and not
> the work. It verifies its callers against an OpenID Connect issuer,
> asks an authorizer or its own owner policy what each may do, and
> holds a parse to the limits it is given. It reads again the pages of
> a parse that failed, streams a parse's pages and progress as events,
> meters what each group, owner and reader read by the hour, and shows
> what waits and what runs per group; the development server answers
> `501` for those 4. Not built yet in the durable server, and answered
> `501`: extraction with a schema and describing figures.

## Run it

```sh
make run          # builds out/lectiod and starts it with LECTIO_DEV=true
```

The development server listens on `:8080`, keeps everything in memory,
and takes the token `dev`, whose holder owns what it creates. With no
reader configured it reads pages
with a stub that calls no model, so the path can be followed end to end
before a key exists.

```sh
api=http://localhost:8080/v1
auth='Authorization: Bearer dev'

# Upload a file. The same bytes twice are one file.
curl -s -H "$auth" --data-binary @report.csv "$api/files?name=report.csv"

# Parse it, holding the answer up to 30 seconds for the parse to end.
curl -s -H "$auth" -H 'Prefer: wait=30' -H 'Content-Type: application/json' \
  -d '{"source":{"file":"fil_..."}}' "$api/parses"

# Read the result: whole, as Markdown, one page, one block, or as chunks.
curl -s -H "$auth" "$api/parses/prs_.../document"
curl -s -H "$auth" "$api/parses/prs_.../document?format=markdown"
curl -s -H "$auth" "$api/parses/prs_.../pages/1"
curl -s -H "$auth" "$api/parses/prs_.../blocks/1.2"
curl -s -H "$auth" "$api/parses/prs_.../chunks?by=section"

# A figure as an image of its own, and what the figures show.
curl -s -H "$auth" "$api/parses/prs_.../blocks/4.1/image" -o figure.png
curl -s -H "$auth" -H 'Prefer: wait=30' -X POST "$api/parses/prs_.../figures"
```

A page is readable as soon as it was read, while the rest of the parse
is still running. How a result looks is chosen when it is read, so
another format or another chunk size never runs the parse again. The
contract is [`api/openapi.yaml`](api/openapi.yaml), and the server
serves it at `/v1/openapi.yaml`.

A reader that finds a figure says where it is. Describing figures is a
second request against the same parse: each figure is cut from its page
and described alone, and no page is read again.

To read pages with a model, declare a reader and point `LECTIO_CONFIG`
at the file. Any endpoint that speaks the OpenAI chat completions API
works, a gateway included:

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1
  model: your-vision-model
  image: { dpi: 160, longEdge: 2048, format: png }
  constrained: true
```

```sh
LECTIO_DEV=true LECTIO_CONFIG=reader.yaml LECTIO_MODEL_KEY=... out/lectiod
```

This build reads PDFs and images (PNG, JPEG, TIFF) through a reader,
and text, Markdown, CSV, Word documents (`.docx`) and Excel workbooks
(`.xlsx`, `.xlsm`) from the file itself, with no model. A Word document
comes back as one page: its headings with their levels, its lists, its
tables with merged cells and header rows, its footnotes, and a figure
where a picture is. A workbook comes back as one page per sheet, each a
table, with dates in ISO 8601 and each formula as the value the workbook
stored for it.

A PDF that carries its text need not be read by a model. A reader of
the `text` adapter reads a page from the words the file holds and where
it draws them, and declines a page it would have to guess at, a scan or
a table set without ruling, so that the next reader in the chain reads
it:

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: own }
spec: { adapter: text }
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read: { chain: [own, default] }
```

A page read that way says `"source": "text_layer"` and costs no call.
[`docs/running.md`](docs/running.md#reading-a-pdfs-own-text) says what
it reads and what it declines.

Presentations (`.pptx`, `.ppt`, `.odp`, `.key`), rich text (`.rtf`),
open document text (`.odt`) and legacy Word files (`.doc`) are converted
first, by a sidecar that holds an office suite. A server with no sidecar
refuses them, and a legacy Excel file (`.xls`) is refused either way.
The sidecar runs a large program on a file somebody else wrote, so it
is run with no network, on a socket the server shares with it:

```sh
podman build -f deploy/converter/Dockerfile -t lectio-convert .
podman volume create lectio-convert
podman run -d --network none -v lectio-convert:/run/lectio \
  -e LECTIO_CONVERT_ADDR=unix:/run/lectio/convert.sock lectio-convert
```

A server in a container that mounts the same volume reaches it with
`LECTIO_CONVERTER_URL=unix:///run/lectio/convert.sock`. For a server on
a laptop, the sidecar can listen on a port instead. It then has a
network, which is for your own files and not for ones you do not trust:

```sh
podman run -d -p 127.0.0.1:8090:8090 lectio-convert
LECTIO_DEV=true LECTIO_CONVERTER_URL=http://127.0.0.1:8090 out/lectiod
```

To check a reader against a real file, end to end:

```sh
LECTIO_LIVE_CONFIG=reader.yaml LECTIO_LIVE_FILE=paper.pdf make live
```

It parses the file with that reader and writes the Markdown, each
page's blocks and each page's image to `LECTIO_LIVE_OUT` when it is
set. It calls a model, so it is never part of `make check`.

To put numbers on a reader, `make live-quality` reads a corpus of files
whose content is known, through the durable server, scores each against
its truth, and fails when a file is under its bar.
[`docs/quality.md`](docs/quality.md) has the corpus, the measures and
the bars.

## Run it durably

Without `LECTIO_DEV`, `lectiod` keeps its work in Postgres and its
bytes in an S3 bucket. For a try on one machine, both run in
containers. The values below are examples; use your own.

```sh
podman run -d --name lectio-db -p 127.0.0.1:5432:5432 \
  -e POSTGRES_USER=lectio -e POSTGRES_PASSWORD=example -e POSTGRES_DB=lectio \
  postgres:16-alpine

# Any S3 compatible server does. A directory under its data path is a bucket.
podman run -d --name lectio-objects -p 127.0.0.1:9000:9000 \
  -e MINIO_ROOT_USER=example -e MINIO_ROOT_PASSWORD=example-secret \
  --entrypoint sh minio/minio -c 'mkdir -p /data/lectio && exec minio server /data'

export LECTIO_DATABASE_URL='postgres://lectio:example@127.0.0.1:5432/lectio?sslmode=disable'
export LECTIO_BUCKET=lectio LECTIO_S3_ENDPOINT=http://127.0.0.1:9000 LECTIO_S3_REGION=us-east-1
export LECTIO_S3_ACCESS_KEY=example LECTIO_S3_SECRET_KEY=example-secret LECTIO_S3_PATH_STYLE=true
export LECTIO_OIDC_ISSUERS=https://issuer.example    # your identity provider

make build
out/lectiod                 # the API and a worker in one process
```

The durable server takes no static token. It verifies each bearer
against the issuers of `LECTIO_OIDC_ISSUERS`, for the audience `lectio`
unless `LECTIO_OIDC_AUDIENCE` names another, and does not start without
an issuer. The same requests as above then work against
`http://localhost:8080/v1` with a token your provider issued, and a
parse survives a restart of the process.

Who may do what is decided in one of 2 ways. With nothing more set, the
owner policy decides: a caller, known by its issuer and its `sub`, acts
on the files and parses it created, and the subjects of
`LECTIO_ADMIN_SUBJECTS` read everyone's. With `LECTIO_AUTHORIZER_URL`
and `LECTIO_AUTHORIZER_TOKEN` set, every request is asked of that
endpoint, whose answer may also say whose a new parse is, which group
it joins, and what it is held to: pages per parse and per day, file
size, classes, readers, priority and retention. The package
[`authorizer`](authorizer) is what such an endpoint is written against,
and [`specs/012-identity-and-authorization.md`](specs/012-identity-and-authorization.md)
is the contract.

A file is kept for 24 hours after its last use and a parse for 30 days
after it ended, unless `LECTIO_FILE_RETENTION` and
`LECTIO_PARSE_RETENTION` say otherwise. To run
the roles apart, as a deployment does, start one process of each with
the same settings:

```sh
LECTIO_ROLE=api out/lectiod
LECTIO_ROLE=worker LECTIO_INTERNAL_ADDR=:8082 out/lectiod
```

The API applies the database schema when it starts. A worker serves no
public address. Each process answers `/livez` and `/readyz` on
`LECTIO_INTERNAL_ADDR`, `:8081` unless set. Workers can be added,
stopped and killed while parses run: a stopped worker gives its pages
back at once, and a killed one loses them after `LECTIO_TASK_LEASE`,
60 seconds unless set, to the workers that are left. With a
transaction-mode pooler in front of the database, name the pooler in
`LECTIO_DATABASE_POOL_URL` and keep the direct address in
`LECTIO_DATABASE_URL`. Every setting is in
[`specs/016-distribution.md`](specs/016-distribution.md).

The durable server does not describe figures yet.

## Build and deploy it

```sh
podman build -t lectiod .                                         # the server
podman build -f deploy/converter/Dockerfile -t lectio-convert .   # the conversion sidecar
```

The server's image holds `lectiod` and nothing else: no shell, no
office suite, 37 MiB. It runs as a user that is not root, with a
read-only root file system. [`docs/running.md`](docs/running.md) runs
it 3 ways: the development server in a container, the whole stack on
one machine with compose, and a cluster.

[`deploy/`](deploy/README.md) holds the cluster's manifests: a base
with one Deployment for the API and one for the workers, a component
that adds the sidecar with no way out to any network, and 2 example
overlays. The base names no host, registry or secret value; an
installation's overlay does. The manifests and the compose file are
written for the durable server with callers who sign in through an
issuer; the server takes one static token until that is wired in, as
the status above says.

A release publishes both images, signed and with a bill of materials
and provenance attested, and attaches the deploy tree with both images
pinned by digest.

## What it is for

A language model, a search index, or a workflow cannot use a scanned
contract or a 400-page report as a file. It needs the text in reading
order, the tables as tables, and a way to point back at the place on
the page a value came from. Hosted vision-language models read a page
well, and a single call is easy. Reading ten thousand pages for forty
tenants through a rate-limited API, without losing work to a restart
and without one tenant's backlog stalling the rest, is the part that
takes a system.

Lectio is that system, and nothing else: it does not serve models,
store files beyond the parse, or hold accounts. Identity comes from any
OpenID Connect issuer. Permission and limits come from an endpoint the
operator writes. Models are reached through any OpenAI-compatible
endpoint, a gateway included, with one key for every tenant or with a
key of each tenant's own that an endpoint of the operator issues, so
the gateway meters and bounds each tenant
([docs/running.md](docs/running.md#whose-key-reads-a-page)).

## Shape

```mermaid
flowchart LR
  caller["caller"] -->|"submit a file"| api["lectiod: API"]
  api --> pg[("Postgres: parses, tasks, fair queue")]
  api --> obj[("object store: sources, pages, documents")]
  pg --> w["lectiod: workers"]
  w -->|"one page per call"| model["model endpoint"]
  w --> obj
  api -. "verify" .-> issuer["OIDC issuer"]
  api -. "ask, receive limits" .-> authz["authorizer"]
  w -. "ask a tenant's key, optional" .-> keys["key endpoint"]
```

## License

Apache-2.0. See [LICENSE](LICENSE).
