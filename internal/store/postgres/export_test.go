// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"time"
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
	doc, err := json.Marshal(submission{Submission: sub, DeadlineMS: sub.Deadline.Milliseconds()})
	return string(doc), err
}

// MigrationURL and Bound are the unexported helpers, for their own tests.
var (
	MigrationURL = migrationURL
	Bound        = bound
)
