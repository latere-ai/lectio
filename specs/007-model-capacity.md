---
title: "Model capacity: reader pools, slots held with the lease, rate limits, the breaker, and fallback"
status: in-progress
track: core
depends_on:
  - specs/004-durable-tasks.md
  - specs/006-fairness-and-priority.md
affects: [internal/pools/, internal/store/]
effort: large
created: 2026-10-03
updated: 2026-10-04
author: changkun
---

# Model capacity

## Overview

A hosted model is not an unlimited resource: its endpoint allows so
many calls at once and so many per minute, slows down, and sometimes
goes away. This spec is the layer between a task and the call. It
bounds how many calls each reader has in flight across the whole fleet,
reacts to the endpoint's own rate limit, stops sending work to a reader
that is failing, and decides when a page may go to another reader
instead. None of it concerns hardware. The capacity being admitted
against is a number in configuration and the answers the endpoint
gives.

## Current state

The earlier service bounded concurrency per process: four pages per
parse times four parses per replica, with no limit across replicas or
per model, no handling of a rate-limit reply, and no breaker, so each
page waited out its full timeout against a dead endpoint. Nothing here
is carried over.

### What changed in review

The first draft of this spec kept one pause and one ceiling per reader.
A rate-limit reply describes the key the call was made with, and when
each tenant reads with its own key ([[013-limits-and-usage]]), one
tenant's limit paused the reader for everyone. The ceiling was halved
on every such reply, and a limit that trips returns many at once: seven
replies took a ceiling of 200 to 1, and at one slot back per 30
seconds it needed 99 minutes to return. A task with no capacity was
chosen first and then written back as waiting, a write for every idle
slot's poll. This revision scopes the pause to the key, halves once per
pause, and admits a task only when there is room, so waiting writes
nothing.

## Design

### Pool

Each configured reader ([[008-readers]]) has one pool, and each key the
reader is called with has a scope inside it.

```sql
CREATE TABLE pools (
  reader        text PRIMARY KEY,
  max_in_flight integer NOT NULL,        -- from configuration; across every scope
  cost          integer NOT NULL DEFAULT 1,  -- the fairness charge of one call, from configuration
  failures      integer NOT NULL DEFAULT 0,
  opened_at     timestamptz,             -- breaker open since
  trial_at      timestamptz,             -- when the one trial call was admitted
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE pool_scopes (
  reader       text NOT NULL,
  scope        text NOT NULL,            -- '' when one key serves every group; the group id when keys are per tenant
  ceiling      integer NOT NULL,         -- allowed in flight for this scope, 1..max_in_flight
  paused_until timestamptz,              -- set by a rate-limit reply made with this scope's key
  halved_at    timestamptz,              -- when the ceiling was last halved
  raised_at    timestamptz,              -- the instant the ceiling was current: recovery is counted from it
  PRIMARY KEY (reader, scope)
);
```

The scope is where a rate limit applies. With one key from
configuration it is the empty scope and there is one per reader. With a
key per tenant it is the group, so one tenant reaching its limit
pauses that tenant and no other. A scope has a row only once it has
been limited; a scope with no row is unpaused at the full ceiling, so
the table grows with tenants that hit limits and not with tenants.

The endpoint's health is the reader's and not a key's: the breaker and
`max_in_flight` are on the pool.

A store writes one `pools` row per configured reader when it opens,
with the reader's `max_in_flight` and `cost`, keeps the breaker state
of a pool that was there, and removes the pool of a reader that left
the configuration. The policy's chains, and whether keys are per
tenant, are in the settings row it writes with them
([[004-durable-tasks]]).

### A slot is a lease

There is no table of grants. A task that holds a slot is a leased row
with `reader` and `scope` set and `calling` true
([[004-durable-tasks]]). A pool's in-flight count is the number of such
rows for the reader, and a scope's is the number for the reader and
the scope. Two consequences are the point of the design:

- A slot cannot leak. A worker that dies holding slots loses its
  lease, the sweep returns its tasks to the queue and clears the slot,
  and the slot is free. No second expiry mechanism has to agree with
  the first.
- A slot is never shorter than the call. It lives as long as the task
  is leased and calling.

A page task makes one call, and takes its slot when it is claimed.
A task that makes several calls, an extraction over windows with its
repairs, does not hold a slot for its whole lease: it asks for one
before each call and gives it back after, through the worker's
exchange, so a long task occupies a model only while it is calling it.
A page that escalates to the next reader ([[008-readers]]) moves its
slot the same way: it gives back the one it held and asks for one in
the next reader's pool.

### Admission

A task that calls a model is claimed only when a reader it may use has
room. In the claim step of the exchange, which is serialized across the
fleet so every count below is exact:

1. Read the pools and the scopes once. A reader has room for a group
   when its breaker admits a call, its in-flight count is below
   `max_in_flight`, the group's scope is not paused, and the scope's
   in-flight count is below what the scope admits now.
2. The candidates of a task are its `pin`, or the routing policy's
   chain ([[008-readers]]). The task is eligible when a candidate has
   room, and the fair queue chooses among eligible tasks
   ([[006-fairness-and-priority]]).
3. The claim sets `reader`, `scope` and `calling` on the task for the
   first candidate with room.

A task with no room is not claimed and nothing is written for it. A
worker with free slots and no eligible task sleeps until the earliest
`paused_until` among the scopes it was refused by, or its poll
interval. `waits` no longer exists as a counter: what waited and why is
answered by reading the pools, not by a column written on every poll.

### Rate limits

A `429` from the endpoint, or a `503` with `Retry-After`, is the
endpoint describing the capacity of the key the call was made with.
The worker ends the attempt as a wait, with no attempt spent, and its
next exchange updates that scope in one statement:

- `paused_until = greatest(paused_until, now() + retry_after)`, or 5s
  when the reply names none, so every worker stops calling that reader
  with that key at once instead of each discovering the limit
  separately;
- the ceiling is halved, `max(1, ceiling / 2)`, only when the call that
  was refused was claimed after `halved_at`. Calls already in flight
  when the ceiling was halved come back refused too, and they say
  nothing new: one pause halves once, however many replies report it.

Each interval of `LECTIO_POOL_RECOVERY` (default 30s) without a
rate-limit reply raises the ceiling by a tenth of `max_in_flight`, at
least one, up to `max_in_flight`: a halved pool is whole again in two
and a half minutes of quiet, and a pool driven to one slot in five.
The raise is read from the clock in the claim, from the stored ceiling
and the instant it was stored, and is written only with a claim in the
scope or with its next rate-limit reply, so a scope that recovers while
nothing is claimed writes nothing.

When a pause ends, the scope does not admit its whole ceiling at once.
For `LECTIO_POOL_RESUME` (default 10s) after `paused_until`, it admits
a share of the ceiling that grows from one slot to all of it in
proportion to the time passed. The share is computed from the clock in
the claim; nothing is written.

An operator who knows the endpoint's limit sets `max_in_flight` on the
reader. A limit in requests per minute is not enforced here: the
endpoint enforces it, and the pause and the ceiling follow what it
answers.

### Breaker

| Setting | Default |
|---|---|
| consecutive failures to open | 3 |
| open period | 30s |
| trial | one call |

A retryable failure of a reader call increments `failures`; a success
resets it. At the threshold the pool records `opened_at` and admits
nothing for the open period. After it, one claim is admitted as a
trial and records `trial_at`; while `trial_at` is later than
`opened_at` no other claim is admitted. Success closes the breaker and
clears both, failure sets `opened_at` again. Canceled calls, waits for
capacity and permanent failures of one page (a corrupt image, a refusal
of the content) do not count; they say nothing about the reader's
health. The state is in the row, so one replica's observation protects
all of them.

A trial that ends without counting, because its call was canceled, was
told to wait, failed for the page's own reason, or its worker died,
would leave the breaker open with its one trial spent. The trial is
therefore outstanding only while it is in flight: a leased task that is
calling the reader and was claimed after `opened_at`. When there is
none, the next claim is the trial.

### Fallback

When the first candidate has no room, the next reader in the policy's
chain is tried in the same claim: the slot goes to the first candidate
with room, as Admission says. The first candidate has no room when its
scope is paused, its breaker is open, or its pool or scope is full, so
a page also goes to the next reader when the first is only busy. An
operator who wants a second reader used for failures and never for
load gives the first a `max_in_flight` the fleet does not reach. Two
rules bound this:

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

### A reader's cost

A reader carries a `cost`, default 1, in its configuration
([[008-readers]]). It is the fairness charge for one call to it
([[006-fairness-and-priority]]): an operator who configures a small
model for pages and a large one for escalation says how much more a
call to the second weighs. It is a weight between readers, not a price.

## Not in this spec

Sharing a pool between tenants by a separate formula. The fair queue
already decides whose page is claimed next, and `max_running` caps a
group; a second share computation over the same weights would have to
be kept consistent with the first for no added guarantee. Token-rate
budgets per pool are deferred until an endpoint is found that limits by
tokens and not by requests. Spacing calls to a rate in configuration.

## Implementation status

Built:

- `internal/store/postgres/migrations`: the pools and their scopes,
  admission in the claim step of `lectio_exchange` with the slot as the
  leased row itself, the pause and the ceiling of a scope from a
  rate-limit reply, recovery and the resume ramp read from the clock,
  the breaker with its one trial, and fallback to the next reader of
  the chain.

Remaining: a slot taken and given back per call, for an extraction and
for a page that moves to the next reader after a reply
([[004-durable-tasks]], step 5 of the exchange): a task holds the slot
of its claim until it settles. In a development server nothing of this
spec applies. The in-process runner bounds the pages
read at once by a fixed number of workers for all readers together,
and a rate-limited page waits by itself for the delay the endpoint
gave: there is no pool, no shared pause, no ceiling that adapts and no
breaker. `maxInFlight` and `cost` are read from a Reader document and not
applied ([[008-readers]]).

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| With `max_in_flight` 8 and 32 workers across 4 processes, a stub endpoint never observes more than 8 concurrent calls | a concurrency test with a counting stub |
| A worker killed mid-call frees its slots after one lease period with no other action | a process-level test |
| With keys per tenant, a `429` on one group's key pauses that group's calls to the reader and no other group's; with one static key it pauses the reader for all | a test with a stub endpoint that limits one key |
| One `429` with `Retry-After: 7` stops every worker's calls in that scope for 7 seconds and no task's `attempt` changes | a test with a stub endpoint and a virtual clock |
| Forty calls in flight that all return `429` in one second halve the ceiling once, from 40 to 20 | the same test |
| After a pause ends, the calls admitted in the first second do not exceed a tenth of the ceiling, and the whole ceiling is admitted after `LECTIO_POOL_RESUME` | the same test, with a counting stub |
| A ceiling driven to 1 returns to a `max_in_flight` of 200 within ten quiet recovery intervals | the same test |
| While a pool is full, idle workers write no row | the write-count test of [[004-durable-tasks]] |
| Three consecutive failures open the breaker for all replicas; exactly one trial call is made after the open period | a concurrency test |
| An extraction over six windows never holds more than one slot, and holds none between its calls | a test with a counting stub |
| With a chain of two readers and the first open, pages are read by the second and their results say so; with the reader pinned, pages wait and none is read by the second | an end-to-end test |
| A budget refusal fails the parse with `budget_exhausted` after one call and leaves the breaker closed | an end-to-end test |
