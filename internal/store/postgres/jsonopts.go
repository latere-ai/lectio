// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// sqlJSON writes a document a statement of this package reads. The
// statements tell a missing list or object, which is null, from an empty
// one, so a nil is written as null. Text that is not UTF-8 is written as
// U+FFFD instead of failing the write.
var sqlJSON = json.JoinOptions(
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowInvalidUTF8(true),
)
