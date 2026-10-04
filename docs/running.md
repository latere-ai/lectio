# Running Lectio

3 ways to run `lectiod`, from the smallest to an installation: the
development server in a container, the whole stack on one machine with
compose, and a Kubernetes cluster. The commands use `podman`; `docker`
takes the same arguments.

## The image

```sh
podman build -t lectiod .
podman run --rm lectiod version
```

The image holds `lectiod` and nothing else: no shell, no package
manager, no office suite. It runs as user 65532 and writes no file, so
it runs with a read-only root file system and no capability.

## The development server

One process that keeps everything in memory, takes the token `dev`, and
reads pages with a stub that calls no model:

```sh
podman run --rm --read-only --cap-drop ALL \
  -e LECTIO_DEV=true -p 127.0.0.1:8080:8080 lectiod
```

Nothing survives a stop. The [README](../README.md#run-it) walks through
the requests it answers.

## The stack on one machine

`deploy/examples/compose.yaml` runs the durable server: Postgres for the
parses and their tasks, an S3-compatible object store for the bytes,
`lectiod` in the role `all`, which is the API and a worker in one
process, and the conversion sidecar on a socket with no network.

The stack has no identity provider of its own, and `lectiod` does not
start without one to verify a caller against. Name yours when you start
it, and use a token it issued for the audience `lectio`. Without
`LECTIO_OIDC_ISSUERS` the command stops and says so:

```sh
LECTIO_OIDC_ISSUERS=https://issuer.example \
  podman compose -f deploy/examples/compose.yaml up --build
```

`lectiod` listens on `127.0.0.1:8080`. It runs the durable server, so it
needs a build that has one: on a build without it, `lectiod` exits at
start and says so, and the other services stay up.

Upload a file and parse it. The second command holds its answer up to 30
seconds for the parse to end:

```sh
curl -s -H "Authorization: Bearer $TOKEN" --data-binary @sample.pptx \
  "http://127.0.0.1:8080/v1/files?name=sample.pptx"

curl -s -H "Authorization: Bearer $TOKEN" -H 'Prefer: wait=30' \
  -H 'Content-Type: application/json' -d '{"source":{"file":"fil_..."}}' \
  "http://127.0.0.1:8080/v1/parses"
```

The first answers with the file's `id`, which the second names. A
presentation goes through the sidecar, so a parse of one that ends with
`succeeded` has exercised every service of the stack.

Pages are read by the stub reader. To read them with a model, write a
file of Reader and Policy documents, mount it into the `lectiod`
service, and set `LECTIO_CONFIG` to its path and `LECTIO_MODEL_KEY` to
the key the endpoint takes.

Every credential in the file is an example and is written in it. The
stack is for one machine: only `lectiod`'s port is published, on the
loopback interface.

```sh
podman compose -f deploy/examples/compose.yaml down --volumes
```

removes the stack and what it stored.

## A cluster

[`deploy/`](../deploy/README.md) holds Kustomize manifests: a base with
one Deployment for the API and one for the workers, a component that
adds the conversion sidecar, and 2 example overlays. Each release
attaches the same tree as `deploy-<tag>.tar.gz`, with both images pinned
to the release by digest, and publishes the images as
`ghcr.io/<owner>/lectiod:<tag>` and `ghcr.io/<owner>/lectio-convert:<tag>`.

## Converting office documents

The sidecar that converts presentations, rich text and legacy
word-processing files has an image of its own:

```sh
podman build -f deploy/converter/Dockerfile -t lectio-convert .
```

It runs a large program on a file somebody else wrote, so it is run
with no network. The compose file gives it a socket on a volume it
shares with `lectiod`; the cluster manifests give it Pods of its own
under a policy that refuses every connection it opens.
