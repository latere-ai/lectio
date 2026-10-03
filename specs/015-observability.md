---
title: "Observability: one trace per parse, the processing record, queue and pool metrics, logs"
status: drafted
track: core
depends_on:
  - specs/004-durable-tasks.md
  - specs/006-fairness-and-priority.md
  - specs/007-model-capacity.md
affects: [internal/telemetry/, internal/worker/, internal/api/]
effort: medium
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Observability

## Overview

A parse crosses processes and waits in queues, so the question "why is
this parse slow, or wrong" has to be answerable from what Lectio
records, without reading code. This spec fixes three things: one trace
that follows a parse through every task, a processing record on the
result that says which model read each page and what happened on the
way, and the metrics that describe the queue and the pools.

## Current state

The earlier service bootstrapped telemetry through the shared package
and traced HTTP requests. Work done after the request returned was
attached to nothing a caller could find, the result did not say which
model produced it, and the queue was invisible outside the process.
The bootstrap is carried over.

## Design

### Trace

A trace id is taken from the submit request's trace context, or
created, and stored on the parse. Every task row inherits it. A worker
that claims a task starts a span as a child of the parse's root, so one
trace shows the submit, the wait, each page, the reader calls,
assembly and extraction, across however many processes ran them. The
id is returned on the Parse and as a response header, and is passed to
the model endpoint as standard trace context.

Spans, with the attributes that matter:

| Span | Attributes |
|---|---|
| `parse.submit` | parse, group, class, pages requested, reused |
| `task.run` | parse, task, kind, attempt, waits, expiries, queue wait |
| `page.render` | page, dpi, bytes |
| `reader.call` | reader, model, input and output tokens, status, validation outcome |
| `assemble`, `extract` | pages, spans found, schema name, constrained, repairs |

No span, log line or metric carries file content, block text, an
image, or a credential. File names and origin paths are content for
this purpose and are not recorded either.

### The processing record

Written by `finalize` into the document index and returned with the
Parse on request (`?include=processing`):

```json
{
  "policy": "sha256:...",
  "prompt": "v3",
  "readers": [ { "reader": "default", "model": "...", "pages": 118, "input_tokens": 1300000, "output_tokens": 310000 },
               { "reader": "strong",  "model": "...", "pages": 2,   "reason": "escalated: invalid reply twice" } ],
  "pages": { "native": 0, "blank": 3, "failed": 0, "retried": 4 },
  "passes": { "headers_found": 2, "spans": 1, "chunks": 57 },
  "timing": { "queued_ms": 4100, "preparing_ms": 900, "reading_ms": 61000, "assembling_ms": 300, "waiting_capacity_ms": 12000 }
}
```

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
| `lectio.task.outcomes` | counter | kind, outcome: succeeded, retried, waited, expired, failed, canceled |
| `lectio.pool.in_flight`, `lectio.pool.ceiling` | gauge | reader |
| `lectio.pool.paused`, `lectio.pool.open` | gauge, 0 or 1 | reader |
| `lectio.reader.duration` | histogram | reader, outcome |
| `lectio.reader.tokens` | counter | reader, direction |
| `lectio.parses` | counter | state |
| `lectio.dispatch.share` | gauge, served share over the last interval against weight share | class |

Group and owner are never metric labels: their number is unbounded.
Per-group figures are read from `GET /queue` and `GET /usage`
([[003-api]]).

### Logs

Structured, through the context-carrying logger so every line has the
trace and span ids. One line per task settlement at info, with parse,
task, kind, outcome, attempt and duration. Reader failures at warn
with the classified reason. Nothing per block.

### Health

`/livez` is the process. `/readyz` is the database and the object
store reachable, and for a worker, a successful claim query within the
last three poll intervals. A worker whose readers are all open or
paused is ready: that state is the pool's to handle, not a reason to
restart.

## Not in this spec

Dashboards and alert rules, which belong to whoever operates a
deployment. An audit log of who read which parse: reads are authorized
by the operator's authorizer, which is where such a log is kept.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| One trace contains the submit span and a span for every task of a parse run by two worker processes | an end-to-end test with an in-memory exporter |
| The processing record's reader totals equal the usage rows and the stub endpoint's own counts | an end-to-end test |
| A parse that waited on a paused pool reports the wait under `waiting_capacity_ms` and its task spans carry `waits` | a test with a rate-limiting stub |
| A run over a fixture whose text, file name and key are marked strings leaves none of them in any exported span, metric or log line | a test that scans every sink |
| No metric has a label whose cardinality grows with tenants or parses | a test asserting the label sets |
| `/readyz` fails when the database is unreachable and recovers when it returns, without a restart | a probe test |
