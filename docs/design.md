# dynamoat design

## Shape

One stateless Go process (scale it horizontally) in front of one Postgres
database per deployment. Managed Postgres (Cloud SQL, Azure Database for
PostgreSQL Flexible Server) provides HA, backups and point-in-time restore.
Supported Postgres: 15, 16, 17, 18; 17 recommended. No extensions, no
superuser features. The server refuses to start on Postgres older than 15.

## Storage

A DynamoDB table is a row in `ddb_tables`, not a Postgres table: an application
creates a table per tenant with about seven GSIs, and DDL per table would not
scale. Every item of every table lives in `items`:

| column    | meaning |
|-----------|---------|
| table_id  | the catalog row |
| pk_hash   | int32 hash of the encoded partition key (spreads Scan segments; leads the primary key) |
| pk, sk    | the encoded partition and sort keys (`bytea`, order-preserving; sk is empty for hash-only tables) |
| item      | the whole item as DynamoDB typed JSON (`{"id":{"S":"x"},"n":{"N":"5"}}`) as raw UTF-8 bytes in `bytea` (migration 0004: `jsonb` cannot hold `\u0000`); read it with `convert_from(item, 'UTF8')::jsonb` |
| ttl_at    | the TTL attribute as a timestamp, for the sweeper's partial index |

GSIs are key-only rows in `index_entries` (index key plus the base item's key),
joined back to `items` for the projection. `stream_records` is written in the
same transaction as the item. A stream record's readable position is assigned only
once every transaction that could precede it has finished (its transaction ID
is below the oldest running one; migration 0006 and `Store.sequence`), so a
consumer that checkpoints the last position it processed never misses a write
that committed late. A long-running transaction anywhere in the database holds
stream delivery back until it ends. Schema: `server/internal/storage/postgres/migrations/`.

## Key encoding (internal/attr)

Keys are compared as bytes by Postgres (`bytea`), so the encoding must sort
exactly as DynamoDB sorts:

- **S**: the UTF-8 bytes. DynamoDB orders strings by UTF-8 byte value.
- **B**: the raw bytes.
- **N**: numeric order, by this encoding of the normalised decimal
  `0.d1d2...dn x 10^E` (d1 != 0, n <= 38, E in -129..126):
  - zero: `0x80`
  - positive: `0xC0`, `byte(E + 129)`, then each digit as `byte(d + 1)`, then `0x00`
  - negative: `0x40`, `byte(126 - E)`, then each digit as `byte(10 - d)`, then `0xFF`

A table's key attributes have one fixed type each, so values of different
types are never compared.

`pk_hash` is the first four bytes of SHA-256 over the encoded partition key,
read big-endian as a signed int32.

## Writes

Each write is one Postgres transaction. It takes
`pg_advisory_xact_lock(table_id, pk_hash)`, reads the current item, evaluates
any condition, writes the item, diffs and writes its index entries, and writes
the stream record. `TransactWriteItems` takes its locks in sorted key order.

## Verified on every supported Postgres

A lab running PG 15-18 checks the schema's assumptions:
bytea order, exact JSON round trip of 38-digit numbers, upsert, item and
stream record committing together, the GSI prefix query as an index range
scan, the TTL partial index, and advisory lock exclusion.
