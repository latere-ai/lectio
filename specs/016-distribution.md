---
title: "Distribution: the repository scaffold, the binary and its images, configuration, the stubs, test tiers, and release"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
affects: [cmd/, internal/config/, deploy/, test/, tools/, Makefile, Dockerfile, .github/, .githooks/]
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
cmd/lectiod/              the server: roles api and worker
cmd/lectio-stubs/         the stub model endpoint, authorizer and key endpoint (not built)
api/                      public: openapi.yaml, the contract, embedded for the server to serve
authorizer/               public: the action vocabulary and limits (not built)
document/                 public: the object model
reader/                   public: the reader and extractor interfaces
reader/chat/              the adapter for OpenAI-compatible chat completions
reader/layout/            the adapter for a layout engine behind its own HTTP contract
reader/stub/              a deterministic reader and extractor that make no call
internal/assemble/        the document-wide passes and the views
internal/config/          settings from the environment; Reader and Policy documents
internal/fault/           the error codes
internal/fetch/           a source URL fetched with the address check
internal/httpapi/         the routes, held to the contract by its tests
internal/id/              prefixed, time-ordered identifiers
internal/intake/          detect, unwrap, pages, tiffx
internal/native/          formats read with no model
internal/parse/           the steps of a parse: Prepare and ReadPage
internal/prompts/         every instruction sent to a model, as template files
internal/render/          the page renderer
internal/run/             the in-process runner
internal/store/           the memory store
internal/testfixtures/    files the tests read
internal/version/         the build's version, stamped by the linker
deploy/base/              manifests for a cluster, with no host or account in them (not built)
deploy/examples/          a compose file: Postgres, an object store, lectiod, the stubs (not built)
docs/                     running it, the configuration reference, the API guide (not built)
test/                     conformance, end-to-end, soak, fixtures (not built)
tools/                    generators and checks (not built)
```

Public packages are the four a consumer, an adapter or an authorizer
imports: `api`, `document`, `reader` with its adapters, and
`authorizer`. A package joins `internal/` with the spec that designs
it: the task store, the dispatcher, the pools, the verifier, the
object store and telemetry each arrive that way.
Module path `latere.ai/x/lectio`. Go, current release, no cgo.
Community files: `LICENSE` (Apache-2.0), `SECURITY.md`,
`CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `CHANGELOG.md`.

### The gate

`make check` runs `go tool lateregate`, the same bar every push and
every hook runs: formatting, no cgo, license notices, the spec tree,
dependency admission, sentence registers, the identity and Postgres
declarations, lint, vulnerability check, and the suite under the race
detector with a coverage floor of 90% per package. The identity block
declares `role: core`, `audience: lectio`, `config_prefix: LECTIO`. The
Postgres block declares `role: pooled` with `LECTIO_DATABASE_URL` for
migrations and `LECTIO_DATABASE_POOL_URL` for serving. Until the
verifier and the database client exist, both blocks declare `role:
none`, since there is nothing yet for them to describe.

`make build` builds `out/lectiod` with the version, the commit and the
date stamped in. `make run` builds and starts the development server.
`make openapi` runs the tests that hold the router to the contract.
`make hooks` points git at `.githooks`, whose two hooks call the same
gate.

### Images

| Image | Contents |
|---|---|
| `lectiod` | the static binary on a distroless base; reads images and PDFs; refuses formats that need conversion unless a converter is configured |
| `lectio-convert` | the conversion sidecar: an office suite behind the one call of the converter interface ([[009-intake]]); holds no credential and is run with no network |
| `lectio-stubs` | the stubs, for a consumer's own tests |

There is one server image. The office suite is several hundred
megabytes and runs a caller's file, so it is not in the image that
holds the model credential: a deployment that converts runs the
sidecar beside its workers, in a container that is given no network,
and a deployment that does not convert runs nothing extra. The task
queue does not know which workers can convert, because every worker
either reaches a converter or refuses the format.

### Configuration

Every variable is `LECTIO_*` and is listed once, here, with its owner.
The table is generated into `docs/configuration.md` and a test fails
when the binary reads a variable the table lacks. The last column says
whether the binary reads the variable today.

| Variable | Default | Owner | Read today |
|---|---|---|---|
| `LECTIO_ROLE` | `all` | [[001-architecture]] | no |
| `LECTIO_ADDR` | `:8080` | [[003-api]] | yes |
| `LECTIO_BASE_PATH` | `/v1` | [[003-api]] | yes |
| `LECTIO_DATABASE_URL` | none | [[004-durable-tasks]] | yes, only to say what is missing |
| `LECTIO_DATABASE_POOL_URL` | none | [[004-durable-tasks]] | no |
| `LECTIO_BUCKET`, `LECTIO_BUCKET_PREFIX`, `LECTIO_S3_*` | none | [[002-object-model]] | no |
| `LECTIO_OIDC_ISSUERS`, `LECTIO_OIDC_AUDIENCE` | none | [[012-identity-and-authorization]] | no |
| `LECTIO_AUTHORIZER_URL`, `LECTIO_AUTHORIZER_TOKEN` | none: owner policy | [[012-identity-and-authorization]] | no |
| `LECTIO_ADMIN_SUBJECTS` | none | [[012-identity-and-authorization]] | no |
| `LECTIO_CONFIG` | none: the stub reader | [[008-readers]] | yes |
| `LECTIO_MODEL_KEY` | none | [[013-limits-and-usage]] | yes |
| `LECTIO_KEYS`, `LECTIO_KEYS_URL` | `static` | [[013-limits-and-usage]] | no |
| `LECTIO_WORKERS` | 8 task slots per process | [[004-durable-tasks]] | yes: pages read at once |
| `LECTIO_TASK_ATTEMPTS` | 5 | [[004-durable-tasks]] | yes: attempts per page |
| `LECTIO_SHUTDOWN_GRACE` | 25s | [[004-durable-tasks]] | yes: how long open requests get to finish |
| `LECTIO_TASK_LEASE`, `LECTIO_TASK_EXPIRIES`, `LECTIO_WORKER_POLL`, `LECTIO_SWEEP_INTERVAL`, `LECTIO_TASK_RETENTION` | see spec | [[004-durable-tasks]] | no |
| `LECTIO_CLASS_WEIGHTS`, `LECTIO_GROUP_DEFAULTS` | `interactive=4,batch=1` | [[006-fairness-and-priority]] | no |
| `LECTIO_POOL_RECOVERY` | 30s | [[007-model-capacity]] | no |
| `LECTIO_MAX_FILE_BYTES`, `LECTIO_MAX_PAGES` | 256 MiB, 3000 | [[009-intake]] | yes |
| `LECTIO_CACHE_BYTES` | 2 GiB | [[009-intake]] | no |
| `LECTIO_CHUNK_MAX_CHARS` | 6000 | [[010-assembly]] | no |
| `LECTIO_MAX_DEADLINE` | 1h | [[013-limits-and-usage]] | yes |
| `LECTIO_SUBMIT_LIMIT`, `LECTIO_MAX_PARSE_TOKENS` | 120 per minute, off | [[013-limits-and-usage]] | no |
| `LECTIO_FETCH_ALLOW` | none | [[014-sources-and-retention]] | yes |
| `LECTIO_FILE_RETENTION`, `LECTIO_PARSE_RETENTION`, `LECTIO_KEEP_PAGE_IMAGES` | 24h, 30 days, true | [[014-sources-and-retention]] | no |
| `LECTIO_USAGE_DETAIL` | 35 days | [[013-limits-and-usage]] | no |
| `LECTIO_DEV` | false | this spec | yes |
| `LECTIO_DEV_TOKEN` | `dev` | this spec | yes |

A value that does not parse is an error that names its variable and
never its value: a variable may hold a secret by mistake.
`LECTIO_FETCH_ALLOW` is a comma-separated list of hosts.

`LECTIO_DEV=true` runs one process with the memory store, the owner
scoping of that store, one static token (`LECTIO_DEV_TOKEN`) and the
stub reader unless `LECTIO_CONFIG` names real ones, and logs at start
that nothing is durable. Without it, `lectiod` refuses to start when a
required setting is missing and names it. `lectiod version` prints the
build's version. The command takes no other argument: it is configured
by its environment.

On `SIGINT` or `SIGTERM` the server stops accepting connections, gives
open requests `LECTIO_SHUTDOWN_GRACE` to finish, and then stops the
runner.

### The stubs

`lectio-stubs` serves three things on three ports: a chat completions
endpoint that returns deterministic blocks and extraction replies and
can be told, per request header or by configuration, to delay, fail,
rate-limit, return an invalid reply, or kill its caller; an authorizer
that allows by a small rule file and returns limits; and a key
endpoint. It records every request it served, which is what the
criteria elsewhere mean by "the stub's own count". Until it is built,
`reader/stub` stands in for the model endpoint inside the test's own
process, which is enough for everything but a test that kills the
server.

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

## Implementation status

Built:

- The module, the gate and its configuration, both hooks, the verify
  workflow, the community files, and the layout above except the
  entries marked not built.
- `cmd/lectiod`: the development server. `LECTIO_DEV=true` is the only
  mode that starts. Without it the command names
  `LECTIO_DATABASE_URL` when that is missing, says the durable server
  is not built when it is set, and exits non-zero either way.
- `internal/config`: the settings marked as read in the table, and the
  Reader and Policy documents ([[008-readers]]).
- A graceful stop: an upload that is still sending its body when the
  signal arrives is answered.

Remaining:

- `lectio-stubs`, the images, `deploy/`, `docs/`, the generated
  configuration reference and its test, and the release workflow.
- Every test tier but the first. The suite today is unit tests and
  end-to-end tests of the API and the development server in one
  process; there is no store conformance suite, no dispatch
  simulation and no soak. The live tier is one test, `make live`,
  which reads a real file with a configured reader and is run by hand.
- `LECTIO_DEV` holds page images in memory and has no local directory
  for objects.
- The health probe is `GET /healthz`. Readiness and version probes
  come with the durable server ([[015-observability]]).

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| `make check` passes on a fresh clone with only the toolchain on `PATH` | the hermetic gate |
| `docker compose -f deploy/examples/compose.yaml up` followed by the documented two commands parses the sample file end to end | a smoke test run in CI |
| The configuration table lists every variable the binary reads and none it does not | a generator test |
| `lectiod` without `LECTIO_DEV` and without a database names the missing variable and exits non-zero; with `LECTIO_DEV` it starts and warns | start-up tests |
| The store conformance suite passes on the memory store, on Postgres, and on Postgres through a transaction-mode pooler | the conformance tier |
| The server image contains no office suite and is under 60 MiB; the conversion sidecar converts the presentation fixture with its network disabled | image tests |
| A tag without a changelog section is refused by the pre-push hook and by the release workflow | the gate's own check |
| No file in the repository contains a deployment's hostname, account or credential | a repository scan in the gate |
