---
title: "Agent-driven parsing: reader tiers, the text layer, reading a page again, and what an agent needs from the core"
status: vague
track: core
depends_on:
  - specs/002-object-model.md
  - specs/003-api.md
  - specs/008-readers.md
  - specs/009-intake.md
affects: [reader/, document/, internal/render/, internal/parse/, internal/config/, internal/httpapi/, api/]
effort: xlarge
created: 2026-10-03
updated: 2026-10-04
author: changkun
---

# Agent-driven parsing

## Overview

This spec is a problem statement and the shape of an answer. One part
of it is a design, and is built: the first tier of the reader ladder,
which reads a page from the text its file carries and calls no model
([The text layer tier](#the-text-layer-tier)). The rest is not a design
to build from yet.

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

Of what follows, the text layer tier and the questions it turned on
are decided, on 2026-10-04. Everything else is the shape proposed for
review.

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

### The text layer tier

The first tier is a reader of its own, the `text` adapter
([[008-readers]]), and nothing else changed to make room for it: a page
task is claimed for a reader as before, and the chain passes the page
down when this reader declines it. The renderer hands the reader what
the file holds of the page: its words, each with where it is drawn, the
size it is drawn at and what its font says of it, the upright
rectangles the page paints, and the boxes around everything else it
paints ([[009-intake]]). From that alone the reader builds blocks, in
the positions the file states. It reads nothing off the page's image,
and looks at it for one thing: that each word shows where the file
places it.

**What it builds.**

| Block | From what |
|---|---|
| a line | words on one baseline, from left to right; a raised or lowered mark in small type joins the line it stands beside |
| the order of reading | lines downward; where pieces of text stand side by side down a run of lines with a strip of clear paper between them, the left side before the right, when each side reads as prose |
| a paragraph | lines set in one type, close under each other, that begin where the paragraph's lines begin; a first line may be indented |
| a heading, with its level | a block set larger than the page's body type, or in bold where the body is not and of at most 3 lines; the levels follow the sizes, and the largest heading is a title when it is alone at its size and well above the body |
| a list item | a line that begins with a bullet, a number closed by a period or a parenthesis, a small mark the file paints before it, or the one character of a symbol font |
| a table | ruling that closes: a grid of lines whose outer frame is whole. A side with no line under it joins 2 cells into one that spans. The leading rows set in bold, or on a background, are its header |
| a figure | what the page paints that is not text, ruling or a background: images, curves, slanted lines, filled areas that hold no word. Shapes near each other are one figure, a frame or a background around them is its edge, and the words inside are its text. It has no description |
| a caption | a block that begins with a word and a number and stands directly above or under a table or a figure |
| a footnote | a block of type smaller than the body, under a separator in the lower half of the page, that begins with a note's mark |

A running header and a page number are text: a reader sees one page,
and assembly tells them from the pages around ([[010-assembly]]). A
formula is the characters the file holds, as text.

**What it declines.** A page it declines is read by the next reader, so
a wrong decline costs one call to that reader. A page it reads wrongly
costs the result. Every rule therefore leans toward declining, and the
reason is one fixed sentence that holds nothing the page says.

| A page is declined when | The measure |
|---|---|
| the file carries no text for it | the format is none that does, or the page's text could not be read within the renderer's bounds |
| its text is not whole | the renderer read up to its bound of characters or of painted objects ([[009-intake]]) |
| no text is drawn on it | every word is hidden: drawn with no fill and no stroke, as the text a recognition pass lays under a scan is. Hidden words are never read |
| a word is drawn at an angle | any word that does not run from left to right along the page: a watermark, a rotated table, a stamp in the margin |
| a word is in the file and not on the page | a word of 3 letters and digits or more whose place in the page's image, at the height of its small letters, is one flat color: it lies under a bar painted over it, or is drawn in the paper's color. The positions do not say what was painted last, and the image does |
| a character has no Unicode | any character the font maps to nothing or maps wrongly, a control character, a private one that is not a lone bullet before a line, a code Unicode leaves unassigned |
| the text reads as decoded twice | 2 or more of the pairs of characters that one character becomes when UTF-8 is read as a single-byte encoding |
| the text is not mostly text | under half of its characters are letters or digits |
| its words are not words | of 20 or more words of 4 letters or more in the basic Latin alphabet, not all capitals, over 15% hold no vowel |
| it is mostly picture | figures cover over 40% of the page, or the page paints over 2,000 shapes |
| a painted region is too full of text to be a figure | the words inside it cover over 30% of its area: a frame around a paragraph with a drawing in it cannot be told from a figure |
| a drawing lies over a table | a figure's region touches a table's, or a drawing lies inside one: a mark or a picture in a cell, where this reader's tables hold words alone |
| ruling does not close | lines make a grid whose frame has a gap, that runs past its last line, or whose cells join around a corner |
| a word lies across ruling | a word inside a table reaches past the lines of its cell |
| there is more ruling than a page of print holds | over 4,000 lines, or a grid of over 20,000 squares |
| text stands side by side and is neither prose nor a ruled table | one side of a gutter holds under 4 lines, or under half of them reach 85% of its widest, or they average under 3 words: the cells of a table set without ruling, a form, a list of names beside prose |
| a line may belong under the columns | a line on the left below the end of the right column, or on the right above the start of the left one, farther from its column than 1.5 times the column's pitch |

**The bounds.** Each is one named constant of `reader/text`, and all of
them, with a revision of the rules, are in the reader's version, so a
result of earlier rules is never taken for a result of these.

| Bound | Value | What it trades |
|---|---|---|
| `minLetters` | 0.5 | the least share of letters and digits. Raised, pages of dotted leaders and of notation are declined; lowered, pages whose glyphs map to symbols are read |
| `mojibakePairs` | 2 | the pairs a double decoding leaves. At 1, a page that quotes one such pair is declined |
| `maxVowelless`, `vowelSample` | 0.15, 20 | the share of plain words with no vowel, and the fewest words the share is judged on. Lowered, pages of identifiers are declined; raised, more pages of wrong letters are read |
| `flat`, `seenLength`, `seenBand` | 24 of 255, 3, 0.5 sizes | how little the samples of a word's place in the page's image may differ for the word to count as not drawn, the fewest letters a word holds to be looked for, and the height above its baseline it is looked for in. `flat` raised, pale type on a tint declines its page; lowered, the noise of an image hides a covered word. `seenBand` raised toward the line's height, a word under a bar that leaves paper above and below it counts as drawn |
| `maxRule` | 3 points | the thickest a rectangle may be and count as a line. Raised, a thin bar of a chart is ruling |
| `snap` | 1.5 points | how far apart 2 positions may lie and be one line of a table. Raised, 2 lines set tight become one; lowered, a line drawn as 2 strokes becomes 2 |
| `edgeCover` | 0.9 | the share of a cell's side that ruling must cover. Lowered, a dash of ruling splits a spanning cell; raised, a line that stops short of a corner joins 2 cells |
| `maxRules`, `maxCells`, `maxShapes` | 4,000, 20,000, 2,000 | what is read of one page's ruling and shapes: finding which touch compares every pair. Raised, a page of 50,000 strokes costs seconds before it is declined |
| `lineDrift` | 0.25 sizes | how far 2 baselines may differ on one line. Raised, a raised mark is set into its line out of place |
| `smallType` | 0.85 | the largest a mark may be, against the line beside it, to be that line's raised or lowered mark |
| `columnGap` | 2 sizes | the gap that parts a line into 2 pieces. Lowered, a loose line of justified prose is cut in 2 and its page declined; raised, 2 cells set close are one sentence |
| `bulletSize`, `bulletReach` | 0.8, 3 sizes | how large a painted mark may be, and how far before a line, to be its bullet |
| `clusterGap` | 0.1 of the width | how far apart 2 shapes may lie in one figure. Raised, 2 figures side by side are one; lowered, one chart is several |
| `minFigure` | 0.002 of the page | the least a painted region covers to be a figure and not an ornament. Raised, small figures are dropped without a trace; lowered, ornaments are figures |
| `maxFigures` | 0.4 of the page | the most figures may cover. Raised, pages of pictures come back as an empty figure and a few words; lowered, a page of text with one large chart costs a call |
| `maxFigureText` | 0.3 | the most of a figure's area its words may cover. The labels of a chart cover a tenth; a frame around a paragraph is half text |
| `minGutter` | 6 points | the narrowest clear strip that parts 2 pieces down a run of lines. Lowered, spaces that align by chance are a gutter |
| `sideBySide` | 0.3 | how much of the lower of 2 pieces lies at the height of the other for the 2 to stand side by side |
| `minColumnLines`, `minFullLines`, `fullLine`, `minColumnWords` | 4, 0.5, 0.85, 3 | what makes a side of a gutter a column of prose. Each lowered reads more tables as columns, which puts a column's cells before the next column's; each raised declines more pages of real columns |
| `maxStray` | 1.5 pitches | how far a line past the end of the other column may lie and be its own column running on |
| `maxLeading`, `pitchSlack`, `same` | 1.9 sizes, 1.25, 0.02 | how far apart 2 lines of a paragraph may stand; a paragraph also ends where the next line stands farther off than the paragraph stands from the one above |
| `indent` | 1 size | how far a line may begin from where its paragraph's lines begin |
| `headingScale`, `headingLines`, `titleScale` | 1.15, 3, 1.6 | how much larger than the body a heading is by size alone, how long one is by weight alone, and when the largest is a title |
| `captionReach` | 2 sizes | how far a caption may stand from what it names |

**What it costs and what it gives.** The reader alone, through the
durable server, on the files of the quality corpus that carry their
text ([`docs/quality.md`](../docs/quality.md)), against the bars of
each file's class:

| File | Class | Pages read | CER | Kinds | Cells | Order | Boxes |
|---|---|---:|---:|---:|---:|---:|---:|
| `survey.pdf` | `typeset` | 3 of 3 | 0.55% | 97.4% | 100% | 100% | 97.4% |
| `slides.pptx` | `converted` | 3 of 3 | 0.00% | 100% | 100% | 100% | - |
| `memo.rtf` | `converted` | 1 of 1 | 0.00% | 100% | 100% | 100% | - |
| `survey-scan.pdf` | `scan` | 0 of 3 | every page declined, as a scan is to be | | | | |
| the bars of `typeset` and `converted` | | | at most 2% | at least 90% | at least 95% | at least 95% | at least 90% |

The one block of the survey that is not right is its formula, which
comes back as text, and the formula is the whole of the character
error: 19 edits in 3,463 characters. No file's truth holds a box but
the survey's. A file of 300 pages that carries its text is parsed in
under 5 seconds on a laptop, 8 pages at once, each page's image
rendered at 160 dpi and stored with its result.

**What it loses.** It is first in a chain and not the chain:

- A figure is found and placed, with the words printed in it, and has
  no description. A request to describe a parse's figures fills it
  ([[003-api]]).
- A formula is text. Nothing in the positions says a line is one.
- A table set without ruling, or with lines under its rows and none
  between its columns, is declined with its page. So is a form.
- A page in columns is read when each column reads as prose, and
  declined when one does not: a short column, a column of names, a
  margin note. A paragraph that runs from the foot of one column to the
  head of the next is 2 blocks.
- A page with a watermark, a rotated table or a stamp is declined.
- A scan with a recognition layer is declined, however good the layer:
  its text is not what the page draws.
- A word broken at the end of a line keeps its hyphen, since whether
  the hyphen is the word's own is not known.
- A word under a flat shape, or in the paper's color, declines its
  page. A word under a picture, and one a clip cuts away over a ground
  that is not flat, cannot be told from a word that shows, and is read
  as the file holds it. Text an annotation or a form field shows is not
  in the page's text, and the renderer does not draw it either.

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

### Figures: the first use of a region

A layout engine finds a figure and says where it is. It does not say
what the figure shows: the specialized model used in the first runs is
trained to return a picture's box and no text. A general vision model
describes a figure well and places it loosely. Each does half.

The object model keeps the halves apart: a figure block has its box,
`text` for the words printed inside it, and `description` for what a
reader says it shows. The step between them is built
([[003-api]], [[008-readers]]):

1. **A block's image.** The page image is stored and a box is a
   fraction of it, so the image of any block is a crop:
   `GET /parses/{parse}/blocks/{ref}/image`. That alone is figure
   extraction.
2. **A describe step.** `POST /parses/{parse}/figures` cuts each figure
   from its page and gives it, with its caption, to a describer behind
   an interface of its own with a prompt of its own. The answer fills
   the block's `description`, its `figure.type`, and the labels printed
   in it.
3. **Where it runs.** As a request against a parse, the way extraction
   is, so figures can be described later, for some pages, without
   reading a page again.

In the first run of both together, a specialized model read two pages
of a typeset paper in 20 seconds and located a figure on each, and a
general vision model described the two crops in 16 seconds and 369
output tokens, naming each diagram's parts and how they connect.

Not built, on the same step: a chart's data as a table; a diagram as
vector graphics, which some specialized models return; and describing
figures on every parse, as an option of the routing policy, for
operators who want it without a second request.

This is the smallest real case of a region read by a second reader. It
settles the first open question below for one kind of block: a figure
is a region a second reader is given. It does not settle it for a
table or a paragraph read again, which need a page to have more than
one result.

### What to change before more is built on top

Each of these is cheap now and expensive once results are stored
durably and clients depend on them.

1. **A page can have more than one result.** Today a page read again
   replaces its blocks, and a block's ref is its page and its position,
   so a second reading renumbers the page and breaks every citation
   into it. A page needs a revision, or a block an identity that
   survives a second reading, and each block needs to say which reader
   and model produced it and how sure an engine was when it says.
   Default taken on 2026-10-04: a stored page result carries a
   revision, 1 for a page's first reading, so that reading a page again
   later needs no migration of what is stored. Refs and the contract do
   not expose it until this spec is a design.
2. **A reader can be handed the page's own text.** Built on
   2026-10-04: the renderer reads it, the page a reader receives
   carries it when the reader asks, and the `text` reader builds blocks
   from it with no model call. A chain names that reader first. Not
   built: a reader that calls a model asking for the text, to
   transcribe exactly.
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

These are the owner's to answer before this spec is drafted. 4 of the
7 are answered: 1 and 6 by a default, 4 and 5 by a decision. 2, 3 and 7
are open, and nothing is built for them.

1. Is a region a first-class unit a reader can be given, or is the page
   the smallest thing read, with a region only a crop of it? Default
   taken on 2026-10-04 so that the control plane can be built: the page
   is the smallest thing read, and a region is a crop of it handed to a
   second reader, as a figure is. It adds no resource to the contract.
2. Who owns the escalation policy: the operator's policy document, the
   caller per parse, a detector per page, or an agent per page? The
   table above assumes all four, layered.
3. Does provenance and confidence per block come back into the object
   model? It was removed because one source never set it.
4. Is reading a file's own text a reader in the chain, or a step of
   intake that the readers build on? Decided on 2026-10-04: a reader
   in the chain, an adapter of its own, `text`. Intake hands the text
   over and decides nothing. The scheduler, the task table and the
   store are untouched: a page task is claimed for a reader as it was,
   and the chain passes the page down when this reader cannot be the
   one to read it. So the tier is configuration: a Policy names it
   first, a parse that names a reader gets that reader alone, and a
   page that was read is kept under a key that names this reader's
   version as it names any reader's.
5. What does a multi-pass parse cost a tenant: is a page read 3
   times 3 pages? Decided on 2026-10-04 for the tier that is built:
   a page read from its own text is 1 page against a group's pages for
   a day, like any other page, whichever reader of the chain read it
   and however many declined it. It calls no model and records no
   token. What a page read again by a second reader costs stays open
   with the reading again itself.
6. Is there a single synchronous read, one image in and blocks out,
   for an agent that needs an answer inside a tool call? `Prefer: wait`
   covers it with two requests today. Default taken on 2026-10-04: no
   such route; `Prefer: wait` stays the way.
7. Does the tool surface ship as a command line, as a tool server, or
   both?

## Not in this spec

The agent itself: its definition, its loop, and its prompts belong to
whoever builds agents on the core. Storage: an agent gets a file to the
core as any caller does, by upload or by a link. Training or tuning a
model.

## Implementation status

Built:

- A block's image, and describing a parse's figures by a second reader
  on request.
- The text layer tier: the page's own text read by the renderer under
  bounds of its own (`internal/render`, [[009-intake]]), handed to a
  reader that asks (`internal/parse`), the `text` adapter that reads a
  page from it or declines it (`reader/text`, [[008-readers]]), its
  Reader document (`internal/config`), and the page source `text_layer`
  ([[002-object-model]]).

Nothing else of this spec is built. What exists and is relied on above:
a result addressed by page and block, read-time views, a parse that
names its reader and its pages, reuse of a page by what was read, and
the reader interface with a chain that passes a page down when a reader
cannot be the one to read it.

Known of the tier, and not built:

- The durable server's meter counts a call for every page a reader
  read or declined ([[013-limits-and-usage]] defines the count as model
  calls). A page the `text` reader read or declined is counted there,
  though no model was called. A page's own `usage` is right: one page
  and no token.
- A page the `text` reader declines in a parse that named it fails with
  the detail of a refusal, which names a model.
- The layout engine and the frontier tiers of the ladder are readers an
  operator configures, and the signals of the table above that move a
  page between them are not built.

## Acceptance criteria

The rows of the text layer tier are proven. The rest are to be written
when the open questions are answered; the last row is the one a draft
should carry.

| Criterion | Proven by |
|---|---|
| A 300-page file that carries its text is parsed with no model call, and a page of it that is an image is read by the next reader in the chain | `TestAFileOf300PagesThatCarriesItsTextIsParsedWithNoModelCall`, through the durable server over Postgres and an object store: a chain of the `text` reader and a reader whose model endpoint counts its calls reads a logbook of 300 pages, one of them a picture, with 1 call, and every other page says `text_layer`, names no model and counts no token. `TestAChainReadsAPageWithTheFirstReaderThatCan` holds the same in one process with a counting stub |
| A parse that names the `text` reader gets that reader alone, and a second parse takes what the first read | the same 2 tests |
| The typeset file of the quality corpus, read from its text alone, is within the bars of its class, and every page of the scanned one is declined | `TestTheTextReaderAloneReadsTheTypesetCorpus` in the gate, and the survey through the durable server in `TestAFileOf300PagesThatCarriesItsTextIsParsedWithNoModelCall` |
| Each rule of the table of declines declines a page built to meet it, with a fixed sentence, and each kind of block is built from a page set to hold it | the tests of `reader/text`, over pages built in the test |
| A page read again by a stronger reader keeps the citations made into its first reading | a test over the object model |
