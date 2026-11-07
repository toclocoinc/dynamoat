package postgres

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// readChunkSize is how many rows each Query or Scan fetch asks Postgres for.
// The page limits (items and bytes) are applied in Go on top of each chunk, so
// the chunk size only bounds memory, not correctness.
const readChunkSize = 100

// keyBounds is a sort-key range: lo/hi are nil for unbounded, and the
// inclusivity flags say whether the bound itself matches. It applies to the
// base sort key sk or a GSI's isk.
type keyBounds struct {
	lo    []byte
	loInc bool
	hi    []byte
	hiInc bool
}

// Query reads one partition of a table or GSI in sort key order.
func (s *Store) Query(ctx context.Context, in storage.QueryInput) (storage.Page, error) {
	idx, err := resolveIndex(in.Table, in.Index)
	if err != nil {
		return storage.Page{}, err
	}
	if idx == nil {
		return s.queryBase(ctx, in)
	}
	return s.queryIndex(ctx, in, idx)
}

// Scan reads a table or GSI in storage order, split into segments.
func (s *Store) Scan(ctx context.Context, in storage.ScanInput) (storage.Page, error) {
	idx, err := resolveIndex(in.Table, in.Index)
	if err != nil {
		return storage.Page{}, err
	}
	segments := in.Segments
	if segments <= 0 {
		segments = 1
	}
	seg := in.Segment
	if seg < 0 || seg >= segments {
		seg = 0
	}
	lo := scanLo(seg, segments)
	hi := scanLo(seg+1, segments)
	if idx == nil {
		return s.scanBase(ctx, in, lo, hi)
	}
	return s.scanIndex(ctx, in, idx, lo, hi)
}

// resolveIndex returns the named GSI, or nil for the base table.
func resolveIndex(t *storage.Table, name string) (*storage.Index, error) {
	if name == "" {
		return nil, nil
	}
	g, ok := t.GSI(name)
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	return g, nil
}

// scanLo is the Scan segment lower bound, matching storagetest's contract.
func scanLo(i, segments int) int64 {
	return int64(math.MinInt32) + (int64(i)*(int64(1)<<32))/int64(segments)
}

// rangeBounds turns a RangeCond into a key range over the sort key. An empty
// or unknown operator matches every key, mirroring the memstore.
func rangeBounds(rc *storage.RangeCond) (keyBounds, error) {
	var b keyBounds
	if rc == nil {
		return b, nil
	}
	enc := func(v attr.Value) ([]byte, error) { return attr.EncodeKey(v) }
	switch rc.Op {
	case "=":
		if len(rc.Values) == 0 {
			return b, nil
		}
		v, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		b.lo, b.loInc = v, true
		b.hi, b.hiInc = v, true
	case "<":
		if len(rc.Values) == 0 {
			return b, nil
		}
		v, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		b.hi, b.hiInc = v, false
	case "<=":
		if len(rc.Values) == 0 {
			return b, nil
		}
		v, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		b.hi, b.hiInc = v, true
	case ">":
		if len(rc.Values) == 0 {
			return b, nil
		}
		v, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		b.lo, b.loInc = v, false
	case ">=":
		if len(rc.Values) == 0 {
			return b, nil
		}
		v, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		b.lo, b.loInc = v, true
	case "BETWEEN":
		if len(rc.Values) < 2 {
			return b, nil
		}
		v0, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		v1, err := enc(rc.Values[1])
		if err != nil {
			return b, err
		}
		b.lo, b.loInc = v0, true
		b.hi, b.hiInc = v1, true
	case "begins_with":
		if len(rc.Values) == 0 {
			return b, nil
		}
		v, err := enc(rc.Values[0])
		if err != nil {
			return b, err
		}
		b.lo, b.loInc = v, true
		b.hi, b.hiInc = nextPrefix(v), false
	}
	return b, nil
}

// nextPrefix returns the first key that no longer begins with p: p with its
// trailing 0xFF bytes dropped and the last remaining byte incremented. When p
// is all 0xFF there is no such key, so it returns nil.
func nextPrefix(p []byte) []byte {
	q := make([]byte, len(p))
	copy(q, p)
	for len(q) > 0 && q[len(q)-1] == 0xFF {
		q = q[:len(q)-1]
	}
	if len(q) == 0 {
		return nil
	}
	q[len(q)-1]++
	return q
}

func tightenLo(b keyBounds, v []byte, inc bool) keyBounds {
	if b.lo == nil {
		b.lo, b.loInc = v, inc
		return b
	}
	switch bytes.Compare(v, b.lo) {
	case 1:
		b.lo, b.loInc = v, inc
	case 0:
		b.loInc = b.loInc && inc
	}
	return b
}

func tightenHi(b keyBounds, v []byte, inc bool) keyBounds {
	if b.hi == nil {
		b.hi, b.hiInc = v, inc
		return b
	}
	switch bytes.Compare(v, b.hi) {
	case -1:
		b.hi, b.hiInc = v, inc
	case 0:
		b.hiInc = b.hiInc && inc
	}
	return b
}

// appendSkBounds appends the sort-key range predicates for col to sql.
func appendSkBounds(sql string, args []any, col string, b keyBounds) (string, []any) {
	if b.lo != nil {
		args = append(args, b.lo)
		op := ">"
		if b.loInc {
			op = ">="
		}
		sql += fmt.Sprintf(" AND %s %s $%d", col, op, len(args))
	}
	if b.hi != nil {
		args = append(args, b.hi)
		op := "<"
		if b.hiInc {
			op = "<="
		}
		sql += fmt.Sprintf(" AND %s %s $%d", col, op, len(args))
	}
	return sql, args
}

// nonNil keeps empty bytea values as empty slices rather than SQL NULL, which
// would poison row-value comparisons.
func nonNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func (s *Store) queryBase(ctx context.Context, in storage.QueryInput) (storage.Page, error) {
	t := in.Table
	pk, err := attr.EncodeKey(in.Hash)
	if err != nil {
		return storage.Page{}, err
	}
	b, err := rangeBounds(in.Range)
	if err != nil {
		return storage.Page{}, err
	}
	if in.StartAfter != nil {
		if t.Key.Range == "" {
			startPk, err := attr.EncodeKey(in.StartAfter[t.Key.Hash])
			if err != nil {
				return storage.Page{}, err
			}
			// A hash-only partition holds at most one item; if that is the
			// start key it was already returned by the previous page.
			if bytes.Equal(startPk, pk) {
				return storage.Page{}, nil
			}
		} else {
			startSk, err := attr.EncodeKey(in.StartAfter[t.Key.Range])
			if err != nil {
				return storage.Page{}, err
			}
			if in.Forward {
				b = tightenLo(b, startSk, false)
			} else {
				b = tightenHi(b, startSk, false)
			}
		}
	}

	a := &pageAccum{limit: in.Limit}
	var lastSk []byte
	for {
		cur := b
		if lastSk != nil {
			if in.Forward {
				cur = tightenLo(cur, lastSk, false)
			} else {
				cur = tightenHi(cur, lastSk, false)
			}
		}
		sql, args := baseQuerySQL(t, pk, cur, in.Forward)
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return storage.Page{}, err
		}
		n := 0
		for rows.Next() {
			var sk []byte
			var itemBytes []byte
			if err := rows.Scan(&sk, &itemBytes); err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			it, err := decodeItem(itemBytes)
			if err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			lastSk = nonNil(sk)
			n++
			if a.add(it) {
				rows.Close()
				return a.queryPage(t, nil), nil
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return storage.Page{}, err
		}
		if n < readChunkSize {
			return a.queryPage(t, nil), nil
		}
	}
}

func baseQuerySQL(t *storage.Table, pk []byte, b keyBounds, forward bool) (string, []any) {
	sql := "SELECT sk, item FROM items WHERE table_id = $1 AND pk_hash = $2 AND pk = $3"
	args := []any{t.ID, attr.KeyHash(pk), pk}
	sql, args = appendSkBounds(sql, args, "sk", b)
	if forward {
		sql += " ORDER BY sk ASC"
	} else {
		sql += " ORDER BY sk DESC"
	}
	sql += " LIMIT " + strconv.Itoa(readChunkSize)
	return sql, args
}

// queryCursor is a GSI query position: the index sort key and the base table
// key that tie-breaks equal index sort keys.
type queryCursor struct {
	isk []byte
	pk  []byte
	sk  []byte
}

func queryCursorFrom(start attr.Item, t *storage.Table, idx *storage.Index) (queryCursor, error) {
	c := queryCursor{isk: []byte{}, sk: []byte{}}
	var err error
	if idx.Key.Range != "" {
		if c.isk, err = attr.EncodeKey(start[idx.Key.Range]); err != nil {
			return c, err
		}
	}
	if c.pk, err = attr.EncodeKey(start[t.Key.Hash]); err != nil {
		return c, err
	}
	if t.Key.Range != "" {
		if c.sk, err = attr.EncodeKey(start[t.Key.Range]); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (s *Store) queryIndex(ctx context.Context, in storage.QueryInput, idx *storage.Index) (storage.Page, error) {
	t := in.Table
	ipk, err := attr.EncodeKey(in.Hash)
	if err != nil {
		return storage.Page{}, err
	}
	b, err := rangeBounds(in.Range)
	if err != nil {
		return storage.Page{}, err
	}

	a := &pageAccum{limit: in.Limit}
	var cur queryCursor
	hasCur := false
	if in.StartAfter != nil {
		cur, err = queryCursorFrom(in.StartAfter, t, idx)
		if err != nil {
			return storage.Page{}, err
		}
		hasCur = true
	}
	for {
		sql, args := indexQuerySQL(t, idx, ipk, b, in.Forward, hasCur, cur)
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return storage.Page{}, err
		}
		n := 0
		for rows.Next() {
			var itemBytes, isk, pk, sk []byte
			if err := rows.Scan(&itemBytes, &isk, &pk, &sk); err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			it, err := decodeItem(itemBytes)
			if err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			cur = queryCursor{isk: nonNil(isk), pk: nonNil(pk), sk: nonNil(sk)}
			hasCur = true
			n++
			if a.add(it) {
				rows.Close()
				return a.queryPage(t, idx), nil
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return storage.Page{}, err
		}
		if n < readChunkSize {
			return a.queryPage(t, idx), nil
		}
	}
}

func indexQuerySQL(t *storage.Table, idx *storage.Index, ipk []byte, b keyBounds, forward bool, hasCur bool, cur queryCursor) (string, []any) {
	sql := `SELECT i.item, ie.isk, ie.pk, ie.sk FROM index_entries ie JOIN items i ON i.table_id = ie.table_id AND i.pk_hash = ie.pk_hash AND i.pk = ie.pk AND i.sk = ie.sk WHERE ie.table_id = $1 AND ie.index_id = $2 AND ie.ipk_hash = $3 AND ie.ipk = $4`
	args := []any{t.ID, idx.ID, attr.KeyHash(ipk), ipk}
	sql, args = appendSkBounds(sql, args, "ie.isk", b)
	if hasCur {
		args = append(args, cur.isk, cur.pk, cur.sk)
		n := len(args)
		op := ">"
		if !forward {
			op = "<"
		}
		sql += fmt.Sprintf(" AND (ie.isk, ie.pk, ie.sk) %s ($%d::bytea, $%d::bytea, $%d::bytea)", op, n-2, n-1, n)
	}
	dir := "ASC"
	if !forward {
		dir = "DESC"
	}
	sql += fmt.Sprintf(" ORDER BY ie.isk %s, ie.pk %s, ie.sk %s LIMIT %d", dir, dir, dir, readChunkSize)
	return sql, args
}

type scanBaseCursor struct {
	pkHash int32
	pk     []byte
	sk     []byte
}

func (s *Store) scanBase(ctx context.Context, in storage.ScanInput, lo, hi int64) (storage.Page, error) {
	t := in.Table
	a := &pageAccum{limit: in.Limit}
	var cur scanBaseCursor
	hasCur := false
	if in.StartAfter != nil {
		pk, err := attr.EncodeKey(in.StartAfter[t.Key.Hash])
		if err != nil {
			return storage.Page{}, err
		}
		sk := []byte{}
		if t.Key.Range != "" {
			if sk, err = attr.EncodeKey(in.StartAfter[t.Key.Range]); err != nil {
				return storage.Page{}, err
			}
		}
		cur = scanBaseCursor{pkHash: attr.KeyHash(pk), pk: pk, sk: sk}
		hasCur = true
	}
	for {
		sql, args := scanBaseSQL(t, lo, hi, hasCur, cur)
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return storage.Page{}, err
		}
		n := 0
		for rows.Next() {
			var h int32
			var pk, sk []byte
			var itemBytes []byte
			if err := rows.Scan(&h, &pk, &sk, &itemBytes); err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			it, err := decodeItem(itemBytes)
			if err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			cur = scanBaseCursor{pkHash: h, pk: nonNil(pk), sk: nonNil(sk)}
			hasCur = true
			n++
			if a.add(it) {
				rows.Close()
				return a.page(t, nil), nil
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return storage.Page{}, err
		}
		if n < readChunkSize {
			return a.page(t, nil), nil
		}
	}
}

func scanBaseSQL(t *storage.Table, lo, hi int64, hasCur bool, cur scanBaseCursor) (string, []any) {
	sql := "SELECT pk_hash, pk, sk, item FROM items WHERE table_id = $1"
	args := []any{t.ID}
	args = append(args, int32(lo))
	sql += fmt.Sprintf(" AND pk_hash >= $%d", len(args))
	if hi <= int64(math.MaxInt32) {
		args = append(args, int32(hi))
		sql += fmt.Sprintf(" AND pk_hash < $%d", len(args))
	}
	if hasCur {
		args = append(args, cur.pkHash, cur.pk, cur.sk)
		n := len(args)
		sql += fmt.Sprintf(" AND (pk_hash, pk, sk) > ($%d::int, $%d::bytea, $%d::bytea)", n-2, n-1, n)
	}
	sql += " ORDER BY pk_hash, pk, sk LIMIT " + strconv.Itoa(readChunkSize)
	return sql, args
}

type scanIndexCursor struct {
	ipkHash int32
	ipk     []byte
	isk     []byte
	pk      []byte
	sk      []byte
}

func (s *Store) scanIndex(ctx context.Context, in storage.ScanInput, idx *storage.Index, lo, hi int64) (storage.Page, error) {
	t := in.Table
	a := &pageAccum{limit: in.Limit}
	var cur scanIndexCursor
	hasCur := false
	if in.StartAfter != nil {
		ipk, err := attr.EncodeKey(in.StartAfter[idx.Key.Hash])
		if err != nil {
			return storage.Page{}, err
		}
		isk := []byte{}
		if idx.Key.Range != "" {
			if isk, err = attr.EncodeKey(in.StartAfter[idx.Key.Range]); err != nil {
				return storage.Page{}, err
			}
		}
		pk, err := attr.EncodeKey(in.StartAfter[t.Key.Hash])
		if err != nil {
			return storage.Page{}, err
		}
		sk := []byte{}
		if t.Key.Range != "" {
			if sk, err = attr.EncodeKey(in.StartAfter[t.Key.Range]); err != nil {
				return storage.Page{}, err
			}
		}
		cur = scanIndexCursor{ipkHash: attr.KeyHash(ipk), ipk: ipk, isk: isk, pk: pk, sk: sk}
		hasCur = true
	}
	for {
		sql, args := scanIndexSQL(t, idx, lo, hi, hasCur, cur)
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return storage.Page{}, err
		}
		n := 0
		for rows.Next() {
			var ih int32
			var itemBytes, ipk, isk, pk, sk []byte
			if err := rows.Scan(&ih, &itemBytes, &ipk, &isk, &pk, &sk); err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			it, err := decodeItem(itemBytes)
			if err != nil {
				rows.Close()
				return storage.Page{}, err
			}
			cur = scanIndexCursor{ipkHash: ih, ipk: nonNil(ipk), isk: nonNil(isk), pk: nonNil(pk), sk: nonNil(sk)}
			hasCur = true
			n++
			if a.add(it) {
				rows.Close()
				return a.page(t, idx), nil
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return storage.Page{}, err
		}
		if n < readChunkSize {
			return a.page(t, idx), nil
		}
	}
}

func scanIndexSQL(t *storage.Table, idx *storage.Index, lo, hi int64, hasCur bool, cur scanIndexCursor) (string, []any) {
	sql := `SELECT i.item, ie.ipk_hash, ie.ipk, ie.isk, ie.pk, ie.sk FROM index_entries ie JOIN items i ON i.table_id = ie.table_id AND i.pk_hash = ie.pk_hash AND i.pk = ie.pk AND i.sk = ie.sk WHERE ie.table_id = $1 AND ie.index_id = $2`
	args := []any{t.ID, idx.ID}
	args = append(args, int32(lo))
	sql += fmt.Sprintf(" AND ie.ipk_hash >= $%d", len(args))
	if hi <= int64(math.MaxInt32) {
		args = append(args, int32(hi))
		sql += fmt.Sprintf(" AND ie.ipk_hash < $%d", len(args))
	}
	if hasCur {
		args = append(args, cur.ipkHash, cur.ipk, cur.isk, cur.pk, cur.sk)
		n := len(args)
		sql += fmt.Sprintf(" AND (ie.ipk_hash, ie.ipk, ie.isk, ie.pk, ie.sk) > ($%d::int, $%d::bytea, $%d::bytea, $%d::bytea, $%d::bytea)", n-4, n-3, n-2, n-1, n)
	}
	sql += " ORDER BY ie.ipk_hash, ie.ipk, ie.isk, ie.pk, ie.sk LIMIT " + strconv.Itoa(readChunkSize)
	return sql, args
}

// lastKeyOf builds the LastEvaluatedKey for an item: the table key, plus the
// index key for a GSI.
func lastKeyOf(t *storage.Table, idx *storage.Index, it attr.Item) attr.Item {
	out := attr.Item{}
	if idx != nil {
		out[idx.Key.Hash] = it[idx.Key.Hash]
		if idx.Key.Range != "" {
			out[idx.Key.Range] = it[idx.Key.Range]
		}
	}
	out[t.Key.Hash] = it[t.Key.Hash]
	if t.Key.Range != "" {
		out[t.Key.Range] = it[t.Key.Range]
	}
	return out
}

// pageAccum applies the page limits to a stream of items and remembers the
// last item added, which becomes LastKey when a limit truncates the page.
type pageAccum struct {
	limit          storage.PageLimit
	items          []attr.Item
	last           attr.Item
	running        int
	bytesTruncated bool
	// hasNext records that a real in-range item was rejected by a limit, so at
	// least one more item exists after the page. Query keys LastKey off this;
	// Scan uses truncated().
	hasNext bool
}

// add reports whether the page is full; a true result means it was not added.
func (a *pageAccum) add(it attr.Item) bool {
	if a.limit.Items > 0 && len(a.items) >= a.limit.Items {
		a.hasNext = true
		return true
	}
	size := attr.ItemSize(it)
	if a.limit.Bytes > 0 && len(a.items) > 0 && a.running+size > a.limit.Bytes {
		a.bytesTruncated = true
		a.hasNext = true
		return true
	}
	a.items = append(a.items, it)
	a.running += size
	a.last = it
	return false
}

func (a *pageAccum) truncated() bool {
	return a.bytesTruncated || (a.limit.Items > 0 && len(a.items) >= a.limit.Items)
}

func (a *pageAccum) page(t *storage.Table, idx *storage.Index) storage.Page {
	if a.truncated() && len(a.items) > 0 {
		return storage.Page{Items: a.items, LastKey: lastKeyOf(t, idx, a.last)}
	}
	return storage.Page{Items: a.items}
}

// queryPage is Query's page: LastKey only when a real next item was rejected,
// so the final page of a partition carries no LastEvaluatedKey.
func (a *pageAccum) queryPage(t *storage.Table, idx *storage.Index) storage.Page {
	if a.hasNext && len(a.items) > 0 {
		return storage.Page{Items: a.items, LastKey: lastKeyOf(t, idx, a.last)}
	}
	return storage.Page{Items: a.items}
}
