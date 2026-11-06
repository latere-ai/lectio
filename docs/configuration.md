# Configuration

This page is for the person who runs `lectiod` or `lectio-convert`. It lists every setting either program reads, its default, what it does, and what stops a start. Both programs are configured by their environment alone and take no flags: the one argument they accept is `version`.

How values are read, for every variable below:

- A variable that is unset, empty or only spaces is not set, and its default applies. Values are trimmed of spaces first.
- A **number** is a whole decimal number of 1 or more, with no unit or suffix: `268435456`, not `256MiB`.
- A **duration** is written as Go writes one, such as `500ms`, `30s`, `2m` or `720h`, and is above zero. There is no `d` unit for days.
- A **boolean** is what Go's `strconv.ParseBool` accepts: `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`, `F`, `FALSE`, `false`, `False`.
- A **list** is separated by commas. Each entry is trimmed and an empty entry is dropped.
- Every value is checked at start in every role and in the development server, whether or not that mode uses it. A bad value stops the start and the log names the variable, never the value, because a variable may hold a secret by mistake.
- A variable marked **secret** holds a credential. The programs never log it. Keep it out of images and manifests that are not secret.

The areas are [server](#server), [callers](#callers), [storage](#storage), [workers](#workers), [readers](#readers), [limits](#limits), [retention](#retention), [conversion](#conversion), [observability](#observability), [fixed values](#fixed-values) and [tests](#tests). [What refuses a start](#what-refuses-a-start) follows them.

## Server

`lectiod` runs in one of two modes. With `LECTIO_DEV` true it is a single process that keeps files and results in memory and loses them when it stops. Without it, it is the durable server, which keeps its work in Postgres and its bytes in an S3 bucket, and `LECTIO_ROLE` says whether the process serves the API, runs tasks, or does both. [local.md](local.md) runs both on one machine, and [install.md](install.md) installs the durable server.

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_DEV` | `false` | Boolean. `true` runs the development server: memory store, an in-process runner, no database, no bucket, and a stub reader unless `LECTIO_CONFIG` names real ones. It logs at start that nothing is kept. |
| `LECTIO_DEV_TOKEN` | `dev` | Secret. The one bearer a development server accepts, as the subject `dev`. Read only by a development server that lists no issuer in `LECTIO_OIDC_ISSUERS`. The durable server accepts no static token. |
| `LECTIO_ROLE` | `all` | `api`, `worker` or `all`. `api` applies the database schema, serves the API on `LECTIO_ADDR` and runs no task. `worker` runs tasks and listens on no public address; started before the schema exists, it retries the store up to 60 times, about 2 minutes, and then stops. `all` does both in one process. Ignored by the development server. Any other value stops the start. |
| `LECTIO_ADDR` | `:8080` | Where the API listens, as `host:port` or `:port`. Not opened by role `worker`. |
| `LECTIO_INTERNAL_ADDR` | `:8081` | Where a durable process serves `/livez`, `/readyz` and `/version`. Every role opens it, and two processes on one host need different values. Ready means the database answers and, for a worker, the task store answered an exchange of it within a third of `LECTIO_TASK_LEASE`. The development server does not open it. |
| `LECTIO_BASE_PATH` | `/v1` | The path the API is mounted at. It must begin with a slash. |
| `LECTIO_SHUTDOWN_GRACE` | `25s` | Duration. On `SIGINT` or `SIGTERM` the server stops accepting connections and gives open requests this long to finish. A worker lets its running tasks finish for the same time, settles them, and gives the rest back to the queue with no attempt counted. A durable process waits up to this time plus 5 seconds before it exits. |

## Callers

Who is calling and who decides are set apart. With `LECTIO_OIDC_ISSUERS` set, a bearer is verified against those issuers, in the development server too. With none set, only the development server starts, and it takes its one static token. With `LECTIO_AUTHORIZER_URL` set, that endpoint decides what a caller may do; otherwise the owner policy decides, with `LECTIO_ADMIN_SUBJECTS` as the subjects allowed to read everyone's files and parses. Limits an authorizer sets per caller are on [authorizer.md](authorizer.md). Only role `api`, role `all` and the development server verify tokens or ask the authorizer. A worker needs none of these variables.

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_OIDC_ISSUERS` | none | List of issuers whose tokens are accepted. Each entry is an absolute `http` or `https` URL, listed once, and a trailing slash is dropped. Required by the durable server in role `api` and `all`. Each issuer's keys are read at start; an issuer that does not answer is logged and its keys are read when its first token arrives. |
| `LECTIO_OIDC_AUDIENCE` | `lectio` | List of audiences a token may be addressed to, each listed once. The first is the primary one. A token addressed to none of them is refused. |
| `LECTIO_AUTHORIZER_URL` | none: the owner policy decides | An absolute `http` or `https` URL of the endpoint that is asked what a caller may do. Setting it without `LECTIO_AUTHORIZER_TOKEN` stops the start. At start the server sends it a probe that every authorizer denies. One that does not answer is logged, and every request then fails closed with `503 authorizer_unavailable` until it does. One that allows the probe is refused. |
| `LECTIO_AUTHORIZER_TOKEN` | none | Secret. The bearer the authorizer requires. Required with `LECTIO_AUTHORIZER_URL`. |
| `LECTIO_ADMIN_SUBJECTS` | none | List of subjects that read every owner's files and parses under the owner policy. Each entry is written as a verified subject is rendered, `<issuer>|<sub>`. A bare `sub` would match no caller, so it stops the start. Not used when `LECTIO_AUTHORIZER_URL` is set. |
| `SSL_CERT_FILE` | the system roots | Read by the Go runtime, not by Lectio. A certificate authority of your own, for an issuer, the authorizer, the key endpoint or the object store, is trusted the way any process trusts one: through the system's roots or this file. |

## Storage

The durable server needs a Postgres database and an S3-compatible bucket. Every durable role opens both, a worker included, so all of the variables below are required in every role except where a row says otherwise.

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_DATABASE_URL` | none | Secret, because the URL holds a password. The direct Postgres connection URL. The API applies the schema over it at start, so it must not be a transaction-mode pooler's. It is also the serving connection when `LECTIO_DATABASE_POOL_URL` is unset. Required by the durable server; the development server does not read it. |
| `LECTIO_DATABASE_POOL_URL` | none: the serving path opens `LECTIO_DATABASE_URL` | Secret. The Postgres URL the serving path opens, for an installation with a transaction-mode pooler in front of the database. Keep the direct address in `LECTIO_DATABASE_URL`. |
| `LECTIO_BUCKET` | none | The bucket that holds file bytes, page images and results. Required. |
| `LECTIO_BUCKET_PREFIX` | none | A path prepended to every key, so several installations share one bucket. Segments separated by slashes. A trailing slash is added if missing. |
| `LECTIO_S3_ENDPOINT` | none | An absolute URL of the object store, such as `http://127.0.0.1:9000`. Required. |
| `LECTIO_S3_REGION` | none | The region the signing uses. Required, even for a store that ignores it. |
| `LECTIO_S3_ACCESS_KEY` | none | The access key id. Required. Treat it as a credential. |
| `LECTIO_S3_SECRET_KEY` | none | Secret. The secret access key. Required. |
| `LECTIO_S3_PATH_STYLE` | `false` | Boolean. `true` addresses the bucket as the first segment of the path and not as a label of the host, which a store reached by an address needs. |

## Workers

These settings are the task store's and a worker's. A durable process writes its settings to the database when it opens the store, so the last process to open decides and the replicas of one deployment must carry the same values. The development server reads only `LECTIO_WORKERS` and `LECTIO_TASK_ATTEMPTS` from this area, for its in-process runner, and `LECTIO_GROUP_DEFAULTS` as the last row says.

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_WORKERS` | `8` | Number. Task slots per process: how many pages one process reads at once. Page reads are bounded across the fleet by each reader's `maxInFlight`, which [readers.md](readers.md) describes. |
| `LECTIO_TASK_ATTEMPTS` | `5` | Number. How many retryable failures fail a task. A retry waits [a growing time](#fixed-values) between tries. |
| `LECTIO_TASK_EXPIRIES` | `3` | Number. How many times a task's worker may die with the task leased before the task fails. |
| `LECTIO_TASK_LEASE` | `60s` | Duration of a worker's lease. A worker renews it every quarter of this time. A killed worker loses its tasks to the others after this time. At least `1ms`. |
| `LECTIO_SWEEP_INTERVAL` | `30s` | Duration. How often the sweeps for dead workers and for passed deadlines run, and how often a worker asks whether the retention sweep is due. At least `1ms`. |
| `LECTIO_WORKER_FLUSH` | `200ms` | Duration. The shortest time between two exchanges of one worker with the task store. |
| `LECTIO_WORKER_POLL` | `1s` | Duration. How long an idle worker waits before it asks for work again. With nothing to claim the wait doubles, with jitter, up to 5 times this value and never past a quarter of the lease. |
| `LECTIO_POOL_RECOVERY` | `30s` | Duration. After a reader's endpoint rate-limits, the quiet time after which the limited scope's ceiling of calls in flight rises again. With `LECTIO_KEYS=endpoint` the scope is the group whose key was limited; otherwise it is the reader for every group. At least `1ms`. |
| `LECTIO_POOL_RESUME` | `10s` | Duration. The ramp over which calls to a reader return to full after a pause ends. At least `1ms`. |
| `LECTIO_CACHE_BYTES` | `2147483648` (2 GiB) | Number of bytes. How much of the working copies of files a worker holds in memory between the pages it reads from them. |
| `LECTIO_GROUP_DEFAULTS` | none: a share of 1, no bound, no budget, priority 0 alone | List of `name=value`, each name used once, from `weight`, `max_running`, `max_queued`, `max_priority` and `pages_per_day`. Each value is a whole number of 0 or more. These are what a fairness group takes when the allow of its parse names none, and under the owner policy always. `weight` is the group's share, 1 to 1000, and 0 takes 1. `max_running` is the leased tasks a group may have at once, `max_queued` the parses that have not ended, `max_priority` the bound on a parse's priority on both sides, and `pages_per_day` the pages counted in one day. For the last three, 0 is no bound. The development server has one queue for every caller and refuses a nonzero `max_running`, `max_queued` or `pages_per_day`. |

## Readers

A reader is the model or the extractor that turns a page into blocks. Which readers exist, and in what order a page tries them, is declared in documents that `LECTIO_CONFIG` points at. [readers.md](readers.md) describes those documents.

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_CONFIG` | none: the stub reader, which calls no model and describes the page it was given | A path to one file, or to a directory. A directory is read for `*.yaml`, `*.yml` and `*.json` files in name order. Each file holds Reader and Policy documents, separated as YAML documents. It must declare at least one Reader and at most one Policy. The path is read once at start and not read again while the process runs: a change to the files takes a restart. Readers and a policy stored through the API are kept in the database and followed without one (see `LECTIO_READERS_POLL`). A path that does not exist, or documents that do not load, stop the start. |
| `LECTIO_MODEL_KEY` | none | Secret. The key every reader's endpoint is called with when `LECTIO_KEYS` is `static`. Whoever runs the server pays for every caller. Not accepted with `LECTIO_KEYS=endpoint` in a process that runs tasks. No document holds a key. |
| `LECTIO_KEYS` | `static` | Where the key a page is read with comes from. `static` is the key of `LECTIO_MODEL_KEY` for every group. `endpoint` asks `LECTIO_KEYS_URL`, with the bearer `LECTIO_KEYS_TOKEN`, for a key of each group's own. Read by every role, because it decides the scope a rate limit pauses. The development server accepts `static` only. Any other value stops the start. |
| `LECTIO_KEYS_URL` | none | An absolute `http` or `https` URL of the key endpoint. Read only by a process that runs tasks (role `worker` and `all`), where it is required with `LECTIO_KEYS=endpoint` and refused with `static`. Role `api` does not read it and does not need it. The endpoint is not part of readiness: while it is down, pages that need a key wait in the queue. |
| `LECTIO_KEYS_TOKEN` | none | Secret. The bearer the key endpoint requires. Read, required and refused exactly as `LECTIO_KEYS_URL` is. The API never holds it. |
| `LECTIO_READER_ENDPOINTS` | the endpoints the configured readers reach, in the order the documents name them | List of endpoint URLs a reader stored through the API may reach. The first is the endpoint of a stored reader that names none. A stored reader whose endpoint is not in the list is refused. Entries are compared as written and are not checked as URLs at start. |
| `LECTIO_READERS_POLL` | `10s` | Duration. How often a durable process checks whether the readers or the policy stored through the API changed, and reloads them when they did. |

## Limits

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_MAX_FILE_BYTES` | `268435456` (256 MiB) | Number of bytes. The largest file the server takes, whether uploaded or fetched from a URL, and the largest conversion it accepts back from the sidecar. `lectio-convert` reads the same variable for the largest file it takes and the largest conversion it returns, so give both programs one value. An authorizer may lower it for a caller and never raises it. |
| `LECTIO_MAX_PAGES` | `3000` | Number. The most pages a document may have, counted before any page selection. A document over it fails with `too_many_pages`. An authorizer may lower it for a caller. |
| `LECTIO_MAX_DEADLINE` | `1h` | Duration. The longest `deadline` a parse request may ask for, and the deadline of a parse submitted with none. An extraction or a figure run that has not ended this long after it was queued is given up. |
| `LECTIO_FETCH_ALLOW` | none | List of hosts, lowercased, that a source URL may name whatever they resolve to, such as the operator's own object store on a private network. An entry is a host name or `host:port`, and covers that host and no other. A redirect from an allowed host to another address is checked like any connection. Without an entry, only `https` URLs that resolve to public addresses are fetched. |

## Retention

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_FILE_RETENTION` | `24h` | Duration. How long a file is kept from an upload of it, and past the end of the last parse that read it. An authorizer may shorten it for a caller and never lengthens it. Workers remove what expired. The development server keeps everything until it stops. |
| `LECTIO_PARSE_RETENTION` | `720h` (30 days) | Duration. How long a parse, with its pages, images and document, is kept after it ended. An authorizer may shorten it for a caller. The development server keeps everything until it stops. |

A retention sweep removes at most 200 parses and 200 files each time it runs, and the next sweep takes what is left. How often it is checked is `LECTIO_SWEEP_INTERVAL`.

## Conversion

Presentations, rich text and legacy word-processing files are converted by a separate program, `lectio-convert`, which holds an office suite and should run with no network. Without `LECTIO_CONVERTER_URL`, `lectiod` refuses those formats.

For `lectiod`:

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_CONVERTER_URL` | none: formats that need conversion are refused | Where the sidecar listens. Either `http://host:port` (an `https` URL is accepted too, and a path prefix is kept), or `unix:///path/to/socket` for a sidecar with no network that shares a directory with the server. Anything else stops the start. A call is given 5 minutes. |

For `lectio-convert`:

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_CONVERT_ADDR` | `:8090` | Where it listens: `host:port`, or `unix:/path/to/socket` with one slash after the colon. A file left at the socket path by a sidecar that was killed is removed at start. |
| `LECTIO_CONVERT_SUITE` | `soffice` | The office suite's program, found on `PATH` once at start. The suite is started with an empty environment, in a scratch directory of its own. |
| `LECTIO_CONVERT_TIMEOUT` | `2m` | Duration. The time one conversion is given. A stopping sidecar waits this long for conversions in flight. |
| `LECTIO_CONVERT_MEMORY_BYTES` | `4294967296` (4 GiB) | Number of bytes. The address space the suite may map. It is set as a limit on the suite's process, which needs a Unix system; on another system the sidecar fails to convert. |
| `LECTIO_MAX_FILE_BYTES` | `268435456` (256 MiB) | The largest file taken and the largest conversion returned. The same variable as in [limits](#limits). |

## Observability

`lectiod` writes its log as JSON to standard error. With `OTEL_EXPORTER_OTLP_ENDPOINT` set it also exports logs, traces and metrics over OTLP/HTTP; without it nothing is exported and no overhead is added. These variables are read by the telemetry package `lectiod` uses and by the OpenTelemetry exporters it starts, not by Lectio's own code. `lectio-convert` writes JSON to standard error and reads none of them.

| Variable | Default | What it is |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none: no export | The base URL of an OTLP/HTTP collector. The exporters append the signal's path (`/v1/traces`, `/v1/metrics`, `/v1/logs`) after any path already in the URL. |
| `OTEL_EXPORTER_OTLP_HEADERS` | none | Secret when it carries a token. Headers sent with each export, as `key=value` pairs separated by commas, with values percent-encoded, such as `Authorization=Bearer%20<token>`. |
| `OTEL_SDK_DISABLED` | unset | `true`, in any case, turns every signal off even when an endpoint is set. Any other value leaves export on. |
| `OTEL_TRACES_SAMPLER_ARG` | `0.2` | A fraction from 0 to 1 of root traces kept. `1` keeps every trace. A parent's decision is honored, so a trace is kept or dropped as a whole. An unparseable or negative value is ignored and the default applies. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | none | Standard resource variables, read into every signal's resource. The service name is always set to `lectiod` and the version to the build's, over anything these set; other attributes are kept. |
| `LATERE_ENV` | `production` | The value of `deployment.environment.name` on every signal. |
| `POD_NAME`, `HOSTNAME` | the machine's host name | The replica name on every log record: `POD_NAME`, else `HOSTNAME`, else the host name. |

The OpenTelemetry Go exporters also read the other `OTEL_EXPORTER_OTLP_*` variables of the OpenTelemetry specification, such as a per-signal endpoint, a timeout and a certificate. They are not tested here.

## Fixed values

These are not settings. No variable changes them, and they are listed so that nothing an operator meets is left unstated.

| Value | Where it applies |
|---|---|
| A retry waits `min(60s, 1s * 2^(attempt - 1))` plus jitter uniform in half of that. | Failed tasks, up to `LECTIO_TASK_ATTEMPTS`. |
| Interactive work takes 4 units in 5 when both classes have work. | The queue's two classes, weights 4 and 1. |
| A rate-limit reply that names no pause pauses calls for 5s. 3 failures in a row open a reader's breaker for 30s. | A reader's pool. |
| A group's `weight` is at most 1000. | `LECTIO_GROUP_DEFAULTS` and an authorizer's allow. |
| A reader's calls in flight across the fleet are bounded by its `maxInFlight`: 8 when the document names none, 64 for a reader of the `text` adapter. | [readers.md](readers.md) |
| The Postgres pool holds at most 4 connections per process, and a connection is kept at most 30 minutes. | The serving path. |
| Reading an issuer's discovery document or keys is given 10s, and a token is read until 30s past its expiry and from 30s before its start. | Token verification. |
| A start waits at most 15s for the issuers' keys and the authorizer's probe. A readiness check is given 2s. | `lectiod` start and the internal listener. |
| A fetch follows at most 5 redirects, takes `https` only, and is given 1 minute with 10s to connect. | A source URL. |
| A submit may hold its answer for at most 60 seconds with `Prefer: wait`. | The API. |
| Logs, traces and metrics are batched and sent every 5s for traces and 30s for metrics. | Export. |
| A header read is given 10s. | Every listener. |

## Tests

These variables are read by the repository's tests and by `make live`, `make live-convert` and `make live-quality`. **The servers do not read them.** Setting one on a deployment does nothing. They call a model or run for minutes, so none belongs to `make check`.

| Variable | Default | What it is |
|---|---|---|
| `LECTIO_LIVE_CONFIG` | none | Path of a Reader and Policy file. Required by `make live` and `make live-quality`. |
| `LECTIO_LIVE_FILE` | none | The file `make live` parses. |
| `LECTIO_LIVE_PAGES` | `1-3` | The page selection `make live` submits. |
| `LECTIO_LIVE_OUT` | none | A directory the results are written to. Required by `make live-quality`, which writes its report there; `make live` and `make live-convert` write there when it is set. |
| `LECTIO_LIVE_DESCRIBE` | unset | Any value makes `make live` also describe the figures of the parse and require a description for each figure that has a box. |
| `LECTIO_LIVE_CONVERTER` | none | The address of a running sidecar, `http://host:port` or `unix:///path/to/socket`. Required by `make live-convert`. Without it `make live-quality` leaves out the files that need conversion. |
| `LECTIO_LIVE_FILES` | all | List of corpus file names, separated by commas or spaces, that `make live-quality` reads. A name outside the corpus fails the run. |
| `LECTIO_LIVE_SERVER` | `durable` | `durable` or `dev`. `dev` runs `make live-quality` against the development server, which needs no container runtime. |
| `LECTIO_LIVE_LECTIOD` | built from the tree | Path of a `lectiod` binary to run in place of the one `make live-quality` builds. |
| `LECTIO_MODEL_KEY` | none | Also the key these tests hand to the server they start. |
| `LECTIO_SOAK`, `LECTIO_SOAK_PARSES` | unset, `1000` | `LECTIO_SOAK` set runs the soak test under random worker kills. The second scales its number of parses. |
| `LECTIO_THROUGHPUT`, `LECTIO_THROUGHPUT_WORKERS`, `LECTIO_THROUGHPUT_PAGES` | unset, `25`, `12000` | `LECTIO_THROUGHPUT` set runs the throughput test behind a pooler of one connection. The others scale it. |
| `LECTIO_SIMULATION` | small | `full` runs the fairness simulations at the size the spec states. |
| `LECTIO_CLAIM_ROWS` | the test's own | Scales the claim test's row count. |
| `LECTIO_CALIBRATE` | unset | Any value prints the extraction validator's time per unit of work. |
| `LECTIO_TEST_CALLS`, `LECTIO_TEST_FAIL`, `LECTIO_TEST_DELAY`, `LECTIO_TEST_FIGURES`, `LECTIO_TEST_GATE`, `LECTIO_TEST_GATE_PAGE`, `LECTIO_TEST_WINDOW`, `LECTIO_TEST_PROCESS` | unset | Hooks of the test reader that `cmd/lectiod`'s process tests run. |

## What refuses a start

Both programs print the error to standard error as `lectiod: <message>` or `lectio-convert: <message>` and exit with status 1. The checks run in the order below. All value errors of the first group are reported together, one per line.

### lectiod

| Condition | What the log says |
|---|---|
| An argument other than `version` | `unknown argument "<arg>"; lectiod is configured by its environment` |
| `LECTIO_DEV` or `LECTIO_S3_PATH_STYLE` is not a boolean | `<variable> is not true or false` |
| A number variable is not a whole number of 1 or more | `<variable> is not a number above zero` |
| A duration variable is not a duration above zero | `<variable> is not a duration above zero, such as 30s` |
| `LECTIO_BASE_PATH` does not begin with a slash | `LECTIO_BASE_PATH does not begin with a slash` |
| `LECTIO_ROLE` is not `api`, `worker` or `all` | `LECTIO_ROLE is not api, worker or all` |
| `LECTIO_GROUP_DEFAULTS` has a name outside the five, no `=`, a name twice, a value that is not a whole number of 0 or more, or a `weight` above 1000 | `LECTIO_GROUP_DEFAULTS has an entry that is not one of weight, max_running, max_queued, max_priority and pages_per_day, as name=value`; `LECTIO_GROUP_DEFAULTS names <name> twice`; `LECTIO_GROUP_DEFAULTS: <name> is not a whole number of zero or more`; `LECTIO_GROUP_DEFAULTS: weight is above 1000` |
| `LECTIO_KEYS` is not `static` or `endpoint` | `LECTIO_KEYS is not static or endpoint` |
| `LECTIO_KEYS=endpoint` in the development server | `LECTIO_KEYS is endpoint while LECTIO_DEV is true: a development server reads every page with the key of LECTIO_MODEL_KEY` |
| In a process that runs tasks, `LECTIO_KEYS_URL` or `LECTIO_KEYS_TOKEN` set while `LECTIO_KEYS` is `static` | `LECTIO_KEYS_URL or LECTIO_KEYS_TOKEN is set while LECTIO_KEYS is static: the endpoint is asked only with LECTIO_KEYS=endpoint` |
| In a process that runs tasks, `LECTIO_KEYS=endpoint` with no `LECTIO_KEYS_URL`, a URL that is not absolute `http` or `https`, or no `LECTIO_KEYS_TOKEN` | `LECTIO_KEYS_URL is not set while LECTIO_KEYS is endpoint: a group's key is asked there`; `LECTIO_KEYS_URL is not an absolute http or https URL`; `LECTIO_KEYS_TOKEN is not set while LECTIO_KEYS is endpoint: the key endpoint requires a bearer` |
| In a process that runs tasks, `LECTIO_KEYS=endpoint` with `LECTIO_MODEL_KEY` set | `LECTIO_MODEL_KEY is set while LECTIO_KEYS is endpoint: a page is read with a key from one source, and these are 2` |
| An entry of `LECTIO_OIDC_ISSUERS` is not an absolute `http` or `https` URL, or an issuer is listed twice | `LECTIO_OIDC_ISSUERS has an entry that is not an absolute http or https URL`; `LECTIO_OIDC_ISSUERS lists an issuer twice` |
| An audience is listed twice | `LECTIO_OIDC_AUDIENCE lists an audience twice` |
| `LECTIO_AUTHORIZER_URL` is not an absolute `http` or `https` URL, or is set with no `LECTIO_AUTHORIZER_TOKEN` | `LECTIO_AUTHORIZER_URL is not an absolute http or https URL`; `LECTIO_AUTHORIZER_TOKEN is not set while LECTIO_AUTHORIZER_URL is: the authorizer requires a bearer` |
| An entry of `LECTIO_ADMIN_SUBJECTS` is not `<issuer>|<sub>` | `LECTIO_ADMIN_SUBJECTS has an entry that is not of the form <issuer>|<sub>` |
| Without `LECTIO_DEV`, no `LECTIO_DATABASE_URL` | `LECTIO_DATABASE_URL is not set. The durable server keeps its work in a database; LECTIO_DEV=true runs without one and keeps nothing` |
| `LECTIO_CONFIG` names a path that cannot be read, a document with a bad `apiVersion`, a kind other than Reader or Policy, a reader name used twice, a second Policy, no Reader at all, or a reader or policy that does not build | `config: <path>: ...`, or `config: <file>, Reader "<name>": ...` followed by the reason. See [readers.md](readers.md). |
| `LECTIO_CONVERTER_URL` is neither an `http` or `https` URL nor `unix:///path` | `LECTIO_CONVERTER_URL: convert: the converter's address is neither an http URL nor unix:///path/to/socket` |
| A server that serves the API (role `api` or `all`) with no `LECTIO_OIDC_ISSUERS` and no `LECTIO_DEV` | `LECTIO_OIDC_ISSUERS is not set. A server verifies its callers against an issuer; LECTIO_DEV=true takes one static token and no issuer` |
| The development server with `LECTIO_GROUP_DEFAULTS` setting `max_running`, `max_queued` or `pages_per_day` | `LECTIO_GROUP_DEFAULTS sets max_running, max_queued or pages_per_day, which a development server does not hold: it has one queue for every caller and no group` |
| `LECTIO_AUTHORIZER_URL` names an endpoint that allows the probe every authorizer denies | `LECTIO_AUTHORIZER_URL names an endpoint that allowed the probe every authorizer denies: it does not read what it is asked` |
| A bucket setting is missing: `LECTIO_S3_ENDPOINT`, `LECTIO_S3_REGION`, `LECTIO_BUCKET`, `LECTIO_S3_ACCESS_KEY` or `LECTIO_S3_SECRET_KEY` | `blob: <variable> is not set` |
| `LECTIO_S3_ENDPOINT` is not an absolute URL, or `LECTIO_BUCKET_PREFIX` is not slash-separated segments | `blob: LECTIO_S3_ENDPOINT is not an absolute URL`; `blob: LECTIO_BUCKET_PREFIX is not a path of segments separated by slashes` |
| The database URL is not a Postgres URL | `store: the serving database URL is not a Postgres URL` |
| The database cannot be reached or the schema cannot be applied | `store: applying the schema: ...`, or `store: opening the pool: ...`. A worker whose store does not open retries up to 60 times, about 2 minutes, before it gives up. |
| A previous migration failed halfway, or a newer binary wrote the schema | `store: a migration failed halfway and an operator repairs it before this process starts`; `store: the schema is newer than the migrations this binary carries: run the version that wrote the schema` |
| A duration is under a millisecond, such as `LECTIO_TASK_LEASE=500us` | `store: tasks: the lease is below a millisecond` (and likewise for the sweep interval, pool recovery and pool resume) |
| A listen address is taken or malformed | `LECTIO_INTERNAL_ADDR: listen tcp ...`, or `listen tcp ...` for `LECTIO_ADDR` |

What `lectiod` logs and then starts anyway:

| Condition | What the log says |
|---|---|
| No `LECTIO_CONFIG` | `LECTIO_CONFIG is not set: pages are read by the stub reader, which calls no model and describes the page it was given` |
| No `LECTIO_CONVERTER_URL` | `LECTIO_CONVERTER_URL is not set: a format that needs conversion is refused` |
| The development server | `LECTIO_DEV: files, parses and results are kept in memory and are lost when the process stops`; with no issuer, `no issuer is configured: the one caller is the holder of LECTIO_DEV_TOKEN` |
| An issuer that does not answer at start | `an issuer's keys could not be read at start; they are read when its first token arrives` |
| An authorizer that does not answer the probe | `the authorizer did not answer the probe at start; requests fail closed until it does` |
| A document sets what this build does not apply | `the configuration sets what this build does not apply`, with the setting named |

### lectio-convert

| Condition | What the log says |
|---|---|
| An argument other than `version` | `unknown argument "<arg>"; lectio-convert is configured by its environment` |
| `LECTIO_CONVERT_TIMEOUT` is not a duration above zero | `LECTIO_CONVERT_TIMEOUT is not a duration above zero, such as 2m` |
| `LECTIO_CONVERT_MEMORY_BYTES` is not a number above zero | `LECTIO_CONVERT_MEMORY_BYTES is not a number above zero` |
| `LECTIO_MAX_FILE_BYTES` is not a number above zero | `LECTIO_MAX_FILE_BYTES is not a number above zero` |
| `LECTIO_CONVERT_SUITE` is not a program that can be run | `LECTIO_CONVERT_SUITE names no program that can be run here` |
| The file at a `unix:` socket path cannot be removed | `the socket's path is taken: ...` |
| The address cannot be listened on | `listen tcp ...` or `listen unix ...` |
