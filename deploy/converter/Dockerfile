# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# The conversion sidecar: an office suite and the one binary that runs it,
# and nothing else. Build it from the repository root:
#
#   podman build -f deploy/converter/Dockerfile -t lectio-convert .
#
# The image holds no credential and expects none, and it runs as a user
# that is not root. It does not deny itself the network: only whoever runs
# the container can. Run it with no network and a socket on a volume it
# shares with the server:
#
#   podman run --network none -v lectio-convert:/run/lectio \
#     -e LECTIO_CONVERT_ADDR=unix:/run/lectio/convert.sock lectio-convert
#
# or, where the server reaches it over a network, under a policy that
# denies it every connection but the server's calls. specs/009-intake.md
# says why.

FROM docker.io/library/golang:1.27 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w" \
    -o /out/lectio-convert ./cmd/lectio-convert

# The suite without its graphical front end: the word processor and the
# presentation program, which are what the conversions need. With it,
# the fonts a converted page is drawn with: the metric-compatible families,
# so a page breaks where its author saw it break, and a CJK family, so text
# in those scripts is not drawn as empty boxes. The directory for the
# socket belongs to the user the sidecar runs as, and a volume mounted
# there for the first time takes that owner.
FROM docker.io/library/debian:trixie-slim AS suite
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      libreoffice-writer-nogui \
      libreoffice-impress-nogui \
      fonts-dejavu-core \
      fonts-liberation \
      fonts-crosextra-carlito \
      fonts-crosextra-caladea \
      fonts-noto-cjk \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 65532 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin nonroot \
 && install -d -o 65532 -g 65532 -m 0750 /run/lectio

FROM suite
COPY --from=build /out/lectio-convert /usr/local/bin/lectio-convert
# The suite's program itself and not the script that starts it: one program
# fewer runs on a file a caller chose, and the sidecar handles the one
# thing the script does, starting the suite again after its first start.
ENV LECTIO_CONVERT_SUITE=/usr/lib/libreoffice/program/soffice.bin
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/lectio-convert"]
