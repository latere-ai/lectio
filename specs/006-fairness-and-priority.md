---
title: "Fairness and priority: groups, weights, the interactive and batch classes, and the order tasks are dispatched in"
status: drafted
track: core
depends_on:
  - specs/004-durable-tasks.md
  - specs/005-parse-graph.md
affects: [internal/dispatch/, internal/store/]
effort: xlarge
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Fairness and priority

## Overview

Workers and model capacity are shared. This spec decides whose task
runs next. Tenants are served in proportion to a weight, measured in
pages over time. Work is in one of two classes, and interactive work
goes ahead of batch work without ever starving it. Inside one tenant's
own queue, the tenant's priorities and the order of its parses decide.
The decision is made in two steps, tenant first and task second, so
that nothing a tenant queues can move it ahead of another tenant.

## Current state

The earlier service chose the tenant with the smallest decayed service
counter and charged one unit per parse, in memory, per replica. The
idea is carried over, with three corrections: the charge is per page
rather than per parse, so a 3,000-page parse no longer costs what a
1-page parse costs; the accounting is in Postgres, so it holds across
restarts and replicas; and the classes, which existed in configuration
and were never wired, are part of the dispatch. Its preemption code is
not carried over, for the reason under Preemption below.

## Design

### Group

A group is the unit fairness is computed over: a tenant. Every parse
belongs to one. The group id and its settings arrive in the limits on
the allow that admitted the parse ([[012-identity-and-authorization]]);
with no authorizer, the group is the owner and the settings are the
configured defaults.

```sql
CREATE TABLE groups (
  group_id    text PRIMARY KEY,
  weight      integer NOT NULL DEFAULT 1,    -- share of service, 1..1000
  max_running integer NOT NULL DEFAULT 0,    -- leased tasks at once; 0 is no cap
  max_queued  integer NOT NULL DEFAULT 0,    -- non-terminal parses; 0 is no cap
  max_priority integer NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE group_service (
  group_id text NOT NULL, class smallint NOT NULL,
  vtime    numeric NOT NULL DEFAULT 0,       -- virtual time: pages served / weight
  queued   integer NOT NULL DEFAULT 0,       -- queued tasks, maintained in the transaction of each transition
  running  integer NOT NULL DEFAULT 0,       -- leased tasks
  PRIMARY KEY (group_id, class)
);
CREATE TABLE class_service (class smallint PRIMARY KEY, vtime numeric NOT NULL DEFAULT 0, weight integer NOT NULL);
```

A group's settings are refreshed from the allow on each submit, so a
change made by the authorizer takes effect with the tenant's next
parse and needs no call into Lectio.

### The dispatch decision

A worker with a free slot runs, in the claim transaction of
[[004-durable-tasks]]:

1. **Class.** Among classes with any queued task, take the one with
   the smaller `class_service.vtime`. The weights are
   `LECTIO_CLASS_WEIGHTS`, default `interactive=4,batch=1`: when both
   classes have work, interactive receives four slots in five and
   batch one in five. When only one class has work it receives all of
   them.
2. **Group.** Among groups with `queued > 0` in that class and
   `running < max_running` across classes, take the smallest
   `group_service.vtime`, ties broken by group id:
   `SELECT ... ORDER BY vtime, group_id FOR UPDATE SKIP LOCKED LIMIT 1`.
   Skipping a locked row means two workers claiming at once take
   different groups instead of waiting on each other.
3. **Task.** That group's next task in that class whose `available_at`
   has passed: `ORDER BY priority DESC, seq, created_at`. A group whose
   queued tasks are all still backing off yields no task; the worker
   takes the next group, and that group is charged nothing.
4. **Charge.** `vtime = max(vtime, floor) + cost / weight` for the
   group, where `floor` is the smallest `vtime` among groups with
   queued tasks in the class; and the same for the class with its weight.

This is start-time fair queuing. Three properties follow.

- **Share follows weight.** Over any interval in which two groups both
  have work, the pages each is served are in the ratio of their weights,
  within one task.
- **Idle time is not banked.** A group that returns after an hour of
  nothing starts at the floor, not at its old, small `vtime`. It gets
  its share from now on and no burst for the past.
- **Arrival is fast.** A group with one new task has the smallest
  `vtime` among the waiting, so its task takes the next slot that
  frees. With page-sized tasks that is at most the length of one page
  call.

### Inside a group

`priority` is the caller's, from `-max_priority` to `max_priority`,
default 0. It orders the group's own tasks only.

`seq` is a task's position within its parse ([[005-parse-graph]]).
Ordering by `seq` before `created_at` serves the first page of every
parse of the group, then the second of each, and so on: parses of one
tenant advance together, a short one finishes early, and a long one
cannot hold the tenant's own queue. A tenant that wants strict
first-in-first-out for its parses sets priorities.

Tasks with `cost` 0 (`prepare`, `assemble`, `extract-*`, `finalize`)
are dispatched like any other but charge nothing, and are ordered ahead
of page tasks of the same priority by a `seq` below 0, so a parse whose
pages are done is not kept waiting to be assembled.

### Admission at submit

A submit is refused with `429 queue_full` and `Retry-After` when the
group already holds `max_queued` non-terminal parses. The count and the
insert are one statement (`INSERT ... SELECT ... WHERE (SELECT
count(*) ...) < $max`), so concurrent submits cannot overshoot. Page
and spend budgets are [[013-limits-and-usage]].

### Preemption

Lectio does not interrupt a model call to give its slot to someone
else. A page call lasts seconds and is paid for whether or not its
result is used, so cutting it short wastes money to save less time
than the call had left. The page is the preemption point: every slot
is re-decided when its page finishes, and by the properties above the
most deserving task takes it. What a larger system needs preemption
for, a tenant holding capacity for minutes, cannot happen when the
unit of work is one page.

### What is deliberately absent

Weights that change by time of day, envelopes within which a tenant
adjusts its own weight, and weights calibrated from past demand. Each
is a layer over one integer. The integer comes from the authorizer,
and the operator's authorizer is the place to compute it from whatever
it likes.

### Visibility

`GET /queue` returns, per group the caller may see: weight, queued
and running tasks per class, queued parses, and the share of service it
received in the last interval. A parse's `progress.waiting` is `turn`
when its group has queued tasks and none leased.

## Not in this spec

Whether the model has room for the claimed page ([[007-model-capacity]]);
that is checked after the task is chosen and can send the worker to the
next candidate. Budgets ([[013-limits-and-usage]]).

## Acceptance criteria

The dispatch decision is a pure function of the rows it reads. The
simulation below drives that function with a virtual clock over the
real store.

| Criterion | Proven by |
|---|---|
| Three groups with weights 1, 2 and 4, all backlogged: after 700 dispatches each has been served within one task of 100, 200 and 400 pages | the dispatch simulation |
| A group that joins after the others have been served 10,000 pages receives its weight's share from its first task and no more | the dispatch simulation |
| Interactive and batch both backlogged: batch receives between 19% and 21% of dispatches; with no interactive work, batch receives all | the dispatch simulation |
| A group raising its own priorities, or queuing 100,000 tasks, changes no other group's dispatch count | the dispatch simulation, run with and without the change |
| A group at `max_running` is skipped and others proceed; it resumes when a task of its settles | a store test |
| 16 workers claiming concurrently from 100 groups: no task is claimed twice, and the served ratio stays within 5% of the weights | a concurrency test over Postgres |
| `queued` and `running` equal a recount from `tasks` after a soak run with kills | a consistency check in the soak test of [[004-durable-tasks]] |
| Concurrent submits at `max_queued` admit exactly the remaining number | a concurrency test |
