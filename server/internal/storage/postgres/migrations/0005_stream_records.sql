-- dynamoat migration 0005. stream_records grows the two fields the stream
-- reader and the TTL sweeper need (ttl marks a REMOVE made by TTL expiry, size
-- is the record's SizeBytes), and its JSON images become bytea: jsonb cannot
-- hold U+0000 (see 0004), and the store never queries the image JSON with SQL,
-- so the exact JSON text is kept as bytea. A (table_id, seq) index serves
-- ReadStream. stream_checkpoints and leases back the StreamStore/TTLStore
-- background jobs.
ALTER TABLE stream_records ADD COLUMN IF NOT EXISTS ttl boolean NOT NULL DEFAULT false;
ALTER TABLE stream_records ADD COLUMN IF NOT EXISTS size int NOT NULL DEFAULT 0;

ALTER TABLE stream_records ALTER COLUMN keys TYPE bytea USING keys::text::bytea;
ALTER TABLE stream_records ALTER COLUMN old_image TYPE bytea USING old_image::text::bytea;
ALTER TABLE stream_records ALTER COLUMN new_image TYPE bytea USING new_image::text::bytea;

CREATE INDEX IF NOT EXISTS stream_records_table_seq ON stream_records (table_id, seq);

CREATE TABLE IF NOT EXISTS stream_checkpoints (
  consumer text NOT NULL,
  table_id int  NOT NULL,
  seq      bigint NOT NULL,
  PRIMARY KEY (consumer, table_id)
);

CREATE TABLE IF NOT EXISTS leases (
  name       text PRIMARY KEY,
  owner      text NOT NULL,
  expires_at timestamptz NOT NULL
);
