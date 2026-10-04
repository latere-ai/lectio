// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Part is the reply to one window that the validator accepted: the object
// and the refs each of its values was read from, by JSON pointer.
type Part struct {
	Data      json.RawMessage     `json:"data"`
	Citations map[string][]string `json:"citations,omitempty"`
}

// Result is what an extraction produced: one object in the shape of the
// caller's schema, and for each of its values the blocks it was read from.
type Result struct {
	Data      json.RawMessage     `json:"data"`
	Citations map[string][]string `json:"citations,omitempty"`
}

// Merge joins the replies to the windows of one document, in reading order,
// into one object. The rule is mechanical: an object is merged member by
// member, a list is the lists joined with a value that is already there
// left out, and of 2 other values the first that is not null stands. A
// citation moves with its value: one of a list's item follows the item to
// its place in the joined list, one of an item that was already there is
// added to that item's, and one of a value that did not stand is dropped.
//
// The rule has known weaknesses, a value a later page states differently
// among them, so a result says how many windows it was merged from.
func Merge(parts []Part) Result {
	m := merger{citations: map[string][]string{}}
	var data any
	for _, part := range parts {
		var value any
		dec := json.NewDecoder(bytes.NewReader(part.Data))
		dec.UseNumber()
		if dec.Decode(&value) != nil {
			continue
		}
		m.from = part.Citations
		data = m.merge(data, value, "", "")
	}
	if data == nil {
		data = map[string]any{}
	}
	out := Result{Data: encode(data)}
	if len(m.citations) > 0 {
		out.Citations = m.citations
	}
	return out
}

// merger holds the citations of the object being built and of the part
// being merged into it.
type merger struct {
	citations map[string][]string
	from      map[string][]string
}

// merge joins a value of a part, at the pointer from, into the value that
// is there, at the pointer to, and returns what stands.
func (m *merger) merge(have, add any, to, from string) any {
	switch {
	case add == nil:
		return have
	case have == nil:
		m.adopt(to, from)
		return add
	}
	switch have := have.(type) {
	case map[string]any:
		object, ok := add.(map[string]any)
		if !ok {
			return have
		}
		m.cite(to, m.from[from])
		for _, name := range slices.Sorted(maps.Keys(object)) {
			token := "/" + escape(name)
			have[name] = m.merge(have[name], object[name], to+token, from+token)
		}
		return have
	case []any:
		list, ok := add.([]any)
		if !ok {
			return have
		}
		m.cite(to, m.from[from])
		for i, item := range list {
			at := slices.IndexFunc(have, func(there any) bool { return same(there, item) })
			if at < 0 {
				at = len(have)
				have = append(have, item)
			}
			// The item's citations, and those of everything inside it,
			// follow it to its place.
			m.adopt(to+"/"+strconv.Itoa(at), from+"/"+strconv.Itoa(i))
		}
		return have
	}
	// The first value that is not null stands. A later window that states
	// the same value adds the blocks it read it from.
	if same(have, add) {
		m.cite(to, m.from[from])
	}
	return have
}

// adopt moves the citations of a part's value, and of everything inside it,
// to where the value stands in the merged object.
func (m *merger) adopt(to, from string) {
	for pointer, refs := range m.from {
		if pointer == from || strings.HasPrefix(pointer, from+"/") {
			m.cite(to+pointer[len(from):], refs)
		}
	}
}

// cite adds refs to the citations of a pointer, each once.
func (m *merger) cite(pointer string, refs []string) {
	for _, ref := range refs {
		if !slices.Contains(m.citations[pointer], ref) {
			m.citations[pointer] = append(m.citations[pointer], ref)
		}
	}
}

// same reports whether 2 JSON values are the same value.
func same(a, b any) bool {
	return bytes.Equal(encode(a), encode(b))
}

// resolve reports whether a JSON pointer names a value of a JSON value.
func resolve(value any, pointer string) bool {
	if pointer == "" {
		return true
	}
	if !strings.HasPrefix(pointer, "/") {
		return false
	}
	for token := range strings.SplitSeq(pointer[1:], "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := value.(type) {
		case map[string]any:
			member, ok := node[token]
			if !ok {
				return false
			}
			value = member
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(node) {
				return false
			}
			value = node[i]
		default:
			return false
		}
	}
	return true
}

// Cited keeps of a reply's citations the ones that stand: a pointer that
// names a value of the reply's object, with the refs that are refs of the
// text the reply was given. A ref a model made up is dropped, and so is a
// citation of a value that is not in the object, such as one that was left
// out for being null.
func Cited(data []byte, citations map[string][]string, refs []string) map[string][]string {
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return nil
	}
	out := map[string][]string{}
	for pointer, cited := range citations {
		if !resolve(value, pointer) {
			continue
		}
		for _, ref := range cited {
			if slices.Contains(refs, ref) && !slices.Contains(out[pointer], ref) {
				out[pointer] = append(out[pointer], ref)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
