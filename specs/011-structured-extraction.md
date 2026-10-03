---
title: "Structured extraction: fields shaped by a caller's schema, each citing the blocks it was read from"
status: drafted
track: core
depends_on:
  - specs/008-readers.md
  - specs/010-assembly.md
affects: [internal/extract/, document/]
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
Extraction runs after assembly, over the document, as one task per
schema.

## Current state

The earlier service accepted a schema in its request and failed every
such request: the extraction stage existed with chunking, merging and
repair retries, and had no model behind it. Its check that a result
fit the schema compared top-level types only. The stage's structure is
carried over. It gets a model through the reader configuration, a real
schema validator, and citations that resolve to blocks.

## Design

### Request

Each entry of `extract` ([[003-api]]):

| Field | Meaning |
|---|---|
| `name` | identifies the result; unique within the parse |
| `schema` | a JSON Schema, draft 2020-12, whose root is an object |
| `instructions` | optional guidance for the model, at most 4,000 characters |
| `citations` | default `true` |
| `required` | default `false`; when `true`, failure of this extraction fails the parse |

A schema is checked at submit. One that does not compile, exceeds 64
KiB, or nests deeper than 16 levels is `400 invalid_schema`.

### How it runs

1. **Input.** The document as text in reading order, each block on its
   own line prefixed with its ref in brackets, repeated headers and
   footers left out, tables as HTML.
2. **Call.** One request through the policy's `extract` chain
   ([[008-readers]]), which names a text model with the same adapter:
   the instructions, the schema, the text, temperature 0. The reply
   schema wraps the caller's: `{"data": <schema>, "citations":
   {"<JSON pointer>": ["<ref>", ...]}}`.
3. **Constrained replies.** When the reader's configuration allows it
   and the schema uses only what constrained decoding supports (no
   `$ref`, no `oneOf`, `anyOf`, `allOf`, no conditionals, no pattern
   properties), the schema is sent as the response format. Otherwise
   it is sent in the prompt. Which path ran is recorded.
4. **Validate.** `data` is validated against the caller's schema with
   a full validator. Citations whose ref does not exist are dropped;
   a citation's ref resolves to `{page, box}` when the result is read.
5. **Repair.** A reply that fails validation is retried up to twice
   with the validator's errors appended. These are attempts within the
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
writes a field result with `state: failed` and the validator's last
errors. The parse still succeeds unless the schema was `required`, in
which case it fails with `schema_not_satisfied`. The document is
readable either way.

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

Extraction reads only the document. A later release adds
`POST /parses/{parse}/fields` to run a new schema against a parse that
already succeeded, with no page read again; the graph already allows
it, since an `extract-*` task depends only on `assemble`.

## Not in this spec

Stored, named extraction configurations; extraction over images
instead of text; confidence per field; reconciling values across
documents. Classification of documents into types.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| A schema that is invalid, too large or too deep is refused at submit with the reason | API tests |
| For a fixture invoice and a stub text model, the result validates against the schema and every citation resolves to a block whose text contains the value | an end-to-end test |
| A reply that violates the schema is repaired within two retries or the field is recorded `failed` with the validator's errors; tokens of all attempts are in usage | a test with a stub that fails then succeeds, and one that always fails |
| `required: true` on a failing schema fails the parse with `schema_not_satisfied` and the document remains readable | an end-to-end test |
| A schema using `oneOf` is sent in the prompt and the result records `constrained: false` | a unit test of the schema check |
| A document four times the input budget is extracted in windows and the merged object validates | a test with a stub text model |
| A cited ref that does not exist is absent from the result | a unit test |
