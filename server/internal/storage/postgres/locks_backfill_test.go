package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres/pgtest"
)

// holdItemLock takes the item's advisory lock in its own transaction on a
// separate connection and returns a function that releases it.
func holdItemLock(t *testing.T, connString string, tableID int32, k storage.Key) func() {
	t.Helper()
	lk, err := keyLock(k)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		_ = c.Close(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int, $2::int)`, tableID, lk.lock); err != nil {
		_ = tx.Rollback(ctx)
		_ = c.Close(ctx)
		t.Fatal(err)
	}
	return func() {
		_ = tx.Commit(ctx)
		_ = c.Close(ctx)
	}
}

func keySN(hash, rng string) storage.Key {
	return storage.Key{Hash: sv(hash), Range: nv(rng)}
}

func putItemFn(it attr.Item) func(attr.Item) (storage.WriteResult, error) {
	return func(attr.Item) (storage.WriteResult, error) {
		return storage.WriteResult{Op: storage.Put, Item: it}, nil
	}
}

func TestItemLevelLockDoesNotBlockOtherItems(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)
	s, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:     "locks",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS, Range: "sk", RangeType: attr.TypeN},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "sk": attr.TypeN},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	for _, rng := range []string{"1", "2"} {
		it := item("id", sv("a"), "sk", nv(rng))
		if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keySN("a", rng), Fn: putItemFn(it)}); err != nil {
			t.Fatalf("put %s: %v", rng, err)
		}
	}

	release := holdItemLock(t, connString, tb.ID, keySN("a", "1"))
	defer release()

	start := time.Now()
	_, _, err = s.WriteItem(ctx, storage.Write{Table: tb, Key: keySN("a", "2"), Fn: putItemFn(item("id", sv("a"), "sk", nv("2"), "v", sv("x")))})
	if err != nil {
		t.Fatalf("WriteItem on unlocked item: %v", err)
	}
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("WriteItem on an unlocked item in the same partition took %v, want < 1s", d)
	}
}

func TestLockTimeoutThrottlesWriteAndTransact(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)
	s, err := OpenWithOptions(ctx, connString, Options{LockTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:     "throttle",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS, Range: "sk", RangeType: attr.TypeN},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "sk": attr.TypeN},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keySN("a", "1"), Fn: putItemFn(item("id", sv("a"), "sk", nv("1")))}); err != nil {
		t.Fatalf("put: %v", err)
	}

	release := holdItemLock(t, connString, tb.ID, keySN("a", "1"))
	defer release()

	start := time.Now()
	_, _, err = s.WriteItem(ctx, storage.Write{Table: tb, Key: keySN("a", "1"), Fn: putItemFn(item("id", sv("a"), "sk", nv("1"), "v", sv("x")))})
	if !errors.Is(err, storage.ErrThrottled) {
		t.Fatalf("WriteItem err = %v, want storage.ErrThrottled", err)
	}
	if d := time.Since(start); d >= 2*time.Second {
		t.Fatalf("WriteItem throttle took %v, want < 2s", d)
	}

	start = time.Now()
	err = s.Transact(ctx, []storage.Write{{Table: tb, Key: keySN("a", "1"), Fn: putItemFn(item("id", sv("a"), "sk", nv("1"), "v", sv("y")))}})
	if !errors.Is(err, storage.ErrThrottled) {
		t.Fatalf("Transact err = %v, want storage.ErrThrottled", err)
	}
	if d := time.Since(start); d >= 2*time.Second {
		t.Fatalf("Transact throttle took %v, want < 2s", d)
	}
}

func TestBackfillUnderLoadIsConsistent(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)
	s, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:     "backfill-load",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "gid": attr.TypeS},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	const n = 2000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%04d", i)
		it := item("id", sv(id), "gid", sv(fmt.Sprintf("g%04d", i)))
		if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: storage.Key{Hash: sv(id)}, Fn: putItemFn(it)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	updated, err := s.UpdateTable(ctx, "backfill-load", func(t *storage.Table) error {
		t.GSIs = append(t.GSIs, storage.Index{
			Name:       "byGid",
			Key:        storage.KeySchema{Hash: "gid", HashType: attr.TypeS},
			Projection: storage.ProjectAll,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTable: %v", err)
	}
	g := updated.GSIs[0]

	var counter int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := rand.Intn(n)
				id := fmt.Sprintf("id%04d", i)
				newGid := fmt.Sprintf("v%09d", atomic.AddInt64(&counter, 1))
				_, _, err := s.WriteItem(ctx, storage.Write{Table: updated, Key: storage.Key{Hash: sv(id)}, Fn: putItemFn(item("id", sv(id), "gid", sv(newGid)))})
				if err != nil && !errors.Is(err, storage.ErrThrottled) {
					errCh <- err
					return
				}
			}
		}()
	}

	waitIndexActive(t, s, "backfill-load", g.ID)

	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	}

	checkIndexMatchesItems(t, s, tb.ID, g)
}

func TestResumeBackfillOnOpen(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)

	s1, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tb := &storage.Table{
		Name:     "resume",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "gid": attr.TypeS},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
	}
	tb, err = s1.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	const n = 100
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%04d", i)
		it := item("id", sv(id), "gid", sv(fmt.Sprintf("g%04d", i)))
		if _, _, err := s1.WriteItem(ctx, storage.Write{Table: tb, Key: storage.Key{Hash: sv(id)}, Fn: putItemFn(it)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	g := storage.Index{
		Name:       "byGid",
		ID:         1,
		Key:        storage.KeySchema{Hash: "gid", HashType: attr.TypeS},
		Projection: storage.ProjectAll,
		Status:     storage.StatusCreating,
	}
	setGSICreating(t, s1, tb.ID, g)

	if got := indexEntryCount(t, s1, tb.ID, g.ID); got != 0 {
		t.Fatalf("index_entries has %d rows before resume, want 0", got)
	}

	s1.Close()

	s2, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	waitIndexActive(t, s2, "resume", g.ID)
	checkIndexMatchesItems(t, s2, tb.ID, g)
}

// --- helpers ---

func waitIndexActive(t *testing.T, s *Store, name string, indexID int16) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		tb, err := s.GetTable(context.Background(), name)
		if err != nil {
			t.Fatalf("GetTable: %v", err)
		}
		for i := range tb.GSIs {
			if tb.GSIs[i].ID != indexID {
				continue
			}
			switch tb.GSIs[i].Status {
			case storage.StatusActive:
				return
			case storage.StatusCreating:
			default:
				t.Fatalf("GSI status = %q, want CREATING or ACTIVE", tb.GSIs[i].Status)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GSI %d still CREATING after 60s", indexID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func setGSICreating(t *testing.T, s *Store, tableID int32, g storage.Index) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var def []byte
	if err := tx.QueryRow(ctx, `SELECT def FROM ddb_tables WHERE table_id = $1 FOR UPDATE`, tableID).Scan(&def); err != nil {
		t.Fatal(err)
	}
	tbl, err := unmarshalTable(def)
	if err != nil {
		t.Fatal(err)
	}
	tbl.GSIs = append(tbl.GSIs, g)
	defJSON, err := json.Marshal(tbl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ddb_tables SET def = $1::jsonb WHERE table_id = $2`, string(defJSON), tableID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func indexEntryCount(t *testing.T, s *Store, tableID int32, indexID int16) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM index_entries WHERE table_id = $1 AND index_id = $2`, tableID, indexID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func entryKey(ih int32, ipk, isk []byte, ph int32, pk, sk []byte) string {
	return fmt.Sprintf("%d|%x|%x|%d|%x|%x", ih, ipk, isk, ph, pk, sk)
}

// checkIndexMatchesItems compares index_entries for one index against the
// entries computed from the items table, in both directions.
func checkIndexMatchesItems(t *testing.T, s *Store, tableID int32, g storage.Index) {
	t.Helper()
	ctx := context.Background()

	want := map[string]struct{}{}
	rows, err := s.pool.Query(ctx, `SELECT pk_hash, pk, sk, item FROM items WHERE table_id = $1`, tableID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h int32
		var pk, sk, itemBytes []byte
		if err := rows.Scan(&h, &pk, &sk, &itemBytes); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var it attr.Item
		if err := json.Unmarshal(itemBytes, &it); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if ih, ipk, isk, ok := indexKey(g, it); ok {
			want[entryKey(ih, ipk, isk, h, pk, sk)] = struct{}{}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	got := map[string]struct{}{}
	rows, err = s.pool.Query(ctx, `SELECT ipk_hash, ipk, isk, pk_hash, pk, sk FROM index_entries WHERE table_id = $1 AND index_id = $2`, tableID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ih int32
		var ph int32
		var ipk, isk, pk, sk []byte
		if err := rows.Scan(&ih, &ipk, &isk, &ph, &pk, &sk); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got[entryKey(ih, ipk, isk, ph, pk, sk)] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("index_entries mismatch: want %d entries, got %d", len(want), len(got))
	}
}
