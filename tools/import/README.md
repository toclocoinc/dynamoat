# dynamoat-import

`dynamoat-import` copies DynamoDB tables into a [dynamoat](../../) server. It
reads the source with a parallel, consistent `Scan` and writes the target with
`BatchWriteItem`, recreating the table's key schema, attribute definitions,
GSIs, stream specification and TTL first. The copy is resumable and, with
`--verify`, checked item-for-item.

It is a Node.js 20+ tool written in TypeScript, run with
[tsx](https://tsx.is). It uses only the AWS SDK v3
(`@aws-sdk/client-dynamodb`).

## Usage

```sh
npx dynamoat-import \
  --source-endpoint http://localhost:8000 \
  --source-region us-east-1 \
  --target-endpoint http://localhost:8001 \
  --target-region us-east-1 \
  --tables tenants,events \
  --segments 8 \
  --state import-state.json \
  --verify
```

While developing in this repository, run the TypeScript directly instead:

```sh
npx tsx src/cli.ts --source-endpoint http://localhost:8000 --source-region us-east-1 \
  --target-endpoint http://localhost:8001 --target-region us-east-1 --tables tenants
```

Options:

| option | meaning |
|--------|---------|
| `--source-endpoint <url>` | source DynamoDB endpoint (omit for the real AWS service) |
| `--source-region <r>` | source region (required) |
| `--target-endpoint <url>` | target dynamoat endpoint (required) |
| `--target-region <r>` | target region (required) |
| `--tables a,b` | the tables to copy, or |
| `--prefix p` | every source table whose name starts with `p` |
| `--segments n` | parallel `Scan` segments (default 8) |
| `--state file` | resume state file (default `import-state.json`) |
| `--verify` | after copying, scan both sides and compare counts and checksums |
| `--dry-run` | print the plan without creating tables or copying items |

Progress goes to stderr; a final JSON summary goes to stdout. The exit status
is 0 when every table copied (and, with `--verify`, matched) and 1 otherwise.

## Credentials

- The **source** uses the normal AWS SDK credential chain
  (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, profiles, instance roles,
  etc.).
- The **target** uses `DYNAMOAT_ACCESS_KEY_ID` and
  `DYNAMOAT_SECRET_ACCESS_KEY`, and never falls back to the ambient AWS chain.

The SDK always signs requests, so even a local dynamoat running with
`DYNAMOAT_AUTH=none` needs *some* credentials. Point a local source at dummy
values (`AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test`) and leave the
`DYNAMOAT_*` variables unset or empty for an unauthenticated target.

## What it copies

For each table it:

1. `DescribeTable` on the source and `CreateTable` on the target with the same
   key schema, `AttributeDefinitions`, GSIs (key schema and projection) and
   `StreamSpecification`, always with `PAY_PER_REQUEST` billing, then waits
   for the table to become `ACTIVE`.
2. `DescribeTimeToLive` on the source and, if enabled, `UpdateTimeToLive` on
   the target. A target that does not implement `UpdateTimeToLive` is a
   warning, not an error.
3. Parallel `Scan` with `TotalSegments` equal to `--segments`,
   `ConsistentRead`, and `Limit 1000`, writing pages with `BatchWriteItem` in
   chunks of 25. `UnprocessedItems` are retried with exponential backoff and
   jitter (100 ms doubling to 10 s, 10 attempts, then fail).

Local secondary indexes are not copied (dynamoat does not support them).

## Resume

After every page the tool records each segment's `LastEvaluatedKey` in the
state file, written atomically (temp file then rename). Rerun with the same
`--state` file and the copy continues where it stopped. A table whose segments
are all complete is skipped.

If the target table already exists with the same key schema the copy resumes
into it; a target table with a different key schema is an error.

## Verify

`--verify` scans both sides and compares the item count and an
order-independent checksum: the sum mod 2^64 of the first 8 bytes of SHA-256 of
each item's canonical JSON (keys sorted at every level, numbers as their
DynamoDB strings). Both counts and checksums are printed, and the tool exits 1
on a mismatch.

## Tests

```sh
npm test
```

runs the unit tests (`node --test` with in-memory fakes) and an integration
test that builds the server, starts two local dynamoat instances, fills the
source with 3,000 items, and imports them with `--verify`. The integration test
skips itself, naming the missing operations, until the server implements `Scan`
and `BatchWriteItem`.
