// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// file is twoReaders and policy as LECTIO_CONFIG.
func file(t *testing.T) File {
	t.Helper()
	f, err := ReadFile(write(t, map[string]string{"10-readers.yaml": twoReaders, "20-policy.yml": policy}))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// added is a chat reader as the API stores it, naming no endpoint.
const added = `{"adapter":"chat","model":"other-model","timeout":"3m","image":{"dpi":160,"longEdge":2048,"format":"png"},"boxes":{"order":"yxyx","space":"grid"}}`

// TestAStoredReaderIsBuiltBesideTheConfiguredOnes: an added reader joins
// the configured ones under its name, reaches the endpoint the configured
// readers reach when it names none, reads, describes and extracts, and is
// told apart from them. A stored policy orders all of them; without one
// the configured Policy does.
func TestAStoredReaderIsBuiltBesideTheConfiguredOnes(t *testing.T) {
	f := file(t)
	if got := f.Endpoints(); strings.Join(got, ",") != "https://gateway.example/v1,http://engine.internal:8000/read" {
		t.Fatalf("the endpoints the configured readers reach: %v", got)
	}
	got, err := f.Build([]Added{{Name: "second", Spec: []byte(added)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Readers["second"] == nil || got.Describers["second"] == nil || got.Extractors["second"] == nil {
		t.Fatalf("the added reader: %+v", got)
	}
	if got.Sources["second"] != SourceAdded || got.Sources["default"] != SourceConfigured || got.Adapters["second"] != "chat" ||
		got.Models["second"] != "other-model" || got.Models["default"] != "some-model" || got.Models["engine"] != "" ||
		string(got.Specs["second"]) != added || got.Specs["default"] != nil {
		t.Fatalf("what the set says of its readers: %v %v %v", got.Sources, got.Models, got.Specs)
	}
	if strings.Join(got.Chain, ",") != "default,engine" || got.PolicySource != SourceConfigured || len(got.Pools) != 3 {
		t.Fatalf("the configured policy: %v, %s, %v", got.Chain, got.PolicySource, got.Pools)
	}

	set := []byte(`{"read":{"chain":["second","engine"]},"describe":{"chain":["second"]},"extract":{"chain":["default"]}}`)
	got, err = f.Build([]Added{{Name: "second", Spec: []byte(added)}}, set)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Chain, ",") != "second,engine" || strings.Join(got.DescribeChain, ",") != "second" ||
		strings.Join(got.ExtractChain, ",") != "default" || got.PolicySource != SourceSet {
		t.Fatalf("the stored policy: %v %v %v %s", got.Chain, got.DescribeChain, got.ExtractChain, got.PolicySource)
	}

	// Load is the file built with nothing stored.
	loaded, err := Load(filepath.Join(write(t, map[string]string{"a.yaml": twoReaders + "---" + policy})))
	if err != nil || loaded.Sources["engine"] != SourceConfigured || loaded.PolicySource != SourceConfigured {
		t.Fatalf("a file loaded alone: %+v, %v", loaded.Sources, err)
	}
}

// TestWhatTheAPIMayStoreIsHeldToTheDeployment: a stored reader is refused
// for a name outside the pattern or a configured reader's, a spec that
// would not load, the stub adapter and an endpoint the deployment does
// not allow. LECTIO_READER_ENDPOINTS replaces the endpoints the configured
// readers reach, and a stored reader that names none takes its first.
func TestWhatTheAPIMayStoreIsHeldToTheDeployment(t *testing.T) {
	f := file(t)
	for name, tc := range map[string]struct {
		name, spec, want string
	}{
		"a name outside the pattern": {"Second", added, "a reader's name"},
		"a long name":                {strings.Repeat("a", 64), added, "a reader's name"},
		"a configured name":          {"default", added, ErrConfigured.Error()},
		"not an object":              {"x", `[]`, "not one JSON object"},
		"not JSON":                   {"x", `{`, "not one JSON object"},
		"the stub adapter":           {"x", `{"adapter":"stub"}`, "stub adapter"},
		"another endpoint":           {"x", `{"adapter":"chat","endpoint":"http://elsewhere.example/v1","model":"m"}`, "LECTIO_READER_ENDPOINTS"},
		"a member no spec has":       {"x", `{"adapter":"chat","model":"m","topP":1}`, "topP"},
		"no model":                   {"x", `{"adapter":"chat"}`, "names no model"},
		"a text reader's endpoint":   {"x", `{"adapter":"text","endpoint":"https://gateway.example/v1"}`, "calls no endpoint"},
	} {
		if err := f.CheckReader(tc.name, []byte(tc.spec)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.CheckReader("default", []byte(added)); !errors.Is(err, ErrConfigured) {
		t.Errorf("a configured name is not told apart: %v", err)
	}
	for _, spec := range []string{added, `{"adapter":"chat","endpoint":"https://gateway.example/v1","model":"m"}`, `{"adapter":"text"}`,
		`{"adapter":"layout","endpoint":"http://engine.internal:8000/read"}`} {
		if err := f.CheckReader("x", []byte(spec)); err != nil {
			t.Errorf("%s: %v", spec, err)
		}
	}

	only := f.WithEndpoints([]string{"http://gateway.internal/v1"})
	if err := only.CheckReader("x", []byte(`{"adapter":"chat","endpoint":"https://gateway.example/v1","model":"m"}`)); err == nil {
		t.Error("an endpoint the list leaves out was stored")
	}
	got, err := only.Build([]Added{{Name: "x", Spec: []byte(`{"adapter":"chat","model":"m"}`)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Specs["x"]) != `{"adapter":"chat","model":"m"}` {
		t.Fatalf("the stored spec is kept as it was sent: %s", got.Specs["x"])
	}
	none, err := ReadFile(write(t, map[string]string{"a.yaml": "apiVersion: lectio.latere.ai/v1\nkind: Reader\nmetadata: {name: only}\nspec: {adapter: stub}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := none.CheckReader("x", []byte(`{"adapter":"chat","model":"m"}`)); err == nil || !strings.Contains(err.Error(), "allows none") {
		t.Errorf("a chat reader where no endpoint is allowed: %v", err)
	}
	if _, err := f.Build([]Added{{Name: "default", Spec: []byte(added)}}, nil); err == nil {
		t.Error("a stored reader under a configured name was built")
	}
}

// TestAStoredPolicyNamesAReaderInEachChain: a stored policy is checked as
// the file's is, and names a reader in each of its chains.
func TestAStoredPolicyNamesAReaderInEachChain(t *testing.T) {
	f := file(t)
	for name, tc := range map[string]struct{ spec, want string }{
		"not JSON":              {`{`, "not one JSON object"},
		"not an object":         {`[]`, "not one JSON object"},
		"a member no spec has":  {`{"write":{"chain":["default"]}}`, "write"},
		"a reader that is none": {`{"read":{"chain":["gone"]},"describe":{"chain":["default"]},"extract":{"chain":["default"]}}`, `names "gone"`},
		"an empty chain":        {`{"read":{"chain":["default"]},"describe":{"chain":["default"]}}`, "extract chain names no reader"},
		"a layout describing":   {`{"read":{"chain":["default"]},"describe":{"chain":["engine"]},"extract":{"chain":["default"]}}`, "cannot describe"},
	} {
		if err := f.CheckPolicy(nil, []byte(tc.spec)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.CheckPolicy(nil, []byte(`{"read":{"chain":["engine","default"]},"describe":{"chain":["default"]},"extract":{"chain":["default"]}}`)); err != nil {
		t.Error(err)
	}
}

// TestTheStubConfigurationTakesStoredReaders: a server with no
// LECTIO_CONFIG is the stub alone until the API stores a reader, and then
// orders it by the policy stored with it.
func TestTheStubConfigurationTakesStoredReaders(t *testing.T) {
	f := StubFile()
	got, err := f.Build(nil, nil)
	if err != nil || len(got.Readers) != 1 || got.Sources["stub"] != SourceConfigured || strings.Join(got.ExtractChain, ",") != "stub" {
		t.Fatalf("the stub file alone: %+v, %v", got, err)
	}
	if !f.Configured("stub") || len(f.Endpoints()) != 0 {
		t.Fatalf("the stub file: configured %t, endpoints %v", f.Configured("stub"), f.Endpoints())
	}
	f = f.WithEndpoints([]string{"http://gateway.internal/v1"})
	got, err = f.Build([]Added{{Name: "model", Spec: []byte(`{"adapter":"chat","model":"m"}`)}},
		[]byte(`{"read":{"chain":["model"]},"describe":{"chain":["model"]},"extract":{"chain":["stub"]}}`))
	if err != nil || strings.Join(got.Chain, ",") != "model" || got.Readers["stub"] == nil || got.PolicySource != SourceSet {
		t.Fatalf("the stub file with a stored reader: %+v, %v", got, err)
	}
	if got, err = f.Build([]Added{{Name: "model", Spec: []byte(`{"adapter":"chat","model":"m"}`)}}, nil); err != nil || strings.Join(got.Chain, ",") != "stub" {
		t.Fatalf("the stub file with a stored reader and no stored policy: %v, %v", got.Chain, err)
	}
}

// TestTheLiveSetIsSwappedWhole: a process reads the set in force and sees
// the next one once it is set.
func TestTheLiveSetIsSwappedWhole(t *testing.T) {
	first, err := StubFile().Build(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(first)
	held := live.Current()
	next := file(t)
	second, err := next.Build(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	live.Set(second)
	if held.Readers["stub"] == nil || live.Current().Readers["default"] == nil || live.Current().Readers["stub"] != nil {
		t.Fatalf("the set held before the swap changed, or the swap did not take: %v, %v", held.Readers, live.Current().Readers)
	}
}
