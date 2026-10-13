// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package readerset keeps a process's readers in force as the readers and
// the routing policy stored through the API change
// (specs/019-readers-at-runtime.md). The configured documents and what is
// stored are built into one set; the process reads it through a
// config.Live, follows the version of what is stored, and writes its pools
// and its chains to the task store whenever the set changes, so the fair
// queue dispatches to the readers the process runs.
package readerset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// DefaultPoll is how often a process checks whether what is stored
// changed, LECTIO_READERS_POLL when it is unset.
const DefaultPoll = 10 * time.Second

// Store is where the readers are stored and the queue's settings written:
// *postgres.Store.
type Store interface {
	ReaderState(ctx context.Context) (postgres.ReaderState, error)
	ReaderVersion(ctx context.Context) (int64, error)
	PutReader(ctx context.Context, name, spec string, version int64) (created bool, err error)
	DeleteReader(ctx context.Context, name string) error
	PutPolicy(ctx context.Context, spec string, version int64) error
	DeletePolicy(ctx context.Context, version int64) error
	Configure(ctx context.Context, settings tasks.Settings) error
}

// Manager builds and follows the set of one process.
type Manager struct {
	// File is the configured documents, with the endpoints a stored reader
	// may reach.
	File config.File
	// Queue is the queue's settings for a set: config.Settings.Queue.
	Queue func(config.Readers) tasks.Settings
	// Wrap, when set, is laid over every set built, as a test wraps the
	// readers it counts the calls of.
	Wrap func(config.Readers) config.Readers
	// Poll is how often the version is checked. Zero takes DefaultPoll.
	Poll time.Duration
	// Log takes a line for each set put in force and each one that could
	// not be built. Nil takes slog.Default.
	Log *slog.Logger

	// Store is set once the task store is open; Settle reads what is
	// stored through the store that is opening.
	Store Store

	// Later runs f after d: a chain made shorter is written again after a
	// lease, for the work that was claimed while it changed. Nil takes
	// time.AfterFunc.
	Later func(d time.Duration, f func())

	live *config.Live

	// mu holds one change or reload of this process at a time.
	mu      sync.Mutex
	version int64
}

// Settle builds the set from what is stored, puts it in force and answers
// the settings the store opens with: postgres.Options.Readers. A stored
// set that does not build, as a binary older than the one that stored it
// may find, is logged, and the configured documents alone are in force in
// this process until one does; the settings are not written, so the
// processes that can build it keep dispatching to it.
func (m *Manager) Settle(ctx context.Context, stored postgres.ReaderState) (tasks.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, err := m.build(stored)
	keep := err != nil
	if keep {
		m.log().ErrorContext(ctx, "the stored readers do not build; the configured ones alone are in force in this process", "error", err)
		if set, err = m.File.Build(nil, nil); err != nil {
			return tasks.Settings{}, err
		}
		set = m.wrap(set)
	}
	m.version = stored.Version
	if m.live == nil {
		m.live = config.NewLive(set)
	} else {
		m.live.Set(set)
	}
	if keep {
		return tasks.Settings{}, postgres.ErrKeepSettings
	}
	return m.Queue(set), nil
}

// Live is the set in force. It is nil until Settle has run.
func (m *Manager) Live() *config.Live { return m.live }

// Run checks the version every Poll until ctx ends, and reloads when it
// changed. A check that fails is logged and tried again at the next.
func (m *Manager) Run(ctx context.Context) {
	poll := m.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := m.check(ctx); err != nil && ctx.Err() == nil {
			m.log().WarnContext(ctx, "the stored readers were not checked; the set in force stays", "error", err)
		}
	}
}

// Check reloads when the version of what is stored moved: a worker asks it
// for a claim of a reader its set does not hold.
func (m *Manager) Check(ctx context.Context) error { return m.check(ctx) }

// check reloads when the version moved.
func (m *Manager) check(ctx context.Context) error {
	v, err := m.Store.ReaderVersion(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	moved := v != m.version
	m.mu.Unlock()
	if !moved {
		return nil
	}
	return m.Reload(ctx)
}

// Reload reads what is stored, puts the set it builds in force, and writes
// the queue's settings for it. A set that does not build leaves the one in
// force, and is the error.
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reload(ctx)
}

func (m *Manager) reload(ctx context.Context) error {
	stored, err := m.Store.ReaderState(ctx)
	if err != nil {
		return err
	}
	set, err := m.build(stored)
	if err != nil {
		// The version is taken, so a set that does not build is logged
		// once and not at every check; the next change is read again.
		m.version = stored.Version
		m.log().ErrorContext(ctx, "the stored readers do not build; the set in force stays", "version", stored.Version, "error", err)
		return err
	}
	if err := m.Store.Configure(ctx, m.Queue(set)); err != nil {
		return err
	}
	if shorter(m.live.Current(), &set) {
		m.again(ctx, m.Queue(set))
	}
	m.live.Set(set)
	m.version = stored.Version
	m.log().InfoContext(ctx, "readers in force", "version", stored.Version, "readers", slices.Sorted(maps.Keys(set.Readers)),
		"read", set.Chain, "describe", set.DescribeChain, "extract", set.ExtractChain, "policy", set.PolicySource)
	return nil
}

// shorter reports whether a chain of next is shorter than the one of was.
func shorter(was, next *config.Readers) bool {
	return len(next.Chain) < len(was.Chain) || len(next.DescribeChain) < len(was.DescribeChain) || len(next.ExtractChain) < len(was.ExtractChain)
}

// again writes the settings once more after a lease and a sweep: work that
// was claimed while a chain got shorter, and was put back at a place the
// chain no longer has, is moved to its last place then, as the work that
// waited was when the change was written. The set in force then is
// written, so a change made since is not undone.
func (m *Manager) again(ctx context.Context, settings tasks.Settings) {
	settings = settings.WithDefaults()
	later := m.Later
	if later == nil {
		later = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	later(settings.Lease+settings.SweepInterval, func() {
		// The write outlives the request that made the change.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := m.Store.Configure(ctx, m.Queue(*m.live.Current())); err != nil {
			m.log().WarnContext(ctx, "the settings were not written again after a chain got shorter", "error", err)
		}
	})
}

// build is the set of what is stored, laid under Wrap.
func (m *Manager) build(stored postgres.ReaderState) (config.Readers, error) {
	added, policy := addedOf(stored)
	set, err := m.File.Build(added, policy)
	if err != nil {
		return config.Readers{}, err
	}
	return m.wrap(set), nil
}

func (m *Manager) wrap(set config.Readers) config.Readers {
	if m.Wrap != nil {
		return m.Wrap(set)
	}
	return set
}

// addedOf is what is stored, as config.File.Build takes it.
func addedOf(stored postgres.ReaderState) ([]config.Added, []byte) {
	added := make([]config.Added, 0, len(stored.Readers))
	for _, r := range stored.Readers {
		added = append(added, config.Added{Name: r.Name, Spec: []byte(r.Spec)})
	}
	var policy []byte
	if stored.Policy != nil {
		policy = []byte(*stored.Policy)
	}
	return added, policy
}

// tries is how often a change is checked again when another process changed
// what is stored between its check and its write.
const tries = 3

// PutReader stores a reader once the set with it builds, and puts that set
// in force in this process at once; the others follow within Poll. A spec
// the set would not build with is invalid_request, and a configured
// reader's name conflict.
func (m *Manager) PutReader(ctx context.Context, name string, spec []byte) (created bool, err error) {
	if err := m.File.CheckReader(name, spec); errors.Is(err, config.ErrConfigured) {
		return false, fault.New(fault.Conflict, "the reader %s is declared by the deployment's configuration, which the API does not change", name)
	} else if err != nil {
		return false, invalid("spec", err)
	}
	err = m.change(ctx, func(stored postgres.ReaderState) error {
		added, policy := addedOf(stored)
		added = slices.DeleteFunc(added, func(a config.Added) bool { return a.Name == name })
		added = append(added, config.Added{Name: name, Spec: spec})
		if _, err := m.File.Build(added, policy); err != nil {
			return invalid("spec", err)
		}
		created, err = m.Store.PutReader(ctx, name, string(spec), stored.Version)
		return err
	})
	return created, err
}

// change checks and writes one change against what is stored, again when
// another process changed it in between, and puts the set it makes in
// force in this process.
func (m *Manager) change(ctx context.Context, write func(postgres.ReaderState) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range tries {
		stored, err := m.Store.ReaderState(ctx)
		if err != nil {
			return err
		}
		if err = write(stored); errors.Is(err, postgres.ErrMoved) {
			continue
		} else if err != nil {
			return err
		}
		return m.reload(ctx)
	}
	return fault.New(fault.Conflict, "the readers were changed by another request %d times while this one was checked; send it again", tries)
}

// DeleteReader removes a stored reader and puts the set without it in
// force in this process at once. A configured reader is conflict; the
// store refuses one that is named.
func (m *Manager) DeleteReader(ctx context.Context, name string) error {
	if m.File.Configured(name) {
		return fault.New(fault.Conflict, "the reader %s is declared by the deployment's configuration, which the API does not change", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.Store.DeleteReader(ctx, name); err != nil {
		return err
	}
	return m.reload(ctx)
}

// PutPolicy stores the routing policy once the set with it builds and names
// a reader in its read chain, and puts it in force in this process at once.
func (m *Manager) PutPolicy(ctx context.Context, spec []byte) error {
	return m.change(ctx, func(stored postgres.ReaderState) error {
		added, _ := addedOf(stored)
		if err := m.File.CheckPolicy(added, spec); err != nil {
			return invalid("policy", err)
		}
		return m.Store.PutPolicy(ctx, string(spec), stored.Version)
	})
}

// DeletePolicy removes the stored policy, once the configured one orders
// the readers there are, and puts it in force in this process at once.
func (m *Manager) DeletePolicy(ctx context.Context) error {
	return m.change(ctx, func(stored postgres.ReaderState) error {
		added, _ := addedOf(stored)
		if _, err := m.File.Build(added, nil); err != nil {
			return invalid("policy", err)
		}
		return m.Store.DeletePolicy(ctx, stored.Version)
	})
}

// invalid is a request the set would not build with: what of the body
// does not hold, and why.
func invalid(what string, err error) error {
	return fault.New(fault.InvalidRequest, "the %s does not load: %v", what, err)
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// String names the manager in a log line.
func (m *Manager) String() string { return fmt.Sprintf("readerset.Manager(%d)", m.version) }
