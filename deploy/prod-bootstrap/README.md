# Production bootstrap

This is Latere's own installation, not the install path: the public one is
`deploy/bootstrap`. The release pipeline never reads this directory.
Everything it applies runs again on every release, so everything it applies
must be re-appliable, and the resources here are not: a token Secret is
minted once, and an identity that could rewrite its own Role could grant
itself anything. An operator applies them.

The Secrets the Deployments read (`lectio-db`, `lectiod-s3`,
`lectiod-authorizer`, `lectiod-keys`, `platformd-internal-ca` and
`ghcr-secret`) are created outside this tree and keep their names. The core
migrates its database at start, so there is no migration Job to run before
a release.

The NetworkPolicy `lectiod-platformd-tls`, which admits the core's Pods to
platformd's internal TLS listener, is applied by platform's release
(latere-ai/platform, `deploy/prod`). No operator step remains for it.

## The rollout identity

`rollout-identity.yaml` is the account the release pipeline deploys with: a
ServiceAccount `lectiod-rollout`, a Role and RoleBinding in the `latere`
namespace over the kinds `deploy/prod` contains, and the token Secret bound
to the account. A kind added to `deploy/prod` is added to the Role and
applied again by an operator.

```sh
kubectl apply -f deploy/prod-bootstrap/rollout-identity.yaml

NS=latere SA=lectiod-rollout
kubeconfig=$(mktemp) && chmod 600 "$kubeconfig"
KUBECONFIG="$kubeconfig" kubectl config set-cluster latere-k8s \
  --server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
KUBECONFIG="$kubeconfig" kubectl config set clusters.latere-k8s.certificate-authority-data \
  "$(kubectl -n "$NS" get secret "$SA-token" -o jsonpath='{.data.ca\.crt}')"
KUBECONFIG="$kubeconfig" kubectl config set-credentials "$SA" \
  --token="$(kubectl -n "$NS" get secret "$SA-token" -o jsonpath='{.data.token}' | base64 -d)"
KUBECONFIG="$kubeconfig" kubectl config set-context "$SA" \
  --cluster=latere-k8s --user="$SA" --namespace="$NS"
KUBECONFIG="$kubeconfig" kubectl config use-context "$SA"

# The server-side dry run authorizes every object a release applies without
# changing any, so a Role that misses a kind fails here and not in a release.
KUBECONFIG="$kubeconfig" kubectl apply -k deploy/prod --dry-run=server

gh secret set LECTIO_KUBECONFIG --repo latere-ai/lectio < "$kubeconfig"
rm "$kubeconfig"
gh variable set LECTIO_RELEASE_DEPLOY --repo latere-ai/lectio --body 1
```

The `deploy` job of `.github/workflows/release.yml` runs only while the
repository variable `LECTIO_RELEASE_DEPLOY` is set, so a fork, which sets
neither it nor the secret, publishes every artifact and deploys nothing.
Unsetting the variable turns the deploy off again.

Rotating the credential is deleting the token Secret, applying the identity
file again, and running the same steps.
