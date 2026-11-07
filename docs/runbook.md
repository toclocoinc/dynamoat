# dynamoat operations runbook

How to run dynamoat in production on a managed Postgres (Cloud SQL, Azure
Database for PostgreSQL Flexible Server). Everything here is checked against
the code in `server/`; where dynamoat does not yet do something an operator
would expect, it says so in place. Values that are tuning *starting points*
rather than code constants are marked as such.

The quick reference for configuration is [docs/running.md](running.md); the
storage and key-encoding model is [docs/design.md](design.md); the Helm chart is
[deploy/helm/dynamoat/README.md](../deploy/helm/dynamoat/README.md).

## 1. Architecture

One deployment is **stateless API pods in front of one Postgres database**. The
pods are the same Go binary (`server/cmd/dynamoat`), built onto a distroless
non-root image, listening on port 8000. They are horizontally scalable: any
pod can serve any request, and there is no shared in-process state except
TransactWriteItems idempotency, which is per-pod and best-effort. The Helm
chart exposes them as a ClusterIP Service with no Ingress (`service.type`
defaults to `ClusterIP`).

The database is the single source of truth. A DynamoDB *table* is a row in
`ddb_tables`, not a Postgres table; every item of every table lives in `items`,
GSI entries in `index_entries`, stream records in `stream_records`, consumer
checkpoints in `stream_checkpoints`, the stream position counter in `stream_pos`,
job leases in `leases`, and applied migrations in `schema_migrations`.

Background work falls into two groups: leased jobs and per-pod workers.

| Job | Started by | Runs on one pod? | Cadence / batch |
|-----|-----------|------------------|-----------------|
| TTL expiry | `jobs.Run` (`server/internal/jobs`) | **Yes** — lease `ttl` | every `DYNAMOAT_TTL_INTERVAL` (default `1s`), 1000 items/batch |
| Stream trim | `jobs.Run` | **Yes** — lease `stream-trim` | every 1 minute, 10000 records/batch, `DYNAMOAT_STREAM_RETENTION` horizon (default 24 h) |
| GSI backfill | `postgres` store (`workers.go`), on `UpdateTable` and at startup | **No** — every pod that starts it | 1000 items/batch |
| Table delete | `postgres` store, on `DeleteTable` and at startup | **No** — every pod that starts it | 1000 rows/batch per table |
| Stream dispatch | `dispatch.Run`, when `DYNAMOAT_STREAM_TARGETS` is set (being built) | **Yes** — lease per target and table | see `docs/streams.md` once it lands |

**Leases.** `TryLease` upserts a row in `leases` (`name`, `owner`, `expires_at`)
and only succeeds if the row is unowned or the previous lease has expired
(`streams.go`). `jobs.Run` holds the lease with a 30 s TTL and renews every 10 s;
the owner is `hostname-pid`. While another pod holds the lease the job does no
work at all. Only `ttl` and `stream-trim` are leased.

**Not leased: GSI backfill and table delete.** These are started by whichever
pod commits the `UpdateTable`/`DeleteTable`, and *also* by every pod at startup
for work a previous run left behind (`resumeBackfills`, `resumeDeletes`). So
several pods can run the same backfill or delete at once. That is safe: each
backfill batch re-reads the item under its advisory lock and inserts with
`ON CONFLICT DO NOTHING`, and each delete batch removes rows by `ctid`, so the
operations are idempotent and cannot deadlock (locks are taken in a fixed
order). But there is no lease keeping a single pod "the" backfiller, so
concurrent pods will share the work rather than one owning it.

**Stream dispatch is inline, not a background job.** A stream record is written
in the same transaction as its item write (`buildStreamRecord` in `items.go`).
Its readable position (`pos`) is not the insert-time `seq`: a later transaction
can commit first, so `Store.sequence` (`streams.go`) renumbers records by
`(xid, seq)` only once their transaction ID is below the oldest still-running
transaction. `sequence` runs lazily when a reader calls `ReadStream` /
`StreamEnd` (that is, `GetRecords` or a `LATEST` shard iterator), guarded by
`pg_try_advisory_xact_lock(7344)`. The stream dispatcher (being built)
reads the same way and delivers records to HTTP consumers; until
it lands, records are only read through the DynamoDB Streams API.

**Writes.** Each item write is one Postgres transaction. It sets
`lock_timeout` to `DYNAMOAT_LOCK_TIMEOUT` (default `5s`), takes
`pg_advisory_xact_lock(table_id, itemLock)`, reads the item, evaluates the
condition, writes the item, diffs and writes GSI entries, and writes the stream
record. `TransactWriteItems` takes its locks in sorted `(table_id, lock)` order.
A lock wait that exceeds the timeout — or a deadlock — is mapped to
`ProvisionedThroughputExceededException`, so SDK clients back off and retry.

## 2. Sizing

**Postgres connections.** Each pod's pool holds at most
`DYNAMOAT_PG_MAX_CONNS` connections (default 10; `DYNAMOAT_PG_MIN_CONNS`,
default 0, keeps some open). The Helm values are `postgres.maxConns` and
`postgres.minConns`. Other pool settings are pgx v5's defaults
(`MaxConnLifetime = 1h`, `MaxConnIdleTime = 30m`, `HealthCheckPeriod = 1m`).

Budget the database side as `replicas × DYNAMOAT_PG_MAX_CONNS`, plus headroom
for the background jobs (they use pool connections) and for admin/psql access:

```text
2 replicas  × 10 =  20 connections
10 replicas × 10 = 100 connections   # HPA max in the chart
```

Set Postgres `max_connections` comfortably above the HPA maximum (a starting
point is 100–200) and alert when `pg_stat_activity` approaches it. On Azure
Flexible Server `max_connections` is derived from the tier's vCores/memory, so
you scale the tier rather than a free parameter; on Cloud SQL it is a settable
flag.

**Postgres instance size.** Start small and scale on signal: 2 vCPU / 8 GB with
enough storage headroom for autovacuum (start at 100 GB and set
`autovacuum` storage growth) is a reasonable starting point for a first
tenant-per-table workload. Scale up when any of these move: lock contention
(rising `ProvisionedThroughputExceededException` rate), write latency, or
connection count near `max_connections`.

**Pods.** The chart ships `resources: {}`; its own comments suggest
`requests: cpu 250m / memory 256Mi` and `limits: cpu 1 / memory 512Mi`, with
HPA on CPU at 70% (`autoscaling.targetCPUUtilizationPercentage`) between 2 and
10 replicas. Start there. The server is I/O-bound on Postgres, so watch
`dynamoat_request_duration_seconds` and the error rates rather than pod CPU
alone.

## 3. Postgres settings

- **Version:** Postgres 15–18, **17 recommended**. The server refuses to start
  below 15 (`serverVersionTooOld` checks `server_version_num >= 150000`).
- **`fillfactor = 85`** on `items` is already set by migration `0001`.
- **Autovacuum** (starting points; `items` is update-heavy because an upsert
  rewrites the row in place via `ON CONFLICT ... DO UPDATE`):
  ```sql
  ALTER SYSTEM SET autovacuum_vacuum_scale_factor = 0.05;   -- or 0.02 on a busy items table
  ALTER SYSTEM SET autovacuum_vacuum_insert_scale_factor = 0.05;
  ALTER SYSTEM SET autovacuum_vacuum_cost_limit = 2000;     -- default 200
  ALTER SYSTEM SET autovacuum_vacuum_cost_delay = 2;        -- ms; 0 makes it aggressive
  ALTER SYSTEM SET autovacuum_naptime = '30s';
  ```
  The trade-off: more aggressive vacuuming keeps `items` and `index_entries`
  from bloating but costs I/O. Watch `pg_stat_user_tables.n_dead_tup` /
  `last_autovacuum`.
- **`work_mem`:** a starting point of 16–64 MB. The item JSON is stored as
  `bytea` and is never parsed in SQL, so the big sorts/joins are over
  `index_entries` and stream reads, not item payloads.
- **Timeouts.** dynamoat sets `statement_timeout` and
  `idle_in_transaction_session_timeout` on every new connection as runtime
  parameters, from `DYNAMOAT_PG_STATEMENT_TIMEOUT` (default `30s`) and
  `DYNAMOAT_PG_IDLE_TX_TIMEOUT` (default `10s`). A statement that runs past the
  statement timeout on a data-plane request is returned as an
  `InternalServerError` and logged with the operation.
  `idle_in_transaction_session_timeout` matters most: an abandoned transaction
  keeps its snapshot alive, which stalls stream sequencing (below). dynamoat
  *also* sets a per-write `lock_timeout` via `SET LOCAL lock_timeout` (from
  `DYNAMOAT_LOCK_TIMEOUT`, default 5 s); leave that to the server, don't
  override it globally.
- **Why a long-running transaction stalls stream delivery.** A stream record's
  `pos` is assigned only when its `xid` is below `pg_snapshot_xmin` of the
  current snapshot — that is, below the oldest transaction still running
  anywhere in the database (`docs/design.md` and `streams.go`). A single
  long-running or idle-in-transaction session therefore holds back *every*
  record committed after it started, for *every* stream, because the renumberer
  cannot prove those records won't be overtaken by an even older transaction.
  This is the first thing to check when `GetRecords` goes quiet (see §6 and
  §7).

## 4. Backups and restore

Use the managed platform's automated backups and point-in-time restore (PITR);
dynamoat has no backup mechanism of its own.

- **Cloud SQL:** enable automated backups (default daily + transaction logs for
  PITR) and set the retention window. Restore to a new instance
  (`gcloud sql instances create` / the console's clone/restore flow) or
  in-place PITR, then point `DYNAMOAT_PG` at the restored instance.
- **Azure Flexible Server:** automated backups are on by default (7-day
  retention, extendable to 35). PITR restores to a new server; re-point the
  connection string.

**What a restore means for `stream_checkpoints`.** Checkpoints live in
`stream_checkpoints` inside the same database as `stream_records` and
`stream_pos`, so a PITR restore rolls all three back together to the restore
point. A consumer that checkpointed *after* the restore point now has a
checkpoint `seq` that is ahead of the restored stream tail (or points at a
position that was trimmed/lost). On restore you must pick one:

- reset consumers to `LATEST`/`TRIM_HORIZON` (they will skip or replay,
  respectively), or
- accept replay from the restored checkpoint, understanding that records
  committed after the restore point are gone and may be re-created by replayed
  writes with *different* `pos` values and possibly different `table_id`
  (serial) / stream label than the consumer remembers.

There is no API to move a consumer's checkpoint; `SaveCheckpoint` is called by
the consuming application. After a restore, coordinate with the consumers and
either let them reset or delete/adjust rows in `stream_checkpoints` manually.

**Restore drill checklist**

1. Note the time of the incident and the PITR target.
2. Create the restore target as a new instance/server; do not overwrite the
   live one.
3. Confirm the restored `schema_migrations` contains `0001`–`0006`.
4. Compare `max(pos)` in `stream_records` and `stream_pos.last` with the live
   database to confirm how far back you landed.
5. Decide the consumer policy from the table above; reset or adjust
   `stream_checkpoints`.
6. Repoint a *staging* `DYNAMOAT_PG` at the restore first, run `readyz` and a
   smoke `ListTables`, then repoint production.
7. Record the drill in the incident; keep the live instance until consumers
   have caught up.

## 5. Upgrades

**Rolling deploy.** The chart runs `replicaCount: 2` and Kubernetes rolls the
Deployment one pod at a time. Each new pod runs `openStore`, which migrates
before it begins serving (`main.go` calls `openStore` before
`ListenAndServe`).

**Migrations run at startup under an advisory lock.** `migrate` (`postgres.go`)
applies `migrations/*.sql` in name order. Each file runs in a transaction that
holds `pg_advisory_xact_lock(7343)` (key `7343`) and is recorded in
`schema_migrations`; every statement is idempotent (`IF NOT EXISTS`,
`ADD COLUMN IF NOT EXISTS`). Two pods starting at once therefore apply each
file exactly once, and later pods see it already applied and do nothing.

**Which migrations rewrite tables.** Three migrations rewrite whole tables and
therefore take `ACCESS EXCLUSIVE` for the duration:

| Migration | Change | Effect |
|-----------|--------|--------|
| `0004` | `items.item` `jsonb` → `bytea` | rewrites every row of `items` |
| `0005` | `stream_records.keys/old_image/new_image` `jsonb` → `bytea` (plus two fast `ADD COLUMN`s) | rewrites every row of `stream_records` |
| `0006` | `stream_records.xid` `ADD COLUMN ... DEFAULT pg_current_xact_id()` (volatile default ⇒ rewrite), then `UPDATE stream_records SET pos = seq` | rewrites `stream_records` twice |

**Downtime on a large database.** The `ALTER TABLE ... TYPE ... USING` in
`0004`/`0005` and the volatile-default `ADD COLUMN` in `0006` hold
`ACCESS EXCLUSIVE` on `items`/`stream_records` while Postgres rewrites the
table. During that window the *already-running* pods' queries against that
table queue behind the lock (writes and reads block, not just the migrating
pod), so this is effectively downtime for that table proportional to its size.
Plan a maintenance window for the first deploy that crosses those migrations on
a large database; size it by a dry run (`SET lock_timeout` / a copy of the
table). The migration itself is transactional and rolls back cleanly on error.

**Postgres major upgrades.** dynamoat accepts 15–18, so a managed in-place
major-version upgrade is transparent to the server. Do it out of hours or scale
replicas to 0 first (the server checks the version only at `Open`), then
upgrade, then bring the pods back. Test on a clone.

## 6. Monitoring and alerting

`/metrics` exposes these metric families (`server/internal/metrics`):

- `dynamoat_requests_total{op="...",code="..."}` — counter of completed POST
  requests. `op` is the registered operation name (`GetItem`, `PutItem`,
  `Streams.GetRecords`, …) or `unknown` for unregistered targets; `code` is the
  DynamoDB short name on error (`ValidationException`,
  `ProvisionedThroughputExceededException`, `ResourceNotFoundException`,
  `ConditionalCheckFailedException`, `InternalServerError`, …) or `OK`.
- `dynamoat_request_duration_seconds{op="..."}` — histogram with buckets
  `0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10` (plus
  `+Inf`), `_sum` and `_count`.
- `dynamoat_inflight_requests` — gauge of requests currently being processed.
- `dynamoat_job_runs_total{job="...",result="ok|error"}` — counter of
  completed background job runs. `job` is `ttl`, `stream-trim` (the leased
  jobs), `backfill` or `table-delete` (the Postgres workers). A run is one
  batch of work.
- `dynamoat_job_items_total{job="..."}` — counter of the items (or rows)
  each background job processed.
- `dynamoat_stream_delivered_total{target="..."}` — counter of stream records
  delivered to each consumer target.
- `dynamoat_stream_delivery_failures_total{target="..."}` — counter of failed
  delivery attempts (transport error, non-2xx, or a Lambda function error) per
  target.
- `dynamoat_stream_lag_seconds{target="...",table="..."}` — gauge of delivery
  lag: seconds since the oldest undelivered stream record was created, or `0`
  when caught up. Labelled only for tables a target currently matches.
- `dynamoat_pg_pool_connections{state="idle|in_use|total"}` — gauge of the
  Postgres connection pool, read from pgxpool at scrape time.
- `dynamoat_pg_pool_max_connections` — gauge of the pool's `MaxConns`.

Suggested alerts (PromQL):

```promql
# Error rate by code — page on 5xx-class, warn on 4xx-class growth.
sum(rate(dynamoat_requests_total{code=~"InternalServerError"}[5m])) > 0.01
sum(rate(dynamoat_requests_total{code!~"OK|InternalServerError"}[5m])) > 1

# p99 latency per op.
histogram_quantile(0.99, sum by (op, le) (rate(dynamoat_request_duration_seconds_bucket[5m]))) > 1

# ProvisionedThroughputExceeded rate == item-lock timeouts (and deadlocks).
sum(rate(dynamoat_requests_total{code="ProvisionedThroughputExceededException"}[5m])) > 0.1

# Inflight headroom.
dynamoat_inflight_requests > 100

# Background jobs failing.
sum(rate(dynamoat_job_runs_total{result="error"}[5m])) by (job) > 0

# Stream delivery is falling behind: lag is the time the oldest undelivered
# record has been waiting. Alert when a consumer lags more than a few minutes,
# and separately on delivery failures (retries are already built in).
max by (target, table) (dynamoat_stream_lag_seconds) > 300
sum(rate(dynamoat_stream_delivery_failures_total[5m])) by (target) > 0.01

# Postgres pool at capacity: in-use connections near MaxConns.
dynamoat_pg_pool_connections{state="in_use"} / dynamoat_pg_pool_max_connections > 0.9
```

Postgres-side checks (SQL, or the equivalent managed metric):

```sql
-- Connections vs. headroom.
SELECT count(*) FROM pg_stat_activity;
SHOW max_connections;

-- Replication lag / replica health (managed PITR keeps the equivalent).
SELECT application_name, pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS lag_bytes
FROM pg_stat_replication;

-- Bloat / vacuum debt.
SELECT relname, n_live_tup, n_dead_tup,
       round(100.0 * n_dead_tup / nullif(n_live_tup + n_dead_tup, 0), 1) AS dead_pct,
       last_autovacuum
FROM pg_stat_user_tables
WHERE relname IN ('items', 'index_entries', 'stream_records')
ORDER BY n_dead_tup DESC;

-- Oldest running transaction: the #1 stream-stall suspect.
SELECT max(now() - xact_start) AS oldest_xact
FROM pg_stat_activity
WHERE xact_start IS NOT NULL AND state <> 'idle';

-- Stream backlog: unsequenced (held back by an open transaction) ...
SELECT count(*) AS unsequenced FROM stream_records WHERE pos IS NULL;

-- ... and undelivered (assigned a position but ahead of a consumer checkpoint).
SELECT r.table_id, c.consumer, max(r.pos) AS stream_head, c.seq AS checkpoint,
       max(r.pos) - c.seq AS undelivered
FROM stream_records r
JOIN stream_checkpoints c ON c.table_id = r.table_id
WHERE r.pos IS NOT NULL
GROUP BY r.table_id, c.consumer, c.seq;
```

`pos IS NULL` growth is the direct symptom of a long-running transaction
(§3). `undelivered` growing means a consumer is not keeping up (its problem) or
is not polling; steady-state it should stay near the write rate.

## 7. Incidents

### ProvisionedThroughputExceeded / throttling

1. Confirm with
   `rate(dynamoat_requests_total{code="ProvisionedThroughputExceededException"}[5m])`.
2. This error means writes waited past `DYNAMOAT_LOCK_TIMEOUT` (default 5 s) for
   an item's advisory lock, or deadlocked — a *hot key*, not table capacity
   (`ReadCU`/`WriteCU` are declared but not enforced).
3. Find the contended items with `pg_locks`/`pg_stat_activity` or by the
   affected key from the client. If it is one key, it is a client design issue.
4. If it is broad, check the oldest transaction (a long transaction holds item
   locks and stream sequencing), check for a stuck backfill/delete (§ below)
   hammering `items`, and check CPU/IO on Postgres.
5. Remediate: kill the long/abandoned transaction, raise the lock timeout, or
   scale the Postgres tier. SDK clients retry with backoff.

### GSI stuck `CREATING`

1. `SELECT def FROM ddb_tables WHERE name = '<table>';` and confirm the GSI's
   status in the JSON is `CREATING` long after creation.
2. The backfill is a per-pod goroutine; look for
   `backfill: reading items` / `backfill: writing index entries` errors in the
   pod logs.
3. If a pod was restarted mid-backfill, every pod resumes it at startup
   (`resumeBackfills`); a `CREATING` GSI that never became `ACTIVE` resumes on
   the next rolling restart automatically.
4. The backfill cursor is `(pk_hash, pk, sk)`; it restarts from the beginning
   on every pod restart (there is no persisted cursor), so a large table
   repeatedly rolled will keep re-reading from the start. Ensure a pod stays up
   long enough to finish.
5. If a specific item blocks it, the backfill takes the item's advisory lock;
   a long-held lock on that item stalls the batch. Clear the blocker.

### Table stuck `DELETING`

1. `DELETE` marks the row `DELETING` and frees rows in the background
   (`deleteTableStep` removes `items`, `index_entries`, `stream_records` by
   `ctid` in batches of 1000). The name is hidden from `ListTables` while
   `DELETING`, and `CreateTable` refuses the name until it is fully gone.
2. Check pod logs for `delete table: removing rows`.
3. Like backfill, deletes resume at startup (`resumeDeletes`) and restart from
   the beginning of the scan each pod restart (no cursor), so keep a pod up.
4. Watch `stream_records` for the table — a table with a large stream (24 h of
   records) is the slow part of the delete.

### Stream delivery stalled

1. `GetRecords` returns empty but writes are happening. First suspect per §3:
   `SELECT max(now() - xact_start) FROM pg_stat_activity WHERE xact_start IS NOT NULL;`
   and `SELECT count(*) FROM stream_records WHERE pos IS NULL;`. A single
   long-running or idle-in-transaction session holds every record back.
2. Kill the offending transaction (or fix the application that left it open);
   `Store.sequence` will then renumber on the next read.
3. Confirm the consumer's checkpoint is advancing
   (`stream_checkpoints`). `undelivered` growing while `pos IS NULL` is 0 means
   the consumer, not dynamoat, is behind.

### Disk filling / `stream_records` retention

1. `stream_records` is trimmed by the `stream-trim` job: records older than
   `DYNAMOAT_STREAM_RETENTION` (default 24 h) are deleted in batches of 10000
   every minute (`trimBatch`).
2. Check the job is actually running on one pod: look for
   `background job: ran` with `job=stream-trim`, and
   `SELECT * FROM leases WHERE name = 'stream-trim';` (a stale `expires_at` in
   the future with a dead owner blocks everyone for up to 30 s, then another
   pod takes it).
3. If the table is still large, the trim has fallen behind or a pod with the
   lease is down; deleting by `ctid` means it resumes where it left off.
4. Find what is consuming disk:
   `SELECT relname, pg_total_relation_size(relid) FROM pg_stat_user_tables ORDER BY 2 DESC;`
   The usual answer is `stream_records` (24 h of change) or `items` bloat.
5. Shorten the trim horizon with `DYNAMOAT_STREAM_RETENTION` (default 24 h) —
   the immediate fix is to ensure the job runs and to delete old rows manually
   if the table has grown past what the batch loop can clear promptly.

### Restoring one table from a backup

A PITR restore restores the *whole database*, not one table. To get one table
back:

1. Restore to a scratch instance (§4).
2. Export the table's rows: its `ddb_tables` catalog row and its `items`,
   `index_entries`, `stream_records` for that `table_id`
   (`\copy ... TO ... WHERE table_id = <id>`), or `pg_dump --table=items
   --table=ddb_tables ...` with a `--where` filter where supported.
3. On the live database, if the table still exists, `DeleteTable` and wait for
   the background delete to finish (the name is reserved until then).
4. Re-import the catalog row and rows; note the serial `table_id` will likely
   differ, and the stream label/`stream_checkpoints` will not carry over — treat
   the restored table's stream as fresh and reset consumers.

## 8. Security

**SigV4 keys.** `DYNAMOAT_AUTH=sigv4` (the default) requires `DYNAMOAT_KEYS`:
either a file with one `<access key id> <secret>` per line (`#` comments
allowed), or a directory with one file per key (file name = access key ID,
trimmed file content = secret) — the shape a Kubernetes Secret mount has. The
secret is an AWS-style SigV4 secret (the `AWS4<secret>` HMAC key); the access
key ID is any string the client presents.

**Rotation.** Keys are hot-reloaded: `main.go` re-reads `DYNAMOAT_KEYS` on
`SIGHUP` and every 30 s when the key file's (or directory's) modification time
changes, and swaps the key set atomically. A failed reload keeps the old keys
and logs an error. To rotate:

1. Add the new key file (or line) alongside the old — within 30 s (or on
   `SIGHUP`) every pod picks it up; old clients keep working on the old key.
2. Reconfigure clients to the new key and verify.
3. Remove the old key file (or line); within 30 s the pods drop it.

**`DYNAMOAT_AUTH=none` only for local use.** It accepts every request and logs
a warning at startup; the Helm chart warns and it must never be used in
production. Use `sigv4`.

**Network policy.** The chart exposes the Service as `ClusterIP` only and
deliberately has no Ingress — dynamoat is an internal service. Keep it that
way; clients (in-cluster Lambdas) reach it over the cluster network.
Do not add an Ingress or a `LoadBalancer`.

**Postgres credentials.** Prefer platform identity over static passwords:

- **GKE + Cloud SQL:** run the Cloud SQL Auth Proxy sidecar with
  `--auto-iam-authn`, and use the pod's Workload Identity (the
  `iam.gke.io/gcp-service-account` annotation) to get a short-lived IAM token —
  no password in the connection string (`values-gke-cloudsql.yaml`).
- **AKS + Azure Flexible Server:** either a password connection string with
  `sslmode=require` (the default, `DYNAMOAT_PG_AUTH=password`; set Helm
  `postgres.auth: password` over the AKS profile), or, without a stored
  password, `DYNAMOAT_PG_AUTH=azure-workload-identity` (Helm `postgres.auth`,
  which the AKS profile sets). With Entra, each new connection's
  password is an Entra access token obtained through AKS Workload Identity
  (the `AZURE_*` variables the webhook injects), cached and refreshed 5 minutes
  before it expires; the connection string has no password and uses
  `sslmode=require`. Token requests time out after 30 s.
- Always deliver the connection string via a Kubernetes Secret
  (`postgres.existingSecret`), and use `sslmode=require` (or the proxy's local
  TLS-terminated hop) so credentials and data are encrypted in transit.

## Appendix: on-demand backups and stream delivery

An on-demand backup (CreateBackup) copies the table in one REPEATABLE READ
transaction, without a statement timeout, so the backup is the table as of the
call. While that transaction is open, stream delivery for every table waits
(a stream record becomes readable only once every older transaction has
finished, docs/design.md), and autovacuum cannot remove rows the snapshot can
still see. For a large table, take backups when a delay in stream delivery is
acceptable, and watch `dynamoat_stream_lag_seconds`.

