// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lectio/internal/testservers"
)

// childEnv marks a process a test started as a lectiod: the test binary is
// run again with it set, and TestMain runs the server and not the tests.
const childEnv = "LECTIO_TEST_PROCESS"

// child runs lectiod's own main, which is configured by the environment the
// test gave the process.
func child() {
	main()
}

// mustPostgres is the suite's Postgres, for a case that already has one.
func mustPostgres(t *testing.T) testservers.Postgres {
	t.Helper()
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Fatalf("the Postgres of the suite: %v", err)
	}
	return srv
}

// connect opens a direct connection to the plane's database, for what the
// API does not show: task rows, workers and counters.
func (p *plane) connect() *pgx.Conn {
	p.t.Helper()
	conn, err := pgx.Connect(context.Background(), p.dsn)
	if err != nil {
		p.t.Fatalf("connecting: %v", err)
	}
	p.t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}
