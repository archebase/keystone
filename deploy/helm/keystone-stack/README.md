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
mysql storageClass: ebs-ssd
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
  --set keystone.syncEnabled=true \
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

### Production

The `Deploy Keystone Stack` workflow provides the production manual deployment
entry point on the repository default branch, `main-v2`. The deployment job runs
on the `archebase-ci-runner` self-hosted runner and executes inside the
configured deploy job container. Run it only after the matching Keystone image
workflow has successfully pushed the selected Keystone commit SHA.
The workflow enables Keystone sync for production by passing
`keystone.syncEnabled=true` to the Helm release.

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

### Staging

The `Deploy Keystone Staging` workflow is the manual staging entry point on
`main-v2`. It promotes the same immutable `prod/keystone` and `prod/synapse`
registry artifacts used by the production workflow instead of rebuilding
environment-specific images. All runtime coordinates still come only from the
`staging` GitHub environment, staging kubeconfig, staging application
credentials, and staging infrastructure configuration. The workflow requires
the IngressClass, TOS bucket, IAM roles, and Hilbert endpoint to identify
staging, then verifies the ALB and workload identity against the staging
cluster before it mutates any Kubernetes resource.

Staging uses one fixed Helm release, factory identifier, and hostname that
cannot collide with production:

```text
keystone-staging.archebase.cn
```

The workflow does not accept a staging release name. Helm release name and
`KEYSTONE_FACTORY_ID` are both fixed to `keystone-staging`.
`keystoneImageTag` may be empty to select the checked-out `main-v2` HEAD;
`synapseImageTag` must be supplied explicitly. Both resolved image tags must
be full 40-character lowercase commit SHAs.

Before dispatching the workflow, create the GitHub `staging` environment and
restrict it to the intended reviewers and `main-v2`, then configure these
secrets:

- `STAGING_KUBECONFIG_B64`: one-line base64 encoding of the cloud-infra staging
  `ci_kubeconfig` output. Its current context and referenced cluster name must
  each identify staging.
- `STAGING_KEYSTONE_HILBERT_ACCESS_KEY`
- `STAGING_KEYSTONE_HILBERT_SECRET_KEY`
- `VOLCENGINE_CR_USERNAME`
- `VOLCENGINE_CR_PASSWORD`

Configure these repository or `staging` environment variables from the reviewed
staging infrastructure outputs and application configuration:

- `STAGING_KEYSTONE_ALB_DNS_NAME`
- `STAGING_KEYSTONE_INGRESS_CLASS`
- `STAGING_KEYSTONE_TOS_BUCKET`
- `STAGING_KEYSTONE_TOS_UPLOAD_ROLE_TRN`
- `STAGING_KEYSTONE_TOS_READ_ROLE_TRN`
- `STAGING_KEYSTONE_IRSA_ROLE_TRN`: cloud-infra output
  `tos_irsa_workload_role_trns["keystone"]`
- `STAGING_KEYSTONE_GRPC_LISTENER_PORT`
- `STAGING_KEYSTONE_HILBERT_BASE_URL`

The staging cluster must already contain:

- namespace `archebase-system`, labeled
  `vke.volcengine.com/pod-identity-injection-enabled=true`, and storage class
  `ebs-ssd`;
- ServiceAccount `archebase-system/keystone`, labeled
  `archebase.io/environment=staging` and with its
  `vke.volcengine.com/role-trn` annotation exactly matching
  `STAGING_KEYSTONE_IRSA_ROLE_TRN`;
- a staging-only Keystone IngressClass backed by a running Standard
  `ALBInstance`, with HTTP/2 HTTPS listeners on port `443` and the dedicated
  gRPC port configured in `STAGING_KEYSTONE_GRPC_LISTENER_PORT`;
- a staging TOS bucket plus upload and read roles that the Keystone workload
  identity can assume;
- a CNAME from `keystone-staging.archebase.cn` to
  `STAGING_KEYSTONE_ALB_DNS_NAME`.

The workflow creates or updates only the application-owned
`archebase-system/keystone-staging-registry` image pull Secret. It does not
create cloud infrastructure, IAM roles, DNS records, namespaces, storage
classes, or workload identities. Dispatch requires `dnsCnameReady=true` and
`confirmStaging=deploy-staging`; Helm uses `--atomic` so a failed rollout is
rolled back.

## Routing

Synapse uses the same-origin `/api/v1` path. The chart's HTTP Ingress sends
`/api/v1/*`, `/api/v1`, `/api/*`, `/api`, `/swagger/*`, and `/swagger` to
Keystone HTTP, `/transfer/*` and `/transfer` to Keystone's transfer WebSocket
port, `/recorder/*` and `/recorder` to Keystone's recorder WebSocket port, and
`/*` plus `/` to Synapse. Volcengine Standard ALB treats `paths.pathType` as
validation-only and defaults to exact path matching, so wildcard paths are
rendered explicitly where prefix matching is required.

Keystone also serves root and `/api` health responses for ALB backend health
checks. The public root path still routes to Synapse because the HTTP Ingress
sends `/` to the Synapse Service.

The gRPC Ingress is separate so the VKE ALB annotations can select a dedicated
listener and backend protocol `grpc` without affecting browser/API routing. It
renders the wildcard path `/*` because Volcengine Standard ALB treats Kubernetes
`pathType: Prefix` as validation-only. It also uses TCP health checks because
Keystone's DGW gRPC listener is not an HTTP health endpoint. Production uses
listener `50053`; staging uses `STAGING_KEYSTONE_GRPC_LISTENER_PORT`. Clients
connect to the corresponding environment endpoint:

```text
production: keystone-<releaseName>.archebase.cn:50053
staging:    keystone-staging.archebase.cn:<STAGING_KEYSTONE_GRPC_LISTENER_PORT>
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
- The default MySQL memory request is intentionally small for the current
  resource-constrained Volcengine prod node pool; raise
  `mysql.resources.requests.memory` for larger dedicated deployments.
- Keystone uses a no-surge rolling update strategy by default so upgrades fit
  the current resource-constrained Volcengine prod node pool. This can create a
  short Keystone API/WebSocket interruption during image updates.
- StatefulSet PVCs are retained after `helm uninstall`. Remove them explicitly
  only when the stack data is no longer needed.
- The default amd64 node selectors match the currently verified CR images.
