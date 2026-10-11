---
title: "Sets: a stored definition of fields, made before its files and asked of each parse that joins it"
status: in-progress
track: core
depends_on:
  - specs/003-api.md
  - specs/011-structured-extraction.md
  - specs/012-identity-and-authorization.md
affects: [api/, authorizer/, internal/access/, internal/httpapi/, internal/durable/, internal/store/, internal/id/]
effort: medium
created: 2026-10-11
updated: 2026-10-11
author: changkun
---

# Sets

## Overview

A caller who reads many files of one kind, such as a quarter's
invoices or a year of surveys, asks each the same fields: one schema,
one set of instructions, one extractor. A set holds that definition
once, under a name, before any file is read. A parse joins it and is
asked the set's fields; a change to the set asks every parse in it
again; and the set's table, a row a parse and a column a field, is one
read.

## Current state

Extraction ([[011-structured-extraction]]) is a request against one
parse. Nothing groups extractions across parses, so a client that wants
a set keeps one itself. The platform console did it with a label on
each parse, `fields=<name>`, and took the set's definition from what
its newest parse was asked with. 3 things follow from that, and each is
why this spec exists:

- A set exists only from its first file on: a definition cannot be
  made, kept or shared before a file is read.
- The list of sets is a scan of the owner's parses, grouped by label,
  and a set's table is a read of each parse's extraction.
- A program reaches a set only by knowing the console's convention.

## Design

### A set

A set belongs to one owner and holds:

| Member | Meaning |
|---|---|
| `id` | `set_` and a ULID, as a parse's and a file's ids are made |
| `name` | what the set is called, and the name of the extraction it asks of each parse |
| `schema`, `instructions`, `citations`, `extractor` | the request for an extraction ([[011-structured-extraction]]), without its name |
| `version` | 1 when made, and 1 more at each change of the definition |
| `parses` | how many parses are in the set, and how many of their extractions are pending, succeeded and failed |
| `created_at`, `updated_at` | when it was made and when its definition last changed |

The name is held to what an extraction's name may be: lower case
letters, digits, underscores and hyphens, a letter first, 63 at most.
It is unique among the owner's sets, and it never changes: a set is
renamed by making another. A set is addressed by its id, since a name
is unique only within its owner, and a stored object is read by its id
before the authorizer is asked about it ([[012-identity-and-authorization]]).
The list finds a set by name.

### Its parses

A parse is in a set when it holds an extraction under the set's name
that the set asked. That extraction is the parse's row of the set: its
state, its object and its citations are read as any extraction's are.
A parse is in a set of its own owner only, so the set's work is the
parse owner's and runs in the parse's group, as an extraction does.

`PUT /sets/{set}/parses/{parse}` adds a parse. What it does depends on
what the parse holds under the set's name:

| The parse holds | The answer |
|---|---|
| nothing under the name | the extraction is asked with the set's definition, as `POST /parses/{parse}/fields` asks one, under the 64 a parse holds |
| the set's extraction, asked with the set's version | nothing is asked; the extraction is answered as it stands |
| an extraction of the name that has ended and whose request is the set's, byte for byte | the extraction joins the set as it stands, and nothing is asked |
| an extraction of the name that has ended, asked with another request | it is asked again with the set's definition ([[011-structured-extraction]], "Asking again") |
| an extraction of the name that has not ended, and is not the set's at its version | `conflict`: its task may be running |

The third row is what lets a client that kept its own sets, as the
console did, move them into stored sets without paying for a model
call again. The request of an extraction is kept as the text of one
JSON document, and a set's definition is kept the same way, so the
comparison is of the bytes the core wrote for each.

A parse joins a set while it is read too. Its extraction then waits for
the parse to end, as one asked of a running parse waits.

A set holds at most 10,000 parses (`MaxSetParses`); a parse beyond it
is refused with `conflict`. A change of the definition touches every
parse in the set in one transaction, and the bound keeps that
transaction short.

A parse leaves a set when it is deleted or its retention ends: its
extractions go with it. There is no other way to leave a set in this
version (see Non-goals).

An extraction of a set is asked with the set's definition and with no
other. `PUT /parses/{parse}/fields/{name}` on it with the set's request
asks it again, as after a failure, and it stays in the set; with
another request it is `conflict`, since the set's table would then
hold a row the set did not ask. A change of what a set's parses are
asked is a change of the set.

### A change of the definition

`PUT /sets/{set}` replaces the definition and adds 1 to the version.
Each parse in the set is then in one of 3 places, and the change does
for each what asking it again would:

- **Its parse is still being read**, so its extraction has no task yet.
  The extraction's request is replaced where it stands, and it is asked
  with the new one when the parse ends.
- **Its extraction has ended.** It is asked again with the new
  definition: pending, its last object, citations and error gone, its
  counts of calls and tokens at 0, its task queued at once. The object
  the asking replaced is removed.
- **Its extraction's task is queued or running.** The change is refused
  with `conflict`, and nothing of the set or of any parse changes.

The last is the rule an extraction asked again already has: a task that
may be running is not raced. A change waits for the set's running
extractions to end, which is a call or a few each. The answer says
nothing of which parse was busy; the set's `parses.pending` says how
many are, and its list of parses which.

### Deleting a set

`DELETE /sets/{set}` removes the set. Its parses, and their extractions
under its name, stay: each is an extraction of its parse as any other
is, read and asked again by name. A set made later under the same name
starts with no parses, and a parse joins it by the third or fourth row
of the table above.

A client that wants a set's parses gone deletes them, as the console
does, and the set after.

### Reads

- `GET /sets` lists the sets the caller may see, newest first, a page at
  a time, as `GET /parses` does. `name` narrows the list to the set of
  that name. The allow's filter narrows it to the owners the caller may
  see.
- `GET /sets/{set}` reads one set.
- `GET /sets/{set}/parses` lists the set's parses, newest first, a page
  at a time, each with its extraction under the set's name: the parse as
  `GET /parses/{parse}` answers it and the extraction as
  `GET /parses/{parse}/fields/{name}` does.

### Authorization

A set is a resource of its own kind, `Set`, with 5 actions:

| Action | Asked by | Resource fields sent |
|---|---|---|
| `set.create` | `POST /sets` | `owner`, `name` |
| `set.read` | `GET /sets/{set}`, `GET /sets/{set}/parses` | `id`, `owner`, `name` |
| `set.list` | `GET /sets` | none |
| `set.update` | `PUT /sets/{set}`, `PUT /sets/{set}/parses/{parse}` | `id`, `owner`, `name` |
| `set.delete` | `DELETE /sets/{set}` | `id`, `owner`, `name` |

`set.update` is asked by the 2 requests that queue model work. Its
allow is read for `readers`, which an extractor the set names is held
to, as the extraction's own request is. The allow of `set.create` is
read for `owner`, the owner the set is recorded under, and for
`readers`. The work a set queues is its parses' owner's, so the limits
of a parse hold it, not the limits of the allow. A join asks about the
set alone: the parse must be the set's owner's, and a parse of another
owner is answered as one that is not there.

### What is stored

A table `sets`: the id, the owner, the name, the definition as one
JSON document kept byte for byte, the extractor, the version, and the
2 times; unique on the owner and the name. The deadline of an
extraction is the server's, sent with each request that asks one. Each row of `fields` gains the set that asked it, a
reference that becomes null when the set is deleted, and the version
of the set it was asked with. Every statement is a SQL function that
takes the lock the exchange takes, as the extraction's do, so a change
of a set is wholly before or wholly after any settle, retry, cancel or
delete of its parses.

## API

| Method | Path | Answer |
|---|---|---|
| `POST` | `/sets` | `201` the set; `conflict` for a name the owner has |
| `GET` | `/sets` | `200` a page of sets; `?name=`, `?after=`, `?limit=` |
| `GET` | `/sets/{set}` | `200` the set; `set_not_found` |
| `PUT` | `/sets/{set}` | `200` the set; `conflict` while an extraction of it is queued or running |
| `DELETE` | `/sets/{set}` | `204`; the parses stay |
| `GET` | `/sets/{set}/parses` | `200` a page of the set's parses, each with its extraction; `?after=`, `?limit=` |
| `PUT` | `/sets/{set}/parses/{parse}` | `202` the extraction when it is asked, `200` when nothing is; `parse_not_found`, `conflict` |

The body of `POST /sets` is a request for an extraction: `name`,
`schema`, and optionally `instructions`, `citations` and `extractor`,
each checked as [[011-structured-extraction]] checks it. The body of
`PUT /sets/{set}` is the same, its `name` the set's or left out; it
replaces the whole definition, so a member it leaves out is gone. A
development server answers `501 not_implemented` on every route of a
set, as it does on extraction.

```json
{
  "id": "set_01M4C0QW6Q4V3KJ0Q8ZJ6Y8D2N",
  "name": "harbor-surveys",
  "schema": { "type": "object", "properties": { "title": { "type": "string" } } },
  "instructions": "The title is the report's own, not its running header.",
  "citations": true,
  "version": 2,
  "parses": { "total": 6, "pending": 1, "succeeded": 4, "failed": 1 },
  "created_at": "2026-10-11T09:12:44Z",
  "updated_at": "2026-10-11T09:40:02Z"
}
```

## Non-goals

- **A set on a submit.** A program parses a file into a set with 2
  requests, the submit and the join; the join is accepted while the
  parse is read.
- **Renaming a set**, since its name is the name of every extraction it
  asked.
- **Leaving a set** without deleting the parse.
- **A set across owners**, or a parse in a set of another owner.
- **A deadline per set.** An extraction of a set has the deadline any
  extraction has.

## Acceptance criteria

- A set is made with no parse, read by its id, found by its name, and
  listed newest first under the allow's filter.
- A second set of a name the owner has is `conflict`; another owner may
  have a set of the same name.
- A parse joins a set with no extraction of its name, and its extraction
  is pending with the set's request; it is queued at once when the
  parse has ended and when it ends otherwise.
- A join repeated is answered `200`, and queues no task.
- A join of a parse holding an ended extraction of the name with the
  set's request takes it in as it stands, with no task queued and its
  object kept.
- A join of a parse holding a pending extraction of the name that is not
  the set's is `conflict`.
- A join of a parse of another owner is `parse_not_found`.
- An extraction of a set asked again by name with the set's request is
  asked again and stays in the set; with another request it is
  `conflict`.
- A change of the definition asks every ended extraction of the set
  again, replaces the request of one whose parse is still read, keeps
  each first lease token at the asking's count less 1 shifted into the
  high 32 bits, and adds 1 to the version.
- A change while an extraction of the set is queued or running is
  `conflict`, and changes nothing.
- A deleted set leaves its parses with their extractions, read by name.
- A deleted or expired parse leaves its sets.
- The down migration restores the functions of the migration before it
  byte for byte and drops what this one added.

## Implementation status

Built in the durable server: the table, the functions, the 7 routes,
the `Set` kind and its 5 actions. The development server answers
`501 not_implemented` on every route of a set.
