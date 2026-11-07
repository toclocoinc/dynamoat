# Running dynamoat

dynamoat is a single Go binary that serves the DynamoDB API (JSON over HTTP,
AWS SDK v3 compatible) on port 8000. Point a client at it with
`AWS_ENDPOINT_URL_DYNAMODB=http://<host>:8000`.

## Docker Compose (Postgres)

The compose file starts dynamoat and a Postgres 17 database with a persistent
volume:

```
docker compose -f deploy/docker-compose.yml up -d
curl http://localhost:8000/readyz
```

Stop it and remove the database volume with:

```
docker compose -f deploy/docker-compose.yml down -v
```

The image is built from `deploy/Dockerfile` with the repository root as the
build context; build it directly with:

```
docker build -f deploy/Dockerfile .
```

## `go run`

Use the pinned Go toolchain (never a system `go`):

```
scripts/go.sh -C server run ./cmd/dynamoat
```

By default this runs with the in-memory store (`DYNAMOAT_STORE=memory`), which
is convenient for trying the API but loses everything on shutdown. For
development without Docker you can point it at a local Postgres instead:

```
DYNAMOAT_STORE=postgres \
DYNAMOAT_PG='postgres://dynamoat:dynamoat@127.0.0.1:5432/dynamoat?sslmode=disable' \
DYNAMOAT_AUTH=none \
scripts/go.sh -C server run ./cmd/dynamoat
```

`scripts/local-pg.sh` runs that Postgres as a Docker container (`dynamoat-pg`,
Postgres 17 on 127.0.0.1:5432, data in the `dynamoat-pgdata` volume): `up`,
`down` (keeps the data), `reset` (deletes it), `status`, `url`, `psql`.

## Environment variables

| Variable            | Meaning |
|---------------------|---------|
| `DYNAMOAT_LISTEN`   | Listen address (default `:8000`, DynamoDB Local's port). |
| `DYNAMOAT_AUTH`     | `sigv4` (default) or `none` for local development. |
| `DYNAMOAT_KEYS`     | Access key file, or directory of one file per key (required for `sigv4`). |
| `DYNAMOAT_REGION`   | Pin the region accepted in the credential scope (default: any). |
| `DYNAMOAT_STORE`    | `memory` or `postgres`; default `postgres` when `DYNAMOAT_PG` is set, `memory` otherwise. |
| `DYNAMOAT_PG`       | Postgres connection string for the `postgres` store. |
| `DYNAMOAT_PG_STATEMENT_TIMEOUT` | `statement_timeout` applied to new Postgres connections (Go duration, default `30s`). |
| `DYNAMOAT_PG_IDLE_TX_TIMEOUT`   | `idle_in_transaction_session_timeout` applied to new Postgres connections (Go duration, default `10s`). |
| `DYNAMOAT_STREAM_RETENTION`     | How long stream records are kept before the stream-trim job deletes them (Go duration, default `24h`). |
| `DYNAMOAT_LOG`      | `json` (default) or `text`. |
| `DYNAMOAT_METRICS`  | `off` disables the `/metrics` Prometheus endpoint. |

## Auth modes

- **`sigv4`** (the default) verifies every request's AWS Signature Version 4
  signature. It requires `DYNAMOAT_KEYS` to name a key file or a directory of
  key files, and optionally `DYNAMOAT_REGION` to pin the region. AWS SDK
  clients work unchanged as long as they have credentials in that key file.
  The keys are re-read on `SIGHUP` and every 30 s when the key file's (or
  directory's) modification time changes, so keys rotate without a restart.
- **`none`** accepts every request without a signature. Use it for local
  development only; the compose file sets it, and logs a warning at startup.

## Limits

dynamoat matches DynamoDB's client-visible limits (index counts, key lengths,
item size). One limit is not yet enforced: the 10 GB item collection size
limit. On a table with local secondary indexes, DynamoDB fails a write with
`ItemCollectionSizeLimitExceededException` when the total size of all items
sharing one partition key exceeds 10 GB. dynamoat does not check this, so an
item collection can grow past 10 GB without an error.

## Endpoints

| Endpoint    | Method | Meaning |
|-------------|--------|---------|
| `/`         | GET    | `200` `healthy: dynamoat` (also served at `/healthz`). |
| `/healthz`  | GET    | Same as `/`. |
| `/readyz`   | GET    | `200` `ready`, or `503` when the store cannot serve requests. |
| `/metrics`  | GET    | Prometheus text metrics (`dynamoat_requests_total`, `dynamoat_request_duration_seconds`, `dynamoat_inflight_requests`); disabled by `DYNAMOAT_METRICS=off`. Not authenticated, and some labels carry table names and stream target names: keep it off the public network or turn it off. |
| `/`         | POST   | The DynamoDB API. Route by the `X-Amz-Target` header, e.g. `DynamoDB_20120810.ListTables`. |

For example, a `ListTables` call without authentication:

```
curl -sS -X POST http://localhost:8000/ \
  -H 'X-Amz-Target: DynamoDB_20120810.ListTables' \
  -H 'Content-Type: application/x-amz-json-1.0' \
  -d '{}'
```

## Operations and limits

Keystone (the DynamoDB desktop client) reads table tags and the continuous
backup settings on every table, so dynamoat implements those operations with
DynamoDB's shapes and limits:

- **Tags.** `ListTagsOfResource` returns a table's tags in key order, 50 per
  page with an opaque `NextToken`. `TagResource` adds or replaces tags through
  `UpdateTable`; `UntagResource` removes them (missing keys are fine). The
  `ResourceArn` must be the table ARN `DescribeTable` reports; a stream or
  index ARN is a `ValidationException`, and a missing table is
  `ResourceNotFoundException`. Limits as DynamoDB: 50 tags per table, keys
  1-128 characters, values 0-256 characters, and keys may not start with
  `aws:`. `CreateTable` accepts `Tags` under the same limits.
- **Continuous backups.** `DescribeContinuousBackups` reports
  `ContinuousBackupsStatus: ENABLED` with
  `PointInTimeRecoveryStatus: DISABLED`; `UpdateContinuousBackups` returns the
  same description for `PointInTimeRecoveryEnabled: false`, and rejects `true`
  with a `ValidationException`. dynamoat's durability and point-in-time
  recovery come from the managed Postgres server (see `docs/runbook.md`
  section 4), not per table.

## Global tables

The global tables API (both the current `UpdateTable` `ReplicaUpdates` and the
legacy `CreateGlobalTable` / `DescribeGlobalTable` / `UpdateGlobalTable` /
`ListGlobalTables` / `*GlobalTableSettings` / `*TableReplicaAutoScaling`
operations) is implemented as bookkeeping. dynamoat serves a table from one
database in every region a request is signed for, so a "replica" in another
region is the same table; nothing is copied between sites. The replicas only
make `DescribeTable` and the global tables API answer as DynamoDB does, so
desktop clients and IaC tools that manage replicas work unchanged.

To actually run the table in several sites, point each dynamoat at the same
database (the single source of truth), or replicate the database itself; real
cross-site replication is out of scope. A table's own region — `DYNAMOAT_REGION`,
or the request's signing region when it is unset — is always the first replica
once any replica exists.
