---
title: "Readers: the interfaces a model sits behind, the adapters, the page contract, validation, and the routing policy"
status: drafted
track: core
depends_on:
  - specs/002-object-model.md
  - specs/005-parse-graph.md
  - specs/007-model-capacity.md
affects: [reader/, internal/config/, internal/prompts/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Readers

## Overview

A reader turns one page image into blocks, and an extractor turns a
document's text into an object in the shape of a schema. The package
that holds the two interfaces is the only part of Lectio that knows a
model exists. This spec defines the interfaces, the adapters the first
version ships, what a model is asked and what its reply must look
like, how a reply is checked, and how a page is assigned a reader.
Swapping the model, or adding a second one for hard pages, is
configuration.

## Current state

The earlier service had a provider interface for page recognition with
one implementation, a client for a self-run OCR model reached at a
fixed address with a fixed token, plus stub interfaces for a
vision-language model and a text extractor that were never backed. The
interface shape is carried over. The implementation is replaced by an
adapter that speaks a common wire format to any endpoint, and the
stubs become real through the same adapter.

## Design

### The interfaces

There are two interfaces, one per kind of outbound call, in the public
package `reader`.

```go
package reader

type Reader interface {
    Describe() Description
    ReadPage(ctx context.Context, page Page) (Result, error)
}

type Description struct {
    Name    string          // the configured name: what a parse pins and a page result records
    Accepts []string        // media types of Page.Data, most preferred first
    Image   ImageSpec       // how a page is rendered for this reader
    Boxes   bool            // whether each block comes back with a position
    Kinds   []document.Kind // the kinds the reader can return; empty is any
}

type ImageSpec struct {
    DPI      int    // the resolution a paged format is rendered at
    LongEdge int    // bound on the longer side in pixels; 0 is none
    Format   string // "png" or "jpeg"
}

type Page struct {
    Number        int        // for logs and traces; a reader must not depend on it
    Data          []byte     // an image, or a PDF holding exactly this page
    MediaType     string
    Width, Height int        // pixels; zero for a PDF
    Languages     []string   // hints, may be empty
    Credential    Credential // the key this call is made with
}

type Result struct {
    Blocks    []document.Block // in reading order, Order from 1, Ref empty
    Model     string           // what the endpoint says answered
    Usage     document.Usage   // tokens, and cost and currency when the endpoint reports them
    Truncated bool             // the reply ended at the model's output limit
}

type Extractor interface {
    Describe() ExtractorDescription
    Extract(ctx context.Context, req ExtractRequest) (ExtractResult, error)
}

type ExtractorDescription struct {
    Name        string
    MaxInput    int  // characters of text in one call; 0 is no bound
    Constrained bool // whether decoding can enforce a schema
}

type ExtractRequest struct {
    Schema       json.RawMessage // the caller's JSON Schema, root an object
    Instructions string
    Text         string          // each block on its own line, its ref in brackets
    Citations    bool
    Constrain    bool
    Problems     []string        // a validator's findings on an earlier reply, to repair it
    Credential   Credential
}

type ExtractResult struct {
    Data        json.RawMessage     // the model's object, not yet validated
    Citations   map[string][]string // JSON pointer into Data, to block refs
    Model       string
    Usage       document.Usage
    Constrained bool
}
```

Both are stateless: one call in, one result out, no memory of the
document and no knowledge of queues, tenants or storage. `Describe`
makes no call. It is what lets the caller prepare a page for a reader
it knows nothing else about: render at this resolution, within this
long edge, in this format. A reader that returns text and no position
still fits: `Boxes` is false and its blocks have `box: null`. A reader
that takes a one-page PDF and not an image says so in `Accepts`.

`Credential` wraps the key a call is made with. It arrives per call
and is never part of a reader's configuration, because the key decides
who the model endpoint charges ([[013-limits-and-usage]]). Its value is
reachable only through `Reveal`: printing, logging or marshaling a
`Credential` shows a placeholder, so a key cannot reach an error, a
log line or a stored result by accident.

### Errors

Every error a reader or an extractor returns carries one class, so the
code that schedules work never looks at a status code or an endpoint's
error body.

| Class | Meaning | What the caller does ([[005-parse-graph]]) |
|---|---|---|
| `Retryable` | the same call may succeed later: a network error, a timeout, a 5xx | spends an attempt and tries again |
| `Invalid` | the model answered and the answer is not usable | spends an attempt; repeated invalid replies send the page to the next reader |
| `RateLimited` | the endpoint is describing its own capacity | waits, for `RetryAfter` when the endpoint said, and spends no attempt |
| `Budget` | the key's budget is spent | final for the page; says nothing about the reader |
| `Permanent` | this call will never succeed | not retried |

An error with no class is `Retryable`: an adapter that failed without
saying why has most likely met a transport problem, and a wrong guess
costs one attempt and not a page. `FromStatus` is the one mapping from
an HTTP response to a class, used by every adapter over HTTP so one
endpoint's `429` means the same as another's: `402`, or a body that
names a spent budget, is `Budget`; `429`, and `503` with `Retry-After`,
are `RateLimited`; `408`, `409` and `5xx` are `Retryable`; any other
status is `Permanent`.

### Adapters

An adapter is an implementation of one or both interfaces over one
wire format. The module ships three, and an adapter for another engine
lives outside the module and needs only the `reader` and `document`
packages.

| Adapter | Implements | Reaches |
|---|---|---|
| `chat` | Reader, Extractor | any endpoint that speaks OpenAI-compatible chat completions with image input |
| `layout` | Reader | an OCR or layout engine someone runs themselves, behind a small HTTP contract |
| `stub` | Reader, Extractor | nothing: its output is a function of its input |

**`chat`** sends one user message holding the instruction and the page
as an image, a JSON schema as the response format when the reader is
configured as constrained, temperature 0, and a bound on the output. That shape is
served by model gateways, by several providers directly, and by local
model servers, so one adapter reaches every model worth configuring,
and when the endpoint is a gateway the gateway owns the differences
between vendors.

**`layout`** posts `multipart/form-data` with one file field, `image`,
holding the page as a PNG or a JPEG, and an optional field,
`languages`, holding comma-separated hints. A key, when the call has
one, is sent as a bearer. The reply is

```json
{
  "elements": [
    { "category": "Table", "bbox": [120, 340, 980, 720], "text": "...", "reading_order": 3 }
  ],
  "model": "the engine's name for itself",
  "usage": { "input_tokens": 0, "output_tokens": 0 }
}
```

with `bbox` in pixels of the image that was sent, origin at the top
left. `category` is the engine's own word for the kind of region. A
table's text may be its HTML. `reading_order` may be left out, and then
the order of the list is the reading order. An engine that is still
loading answers `503`, with `Retry-After` when it knows how long.

**`stub`** returns the same three blocks for every page: a title
naming the page, a line holding a digest of the page's bytes, and a
page number. A test that knows the bytes knows the blocks, and a
development server with no model configured can still parse a file.
Its output describes the input and is not a reading of it.

A Reader document configures one reader:

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat                             # chat, layout or stub
  endpoint: https://gateway.example/v1      # any OpenAI-compatible base URL
  model: some-model                         # passed through; Lectio assigns it no meaning
  maxInFlight: 16
  requestsPerMinute: 0                      # 0 is unlimited
  timeout: 120s
  image: { dpi: 160, longEdge: 2048, format: png }
  constrained: true                         # send the response schema
  maxOutputTokens: 8192
```

No model name is compiled in. The example is an example. A `chat`
reader defaults to 160 dpi, a long edge of 2,048 pixels, PNG, 8,192
output tokens and a two-minute timeout; a `layout` reader to 200 dpi,
no bound on the long edge, PNG and a five-minute timeout, since an
engine that scales to zero may load its model on the first call.

### What the model is asked

Every instruction sent to a model is a template file in one package,
`internal/prompts`: `page.tmpl` for reading a page and `extract.tmpl`
for filling a schema ([[011-structured-extraction]]). The files are
compiled into the binary and parsed once at start. A prompt is rendered
when the call is made, from that call's data, and no adapter builds one
out of strings. Each prompt has one data struct, which is the list of
what its template may refer to:

| Prompt | Rendered with |
|---|---|
| `page` | the kinds, the grid, the page's language hints |
| `extract` | whether to cite, the caller's instructions, the schema, what an earlier reply got wrong, the document text |

What a caller supplies is written in as data and is never parsed as a
template. The reply schemas are not prompts: they are the wire
structure an adapter decodes and they stay with that code. A prompt is
versioned by a digest of its template and its fixed inputs, so an edit
changes the version and no number has to be raised by hand. The
version is meant to be part of the options fingerprint
([[005-parse-graph]]).

The page prompt, in substance:

- Return every region of the page as a block, in reading order.
- Give each block one kind from the closed set ([[002-object-model]]).
- Give each block's box as `[x0, y0, x1, y1]` on a 0 to 1000 grid.
- Transcribe text exactly. Do not summarize, translate, repeat, or
  invent text. Leave out what cannot be read.
- Write a table as HTML with `rowspan` and `colspan`, a formula as
  LaTeX, a figure as one sentence saying what it shows.
- Mark running headers, footers and page numbers with their own kinds.

The reply is one object, `{"blocks": [{kind, text, box, level}]}`, with
`kind` as an enumeration, `box` as integers and `level` an integer or
null. A model that returns the bare list is read too. The grid is
integers because models place integers more reliably than fractions;
the adapter divides by 1000. When the page has language hints, the
instruction ends by naming them. `chat.PromptVersion` is the page
prompt's version for this adapter's kinds and grid.

### Normalization

An adapter decodes its wire format into raw regions, `{label, text,
box, order, level, html}`, and calls `reader.Normalize` with the grid
its boxes are on, so every adapter's blocks mean the same thing.

| Step | Effect |
|---|---|
| the label is mapped onto the closed set of kinds by `KindOf`, which ignores case, hyphens and spaces and knows the names engines commonly use | a label nobody knows becomes `text`, flagged `kind_coerced` |
| the box is scaled to a fraction of the page | corners in the wrong order are swapped and coordinates outside the page are clamped, flagged `box_clamped`; a box with no area left is `box: null`, also flagged |
| a table's markup is read by `TableFromHTML` | the block gets its rows, columns and cells with their spans, keeps the markup verbatim, and takes the cells joined row by row as its text |
| a level is kept on a title or a heading only | 0 on every other kind |
| a region with no content is dropped | |

The blocks come back in reading order with `order` dense from 1. Refs
are set by the caller, which numbers the page.

### Validation

A reply is checked before it becomes a page result.

| Check | Where | On failure |
|---|---|---|
| parses as JSON, after the shared JSON repair for a wrapper object or a trailing fragment | the adapter | `Invalid` |
| the reply ended because the output limit was reached | the adapter | the last block is flagged `truncated` and the result says so |
| the page is not blank and the reply has no blocks | `reader.Check` | `Invalid` |
| one line makes up more than half of a reply of 20 lines or more, lines under 8 characters not counted | `reader.Check` | `Invalid`; this is the failure where a model loops |
| the numbered blocks fit the object model | the page step | `Invalid` |

Blocks are then sorted by the reader's order, renumbered densely, and
given their refs.

### Escalation

A page task whose reader has returned two invalid replies is sent once
to the next reader in the policy's chain, when the chain has one and
the parse did not pin its reader. The next reader gets attempts of its
own. That is the whole of quality routing in the first version. It
rests on a signal that exists, a reply that fails a check. Routing on
a quality score a model does not supply is left out until a reader can
supply one.

### Routing policy

```yaml
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read:    { chain: [default, strong] }     # readers tried in order
  extract: { chain: [text] }                # for structured extraction
  escalate: { onInvalid: 2, max: 1 }
```

Readers and the policy are documents in `LECTIO_CONFIG`: a file, or a
directory whose `.yaml`, `.yml` and `.json` files are read in name
order, each holding one or more documents. They are read at start and
re-read on `SIGHUP`; a document that does not validate keeps the
previous one in force and logs why. The policy's content hash is its
version. They are in the shape of declared objects so that a later
administrative API can apply the same documents; the first version has
no such API.

A configuration is taken whole or not at all. A document of another
kind or version, a member a spec does not have, a reader that cannot
be built, a name used twice, a second Policy, several readers with no
Policy to order them, and a chain that names a reader that is not
there are each an error, and nothing is loaded: a configuration that
half loads would read pages with a model nobody chose. One reader with
no Policy is its own chain. With no `LECTIO_CONFIG` at all, the
development server reads pages with the stub reader and says so at
start ([[016-distribution]]).

### Image preparation

The page task renders at the reader's `dpi`, scales down so the long
edge does not exceed `longEdge`, and encodes as the configured format.
Nothing else: no tiling, deskewing or binarization. The image written
to the object store is the image the model saw, which is what makes a
box drawn over it line up.

### The stub endpoint

`lectio-stubs` serves a deterministic chat endpoint for tests: it
returns blocks derived from the request's bytes, with switches to
delay, fail, rate-limit, loop, and exit the calling process. Every
durability and capacity criterion in this spec set runs against it.
It is a process of its own because those criteria kill and restart
the server that calls it. The `stub` adapter above is the same idea
inside one process: it takes a function that makes a chosen page fail,
wait or be rate limited, and counts its calls per page.

## Not in this spec

Adapters for a provider's native wire format, for sending a PDF page
as a document instead of an image, and for self-run recognition models
behind their own HTTP contract. Each is a new `adapter` value behind
the same interface. Prompt tuning per document type.

## Implementation status

Built:

- The `reader` package: both interfaces, `Description`, `Credential`,
  the five error classes with `FromStatus` and `FromTransport`,
  `Normalize`, `KindOf`, `TableFromHTML` and `Check`.
- `reader/chat` (Reader and Extractor), `reader/layout` (Reader) and
  `reader/stub` (Reader and Extractor).
- `internal/config`: Reader and Policy documents from a file or a
  directory, strict, refused whole on any error.
- `internal/prompts`: the two prompts as template files, rendered per
  call, each with a test that holds its full text.
- Escalation and the handling of each error class, in the in-process
  runner ([[005-parse-graph]]).

Remaining:

- Reload on `SIGHUP`: documents are read once, at start.
- `maxInFlight` and `requestsPerMinute` on a Reader, and `extract.chain`
  and `escalate` on the Policy, are read and not applied. The server
  names each at start. Capacity is [[007-model-capacity]], extraction
  is [[011-structured-extraction]], and escalation is fixed at two
  invalid replies and one next reader.
- A truncated reply is flagged and is not retried with a higher limit.
- A reply is not checked against the reply schema beyond decoding;
  with `constrained: true` the endpoint enforces it.
- `lectio-stubs`, and the tests that record and replay real exchanges.
- A prompt an operator can change without a build. The place for it is
  a `Prompt` document beside Reader and Policy, named from a Reader's
  spec and loaded with them. It is not designed here: prompt tuning is
  out of the first version.
- The prompt's version in the reuse fingerprint. A reader does not yet
  say which prompt version it reads with, so two parses under
  different instructions can still be taken for the same work.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| The `chat` adapter's request is accepted by a recorded OpenAI-compatible server, and its reply decoding handles a bare array, a wrapped array, and a truncated reply | adapter tests over recorded exchanges |
| Changing `model` or `endpoint` in a Reader document and sending `SIGHUP` changes which model reads the next page, with no restart and no lost task | an end-to-end test with two stub endpoints |
| Each validation row has a fixture reply that triggers it and the stated effect | a table test |
| A reader that fails validation twice sends the page to the next reader once, and a pinned parse never escalates | an end-to-end test |
| A credential does not appear in any formatted, logged or marshaled form of a page, a request or an error | a unit test over every such form |
| An engine that answers the `layout` contract reads a page with no change to Lectio | an adapter test against a recorded engine reply |
| An invalid configuration document leaves the previous one in force | a reload test |
| The stored page image is byte-identical to the image in the request the reader sent | a test comparing the object with the stub's received body |
| A fixture set of 50 real pages read through a configured live model yields blocks whose boxes overlap the page's text regions | an opt-in live test, run before a release and never in the per-push gate |
