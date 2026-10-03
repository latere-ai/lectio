---
title: "Readers: the interface a model sits behind, the chat adapter, the page contract, validation, and the routing policy"
status: drafted
track: core
depends_on:
  - specs/002-object-model.md
  - specs/005-parse-graph.md
  - specs/007-model-capacity.md
affects: [reader/, internal/readers/, internal/policy/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Readers

## Overview

A reader turns one page image into blocks. It is the only part of
Lectio that knows a model exists. This spec defines the interface, the
one adapter the first version ships, what the model is asked and what
its reply must look like, how a reply is checked, and how a page is
assigned a reader. Swapping the model, or adding a second one for hard
pages, is configuration.

## Current state

The earlier service had a provider interface for page recognition with
one implementation, a client for a self-run OCR model reached at a
fixed address with a fixed token, plus stub interfaces for a
vision-language model and a text extractor that were never backed. The
interface shape is carried over. The implementation is replaced by an
adapter that speaks a common wire format to any endpoint, and the
stubs become real through the same adapter.

## Design

### The interface

```go
package reader

type Reader interface {
    Name() string
    Accepts() Accepts            // media types, largest image edge, whether replies can be schema-constrained
    ReadPage(ctx context.Context, in Page) (Result, error)
}

type Page struct {
    Image     []byte
    MediaType string             // image/png or image/jpeg
    Width, Height int            // pixels
    Number    int
    Languages []string           // hints, may be empty
    Credential Credential        // the key this call is made with
}

type Result struct {
    Blocks []document.Block
    Model  string                // what the endpoint says answered
    Usage  Usage                 // input and output tokens; cost and currency when the endpoint reports them
}
```

A reader is stateless: one page in, one result out, no memory of the
document. Errors are classified by the reader into the four outcomes
of [[004-durable-tasks]] through sentinel errors (`ErrRetryable`,
`ErrPermanent`, `ErrRateLimited` with a delay, `ErrBudget`), so the
worker never inspects a status code.

### The chat adapter

The first version ships one adapter, `chat`. It sends an
OpenAI-compatible chat completions request: one user message holding
the instruction and the page as an image, a JSON schema as the response
format, temperature 0. That shape is served by model gateways, by
several providers directly, and by local model servers, so one adapter
reaches every model worth configuring, and when the endpoint is a
gateway the gateway owns the differences between vendors.

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1      # any OpenAI-compatible base URL
  model: gemini-3-flash                     # passed through; Lectio assigns it no meaning
  maxInFlight: 16
  requestsPerMinute: 0                      # 0 is unlimited
  timeout: 120s
  image: { dpi: 160, longEdge: 2048, format: png }
  constrained: true                         # send the response schema
```

No model name is compiled in. The example is an example.

### What the model is asked

The instruction is fixed text, versioned, and part of the options
fingerprint ([[005-parse-graph]]). In substance:

- Return every region of the page as a block, in reading order.
- Give each block one kind from the closed set ([[002-object-model]]).
- Give each block's box as `[x0, y0, x1, y1]` on a 0 to 1000 grid.
- Transcribe text exactly. Do not summarize, translate, repeat, or
  invent text. Leave out what cannot be read.
- Write a table as HTML with `rowspan` and `colspan`, a formula as
  LaTeX, a figure as one sentence saying what it shows.
- Mark running headers, footers and page numbers with their own kinds.

The response schema is an array of `{kind, text, box, level}` with
`kind` as an enumeration and `box` as four integers. The grid is
integers because models place integers more reliably than fractions;
the adapter divides by 1000.

### Validation

A reply is checked before it becomes a page result.

| Check | On failure |
|---|---|
| parses as JSON, after the shared JSON repair for a wrapper object or a trailing fragment | retryable |
| matches the schema | retryable |
| a box lies in range and is not inverted | the box is clamped or its corners swapped, and the block is flagged `box_clamped`; a block with no usable box keeps `box: null` |
| a kind is in the set | coerced to `text`, flagged `kind_coerced` |
| the reply ended because the output limit was reached | flagged `truncated`; retryable once with a higher limit |
| one line repeats past a threshold share of the reply | retryable; this is the failure where a model loops |
| the page is not blank and the reply has no blocks | retryable |

Blocks are then sorted by the reader's order, renumbered densely, and
given their refs.

### Escalation

A page task that has failed validation twice on one reader may be sent
once to the next reader in the policy's chain, when the chain has one
and the parse did not pin its reader. That is the whole of quality
routing in the first version. It rests on a signal that exists, a
reply that fails a check. Routing on a quality score a model does not
supply is left out until a reader can supply one.

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

Readers and the policy are documents in `LECTIO_CONFIG` (a file or a
directory), read at start and re-read on `SIGHUP`; a document that
does not validate keeps the previous one in force and logs why. The
policy's content hash is its version. They are in the shape of
declared objects so that a later administrative API can apply the same
documents; the first version has no such API.

### Image preparation

The page task renders at the reader's `dpi`, scales down so the long
edge does not exceed `longEdge`, and encodes as the configured format.
Nothing else: no tiling, deskewing or binarization. The image written
to the object store is the image the model saw, which is what makes a
box drawn over it line up.

### The stub reader

`lectio-stubs` serves a deterministic chat endpoint for tests: it
returns blocks derived from the request's bytes, with switches to
delay, fail, rate-limit, loop, and exit the calling process. Every
durability and capacity criterion in this spec set runs against it.

## Not in this spec

Adapters for a provider's native wire format, for sending a PDF page
as a document instead of an image, and for self-run recognition models
behind their own HTTP contract. Each is a new `adapter` value behind
the same interface. Prompt tuning per document type.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| The `chat` adapter's request is accepted by a recorded OpenAI-compatible server, and its reply decoding handles a bare array, a wrapped array, and a truncated reply | adapter tests over recorded exchanges |
| Changing `model` or `endpoint` in a Reader document and sending `SIGHUP` changes which model reads the next page, with no restart and no lost task | an end-to-end test with two stub endpoints |
| Each validation row has a fixture reply that triggers it and the stated effect | a table test |
| A reader that fails validation twice sends the page to the next reader once, and a pinned parse never escalates | an end-to-end test |
| An invalid configuration document leaves the previous one in force | a reload test |
| The stored page image is byte-identical to the image in the request the reader sent | a test comparing the object with the stub's received body |
| A fixture set of 50 real pages read through a configured live model yields blocks whose boxes overlap the page's text regions | an opt-in live test, run before a release and never in the per-push gate |
