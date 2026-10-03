---
title: "Distribution: the repository scaffold, the binary and its images, configuration, the stubs, test tiers, and release"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
affects: [cmd/, deploy/, test/, tools/, Makefile, Dockerfile, .github/]
effort: large
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Distribution

## Overview

How Lectio is built, configured, tested and shipped: the layout of the
repository, what the binary and the images contain, the one table of
configuration variables, the stub services tests run against, the
tiers of tests and which claims each proves, and how a release is cut.
This spec is implemented first, so that every later spec lands into a
repository with its gate already running.

## Current state

The repository holds a readme, a license, the agent guide, and the
gate's configuration for the spec tree. The earlier service's gate
configuration, changelog rule and store conformance tests are the
model for what follows. Its deployment manifests, release pipeline for
a hosted service, and browser front end are not carried over: this
repository ships a server anyone runs, and no interface.

## Design

### Layout

```
cmd/lectiod/          the server: roles api and worker
cmd/lectio-stubs/     the stub model endpoint, authorizer and key endpoint
api/openapi.yaml      the contract, generated routes checked against it
authorizer/           public: the action vocabulary and limits
document/             public: the object model
reader/               public: the reader interface
internal/             everything else, one package per spec's `affects`
deploy/base/          manifests for a cluster, with no host or account in them
deploy/examples/      a compose file: Postgres, an object store, lectiod, the stubs
docs/                 running it, the configuration reference, the API guide
test/                 conformance, end-to-end, soak, fixtures
tools/                generators and checks
```

Public packages are the three a consumer or an authorizer imports.
Module path `latere.ai/x/lectio`. Go, current release, no cgo.
Community files: `LICENSE` (Apache-2.0), `SECURITY.md`,
`CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `CHANGELOG.md`.

### The gate

`make check` runs `go tool lateregate`, the same bar every push and
every hook runs: formatting, no cgo, license notices, the spec tree,
dependency admission, sentence registers, the identity and Postgres
declarations, lint, vulnerability check, and the suite under the race
detector with a coverage floor of 90%. The identity block declares
`role: core`, `audience: lectio`, `config_prefix: LECTIO`. The Postgres
block declares `role: pooled` with `LECTIO_DATABASE_URL` for migrations
and `LECTIO_DATABASE_POOL_URL` for serving.

### Images

| Image | Contents |
|---|---|
| `lectiod` | the static binary on a distroless base; reads images and PDFs; refuses formats that need conversion |
| `lectiod-office` | the same binary plus LibreOffice Writer and Impress; converts office formats ([[009-intake]]) |
| `lectio-stubs` | the stubs, for a consumer's own tests |

Two server images because the office suite is several hundred
megabytes and a deployment that never converts should not carry it.
A deployment that converts runs a small pool of `-office` workers
beside its plain ones.

### Configuration

Every variable is `LECTIO_*` and is listed once, here, with its owner.
The table is generated into `docs/configuration.md` and a test fails
when the binary reads a variable the table lacks.

| Variable | Default | Owner |
|---|---|---|
| `LECTIO_ROLE` | `all` | [[001-architecture]] |
| `LECTIO_ADDR` | `:8080` | [[003-api]] |
| `LECTIO_BASE_PATH` | `/v1` | [[003-api]] |
| `LECTIO_DATABASE_URL`, `LECTIO_DATABASE_POOL_URL` | none | [[004-durable-tasks]] |
| `LECTIO_BUCKET`, `LECTIO_BUCKET_PREFIX`, `LECTIO_S3_*` | none | [[002-object-model]] |
| `LECTIO_OIDC_ISSUERS`, `LECTIO_OIDC_AUDIENCE` | none | [[012-identity-and-authorization]] |
| `LECTIO_AUTHORIZER_URL`, `LECTIO_AUTHORIZER_TOKEN` | none: owner policy | [[012-identity-and-authorization]] |
| `LECTIO_ADMIN_SUBJECTS` | none | [[012-identity-and-authorization]] |
| `LECTIO_CONFIG` | none | [[008-readers]] |
| `LECTIO_KEYS`, `LECTIO_KEYS_URL`, `LECTIO_MODEL_KEY` | `static` | [[013-limits-and-usage]] |
| `LECTIO_WORKERS` | 8 task slots per process | [[004-durable-tasks]] |
| `LECTIO_TASK_LEASE`, `LECTIO_TASK_ATTEMPTS`, `LECTIO_TASK_EXPIRIES`, `LECTIO_WORKER_POLL`, `LECTIO_SWEEP_INTERVAL`, `LECTIO_SHUTDOWN_GRACE`, `LECTIO_TASK_RETENTION` | see spec | [[004-durable-tasks]] |
| `LECTIO_CLASS_WEIGHTS`, `LECTIO_GROUP_DEFAULTS` | `interactive=4,batch=1` | [[006-fairness-and-priority]] |
| `LECTIO_POOL_RECOVERY` | 30s | [[007-model-capacity]] |
| `LECTIO_MAX_FILE_BYTES`, `LECTIO_MAX_PAGES`, `LECTIO_CACHE_BYTES` | 256 MiB, 3000, 2 GiB | [[009-intake]] |
| `LECTIO_CHUNK_MAX_CHARS` | 6000 | [[010-assembly]] |
| `LECTIO_SUBMIT_LIMIT`, `LECTIO_MAX_DEADLINE`, `LECTIO_MAX_PARSE_TOKENS` | 120 per minute, 1h, off | [[013-limits-and-usage]] |
| `LECTIO_FETCH_ALLOW`, `LECTIO_FILE_RETENTION`, `LECTIO_PARSE_RETENTION`, `LECTIO_KEEP_PAGE_IMAGES` | none, 24h, 30 days, true | [[014-sources-and-retention]] |
| `LECTIO_USAGE_DETAIL` | 35 days | [[013-limits-and-usage]] |
| `LECTIO_DEV` | false | this spec |

`LECTIO_DEV=true` runs one process with the memory store, a local
directory for objects, the owner policy, a static token and the stub
reader, and prints at start that nothing is durable. Without it,
`lectiod` refuses to start when a required setting is missing and
names it.

### The stubs

`lectio-stubs` serves three things on three ports: a chat completions
endpoint that returns deterministic blocks and extraction replies and
can be told, per request header or by configuration, to delay, fail,
rate-limit, return an invalid reply, or kill its caller; an authorizer
that allows by a small rule file and returns limits; and a key
endpoint. It records every request it served, which is what the
criteria elsewhere mean by "the stub's own count".

### Test tiers

| Tier | Runs | Proves |
|---|---|---|
| unit, hermetic | every push | pure logic: detection, validation, assembly passes, the dispatch decision |
| store conformance | every push, against the memory store and Postgres in a container, directly and through a transaction-mode pooler | every store method, identically on each |
| end-to-end | every push | the API, two processes, the stubs, kills and restarts |
| dispatch simulation | every push | the fairness properties, with a virtual clock over the real store |
| soak | nightly and before a release | exactly-once settlement under random kills |
| live | before a release, opt-in | a real model reads the fixture pages |

A claim about durability or fairness is accepted only from the
end-to-end, simulation or soak tiers.

### Deploying

`deploy/base` holds a Deployment for each role, a Service, and a
ConfigMap for readers and the policy, with every host, bucket and
secret name left to an overlay the operator writes. The API and the
workers scale independently; the workers' count times `LECTIO_WORKERS`
is the fleet's task concurrency, and the pools bound what reaches a
model regardless. Migrations are embedded and run by the API at start
on the direct connection, guarded so that one replica runs them.

### Release

A version tag is cut with `lateregate release vX.Y.Z` from a green
main: it moves the changelog's unreleased notes under the version,
stamps the files that carry it, tags and pushes. The release workflow
builds the binaries for four platforms, the three images with
signatures and attestations, and an archive of `deploy/`. Every
release has a changelog section written for the person who runs the
server.

## Not in this spec

A command line client and client libraries. A Helm chart. Manifests
for any particular deployment, which live with that deployment.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| `make check` passes on a fresh clone with only the toolchain on `PATH` | the hermetic gate |
| `docker compose -f deploy/examples/compose.yaml up` followed by the documented two commands parses the sample file end to end | a smoke test run in CI |
| The configuration table lists every variable the binary reads and none it does not | a generator test |
| `lectiod` without `LECTIO_DEV` and without a database names the missing variable and exits non-zero; with `LECTIO_DEV` it starts and warns | start-up tests |
| The store conformance suite passes on the memory store, on Postgres, and on Postgres through a transaction-mode pooler | the conformance tier |
| The plain image contains no office suite and is under 60 MiB; the `-office` image converts the presentation fixture | image tests |
| A tag without a changelog section is refused by the pre-push hook and by the release workflow | the gate's own check |
| No file in the repository contains a deployment's hostname, account or credential | a repository scan in the gate |
