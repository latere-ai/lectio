// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package lectio_test holds what the repository ships beside its Go
// packages to what it says of it: the deploy tree, the images and the
// release workflow.
package lectio_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/lectio/internal/config"
)

// kustomizations are every directory `kubectl kustomize` is run over: the
// base and one per example. A kustomization added under deploy/ without a
// row here fails TestEveryKustomizationIsRendered.
var kustomizations = []string{
	"deploy/base",
	"deploy/examples/generic",
	"deploy/examples/with-converter",
}

// components are the kustomize components, which do not render alone, each
// with the overlay that renders it.
var components = map[string]string{
	"deploy/components/converter": "deploy/examples/with-converter",
}

// The names the tree is written under.
const (
	serverImage    = "lectiod"
	converterImage = "lectio-convert"
	apiName        = "lectiod"
	workerName     = "lectiod-worker"
	converterName  = "lectio-convert"
	uid            = 65532
)

// object is one YAML document, read as a tree so a test asserts over the
// fields it names and nothing else.
type object = map[string]any

func kindOf(o object) string { return str(o["kind"]) }
func nameOf(o object) string { return str(dig(o, "metadata", "name")) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

// list reads a field that holds a sequence.
func list(v any) []any {
	s, _ := v.([]any)
	return s
}

// dig walks a path of map keys and returns what is there, or nil.
func dig(o any, path ...string) any {
	for _, key := range path {
		m, ok := o.(map[string]any)
		if !ok {
			return nil
		}
		o = m[key]
	}
	return o
}

// decode reads every document of a YAML text. A document that is not a
// mapping, such as the empty one before a leading separator, is left out.
func decode(where string, data []byte) ([]object, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []object
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if o, ok := doc.(map[string]any); ok {
			out = append(out, o)
		}
	}
}

// written is every YAML file under deploy/ as it is written, by path. The
// rules that read it hold where kubectl is absent, which is every run of
// the gate.
func written(t *testing.T) map[string][]object {
	t.Helper()
	out := map[string][]object{}
	err := filepath.WalkDir("deploy", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		docs, err := decode(path, data)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(path)] = docs
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("the walk read %d file(s) under deploy/, too few to be the tree", len(out))
	}
	return out
}

// manifests is every Kubernetes document under deploy/ as written, less the
// compose file and the kustomizations, which are not objects of a cluster.
func manifests(t *testing.T) []object {
	t.Helper()
	var out []object
	files := written(t)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		for _, o := range files[path] {
			if str(o["apiVersion"]) == "" || strings.HasPrefix(str(o["apiVersion"]), "kustomize.config.k8s.io/") {
				continue
			}
			out = append(out, o)
		}
	}
	return out
}

// render runs `kubectl kustomize` over a directory and returns the
// documents. The command reads files and reaches no cluster. Where kubectl
// is absent the test skips by name and does not pass quietly; the deploy
// job of verify.yml is the run that always has it and fails on a skip.
func render(t *testing.T, dir string) []object {
	t.Helper()
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skipf("kubectl is not on PATH, so %s is not rendered here: %v", dir, err)
	}
	cmd := exec.CommandContext(t.Context(), bin, "kustomize", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	objects, err := decode(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) == 0 {
		t.Fatalf("%s rendered nothing; the assertions would pass vacuously", dir)
	}
	return objects
}

// find returns the one object of a kind and name, and fails when there is
// none.
func find(t *testing.T, objects []object, kind, name string) object {
	t.Helper()
	for _, o := range objects {
		if kindOf(o) == kind && nameOf(o) == name {
			return o
		}
	}
	t.Fatalf("no %s named %s", kind, name)
	return nil
}

// has reports whether objects hold one of a kind and name.
func has(objects []object, kind, name string) bool {
	return slices.ContainsFunc(objects, func(o object) bool { return kindOf(o) == kind && nameOf(o) == name })
}

// workloads returns the Deployments that declare a container, which leaves
// out a patch: a patch names a container and no image.
func workloads(objects []object) []object {
	var out []object
	for _, o := range objects {
		if kindOf(o) != "Deployment" {
			continue
		}
		cs := containers(o)
		if len(cs) > 0 && str(dig(cs[0], "image")) != "" {
			out = append(out, o)
		}
	}
	return out
}

// containers returns the containers of a Deployment's pod template.
func containers(o object) []any {
	return list(dig(o, "spec", "template", "spec", "containers"))
}

// env returns a container's environment entries by name.
func env(c any) map[string]any {
	out := map[string]any{}
	for _, e := range list(dig(c, "env")) {
		out[str(dig(e, "name"))] = e
	}
	return out
}

// labels reads a mapping of labels.
func labels(v any) map[string]string {
	out := map[string]string{}
	m, _ := v.(map[string]any)
	for k, val := range m {
		out[k] = str(val)
	}
	return out
}

// selects reports whether a selector's matchLabels are all among a Pod's
// labels. An empty selector selects every Pod.
func selects(selector any, pod map[string]string) bool {
	for k, v := range labels(dig(selector, "matchLabels")) {
		if pod[k] != v {
			return false
		}
	}
	return true
}

// port reads the port of a listen address such as ":8080".
func port(t *testing.T, addr string) int {
	t.Helper()
	_, p, ok := strings.Cut(addr, ":")
	n, err := strconv.Atoi(p)
	if !ok || err != nil {
		t.Fatalf("%q is not a listen address with a port", addr)
	}
	return n
}

// TestEveryKustomizationIsRendered: a directory added under deploy/ with a
// kustomization joins the rendered set, and a component is included by the
// overlay that renders it, so a new overlay cannot be the one nothing
// checks.
func TestEveryKustomizationIsRendered(t *testing.T) {
	var found, parts []string
	for path, docs := range written(t) {
		if filepath.Base(path) != "kustomization.yaml" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		if len(docs) != 1 {
			t.Fatalf("%s holds %d documents, want one", path, len(docs))
		}
		switch kindOf(docs[0]) {
		case "Kustomization":
			found = append(found, dir)
		case "Component":
			parts = append(parts, dir)
		default:
			t.Errorf("%s is a %q, which is neither a Kustomization nor a Component", path, kindOf(docs[0]))
		}
		if docs[0]["secretGenerator"] != nil {
			t.Errorf("%s generates a Secret; a credential is applied by hand and is in no kustomization", path)
		}
	}
	slices.Sort(found)
	if !slices.Equal(found, slices.Sorted(slices.Values(kustomizations))) {
		t.Errorf("deploy/ holds the kustomizations %v and the tests render %v", found, kustomizations)
	}
	slices.Sort(parts)
	if !slices.Equal(parts, slices.Sorted(maps.Keys(components))) {
		t.Errorf("deploy/ holds the components %v and the tests name %v", parts, slices.Sorted(maps.Keys(components)))
	}
	for part, overlay := range components {
		docs := written(t)[overlay+"/kustomization.yaml"]
		if len(docs) != 1 {
			t.Fatalf("%s has no kustomization", overlay)
		}
		included := false
		for _, c := range list(docs[0]["components"]) {
			included = included || filepath.ToSlash(filepath.Join(overlay, str(c))) == part
		}
		if !included {
			t.Errorf("%s does not include %s, so nothing renders the component", overlay, part)
		}
	}
}

// TestTheBaseRenders is the base as an installation receives it: the 7
// objects, in no namespace, with the image placeholders and with nothing
// an installation chooses.
func TestTheBaseRenders(t *testing.T) {
	objects := render(t, "deploy/base")
	var got []string
	for _, o := range objects {
		got = append(got, kindOf(o)+"/"+nameOf(o))
		if ns := str(dig(o, "metadata", "namespace")); ns != "" {
			t.Errorf("the base puts %s/%s in the namespace %q; an overlay names one", kindOf(o), nameOf(o), ns)
		}
	}
	want := []string{
		"Deployment/" + apiName, "Deployment/" + workerName, "NetworkPolicy/" + apiName, "NetworkPolicy/" + workerName,
		"PodDisruptionBudget/" + apiName, "Service/" + apiName, "ServiceAccount/" + apiName,
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the base renders %v, want %v", got, want)
	}
	for _, d := range workloads(objects) {
		for _, c := range containers(d) {
			if image := str(dig(c, "image")); image != serverImage {
				t.Errorf("%s names the image %q; the placeholder is %q, which an overlay or the release points at a registry", nameOf(d), image, serverImage)
			}
		}
	}
	for _, o := range objects {
		if kindOf(o) == "ConfigMap" || kindOf(o) == "Secret" {
			t.Errorf("the base holds the %s %s; a value an installation chooses is the overlay's", kindOf(o), nameOf(o))
		}
	}
	if _, set := env(containers(find(t, objects, "Deployment", workerName))[0])["LECTIO_CONVERTER_URL"]; set {
		t.Error("the base gives the workers a converter; an installation without the component refuses the formats that need one")
	}
}

// TestTheExamplesRender is each example as it is applied: every object in
// the overlay's namespace, every ConfigMap the Deployments read present,
// and the conversion sidecar in the one overlay that adds it.
func TestTheExamplesRender(t *testing.T) {
	for _, dir := range kustomizations[1:] {
		t.Run(dir, func(t *testing.T) {
			objects := render(t, dir)
			for _, o := range objects {
				if ns := str(dig(o, "metadata", "namespace")); ns != "lectio" {
					t.Errorf("%s/%s is in the namespace %q, want lectio", kindOf(o), nameOf(o), ns)
				}
			}
			for _, d := range workloads(objects) {
				for _, c := range containers(d) {
					for _, from := range list(dig(c, "envFrom")) {
						find(t, objects, "ConfigMap", str(dig(from, "configMapRef", "name")))
					}
				}
				for _, v := range list(dig(d, "spec", "template", "spec", "volumes")) {
					if cm := str(dig(v, "configMap", "name")); cm != "" {
						find(t, objects, "ConfigMap", cm)
					}
				}
			}
			worker := containers(find(t, objects, "Deployment", workerName))[0]
			api := containers(find(t, objects, "Deployment", apiName))[0]
			url := str(dig(env(worker)["LECTIO_CONVERTER_URL"], "value"))
			if _, set := env(api)["LECTIO_CONVERTER_URL"]; set {
				t.Error("the API is given a converter; only a worker prepares a file")
			}
			if dir != components["deploy/components/converter"] {
				if url != "" || has(objects, "Deployment", converterName) {
					t.Error("an overlay without the component deploys or names a converter")
				}
				return
			}
			find(t, objects, "Deployment", converterName)
			svc := find(t, objects, "Service", converterName)
			ports := list(dig(svc, "spec", "ports"))
			if len(ports) != 1 {
				t.Fatalf("the converter's Service has %d ports, want one", len(ports))
			}
			if want := fmt.Sprintf("http://%s:%v", nameOf(svc), dig(ports[0], "port")); url != want {
				t.Errorf("the workers convert through %q and the Service answers at %q", url, want)
			}
		})
	}
}

// confined holds one Deployment to the pod security the base documents: a
// numeric non-root user, the default seccomp profile, no token, and in
// every container no privilege escalation, no capability and a read-only
// root file system.
func confined(t *testing.T, d object) {
	t.Helper()
	pod := dig(d, "spec", "template", "spec")
	if dig(pod, "automountServiceAccountToken") != false {
		t.Errorf("%s mounts a service account token", nameOf(d))
	}
	if dig(pod, "securityContext", "runAsNonRoot") != true {
		t.Errorf("%s does not declare runAsNonRoot", nameOf(d))
	}
	for _, field := range []string{"runAsUser", "runAsGroup"} {
		if got := dig(pod, "securityContext", field); got != uid {
			t.Errorf("%s has %s %v, want %d, the user the image is built for", nameOf(d), field, got, uid)
		}
	}
	if dig(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("%s does not ask for the default seccomp profile", nameOf(d))
	}
	for _, field := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if dig(pod, field) == true {
			t.Errorf("%s sets %s", nameOf(d), field)
		}
	}
	if len(list(dig(pod, "initContainers"))) != 0 {
		t.Errorf("%s has an init container, which these rules do not read", nameOf(d))
	}
	cs := containers(d)
	if len(cs) != 1 {
		t.Fatalf("%s holds %d containers; one image, one role per Pod", nameOf(d), len(cs))
	}
	sc := dig(cs[0], "securityContext")
	if dig(sc, "allowPrivilegeEscalation") != false {
		t.Errorf("%s permits privilege escalation", nameOf(d))
	}
	if dig(sc, "readOnlyRootFilesystem") != true {
		t.Errorf("%s has a writable root file system", nameOf(d))
	}
	if dig(sc, "privileged") == true || dig(sc, "capabilities", "add") != nil {
		t.Errorf("%s is privileged or adds a capability", nameOf(d))
	}
	if drop := list(dig(sc, "capabilities", "drop")); len(drop) != 1 || str(drop[0]) != "ALL" {
		t.Errorf("%s drops %v, want every capability", nameOf(d), drop)
	}
}

// writable returns the paths a Deployment's container can write to, and
// fails on a volume that is neither a ConfigMap nor a bounded emptyDir.
func writable(t *testing.T, d object) []string {
	t.Helper()
	volumes := map[string]any{}
	for _, v := range list(dig(d, "spec", "template", "spec", "volumes")) {
		volumes[str(dig(v, "name"))] = v
	}
	var out []string
	for _, m := range list(dig(containers(d)[0], "volumeMounts")) {
		v := volumes[str(dig(m, "name"))]
		switch {
		case dig(v, "configMap") != nil:
			if dig(m, "readOnly") != true {
				t.Errorf("%s mounts a ConfigMap at %s without readOnly", nameOf(d), str(dig(m, "mountPath")))
			}
		case dig(v, "emptyDir") != nil:
			if dig(v, "emptyDir", "sizeLimit") == nil {
				t.Errorf("%s has scratch space at %s with no bound", nameOf(d), str(dig(m, "mountPath")))
			}
			out = append(out, str(dig(m, "mountPath")))
		default:
			t.Errorf("%s mounts %v, which is neither a ConfigMap nor scratch space", nameOf(d), v)
		}
	}
	return out
}

// held runs the rules of one Deployment over a set of objects, so the files
// as written and each rendered overlay are held to the same ones.
func held(t *testing.T, objects []object) {
	t.Helper()
	ds := workloads(objects)
	if len(ds) < 2 {
		t.Fatalf("found %d Deployments, too few to be the tree", len(ds))
	}
	for _, d := range ds {
		confined(t, d)
		c := containers(d)[0]
		paths := writable(t, d)
		switch str(dig(c, "image")) {
		case serverImage:
			if len(paths) != 0 {
				t.Errorf("%s can write to %v; lectiod writes no file", nameOf(d), paths)
			}
		case converterImage:
			if !slices.Equal(paths, []string{"/tmp"}) {
				t.Errorf("%s can write to %v, want its scratch space at /tmp alone", nameOf(d), paths)
			}
		default:
			t.Errorf("%s runs the image %q, which is not one this repository builds", nameOf(d), str(dig(c, "image")))
		}
		for _, probe := range []string{"readinessProbe", "livenessProbe"} {
			if dig(c, probe) == nil {
				t.Errorf("%s has no %s", nameOf(d), probe)
			}
		}
		for _, bound := range []string{"requests", "limits"} {
			for _, resource := range []string{"cpu", "memory"} {
				if dig(c, "resources", bound, resource) == nil {
					t.Errorf("%s sets no %s of %s", nameOf(d), bound, resource)
				}
			}
		}
	}
}

// TestEveryContainerIsConfinedProbedAndBounded is the rule every Deployment
// of the tree is held to, read from the files as written, so it holds in
// the gate, and again from every rendered overlay, so no patch undoes it.
func TestEveryContainerIsConfinedProbedAndBounded(t *testing.T) {
	t.Run("as written", func(t *testing.T) {
		held(t, manifests(t))
		if n := len(workloads(manifests(t))); n != 3 {
			t.Errorf("the tree holds %d Deployments, want the API, the workers and the converter", n)
		}
	})
	for _, dir := range kustomizations {
		t.Run(dir, func(t *testing.T) { held(t, render(t, dir)) })
	}
}

// TestTheRolesListenWhereTheProbesAsk holds the 2 roles of lectiod to the
// listeners the binary is given: the probes ask the internal listener for
// /readyz and /livez, the named ports are the ports of LECTIO_ADDR and
// LECTIO_INTERNAL_ADDR, and a worker has no public port.
func TestTheRolesListenWhereTheProbesAsk(t *testing.T) {
	objects := manifests(t)
	for role, name := range map[string]string{"api": apiName, "worker": workerName} {
		d := find(t, objects, "Deployment", name)
		c := containers(d)[0]
		vars := env(c)
		if got := str(dig(vars["LECTIO_ROLE"], "value")); got != role {
			t.Errorf("%s runs the role %q, want %q", name, got, role)
		}
		ports := map[string]any{}
		for _, p := range list(dig(c, "ports")) {
			ports[str(dig(p, "name"))] = dig(p, "containerPort")
		}
		want := map[string]any{"internal": port(t, str(dig(vars["LECTIO_INTERNAL_ADDR"], "value")))}
		if role == "api" {
			want["public"] = port(t, str(dig(vars["LECTIO_ADDR"], "value")))
		} else if _, set := vars["LECTIO_ADDR"]; set {
			t.Errorf("%s sets LECTIO_ADDR; a worker serves no caller", name)
		}
		if !maps.Equal(ports, want) {
			t.Errorf("%s names the ports %v, and its listeners are %v", name, ports, want)
		}
		for probe, path := range map[string]string{"readinessProbe": "/readyz", "livenessProbe": "/livez"} {
			if got := str(dig(c, probe, "httpGet", "path")); got != path {
				t.Errorf("%s asks %q for its %s, want %s", name, got, probe, path)
			}
			if got := dig(c, probe, "httpGet", "port"); got != "internal" {
				t.Errorf("%s probes the port %v, want the internal listener", name, got)
			}
		}
	}
	svc := find(t, objects, "Service", apiName)
	ports := list(dig(svc, "spec", "ports"))
	if len(ports) != 1 || dig(ports[0], "targetPort") != "public" {
		t.Errorf("the Service routes %v, want the public listener alone", ports)
	}
}

// TestTheSelectorsKeepTheRolesApart: both roles carry one name, so the
// component label is what keeps a Deployment, the Service and the budget on
// their own Pods.
func TestTheSelectorsKeepTheRolesApart(t *testing.T) {
	objects := manifests(t)
	pods := map[string]map[string]string{}
	for _, d := range workloads(objects) {
		pods[nameOf(d)] = labels(dig(d, "spec", "template", "metadata", "labels"))
	}
	for _, d := range workloads(objects) {
		for name, pod := range pods {
			if got := selects(dig(d, "spec", "selector"), pod); got != (name == nameOf(d)) {
				t.Errorf("the selector of %s selects the Pods of %s: %v", nameOf(d), name, got)
			}
		}
	}
	for _, o := range []object{find(t, objects, "Service", apiName), find(t, objects, "PodDisruptionBudget", apiName)} {
		selector := dig(o, "spec", "selector")
		if kindOf(o) == "Service" {
			selector = map[string]any{"matchLabels": selector}
		}
		for name, pod := range pods {
			if got := selects(selector, pod); got != (name == apiName) {
				t.Errorf("the %s selects the Pods of %s: %v", kindOf(o), name, got)
			}
		}
	}
}

// TestTheTerminationGraceCoversTheShutdown: a Pod is given longer to stop
// than its process takes. The numbers are read from one manifest each, so
// raising one without the other fails here.
func TestTheTerminationGraceCoversTheShutdown(t *testing.T) {
	objects := manifests(t)
	grace := func(d object) time.Duration {
		seconds, ok := dig(d, "spec", "template", "spec", "terminationGracePeriodSeconds").(int)
		if !ok {
			t.Fatalf("%s sets no termination grace", nameOf(d))
		}
		return time.Duration(seconds) * time.Second
	}
	takes := func(d object, variable string) time.Duration {
		took, err := time.ParseDuration(str(dig(env(containers(d)[0])[variable], "value")))
		if err != nil {
			t.Fatalf("%s sets no %s beside its termination grace: %v", nameOf(d), variable, err)
		}
		return took
	}

	api := find(t, objects, "Deployment", apiName)
	hook := dig(containers(api)[0], "lifecycle", "preStop")
	sleep, ok := dig(hook, "sleep", "seconds").(int)
	if !ok || sleep < 1 || dig(hook, "exec") != nil {
		t.Fatalf("the API's preStop hook is %v; the image has no shell, so the hook is the kubelet's own sleep", hook)
	}
	if need := time.Duration(sleep)*time.Second + takes(api, "LECTIO_SHUTDOWN_GRACE"); grace(api) <= need {
		t.Errorf("the API has %v to stop and takes %v", grace(api), need)
	}
	if dig(api, "spec", "replicas") != 2 {
		t.Errorf("the API runs %v replicas, want 2", dig(api, "spec", "replicas"))
	}
	roll := dig(api, "spec", "strategy", "rollingUpdate")
	if dig(roll, "maxSurge") != 1 || dig(roll, "maxUnavailable") != 0 {
		t.Errorf("the API rolls with %v, want one more and none missing", roll)
	}

	worker := find(t, objects, "Deployment", workerName)
	if need := takes(worker, "LECTIO_SHUTDOWN_GRACE"); grace(worker) <= need {
		t.Errorf("a worker has %v to stop and takes %v", grace(worker), need)
	}
	converter := find(t, objects, "Deployment", converterName)
	if need := takes(converter, "LECTIO_CONVERT_TIMEOUT"); grace(converter) <= need {
		t.Errorf("the converter has %v to stop and a conversion takes up to %v", grace(converter), need)
	}
}

// TestEveryPodIsBehindAPolicy: for each Deployment a policy selects its
// Pods and names both directions, so a connection no rule admits is
// refused. The API admits its public port and the workers admit nothing.
func TestEveryPodIsBehindAPolicy(t *testing.T) {
	objects := manifests(t)
	for _, d := range workloads(objects) {
		pod := labels(dig(d, "spec", "template", "metadata", "labels"))
		denied := false
		for _, p := range objects {
			if kindOf(p) != "NetworkPolicy" || !selects(dig(p, "spec", "podSelector"), pod) {
				continue
			}
			if dig(p, "spec", "podSelector", "matchExpressions") != nil {
				t.Errorf("the policy %s selects by an expression, which these rules do not read", nameOf(p))
			}
			var types []string
			for _, ty := range list(dig(p, "spec", "policyTypes")) {
				types = append(types, str(ty))
			}
			denied = denied || (slices.Contains(types, "Ingress") && slices.Contains(types, "Egress"))
		}
		if !denied {
			t.Errorf("no policy names both directions for the Pods of %s", nameOf(d))
		}
	}
	if in := list(dig(find(t, objects, "NetworkPolicy", workerName), "spec", "ingress")); len(in) != 0 {
		t.Errorf("the workers admit %v; nothing dials a worker", in)
	}
	in := list(dig(find(t, objects, "NetworkPolicy", apiName), "spec", "ingress"))
	if len(in) != 1 || len(list(dig(in[0], "ports"))) != 1 || dig(list(dig(in[0], "ports"))[0], "port") != 8080 {
		t.Errorf("the API admits %v, want its public port alone", in)
	}
}

// TestTheConverterHasNoNetworkAndNoCredential is the converter's rules of
// specs/009-intake.md as the manifests can hold them: no policy that
// selects its Pods has an egress rule, the only connection admitted is a
// worker's on its one port, and it is given no ConfigMap, no Secret, no
// token and no address of another Service.
func TestTheConverterHasNoNetworkAndNoCredential(t *testing.T) {
	check := func(t *testing.T, objects []object) {
		t.Helper()
		d := find(t, objects, "Deployment", converterName)
		pod := labels(dig(d, "spec", "template", "metadata", "labels"))
		worker := labels(dig(find(t, objects, "Deployment", workerName), "spec", "template", "metadata", "labels"))
		api := labels(dig(find(t, objects, "Deployment", apiName), "spec", "template", "metadata", "labels"))

		selected := 0
		for _, p := range objects {
			if kindOf(p) != "NetworkPolicy" || !selects(dig(p, "spec", "podSelector"), pod) {
				continue
			}
			selected++
			if out := list(dig(p, "spec", "egress")); len(out) != 0 {
				t.Errorf("the policy %s gives the converter the egress rule %v", nameOf(p), out)
			}
			var types []string
			for _, ty := range list(dig(p, "spec", "policyTypes")) {
				types = append(types, str(ty))
			}
			if !slices.Contains(types, "Egress") {
				t.Errorf("the policy %s does not name Egress, so it refuses nothing outbound", nameOf(p))
			}
			in := list(dig(p, "spec", "ingress"))
			if len(in) != 1 || len(list(dig(in[0], "from"))) != 1 || len(list(dig(in[0], "ports"))) != 1 {
				t.Fatalf("the policy %s admits %v, want one peer on one port", nameOf(p), in)
			}
			peer := list(dig(in[0], "from"))[0]
			if dig(peer, "namespaceSelector") != nil || dig(peer, "ipBlock") != nil ||
				len(labels(dig(peer, "podSelector", "matchLabels"))) == 0 {
				t.Errorf("the policy %s admits %v, want Pods of its own namespace by label", nameOf(p), peer)
			}
			if !selects(dig(peer, "podSelector"), worker) || selects(dig(peer, "podSelector"), api) || selects(dig(peer, "podSelector"), pod) {
				t.Errorf("the policy %s admits %v, want the workers and no other Pod", nameOf(p), peer)
			}
			listens := dig(list(dig(containers(d)[0], "ports"))[0], "containerPort")
			if got := dig(list(dig(in[0], "ports"))[0], "port"); got != listens {
				t.Errorf("the policy %s admits port %v and the converter listens on %v", nameOf(p), got, listens)
			}
		}
		if selected != 1 {
			t.Errorf("%d policies select the converter's Pods, want one", selected)
		}

		spec := dig(d, "spec", "template", "spec")
		if dig(spec, "enableServiceLinks") != false {
			t.Error("the converter's environment carries the addresses of the namespace's Services")
		}
		c := containers(d)[0]
		if dig(c, "envFrom") != nil {
			t.Error("the converter reads a ConfigMap or a Secret whole")
		}
		for name, e := range env(c) {
			if dig(e, "valueFrom") != nil {
				t.Errorf("the converter reads %s from another object", name)
			}
		}
		for _, v := range list(dig(spec, "volumes")) {
			if dig(v, "emptyDir") == nil {
				t.Errorf("the converter mounts %v, want scratch space alone", v)
			}
		}
	}
	t.Run("as written", func(t *testing.T) { check(t, manifests(t)) })
	t.Run("rendered", func(t *testing.T) { check(t, render(t, components["deploy/components/converter"])) })
}

// tableVariables reads the configuration table of specs/016-distribution.md:
// the names its first column lists, and the prefixes a row such as
// `LECTIO_S3_*` stands for.
func tableVariables(t *testing.T) (names map[string]bool, prefixes []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("specs", "016-distribution.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(data), "| Variable | Default | Owner | Read today |\n")
	if !ok {
		t.Fatal("specs/016-distribution.md has no configuration table")
	}
	cell := regexp.MustCompile("`(LECTIO_[A-Z0-9_]+\\*?)`")
	names = map[string]bool{}
	for line := range strings.SplitSeq(table, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		for _, m := range cell.FindAllStringSubmatch(first, -1) {
			if prefix, wild := strings.CutSuffix(m[1], "*"); wild {
				prefixes = append(prefixes, prefix)
			} else {
				names[m[1]] = true
			}
		}
	}
	if len(names) < 30 {
		t.Fatalf("the table lists %d variables, too few to be the table", len(names))
	}
	return names, prefixes
}

// variable matches a whole name of this repository's prefix.
var variable = regexp.MustCompile(`^LECTIO_[A-Z0-9_]*[A-Z0-9]$`)

// variablesSetIn is every LECTIO_ name a YAML tree sets: a map key, which is
// how a ConfigMap's data, a Secret's stringData and a compose service's
// environment name one, and the value of a name field, which is how a
// container's env entry does.
func variablesSetIn(tree any) []string {
	var out []string
	switch v := tree.(type) {
	case map[string]any:
		for key, value := range v {
			if variable.MatchString(key) {
				out = append(out, key)
			}
			if name, ok := value.(string); ok && key == "name" && variable.MatchString(name) {
				out = append(out, name)
			}
			out = append(out, variablesSetIn(value)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, variablesSetIn(item)...)
		}
	}
	return out
}

// TestTheDeployTreeSetsOnlyVariablesTheSpecLists: every LECTIO_ variable a
// file under deploy/ sets is a row of the configuration table. An example
// is copied as a whole, so a key no row describes is a setting its reader
// believes they configured.
func TestTheDeployTreeSetsOnlyVariablesTheSpecLists(t *testing.T) {
	names, prefixes := tableVariables(t)
	listed := func(name string) bool {
		return names[name] || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
	}
	set := map[string]bool{}
	for path, docs := range written(t) {
		for _, doc := range docs {
			for _, name := range variablesSetIn(doc) {
				set[name] = true
				if !listed(name) {
					t.Errorf("%s sets %s, which the configuration table of specs/016-distribution.md does not list", path, name)
				}
			}
		}
	}
	if len(set) < 20 {
		t.Fatalf("found %d variables set under deploy/, too few to be the tree", len(set))
	}
}

// credential matches the name of a variable that holds a secret.
var credential = regexp.MustCompile(`(_KEY|_TOKEN|_SECRET|PASSWORD|DATABASE_URL|DATABASE_POOL_URL)$`)

// TestNoManifestCarriesACredential: a container reads each credential from
// a Secret by a key of the variable's own name, no key is read twice, no
// ConfigMap holds one, and the one file that declares Secrets holds
// placeholders. The compose file runs on one machine with credentials that
// say they are examples.
func TestNoManifestCarriesACredential(t *testing.T) {
	secrets := 0
	for path, docs := range written(t) {
		for _, doc := range docs {
			switch {
			case doc["services"] != nil:
				services, _ := doc["services"].(map[string]any)
				for service, spec := range services {
					vars, _ := dig(spec, "environment").(map[string]any)
					for name, value := range vars {
						if credential.MatchString(name) && !strings.Contains(str(value), "example") {
							t.Errorf("%s: %s of %s does not say it is an example", path, name, service)
						}
					}
				}
			case kindOf(doc) == "Secret":
				secrets++
				if path != "deploy/bootstrap/secrets.example.yaml" {
					t.Errorf("%s declares the Secret %s; the template of deploy/bootstrap is the one place", path, nameOf(doc))
				}
				if doc["data"] != nil {
					t.Errorf("%s: %s carries encoded data, which a reader cannot see is a placeholder", path, nameOf(doc))
				}
				values, _ := doc["stringData"].(map[string]any)
				for key, value := range values {
					if !strings.Contains(str(value), "...") && !exampleAddress(str(value)) {
						t.Errorf("%s: %s of %s is neither a placeholder nor an example address", path, key, nameOf(doc))
					}
				}
			case kindOf(doc) == "ConfigMap":
				values, _ := doc["data"].(map[string]any)
				for key := range values {
					if credential.MatchString(key) {
						t.Errorf("%s: the ConfigMap %s holds %s, which is a credential", path, nameOf(doc), key)
					}
				}
			case kindOf(doc) == "Deployment":
				for _, c := range containers(doc) {
					read := map[string]string{}
					for name, e := range env(c) {
						ref := dig(e, "valueFrom", "secretKeyRef")
						if ref == nil {
							if credential.MatchString(name) {
								t.Errorf("%s: %s of %s is written in the manifest and not read from a Secret", path, name, nameOf(doc))
							}
							continue
						}
						if str(dig(ref, "key")) != name {
							t.Errorf("%s: %s reads the key %q, want the key of its own name", path, name, str(dig(ref, "key")))
						}
						key := str(dig(ref, "name")) + "/" + str(dig(ref, "key"))
						if first, taken := read[key]; taken {
							t.Errorf("%s: %s and %s both read %s", path, first, name, key)
						}
						read[key] = name
					}
				}
			}
		}
	}
	if secrets < 3 {
		t.Errorf("the template declares %d Secrets, too few to be the one the Deployments read", secrets)
	}
}

// address matches a URL's scheme and authority anywhere in a text.
var address = regexp.MustCompile("[a-z][a-z0-9+.-]*://([^/\\s\"'<>)}`,]*)")

// hosts returns the hosts of the URLs in a text, without user information
// and port. A URL with no host, such as a socket's, yields none.
func hosts(text string) []string {
	var out []string
	for _, m := range address.FindAllStringSubmatch(text, -1) {
		authority := m[1]
		if i := strings.LastIndex(authority, "@"); i >= 0 {
			authority = authority[i+1:]
		}
		host, _, _ := strings.Cut(authority, ":")
		if host != "" {
			out = append(out, host)
		}
	}
	return out
}

// exampleHost reports whether a host is nobody's: a name reserved for
// examples, a name with no dot, which resolves only inside a namespace or a
// compose project, or this machine.
func exampleHost(host string) bool {
	if !strings.Contains(host, ".") || host == "127.0.0.1" {
		return true
	}
	for _, reserved := range []string{"example", "example.com", "example.org", "example.net"} {
		if host == reserved || strings.HasSuffix(host, "."+reserved) {
			return true
		}
	}
	return false
}

// exampleAddress reports whether a value is a URL of an example host with
// no credential in it.
func exampleAddress(value string) bool {
	found := hosts(value)
	return len(found) == 1 && exampleHost(found[0]) && !strings.Contains(value, "@")
}

// shipped are the files an operator receives or copies from: the deploy
// tree, the image files and the page that says how to run them.
func shipped(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	read := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(path)] = string(data)
	}
	err := filepath.WalkDir("deploy", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			read(path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"Dockerfile", filepath.Join("docs", "running.md")} {
		read(path)
	}
	return out
}

// publisher is the name of whoever publishes this repository, assembled
// from pieces so this file is not its own finding.
const publisher = "late" + "re"

// publisherForms are the forms the publisher's name is legal in: the ones
// a schema, a module path, a license notice, a shared workflow and an image
// reference spell it.
var publisherForms = []string{
	"lectio." + publisher + ".ai/v1",              // the version every Reader and Policy document declares
	publisher + ".ai/x/lectio",                    // the module path, in a linker flag
	"Latere AI",                                   // the holder of the license notice
	publisher + "-ai/ci/.github/workflows/",       // the shared release-note workflow
	publisher + "gate",                            // the gate, a tool of go.mod
	"ghcr.io/" + publisher + "-ai/minio:RELEASE.", // the object store of the compose file
	"ghcr.io/" + publisher + "-ai/mc:RELEASE.",    // its client, which creates the bucket
}

// namesNoInstallation holds one file to naming no installation: the
// publisher appears in its legal forms alone and, where addresses are
// read, every address is an example's.
func namesNoInstallation(t *testing.T, path, text string, addresses bool) {
	t.Helper()
	for i, line := range strings.Split(text, "\n") {
		if addresses {
			for _, host := range hosts(line) {
				if !exampleHost(host) {
					t.Errorf("%s:%d names the host %q; an address in a shipped file is an example's", path, i+1, host)
				}
			}
		}
		for _, form := range publisherForms {
			line = strings.ReplaceAll(line, form, "")
		}
		if strings.Contains(strings.ToLower(line), publisher) {
			t.Errorf("%s:%d names the publisher outside the forms a schema, a module path and an image take: %s",
				path, i+1, strings.TrimSpace(line))
		}
	}
}

// TestNoShippedFileNamesAnInstallation: every address under deploy/ and in
// the page that runs it is an example's, and no shipped file names the
// coordinates of one installation.
func TestNoShippedFileNamesAnInstallation(t *testing.T) {
	for path, text := range shipped(t) {
		namesNoInstallation(t, path, text, true)
	}
}

// TestHostsAreReadFromAnAddress drives the address rule over planted
// values: a walk over a clean tree passes whether or not the rule works.
func TestHostsAreReadFromAnAddress(t *testing.T) {
	for value, want := range map[string]bool{
		"https://issuer.example":                                    true,
		"https://models.example.com/v1":                             true,
		"http://lectio-convert:8090":                                true,
		"unix:///run/lectio/convert.sock":                           true,
		"postgres://lectio:...@postgres.example.com:5432/lectio":    true,
		"postgres://lectio:example-password@postgres:5432/lectio":   true,
		"http://127.0.0.1:8080/v1":                                  true,
		"https://objects.internal.corp/bucket":                      false,
		"postgres://lectio:...@db.prod.internal:5432/lectio":        false,
		"see https://issuer.example and https://login.real.io/keys": false,
		"${LECTIO_OIDC_ISSUERS:-https://issuer.example}":            true,
		"the address `https://issuer.example`, in a page":           true,
	} {
		ok := true
		for _, host := range hosts(value) {
			ok = ok && exampleHost(host)
		}
		if ok != want {
			t.Errorf("%q passes: %v, want %v (hosts %v)", value, ok, want, hosts(value))
		}
	}
	if exampleAddress("https://user:secret@authz.example.com") || !exampleAddress("https://authz.example.com/authorize") {
		t.Error("an example address is a URL of an example host with no credential in it")
	}
}

// TestTheExampleReadersLoad: the Reader and Policy documents of the example
// are read by the loader lectiod reads them with, from a directory laid out
// as the ConfigMap is mounted, and both roles mount that ConfigMap where
// LECTIO_CONFIG points.
func TestTheExampleReadersLoad(t *testing.T) {
	objects := manifests(t)
	cm := find(t, objects, "ConfigMap", "lectiod-readers")
	data, _ := cm["data"].(map[string]any)
	if len(data) == 0 {
		t.Fatal("the example ConfigMap holds no document")
	}
	dir := t.TempDir()
	for key, value := range data {
		if err := os.WriteFile(filepath.Join(dir, key), []byte(str(value)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readers, err := config.Load(dir)
	if err != nil {
		t.Fatalf("the example documents do not load: %v", err)
	}
	if len(readers.Readers) != 1 || len(readers.Chain) != 1 || readers.Readers[readers.Chain[0]] == nil {
		t.Errorf("the example declares the readers %v in the chain %v, want one Reader the Policy names", slices.Sorted(maps.Keys(readers.Readers)), readers.Chain)
	}
	if len(readers.Unapplied) != 0 {
		t.Errorf("the example sets what this build does not apply: %v", readers.Unapplied)
	}

	for _, name := range []string{apiName, workerName} {
		d := find(t, objects, "Deployment", name)
		c := containers(d)[0]
		path := str(dig(env(c)["LECTIO_CONFIG"], "value"))
		mounted := ""
		for _, m := range list(dig(c, "volumeMounts")) {
			if str(dig(m, "mountPath")) == path {
				mounted = str(dig(m, "name"))
			}
		}
		source := ""
		for _, v := range list(dig(d, "spec", "template", "spec", "volumes")) {
			if str(dig(v, "name")) == mounted {
				source = str(dig(v, "configMap", "name"))
			}
		}
		if path == "" || source != nameOf(cm) {
			t.Errorf("%s reads its documents at %q, where the ConfigMap %q is mounted, want %s", name, path, source, nameOf(cm))
		}
	}
}

// finalStage is the text of a Dockerfile from its last FROM, which is the
// image that runs.
func finalStage(t *testing.T, path string) (whole, stage string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	whole = string(data)
	i := strings.LastIndex(whole, "\nFROM ")
	if i < 0 {
		t.Fatalf("%s has no stage", path)
	}
	return whole, whole[i:]
}

// TestTheImagesAreWhatTheManifestsRun holds the 2 Dockerfiles to what the
// manifests assume of them: the user the Pods run as, the ports the
// containers name, and for the server a static binary with its version
// stamped, alone on a base with no shell.
func TestTheImagesAreWhatTheManifestsRun(t *testing.T) {
	user := fmt.Sprintf("\nUSER %d:%d\n", uid, uid)

	whole, stage := finalStage(t, "Dockerfile")
	for _, want := range []string{
		"\nFROM gcr.io/distroless/static-debian12:nonroot\n", user, "\nEXPOSE 8080 8081\n",
		"\nENTRYPOINT [\"/usr/local/bin/lectiod\"]\n",
	} {
		if !strings.Contains(stage, want) {
			t.Errorf("Dockerfile: the image that runs lacks %q", strings.TrimSpace(want))
		}
	}
	if strings.Contains(stage, "\nRUN ") {
		t.Error("Dockerfile: the image that runs has a RUN step; it holds the binary and nothing installed beside it")
	}
	module, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	path, _, _ := strings.Cut(strings.TrimPrefix(string(module), "module "), "\n")
	for _, want := range []string{
		"CGO_ENABLED=0", "GOOS=${TARGETOS} GOARCH=${TARGETARCH}", "FROM --platform=$BUILDPLATFORM ",
		"-X " + path + "/internal/version.Version=${VERSION}",
		"-X " + path + "/internal/version.Commit=${COMMIT}",
		"-X " + path + "/internal/version.Date=${DATE}",
		"./cmd/lectiod",
	} {
		if !strings.Contains(whole, want) {
			t.Errorf("Dockerfile: the build lacks %q", want)
		}
	}

	whole, stage = finalStage(t, filepath.Join("deploy", "converter", "Dockerfile"))
	for _, want := range []string{user, "\nEXPOSE 8090\n", "\nENTRYPOINT [\"/usr/local/bin/lectio-convert\"]\n"} {
		if !strings.Contains(stage, want) {
			t.Errorf("deploy/converter/Dockerfile: the image that runs lacks %q", strings.TrimSpace(want))
		}
	}
	if !strings.Contains(whole, "-X "+path+"/internal/version.Version=${VERSION}") {
		t.Error("deploy/converter/Dockerfile: the build stamps no version, so a released sidecar would not name its release")
	}
}

// workflow reads one workflow of the repository as a tree.
func workflow(t *testing.T, name string) (object, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	docs, err := decode(name, data)
	if err != nil || len(docs) != 1 {
		t.Fatalf("%s is not one document: %v", name, err)
	}
	return docs[0], string(data)
}

// TestVerifyRendersTheDeployTreeOnEveryPush: the tests that render skip
// where kubectl is absent, which is every run of the gate. The deploy job
// of verify.yml runs them on a runner that has kubectl and fails on a
// skip, so they cannot be the tests nobody runs. A step that reads a
// command through a pipe fails on the command and not on the reader.
func TestVerifyRendersTheDeployTreeOnEveryPush(t *testing.T) {
	verify, _ := workflow(t, "verify.yml")
	job, ok := dig(verify, "jobs", "deploy").(map[string]any)
	if !ok {
		t.Fatal("verify.yml has no deploy job")
	}
	var steps string
	for _, s := range list(job["steps"]) {
		run := str(dig(s, "run"))
		steps += run + "\n"
		if strings.Contains(run, "| tee ") && (str(dig(s, "shell")) != "bash" || !strings.Contains(run, "set -o pipefail")) {
			t.Error("a step pipes into tee without `shell: bash` and `set -o pipefail`, so a failing command would pass")
		}
	}
	for _, want := range []string{"kubectl version --client", "go test -count=1 -v . ", "| tee ", "! grep -q -- '--- SKIP'"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the deploy job does not run %q, so a skipped render would pass unnoticed", want)
		}
	}
}
