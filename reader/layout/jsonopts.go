// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package layout

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// foreignJSON reads JSON this package does not produce, a model's reply or
// an engine's: duplicate names, invalid UTF-8 and any letter case in a
// member name are accepted.
var foreignJSON = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	json.MatchCaseInsensitiveNames(true),
)
