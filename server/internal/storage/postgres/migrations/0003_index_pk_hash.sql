-- dynamoat migration 0003. index_entries gains the base partition key hash so
-- GSI queries can join back to items on the items primary key. Rows written
-- before this column existed (the part B1 schema) are backfilled in Go after
-- migration: pk_hash is SHA-256's first four bytes over pk, which Postgres
-- core cannot compute. The partial index makes the backfill's "any NULL left?"
-- probe cheap once the column is filled.
ALTER TABLE index_entries ADD COLUMN IF NOT EXISTS pk_hash int;
CREATE INDEX IF NOT EXISTS index_entries_pk_hash_null ON index_entries (pk_hash) WHERE pk_hash IS NULL;
