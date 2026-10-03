---
title: "Sources and retention: uploads, fetching by URL, the snapshot, origin, and when files and results are deleted"
status: drafted
track: core
depends_on:
  - specs/002-object-model.md
  - specs/003-api.md
  - specs/004-durable-tasks.md
affects: [internal/sources/, internal/objects/, internal/store/]
effort: medium
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Sources and retention

## Overview

Where a file comes from, what Lectio keeps of it, and for how long. A
caller either uploads the bytes or gives a URL. Either way Lectio takes
a snapshot before any work, so a parse reads bytes that cannot change
or disappear under it, and a worker needs nobody's permission later.
Everything Lectio stores has a retention period and is deleted when it
ends.

## Current state

The earlier service had uploads deduplicated by content hash, a
URL fetcher with a guard against requests to internal addresses, and a
reference type for a file service that no longer exists. Uploads kept
a 24-hour expiry that was checked on read and never swept, so bytes and
rows outlived it; parses and their results were never deleted. The
upload path and the fetcher are carried over. The dead reference type
is dropped, and deletion becomes real.

## Design

### Two sources

- **`{"file": "fil_..."}`**: bytes uploaded with `POST /files`
  ([[003-api]]). An upload whose SHA-256 matches a file the same owner
  already has returns that File.
- **`{"url": "https://..."}`**: fetched by `prepare`. `https` only by
  default; redirects followed up to five; the size limit enforced while
  streaming; the connection refused, at dial time and after every
  redirect, when the address is loopback, link-local, private or
  otherwise not publicly routable, unless the host is in
  `LECTIO_FETCH_ALLOW`. A fetch that fails is `source_unreachable` on
  the parse.

A URL is how Lectio reads from a storage service without knowing one.
The caller asks its storage for a short-lived download link to the
file, which proves the caller may read it, and submits the link. No
token crosses from one service to another, Lectio needs no client for
any particular store, and the link only has to live until `prepare`
runs. For a parse that waits in a queue longer than the link lives,
the fix is ordering, not a longer link: `prepare` charges nothing and
runs ahead of pages ([[006-fairness-and-priority]]), so it is claimed
in seconds even when the tenant's pages wait for an hour.

### Origin

`origin` on a submit ([[003-api]]) records where the file lives for the
caller: a store name, a path, a version. Lectio stores it, returns it,
and lets `GET /parses?origin.path=` filter on it. It is the link a
file browser uses to show that a file has been parsed and to open the
result. Lectio never dereferences it.

### The snapshot

The source is stored once per owner under its content hash:
`sources/<owner key>/<sha256>`, where the owner key is a hash of the
owner string, so one tenant's upload of a file is never served to
another and the key reveals neither. The File row holds the size, the
hash, the detected media type, the name, and `expires_at`. Parses refer
to the File. Reuse of results ([[005-parse-graph]]) keys on the hash,
so the same bytes arriving by upload and by URL are one piece of work.

### Retention

| What | Default | Set by |
|---|---|---|
| a File and its snapshot | 24 hours after the last parse that used it settled | `LECTIO_FILE_RETENTION`; `?retain=<duration>` on upload, capped by the server |
| a parse, its page images, page results, document, renderings, fields | 30 days after it settled | `LECTIO_PARSE_RETENTION`, lowered by `Retention` on the allow |
| the working copy and the worker's cache entry | until the parse settles | fixed |
| idempotency keys | 24 hours | fixed |
| settled task rows | 7 days | [[004-durable-tasks]] |
| usage detail | 35 days; aggregates kept | [[013-limits-and-usage]] |

A retention sweep, run by any worker like the other sweeps, deletes
what has expired: objects first, then the row, so a crash between the
two leaves a row that is swept again and never an object nothing
points to. `DELETE /parses/{parse}` and `DELETE /files/{file}` do the
same on demand, and a File that a non-terminal parse uses is refused
with `409 not_terminal`.

Page images are the largest thing a parse stores. `output.images:
false` on a submit, or `LECTIO_KEEP_PAGE_IMAGES=false`, deletes each
image once its page has been read, at the cost of the image route
returning `404` and overlays needing the caller's own render.

### Encryption and location

Lectio writes to the bucket it is configured with and relies on that
bucket's encryption at rest. It adds none of its own. Where the bucket
is and which model endpoint pages are sent to are the operator's
configuration, and the page result records the reader and model that
saw each page ([[015-observability]]), which is the record a data
residency question is answered from.

## Not in this spec

Reading directly from a named storage service with a delegated
credential. Watching a store and parsing new files as they arrive.
Writing results back into a caller's storage. Each can be built on
this spec from outside Lectio, with a link in and the API out.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| The fetcher refuses loopback, private, link-local and metadata addresses at dial time, including after a redirect and for a name that resolves to one | tests with a resolver and redirect fixture |
| A URL whose body exceeds the limit is stopped at the limit, not after | a streaming test |
| The same bytes uploaded twice by one owner are one File; by two owners, two Files with different keys | a store test |
| A parse submitted with a link that expires in 60 seconds, behind a 10-minute backlog of the same group's pages, succeeds | a dispatch test with a virtual clock |
| After the retention period, the sweep leaves no object and no row for an expired File or parse, and an interrupted sweep completes on the next run | a store test over the S3 implementation |
| `DELETE` of a parse removes every object under its prefix | a store test listing the prefix |
| `GET /parses?origin.path=...` returns the parses submitted with that origin and no others | an API test |
