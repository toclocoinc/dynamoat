-- dynamoat migration 0006. Commit-safe stream positions.
--
-- seq comes from a sequence while a write's transaction is still open, so two
-- writes can commit in the opposite order to their seq: a reader that has moved
-- past seq 105 would never see seq 100 when it commits later. Each record now
-- carries the ID of the transaction that wrote it (xid), and gets its final
-- position (pos) only once every transaction that could precede it has
-- finished (xid below the oldest running transaction). Readers read by pos.
-- See Store.sequence in streams.go.
ALTER TABLE stream_records ADD COLUMN IF NOT EXISTS xid xid8 NOT NULL DEFAULT pg_current_xact_id();
ALTER TABLE stream_records ADD COLUMN IF NOT EXISTS pos bigint;

-- Records written before this migration were committed when it ran (it holds
-- the migration lock at startup); they keep their seq as their position.
UPDATE stream_records SET pos = seq WHERE pos IS NULL;

CREATE TABLE IF NOT EXISTS stream_pos (
  id   int PRIMARY KEY CHECK (id = 1),
  last bigint NOT NULL
);
INSERT INTO stream_pos (id, last)
SELECT 1, COALESCE(max(pos), 0) FROM stream_records
ON CONFLICT (id) DO NOTHING;

CREATE INDEX IF NOT EXISTS stream_records_table_pos ON stream_records (table_id, pos) WHERE pos IS NOT NULL;
CREATE INDEX IF NOT EXISTS stream_records_unsequenced ON stream_records (xid, seq) WHERE pos IS NULL;
