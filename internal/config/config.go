// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package config reads what an operator sets: the settings of the process
// from its environment, and the readers and the routing policy from
// documents in a file or a directory. The documents are declared objects,
// one Reader per reader and one Policy, so that what is a file today can be
// applied through an API later without changing shape. The design is
// specs/008-readers.md and specs/016-distribution.md.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/lectio/reader"
	"latere.ai/x/lectio/reader/chat"
	"latere.ai/x/lectio/reader/layout"
	"latere.ai/x/lectio/reader/stub"
)

// APIVersion is the version every document declares.
const APIVersion = "lectio.latere.ai/v1"

// Settings are the settings of the process.
type Settings struct {
	Addr     string // LECTIO_ADDR
	BasePath string // LECTIO_BASE_PATH

	// Dev runs everything in the process and keeps nothing: the memory
	// store, the in-process runner, and one token. DevToken is that token.
	Dev      bool   // LECTIO_DEV
	DevToken string // LECTIO_DEV_TOKEN

	DatabaseURL string // LECTIO_DATABASE_URL
	ConfigPath  string // LECTIO_CONFIG

	// ModelKey is the key every reader is called with.
	ModelKey reader.Credential // LECTIO_MODEL_KEY

	MaxFileBytes int64         // LECTIO_MAX_FILE_BYTES
	MaxPages     int           // LECTIO_MAX_PAGES
	Workers      int           // LECTIO_WORKERS
	Attempts     int           // LECTIO_TASK_ATTEMPTS
	MaxDeadline  time.Duration // LECTIO_MAX_DEADLINE
	Grace        time.Duration // LECTIO_SHUTDOWN_GRACE

	// FetchAllow lists the hosts a source URL may name whatever they
	// resolve to.
	FetchAllow []string // LECTIO_FETCH_ALLOW
}

// FromEnv reads the settings. getenv is os.Getenv, or a test's own. A
// value that does not parse is an error that names its variable, and never
// the value: a variable may hold a secret by mistake.
func FromEnv(getenv func(string) string) (Settings, error) {
	s := Settings{
		Addr: ":8080", BasePath: "/v1", DevToken: "dev",
		MaxFileBytes: 256 << 20, MaxPages: 3000, Workers: 8, Attempts: 5,
		MaxDeadline: time.Hour, Grace: 25 * time.Second,
	}
	var errs []error
	text := func(name string, into *string) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			*into = v
		}
	}
	number := func(name string, into *int64) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 {
				errs = append(errs, fmt.Errorf("%s is not a number above zero", name))
				return
			}
			*into = n
		}
	}
	duration := func(name string, into *time.Duration) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("%s is not a duration above zero, such as 30s", name))
				return
			}
			*into = d
		}
	}

	text("LECTIO_ADDR", &s.Addr)
	text("LECTIO_BASE_PATH", &s.BasePath)
	text("LECTIO_DEV_TOKEN", &s.DevToken)
	text("LECTIO_DATABASE_URL", &s.DatabaseURL)
	text("LECTIO_CONFIG", &s.ConfigPath)
	if v := strings.TrimSpace(getenv("LECTIO_DEV")); v != "" {
		dev, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, errors.New("LECTIO_DEV is not true or false"))
		}
		s.Dev = dev
	}
	if !strings.HasPrefix(s.BasePath, "/") {
		errs = append(errs, errors.New("LECTIO_BASE_PATH does not begin with a slash"))
	}
	if key := strings.TrimSpace(getenv("LECTIO_MODEL_KEY")); key != "" {
		s.ModelKey = reader.NewCredential(key)
	}
	pagesMax, workers, attempts := int64(s.MaxPages), int64(s.Workers), int64(s.Attempts)
	number("LECTIO_MAX_FILE_BYTES", &s.MaxFileBytes)
	number("LECTIO_MAX_PAGES", &pagesMax)
	number("LECTIO_WORKERS", &workers)
	number("LECTIO_TASK_ATTEMPTS", &attempts)
	s.MaxPages, s.Workers, s.Attempts = int(pagesMax), int(workers), int(attempts)
	duration("LECTIO_MAX_DEADLINE", &s.MaxDeadline)
	duration("LECTIO_SHUTDOWN_GRACE", &s.Grace)
	for host := range strings.SplitSeq(getenv("LECTIO_FETCH_ALLOW"), ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			s.FetchAllow = append(s.FetchAllow, host)
		}
	}
	return s, errors.Join(errs...)
}

// document is one declared object.
type document struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec yaml.Node `yaml:"spec"`
}

// readerSpec is the spec of a Reader.
type readerSpec struct {
	Adapter  string `yaml:"adapter"`
	Endpoint string `yaml:"endpoint"`
	Model    string `yaml:"model"`
	Timeout  string `yaml:"timeout"`
	Image    struct {
		DPI      int    `yaml:"dpi"`
		LongEdge int    `yaml:"longEdge"`
		Format   string `yaml:"format"`
	} `yaml:"image"`
	Constrained     bool `yaml:"constrained"`
	MaxOutputTokens int  `yaml:"maxOutputTokens"`

	// Capacity is the concern of the durable runner, which shares a
	// reader's slots between workers. The in-process runner does not apply
	// these two.
	MaxInFlight       int `yaml:"maxInFlight"`
	RequestsPerMinute int `yaml:"requestsPerMinute"`
}

// policySpec is the spec of the Policy.
type policySpec struct {
	Read struct {
		Chain []string `yaml:"chain"`
	} `yaml:"read"`
	Extract struct {
		Chain []string `yaml:"chain"`
	} `yaml:"extract"`
	Escalate struct {
		OnInvalid int `yaml:"onInvalid"`
		Max       int `yaml:"max"`
	} `yaml:"escalate"`
}

// Readers is what the documents configure.
type Readers struct {
	// Readers are the readers by name. Chain is the order the policy tries
	// them in.
	Readers map[string]reader.Reader
	Chain   []string

	// Unapplied names what the documents set that this build reads and
	// does not act on, so a start can say so and not stay silent.
	Unapplied []string
}

// Stub is the configuration of a server with none: the stub reader, which
// calls no model.
func Stub() Readers {
	return Readers{Readers: map[string]reader.Reader{stub.Name: &stub.Reader{}}, Chain: []string{stub.Name}}
}

// Load reads the documents at path, a file or a directory of .yaml, .yml
// and .json files. Everything must hold: a document of an unknown kind or
// version, a member a spec does not have, a reader that cannot be built, a
// name used twice, and a chain that names a reader that is not there are
// each an error, and nothing is returned. A configuration that half loads
// would read pages with a model nobody chose.
func Load(path string) (Readers, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Readers{}, fmt.Errorf("config: %w", err)
	}
	files := []string{path}
	if info.IsDir() {
		files = nil
		for _, pattern := range []string{"*.yaml", "*.yml", "*.json"} {
			// The pattern is fixed and well formed, so Glob does not fail.
			found, _ := filepath.Glob(filepath.Join(path, pattern))
			files = append(files, found...)
		}
		slices.Sort(files)
	}

	out := Readers{Readers: map[string]reader.Reader{}}
	policies := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return Readers{}, fmt.Errorf("config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for n := 1; ; n++ {
			var doc document
			if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return Readers{}, fmt.Errorf("config: %s, document %d: %w", file, n, err)
			}
			where := fmt.Sprintf("config: %s, %s %q", file, doc.Kind, doc.Metadata.Name)
			if doc.APIVersion != APIVersion {
				return Readers{}, fmt.Errorf("%s: apiVersion is %q, want %s", where, doc.APIVersion, APIVersion)
			}
			switch doc.Kind {
			case "Reader":
				if _, taken := out.Readers[doc.Metadata.Name]; taken {
					return Readers{}, fmt.Errorf("%s: the name is used twice", where)
				}
				rd, unapplied, err := build(doc)
				if err != nil {
					return Readers{}, fmt.Errorf("%s: %w", where, err)
				}
				out.Readers[doc.Metadata.Name] = rd
				out.Unapplied = append(out.Unapplied, unapplied...)
			case "Policy":
				if policies++; policies > 1 {
					return Readers{}, fmt.Errorf("%s: there is one Policy, and this is the second", where)
				}
				var spec policySpec
				if err := strict(doc.Spec, &spec); err != nil {
					return Readers{}, fmt.Errorf("%s: %w", where, err)
				}
				out.Chain = spec.Read.Chain
				if len(spec.Extract.Chain) > 0 {
					out.Unapplied = append(out.Unapplied, "Policy extract.chain")
				}
				if spec.Escalate.OnInvalid != 0 || spec.Escalate.Max != 0 {
					out.Unapplied = append(out.Unapplied, "Policy escalate")
				}
			default:
				return Readers{}, fmt.Errorf("%s: the kind is not Reader or Policy", where)
			}
		}
	}

	if len(out.Readers) == 0 {
		return Readers{}, fmt.Errorf("config: %s declares no Reader", path)
	}
	if len(out.Chain) == 0 {
		// With no policy, one reader is the chain. With several, the order
		// would be a guess.
		if len(out.Readers) > 1 {
			return Readers{}, fmt.Errorf("config: %s declares %d readers and no Policy to order them", path, len(out.Readers))
		}
		for name := range out.Readers {
			out.Chain = []string{name}
		}
	}
	for _, name := range out.Chain {
		if out.Readers[name] == nil {
			return Readers{}, fmt.Errorf("config: the Policy's read chain names %q, which is no Reader", name)
		}
	}
	return out, nil
}

// strict decodes a spec and refuses a member the spec does not have.
func strict(node yaml.Node, into any) error {
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// build makes the reader a Reader document declares.
func build(doc document) (rd reader.Reader, unapplied []string, err error) {
	name := doc.Metadata.Name
	if name == "" {
		return nil, nil, errors.New("metadata.name is empty")
	}
	var spec readerSpec
	if err := strict(doc.Spec, &spec); err != nil {
		return nil, nil, err
	}
	var timeout time.Duration
	if spec.Timeout != "" {
		if timeout, err = time.ParseDuration(spec.Timeout); err != nil || timeout <= 0 {
			return nil, nil, errors.New("timeout is not a duration above zero, such as 120s")
		}
	}
	if spec.MaxInFlight != 0 || spec.RequestsPerMinute != 0 {
		unapplied = append(unapplied, fmt.Sprintf("Reader %q maxInFlight and requestsPerMinute", name))
	}
	img := reader.ImageSpec{DPI: spec.Image.DPI, LongEdge: spec.Image.LongEdge, Format: spec.Image.Format}

	switch spec.Adapter {
	case "chat":
		rd, err = chat.NewReader(chat.Config{
			Name: name, Endpoint: spec.Endpoint, Model: spec.Model, Image: img,
			Constrain: spec.Constrained, MaxOutputTokens: spec.MaxOutputTokens, Timeout: timeout,
		})
	case "layout":
		rd, err = layout.New(layout.Config{Name: name, Endpoint: spec.Endpoint, Image: img, Timeout: timeout})
	case "stub":
		rd = &stub.Reader{}
	default:
		err = fmt.Errorf("adapter is %q, want chat, layout or stub", spec.Adapter)
	}
	if err != nil {
		return nil, nil, err
	}
	return rd, unapplied, nil
}
