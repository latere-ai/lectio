// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package lectio_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
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

// stageDir is where the shared release stages live; each job of the
// release calls one of them.
const stageDir = publisher + "-ai/ci/.github/workflows/"

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

// TestTheReleaseRunsItsJobsInOrder: the pipeline's shape is a rule of its
// own. The build stage builds, signs and attests both images, the publish
// stage tags the digests and creates the release only after it, and the
// verify stage checks the release only once it is published. Every job is
// a call of a shared stage at one version of them, and runs on a hosted
// runner, as every job of a public repository does.
func TestTheReleaseRunsItsJobsInOrder(t *testing.T) {
	release, _ := workflow(t, "release.yml")
	jobs, ok := release["jobs"].(map[string]any)
	if !ok {
		t.Fatal("release.yml declares no jobs")
	}
	want := map[string]struct {
		stage string
		needs []string
	}{
		"build":   {"image-build.yml", nil},
		"publish": {"image-publish.yml", []string{"build"}},
		"verify":  {"release-verify.yml", []string{"publish"}},
	}
	versions := map[string]bool{}
	for name, w := range want {
		job, ok := jobs[name].(map[string]any)
		if !ok {
			t.Errorf("release.yml has no job %s", name)
			continue
		}
		if got := needsOf(job["needs"]); !slices.Equal(got, w.needs) {
			t.Errorf("%s needs %v, want %v", name, got, w.needs)
		}
		uses := str(job["uses"])
		stage, version, found := strings.Cut(uses, "@")
		if !found || stage != stageDir+w.stage || version == "" {
			t.Errorf("%s uses %q, want the shared stage %s at a version", name, uses, w.stage)
		}
		versions[version] = true
		if job["steps"] != nil {
			t.Errorf("%s runs steps of its own; the release runs through the shared stages", name)
		}
		if got := str(dig(job, "with", "runs_on")); got != "ubuntu-latest" {
			t.Errorf("%s runs on %q, want a hosted runner, ubuntu-latest", name, got)
		}
	}
	if len(versions) != 1 {
		t.Errorf("the stages are called at %v, want one version of them", slices.Sorted(maps.Keys(versions)))
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
}

// TestTheReleaseHoldsEachJobToItsOwnPermissions: the workflow's token reads
// the repository and nothing else, and each job is given what its stage
// does and no more. No secret is passed, since no stage reads one. No shell
// command handed to a stage carries an expression, so no value becomes
// part of the text a runner executes. Signing, attesting and tagging are
// the stages' alone: the file calls none of those tools itself.
func TestTheReleaseHoldsEachJobToItsOwnPermissions(t *testing.T) {
	release, text := workflow(t, "release.yml")
	if got := labels(release["permissions"]); !maps.Equal(got, map[string]string{"contents": "read"}) {
		t.Errorf("the workflow's token holds %v, want to read the repository alone", got)
	}
	for job, want := range map[string]map[string]string{
		"build":   {"contents": "read", "actions": "read", "packages": "write", "id-token": "write", "attestations": "write"},
		"publish": {"contents": "write", "packages": "write", "id-token": "write"},
		"verify":  {"contents": "read", "packages": "read", "attestations": "read"},
	} {
		if got := labels(dig(release, "jobs", job, "permissions")); !maps.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", job, got, want)
		}
	}
	jobs, _ := release["jobs"].(map[string]any)
	for name, job := range jobs {
		if dig(job, "secrets") != nil {
			t.Errorf("%s passes secrets, and no stage reads one", name)
		}
		for key, value := range labels(dig(job, "with")) {
			if strings.HasSuffix(key, "_command") && strings.Contains(value, "${{") {
				t.Errorf("the %s of %s carries an expression; a value reaches a command through its environment", key, name)
			}
		}
	}
	for _, direct := range []string{"cosign", "anchore/sbom-action", "actions/attest-", "imagetools"} {
		if strings.Contains(text, direct) {
			t.Errorf("release.yml names %s; the shared stages sign, attest and tag", direct)
		}
	}
}

// TestTheReleasePublishesBothImages: each image is built from its own file
// for both platforms, the stage's default, and must report the version the
// tag pins in the form its binary prints. The deploy archive is written
// once both digests exist, from those digests, and the publish and verify
// stages are given every image the build made. No job names a namespace,
// so the images publish under the repository owner's, or the one the
// repository variable sets.
func TestTheReleasePublishesBothImages(t *testing.T) {
	release, _ := workflow(t, "release.yml")
	build := dig(release, "jobs", "build", "with")
	for _, image := range []struct{ slot, name, dockerfile string }{
		{"image_1", serverImage, "Dockerfile"},
		{"image_2", converterImage, "deploy/converter/Dockerfile"},
	} {
		var spec map[string]string
		if err := json.Unmarshal([]byte(str(dig(build, image.slot))), &spec); err != nil {
			t.Errorf("%s is not the JSON of an image: %v", image.slot, err)
			continue
		}
		if spec["name"] != image.name {
			t.Errorf("%s builds %q, want %s", image.slot, spec["name"], image.name)
		}
		dockerfile := spec["dockerfile"]
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		if dockerfile != image.dockerfile {
			t.Errorf("%s is built from %s, want %s", image.name, dockerfile, image.dockerfile)
		}
		if _, err := os.Stat(dockerfile); err != nil {
			t.Errorf("%s is built from %s: %v", image.name, dockerfile, err)
		}
		if platforms, ok := spec["platforms"]; ok && platforms != "linux/amd64,linux/arm64" {
			t.Errorf("%s is built for %s, want linux/amd64 and linux/arm64", image.name, platforms)
		}
		if spec["version_args"] != "version" || spec["version_expect"] != "{name} {tag} (" {
			t.Errorf("%s is asked for its version with %q and expected to print %q, want `version` and \"{name} {tag} (\"",
				image.name, spec["version_args"], spec["version_expect"])
		}
	}
	if got := str(dig(build, "image_3")); got != "" {
		t.Errorf("the build makes a third image, %s, which this test does not name", got)
	}

	if got := str(dig(build, "archives_command")); got != "" {
		t.Errorf("the build runs %q before the images; the deploy archive pins their digests, so it is written after them", got)
	}
	after := str(dig(build, "after_images_command"))
	for _, want := range []string{
		`REGISTRY="${IMAGE_1%%/*}"`, `LECTIOD_DIGEST="${IMAGE_1#*@}"`, `CONVERT_DIGEST="${IMAGE_2#*@}"`,
		`tools/release/deploy-archive.sh "$TAG" dist`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("the command run after the images does not carry %q", want)
		}
	}

	if got := str(dig(release, "jobs", "publish", "with", "images")); got != "${{ needs.build.outputs.images }}" {
		t.Errorf("the publish stage is given the images %q, want every image the build made", got)
	}
	if got := strings.Fields(str(dig(release, "jobs", "verify", "with", "images"))); !slices.Equal(got, []string{serverImage, converterImage}) {
		t.Errorf("the verify stage checks %v, want %s and %s", got, serverImage, converterImage)
	}
	for _, job := range []string{"build", "publish", "verify"} {
		if dig(release, "jobs", job, "with", "namespace") != nil {
			t.Errorf("%s names an image namespace; it is the repository owner's, or the repository variable's", job)
		}
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
