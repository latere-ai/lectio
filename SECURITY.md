# Security

Lectio receives files other people wrote, fetches addresses its callers
name, and sends pages to a model endpoint. This file is what the project
promises about that, what it does not, and how to tell us when it is
wrong. [`docs/security.md`](docs/security.md) says the same for the
people who run it and build on it.

## Reporting a vulnerability

Report one privately, through
[GitHub's private vulnerability reporting](https://github.com/latere-ai/lectio/security/advisories/new)
or by email to security@latere.ai. Do not open a public issue for it. The
[organization's security policy](https://github.com/latere-ai/.github/blob/main/SECURITY.md)
sets the response times, how disclosure works, and the safe harbor for
research done in good faith. Credit in the release notes on request.

Fixes ship in the next release of the current major line, as the
organization's policy on
[supported versions](https://github.com/latere-ai/.github/blob/main/SECURITY.md#supported-versions)
says; the [releases page](https://github.com/latere-ai/lectio/releases)
lists them.

The reports we most want: a caller that reads or changes another
caller's file or parse; a request that acts after a deny, or while the
authorizer does not answer; a fetch that reaches a private address; a
file that makes a worker exceed the bounds below, read a file of the
host, or reach the network; and a model key or a bearer that shows in
an answer, a log line, a row or an object. What
[is not defended](#what-is-not-defended) is not a vulnerability, but a
report that one of those limits is wider than written here is.

## What is protected

Lectio is attacked from four places: a caller with a token of its own, a
file written to harm the worker that reads it, an address a caller
names, and a reader of what the server answers, logs and stores. Each
commitment below is held by the named tests, which run on every change.
Tests marked (unix) build on Unix systems only; tests marked (container)
run against Postgres in a container and skip where no container runtime
answers. A number in a row is a bound fixed in the code; the tests show
what happens past it, not its exact value.

| Against | What Lectio commits to | Held by |
|---|---|---|
| a caller with a token of another owner | every operation but the contract at `GET /v1/openapi.yaml` and the `/healthz` probe needs a token, which is refused when its issuer is not listed, its audience is another, it has expired, or its signature does not check; each operation asks the authorizer, or the owner policy without one, before it acts; under the owner policy another caller's file or parse is not found, whatever is asked of it | `TestWhatTheVerifierRefuses`, `TestEveryRouteAsksItsAction`, `TestACallerIsKnownAndSeesOnlyItsOwn`, `TestTheOwnerPolicyThroughTheAPI` |
| a caller when the authorizer denies or cannot answer | the request is answered before anything is done; an authorizer that allows the reserved probe stops the start | `TestADenyAndAnOutageAnswerBeforeAnythingIsDone`, `TestTheClientFailsClosed`, `TestCheckReadsTheProbesAnswer`, `TestTheModesOfADevelopmentServer` |
| a caller on another machine, against a development server | the development server listens on `127.0.0.1:8080` unless `LECTIO_ADDR` names another address, so its static token is reachable from its own machine alone; the image sets `LECTIO_ADDR=:8080`, every interface of its container, which is reached only as its port is published | `TestTheDevelopmentServerListensOnTheLoopbackAddress`, `TestTheImagesAreWhatTheManifestsRun` |
| a caller held to limits | an allow lowers the server's limits on a file's size, a parse's pages and the retention, and never raises them; a limit the server cannot hold, such as a cap on the queue in the development server, refuses the request and names the limit | `TestEachLimitOfASubmit`, `TestALimitTheServerDoesNotEnforceIsRefused` |
| the network around the server | a source URL is fetched over `https` only, from a publicly routable address, checked at dial time and after every redirect; a host listed in `LECTIO_FETCH_ALLOW` is fetched whatever it resolves to, and a redirect from it to another host is checked like any connection; a URL that carries a user and a password is refused | `TestFetchRefusesNonPublicAddresses`, `TestFetchFailures` |
| a source URL, and whatever its query proves | the fetch sends no trace header to the host, and a fetch that fails tells the caller neither the address nor the query | `TestFetchCarriesNoTraceToTheHostACallerChose` |
| the worker's memory and time, from a PDF | a PDF page is rendered by an engine instance bounded to 512 MiB and 30 seconds; a page written to exhaust it fails, and the next file is read | `TestAPageWrittenToExhaustTheEngineFailsWithinItsBounds`, `TestARenderStopsWhenItsTimeIsUp` |
| the worker's files | the PDF engine is given no file system, so a PDF cannot have it open a file of the host | `TestTheEngineSeesNoFileOfTheHost` |
| the worker's memory, from an image | an image is refused from its header when it declares more than 40 megapixels, a TIFF frame when it does, and a TIFF when it declares more than 100,000 pages, before anything is allocated for either | `TestAnImageThatDeclaresAHugeSizeIsRefusedFromItsHeader`, `TestFramePNGRejectsOversizeFrame`, `TestCountFramesStopsAtABoundOnDirectories` |
| the worker's memory and time, from a zipped office document | a package is refused for what its directory declares before a byte is inflated, a part cannot inflate past what it declares, and markup is held to bounds on its depth, its tokens and the size of one tag | `TestAPartThatDeclaresMoreThanItsBoundIsRefusedBeforeItIsInflated`, `TestAPartThatInflatesPastWhatItDeclaresIsRefused`, `TestAPackageOfTooManyEntriesIsRefused`, `TestMarkupIsHeldToItsBounds` |
| the worker's memory, from a workbook | the size a sheet declares and the count of strings a workbook declares are checked and never allocated for, and the cells and the text a workbook's tables may hold are bounded, so one long string named by many cells is refused | `TestWhatASheetDeclaresIsNeverAllocatedFor`, `TestAWorkbookIsHeldToItsBounds` |
| the worker's files and network, from a zipped office document | a part that declares a document type or an entity is refused, and a relationship reaches a part of the same package or nothing | `TestAPartThatDeclaresAnEntityIsRefused`, `TestARelationshipThatLeavesThePackageIsNotFollowed` |
| the server's credentials and network, from a file that is converted | conversion runs in a sidecar; its office suite is started with an environment that holds nothing of the sidecar's, in a scratch directory of its own, with a profile that turns macros and links off, and is killed with everything it started when it passes its time limit; the shipped manifests give the sidecar no egress, admit only a worker on its one port, and give it no Secret, no ConfigMap, no token and no address of another service | `TestTheSuiteIsGivenNothingOfTheSidecar` (unix), `TestAConversionPastItsTimeLimitIsKilledWithItsChildren` (unix), `TestTheConverterHasNoNetworkAndNoCredential` |
| where a file that is converted is sent | the server sends it to the sidecar it was configured with and follows no redirect from it | `TestTheClientMapsWhatTheSidecarAnswers` |
| what a parse reports about a file | a PDF's pages are counted by the engine that renders them, so a file cannot claim pages it does not have or hide ones it has | `TestPrepareCountsAPDFsPagesWithTheEngineThatRendersThem` |
| a model key | a key is passed to a reader per call, prints as nothing, and is in no error, log line or JSON encoding; with a key endpoint, the API process reads neither the endpoint nor its bearer, the request to the endpoint names the group, the owner, the parse and the models and nothing of the file, and neither a key nor the bearer is in a log, a row, an object or an answer | `TestCredentialNeverLeaves`, `TestTheAPIReadsNeitherTheEndpointNorItsBearer`, `TestTheRequestToTheEndpointIsExact`, `TestAKeyIsAskedForTheModelsOfTheReaders`, `TestAnEndpointSourcePrintsNothingItHolds`, `TestPagesOfTwoGroupsAreReadWithTwoKeys` (container), `TestExtractionsAndFiguresOfTwoGroupsAreReadWithTwoKeys` (container) |
| what an error tells a caller | an error nobody classified is logged and answered as `internal` with no detail | `TestAnErrorNobodyClassifiedIsInternalAndSaysNothing` |
| what is kept past its time | a file and a parse past their retention are removed, the objects before the rows that name them, and a delete that stopped halfway is finished by the sweep | `TestTheSweepRemovesObjectsAndThenRows`, `TestAParseIsKeptForItsRetentionAndThenListed` (container), `TestADeleteThatStoppedHalfwayIsFinishedByTheSweep` (container) |

## What is not defended

- **Text in a file that reads like an instruction.** A page or a
  document can hold text written to be taken for an instruction to the
  model. The page, figure and extraction prompts tell the model that
  such text is content and never an instruction, and Lectio checks the
  shape of what a model returns. It cannot promise that a model was not
  steered: a transcription or an extracted value from a hostile file is
  that model's output, and a caller that acts on it should treat it as
  the file's claim.
- **Where content goes once it is sent.** Each page's image, a figure's
  cut-out, and for an extraction the document's text go to the model
  endpoint the reader names; a file that is converted goes to the
  sidecar. What the model endpoint keeps is the endpoint's. Each page
  records which reader and which model read it.
- **What a deleted file leaves.** Deleting a file removes its stored
  copy. The parses that read it keep their page text, page images,
  document, fields, figures and working copy until each is deleted or
  its retention passes.
- **A sidecar run with a network.** An office document can name
  resources for the program that opens it to fetch. The sidecar cannot
  take the network from itself: whoever runs it does, with a container
  that has none or a policy that denies it all traffic but the workers'
  calls, as the shipped manifests do. The memory limit on its suite
  bounds address space; the memory its container may hold is the
  container's own limit.
- **A development server moved to a public address.** The development
  server accepts the static token `dev` unless issuers are listed. It
  listens on the loopback address alone, but nothing stops an operator
  from setting `LECTIO_ADDR` to an address others can reach, or from
  publishing the image's port beyond the machine.
- **Hosts listed in `LECTIO_FETCH_ALLOW`.** A fetch to one of them skips
  the public-address check, so any caller can have Lectio read what that
  host serves.
- **Subjects listed in `LECTIO_ADMIN_SUBJECTS`.** Under the owner policy
  they read and list every owner's files and parses.
- **Plain HTTP.** `lectiod` serves plain HTTP; a proxy or the cluster in
  front of it provides TLS. `LECTIO_OIDC_ISSUERS` and the authorizer's
  address accept `http`, and a token is trusted as far as the keys it is
  checked against. The internal listener of a durable process,
  `LECTIO_INTERNAL_ADDR`, serves its probes and `/version` to anyone who
  can reach it.
- **An authorizer that answers wrongly,** or an issuer that signs a token
  for the wrong subject. Lectio refuses when no decision arrives, and
  follows the decision that does.
- **The services an operator configures:** the model endpoint, the
  bucket, Postgres, the issuer and the key endpoint, and a host that is
  compromised.

## Audit

Lectio has had no external security audit. The commitments above are
held by tests in this repository, which run on every change.

## Supply chain

Dependencies are checked for known vulnerabilities on every push that
changes the code or its dependencies. A release is built from a tagged
commit whose verify run passed, through the shared release stages in
[latere-ai/ci](https://github.com/latere-ai/ci): both images, `lectiod`
and `lectio-convert`, are signed by the build stage's identity, run for
this repository, and the release carries the deploy archive, a bill of
materials for each image and `checksums.txt` over them, signed by the
publish stage's identity.

### Verifying a release

With `TAG` the release's tag:

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
