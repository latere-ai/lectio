// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package keys

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// outboundJSON writes the body of a request to the key endpoint: members in
// order of their names, and a missing list as null.
var outboundJSON = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowInvalidUTF8(true),
)

// foreignJSON reads the answer of the key endpoint, which this package does
// not produce: duplicate names, invalid UTF-8 and any letter case in a
// member name are accepted.
var foreignJSON = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	json.MatchCaseInsensitiveNames(true),
)
