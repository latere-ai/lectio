// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// The bounds of a JSON value this package holds to a schema or reads a
// schema from. A validator works on a number as a rational of any size and
// on a value's place as a path it copies, so a value is read with numbers
// that are machine numbers and with a depth that has an end. What is past a
// bound is said by where it is, and nothing is validated.
const (
	// MaxNumberLength is how many characters a number is written in at
	// most. A machine number round-trips in 24, and 32 leaves room for
	// digits a writer did not need.
	MaxNumberLength = 32

	// MaxValueDepth is how deep a reply nests at most, counting every
	// object and every array as a level. The validator copies the path to
	// a value each time it goes a level down, so depth is work for every
	// value below it.
	MaxValueDepth = 64

	// maxPointer is how many bytes of a value's place a finding names. A
	// member's name is the reply's, of any length.
	maxPointer = 512
)

// flaw is something in a JSON value that is past a bound of this package:
// what, and where as a JSON pointer.
type flaw struct {
	pointer string
	what    string
}

// decode reads one JSON value with every number a machine number. A text
// that is not one JSON value answers an error. A number written in more
// characters than MaxNumberLength or outside what a machine number holds,
// and a value nested deeper than depth levels, are answered as flaws, and
// the value is then not to be used. Not more than a few flaws are named.
func decode(data []byte, depth int) (value any, flaws []flaw, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("extract: text after the JSON value")
	}
	n := narrowing{depth: depth}
	value = n.narrow(value, nil)
	return value, n.flaws, nil
}

// shownFlaws is how many flaws of one value are named.
const shownFlaws = 5

// narrowing turns the numbers of a decoded value into machine numbers.
type narrowing struct {
	depth int
	flaws []flaw
}

func (n *narrowing) flawed(path []string, what string) {
	if len(n.flaws) < shownFlaws {
		n.flaws = append(n.flaws, flaw{pointer: pointer(path), what: what})
	}
}

// narrow returns v with each number in it a float64, in place for objects
// and arrays.
func (n *narrowing) narrow(v any, path []string) any {
	switch v := v.(type) {
	case json.Number:
		f, ok := number(string(v))
		if !ok {
			n.flawed(path, "the number is written in more than "+strconv.Itoa(MaxNumberLength)+" characters or is outside what a number holds")
		}
		return f
	case map[string]any:
		if len(path) >= n.depth {
			n.flawed(path, "the value nests deeper than "+strconv.Itoa(n.depth)+" levels")
			return v
		}
		for name, member := range v {
			v[name] = n.narrow(member, append(path, name))
		}
	case []any:
		if len(path) >= n.depth {
			n.flawed(path, "the value nests deeper than "+strconv.Itoa(n.depth)+" levels")
			return v
		}
		for i, item := range v {
			v[i] = n.narrow(item, append(path, strconv.Itoa(i)))
		}
	}
	return v
}

// number reads a JSON number as a machine number. ok is false for one
// written in more than MaxNumberLength characters, one too large for a
// machine number, and one that is not zero and too small for it.
func number(literal string) (f float64, ok bool) {
	if len(literal) > MaxNumberLength {
		return 0, false
	}
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return 0, false
	}
	if f == 0 {
		mantissa, _, _ := strings.Cut(strings.ToLower(literal), "e")
		if strings.ContainsAny(mantissa, "123456789") {
			return 0, false
		}
	}
	return f, true
}

// pointer writes a path as a JSON pointer of at most maxPointer bytes and
// the mark of a cut. The root is the empty pointer.
func pointer(path []string) string {
	var b strings.Builder
	for _, token := range path {
		if b.Len()+len(token) > maxPointer {
			b.WriteString("/...")
			break
		}
		b.WriteString("/")
		b.WriteString(escape(token))
	}
	return b.String()
}
