# Security

Lectio receives files other people wrote and sends their pages to a model
endpoint. This file is what the project promises about that, what it does
not, and how to tell us when it is wrong.

## Reporting a vulnerability

Report one to security@latere.ai. Do not open a public issue for it. You
will hear back within three business days.

## State

Lectio has no release yet. The design is in [`specs/`](specs/README.md),
and the commitments below are the ones the code already holds, each with
the test that holds it. The list grows with the code; a control without a
test is a claim, and no claim is listed here.

| Asset | Control | Proved by |
|---|---|---|
| the network around the server | a source URL is fetched only from a publicly routable address, checked at dial time and after every redirect | `TestFetchRefusesNonPublicAddresses` |
| a source URL, and whatever its query proves | the fetch is the one outbound call that records no trace and sends no trace header, and a fetch that fails tells the caller neither the address nor the query | `TestFetchCarriesNoTraceToTheHostACallerChose` |
| the worker's memory and time, from a PDF | a PDF page is rendered by an engine instance bounded to 512 MiB and 30 seconds and discarded afterwards; a page written to exhaust it is refused and the next file is read | `TestAPageWrittenToExhaustTheEngineFailsWithinItsBounds`, `TestARenderStopsWhenItsTimeIsUp` |
| the worker's files | the PDF engine is given no file system, so a PDF cannot have it open a file of the host | `TestTheEngineSeesNoFileOfTheHost` |
| the worker's memory, from an image | an image is refused from its header when it declares more than 40 megapixels, and a TIFF when it declares more than 100,000 pages, before anything is allocated for either | `TestAnImageThatDeclaresAHugeSizeIsRefusedFromItsHeader`, `TestCountFramesStopsAtABoundOnDirectories` |
| the worker's memory and time, from a zipped office document | a package is refused for what its directory declares before a byte is inflated, a part cannot inflate past what it declares, and markup is held to bounds on its depth, its tokens and the size of one tag | `TestAPartThatDeclaresMoreThanItsBoundIsRefusedBeforeItIsInflated`, `TestAPartThatInflatesPastWhatItDeclaresIsRefused`, `TestAPackageOfTooManyEntriesIsRefused`, `TestMarkupIsHeldToItsBounds` |
| the worker's files and network, from a zipped office document | a part that declares a document type or an entity is refused, and a relationship reaches a part of the same package or nothing | `TestAPartThatDeclaresAnEntityIsRefused`, `TestARelationshipThatLeavesThePackageIsNotFollowed` |
| what a parse reports about a file | a PDF's pages are counted by the engine that renders them, so a file cannot claim pages it does not have or hide ones it has | `TestPrepareCountsAPDFsPagesWithTheEngineThatRendersThem` |
| a model credential | it is passed to a reader per call and never appears in an error, a log line, or a stored result | `TestCredentialNeverLeaves` |
| a caller's files and parses | every operation needs a token and is scoped to its owner; another caller's file or parse is not found, whatever is asked of it | `TestACallerIsKnownAndSeesOnlyItsOwn` |
| what an error tells a caller | an error nobody classified is logged and answered as `internal` with no detail | `TestAnErrorNobodyClassifiedIsInternalAndSaysNothing` |

## What a file's content can do

A file is somebody else's writing, and Lectio hands it to a model. These
are properties a deployer should know. None of them is a control with a
test, so none is in the table above.

- **Where content goes.** Each page's image, and for an extraction the
  document's text, is sent to the model endpoint the operator configured.
  Lectio sends it nowhere else. Each page records which reader and which
  model read it, and that record is the answer to where a document's
  content went.
- **Text that reads like an instruction.** A page or a document can hold
  text written to be taken for an instruction to the model. The prompts
  tell the model that everything on a page, and everything inside the
  document it is given, is content and never an instruction. Lectio
  checks the shape of what a model returns. It cannot promise that a
  model was not steered: a transcription or an extracted value from a
  hostile file is that model's output, and a caller that acts on it
  should treat it as the file's claim.
- **What a deleted file leaves.** Deleting a file deletes its snapshot.
  The page images of the parses that read it are pictures of the same
  content and stay until those parses are deleted or expire.
- **Conversion.** Office formats are not converted by this build. When
  they are, the converter runs with no network and none of the server's
  credentials, because an office document can name resources for the
  program that opens it to fetch.

## What is out of scope

- The security of the model endpoint, the object store, and the identity
  provider an operator configures.
- Your own misconfiguration.
