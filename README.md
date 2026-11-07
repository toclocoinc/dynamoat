# dynamoat

dynamoat is a self-hosted server that speaks the DynamoDB API — JSON over HTTP,
compatible with the AWS SDKs — and stores its data in PostgreSQL 15–18. Point an
existing AWS SDK or the AWS CLI at it and most DynamoDB code runs unchanged,
backed by your own database (see [what differs](docs/compatibility.md)).

## Quick start

With Docker Compose (dynamoat plus a persistent Postgres 17 database):

```
docker compose -f deploy/docker-compose.yml up -d
curl http://localhost:8000/readyz
```

Or run the binary directly (this starts with the in-memory store, which is
convenient for trying the API and loses everything on shutdown):

```
DYNAMOAT_AUTH=none scripts/go.sh -C server run ./cmd/dynamoat
```

Point a client at it:

```
export AWS_ENDPOINT_URL_DYNAMODB=http://localhost:8000
aws --endpoint-url http://localhost:8000 --region us-east-1 dynamodb list-tables
```

The compose file and the memory-store run accept every request
(`DYNAMOAT_AUTH=none`). To require AWS Signature Version 4 instead, set
`DYNAMOAT_AUTH=sigv4` and `DYNAMOAT_KEYS` to a key file or directory; see
[docs/running.md](docs/running.md).

## What is supported

The DynamoDB data plane, control plane, transactions, PartiQL, backups and
point-in-time recovery, TTL, and the DynamoDB Streams API are implemented; a
number of DynamoDB features are not. The full operation-by-operation table is in
[docs/compatibility.md](docs/compatibility.md).

## Status

dynamoat is pre-1.0. The API surface is broad and tested against DynamoDB, but
the project has not reached a stable 1.0 release: expect changes, and check
[docs/compatibility.md](docs/compatibility.md) before relying on a feature in
production.

## Building and testing

Use the pinned Go toolchain (never a system `go`):

```
scripts/go.sh -C server build ./...
scripts/test.sh                       # gofmt, go vet, go test — the full gate
```

## Documentation

- [docs/compatibility.md](docs/compatibility.md): every DynamoDB and DynamoDB
  Streams operation, and where dynamoat differs from DynamoDB
- [docs/running.md](docs/running.md): running the server, configuration, auth
  modes, and the HTTP endpoints
- [docs/design.md](docs/design.md): architecture, storage model and key encoding
- [docs/runbook.md](docs/runbook.md): operating dynamoat in production
- [docs/streams.md](docs/streams.md): DynamoDB Streams and the stream dispatcher
- [CONTRIBUTING.md](CONTRIBUTING.md): how to build, test and propose changes
- [SECURITY.md](SECURITY.md): how to report a vulnerability

## Licence

dynamoat is licensed under the Apache License 2.0; see [LICENSE](LICENSE) and
[NOTICE](NOTICE).

dynamoat is an independent project. It is not affiliated with, endorsed by or
sponsored by Amazon Web Services. Amazon DynamoDB is a trademark of Amazon.com,
Inc. or its affiliates.
