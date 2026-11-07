# Readers

For the operator who chooses how pages are read. It answers which models
and engines read pages, in what order they are tried, how a page falls from
one to the next, what a PDF's own text can do without a model, how to change
readers while the server runs, whose key pays for a read, and how to measure
a reader.

A **reader** turns one page into blocks. A **Policy** orders the readers.
Both are declared as documents, in a file or a directory that
`LECTIO_CONFIG` names ([Configuration](configuration.md#readers)), and can
also be added through the API while the server runs. A reader declared by a
`chat` document also describes figures and extracts objects under the same
name, so the same document serves all three.

## Declaring readers

`LECTIO_CONFIG` is a file, or a directory whose `.yaml`, `.yml` and `.json`
files are read in name order. Each file holds one or more YAML documents
separated by `---`. `lectiod` reads them once, when it starts. Without
`LECTIO_CONFIG`, a development server reads pages with a stub that calls no
model and says so in its log.

Every document has the same envelope:

| Member | Type | Meaning |
|---|---|---|
| `apiVersion` | string | Always `lectio.latere.ai/v1`. Any other value is refused. |
| `kind` | string | `Reader` or `Policy`. Any other kind is refused. |
| `metadata.name` | string | The reader's name, which a Policy, a parse and an authorizer refer to. Required for a Reader, and unique among the Readers. A Policy's name is not used. |
| `spec` | object | The members below. A member a spec does not have is refused. |

A configuration is taken whole or not at all. These each stop `lectiod` from
starting, and nothing is loaded:

- a document of another kind or version, or a member a spec does not have;
- a reader that cannot be built, and a name used twice;
- a second Policy, and no Reader at all;
- several Readers and no Policy to order them. One Reader with no Policy is
  its own chain;
- a chain that names a reader that is not there.

## The Reader document

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1
  model: your-vision-model
  timeout: 120s
  image: { dpi: 160, longEdge: 2048, format: png }
  constrained: true
  maxOutputTokens: 8192
  boxes: { order: xyxy, space: grid }
  maxInFlight: 16
```

No model name is compiled in. The example is an example. These are all the
members of a Reader's `spec`.

| Member | Type | Default | Meaning |
|---|---|---|---|
| `adapter` | string | none, required | `chat`, `layout`, `text` or `stub`. See [The adapters](#the-adapters). |
| `endpoint` | URL | none, required by `chat` and `layout` | For `chat`, the base URL of an OpenAI-compatible API, the part before `/chat/completions`. For `layout`, the URL a page is posted to. It must be an absolute `http` or `https` URL. A `text` reader takes none. |
| `model` | string | none, required by `chat` | Passed to the endpoint as the request's `model`. Lectio gives it no meaning. A `text` reader takes none. |
| `timeout` | duration | `2m` for `chat`, `5m` for `layout` | The time one call is given, written as `120s` or `3m`. It must be above zero. |
| `image.dpi` | integer | `160` for `chat` and `text`, `200` for `layout` | The resolution a page is rendered at. |
| `image.longEdge` | integer | `2048` for `chat` and `text`, no bound for `layout` | The longer side of the image in pixels. A larger render is scaled down to it. |
| `image.format` | string | `png` | `png` or `jpeg`. |
| `constrained` | boolean | `false` | `chat` only. Sends the reply schema as the response format, for an endpoint that enforces one. Without it, the schema is stated in the instruction alone. It applies to extraction too. |
| `maxOutputTokens` | integer | `8192` | `chat` only. Bounds the model's reply. A reply cut at the bound keeps its whole blocks and is marked cut. The page is not read again with a higher bound. |
| `outputLimitParam` | string | `max_completion_tokens` | `chat` only. The request member the bound is sent as: `max_completion_tokens` or `max_tokens`, for an endpoint that knows only the older name. |
| `temperature` | number | not sent | `chat` only. Sent as the request's temperature when set. Several current models refuse a request that names one, so it is not sent unless you set it. |
| `boxes.order` | string | `xyxy` | `chat` only. The order of a box's four numbers in the model's answer: `xyxy` for `[x0, y0, x1, y1]`, or `yxyx` for `[y0, x0, y1, x1]`. |
| `boxes.space` | string | `grid` | `chat` only. What the numbers measure: `grid`, a 1000 by 1000 grid over the page, or `pixels`, the pixels of the image as it was sent. |
| `maxInput` | integer | `400000` | `chat` only; any other adapter that sets it is refused. The most text one extraction call is given, in bytes. A longer document is read in windows of that size, at most 32, and the replies are merged. It must not be negative. |
| `maxInFlight` | integer | `8`; `64` for `text` | The reader's calls in flight across every worker. It must not be negative, and zero takes the default. |
| `cost` | whole number | `1` | What one call to this reader weighs in the fair queue. It is a weight between readers and not a price: set it higher on a large model than on a small one. It must be a whole number. |

How a model is asked for positions matters. Model families are trained on
different conventions, and a model asked in one it was not trained on places
boxes worse, or returns four valid numbers in the wrong order. The
convention is used to read the answer back, so a block's box is always
`[x0, y0, x1, y1]` as fractions of the page, whichever you chose.

A development server reads `maxInFlight` and `cost` and does not apply them,
and says so at start. A durable server applies both.

Members that belong to another adapter than the reader's are not all
refused. A `layout` reader loads with `constrained`, `model`, `boxes` and the
other `chat` members set, and ignores them. A `text` reader is refused if it
sets `endpoint` or `model`, and ignores the rest. Write only what the adapter
uses.

### The adapters

| Adapter | Reads a page by | Describes figures and extracts | Needs |
|---|---|---|---|
| `chat` | sending the page image to a model behind OpenAI-compatible chat completions | yes | `endpoint` and `model` |
| `layout` | posting the page image to an OCR or layout engine you run, behind a small HTTP contract | no | `endpoint` |
| `text` | reading the words, type and ruling a PDF itself holds, with no model call | no | nothing |
| `stub` | returning the same three blocks for every page: a title naming the page, a line with a digest of its bytes and a page number | yes | nothing |

`chat` reaches every model worth configuring, and when the endpoint is a
gateway, the gateway owns the differences between vendors. The prompts
`chat` sends are part of the binary. A Reader document cannot change them.

`layout` posts `multipart/form-data` with one file field, `file`, holding the
page as a PNG or a JPEG, and an optional `languages` field with
comma-separated hints. A key, when the call has one, is sent as a bearer. The
engine answers JSON:

```json
{
  "elements": [
    { "category": "Table", "bbox": [120, 340, 980, 720], "text": "...", "reading_order": 3 }
  ],
  "model": "the engine's name for itself",
  "usage": { "input_tokens": 0, "output_tokens": 0 }
}
```

`bbox` is in pixels of the image that was sent, origin at the top left.
`category` is the engine's own word for the kind of region, and a word
nobody knows becomes text. A table's text may be its HTML. `reading_order` may
be left out, and then the order of the list is the reading order. An engine
that is still loading answers `503`, with `Retry-After` when it knows how
long. The engine holds its own prompt.

`stub` is for tests and for a development server with nothing configured. It
is not a reading of the page, and the API refuses to store it.

A `chat` reader with no `model`, and a `chat` or `layout` reader whose
endpoint is not an absolute `http` or `https` URL, are refused when the
configuration loads.

### What changes a reader's results

Two readers with the same version read a page the same way, and a page that
was read once stands in for a second read of the same page. A change to
a reader's configuration that changes what it returns changes its version, so
earlier pages are not reused for new parses by it.

| Adapter | Its version covers |
|---|---|
| `chat` | the model, the page prompt as the reader asks it (including the box convention), `constrained`, `temperature`, `maxOutputTokens`, and the image's resolution, long edge and format |
| `layout` | the endpoint, and the image's resolution, long edge and format |
| `text` | the revision of its rules, every bound it judges a page by, and the image's resolution, long edge and format |
| `stub` | a constant |

## The Policy document

The Policy orders the readers. There is one.

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: pages }
spec: { adapter: chat, endpoint: https://gateway.example/v1, model: your-vision-model }
---
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: strong }
spec: { adapter: chat, endpoint: https://gateway.example/v1, model: your-strongest-model, cost: 3 }
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read: { chain: [pages, strong] }
  describe: { chain: [strong] }
  extract: { chain: [pages] }
```

| Member | Type | Default | Meaning |
|---|---|---|---|
| `read.chain` | list of reader names | the one configured Reader, when there is one | The readers a page is tried with, in order. A request that names no reader goes down this chain. A stored Policy must name at least one. |
| `describe.chain` | list of reader names | empty | The readers that describe a figure, tried in order. Each must be able to describe: a `chat` or `stub` reader. With no chain, a figure is described only by a describer a request names. |
| `extract.chain` | list of reader names | empty | The readers that fill a schema from a document's text, tried in order. Each must be a `chat` or `stub` reader. With no chain, an extraction runs only when its request names an `extractor`. |
An error names a chain whose reader is missing, and a `describe` or `extract`
chain whose reader cannot do the job: a `layout` or `text` reader named
there stops the load, and the message says so.

A Policy has no `escalate` member. When a page moves on replies that are not
usable is fixed, as [How a chain falls through](#how-a-chain-falls-through)
says, and a Policy that sets `escalate` is refused at load, and by
`PUT /policy` with `400 invalid_request`. A Policy stored through the API
by an earlier release that sets it no longer loads: the server logs the error
and runs on the configured readers and Policy alone until the stored Policy
is replaced or removed.

A development server has no task store. It does not apply `extract.chain`,
and says so at start.

The `chat` document in the first example gives three things under one name: a
reader of pages, a describer of figures and an extractor. They share the
endpoint, the model and one pool of calls in flight. To use one model to
read pages, another to describe figures and a text model to extract, declare
three Readers and name each in its chain. No document holds a key: every call
is made with the key described in [Whose key reads a
page](#whose-key-reads-a-page).

## How a chain falls through

A page is read by the first reader of `read.chain`. It moves down the chain
for three reasons, and for no other:

| What happened | What the page does |
|---|---|
| The reader **declined** the page: a model or a filter declined its content, or a `text` reader found no text it can read the page from. | Moves to the next reader. No limit on how far, no attempt spent, and nothing counted against the reader's health. With no next reader, the page fails `page_unreadable`. |
| The reader's endpoint **rejected the request**: a parameter it does not take, a model it does not have, a key it does not know. | Moves to the next reader, as far as the chain goes. With no next reader, the page fails `reader_unavailable`. |
| The reader answered **twice with a reply that is not usable**: it does not parse, holds no block for a page that is not blank, or loops. | Moves once. If the next reader cannot answer it either, the page fails `page_unreadable`. |

The reader a page moves to has attempts of its own. These do not move a page:

- A network error, a timeout or a `5xx` is tried again with the same reader,
  after a backoff, and spends an attempt. When the attempts are used the page
  fails `reader_unavailable`.
- A rate limit waits, for `Retry-After` when the endpoint gave one, and
  spends no attempt.
- A spent budget fails the page at once with `budget_exhausted`, and says
  nothing about the reader.
- A page this reader will never read, such as an image too large for it,
  fails at once with `page_unreadable`.

A parse that names its reader, or a caller whose authorizer gives it a
default reader, gets that reader alone: its pages never move. A parse that
names a `text` reader fails the pages the reader declines with
`page_unreadable`, and never calls a model.

A valid reply that transcribes a page wrongly is caught by none of these. To
find such a reader, measure it ([Measuring a reader](#measuring-a-reader)).

## Reading a PDF's own text

Most PDFs carry their text. A reader of the `text` adapter reads a page from
it: the words the file holds, where it draws each and in what type, and the
ruling and the pictures around them. It calls no model and needs no endpoint
and no key, and its characters are the file's own. Put it first in the
chain, with a reader that calls a model after it:

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
`text_layer`, it names its `reader` and no `model`, and its usage is one page
and no token. A page a model read says `reader` and `model`. Either counts as
one page against a group's pages for a day.

The text reader reads PDFs. A file of another format carries no text of its
own for it, and the page goes to the next reader.

It declines a page it cannot read without guessing, and the page goes to the
next reader of the chain at the cost of one call:

- a scan, with or without a text layer laid under it by a recognition pass,
  and any page that is mostly picture;
- a table set without ruling, a form, and text that stands side by side and is
  not columns of prose;
- a page with a watermark or any other text set at an angle;
- a page with a word the file holds and the page does not show: one under a
  bar painted over it, or one in the paper's color;
- a page whose font says nothing of what its glyphs are, or whose text does
  not read as text.

What it reads, it reads as print is laid out: paragraphs, headings by the
size and the weight of their type, list items, a table where ruling closes
every cell, and a figure where the page paints one. It does not say what a
figure shows. A request to describe a parse's figures does, with a reader of
the `describe` chain. It writes a formula as the characters the file holds.

Its Reader document takes an `image`, which is the image a result holds of the
page, and `maxInFlight`, the pages it reads at once across every worker. It
needs no more. A document that names an `endpoint` or a `model` for it is
refused. [Quality](quality.md#without-a-model) has what it scores alone.

## Changing readers while the server runs

The readers of `LECTIO_CONFIG` are the deployment's, and are read once when a
process starts. A change to those files takes a restart of every process.
There is no reload on a signal.

A subject of `LECTIO_ADMIN_SUBJECTS`, or one your authorizer allows
`reader.update`, adds more through the API. Every process of the durable
server reads with them within `LECTIO_READERS_POLL` and at once in the
process that took the change, with no restart
([Configuration](configuration.md#readers)).

```sh
curl -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"adapter":"chat","model":"vendor/another-model","timeout":"3m","boxes":{"order":"yxyx","space":"grid"}}' \
  "http://127.0.0.1:8080/v1/readers/another"
```

The body is the `spec` of a Reader document, in JSON. The rules:

- **Name.** One to 63 of `a-z`, `0-9`, `.` and `-`, starting with a letter
  or a digit, and not `policy`.
- **Endpoint.** It must be one of `LECTIO_READER_ENDPOINTS`. A body that names
  none takes the first. With the variable unset, the endpoints of the
  configured readers are the list, so an operator whose readers all reach one
  gateway lets an administrator add a model of that gateway and nothing else.
- **Adapters.** The `stub` adapter is refused.
- **Conflicts.** A name a configured reader holds is `409`. A configured
  reader is never changed or removed through the API.
- **Removing.** `DELETE /v1/readers/{name}` is `409` while a chain in force
  names the reader, or while a parse, a figure run or an extraction that
  named it has a task that has not ended.

`PUT /v1/policy` sets the order the readers are tried in, in the place of the
configured Policy, as a whole. Its body is the `spec` of a Policy document in
JSON, and it is checked as the file's is. `GET /v1/policy` reads the policy in
force and whether it is `configured` or `set`. `DELETE /v1/policy` puts the
configured Policy back. A change of the chains moves the work that has not
been claimed: a page waiting at a chain's first place is read by the new first
reader. Work already claimed finishes with its reader.

A process that cannot build what is stored, because it is older than the
binary that stored it and does not know an adapter or a member, keeps the
readers it had. If it starts, it reads with the configured documents alone,
and writes no settings, so the processes that can build the stored readers
keep the queue dispatching to them.

An authorizer may also give a caller a default reader: `reader` on the allow
of a submit, a figure run, an extraction or a set reads a request that names
none with it, as if the request named it. The routes are in [The
API](api.md), and the actions `reader.list`, `reader.update`, `reader.delete`
and the allow's `reader` member are in [The authorizer](authorizer.md).

A development server stores no reader and answers `501` on the routes that
change them.

## Whose key reads a page

The key a model endpoint is called with decides who it charges.
`LECTIO_KEYS` names where the key comes from
([Configuration](configuration.md#readers)).

- `static`, the default. Every page is read with the key of
  `LECTIO_MODEL_KEY`, and whoever runs the server pays for every caller.
- `endpoint`. A page is read with a key of its group's own, which a service of
  yours issues. A gateway then attributes each tenant's spend to that tenant
  and bounds it by the tenant's budget. Set `LECTIO_KEYS_URL` to the service
  and `LECTIO_KEYS_TOKEN` to the bearer it requires, and set no
  `LECTIO_MODEL_KEY`. The durable server takes this source. The development
  server does not.

| Process | `LECTIO_KEYS` | `LECTIO_KEYS_URL`, `LECTIO_KEYS_TOKEN` |
|---|---|---|
| role `worker`, role `all` | read | read, and both required with `endpoint` |
| role `api` | read | not read, and not required |

A process that runs tasks does not start with `endpoint` and no address or no
bearer, with `endpoint` and a `LECTIO_MODEL_KEY`, or with an address or a
bearer while the source is `static`. The API never holds the bearer: it faces
callers, and the bearer obtains every tenant's key. `LECTIO_KEYS` is read by
every role because it also decides whether a rate limit pauses a reader for
every group or only for the group whose key it was.

### The key endpoint's contract

A worker asks for a group's key when it first reads a page of the group:

```
POST <LECTIO_KEYS_URL>
Authorization: Bearer <LECTIO_KEYS_TOKEN>
Content-Type: application/json

{"group": "...", "owner": "...", "parse": "...", "models": ["..."]}
```

| Member | Meaning |
|---|---|
| `group` | The group the key is for. |
| `owner` | The owner of the parse the page belongs to. |
| `parse` | The parse's id. |
| `models` | The models the readers in force reach, by name, once each, in name order. It is left out when no reader reaches a model, such as when only `text` and `layout` readers are in force. The endpoint may scope the key to them. An endpoint that does not read the member answers as before. |

| The service answers | The group's pages |
|---|---|
| `200` with `{"key": "...", "expires_at": "<RFC 3339>"}` | are read with the key. A worker holds it in memory and never writes it down, and uses it until 1 minute before it expires. Issue a key that is good for more than that. |
| `402` | fail with `budget_exhausted`: the group has no budget. |
| `403` | fail with `reader_not_permitted`: the group is issued no key. |
| anything else, or nothing | wait in the queue. No page fails and none spends an attempt. |

"Anything else" covers a wrong bearer (`401`), a rate limit (`429`), a failure
of the endpoint (`5xx`), a request that times out or does not complete, an
answer that is not valid JSON, an answer with no key or no `expires_at`, and a
key that is expired or expires within the minute. A `401` is not a refusal of
the group: it says the bearer is wrong, which is yours to mend.

How a worker behaves:

- **One request per group** is in flight at a time. Calls for the group that
  arrive meanwhile wait for its answer and make none of their own.
- **A request is given 10 seconds.** The answer is read up to 64 KiB.
- **A key is held** under its group and the list of models, so a reader added
  through the API that reaches a model the key was not asked for gets a key
  asked for it.
- **A refusal is remembered for 5 seconds**, so the queued pages of a refused
  group fail on one request and not on one each. A group whose budget was
  raised reads again soon.
- **When the endpoint does not answer**, the worker asks again after 1 second,
  then after twice as long each time, up to 30 seconds. If the endpoint sent
  `Retry-After`, in seconds or as a date, the worker waits that long instead,
  up to 30 seconds.
- **A worker whose key endpoint is down stays ready.** The pages that need a
  key wait in the queue, and the worker runs every other task.

A figure and an extraction are read with the same key as the pages of their
parse, by the same rules: a `402` fails them with `budget_exhausted`, a `403`
with `reader_not_permitted`, and a service that does not answer leaves them
waiting. With a key per group, a rate limit of the model endpoint pauses the
calls of the group whose key was limited and no other group's.

To switch a cluster from `static` to `endpoint`, apply the Secret that holds
the address and the bearer, delete the Secret that holds `LECTIO_MODEL_KEY`,
set `LECTIO_KEYS` to `endpoint` in the settings, and roll both Deployments
([Installing](install.md#the-cluster-manifests)).

## Measuring a reader

A reader is judged by numbers and not by eye. [Quality](quality.md) has the
corpus of files whose content is known, the five measures that compare what a
parse returned with that content, and a bar for each.

- `make live` reads one real file with the reader you configure, through the
  server as it ships. Set `LECTIO_LIVE_CONFIG` to a file of Reader and Policy
  documents and `LECTIO_LIVE_FILE` to the file. `LECTIO_LIVE_PAGES` selects
  pages (`1-3` unless set), `LECTIO_MODEL_KEY` is the key the reader is called
  with, and `LECTIO_LIVE_OUT` is a directory the Markdown, each page's blocks
  and each page's image are written to. It calls a model, so it is never part
  of `make check`.
- `make live-quality` reads the whole corpus through the durable server with
  the reader that `LECTIO_LIVE_CONFIG` names, scores each file against its
  truth, writes `report.md` and `report.json`, and fails when a file is under
  the bar of its class.

```sh
LECTIO_LIVE_CONFIG=reader.yaml LECTIO_LIVE_FILE=paper.pdf make live
LECTIO_LIVE_CONFIG=reader.yaml make live-quality
```

Measure a reader before you put it in a chain, and again after you change its
model, its image settings or its box convention. The per-page record of which
reader and which model read it, and `GET /v1/usage?by=reader`, tell you what
a chain did on your own traffic ([The API](api.md)).
