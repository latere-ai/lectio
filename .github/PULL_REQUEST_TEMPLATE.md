## What this changes

<!-- What behavior is different after this, for whoever runs lectiod, chooses its readers, calls the API, or writes an authorizer. -->

## Why

<!-- The problem it solves. Link the issue if there is one. -->

## Checklist

- [ ] `make` passes locally.
- [ ] A bug fix carries a test that fails without it.
- [ ] A model is reached only through `reader/`, and a new instruction to
      a model is a template in `internal/prompts/` with a test that holds
      its text.
- [ ] A change to the API, a configuration variable, a Reader or Policy
      document, or the authorizer's vocabulary or limits is reflected in
      its spec, in `docs/`, and in `api/openapi.yaml` where it applies.
- [ ] A change a person running `lectiod` would notice has a line under
      `## Unreleased` in `CHANGELOG.md`.
