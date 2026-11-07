package postgres

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres/pgtest"
	"github.com/toclocoinc/dynamoat/server/internal/storage/storagetest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestStoreCatalog(t *testing.T) {
	storagetest.RunCatalog(t, newStore)
}

func TestStoreItems(t *testing.T) {
	storagetest.RunItems(t, newStore)
}

func TestStoreReads(t *testing.T) {
	storagetest.RunReads(t, newStore)
}

func TestStoreStreams(t *testing.T) {
	storagetest.RunStreams(t, newStore)
}

func TestStoreTTL(t *testing.T) {
	storagetest.RunTTL(t, newStore)
}

func TestStorePITR(t *testing.T) {
	storagetest.RunPITR(t, newStore)
}

func TestStoreBackups(t *testing.T) {
	storagetest.RunBackups(t, newStore)
}

func newStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := Open(context.Background(), pgtest.DB(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestServerVersionTooOld(t *testing.T) {
	err := serverVersionTooOld(140011, "14.11")
	if err == nil {
		t.Fatal("want an error for 14.11")
	}
	want := "dynamoat needs PostgreSQL 15 or newer; this server is 14.11"
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	for _, tc := range []struct {
		num     int
		version string
	}{
		{150000, "15.0"},
		{160004, "16.4"},
		{170004, "17.4"},
		{180000, "18.0"},
	} {
		if err := serverVersionTooOld(tc.num, tc.version); err != nil {
			t.Fatalf("serverVersionTooOld(%d, %q) = %v, want nil", tc.num, tc.version, err)
		}
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	connString := pgtest.DB(t)
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), connString)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		s.Close()
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(names) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(names))
	}
	var dup int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM (SELECT name FROM schema_migrations GROUP BY name HAVING count(*) > 1) d`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("%d migration names applied more than once", dup)
	}
}

func TestDeletionProtectionColumn(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)
	s, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:               "dp",
		Key:                storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:           map[string]attr.Type{"id": attr.TypeS},
		Billing:            "PAY_PER_REQUEST",
		Status:             storage.StatusActive,
		DeletionProtection: true,
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	var dp bool
	if err := s.pool.QueryRow(ctx, `SELECT deletion_protection FROM ddb_tables WHERE table_id = $1`, tb.ID).Scan(&dp); err != nil {
		t.Fatal(err)
	}
	if !dp {
		t.Fatalf("deletion_protection column = false, want true")
	}

	if _, err := s.UpdateTable(ctx, "dp", func(t *storage.Table) error {
		t.DeletionProtection = false
		return nil
	}); err != nil {
		t.Fatalf("UpdateTable: %v", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT deletion_protection FROM ddb_tables WHERE table_id = $1`, tb.ID).Scan(&dp); err != nil {
		t.Fatal(err)
	}
	if dp {
		t.Fatalf("deletion_protection column = true after update, want false")
	}
}

func TestConcurrentOpen(t *testing.T) {
	connString := pgtest.DB(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), connString)
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
			s.Close()
			errs <- nil
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
}

func TestStreamRecordsByView(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		view        string
		oldOnModify bool
		newOnModify bool
		oldOnRemove bool
		newOnInsert bool
	}{
		{storage.StreamKeysOnly, false, false, false, false},
		{storage.StreamNewImage, false, true, false, true},
		{storage.StreamOldImage, true, false, true, false},
		{storage.StreamNewAndOld, true, true, true, true},
	} {
		t.Run(tc.view, func(t *testing.T) {
			connString := pgtest.DB(t)
			s, err := Open(ctx, connString)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()

			tb := &storage.Table{
				Name:       "s",
				Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS},
				AttrDefs:   map[string]attr.Type{"id": attr.TypeS},
				Billing:    "PAY_PER_REQUEST",
				Status:     storage.StatusActive,
				StreamView: tc.view,
			}
			tb, err = s.CreateTable(ctx, tb)
			if err != nil {
				t.Fatalf("CreateTable: %v", err)
			}

			put := func(it attr.Item) {
				t.Helper()
				_, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("a", ""), Fn: func(attr.Item) (storage.WriteResult, error) {
					return storage.WriteResult{Op: storage.Put, Item: it}, nil
				}})
				if err != nil {
					t.Fatalf("put: %v", err)
				}
			}
			put(item("id", sv("a"), "v", sv("1")))
			put(item("id", sv("a"), "v", sv("2")))
			_, _, err = s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("a", ""), Fn: func(attr.Item) (storage.WriteResult, error) {
				return storage.WriteResult{Op: storage.Delete}, nil
			}})
			if err != nil {
				t.Fatalf("delete: %v", err)
			}

			rows := streamRows(t, connString, tb.ID)
			if len(rows) != 3 {
				t.Fatalf("got %d stream records, want 3", len(rows))
			}
			if rows[0].event != "INSERT" || rows[1].event != "MODIFY" || rows[2].event != "REMOVE" {
				t.Fatalf("events = %q, %q, %q", rows[0].event, rows[1].event, rows[2].event)
			}

			keys := item("id", sv("a"))
			for i, r := range rows {
				if !reflect.DeepEqual(r.keys, keys) {
					t.Fatalf("record %d keys = %#v, want %#v", i, r.keys, keys)
				}
			}

			img := func(want bool, got attr.Item) {
				t.Helper()
				if want && got == nil {
					t.Fatal("expected an image, got NULL")
				}
				if !want && got != nil {
					t.Fatalf("expected NULL image, got %#v", got)
				}
			}
			img(tc.newOnInsert, rows[0].newImage)
			img(false, rows[0].oldImage)
			img(tc.oldOnModify, rows[1].oldImage)
			img(tc.newOnModify, rows[1].newImage)
			img(tc.oldOnRemove, rows[2].oldImage)
			img(false, rows[2].newImage)
		})
	}
}

func TestTTLSetAndCleared(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)
	s, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:       "ttl",
		Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:   map[string]attr.Type{"id": attr.TypeS},
		Billing:    "PAY_PER_REQUEST",
		Status:     storage.StatusActive,
		TTLAttr:    "expires",
		TTLEnabled: true,
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	put := func(it attr.Item) {
		t.Helper()
		if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("a", ""), Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{Op: storage.Put, Item: it}, nil
		}}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	put(item("id", sv("a"), "expires", nv("1735689600")))
	var ts time.Time
	err = s.pool.QueryRow(ctx, `SELECT ttl_at FROM items WHERE table_id = $1`, tb.ID).Scan(&ts)
	if err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(time.Unix(1735689600, 0)) {
		t.Fatalf("ttl_at = %v, want epoch 1735689600", ts)
	}

	put(item("id", sv("a")))
	var isNull bool
	err = s.pool.QueryRow(ctx, `SELECT ttl_at IS NULL FROM items WHERE table_id = $1`, tb.ID).Scan(&isNull)
	if err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("ttl_at not cleared when the TTL attribute is removed")
	}
}

func TestGSISparseRows(t *testing.T) {
	ctx := context.Background()
	connString := pgtest.DB(t)
	s, err := Open(ctx, connString)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:     "g",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "name": attr.TypeS},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
		GSIs: []storage.Index{{
			Name:       "byName",
			Key:        storage.KeySchema{Hash: "name", HashType: attr.TypeS},
			Projection: storage.ProjectAll,
		}},
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	put := func(k string, it attr.Item) {
		t.Helper()
		if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS(k, ""), Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{Op: storage.Put, Item: it}, nil
		}}); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}

	put("a", item("id", sv("a"), "name", sv("alice")))
	put("b", item("id", sv("b")))                  // no index key
	put("c", item("id", sv("c"), "name", nv("5"))) // wrong type

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM index_entries WHERE table_id = $1`, tb.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("index_entries has %d rows, want 1", n)
	}
	var ipk, pk []byte
	if err := s.pool.QueryRow(ctx, `SELECT ipk, pk FROM index_entries WHERE table_id = $1`, tb.ID).Scan(&ipk, &pk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ipk, mustEncode(sv("alice"))) {
		t.Fatalf("ipk = %v, want encoded alice", ipk)
	}
	if !reflect.DeepEqual(pk, mustEncode(sv("a"))) {
		t.Fatalf("pk = %v, want encoded a", pk)
	}
}

func TestPoolSizing(t *testing.T) {
	s, err := OpenWithOptions(context.Background(), pgtest.DB(t), Options{MaxConns: 3, MinConns: 2})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()
	cfg := s.pool.Config()
	if cfg.MaxConns != 3 {
		t.Fatalf("MaxConns = %d, want 3", cfg.MaxConns)
	}
	if cfg.MinConns != 2 {
		t.Fatalf("MinConns = %d, want 2", cfg.MinConns)
	}
}

func TestPoolSizingDefaults(t *testing.T) {
	s, err := Open(context.Background(), pgtest.DB(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	cfg := s.pool.Config()
	if cfg.MaxConns != 10 {
		t.Fatalf("MaxConns = %d, want default 10", cfg.MaxConns)
	}
	if cfg.MinConns != 0 {
		t.Fatalf("MinConns = %d, want default 0", cfg.MinConns)
	}
}

func TestRuntimeParamsSetOnNewConnections(t *testing.T) {
	ctx := context.Background()
	s, err := OpenWithOptions(ctx, pgtest.DB(t), Options{
		StatementTimeout: 7 * time.Second,
		IdleTxTimeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()

	var st, it string
	if err := s.pool.QueryRow(ctx, `SHOW statement_timeout`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SHOW idle_in_transaction_session_timeout`).Scan(&it); err != nil {
		t.Fatal(err)
	}
	if st != "7s" {
		t.Fatalf("statement_timeout = %q, want %q", st, "7s")
	}
	if it != "3s" {
		t.Fatalf("idle_in_transaction_session_timeout = %q, want %q", it, "3s")
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type tokenSourceFunc func(context.Context) (string, error)

func (f tokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

func TestBeforeConnectSetsPassword(t *testing.T) {
	connString := pgtest.DB(t)
	u, err := url.Parse(connString)
	if err != nil {
		t.Fatal(err)
	}
	// The token source supplies the real password; the connection string's
	// own password is wrong, so the connection only works if BeforeConnect
	// replaced it.
	password, _ := u.User.Password()
	u.User = url.UserPassword(u.User.Username(), "wrong-password")
	s, err := OpenWithOptions(context.Background(), u.String(), Options{TokenSource: staticToken(password)})
	if err != nil {
		t.Fatalf("OpenWithOptions with token password: %v", err)
	}
	s.Close()
}

func TestTokenSourceErrorFailsOpen(t *testing.T) {
	connString := pgtest.DB(t)
	_, err := OpenWithOptions(context.Background(), connString, Options{
		TokenSource: tokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("token unavailable")
		}),
	})
	if err == nil {
		t.Fatal("want an error when the token source fails")
	}
	if !strings.Contains(err.Error(), "token unavailable") {
		t.Fatalf("error = %v, want it to mention the token failure", err)
	}
}

// --- helpers ---

func item(pairs ...any) attr.Item {
	it := attr.Item{}
	for i := 0; i < len(pairs); i += 2 {
		it[pairs[i].(string)] = pairs[i+1].(attr.Value)
	}
	return it
}

func sv(s string) attr.Value { return attr.Value{Type: attr.TypeS, S: s} }

func nv(s string) attr.Value {
	n, err := attr.NormalizeNumber(s)
	if err != nil {
		panic(err)
	}
	return attr.Value{Type: attr.TypeN, N: n}
}

func keyS(hash, rng string) storage.Key {
	k := storage.Key{Hash: sv(hash)}
	if rng != "" {
		k.Range = sv(rng)
	}
	return k
}

func mustEncode(v attr.Value) []byte {
	b, err := attr.EncodeKey(v)
	if err != nil {
		panic(err)
	}
	return b
}

type streamRow struct {
	event    string
	keys     attr.Item
	oldImage attr.Item
	newImage attr.Item
}

func streamRows(t *testing.T, connString string, tableID int32) []streamRow {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, `SELECT event, keys, old_image, new_image FROM stream_records WHERE table_id = $1 ORDER BY seq`, tableID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []streamRow
	for rows.Next() {
		var r streamRow
		var keysRaw, oldRaw, newRaw []byte
		if err := rows.Scan(&r.event, &keysRaw, &oldRaw, &newRaw); err != nil {
			t.Fatal(err)
		}
		r.keys = decodeItemBytes(t, keysRaw)
		r.oldImage = decodeItemBytes(t, oldRaw)
		r.newImage = decodeItemBytes(t, newRaw)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeItemBytes(t *testing.T, b []byte) attr.Item {
	t.Helper()
	if b == nil {
		return nil
	}
	var it attr.Item
	if err := it.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	return it
}
