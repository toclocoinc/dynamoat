# DynamoDB compatibility

dynamoat implements the DynamoDB API (JSON over HTTP, AWS SDK v3 compatible) and
the DynamoDB Streams API. This page lists every operation in the AWS API
reference and where dynamoat differs from DynamoDB.

- **Supported** — implemented, matching DynamoDB's request and response shapes
  and error types where a client can observe them.
- **Partial** — implemented with limitations; see the note.
- **Not supported** — not implemented; the server answers
  `UnknownOperationException`.

Totals: **DynamoDB — 28 supported, 11 partial, 23 not supported** (62
operations). **DynamoDB Streams — 4 supported** (4 operations).

## DynamoDB

| Operation | Status | Note |
|-----------|--------|------|
| BatchExecuteStatement | Partial | PartiQL subset: `SELECT`, `INSERT`, `UPDATE`, `DELETE` only |
| BatchGetItem | Supported | |
| BatchWriteItem | Supported | |
| CancelBulkOperation | Not supported | |
| CreateBackup | Supported | |
| CreateGlobalTable | Partial | Replicas are bookkeeping; no cross-site replication |
| CreateTable | Supported | |
| DeleteBackup | Supported | |
| DeleteItem | Supported | |
| DeleteResourcePolicy | Not supported | |
| DeleteTable | Supported | |
| DescribeBackup | Supported | |
| DescribeBulkOperation | Not supported | |
| DescribeContinuousBackups | Supported | |
| DescribeContributorInsights | Not supported | |
| DescribeEndpoints | Not supported | |
| DescribeExport | Not supported | No S3 export |
| DescribeGlobalTable | Partial | Replicas are bookkeeping; no cross-site replication |
| DescribeGlobalTableSettings | Partial | Replicas are bookkeeping; no cross-site replication |
| DescribeImport | Not supported | No S3 import |
| DescribeKinesisStreamingDestination | Not supported | |
| DescribeLimits | Not supported | |
| DescribeTable | Supported | |
| DescribeTableReplicaAutoScaling | Partial | Auto-scaling settings are bookkeeping |
| DescribeTimeToLive | Supported | |
| DisableKinesisStreamingDestination | Not supported | |
| EnableKinesisStreamingDestination | Not supported | |
| ExecuteStatement | Partial | PartiQL subset: `SELECT`, `INSERT`, `UPDATE`, `DELETE` only |
| ExecuteTransaction | Partial | PartiQL subset: `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `EXISTS` only |
| ExportTableToPointInTime | Not supported | No S3 export |
| GetItem | Supported | |
| GetResourcePolicy | Not supported | |
| ImportTable | Not supported | No S3 import |
| ListBackups | Supported | |
| ListBulkOperations | Not supported | |
| ListContributorInsights | Not supported | |
| ListExports | Not supported | No S3 export |
| ListGlobalTables | Partial | Replicas are bookkeeping; no cross-site replication |
| ListImports | Not supported | No S3 import |
| ListTables | Supported | |
| ListTagsOfResource | Supported | |
| PutItem | Supported | |
| PutResourcePolicy | Not supported | |
| Query | Supported | |
| RestoreTableFromBackup | Supported | |
| RestoreTableToPointInTime | Supported | |
| Scan | Supported | |
| SearchVectors | Not supported | |
| StartBulkPutItems | Not supported | |
| TagResource | Supported | |
| TransactGetItems | Supported | |
| TransactWriteItems | Supported | |
| UntagResource | Supported | |
| UpdateContinuousBackups | Supported | |
| UpdateContributorInsights | Not supported | |
| UpdateGlobalTable | Partial | Replicas are bookkeeping; no cross-site replication |
| UpdateGlobalTableSettings | Partial | Replicas are bookkeeping; no cross-site replication |
| UpdateItem | Supported | |
| UpdateKinesisStreamingDestination | Not supported | |
| UpdateTable | Supported | |
| UpdateTableReplicaAutoScaling | Partial | Auto-scaling settings are bookkeeping |
| UpdateTimeToLive | Supported | |

## DynamoDB Streams

| Operation | Status | Note |
|-----------|--------|------|
| DescribeStream | Supported | |
| GetRecords | Supported | |
| GetShardIterator | Supported | |
| ListStreams | Supported | |

## Differences from DynamoDB

- **Provisioned throughput is accepted but not enforced.** `ReadCapacityUnits`
  and `WriteCapacityUnits` are stored and echoed back by `DescribeTable`, but
  they do not limit or throttle reads or writes. Writes are throttled only when
  a per-item lock times out or deadlocks, which is reported as
  `ProvisionedThroughputExceededException` so SDK clients back off and retry.
- **The 10 GB item-collection limit is not enforced.** On a table with local
  secondary indexes, DynamoDB fails a write with
  `ItemCollectionSizeLimitExceededException` when the total size of all items
  sharing one partition key exceeds 10 GB. dynamoat does not check this, so an
  item collection can grow past 10 GB without an error.
- **Global table replicas are bookkeeping.** dynamoat serves a table from one
  database in every region a request is signed for, so a "replica" in another
  region is the same table. The replicas only make `DescribeTable` and the
  global tables API answer as DynamoDB does; nothing is copied between sites.
  To run a table in several sites, point each dynamoat at the same database or
  replicate the database itself.
- **Backups and point-in-time recovery live in the same Postgres database.**
  On-demand backups and per-table PITR history are stored by the same Postgres
  server that holds your data. They are not a substitute for database backups:
  a failure of that database can take the backups with it. Back up the database
  itself (see the runbook).
- **Streams are delivered to HTTP targets, not Lambda or Kinesis.** The
  DynamoDB Streams API is implemented, and a stream dispatcher `POST`s batches
  to HTTP endpoints you configure. There are no Lambda event source mappings and
  no Kinesis Data Streams destinations.
- **No CloudWatch metrics.** dynamoat exposes Prometheus metrics on `/metrics`
  instead (`DYNAMOAT_METRICS=off` disables the endpoint).
- **No S3 import or export.** `ImportTable`, `ExportTableToPointInTime` and the
  related list/describe operations are not implemented.
- **No DAX.** The DynamoDB Accelerator API is a separate service and is not
  implemented.
- **PartiQL is a subset.** Only `SELECT`, `INSERT`, `UPDATE`, `DELETE` and (in
  `ExecuteTransaction`) `EXISTS` statements are supported, with a
  DynamoDB-shaped `WHERE` clause and expression functions; the full PartiQL
  language is not.
- **Legacy request parameters are not supported.** `Expected`,
  `AttributesToGet`, `KeyConditions`, `QueryFilter`, `ScanFilter` and
  `ConditionalOperator` are rejected with a `ValidationException`; use the
  expression parameters (`ConditionExpression`, `ProjectionExpression`, and so
  on) instead.
- **Server-side encryption and table classes are not implemented.**
  `SSESpecification` and `TableClass` are accepted by `CreateTable` and ignored;
  `UpdateTable` rejects them. Encryption at rest is your Postgres server's
  responsibility.
- **`DescribeTable` reports counts as zero.** `ItemCount` and `TableSizeBytes`
  are not tracked, so `DescribeTable` always reports `0` for both.
