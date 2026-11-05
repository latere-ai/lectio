// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package quality

import (
	"strings"
	"testing"
)

// A truth written by hand is read with a repeated name, another letter
// case and bad UTF-8 in a string; a member it does not describe is refused.
func TestATruthWrittenByHandIsRead(t *testing.T) {
	truth, err := Parse([]byte("{\"source\":\"a\",\"Source\":\"\xff\",\"PAGES\":[]}"))
	if err != nil || truth.Source != "�" {
		t.Fatalf("%+v, %v", truth, err)
	}
	if _, err := Parse([]byte(`{"pages":[],"unknown":1}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("a member the truth does not describe is read: %v", err)
	}
	if _, err := Parse([]byte(`{"pages":[]} {"pages":[]}`)); err != nil {
		t.Errorf("the first value is not read: %v", err)
	}
}

// The report file is indented by two spaces, ends in a newline, keeps the
// bars of its classes in order of their names, and writes no escape for
// <, > or &.
func TestTheReportFileIsExact(t *testing.T) {
	raw, err := (&Report{Started: "s", Reader: "r<&>", Server: "v", Bars: map[Class]Bars{"z": {}, "a": {}}}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, "{\n  \"started\": \"s\",\n  \"reader\": \"r<&>\",\n  \"server\": \"v\",\n  \"bars\": {\n    \"a\": {") || !strings.HasSuffix(got, "\n}\n") || strings.Index(got, `"z"`) < strings.Index(got, `"a"`) {
		t.Errorf("the report file is\n%s", got)
	}
}
