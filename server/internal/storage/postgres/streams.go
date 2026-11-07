package postgres

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// sequenceLockKey serialises Store.sequence across every server process.
const sequenceLockKey = 7344

// sequence gives committed records their final position (pos). A record is
// numbered only when its transaction ID is below the oldest transaction still
// running, so no transaction that could commit later can be numbered before
// it. Records are numbered in (xid, seq) order: per item that is commit order,
// because a write takes the item's lock before its first change (which is when
// its transaction ID is assigned), so the next write to that item gets a later
// ID. If another process is numbering, this call does nothing; its numbers are
// visible once it commits.
func (s *Store) sequence(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1::bigint)`, int64(sequenceLockKey)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		WITH next AS (
			SELECT ctid, row_number() OVER (ORDER BY xid, seq) AS n
			FROM stream_records
			WHERE pos IS NULL AND xid < pg_snapshot_xmin(pg_current_snapshot())
			ORDER BY xid, seq
			LIMIT 10000
		), base AS (
			SELECT last FROM stream_pos WHERE id = 1 FOR UPDATE
		), numbered AS (
			UPDATE stream_records r SET pos = base.last + next.n
			FROM next, base WHERE r.ctid = next.ctid
			RETURNING 1
		)
		UPDATE stream_pos SET last = last + (SELECT count(*) FROM numbered) WHERE id = 1`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReadStream returns up to limit records of t with Seq > after, in Seq order.
// limit <= 0 reads to the end of the stream. Seq is the record's commit-safe
// position (pos), not the raw seq it was inserted with.
func (s *Store) ReadStream(ctx context.Context, t *storage.Table, after int64, limit int) ([]storage.StreamRecord, error) {
	if err := s.sequence(ctx); err != nil {
		return nil, err
	}
	sql := `SELECT pos, event, keys, old_image, new_image, created_at, ttl, size
	        FROM stream_records WHERE table_id = $1 AND pos > $2 ORDER BY pos`
	args := []any{t.ID, after}
	if limit > 0 {
		sql += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.StreamRecord
	for rows.Next() {
		var r storage.StreamRecord
		var keysRaw, oldRaw, newRaw []byte
		if err := rows.Scan(&r.Seq, &r.Event, &keysRaw, &oldRaw, &newRaw, &r.At, &r.TTL, &r.Size); err != nil {
			return nil, err
		}
		if r.Keys, err = decodeItem(keysRaw); err != nil {
			return nil, err
		}
		if len(oldRaw) > 0 {
			if r.Old, err = decodeItem(oldRaw); err != nil {
				return nil, err
			}
		}
		if len(newRaw) > 0 {
			if r.New, err = decodeItem(newRaw); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StreamEnd returns the highest position ReadStream can return for t now.
func (s *Store) StreamEnd(ctx context.Context, t *storage.Table) (int64, error) {
	if err := s.sequence(ctx); err != nil {
		return 0, err
	}
	var end int64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(max(pos), 0) FROM stream_records WHERE table_id = $1 AND pos IS NOT NULL`, t.ID).Scan(&end)
	return end, err
}

// TrimStreams deletes every record created before before, at most limit per
// call (0 means no limit), and returns how many it deleted. Records are
// deleted by ctid so a second call resumes where the first left off.
func (s *Store) TrimStreams(ctx context.Context, before time.Time, limit int) (int, error) {
	total := 0
	for {
		batch := limit
		if batch <= 0 {
			batch = deleteBatchSize
		}
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM stream_records WHERE ctid IN (SELECT ctid FROM stream_records WHERE created_at < $1 LIMIT $2)`,
			before, batch)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		total += n
		if limit > 0 || n < batch {
			return total, nil
		}
	}
}

// Checkpoint returns the last Seq the consumer finished for t, 0 if none.
func (s *Store) Checkpoint(ctx context.Context, consumer string, t *storage.Table) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx,
		`SELECT seq FROM stream_checkpoints WHERE consumer = $1 AND table_id = $2`, consumer, t.ID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// SaveCheckpoint records the last Seq the consumer finished for t.
func (s *Store) SaveCheckpoint(ctx context.Context, consumer string, t *storage.Table, seq int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO stream_checkpoints (consumer, table_id, seq) VALUES ($1, $2, $3)
		 ON CONFLICT (consumer, table_id) DO UPDATE SET seq = EXCLUDED.seq`,
		consumer, t.ID, seq)
	return err
}

// TryLease takes or renews the named lease for owner until ttl from now. One
// upsert replaces only an expired lease or one already held by owner; it
// returns whether owner ends up holding the lease.
func (s *Store) TryLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	expires := time.Now().Add(ttl)
	var got string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO leases (name, owner, expires_at) VALUES ($1, $2, $3)
		 ON CONFLICT (name) DO UPDATE
		 SET owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
		 WHERE leases.owner = EXCLUDED.owner OR leases.expires_at <= now()
		 RETURNING owner`,
		name, owner, expires).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == owner, nil
}
