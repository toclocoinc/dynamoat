-- dynamoat migration 0007. On-demand backups (BackupStore).
--
-- backups holds one row per backup: the table definition as of the snapshot
-- and, once the copy is done, the item and byte counts. backup_items holds the
-- copied items; item is raw JSON bytes (bytea), like items.item (see 0004).
-- A backup stays CREATING while its items are copied, then AVAILABLE.
CREATE TABLE IF NOT EXISTS backups (
  backup_id  text PRIMARY KEY,
  name       text,
  table_name text,
  def        jsonb,
  status     text,
  created_at timestamptz,
  items      bigint,
  bytes      bigint
);

CREATE TABLE IF NOT EXISTS backup_items (
  backup_id text,
  pk_hash   int,
  pk        bytea,
  sk        bytea,
  item      bytea,
  PRIMARY KEY (backup_id, pk_hash, pk, sk)
);

-- A table restored from a backup records its source here so Open can resume a
-- restore left CREATING by a previous run.
ALTER TABLE ddb_tables ADD COLUMN IF NOT EXISTS restore_backup_id text;
