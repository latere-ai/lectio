# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

GO ?= go

.PHONY: build check clean fmt hooks live openapi run specs

# The whole bar. Every gate lives in latere.ai/x/ci-gate, pinned as a tool
# in go.mod and configured in .lateregate.yaml, so this target is a name for
# `go tool lateregate` and nothing else. One gate at a time: `go tool
# lateregate cover`. The plan: `go tool lateregate list`.
check:
	@$(GO) tool lateregate

.DEFAULT_GOAL := check

OUT_DIR := out
SERVICE := lectiod
MODULE := $(shell $(GO) list -m)

# Build metadata, deferred so the git and date calls run only for a build. A
# dirty tree marks the commit, because a binary built from uncommitted
# changes cannot be reproduced from its commit.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DIRTY = $(shell test -n "$$(git status --porcelain 2>/dev/null)" && echo -dirty)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG = $(MODULE)/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) \
          -X $(VERSION_PKG).Commit=$(COMMIT)$(DIRTY) \
          -X $(VERSION_PKG).Date=$(BUILD_DATE)

build:
	@mkdir -p $(OUT_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(OUT_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	@echo "built $(OUT_DIR)/$(SERVICE)"

# run starts the development server: one process, nothing durable, the
# stub reader unless LECTIO_CONFIG names a real one. It logs the address it
# listens on; the token is `dev` unless LECTIO_DEV_TOKEN sets another.
run: build
	LECTIO_DEV=true $(OUT_DIR)/$(SERVICE)

# specs checks the spec tree alone.
specs:
	@$(GO) tool lateregate spec-lint

# live parses a real file with a real reader, end to end. It calls a model,
# so it is never part of `make check`:
#   LECTIO_LIVE_CONFIG=reader.yaml LECTIO_LIVE_FILE=paper.pdf make live
# LECTIO_LIVE_PAGES selects pages (default 1-3), LECTIO_LIVE_OUT is a
# directory the result is written to, and LECTIO_LIVE_DESCRIBE=1 also has
# the figures described by the Policy's describe chain.
live:
	@$(GO) test ./cmd/lectiod -run '^TestLiveReader$$' -count=1 -v -timeout 40m

# openapi checks that api/openapi.yaml and the router agree.
openapi:
	@$(GO) test ./internal/httpapi -run '^TestTheServer'

fmt:
	@gofmt -w .

# hooks points git at the repository hook directory.
hooks:
	git config core.hooksPath .githooks
	@echo "installed git hooks (core.hooksPath=.githooks)"

clean:
	rm -rf $(OUT_DIR)
