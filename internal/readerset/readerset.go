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
	PutReader(ctx context.Context, name, spec string) (created bool, err error)
	DeleteReader(ctx context.Context, name string) error
	PutPolicy(ctx context.Context, spec string) error
	DeletePolicy(ctx context.Context) error
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

	live *config.Live

	// mu holds one change or reload of this process at a time.
	mu      sync.Mutex
	version int64
}

// Settle builds the set from what is stored, puts it in force and answers
// the settings the store opens with: postgres.Options.Readers. A stored
// set that does not build is logged, and the configured documents alone
// are in force until one does.
func (m *Manager) Settle(_ context.Context, stored postgres.ReaderState) (tasks.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, err := m.build(stored)
	if err != nil {
		m.log().Error("the stored readers do not build; the configured ones alone are in force", "error", err)
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
	m.live.Set(set)
	m.version = stored.Version
	m.log().InfoContext(ctx, "readers in force", "version", stored.Version, "readers", slices.Sorted(maps.Keys(set.Readers)),
		"read", set.Chain, "describe", set.DescribeChain, "extract", set.ExtractChain, "policy", set.PolicySource)
	return nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.Store.ReaderState(ctx)
	if err != nil {
		return false, err
	}
	added, policy := addedOf(stored)
	added = slices.DeleteFunc(added, func(a config.Added) bool { return a.Name == name })
	added = append(added, config.Added{Name: name, Spec: spec})
	if _, err := m.File.Build(added, policy); err != nil {
		return false, invalid("spec", err)
	}
	if created, err = m.Store.PutReader(ctx, name, string(spec)); err != nil {
		return false, err
	}
	return created, m.reload(ctx)
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
// a reader in each chain, and puts it in force in this process at once.
func (m *Manager) PutPolicy(ctx context.Context, spec []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.Store.ReaderState(ctx)
	if err != nil {
		return err
	}
	added, _ := addedOf(stored)
	if err := m.File.CheckPolicy(added, spec); err != nil {
		return invalid("policy", err)
	}
	if err := m.Store.PutPolicy(ctx, string(spec)); err != nil {
		return err
	}
	return m.reload(ctx)
}

// DeletePolicy removes the stored policy, once the configured one orders
// the readers there are, and puts it in force in this process at once.
func (m *Manager) DeletePolicy(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.Store.ReaderState(ctx)
	if err != nil {
		return err
	}
	added, _ := addedOf(stored)
	if _, err := m.File.Build(added, nil); err != nil {
		return invalid("policy", err)
	}
	if err := m.Store.DeletePolicy(ctx); err != nil {
		return err
	}
	return m.reload(ctx)
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
