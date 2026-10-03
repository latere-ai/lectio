---
title: "Limits and usage: what a parse and a group are held to, whose credential a page is read with, and the meters Lectio records"
status: validated
track: core
depends_on:
  - specs/005-parse-graph.md
  - specs/006-fairness-and-priority.md
  - specs/012-identity-and-authorization.md
affects: [internal/limits/, internal/usage/, internal/keys/]
effort: medium
created: 2026-10-03
updated: 2026-10-04
author: changkun
---

# Limits and usage

## Overview

Three questions about cost. What stops a parse or a tenant from using
more than it was given. Who pays the model for a page. And what Lectio
writes down so that an operator can bill, or simply see, what was
done. Lectio enforces limits it is handed, reads pages with a
credential that belongs to whoever is paying, and records meters. It
sets no price and holds no balance.

## Current state

The earlier service limited submits per organization per minute with a
counter in each replica's memory and counted in-flight parses with a
check that could overshoot. It recorded token counts on the parse and
no model, cost or tenant total. The submit-rate limiter's interface is
carried over; its state moves to the database. The rest is new.

### What changed in review

The first draft of this spec repeated the check that could overshoot.
Pages per day were compared with a meter that is written when a page
completes, so fifty parses of ten pages submitted together, with ten
pages of budget left, all passed. The meter was one row per page,
written from the task row, and a task that was retried had nowhere to
carry the tokens of its earlier attempts: the sum the draft promised
could not be built, and at a sustained 40 pages a second the rows
passed a hundred million within the detail's retention. A budget is
now a reservation made when the page count is known, and the meter is
one row per parse and reader, summed from what each task accumulated.

## Design

### Limits and where each is enforced

| Limit | Source | Enforced |
|---|---|---|
| file size | server setting, lowered by the allow | at upload and at snapshot, while streaming |
| pages per parse | server setting, lowered by the allow | in `prepare`, after counting; `too_many_pages` |
| pages per day for a group | the allow | reserved in `prepare`, once the page count is known, by one statement that refuses a reservation past the limit; `budget_exhausted`. A submit of a group with nothing left is refused at once |
| queued parses, running tasks | the allow | [[006-fairness-and-priority]] |
| submits per minute | server setting, overridable by the allow | a fixed window per group, one row, one statement |
| deadline | the request, capped by `LECTIO_MAX_DEADLINE`; a parse that names none takes the cap, so nothing waits without bound | the deadline sweep ([[004-durable-tasks]]) |
| attempts, expiries | server settings | [[004-durable-tasks]] |
| model tokens per parse | server setting, default off | the page task stops reading when the parse's recorded tokens pass it; `budget_exhausted` |

A limit on pages cannot be enforced at submit, where the page count is
not known, and must not be enforced against what has already been read,
which lets every parse in flight pass. It is a reservation:

```sql
CREATE TABLE group_days (
  group_id text NOT NULL, day date NOT NULL,
  reserved integer NOT NULL DEFAULT 0,    -- pages promised to parses that began this day
  PRIMARY KEY (group_id, day)
);
```

`prepare` adds its selected pages to the group's row for the day in
one statement, `... SET reserved = reserved + $n WHERE reserved + $n <=
$limit`, and a statement that changes no row is the refusal. Two parses
that would together pass the limit cannot both reserve: the second
waits for the first's row and then sees its number. A 3,000-page file
submitted with 10 pages of budget left is refused in `prepare`, before
any page is read. When the parse ends, the pages it reserved and did
not read, because they failed, were canceled, or the parse failed, are
given back to the row of the day it reserved on.

The two counts of [[006-fairness-and-priority]], queued parses and
running tasks, are made under a lock on the group's row for the same
reason.

### Whose credential reads a page

The key a reader call is made with decides who the model endpoint
charges. Lectio resolves it per group through one interface.

```go
type KeySource interface {
    Key(ctx context.Context, group, owner, parse string) (Credential, error)
}
```

| `LECTIO_KEYS` | Behavior |
|---|---|
| `static` (default) | one key from configuration for every group; the operator pays and bills as it sees fit from the meters |
| `endpoint` | `POST LECTIO_KEYS_URL` with the group, owner and parse, authenticated with Lectio's bearer; the answer is a key and its expiry, cached until shortly before it expires |

With `endpoint`, an operator's plane issues a key scoped to the tenant
and its budget at a gateway, the tenant's spend is attributed by the
gateway that already meters it, and a tenant out of budget is refused
by the gateway, which Lectio reports as `budget_exhausted`
([[007-model-capacity]]). Lectio never stores a key in the database and
never logs one. A key endpoint that is down leaves the group's pages
unclaimed until it answers, not failed.

Whose key a call is made with is also whose rate limit its reply
describes. With `static` every group shares one key, and a rate-limit
reply pauses the reader for all. With `endpoint` the key is the
group's, and a rate-limit reply pauses that group's calls to the reader
and no other group's ([[007-model-capacity]]).

### Meters

A task accumulates what it used on its own row, over every attempt:
model calls, input and output tokens ([[004-durable-tasks]]). An
attempt that failed, or was told to wait, reports its tokens with its
outcome, so what was spent is recorded and not only what was useful.
When the parse ends, the settle of `assemble` sums the rows into the
meter, one row per reader and model that read for it:

```sql
CREATE TABLE usage (
  at        timestamptz NOT NULL DEFAULT now(),
  group_id  text NOT NULL, owner text NOT NULL, parse_id text NOT NULL,
  kind      text NOT NULL,        -- page | extract
  reader    text, model text,
  pages     integer NOT NULL DEFAULT 0,
  calls     integer NOT NULL DEFAULT 0,  -- model calls, retries included
  input_tokens bigint NOT NULL DEFAULT 0, output_tokens bigint NOT NULL DEFAULT 0,
  cost      numeric, currency text       -- when the endpoint reports cost
);
```

A row carries counts and identifiers, never content. Native pages are
metered with zero tokens under no reader, so "pages parsed" means the
same thing for every format. An extraction that is asked for after the
parse ended adds its own row when it settles. A parse that is canceled
or fails is metered the same way, by the statement that ends it.

One thing is not metered here: a call whose worker died or was taken
for dead before it could report. Its tokens were spent at the endpoint
and no live lease can record them ([[004-durable-tasks]]). The
endpoint's own meter has them, and the difference is bounded by the
calls in flight at each kill.

The row count follows parses, not pages. At a sustained 40 pages a
second in parses of 20 pages, that is two parses a second: about
170,000 rows a day, six million over the detail's retention.

`GET /usage?by=group|owner|reader|model&interval=hour|day&from=&to=`
returns sums. Rows are rolled into hourly aggregates by a sweep and
the detail is dropped after `LECTIO_USAGE_DETAIL` (default 35 days);
the aggregates are kept. The shape, sums by a key over fixed
intervals, is what a billing job folds into a ledger without asking
Lectio about each parse.

The parse's own `usage` ([[003-api]]) is kept on the parse row,
advanced by each settle while the parse runs, and equals the sum of its
meter rows when it ends. Each page's result carries the attempts and
tokens of that page ([[002-object-model]]).

### What Lectio does not do

It does not convert tokens to money unless the endpoint reported the
cost, does not know a price per page, and does not stop a tenant
because of a balance. A plane that prices by the page reads the page
meter; one that passes model cost through reads the gateway's.

## Not in this spec

Plans, prices, invoices, wallets and entitlements. Budget alerts. A
per-tenant report in a user interface.

## Implementation status

One part is built, in the task store
([[004-durable-tasks]], [[007-model-capacity]]): whether keys are one
for every group or one per group is a setting the store is opened
with, and it decides the scope a slot is taken in and a rate limit
pauses. The key source that would make a key per group real is not
built, and nothing sets the setting yet. The rest of this spec is not
built. The server's own limits on a file's
size and page count and on a deadline are enforced ([[009-intake]],
[[003-api]]), one key from `LECTIO_MODEL_KEY` is passed to every
reader call, and a parse sums its pages' usage. There is no limit per
group, no key source, no meter, and the `usage` route answers `501`.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Each row of the limits table has a test at the limit and one past it, with the stated code | a table test |
| A group with 10 pages of daily budget left submits a 300-page file: the parse fails in `prepare` with `budget_exhausted` and no reader call is made | an end-to-end test with a counting stub reader |
| A group with 10 pages of daily budget left submits 50 parses of 10 pages at once: exactly one reserves, 49 fail in `prepare`, and 10 pages are read | a concurrency test over Postgres |
| A parse that reserved 100 pages and was canceled after 30 gives 70 back, and a parse of 70 pages submitted next reserves | an end-to-end test |
| A parse submitted with no deadline has `deadline_at` at `LECTIO_MAX_DEADLINE` from its submit | an API test |
| Submit-rate limiting holds across two API replicas: 120 per minute in total, not each | a test with two processes |
| With `LECTIO_KEYS=endpoint`, pages of two groups are read with two different keys, and the stub gateway's records attribute each page to its group | an end-to-end test |
| No key appears in any log line, trace attribute, database row or error body | a test that greps every sink after a run with a marked key |
| The usage rows of a parse sum to the stub endpoint's own count of tokens served, failed and rate-limited attempts included, in a run with no worker killed | an end-to-end test |
| In a run with workers killed, the usage rows sum to no more than the endpoint's count, and the difference is at most the calls in flight at the kills | the soak test of [[004-durable-tasks]] |
| A parse of 3,000 pages read by two readers ends with two usage rows and no `succeeded` task row | a store test |
| `GET /usage` over a day equals the detail rows summed, before and after the roll-up sweep | a store test |
