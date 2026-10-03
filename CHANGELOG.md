# Changelog

What changed for whoever runs `lectiod`, calls its API, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- Added: the repository scaffold. The object model (`document`), the
  interfaces a page-reading model and a field-extracting model sit behind
  (`reader`, `extractor`) with their first adapters, the HTTP contract in
  `api/openapi.yaml`, and a development server that parses in process with
  nothing durable.
