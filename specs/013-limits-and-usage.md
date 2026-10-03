---
title: "Limits and usage: what a parse and a group are held to, whose credential a page is read with, and the meters Lectio records"
status: drafted
track: core
depends_on:
  - specs/005-parse-graph.md
  - specs/006-fairness-and-priority.md
  - specs/012-identity-and-authorization.md
affects: [internal/limits/, internal/usage/, internal/keys/]
effort: medium
created: 2026-10-03
updated: 2026-10-03
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

## Design

### Limits and where each is enforced

| Limit | Source | Enforced |
|---|---|---|
| file size | server setting, lowered by the allow | at upload and at snapshot, while streaming |
| pages per parse | server setting, lowered by the allow | in `prepare`, after counting; `too_many_pages` |
| pages per day for a group | the allow | at submit against the meter below, and again in `prepare` once the page count is known; `budget_exhausted` |
| queued parses, running tasks | the allow | [[006-fairness-and-priority]] |
| submits per minute | server setting, overridable by the allow | a fixed window per group, one row, one statement |
| deadline | the request, capped by a server setting | the deadline sweep ([[004-durable-tasks]]) |
| attempts, expiries | server settings | [[004-durable-tasks]] |
| model tokens per parse | server setting, default off | the page task stops reading when the parse's recorded tokens pass it; `budget_exhausted` |

A limit that is checked before the work is known, pages per day at
submit, is checked again when it is: a 3,000-page file submitted with
10 pages of budget left is refused in `prepare`, before any page is
read, and not after the budget is overrun.

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
never logs one. A key endpoint that is down is a capacity wait for the
page, not a failure.

### Meters

One usage row per settled task that did metered work, written in the
completing transaction:

```sql
CREATE TABLE usage (
  at        timestamptz NOT NULL DEFAULT now(),
  group_id  text NOT NULL, owner text NOT NULL, parse_id text NOT NULL,
  kind      text NOT NULL,        -- page | extract
  reader    text, model text,
  pages     integer NOT NULL DEFAULT 0,
  input_tokens bigint NOT NULL DEFAULT 0, output_tokens bigint NOT NULL DEFAULT 0,
  cost      numeric, currency text,   -- when the endpoint reports cost
  attempts  integer NOT NULL
);
```

A row carries counts and identifiers, never content. A page that was
read three times before it succeeded is one row with `attempts = 3`
and the tokens of all three: what was spent is recorded, not what was
useful. Native pages are metered with zero tokens, so "pages parsed"
means the same thing for every format.

`GET /usage?by=group|owner|reader|model&interval=hour|day&from=&to=`
returns sums. Rows are rolled into hourly aggregates by a sweep and
the detail is dropped after `LECTIO_USAGE_DETAIL` (default 35 days);
the aggregates are kept. The shape, sums by a key over fixed
intervals, is what a billing job folds into a ledger without asking
Lectio about each parse.

The parse's own `usage` ([[003-api]]) is the sum of its rows.

### What Lectio does not do

It does not convert tokens to money unless the endpoint reported the
cost, does not know a price per page, and does not stop a tenant
because of a balance. A plane that prices by the page reads the page
meter; one that passes model cost through reads the gateway's.

## Not in this spec

Plans, prices, invoices, wallets and entitlements. Budget alerts. A
per-tenant report in a user interface.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Each row of the limits table has a test at the limit and one past it, with the stated code | a table test |
| A group with 10 pages of daily budget left submits a 300-page file: the parse fails in `prepare` with `budget_exhausted` and no reader call is made | an end-to-end test with a counting stub reader |
| Submit-rate limiting holds across two API replicas: 120 per minute in total, not each | a test with two processes |
| With `LECTIO_KEYS=endpoint`, pages of two groups are read with two different keys, and the stub gateway's records attribute each page to its group | an end-to-end test |
| No key appears in any log line, trace attribute, database row or error body | a test that greps every sink after a run with a marked key |
| The sum of usage rows for a parse equals the stub endpoint's own count of tokens served, including failed attempts | an end-to-end test |
| `GET /usage` over a day equals the detail rows summed, before and after the roll-up sweep | a store test |
