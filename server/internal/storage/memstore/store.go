// Package memstore is an in-memory storage.Store for operation tests and
// local development. It trades speed for simplicity: one mutex guards the
// whole store, and Query and Scan gather every item of a table on each call.
// It is correct: it passes storagetest.Run.
package memstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// Store is an in-memory storage backend.
type Store struct {
	mu          sync.Mutex
	tables      map[string]*table
	names       []string // table names, byte order
	nextID      int32
	nextSeq     int64 // store-wide stream sequence number
	checkpoints map[cpKey]int64
	leases      map[string]lease
	backups     map[string]*memBackup
}

type table struct {
	def      storage.Table
	items    map[itemKey]attr.Item
	stream   []storage.StreamRecord
	history  map[itemKey][]histVersion
	pitrDone bool
}

// memBackup is one on-demand backup: its metadata and a deep copy of the items
// as of the snapshot, kept after the source table is deleted.
type memBackup struct {
	def   storage.Backup
	items []attr.Item
}

// cpKey identifies one consumer's checkpoint on one table.
type cpKey struct {
	consumer string
	tableID  int32
}

// lease is a named background-job lease held by one owner until expiresAt.
type lease struct {
	owner     string
	expiresAt time.Time
}

// itemKey is the encoded primary key of an item. The fields hold the output of
// attr.EncodeKey, so string comparison matches DynamoDB key order.
type itemKey struct {
	pk string
	sk string
}

// New returns an empty store.
func New() *Store {
	return &Store{
		tables:      make(map[string]*table),
		nextID:      1,
		nextSeq:     1,
		checkpoints: make(map[cpKey]int64),
		leases:      make(map[string]lease),
		backups:     make(map[string]*memBackup),
	}
}

// Ready reports whether the store can serve requests.
func (s *Store) Ready(ctx context.Context) error { return nil }

// CreateTable stores a new table, assigning the table ID and GSI IDs.
func (s *Store) CreateTable(ctx context.Context, t *storage.Table) (*storage.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tables[t.Name]; ok {
		return nil, storage.ErrTableExists
	}
	c := cloneTable(t)
	c.ID = s.nextID
	s.nextID++
	assignGSIIDs(c)
	c.Status = storage.StatusActive
	for i := range c.GSIs {
		c.GSIs[i].Status = storage.StatusActive
	}
	s.tables[c.Name] = &table{def: *cloneTable(c), items: make(map[itemKey]attr.Item)}
	s.names = append(s.names, c.Name)
	sort.Strings(s.names)
	return cloneTable(c), nil
}

// GetTable returns the table definition, or ErrTableNotFound.
func (s *Store) GetTable(ctx context.Context, name string) (*storage.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	return cloneTable(&st.def), nil
}

// ListTables returns up to limit names in byte order, after startAfter.
func (s *Store) ListTables(ctx context.Context, startAfter string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range s.names {
		if n <= startAfter {
			continue
		}
		out = append(out, n)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// UpdateTable applies fn to a copy of the current definition; an error from fn
// changes nothing. A new GSI (ID 0) is assigned an ID and backfilled
// synchronously, then set ACTIVE.
func (s *Store) UpdateTable(ctx context.Context, name string, fn func(*storage.Table) error) (*storage.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	draft := cloneTable(&st.def)
	if err := fn(draft); err != nil {
		return nil, err
	}
	wasPITR := st.def.PITREnabled
	assignGSIIDs(draft)
	for i := range draft.GSIs {
		if draft.GSIs[i].Status == "" || draft.GSIs[i].Status == storage.StatusCreating {
			draft.GSIs[i].Status = storage.StatusActive
		}
	}
	st.def = *cloneTable(draft)
	if !wasPITR && st.def.PITREnabled {
		s.pitrEnableLocked(st)
	}
	return cloneTable(&st.def), nil
}

// DeleteTable removes the table at once and returns its definition, marked
// DELETING, so the name can be reused immediately.
func (s *Store) DeleteTable(ctx context.Context, name string) (*storage.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	def := cloneTable(&st.def)
	def.Status = storage.StatusDeleting
	delete(s.tables, name)
	for i, n := range s.names {
		if n == name {
			s.names = append(s.names[:i], s.names[i+1:]...)
			break
		}
	}
	return def, nil
}

// GetItem returns the item, or nil when it does not exist.
func (s *Store) GetItem(ctx context.Context, t *storage.Table, k storage.Key) (attr.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[t.Name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	key, err := keyFor(k)
	if err != nil {
		return nil, err
	}
	it, ok := st.items[key]
	if !ok {
		return nil, nil
	}
	return cloneItem(it), nil
}

// WriteItem runs one write atomically and returns the item before and after it.
func (s *Store) WriteItem(ctx context.Context, w storage.Write) (attr.Item, attr.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[w.Table.Name]
	if !ok {
		return nil, nil, storage.ErrTableNotFound
	}
	key, err := keyFor(w.Key)
	if err != nil {
		return nil, nil, err
	}
	cur := st.items[key]
	var oldClone attr.Item
	if cur != nil {
		oldClone = cloneItem(cur)
	}
	res, err := w.Fn(oldClone)
	if err != nil {
		return nil, nil, err
	}
	switch res.Op {
	case storage.Keep:
		if cur == nil {
			return nil, nil, nil
		}
		return cloneItem(cur), cloneItem(cur), nil
	case storage.Put:
		item := cloneItem(res.Item)
		st.items[key] = item
		s.appendStream(st, cur, item, false)
		s.appendHistory(st, key, item)
		return cloneOrNil(cur), cloneItem(item), nil
	case storage.Delete:
		delete(st.items, key)
		s.appendStream(st, cur, nil, false)
		s.appendHistory(st, key, nil)
		return cloneOrNil(cur), nil, nil
	}
	return nil, nil, storage.ErrTableNotFound
}

// Transact runs every write in one transaction. Every WriteFunc sees the
// pre-transaction state; if any returns an error nothing is written and the
// error is wrapped in a *TxError.
func (s *Store) Transact(ctx context.Context, writes []storage.Write) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	type pending struct {
		key itemKey
		op  storage.WriteOp
		old attr.Item
		it  attr.Item
	}
	pend := make([]pending, 0, len(writes))
	for i, w := range writes {
		st, ok := s.tables[w.Table.Name]
		if !ok {
			return &storage.TxError{Index: i, Err: storage.ErrTableNotFound}
		}
		key, err := keyFor(w.Key)
		if err != nil {
			return &storage.TxError{Index: i, Err: err}
		}
		cur := st.items[key]
		var oldClone attr.Item
		if cur != nil {
			oldClone = cloneItem(cur)
		}
		res, err := w.Fn(oldClone)
		if err != nil {
			return &storage.TxError{Index: i, Err: err}
		}
		pend = append(pend, pending{key: key, op: res.Op, old: oldClone, it: cloneItem(res.Item)})
	}
	for i, p := range pend {
		st := s.tables[writes[i].Table.Name]
		switch p.op {
		case storage.Keep:
		case storage.Put:
			st.items[p.key] = cloneItem(p.it)
			s.appendStream(st, p.old, p.it, false)
			s.appendHistory(st, p.key, p.it)
		case storage.Delete:
			delete(st.items, p.key)
			s.appendStream(st, p.old, nil, false)
			s.appendHistory(st, p.key, nil)
		}
	}
	return nil
}

// Query reads one partition of a table or GSI in sort key order.
func (s *Store) Query(ctx context.Context, in storage.QueryInput) (storage.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.index(in.Table, in.Index)
	if err != nil {
		return storage.Page{}, err
	}
	rows, err := s.rows(in.Table, idx)
	if err != nil {
		return storage.Page{}, err
	}
	hashEnc, err := attr.EncodeKey(in.Hash)
	if err != nil {
		return storage.Page{}, err
	}
	want := string(hashEnc)
	var out []row
	for _, r := range rows {
		if idx == nil {
			if r.pk != want {
				continue
			}
		} else if r.ipk != want {
			continue
		}
		if !matchRange(in.Range, sortKey(r, idx)) {
			continue
		}
		out = append(out, r)
	}
	sortRows(out, idx, in.Forward)
	out = skipQueryAfter(out, in.StartAfter, in.Table, idx, in.Forward)
	return paginateQuery(out, in.Table, idx, in.Limit), nil
}

// Scan reads a table or GSI in storage order, split into segments.
func (s *Store) Scan(ctx context.Context, in storage.ScanInput) (storage.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.index(in.Table, in.Index)
	if err != nil {
		return storage.Page{}, err
	}
	rows, err := s.rows(in.Table, idx)
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
	var out []row
	for _, r := range rows {
		if inSegment(int64(attr.KeyHash(scanPK(r, idx))), seg, segments) {
			out = append(out, r)
		}
	}
	sortScanRows(out, idx)
	out = skipScanAfter(out, in.StartAfter, in.Table, idx)
	return paginate(out, in.Table, idx, in.Limit), nil
}

// index resolves the named GSI, or nil for the base table.
func (s *Store) index(t *storage.Table, name string) (*storage.Index, error) {
	if name == "" {
		return nil, nil
	}
	g, ok := t.GSI(name)
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	return g, nil
}

// row is an item together with its encoded base and (optionally) index keys.
type row struct {
	item     attr.Item
	pk, sk   string // base key, encoded
	ipk, isk string // index key, encoded (empty for the base table)
}

// rows gathers every item of the table that is a member of idx (a sparse GSI
// holds only items with both index key attributes of the declared types).
func (s *Store) rows(t *storage.Table, idx *storage.Index) ([]row, error) {
	st, ok := s.tables[t.Name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	rows := make([]row, 0, len(st.items))
	for _, it := range st.items {
		pk, err := attr.EncodeKey(it[t.Key.Hash])
		if err != nil {
			continue
		}
		r := row{item: it, pk: string(pk)}
		if t.Key.Range != "" {
			sk, err := attr.EncodeKey(it[t.Key.Range])
			if err != nil {
				continue
			}
			r.sk = string(sk)
		}
		if idx != nil {
			hv, ok := it[idx.Key.Hash]
			if !ok || hv.Type != idx.Key.HashType {
				continue
			}
			ipk, err := attr.EncodeKey(hv)
			if err != nil {
				continue
			}
			r.ipk = string(ipk)
			if idx.Key.Range != "" {
				rv, ok := it[idx.Key.Range]
				if !ok || rv.Type != idx.Key.RangeType {
					continue
				}
				isk, err := attr.EncodeKey(rv)
				if err != nil {
					continue
				}
				r.isk = string(isk)
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// sortKey is the value a RangeCond applies to: the base sort key, or the
// index sort key for a GSI query.
func sortKey(r row, idx *storage.Index) []byte {
	if idx == nil {
		return []byte(r.sk)
	}
	return []byte(r.isk)
}

// scanPK is the partition key whose hash leads Scan order.
func scanPK(r row, idx *storage.Index) []byte {
	if idx == nil {
		return []byte(r.pk)
	}
	return []byte(r.ipk)
}

func matchRange(rc *storage.RangeCond, key []byte) bool {
	if rc == nil {
		return true
	}
	switch rc.Op {
	case "=":
		return len(rc.Values) > 0 && bytes.Equal(key, enc(rc.Values[0]))
	case "<":
		return len(rc.Values) > 0 && bytes.Compare(key, enc(rc.Values[0])) < 0
	case "<=":
		return len(rc.Values) > 0 && bytes.Compare(key, enc(rc.Values[0])) <= 0
	case ">":
		return len(rc.Values) > 0 && bytes.Compare(key, enc(rc.Values[0])) > 0
	case ">=":
		return len(rc.Values) > 0 && bytes.Compare(key, enc(rc.Values[0])) >= 0
	case "BETWEEN":
		return len(rc.Values) > 1 && bytes.Compare(key, enc(rc.Values[0])) >= 0 && bytes.Compare(key, enc(rc.Values[1])) <= 0
	case "begins_with":
		return len(rc.Values) > 0 && bytes.HasPrefix(key, enc(rc.Values[0]))
	}
	return true
}

func enc(v attr.Value) []byte {
	b, _ := attr.EncodeKey(v)
	return b
}

func sortRows(rows []row, idx *storage.Index, forward bool) {
	sort.Slice(rows, func(i, j int) bool {
		c := cmpQuery(rows[i], rows[j], idx)
		if forward {
			return c < 0
		}
		return c > 0
	})
}

// cmpQuery orders rows within one partition: index sort key then base key for
// a GSI, base sort key for the base table.
func cmpQuery(a, b row, idx *storage.Index) int {
	if idx == nil {
		if c := strings.Compare(a.pk, b.pk); c != 0 {
			return c
		}
		return strings.Compare(a.sk, b.sk)
	}
	if c := strings.Compare(a.ipk, b.ipk); c != 0 {
		return c
	}
	if c := strings.Compare(a.isk, b.isk); c != 0 {
		return c
	}
	if c := strings.Compare(a.pk, b.pk); c != 0 {
		return c
	}
	return strings.Compare(a.sk, b.sk)
}

func sortScanRows(rows []row, idx *storage.Index) {
	sort.Slice(rows, func(i, j int) bool { return cmpScan(rows[i], rows[j], idx) < 0 })
}

// cmpScan orders rows in Scan order: pk_hash (signed int32), then encoded
// partition key, then encoded sort key.
func cmpScan(a, b row, idx *storage.Index) int {
	ah := attr.KeyHash(scanPK(a, idx))
	bh := attr.KeyHash(scanPK(b, idx))
	if ah != bh {
		if ah < bh {
			return -1
		}
		return 1
	}
	if idx == nil {
		if c := strings.Compare(a.pk, b.pk); c != 0 {
			return c
		}
		return strings.Compare(a.sk, b.sk)
	}
	if c := strings.Compare(a.ipk, b.ipk); c != 0 {
		return c
	}
	if c := strings.Compare(a.isk, b.isk); c != 0 {
		return c
	}
	if c := strings.Compare(a.pk, b.pk); c != 0 {
		return c
	}
	return strings.Compare(a.sk, b.sk)
}

// skipQueryAfter drops rows at or before StartAfter in the query direction.
func skipQueryAfter(rows []row, start attr.Item, t *storage.Table, idx *storage.Index, forward bool) []row {
	if start == nil {
		return rows
	}
	i := 0
	for ; i < len(rows); i++ {
		c := cmpQueryAfter(rows[i], start, t, idx)
		if forward {
			if c > 0 {
				break
			}
		} else if c < 0 {
			break
		}
	}
	return rows[i:]
}

func cmpQueryAfter(r row, start attr.Item, t *storage.Table, idx *storage.Index) int {
	if idx == nil {
		if c := strings.Compare(r.pk, string(enc(start[t.Key.Hash]))); c != 0 {
			return c
		}
		if t.Key.Range != "" {
			return strings.Compare(r.sk, string(enc(start[t.Key.Range])))
		}
		return 0
	}
	if c := strings.Compare(r.ipk, string(enc(start[idx.Key.Hash]))); c != 0 {
		return c
	}
	if idx.Key.Range != "" {
		if c := strings.Compare(r.isk, string(enc(start[idx.Key.Range]))); c != 0 {
			return c
		}
	}
	if c := strings.Compare(r.pk, string(enc(start[t.Key.Hash]))); c != 0 {
		return c
	}
	if t.Key.Range != "" {
		return strings.Compare(r.sk, string(enc(start[t.Key.Range])))
	}
	return 0
}

// skipScanAfter drops rows at or before StartAfter in Scan order.
func skipScanAfter(rows []row, start attr.Item, t *storage.Table, idx *storage.Index) []row {
	if start == nil {
		return rows
	}
	i := 0
	for ; i < len(rows); i++ {
		if cmpScan(rows[i], startRow(start, t, idx), idx) > 0 {
			break
		}
	}
	return rows[i:]
}

func startRow(start attr.Item, t *storage.Table, idx *storage.Index) row {
	var r row
	if idx == nil {
		r.pk = string(enc(start[t.Key.Hash]))
		if t.Key.Range != "" {
			r.sk = string(enc(start[t.Key.Range]))
		}
		return r
	}
	r.ipk = string(enc(start[idx.Key.Hash]))
	if idx.Key.Range != "" {
		r.isk = string(enc(start[idx.Key.Range]))
	}
	r.pk = string(enc(start[t.Key.Hash]))
	if t.Key.Range != "" {
		r.sk = string(enc(start[t.Key.Range]))
	}
	return r
}

// paginate applies the page limits and builds LastKey. LastKey is set whenever
// a limit truncated the result, even when the stop is the last item.
func paginate(rows []row, t *storage.Table, idx *storage.Index, lim storage.PageLimit) storage.Page {
	var page storage.Page
	var running int
	var last row
	bytesTruncated := false
	for _, r := range rows {
		if lim.Items > 0 && len(page.Items) >= lim.Items {
			break
		}
		size := attr.ItemSize(r.item)
		if lim.Bytes > 0 && len(page.Items) > 0 && running+size > lim.Bytes {
			bytesTruncated = true
			break
		}
		page.Items = append(page.Items, cloneItem(r.item))
		running += size
		last = r
	}
	truncated := bytesTruncated || (lim.Items > 0 && len(page.Items) >= lim.Items)
	if truncated && len(page.Items) > 0 {
		page.LastKey = lastKey(last, t, idx)
	}
	return page
}

// paginateQuery is Query's pagination: LastKey appears only when a limit stopped
// the page before the end of the in-range rows, so a Query that reaches its last
// item returns no LastEvaluatedKey even when it lands exactly on Limit. Scan
// keeps paginate's more eager behaviour.
func paginateQuery(rows []row, t *storage.Table, idx *storage.Index, lim storage.PageLimit) storage.Page {
	var page storage.Page
	var running int
	var last row
	hasNext := false
	for _, r := range rows {
		if lim.Items > 0 && len(page.Items) >= lim.Items {
			hasNext = true
			break
		}
		size := attr.ItemSize(r.item)
		if lim.Bytes > 0 && len(page.Items) > 0 && running+size > lim.Bytes {
			hasNext = true
			break
		}
		page.Items = append(page.Items, cloneItem(r.item))
		running += size
		last = r
	}
	if hasNext && len(page.Items) > 0 {
		page.LastKey = lastKey(last, t, idx)
	}
	return page
}

// lastKey is the LastEvaluatedKey: the table key, plus the index key for a GSI.
func lastKey(r row, t *storage.Table, idx *storage.Index) attr.Item {
	it := cloneItem(r.item)
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

// inSegment reports whether a signed pk_hash h belongs to segment seg of a
// Scan with `segments` segments, using the contract's equal-width split.
func inSegment(h int64, seg, segments int) bool {
	lo := scanLo(seg, segments)
	hi := scanLo(seg+1, segments)
	return h >= lo && h < hi
}

func scanLo(i, segments int) int64 {
	return int64(math.MinInt32) + (int64(i)*(int64(1)<<32))/int64(segments)
}

func keyFor(k storage.Key) (itemKey, error) {
	pk, err := attr.EncodeKey(k.Hash)
	if err != nil {
		return itemKey{}, err
	}
	var sk []byte
	if k.Range.Type != 0 {
		if sk, err = attr.EncodeKey(k.Range); err != nil {
			return itemKey{}, err
		}
	}
	return itemKey{string(pk), string(sk)}, nil
}

// assignGSIIDs gives a non-zero, distinct ID (within the table) to every GSI
// that still has ID 0.
func assignGSIIDs(t *storage.Table) {
	next := int16(1)
	for i := range t.GSIs {
		if t.GSIs[i].ID >= next {
			next = t.GSIs[i].ID + 1
		}
	}
	for i := range t.GSIs {
		if t.GSIs[i].ID == 0 {
			t.GSIs[i].ID = next
			next++
		}
	}
}

func cloneOrNil(it attr.Item) attr.Item {
	if it == nil {
		return nil
	}
	return cloneItem(it)
}

func cloneItem(it attr.Item) attr.Item {
	if it == nil {
		return nil
	}
	out := make(attr.Item, len(it))
	for k, v := range it {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v attr.Value) attr.Value {
	switch v.Type {
	case attr.TypeB:
		v.B = append([]byte(nil), v.B...)
	case attr.TypeM:
		m := make(map[string]attr.Value, len(v.M))
		for k, mv := range v.M {
			m[k] = cloneValue(mv)
		}
		v.M = m
	case attr.TypeL:
		l := make([]attr.Value, len(v.L))
		for i, lv := range v.L {
			l[i] = cloneValue(lv)
		}
		v.L = l
	case attr.TypeSS:
		v.SS = append([]string(nil), v.SS...)
	case attr.TypeNS:
		v.NS = append([]string(nil), v.NS...)
	case attr.TypeBS:
		bs := make([][]byte, len(v.BS))
		for i, b := range v.BS {
			bs[i] = append([]byte(nil), b...)
		}
		v.BS = bs
	}
	return v
}

func cloneTable(t *storage.Table) *storage.Table {
	if t == nil {
		return nil
	}
	out := *t
	out.AttrDefs = cloneTypes(t.AttrDefs)
	out.GSIs = make([]storage.Index, len(t.GSIs))
	for i, g := range t.GSIs {
		out.GSIs[i] = cloneIndex(g)
	}
	out.Tags = cloneTags(t.Tags)
	out.Replicas = append([]storage.Replica(nil), t.Replicas...)
	return &out
}

func cloneIndex(g storage.Index) storage.Index {
	g.NonKeyAttrs = append([]string(nil), g.NonKeyAttrs...)
	return g
}

func cloneTypes(m map[string]attr.Type) map[string]attr.Type {
	if m == nil {
		return nil
	}
	out := make(map[string]attr.Type, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneTags(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- StreamStore ---

// ReadStream returns up to limit records of t with Seq > after, in Seq order.
func (s *Store) ReadStream(ctx context.Context, t *storage.Table, after int64, limit int) ([]storage.StreamRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[t.Name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	var out []storage.StreamRecord
	for _, r := range st.stream {
		if r.Seq <= after {
			continue
		}
		out = append(out, cloneStreamRecord(r))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// StreamEnd returns the Seq of t's newest stream record, 0 when there is none.
func (s *Store) StreamEnd(ctx context.Context, t *storage.Table) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[t.Name]
	if !ok {
		return 0, storage.ErrTableNotFound
	}
	if n := len(st.stream); n > 0 {
		return st.stream[n-1].Seq, nil
	}
	return 0, nil
}

// TrimStreams deletes every record created before before, at most limit per
// call (0 means no limit), and returns how many it deleted.
func (s *Store) TrimStreams(ctx context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for name := range s.tables {
		if limit > 0 && deleted >= limit {
			break
		}
		st := s.tables[name]
		kept := st.stream[:0]
		for _, r := range st.stream {
			if r.At.Before(before) && (limit <= 0 || deleted < limit) {
				deleted++
				continue
			}
			kept = append(kept, r)
		}
		st.stream = kept
	}
	return deleted, nil
}

// Checkpoint returns the last Seq the consumer finished for t, 0 if none.
func (s *Store) Checkpoint(ctx context.Context, consumer string, t *storage.Table) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpoints[cpKey{consumer: consumer, tableID: t.ID}], nil
}

// SaveCheckpoint records the last Seq the consumer finished for t.
func (s *Store) SaveCheckpoint(ctx context.Context, consumer string, t *storage.Table, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoints[cpKey{consumer: consumer, tableID: t.ID}] = seq
	return nil
}

// TryLease takes or renews the named lease for owner until ttl from now.
func (s *Store) TryLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if l, ok := s.leases[name]; ok && l.owner != owner && l.expiresAt.After(now) {
		return false, nil
	}
	s.leases[name] = lease{owner: owner, expiresAt: now.Add(ttl)}
	return true, nil
}

// --- TTLStore ---

// ExpireItems deletes up to limit expired items across all tables, each as a
// WriteItem Delete would (GSI membership is derived from items, so removing the
// item drops it; a REMOVE stream record with TTL set is written when the table
// has a stream). It returns how many it deleted.
func (s *Store) ExpireItems(ctx context.Context, now time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := 0
	for name := range s.tables {
		if limit > 0 && expired >= limit {
			break
		}
		st := s.tables[name]
		if !st.def.TTLEnabled || st.def.TTLAttr == "" {
			continue
		}
		for key, it := range st.items {
			if limit > 0 && expired >= limit {
				break
			}
			if !itemExpired(&st.def, it, now) {
				continue
			}
			old := cloneItem(it)
			delete(st.items, key)
			s.appendStream(st, old, nil, true)
			expired++
		}
	}
	return expired, nil
}

// --- BackupStore ---

// CreateBackup snapshots t's definition and every item in memory. The copy is
// synchronous, so the returned backup is already AVAILABLE.
func (s *Store) CreateBackup(ctx context.Context, t *storage.Table, name string) (*storage.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tables[t.Name]
	if !ok {
		return nil, storage.ErrTableNotFound
	}
	id, createdAt := newBackupID()
	items := make([]attr.Item, 0, len(st.items))
	var bytes int64
	for _, it := range st.items {
		c := cloneItem(it)
		items = append(items, c)
		bytes += int64(attr.ItemSize(c))
	}
	b := storage.Backup{
		ID:        id,
		Name:      name,
		Table:     *cloneTable(&st.def),
		Status:    "AVAILABLE",
		CreatedAt: createdAt,
		Items:     int64(len(items)),
		Bytes:     bytes,
	}
	s.backups[id] = &memBackup{def: b, items: items}
	return cloneBackup(&b), nil
}

// GetBackup returns the backup metadata, or ErrBackupNotFound.
func (s *Store) GetBackup(ctx context.Context, id string) (*storage.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, ok := s.backups[id]
	if !ok {
		return nil, storage.ErrBackupNotFound
	}
	return cloneBackup(&mb.def), nil
}

// ListBackups returns matching backups oldest first (IDs encode creation time),
// after startAfter, at most limit.
func (s *Store) ListBackups(ctx context.Context, table string, from, to time.Time, startAfter string, limit int) ([]storage.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Backup
	for _, mb := range s.backups {
		b := mb.def
		if table != "" && b.Table.Name != table {
			continue
		}
		if !from.IsZero() && b.CreatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && !b.CreatedAt.Before(to) {
			continue
		}
		if b.ID <= startAfter {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	res := make([]storage.Backup, len(out))
	for i := range out {
		res[i] = *cloneBackup(&out[i])
	}
	return res, nil
}

// DeleteBackup drops the backup and returns it marked DELETED.
func (s *Store) DeleteBackup(ctx context.Context, id string) (*storage.Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, ok := s.backups[id]
	if !ok {
		return nil, storage.ErrBackupNotFound
	}
	b := cloneBackup(&mb.def)
	b.Status = "DELETED"
	delete(s.backups, id)
	return b, nil
}

// RestoreBackup creates target as a new table and fills it with the backup's
// items. The copy is synchronous, so the returned table is ACTIVE.
func (s *Store) RestoreBackup(ctx context.Context, id string, target *storage.Table) (*storage.Table, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb, ok := s.backups[id]
	if !ok {
		return nil, storage.ErrBackupNotFound
	}
	if _, exists := s.tables[target.Name]; exists {
		return nil, storage.ErrTableExists
	}
	c := cloneTable(target)
	c.ID = s.nextID
	s.nextID++
	assignGSIIDs(c)
	c.Status = storage.StatusActive
	for i := range c.GSIs {
		c.GSIs[i].Status = storage.StatusActive
	}
	st := &table{def: *cloneTable(c), items: make(map[itemKey]attr.Item, len(mb.items))}
	for _, it := range mb.items {
		key, err := itemKeyOf(c, it)
		if err != nil {
			return nil, err
		}
		st.items[key] = cloneItem(it)
	}
	s.tables[c.Name] = st
	s.names = append(s.names, c.Name)
	sort.Strings(s.names)
	return cloneTable(c), nil
}

// itemKeyOf computes an item's stored key from its key attributes.
func itemKeyOf(t *storage.Table, it attr.Item) (itemKey, error) {
	k := storage.Key{Hash: it[t.Key.Hash]}
	if t.Key.Range != "" {
		k.Range = it[t.Key.Range]
	}
	return keyFor(k)
}

// newBackupID builds an ID from the current millisecond timestamp (0-padded,
// so IDs sort by creation time) plus 8 random hex bytes.
func newBackupID() (string, time.Time) {
	ms := time.Now().UnixMilli()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%013d-%s", ms, hex.EncodeToString(b[:])), time.UnixMilli(ms)
}

func cloneBackup(b *storage.Backup) *storage.Backup {
	out := *b
	out.Table = *cloneTable(&b.Table)
	return &out
}

// appendStream writes one stream record for a table with streams on. old and
// new are the item before and after the write (either may be nil). There is no
// record for a no-op (Keep or a Put that does not change the item).
func (s *Store) appendStream(st *table, old, new attr.Item, ttl bool) {
	t := &st.def
	if t.StreamView == "" {
		return
	}
	ev := streamEvent(old, new)
	if ev == "" {
		return
	}
	rec := storage.StreamRecord{
		Seq:   s.nextSeq,
		Event: ev,
		Keys:  cloneItem(storage.KeyItem(t.Key, keySource(old, new))),
		At:    time.Now(),
		TTL:   ttl,
	}
	s.nextSeq++
	if streamWantsOld(t.StreamView) && old != nil {
		rec.Old = cloneItem(old)
	}
	if streamWantsNew(t.StreamView) && new != nil {
		rec.New = cloneItem(new)
	}
	rec.Size = attr.ItemSize(rec.Keys)
	if rec.Old != nil {
		rec.Size += attr.ItemSize(rec.Old)
	}
	if rec.New != nil {
		rec.Size += attr.ItemSize(rec.New)
	}
	st.stream = append(st.stream, rec)
}

// itemExpired reports whether it has passed its TTL: TTL on, the TTL attribute
// an N of epoch seconds strictly before now.
func itemExpired(t *storage.Table, it attr.Item, now time.Time) bool {
	if !t.TTLEnabled || t.TTLAttr == "" {
		return false
	}
	v, ok := it[t.TTLAttr]
	if !ok || v.Type != attr.TypeN {
		return false
	}
	sec, err := strconv.ParseInt(v.N, 10, 64)
	if err != nil {
		return false
	}
	return time.Unix(sec, 0).Before(now)
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

func cloneStreamRecord(r storage.StreamRecord) storage.StreamRecord {
	r.Keys = cloneItem(r.Keys)
	r.Old = cloneItem(r.Old)
	r.New = cloneItem(r.New)
	return r
}
