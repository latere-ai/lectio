---
title: "Agent-driven parsing: reader tiers, the text layer, reading a page again, and what an agent needs from the core"
status: vague
track: core
depends_on:
  - specs/002-object-model.md
  - specs/003-api.md
  - specs/008-readers.md
  - specs/009-intake.md
affects: [reader/, document/, internal/render/, internal/httpapi/, api/]
effort: xlarge
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Agent-driven parsing

## Overview

This spec is a problem statement and the shape of an answer. It is not
a design to build from yet.

The problem: an agent has files in storage, a batch of them, and has to
get structured information out. Sometimes the right move is to hand a
file to the parse API and read the result. Sometimes the agent should
work on the file itself: look at what it is, pick the pages that
matter, read those with a cheap reader, look again at the two pages it
does not trust with a stronger one, and put the outcome together by its
own judgment. Both have to work, against the same core, and the second
must not cost what a frontier model reading every page costs.

Two questions sit under it. Where is the line between a pipeline that
runs the same way every time and an agent that decides? And what is the
right grain of work, when a small model reads a page for a hundredth of
a cent and a frontier model reads it for ten cents?

## Current state

The core today reads every page of a file with one chain of readers.
What it already gives an agent is the part that is hard to add later:
a result addressed by page and by block, readable while the parse runs,
rendered in the form the reader of the result asks for, with a page
that was read once never read again. What it lacks is everything that
lets a caller choose how much to spend on which page.

Three facts from the first runs and the review shape what follows.

- Most business PDFs carry their own text. Rendering such a page to an
  image and sending it to a vision model costs four to five orders of
  magnitude more than reading the text that is already in the file,
  and the model can misread what the file states exactly. The core now
  renders with an engine that also exposes each page's text and the
  position of every character, so the text is there to take.
- A specialized small model gives exact boxes and good structure at a
  cost near zero when it runs on hardware the operator owns. A general
  vision model reads text and understands figures well and places boxes
  loosely. A frontier model is the best reader of a hard page and the
  worst value on an easy one.
- One call to a frontier model over a whole long file is not an
  option: the transcription of a few hundred pages is several times
  the output any model returns in one reply, and it comes back with no
  positions. A large model earns its cost on the pages and the
  questions that need it, working over text the cheaper tiers produced.

## Design

Nothing here is decided. This is the shape proposed for review.

### Two modes, one surface

**Delegated.** The agent submits the file and reads the result: the
document, pages, blocks, chunks, fields. This is the API as it stands.

**Agent-driven.** The agent works in steps and decides between them. It
asks what the file is, without a model call. It parses a selection of
pages with the reader it names. It reads results as they arrive. It
looks at a page image, searches the parsed text, and reads a page or a
region again with a stronger reader when what came back does not hold
up. It extracts with its own schema over the pages it chose.

Both modes use the same objects and the same routes. The second needs a
few operations the first does not, and no second service.

### What an agent calls

| Operation | Today | Needed |
|---|---|---|
| What is this file: its pages, each page's size, whether it carries text | no | a read on a file that calls no model |
| The text a page carries, with positions | no | the same |
| A page's image, whole or cropped to a box | whole, after a parse | before a parse, and by region |
| Parse these pages with this reader | yes: `pages`, `reader` | |
| Read a page, a block, the outline, every block, chunks | yes | |
| Search within a parse | no | a text search over blocks, answering refs |
| Read this page, or this region, again with another reader | no | needs a page to have more than one result |
| Extract with a schema over a range of pages | planned, whole document | a page range |
| Submit many files and follow them as one | labels only | a batch that can be canceled, bounded and watched as one |

The agent reaches these through the HTTP contract. A thin command line
and a tool description over the same contract are what goes into a
sandbox image; they hold no logic of their own.

### Where the line sits

The core stays a pipeline: it renders, queues, shares model capacity
between tenants, holds the model credentials, retries, and keeps
addressable results. It runs the same way every time, and its ladder of
readers is configuration.

The agent is the judgment: which pages matter, when a result is good
enough, what to ask for. It is defined outside the core and calls it.

An agent calls the core from its sandbox. The core does not run inside
the agent's sandbox: the model credential, the fair queue and the
breaker are properties of a service that many callers share, and a
sandbox is where code runs that must never see a credential.

One part of the core does belong in a sandbox of its own: opening files
that other people wrote. Rendering and converting are where a hostile
file meets a large parser. Running those per file in an isolated
process, behind the renderer interface the core already has, is the
hybrid worth building. It is a hardening of the core and independent of
agents.

### Reader tiers

A page can be read at four costs. Each is a reader behind the same
interface, and the routing policy's chain is the ladder.

| Tier | What reads the page | Cost per page, rough | Gives |
|---|---|---|---|
| text layer | the file's own text and positions, no model | none, milliseconds | exact characters, positions, weak structure |
| layout engine | a small specialized model on the operator's hardware, or an OCR service | none to a fraction of a cent | exact boxes, kinds, tables, formulas |
| vision model | a fast general model through a gateway | a few cents | text, understanding of figures, loose boxes |
| frontier model | the strongest model, on a page or a region | about ten cents | the best reading of a hard page |

For a 300-page file that carries its text, at list prices of late 2026
and about 11,000 input and 2,500 output tokens per page image: the text
layer costs nothing, a layout service about one dollar, a fast vision
model about twelve dollars, a frontier model about twenty-eight. The
ratios are the point, not the cents.

What moves a page up the ladder has to be something observable, since a
general model reports no confidence.

| From | To | Signal |
|---|---|---|
| text layer | layout engine | the page has no text, or its text covers little of what is drawn on it |
| layout engine | vision model | the engine's text disagrees with the text layer; its boxes leave drawn regions of the page uncovered; it reports low confidence |
| vision model | frontier model | no blocks on a page that is not blank; a table whose cells do not fit its rows and columns; a reply cut at its output limit; an extraction that failed validation and cites the page |
| any | any | the caller, or an agent, asks for it |

Who decides is layered: the policy sets the default ladder, a parse may
name its reader, and an agent may ask for one page or region to be read
again. The first two exist. The third is new.

### Grain

The page stays the unit of scheduling, retry, storage and addressing.
Two additions make finer and coarser work possible without disturbing
it.

- **Finer: a region.** Reading a table or a signature block again at a
  higher resolution with a stronger reader is cheaper and better than
  reading the whole page again. A region is a box on a page.
- **Coarser: a question over text.** Extraction, classification and
  reasoning run over the text the readers produced, by reference to
  blocks, and can span pages. This is where a frontier model is worth
  its price, and it reads text, not images.

### What to change before more is built on top

Each of these is cheap now and expensive once results are stored
durably and clients depend on them.

1. **A page can have more than one result.** Today a page read again
   replaces its blocks, and a block's ref is its page and its position,
   so a second reading renumbers the page and breaks every citation
   into it. A page needs a revision, or a block an identity that
   survives a second reading, and each block needs to say which reader
   and model produced it and how sure an engine was when it says.
2. **A reader can be handed the page's own text.** The renderer
   extracts it; the page a reader receives carries it; one reader
   builds blocks from it with no model call; a vision reader may use it
   to transcribe exactly. The chain tries the text reader first.
3. **A cost unit per reader.** A page read from its text layer and a
   page read by a frontier model are not the same unit of fairness or
   of billing. The control plane's cost is now designed per reader
   ([[006-fairness-and-priority]]); usage should report it the same
   way.
4. **A batch.** One id over many parses, with a deadline, a cancel and
   a progress of its own.

Already in place: page results are kept by what was read and not by who
asked, so an agent that reads pages 1 to 3 and later 1 to 10 pays for
seven.

## Open questions

These are the owner's to answer before this spec is drafted.

1. Is a region a first-class unit a reader can be given, or is the page
   the smallest thing read, with a region only a crop of it?
2. Who owns the escalation policy: the operator's policy document, the
   caller per parse, a detector per page, or an agent per page? The
   table above assumes all four, layered.
3. Does provenance and confidence per block come back into the object
   model? It was removed because one source never set it.
4. Is reading a file's own text a reader in the chain, or a step of
   intake that the readers build on?
5. What does a multi-pass parse cost a tenant: is a page read three
   times three pages?
6. Is there a single synchronous read, one image in and blocks out,
   for an agent that needs an answer inside a tool call? `Prefer: wait`
   covers it with two requests today.
7. Does the tool surface ship as a command line, as a tool server, or
   both?

## Not in this spec

The agent itself: its definition, its loop, and its prompts belong to
whoever builds agents on the core. Storage: an agent gets a file to the
core as any caller does, by upload or by a link. Training or tuning a
model.

## Implementation status

Nothing of this spec is built. What exists and is relied on above: a
result addressed by page and block, read-time views, a parse that names
its reader and its pages, reuse of a page by what was read, the reader
interface with a chain that passes a page down when a reader cannot be
the one to read it, and a renderer that can expose a page's text.

## Acceptance criteria

To be written when the open questions are answered. The first two a
draft should carry:

| Criterion | Proven by |
|---|---|
| A 300-page file that carries its text is parsed with no model call, and a page of it that is an image is read by the next reader in the chain | an end-to-end test with a counting stub |
| A page read again by a stronger reader keeps the citations made into its first reading | a test over the object model |
