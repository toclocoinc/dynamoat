# Conformance suite

TypeScript tests written against the AWS SDK v3 (`@aws-sdk/client-dynamodb`
and `@aws-sdk/lib-dynamodb`, the clients applications use). They run against
dynamoat and can be recorded against real DynamoDB, so dynamoat's responses
and errors are diffed against the real service's.

Requires Node 22.18+ (the tests run with `node --test` using Node's built-in
TypeScript type stripping, so there is no TypeScript toolchain or test runner
dependency). Dependencies are pinned to exact versions in `package.json` and
`package-lock.json`.

## Targets

`CONFORMANCE_TARGET` chooses the target:

- `dynamoat` (the default): builds the server with
  `../scripts/go.sh -C ../server build -o <tmp>/dynamoat ./cmd/dynamoat`,
  starts it with `DYNAMOAT_AUTH=none`, `DYNAMOAT_STORE=memory` and
  `DYNAMOAT_LISTEN` on a free port, waits for `/readyz`, points the clients at
  it (region `us-east-1`, dummy credentials) and stops it at the end.

  The store is chosen with `CONFORMANCE_STORE`:

  - `memory` (the default): starts the server with `DYNAMOAT_STORE=memory`.
  - `postgres`: starts the server with `DYNAMOAT_STORE=postgres` and
    `DYNAMOAT_PG` set to `CONFORMANCE_PG` (a Postgres connection string), so
    the suite runs against the Postgres store. The database must be empty.
- `aws`: real DynamoDB using the caller's credentials and `AWS_REGION`. Every
  table name is prefixed with `conformance-<random>-`, and every created table
  is deleted at the end, including on failure.

Run a target:

```sh
CONFORMANCE_TARGET=dynamoat npm test
AWS_REGION=us-east-1 CONFORMANCE_TARGET=aws npm test
CONFORMANCE_STORE=postgres CONFORMANCE_PG=postgres://postgres:pgtest@127.0.0.1:5432/dynamoat_test npm test
```

## Recording golden files

`expectSame(name, fn)` runs each case as a sequence of SDK calls and captures
each call's result or error, normalising the volatile fields (request IDs,
timestamps and `CreationDateTime`, ARN account and region, `TableId`,
`LatestStreamLabel`, the opaque PartiQL `NextToken`, and the random table
prefix — the prefix is also stripped from object keys such as the table names
that key a `BatchGetItem` `Responses` map). Order DynamoDB does not define is
also normalised: `GlobalSecondaryIndexes` and `LocalSecondaryIndexes` are
sorted by `IndexName`, `Replicas` by `RegionName`, and each `Responses[table]`
list by a stable JSON form of its items. Cases that page a `Scan` or a
full-table PartiQL `SELECT` capture one synthetic result (sorted items and the
count) instead of the per-page items and `LastEvaluatedKey`/`NextToken`.

With `CONFORMANCE_RECORD=1` (aws target only) it writes the capture to
`golden/<name>.json`; otherwise it compares against that golden file and fails
with a readable diff, or prints `no golden yet: <name>` when no golden exists.

Record the suite against real DynamoDB (requires credentials):

```sh
AWS_REGION=us-east-1 CONFORMANCE_TARGET=aws CONFORMANCE_RECORD=1 npm test
```

Golden files are committed, so a later `dynamoat` run diffs against them.

## Flags

- `CONFORMANCE_SKIP_SLOW=1` skips the cases marked `slow` (`tags`, `streams`,
  `backups`, `pitr`). Real DynamoDB is asynchronous in places dynamoat is
  immediate, so these poll with plain (uncaptured) sends before capturing the
  final call — tags and streams with a 2-minute cap, backups until the backup
  is `AVAILABLE` (and the restored table is `ACTIVE`), and PITR until the
  recovery window is open (2 minutes) and the restored table is `ACTIVE` (up
  to 30 minutes). The suite is much faster with these skipped, which is handy
  while iterating on the harness.
- The `global-tables-replica` case is `dynamoatOnly`: dynamoat deliberately
  differs from real DynamoDB (replicas are bookkeeping and `ACTIVE` at once;
  on AWS the replica is `CREATING` for minutes and deleting it then fails).
  It runs against dynamoat with its assertions and is skipped (with a message)
  when recording or comparing on the `aws` target.

## Notes

- Operations not merged into dynamoat yet fail with
  `UnknownOperationException`; the cases that need them carry a `requires`
  list and are skipped (with a message) when the server is missing them.
- Record against a dedicated account/region that has no other tables. In
  particular, `ListTables` reflects the whole account's table list on the
  `aws` target (the table prefix is normalised, but unrelated tables are not
  filtered out), so unrelated tables would make that golden file unusable.
- `DescribeTable` after `CreateTable` is timing-dependent on real DynamoDB;
  the harness waits for the table and its GSIs to reach `ACTIVE` before data
  operations, but the recorded `CreateTable` response itself is `CREATING`.
