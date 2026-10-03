---
title: "Model capacity: reader pools, slots held with the lease, rate limits, the breaker, and fallback"
status: drafted
track: core
depends_on:
  - specs/004-durable-tasks.md
  - specs/006-fairness-and-priority.md
affects: [internal/pools/, internal/store/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Model capacity

## Overview

A hosted model is not an unlimited resource: its endpoint allows so
many calls at once and so many per minute, slows down, and sometimes
goes away. This spec is the layer between a claimed page task and the
call. It bounds how many calls each reader has in flight across the
whole fleet, reacts to the endpoint's own rate limit, stops sending
work to a reader that is failing, and decides when a page may go to
another reader instead. None of it concerns hardware. The capacity
being admitted against is a number in configuration and the answers
the endpoint gives.

## Current state

The earlier service bounded concurrency per process: four pages per
parse times four parses per replica, with no limit across replicas or
per model, no handling of a rate-limit reply, and no breaker, so each
page waited out its full timeout against a dead endpoint. Nothing here
is carried over.

## Design

### Pool

Each configured reader ([[008-readers]]) has one pool, a row:

```sql
CREATE TABLE pools (
  reader        text PRIMARY KEY,
  max_in_flight integer NOT NULL,        -- from configuration
  ceiling       integer NOT NULL,        -- current allowed in flight, 1..max_in_flight
  paused_until  timestamptz,             -- set by a rate-limit reply
  failures      integer NOT NULL DEFAULT 0,
  opened_at     timestamptz,             -- breaker open since
  updated_at    timestamptz NOT NULL DEFAULT now()
);
```

### A slot is a lease

There is no table of grants. A page task that holds a slot is a leased
row with `reader` set ([[004-durable-tasks]]), and a pool's in-flight
count is `count(*) FROM tasks WHERE state = 'leased' AND reader = $1`.
Two consequences are the point of the design:

- A slot cannot leak. A worker that dies holding one loses its lease,
  the sweep clears `reader`, and the slot is free. No second expiry
  mechanism has to agree with the first.
- A slot is never shorter than the call. It lives exactly as long as
  the task's lease, which the worker extends while the call runs.

Taking a slot happens inside the claim transaction, after the task is
chosen:

1. Resolve the candidate readers for the page: the pinned reader, or
   the routing policy's chain ([[008-readers]]).
2. For the first candidate that is not paused and whose breaker admits
   a call: `pg_advisory_xact_lock(hashtext(reader))`, count the leased
   tasks of that reader, and if the count is below `ceiling`, set
   `reader` on the task and proceed. The lock makes the count and the
   update one critical section, so concurrent claims cannot oversubscribe
   a pool.
3. If no candidate admits the page, the claim for this task is a
   capacity wait: `available_at` moves to the earliest of the
   candidates' `paused_until` or a short poll interval, `waits`
   increases, and the worker goes on to the next group. The fair
   queue's charge is not applied.

### Rate limits

A `429` from the endpoint, or a `503` with `Retry-After`, is the
endpoint describing its own capacity. The worker ends the attempt as a
capacity wait and updates the pool in one statement:

- `paused_until = now() + retry_after` (or 5s when the reply names
  none), so every worker in the fleet stops calling that reader at
  once instead of each discovering the limit separately;
- `ceiling = max(1, ceiling / 2)`.

Each interval of `LECTIO_POOL_RECOVERY` (default 30s) without a
rate-limit reply raises `ceiling` by one, up to `max_in_flight`. The
pool finds the concurrency the endpoint tolerates and follows it when
it changes. An operator who knows the limit sets `max_in_flight` and
`requests_per_minute` on the reader; the second is enforced as a
minimum spacing between claims of that reader, kept in the pool row.

### Breaker

| Setting | Default |
|---|---|
| consecutive failures to open | 3 |
| open period | 30s |
| trial | one call |

A retryable failure of a reader call increments `failures`; a success
resets it. At the threshold the pool records `opened_at` and admits
nothing for the open period. After it, exactly one claim is admitted as
a trial, which the advisory lock makes easy to guarantee: success
closes the breaker, failure reopens it. Canceled calls, capacity waits
and permanent failures of one page (a corrupt image, a refusal of the
content) do not count; they say nothing about the reader's health.
The state is in the row, so one replica's observation protects all of
them.

### Fallback

When the first candidate is paused or open, the next reader in the
policy's chain is tried in the same claim. Two rules bound this:

- A parse that named its reader has a chain of one. It waits for that
  reader. A caller who pinned a model for a reason, the place its data
  may go among them, never has a page silently sent elsewhere.
- Fallback follows the pool's state, not a single failed call. A page
  whose call failed retries on the reader it was given until the
  breaker says the reader is unhealthy.

The page result records the reader and model that read it, and when it
was not the first choice, why ([[015-observability]]).

### Budget refusals

When the model credential belongs to the tenant
([[013-limits-and-usage]]) and the endpoint refuses a call because that
tenant's budget is spent, the failure is permanent for the parse, with
code `budget_exhausted`. It is not retried and does not count against
the breaker: the reader is healthy and the tenant is out of funds.

## Not in this spec

Sharing a pool between tenants by a separate formula. The fair queue
already decides whose page is claimed next, and `max_running` caps a
group; a second share computation over the same weights would have to
be kept consistent with the first for no added guarantee. Token-rate
budgets per pool are deferred until an endpoint is found that limits by
tokens and not by requests.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| With `max_in_flight` 8 and 32 workers across 4 processes, a stub endpoint never observes more than 8 concurrent calls | a concurrency test with a counting stub |
| A worker killed mid-call frees its slot after one lease period with no other action | a process-level test |
| One `429` with `Retry-After: 7` stops every worker's calls to that reader for 7 seconds, halves the ceiling, and no task's `attempt` changes | a test with a stub endpoint and a virtual clock |
| The ceiling returns to `max_in_flight` after the configured quiet intervals | the same test |
| Three consecutive failures open the breaker for all replicas; exactly one trial call is made after the open period | a concurrency test |
| With a chain of two readers and the first open, pages are read by the second and their results say so; with the reader pinned, pages wait and none is read by the second | an end-to-end test |
| A budget refusal fails the parse with `budget_exhausted` after one call and leaves the breaker closed | an end-to-end test |
