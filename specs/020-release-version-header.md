---
title: "The release on every answer: a Lectio-Version response header on everything the public listener answers"
status: drafted
track: core
depends_on:
  - specs/003-api.md
  - specs/016-distribution.md
affects: [api/, internal/httpapi/, cmd/lectiod/, specs/003-api.md, specs/016-distribution.md]
effort: small
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# The release on every answer

## Overview

Every answer the API writes on `LECTIO_ADDR` carries one response
header naming the release of the process that wrote it:

```
Lectio-Version: v0.8.0
```

A caller that compares results across releases, such as an evaluation
that runs the same files through two deployments or a regression check
after an upgrade, reads which release produced each answer from the
answer itself.

## Current state

The release is reported in two places, and a caller of the API reaches
neither:

- `GET /version` on `LECTIO_INTERNAL_ADDR` ([[016-distribution]]), the
  listener a deployment keeps off its public address with the other
  probes.
- `lectiod version`, which prints it on the machine the binary runs on.

The release a deployment intends to run, the tag in its manifests, is
not a substitute: during a rolling update two releases answer behind one
address, and a request reaches either. Only the process that answered
knows which it is.

## Design

### The header

| | |
|---|---|
| Name | `Lectio-Version` |
| Value | the version `lectiod version` prints and `/version` answers: a release tag such as `v0.8.0`, stamped by the linker, or `dev` for a build that was not stamped |
| On | every answer the API handler writes: every route of [[003-api]] and its refusals (`400` to `503`), the `404` of an address that is not routed and the `405` of one routed under another method, `GET {base}/openapi.yaml`, and `GET /healthz` |

The name carries the product, so it does not collide with another
service's version header behind a gateway that fronts several, and it
has no `X-` prefix (RFC 6648). `httpapi.HeaderVersion` is the constant
the server sets and a Go caller reads.

The value is the release and nothing else: not the build's commit or
date, which `/version` reports to an operator, and not `info.version` of
the contract, which versions the document. A caller that wants to tell
two releases apart compares the strings; an unstamped build says `dev`
whichever commit it was built from.

### Where it is set

`(*httpapi.Server).Handler` sets it in its outermost handler, before the
request is dispatched, so an answer written by any route, by the error
envelope, or by the handler's own `404` and `405` carries it, and a
route cannot answer without it. `Server.Version` holds the value; empty
takes `version.Version`, the value the linker stamps, which is what both
the development server and the durable server leave it as, so the
header and `/version` cannot disagree.

What the handler never sees is not covered: an answer Go's HTTP server
writes on its own before any handler runs, such as `400` for a request
line it cannot parse or `431` for headers past its limit.

### Exposure

The header tells anyone who can reach the API which release answers.
Lectio's source and its releases are public, so the release is not a
secret, and an operator who does not want it seen removes the header at
the edge in front of the API. There is no setting to turn it off.

## Non-goals

- The commit and the build time on the public listener.
- A header on the internal listener's probes, which answer the version
  in their body already.
- A version of the contract separate from the release.

## Acceptance criteria

- A routed answer, a refusal of each kind the handler writes (`401`,
  `400`, `404` for a missing object, `404` for an address not routed,
  `405`), the contract and the health check each carry
  `Lectio-Version` with the server's version.
- A server whose `Version` is empty answers `version.Version`.
- The durable server, as a process, answers the header on its public
  listener with the version its internal `/version` reports, and the
  development server with `version.Version`.
- The contract's description names the header.

## Implementation status

Not built.
