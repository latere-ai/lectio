// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

// A stored spec keeps its numbers as they were written, takes the last of a
// repeated name, and is read when its text is not UTF-8.
func TestAStoredSpecKeepsItsNumbers(t *testing.T) {
	doc, err := addedDocument(Added{Name: "r", Spec: []byte("{\"adapter\":\"chat\",\"endpoint\":\"http://e\",\"model\":\"m\",\"cost\":1.50,\"big\":123456789012345678901,\"dup\":1,\"dup\":2,\"note\":\"\xff\"}")}, []string{"http://e"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for i := 0; i+1 < len(doc.Spec.Content); i += 2 {
		got[doc.Spec.Content[i].Value] = doc.Spec.Content[i+1].Value
	}
	if got["cost"] != "1.50" || got["big"] != "123456789012345678901" || got["dup"] != "2" || got["note"] != "�" {
		t.Errorf("the spec reads %v", got)
	}
	if doc.Spec.Kind != yaml.MappingNode {
		t.Errorf("the spec is a %v", doc.Spec.Kind)
	}
}

// A stored policy that repeats a name is one JSON object.
func TestAStoredPolicyMayRepeatAName(t *testing.T) {
	if _, err := policyDocument([]byte(`{"read":{"chain":["a"]},"read":{"chain":["b"]}}`)); err != nil {
		t.Fatal(err)
	}
}
