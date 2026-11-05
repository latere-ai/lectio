// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// storedJSON writes what a task keeps or compares as bytes: members in order
// of their names, and text that is not UTF-8 as U+FFFD instead of a failure.
var storedJSON = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)
