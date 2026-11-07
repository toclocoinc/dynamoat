# Smoke scripts

Readable, runnable checks of desktop DynamoDB client workflows, driven through
the AWS SDK v3 the way a client like Keystone drives an endpoint. They are not
the conformance suite: each script is a short sequence of named checks with
assertions on the results, printed one per line.

Every script is standalone. Each check prints one of:

- `✔ name (12 ms)` — the check passed.
- `✖ name: <error name>: <message>` — the check failed (plus the request that
  failed when the error came from the endpoint). A failed check makes the
  script exit 1.
- `○ name: unsupported (<Operation>)` — the endpoint does not implement the
  operation (UnknownOperationException, or a `dynamoat does not support ...` /
  `dynamoat:` ValidationException). Unsupported checks are not failures unless
  you pass `--strict`.

Tables are named `smoke-<random>-<name>` and are deleted at the end, also when
a script fails part way through.

## Running

Against a local dynamoat (builds and starts one on a free port, then stops it):

```sh
node smoke/01-connection.mjs --start
node smoke/run-all.mjs --start          # all ten scripts, one shared server
```

Against a remote dynamoat or DynamoDB Local (default endpoint is
`http://localhost:8000`):

```sh
ENDPOINT=http://127.0.0.1:8000 node smoke/run-all.mjs
```

Against real DynamoDB (normal credential chain, `AWS_REGION` defaults to
`us-east-1`):

```sh
AWS_REGION=us-east-1 node smoke/run-all.mjs --aws
```

`--strict` turns unsupported checks into failures, so an endpoint that is
missing an operation cannot pass silently.

Environment variables:

- `ENDPOINT` — the DynamoDB endpoint (default `http://localhost:8000`). The
  DynamoDB Streams client points at the same endpoint. Dummy credentials are
  used whenever an endpoint is set.
- `AWS_REGION` (or `AWS_DEFAULT_REGION`) — the region (default `us-east-1`).
- `DYNAMOAT_PG` — when set, `--start` runs dynamoat with the Postgres store
  pointed at that connection string instead of the in-memory store.

Requires Node 20+. Dependencies are pinned to the same versions as the
conformance suite and installed with `npm ci`.
