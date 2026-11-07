package postgres

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// ExpireItems deletes up to limit expired items across all tables, each exactly
// as a WriteItem Delete would (GSI entries and a REMOVE stream record with TTL
// set when the table has a stream), and returns how many it deleted. limit <= 0
// means no limit.
func (s *Store) ExpireItems(ctx context.Context, now time.Time, limit int) (int, error) {
	total := 0
	for {
		batch := limit
		if batch <= 0 {
			batch = deleteBatchSize
		}
		n, err := s.expireBatch(ctx, now, batch)
		if err != nil {
			return total, err
		}
		total += n
		if limit > 0 || n < batch {
			return total, nil
		}
	}
}

// expireBatch selects up to limit expired items (via the items_ttl partial
// index) and deletes each through the locked write path.
func (s *Store) expireBatch(ctx context.Context, now time.Time, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.table_id, i.pk_hash, i.pk, i.sk, d.def
		FROM items i JOIN ddb_tables d ON d.table_id = i.table_id
		WHERE i.ttl_at < $1
		ORDER BY i.ttl_at
		LIMIT $2`, now, limit)
	if err != nil {
		return 0, err
	}
	type pending struct {
		t  *storage.Table
		lk lockKey
	}
	var pend []pending
	for rows.Next() {
		var tableID int32
		var def []byte
		var lk lockKey
		if err := rows.Scan(&tableID, &lk.pkHash, &lk.pk, &lk.sk, &def); err != nil {
			rows.Close()
			return 0, err
		}
		lk.tableID = tableID
		lk.lock = itemLock(lk.pk, lk.sk)
		t, err := unmarshalTable(def)
		if err != nil {
			rows.Close()
			return 0, err
		}
		t.ID = tableID
		pend = append(pend, pending{t: t, lk: lk})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, p := range pend {
		if err := s.expireItem(ctx, p.t, p.lk); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// expireItem deletes one item through the locked path WriteItem uses: take the
// advisory lock, read the current item, then remove the item, its GSI entries
// and write a REMOVE stream record with TTL set.
func (s *Store) expireItem(ctx context.Context, t *storage.Table, lk lockKey) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	old, err := s.readItemLocked(ctx, tx, lk)
	if err != nil {
		return err
	}
	if old == nil {
		return nil
	}
	// The table may have had TTL turned off since the row was selected.
	if !t.TTLEnabled || t.TTLAttr == "" {
		return nil
	}

	b := &pgx.Batch{}
	b.Queue(`DELETE FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3 AND sk = $4`,
		t.ID, lk.pkHash, lk.pk, lk.sk)
	buildGSI(b, t, lk, old, nil)
	buildStreamRecord(b, t, old, nil, true)
	if err := execBatch(ctx, tx, b); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recomputeTTL rewrites items.ttl_at for every item of a table after its
// TTLAttr or TTLEnabled changed, in (pk_hash, pk, sk) order so it can be
// restarted. It mirrors the GSI backfill's batching.
func (s *Store) recomputeTTL(ctx context.Context, tableID int32) {
	defer s.wg.Done()
	t, err := s.tableByID(ctx, tableID)
	if err != nil {
		return
	}
	curHash := int32(math.MinInt32)
	curPK := []byte{}
	curSK := []byte{}
	for {
		if ctx.Err() != nil {
			return
		}
		rows, err := s.pool.Query(ctx, `
			SELECT pk_hash, pk, sk, item
			FROM items
			WHERE table_id = $1 AND (pk_hash, pk, sk) > ($2::int, $3::bytea, $4::bytea)
			ORDER BY pk_hash, pk, sk
			LIMIT 1000`, tableID, curHash, curPK, curSK)
		if err != nil {
			return
		}
		n := 0
		var lastHash int32
		var lastPK, lastSK []byte
		b := &pgx.Batch{}
		for rows.Next() {
			var h int32
			var pk, sk, itemBytes []byte
			if err := rows.Scan(&h, &pk, &sk, &itemBytes); err != nil {
				rows.Close()
				return
			}
			n++
			lastHash, lastPK, lastSK = h, pk, sk
			var it attr.Item
			if err := json.Unmarshal(itemBytes, &it); err != nil {
				continue
			}
			b.Queue(`UPDATE items SET ttl_at = $1 WHERE table_id = $2 AND pk_hash = $3 AND pk = $4 AND sk = $5`,
				ttlAt(t, it), tableID, h, pk, sk)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return
		}
		if b.Len() > 0 {
			br := s.pool.SendBatch(ctx, b)
			for i := 0; i < b.Len(); i++ {
				if _, err := br.Exec(); err != nil {
					br.Close()
					return
				}
			}
			br.Close()
		}
		if n < deleteBatchSize {
			return
		}
		curHash, curPK, curSK = lastHash, lastPK, lastSK
	}
}

// tableByID loads a table definition by its ID.
func (s *Store) tableByID(ctx context.Context, tableID int32) (*storage.Table, error) {
	var def []byte
	if err := s.pool.QueryRow(ctx, `SELECT def FROM ddb_tables WHERE table_id = $1`, tableID).Scan(&def); err != nil {
		return nil, err
	}
	t, err := unmarshalTable(def)
	if err != nil {
		return nil, err
	}
	t.ID = tableID
	return t, nil
}
