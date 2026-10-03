// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package api holds the HTTP contract: openapi.yaml, the one description
// of every route, request, response, and error code. The server serves this
// file and is tested against it, and a client may be generated from it.
package api

import _ "embed"

// OpenAPI is the contract, as OpenAPI 3.1 in YAML.
//
//go:embed openapi.yaml
var OpenAPI []byte
