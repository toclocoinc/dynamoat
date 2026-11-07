# dynamoat Helm chart

dynamoat serves the DynamoDB API (JSON over HTTP, AWS SDK v3 compatible) from
PostgreSQL. This chart deploys it as a stateless, horizontally-scalable
ClusterIP service. It deliberately has **no Ingress** — dynamoat is an internal
service and must stay inside the cluster.

The Docker image is `ghcr.io/toclocoinc/dynamoat`, listens on port 8000 and
runs as a non-root user (65532) with no shell.

## Install

```console
helm install dynamoat ./deploy/helm/dynamoat \
  --set auth.keys.AKIAIOSFODNN7EXAMPLE='wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY' \
  --set postgres.connectionString='postgres://user:password@db:5432/dynamoat?sslmode=require'
```

With a user-created access-key Secret (one file per key: file name = access
key ID, file content = secret):

```console
helm install dynamoat ./deploy/helm/dynamoat \
  --set auth.existingSecret=my-dynamoat-keys \
  --set postgres.existingSecret=my-dynamoat-pg \
  --set postgres.secretKey=connection-string
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `replicaCount` | int | `2` | Number of replicas. |
| `image.repository` | string | `ghcr.io/toclocoinc/dynamoat` | Image repository. |
| `image.tag` | string | `""` | Image tag; defaults to the chart `appVersion` when empty. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | list | `[]` | Pull secrets for the image. |
| `nameOverride` | string | `""` | Override the chart name used in resources. |
| `fullnameOverride` | string | `""` | Override the fully-qualified resource name. |
| `store` | string | `postgres` | Backend store (`DYNAMOAT_STORE`). |
| `serviceAccount.create` | bool | `true` | Create a ServiceAccount. |
| `serviceAccount.name` | string | `""` | ServiceAccount name (generated when empty). |
| `serviceAccount.annotations` | map | `{}` | ServiceAccount annotations (e.g. Workload Identity). |
| `podAnnotations` | map | `{}` | Pod annotations. |
| `podLabels` | map | `{}` | Extra pod labels. |
| `podSecurityContext` | map | `{}` | Pod-level security context. |
| `securityContext` | map | see below | Container security context (runAsNonRoot, readOnlyRootFilesystem, drop ALL capabilities, seccomp RuntimeDefault). |
| `service.type` | string | `ClusterIP` | Service type (ClusterIP only). |
| `service.port` | int | `8000` | Service port. |
| `resources` | map | `{}` | Container resources. |
| `autoscaling.enabled` | bool | `false` | Enable the HorizontalPodAutoscaler. |
| `autoscaling.minReplicas` | int | `2` | HPA minimum replicas. |
| `autoscaling.maxReplicas` | int | `10` | HPA maximum replicas. |
| `autoscaling.targetCPUUtilizationPercentage` | int | `70` | HPA CPU target (%). |
| `topologySpreadConstraints.maxSkew` | int | `1` | Max skew across zones. |
| `topologySpreadConstraints.topologyKey` | string | `topology.kubernetes.io/zone` | Topology key. |
| `topologySpreadConstraints.whenUnsatisfiable` | string | `ScheduleAnyway` | Soft (ScheduleAnyway) spread. |
| `auth.mode` | string | `sigv4` | `sigv4` or `none` (no authentication). |
| `auth.existingSecret` | string | `""` | User-created Secret of access keys (one file per key). |
| `auth.keys` | map | `{}` | Map of access key ID -> secret, rendered into a Secret. |
| `auth.region` | string | `""` | Pin the SigV4 credential-scope region. |
| `postgres.existingSecret` | string | `""` | User-created Secret holding the connection string. |
| `postgres.secretKey` | string | `connection-string` | Key holding the connection string (`DYNAMOAT_PG`). |
| `postgres.connectionString` | string | `""` | Connection string, rendered into a Secret when no `existingSecret`. |
| `postgres.auth` | string | `password` | `password` or `azure-workload-identity` (`DYNAMOAT_PG_AUTH`). |
| `postgres.maxConns` | int | `10` | Maximum Postgres connections in the pool. |
| `postgres.minConns` | int | `0` | Minimum Postgres connections in the pool. |
| `cloudSqlProxy.enabled` | bool | `false` | Add the Cloud SQL Auth Proxy sidecar. |
| `cloudSqlProxy.image.repository` | string | `gcr.io/cloud-sql-connectors/cloud-sql-proxy` | Proxy image. |
| `cloudSqlProxy.image.tag` | string | `2.15.2` | Proxy image tag (pinned 2.x). |
| `cloudSqlProxy.image.pullPolicy` | string | `IfNotPresent` | Proxy image pull policy. |
| `cloudSqlProxy.instance` | string | `""` | Cloud SQL instance connection name. |

## Authentication

With `auth.mode: sigv4` (the default) the chart mounts a Secret as a directory
at `DYNAMOAT_KEYS`, with **one file per access key** — the file name is the
access key ID and the file content is the secret (see
`server/internal/sigv4/keys.go`). Either point `auth.existingSecret` at a
Secret you created, or list the keys under `auth.keys` and the chart renders
one. `auth.mode: none` disables authentication; `helm template`/`install`
prints a warning, and it must never be used in production.

## Profiles

### GKE + Cloud SQL (`values-gke-cloudsql.yaml`)

Runs the [Cloud SQL Auth Proxy](https://github.com/GoogleCloudPlatform/cloud-sql-proxy)
as a native sidecar (an initContainer with `restartPolicy: Always`), pinned to
a 2.x tag, with `--auto-iam-authn --private-ip --port 5432`. `DYNAMOAT_PG`
points at `127.0.0.1:5432` with the Cloud SQL IAM database user, and the proxy
turns the pod's Workload Identity into a short-lived token, so no password is
needed. The ServiceAccount carries the `iam.gke.io/gcp-service-account`
annotation.

```console
helm install dynamoat ./deploy/helm/dynamoat -f ./deploy/helm/dynamoat/values-gke-cloudsql.yaml
```

Edit the profile first: set `cloudSqlProxy.instance`, the
`iam.gke.io/gcp-service-account` annotation, and the IAM user / database in
`postgres.connectionString`.

### AKS + Azure Flexible Server (`values-aks-flexible.yaml`)

Targets Azure Database for PostgreSQL Flexible Server with `sslmode=require`
and `postgres.auth: azure-workload-identity`. The pod carries the
`azure.workload.identity/use: "true"` label and the ServiceAccount carries the
`azure.workload.identity/client-id` annotation. The AKS workload identity
webhook injects the `AZURE_*` environment variables and the federated service
account token; the server exchanges that token for a short-lived Entra access
token and uses it as each connection's password, so the connection string
(`postgres.existingSecret`) has no password:

```
postgres://ENTRA_USER@SERVER.postgres.database.azure.com:5432/DATABASE?sslmode=require
```

```console
helm install dynamoat ./deploy/helm/dynamoat -f ./deploy/helm/dynamoat/values-aks-flexible.yaml
```

## Using as a subchart

Add dynamoat to the parent chart's `Chart.yaml` and set its values under the
`dynamoat` prefix:

```yaml
dependencies:
  - name: dynamoat
    version: 0.1.0
    repository: "oci://ghcr.io/toclocoinc/dynamoat-helm"
    condition: dynamoat.enabled
```

```yaml
dynamoat:
  enabled: true
  auth:
    keys:
      AKIAIOSFODNN7EXAMPLE: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
  postgres:
    existingSecret: my-dynamoat-pg
    secretKey: connection-string
```

Every value in this chart's `values.yaml` is then reachable under the
`dynamoat` key, and client Lambdas point at it with
`AWS_ENDPOINT_URL_DYNAMODB` unchanged.
