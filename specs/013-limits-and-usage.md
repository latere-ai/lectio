---
title: "Limits and usage: what a parse and a group are held to, whose credential a page is read with, and the meters Lectio records"
status: in-progress
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
now a reservation made when the page count is known.

The second draft made the meter one row per parse and reader, summed
from the task rows when the parse ended and rolled into hourly sums by
a sweep. A task row cannot give that sum either: it holds the calls and
the tokens of every attempt in one figure and the reader of its last
claim alone, so a page that moved from one reader to the next would be
metered under the second for what the first was called for. The meter
is now written by each settle, which knows the reader its attempt was
claimed for, straight into the row of its hour.

## Design

### Limits and where each is enforced

| Limit | Source | Enforced |
|---|---|---|
| file size | server setting, lowered by the allow | at upload and at snapshot, while streaming |
| pages per parse | server setting on the document's pages, lowered by the allow for the pages the parse selects | in `prepare`, after counting; `too_many_pages` |
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

The day is a date in UTC. The limit is the group's setting, written by
each submit of the group as its bounds are
([[006-fairness-and-priority]]), so a limit the authorizer changes holds
from the group's next submit. A page taken from an earlier read counts
as a page read: it is reserved and not given back.

A retry reserves the pages it queues again, of the day of the retry and
against the limit as the group's row then holds it, and is refused with
`402 budget_exhausted` when the day does not hold them
([[004-durable-tasks]]). The pages the parse read before stay counted on
the day they were reserved on, and what the retried parse gives back
when it ends is the pages of the retry it did not read, to the day of
the retry.

The two counts of [[006-fairness-and-priority]], queued parses and
running tasks, are made under a lock on the group's row for the same
reason.

### Whose credential reads a page

The key a reader call is made with decides who the model endpoint
charges. Lectio resolves it per group through one interface, in
`internal/keys`.

```go
type Source interface {
    Key(ctx context.Context, group, owner, parse string) (reader.Credential, error)
}
```

| `LECTIO_KEYS` | Behavior |
|---|---|
| `static` (default) | one key from configuration, `LECTIO_MODEL_KEY`, for every group; the operator pays and bills as it sees fit from the meters |
| `endpoint` | `POST LECTIO_KEYS_URL` with the group, owner and parse, authenticated with the bearer of `LECTIO_KEYS_TOKEN`; the answer is a key and its expiry, held in memory until 1 minute before it expires |

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

### Which process reads the key variables

The bearer is `LECTIO_KEYS_TOKEN`, a credential of its own and not the
authorizer's. The API faces callers and holds the authorizer's bearer.
A bearer that obtains every tenant's key must not be in that process,
so only a process that runs tasks reads it.

| Variable | Roles `worker` and `all` | Role `api` |
|---|---|---|
| `LECTIO_KEYS` | read | read |
| `LECTIO_KEYS_URL`, `LECTIO_KEYS_TOKEN` | read; with `endpoint` both are required | not read, and their absence is no error |
| `LECTIO_MODEL_KEY` | read; with `endpoint` it must not be set | read and not used: the API reads no page, and the variable beside `endpoint` is no error there |

The API reads `LECTIO_KEYS` because the source decides the scope a rate
limit pauses, which is a setting of the task store, and each process
that opens the store writes its settings ([[004-durable-tasks]]). An API
and a worker that disagreed would rewrite the scope with every start.

A process that runs tasks is refused at start, with an error that names
the variables and no value, when:

- `LECTIO_KEYS` is neither `static` nor `endpoint`;
- it is `endpoint` and `LECTIO_KEYS_URL` is not set or is not an
  absolute `http` or `https` URL, or `LECTIO_KEYS_TOKEN` is not set;
- it is `endpoint` and `LECTIO_MODEL_KEY` is set: a page is read with a
  key from one source, and an installation that names 2 has not decided
  who pays;
- it is `static` and `LECTIO_KEYS_URL` or `LECTIO_KEYS_TOKEN` is set: an
  installation that believes its tenants read with their own keys must
  not read every page with the operator's.

A development server is refused `endpoint`. Its runner reads every page
with one key and has no scope to pause
([[012-identity-and-authorization]]).

### The key endpoint: the request and its answers

A worker asks when it is about to read a page of a group and holds no
key for the group that still stands.

```
POST <LECTIO_KEYS_URL>
Authorization: Bearer <LECTIO_KEYS_TOKEN>
Content-Type: application/json

{"group": "<the task's group>", "owner": "<the parse's owner>", "parse": "<the parse id>"}
```

| Answer | Meaning | What the page does |
|---|---|---|
| `200` with `{"key": "...", "expires_at": "<RFC 3339>"}` | the group's key | is read with it |
| `402` | the group has no budget | fails at once with `budget_exhausted`, as a budget refusal of the reader's own endpoint does ([[008-readers]]) |
| `403` | the group may not read with this operator's keys | fails at once with `reader_not_permitted` |
| a transport error, a timeout, `5xx`, `429`, `401`, any other status, a body that does not parse, a body with no key or no expiry, an expiry that is past or less than 1 minute away | the endpoint is unavailable | waits, and does not fail |

`402` and `403` are about the group and no wait changes them, so they
are permanent failures of the page ([[004-durable-tasks]]): no attempt
is spent on a retry, the page does not move to another reader, since
every reader would be called with the same missing key, and nothing
counts against a reader's breaker, since no reader was called. A `403`
is reported as `reader_not_permitted`, the code a submit is refused with
when it names a reader its group may not use: the page is not at fault,
so it is not `page_unreadable`, and no reader failed, so it is not
`reader_unavailable`, which a caller reads as an outage and tries again.
What a refused group needs is its operator's permission, and a client can
say so only when the code says so. A `401` is
not a refusal. It says the bearer is wrong, which is the operator's to
mend and says nothing about a tenant, so a tenant's page does not fail
for it.

The details of the 2 failures are fixed sentences. Neither holds the
endpoint's address, its bearer, or a byte of its answer.

### What a worker holds of a key, and for how long

Everything below is in the memory of one worker process and nowhere
else. A key is never written to the database, the object store, a log
line or an error.

- A key is held by group until 1 minute before its `expires_at`, and is
  then asked again. The minute is the margin between handing a key to a
  call and the call reaching the gateway, which checks the key when the
  call arrives: it covers rendering the page and the difference between
  the 2 clocks. A key is never handed to a call with less than the
  margin left, so an answer whose key expires within the margin is
  unusable and counts as unavailability. An operator's plane therefore
  issues keys good for well over 1 minute.
- One request per group is in flight at a time. Tasks of the group that
  need the key meanwhile wait for that answer and send nothing. A task
  that is canceled while it waits leaves the request to the others.
- A refusal is held for 5 seconds, so the queued pages of a refused
  group fail on one request and a group whose budget was raised reads
  again within seconds.
- Unavailability is held until the next request is due: 1 second after
  the first such answer for the group, twice as long after each further
  one in a row, and at most 30 seconds. A `Retry-After` in whole
  seconds on the answer is taken as the wait, with the same bound.
- One request is bounded at 10 seconds.

### A key endpoint that does not answer

A page whose group has no key yet must not fail, must not spend an
attempt and must not be claimed over and over. The mechanism is the one
a rate limit already uses ([[004-durable-tasks]],
[[007-model-capacity]]): the worker ends the attempt as a wait, with the
time until the endpoint is asked again. The store returns the task to
the queue with its `attempt` unchanged and pauses the key scope the
task was claimed in, which with `endpoint` is the group. Until the
pause ends no page of the group is claimed for that reader, by any
worker, and nothing is written for them. When it ends the scope admits
pages again, the endpoint is asked once for however many were claimed,
and either they are read or the scope is paused for the next, longer
wait. No reader is called, so no call is metered and the breaker hears
nothing.

Reusing the rate limit's pause has 2 costs, both taken on purpose over
a second mechanism in the store. Each wait halves the scope's ceiling
as a rate-limit reply does, so an outage of several waits leaves the
group at one call in flight, and its calls to the reader return over up
to 10 intervals of `LECTIO_POOL_RECOVERY`, 5 minutes at the default,
and not all at once. And a paused scope is passed over by a page that
follows the routing policy's chain, so while the endpoint is down a
group's pages are offered to the next reader of the chain, which finds
no key either and pauses in its turn; a few pages may be read by that
reader in the moment the endpoint returns, and their results say so. A
parse that named its reader waits for it.

A page waits no longer than its parse's deadline, as every page does.

A worker whose key endpoint is down is ready. Readiness is the database
and a recent exchange ([[016-distribution]]). The pages that need a key
wait in the queue, the worker still runs `prepare`, `assemble` and the
pages of every group it holds a key for, and taking it out of a roll
would mend nothing. A server does not ask the endpoint at start: a
group's key is asked when its first page is read.

### Meters

The meter is written as tasks settle. Each settle carries what its
attempt used, the model calls and the tokens they took in and gave out
([[004-durable-tasks]]), and the task store adds it, in the transaction
of the settle, to the row of the hour:

```sql
CREATE TABLE usage (
  hour          timestamptz NOT NULL,        -- the start of the hour, in UTC, the work settled in
  group_id      text   NOT NULL,
  owner         text   NOT NULL,
  kind          text   NOT NULL,             -- the kind of task: page
  reader        text   NOT NULL DEFAULT '',  -- the reader the attempt was claimed for; '' for pages no reader read
  pages         bigint NOT NULL DEFAULT 0,   -- pages that were read
  calls         bigint NOT NULL DEFAULT 0,   -- model calls, the ones that failed or were told to wait included
  input_tokens  bigint NOT NULL DEFAULT 0,
  output_tokens bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, group_id, owner, kind, reader)
);
```

A row is the sum of an hour for one group, one owner, one kind of task
and one reader. It holds counts and names, never content.

- **What was spent is recorded, and not only what was useful.** An
  attempt that failed, or was told to wait, reports its calls and
  tokens with its outcome, and they are metered as the success's are.
- **Under the reader that was called.** A settle meters under the
  reader its attempt was claimed for. A page that moved down the
  policy's chain is metered under each reader that was called for it.
- **A page counts when it is read.** A page task that succeeds adds 1
  page, whether or not a call was made: a page with nothing on it and a
  page taken from an earlier read are pages read, under the reader they
  were claimed for and with no call. A page that failed adds its calls
  and no page, and adds its page when a retry reads it.
- **Native pages are pages.** The pages of a format that needs no
  reader are metered when `prepare` settles, with no call and no token,
  under the reader with no name, so a page means the same thing for
  every format.
- **In the hour of the settle.** A parse that runs across the turn of
  an hour is metered in both hours, each with what settled in it.

The table grows with the hours a group, an owner and a reader were
active in, and never with pages or parses: a group that reads a million
pages in an hour with one reader adds 1 row. There is no row per page,
no row per parse, no sweep that rolls rows up and no retention of
detail: the detail of one parse is on the parse.

The meter and the parses agree. Every settle that adds to a parse's
`calls`, tokens and `pages_done` adds the same to the meter in the same
transaction, so the meter summed over a group and an owner equals their
parses summed, for as long as the parses are there. A parse that is
deleted takes its row with it and leaves the meter as it is.

One thing is not metered here: a call whose worker died or was taken
for dead before it could report. Its tokens were spent at the endpoint
and no live lease can record them ([[004-durable-tasks]]). The
endpoint's own meter has them, and the difference is bounded by the
calls in flight at each kill.

`GET /usage?by=group|owner|reader&interval=hour|day&from=&to=` returns
sums by a key over fixed intervals in UTC ([[003-api]]). `from` is
moved back to the beginning of its interval and `to` on to the end of
its own, so no sum is of a part of an interval, and the answer says the
span it covers. A read that names no `to` ends now, and one that names
no `from` goes back 24 hours, or 30 days for `interval=day`. A read
spans at most 1000 intervals. An interval with nothing in it has no
sum. The shape, sums by a key over fixed intervals, is what a billing
job folds into a ledger without asking Lectio about each parse.

Whose usage a caller is answered follows from the question,
`usage.read` ([[012-identity-and-authorization]]). `owner` and `group`
on the request ask for one owner's or one group's, and the question
carries them. The answer is narrowed to them and to the owners the
allow's filter lists: under the owner policy a caller reads its own
usage and an admin everyone's. A filter that lists nobody, or that
narrows by labels, which the meter does not carry, is answered with no
sum.

The parse's own `usage` ([[003-api]]) is kept on the parse row and
advanced by each settle. Each page's result carries the attempts and
tokens of that page ([[002-object-model]]).

Not in the meter: the model, and a cost. A settle carries the reader
its attempt was claimed for and no model name, so the sums are by
reader, and an operator who runs 2 models as 2 readers reads them
apart. No endpoint's reported cost reaches a settle, so the meter
holds none. An extraction, when it is built, meters under its own kind.

### What Lectio does not do

It does not convert tokens to money unless the endpoint reported the
cost, does not know a price per page, and does not stop a tenant
because of a balance. A plane that prices by the page reads the page
meter; one that passes model cost through reads the gateway's.

## Not in this spec

Plans, prices, invoices, wallets and entitlements. Budget alerts. A
per-tenant report in a user interface.

## Implementation status

Built:

- Pages per day, in the durable server. Migration `000004_limits`
  carries `group_days`, the group's `pages_per_day`, and the statement
  that reserves (`lectio_reserve`). The settle of `prepare` reserves the
  parse's selected pages against the group's day and fails the parse
  with `budget_exhausted` when the day does not hold them, before a
  page task is written. A submit of a group whose day holds nothing more
  is `402 budget_exhausted`. The pages a parse reserved and did not read
  go back to the day it reserved on when it ends, by its last task, by a
  cancel or by its deadline. Every statement is one function call, as
  the rest of the store's, and runs behind a transaction-mode pooler.
- Pages per parse. The server's `LECTIO_MAX_PAGES` bounds a document's
  pages in intake ([[009-intake]]). The allow's lower `max_pages` bounds
  what one parse selects: it is stored on the parse and held in the
  settle of `prepare`, and in a development server by the in-process
  runner.
- File size: `LECTIO_MAX_FILE_BYTES`, lowered by the allow, while an
  upload is read and at a submit ([[003-api]]).
- The deadline, attempts and expiries ([[004-durable-tasks]]).
- The default of a group's pages per day, with its other defaults, is
  `LECTIO_GROUP_DEFAULTS` ([[016-distribution]]).
- In the task store ([[004-durable-tasks]], [[007-model-capacity]]):
  whether keys are one for every group or one per group is a setting
  the store is opened with, and it decides the scope a slot is taken in
  and a rate limit pauses. `LECTIO_KEYS` sets it, in every role.
- The key source, in the durable server. `internal/keys` holds the
  interface, the static source, and the endpoint source with its
  memory: a key until 1 minute before it expires, a refusal for 5
  seconds, one request in flight per group, and the wait that grows
  while the endpoint issues nothing. `internal/config` reads the 3
  variables by role and refuses what a process cannot run with. A
  worker resolves the key before it fetches or renders anything of the
  page, ends the attempt as a wait when the source cannot say, and
  fails the page on a refusal. A page taken from an earlier read calls
  no reader and asks for no key.

- The meters, in the durable server. The fifth migration carries the
  `usage` table, `lectio_meter`, which is its one writer, and
  `lectio_usage`, which reads it. `lectio_settle` meters what each
  attempt used under the reader it was claimed for, with the page when
  the task succeeds, and the settle of `prepare` meters the pages of a
  format that needs no reader. `GET /usage` reads the sums by group,
  owner or reader over hours or days, asks `usage.read` with the owner
  and the group its request names, and narrows its answer by the
  allow's filter. No worker changed: the settle already carried the
  calls and the tokens.
- A retry reserves its failed pages again, in `lectio_retry`.

Remaining:

- A development server holds no budget: it refuses an allow, and a
  default, that sets `pages_per_day`
  ([[012-identity-and-authorization]]).
- Submits per minute, and model tokens per parse.
- The key source in a development server: it reads every page with the
  key of `LECTIO_MODEL_KEY` and is refused `endpoint`.
- A page with nothing on it asks for its group's key although it calls
  no reader: the key is resolved before the page is rendered, which is
  where a blank page is found.
- A reader's endpoint that refuses a key the key endpoint issued, a key
  revoked before its expiry for one, is the reader's `Misconfigured`
  ([[008-readers]]): the key is not asked again before it expires, and
  the refusals count against the reader's breaker for every group.
- No test reads a trace for a key.
- The meters hold no model and no cost, and `GET /usage` has no `by`
  for either. A development server keeps no meter and answers the route
  with `501`.
- A URL source is held to the allow's lower file size after it was
  fetched under the server's own, and not while it streams.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| Each row of the limits table has a test at the limit and one past it, with the stated code | for the rows that are built: file size `TestEachMemberOfAnAllowChangesTheOutcome/MaxFileBytes` and `TestUploadsAreChecked`; pages per parse `TestAParseSelectsNoMorePagesThanItsAllowLets` and `TestAParseAtTheLimitOfItsPagesIsRead`; pages per day `TestAGroupsPagesForADayAreReserved`; queued parses `TestConcurrentSubmitsAtMaxQueued`. Submits per minute and tokens per parse are not built |
| A group with 10 pages of daily budget left submits a 300-page file: the parse fails in `prepare` with `budget_exhausted` and no reader call is made | `TestEachMemberOfAnAllowChangesTheOutcome/PagesPerDay`, through the API over the durable backend with a counting stub reader, at 2 pages left and a file of 3 |
| A group with 10 pages of daily budget left submits 50 parses of 10 pages at once: exactly one reserves, 49 fail in `prepare`, and 10 pages are read | `TestFiftyParsesWithTenPagesLeft`, with 5 workers settling at once, and `TestReservationsAtOnceNeverPassTheLimit` for the statement alone; both on a direct connection, in the query mode that prepares nothing, and through PgBouncer in transaction mode |
| A parse that reserved 100 pages and was canceled after 30 gives 70 back, and a parse of 70 pages submitted next reserves | `TestPagesReservedAndNotReadAreGivenBack`, a store test over Postgres |
| A parse submitted with no deadline has `deadline_at` at `LECTIO_MAX_DEADLINE` from its submit | an API test |
| Submit-rate limiting holds across two API replicas: 120 per minute in total, not each | a test with two processes |
| With `LECTIO_KEYS=endpoint`, pages of 2 groups are read with 2 different keys, and the stub gateway's records attribute each page to its group | `TestPagesOfTwoGroupsAreReadWithTwoKeys`: the API and a worker as 2 servers of `cmd/lectiod` over Postgres and a bucket, a stub key endpoint, and a stub gateway that speaks chat completions and records the key of every call. A parse of 3 pages and one of 2, of 2 groups: one request per group to the key endpoint, 3 calls with the first group's key and 2 with the second's |
| No key appears in any log line, trace attribute, database row or error body | in part. The same test reads, after its run, both servers' logs, every row of every table, every object of the bucket and the pages the API answers, for the keys and for the endpoint's bearer. `TestAnAnswerThatIsNoKeyAndNoRefusalIsUnavailability` and `TestAnEndpointSourcePrintsNothingItHolds` hold what a source returns, logs and prints as to the same. No test reads a trace |
| A key endpoint that fails, or that refuses the bearer, fails no page and spends no attempt: the pages wait unclaimed behind the group's paused scope, no reader is called, the worker stays ready, and the pages are read on their first attempt once the endpoint answers | `TestAKeyEndpointThatIsDownLeavesPagesUnclaimedAndNotFailed`, through the durable server, with the endpoint answering `503`, `401` and `503` again before it issues |
| A `402` fails the group's pages with `budget_exhausted` and a `403` with `reader_not_permitted`, on one request for a parse of 3 pages, with no reader called, no scope paused and nothing counted against a reader, while another group reads | `TestAGroupTheKeyEndpointRefusesFailsItsPagesAtOnce`, through the durable server |
| A key is asked again 1 minute before it expires and not before; calls of one group that arrive while its key is asked for wait for the one request | `TestAGroupsKeyIsAskedOnceAndHeldUntilShortlyBeforeItExpires` and `TestCallsOfOneGroupWaitForOneRequest`, on a clock the test moves |
| `401`, `5xx`, `429`, a body that is not the contract's, no key, no expiry, an expiry that is past or within the margin, a refused connection and a request that outlasts its bound are each unavailability, with a wait that doubles from 1 second to 30 | `TestAnAnswerThatIsNoKeyAndNoRefusalIsUnavailability` and `TestTheWaitGrowsWithEachAnswerInARowAndIsBounded` |
| A worker and a process in both roles are refused at start `endpoint` with no address, with no bearer or beside a model key, and an address or a bearer beside `static`; the API reads neither and starts | `TestAKeySourceThatCannotRunIsRefused`, `TestTheAPIReadsNeitherTheEndpointNorItsBearer` and `TestTheEndpointIsReadByAProcessThatRunsTasks` of `internal/config`, and `TestAKeySourceThatCannotRunStopsAProcessThatRunsTasks` of `cmd/lectiod` |
| The meter of a group sums to the stub endpoint's own count of calls and tokens served to it, failed and rate-limited attempts included, in a run with no worker killed | `TestARateLimitOnOneGroupsKeyPausesThatGroupAlone` of `cmd/lectiod`, through the durable server with a stub gateway that records every call: the limited group's meter reads its 3 pages, a call for each and for each rate limit, and the tokens of the 3 calls that were served |
| In a run with workers killed, the meter holds no more calls than the endpoint saw, and fewer by at most the calls in flight at the kills, and it equals a recount of what the settles recorded on the parses | `TestAKilledWorkerLosesItsLeaseAndNotTheWork` of `cmd/lectiod`, on every run; `TestSoak`, with `LECTIO_SOAK=1`, over 1,000 parses and random kills |
| Each settle meters what its attempt used under the reader it was claimed for: a failed call as the call it was, a page that moved to the next reader under both, a page with no call and a native page as pages read; the rows are one per hour, group, owner and reader | `TestTheMeterIsWrittenAsTasksSettle`, on a direct connection, in the query mode that prepares nothing, and through PgBouncer in transaction mode: 10 pages of 3 owners in 2 groups over 2 hours are 5 rows |
| The meter equals a recount of the parses after a failed page, a wait, a retry, a cancel with a call spent and a worker that died | `TestTheMeterEqualsARecountAfterEveryWayAParseEnds`; every case of the store ends with the check that no owner's meter holds less than its parses recorded |
| `GET /usage` sums by group, owner or reader over hours or days, equals what the caller's parses say they used, answers a caller its own usage and an admin everyone's, and refuses a parameter that is not the contract's | `TestUsageIsReadFromTheMeters` through the API over the durable backend, `TestAReadOfTheMetersIsCheckedAndNarrowed` |
