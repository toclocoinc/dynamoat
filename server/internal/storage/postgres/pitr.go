package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// pitrTrail is how far the latest restorable time trails now. DynamoDB's latest
// restorable time trails by a few minutes; five seconds is our choice.
const pitrTrail = 5 * time.Second

// pitrWindow returns the times t can currently be restored to. The earliest is
// the later of PITRSince and now minus PITRDays days; the latest is pitrTrail
// behind now.
func pitrWindow(t *storage.Table, now time.Time) (earliest, latest time.Time) {
	days := t.PITRDays
	if days <= 0 {
		days = 35
	}
	earliest = t.PITRSince
	if start := now.AddDate(0, 0, -days); start.After(earliest) {
		earliest = start
	}
	return earliest, now.Add(-pitrTrail)
}

// buildHistory queues one history row for a write to a table with PITR enabled:
// the item's new version, or a tombstone (item NULL) for a delete. The timestamp
// comes from the database clock, in the same transaction as the write.
func buildHistory(b *pgx.Batch, t *storage.Table, lk lockKey, new attr.Item) {
	if !t.PITREnabled {
		return
	}
	var itemJSON []byte
	if new != nil {
		itemJSON, _ = json.Marshal(new)
	}
	b.Queue(`INSERT INTO item_history (table_id, pk_hash, pk, sk, at, item)
	         VALUES ($1, $2, $3, $4, clock_timestamp(), $5)`,
		t.ID, lk.pkHash, lk.pk, lk.sk, itemJSON)
}

// pitrReady reports nil when t's recovery window is ready: recovery is on and
// the initial copy taken when it was enabled has finished.
func (s *Store) pitrReady(ctx context.Context, t *storage.Table) (time.Time, error) {
	if !t.PITREnabled {
		return time.Time{}, storage.ErrOutsideRecoveryWindow
	}
	var done bool
	var doneAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT done, done_at FROM pitr_state WHERE table_id = $1`, t.ID).Scan(&done, &doneAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, storage.ErrOutsideRecoveryWindow
	}
	if err != nil {
		return time.Time{}, err
	}
	if !done || doneAt == nil {
		return time.Time{}, storage.ErrOutsideRecoveryWindow
	}
	return *doneAt, nil
}

// readyWindow is t's restorable window: pitrWindow, but starting no earlier
// than doneAt. The initial copy labels each item with PITRSince yet reads it a
// little later, so a time before the copy finished could see a later version.
func readyWindow(t *storage.Table, doneAt, now time.Time) (time.Time, time.Time) {
	earliest, latest := pitrWindow(t, now)
	if doneAt.After(earliest) {
		earliest = doneAt
	}
	return earliest, latest
}

// RecoveryWindow returns the earliest and latest times t can be restored to now.
func (s *Store) RecoveryWindow(ctx context.Context, t *storage.Table) (time.Time, time.Time, error) {
	cur, err := s.GetTable(ctx, t.Name)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	doneAt, err := s.pitrReady(ctx, cur)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	earliest, latest := readyWindow(cur, doneAt, time.Now())
	return earliest, latest, nil
}

// RestoreToTime creates target (as CreateTable does, ErrTableExists if the name
// is taken) and fills it with t's items as they were at time at: for each key,
// the newest version at or before at that is not a tombstone. GSI entries are
// built from the restored items; the table is CREATING until that is done.
func (s *Store) RestoreToTime(ctx context.Context, t *storage.Table, at time.Time, target *storage.Table) (*storage.Table, error) {
	cur, err := s.GetTable(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	doneAt, err := s.pitrReady(ctx, cur)
	if err != nil {
		return nil, err
	}
	earliest, latest := readyWindow(cur, doneAt, time.Now())
	if at.Before(earliest) || at.After(latest) {
		return nil, storage.ErrOutsideRecoveryWindow
	}

	c := cloneTable(target)
	assignGSIIDs(c)
	c.Status = storage.StatusCreating
	for i := range c.GSIs {
		c.GSIs[i].Status = storage.StatusCreating
	}

	keyJSON, err := json.Marshal(c.Key)
	if err != nil {
		return nil, err
	}

	// Record the CREATING table now and fill it in the background, in
	// batches, so a large restore neither runs inside the request nor holds
	// every item in memory.
	defJSON, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var id int32
	err = s.pool.QueryRow(ctx,
		`INSERT INTO ddb_tables (name, key_schema, status, def, restore_pitr_source, restore_pitr_at, deletion_protection)
		 VALUES ($1, $2::jsonb, $3, $4::jsonb, $5, $6, $7) RETURNING table_id`,
		c.Name, string(keyJSON), c.Status, string(defJSON), cur.ID, at, c.DeletionProtection).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, storage.ErrTableExists
		}
		return nil, err
	}
	c.ID = id
	if defJSON, err = json.Marshal(c); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb WHERE table_id = $2`, string(defJSON), id); err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go s.restorePITR(s.ctx, cur.ID, at, id)
	return cloneTable(c), nil
}

// restorePITR fills a point-in-time restore's target table in batches, then
// marks it ACTIVE, retrying on error until the store closes.
func (s *Store) restorePITR(ctx context.Context, sourceID int32, at time.Time, tableID int32) {
	defer s.wg.Done()
	t, err := s.tableByID(ctx, tableID)
	if err != nil {
		s.log.Error("pitr restore: loading table", "table_id", tableID, "err", err)
		return
	}
	cur := pitrCursor{hash: math.MinInt32, pk: []byte{}, sk: []byte{}}
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		n, last, err := s.restorePITRBatch(ctx, sourceID, at, t, cur)
		if err != nil {
			s.log.Error("pitr restore: copying items", "table_id", tableID, "source_table_id", sourceID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		cur = last
		if n < deleteBatchSize {
			break
		}
		delay = time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.markTableActive(ctx, tableID); err != nil {
			s.log.Error("pitr restore: marking table active", "table_id", tableID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		return
	}
}

// restorePITRBatch restores the next keys after cur: for each, the newest
// version at or before at (skipping tombstones), with its GSI entries. It
// returns how many keys it read, tombstones included, so the caller knows when
// the history is exhausted. The upserts make a resume after a partial copy safe.
func (s *Store) restorePITRBatch(ctx context.Context, sourceID int32, at time.Time, t *storage.Table, cur pitrCursor) (int, pitrCursor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (pk_hash, pk, sk) pk_hash, pk, sk, item
		FROM item_history
		WHERE table_id = $1 AND at <= $2 AND (pk_hash, pk, sk) > ($3::int, $4::bytea, $5::bytea)
		ORDER BY pk_hash, pk, sk, at DESC
		LIMIT 1000`, sourceID, at, cur.hash, cur.pk, cur.sk)
	if err != nil {
		return 0, cur, err
	}
	type row struct {
		h        int32
		pk, sk   []byte
		itemJSON []byte
	}
	var batch []row
	last := cur
	n := 0
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.h, &r.pk, &r.sk, &r.itemJSON); err != nil {
			rows.Close()
			return 0, cur, err
		}
		n++
		last = pitrCursor{hash: r.h, pk: r.pk, sk: r.sk}
		if r.itemJSON != nil {
			batch = append(batch, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, cur, err
	}
	if len(batch) == 0 {
		return n, last, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, cur, err
	}
	defer tx.Rollback(ctx)
	b := &pgx.Batch{}
	for _, r := range batch {
		it, err := decodeItem(r.itemJSON)
		if err != nil {
			return 0, cur, err
		}
		lk := lockKey{tableID: t.ID, pkHash: r.h, pk: r.pk, sk: r.sk}
		if err := queueRestoreItem(b, t, lk, it); err != nil {
			return 0, cur, err
		}
	}
	if err := execBatch(ctx, tx, b); err != nil {
		return 0, cur, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, cur, err
	}
	return n, last, nil
}

// queueRestoreItem queues the item and its GSI entries for a restore. It never
// writes stream records: a restored table has no stream.
func queueRestoreItem(b *pgx.Batch, t *storage.Table, lk lockKey, it attr.Item) error {
	itemJSON, err := json.Marshal(it)
	if err != nil {
		return err
	}
	b.Queue(`INSERT INTO items (table_id, pk_hash, pk, sk, item, ttl_at)
	         VALUES ($1, $2, $3, $4, $5, $6)`,
		t.ID, lk.pkHash, lk.pk, lk.sk, itemJSON, ttlAt(t, it))
	buildGSI(b, t, lk, nil, it)
	return nil
}

// queuePITREnable records the start of an enable inside the UpdateTable
// transaction, so recovery stays unavailable until the copy finishes.
func queuePITREnable(ctx context.Context, tx pgx.Tx, tableID int32, since time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO pitr_state (table_id, since, done) VALUES ($1, $2, false)
		ON CONFLICT (table_id) DO UPDATE SET since = EXCLUDED.since, done = EXCLUDED.done`,
		tableID, since)
	return err
}

// pitrCursor is the position of the initial copy in (pk_hash, pk, sk) order.
type pitrCursor struct {
	hash int32
	pk   []byte
	sk   []byte
}

// resumePITR restarts the initial copy for every table a previous run left with
// pitr_state.done false.
func (s *Store) resumePITR() {
	rows, err := s.pool.Query(s.ctx, `SELECT table_id, since FROM pitr_state WHERE done = false`)
	if err != nil {
		s.log.Error("resume pitr: listing copies", "err", err)
		return
	}
	type job struct {
		tableID int32
		since   time.Time
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.tableID, &j.since); err != nil {
			s.log.Error("resume pitr: scanning copy", "err", err)
			rows.Close()
			return
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		s.wg.Add(1)
		go s.pitrCopy(s.ctx, j.tableID, j.since)
	}

	rows, err = s.pool.Query(s.ctx, `SELECT table_id, restore_pitr_source, restore_pitr_at FROM ddb_tables
		WHERE status = $1 AND restore_pitr_source IS NOT NULL`, storage.StatusCreating)
	if err != nil {
		s.log.Error("resume pitr restores: listing tables", "err", err)
		return
	}
	type restore struct {
		tableID, sourceID int32
		at                time.Time
	}
	var restores []restore
	for rows.Next() {
		var r restore
		if err := rows.Scan(&r.tableID, &r.sourceID, &r.at); err != nil {
			s.log.Error("resume pitr restores: scanning table", "err", err)
			rows.Close()
			return
		}
		restores = append(restores, r)
	}
	rows.Close()
	for _, r := range restores {
		s.wg.Add(1)
		go s.restorePITR(s.ctx, r.sourceID, r.at, r.tableID)
	}
}

// pitrCopy copies a table's current items into item_history as of since, then
// marks the copy done, retrying on error until the store closes. It is the
// background half of enabling PITR.
func (s *Store) pitrCopy(ctx context.Context, tableID int32, since time.Time) {
	defer s.wg.Done()
	t, err := s.tableByID(ctx, tableID)
	if err != nil || !t.PITREnabled || !t.PITRSince.Equal(since) {
		return
	}
	cur := pitrCursor{hash: math.MinInt32, pk: []byte{}, sk: []byte{}}
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		n, last, err := s.copyPITRBatch(ctx, tableID, since, cur)
		if err != nil {
			s.log.Error("pitr copy: copying items", "table_id", tableID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		cur = last
		if n < deleteBatchSize {
			break
		}
		delay = time.Second
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.markPITRDone(ctx, tableID, since); err != nil {
			s.log.Error("pitr copy: marking done", "table_id", tableID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		return
	}
}

// copyPITRBatch reads up to 1000 items and writes their history rows, all as of
// since. It is idempotent: retrying a batch re-inserts the same rows.
func (s *Store) copyPITRBatch(ctx context.Context, tableID int32, since time.Time, cur pitrCursor) (int, pitrCursor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pk_hash, pk, sk, item
		FROM items
		WHERE table_id = $1 AND (pk_hash, pk, sk) > ($2::int, $3::bytea, $4::bytea)
		ORDER BY pk_hash, pk, sk
		LIMIT 1000`, tableID, cur.hash, cur.pk, cur.sk)
	if err != nil {
		return 0, cur, err
	}
	defer rows.Close()
	b := &pgx.Batch{}
	n := 0
	last := cur
	for rows.Next() {
		var h int32
		var pk, sk, itemBytes []byte
		if err := rows.Scan(&h, &pk, &sk, &itemBytes); err != nil {
			return 0, cur, err
		}
		b.Queue(`INSERT INTO item_history (table_id, pk_hash, pk, sk, at, item)
		         VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			tableID, h, pk, sk, since, itemBytes)
		n++
		last = pitrCursor{hash: h, pk: pk, sk: sk}
	}
	if err := rows.Err(); err != nil {
		return 0, cur, err
	}
	if b.Len() > 0 {
		br := s.pool.SendBatch(ctx, b)
		for i := 0; i < b.Len(); i++ {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return 0, cur, err
			}
		}
		br.Close()
	}
	return n, last, nil
}

// markPITRDone records that the initial copy for since is complete. A zero
// affected count means the table was re-enabled while the copy ran; there is
// nothing to mark.
func (s *Store) markPITRDone(ctx context.Context, tableID int32, since time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE pitr_state SET done = true, done_at = now() WHERE table_id = $1 AND since = $2`, tableID, since)
	return err
}

// TrimHistory deletes item versions no table's recovery window needs any more,
// at most limit per call (0 means no limit), and returns how many it deleted.
func (s *Store) TrimHistory(ctx context.Context, now time.Time, limit int) (int, error) {
	total := 0
	for {
		batch := limit
		if batch <= 0 {
			batch = deleteBatchSize
		}
		n, err := s.trimHistoryBatch(ctx, now, batch)
		if err != nil {
			return total, err
		}
		total += n
		if limit > 0 || n < batch {
			return total, nil
		}
	}
}

// trimHistoryBatch deletes up to limit history rows in one pass: orphans first,
// then the whole history of tables with recovery off or deleted, then versions
// older than each enabled table's window start (keeping the newest at or before
// it per key).
func (s *Store) trimHistoryBatch(ctx context.Context, now time.Time, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT table_id, def, status FROM ddb_tables`)
	if err != nil {
		return 0, err
	}
	type tab struct {
		enabled bool
		gone    bool
		start   time.Time
	}
	tabs := make(map[int32]tab)
	for rows.Next() {
		var id int32
		var def []byte
		var status string
		if err := rows.Scan(&id, &def, &status); err != nil {
			rows.Close()
			return 0, err
		}
		t, err := unmarshalTable(def)
		if err != nil {
			rows.Close()
			return 0, err
		}
		start, _ := pitrWindow(t, now)
		tabs[id] = tab{enabled: t.PITREnabled, gone: status == storage.StatusDeleting, start: start}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	deleted := 0

	// History of tables whose catalog row is gone.
	if deleted < limit {
		n, err := s.deleteOrphanHistory(ctx, limit-deleted)
		if err != nil {
			return deleted, err
		}
		deleted += n
	}

	for id, tb := range tabs {
		if deleted >= limit {
			break
		}
		var n int
		if !tb.enabled || tb.gone {
			n, err = s.deleteAllHistory(ctx, id, limit-deleted)
		} else {
			n, err = s.deleteOldHistory(ctx, id, tb.start, limit-deleted)
		}
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

func (s *Store) deleteOrphanHistory(ctx context.Context, max int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM item_history WHERE ctid IN (
			SELECT h.ctid FROM item_history h
			WHERE NOT EXISTS (SELECT 1 FROM ddb_tables d WHERE d.table_id = h.table_id)
			LIMIT $1)`, max)
	if err != nil {
		return 0, err
	}
	// Drop orphaned copy bookkeeping too.
	if _, err := s.pool.Exec(ctx, `DELETE FROM pitr_state WHERE NOT EXISTS (SELECT 1 FROM ddb_tables d WHERE d.table_id = pitr_state.table_id)`); err != nil {
		return int(tag.RowsAffected()), err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) deleteAllHistory(ctx context.Context, tableID int32, max int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM item_history WHERE ctid IN (
			SELECT ctid FROM item_history WHERE table_id = $1 LIMIT $2)`, tableID, max)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) deleteOldHistory(ctx context.Context, tableID int32, start time.Time, max int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM item_history WHERE ctid IN (
			SELECT h.ctid FROM item_history h
			WHERE h.table_id = $1 AND h.at < $2
			  AND EXISTS (
				SELECT 1 FROM item_history h2
				WHERE h2.table_id = h.table_id
				  AND h2.pk_hash = h.pk_hash AND h2.pk = h.pk AND h2.sk = h.sk
				  AND h2.at > h.at AND h2.at <= $2)
			LIMIT $3)`, tableID, start, max)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
