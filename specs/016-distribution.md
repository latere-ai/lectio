---
title: "Distribution: the repository scaffold, the binary and its images, configuration, the stubs, test tiers, and release"
status: validated
track: core
depends_on:
  - specs/001-architecture.md
affects: [cmd/, internal/config/, deploy/, test/, tools/, Makefile, Dockerfile, .github/, .githooks/]
effort: large
created: 2026-10-03
updated: 2026-10-04
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
cmd/lectio-convert/       the conversion sidecar: an office suite behind one call
cmd/lectio-stubs/         the stub model endpoint, authorizer and key endpoint (not built)
api/                      public: openapi.yaml, the contract, embedded for the server to serve
authorizer/               public: the action vocabulary and limits
document/                 public: the object model
reader/                   public: the reader and extractor interfaces
reader/chat/              the adapter for OpenAI-compatible chat completions
reader/layout/            the adapter for a layout engine behind its own HTTP contract
reader/stub/              a deterministic reader and extractor that make no call
internal/access/          who is calling and who decides: the verifier, the authorizer client, the owner policy
internal/assemble/        the document-wide passes and the views
internal/config/          settings from the environment; Reader and Policy documents
internal/convert/         conversion: the client a pipeline converts through, and the sidecar's service
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
internal/store/postgres/  the durable task store, and its migrations
internal/tasks/           the protocol a worker and a task store share
internal/testfixtures/    files the tests read
internal/version/         the build's version, stamped by the linker
deploy/base/              manifests for a cluster, with no host or account in them
deploy/bootstrap/         the namespace and the template of the Secrets, applied by hand
deploy/components/        the conversion sidecar for a cluster, as a part an overlay adds
deploy/converter/         the image of the conversion sidecar
deploy/examples/          2 example overlays; a compose file: Postgres, an object store, lectiod, the sidecar
docs/                     running it; the configuration reference and the API guide (not built)
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
declares `role: core`, `audience: lectio`, `config_prefix: LECTIO`,
`api_group: lectio.latere.ai`, the group the Reader and Policy documents
are written under, which a core must name, and `reached_by:
self-hosted`, since the audience is the default of a server somebody
runs themselves. The Postgres block declares `role: pooled` with
`LECTIO_DATABASE_URL` for migrations and `LECTIO_DATABASE_POOL_URL` for
serving.

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
| `LECTIO_INTERNAL_ADDR` | `:8081` | [[015-observability]] | no |
| `LECTIO_DATABASE_URL` | none | [[004-durable-tasks]] | yes, only to say what is missing |
| `LECTIO_DATABASE_POOL_URL` | none: the serving path opens `LECTIO_DATABASE_URL` | [[004-durable-tasks]] | yes: read, and opened by no command yet |
| `LECTIO_BUCKET`, `LECTIO_BUCKET_PREFIX`, `LECTIO_S3_*` | none | [[002-object-model]] | no |
| `LECTIO_OIDC_ISSUERS` | none | [[012-identity-and-authorization]] | yes: read and checked, and used by no command yet |
| `LECTIO_OIDC_AUDIENCE` | `lectio` | [[012-identity-and-authorization]] | yes: read and checked, and used by no command yet |
| `LECTIO_AUTHORIZER_URL`, `LECTIO_AUTHORIZER_TOKEN` | none: owner policy | [[012-identity-and-authorization]] | yes: read and checked, and used by no command yet |
| `LECTIO_ADMIN_SUBJECTS` | none | [[012-identity-and-authorization]] | yes: read and checked, and used by no command yet |
| `LECTIO_CONFIG` | none: the stub reader | [[008-readers]] | yes |
| `LECTIO_MODEL_KEY` | none | [[013-limits-and-usage]] | yes |
| `LECTIO_KEYS`, `LECTIO_KEYS_URL` | `static` | [[013-limits-and-usage]] | no |
| `LECTIO_WORKERS` | 8 task slots per process | [[004-durable-tasks]] | yes: pages read at once |
| `LECTIO_TASK_ATTEMPTS` | 5 | [[004-durable-tasks]] | yes: attempts per page |
| `LECTIO_SHUTDOWN_GRACE` | 25s | [[004-durable-tasks]] | yes: how long open requests get to finish |
| `LECTIO_TASK_LEASE`, `LECTIO_TASK_EXPIRIES`, `LECTIO_WORKER_POLL`, `LECTIO_SWEEP_INTERVAL`, `LECTIO_TASK_RETENTION` | see spec | [[004-durable-tasks]] | no |
| `LECTIO_CLASS_WEIGHTS`, `LECTIO_GROUP_DEFAULTS` | `interactive=4,batch=1` | [[006-fairness-and-priority]] | no |
| `LECTIO_POOL_RECOVERY`, `LECTIO_POOL_RESUME` | 30s, 10s | [[007-model-capacity]] | no |
| `LECTIO_MAX_FILE_BYTES`, `LECTIO_MAX_PAGES` | 256 MiB, 3000 | [[009-intake]] | yes |
| `LECTIO_CACHE_BYTES` | 2 GiB | [[009-intake]] | no |
| `LECTIO_CONVERTER_URL` | none: formats that need conversion are refused | [[009-intake]] | yes |
| `LECTIO_CONVERT_ADDR` | `:8090` | [[009-intake]] | yes, by `lectio-convert` |
| `LECTIO_CONVERT_SUITE` | `soffice`, found on `PATH` | [[009-intake]] | yes, by `lectio-convert` |
| `LECTIO_CONVERT_TIMEOUT`, `LECTIO_CONVERT_MEMORY_BYTES` | 2m, 4 GiB | [[009-intake]] | yes, by `lectio-convert` |
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
`LECTIO_CONVERTER_URL` is where the conversion sidecar listens:
`http://host:port`, or `unix:///path/to/socket` for a sidecar that has
no network and listens on a socket in a directory it shares with the
server. The `LECTIO_CONVERT_*` variables are the sidecar's own, and it
also reads `LECTIO_MAX_FILE_BYTES`, for the largest file it takes and
the largest conversion it returns. `LECTIO_CONVERT_ADDR` is `host:port`
or `unix:/path/to/socket`.

`LECTIO_OIDC_ISSUERS`, `LECTIO_OIDC_AUDIENCE` and
`LECTIO_ADMIN_SUBJECTS` are comma-separated lists. An issuer is an
absolute `http` or `https` URL and is listed once, its trailing slash
aside. An admin subject is written as a verified subject is rendered,
`<issuer>|<sub>`. `LECTIO_AUTHORIZER_URL` is an absolute `http` or
`https` URL, and setting it without `LECTIO_AUTHORIZER_TOKEN` is an
error: the endpoint requires a bearer. A certificate authority of the
operator's own, for an issuer or an authorizer, is trusted the way the
process trusts any: through the system's roots or `SSL_CERT_FILE`.

Who is calling and who decides are selected apart
([[012-identity-and-authorization]]):

| Set | Who is calling | Who decides |
|---|---|---|
| `LECTIO_OIDC_ISSUERS` and `LECTIO_AUTHORIZER_URL` | a token verified against the issuers | the authorizer |
| `LECTIO_OIDC_ISSUERS` alone | a token verified against the issuers | the owner policy, with `LECTIO_ADMIN_SUBJECTS` |
| `LECTIO_DEV=true` and no issuer | the one static token, as the subject `dev` | the owner policy, or the authorizer when its URL is set |
| neither `LECTIO_DEV` nor an issuer | nobody: the server is refused, naming `LECTIO_OIDC_ISSUERS` | |

A development server that lists issuers verifies tokens against them
and takes no static token, so the verifier and an authorizer can be
tried over the memory store.

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

`deploy/base` holds a Deployment for each role, the Service of the API,
the account both run as, a network policy for each role and the API's
disruption budget, and nothing an installation chooses. The settings
and the Reader and Policy documents are 2 ConfigMaps an overlay
carries, and the credentials are Secrets an operator applies by hand;
the base reads each by name, and `deploy/README.md` lists the names and
the keys. Every host, bucket and namespace is the overlay's.
`deploy/components/converter` is the conversion sidecar, which an
overlay adds: Pods of its own, since containers of one Pod share a
network, under a policy that admits the workers' calls and has no
egress rule, and the one variable that gives the workers its address.
An installation without it runs no converter and refuses the formats
that need one. The API and the workers scale independently; the workers' count times `LECTIO_WORKERS`
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
- The Postgres declaration of the gate: `role: pooled`, since
  `internal/store/postgres` is a client and `internal/config` reads
  both URLs.
- The identity declaration of the gate: `role: core`, since
  `internal/access` verifies with the shared verifier and asks through
  the shared authorizer client. The rule about roles is not turned on:
  it cannot be turned off again, and no file here names the flag it
  forbids. The selection of the table above is `access.New`, which no
  command calls yet: `lectiod` still takes its one static token
  through `httpapi.Tokens`.
- A graceful stop: an upload that is still sending its body when the
  signal arrives is answered.
- `cmd/lectio-convert` and `deploy/converter/Dockerfile`: the conversion
  sidecar and its image, which holds the suite and the one binary and
  runs as a user that is not root. `lectio-convert version` prints the
  build's version, which the image's build stamps as `make build` does.
  The image is built by hand; no workflow builds or publishes it.
- `Dockerfile`: the server's image, `lectiod` alone on a distroless
  base, with no shell and no office suite, run as the numeric user
  65532. The build stage cross-compiles, so one file builds the image
  for each platform. Built for one platform it is 37 MiB, and it starts
  and reads a PDF with a read-only root file system and every
  capability dropped. It is built by hand; no workflow builds or
  publishes it.

- `deploy/base`, `deploy/bootstrap` and `deploy/examples/generic`: the
  manifests of both roles, written against the listeners, the probes
  and the settings of the durable server, which no build has yet, so no
  cluster has run them. They render with `kubectl kustomize`.
- `deploy/components/converter` and `deploy/examples/with-converter`:
  the sidecar's Deployment, Service, account and policies. Its
  container has a read-only root file system and scratch space at
  `/tmp`. No cluster has run them, and the sidecar has not been run
  with a read-only root file system.

- `deploy/examples/compose.yaml` and `docs/running.md`: Postgres, an
  object store with its bucket, `lectiod` in the role `all` with the
  stub reader, and the sidecar on a socket with no network. It is
  written against the settings of the durable server and has not parsed
  a file: Postgres and the object store start and the bucket is made,
  and `lectiod` of this build reads the stack's settings and exits,
  since it starts with `LECTIO_DEV=true` only. The stack has no stubs
  and no identity provider, so a caller brings a token of its own
  issuer.

- `deploy_test.go`, the tests of the deploy tree, and 2 jobs of the
  verify workflow. From the files as written, on every run of the gate:
  every container's security context, probes and resources; the
  listeners against the ports and the probes; the termination grace
  against what a stop takes; a policy with both directions around every
  Pod, and no egress rule and no credential for the sidecar; every
  variable set under `deploy/` against the configuration table; no
  credential and no address but an example's in a shipped file; and the
  example Reader and Policy against the loader. The tests that render
  the base and the examples need `kubectl`, skip in the gate, and run
  in the `deploy` job, which fails on a skip. The `image` job builds
  the server's image and holds it under 60 MiB, with no shell, starting
  and answering with a read-only root file system.

Remaining:

- `lectio-stubs` and its image, the rest of `docs/`, the generated
  configuration reference and its test, an image test of the
  conversion sidecar, the smoke test of the compose file, and the
  release workflow.
- Most of the test tiers. The suite today is unit tests, end-to-end
  tests of the API and the development server in one process, and the
  tests of `internal/store/postgres`, which start one Postgres and one
  PgBouncer in transaction mode per test binary and run every store
  case 3 ways: on a direct connection, in the query mode that prepares
  and describes nothing, and through the pooler. They skip where no
  container runtime answers. The dispatch simulation is among them: it
  drives the exchange function one task at a time with a virtual
  clock. There is no memory twin of the task store for a conformance
  suite to hold to the same cases, and no soak. The live tier is 2
  tests run by hand: `make live`, which reads a real file with a
  configured reader, and `make live-convert`, which converts a fixture
  of each converted format through a running sidecar.
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
