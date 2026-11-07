# DynamoDB Streams

dynamoat implements the DynamoDB Streams API (`ListStreams`, `DescribeStream`,
`GetShardIterator`, `GetRecords`) so the AWS SDK's stream client works
unchanged, and a stream dispatcher that pushes each table's stream records to
an HTTP consumer in the Lambda event shape.

## Stream dispatcher

The dispatcher is dynamoat's replacement for a Lambda event source mapping.
An application's stream Lambdas (for example `opensearchStream`) run unchanged in a
container whose Lambda Runtime Interface Emulator takes the event on
`POST /2015-03-31/functions/function/invocations`; any HTTP endpoint that
takes the same body works.

Every 5 seconds the dispatcher lists the tables with a stream that match each
configured target. For each (target, table) it holds a lease
(`dispatch:<target name>:<table id>`, owner `hostname:pid`, renewed every 10
seconds for a 30-second lifetime) so exactly one server delivers that table.
It then reads from the target's checkpoint and `POST`s batches of the shape
`{"Records": [...]}`; each record is a DynamoDB stream record with an
`eventSourceARN` field naming the stream ARN. On a successful delivery it saves
the checkpoint to the last sequence number.

A delivery succeeds only on a 2xx response whose body is not a Lambda function
error (a JSON object with an `errorType` or `errorMessage` field, which is how
the emulator reports a thrown error). Any other response, a network error, or
a function error is a failure: it is logged and the same batch is retried after
1 second, doubling to at most 1 minute. A failing batch blocks later records of
that table only, as Lambda does; other tables continue. With no records the
dispatcher polls again after 250 ms. A table whose stream is disabled, or which
is deleted, stops its loop (its lease then expires; there is no explicit
release).

`ReportBatchItemFailures` is not supported yet.

### Configuration

Set `DYNAMOAT_STREAM_TARGETS` to a JSON array of targets, or `@<path>` to read
that array from a file. Field names are lowerCamelCase; durations are Go
duration strings:

```json
[
  {
    "name": "opensearch",
    "tables": "*",
    "url": "http://opensearch-stream:8080/2015-03-31/functions/function/invocations",
    "batchSize": 100,
    "batchWindow": "1s",
    "headers": { "X-Forwarded-For": "dynamoat" }
  }
]
```

| Field         | Meaning |
|---------------|---------|
| `name`        | The consumer name, used for checkpoints and lease names. |
| `tables`      | A `path.Match` glob over table names, e.g. `"*"` or `"tenant-*"`. |
| `url`         | The consumer endpoint (http or https). |
| `batchSize`   | Records per batch; default 100, maximum 1000. |
| `batchWindow` | Wait up to this long for a short batch to fill; default `0` (send immediately). |
| `timeout` | Longest wait for one invocation (a Go duration, at most `15m`, a Lambda's limit); default `30s`. A timeout is a failed delivery and is retried. |
| `headers`     | Extra HTTP headers to send with each batch. |

`name`, `tables` and `url` are required. An invalid value stops the server at
startup with a clear message.

### Docker Compose example

Run a Lambda container image as the consumer, next to dynamoat and Postgres:

```yaml
services:
  postgres:
    image: postgres:17
    environment:
      POSTGRES_USER: dynamoat
      POSTGRES_PASSWORD: dynamoat
      POSTGRES_DB: dynamoat
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U dynamoat"]
      interval: 2s
      retries: 30

  dynamoat:
    build: .
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DYNAMOAT_STORE: postgres
      DYNAMOAT_PG: postgres://dynamoat:dynamoat@postgres:5432/dynamoat?sslmode=disable
      DYNAMOAT_AUTH: none
      DYNAMOAT_STREAM_TARGETS: >
        [{"name":"opensearch","tables":"*","url":"http://consumer:8080/2015-03-31/functions/function/invocations"}]
    ports:
      - "8000:8000"

  consumer:
    image: <your-lambda-image>
    # The Lambda Runtime Interface Emulator listens on 8080 by default.
    ports:
      - "8080:8080"
```
