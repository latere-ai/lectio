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

	"latere.ai/x/lectio/internal/tasks"
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

	// DatabaseURL is the direct endpoint of the database: migrations run
	// over it, because the migrator holds a session lock across its
	// statements. DatabasePoolURL is the endpoint the serving path opens, a
	// transaction-mode pooler's where an installation has one.
	DatabaseURL     string // LECTIO_DATABASE_URL
	DatabasePoolURL string // LECTIO_DATABASE_POOL_URL
	ConfigPath      string // LECTIO_CONFIG

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

	// FileRetention is how long a file is kept from an upload of it, and
	// past the end of the last parse that read it. ParseRetention is how
	// long a parse and what it wrote are kept after it ended. An allow
	// lowers either and never raises it
	// (specs/014-sources-and-retention.md).
	FileRetention  time.Duration // LECTIO_FILE_RETENTION
	ParseRetention time.Duration // LECTIO_PARSE_RETENTION

	// Role is what the durable server runs: the API, the worker, or both in
	// one process. InternalAddr is where it serves its probes.
	Role         string // LECTIO_ROLE
	InternalAddr string // LECTIO_INTERNAL_ADDR

	// The bucket the durable server keeps bytes in, the prefix every key is
	// written under, and how the bucket is reached.
	Bucket       string            // LECTIO_BUCKET
	BucketPrefix string            // LECTIO_BUCKET_PREFIX
	S3Endpoint   string            // LECTIO_S3_ENDPOINT
	S3Region     string            // LECTIO_S3_REGION
	S3AccessKey  string            // LECTIO_S3_ACCESS_KEY
	S3SecretKey  reader.Credential // LECTIO_S3_SECRET_KEY
	S3PathStyle  bool              // LECTIO_S3_PATH_STYLE

	// What the task store and a worker are run with
	// (specs/004-durable-tasks.md, specs/007-model-capacity.md). Flush is
	// the shortest time between two exchanges of one worker, and Poll the
	// time an idle worker waits before it asks again, which backs off to 5
	// times that.
	Lease         time.Duration // LECTIO_TASK_LEASE
	Expiries      int           // LECTIO_TASK_EXPIRIES
	SweepInterval time.Duration // LECTIO_SWEEP_INTERVAL
	Flush         time.Duration // LECTIO_WORKER_FLUSH
	Poll          time.Duration // LECTIO_WORKER_POLL
	PoolRecovery  time.Duration // LECTIO_POOL_RECOVERY
	PoolResume    time.Duration // LECTIO_POOL_RESUME

	// CacheBytes bounds the working copies a worker holds between the pages
	// it reads from them.
	CacheBytes int64 // LECTIO_CACHE_BYTES

	// ConverterURL is where the conversion sidecar listens. Empty runs
	// without one, and the formats that need conversion are refused.
	ConverterURL string // LECTIO_CONVERTER_URL

	// OIDCIssuers are the issuers whose tokens are accepted, each without
	// a trailing slash. OIDCAudiences are the audiences a token may be
	// addressed to, the first the primary one.
	OIDCIssuers   []string // LECTIO_OIDC_ISSUERS
	OIDCAudiences []string // LECTIO_OIDC_AUDIENCE

	// AuthorizerURL is the endpoint that is asked what a caller may do,
	// and AuthorizerToken the bearer it requires. With no URL the owner
	// policy decides, and AdminSubjects are the subjects it lets read
	// every owner's parses and files.
	AuthorizerURL   string            // LECTIO_AUTHORIZER_URL
	AuthorizerToken reader.Credential // LECTIO_AUTHORIZER_TOKEN
	AdminSubjects   []string          // LECTIO_ADMIN_SUBJECTS
}

// FromEnv reads the settings. getenv is os.Getenv, or a test's own. A
// value that does not parse is an error that names its variable, and never
// the value: a variable may hold a secret by mistake.
func FromEnv(getenv func(string) string) (Settings, error) {
	s := Settings{
		Addr: ":8080", BasePath: "/v1", DevToken: "dev",
		MaxFileBytes: 256 << 20, MaxPages: 3000, Workers: 8, Attempts: tasks.DefaultAttempts,
		MaxDeadline: time.Hour, Grace: 25 * time.Second,
		FileRetention: DefaultFileRetention, ParseRetention: DefaultParseRetention,
		Role: RoleAll, InternalAddr: ":8081",
		Lease: tasks.DefaultLease, Expiries: tasks.DefaultExpiries, SweepInterval: tasks.DefaultSweepInterval,
		Flush: 200 * time.Millisecond, Poll: time.Second,
		PoolRecovery: tasks.DefaultPoolRecovery, PoolResume: tasks.DefaultPoolResume,
		CacheBytes: 2 << 30,
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
	s.DatabaseURL = strings.TrimSpace(getenv("LECTIO_DATABASE_URL"))
	s.DatabasePoolURL = strings.TrimSpace(getenv("LECTIO_DATABASE_POOL_URL"))
	text("LECTIO_CONFIG", &s.ConfigPath)
	text("LECTIO_CONVERTER_URL", &s.ConverterURL)
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
	duration("LECTIO_FILE_RETENTION", &s.FileRetention)
	duration("LECTIO_PARSE_RETENTION", &s.ParseRetention)
	truth := func(name string, into *bool) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s is not true or false", name))
				return
			}
			*into = b
		}
	}
	text("LECTIO_ROLE", &s.Role)
	if s.Role != RoleAPI && s.Role != RoleWorker && s.Role != RoleAll {
		errs = append(errs, errors.New("LECTIO_ROLE is not api, worker or all"))
	}
	text("LECTIO_INTERNAL_ADDR", &s.InternalAddr)
	text("LECTIO_BUCKET", &s.Bucket)
	text("LECTIO_BUCKET_PREFIX", &s.BucketPrefix)
	text("LECTIO_S3_ENDPOINT", &s.S3Endpoint)
	text("LECTIO_S3_REGION", &s.S3Region)
	text("LECTIO_S3_ACCESS_KEY", &s.S3AccessKey)
	if key := strings.TrimSpace(getenv("LECTIO_S3_SECRET_KEY")); key != "" {
		s.S3SecretKey = reader.NewCredential(key)
	}
	truth("LECTIO_S3_PATH_STYLE", &s.S3PathStyle)
	expiries := int64(s.Expiries)
	number("LECTIO_TASK_EXPIRIES", &expiries)
	s.Expiries = int(expiries)
	number("LECTIO_CACHE_BYTES", &s.CacheBytes)
	duration("LECTIO_TASK_LEASE", &s.Lease)
	duration("LECTIO_SWEEP_INTERVAL", &s.SweepInterval)
	duration("LECTIO_WORKER_FLUSH", &s.Flush)
	duration("LECTIO_WORKER_POLL", &s.Poll)
	duration("LECTIO_POOL_RECOVERY", &s.PoolRecovery)
	duration("LECTIO_POOL_RESUME", &s.PoolResume)
	// One key source is built: the key of LECTIO_MODEL_KEY for every group.
	if keys := strings.TrimSpace(getenv("LECTIO_KEYS")); keys != "" && keys != "static" {
		errs = append(errs, errors.New("LECTIO_KEYS is not static, the one key source this build has"))
	}
	for host := range strings.SplitSeq(getenv("LECTIO_FETCH_ALLOW"), ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			s.FetchAllow = append(s.FetchAllow, host)
		}
	}
	errs = append(errs, s.readIdentity(getenv)...)
	return s, errors.Join(errs...)
}

// How long what is stored is kept when no setting says otherwise.
const (
	// DefaultFileRetention is a file's: 24 hours.
	DefaultFileRetention = 24 * time.Hour
	// DefaultParseRetention is a parse's: 30 days.
	DefaultParseRetention = 30 * 24 * time.Hour
)

// The roles of the durable server.
const (
	RoleAPI    = "api"
	RoleWorker = "worker"
	RoleAll    = "all"
)

// DefaultMaxInFlight is the bound of a reader's calls in flight across the
// fleet when its document names none: the slots of one worker process.
const DefaultMaxInFlight = 8

// Queue is what the task store is opened with: the settings of the process
// and the readers the documents declare, each with its pool.
func (s Settings) Queue(r Readers) tasks.Settings {
	return tasks.Settings{
		Lease: s.Lease, SweepInterval: s.SweepInterval, Attempts: s.Attempts, Expiries: s.Expiries,
		PoolRecovery: s.PoolRecovery, PoolResume: s.PoolResume,
		Pools: r.Pools, ReadChain: r.Chain,
	}
}

// ServingURL is the URL the store's pool opens: the pooled endpoint where
// one is named, and the direct one otherwise.
func (s Settings) ServingURL() string {
	if s.DatabasePoolURL != "" {
		return s.DatabasePoolURL
	}
	return s.DatabaseURL
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

	// What differs from one model family to the next, for the chat
	// adapter: how the model is asked for boxes, whether a temperature is
	// sent at all, and the name the output bound is sent under.
	Boxes struct {
		Order string `yaml:"order"`
		Space string `yaml:"space"`
	} `yaml:"boxes"`
	Temperature      *float64 `yaml:"temperature"`
	OutputLimitParam string   `yaml:"outputLimitParam"`

	// What the durable control plane reads and the in-process runner does
	// not apply: how many calls a reader may have in flight, and what a
	// page read by it costs against a tenant's share.
	MaxInFlight int     `yaml:"maxInFlight"`
	Cost        float64 `yaml:"cost"`
}

// policySpec is the spec of the Policy.
type policySpec struct {
	Read struct {
		Chain []string `yaml:"chain"`
	} `yaml:"read"`
	Extract struct {
		Chain []string `yaml:"chain"`
	} `yaml:"extract"`
	Describe struct {
		Chain []string `yaml:"chain"`
	} `yaml:"describe"`
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

	// Describers are the Reader documents whose adapter can also say what
	// a figure shows, by the same names. DescribeChain is the order the
	// policy tries them in; it is empty when the policy names none, and
	// then a figure is described only by a describer a request names.
	Describers    map[string]reader.Describer
	DescribeChain []string

	// Pools are the readers as the task store sees them: each with the
	// bound of its calls in flight and the cost of one call.
	Pools []tasks.Pool

	// Unapplied names what the documents set that this build reads and
	// does not act on, so a start can say so and not stay silent.
	// RunnerUnapplied names what the task store applies and the in-process
	// runner of a development server does not.
	Unapplied       []string
	RunnerUnapplied []string
}

// Stub is the configuration of a server with none: the stub reader, which
// calls no model.
func Stub() Readers {
	return Readers{
		Readers: map[string]reader.Reader{stub.Name: &stub.Reader{}}, Chain: []string{stub.Name},
		Describers: map[string]reader.Describer{stub.Name: &stub.Describer{}}, DescribeChain: []string{stub.Name},
		Pools: []tasks.Pool{{Reader: stub.Name, MaxInFlight: DefaultMaxInFlight, Cost: 1}},
	}
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

	out := Readers{Readers: map[string]reader.Reader{}, Describers: map[string]reader.Describer{}}
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
				rd, describer, pool, unapplied, err := build(doc)
				if err != nil {
					return Readers{}, fmt.Errorf("%s: %w", where, err)
				}
				out.Readers[doc.Metadata.Name] = rd
				if describer != nil {
					out.Describers[doc.Metadata.Name] = describer
				}
				out.Pools = append(out.Pools, pool)
				out.RunnerUnapplied = append(out.RunnerUnapplied, unapplied...)
			case "Policy":
				if policies++; policies > 1 {
					return Readers{}, fmt.Errorf("%s: there is one Policy, and this is the second", where)
				}
				var spec policySpec
				if err := strict(doc.Spec, &spec); err != nil {
					return Readers{}, fmt.Errorf("%s: %w", where, err)
				}
				out.Chain, out.DescribeChain = spec.Read.Chain, spec.Describe.Chain
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
	for _, name := range out.DescribeChain {
		switch {
		case out.Readers[name] == nil:
			return Readers{}, fmt.Errorf("config: the Policy's describe chain names %q, which is no Reader", name)
		case out.Describers[name] == nil:
			return Readers{}, fmt.Errorf("config: the Policy's describe chain names %q, whose adapter reads pages and cannot describe a figure", name)
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

// build makes the reader a Reader document declares, and the describer
// when the document's adapter can say what a figure shows: one that
// reaches a model that takes an instruction can, and one that reaches a
// layout engine with a contract of its own cannot. pool is the reader as the
// task store sees it.
func build(doc document) (rd reader.Reader, describer reader.Describer, pool tasks.Pool, unapplied []string, err error) {
	name := doc.Metadata.Name
	if name == "" {
		return nil, nil, pool, nil, errors.New("metadata.name is empty")
	}
	var spec readerSpec
	if err := strict(doc.Spec, &spec); err != nil {
		return nil, nil, pool, nil, err
	}
	var timeout time.Duration
	if spec.Timeout != "" {
		if timeout, err = time.ParseDuration(spec.Timeout); err != nil || timeout <= 0 {
			return nil, nil, pool, nil, errors.New("timeout is not a duration above zero, such as 120s")
		}
	}
	if spec.MaxInFlight != 0 || spec.Cost != 0 {
		unapplied = append(unapplied, fmt.Sprintf("Reader %q maxInFlight and cost", name))
	}
	// The fair queue counts in whole units, so a cost is a whole number: a
	// fraction would be charged as another cost than the one written.
	pool = tasks.Pool{Reader: name, MaxInFlight: spec.MaxInFlight, Cost: int(spec.Cost)}
	switch {
	case spec.MaxInFlight < 0:
		return nil, nil, pool, nil, errors.New("maxInFlight is below zero")
	case spec.Cost < 0 || spec.Cost != float64(pool.Cost):
		return nil, nil, pool, nil, errors.New("cost is not a whole number of units, 1 or more")
	case spec.MaxInFlight == 0:
		pool.MaxInFlight = DefaultMaxInFlight
	}
	if pool.Cost == 0 {
		pool.Cost = 1
	}
	img := reader.ImageSpec{DPI: spec.Image.DPI, LongEdge: spec.Image.LongEdge, Format: spec.Image.Format}

	switch spec.Adapter {
	case "chat":
		cfg := chat.Config{
			Name: name, Endpoint: spec.Endpoint, Model: spec.Model, Image: img,
			Constrain: spec.Constrained, MaxOutputTokens: spec.MaxOutputTokens, Timeout: timeout,
			Boxes:       chat.Boxes{Order: spec.Boxes.Order, Space: spec.Boxes.Space},
			Temperature: spec.Temperature, OutputLimit: spec.OutputLimitParam,
		}
		if rd, err = chat.NewReader(cfg); err == nil {
			// The same configuration reaches the same model for a figure.
			describer, err = chat.NewDescriber(cfg)
		}
	case "layout":
		rd, err = layout.New(layout.Config{Name: name, Endpoint: spec.Endpoint, Image: img, Timeout: timeout})
	case "stub":
		rd, describer = &stub.Reader{}, &stub.Describer{}
	default:
		err = fmt.Errorf("adapter is %q, want chat, layout or stub", spec.Adapter)
	}
	if err != nil {
		return nil, nil, pool, nil, err
	}
	return rd, describer, pool, unapplied, nil
}
