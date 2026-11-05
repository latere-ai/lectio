// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"encoding/json/v2"
	"testing"

	"latere.ai/x/lectio/internal/tasks"
)

// A document for a statement writes a missing list or object as null, which
// the statements read as "every owner" or "no filter", and an empty one as
// empty.
func TestADocumentForAStatementWritesNilAsNull(t *testing.T) {
	for name, tc := range map[string]struct {
		in   any
		want string
	}{
		"no owners":      {[]string(nil), `null`},
		"nobody":         {[]string{}, `[]`},
		"no labels":      {map[string]string(nil), `null`},
		"a bad string":   {"a\xffb", "\"a�b\""},
		"an exchange":    {tasks.Request{Free: 1}, `{"settles":null,"held":null,"free":1,"idle":false,"shutdown":false,"kinds":null}`},
		"a settled page": {tasks.Settle{Parse: "p", Task: "t", Prepare: &tasks.Prepared{}}, `{"parse":"p","task":"t","token":0,"outcome":"","usage":{},"prepare":{"pages":null}}`},
	} {
		got, err := json.Marshal(tc.in, sqlJSON)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}
