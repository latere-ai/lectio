// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The suite runs against a real server, because what this package is for
// lives in Postgres and not in Go: the exchange is a function in the
// database, and its claims are true only of the server that runs it.
//
// One Postgres is started per test binary, and beside it one PgBouncer in
// transaction mode with prepared statements turned off, on a network the two
// share. Every case gets a database of its own. Where no container runtime
// answers, every case here skips and says so.

const (
	postgresImage = "postgres:16-alpine" // the oldest server the design supports
	poolerImage   = "edoburu/pgbouncer:v1.24.1-p1"

	dbUser     = "lectio"
	dbPassword = "lectio"
	// dbAlias is the name the pooler reaches the server by on their network.
	dbAlias = "lectio-postgres"
)

// TestMain removes what this binary started, whatever the suite did, so a
// failed run leaves no container and no network behind.
func TestMain(m *testing.M) {
	code := m.Run()
	terminate()
	os.Exit(code)
}

// servers is what the suite runs against.
type servers struct {
	// direct is the URL of the server's maintenance database, and pooler the
	// same database reached through PgBouncer. poolerErr says why there is
	// no pooler when the server started and the pooler did not.
	direct    string
	pooler    string
	poolerErr error
}

var (
	once       sync.Once
	started    servers
	startErr   error
	containers []testcontainers.Container
	shared     *testcontainers.DockerNetwork
)

// server is the Postgres every case runs against, started once per binary.
func server(t testing.TB) servers {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Skipf("no container runtime answered, so the Postgres suite did not run: %v", startErr)
	}
	return started
}

func start() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	socket(ctx)

	nw, err := network.New(ctx)
	if err != nil {
		startErr = err
		return
	}
	shared = nw
	pg, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("lectio_admin"),
		tcpostgres.WithUsername(dbUser),
		tcpostgres.WithPassword(dbPassword),
		network.WithNetwork([]string{dbAlias}, nw),
		// The suite is not a durability test of Postgres: it trades the
		// server's own crash safety for speed. The connection bound holds
		// the pools of every case that runs at once.
		testcontainers.WithCmd("postgres", "-c", "fsync=off", "-c", "synchronous_commit=off",
			"-c", "full_page_writes=off", "-c", "max_connections=300"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(3*time.Minute)),
	)
	if pg != nil {
		containers = append(containers, pg)
	}
	if err != nil {
		startErr = err
		return
	}
	if started.direct, startErr = pg.ConnectionString(ctx, "sslmode=disable"); startErr != nil {
		return
	}

	// The pooler: transaction mode, and no support for prepared statements,
	// which is the strictest pooler a deployment may put in front. Any
	// database name is passed through to the server.
	pooler, err := testcontainers.Run(ctx, poolerImage,
		network.WithNetwork(nil, nw),
		testcontainers.WithExposedPorts("5432/tcp"),
		testcontainers.WithEnv(map[string]string{
			"DB_HOST": dbAlias, "DB_USER": dbUser, "DB_PASSWORD": dbPassword,
			"AUTH_TYPE": "scram-sha-256", "POOL_MODE": "transaction",
			"MAX_PREPARED_STATEMENTS": "0", "DEFAULT_POOL_SIZE": "8", "MAX_CLIENT_CONN": "1000",
		}),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute)),
	)
	if pooler != nil {
		containers = append(containers, pooler)
	}
	if err != nil {
		started.poolerErr = err
		return
	}
	host, err := pooler.Host(ctx)
	if err != nil {
		started.poolerErr = err
		return
	}
	port, err := pooler.MappedPort(ctx, "5432/tcp")
	if err != nil {
		started.poolerErr = err
		return
	}
	started.pooler = (&url.URL{
		Scheme: "postgres", User: url.UserPassword(dbUser, dbPassword),
		Host: net.JoinHostPort(host, port.Port()), Path: "/lectio_admin", RawQuery: "sslmode=disable",
	}).String()
}

func terminate() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, c := range containers {
		if err := c.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "removing a test container: %v\n", err)
		}
	}
	containers = nil
	if shared != nil {
		if err := shared.Remove(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "removing the test network: %v\n", err)
		}
		shared = nil
	}
}

// database creates an empty database on the server and returns its direct
// URL, so one case cannot read another's rows and the migrations run on
// each. The database is dropped when the case ends.
func database(t testing.TB, srv servers) string {
	t.Helper()
	name := "lectio_" + strings.ToLower(rand.Text()[:12])
	conn, err := pgx.Connect(context.Background(), srv.direct)
	if err != nil {
		t.Fatalf("connecting to the server: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("creating the database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drop, err := pgx.Connect(ctx, srv.direct)
		if err != nil {
			t.Errorf("connecting to drop the database: %v", err)
			return
		}
		defer func() { _ = drop.Close(ctx) }()
		if _, err := drop.Exec(ctx, "drop database if exists "+name+" with (force)"); err != nil {
			t.Errorf("dropping the database: %v", err)
		}
	})
	return onDatabase(t, srv.direct, name)
}

// onDatabase is a server URL pointed at another database.
func onDatabase(t testing.TB, server, name string) string {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatalf("the server URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// nameOf is the database a URL points at.
func nameOf(t testing.TB, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("the database URL: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/")
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

// socket points the container library at a Podman machine where no Docker
// socket answers, which is what a developer on macOS may have. Ryuk, the
// library's reaper container, does not start on rootless Podman, and
// TestMain removes what this binary started anyway.
func socket(ctx context.Context) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		reaperFor(strings.TrimPrefix(host, "unix://"))
		return
	}
	for _, path := range []string{"/var/run/docker.sock", filepath.Join(os.Getenv("HOME"), ".docker/run/docker.sock")} {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			reaperFor(path)
			return
		}
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probe, "podman", "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}").Output()
	if err != nil {
		return
	}
	path := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		return
	}
	setenv("DOCKER_HOST", "unix://"+path)
	setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
}

// reaperFor turns Ryuk off where the Docker socket that answers is a Podman
// machine's under another name: the link a Podman helper installs at
// /var/run/docker.sock is one, and Ryuk there asks for a bridge network the
// machine does not have, which would skip the whole suite.
func reaperFor(path string) {
	if onPodman(path) {
		setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// onPodman reports whether a socket path resolves into a Podman machine's
// directory.
func onPodman(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return strings.Contains(resolved, "podman")
}

func setenv(name, value string) {
	if err := os.Setenv(name, value); err != nil {
		fmt.Fprintf(os.Stderr, "setting %s: %v\n", name, err)
	}
}

// TestTheReaperIsOffBehindAPodmanLink: a Docker socket that is a link into a
// Podman machine's directory is Podman, and the reaper is turned off for it;
// a Docker socket of Docker's own leaves the reaper as the library sets it.
func TestTheReaperIsOffBehindAPodmanLink(t *testing.T) {
	dir := t.TempDir()
	machine := filepath.Join(dir, "podman", "machine")
	if err := os.MkdirAll(machine, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(machine, "podman.sock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "docker.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "docker", "docker.sock")
	if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "")
	reaperFor(own)
	if got := os.Getenv("TESTCONTAINERS_RYUK_DISABLED"); got != "" {
		t.Fatalf("Docker's own socket turned the reaper off: %q", got)
	}
	reaperFor(filepath.Join(dir, "absent.sock"))
	if got := os.Getenv("TESTCONTAINERS_RYUK_DISABLED"); got != "" {
		t.Fatalf("a socket that does not resolve turned the reaper off: %q", got)
	}
	reaperFor(link)
	if got := os.Getenv("TESTCONTAINERS_RYUK_DISABLED"); got != "true" {
		t.Fatalf("a link into a Podman machine left the reaper at %q", got)
	}
}
