// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"time"

	"latere.ai/x/lectio/internal/tasks"
)

// SetClock makes every write of the store pass at as the database's now(),
// so a test moves leases, backoff, pauses and deadlines without sleeping.
func (s *Store) SetClock(at time.Time) { s.clock.Store(&at) }

// Decode runs a statement that answers one JSON document and reads it, the
// way every read of the store does.
func (s *Store) Decode(ctx context.Context, into any, sql string, args ...any) error {
	return s.decode(ctx, into, sql, "", args...)
}

// SubmitDocument is the document Submit sends to lectio_submit, for a test
// that queues many parses in one round trip.
func SubmitDocument(sub Submission) (string, error) {
	doc, err := json.Marshal(submission{
		Submission: sub, DeadlineMS: sub.Deadline.Milliseconds(), RetentionMS: sub.Retention.Milliseconds(),
	})
	return string(doc), err
}

// Reserve makes the one statement that promises pages of a group's day,
// outside an exchange, so a test runs many of them at once with nothing
// else serializing them.
func (s *Store) Reserve(ctx context.Context, group string, day time.Time, pages, limit int) (reserved bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT lectio_reserve($1, $2::date, $3, $4)`, group, day.Format(time.DateOnly), pages, limit).Scan(&reserved)
	return reserved, err
}

// Configure writes settings as a process that starts writes its own, for a
// case in which a second process comes up with another configuration.
func (s *Store) Configure(ctx context.Context, settings tasks.Settings) error {
	return s.prepare(ctx, settings.WithDefaults())
}

// MigrationURL and Bound are the unexported helpers, for their own tests.
var (
	MigrationURL = migrationURL
	Bound        = bound
)
