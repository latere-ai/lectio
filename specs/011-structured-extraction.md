---
title: "Structured extraction: fields shaped by a caller's schema, each citing the blocks it was read from"
status: in-progress
track: core
depends_on:
  - specs/008-readers.md
  - specs/010-assembly.md
affects: [reader/, document/, internal/httpapi/, internal/extract/, internal/worker/, internal/store/]
effort: large
created: 2026-10-03
updated: 2026-10-04
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
costs one call to a text model. What the model is told is written so
that a value the document does not state comes back as missing and is
never made up, and so that text inside the document cannot pass for an
instruction.

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

The answer is `202` with the Field, `pending`. A second request with a
name the parse already has is `409 conflict`, and so is one for a parse
that holds 64 extractions, which is the most a parse holds. With no
extractor configured, or one named that is not, the answer is `400
reader_not_found`, and an extractor outside the readers the caller's
allow lets it name is `403 reader_not_permitted`
([[012-identity-and-authorization]]).

A schema is checked when the request arrives, and nothing is queued for
one that is refused. `400 invalid_schema` with the reason is the answer
for a schema that

- does not compile as draft 2020-12, or names another dialect in
  `$schema`;
- is larger than 64 KiB, or nests deeper than 16 levels, counting every
  object and every array of the schema document as a level;
- does not have the type `object` at its root;
- refers to anything outside itself. A `$ref` resolves inside the
  schema or not at all: a caller's schema never makes the server read
  a file or fetch an address;
- applies more than 256 subschemas to one value, or applies a subschema
  to the value it is itself applied to, without end;
- gives one `$dynamicAnchor` to 2 subschemas.

**What a check may cost.** A validator applies a schema to a value by
applying to the same value every subschema it reaches through `$ref`,
`$dynamicRef`, `allOf`, `anyOf`, `oneOf`, `not`, `if`, `then`, `else`
and `dependentSchemas`, and it keeps nothing of what it has applied. A
definition that applies the next one 2 times doubles the work at every
link: 40 such definitions fit in under 2,000 bytes, compile at once,
and cost 2^40 applications against `{}`. A validation that began cannot
be stopped, and it runs in a worker that holds a slot. So the work is
bounded before it begins, in 2 places.

When the schema arrives, each subschema is counted with everything it
applies to the same value, each as often as it is reached, the counts
taken once each by following the references. A count above 256 is
refused, and so is a subschema that reaches itself with no member and
no item in between, which would never end. A choice between 64
definitions counts 129, so the bound is above what a schema written to
describe a document needs. A keyword that moves to a member or an item,
`properties`, `patternProperties`, `additionalProperties`,
`propertyNames`, `items`, `prefixItems`, `contains` and the 2
`unevaluated` keywords, starts a new count: a schema that recurs
through them is taken, and is applied as often as the object nests.
With 1 subschema under a dynamic anchor a `$dynamicRef` to it is the
reference it reads as. With 2 the object would decide which one is
applied, and the count could not be taken from the schema.

That count is not the whole cost. A schema can describe one member 2
times, by 2 patterns that both match its name or by 2 subschemas that
each name it, with nothing doubled at any one value. The work then
doubles with every level the object nests, and an object 40 levels
deep is 240 bytes that a document can make a model write. No reading of
the schema alone bounds that, short of refusing schemas that are
honest: a choice between 2 shapes that both hold a list of the same
node is one. So when a reply arrives, the applications the validator
would make of this schema to this object are counted first, in time
that grows with the object and not with that count, and an object that
would take more than 2,097,152 of them is not held to the schema. The
field then fails with `schema_not_satisfied`, at once and with no
repair, since another call would end the same way. The validator makes
that many applications in under 1 second, and a reply of 4,000 objects
held to a choice between 8 shapes each takes less than a quarter of
them.

A `pattern` is matched by an engine that runs in time linear in the
text, with no backtracking, so no pattern needs a bound of its own.

**When it runs.** A request may arrive while the parse is still
running. It then waits, with no task, until the parse ends, and its
task is queued in the transaction that ends the parse, whichever way
the parse ends. An extraction runs over the document the parse ended
with, which is every page that was read: a parse that was canceled, ran
out of time or failed has a document ([[003-api]]), and a question
about it is answered from that document. A cancel stops the reading of
pages. It is not a cancel of the questions asked of what was read, for
which there is no route, and which cost one call each.

An extraction has as long as a parse submitted with no deadline has,
`LECTIO_MAX_DEADLINE`, counted from when its task is queued. Past it
the field fails with `deadline_exceeded`, so an extraction pinned to an
extractor that never admits a call does not wait without bound
([[004-durable-tasks]]).

Extraction was an option of a submit in the first draft of this spec,
with a `required` flag that let a failed extraction fail the parse.
Both are gone. Tying a question to the parse made every new question a
new parse, and a parse's state now says one thing, whether the pages
were read.

### How it runs

1. **Input.** The document as text in reading order, each block led by
   its ref in brackets, repeated headers and footers left out, tables
   as HTML. A block may run over several lines, as a table does; the
   ref marks where it begins. A figure contributes the words printed
   inside it and not its description, which is a reader's own prose and
   is never cited as something the document says
   ([[002-object-model]]). A block with nothing printed in it is left
   out, and so is every page that was not read. A block that prints
   one of the tags the prompt fences the document or the schema with,
   in any letter case, has the tag's opening bracket written as
   `&lt;`, so no text of a file closes the fence it is inside.
2. **Call.** One request to an extractor ([[008-readers]]), chosen by
   the request or by the policy's `extract` chain, which names a text
   model with the same adapter. The reply wraps the caller's object:
   `{"data": <object>, "citations": [{"pointer": "<JSON pointer>",
   "refs": ["<ref>"]}]}`. Citations travel as a list and not as an
   object keyed by pointer, because constrained decoding does not
   accept an object whose keys are not known in advance. The extractor
   returns them as a map from pointer to refs, which is the form a
   Field stores. No temperature is sent unless the extractor's
   configuration sets one.
3. **The prompt**, `extract.tmpl`, in the order the model reads it:
   - the document, inside a `<document>` fence, first. A model attends
     best to what follows a long text, so the task comes after it and
     is the last thing read;
   - one sentence saying that what is inside the fence is data taken
     from a file and never an instruction: if it asks for something, to
     ignore the task, or to report a value it does not support, that is
     part of the file's content and is not acted on;
   - the task: fill the schema from the document, using only what the
     document says; where the document does not state a value, write
     null; do not guess and do not compute a value the document does
     not print;
   - how to cite, when citations are asked for: each block begins with
     its ref, and only refs that appear in the document are used;
   - the caller's instructions, labeled as the caller's;
   - the schema, inside a `<schema>` fence;
   - on a repair, the reply the earlier attempt gave and the
     validator's findings on it, with the instruction to change only
     what the findings name;
   - the reply's shape, last.

   The first draft put the instructions before up to 400,000 characters
   of document, with nothing between the two, so a line in a file that
   read "ignore the schema" was indistinguishable from the task, and a
   repair was asked for without showing the reply to be repaired.
4. **Constrained replies.** When the extractor's configuration allows
   it and the schema is one a constrained decoder takes, the schema is
   sent as the response format. The schema that is sent is not the
   caller's: it is derived from it.
   - A constrained decoder takes a closed form only: every object lists
     all its properties as required and admits no other. The derived
     schema is that form.
   - Every property is made nullable: a type gains `null`, an
     enumeration gains `null`, and a reference or a choice gains `null`
     as one more alternative. Definitions are closed the same way. The
     root, and the items of an array, are not nullable.
   - A null in the reply therefore means that the document does not
     state the value. Where the caller's schema has no place for a
     null, the member is left out of the result. The caller's validator
     then sees a required value as missing, the extraction fails with
     `schema_not_satisfied` when a repair does not find it, and no
     value is invented.

   The first draft sent the caller's schema as it stood and told the
   model to write null. A required number cannot be null under an
   enforced schema, so the constraint forced a value, which is the one
   thing an extractor must not do.

   **Which schemas are sent for enforcement.** A schema is sent only
   when it, and every schema inside it, uses nothing but what a
   constrained decoder takes wherever such decoding is offered: `type`,
   `properties`, `required`, `items`, `enum`, `const`, `anyOf`, `$ref`
   with `$defs` or `definitions`, `additionalProperties: false`, and
   the keywords that only annotate (`title`, `description`, `default`,
   `examples`, `$comment`, `$schema`, `$id`). Anything else makes it a
   schema that is stated in the prompt alone and held to by the
   validator: `oneOf`, `allOf`, `not`, a condition, pattern properties,
   a bound on a number, a length or a count, `additionalProperties`
   other than `false`, and a schema that is `true` or `false`. The list
   is of what is taken, and not of what is refused, because an endpoint
   that enforces a schema rejects the whole request for a keyword it
   does not know, and that fails every call of the extraction. Which
   path ran is recorded as `constrained`, which says the schema was
   sent for enforcement; an endpoint that ignores a response format
   cannot be told from one that honors it.
5. **Validate.** `data` is validated against the caller's schema with
   a full validator of draft 2020-12, and not by comparing the types of
   the root's members. A finding names where in the object the value
   is, as a JSON pointer, where in the schema the rule it broke is, and
   what is wrong. Citations are then cut to the ones that stand: a ref
   that is no ref of the text the call was given is dropped, and so is
   a citation whose pointer names no value of the object, such as a
   member that was left out for being null. A citation's ref resolves
   to `{ref, page, box}` when the result is read with `resolve=true`. A
   reply that ended at the model's output limit is not usable.
6. **Repair.** A reply that fails validation is sent back up to 2 times,
   with the reply and the validator's findings (`ExtractRequest.Previous`
   and `Problems`) and the same text. A repair is a step of the
   extraction and not a failed attempt of its task: the reader answered,
   and nothing waits a backoff. The field's `attempts` is 1 and 1 more
   for each repair, and every call's tokens are in its `usage`.
7. **Write** the field under a key that carries the task's lease
   token, `fields/<name>.<token>.json`, and record the key at the
   settle ([[004-durable-tasks]]).

The validator is a library, `github.com/santhosh-tekuri/jsonschema`,
version 6: pure Go, under the Apache-2.0 license, with the whole of
draft 2020-12 and its test suite. Writing the subset by hand would have
been a second validator to keep right. It is compiled with a loader
that loads nothing, which is what makes a `$ref` out of the schema an
error and not a fetch, and with the regular expressions of the Go
standard library, which run in time linear in their input.

### Capacity and charge

An extraction calls a model, so it is dispatched like any work that
does. An `extract-<name>` task takes a slot in its reader's pool for
each call it makes, a window or a repair, and not one slot for its
whole lease, and it is charged its reader's `cost` per call
([[006-fairness-and-priority]], [[007-model-capacity]]). The first
draft dispatched it with no slot and no charge, so a group that queued
extractions was never charged and never waited for a pool.

**One call a claim.** The task makes one model call each time it is
claimed. A claim that has another call to make, the next window or a
repair, writes what the extraction has so far under its own token,
`fields/<name>.<token>.progress.json`, and settles with the outcome
`continue`, which names that key. The store returns the task to the
queue at once, with no attempt spent and its attempts as a new task has
them, and the next claim carries the key ([[004-durable-tasks]]). So:

- each call is admitted by its reader's pool as a page is, holds its
  slot for the length of the call, and holds none between 2 calls;
- each call is charged at its claim, and takes its group's turn in the
  fair queue, so a long extraction is interleaved with other groups'
  work between its windows and never runs ahead of its share;
- a rate-limit reply pauses the key scope between 2 calls with what was
  extracted so far kept;
- a worker that dies, or stops, loses one call and not the calls before
  it: the task comes back with what the last step kept.

The first claim reads the document, cuts it for the extractor it was
claimed for, and writes the cut once, `fields/<name>.<token>.input.json`.
Every later claim reads its window from there, so the calls of one
extraction are over one reading of the document, and the pages are
read from the object store once.

An extraction that has made a call stays with the extractor that made
it. A page that finds its reader paused or its breaker open passes over
it to the next of the chain, and an extraction does too until its first
call has been answered. From then on what it has was cut for that
extractor's input, and another extractor would cut the document again
and start the calls over, so an extraction between 2 extractors whose
pause or breaker comes and goes would be billed for the same windows
each time it changed hands. So the step that keeps the first reply
also pins the task to its extractor: it waits while that extractor is
paused, shut or full, as a task whose request named an extractor does,
and until its deadline. It leaves the extractor only by the 2 outcomes
that move a page down the chain ([[005-parse-graph]]): the extractor
declined the document or its endpoint rejects the request, or its
second reply in a row was not usable. It then goes to the extractor
after, cuts the document again for that one, whose bound on a call's
text is its own, starts its calls over, and stays there. A chain is
walked in one direction, so an extraction starts over at most once for
each extractor of it.

The task is the parse's: it runs in the parse's group and project, in
its class and at its priority, behind the parse's own `prepare` and
`assemble` and ahead of the group's pages of that priority. It is
claimed for the extractor its request named, which pins it, for the
extractor that made its first call, or, before that call, for the
first extractor of the policy's `extract` chain that is not passed over
([[007-model-capacity]]).

### Whose key, and what it is held to

A call is made with the key of the parse's group, resolved as a page's
is ([[013-limits-and-usage]]): a key endpoint that does not answer
leaves the task waiting behind its group's paused scope, a `402` fails
the field with `budget_exhausted`, and a `403` with
`reader_not_permitted`.

An extraction reads no page, so it counts nothing against a group's
pages for a day, and a group with none left may still ask. What bounds
it:

| Bound | What it holds |
|---|---|
| the group's `max_running` | the claim counts leased tasks of every kind, so an extraction waits as a page does |
| the allow's `readers` | the extractors a caller may name |
| 64 extractions a parse | how many a caller may queue against one document |
| 32 windows an extraction | what a document of any length can cost: more is `too_many_pages` |
| 2 repairs a window | what a schema a model never satisfies can cost |
| `LECTIO_MAX_DEADLINE` | how long one waits for a model |
| the key's own budget, at the gateway | what the calls cost |

`max_queued` does not: it counts parses that have not ended, and an
extraction adds none.

### Long documents

A document that exceeds the text reader's input budget is extracted in
windows of consecutive sections, with the schema applied to each, and
merged: scalars take the first non-null value in reading order, arrays
concatenate with exact duplicates removed, objects merge by key.
Citations merge with the values. The merged object is validated as a
whole. This is a mechanical rule with known weaknesses, a value
restated differently on a later page among them, and it is stated in
the result: `"windows": 4`.

**The cut.** The budget is the extractor's `MaxInput`, the most text
one call takes, in bytes ([[008-readers]]); an extractor with none
reads the document in one window. A window holds whole sections, a
section being a title or a heading and what follows it, as many as fit.
A section that does not fit in what is left of a window begins the
next. A section longer than a window is cut between 2 of its blocks,
and a block longer than a window in its text, at a character's
boundary, each part led by the block's ref. An extraction reads at most
32 windows. A document that takes more fails the field with
`too_many_pages` before any call is made.

**What a window is held to.** A window's reply is validated when it
arrives, against everything in the schema but what another window may
hold: a required member, and the least a list or an object may hold
(`required`, `minItems`, `minProperties`, `contains`, `minContains`,
`dependentRequired`). A reply that fails the rest is repaired with its
own window, up to 2 times. The merged object is then held to the whole
schema. One that fails is not repaired, and the field fails: no single
call reads the whole document, so none could mend it.

**The merge and citations.** A citation follows its value: one of a
list's item moves with the item to its place in the joined list, one of
an item that was already there is added to that item's, one of a value
a later window states the same is added, and one of a value that did
not stand is dropped. A window's reply may cite only the refs of that
window.

### Failure

An extraction that cannot produce a valid object after its repairs
ends with `state: failed`, the error `schema_not_satisfied` and the
validator's last errors. The parse and its document are not affected:
the failure is the field's. The errors are in `error.detail` as the
rules of the schema that were broken, each by its place in the schema,
`#/properties/total/type`, and how many places broke one. A value of
the reply is never in it: a reply holds content of the document, and an
error is a row ([[002-object-model]]).

What an extraction does about a failed call follows from the class of
the extractor's error, as a page's does ([[005-parse-graph]]), and what
a field can fail with is:

| The field's error | When |
|---|---|
| `schema_not_satisfied` | the object did not satisfy the schema after its repairs; the merged object of a document in windows did not; the object would take more than 2,097,152 applications of the schema to check; or no extractor could take the document: it was declined by every extractor, or its replies were never usable |
| `too_many_pages` | the document's text takes more than 32 windows |
| `reader_unavailable` | the extractor could not be reached within the task's attempts, or its endpoint rejects the request itself |
| `budget_exhausted` | the key's budget is spent, at the gateway or at the key endpoint |
| `reader_not_permitted` | the key endpoint issues the group no key |
| `deadline_exceeded` | the extraction did not end by its deadline |
| `internal` | a fault of the server's own, or a task that ended the workers that ran it |

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

A field is `pending` from its request until its task ends, whether it
waits for its parse to end, for its turn, or for a model, and a read of
it then answers `200` with its name and its state. It is `succeeded`
with `data` and `citations`, or `failed` with `error` and no `data`.
`model`, `constrained`, `attempts` and `windows` are known once it has
ended, and `usage` holds the tokens of every call it made, the ones
that failed included. A parse with no field of the name answers `404
not_found`, and a field goes with its parse: after the parse was
deleted, or its retention ended, the answer is `404 parse_not_found`.

**Where it lives.** A field is a row and an object. The row, in
`fields`, holds what was asked, where the field stands, its error, what
its calls used, and the key of its result. The object holds `data` and
`citations`, which are content. A field that failed has no object. What
was asked, the schema, the instructions and the citations flag, is kept
as one JSON document of text and handed to the task as text, so a
schema reaches the model byte for byte, its members in the order the
caller wrote them, which is the order a model is asked for them in.

### Running it again

Extraction reads only the document, so any number of schemas can be
run against one parse, at any time after it, with no page read again.
In the graph an `extract-*` task depends only on the parse having ended
([[005-parse-graph]]), and it is added when a caller asks, not by the
submit.

**What ends it.** A delete of the parse, and the end of the parse's
retention, drop its extractions with it: a task that is queued or
running is removed first, in the transaction that removes the parse, so
its settle is refused and no counter drifts. A retry of the parse
([[003-api]]) is refused while an extraction of it is queued or
running, since a retry writes again the pages the extraction reads. A
field that was filled before a retry stays as it was: it was read from
the pages the parse had then, whose refs do not change.

### Configuration

An extractor is no document of its own. A Reader document
([[008-readers]]) whose adapter reaches a model that takes an
instruction, `chat`, gives a reader, a describer and an extractor under
one name, reaching the same model with the same parameters and sharing
one pool. `maxInput` on the document is the most text one extraction
call is given, in bytes, 400,000 unless set. The Policy's
`extract.chain` names the documents an extraction is tried on, in
order; naming one whose adapter cannot extract is an error. With no
`extract` chain an extraction runs only when its request names an
`extractor`, as a figure with no `describe` chain is described only by
a describer a request names. No document holds a key.

One chat model for pages, figures and extractions is one document that
the 3 chains name, or one document for each:

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec: { adapter: chat, endpoint: https://gateway.example/v1, model: some-model }
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read:     { chain: [default] }
  describe: { chain: [default] }
  extract:  { chain: [default] }
```

A separate kind of document for an extractor was the alternative. It
would have given a text model a name that is no reader's, and a second
place for an endpoint, a model and a timeout that mean the same in
both. The pool is what decided: a model's capacity is one number
whoever calls it, and one document is one pool.

## Not in this spec

Stored, named extraction configurations; extraction over images
instead of text; confidence per field; reconciling values across
documents. Classification of documents into types.

## Implementation status

Built:

- The `Extractor` interface, its request with `Previous` and
  `Problems`, and its result ([[008-readers]]).
- `internal/prompts/extract.tmpl`: the prompt as described above, with
  a test that holds its full text and one that shows a caller's text
  is written in as data.
- `reader/chat`: an extractor over chat completions that asks for the
  wrapped reply; derives the closed, nullable schema and sends it as
  the response format when asked to constrain; leaves out of the
  result a null the caller's schema has no place for; passes the
  earlier reply and a validator's findings back for a repair; and
  reports a truncated or undecodable reply as invalid.
- `reader/stub`: a deterministic extractor for tests.

- `internal/extract`: the document as text with refs, the check of a
  schema and the validator, which schemas are sent for enforcement, the
  cut into windows and the merge of their replies, which citations
  stand, and the steps of an extraction from one call to the next. It
  reaches nothing and holds no state.
- `internal/worker`: the `extract` task, one call a claim, with the
  group's key, the input and the progress kept under the claim's token,
  and the class of the extractor's error deciding the settle.
- `internal/store/postgres`, migration `000006`: the `fields` table,
  `lectio_field_create`, the task queued when the parse ends, the
  outcome `continue`, the deadline, and the delete that drops the task.
- `internal/httpapi` and `internal/durable`: the 3 routes in the durable
  server, each asking its action about the stored parse.
- `internal/config`: an extractor from a Reader document, `maxInput`,
  and the Policy's `extract.chain` applied.
- The key of the group, the pool's slot per call, the charge per call
  and the meter under the kind `extract` ([[013-limits-and-usage]]).

Remaining:

- A development server answers `501 not_implemented` for the 3 routes:
  an extraction is a task of the task store, which keeps what it has
  between 2 of its calls, and the in-process runner has none
  ([[003-api]]).
- The opt-in live test of the row about a line written as an
  instruction: the row is proven against an endpoint that obeys
  whatever it finds outside the fence, and not against a model.
- A merged object that fails the whole schema is not repaired.
- The words a describer found printed in a figure are part of an
  extraction's text, as they are of the block's `text`. An extraction
  asked before the figures were described does not see them.
- The resource keeps the name `fields` ([[003-api]]).

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| A schema that is invalid, too large or too deep is refused when the request arrives, with the reason | `TestASchemaIsCheckedWhenAnExtractionIsAsked`, through the API over the durable backend: nothing is queued and no model is called; `TestASchemaIsCheckedWhenItArrives` of `internal/extract`, with a reference to an address, to a file and to a sibling document among the refused |
| A schema of 40 definitions that each apply the next one 2 times is refused with `invalid_schema` in well under 1 second, and so is every schema that applies more than 256 subschemas to one value or applies itself without end; a schema that recurs through its members and items, and one that shares its definitions, are taken and held | `TestASchemaThatDoublesItsWorkIsRefusedWhenItArrives`, `TestASchemaThatAppliesItselfWithoutEndIsRefused`, `TestASchemaThatRecursThroughItsMembersIsTakenAndHeld`, `TestASchemaThatSharesItsDefinitionsIsTaken`, `TestADynamicAnchorNamesOneSubschema` and `TestASubschemaInAnotherDialectIsRefused` of `internal/extract`; `TestASchemaIsCheckedWhenAnExtractionIsAsked` through the API |
| With 2 extractors in the chain, an extraction whose first call was answered by the first waits when that one is paused between 2 claims, while an extraction that has made no call is taken by the second; it is claimed for the first again when the pause ends, and moves to the second, once, only when the first declines it or its replies are not usable | `TestAnExtractionStaysWithTheExtractorThatBeganIt` at the store, with a virtual clock |
| A reply that would take more than 2,097,152 applications of its schema to check is not held to it: the field fails `schema_not_satisfied` after 1 call, with no repair, and the claim returns its slot | `TestAnObjectThatWouldCostTooMuchToCheckIsNotHeldToTheSchema` and `TestTheCountOfACheckIsWhatTheValidatorAppliesAtMost` of `internal/extract`; `TestAReplyThatWouldCostTooMuchToCheckFailsTheFieldInOneCall` of `internal/worker` |
| Two schemas requested against one succeeded parse, one after the other, produce two fields and no reader call | `TestTwoSchemasAreExtractedFromOneParseAndNoPageIsReadAgain`, through the API over the durable backend, with a reader that counts its calls |
| For a fixture invoice and a stub text model, the result validates against the schema and every citation resolves to a block whose text contains the value | the same test, reading each citation with `resolve=true` and the block it names |
| A reply that violates the schema is repaired within two retries or the field is recorded `failed` with the validator's errors; tokens of all attempts are in usage | `TestAReplyThatViolatesTheSchemaIsRepairedOrTheFieldFails`, through the API; `TestAReplyThatFailsValidationIsRepairedInTheNextClaim` of `internal/worker`; `TestAReplyThatFailsIsRepairedTwiceAndThenTheFieldFails` of `internal/extract` |
| A failing schema leaves a field `failed` with `schema_not_satisfied`, and the parse and its document as they were | `TestAReplyThatViolatesTheSchemaIsRepairedOrTheFieldFails` |
| A schema using `oneOf` is sent in the prompt and the result records `constrained: false` | `TestASchemaADecoderCannotEnforceIsStatedInThePromptAlone` of `internal/extract`; `TestASchemaIsSentForEnforcementOnlyWhenADecoderTakesIt` of `internal/worker` |
| The schema sent for enforcement is closed, lists every property as required, and lets every property be null, for a caller's schema with types, enumerations, references, choices, arrays and definitions | `TestTheEnforcedSchemaLetsEveryMemberBeNull` of `reader/chat` |
| A value the document does not state, required by the caller's schema, is absent from the result and never a made-up value; the field fails `schema_not_satisfied` | `TestAValueTheDocumentDoesNotStateFailsTheFieldAndIsNeverMadeUp` of `internal/worker`; `TestAValueTheDocumentDoesNotStateIsNeverMadeUp` of `internal/extract` |
| A document that holds a line written as an instruction yields the same result as the document without it | `TestALineWrittenAsAnInstructionIsData` of `internal/worker`, with the chat adapter against an endpoint that obeys whatever it finds outside the document's fence: the line closes the fence and orders a reply, and the object and its citations are those of the document without it. `TestNoTextOfAFileClosesTheFence` of `internal/extract`. The opt-in live test is not written |
| A repair call shows the model its earlier reply and the findings | `TestExtract` of `reader/chat`, over the request the adapter sends; `TestAReplyThatFailsValidationIsRepairedInTheNextClaim` of `internal/worker`, for what the worker hands the adapter |
| Extractions queued by one group do not delay another group's pages beyond its share, and never exceed their reader's pool | `TestExtractionsOfOneGroupDoNotDelayAnothersPages` and `TestExtractionsNeverExceedTheirReadersPool`, at the store |
| A document four times the input budget is extracted in windows and the merged object validates | `TestALongDocumentIsExtractedInWindowsThroughTheAPI`, through the API; `TestALongDocumentIsExtractedOneWindowAClaim` of `internal/worker`; `TestADocumentInWindowsIsMergedAndHeldToTheSchemaAsAWhole` of `internal/extract` |
| A cited ref that does not exist is absent from the result | `TestACitedRefThatDoesNotExistIsAbsent` of `internal/extract`; `TestAShortDocumentIsExtractedInOneCall` of `internal/worker` |
| A worker killed in the middle of an extraction loses one call: another worker completes the extraction from what the last step kept, the model is called once per window plus at most the call in flight at the kill, and the field is filled once | `TestAnExtractionOutlivesAKilledWorker` of `cmd/lectiod`, with processes; `TestAWorkerThatDiesMidExtractionLosesOneCall` at the store |
| An extraction asked while its parse runs makes no call until the parse has ended, and is then filled | `TestAnExtractionAskedWhileItsParseRunsWaitsForTheDocument`, through the API; `TestAnExtractionAskedWhileItsParseRunsWaitsForItsEnd` at the store, for a parse that ends by its assemble, by a cancel and by its deadline |
| An extraction is read with its group's key, waits out a rate limit and a key endpoint that is down with no attempt spent, and fails on the endpoint's `402` and `403` with `budget_exhausted` and `reader_not_permitted` | `TestExtractionsAndFiguresOfTwoGroupsAreReadWithTwoKeys`, `TestARateLimitPausesAnExtractionAndAFigureOfItsGroupAlone` and `TestAGroupTheKeyEndpointRefusesFailsItsExtractionsAndItsFigures` of `cmd/lectiod` |
| An extraction that has not ended by its deadline fails with `deadline_exceeded`, and a delete of its parse drops it | `TestWorkOutOfTimeIsGivenUp` and `TestADeleteDropsTheWorkOnItsParse`, at the store |
