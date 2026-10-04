# Deploy

Kustomize manifests for running `lectiod` on a Kubernetes cluster, the
image of the conversion sidecar, and a compose file for one machine.
This page is the reference for what each file is, what the base reads,
and what an installation adds.

```
base/                     both roles of lectiod: 2 Deployments, Service, account, policies, budget
components/converter/     the conversion sidecar, as a component an overlay adds
bootstrap/                the Namespace and the Secrets, applied once by hand
examples/generic/         an installation without conversion
examples/with-converter/  the same installation with the sidecar
examples/compose.yaml     Postgres, an object store, lectiod and the sidecar on one machine
converter/Dockerfile      the image of the conversion sidecar
```

The compose file is described in [docs/running.md](../docs/running.md).

## What it needs

- Kubernetes 1.30 or later. The API's Pods use the kubelet's own sleep
  as their `preStop` hook, because the image has no shell.
- A network plugin that enforces NetworkPolicy, egress included. Without
  one the policies here admit and refuse nothing, and the conversion
  sidecar has a network.
- A Postgres database, an S3-compatible object store with a bucket, and
  an OpenID Connect issuer. None is deployed here.

## Apply order

```sh
kubectl apply -f bootstrap/namespace.yaml
cp bootstrap/secrets.example.yaml secrets.yaml   # fill it, keep it out of a checkout
kubectl -n lectio apply -f secrets.yaml
kubectl apply -k examples/generic
kubectl -n lectio rollout status deployment/lectiod deployment/lectiod-worker
```

The Secrets come before the overlay: both Deployments read the database
and the object store's key pair as required keys, so a Pod applied
without them waits and does not start.

The namespace is applied on its own and is in no kustomization. A
namespace outlives every apply of what is in it.

## What the base pins, and what it does not

The base names **no namespace**, **no image registry**, **no host** and
**no value of any setting an installation chooses**. An overlay sets the
namespace and carries the 2 ConfigMaps. The images are the placeholders
`lectiod` and `lectio-convert`, which an overlay points at a release:

```sh
cd examples/with-converter
kustomize edit set image lectiod=ghcr.io/<owner>/lectiod:<tag>
kustomize edit set image lectio-convert=ghcr.io/<owner>/lectio-convert:<tag>
```

The deploy archive attached to a release, `deploy-<tag>.tar.gz`, is this
directory with both references already pinned to the release by digest,
so an installation from the archive edits nothing to run the version it
downloaded.

## What the base creates

| Object | Name | What it is |
|---|---|---|
| ServiceAccount | `lectiod` | the account both roles run as; no Role, no token mounted |
| Deployment | `lectiod` | the API, `LECTIO_ROLE=api`: 2 replicas, rolled one at a time with none missing |
| Deployment | `lectiod-worker` | the workers, `LECTIO_ROLE=worker`: 2 replicas, no public port |
| Service | `lectiod` | the API's public listener, port 8080 |
| PodDisruptionBudget | `lectiod` | at most one replica of the API down at a time |
| NetworkPolicy | `lectiod` | the API: in on 8080; out to DNS, 443 and 5432 |
| NetworkPolicy | `lectiod-worker` | the workers: nothing in; out to DNS, 443 and 5432 |

Both Deployments run one image. Each container runs as user 65532 with
the default seccomp profile, no privilege escalation, every capability
dropped and a read-only root file system, and mounts no volume it can
write to: `lectiod` writes no file.

| Container | Ports | Readiness | Liveness | Requests | Limits |
|---|---|---|---|---|---|
| `lectiod` (API) | `public` 8080, `internal` 8081 | `GET /readyz` on `internal` | `GET /livez` on `internal` | 100m, 256Mi | 1 CPU, 1Gi |
| `lectiod` (worker) | `internal` 8081 | `GET /readyz` on `internal` | `GET /livez` on `internal` | 500m, 1Gi | 2 CPU, 4Gi |
| `lectio-convert` | `convert` 8090 | TCP on `convert` | TCP on `convert` | 100m, 512Mi | 2 CPU, 4Gi |

The API and the workers scale apart. The workers' count times
`LECTIO_WORKERS` is how many tasks the fleet runs at once. A worker
renders PDF pages in process, so its memory follows the files an
installation parses; the limits are a starting point.

## Configuration

Every setting is an environment variable, and
[the configuration table](../specs/016-distribution.md#configuration)
lists each with its default. The base reads them from 3 places.

**Set by the base**, in the Deployments, because the manifests depend on
them:

| Variable | API | Worker |
|---|---|---|
| `LECTIO_ROLE` | `api` | `worker` |
| `LECTIO_ADDR` | `:8080` | not set: a worker serves no caller |
| `LECTIO_INTERNAL_ADDR` | `:8081` | `:8081` |
| `LECTIO_OIDC_AUDIENCE` | `lectio` | not set: a worker verifies no token |
| `LECTIO_CONFIG` | `/etc/lectio` | `/etc/lectio` |
| `LECTIO_SHUTDOWN_GRACE` | `25s` | `25s` |

**The ConfigMap `lectiod`**, which the overlay carries and both roles
read whole. Any variable of the configuration table may be a key; a
variable the base sets itself is not overridden from here.

| Key | What it is |
|---|---|
| `LECTIO_OIDC_ISSUERS` | the issuers whose tokens the API accepts, as a comma list |
| `LECTIO_ADMIN_SUBJECTS` | under the owner policy, the subjects that may read every owner's resources |
| `LECTIO_BUCKET`, `LECTIO_BUCKET_PREFIX` | the bucket, and the prefix this installation keeps its objects under |
| `LECTIO_S3_ENDPOINT`, `LECTIO_S3_REGION`, `LECTIO_S3_PATH_STYLE` | where the object store answers, and how a bucket is addressed there |
| `LECTIO_WORKERS` | the tasks one worker runs at once |
| `LECTIO_MAX_FILE_BYTES`, `LECTIO_MAX_PAGES` | the largest file and the most pages a parse takes |
| `LECTIO_GROUP_DEFAULTS` | what a group takes where no allow names it, as `name=value` pairs: its `weight`, `max_running`, `max_queued`, `max_priority` and `pages_per_day` |
| `LECTIO_FILE_RETENTION`, `LECTIO_PARSE_RETENTION` | how long a file and a parse are kept, `24h` and `720h` unless set |
| `LECTIO_KEYS` | where the key a page is read with comes from: `static`, the default, or `endpoint`; see [A key per tenant](#a-key-per-tenant) |

**The ConfigMap `lectiod-readers`**, which the overlay carries: the
Reader and Policy documents, mounted read-only at `/etc/lectio`. Every
`.yaml`, `.yml` and `.json` key is read at start, in name order.
`examples/generic/readers.yaml` holds one Reader and one Policy with
example values.

**Secrets**, applied by hand from `bootstrap/secrets.example.yaml`. Each
variable reads one key of the same name.

| Secret | Keys | Read by | Without it |
|---|---|---|---|
| `lectiod-db` | `LECTIO_DATABASE_URL`, and optionally `LECTIO_DATABASE_POOL_URL` | both roles | the Pods do not start |
| `lectiod-s3` | `LECTIO_S3_ACCESS_KEY`, `LECTIO_S3_SECRET_KEY` | both roles | the Pods do not start |
| `lectiod-model` | `LECTIO_MODEL_KEY` | the workers | with `LECTIO_KEYS=static`, the readers' endpoints are called with no key |
| `lectiod-keys` | `LECTIO_KEYS_URL`, `LECTIO_KEYS_TOKEN` | the workers | every page is read with the key of `lectiod-model`; with `LECTIO_KEYS=endpoint` the workers do not start |
| `lectiod-authorizer` | `LECTIO_AUTHORIZER_URL`, `LECTIO_AUTHORIZER_TOKEN` | the API | the owner policy decides: a subject acts on what it owns |

`LECTIO_DATABASE_URL` is the direct endpoint of the database. The API
runs the migrations over it at start. `LECTIO_DATABASE_POOL_URL` is a
transaction-mode pooler's endpoint, which the serving path opens where
there is one.

The model key and the key endpoint's bearer are the workers' alone, and
the authorizer's bearer the API's alone: a role holds the credentials of
what it dials and no others.

## A key per tenant

The key a reader's endpoint is called with decides who that endpoint
charges. `LECTIO_KEYS` names where the key comes from.

| `LECTIO_KEYS` | The key of a page | Apply |
|---|---|---|
| `static`, the default | one key for every group: the operator pays for every page | the Secret `lectiod-model`, or none for an endpoint that takes no key |
| `endpoint` | a key an endpoint of yours issued for the page's group, so a gateway attributes each tenant's spend and bounds it by that tenant's budget | the Secret `lectiod-keys`, and `LECTIO_KEYS: "endpoint"` in the ConfigMap `lectiod` |

`LECTIO_KEYS` is a key of the ConfigMap and not of the workers' Secret
because both roles act on it: with a key per group, a rate limit pauses
the calls of the group whose key was limited and no other group's, and
that is a setting of the task store each role opens. The API reads
neither `LECTIO_KEYS_URL` nor `LECTIO_KEYS_TOKEN` and starts without
them.

The 2 sources exclude each other. A worker that is given
`LECTIO_KEYS=endpoint` without both keys of `lectiod-keys`, or with
`lectiod-model` beside them, does not start and names the variable. A
worker that is given `lectiod-keys` while `LECTIO_KEYS` is `static` does
not start either: an installation must not believe its tenants read with
their own keys while every page is read with the operator's.

To switch an installation to the endpoint, apply `lectiod-keys`, delete
`lectiod-model`, set `LECTIO_KEYS: "endpoint"` in the ConfigMap, and
roll both Deployments.

The workers send the endpoint one request per group, with the bearer:

```
POST <LECTIO_KEYS_URL>
Authorization: Bearer <LECTIO_KEYS_TOKEN>
Content-Type: application/json

{"group": "...", "owner": "...", "parse": "..."}
```

| The endpoint answers | The group's pages |
|---|---|
| `200` with `{"key": "...", "expires_at": "<RFC 3339>"}` | are read with the key, which a worker holds in memory and asks again 1 minute before it expires |
| `402` | fail with `budget_exhausted`: the group has no budget |
| `403` | fail with `reader_unavailable`: the group is issued no key |
| anything else, or nothing | wait in the queue and fail nothing; the worker asks again after 1 second, then after twice as long each time, up to 30 seconds |

A key has to be good for more than 1 minute when it is issued, or it is
never used. A worker whose key endpoint does not answer stays ready.

## Stopping and rolling

A stop gives open requests and running tasks `LECTIO_SHUTDOWN_GRACE` to
finish. The API's Pods sleep 5 seconds first, so that every proxy has
taken the Pod out of the Service before its listener closes. The
termination grace of both Deployments, 40 seconds, covers the sleep and
the 25 seconds of grace. An installation that raises
`LECTIO_SHUTDOWN_GRACE` patches `terminationGracePeriodSeconds` with it.

A worker that stops returns the tasks it did not finish to the queue,
and another worker takes them.

## Network

Both policies name both directions, so a connection no rule admits is
refused. The rules are generic ports, because the destinations are the
installation's:

| Role | In | Out |
|---|---|---|
| API | 8080 from any Pod of the cluster | cluster DNS; 443 for the issuers, the authorizer, the object store and a source a caller submits by address; 5432 for the database |
| Worker | nothing | cluster DNS; 443 for the object store, the model endpoint and the key endpoint; 5432 for the database |

A port in a rule is the destination Pod's own and not its Service's. A
pooler on 6432, an object store on 9000 or a model gateway or a key
endpoint inside the cluster on another port is refused until an overlay
adds it. The
kubelet's probes are not subject to policy, so port 8081 is in no rule;
an installation that scrapes the internal listener admits its scraper.

## Conversion

Presentations, rich text and legacy word-processing files are converted
before they are read, by `lectio-convert`, a sidecar that holds an
office suite. It is optional. `components/converter` deploys it, and
`examples/with-converter` shows an overlay that adds it:

```yaml
components:
  - ../../components/converter
```

The component adds a Deployment, a Service and a ServiceAccount named
`lectio-convert`, 2 policies, and one variable on the workers,
`LECTIO_CONVERTER_URL=http://lectio-convert:8090`. Without the
component the workers have no converter, and a parse of a format that
needs one fails with `unsupported_media_type`.

The sidecar runs a large program on a file somebody else wrote, so:

- **It has no network.** The policy `lectio-convert` names both
  directions and has no egress rule: every connection out is refused,
  DNS included. Its one ingress rule admits the workers' Pods on port
  8090. The policy `lectiod-worker-convert` opens that port for the
  workers.
- **It holds no credential.** It reads no ConfigMap and no Secret,
  mounts no token, and the addresses of the namespace's Services are
  kept out of its environment.
- **It is bounded.** Its root file system is read-only; each conversion
  writes under `/tmp`, a volume of at most 2Gi. Its memory limit is 4Gi.

## What an installation adds

The base stops where an installation's own choices begin. An overlay
adds:

- **The namespace**, and the 2 ConfigMaps with its own values.
- **The Secrets**, from wherever it keeps secrets.
- **An Ingress or a Gateway route** to the Service `lectiod`, port 8080,
  with TLS. The contract is served under `/v1`, or under
  `LECTIO_BASE_PATH` where an installation sets one.
- **Narrower network rules**, where it can name its ingress controller,
  its database, its object store and its model endpoint, and the ports
  of any of them that listen off 443 and 5432.
- **An image pull secret**, where it mirrors the images to a private
  registry: an `imagePullSecrets` entry on each ServiceAccount.
- **Its own replica counts and resource limits**, and an autoscaler if
  it wants one.
