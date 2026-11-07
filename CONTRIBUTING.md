# Contributing to dynamoat

dynamoat is a Go server that speaks the DynamoDB API and stores data in
PostgreSQL. This file explains how to build and test it, the coding rules, and
how to propose a change.

## Building and testing

Always use the pinned Go toolchain — never a system `go`:

```
scripts/go.sh -C server build ./...
scripts/go.sh -C server test ./internal/attr/ -run TestName -v
```

The full gate is:

```
scripts/test.sh
```

It runs `gofmt`, `go vet`, and `go test` (with `-race` when a C compiler is
available). Database tests use `pgtest.DB(t)`, which returns a fresh Postgres
database: on the server named by `DYNAMOAT_TEST_PG` when it is set, otherwise on
a private Postgres it starts itself (the binaries are cached). A package using
it needs `func TestMain(m *testing.M) { pgtest.Main(m) }`. Never skip a database
test because no server is configured. Operation tests use `memstore.New()`, so
they stay fast.

## Coding rules

- **Standard library first.** The approved third-party modules are
  `github.com/cockroachdb/apd/v3` (decimals), `github.com/jackc/pgx/v5`
  (Postgres) and, in tests only, `github.com/fergusstrange/embedded-postgres`
  (through pgtest). Anything else must be justified in the issue.
- **Match DynamoDB exactly** where a client can see it: field names, error
  `__type`, error messages, limits, ordering. When unsure what DynamoDB does,
  say so in the issue or PR rather than guessing.
- **Return client errors as `*ddberr.Error`** (usually
  `ddberr.Validation(...)`). Never panic on client input.
- **Table-driven tests** in the package's `_test.go` files, named for the
  behaviour they check.
- **Keep `gofmt` clean.** Comments explain why, not what.
- **Do not change `internal/storage/storage.go`** (the contract between the
  operations and a storage backend) unless the change requires it; if it is in
  your way, say so.

## Proposing a change

- For anything large, open an issue first to describe the problem and the
  approach before writing code.
- Open pull requests against `main`.
- Keep commits plain: clear messages, no attribution lines, no model names.
- Do not mention internal machine names or customer names in code, commits,
  documentation or issues.

## Code of conduct

Be kind and constructive. Assume good faith, and keep discussion about the code
and the problem it solves.
