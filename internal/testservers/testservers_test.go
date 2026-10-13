// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testservers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"latere.ai/x/ci-gate/reap"
)

// errStop is what every run of the stand-in engine answers, so a start goes
// no further than the container it asked for.
var errStop = errors.New("the stand-in engine starts nothing")

// engine stands in for the container engine for one test. It records each
// call in order, as sweep, network, or the image a run asked for, and holds
// every container request to the labels reap gives this process.
func engine(t *testing.T) *[]string {
	t.Helper()
	// A DOCKER_HOST that resolves nowhere keeps runtime from probing for a
	// Podman machine, and a run label makes the labels two.
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	t.Setenv("LATEREGATE_RUN", "testservers-run")
	prevSweep, prevNetwork, prevPostgres, prevContainer := sweep, newNetwork, runPostgres, runContainer
	t.Cleanup(func() {
		sweep, newNetwork, runPostgres, runContainer = prevSweep, prevNetwork, prevPostgres, prevContainer
		// What the stand-in returned is nothing Stop can remove.
		mu.Lock()
		containers, shared = nil, nil
		mu.Unlock()
	})
	var calls []string
	labeled := func(image string, opts []testcontainers.ContainerCustomizer) {
		req := testcontainers.GenericContainerRequest{}
		for _, o := range opts {
			if err := o.Customize(&req); err != nil {
				t.Fatal(err)
			}
		}
		want := reap.Labels()
		if len(want) != 2 {
			t.Fatalf("reap.Labels() = %v, want the owner and the run", want)
		}
		for k, v := range want {
			if req.Labels[k] != v {
				t.Errorf("%s: the label %s = %q, want %q", image, k, req.Labels[k], v)
			}
		}
		calls = append(calls, image)
	}
	sweep = func(context.Context) (int, error) {
		calls = append(calls, "sweep")
		return 0, nil
	}
	newNetwork = func(context.Context, ...network.NetworkCustomizer) (*testcontainers.DockerNetwork, error) {
		calls = append(calls, "network")
		return &testcontainers.DockerNetwork{Name: "testservers-network"}, nil
	}
	runPostgres = func(_ context.Context, image string, opts ...testcontainers.ContainerCustomizer) (*tcpostgres.PostgresContainer, error) {
		labeled(image, opts)
		return nil, errStop
	}
	runContainer = func(_ context.Context, image string, opts ...testcontainers.ContainerCustomizer) (*testcontainers.DockerContainer, error) {
		labeled(image, opts)
		return nil, errStop
	}
	return &calls
}

// TestAStartSweepsThenLabels: each start removes what exited binaries left
// before it makes anything of its own, and labels its container so the next
// start, and the gate, can find it if this binary dies before Stop. Either
// start may be a binary's first.
func TestAStartSweepsThenLabels(t *testing.T) {
	for _, c := range []struct {
		name  string
		start func(context.Context) error
		want  []string
	}{
		{"Postgres", func(ctx context.Context) error { _, err := startPostgres(ctx); return err }, []string{"sweep", "network", postgresImage}},
		{"the object store", func(ctx context.Context) error { _, err := startObjects(ctx); return err }, []string{"sweep", objectsImage}},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := engine(t)
			if err := c.start(context.Background()); !errors.Is(err, errStop) {
				t.Fatalf("the start = %v, want the stand-in's refusal", err)
			}
			if !slices.Equal(*calls, c.want) {
				t.Fatalf("calls = %v, want %v", *calls, c.want)
			}
		})
	}
}

// TestThePoolerIsLabeled: a pooler, the first or one a suite asks for, is
// labeled as the server is; a container left unlabeled is one no sweep
// removes.
func TestThePoolerIsLabeled(t *testing.T) {
	calls := engine(t)
	if _, err := startPooler(context.Background(), &testcontainers.DockerNetwork{Name: "testservers-network"}, 1); !errors.Is(err, errStop) {
		t.Fatalf("startPooler = %v, want the stand-in's refusal", err)
	}
	if want := []string{poolerImage}; !slices.Equal(*calls, want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
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

// TestAURLIsPointedAtAnotherDatabase: every case runs on a database of its
// own, reached by the server's URL with the name replaced.
func TestAURLIsPointedAtAnotherDatabase(t *testing.T) {
	at := OnDatabase(t, "postgres://lectio:lectio@db.example:5432/lectio_admin?sslmode=disable", "lectio_case")
	if at != "postgres://lectio:lectio@db.example:5432/lectio_case?sslmode=disable" || NameOf(t, at) != "lectio_case" {
		t.Fatalf("the URL of another database is %q, named %q", at, NameOf(t, at))
	}
}
