# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-11-07

First public release. dynamoat is pre-1.0: expect changes, and check
[docs/compatibility.md](docs/compatibility.md) before relying on a feature.
Responses are tested against output recorded from Amazon DynamoDB
(`conformance/golden`) on PostgreSQL 15-18.

### Added

- DynamoDB API server speaking JSON over HTTP, compatible with the AWS SDKs,
  with SigV4 request authentication (`sigv4`) and a `none` mode for local
  development.
- Control plane: `CreateTable`, `DeleteTable`, `DescribeTable`, `ListTables` and
  `UpdateTable`, including local and global secondary indexes, tags, and the
  global tables API (replicas are bookkeeping).
- Data plane: `GetItem`, `PutItem`, `UpdateItem`, `DeleteItem`, `Query`, `Scan`,
  `BatchGetItem` and `BatchWriteItem` with condition, key-condition, filter,
  projection and update expressions.
- Transactions: `TransactGetItems` and `TransactWriteItems`, with
  `ClientRequestToken` idempotency.
- PartiQL: `ExecuteStatement`, `BatchExecuteStatement` and `ExecuteTransaction`
  for a subset of PartiQL statements.
- DynamoDB Streams API (`ListStreams`, `DescribeStream`, `GetShardIterator`,
  `GetRecords`) and an HTTP stream dispatcher that delivers batches to
  configured targets.
- Time-to-live expiry for tables with TTL enabled.
- On-demand backups and point-in-time recovery, stored by the Postgres backend.
- Prometheus metrics on `/metrics` covering requests, background jobs and stream
  delivery.
- Postgres storage backend for PostgreSQL 15–18, and an in-memory store for
  development and tests.
