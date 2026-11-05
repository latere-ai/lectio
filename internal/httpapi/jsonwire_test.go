// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/lectio/internal/fault"
)

// A body is refused for a member the contract does not name, for a value of
// another type, for a second value and for no object, and the error names
// the member.
func TestARequestBodyIsHeldToTheContract(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		code   fault.Code
		detail string
	}{
		"unknown":       {`{"pages":"1","colour":1}`, fault.UnknownField, `"colour"`},
		"nested":        {`{"source":{"file":"f","path":"p"}}`, fault.UnknownField, `"path"`},
		"wrong type":    {`{"priority":"high"}`, fault.InvalidRequest, "priority is a int"},
		"nested type":   {`{"source":{"file":3}}`, fault.InvalidRequest, "source.file is a string"},
		"two values":    {`{} {}`, fault.InvalidRequest, "more than one JSON value"},
		"not an object": {`[]`, fault.InvalidRequest, "not a JSON object"},
		"empty":         {``, fault.InvalidRequest, "not a JSON object"},
		"cut short":     {`{"pages":`, fault.InvalidRequest, "not a JSON object"},
	} {
		var req parseRequest
		_, err := decode(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(tc.body)), &req)
		if err == nil || fault.CodeOf(err) != tc.code || !strings.Contains(err.Error(), tc.detail) {
			t.Errorf("%s: %v, want %s about %s", name, err, tc.code, tc.detail)
		}
	}
	var req parseRequest
	raw, err := decode(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(` {"pages":"2"} `)), &req)
	if err != nil || req.Pages != "2" || string(raw) != ` {"pages":"2"} ` {
		t.Errorf("%+v, %q, %v", req, raw, err)
	}
}
