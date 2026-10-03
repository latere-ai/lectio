---
title: "Architecture: one binary in two roles, Postgres for state and the queue, an object store for bytes, a model endpoint for pages"
status: drafted
track: core
depends_on: []
affects: [cmd/lectiod/, internal/, authorizer/, api/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Architecture

## Overview

Lectio turns a file into a structured document. A caller submits a file
and receives a parse: an asynchronous job whose result is the file's
content as pages, blocks, tables and extracted fields
([[002-object-model]]). Each page is read by a vision-language model.
Lectio does not run that model. It owns everything between the submit
and the result: accepting and snapshotting the file, splitting it into
pages, scheduling one small task per page, sharing the model's capacity
between tenants, retrying what fails, assembling the pages into one
document, and keeping every step durable.

This spec fixes the components, the boundaries between them, and the
invariants every other spec assumes.

## Current state

Nothing is built in this repository. An earlier service of the same
name parsed documents for one deployment: it detected file types,
converted office formats, extracted text natively where the format
carried it, called a self-run OCR model per page, and scheduled parses
between tenants. Its queue lived in process memory, a parse was one
unit of work, and a restart began every running parse again from the
first page. Its intake, layout and formatting packages are sound and
are carried over; its queue, its worker pool and its model client are
replaced. Each spec says under this heading what it carries over.

## Design

### Components

```mermaid
flowchart TB
  subgraph clients["callers"]
    sdk["HTTP client, CLI"]
  end
  subgraph lectiod["lectiod, one binary"]
    api["role api: HTTP, admission, results"]
    wrk["role worker: claim, run, checkpoint"]
  end
  subgraph state["durable state"]
    pg[("Postgres: files, parses, tasks, groups, pools, usage")]
    obj[("object store: source snapshots, page images, page results, documents")]
  end
  subgraph outside["operated by someone else"]
    iss["OIDC issuer"]
    az["authorizer endpoint"]
    mdl["model endpoint, OpenAI-compatible"]
    keys["key source, optional"]
  end
  sdk --> api
  api --> pg
  api --> obj
  wrk --> pg
  wrk --> obj
  wrk --> mdl
  wrk -.-> keys
  api -.-> iss
  api -.-> az
```

- **`lectiod`** is one binary. `--role api` serves HTTP, `--role worker`
  runs tasks, and the default runs both in one process. Replicas of
  either role coordinate only through Postgres. No replica is a leader.
- **Postgres** holds every fact that must survive a process: files,
  parses, tasks and their leases, the fair queue's accounting, model
  pool state, and usage. It is the only queue: there is no broker.
- **The object store** (S3-compatible) holds bytes: the source
  snapshot, each page's rendered image, each page's result, and the
  assembled document. A database row never holds an image or a page of
  blocks.
- **The model endpoint** reads a page. Lectio speaks the
  OpenAI-compatible chat completions shape with image input and a JSON
  schema for the reply ([[008-readers]]). Pointing it at a gateway is
  what makes the model a line of configuration.
- **The issuer and the authorizer** answer who is calling and what the
  caller may do, with which limits ([[012-identity-and-authorization]]).
- **The key source**, when configured, supplies the model credential a
  tenant's pages are read with, so model spend lands on that tenant
  ([[013-limits-and-usage]]).

### The path of one parse

1. `POST /v1/parses` names a file ([[003-api]], [[014-sources-and-retention]]).
   The API verifies the token, asks the authorizer, reads the limits on
   the allow, and writes a parse and its first task, `prepare`, in one
   transaction. It answers `202`.
2. A worker claims `prepare` ([[004-durable-tasks]]). It snapshots the
   source, detects the type, converts what needs converting, counts
   pages, and writes one `page` task per selected page plus the tasks
   that follow them ([[005-parse-graph]], [[009-intake]]).
3. Workers claim page tasks. Which tenant's task a worker takes next is
   the fair queue's decision ([[006-fairness-and-priority]]). Whether
   the model has room for the call is the pool's
   ([[007-model-capacity]]). The worker renders the page, calls a
   reader, validates the reply, and writes the page result.
4. When every page task is settled, `assemble` builds the document
   ([[010-assembly]]), `extract` tasks fill the requested schemas
   ([[011-structured-extraction]]), and `finalize` records usage and
   the terminal state.
5. The caller reads state, pages as they land, and the document.

### Invariants

1. **A committed step is never redone.** A task's output is written to
   a deterministic key before the task is marked done, and both are
   idempotent. A retry of a parse re-runs only tasks that did not
   succeed.
2. **A lost process loses a lease, not work.** Every task is held under
   a lease with an expiry on the database clock. Expiry returns the
   task to the queue. No parse depends on the process that accepted it.
3. **A stale worker cannot write.** Completing or extending a task
   requires the lease token issued at claim. A worker whose lease
   expired and was reissued is refused.
4. **Fairness is chosen before the task.** The dispatcher picks a
   tenant, then that tenant's next task. Nothing a tenant queues can
   change which tenant is picked.
5. **Waiting for capacity is not failing.** A task turned away by a
   full pool, a rate limit, or an open breaker is rescheduled without
   spending an attempt.
6. **Cancel stops work.** A canceled parse's queued tasks are removed
   in the same transaction, and a running task learns at its next
   heartbeat and aborts its model call.
7. **Memory is bounded by a page.** No role holds a whole document, a
   whole result, or more than one page image per running task in heap.
8. **An option is honored or refused.** The API rejects a field it does
   not implement. Nothing is accepted and ignored.
9. **The core verifies and asks.** It never reads a claim for meaning,
   holds no account, and decides nothing about a person. With no
   authorizer configured, the built-in owner policy applies.
10. **Nothing here names a deployment.** No hostname, vendor account,
    or model identifier is compiled in. Defaults that appear are
    examples.

### What Lectio does not own

- Serving or hosting a model, and managing accelerator capacity for
  one. The scarce resource is the model endpoint's rate limit and the
  tenant's budget, not a GPU.
- Storing a caller's files beyond the parse. A source is a snapshot
  with a retention period.
- Accounts, billing, plans and prices. It records what it did
  ([[013-limits-and-usage]]) and enforces the limits it is handed.
- A user interface.

### Dependencies

Go, with no cgo. `latere.ai/x/pkg` for the JWT verifier, the authorizer
contract, embedded migrations, the S3 client, telemetry, retry and
health probes. Postgres 16 or later. Any S3-compatible object store.
A memory store and a local-directory object store exist for tests and
for a one-process development run; neither is durable and `lectiod`
says so at start.

## Not in this spec

Wire shapes, table layouts, algorithms and configuration variables:
each belongs to the spec linked above. Packaging, images and the
configuration reference are [[016-distribution]]. Telemetry is
[[015-observability]].

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| `lectiod` runs as `api`, as `worker`, and as both, and a parse submitted to an `api` replica completes on a separate `worker` replica | an end-to-end test with two processes over one Postgres and one bucket |
| Killing every worker mid-parse and starting a new one completes the parse, and no page succeeded twice | a test that kills the worker process after k of n pages and counts reader calls per page |
| Two worker replicas over one database never run the same task at once | a concurrency test asserting at most one valid lease token per task |
| The API holds no parse state in memory: restarting it between submit and result changes nothing | an end-to-end test |
| With no issuer, authorizer, model endpoint or key source configured, `lectiod` refuses to start in production mode and names the missing setting; in development mode it starts with the stub reader and says so | start-up tests |
| A heap profile taken while a 500-page parse runs shows no allocation proportional to the page count in either role | a memory test over a generated document |
