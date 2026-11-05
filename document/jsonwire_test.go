// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package document

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
)

// A field's data is left out only when it is not set: an empty object and a
// null are values, and are written.
func TestAFieldsDataIsLeftOutOnlyWhenUnset(t *testing.T) {
	for name, tc := range map[string]struct {
		data jsontext.Value
		want string
	}{
		"unset": {nil, `{"name":"n","state":"pending","constrained":false}`},
		"empty": {jsontext.Value(`{}`), `{"name":"n","state":"pending","data":{},"constrained":false}`},
		"null":  {jsontext.Value(`null`), `{"name":"n","state":"pending","data":null,"constrained":false}`},
	} {
		got, err := json.Marshal(Field{Name: "n", State: "pending", Data: tc.data})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}
