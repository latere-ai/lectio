# Contributing

This file is for people and agents changing Lectio. Users read the
[README](README.md); the design and the reasoning behind it live in
[`specs/`](specs/README.md).

## Getting set up

You need Go 1.27 or newer and `git`. Then:

```sh
make        # the quality gate
make build  # out/lectiod
```

`make` needs only the Go toolchain and git. Everything it pins comes from
public modules, so it runs the same on your machine as in CI.

Install the hooks once with `make hooks`. They run formatting and license
checks before a commit and the linter before a push, so you see a finding
before CI does.

## Sending a change

Keep one logical change per commit, stage the files explicitly, and write
the subject as `scope: what changed`, for whoever reads the log. The
pipeline runs the gate on every push and pull request.

A change to a behavior a spec describes updates that spec in the same
change: its status, and its Outcome once it is complete. If you are
planning something large, write the spec first. A design that lands
without one is harder to review than one that arrives with the reasoning
attached.

## The bar

`make` runs the whole gate (`go tool lateregate`): formatting, the linter,
modernization, known vulnerabilities, the suite with and without the race
detector, per-package coverage at 90% or more, the suite with only the
toolchain on `PATH`, the license notice, and the spec tree.
`go tool lateregate list` names the gates and `go tool lateregate <name>`
runs one.

A bug fix carries a test that fails without it. A change that lowers a
threshold or adds a waiver records the reason in `.lateregate.yaml`, so the
exception is reviewable rather than invisible.

## Where things go

| Path | What lives there |
|---|---|
| `document/` | the object model a parse produces: page, block, table, field. Public |
| `reader/` | the interfaces a model sits behind, for reading a page and for extracting fields, and their adapters. Public |
| `api/` | the HTTP contract, `openapi.yaml` |
| `internal/` | everything else, one package per concern |
| `cmd/lectiod/` | the server |
| `specs/` | one spec per component |

A model, an OCR engine, or a gateway is reached only through `reader/`.
Nothing else in the tree knows that a model exists.

An instruction to a model is written in one place, `internal/prompts/`,
as a template file, and rendered when the call is made. An adapter that
needs a new instruction adds a template, a data struct for it, and a
test that holds its full text. It does not build one from strings.
