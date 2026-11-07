# dynamoat-bench

A latency benchmark that drives dynamoat and real DynamoDB with the same
workload, over the AWS SDK v3 as client applications use it. It creates a
tenant table, seeds it with BACKLOG-like items, then runs
closed-loop scenarios and reports throughput and latency percentiles from an
HDR-style histogram (1 % precision, written in the tool, no dependency).

## Requirements

- Node 20+
- The pinned Go toolchain for building dynamoat (`../scripts/go.sh`), only for
  the smoke test

Install once:

```
npm ci --no-audit --no-fund
```

## Run against dynamoat

Start dynamoat somewhere, e.g.:

```
DYNAMOAT_AUTH=none DYNAMOAT_STORE=memory ../scripts/go.sh -C ../server run ./cmd/dynamoat
```

Then, from this directory:

```
npx tsx src/cli.ts \
  --endpoint http://127.0.0.1:8000 \
  --region us-east-1 \
  --scenario all \
  --concurrency 32 \
  --duration 60s \
  --warmup 10s \
  --seed-items 10000 \
  --out result.json
```

(The `--endpoint` points the SDK at dynamoat; `--keep` leaves the table behind,
otherwise it is deleted at the end.)

## Run against DynamoDB

Omit `--endpoint`; the SDK uses its normal credential chain:

```
npx tsx src/cli.ts --region us-east-1 --scenario all --concurrency 32 --duration 60s --warmup 10s --out result.json
```

### CLI

```
npx tsx src/cli.ts --scenario <name|all> [options]

  --endpoint <url>     dynamoat or DynamoDB Local endpoint; omit for real DynamoDB
  --region <r>         AWS region (default us-east-1)
  --scenario <name|all>  get | query-gsi | update-7gsi | put | all
  --concurrency <n>    closed-loop workers (default 32)
  --duration <t>       measurement window, e.g. 60s (default 60s)
  --warmup <t>         warmup before measurement (default 10s)
  --seed-items <n>     items to seed (default 10000)
  --keep               do not delete the table at the end
  --out <path>         write JSON results to this file
```

The `bin` is also installed as `dynamoat-bench`, so after `npm link` the same
invocation works as `npx dynamoat-bench ...`.

## Setup

The run creates `bench-<random>`, a table shaped like a tenant table:

- hash key `id` (S), `PAY_PER_REQUEST`
- GSIs: `entity-comboKey-index` (entity, comboKey, ALL),
  `entity-comboKey-name-index` (entity, comboKey, INCLUDE name),
  `comboKey-index`, `subscription-entity-index`, and three more on `parent`,
  `ownerId` and `status` (7 in all)

It seeds `--seed-items` items with `BatchWriteItem`, 100 items per board, each
1-2 KB, with `comboKey` = `SUB:<s>|KBN:<board>|ID:<id>` and `entity` = `BACKLOG`.
If the server answers `UnknownOperationException` for `BatchWriteItem` (dynamoat
does not implement it yet) the seed falls back to one `PutItem` per item.

## Scenarios

Each runs closed-loop: `--concurrency` workers issue one request at a time for
`--duration` after `--warmup`. Latency is measured over every completed request
and reported as p50/p90/p99/p99.9/max in milliseconds.

- `get` — `GetItem` of a random seeded id.
- `query-gsi` — `Query` on `entity-comboKey-index` with
  `entity = BACKLOG AND begins_with(comboKey, SUB:<s>|KBN:<board>|)`, `Limit 100`
  (one board).
- `update-7gsi` — `UpdateItem` `SET #name = :n, updated = :t ADD version :one`
  on a random id, also setting `status` (a GSI key) in half the calls.
- `put` — `PutItem` of a fresh item with `attribute_not_exists(id)`.

## Report

Per scenario: requests, errors by name, throughput (requests/s), and latency
p50/p90/p99/p99.9/max, printed as a table and written to `--out` as JSON with
the run's parameters, Node version and host CPU count.

## Tests

```
npm test          # unit tests: histogram percentiles and the item generator
npm run smoke     # builds dynamoat, runs every scenario for 3 s at concurrency 4
```

Scenarios whose operation the server does not implement yet (it answers
`UnknownOperationException`) are skipped with a message in the smoke run.
