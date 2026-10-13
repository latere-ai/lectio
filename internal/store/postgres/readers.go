// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/tasks"
)

// Readers at runtime (specs/019-readers-at-runtime.md): the readers and the
// routing policy stored through the API, which every process builds beside
// its configuration. The store keeps them as they were sent and does not
// read them; what they may hold is the caller's to check.

const (
	readerStateSQL    = `SELECT lectio_reader_state()`
	readerVersionSQL  = `SELECT version FROM reader_policy`
	readerPutSQL      = `SELECT lectio_reader_put($1, $2)`
	readerPutAtSQL    = `SELECT lectio_reader_put($1, $2, $3)`
	readerDeleteSQL   = `SELECT lectio_reader_delete($1)`
	readerDeleteAtSQL = `SELECT lectio_reader_delete($1, $2)`
	policyPutSQL      = `SELECT lectio_policy_put($1)`
	policyPutAtSQL    = `SELECT lectio_policy_put($1, $2)`
	policyDeleteSQL   = `SELECT lectio_policy_put(NULL)`
	policyDeleteAtSQL = `SELECT lectio_policy_put(NULL, $1)`
)

// ReaderState is what is stored: its version, the policy's spec, empty
// while the configured policy is in force, and the readers by name.
type ReaderState struct {
	Version int64          `json:"version"`
	Policy  *string        `json:"policy"`
	Readers []StoredReader `json:"readers"`
}

// StoredReader is one reader stored through the API: its name and its spec,
// one JSON object as it was sent.
type StoredReader struct {
	Name string `json:"name"`
	Spec string `json:"spec"`
}

// ReaderState reads what is stored.
func (s *Store) ReaderState(ctx context.Context) (ReaderState, error) {
	var out ReaderState
	if err := s.decode(ctx, &out, readerStateSQL, ""); err != nil {
		return ReaderState{}, fmt.Errorf("store: reading the stored readers: %w", err)
	}
	return out, nil
}

// ReaderVersion is the version of what is stored, which every change adds
// 1 to. A process that sees it change reads ReaderState again.
func (s *Store) ReaderVersion(ctx context.Context) (int64, error) {
	var v int64
	if err := s.pool.QueryRow(ctx, readerVersionSQL).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: reading the version of the stored readers: %w", err)
	}
	return v, nil
}

// PutReader stores a reader's spec under its name, replacing the one there,
// and reports whether the name was new.
func (s *Store) PutReader(ctx context.Context, name, spec string) (created bool, err error) {
	answer, err := s.text(ctx, readerPutSQL, readerPutAtSQL, name, spec)
	if err != nil {
		return false, fmt.Errorf("store: storing the reader %s: %w", name, err)
	}
	return answer == "created", nil
}

// DeleteReader removes a stored reader. A name no stored reader has is
// refused with not_found; with conflict, a reader a chain in force
// or the stored policy names, one a set's definition names, and one that a
// parse, a task or an extraction that has not ended names, which would
// wait for a reader nobody runs.
func (s *Store) DeleteReader(ctx context.Context, name string) error {
	answer, err := s.text(ctx, readerDeleteSQL, readerDeleteAtSQL, name)
	if err != nil {
		return fmt.Errorf("store: removing the reader %s: %w", name, err)
	}
	switch answer {
	case "missing":
		return fault.New(fault.NotFound, "no stored reader is named %s", name)
	case "chained":
		return fault.New(fault.Conflict, "the routing policy names the reader %s", name)
	case "set":
		return fault.New(fault.Conflict, "a set's definition names the reader %s", name)
	case "busy":
		return fault.New(fault.Conflict, "work that has not ended names the reader %s", name)
	case "deleted":
		return nil
	}
	return fmt.Errorf("store: removing the reader %s answered %q", name, answer)
}

// PutPolicy stores the routing policy's spec, which is in force in the
// place of the configured one.
func (s *Store) PutPolicy(ctx context.Context, spec string) error {
	if _, err := s.text(ctx, policyPutSQL, policyPutAtSQL, spec); err != nil {
		return fmt.Errorf("store: storing the routing policy: %w", err)
	}
	return nil
}

// DeletePolicy removes the stored routing policy, so the configured one is
// in force again.
func (s *Store) DeletePolicy(ctx context.Context) error {
	if _, err := s.text(ctx, policyDeleteSQL, policyDeleteAtSQL); err != nil {
		return fmt.Errorf("store: removing the routing policy: %w", err)
	}
	return nil
}

// Configure writes the settings, as Open does: a process whose readers
// changed writes its pools and its chains, and the newest write is in
// force.
func (s *Store) Configure(ctx context.Context, settings tasks.Settings) error {
	settings = settings.WithDefaults()
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return s.prepare(ctx, settings, nil)
}
