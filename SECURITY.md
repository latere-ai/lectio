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
| a model credential | it is passed to a reader per call and never appears in an error, a log line, or a stored result | `TestCredentialNeverLeaves` |
| a caller's files and parses | every operation needs a token and is scoped to its owner; another caller's file or parse is not found, whatever is asked of it | `TestACallerIsKnownAndSeesOnlyItsOwn` |
| what an error tells a caller | an error nobody classified is logged and answered as `internal` with no detail | `TestAnErrorNobodyClassifiedIsInternalAndSaysNothing` |

## What is out of scope

- The security of the model endpoint, the object store, and the identity
  provider an operator configures.
- The content of a file. Lectio parses what it is given and does not judge
  it; a page is sent to the configured model as it is.
- Your own misconfiguration.
