# Keystone Stack Helm Chart

One Helm release installs exactly one isolated stack:

- one Keystone Deployment and Service;
- one Synapse Deployment and Service;
- one MySQL StatefulSet, headless Service, and release-specific PVC.

Every selector and resource name includes the Helm release identity. Install the
chart repeatedly in the same namespace with different release names to create
independent Keystone instances.

## Production Defaults

The chart is production-first for Volcengine prod. Defaults assume the
`cloud-infra` Keystone shared resources have already been applied.

Default image repositories:

```text
archebase-cr-cn-beijing.cr.volces.com/prod/keystone:<keystoneCommit>
archebase-cr-cn-beijing.cr.volces.com/prod/synapse:<synapseCommit>
archebase-cr-cn-beijing.cr.volces.com/upstream/mysql:8.4.10
```

Keystone and Synapse tags are required at deploy time. Use the full Git commit
SHA that produced each image; do not use mutable tags such as `latest` or
`main-v2-latest`. The production image workflow publishes Keystone as
`archebase-cr-cn-beijing.cr.volces.com/prod/keystone:${GITHUB_SHA}` after a
successful `main-v2` push.

Default infrastructure contract:

```text
namespace: archebase-system
imagePullSecret: keystone-production-registry
serviceAccount: keystone
ingressClass: keystone-prod
keystone service type: NodePort
synapse service type: NodePort
object storage: TOS bucket archebase-prod-keystone-2117611051
HTTP/WebSocket/API listener: 443
gRPC listener: 50053
```

VKE ALB Ingress validates non-pass-through backends and rejects `ClusterIP`
Services. The chart therefore defaults the externally routed Keystone and
Synapse Services to `NodePort`; MySQL remains internal.
Ingress backend ports are rendered as numeric Service ports instead of named
ports because the same VKE ALB webhook validates the referenced numeric port.

The MySQL image manifest was verified in Volcengine CR on 2026-07-23 and
contains `linux/amd64` with manifest digest
`sha256:02aa6476f6b675e5d4d19c5b437798b1b8a4048ae39383eac609814998ed15b8`.

## Release Name

Use the Helm release name as the Keystone instance identifier. It must match
`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` and have length 1-53.

The chart derives exactly one public hostname:

```text
keystone-<releaseName>.archebase.cn
```

For example, `releaseName=factory-a` derives
`keystone-factory-a.archebase.cn`.

## Credentials

The chart creates a release-local Secret unless `credentials.existingSecret` is
set. These local values are auto-generated on first install and reused on
upgrade:

- `mysql-root-password`
- `mysql-password`
- `jwt-secret`
- `admin-password`

Hilbert credentials cannot be generated. Production installs must provide:

- `hilbert-access-key`
- `hilbert-secret-key`

Production TOS uses VKE IRSA through the `keystone` ServiceAccount. Do not set
`storage-access-key` or `storage-secret-key` for production TOS. Those keys are
only used by `storage.type=s3`.

To read an auto-generated admin password:

```bash
kubectl -n archebase-system get secret <releaseName>-credentials \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

If `credentials.existingSecret` is set, replace `<releaseName>-credentials` with
that Secret name.

## Install

Production install example:

```bash
helm upgrade --install factory-a deploy/helm/keystone-stack \
  --namespace archebase-system \
  --set keystone.image.tag="${KEYSTONE_COMMIT}" \
  --set synapse.image.tag="${SYNAPSE_COMMIT}" \
  --set credentials.hilbertAccessKey="${KEYSTONE_HILBERT_ACCESS_KEY}" \
  --set credentials.hilbertSecretKey="${KEYSTONE_HILBERT_SECRET_KEY}"
```

Before exposing the release publicly, create the DNS CNAME:

```text
keystone-factory-a.archebase.cn -> alb-1pfcqpoqechs0845wfb1q8bq4.cn-beijing.volcenginealb.com
```

To deploy multiple independent stacks, run the same command with different Helm
release names. MySQL PVCs remain release-specific.

## GitHub Actions Deployment

The `Deploy Keystone Stack` workflow provides the production manual deployment
entry point on the repository default branch, `main-v2`. The deployment job runs
on the `archebase-ci-runner` self-hosted runner and executes inside the
configured deploy job container. Run it only after the matching Keystone image
workflow has successfully pushed the selected Keystone commit SHA.

Required repository or `production` environment secrets:

- `PROD_KUBECONFIG_B64`: one-line base64 encoding of a production kubeconfig
  that can deploy to the Volcengine production cluster. The workflow uses the
  kubeconfig's own `current-context`; it does not require a fixed context name
  or a fixed Kubernetes user identity.
- `KEYSTONE_HILBERT_ACCESS_KEY`
- `KEYSTONE_HILBERT_SECRET_KEY`
- `VOLCENGINE_CR_USERNAME`
- `VOLCENGINE_CR_PASSWORD`

Generate `PROD_KUBECONFIG_B64` from the kubeconfig file without printing the
raw token in logs:

```sh
base64 < prod-kubeconfig.yaml | tr -d '\n'
```

Workflow inputs:

- `releaseName`: the Helm release name and Keystone instance identifier.
- `keystoneImageTag`: optional full Keystone commit SHA. Empty uses the
  checked-out `main-v2` HEAD.
- `synapseImageTag`: full Synapse commit SHA.
- `dnsCnameReady`: must be `true` after
  `keystone-<releaseName>.archebase.cn` points to the production Keystone ALB.
- `confirmProduction`: must be exactly `deploy-production`.
- `jobContainerImage`: optional deploy job container image override. Empty uses
  `CI_JOB_CONTAINER_IMAGE` or the workflow default deploy image.

The workflow deploys into `archebase-system` using the kubeconfig's
`current-context`. It does not create DNS records or manage cloud-infra
resources. Keep the GitHub `production` environment protected so production
deployments require the intended reviewer approval.

## Routing

Synapse uses the same-origin `/api/v1` path. The chart's HTTP Ingress sends
`/api` and `/swagger` to Keystone, `/transfer` and `/recorder` to Keystone's
WebSocket ports, and all remaining paths to Synapse.

The gRPC Ingress is separate so the VKE ALB annotations can select listener
`50053` and backend protocol `grpc` without affecting browser/API routing.
gRPC clients connect to:

```text
keystone-<releaseName>.archebase.cn:50053
```

## S3 Override

S3-compatible storage is still supported for local or non-prod use. Set
`storage.type=s3`, provide `storage.s3.*`, and include `storage-access-key` and
`storage-secret-key` in the credentials Secret or inline values. Also override
prod-specific defaults such as `serviceAccount` and `ingress` for the target
cluster.

## Operational Notes

- MySQL credentials initialize a fresh data directory only. Changing them after
  the PVC contains data does not alter existing MySQL accounts.
- StatefulSet PVCs are retained after `helm uninstall`. Remove them explicitly
  only when the stack data is no longer needed.
- The default amd64 node selectors match the currently verified CR images.
