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
units of work over time. Work is in one of two classes, and interactive
work goes ahead of batch work without ever starving it. Inside one
tenant's own queue, the tenant's priorities and the order of its parses
decide. The decision is made in two steps, tenant first and task
second, so that nothing a tenant queues can move it ahead of another
tenant.

## Current state

The earlier service chose the tenant with the smallest decayed service
counter and charged one unit per parse, in memory, per replica. The
idea is carried over, with three corrections: the charge is per page
rather than per parse, so a 3,000-page parse no longer costs what a
1-page parse costs; the accounting is in Postgres, so it holds across
restarts and replicas; and the classes, which existed in configuration
and were never wired, are part of the dispatch. Its preemption code is
not carried over, for the reason under Preemption below.

### What changed in review

The first draft of this spec charged `max(vtime, floor) + cost /
weight`, with `floor` the smallest virtual time among the groups that
had work. The group being charged was the one with the smallest virtual
time, so the floor was its own value and never applied. Run as written,
a group that joined after the others had been served 10,000 pages took
the next 5,000 slots in a row, and batch work that arrived after
100,000 interactive-only pages took 25,001 in a row. Taking the floor
over the other groups stops that and breaks the weights: three groups
at 1, 2 and 4 are served 140, 280 and 280. The draft also charged
nothing for `prepare`, `assemble` and extraction, so a group holding
only such tasks never advanced and won every pick. This revision keeps
a clock per class and charges every task.

## Design

### Group

A group is the unit fairness is computed over: a tenant. Every parse
belongs to one. The group id and its settings arrive in the limits on
the allow that admitted the parse ([[012-identity-and-authorization]]);
with no authorizer, the group is the owner and the settings are the
configured defaults.

```sql
CREATE TABLE groups (
  group_id     text PRIMARY KEY,
  weight       integer NOT NULL DEFAULT 1,   -- share of service, 1..1000
  max_running  integer NOT NULL DEFAULT 0,   -- leased tasks at once; 0 is no cap
  max_queued   integer NOT NULL DEFAULT 0,   -- non-terminal parses; 0 is no cap
  max_priority integer NOT NULL DEFAULT 0,
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE group_service (
  group_id text NOT NULL, class smallint NOT NULL,
  vtime    numeric NOT NULL DEFAULT 0,       -- virtual time: units charged / weight
  queued   integer NOT NULL DEFAULT 0,       -- queued tasks, maintained in the transaction of each transition
  running  integer NOT NULL DEFAULT 0,       -- leased tasks
  PRIMARY KEY (group_id, class)
);
CREATE TABLE class_service (
  class  smallint PRIMARY KEY,
  weight integer NOT NULL,
  vtime  numeric NOT NULL DEFAULT 0,         -- virtual time of the class: units charged / weight
  clock  numeric NOT NULL DEFAULT 0          -- virtual time inside the class: the start of its last dispatch
);
CREATE TABLE dispatch (
  one   boolean PRIMARY KEY DEFAULT true CHECK (one),
  clock numeric NOT NULL DEFAULT 0           -- virtual time across the classes
);
```

A group's settings are refreshed from the allow on each submit, so a
change made by the authorizer takes effect with the tenant's next
parse and needs no call into Lectio.

### The unit

Every task that is dispatched is charged. A task's charge is the
`cost` of the reader it is about to call, a number in the reader's
configuration that defaults to 1 ([[008-readers]]): a page read by a
small model and a call to a large one are not the same amount of work,
and an operator who runs both says so there. `prepare` and `assemble`
call no model and charge 1. An extraction charges its reader's `cost`
for each model call it makes ([[011-structured-extraction]]). No task
is free: a group whose queue holds only tasks that cost nothing would
never advance and would be chosen every time.

The charge is made at the claim, when the work is chosen, and corrected
at the settle to what the task used: the sum of the `cost` of every
call it made, and never less than 1, which is the worker slot it held.
A page that escalated to a second reader is corrected to both readers'
costs, an extraction to the calls it made, and a page that turned out
blank under a reader of `cost` 5 to 1. The correction is `vtime +=
(used - charged) / weight`, on the group and on its class. The floor of
1 is what keeps the correction from undoing the rule above: a group
whose pages are all blank, corrected to nothing, would be chosen every
time.

### The dispatch decision

For each free slot, in the claim step of the exchange
([[004-durable-tasks]]), which is serialized across the fleet:

1. **Eligible.** A task can be chosen when it is `queued`, its
   `available_at` has passed, its group's `running` is below
   `max_running`, and for a task that calls a model, a reader it may
   use has room ([[007-model-capacity]]). A group or a class is
   eligible when it holds such a task. What is not eligible is not
   looked at again in this step and is charged nothing.
2. **Class.** Among eligible classes, take the one with the smaller
   `max(vtime, dispatch.clock)`, interactive first on a tie. The
   weights are `LECTIO_CLASS_WEIGHTS`, default `interactive=4,batch=1`:
   when both classes have work, interactive receives four units in five
   and batch one in five. When only one class has work it receives all
   of them.
3. **Group.** Among eligible groups of that class, take the one with
   the smallest `max(vtime, class.clock)`, ties broken by group id.
4. **Task.** That group's first eligible task in the class, `ORDER BY
   priority DESC, seq, created_at`.
5. **Charge.** With `start = max(group.vtime, class.clock)`: set
   `class.clock = start` and `group.vtime = start + cost /
   group.weight`. The same one level up: with `start = max(class.vtime,
   dispatch.clock)`, set `dispatch.clock = start` and `class.vtime =
   start + cost / class.weight`.

This is start-time fair queuing. The clock is what the first draft
lacked: it is the virtual time of the work being started, it only
moves forward, and a group or a class whose own virtual time fell
behind it, because it had nothing to run or nothing that could run,
starts at the clock and not at its old value. Three properties follow.

- **Share follows weight.** Over any interval in which two groups both
  have eligible work, the units each is served are in the ratio of
  their weights, within one task.
- **Absence is not banked.** A group that returns after an hour of
  nothing, or whose reader was paused for an hour, starts at the clock.
  It gets its share from now on and no burst for the past. The same
  holds for a class: batch work that arrives after a day of
  interactive-only service starts at the clock and takes one unit in
  five, not the next twenty thousand.
- **Arrival is fast.** A group with new work is at the clock, which no
  waiting group is below, so its task is among the next to be served.
  With page-sized tasks that is at most the length of one page call
  per group tied with it.

### Inside a group

`priority` is the caller's, from `-max_priority` to `max_priority`,
default 0. It orders the group's own tasks only.

`seq` is a task's position within its parse ([[005-parse-graph]]).
Ordering by `seq` before `created_at` serves the first page of every
parse of the group, then the second of each, and so on: parses of one
tenant advance together, a short one finishes early, and a long one
cannot hold the tenant's own queue. A tenant that wants strict
first-in-first-out for its parses sets priorities.

`prepare`, `assemble` and extraction tasks are ordered ahead of page
tasks of the same priority by a `seq` below 0, so a parse whose pages
are done is not kept waiting behind its own tenant's backlog to be
assembled. They are charged like any other task; the order inside a
group moves no group ahead of another.

### Admission at submit

A submit is refused with `429 queue_full` and `Retry-After` when the
group already holds `max_queued` non-terminal parses. The submit locks
the group's row (`SELECT ... FROM groups WHERE group_id = $1 FOR
UPDATE`), counts, and inserts, in one transaction, so two submits of
one group are serialized and cannot both pass at one below the limit.
A count inside the insert statement does not do this: each statement
counts from its own snapshot. Page and spend budgets are
[[013-limits-and-usage]].

### Preemption

Lectio does not interrupt a model call to give its slot to someone
else. A page call lasts seconds and is paid for whether or not its
result is used, so cutting it short wastes money to save less time
than the call had left. The page is the preemption point: every slot
is re-decided when its page finishes, and by the properties above the
most deserving task takes it. What a larger system needs preemption
for, a tenant holding capacity for minutes, cannot happen when the
unit of work is one page.

That argument rests on a task being one model call. A task that makes
several calls keeps it true for model capacity by holding a slot per
call and not for its whole lease ([[007-model-capacity]]). It does not
keep it true for the worker slot: a task that ran for minutes would
hold a worker that long, and whether such a task is split into
page-sized steps or is preempted between its calls is open
([[004-durable-tasks]]).

### What is deliberately absent

Weights that change by time of day, envelopes within which a tenant
adjusts its own weight, and weights calibrated from past demand. Each
is a layer over one integer. The integer comes from the authorizer,
and the operator's authorizer is the place to compute it from whatever
it likes. A unit that follows the tokens a call used: the unit is a
call weighed by its reader, which is known when the task is chosen.

### Visibility

`GET /queue` returns, per group the caller may see: weight, queued
and running tasks per class, queued parses, and the share of service it
received in the last interval. A parse's `progress.waiting` is computed
when it is read: `turn` when the parse has queued tasks that could run
and none leased, `capacity` when its queued tasks have no reader with
room. Nothing is written while a task waits.

## Not in this spec

How a reader's room is counted ([[007-model-capacity]]); the dispatch
decision only asks whether there is any. Budgets
([[013-limits-and-usage]]).

## Implementation status

Nothing of this spec is built. `internal/run`, the in-process runner,
stands in for it: every parse's pages wait in one queue ordered by
class, then priority, then position in their parse, then age, so
interactive work goes first and two parses of the same standing
advance together. It knows no group and no weight: nothing is fair
between tenants, batch work can wait without bound behind interactive
work, and no submit is refused for a full queue.

## Acceptance criteria

The dispatch decision is a function of the rows it reads. The
simulation below drives it with a virtual clock over the real store.

| Criterion | Proven by |
|---|---|
| Three groups with weights 1, 2 and 4, all backlogged: after 700 dispatches each has been served within one task of 100, 200 and 400 units | the dispatch simulation |
| A group that joins after three others were served 10,000 pages is served at most one task before another group is served, and over the next 3,000 dispatches four equal groups receive 750 each within one task | the dispatch simulation |
| Batch work that arrives after 100,000 interactive-only dispatches receives at most one dispatch in a row, and then between 19% and 21% of dispatches; with no interactive work, batch receives all | the dispatch simulation |
| A group whose reader was paused while two others were served 10,000 pages receives its weight's share from the first dispatch after the pause and no more | the dispatch simulation with a stub pool |
| A group that queues 10,000 `prepare` tasks, or 10,000 extractions, receives its weight's share of dispatches and changes no other group's share | the dispatch simulation, run with and without the group |
| A group raising its own priorities, or queuing 100,000 tasks, changes no other group's dispatch count | the dispatch simulation, run with and without the change |
| A page read by a reader of `cost` 5 advances its group's virtual time five times as far as one of `cost` 1; a blank page under that reader is corrected to 1; a page that escalated is corrected to the sum of both readers' costs | a store test |
| A group whose 3,000 pages are all blank receives its weight's share of dispatches against a group whose pages are read, and no more | the dispatch simulation |
| A group at `max_running` is skipped and others proceed; it resumes when a task of its settles | a store test |
| 16 workers claiming concurrently from 100 groups: no task is claimed twice, and the served ratio stays within 5% of the weights | a concurrency test over Postgres |
| `queued` and `running` equal a recount from `tasks` after a soak run with kills | a consistency check in the soak test of [[004-durable-tasks]] |
| 50 concurrent submits of one group at `max_queued` minus 10 admit exactly 10 | a concurrency test over Postgres |
