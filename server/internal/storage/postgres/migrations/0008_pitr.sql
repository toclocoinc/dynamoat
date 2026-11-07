-- dynamoat migration 0008. Point-in-time recovery history.
--
-- item_history keeps every version of every item of a table with PITR enabled:
-- one row per write (the new item, or NULL for a delete) with the database
-- clock's timestamp, written in the same transaction as the write. Restore
-- reads the newest non-NULL version at or before the restore time with
-- DISTINCT ON (pk_hash, pk, sk). The primary key sorts by key then time, and a
-- write takes its item's lock, so one item's versions have increasing at.
CREATE TABLE IF NOT EXISTS item_history (
  table_id int         NOT NULL,
  pk_hash  int         NOT NULL,
  pk       bytea       NOT NULL,
  sk       bytea       NOT NULL,
  at       timestamptz NOT NULL,
  item     bytea,
  PRIMARY KEY (table_id, pk_hash, pk, sk, at)
);

-- pitr_state records the initial copy taken when PITR is enabled: RecoveryWindow
-- reports the window only once the copy (done) has finished.
-- done_at is when the copy finished: the window starts there, because the copy
-- labels items with since but reads them a little later.
CREATE TABLE IF NOT EXISTS pitr_state (
  table_id int         PRIMARY KEY,
  since    timestamptz NOT NULL,
  done     boolean     NOT NULL DEFAULT false,
  done_at  timestamptz
);

-- A point-in-time restore runs in the background; these record it on the
-- target table so it resumes after a restart.
ALTER TABLE ddb_tables ADD COLUMN IF NOT EXISTS restore_pitr_source int;
ALTER TABLE ddb_tables ADD COLUMN IF NOT EXISTS restore_pitr_at timestamptz;
