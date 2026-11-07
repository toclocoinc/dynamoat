package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// deleteBatchSize is the number of rows each background delete or backfill
// batch touches.
const deleteBatchSize = 1000

// backoff sleeps delay (at least 1s) and returns the next delay, doubled to at
// most 1 minute. The worker's loop re-checks ctx after it returns.
func backoff(ctx context.Context, delay time.Duration) time.Duration {
	if delay < time.Second {
		delay = time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	next := delay * 2
	if next > time.Minute {
		next = time.Minute
	}
	return next
}

// resumeDeletes restarts the delete worker for every table a previous run left
// DELETING.
func (s *Store) resumeDeletes() {
	rows, err := s.pool.Query(s.ctx, `SELECT table_id FROM ddb_tables WHERE status = $1`, storage.StatusDeleting)
	if err != nil {
		s.log.Error("resume deletes: listing tables", "err", err)
		return
	}
	var ids []int32
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			s.log.Error("resume deletes: scanning table id", "err", err)
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.wg.Add(1)
		go s.deleteTable(s.ctx, id)
	}
}

// resumeBackfills restarts the backfill worker for every GSI a previous run
// left CREATING.
func (s *Store) resumeBackfills() {
	rows, err := s.pool.Query(s.ctx, `SELECT table_id, def FROM ddb_tables WHERE status <> $1`, storage.StatusDeleting)
	if err != nil {
		s.log.Error("resume backfills: listing tables", "err", err)
		return
	}
	type pending struct {
		tableID int32
		g       storage.Index
	}
	var ps []pending
	for rows.Next() {
		var tableID int32
		var def []byte
		if err := rows.Scan(&tableID, &def); err != nil {
			s.log.Error("resume backfills: scanning table", "err", err)
			rows.Close()
			return
		}
		t, err := unmarshalTable(def)
		if err != nil {
			s.log.Error("resume backfills: decoding table def", "table_id", tableID, "err", err)
			continue
		}
		for _, g := range t.GSIs {
			if g.Status == storage.StatusCreating {
				ps = append(ps, pending{tableID: tableID, g: g})
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.log.Error("resume backfills: listing tables", "err", err)
		return
	}
	for _, p := range ps {
		s.wg.Add(1)
		go s.backfill(s.ctx, p.tableID, p.g)
	}
}

// deleteTable removes a table's items, index entries and stream records in
// batches, then the catalog row, retrying on error until the store is closed.
func (s *Store) deleteTable(ctx context.Context, tableID int32) {
	defer s.wg.Done()
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		done, deleted, err := s.deleteTableStep(ctx, tableID)
		if err != nil {
			s.log.Error("delete table: removing rows", "table_id", tableID, "err", err)
			s.jobRun("table-delete", err)
			delay = backoff(ctx, delay)
			continue
		}
		s.jobRun("table-delete", nil)
		s.jobItems("table-delete", deleted)
		if done {
			return
		}
		delay = time.Second
	}
}

// deleteTableStep deletes one batch of rows from each table, then the catalog
// row once every table is empty. done means the table is fully gone; deleted
// is the number of rows removed in this step.
func (s *Store) deleteTableStep(ctx context.Context, tableID int32) (done bool, deleted int64, err error) {
	var items, idx, streams int64
	tag, err := s.pool.Exec(ctx, `DELETE FROM items WHERE ctid IN (SELECT ctid FROM items WHERE table_id = $1 LIMIT 1000)`, tableID)
	if err != nil {
		return false, 0, err
	}
	items = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM index_entries WHERE ctid IN (SELECT ctid FROM index_entries WHERE table_id = $1 LIMIT 1000)`, tableID)
	if err != nil {
		return false, 0, err
	}
	idx = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM stream_records WHERE ctid IN (SELECT ctid FROM stream_records WHERE table_id = $1 LIMIT 1000)`, tableID)
	if err != nil {
		return false, 0, err
	}
	streams = tag.RowsAffected()
	if items == 0 && idx == 0 && streams == 0 {
		if _, err := s.pool.Exec(ctx, `DELETE FROM ddb_tables WHERE table_id = $1`, tableID); err != nil {
			return false, 0, err
		}
		return true, 0, nil
	}
	return false, items + idx + streams, nil
}

// backfillCursor is the position backfill reached in (pk_hash, pk, sk) order.
type backfillCursor struct {
	hash int32
	pk   []byte
	sk   []byte
}

// backfillKey is one item backfill must add to a new GSI, with its lock key.
type backfillKey struct {
	pkHash int32
	pk     []byte
	sk     []byte
	lock   int32
}

// backfill copies items into a new GSI's index_entries, then marks it ACTIVE,
// retrying on error until the store is closed.
func (s *Store) backfill(ctx context.Context, tableID int32, g storage.Index) {
	defer s.wg.Done()
	cur := backfillCursor{hash: math.MinInt32, pk: []byte{}, sk: []byte{}}
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		keys, last, err := s.readBackfillKeys(ctx, tableID, cur)
		if err != nil {
			s.log.Error("backfill: reading items", "table_id", tableID, "index_id", g.ID, "err", err)
			s.jobRun("backfill", err)
			delay = backoff(ctx, delay)
			continue
		}
		written := 0
		if len(keys) > 0 {
			written, err = s.writeBackfillBatch(ctx, tableID, g, keys)
			if err != nil {
				s.log.Error("backfill: writing index entries", "table_id", tableID, "index_id", g.ID, "err", err)
				s.jobRun("backfill", err)
				delay = backoff(ctx, delay)
				continue
			}
		}
		s.jobRun("backfill", nil)
		s.jobItems("backfill", int64(written))
		cur = last
		if len(keys) < deleteBatchSize {
			break
		}
		delay = time.Second
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.markIndexActive(ctx, tableID, g.ID); err != nil {
			s.log.Error("backfill: marking index active", "table_id", tableID, "index_id", g.ID, "err", err)
			delay = backoff(ctx, delay)
			continue
		}
		return
	}
}

// readBackfillKeys reads the next batch of item keys in key order.
func (s *Store) readBackfillKeys(ctx context.Context, tableID int32, cur backfillCursor) ([]backfillKey, backfillCursor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pk_hash, pk, sk
		FROM items
		WHERE table_id = $1 AND (pk_hash, pk, sk) > ($2::int, $3::bytea, $4::bytea)
		ORDER BY pk_hash, pk, sk
		LIMIT 1000`, tableID, cur.hash, cur.pk, cur.sk)
	if err != nil {
		return nil, cur, err
	}
	defer rows.Close()
	var keys []backfillKey
	var last backfillCursor
	for rows.Next() {
		var h int32
		var pk, sk []byte
		if err := rows.Scan(&h, &pk, &sk); err != nil {
			return nil, cur, err
		}
		keys = append(keys, backfillKey{pkHash: h, pk: pk, sk: sk, lock: itemLock(pk, sk)})
		last = backfillCursor{hash: h, pk: pk, sk: sk}
	}
	if err := rows.Err(); err != nil {
		return nil, cur, err
	}
	return keys, last, nil
}

// writeBackfillBatch adds each item's index entry in one transaction: for each
// item it takes the item lock, re-reads the item and inserts its entry only if
// it is still a member of the index. Locks are taken in item-lock order so a
// backfill transaction and a Transact cannot deadlock. It returns how many
// entries it wrote.
func (s *Store) writeBackfillBatch(ctx context.Context, tableID int32, g storage.Index, keys []backfillKey) (int, error) {
	ordered := append([]backfillKey(nil), keys...)
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].lock != ordered[b].lock {
			return ordered[a].lock < ordered[b].lock
		}
		if ordered[a].pkHash != ordered[b].pkHash {
			return ordered[a].pkHash < ordered[b].pkHash
		}
		if c := bytes.Compare(ordered[a].pk, ordered[b].pk); c != 0 {
			return c < 0
		}
		return bytes.Compare(ordered[a].sk, ordered[b].sk) < 0
	})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	read := &pgx.Batch{}
	read.Queue(s.setLockTimeoutSQL())
	for _, k := range ordered {
		read.Queue(`SELECT pg_advisory_xact_lock($1::int, $2::int)`, tableID, k.lock)
		read.Queue(`SELECT item FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3 AND sk = $4 FOR UPDATE`,
			tableID, k.pkHash, k.pk, k.sk)
	}
	br := tx.SendBatch(ctx, read)
	if _, err := br.Exec(); err != nil { // SET LOCAL lock_timeout
		br.Close()
		return 0, err
	}
	type insert struct {
		ih     int32
		ipk    []byte
		isk    []byte
		pkHash int32
		pk     []byte
		sk     []byte
	}
	var inserts []insert
	for _, k := range ordered {
		if _, err := br.Exec(); err != nil { // advisory lock
			br.Close()
			return 0, err
		}
		var itemBytes []byte
		err := br.QueryRow().Scan(&itemBytes)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // deleted while backfilling
		}
		if err != nil {
			br.Close()
			return 0, err
		}
		var it attr.Item
		if err := json.Unmarshal(itemBytes, &it); err != nil {
			br.Close()
			return 0, err
		}
		if ih, ipk, isk, ok := indexKey(g, it); ok {
			inserts = append(inserts, insert{ih: ih, ipk: ipk, isk: isk, pkHash: k.pkHash, pk: k.pk, sk: k.sk})
		}
	}
	br.Close()

	if len(inserts) > 0 {
		wb := &pgx.Batch{}
		for _, in := range inserts {
			wb.Queue(`INSERT INTO index_entries (table_id, index_id, ipk_hash, ipk, isk, pk_hash, pk, sk) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
				tableID, g.ID, in.ih, in.ipk, in.isk, in.pkHash, in.pk, in.sk)
		}
		if err := execBatch(ctx, tx, wb); err != nil {
			return 0, err
		}
	}
	return len(inserts), tx.Commit(ctx)
}

// jobRun records one completed run of a background worker, ok or error.
func (s *Store) jobRun(job string, err error) {
	if s.metrics != nil {
		s.metrics.JobRun(job, err)
	}
}

// jobItems records the number of rows a background worker processed.
func (s *Store) jobItems(job string, n int64) {
	if s.metrics != nil {
		s.metrics.JobItems(job, n)
	}
}

// markIndexActive sets a GSI ACTIVE once its backfill has finished. It reports
// nil when the index or table is gone, so a backfill racing a DeleteTable can
// finish cleanly.
func (s *Store) markIndexActive(ctx context.Context, tableID int32, indexID int16) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var def []byte
	err = tx.QueryRow(ctx, `SELECT def FROM ddb_tables WHERE table_id = $1 FOR UPDATE`, tableID).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	t, err := unmarshalTable(def)
	if err != nil {
		return err
	}
	for i := range t.GSIs {
		if t.GSIs[i].ID == indexID {
			t.GSIs[i].Status = storage.StatusActive
			defJSON, err := json.Marshal(t)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb WHERE table_id = $2`, string(defJSON), tableID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
	}
	return tx.Commit(ctx)
}
