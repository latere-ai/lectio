// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package testservers starts the servers the suites run against: one
// Postgres with a PgBouncer in transaction mode beside it, and an object
// store that speaks S3. What the durable control plane is for lives in
// those servers and not in Go, so its tests run against the real ones.
//
// Each server is started once per test binary, on first use, and Stop
// removes everything this binary started. A test binary calls Stop from its
// TestMain, so a failed run leaves no container and no network behind. Where
// no container runtime answers, the start functions return an error and the
// caller skips.
package testservers

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

const (
	postgresImage = "postgres:16-alpine" // the oldest server the design supports
	poolerImage   = "edoburu/pgbouncer:v1.24.1-p1"
	objectsImage  = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"

	dbUser     = "lectio"
	dbPassword = "lectio"
	// dbAlias is the name a pooler reaches the server by on their network.
	dbAlias = "lectio-postgres"
	// adminDB is the server's maintenance database. Every case gets a
	// database of its own beside it.
	adminDB = "lectio_admin"

	// poolSize is the pool of the pooler every suite shares.
	poolSize = 8

	objectsUser     = "lectio"
	objectsPassword = "lectio-secret"
	objectsBucket   = "lectio"
)

// Postgres is the database server a suite runs against.
type Postgres struct {
	// Direct is the URL of the server's maintenance database, and Pooler the
	// same database reached through PgBouncer in transaction mode with
	// prepared statements turned off, which is the strictest pooler a
	// deployment may put in front. PoolerErr says why there is no pooler
	// when the server started and the pooler did not.
	Direct    string
	Pooler    string
	PoolerErr error
}

// Objects is an object store that speaks S3, addressed in path style, with
// one empty bucket and the one key that may use it.
type Objects struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
}

var (
	mu         sync.Mutex
	containers []testcontainers.Container
	shared     *testcontainers.DockerNetwork

	postgresOnce sync.Once
	postgres     Postgres
	postgresErr  error

	objectsOnce sync.Once
	objects     Objects
	objectsErr  error

	runtimeOnce sync.Once
)

// keep records a container for Stop. A container that failed to start is
// still kept when the library returned one: it may hold a name or a port.
func keep(c testcontainers.Container) {
	if c == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	containers = append(containers, c)
}

// StartPostgres returns the Postgres of this test binary, starting it and
// its pooler on first use.
func StartPostgres() (Postgres, error) {
	postgresOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		postgres, postgresErr = startPostgres(ctx)
	})
	return postgres, postgresErr
}

func startPostgres(ctx context.Context) (Postgres, error) {
	runtime(ctx)
	nw, err := network.New(ctx)
	if err != nil {
		return Postgres{}, err
	}
	mu.Lock()
	shared = nw
	mu.Unlock()
	pg, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase(adminDB),
		tcpostgres.WithUsername(dbUser),
		tcpostgres.WithPassword(dbPassword),
		network.WithNetwork([]string{dbAlias}, nw),
		// The suites are not a durability test of Postgres: they trade the
		// server's own crash safety for speed. The connection bound holds
		// the pools of every case, and of every process, that runs at once.
		testcontainers.WithCmd("postgres", "-c", "fsync=off", "-c", "synchronous_commit=off",
			"-c", "full_page_writes=off", "-c", "max_connections=300"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(3*time.Minute)),
	)
	keep(pg)
	if err != nil {
		return Postgres{}, err
	}
	direct, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return Postgres{}, err
	}
	out := Postgres{Direct: direct}
	out.Pooler, out.PoolerErr = startPooler(ctx, nw, poolSize)
	return out, nil
}

// StartPooler starts one more PgBouncer in front of the server, in
// transaction mode, that opens at most size connections to a database. A
// suite that measures the design behind a pool of one asks for it here.
func StartPooler(size int) (string, error) {
	if _, err := StartPostgres(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	mu.Lock()
	nw := shared
	mu.Unlock()
	return startPooler(ctx, nw, size)
}

func startPooler(ctx context.Context, nw *testcontainers.DockerNetwork, size int) (string, error) {
	// Any database name is passed through to the server.
	pooler, err := testcontainers.Run(ctx, poolerImage,
		network.WithNetwork(nil, nw),
		testcontainers.WithExposedPorts("5432/tcp"),
		testcontainers.WithEnv(map[string]string{
			"DB_HOST": dbAlias, "DB_USER": dbUser, "DB_PASSWORD": dbPassword,
			"AUTH_TYPE": "scram-sha-256", "POOL_MODE": "transaction",
			"MAX_PREPARED_STATEMENTS": "0", "DEFAULT_POOL_SIZE": strconv.Itoa(size), "MAX_CLIENT_CONN": "1000",
		}),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute)),
	)
	keep(pooler)
	if err != nil {
		return "", err
	}
	addr, err := address(ctx, pooler, "5432/tcp")
	if err != nil {
		return "", err
	}
	return (&url.URL{
		Scheme: "postgres", User: url.UserPassword(dbUser, dbPassword),
		Host: addr, Path: "/" + adminDB, RawQuery: "sslmode=disable",
	}).String(), nil
}

// StartObjects returns the object store of this test binary, starting it on
// first use.
func StartObjects() (Objects, error) {
	objectsOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		objects, objectsErr = startObjects(ctx)
	})
	return objects, objectsErr
}

func startObjects(ctx context.Context) (Objects, error) {
	runtime(ctx)
	c, err := testcontainers.Run(ctx, objectsImage,
		testcontainers.WithExposedPorts("9000/tcp"),
		testcontainers.WithEnv(map[string]string{"MINIO_ROOT_USER": objectsUser, "MINIO_ROOT_PASSWORD": objectsPassword}),
		// A directory at the root of the server's one drive is a bucket, so
		// the bucket is there before the server answers.
		testcontainers.WithEntrypoint("sh"),
		testcontainers.WithCmd("-c", "mkdir -p /data/"+objectsBucket+" && exec minio server /data"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(2*time.Minute)),
	)
	keep(c)
	if err != nil {
		return Objects{}, err
	}
	addr, err := address(ctx, c, "9000/tcp")
	if err != nil {
		return Objects{}, err
	}
	return Objects{
		Endpoint: "http://" + addr, Region: "us-east-1", Bucket: objectsBucket,
		AccessKey: objectsUser, SecretKey: objectsPassword,
	}, nil
}

// address is where a container's port is reached from the test.
func address(ctx context.Context, c testcontainers.Container, port string) (string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, mapped.Port()), nil
}

// Stop removes every container and the network this binary started.
func Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mu.Lock()
	defer mu.Unlock()
	var errs []error
	for _, c := range containers {
		if err := c.Terminate(ctx); err != nil {
			errs = append(errs, fmt.Errorf("removing a test container: %w", err))
		}
	}
	containers = nil
	if shared != nil {
		if err := shared.Remove(ctx); err != nil {
			errs = append(errs, fmt.Errorf("removing the test network: %w", err))
		}
		shared = nil
	}
	return errors.Join(errs...)
}

// Main runs a test binary's tests and then removes what they started. It is
// the body of a TestMain.
func Main(m *testing.M) {
	code := m.Run()
	if err := Stop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

// Database creates an empty database on the server and returns its direct
// URL, so one case cannot read another's rows and the migrations run on
// each. The database is dropped when the case ends.
func Database(t testing.TB, srv Postgres) string {
	t.Helper()
	name := "lectio_" + strings.ToLower(rand.Text()[:12])
	run := func(ctx context.Context, statement string) error {
		conn, err := pgx.Connect(ctx, srv.Direct)
		if err != nil {
			return err
		}
		_, err = conn.Exec(ctx, statement)
		return errors.Join(err, conn.Close(ctx))
	}
	if err := run(context.Background(), "create database "+name); err != nil {
		t.Fatalf("creating the database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := run(ctx, "drop database if exists "+name+" with (force)"); err != nil {
			t.Errorf("dropping the database: %v", err)
		}
	})
	return OnDatabase(t, srv.Direct, name)
}

// OnDatabase is a server URL pointed at another database.
func OnDatabase(t testing.TB, server, name string) string {
	t.Helper()
	u, err := url.Parse(server)
	if err != nil {
		t.Fatalf("the server URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// NameOf is the database a URL points at.
func NameOf(t testing.TB, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("the database URL: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

// runtime points the container library at a Podman machine where no Docker
// socket answers, which is what a developer on macOS may have. Ryuk, the
// library's reaper container, does not start on rootless Podman, and Stop
// removes what this binary started anyway.
func runtime(ctx context.Context) {
	runtimeOnce.Do(func() {
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
	})
}

// reaperFor turns Ryuk off where the Docker socket that answers is a Podman
// machine's under another name: the link a Podman helper installs at
// /var/run/docker.sock is one, and Ryuk there asks for a bridge network the
// machine does not have, which would skip a whole suite.
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
