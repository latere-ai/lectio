// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import "encoding/json/jsontext"

// bodyText is for bodies that carry a document's text, which is whatever the
// file or the model held: bytes that are not UTF-8 are written as U+FFFD
// instead of failing the response.
var bodyText = jsontext.AllowInvalidUTF8(true)
