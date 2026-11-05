// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"encoding/json/jsontext"
	"testing"
)

// A reply is read as the model wrote it: a repeated name takes its last
// value, and a string that is not UTF-8 is read.
func TestAReplyWithRepeatedNamesAndBadUTF8IsRead(t *testing.T) {
	value, flaws, err := decode([]byte("{\"a\":1,\"a\":2,\"s\":\"\xff\"}"), MaxValueDepth)
	if err != nil || len(flaws) != 0 {
		t.Fatalf("%v, %v", flaws, err)
	}
	obj, _ := value.(map[string]any)
	if obj["a"] != float64(2) || obj["s"] != "�" {
		t.Errorf("read as %v", value)
	}
}

// Merged parts keep the digits of every number, however many, and write
// the members in order of their names.
func TestMergeKeepsTheDigitsOfANumber(t *testing.T) {
	out := Merge([]Part{{Data: jsontext.Value(`{"z":1.50,"a":[12345678901234567890123,null],"m":"x"}`)}})
	if want := `{"a":[12345678901234567890123,null],"m":"x","z":1.50}`; string(out.Data) != want {
		t.Errorf("%s, want %s", out.Data, want)
	}
}

// A value of a model's reply is written with its members in order and a
// missing list or object as null.
func TestEncodeIsExact(t *testing.T) {
	got := encode(map[string]any{"b": []string(nil), "a": map[string]int(nil), "c": []any{}, "d": "\xff"})
	if want := "{\"a\":null,\"b\":null,\"c\":[],\"d\":\"�\"}"; string(got) != want {
		t.Errorf("%s, want %s", got, want)
	}
}
