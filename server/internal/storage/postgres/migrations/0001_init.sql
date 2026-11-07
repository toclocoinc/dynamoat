-- dynamoat migration 0001. Same schema verified on PG 15-18.
-- dynamoat storage schema. Idempotent.

CREATE TABLE IF NOT EXISTS ddb_tables (
  table_id     serial PRIMARY KEY,
  name         text UNIQUE NOT NULL,
  key_schema   jsonb NOT NULL,
  indexes      jsonb NOT NULL DEFAULT '[]',
  ttl_attr     text,
  stream_view  text,
  status       text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS items (
  table_id  int    NOT NULL,
  pk_hash   int    NOT NULL,
  pk        bytea  NOT NULL,
  sk        bytea  NOT NULL,
  item      jsonb  NOT NULL,
  ttl_at    timestamptz,
  PRIMARY KEY (table_id, pk_hash, pk, sk)
) WITH (fillfactor = 85);

CREATE TABLE IF NOT EXISTS index_entries (
  table_id  int      NOT NULL,
  index_id  smallint NOT NULL,
  ipk_hash  int      NOT NULL,
  ipk       bytea    NOT NULL,
  isk       bytea    NOT NULL,
  pk        bytea    NOT NULL,
  sk        bytea    NOT NULL,
  PRIMARY KEY (table_id, index_id, ipk_hash, ipk, isk, pk, sk)
);

CREATE TABLE IF NOT EXISTS stream_records (
  seq        bigserial PRIMARY KEY,
  table_id   int NOT NULL,
  event      text NOT NULL,
  keys       jsonb NOT NULL,
  old_image  jsonb,
  new_image  jsonb,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS items_ttl ON items (ttl_at) WHERE ttl_at IS NOT NULL;
