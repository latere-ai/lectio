---
title: "Sources and retention: uploads, fetching by URL, the snapshot, origin, and when files and results are deleted"
status: validated
track: core
depends_on:
  - specs/002-object-model.md
  - specs/003-api.md
  - specs/004-durable-tasks.md
affects: [internal/fetch/, internal/httpapi/, internal/store/, internal/objects/]
effort: medium
created: 2026-10-03
updated: 2026-10-04
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
  ([[003-api]]), as the body itself or as the part named `file` of a
  multipart form. An upload whose SHA-256 matches a file the same owner
  already has returns that File with `200`.
- **`{"url": "https://..."}`**: fetched by `prepare`. `https` only by
  default; redirects followed up to five; the size limit enforced while
  streaming; the connection refused, at dial time and after every
  redirect, when the address is loopback, link-local, private or
  otherwise not publicly routable, unless the host is in
  `LECTIO_FETCH_ALLOW`. A fetch that fails is `source_unreachable` on
  the parse.

### The address check

The caller chooses the URL, so without a check the server would
connect wherever it is told: to its own loopback, to a metadata
endpoint, to a host on its private network. The check is at the
socket. Every connection a fetch opens, the first and the one after
each redirect, is checked against the address it is about to connect
to, after the name was resolved. A name that resolves to a private
address and a redirect to one are therefore both refused, and resolving
twice cannot get an address past the check.

- Refused: loopback, private, link-local, multicast and unspecified
  addresses, the shared address space of carrier NAT, the ranges set
  aside for documentation, benchmarking and protocol assignments, and
  the IPv6 forms that carry an IPv4 address inside them (IPv4-mapped,
  NAT64, 6to4), each judged as the IPv4 address it is.
- No proxy is used: behind one, the socket's address would be the
  proxy's and the check would pass for any destination.
- One connection per fetch: a kept connection would carry the next
  request with no check of its own.
- A URL with a user or a password, or with a scheme other than `https`,
  is refused before any connection.
- A host in `LECTIO_FETCH_ALLOW`, named alone or with its port, is
  fetched whatever it resolves to. That is how an operator's own object
  store on a private network is reached. The allowance is the host's
  own: a redirect from it to another address is checked like any
  connection.
- What the caller is told names no address. The transport's error may
  hold the address or a token in the URL's query, so it is kept out of
  the answer.
- The fetch carries no trace. It is the one outbound call that is not
  instrumented: a tracing transport records the URL it requests, and
  what a short-lived download link proves is in its query, so a span
  would hand the link to whoever reads traces. It would also send this
  server's trace headers to a host a caller chose. What a trace keeps
  of a fetch is its host and how it ended, never its URL.

The response's file name, from the URL's path or `Content-Disposition`,
and its `Content-Type` are hints for telling the file's type, as a
caller's own would be ([[009-intake]]).

A URL is how Lectio reads from a storage service without knowing one.
The caller asks its storage for a short-lived download link to the
file, which proves the caller may read it, and submits the link. No
token crosses from one service to another, Lectio needs no client for
any particular store, and the link only has to live until `prepare`
runs. For a parse that waits in a queue longer than the link lives,
the fix is ordering, not a longer link: `prepare` is ordered ahead of
a parse's pages ([[006-fairness-and-priority]]), so it is claimed in
seconds even when the tenant's pages wait for an hour.

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

Deleting a File deletes the snapshot and nothing a parse wrote. The
page images of the parses that read it are pictures of the same
content, and they stay, with the page results and the document, until
each parse is deleted or its retention ends. A caller that wants a
file's content gone deletes its parses too: `GET /parses?file=` lists
them.

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

## Implementation status

Built:

- Uploads, in `internal/httpapi`: a raw body or a multipart form, the
  size limit, type detection, and the same bytes being one File per
  owner.
- `internal/fetch`: the fetcher with the address check above, the
  redirect bound, the size limit while streaming, the allowed hosts,
  and no trace.
- `origin` stored, returned and filterable, and `DELETE` of a file and
  of a parse, with a file that a parse which has not ended reads being
  refused.

Remaining:

- A URL is fetched inside the submit request, and a fetch that fails
  is `422 source_unreachable` on the submit. Fetching in `prepare`,
  where it fails the parse, comes with the durable tasks
  ([[004-durable-tasks]]).
- The snapshot in an object store. Files are held in memory, keyed by
  owner and content hash.
- Retention: `expires_at`, `?retain=`, every row of the retention
  table, and the sweep. Nothing expires; a restart deletes everything.
- `output.images` and `LECTIO_KEEP_PAGE_IMAGES`: every page image is
  kept. Whether a submit option to drop page images still belongs in
  the contract, now that a submit carries no output options
  ([[003-api]]), is open.

## Acceptance criteria

| Criterion | Proven by |
|---|---|
| The fetcher refuses loopback, private, link-local and metadata addresses at dial time, including after a redirect and for a name that resolves to one | tests with a resolver and redirect fixture |
| A URL whose body exceeds the limit is stopped at the limit, not after | a streaming test |
| A fetch sends no trace header to the host, and a fetch that fails tells the caller neither the address nor anything from the URL's query | `TestFetchCarriesNoTraceToTheHostACallerChose` |
| A host in the allow list that redirects to an address outside it is refused | `TestFetchRefusesNonPublicAddresses` |
| Deleting a File leaves the page images of the parses that read it, and deleting those parses removes them | a store test |
| The same bytes uploaded twice by one owner are one File; by two owners, two Files with different keys | a store test |
| A parse submitted with a link that expires in 60 seconds, behind a 10-minute backlog of the same group's pages, succeeds | a dispatch test with a virtual clock |
| After the retention period, the sweep leaves no object and no row for an expired File or parse, and an interrupted sweep completes on the next run | a store test over the S3 implementation |
| `DELETE` of a parse removes every object under its prefix | a store test listing the prefix |
| `GET /parses?origin.path=...` returns the parses submitted with that origin and no others | an API test |
