// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package readerset

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lectio/internal/config"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/store/postgres"
	"latere.ai/x/lectio/internal/tasks"
)

// memory is a Store in memory: what is stored, its version, and the
// settings last written.
type memory struct {
	mu       sync.Mutex
	readers  map[string]string
	policy   *string
	version  int64
	written  []tasks.Settings
	failRead error
	failSave error
	// races is how many writes find another change made after their check.
	races int
}

// moved reports whether a write checked at version finds what is stored
// changed, as another process's change in between would leave it.
func (m *memory) moved(version int64) bool {
	if m.races > 0 {
		m.races--
		m.version++
		return true
	}
	return version != m.version
}

func newMemory() *memory { return &memory{readers: map[string]string{}} }

func (m *memory) ReaderState(context.Context) (postgres.ReaderState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead != nil {
		return postgres.ReaderState{}, m.failRead
	}
	st := postgres.ReaderState{Version: m.version, Policy: m.policy}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range m.readers {
			if !yield(n) {
				return
			}
		}
	}) {
		st.Readers = append(st.Readers, postgres.StoredReader{Name: name, Spec: m.readers[name]})
	}
	return st, nil
}

func (m *memory) ReaderVersion(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version, m.failRead
}

func (m *memory) PutReader(_ context.Context, name, spec string, version int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moved(version) {
		return false, postgres.ErrMoved
	}
	_, had := m.readers[name]
	m.readers[name] = spec
	m.version++
	return !had, nil
}

func (m *memory) DeleteReader(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.readers[name]; !ok {
		return fault.New(fault.NotFound, "no stored reader is named %s", name)
	}
	delete(m.readers, name)
	m.version++
	return nil
}

func (m *memory) PutPolicy(_ context.Context, spec string, version int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moved(version) {
		return postgres.ErrMoved
	}
	m.policy = &spec
	m.version++
	return nil
}

func (m *memory) DeletePolicy(_ context.Context, version int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moved(version) {
		return postgres.ErrMoved
	}
	m.policy = nil
	m.version++
	return nil
}

func (m *memory) Configure(_ context.Context, s tasks.Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSave != nil {
		return m.failSave
	}
	m.written = append(m.written, s)
	return nil
}

func (m *memory) lastChains() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.written) == 0 {
		return nil
	}
	return m.written[len(m.written)-1].ReadChain
}

// manager is a manager over the stub configuration and m, settled.
func manager(t *testing.T, m *memory, log *bytes.Buffer) *Manager {
	t.Helper()
	mgr := &Manager{
		File:  config.StubFile().WithEndpoints([]string{"http://gateway.internal/v1"}),
		Queue: config.Settings{}.Queue, Store: m, Poll: 10 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(log, nil)),
	}
	st, err := m.ReaderState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Settle(context.Background(), st); err != nil && !errors.Is(err, postgres.ErrKeepSettings) {
		t.Fatal(err)
	}
	return mgr
}

const textReader = `{"adapter":"text"}`

// TestAChangeIsInForceAtOnceAndWrittenToTheQueue: storing a reader and a
// policy puts the set they make in force in the process that stored them,
// and writes its chains for the fair queue; removing them takes them out.
func TestAChangeIsInForceAtOnceAndWrittenToTheQueue(t *testing.T) {
	ctx := context.Background()
	m := newMemory()
	mgr := manager(t, m, &bytes.Buffer{})
	if mgr.Live().Current().Readers["stub"] == nil || mgr.String() != "readerset.Manager(0)" {
		t.Fatalf("the settled set: %+v", mgr.Live().Current())
	}
	if created, err := mgr.PutReader(ctx, "own", []byte(textReader)); err != nil || !created {
		t.Fatalf("storing a reader: %t, %v", created, err)
	}
	if mgr.Live().Current().Readers["own"] == nil || mgr.Live().Current().Sources["own"] != config.SourceAdded {
		t.Fatal("the stored reader is not in force")
	}
	if created, err := mgr.PutReader(ctx, "own", []byte(textReader)); err != nil || created {
		t.Fatalf("replacing a reader: %t, %v", created, err)
	}
	if err := mgr.PutPolicy(ctx, []byte(`{"read":{"chain":["own","stub"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`)); err != nil {
		t.Fatal(err)
	}
	if got := m.lastChains(); strings.Join(got, ",") != "own,stub" {
		t.Fatalf("the queue's read chain is %v", got)
	}
	if err := mgr.DeletePolicy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DeleteReader(ctx, "own"); err != nil {
		t.Fatal(err)
	}
	if set := mgr.Live().Current(); set.Readers["own"] != nil || strings.Join(set.Chain, ",") != "stub" {
		t.Fatalf("after the removal the set is %+v", set)
	}
}

// TestWhatWouldNotBuildIsRefusedAndChangesNothing: a configured reader's
// name is conflict, a spec that does not load and a policy that does not
// order the readers there are invalid_request, and none of them is
// stored.
func TestWhatWouldNotBuildIsRefusedAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	m := newMemory()
	mgr := manager(t, m, &bytes.Buffer{})
	for name, err := range map[string]error{
		"a configured name":    first(mgr.PutReader(ctx, "stub", []byte(textReader))),
		"removing configured":  mgr.DeleteReader(ctx, "stub"),
		"a spec that is none":  first(mgr.PutReader(ctx, "own", []byte(`{"adapter":"vision"}`))),
		"a policy naming none": mgr.PutPolicy(ctx, []byte(`{"read":{"chain":["gone"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`)),
		"an empty read chain":  mgr.PutPolicy(ctx, []byte(`{"describe":{"chain":["stub"]}}`)),
		"removing what is not": mgr.DeleteReader(ctx, "gone"),
	} {
		want := fault.InvalidRequest
		switch name {
		case "a configured name", "removing configured":
			want = fault.Conflict
		case "removing what is not":
			want = fault.NotFound
		}
		if fault.CodeOf(err) != want {
			t.Errorf("%s: %v, want %s", name, err, want)
		}
	}
	if m.version != 0 {
		t.Fatalf("a refused change was stored: version %d", m.version)
	}

	// A reader the stored policy names in its describe chain is not
	// replaced by one that cannot describe.
	if _, err := mgr.PutReader(ctx, "own", []byte(`{"adapter":"chat","model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PutPolicy(ctx, []byte(`{"read":{"chain":["stub"]},"describe":{"chain":["own"]},"extract":{"chain":["stub"]}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.PutReader(ctx, "own", []byte(textReader)); fault.CodeOf(err) != fault.InvalidRequest {
		t.Fatalf("a replacement that would break the policy: %v", err)
	}
	if mgr.Live().Current().Describers["own"] == nil {
		t.Fatal("the refused replacement is in force")
	}
}

// TestEveryProcessFollowsTheVersion: a process that did not make a change
// reads it within its poll; a stored set that does not build is logged once,
// and the set in force stays; a read that fails changes nothing.
func TestEveryProcessFollowsTheVersion(t *testing.T) {
	ctx := t.Context()
	m := newMemory()
	var log syncBuffer
	other := manager(t, m, &bytes.Buffer{})
	other.Log = slog.New(slog.NewTextHandler(&log, nil))
	go other.Run(ctx)
	writer := manager(t, m, &bytes.Buffer{})
	if _, err := writer.PutReader(ctx, "own", []byte(textReader)); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return other.Live().Current().Readers["own"] != nil })

	// A spec stored by a newer binary, which this one cannot build.
	m.mu.Lock()
	m.readers["newer"] = `{"adapter":"chat","model":"m","newMember":1}`
	m.version++
	m.mu.Unlock()
	until(t, func() bool { return strings.Contains(log.String(), "do not build") })
	if other.Live().Current().Readers["own"] == nil {
		t.Fatal("a set that does not build replaced the one in force")
	}
	if err := other.Reload(ctx); err == nil {
		t.Fatal("a reload of a set that does not build succeeded")
	}

	m.mu.Lock()
	m.failRead = errors.New("the database is away")
	m.mu.Unlock()
	if err := other.Check(ctx); err == nil {
		t.Fatal("a check that could not read succeeded")
	}
	until(t, func() bool { return strings.Contains(log.String(), "were not checked") })
}

// TestAStoredSetThatDoesNotBuildAtStartLeavesTheConfiguredOne: a process
// that starts beside a stored set it cannot build reads with the configured
// readers alone and says so.
func TestAStoredSetThatDoesNotBuildAtStartLeavesTheConfiguredOne(t *testing.T) {
	m := newMemory()
	m.readers["broken"] = `{"adapter":"vision"}`
	var log bytes.Buffer
	mgr := manager(t, m, &log)
	if set := mgr.Live().Current(); set.Readers["broken"] != nil || set.Readers["stub"] == nil {
		t.Fatalf("the set at start: %+v", set)
	}
	if !strings.Contains(log.String(), "the configured ones alone are in force") {
		t.Fatalf("the log: %s", log.String())
	}
	// The settings the processes that build it wrote are kept.
	if _, err := mgr.Settle(context.Background(), postgres.ReaderState{Readers: []postgres.StoredReader{{Name: "broken", Spec: `{"adapter":"vision"}`}}}); !errors.Is(err, postgres.ErrKeepSettings) {
		t.Fatalf("a process that cannot build the stored set answers %v", err)
	}
	wrapped := &Manager{File: config.StubFile(), Queue: config.Settings{}.Queue, Wrap: func(r config.Readers) config.Readers {
		r.Chain = append(r.Chain, "wrapped")
		return r
	}}
	if _, err := wrapped.Settle(context.Background(), postgres.ReaderState{}); err != nil || wrapped.Live().Current().Chain[1] != "wrapped" || wrapped.log() == nil {
		t.Fatalf("a wrapped set: %v", err)
	}
}

func first[T any](_ T, err error) error { return err }

func until(t *testing.T, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("it never happened")
		}
	}
}

// syncBuffer is a log a case reads while a manager writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestAStoreThatFailsChangesNothingInForce: a change whose store cannot be
// read is refused, and one whose settings cannot be written leaves the set
// in force as it was.
func TestAStoreThatFailsChangesNothingInForce(t *testing.T) {
	ctx := context.Background()
	m := newMemory()
	mgr := manager(t, m, &bytes.Buffer{})
	away := errors.New("the database is away")
	m.failRead = away
	for name, err := range map[string]error{
		"storing a reader":  first(mgr.PutReader(ctx, "own", []byte(textReader))),
		"setting a policy":  mgr.PutPolicy(ctx, []byte(`{"read":{"chain":["stub"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`)),
		"removing a policy": mgr.DeletePolicy(ctx),
	} {
		if !errors.Is(err, away) {
			t.Errorf("%s with a store that cannot be read: %v", name, err)
		}
	}
	m.failRead, m.failSave = nil, away
	if _, err := mgr.PutReader(ctx, "own", []byte(textReader)); !errors.Is(err, away) {
		t.Fatalf("a change whose settings could not be written: %v", err)
	}
	if mgr.Live().Current().Readers["own"] != nil {
		t.Fatal("a set whose settings were not written is in force")
	}
}

// TestAChangeMadeBesideAnotherIsCheckedAgain: a change whose check another
// process's change overtook is checked again against what is stored now,
// and one overtaken every time is conflict and changes nothing.
func TestAChangeMadeBesideAnotherIsCheckedAgain(t *testing.T) {
	ctx := context.Background()
	m := newMemory()
	mgr := manager(t, m, &bytes.Buffer{})
	m.races = 1
	if _, err := mgr.PutReader(ctx, "own", []byte(textReader)); err != nil || mgr.Live().Current().Readers["own"] == nil {
		t.Fatalf("a reader whose first check was overtaken: %v", err)
	}
	m.races = tries
	if err := mgr.DeletePolicy(ctx); fault.CodeOf(err) != fault.Conflict {
		t.Fatalf("a change overtaken every time: %v", err)
	}
}

// TestAShorterChainIsWrittenAgainAfterALease: a change that makes a chain
// shorter writes the settings once more after a lease and a sweep, with
// the set in force then, for the work that was claimed while it changed.
func TestAShorterChainIsWrittenAgainAfterALease(t *testing.T) {
	ctx := context.Background()
	m := newMemory()
	mgr := manager(t, m, &bytes.Buffer{})
	var waited time.Duration
	var later []func()
	mgr.Later = func(d time.Duration, f func()) { waited, later = d, append(later, f) }
	if _, err := mgr.PutReader(ctx, "own", []byte(textReader)); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PutPolicy(ctx, []byte(`{"read":{"chain":["own","stub"]},"describe":{"chain":["stub"]},"extract":{"chain":["stub"]}}`)); err != nil {
		t.Fatal(err)
	}
	if len(later) != 0 {
		t.Fatal("a longer chain was written again")
	}
	if err := mgr.DeletePolicy(ctx); err != nil {
		t.Fatal(err)
	}
	settings := config.Settings{}.Queue(*mgr.Live().Current()).WithDefaults()
	if len(later) != 1 || waited != settings.Lease+settings.SweepInterval {
		t.Fatalf("a shorter chain is written again %d times, after %v", len(later), waited)
	}
	written := len(m.written)
	later[0]()
	if len(m.written) != written+1 || strings.Join(m.lastChains(), ",") != "stub" {
		t.Fatalf("the second write: %d writes, chains %v", len(m.written)-written, m.lastChains())
	}
	m.failSave = errors.New("the database is away")
	later[0]()
}
