// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package jsontree reads one JSON value into plain Go values and keeps each
// number as it was written, so a value read and written again keeps its
// digits.
package jsontree

import "encoding/json/jsontext"

// Number is a JSON number as it was written.
type Number string

// MarshalJSON writes the number as it was read.
func (n Number) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// Read reads the next value of dec as nil, bool, string, Number, []any and
// map[string]any. A name that repeats in an object takes the last value.
// Whatever follows the value is left unread.
func Read(dec *jsontext.Decoder) (any, error) {
	switch dec.PeekKind() {
	case '{':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		obj := map[string]any{}
		for dec.PeekKind() != '}' {
			name, err := dec.ReadToken()
			if err != nil {
				return nil, err
			}
			key := name.String()
			member, err := Read(dec)
			if err != nil {
				return nil, err
			}
			obj[key] = member
		}
		_, err := dec.ReadToken()
		return obj, err
	case '[':
		if _, err := dec.ReadToken(); err != nil {
			return nil, err
		}
		list := []any{}
		for dec.PeekKind() != ']' {
			item, err := Read(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, item)
		}
		_, err := dec.ReadToken()
		return list, err
	case '0':
		raw, err := dec.ReadValue()
		return Number(raw), err
	}
	tok, err := dec.ReadToken()
	if err != nil {
		return nil, err
	}
	switch tok.Kind() {
	case '"':
		return tok.String(), nil
	case 't':
		return true, nil
	case 'f':
		return false, nil
	}
	return nil, nil
}
