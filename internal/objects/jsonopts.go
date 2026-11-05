// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package objects

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// storedJSON writes what the object store keeps: members in order of their
// names, so the same value is the same bytes, and text that is not UTF-8 is
// written as U+FFFD instead of failing the write. A stored value may hold a
// raw value a reader returned, which is written and read as it came: a
// repeated name is not an error.
var storedJSON = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// storedRead reads what storedJSON wrote.
var storedRead = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)
