// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package testservers

import (
	"os"
	"path/filepath"
	"testing"
)

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
