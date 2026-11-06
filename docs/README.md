# Documentation

For people who run `lectiod`, choose the models that read its pages,
build a client of its API, or write the authorizer it asks.

## Trying it

| Page | |
|---|---|
| [Running it locally](local.md) | the development server with no model, then the durable stack on one machine with a local issuer, a parse that survives a restart, the conversion sidecar, and a reader that calls a model |

## Running it

| Page | |
|---|---|
| [Install](install.md) | the two images, Postgres, the bucket, the roles and probes, the conversion sidecar, the cluster manifests, upgrading, and what a production installation must have |
| [Configuration](configuration.md) | every `LECTIO_*` variable, its default, and what refuses a start |
| [Readers](readers.md) | the Reader and Policy documents, chains, reading a PDF's own text, changing readers while the server runs, and whose key reads a page |
| [Security](security.md) | what Lectio guarantees, what it sends where and how long it keeps it, what it does not defend, and what an operator keeps safe |
| [Quality](quality.md) | the corpus a reader is measured on, the measures and the bars a reader must meet |

## Building against it

| Page | |
|---|---|
| [API](api.md) | the objects, uploading, parsing, events, failed pages, figures, extraction with a schema, usage, the errors to branch on, and the limits a client meets |
| [Writing an authorizer](authorizer.md) | the question Lectio asks, the vocabulary, the limits an allow carries, failing closed, and the owner policy |

The OpenAPI document is [`api/openapi.yaml`](../api/openapi.yaml), and
every installation serves it at `GET /v1/openapi.yaml`, under its base
path.

## Changing it

[`CONTRIBUTING.md`](../CONTRIBUTING.md) is how to build and test a change
and where code belongs. [`specs/`](../specs/README.md) is the design,
and each spec says what of it is built. [`SECURITY.md`](../SECURITY.md)
is how to report a vulnerability.
