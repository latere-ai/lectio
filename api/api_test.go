// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestNoDescriptionIsCutAtAComma holds the contract to saying what it
// means. Inside a flow mapping, an unquoted description ends at its first
// comma, and the rest of the sentence becomes a key of its own with no
// value: a reader of the contract sees half a description and a stray
// field. A key holding a space with no value is that remainder.
func TestNoDescriptionIsCutAtAComma(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal(OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	var stray []string
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if strings.Contains(key.Value, " ") && value.Tag == "!!null" {
					stray = append(stray, path+"/"+key.Value)
				}
				walk(value, path+"/"+key.Value)
			}
			return
		}
		for _, child := range n.Content {
			walk(child, path)
		}
	}
	walk(&doc, "")
	for _, at := range stray {
		t.Errorf("a description is cut at a comma; the rest reads as the key %q", at)
	}
}
