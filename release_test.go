// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package lectio_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The digests a test pins the images to. They name no image.
const (
	serverDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	sidecarDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// needsOf reads a job's dependencies. YAML writes one as a scalar and
// several as a sequence, and both are one list here.
func needsOf(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
	var out []string
	for _, n := range list(v) {
		out = append(out, str(n))
	}
	return out
}

// steps returns the steps of one job of a workflow.
func steps(t *testing.T, w object, job string) []any {
	t.Helper()
	out := list(dig(w, "jobs", job, "steps"))
	if len(out) == 0 {
		t.Fatalf("the workflow has no job %s with steps", job)
	}
	return out
}

// scripts is every run script of one job, joined.
func scripts(t *testing.T, w object, job string) string {
	t.Helper()
	var out strings.Builder
	for _, s := range steps(t, w, job) {
		out.WriteString(str(dig(s, "run")))
		out.WriteString("\n")
	}
	return out.String()
}

// TestTheReleaseRunsItsJobsInOrder: the pipeline's shape is a rule of its
// own. Nothing is built before the tag's commit passed verify, no digest is
// tagged before both images were built, signed and attested, and the
// release is created only for images that are tagged.
func TestTheReleaseRunsItsJobsInOrder(t *testing.T) {
	release, _ := workflow(t, "release.yml")
	jobs, ok := release["jobs"].(map[string]any)
	if !ok {
		t.Fatal("release.yml declares no jobs")
	}
	want := map[string][]string{
		"gate-green": nil,
		"build":      {"gate-green"},
		"publish":    {"build"},
		"release":    {"publish"},
		"assets":     {"build", "release"},
	}
	for name, needs := range want {
		job, ok := jobs[name].(map[string]any)
		if !ok {
			t.Errorf("release.yml has no job %s", name)
			continue
		}
		if got := needsOf(job["needs"]); !slices.Equal(got, needs) {
			t.Errorf("%s needs %v, want %v", name, got, needs)
		}
	}
	for name := range jobs {
		if _, ok := want[name]; !ok {
			t.Errorf("release.yml has the job %s, which this test does not name", name)
		}
	}
	triggers, _ := release["on"].(map[string]any)
	if tags := list(dig(triggers, "push", "tags")); len(triggers) != 1 || len(tags) != 1 || str(tags[0]) != "v*" {
		t.Errorf("release.yml runs on %v, want the push of a v* tag alone", release["on"])
	}
	if uses := str(dig(release, "jobs", "release", "uses")); uses != publisher+"-ai/ci/.github/workflows/notes-release.yml@v1" {
		t.Errorf("the release job uses %q, want the shared notes pipeline", uses)
	}
	if !strings.Contains(scripts(t, release, "gate-green"), "actions/workflows/verify.yml/runs?head_sha=${GITHUB_SHA}") {
		t.Error("the first job does not read the verify run of the tagged commit")
	}
	if !strings.Contains(scripts(t, release, "build"), `go tool lateregate release-notes "${GITHUB_REF_NAME}"`) {
		t.Error("the build does not refuse a tag with no CHANGELOG section")
	}
}

// TestTheReleaseHoldsEachJobToItsOwnPermissions: the workflow's token reads
// the repository and nothing else, and each job is given what it does and
// no more. No script carries an expression, so no value becomes part of
// the text a runner executes, and every third-party action is pinned by
// commit.
func TestTheReleaseHoldsEachJobToItsOwnPermissions(t *testing.T) {
	release, _ := workflow(t, "release.yml")
	if got := labels(release["permissions"]); !maps.Equal(got, map[string]string{"contents": "read"}) {
		t.Errorf("the workflow's token holds %v, want to read the repository alone", got)
	}
	for job, want := range map[string]map[string]string{
		"gate-green": {"contents": "read", "actions": "read"},
		"build":      {"contents": "read", "packages": "write", "id-token": "write", "attestations": "write"},
		"publish":    {"contents": "read", "packages": "write"},
		"release":    {"contents": "write"},
		"assets":     {"contents": "write", "id-token": "write"},
	} {
		if got := labels(dig(release, "jobs", job, "permissions")); !maps.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", job, got, want)
		}
	}
	pinned := regexp.MustCompile(`@[0-9a-f]{40}$`)
	jobs, _ := release["jobs"].(map[string]any)
	for name, job := range jobs {
		for _, s := range list(dig(job, "steps")) {
			if uses := str(dig(s, "uses")); uses != "" && !pinned.MatchString(uses) {
				t.Errorf("%s uses %s, which is not pinned by commit", name, uses)
			}
			if strings.Contains(str(dig(s, "run")), "${{") {
				t.Errorf("a script of %s carries an expression; a value reaches a script through its environment", name)
			}
		}
	}
}

// TestTheReleasePublishesBothImages: each image is built for both
// platforms from its own file with the tag's build identity, pushed by
// digest and under no tag, asked for its version, signed, given an attested
// bill of materials and provenance, and tagged only in publish. The
// namespace is derived and overridable, never written down.
func TestTheReleasePublishesBothImages(t *testing.T) {
	release, text := workflow(t, "release.yml")
	for _, want := range []string{"GITHUB_REPOSITORY_OWNER", "tr '[:upper:]' '[:lower:]'", "RELEASE_IMAGE_NAMESPACE"} {
		if !strings.Contains(text, want) {
			t.Errorf("release.yml does not read %s; the namespace is derived and overridable", want)
		}
	}
	build, publish := scripts(t, release, "build"), scripts(t, release, "publish")
	for _, image := range []struct{ name, variable, step, output, file, digest string }{
		{serverImage, "IMAGE", "image", "digest", "Dockerfile", "LECTIOD_DIGEST"},
		{converterImage, "CONVERT_IMAGE", "convert", "convert", "deploy/converter/Dockerfile", "CONVERT_DIGEST"},
	} {
		if got := str(dig(release, "env", image.variable)); got != image.name {
			t.Errorf("the workflow's %s is %q, want %s", image.variable, got, image.name)
		}
		var run string
		var attested []string
		for _, s := range steps(t, release, "build") {
			if str(dig(s, "id")) == image.step {
				run = str(dig(s, "run"))
			}
			if strings.HasSuffix(str(dig(s, "with", "subject-name")), "/"+image.name) {
				attested = append(attested, str(dig(s, "uses")))
				if got := str(dig(s, "with", "subject-digest")); got != "${{ steps."+image.step+".outputs.digest }}" {
					t.Errorf("an attestation of %s names the digest %q", image.name, got)
				}
			}
		}
		for _, want := range []string{
			"-f " + image.file + " ", "--platform linux/amd64,linux/arm64", "push-by-digest=true",
			`--build-arg "VERSION=${GITHUB_REF_NAME}"`, `--build-arg "COMMIT=${COMMIT}"`, `--build-arg "DATE=${DATE}"`,
			"name=${REGISTRY}/${OWNER}/${" + image.variable + "}", `echo "digest=${digest}" >> "$GITHUB_OUTPUT"`,
		} {
			if !strings.Contains(run, want) {
				t.Errorf("the build of %s does not carry %q", image.name, want)
			}
		}
		if strings.Contains(run, ":${GITHUB_REF_NAME}") {
			t.Errorf("the build of %s names the tag; a digest is tagged only in publish", image.name)
		}
		for _, action := range []string{"actions/attest-sbom@", "actions/attest-build-provenance@"} {
			if !slices.ContainsFunc(attested, func(uses string) bool { return strings.HasPrefix(uses, action) }) {
				t.Errorf("%s is not attested by %s", image.name, strings.TrimSuffix(action, "@"))
			}
		}
		if got := str(dig(release, "jobs", "build", "outputs", image.output)); got != "${{ steps."+image.step+".outputs.digest }}" {
			t.Errorf("the build job's output %s is %q", image.output, got)
		}
		reference := "${REGISTRY}/${OWNER}/${" + image.variable + "}"
		for _, want := range []string{
			`cosign sign --yes "` + reference + `@${` + image.digest + `}"`,
			`reports "` + reference + `@${` + image.digest + `}" ` + image.name,
		} {
			if !strings.Contains(build, want) {
				t.Errorf("the build job does not carry %q", want)
			}
		}
		for _, want := range []string{`-t "` + reference + `:${GITHUB_REF_NAME}"`, `"` + reference + `@${` + image.digest + `}"`} {
			if !strings.Contains(publish, want) {
				t.Errorf("the publish job does not carry %q", want)
			}
		}
		if !strings.Contains(text, "output-file: dist/sbom-"+image.name+".spdx.json") {
			t.Errorf("the bill of materials of %s is not written into dist, so it is not attached to the release", image.name)
		}
	}
	assets := scripts(t, release, "assets")
	for _, want := range []string{
		"deploy-${GITHUB_REF_NAME}.tar.gz", "sbom-lectiod.spdx.json", "sbom-lectio-convert.spdx.json",
		"checksums.txt", "cosign sign-blob --yes --bundle checksums.txt.sigstore.json", "gh release upload",
	} {
		if !strings.Contains(assets, want) {
			t.Errorf("the assets job does not carry %q", want)
		}
	}
	if !strings.Contains(build, `tools/release/deploy-archive.sh "${GITHUB_REF_NAME}" dist`) {
		t.Error("the build job does not write the deploy archive")
	}
}

// imageRef matches a registry reference under ghcr.io and captures the
// namespace segment and what follows it.
var imageRef = regexp.MustCompile(`ghcr\.io/([^/\s"'` + "`" + `]+)/([^\s"'` + "`" + `]*)`)

// TestTheImagesPublishUnderTheOwnersNamespace: no workflow, manifest,
// script or page names a fixed namespace for this repository's images. The
// published reference derives from the repository owner at run time, so a
// fork's tag publishes under the fork's. The 2 images of the compose
// file's object store are another project's and are named in full.
func TestTheImagesPublishUnderTheOwnersNamespace(t *testing.T) {
	files := shipped(t)
	for _, root := range []string{".github", "tools", "docs"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(path)] = string(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	files["README.md"] = string(readme)

	found := 0
	for path, text := range files {
		for i, line := range strings.Split(text, "\n") {
			for _, m := range imageRef.FindAllStringSubmatch(line, -1) {
				found++
				namespace, rest := m[1], m[2]
				derived := strings.HasPrefix(namespace, "$") || strings.Contains(namespace, "<") || namespace == "example"
				other := namespace == publisher+"-ai" && (strings.HasPrefix(rest, "minio:RELEASE.") || strings.HasPrefix(rest, "mc:RELEASE."))
				if !derived && !other {
					t.Errorf("%s:%d names a fixed image namespace: %s", path, i+1, m[0])
				}
			}
		}
	}
	if found < 4 {
		t.Fatalf("the walk found %d image references, too few to be the tree", found)
	}
	for _, path := range []string{".github/workflows/release.yml", "tools/release/deploy-archive.sh"} {
		namesNoInstallation(t, path, files[path], false)
	}
}

// extract unpacks a gzipped tar archive into a directory.
func extract(t *testing.T, archive, into string) {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsLocal(header.Name) {
			t.Fatalf("the archive holds %q, which leaves the directory it is unpacked in", header.Name)
		}
		target := filepath.Join(into, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, body, 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("the archive holds %q, which is neither a file nor a directory", header.Name)
		}
	}
}

// TestTheDeployArchivePinsBothImages runs the script the release runs, over
// a copy of the deploy tree, and reads the archive it writes as an operator
// does: unpacked somewhere else, every overlay renders, and every container
// names a published image by digest. The script needs kubectl, so the test
// skips where it is absent and runs in the deploy job of verify.yml.
func TestTheDeployArchivePinsBothImages(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skipf("kubectl is not on PATH, so the archive is not written here: %v", err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on PATH, so the archive is not written here: %v", err)
	}
	script, err := filepath.Abs(filepath.Join("tools", "release", "deploy-archive.sh"))
	if err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if err := os.CopyFS(filepath.Join(checkout, "deploy"), os.DirFS("deploy")); err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), bash, script, "v0.0.0-test", "dist")
		cmd.Dir = checkout
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// A digest that is missing ends the script before it edits a file.
	if out, err := run("REGISTRY=ghcr.io", "OWNER=example", "LECTIOD_DIGEST="+serverDigest, "CONVERT_DIGEST="); err == nil {
		t.Fatalf("the script wrote an archive with no digest for the sidecar:\n%s", out)
	}
	if out, err := run("REGISTRY=ghcr.io", "OWNER=example", "LECTIOD_DIGEST="+serverDigest, "CONVERT_DIGEST="+sidecarDigest); err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}

	unpacked := t.TempDir()
	extract(t, filepath.Join(checkout, "dist", "deploy-v0.0.0-test.tar.gz"), unpacked)
	want := map[string]string{
		serverImage:    "ghcr.io/example/" + serverImage + "@" + serverDigest,
		converterImage: "ghcr.io/example/" + converterImage + "@" + sidecarDigest,
	}
	seen := map[string]bool{}
	for _, dir := range kustomizations {
		for _, d := range workloads(render(t, filepath.Join(unpacked, filepath.FromSlash(dir)))) {
			for _, c := range containers(d) {
				image := str(dig(c, "image"))
				seen[image] = true
				if !slices.Contains(slices.Collect(maps.Values(want)), image) {
					t.Errorf("%s of the archive runs %s the image %q, which is not pinned to the release", dir, nameOf(d), image)
				}
			}
		}
	}
	for name, image := range want {
		if !seen[image] {
			t.Errorf("no overlay of the archive runs %s as %s", name, image)
		}
	}
}
