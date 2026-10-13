// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/lectio/reader/stub"
)

// Readers changed at runtime (specs/019-readers-at-runtime.md): the readers
// and the policy the API stores are built beside the documents of
// LECTIO_CONFIG, into one set, as one file of both would load.

// Where a reader or the policy came from.
const (
	// SourceConfigured is a document of LECTIO_CONFIG. The API reads it and
	// never changes it.
	SourceConfigured = "configured"
	// SourceAdded is a reader the API stored.
	SourceAdded = "added"
	// SourceSet is a policy the API stored, in force in the place of the
	// configured one.
	SourceSet = "set"
)

// readerName is what a reader stored through the API may be called.
var readerName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

// File is the documents of LECTIO_CONFIG, read once: the configured readers
// and policy, kept as documents so that what the API stores is built beside
// them.
type File struct {
	path      string
	readers   []document
	policy    *document
	endpoints []string
	stub      bool
}

// StubFile is the configuration of a server with no LECTIO_CONFIG: the
// stub reader, which calls no model.
func StubFile() File {
	doc := document{APIVersion: APIVersion, Kind: "Reader", where: "the stub configuration", source: SourceConfigured}
	doc.Metadata.Name = stub.Name
	if err := doc.Spec.Encode(map[string]string{"adapter": "stub"}); err != nil {
		// A map of strings always encodes.
		panic(err)
	}
	return File{path: "the stub configuration", readers: []document{doc}, stub: true}
}

// WithEndpoints returns the file with the endpoints a stored reader may
// reach, LECTIO_READER_ENDPOINTS. With none, they are the endpoints the
// configured readers reach, in the order the documents name them.
func (f File) WithEndpoints(endpoints []string) File {
	f.endpoints = slices.Clone(endpoints)
	return f
}

// Endpoints are the endpoints a stored reader may reach; the first is the
// one a stored reader that names none reaches.
func (f File) Endpoints() []string {
	if len(f.endpoints) > 0 {
		return slices.Clone(f.endpoints)
	}
	var out []string
	for _, doc := range f.readers {
		var spec readerSpec
		if strict(doc.Spec, &spec) != nil || spec.Endpoint == "" || slices.Contains(out, spec.Endpoint) {
			continue
		}
		out = append(out, spec.Endpoint)
	}
	return out
}

// Configured reports whether a document of the file declares the reader.
func (f File) Configured(name string) bool {
	return slices.ContainsFunc(f.readers, func(d document) bool { return d.Metadata.Name == name })
}

// Added is a reader the API stored: its name, and its spec as one JSON
// document in the members of a Reader document's spec.
type Added struct {
	Name string
	Spec []byte
}

// Build builds the configured readers and the added ones into one set,
// ordered by policy, the spec of a Policy document as one JSON document,
// or by the configured Policy when policy is nil. Everything must hold, as
// Load says, and an added reader is held besides to what CheckReader holds
// it to.
func (f File) Build(added []Added, policy []byte) (Readers, error) {
	if f.stub && len(added) == 0 && policy == nil {
		out := Stub()
		out.Sources = map[string]string{stub.Name: SourceConfigured}
		out.Adapters = map[string]string{stub.Name: "stub"}
		out.Models, out.Specs, out.PolicySource = map[string]string{}, map[string][]byte{}, SourceConfigured
		return out, nil
	}
	docs := make([]document, 0, len(f.readers)+len(added))
	for _, doc := range f.readers {
		doc.source = SourceConfigured
		docs = append(docs, doc)
	}
	endpoints := f.Endpoints()
	for _, a := range added {
		if f.Configured(a.Name) {
			return Readers{}, fmt.Errorf("config: the stored reader %q is a configured reader's name", a.Name)
		}
		doc, err := addedDocument(a, endpoints)
		if err != nil {
			return Readers{}, err
		}
		docs = append(docs, doc)
	}
	in := f.policy
	if f.stub {
		in = stubPolicy()
	}
	if policy != nil {
		doc, err := policyDocument(policy)
		if err != nil {
			return Readers{}, err
		}
		in = &doc
	}
	return assemble(f.path, docs, in)
}

// stubPolicy orders the stub configuration: the stub reader reads, describes
// and extracts, as Stub does.
func stubPolicy() *document {
	doc := document{APIVersion: APIVersion, Kind: "Policy", where: "the stub configuration", source: SourceConfigured}
	chain := map[string][]string{"chain": {stub.Name}}
	if err := doc.Spec.Encode(map[string]any{"read": chain, "describe": chain, "extract": chain}); err != nil {
		panic(err)
	}
	return &doc
}

// CheckReader reports why a reader the API is asked to store could not be:
// a name outside the pattern or a configured reader's, a spec that would not
// load from the file, the stub adapter, or an endpoint outside Endpoints.
func (f File) CheckReader(name string, spec []byte) error {
	switch {
	case !readerName.MatchString(name):
		return errors.New("a reader's name is 1 to 63 of a-z, 0-9, '.' and '-', starting with a letter or a digit")
	case f.Configured(name):
		return ErrConfigured
	}
	doc, err := addedDocument(Added{Name: name, Spec: spec}, f.Endpoints())
	if err != nil {
		return err
	}
	_, _, _, _, _, err = build(&doc)
	return err
}

// CheckPolicy reports why a policy the API is asked to store could not be:
// a spec that would not load with the readers there are, or a chain that
// names no reader. A stored policy names a reader in each of its chains,
// so that no kind of work is left with nobody to do it.
func (f File) CheckPolicy(added []Added, policy []byte) error {
	r, err := f.Build(added, policy)
	if err != nil {
		return err
	}
	for _, chain := range []struct {
		name    string
		readers []string
	}{{"read", r.Chain}, {"describe", r.DescribeChain}, {"extract", r.ExtractChain}} {
		if len(chain.readers) == 0 {
			return fmt.Errorf("the %s chain names no reader", chain.name)
		}
	}
	return nil
}

// ErrConfigured is a change asked of a configured reader.
var ErrConfigured = errors.New("the reader is declared by the configuration, which the API does not change")

// addedDocument is the Reader document of a stored reader. Its spec is held
// to what the API may store: no stub adapter, and an endpoint among
// endpoints, the first when the spec names none and its adapter reaches
// one.
func addedDocument(a Added, endpoints []string) (document, error) {
	doc := document{APIVersion: APIVersion, Kind: "Reader", source: SourceAdded, raw: a.Spec,
		where: fmt.Sprintf("the stored reader %q", a.Name)}
	doc.Metadata.Name = a.Name
	var members map[string]any
	dec := json.NewDecoder(bytes.NewReader(a.Spec))
	dec.UseNumber()
	if err := dec.Decode(&members); err != nil || members == nil {
		return document{}, fmt.Errorf("%s: the spec is not one JSON object", doc.where)
	}
	switch adapter, _ := members["adapter"].(string); adapter {
	case "stub":
		return document{}, fmt.Errorf("%s: the stub adapter reads nothing, and is not stored", doc.where)
	case "chat", "layout":
		endpoint, _ := members["endpoint"].(string)
		switch {
		case endpoint == "" && len(endpoints) == 0:
			return document{}, fmt.Errorf("%s: the spec names no endpoint, and the deployment allows none", doc.where)
		case endpoint == "":
			members["endpoint"] = endpoints[0]
		case !slices.Contains(endpoints, endpoint):
			return document{}, fmt.Errorf("%s: the endpoint is not one LECTIO_READER_ENDPOINTS allows", doc.where)
		}
	}
	// JSON is YAML, so the spec decodes as the file's does, with its
	// numbers as the numbers they were written as.
	raw, err := json.Marshal(members)
	if err != nil {
		return document{}, fmt.Errorf("%s: %w", doc.where, err)
	}
	if err := yaml.Unmarshal(raw, &doc.Spec); err != nil {
		return document{}, fmt.Errorf("%s: %w", doc.where, err)
	}
	if doc.Spec.Kind == yaml.DocumentNode && len(doc.Spec.Content) == 1 {
		doc.Spec = *doc.Spec.Content[0]
	}
	return doc, nil
}

// policyDocument is the Policy document of a stored policy.
func policyDocument(spec []byte) (document, error) {
	doc := document{APIVersion: APIVersion, Kind: "Policy", source: SourceSet, where: "the stored policy"}
	doc.Metadata.Name = "stored"
	if !json.Valid(spec) {
		return document{}, errors.New("config: the stored policy is not one JSON object")
	}
	if err := yaml.Unmarshal(spec, &doc.Spec); err != nil {
		return document{}, fmt.Errorf("config: the stored policy: %w", err)
	}
	if doc.Spec.Kind == yaml.DocumentNode && len(doc.Spec.Content) == 1 {
		doc.Spec = *doc.Spec.Content[0]
	}
	if doc.Spec.Kind != yaml.MappingNode {
		return document{}, errors.New("config: the stored policy is not one JSON object")
	}
	return doc, nil
}

// Live is the set of readers in force in a process, swapped whole when the
// stored readers or the policy change. A reader of it takes Current once
// per piece of work, so that one piece sees one set.
type Live struct{ p atomic.Pointer[Readers] }

// NewLive holds r.
func NewLive(r Readers) *Live {
	l := &Live{}
	l.p.Store(&r)
	return l
}

// Current is the set in force.
func (l *Live) Current() *Readers { return l.p.Load() }

// Set puts r in force.
func (l *Live) Set(r Readers) { l.p.Store(&r) }
