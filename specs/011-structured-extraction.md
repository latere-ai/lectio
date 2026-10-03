---
title: "Structured extraction: fields shaped by a caller's schema, each citing the blocks it was read from"
status: drafted
track: core
depends_on:
  - specs/008-readers.md
  - specs/010-assembly.md
affects: [reader/, document/, internal/httpapi/, internal/extract/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Structured extraction

## Overview

A caller who wants an invoice number, a list of parties, or a table of
line items sends a JSON schema and gets an object in that shape. Each
value in it cites the blocks it came from, and through them a page and
a box, so a person or a program can check the value against the source.
Extraction is a request against a parse, not an option of a submit:
it runs after assembly, over the document, as one task per schema, and
reads no page. A new question about a document that is already parsed
costs one call to a text model.

## Current state

The earlier service accepted a schema in its request and failed every
such request: the extraction stage existed with chunking, merging and
repair retries, and had no model behind it. Its check that a result
fit the schema compared top-level types only. The stage's structure is
carried over. It gets a model through the reader configuration, a real
schema validator, and citations that resolve to blocks.

## Design

### Request

`POST /parses/{parse}/fields` ([[003-api]]) takes one extraction:

| Field | Meaning |
|---|---|
| `name` | identifies the result; unique within the parse, lower case, at most 63 characters |
| `schema` | a JSON Schema, draft 2020-12, whose root is an object |
| `instructions` | optional guidance for the model, at most 4,000 characters |
| `citations` | default `true` |
| `extractor` | the name of a configured extractor; absent, the routing policy decides |

The answer is `202` with the Field. A request may arrive while the
parse is still running; it waits for the document. A second request
with a name the parse already has is `409 conflict`. A schema is
checked when the request arrives. One that does not compile, exceeds
64 KiB, or nests deeper than 16 levels is `400 invalid_schema`.

Extraction was an option of a submit in the first draft of this spec,
with a `required` flag that let a failed extraction fail the parse.
Both are gone. Tying a question to the parse made every new question a
new parse, and a parse's state now says one thing, whether the pages
were read.

### How it runs

1. **Input.** The document as text in reading order, each block on its
   own line prefixed with its ref in brackets, repeated headers and
   footers left out, tables as HTML.
2. **Call.** One request to an extractor ([[008-readers]]), chosen by
   the request or by the policy's `extract` chain, which names a text
   model with the same adapter: the instructions, the schema, the
   text, temperature 0. The reply wraps the caller's object: `{"data": <object>,
   "citations": [{"pointer": "<JSON pointer>", "refs": ["<ref>"]}]}`.
   Citations travel as a list and not as an object keyed by pointer,
   because constrained decoding does not accept an object whose keys
   are not known in advance. The extractor returns them as a map from
   pointer to refs, which is the form a Field stores.
3. **Constrained replies.** When the reader's configuration allows it
   and the schema uses only what constrained decoding supports (no
   `$ref`, no `oneOf`, `anyOf`, `allOf`, no conditionals, no pattern
   properties), the schema is sent as the response format. Otherwise
   it is sent in the prompt. Which path ran is recorded.
4. **Validate.** `data` is validated against the caller's schema with
   a full validator. Citations whose ref does not exist are dropped;
   a citation's ref resolves to `{page, box}` when the result is read.
   A reply that ended at the model's output limit is not usable.
5. **Repair.** A reply that fails validation is retried up to twice
   with the validator's errors passed back to the model. These are attempts within the
   task; every attempt's tokens are counted.
6. **Write** `fields/<name>.json`.

### Long documents

A document that exceeds the text reader's input budget is extracted in
windows of consecutive sections, with the schema applied to each, and
merged: scalars take the first non-null value in reading order, arrays
concatenate with exact duplicates removed, objects merge by key.
Citations merge with the values. The merged object is validated as a
whole. This is a mechanical rule with known weaknesses, a value
restated differently on a later page among them, and it is stated in
the result: `"windows": 4`.

### Failure

An extraction that cannot produce a valid object after its repairs
writes a field result with `state: failed`, the error
`schema_not_satisfied` and the validator's last errors. The parse and
its document are not affected: the failure is the field's.

### Result

```json
{
  "name": "invoice", "state": "succeeded",
  "data": { "number": "INV-0042", "total": 1280.5 },
  "citations": { "/number": ["1.3"], "/total": ["2.7"] },
  "model": "...", "constrained": true, "attempts": 1, "windows": 1,
  "usage": { "input_tokens": 9100, "output_tokens": 210 }
}
```

`GET /parses/{parse}/fields/{name}?resolve=true` returns each citation
as `{ref, page, box}`.

### Running it again

Extraction reads only the document, so any number of schemas can be
run against one parse, at any time after it, with no page read again.
In the graph an `extract-*` task depends only on `assemble`
([[005-parse-graph]]).

## Not in this spec

Stored, named extraction configurations; extraction over images
instead of text; confidence per field; reconciling values across
documents. Classification of documents into types.

## Implementation status

Built:

- The `Extractor` interface, its request and result ([[008-readers]]).
- `reader/chat`: an extractor over chat completions that asks for the
  wrapped reply, sends the wrapped schema as the response format when
  asked to constrain, passes a validator's findings back for a repair,
  and reports a truncated or undecodable reply as invalid.
- `reader/stub`: a deterministic extractor for tests.
- The three `fields` routes are in the contract and answer `501
  not_implemented` ([[003-api]]).

Remaining: everything that runs an extraction. The text input with
refs, the schema check and the validator, the choice between a
constrained and a prompted schema, the repair loop, windows and their
merge, the field store, the routes' handlers, and the policy's
`extract` chain, which is read from the configuration and not applied.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| A schema that is invalid, too large or too deep is refused when the request arrives, with the reason | API tests |
| Two schemas requested against one succeeded parse, one after the other, produce two fields and no reader call | an end-to-end test counting reader calls |
| For a fixture invoice and a stub text model, the result validates against the schema and every citation resolves to a block whose text contains the value | an end-to-end test |
| A reply that violates the schema is repaired within two retries or the field is recorded `failed` with the validator's errors; tokens of all attempts are in usage | a test with a stub that fails then succeeds, and one that always fails |
| A failing schema leaves a field `failed` with `schema_not_satisfied`, and the parse and its document as they were | an end-to-end test |
| A schema using `oneOf` is sent in the prompt and the result records `constrained: false` | a unit test of the schema check |
| A document four times the input budget is extracted in windows and the merged object validates | a test with a stub text model |
| A cited ref that does not exist is absent from the result | a unit test |
