# Changelog

What changed for whoever runs `lectiod`, calls its API, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- Added: the repository scaffold. The object model (`document`); the
  interfaces a page-reading model and a field-extracting model sit behind
  (`reader`), with adapters for an OpenAI-compatible chat endpoint, for a
  layout engine behind HTTP, and a stub that calls nothing; intake for
  images, text, Markdown and CSV; assembly with Markdown, text and chunk
  views chosen when a result is read; and the HTTP contract in
  `api/openapi.yaml`.
- Added: `lectiod` with `LECTIO_DEV=true`, a development server that
  serves the whole contract in one process and keeps nothing. Routes the
  contract marks planned answer `501 not_implemented`.
