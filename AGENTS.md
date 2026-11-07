# Working in this repository

dynamoat is a server that speaks the DynamoDB API (JSON over HTTP, AWS SDK v3
compatible) and stores data in PostgreSQL 15-18. Read `docs/design.md` before
changing storage or key handling.

## Layout

```
server/                     Go module github.com/toclocoinc/dynamoat/server
  cmd/dynamoat/             main: configuration, wiring, shutdown
  internal/api/             wire protocol: routing by X-Amz-Target, CRC32, error envelope
  internal/ddberr/          DynamoDB error types and exact __type names
  internal/attr/            attribute values, number handling, key encoding, item size
  internal/expr/            expression parser (AST) and evaluator
  internal/sigv4/           SigV4 request authentication
  internal/storage/         the Store interface the operations use (storage.go is the contract)
  internal/storage/storagetest/  contract tests every Store must pass
  internal/storage/memstore/     in-memory Store, for operation tests
  internal/storage/postgres/     the Postgres Store and migrations/
  internal/storage/postgres/pgtest/  a real, empty Postgres database for a test
  internal/ops/             one file per DynamoDB operation, registered in init() (see ops.go)
conformance/                TypeScript suite run against real DynamoDB and dynamoat
deploy/                     Dockerfile, compose, Helm chart
scripts/go.sh               the pinned Go toolchain (downloads itself; never use a system go)
scripts/test.sh             gofmt, go vet, go test (with -race when a C compiler exists)
```

## Commands

```
scripts/go.sh -C server build ./...
scripts/test.sh                       # what the gate runs (npm test calls it)
scripts/go.sh -C server test ./internal/attr/ -run TestName -v
```

Database tests use `pgtest.DB(t)`, which returns a fresh database: on the
server named by `DYNAMOAT_TEST_PG` when it is set, otherwise on a private
Postgres it starts itself (no Docker, no setup; the binaries are cached in
~/.cache/dynamoat). A package using it needs
`func TestMain(m *testing.M) { pgtest.Main(m) }`. Never skip a database test
because no server is configured.

Operation tests (internal/ops) use `memstore.New()`, so they stay fast.

## Rules

The coding rules and how to propose a change are in `CONTRIBUTING.md`. In short:
standard library first (the approved modules are listed there), match DynamoDB
exactly where a client can see it, return client errors as `*ddberr.Error`,
write table-driven tests, keep `gofmt` clean, and use plain commit messages with
no attribution lines or model names. Do not edit `scripts/` or `.github/`.
