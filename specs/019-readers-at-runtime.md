---
title: "Readers at runtime: readers and the routing policy changed through the API, and an owner's default reader"
status: in-progress
track: core
depends_on:
  - specs/003-api.md
  - specs/008-readers.md
  - specs/012-identity-and-authorization.md
  - specs/013-limits-and-usage.md
affects: [api/, authorizer/, internal/access/, internal/config/, internal/httpapi/, internal/durable/, internal/keys/, internal/store/, internal/worker/, cmd/lectiod/]
effort: large
created: 2026-10-13
updated: 2026-10-13
author: changkun
---

# Readers at runtime

## Overview

A deployment's readers and the order the routing policy tries them in
are documents in `LECTIO_CONFIG`, read when a process starts
([[008-readers]]). Adding a model as a reader, or changing which model
a parse that names none is read with, takes a change of that file and a
restart of every process. [[008-readers]] shaped them as declared
objects "so that a later administrative API can apply the same
documents". This spec is that API, and the one limit that lets an
authorizer give each owner a default reader of its own.

## Current state

- `config.Load` builds the readers, the describers, the extractors and
  the 3 chains once; the API process holds them for `GET /readers` and
  for checking a reader a request names, and the worker holds them to
  call. The task store writes the pools and the chains to its
  `settings` row when a process opens it; the newest process's row is
  in force.
- `GET /readers` lists the configured readers. No route changes one.
- A request that names no reader is read by the read chain, and its
  figures and fields by the describe and extract chains. The allow's
  `readers` bounds the readers a request may name; nothing gives an
  owner another default.
- The worker asks the key endpoint for a context's key with the
  context's owner and group alone, so the key reaches the models the
  endpoint's side configured, whatever the readers are.

## Design

### Two kinds of reader

A reader is **configured** when a document of `LECTIO_CONFIG` declares
it, and **added** when the API stored it. The 2 are one set: names are
unique across both, a chain names either, and a request names either.
A configured reader is the deployment's file: the API reads it and
never changes or removes it, so a restart cannot bring back what an
administrator removed, nor remove what one added.

The routing policy is likewise **configured** (the file's `Policy`) or
**set** through the API. A set policy replaces the configured one
whole; removing it puts the configured one back.

### Adding a reader

`PUT /readers/{name}` stores a reader under the name: its body is the
`spec` of a `Reader` document as [[008-readers]] gives it, in JSON. It
is built as the file's documents are, and a body that would not load
from the file is refused with `400 invalid_request` naming the member.
The answer is the reader as `GET /readers/{name}` reads it, `201` when
the name was new and `200` when it replaced an added reader.

What a stored document may reach is the deployment's to say, not the
caller's:

- An endpoint must be one of `LECTIO_READER_ENDPOINTS`, a comma list.
  A body that names no endpoint takes the first. With the variable
  unset, the endpoints of the configured readers are the list, so a
  deployment whose readers all reach one gateway lets an administrator
  add a model of that gateway and nothing else.
- The `stub` adapter is refused: it reads nothing.
- A name is 1 to 63 of `a-z`, `0-9`, `.` and `-`, starting with a
  letter or a digit.

A name a configured reader holds is `409 conflict`. Removing an added
reader with `DELETE /readers/{name}` is `409 conflict` while a chain in
force names it, or while a parse, a figure run or an extraction that
named it has a task that has not ended: that work would wait for a
reader nobody runs. A configured reader is `409 conflict` too.

### Setting the policy

`PUT /policy` stores the routing policy: its body is the `spec` of a
`Policy` document, in JSON, checked as the file's is (every chain names
a reader that is there and can do what the chain asks of it). `GET
/policy` reads the policy in force, with `source`, `configured` or
`set`, and `DELETE /policy` removes a set policy and answers the
configured one. A change of the chains moves the work that has not
been claimed: a page waiting at a chain's first place is read by the
new first reader. Work already claimed finishes with its reader.

### Every process follows

A process reads the stored readers and policy when it opens the store,
and again whenever their version changes, which it checks every 10
seconds (`LECTIO_READERS_POLL`) and at once after a change it made
itself. It builds the configured documents and the stored ones into
one set, as one file of both would load; one that does not load is
logged and the set in force stays. A worker then writes the pools and
the chains to the task store's `settings`, as it does at start, so the
fair queue dispatches to the new set. Work a process holds when the set
changes ends with the reader it was claimed for.

A stored reader that every process cannot build, because this build
does not know an adapter or a member, is the case of a binary older
than the one that stored it; the older process keeps the set it had
and logs the reader it skipped. The change is not refused for it.

### Keys follow the readers

A worker asks the key endpoint ([[013-limits-and-usage]]) with
`models`, the models its chat readers reach, in name order. The key
endpoint may scope the key to them; one that does not read the member
answers as before. A worker's keys are kept by group and by that list,
so a reader added after a key was issued gets a key that reaches it.

### An owner's default reader

An allow may carry `reader`: the reader a request that names none is
held to, for the role each request asks of a reader.

| Request | Reads `reader` when |
|---|---|
| a submit | it names no `reader`: its pages are read by `reader` alone, as when the request names it |
| a figure run | it names no `describer` and `reader` describes figures |
| an extraction, and asking one again | it names no `extractor` and `reader` extracts |
| a set's create and change | the definition names no `extractor` and `reader` extracts: the set records it |

A `reader` that is no reader, or that `readers` on the same allow
leaves out, makes the answer no decision (`503
authorizer_unavailable`), as a limit that cannot be held does. A
default reader is a pin: a parse read by it is read by it alone, a page
that carries its own text included, as one that names it is. The
allows of `parse.create`, `set.create` and `set.update` are read for
it.

### Authorization

| Action | Kind | Asked by | Resource fields |
|---|---|---|---|
| `reader.list` | `Reader` | `GET /readers`, `GET /readers/{name}`, `GET /policy` | none |
| `reader.update` | `Reader` | `PUT /readers/{name}`, `PUT /policy`, `DELETE /policy` | `name` (the reader's; `policy` for the policy) |
| `reader.delete` | `Reader` | `DELETE /readers/{name}` | `name` |

The readers are the deployment's and nobody's: an authorizer allows
`reader.update` and `reader.delete` to the deployment's operators
alone. The owner policy allows them to `LECTIO_ADMIN_SUBJECTS`.

### What is stored

Migration 000010 adds `readers`, one row per added reader: the name,
the spec as one JSON document, and the 2 times; and `reader_policy`,
one row holding the set chains, or none, and the version every change
of either table adds 1 to. It touches no table in use.

## API

| Method | Path | Answer |
|---|---|---|
| `GET` | `/readers/{name}` | `200` the reader, with `source` and, for an added one, its `spec`; `reader_not_found` |
| `PUT` | `/readers/{name}` | `201` or `200` the reader; `400` for a spec that does not load; `409` for a configured name |
| `DELETE` | `/readers/{name}` | `204`; `409` for a configured reader, one a chain names, or one with work that has not ended |
| `GET` | `/policy` | `200` the chains in force and `source` |
| `PUT` | `/policy` | `200` the policy; `400` for chains that do not load |
| `DELETE` | `/policy` | `200` the configured policy |

`GET /readers` gains, on each reader, `source`, `adapter`, `model` for
one that reaches a model, and `describes` and `extracts`. A development
server answers `501 not_implemented` on the routes that change readers,
since it keeps nothing.

## Non-goals

- **A reader per owner.** An added reader is the deployment's; an owner
  is given a default among them, or a narrower list with `readers`.
- **A chain per owner.** The queue dispatches a chain's places to one
  reader each; an owner's default is a pin.
- **Changing a configured reader** through the API.

## Acceptance criteria

- A reader added through the API is listed, read by its pins and by a
  chain that names it, on every process within the poll, with no
  restart; its pages are read by its model.
- A spec that would not load from the file, an endpoint outside the
  list, the stub adapter and a configured name are refused, and change
  nothing.
- Removing an added reader a chain names, or one with work that has not
  ended, is `conflict`; once neither holds, it is removed everywhere.
- A set policy is in force on every process, and moves the work not yet
  claimed; removing it puts the configured policy back.
- A worker's key ask names its readers' models, and a reader added after
  a key was issued is read with a key that reaches its model.
- An allow's `reader` reads a submit, a figure run, an extraction and a
  set that name none with it; one that names a reader keeps its own; a
  `reader` outside `readers` fails closed.
- The down migration drops what this one adds.

## Implementation status

Built in the durable server: migration 000010 (the 2 tables, the 4
functions, and `lectio_configure` moving work past the end of a chain
that is now shorter), `config.File` building the configured and the
stored documents into one set, `readerset.Manager` putting a change in
force at once in the process that made it and in every other within
the poll, a worker reading the stored readers again for a claim of a
reader it does not hold, `models` on a key ask, `reader` on an allow,
the 6 routes and the 2 actions. A development server answers
`501 not_implemented` on the routes that change readers.
