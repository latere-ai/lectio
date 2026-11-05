// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package jsontree

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

// A value is read with its numbers as written, a repeated name takes its
// last value, and it is written again with the same digits.
func TestAValueKeepsItsNumbers(t *testing.T) {
	dec := jsontext.NewDecoder(strings.NewReader(`{"a":[1.50,12345678901234567890123,true,false,null,"s"],"a":[0.10],"b":{}} trailing`), jsontext.AllowDuplicateNames(true))
	got, err := Read(dec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got, json.Deterministic(true))
	if want := `{"a":[0.10],"b":{}}`; err != nil || string(raw) != want {
		t.Errorf("%s, %v, want %s", raw, err, want)
	}
	dec = jsontext.NewDecoder(strings.NewReader(`[1.50,12345678901234567890123,true,false,null,"s"]`))
	got, err = Read(dec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(got)
	if want := `[1.50,12345678901234567890123,true,false,null,"s"]`; err != nil || string(raw) != want {
		t.Errorf("%s, %v, want %s", raw, err, want)
	}
}
