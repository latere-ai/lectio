// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"net/url"
	"testing"

	"latere.ai/x/lectio/internal/testservers"
)

// The suite runs against a real server, because what this package is for
// lives in Postgres and not in Go: the exchange is a function in the
// database, and its claims are true only of the server that runs it.
//
// One Postgres is started per test binary, and beside it one PgBouncer in
// transaction mode with prepared statements turned off, by
// internal/testservers. Every case gets a database of its own. Where no
// container runtime answers, every case here skips and says so.

// TestMain removes what this binary started, whatever the suite did, so a
// failed run leaves no container and no network behind.
func TestMain(m *testing.M) { testservers.Main(m) }

// servers is what the suite runs against.
type servers = testservers.Postgres

// server is the Postgres every case runs against, started once per binary.
func server(t testing.TB) servers {
	t.Helper()
	srv, err := testservers.StartPostgres()
	if err != nil {
		t.Skipf("no container runtime answered, so the Postgres suite did not run: %v", err)
	}
	return srv
}

// database creates an empty database on the server and returns its direct
// URL. The database is dropped when the case ends.
func database(t testing.TB, srv servers) string {
	t.Helper()
	return testservers.Database(t, srv)
}

// onDatabase is a server URL pointed at another database.
func onDatabase(t testing.TB, server, name string) string {
	t.Helper()
	return testservers.OnDatabase(t, server, name)
}

// nameOf is the database a URL points at.
func nameOf(t testing.TB, dsn string) string {
	t.Helper()
	return testservers.NameOf(t, dsn)
}

// execMode is dsn with the query mode in which no statement is prepared or
// described, and every parameter is encoded from its Go type and sent as
// text. It is the one mode that shows how a parameter is bound: a JSON
// document bound as bytes is refused in it and accepted in every other.
func execMode(t testing.TB, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("the database URL: %v", err)
	}
	q := u.Query()
	q.Set("default_query_exec_mode", "exec")
	u.RawQuery = q.Encode()
	return u.String()
}
