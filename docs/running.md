# Running Lectio

3 ways to run `lectiod`, from the smallest to an installation: the
development server in a container, the whole stack on one machine with
compose, and a Kubernetes cluster. The commands use `podman`; `docker`
takes the same arguments.

## The image

```sh
podman build -t lectiod .
podman run --rm lectiod version
```

The image holds `lectiod` and nothing else: no shell, no package
manager, no office suite. It runs as user 65532 and writes no file, so
it runs with a read-only root file system and no capability.

## The development server

One process that keeps everything in memory, takes the token `dev`
(`LECTIO_DEV_TOKEN` names another), and reads pages with a stub that
calls no model:

```sh
podman run --rm --read-only --cap-drop ALL \
  -e LECTIO_DEV=true -p 127.0.0.1:8080:8080 lectiod
```

Nothing survives a stop. The [README](../README.md#run-it) walks through
the requests it answers.

## The stack on one machine

`deploy/examples/compose.yaml` runs the durable server: Postgres for the
parses and their tasks, an S3-compatible object store for the bytes,
`lectiod` in the role `all`, which is the API and a worker in one
process, and the conversion sidecar on a socket with no network.

The stack has no identity provider of its own, and `lectiod` does not
start without one to verify a caller against. Name yours when you start
it, and use a token it issued for the audience `lectio`. Without
`LECTIO_OIDC_ISSUERS` the command stops and says so:

```sh
LECTIO_OIDC_ISSUERS=https://issuer.example \
  podman compose -f deploy/examples/compose.yaml up --build
```

`lectiod` listens on `127.0.0.1:8080`. It reads the provider's keys
when it starts, and when the provider does not answer then, it says so
and reads them when the first token arrives.

A caller is the `sub` of its token at that issuer, and owns the files
and the parses it creates: nobody else reads or changes them. To let
some subjects read everyone's, name them in `LECTIO_ADMIN_SUBJECTS`,
each as `<issuer>|<sub>`. To have a service of your own decide instead,
with limits per caller, set `LECTIO_AUTHORIZER_URL` and
`LECTIO_AUTHORIZER_TOKEN`; the `lectiod` service of the compose file is
where to add either.

Upload a file and parse it. The second command holds its answer up to 30
seconds for the parse to end:

```sh
curl -s -H "Authorization: Bearer $TOKEN" --data-binary @sample.pptx \
  "http://127.0.0.1:8080/v1/files?name=sample.pptx"

curl -s -H "Authorization: Bearer $TOKEN" -H 'Prefer: wait=30' \
  -H 'Content-Type: application/json' -d '{"source":{"file":"fil_..."}}' \
  "http://127.0.0.1:8080/v1/parses"
```

The first answers with the file's `id`, which the second names. A
presentation goes through the sidecar, so a parse of one that ends with
`succeeded` has exercised every service of the stack.

Pages are read by the stub reader. To read them with a model, write a
file of Reader and Policy documents, mount it into the `lectiod`
service, and set `LECTIO_CONFIG` to its path and `LECTIO_MODEL_KEY` to
the key the endpoint takes.

## Reading a PDF's own text

Most PDFs carry their text. A reader of the `text` adapter reads a page
from it: the words the file holds, where it draws each and in what
type, and the ruling and the pictures around them. It calls no model
and needs no endpoint and no key, and its characters are the file's
own. Put it first in the chain, with a reader that calls a model after
it:

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: own }
spec: { adapter: text }
---
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: vision }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1
  model: your-vision-model
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read: { chain: [own, vision] }
```

A page the text reader reads costs no call. It says so: its `source` is
`text_layer`, it names its `reader` and no `model`, and its `usage` is
one page and no token. A page a model read says `reader`, as before.
Either counts as one page against a group's pages for a day.

The text reader declines a page it cannot read without guessing, and
the page goes to the next reader of the chain at the cost of one call:

- a scan, with or without a text layer laid under it by a recognition
  pass, and any page that is mostly picture;
- a table set without ruling, a form, and text that stands side by side
  and is not columns of prose;
- a page with a watermark or any other text set at an angle;
- a page whose font says nothing of what its glyphs are, or whose text
  does not read as text.

What it reads, it reads as print is laid out: paragraphs, headings by
the size and the weight of their type, list items, a table where
ruling closes every cell, and a figure where the page paints one. It
does not say what a figure shows; a request to describe a parse's
figures does. It writes a formula as the characters the file holds.

A parse that names its reader gets that reader alone. One that names
the text reader fails the pages it declines with `page_unreadable`,
and never calls a model.

Its Reader document takes an `image`, which is the image a result holds
of the page, 160 dpi and PNG unless set, and `maxInFlight`, which is 64
unless set: the pages it reads at once across every worker. A document
that names an `endpoint` or a `model` for it is refused.
[Quality](quality.md#without-a-model) has what it scores alone.

## Following a parse, and reading its failed pages again

The durable server streams a parse as server-sent events until it ends:
a `page` event for each page as it is read or fails, and the parse's
`progress` and `state` as they change.

```sh
curl -sN -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8080/v1/parses/prs_.../events"
```

Each event has an id. The server ends a stream it has held for 5
minutes, and a proxy may end one sooner: connect again with the header
`Last-Event-ID` set to the last id you saw, and the stream continues
after it, with nothing sent twice. Any replica of the API answers, and
the ids are the same after a restart. A stream that says nothing for 15
seconds sends a comment line, so a proxy with an idle timeout above
that leaves it open.

A parse that ended with pages that failed keeps the pages it read.
Reading the failed ones again is one request, and no other page is
read:

```sh
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8080/v1/parses/prs_.../retry"
```

The parse is `running` again, in the group it was submitted to and held
to that group's bounds, and ends when the pages have settled. A parse
with no failed page, and one that was canceled or ran out of time, is
answered `409`: submit the file again, and the pages that were read
are taken from the earlier read.

## What was used, and what waits

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8080/v1/usage?by=reader&interval=day"
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/v1/queue"
```

`/usage` sums the pages read, the model calls made for them and the
tokens those calls took in and gave out, by `group`, `owner` or
`reader`, over hours or days in UTC. A call that failed is counted as
the call it was. With no `from` it covers the last 24 hours, or the
last 30 days with `interval=day`.

`/queue` lists each group with its weight, its bounds, its parses that
have not ended and its queued and running tasks per class, the same for
each of its projects, and each reader's pool: the calls in flight
against its bound, whether its breaker admits calls, and the keys a
rate limit paused.

Under the owner policy a caller is answered its own usage and its own
group, and a subject of `LECTIO_ADMIN_SUBJECTS` everyone's. With an
authorizer, `usage.read` and `queue.read` are asked of it, with the
`owner` and the `group` the request names.

The development server answers `501` for these 4 routes: they are built
over the task store.

## Whose key reads a page

The key a model endpoint is called with decides who it charges. With
`LECTIO_KEYS=static`, the default, every page is read with the key of
`LECTIO_MODEL_KEY`, and whoever runs the server pays for every caller.

With `LECTIO_KEYS=endpoint` a page is read with a key of its group's
own, which a service of yours issues: a gateway then attributes each
tenant's spend to that tenant and bounds it by the tenant's budget. Set
`LECTIO_KEYS_URL` to the service and `LECTIO_KEYS_TOKEN` to the bearer
it requires, and set no `LECTIO_MODEL_KEY`. The durable server takes
this source; the development server does not.

| Process | `LECTIO_KEYS` | `LECTIO_KEYS_URL`, `LECTIO_KEYS_TOKEN` |
|---|---|---|
| role `worker`, role `all` | read | read, and both required with `endpoint` |
| role `api` | read | not read, and not required |

A process that runs tasks does not start with `endpoint` and no address
or no bearer, with `endpoint` and a `LECTIO_MODEL_KEY`, or with an
address or a bearer while the source is `static`. The API never holds
the bearer: it faces callers, and the bearer obtains every tenant's key.

A worker asks for a group's key when it first reads a page of the
group:

```
POST <LECTIO_KEYS_URL>
Authorization: Bearer <LECTIO_KEYS_TOKEN>
Content-Type: application/json

{"group": "...", "owner": "...", "parse": "..."}
```

| The service answers | The group's pages |
|---|---|
| `200` with `{"key": "...", "expires_at": "<RFC 3339>"}` | are read with the key. A worker holds it in memory, never writes it down, and asks again 1 minute before it expires, so a key has to be good for more than 1 minute when it is issued |
| `402` | fail with `budget_exhausted`: the group has no budget |
| `403` | fail with `reader_unavailable`: the group is issued no key |
| anything else, or nothing | wait in the queue. No page fails and none spends an attempt. The worker asks again after 1 second, then after twice as long each time, up to 30 seconds |

One request per group is in flight at a time, and a refusal is asked
again after 5 seconds. A worker whose key service is down stays ready.
With a key per group, a rate limit of the model endpoint pauses the
calls of the group whose key was limited and no other group's.

## Signing in and deciding

What `lectiod` needs set to serve callers, in each of its 3 setups:

| Setup | Set | Who is calling | Who decides |
|---|---|---|---|
| development | `LECTIO_DEV=true` | the holder of `LECTIO_DEV_TOKEN`, as the subject `dev` | the owner policy |
| durable | `LECTIO_OIDC_ISSUERS`, and `LECTIO_OIDC_AUDIENCE` when it is not `lectio` | a token one of the issuers signed for that audience | the owner policy, with `LECTIO_ADMIN_SUBJECTS` |
| durable, with an authorizer | the same, and `LECTIO_AUTHORIZER_URL` with `LECTIO_AUTHORIZER_TOKEN` | the same | the endpoint, asked with that bearer |

A durable process that serves the API does not start without an issuer,
and none starts with an authorizer's URL and no token for it. A worker
needs neither. An authorizer that does not answer does not stop the
server from starting: it is named in the log, and every request is
refused with `503 authorizer_unavailable` until it answers. An
authorizer that allows the probe the server sends at start is refused:
it does not read what it is asked.

Under the owner policy a submit's `priority` is 0 unless
`LECTIO_GROUP_DEFAULTS` names a bound, as in
`LECTIO_GROUP_DEFAULTS=max_priority=10`.

## What is kept, and for how long

The durable server keeps a file for 24 hours after it was last uploaded
and after the last parse that read it ended, and a parse with its pages,
images and document for 30 days after it ended. `LECTIO_FILE_RETENTION`
and `LECTIO_PARSE_RETENTION` set other times, as durations such as
`48h`, and an authorizer may shorten either for a caller. The workers
remove what has expired. The development server keeps everything until
it stops.

## The credentials of the example

Every credential in the file is an example and is written in it. The
stack is for one machine: only `lectiod`'s port is published, on the
loopback interface.

```sh
podman compose -f deploy/examples/compose.yaml down --volumes
```

removes the stack and what it stored.

## A cluster

[`deploy/`](../deploy/README.md) holds Kustomize manifests: a base with
one Deployment for the API and one for the workers, a component that
adds the conversion sidecar, and 2 example overlays. Each release
attaches the same tree as `deploy-<tag>.tar.gz`, with both images pinned
to the release by digest, and publishes the images as
`ghcr.io/<owner>/lectiod:<tag>` and `ghcr.io/<owner>/lectio-convert:<tag>`.

## Converting office documents

The sidecar that converts presentations, rich text and legacy
word-processing files has an image of its own:

```sh
podman build -f deploy/converter/Dockerfile -t lectio-convert .
```

It runs a large program on a file somebody else wrote, so it is run
with no network. The compose file gives it a socket on a volume it
shares with `lectiod`; the cluster manifests give it Pods of its own
under a policy that refuses every connection it opens.
