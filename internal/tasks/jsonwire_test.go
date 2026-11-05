// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"
)

// A settle writes its wait as milliseconds, a wait under a millisecond as 1,
// and a result as the document it is.
func TestASettleOnTheWireIsExact(t *testing.T) {
	for name, tc := range map[string]struct {
		in   Settle
		want string
		back time.Duration
	}{
		"no wait":      {Settle{Parse: "p", Task: "t", Token: 3}, `{"parse":"p","task":"t","token":3,"outcome":"","usage":{}}`, 0},
		"a wait":       {Settle{Parse: "p", Task: "t", Token: 3, RetryAfter: 1500 * time.Millisecond}, `{"parse":"p","task":"t","token":3,"outcome":"","usage":{},"retry_after_ms":1500}`, 1500 * time.Millisecond},
		"a short wait": {Settle{Parse: "p", Task: "t", Token: 3, RetryAfter: time.Microsecond}, `{"parse":"p","task":"t","token":3,"outcome":"","usage":{},"retry_after_ms":1}`, time.Millisecond},
		"a result":     {Settle{Parse: "p", Task: "t", Token: 3, Result: jsontext.Value(`{}`)}, `{"parse":"p","task":"t","token":3,"outcome":"","usage":{},"result":{}}`, 0},
	} {
		got, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
		var back Settle
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if back.RetryAfter != tc.back {
			t.Errorf("%s: the wait came back as %s", name, back.RetryAfter)
		}
	}
}

// The options of the call that writes a request reach the settles in it: a
// missing list is null where the call asks for it.
func TestTheOptionsOfARequestReachItsSettles(t *testing.T) {
	req := Request{Settles: []Settle{{Parse: "p", Task: "t", Prepare: &Prepared{}}}}
	got, err := json.Marshal(req, json.FormatNilSliceAsNull(true))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"settles":[{"parse":"p","task":"t","token":0,"outcome":"","usage":{},"prepare":{"pages":null}}],"held":null,"free":0,"idle":false,"shutdown":false,"kinds":null}`
	if string(got) != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// A raw value that is empty or null is kept when it is set, and left out
// only when it is not.
func TestARawValueIsLeftOutOnlyWhenUnset(t *testing.T) {
	for name, tc := range map[string]struct {
		in   jsontext.Value
		want string
	}{
		"unset": {nil, `{"pages":[],"native":true}`},
		"empty": {jsontext.Value(`{}`), `{"manifest":{},"pages":[],"native":true}`},
		"null":  {jsontext.Value(`null`), `{"manifest":null,"pages":[],"native":true}`},
	} {
		got, err := json.Marshal(Prepared{Manifest: tc.in, Pages: []int{}, Native: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}
