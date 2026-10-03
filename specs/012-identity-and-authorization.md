---
title: "Identity and authorization: verifying a caller, the action vocabulary, the question to the authorizer, limits on an allow, the owner policy"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-api.md
affects: [authorizer/, internal/auth/, internal/httpapi/]
effort: medium
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Identity and authorization

## Overview

Lectio answers two questions about every request and decides neither
alone. Who is calling is settled by verifying a token against a
configured OpenID Connect issuer. What the caller may do is asked of an
authorizer, an HTTP endpoint the operator runs, which also returns the
limits the request is held to. With no authorizer configured, a
built-in policy lets a caller act on what it owns. Lectio holds no
accounts, no roles and no plans.

## Current state

The earlier service verified tokens with the shared verifier and
authorized by tenant equality alone, reading an organization claim to
find the tenant. It had no authorizer, no action vocabulary and no
per-caller limits. The verifier is carried over. Reading a claim for
meaning is not: the tenant now comes from the authorizer's answer.

## Design

### Verification

`latere.ai/x/pkg/authkit/jwt` verifies every bearer: signature against
the issuer's published keys, `iss` among `LECTIO_OIDC_ISSUERS`, `aud`
among `LECTIO_OIDC_AUDIENCE` (a comma list, the first entry primary),
`exp` and `nbf` with skew. The subject is rendered once as
`<iss>|<sub>` and is what `owner` fields, events and the authorizer
request carry.

### The vocabulary

Published as Go constants in the public package `authorizer`, with
`Vocabulary()` for an authorizer to validate against.

| Kind | Actions | Resource fields sent |
|---|---|---|
| `Parse` | `parse.create`, `parse.read`, `parse.list`, `parse.cancel`, `parse.delete` | `id`, `owner`, `class`, `priority`, `reader`, `labels`, `origin`, `pages` (the selection's size, when known) |
| `File` | `file.create`, `file.read`, `file.delete` | `id`, `owner`, `size`, `media_type` |
| `Reader` | `reader.list` | none |
| `Usage` | `usage.read` | `owner`, `group` |
| `Queue` | `queue.read` | `group` |

A request may name an `owner` other than the caller's subject (a
service submitting for a user, a member acting in an organization's
space); the authorizer decides whether the caller may.

### The question

The shared contract, `latere.ai/x/pkg/authz`: Lectio posts the subject,
every verified claim verbatim, the action and the resource, with a
bearer of its own, and receives `{allow, reason, ttl, limits, filter}`.
The client's rules are the contract's: an allow is cached for its
`ttl`, a deny briefly, unavailability never; one retry on a connection
failure; anything but a well-formed allow is treated as unavailable,
and the request fails closed with `503 authorizer_unavailable`.
`filter` narrows a list to owners and labels.

### Limits

The limits on an allow of `parse.create` are how an operator's plane
tells Lectio what this caller's work is held to. Lectio enforces what
it is told and stores no plan.

```go
package authorizer

type Limits struct {
    Group         string        // the fairness group the parse joins; empty is the owner
    Weight        int           // the group's share, 1..1000; 0 keeps the default
    Project       string        // the project of the group the parse joins; empty is the group's own
    ProjectWeight int           // the project's share of its group, 1..1000; 0 keeps the default
    MaxRunning    int           // leased tasks at once for the group
    MaxQueued     int           // non-terminal parses for the group
    MaxPriority   int           // bound on |priority|
    Classes       []string      // classes the caller may use; empty is both
    Readers       []string      // readers the caller may pin; empty is all
    MaxFileBytes  int64         // lower than the server's, never higher
    MaxPages      int           // per parse
    PagesPerDay   int           // for the group, rolling 24 hours
    Retention     time.Duration // how long results are kept; lower than the server's
}
```

`WireLimits` is the JSON form with every member optional, and
`DecodeLimits(authz.Decision)` reads it. A member the answer does not
name leaves the configured default in force; zero means no ceiling
where the comment says so. A limit Lectio is handed and cannot enforce
is a refusal with `capability_unsupported`, never a silent pass.

`Group` is the important one. It is how two members of one
organization share one queue and one budget, and how a person's own
work is kept apart from their organization's. Lectio does not derive
it from a claim.

`Project` divides a group's own work, and `ProjectWeight` is that
project's share of what the group is served
([[006-fairness-and-priority]]). Lectio does not know who chose
`Weight` or `ProjectWeight`. What it guarantees is that
`ProjectWeight` moves service between the projects of one group and
never between groups, so an operator's plane can hand that number to a
tenant's own administrators and keep `Weight` to itself.

### The owner policy

With no `LECTIO_AUTHORIZER_URL`, `lectiod` logs `owner policy` at start
and decides itself: a subject may perform every action on a resource
whose `owner` is that subject, lists are filtered to its own, the group
is the owner, and limits are the configured defaults. Subjects in
`LECTIO_ADMIN_SUBJECTS` may read any owner's resources and the queue.

### Service callers

A service that submits parses for many users authenticates as itself
and names each user as `owner`; the authorizer decides whether it may.
Lectio has no exchange endpoint and mints no token for a caller.

### What a worker carries

A worker acts on tasks, not for a caller. It holds no user token. The
model credential for a page is resolved per group
([[013-limits-and-usage]]); the source was snapshotted at submit
([[014-sources-and-retention]]), so nothing a worker does later needs
the caller's authority.

## Not in this spec

Browser sessions and cookies; Lectio has no user interface. An
administrative API for groups and pools. Per-block or per-page access
control: access is to a parse.

## Implementation status

Built: a stand-in, and none of the design above.

- `httpapi.Tokens`, a fixed table from bearer token to owner. The
  development server holds one entry, the token in `LECTIO_DEV_TOKEN`
  (default `dev`) for the owner `dev`. A request with no bearer is
  `401 missing_token`, and one with a token the table lacks is `401
  invalid_token`.
- Owner scoping in the store. Every read and write of a file or a
  parse names the owner, and another owner's object is not found, so a
  caller reads, lists, cancels and deletes its own and no one else's.
  That is the owner policy's rule for one subject, without admin
  subjects.

Remaining: the verifier, the `authorizer` package with its vocabulary
and limits, the authorizer client, the action asked by each route, the
admin subjects, and service callers naming an owner. The action a
field request asks ([[011-structured-extraction]]) is not in the
vocabulary yet, since that route did not exist when the table above
was written.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| The verifier passes the shared conformance suite | `authkit/conformance` |
| Every route asks exactly the action in the table of [[003-api]], with the resource fields above | a test that records the authorizer's requests for each route |
| The authorizer client passes the contract's conformance suite: cache, retry, fail closed | `authz/conformance` |
| Each member of `Limits` has a test in which an allow carrying it changes the outcome, and the absent member leaves the default | a table test |
| Two subjects whose allows name one `Group` share `MaxQueued` and are served as one group; two with different groups are served by weight | a dispatch test |
| Two subjects whose allows name one `Group` and two `Project`s share `MaxQueued`, are served in the ratio of their `ProjectWeight`s within the group, and change no other group's dispatch count | a dispatch test |
| Under the owner policy, a subject cannot read, list, cancel or delete another subject's parse, and an admin subject can read it | API tests |
| A probe id is denied for every subject and action by the stub authorizer and by the owner policy | the vocabulary's `Probe()` test |
