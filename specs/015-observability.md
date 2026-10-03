---
title: "Observability: a trace per task linked to its parse, the processing record, queue and pool metrics, logs"
status: drafted
track: core
depends_on:
  - specs/004-durable-tasks.md
  - specs/006-fairness-and-priority.md
  - specs/007-model-capacity.md
affects: [internal/telemetry/, internal/worker/, internal/httpapi/]
effort: medium
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Observability

## Overview

A parse crosses processes and waits in queues, so the question "why is
this parse slow, or wrong" has to be answerable from what Lectio
records, without reading code. This spec fixes three things: traces
that follow a parse through its tasks, a processing record on the
result that says which model read each page and what happened on the
way, and the metrics that describe the queue and the pools.

## Current state

The earlier service bootstrapped telemetry through the shared package
and traced HTTP requests. Work done after the request returned was
attached to nothing a caller could find, the result did not say which
model produced it, and the queue was invisible outside the process.
The bootstrap is carried over.

### What changed in review

The first draft of this spec put every task of a parse in one trace. A
3,000-page parse is then about nine thousand spans in a trace that
stays open for hours, which is past what a trace backend shows and
past what anyone reads. A task now has a trace of its own, linked to
its parse, and most of the ones that went well are not kept.

## Design

### Trace

A parse has a trace and each of its tasks has one.

The parse's trace is the submit: its id is taken from the submit
request's trace context, or created, stored on the parse, returned on
the Parse and as a response header. It holds the submit span and, when
the parse ends, one span for the settle with the parse's totals. It is
small whatever the size of the file.

A worker that runs a task starts a new trace for it, with a link to
the parse's submit span. A task's trace holds the wait, the render,
the reader calls and the settle of that one task, a handful of spans,
and its context is passed to the model endpoint as standard trace
context. From a parse, its tasks are found by the link or by the
`parse` attribute every task span carries; from a slow page, the parse
is one link away.

Not every task's trace is kept. A task's spans are held in the worker
until the task ends, and then:

- a task that failed, was retried, waited on a rate limit, escalated
  to another reader, or lost its worker is always exported;
- a task that succeeded on its first attempt is exported with
  probability `LECTIO_TRACE_SAMPLE` (default 0.05).

So what went wrong is always there, and what went well is a sample
that still shows what a normal page looks like. The decision is made
by the worker that ran the task, on the task's own outcome, and needs
no collector that sees a whole trace.

Spans, with the attributes that matter:

| Span | Attributes |
|---|---|
| `parse.submit` | parse, group, class, pages requested, reused |
| `parse.settle` | parse, state, pages done and failed, tokens, duration |
| `task.run` | parse, task, kind, attempt, expiries, queue wait, a link to `parse.submit` |
| `page.render` | page, dpi, bytes |
| `reader.call` | reader, model, input and output tokens, status, validation outcome |
| `assemble`, `extract` | pages, spans found, schema name, constrained, repairs |

No span, log line or metric carries file content, block text, an
image, or a credential. File names and origin paths are content for
this purpose and are not recorded either.

### The processing record

Written into the document index when the parse ends, by the settle of
`assemble` ([[005-parse-graph]]), and returned with the Parse on
request (`?include=processing`):

```json
{
  "policy": "sha256:...",
  "prompt": "v3",
  "readers": [ { "reader": "default", "model": "...", "pages": 118, "input_tokens": 1300000, "output_tokens": 310000 },
               { "reader": "strong",  "model": "...", "pages": 2,   "reason": "escalated: invalid reply twice" } ],
  "pages": { "native": 0, "blank": 3, "failed": 0, "retried": 4 },
  "passes": { "headers_found": 2, "spans": 1, "chunks": 57 },
  "timing": { "queued_ms": 4100, "preparing_ms": 900, "reading_ms": 61000, "assembling_ms": 300 },
  "rate_limited": 2
}
```

`queued_ms` is the time the parse's tasks spent waiting to be claimed,
for a turn or for capacity alike. The record does not split the two:
a task that waits for capacity is not written to
([[007-model-capacity]]), so there is no per-task figure to sum.
`rate_limited` counts the replies in which an endpoint told this
parse's calls to wait, which is the part of the wait the parse itself
observed. How long a pool was paused is a metric below.

Each page result carries its own line of the same facts: the reader,
the model, the attempts, and when it was not read by the first choice,
why. This is the record that answers which model saw which page.

### Metrics

OpenTelemetry instruments, exported through the shared bootstrap.

| Metric | Kind | Labels |
|---|---|---|
| `lectio.tasks.queued`, `lectio.tasks.running` | gauge, from the group counters | class |
| `lectio.task.wait` | histogram, claim time minus available time | class, kind |
| `lectio.task.duration` | histogram | kind, outcome |
| `lectio.task.outcomes` | counter | kind, outcome: succeeded, retried, rate_limited, expired, failed, canceled |
| `lectio.exchange.duration`, `lectio.exchange.tasks` | histogram: the time of a worker's exchange, and the tasks it settled and claimed | none |
| `lectio.pool.in_flight` | gauge | reader |
| `lectio.pool.paused_scopes`, `lectio.pool.limited_scopes` | gauge: key scopes of a reader that are paused, and that are below the full ceiling | reader |
| `lectio.pool.paused_seconds` | counter: time some scope of the reader was paused | reader |
| `lectio.pool.open` | gauge, 0 or 1 | reader |
| `lectio.reader.duration` | histogram | reader, outcome |
| `lectio.reader.tokens` | counter | reader, direction |
| `lectio.parses` | counter | state |
| `lectio.dispatch.share` | gauge, served share over the last interval against weight share | class |

Group and owner are never metric labels: their number is unbounded.
A pool's key scope is a group when keys are per tenant
([[007-model-capacity]]), so it is not a label either, and the pool
metrics count scopes instead of naming them. Per-group figures are
read from `GET /queue` and `GET /usage` ([[003-api]]).

### Logs

Structured, through the context-carrying logger so every line has the
trace and span ids. One line per task settlement at info, with parse,
task, kind, outcome, attempt and duration. Reader failures at warn
with the classified reason. Nothing per block.

### Health

`/livez` is the process. `/readyz` is the database and the object
store reachable, and for a worker, a successful exchange within the
last third of its lease ([[004-durable-tasks]]). A worker whose readers
are all open or paused is ready: that state is the pool's to handle,
not a reason to restart.

## Not in this spec

Dashboards and alert rules, which belong to whoever operates a
deployment. An audit log of who read which parse: reads are authorized
by the operator's authorizer, which is where such a log is kept.

## Implementation status

Nothing of this spec is built. The server logs structured lines at
start and stop and for an error nobody classified, outbound calls to
a model endpoint and to a source URL go through the shared tracing
transport with no tracer set up to record them, and the one probe is
`GET /healthz`. There is no trace for a parse or a task, no
processing record and no metric.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Every task of a parse run by two worker processes has a trace of its own whose root links to the parse's submit span, and the parse's trace holds two spans, the submit and the settle | an end-to-end test with an in-memory exporter and the sample rate at 1 |
| With the sample rate at 0, a 100-page parse in which three pages were retried and one failed exports exactly four task traces | the same test |
| A 3,000-page parse exports no trace of more than 20 spans | the same test |
| The processing record's reader totals equal the usage rows and the stub endpoint's own counts | an end-to-end test |
| A parse whose calls were rate limited twice reports `rate_limited: 2`, and `lectio.pool.paused_seconds` grew by the pause | a test with a rate-limiting stub |
| A run over a fixture whose text, file name and key are marked strings leaves none of them in any exported span, metric or log line | a test that scans every sink |
| No metric has a label whose cardinality grows with tenants or parses | a test asserting the label sets |
| `/readyz` fails when the database is unreachable and recovers when it returns, without a restart | a probe test |
