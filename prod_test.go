// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package lectio_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The production overlay and the objects an operator applies beside it are
// not part of the tree an installation copies from, so deploy_test.go does
// not read them. These tests hold them to what the release job and the
// rollout identity assume of them. The ones that render skip where kubectl
// is absent, and the deploy suite of .lateregate.yaml fails on a skip.

// prodKinds is the Kubernetes resource each kind in the production render
// is applied as.
var prodKinds = map[string]string{
	"Deployment":          "deployments",
	"Service":             "services",
	"ServiceAccount":      "serviceaccounts",
	"ConfigMap":           "configmaps",
	"Ingress":             "ingresses",
	"NetworkPolicy":       "networkpolicies",
	"PodDisruptionBudget": "poddisruptionbudgets",
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestProdRendersWhatTheRolloutIdentityMayApply: every object of the render
// is namespaced in latere, is of a kind the identity's Role grants, and the
// Role grants no kind the render lacks. A Secret, a cluster-scoped object or
// a policy that selects another service's Pods is not in the render: the
// identity may write none of them.
func TestProdRendersWhatTheRolloutIdentityMayApply(t *testing.T) {
	objects := render(t, "deploy/prod")
	var kinds []string
	for _, o := range objects {
		kind := kindOf(o)
		resource, ok := prodKinds[kind]
		if !ok {
			t.Errorf("deploy/prod renders the %s %s, which the rollout identity's Role does not cover; add it to deploy/prod-bootstrap/rollout-identity.yaml and to prodKinds", kind, nameOf(o))
			continue
		}
		kinds = append(kinds, resource)
		if ns := str(dig(o, "metadata", "namespace")); ns != "latere" {
			t.Errorf("%s %s is in the namespace %q, want latere", kind, nameOf(o), ns)
		}
		if kind == "NetworkPolicy" {
			for k, v := range labels(dig(o, "spec", "podSelector", "matchLabels")) {
				if k == "app" && v == "platformd" {
					t.Errorf("NetworkPolicy %s selects platformd's Pods; it belongs to deploy/prod-bootstrap", nameOf(o))
				}
			}
		}
	}
	slices.Sort(kinds)
	kinds = slices.Compact(kinds)

	identity := readFile(t, "deploy/prod-bootstrap/rollout-identity.yaml")
	role := identity[strings.Index(identity, "kind: Role\n"):strings.Index(identity, "kind: RoleBinding")]
	var granted []string
	list := regexp.MustCompile(`(?m)^\s+resources: \[(.*)\]\s*$`)
	for _, m := range list.FindAllStringSubmatch(role, -1) {
		for r := range strings.SplitSeq(m[1], ",") {
			granted = append(granted, strings.Trim(strings.TrimSpace(r), `"`))
		}
	}
	slices.Sort(granted)
	granted = slices.Compact(granted)
	if !slices.Equal(granted, kinds) {
		t.Errorf("the Role grants %v, deploy/prod renders %v", granted, kinds)
	}
	for _, not := range []string{`"secrets"`, `"delete"`, `"namespaces"`, `"*"`, "ClusterRole"} {
		if strings.Contains(identity, not) {
			t.Errorf("the rollout identity names %s; it holds no such grant", not)
		}
	}
	if !strings.Contains(role, `resources: ["deployments"]`+"\n"+`    verbs: ["watch"]`) {
		t.Error("the Role does not let the rollout watch the Deployments")
	}
	for _, want := range []string{
		"name: lectiod-rollout\n  namespace: latere",
		"kubernetes.io/service-account.name: lectiod-rollout",
		"name: lectiod-rollout-token",
		"type: kubernetes.io/service-account-token",
		"the `LECTIO_KUBECONFIG` repository secret",
	} {
		if !strings.Contains(identity, want) {
			t.Errorf("the rollout identity does not carry %q", want)
		}
	}
}

// TestProdSetsOnlyListedVariablesAndHoldsNoCredential: the overlay sets
// only variables the configuration table lists, reads every credential from
// a Secret by a key of its own name, and holds none.
func TestProdSetsOnlyListedVariablesAndHoldsNoCredential(t *testing.T) {
	names, prefixes := tableVariables(t)
	for _, o := range render(t, "deploy/prod") {
		for _, name := range variablesSetIn(o) {
			if !names[name] && !slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
				t.Errorf("%s %s sets %s, which the configuration table of specs/016-distribution.md does not list", kindOf(o), nameOf(o), name)
			}
		}
		if kindOf(o) == "ConfigMap" {
			data, _ := o["data"].(map[string]any)
			for key := range data {
				if credential.MatchString(key) {
					t.Errorf("the ConfigMap %s holds %s, which is a credential", nameOf(o), key)
				}
			}
		}
		if kindOf(o) != "Deployment" {
			continue
		}
		for _, c := range containers(o) {
			for name, e := range env(c) {
				ref := dig(e, "valueFrom", "secretKeyRef")
				if credential.MatchString(name) && ref == nil {
					t.Errorf("%s of %s is written in the manifest and not read from a Secret", name, nameOf(o))
				}
				// The two database keys are the ones terraform writes into the
				// Secret lectio-db, named by the endpoint they hold.
				if ref != nil && str(dig(ref, "key")) != name && str(dig(ref, "name")) != "lectio-db" {
					t.Errorf("%s of %s reads the key %q, want the key of its own name", name, nameOf(o), str(dig(ref, "key")))
				}
			}
		}
	}
}

// TestProdNamesTheImagesTheDeployPins: the deploy stage sets each image by
// the name the manifests write for it, so every image a Deployment of the
// render runs has an images entry in the kustomization under that name,
// pointing at the release's registry path with a released tag, and the
// release stamp moves each tag at a cut.
func TestProdNamesTheImagesTheDeployPins(t *testing.T) {
	kustomization := readFile(t, "deploy/prod/kustomization.yaml")
	gate := readFile(t, ".lateregate.yaml")
	for _, image := range []string{serverImage, converterImage} {
		entry := "  - name: " + image + "\n    newName: ghcr.io/" + publisher + "-ai/" + image + "\n    newTag: v"
		if !strings.Contains(kustomization, entry) {
			t.Errorf("deploy/prod/kustomization.yaml has no images entry that points %s at its registry path with a released tag", image)
		}
		if !strings.Contains(gate, `newName: ghcr\.io/`+publisher+`-ai/`+image+`\n    newTag: v`) {
			t.Errorf(".lateregate.yaml does not stamp the tag of %s at a release", image)
		}
	}
	if strings.Contains(kustomization, "digest:") {
		t.Error("deploy/prod/kustomization.yaml pins a digest; the stamp rewrites the tag alone, and the stage pins the digest")
	}
	// The placeholders the manifests write are the names the entries carry.
	for path, docs := range written(t) {
		for _, d := range workloads(docs) {
			for _, c := range containers(d) {
				if image := str(dig(c, "image")); image != serverImage && image != converterImage {
					t.Errorf("%s: %s runs %q, not one of the placeholders the overlay's entries name", path, nameOf(d), image)
				}
			}
		}
	}
	for _, d := range workloads(render(t, "deploy/prod")) {
		for _, c := range containers(d) {
			if image := str(dig(c, "image")); !strings.HasPrefix(image, "ghcr.io/"+publisher+"-ai/lectio") || strings.Contains(image, "@") {
				t.Errorf("deploy/prod renders %s running %q, want a registry path with a tag and no digest", nameOf(d), image)
			}
		}
	}
}

// TestTheDeployJobMatchesWhatProdRuns: the job gates itself on the
// repository variable, hands the stage the kubeconfig secret, passes both
// images by digest under the names the overlay gives them, waits for exactly
// the Deployments the render holds, and smokes the address the smoke script
// documents as the production BASE_URL.
func TestTheDeployJobMatchesWhatProdRuns(t *testing.T) {
	release, text := workflow(t, "release.yml")
	job, _ := dig(release, "jobs", "deploy").(map[string]any)
	if job == nil {
		t.Fatal("release.yml has no deploy job")
	}
	if got := str(job["if"]); got != "${{ vars.LECTIO_RELEASE_DEPLOY != '' }}" {
		t.Errorf("the deploy job runs on %q, want it gated on LECTIO_RELEASE_DEPLOY alone", got)
	}
	if got := strings.Split(strings.TrimSpace(str(dig(job, "with", "images"))), "\n"); !slices.Equal(got, []string{
		"lectiod=${{ needs.build.outputs.ref_1 }}@${{ needs.build.outputs.digest_1 }}",
		"lectio-convert=${{ needs.build.outputs.ref_2 }}@${{ needs.build.outputs.digest_2 }}",
	}) {
		t.Errorf("the deploy job passes the images %v, want both by digest under the overlay's names", got)
	}
	var running []string
	for _, o := range render(t, "deploy/prod") {
		if kindOf(o) == "Deployment" {
			running = append(running, nameOf(o))
		}
	}
	waited := strings.Fields(str(dig(job, "with", "deployments")))
	if slices.Sort(running); !slices.Equal(slices.Sorted(slices.Values(waited)), running) || len(waited) != len(running) {
		t.Errorf("the deploy job waits for %v, deploy/prod renders the Deployments %v", waited, running)
	}
	if got := str(dig(job, "with", "url")); got != "https://api."+publisher+".ai/v1/parsing" {
		t.Errorf("the deploy job smokes %q, want the production origin and base path", got)
	}
	if !strings.Contains(readFile(t, "tools/smoke/release.sh"), "for production https://api."+publisher+".ai/v1/parsing") {
		t.Error("tools/smoke/release.sh does not document the deploy job's url as the production BASE_URL")
	}
	if n := strings.Count(text, "latere-ai/ci/.github/workflows/image-deploy.yml@"); n != 1 {
		t.Errorf("release.yml calls the shared deploy stage %d times, want once", n)
	}
	publish := str(dig(release, "jobs", "publish", "if"))
	for _, want := range []string{"always()", "needs.build.result == 'success'", "needs.deploy.result != 'failure'", "needs.deploy.result != 'cancelled'"} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish does not carry %q in its condition, so a skipped deploy would skip the release", want)
		}
	}
	if got := str(dig(release, "jobs", "publish", "with", "evidence")); got != "${{ needs.deploy.result == 'success' && 'smoke-evidence' || '' }}" {
		t.Errorf("publish names the smoke evidence as %q, want it named only when the deploy succeeded", got)
	}
	if got := str(dig(release, "jobs", "verify", "if")); got != "${{ always() && needs.publish.result == 'success' }}" {
		t.Errorf("verify runs on %q, want it to run on a skipped deploy", got)
	}
}
