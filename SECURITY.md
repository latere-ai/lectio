# Security

Lectio receives files other people wrote and sends their pages to a model
endpoint. This file is what the project promises about that, what it does
not, and how to tell us when it is wrong.

## Reporting a vulnerability

Report one to security@latere.ai. Do not open a public issue for it. You
will hear back within three business days.

## State

Lectio publishes signed releases (see Supply chain below). The design is in
[`specs/`](specs/README.md), and the commitments below are the ones the code already holds, each with
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
| the worker's memory, from a workbook | the size a sheet declares and the count of strings a workbook declares are checked and never allocated for, and the cells and the text a workbook's tables may hold are bounded, so one long string named by many cells is refused | `TestWhatASheetDeclaresIsNeverAllocatedFor` |
| the worker's files and network, from a zipped office document | a part that declares a document type or an entity is refused, and a relationship reaches a part of the same package or nothing | `TestAPartThatDeclaresAnEntityIsRefused`, `TestARelationshipThatLeavesThePackageIsNotFollowed` |
| the server's credentials and network, from a file that is converted | conversion runs in a sidecar that holds no credential; its office suite is started with an environment that holds nothing of the sidecar's, in a scratch directory of its own, with a profile that turns macros and links off, and is killed with everything it started when it passes its time limit | `TestTheSuiteIsGivenNothingOfTheSidecar`, `TestAConversionPastItsTimeLimitIsKilledWithItsChildren` |
| where a file that is converted is sent | the server sends it to the sidecar it was configured with and follows no redirect from it | `TestTheClientMapsWhatTheSidecarAnswers` |
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
- **Conversion.** An office document can name resources for the program
  that opens it to fetch, so the sidecar that converts must have no
  network. It cannot take the network from itself: whoever runs it
  does, with a container that has none or a policy that denies it all
  traffic but the server's calls. A sidecar that is run with a network
  is not protected by anything in the table above against a file that
  names an address. The memory limit on its suite bounds address space;
  the limit on the memory its container holds is the container's own.

## Supply chain

A release is built from a tagged commit whose verify run passed, through
the shared release stages in
[latere-ai/ci](https://github.com/latere-ai/ci): both images, `lectiod`
and `lectio-convert`, are signed by the build stage's identity, run for
this repository, and the release carries the deploy archive, a bill of
materials for each image and `checksums.txt` over them, signed by the
publish stage's identity. To check a release, with `TAG` its tag:

```sh
gh release download "$TAG" -R latere-ai/lectio -D lectio-release
cd lectio-release
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository latere-ai/lectio checksums.txt
sha256sum -c checksums.txt

for image in lectiod lectio-convert; do
  cosign verify "ghcr.io/latere-ai/$image:$TAG" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
    --certificate-github-workflow-repository latere-ai/lectio
  gh attestation verify "oci://ghcr.io/latere-ai/$image:$TAG" -R latere-ai/lectio \
    --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
done
```

The release workflow runs the same checks against every release it
publishes.

Releases up to v0.8.2 were signed by this repository's own release
workflow. Check one of those with the same steps, the identity regexp
naming that workflow in both `cosign` commands, and no
`--signer-workflow`:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/latere-ai/lectio/\.github/workflows/release\.yml@' \
  checksums.txt
sha256sum -c checksums.txt

for image in lectiod lectio-convert; do
  cosign verify "ghcr.io/latere-ai/$image:$TAG" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github\.com/latere-ai/lectio/\.github/workflows/release\.yml@'
  gh attestation verify "oci://ghcr.io/latere-ai/$image:$TAG" -R latere-ai/lectio
done
```

## What is out of scope

- The security of the model endpoint, the object store, and the identity
  provider an operator configures.
- Your own misconfiguration.
