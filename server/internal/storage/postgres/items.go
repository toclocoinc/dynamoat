package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// lockKey is the encoded primary key plus the values the advisory lock and the
// items row are keyed on.
type lockKey struct {
	tableID int32
	pkHash  int32
	pk      []byte
	sk      []byte
	lock    int32 // the item's advisory-lock key, itemLock(pk, sk)
}

// itemLock is the advisory-lock key for one item: the first four bytes of
// SHA-256 over len(pk) (big-endian), pk and sk, read big-endian as a signed
// int32. Locks are per item, not per partition, so writes to different items
// in one partition do not wait for each other.
func itemLock(pk, sk []byte) int32 {
	h := sha256.New()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(pk)))
	h.Write(n[:])
	h.Write(pk)
	h.Write(sk)
	sum := h.Sum(nil)
	return int32(binary.BigEndian.Uint32(sum[:4]))
}

// keyLock encodes a Key. sk is empty (not NULL) for hash-only tables, matching
// the items.sk column.
func keyLock(k storage.Key) (lockKey, error) {
	pk, err := attr.EncodeKey(k.Hash)
	if err != nil {
		return lockKey{}, err
	}
	sk := []byte{}
	if k.Range.Type != 0 {
		if sk, err = attr.EncodeKey(k.Range); err != nil {
			return lockKey{}, err
		}
	}
	return lockKey{pkHash: attr.KeyHash(pk), pk: pk, sk: sk, lock: itemLock(pk, sk)}, nil
}

// lockLess orders locks by (table_id, item lock), the order Transact acquires
// them, so two transactions over the same items cannot deadlock.
func lockLess(a, b lockKey) bool {
	if a.tableID != b.tableID {
		return a.tableID < b.tableID
	}
	return a.lock < b.lock
}

// transactWrite is one write in a Transact call, with its lock key computed.
type transactWrite struct {
	w  storage.Write
	lk lockKey
}

// isLockErr reports whether err is Postgres SQLSTATE 55P03 (lock_not_available,
// lock_timeout) or 40P01 (deadlock).
func isLockErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01")
}

// mapWriteErr turns lock wait and deadlock errors into storage.ErrThrottled,
// which the ops layer reports as ProvisionedThroughputExceededException.
func mapWriteErr(err error) error {
	if isLockErr(err) {
		return storage.ErrThrottled
	}
	return err
}

// GetItem returns the item, or nil when it does not exist.
func (s *Store) GetItem(ctx context.Context, t *storage.Table, k storage.Key) (attr.Item, error) {
	lk, err := keyLock(k)
	if err != nil {
		return nil, err
	}
	lk.tableID = t.ID
	var itemBytes []byte
	err = s.pool.QueryRow(ctx, `SELECT item FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3 AND sk = $4`,
		lk.tableID, lk.pkHash, lk.pk, lk.sk).Scan(&itemBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeItem(itemBytes)
}

// WriteItem runs one write atomically and returns the item before and after it.
func (s *Store) WriteItem(ctx context.Context, w storage.Write) (old, new attr.Item, err error) {
	lk, err := keyLock(w.Key)
	if err != nil {
		return nil, nil, err
	}
	lk.tableID = w.Table.ID

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	old, err = s.readItemLocked(ctx, tx, lk)
	if err != nil {
		return nil, nil, mapWriteErr(err)
	}

	res, err := w.Fn(old)
	if err != nil {
		return nil, nil, err
	}

	b := &pgx.Batch{}
	if err := buildApply(b, w.Table, lk, old, res); err != nil {
		return nil, nil, err
	}
	if b.Len() > 0 {
		if err := execBatch(ctx, tx, b); err != nil {
			return nil, nil, mapWriteErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, mapWriteErr(err)
	}
	return resultItems(old, res)
}

// Transact runs every write in one transaction, taking the item locks in a
// fixed order so concurrent transactions cannot deadlock. If any WriteFunc
// returns an error nothing is written and the error is wrapped in a *TxError.
func (s *Store) Transact(ctx context.Context, writes []storage.Write) error {
	ws := make([]transactWrite, len(writes))
	for i, w := range writes {
		lk, err := keyLock(w.Key)
		if err != nil {
			return &storage.TxError{Index: i, Err: err}
		}
		lk.tableID = w.Table.ID
		ws[i] = transactWrite{w: w, lk: lk}
	}
	order := make([]int, len(ws))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return lockLess(ws[order[a]].lk, ws[order[b]].lk) })

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if i, err := s.lockItems(ctx, tx, ws, order); err != nil {
		return &storage.TxError{Index: i, Err: mapWriteErr(err)}
	}

	for i := range ws {
		old, err := readItem(ctx, tx, ws[i].lk)
		if err != nil {
			return &storage.TxError{Index: i, Err: mapWriteErr(err)}
		}
		res, err := ws[i].w.Fn(old)
		if err != nil {
			return &storage.TxError{Index: i, Err: err}
		}
		b := &pgx.Batch{}
		if err := buildApply(b, ws[i].w.Table, ws[i].lk, old, res); err != nil {
			return &storage.TxError{Index: i, Err: err}
		}
		if b.Len() > 0 {
			if err := execBatch(ctx, tx, b); err != nil {
				return &storage.TxError{Index: i, Err: mapWriteErr(err)}
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if isLockErr(err) {
			return &storage.TxError{Index: 0, Err: storage.ErrThrottled}
		}
		return err
	}
	return nil
}

// lockItems runs SET LOCAL lock_timeout and takes every write's advisory lock
// in lock order, skipping repeated (table_id, item lock) pairs, in one batch.
// It returns the index of the write whose lock failed, or 0 for the timeout
// statement itself.
func (s *Store) lockItems(ctx context.Context, tx pgx.Tx, ws []transactWrite, order []int) (int, error) {
	b := &pgx.Batch{}
	b.Queue(s.setLockTimeoutSQL())
	var lockIdx []int
	var last lockKey
	for _, i := range order {
		lk := ws[i].lk
		if len(lockIdx) > 0 && last.tableID == lk.tableID && last.lock == lk.lock {
			continue
		}
		last = lk
		lockIdx = append(lockIdx, i)
		b.Queue(`SELECT pg_advisory_xact_lock($1::int, $2::int)`, lk.tableID, lk.lock)
	}
	br := tx.SendBatch(ctx, b)
	defer br.Close()
	if _, err := br.Exec(); err != nil { // SET LOCAL
		return 0, err
	}
	for _, i := range lockIdx {
		if _, err := br.Exec(); err != nil {
			return i, err
		}
	}
	return 0, nil
}

// readItemLocked sets the transaction's lock_timeout, takes the item's advisory
// lock and reads the current item in one batch, so a single write still costs
// about two round trips for its read and apply phases.
func (s *Store) readItemLocked(ctx context.Context, tx pgx.Tx, lk lockKey) (attr.Item, error) {
	b := &pgx.Batch{}
	b.Queue(s.setLockTimeoutSQL())
	b.Queue(`SELECT pg_advisory_xact_lock($1::int, $2::int)`, lk.tableID, lk.lock)
	b.Queue(`SELECT item FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3 AND sk = $4 FOR UPDATE`,
		lk.tableID, lk.pkHash, lk.pk, lk.sk)
	br := tx.SendBatch(ctx, b)
	defer br.Close()
	if _, err := br.Exec(); err != nil { // SET LOCAL lock_timeout
		return nil, err
	}
	if _, err := br.Exec(); err != nil { // the advisory lock's void result
		return nil, err
	}
	var itemBytes []byte
	err := br.QueryRow().Scan(&itemBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeItem(itemBytes)
}

// readItem reads the current item inside tx, locking the row.
func readItem(ctx context.Context, tx pgx.Tx, lk lockKey) (attr.Item, error) {
	var itemBytes []byte
	err := tx.QueryRow(ctx, `SELECT item FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3 AND sk = $4 FOR UPDATE`,
		lk.tableID, lk.pkHash, lk.pk, lk.sk).Scan(&itemBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeItem(itemBytes)
}

func decodeItem(b []byte) (attr.Item, error) {
	var it attr.Item
	if err := json.Unmarshal(b, &it); err != nil {
		return nil, err
	}
	return it, nil
}

// buildApply appends the statements for one write's item, GSI entries and
// stream record to b. It returns an error only for invalid input.
func buildApply(b *pgx.Batch, t *storage.Table, lk lockKey, old attr.Item, res storage.WriteResult) error {
	switch res.Op {
	case storage.Keep:
		return nil
	case storage.Put:
		itemJSON, err := json.Marshal(res.Item)
		if err != nil {
			return err
		}
		b.Queue(`INSERT INTO items (table_id, pk_hash, pk, sk, item, ttl_at)
		         VALUES ($1, $2, $3, $4, $5, $6)
		         ON CONFLICT (table_id, pk_hash, pk, sk)
		         DO UPDATE SET item = EXCLUDED.item, ttl_at = EXCLUDED.ttl_at`,
			t.ID, lk.pkHash, lk.pk, lk.sk, itemJSON, ttlAt(t, res.Item))
		buildGSI(b, t, lk, old, res.Item)
		buildStream(b, t, old, res.Item)
		buildHistory(b, t, lk, res.Item)
		return nil
	case storage.Delete:
		b.Queue(`DELETE FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3 AND sk = $4`,
			t.ID, lk.pkHash, lk.pk, lk.sk)
		buildGSI(b, t, lk, old, nil)
		buildStream(b, t, old, nil)
		buildHistory(b, t, lk, nil)
		return nil
	}
	return storage.ErrTableNotFound
}

// buildGSI diffs the old and new index keys of each GSI, deleting the old
// entry and inserting the new one only when they differ. An item is a member
// of a GSI only when it has both index key attributes with the declared types.
func buildGSI(b *pgx.Batch, t *storage.Table, lk lockKey, old, new attr.Item) {
	for _, g := range t.GSIs {
		oh, opk, osk, ook := indexKey(g, old)
		nh, npk, nsk, nok := indexKey(g, new)
		if !ook && !nok {
			continue
		}
		if ook && nok && oh == nh && bytes.Equal(opk, npk) && bytes.Equal(osk, nsk) {
			continue
		}
		if ook {
			b.Queue(`DELETE FROM index_entries WHERE table_id = $1 AND index_id = $2 AND ipk_hash = $3 AND ipk = $4 AND isk = $5 AND pk = $6 AND sk = $7`,
				t.ID, g.ID, oh, opk, osk, lk.pk, lk.sk)
		}
		if nok {
			b.Queue(`INSERT INTO index_entries (table_id, index_id, ipk_hash, ipk, isk, pk_hash, pk, sk) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
				t.ID, g.ID, nh, npk, nsk, lk.pkHash, lk.pk, lk.sk)
		}
	}
}

func indexKey(g storage.Index, it attr.Item) (h int32, pk, sk []byte, ok bool) {
	if it == nil {
		return 0, nil, nil, false
	}
	hv, present := it[g.Key.Hash]
	if !present || hv.Type != g.Key.HashType {
		return 0, nil, nil, false
	}
	ipk, err := attr.EncodeKey(hv)
	if err != nil {
		return 0, nil, nil, false
	}
	sk = []byte{}
	if g.Key.Range != "" {
		rv, present := it[g.Key.Range]
		if !present || rv.Type != g.Key.RangeType {
			return 0, nil, nil, false
		}
		isk, err := attr.EncodeKey(rv)
		if err != nil {
			return 0, nil, nil, false
		}
		sk = isk
	}
	return attr.KeyHash(ipk), ipk, sk, true
}

// buildStream writes one stream record when the table has streams on. There is
// no record for Keep, and none for a Put that does not change the item.
func buildStream(b *pgx.Batch, t *storage.Table, old, new attr.Item) {
	buildStreamRecord(b, t, old, new, false)
}

// buildStreamRecord writes one stream record. ttl marks a REMOVE made by TTL
// expiry (buildStream passes false; ExpireItems passes true).
func buildStreamRecord(b *pgx.Batch, t *storage.Table, old, new attr.Item, ttl bool) {
	if t.StreamView == "" {
		return
	}
	ev := streamEvent(old, new)
	if ev == "" {
		return
	}
	keys := storage.KeyItem(t.Key, keySource(old, new))
	keysJSON, _ := json.Marshal(keys)
	size := attr.ItemSize(keys)

	var oldJSON, newJSON []byte
	if streamWantsOld(t.StreamView) && old != nil {
		oldJSON, _ = json.Marshal(old)
		size += attr.ItemSize(old)
	}
	if streamWantsNew(t.StreamView) && new != nil {
		newJSON, _ = json.Marshal(new)
		size += attr.ItemSize(new)
	}

	b.Queue(`INSERT INTO stream_records (table_id, event, keys, old_image, new_image, size, ttl) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		t.ID, ev, keysJSON, oldJSON, newJSON, size, ttl)
}

func streamEvent(old, new attr.Item) string {
	switch {
	case old == nil && new != nil:
		return "INSERT"
	case old != nil && new == nil:
		return "REMOVE"
	case old != nil && new != nil:
		if reflect.DeepEqual(old, new) {
			return ""
		}
		return "MODIFY"
	}
	return ""
}

func streamWantsOld(view string) bool {
	return view == storage.StreamOldImage || view == storage.StreamNewAndOld
}

func streamWantsNew(view string) bool {
	return view == storage.StreamNewImage || view == storage.StreamNewAndOld
}

func keySource(old, new attr.Item) attr.Item {
	if new != nil {
		return new
	}
	return old
}

// ttlAt is the TTL timestamp for an item: the TTL attribute's epoch seconds
// when TTL is on and the attribute is an N, else NULL.
func ttlAt(t *storage.Table, it attr.Item) *time.Time {
	if !t.TTLEnabled || t.TTLAttr == "" || it == nil {
		return nil
	}
	v, ok := it[t.TTLAttr]
	if !ok || v.Type != attr.TypeN {
		return nil
	}
	sec, err := strconv.ParseInt(v.N, 10, 64)
	if err != nil {
		return nil
	}
	ts := time.Unix(sec, 0)
	return &ts
}

func resultItems(old attr.Item, res storage.WriteResult) (attr.Item, attr.Item, error) {
	switch res.Op {
	case storage.Keep:
		return old, old, nil
	case storage.Put:
		return old, res.Item, nil
	case storage.Delete:
		return old, nil, nil
	}
	return nil, nil, storage.ErrTableNotFound
}

func execBatch(ctx context.Context, tx pgx.Tx, b *pgx.Batch) error {
	br := tx.SendBatch(ctx, b)
	defer br.Close()
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}
