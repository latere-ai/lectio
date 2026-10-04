---
title: "Identity and authorization: verifying a caller, the action vocabulary, the question to the authorizer, limits on an allow, the owner policy"
status: in-progress
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-api.md
affects: [authorizer/, internal/access/, internal/httpapi/]
effort: medium
created: 2026-10-03
updated: 2026-10-04
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
among `LECTIO_OIDC_AUDIENCE` (a comma list, the first entry primary,
`lectio` when the variable is unset), `exp` and `nbf` with 30 seconds
of skew. A token that carries `iat` is also held to the shared
verifier's age bound of 24 hours. The subject is rendered once as
`<iss>|<sub>` and is what `owner` fields, events and the authorizer
request carry. Two issuers that agree on a `sub` are two subjects.

A request with no bearer is `401 missing_token`. A bearer that is
refused is `401 invalid_token`, and its `details.reason` carries the
shared verifier's word for why: `audience`, `expired`, `issuer`,
`signature` and the others of its table. No part of a token is ever
in an error or a log line.

The verifier reads an issuer's discovery document and its key set and
calls the issuer for nothing else. It reads them when the first token
of that issuer arrives, or at start when the server warms it, and an
issuer that does not answer then is named.

A token may carry grants: a personal access token is narrowed by its
holder to some actions on some resources, in the claim
`authorization_details`. The verifier admits such a token and hands
the grants on with every other claim. They are applied where the
decision is made: an authorizer built on
`latere.ai/x/pkg/authz/server` narrows its answer by them, and so does
the owner policy below.

### The vocabulary

Published as Go constants in the public package `authorizer`, with
`Vocabulary()` for an authorizer to validate against and `Fields` for
the members each action sends.

| Kind | Action | Resource fields sent |
|---|---|---|
| `Parse` | `parse.create` | `id`, `owner`, `class`, `priority`, `reader`, `labels`, `origin`, `pages` |
| `Parse` | `parse.read` | `id`, `owner`, `class`, `priority`, `reader`, `labels`, `origin`, `pages` |
| `Parse` | `parse.list` | none |
| `Parse` | `parse.cancel` | `id`, `owner`, `class`, `priority`, `reader`, `labels`, `origin`, `pages` |
| `Parse` | `parse.delete` | `id`, `owner`, `class`, `priority`, `reader`, `labels`, `origin`, `pages` |
| `File` | `file.create` | `owner`, `size`, `media_type` |
| `File` | `file.read` | `id`, `owner`, `size`, `media_type` |
| `File` | `file.delete` | `id`, `owner`, `size`, `media_type` |
| `Reader` | `reader.list` | none |
| `Usage` | `usage.read` | `owner`, `group` |
| `Queue` | `queue.read` | `group` |

A member Lectio does not know for a request is left out, never sent
empty. `pages` is the size of the parse's selection and is sent once it
is known: at a submit only for a selection with no open range, and for
a stored parse once its pages were counted. `reader` is the reader a
parse pins. `size` is sent by an upload only when the request declares
one.

`parse.create` is asked by a submit, which sends no `id`, and by every
request that queues model work on a stored parse: a retry, a figure
run, an extraction ([[003-api]]). Those send the stored parse's `id`
and its fields, and the work stays the parse's owner's: the limits of
such an allow hold, and its `owner` is not read.

On a create, `owner` is the owner the request names. A request may
name an owner other than the caller's subject (a service submitting
for a user, a member acting in an organization's space); the
authorizer decides whether the caller may. A request that names none
leaves `owner` out, which means: in the caller's context. Either way
the allow says whose the new object is, in its limits, so Lectio reads
no claim to learn it.

### The question

The shared contract, `latere.ai/x/pkg/authz`: Lectio posts the subject,
every verified claim verbatim, the action and the resource, with a
bearer of its own, and receives `{allow, reason, ttl, limits, filter}`.
The request also carries what is known of the HTTP request: its id, the
peer address and the user agent.

The client's rules are the contract's: an allow is cached for its
`ttl`, a deny briefly, unavailability never; one retry on a connection
failure. An answer about a resource with no id is never cached, so
every submit and every upload is asked, and limits changed at the
authorizer hold from the caller's next create.

A well-formed deny is `403 forbidden`, with the authorizer's `reason`
as the developer detail of the error and never in the sentence a
person reads. Anything that is not a well-formed answer is treated as
unavailable: another status than `200`, a body that does not parse or
carries no `allow`, no answer within the deadline, and limits that
cannot be read. The request then fails closed with `503
authorizer_unavailable`, and the error names neither the endpoint nor
its bearer. `filter` narrows a list to owners and labels.

A read of a stored object answers a deny the way it answers a missing
object, with the `404` of its kind, so an id cannot be probed for what
somebody else owns.

### Limits

The limits on an allow are how an operator's plane tells Lectio what
this caller's work is held to. Lectio enforces what it is told and
stores no plan.

```go
package authorizer

type Limits struct {
    Owner         string        // the owner a created parse or file is recorded under; empty is the caller's subject
    Group         string        // the fairness group the parse joins; empty is the owner
    Weight        int           // the group's share, 1..1000; 0 keeps the default
    Project       string        // the project of the group the parse joins; empty is the group's own
    ProjectWeight int           // the project's share of its group, 1..1000; 0 keeps the default
    MaxRunning    int           // leased tasks at once for the group; 0 is no cap
    MaxQueued     int           // non-terminal parses for the group; 0 is no cap
    MaxPriority   int           // bound on |priority|; 0 admits priority 0 alone
    Classes       []string      // classes the caller may use; empty is both
    Readers       []string      // readers the caller may pin; empty is all
    MaxFileBytes  int64         // lower than the server's, never higher
    MaxPages      int           // per parse; lower than the server's, never higher
    PagesPerDay   int           // for the group, rolling 24 hours; 0 is no budget
    Retention     time.Duration // how long results are kept; lower than the server's
}
```

`WireLimits` is the JSON form, with every member optional and exactly
these keys:

| Key | Member | Type |
|---|---|---|
| `owner` | `Owner` | string |
| `group` | `Group` | string |
| `weight` | `Weight` | integer |
| `project` | `Project` | string |
| `project_weight` | `ProjectWeight` | integer |
| `max_running` | `MaxRunning` | integer |
| `max_queued` | `MaxQueued` | integer |
| `max_priority` | `MaxPriority` | integer |
| `classes` | `Classes` | list of strings |
| `readers` | `Readers` | list of strings |
| `max_file_bytes` | `MaxFileBytes` | integer |
| `max_pages` | `MaxPages` | integer |
| `pages_per_day` | `PagesPerDay` | integer |
| `retention_seconds` | `Retention` | integer, in seconds |

The allow of `parse.create` is read for any of them. The allow of
`file.create` is read for `owner`, `group`, `max_file_bytes` and
`retention_seconds`. The limits of any other allow are not read.

`DecodeLimits(authz.Decision)` reads the object into a `WireLimits`,
and `WireLimits.Over(defaults)` lays what it names over the server's
configured defaults and returns the `Limits` in force. A member the
answer does not name leaves the configured default in force. Of the
members it names:

- `owner`, `group`, `project`, `classes` and `readers` replace the
  default when they are not empty;
- `weight` and `project_weight` replace it when above zero;
- `max_running`, `max_queued`, `max_priority` and `pages_per_day`
  replace it whatever they are, so an answer of zero lifts a default
  cap, and bounds the priority at zero;
- `max_file_bytes`, `max_pages` and `retention_seconds` replace it only
  when above zero and below the server's: an allow lowers those and
  never raises them.

An object that does not parse, a figure below zero and a weight above
1000 make the answer no decision, and the request fails closed with
`503 authorizer_unavailable`: a ceiling Lectio cannot read is not one
it can hold. A limit Lectio is handed and cannot enforce is a refusal
with `422 capability_unsupported`, never a silent pass; a member this
version does not know is such a limit.

`Owner` is what makes a create safe to ask for somebody else. The
request says whose the new parse or file should be, or says nothing;
the allow answers whose it is. With no `owner` on the allow, it is the
owner the request named, and the caller's subject when it named none.

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

The rows, in the order they are tried:

| Question | Answer |
|---|---|
| the probe id, as any subject | deny, `probe` |
| no subject | deny, `anonymous` |
| an action outside the vocabulary, or asked about another kind | deny, `unknown_action` |
| `reader.list` | allow: the readers are the server's configuration and nobody's |
| `parse.read` or `file.read`, as an admin | allow |
| `parse.list` | allow, with a filter that narrows the list to the subject's own; no filter for an admin |
| `usage.read` or `queue.read` that names no owner and no group | as `parse.list` |
| `usage.read` or `queue.read` that names one | allow for an admin, and when every name is the subject; otherwise deny, `not_owner` |
| a stored parse or file whose owner is the subject, any action | allow |
| a create that names no owner | allow |
| anything else | deny, `not_owner` |

An admin reads and does not change: it cannot cancel, delete or retry
another owner's parse, and cannot create in another owner's name. A
create that names another owner is denied for every subject, so a
service that submits for its users needs an authorizer.

The policy names no limit: every request is held to the configured
defaults, and a create is recorded under the caller's subject. An allow
is narrowed by the grants of the caller's token, as an authorizer's is,
and a token whose grants cannot be read is denied.

### Service callers

A service that submits parses for many users authenticates as itself
and names each user as `owner`; the authorizer decides whether it may.
Lectio has no exchange endpoint and mints no token for a caller.

### What a handler depends on

Two interfaces, and it cannot tell what is behind either. One turns a
request's bearer into a caller: the subject, its issuer and `sub`
apart, and every verified claim. The other answers a question, an
action and a resource, with a decision: allow or deny, the reason, the
limits in force, and the filter of a list. Every route is written the
same way: authenticate, build the resource from the request and the
stored object, ask, and act within the decision.

Which is behind each is selected by the settings
([[016-distribution]]). The issuers select the verifier; a development
server that lists none takes one static token, `LECTIO_DEV_TOKEN`, for
the subject `dev`, which names no issuer and so can be no verified
subject. The authorizer's URL selects the authorizer, and its absence
the owner policy.

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

Built, as libraries the server does not call yet:

- `authorizer`: the vocabulary and the fields each action sends, held
  equal to the table above by a test, and `Limits`, `WireLimits`,
  `DecodeLimits` and `Over`.
- `internal/access`: the two interfaces a handler depends on, one that
  turns a request's bearer into a caller and one that answers a
  question with a decision; the resources a question is about; the
  verifier; the authorizer client; the owner policy with its admin
  subjects; the development identity, one static token for the
  subject `dev`, behind the same interface as the verifier; and the
  table of routes, each with the action it asks and the fields it
  sends, held equal to the contract and to the table of [[003-api]].
  `access.New` selects what is behind the two interfaces from the
  settings.
- The identity declaration of the gate: `role: core`
  ([[016-distribution]]).

A stand-in, in the server:

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

Remaining: the handlers asking. No route asks its action yet, a submit
and an upload take no `owner`, and no limit of an allow is enforced.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| The verifier passes the shared conformance suite | `TestVerifierConformance`, which runs `authkit/conformance` |
| A verified token becomes a caller whose subject is `<iss>\|<sub>` and whose claims are the token's, verbatim; two issuers that agree on a `sub` are two subjects | `TestAVerifiedTokenBecomesACaller`, `TestTwoIssuersAreTwoSubjects` |
| A token for another audience, of an issuer that is not listed, expired or not yet valid past the skew, or with a signature that does not check out is `invalid_token` with the reason, and the error never repeats the token | `TestWhatTheVerifierRefuses`, `TestTheSkewOnExpAndNbf` |
| The table of routes kept as data has one row for every route of the contract, in the contract's order, each asking an action of the vocabulary with fields the vocabulary publishes for it, and equals the table of [[003-api]] | `TestEveryRouteOfTheContractHasARow`, `TestEveryRowAsksAnActionOfTheVocabulary`, `TestTheRowsAreTheSpecs` |
| Every route asks exactly the action in the table of [[003-api]], with the resource fields above | a test that records the authorizer's requests for each route |
| The endpoint `lectiod` asks answers the contract for every row of the vocabulary | `TestAuthorizerConformance`, which runs `authz/conformance` against the stub authorizer |
| The client holds an allow for its `ttl`, a deny briefly and an outage never, asks a create every time, retries once on a connection failure, and fails closed with `authorizer_unavailable` on anything that is not a well-formed answer | `TestWhatTheClientRemembers`, `TestACreateIsAlwaysAsked`, `TestOneRetryOnAConnectionFailure`, `TestTheClientFailsClosed` |
| The authorizer receives the subject, every claim verbatim, the action and the resource; a deny is a decision with its reason, limits that cannot be read are no decision, and a limit this version does not know is `capability_unsupported` | `TestTheEnvelopeCarriesTheCallerAndTheQuestion`, `TestOneQuestionEndToEnd`, `TestADenyIsADecision`, `TestLimitsTheServerCannotHold` |
| A create is recorded under the owner its allow names, else the owner the request named, else the caller's subject, and its group is that owner unless the allow names one; an allow about a stored parse does not move it to another owner | `TestWhoseACreateIs`, `TestAStoredParseKeepsItsOwner` |
| The constants of `authorizer` are the vocabulary table above: the same actions in the same order, each on its kind with its fields | `TestTheVocabularyIsTheSpecs` |
| The limits object carries exactly the 14 keys of the table above, and `DecodeLimits` reads back what `WireLimits` renders | `TestTheWireNamesEveryMemberAndNoOther`, `TestDecodeReadsWhatAnAuthorizerRenders` |
| Each member of `Limits`: an allow carrying it changes the limits in force, and the absent member leaves the default | `TestEachMemberOverTheDefaults`, and through the question a submit asks, `TestEachLimitOfASubmit` |
| The allow of `file.create` is read for 4 members, and the limits of any other action's allow are not read | `TestAnUploadIsHeldToFourMembers`, `TestTheLimitsOfAReadAreNotRead` |
| A limits object with a figure out of range is read as no decision, and one that names a member this version does not know is told apart from it | `TestDecodeRefusesWhatItCannotHold`, `TestDecodeNamesAMemberItDoesNotKnow` |
| Each member of `Limits` has a test in which an allow carrying it changes the outcome of a request | a table test over the API |
| Two subjects whose allows name one `Group` share `MaxQueued` and are served as one group; two with different groups are served by weight | a dispatch test |
| Two subjects whose allows name one `Group` and two `Project`s share `MaxQueued`, are served in the ratio of their `ProjectWeight`s within the group, and change no other group's dispatch count | a dispatch test |
| Under the owner policy, a subject cannot read, list, cancel or delete another subject's parse, and an admin subject can read it | API tests |
| The rows of the owner policy: a subject and its own, another's, a create, the reads that range over owners, an admin that reads and does not change, and no subject | `TestTheOwnerPolicy`, `TestASubjectAndAnotherSubjectsParse` |
| The owner policy answers the contract's conformance suite, and narrows an allow by the grants of a token | `TestOwnerPolicyConformance`, `TestOwnerPolicyNarrowsByTheGrants`, `TestTheGrantsOfATokenNarrowTheOwnersReach` |
| The settings select who is calling and who decides, apart; a server that is not a development one and lists no issuer is refused, naming the variable | `TestTheSettingsSelectTheMode`, `TestAServerWithNoIssuerIsRefused`, and the 3 tests named `EndToEnd` in `internal/access` |
| A probe id is denied for every subject and action by the stub authorizer and by the owner policy | `TestTheProbeIsDeniedForEverySubjectAndAction` |
| The development token stands for one subject and is never printed; under the owner policy its caller owns what it creates and nothing else | `TestTheDevelopmentTokenStandsForOneSubject`, `TestWhatTheDevelopmentTokenRefuses`, `TestTheDevelopmentCallerUnderTheOwnerPolicy` |
