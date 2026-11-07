// Package storagetest is the contract test suite for storage.Store. Every
// backend must pass Run; the Postgres store and the in-memory store both do.
// The suite is written from the doc comments in storage.go, which are the
// contract.
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

// New returns a fresh, empty store for one test.
type New func(t *testing.T) storage.Store

// Run runs the whole suite.
func Run(t *testing.T, newStore New) {
	t.Run("catalog", func(t *testing.T) { RunCatalog(t, newStore) })
	t.Run("items", func(t *testing.T) { RunItems(t, newStore) })
	t.Run("reads", func(t *testing.T) { RunReads(t, newStore) })
	t.Run("streams", func(t *testing.T) { RunStreams(t, newStore) })
	t.Run("ttl", func(t *testing.T) { RunTTL(t, newStore) })
	t.Run("backups", func(t *testing.T) { RunBackups(t, newStore) })
	t.Run("pitr", func(t *testing.T) { RunPITR(t, newStore) })
}

// RunCatalog covers the table catalog: CreateTable, GetTable, ListTables,
// UpdateTable and DeleteTable.
func RunCatalog(t *testing.T, newStore New) {
	t.Run("create assigns ids", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		got, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0,
			gsi("byName", "name", attr.TypeS, "", 0), gsi("byAge", "age", attr.TypeN, "", 0)))
		if err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		if got.ID == 0 {
			t.Fatalf("table ID is zero")
		}
		if len(got.GSIs) != 2 {
			t.Fatalf("got %d GSIs, want 2", len(got.GSIs))
		}
		if got.GSIs[0].ID == 0 || got.GSIs[1].ID == 0 {
			t.Fatalf("GSI ID is zero: %#v", got.GSIs)
		}
		if got.GSIs[0].ID == got.GSIs[1].ID {
			t.Fatalf("GSI IDs are not distinct: %#v", got.GSIs)
		}
	})

	t.Run("create existing name", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if _, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0)); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		_, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0))
		if !errors.Is(err, storage.ErrTableExists) {
			t.Fatalf("got %v, want ErrTableExists", err)
		}
	})

	t.Run("get missing name", func(t *testing.T) {
		s := newStore(t)
		_, err := s.GetTable(context.Background(), "nope")
		if !errors.Is(err, storage.ErrTableNotFound) {
			t.Fatalf("got %v, want ErrTableNotFound", err)
		}
	})

	t.Run("list tables byte order", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		for _, n := range []string{"zeta", "alpha", "mike", "Beta"} {
			if _, err := s.CreateTable(ctx, table(n, "id", attr.TypeS, "", 0)); err != nil {
				t.Fatalf("CreateTable %q: %v", n, err)
			}
		}
		got, err := s.ListTables(ctx, "", 0)
		if err != nil {
			t.Fatalf("ListTables: %v", err)
		}
		want := []string{"Beta", "alpha", "mike", "zeta"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("list tables pagination", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		var names []string
		for _, n := range []string{"a", "b", "c", "d", "e"} {
			if _, err := s.CreateTable(ctx, table(n, "id", attr.TypeS, "", 0)); err != nil {
				t.Fatalf("CreateTable %q: %v", n, err)
			}
			names = append(names, n)
		}
		page1, err := s.ListTables(ctx, "", 2)
		if err != nil {
			t.Fatalf("ListTables: %v", err)
		}
		if !reflect.DeepEqual(page1, []string{"a", "b"}) {
			t.Fatalf("page1 = %v", page1)
		}
		page2, err := s.ListTables(ctx, page1[len(page1)-1], 2)
		if err != nil {
			t.Fatalf("ListTables: %v", err)
		}
		if !reflect.DeepEqual(page2, []string{"c", "d"}) {
			t.Fatalf("page2 = %v", page2)
		}
		page3, err := s.ListTables(ctx, page2[len(page2)-1], 2)
		if err != nil {
			t.Fatalf("ListTables: %v", err)
		}
		if !reflect.DeepEqual(page3, []string{"e"}) {
			t.Fatalf("page3 = %v", page3)
		}
	})

	t.Run("update persists", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if _, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0)); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		got, err := s.UpdateTable(ctx, "alpha", func(t *storage.Table) error {
			t.Billing = "PROVISIONED"
			t.WriteCU = 10
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateTable: %v", err)
		}
		if got.Billing != "PROVISIONED" || got.WriteCU != 10 {
			t.Fatalf("update not applied: %#v", got)
		}
		again, err := s.GetTable(ctx, "alpha")
		if err != nil {
			t.Fatalf("GetTable: %v", err)
		}
		if again.Billing != "PROVISIONED" || again.WriteCU != 10 {
			t.Fatalf("update did not persist: %#v", again)
		}
	})

	t.Run("deletion protection persists", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := table("alpha", "id", attr.TypeS, "", 0)
		tb.DeletionProtection = true
		got, err := s.CreateTable(ctx, tb)
		if err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		if !got.DeletionProtection {
			t.Fatalf("created table lost deletion protection: %#v", got)
		}
		again, err := s.GetTable(ctx, "alpha")
		if err != nil {
			t.Fatalf("GetTable: %v", err)
		}
		if !again.DeletionProtection {
			t.Fatalf("deletion protection did not persist: %#v", again)
		}
		updated, err := s.UpdateTable(ctx, "alpha", func(t *storage.Table) error {
			t.DeletionProtection = false
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateTable: %v", err)
		}
		if updated.DeletionProtection {
			t.Fatalf("update did not clear deletion protection: %#v", updated)
		}
	})

	t.Run("update error changes nothing", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if _, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0)); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		sentinel := errors.New("boom")
		_, err := s.UpdateTable(ctx, "alpha", func(t *storage.Table) error {
			t.Billing = "PROVISIONED"
			t.GSIs = append(t.GSIs, gsi("byName", "name", attr.TypeS, "", 0))
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want the fn error", err)
		}
		got, err := s.GetTable(ctx, "alpha")
		if err != nil {
			t.Fatalf("GetTable: %v", err)
		}
		if got.Billing != "" && got.Billing != "PAY_PER_REQUEST" {
			t.Fatalf("Billing changed on error: %#v", got.Billing)
		}
		if len(got.GSIs) != 0 {
			t.Fatalf("GSIs changed on error: %d", len(got.GSIs))
		}
	})

	t.Run("delete returns definition and frees name", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if _, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0)); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		got, err := s.DeleteTable(ctx, "alpha")
		if err != nil {
			t.Fatalf("DeleteTable: %v", err)
		}
		if got.Name != "alpha" {
			t.Fatalf("deleted definition name = %q", got.Name)
		}
		if got.Status != storage.StatusDeleting {
			t.Fatalf("deleted status = %q, want DELETING", got.Status)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err := s.GetTable(ctx, "alpha")
			if errors.Is(err, storage.ErrTableNotFound) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("table still visible after delete: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := s.CreateTable(ctx, table("alpha", "id", attr.TypeS, "", 0)); err != nil {
			t.Fatalf("name not reusable after delete: %v", err)
		}
	})
}

// RunItems covers GetItem, WriteItem and Transact. GSIs and streams are not
// observable here.
func RunItems(t *testing.T, newStore New) {
	t.Run("put then get equal", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeN))
		want := item("id", sv("x"), "sk", nv("1"), "v", sv("hello"), "bin", bv([]byte{0, 1, 2}))
		put(t, s, tb, keyS("x", "1"), want)
		got, err := s.GetItem(ctx, tb, keyS("x", "1"))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("put replaces", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("x", ""), item("id", sv("x"), "v", sv("one")))
		put(t, s, tb, keyS("x", ""), item("id", sv("x"), "v", sv("two")))
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		want := item("id", sv("x"), "v", sv("two"))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("delete removes", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("x", ""), item("id", sv("x"), "v", sv("one")))
		del(t, s, tb, keyS("x", ""))
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got != nil {
			t.Fatalf("got %#v, want nil", got)
		}
	})

	t.Run("keep changes nothing", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		want := item("id", sv("x"), "v", sv("one"))
		put(t, s, tb, keyS("x", ""), want)
		old, new, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("x", ""), Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{Op: storage.Keep}, nil
		}})
		if err != nil {
			t.Fatalf("WriteItem: %v", err)
		}
		if !reflect.DeepEqual(old, want) || !reflect.DeepEqual(new, want) {
			t.Fatalf("old=%#v new=%#v", old, new)
		}
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("get missing key is nil", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got != nil {
			t.Fatalf("got %#v, want nil", got)
		}
	})

	t.Run("write item old and new", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		// Put on missing: old nil, new the item.
		old, new, err := s.WriteItem(ctx, putWrite(tb, keyS("x", ""), item("id", sv("x"), "v", sv("one"))))
		if err != nil {
			t.Fatalf("WriteItem: %v", err)
		}
		if old != nil {
			t.Fatalf("old = %#v, want nil", old)
		}
		if !reflect.DeepEqual(new, item("id", sv("x"), "v", sv("one"))) {
			t.Fatalf("new = %#v", new)
		}
		// Put on existing: old the previous item, new the replacement.
		old, new, err = s.WriteItem(ctx, putWrite(tb, keyS("x", ""), item("id", sv("x"), "v", sv("two"))))
		if err != nil {
			t.Fatalf("WriteItem: %v", err)
		}
		if !reflect.DeepEqual(old, item("id", sv("x"), "v", sv("one"))) {
			t.Fatalf("old = %#v", old)
		}
		if !reflect.DeepEqual(new, item("id", sv("x"), "v", sv("two"))) {
			t.Fatalf("new = %#v", new)
		}
		// Delete: old the item, new nil.
		old, new, err = s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("x", ""), Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{Op: storage.Delete}, nil
		}})
		if err != nil {
			t.Fatalf("WriteItem: %v", err)
		}
		if !reflect.DeepEqual(old, item("id", sv("x"), "v", sv("two"))) {
			t.Fatalf("old = %#v", old)
		}
		if new != nil {
			t.Fatalf("new = %#v, want nil", new)
		}
	})

	t.Run("write func error returns unchanged and writes nothing", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		sentinel := errors.New("nope")
		_, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("x", ""), Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{}, sentinel
		}})
		if !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want the fn error unchanged", err)
		}
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got != nil {
			t.Fatalf("item written despite error: %#v", got)
		}
	})

	t.Run("hash-only table", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("x", ""), item("id", sv("x"), "v", sv("1")))
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(got, item("id", sv("x"), "v", sv("1"))) {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("hash and range table", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeN))
		put(t, s, tb, keyS("x", "2"), item("id", sv("x"), "sk", nv("2"), "v", sv("two")))
		put(t, s, tb, keyS("x", "1"), item("id", sv("x"), "sk", nv("1"), "v", sv("one")))
		got, err := s.GetItem(ctx, tb, keyS("x", "2"))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(got, item("id", sv("x"), "sk", nv("2"), "v", sv("two"))) {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("S N B keys", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeN, "sk", attr.TypeB))
		want := item("id", nv("5"), "sk", bv([]byte{9, 8, 7}), "v", sv("x"))
		put(t, s, tb, storage.Key{Hash: nv("5"), Range: bv([]byte{9, 8, 7})}, want)
		got, err := s.GetItem(ctx, tb, storage.Key{Hash: nv("5"), Range: bv([]byte{9, 8, 7})})
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("isolation returned copy", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		want := item("id", sv("x"), "bin", bv([]byte{1, 2, 3}), "list", attr.Value{Type: attr.TypeL, L: []attr.Value{sv("a")}})
		put(t, s, tb, keyS("x", ""), want)
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		got["bin"] = bv([]byte{9, 9, 9})
		got["list"] = attr.Value{Type: attr.TypeL, L: []attr.Value{sv("b")}}
		again, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(again, want) {
			t.Fatalf("mutating returned item changed the store: %#v", again)
		}
	})

	t.Run("isolation input copy", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		in := item("id", sv("x"), "bin", bv([]byte{1, 2, 3}))
		put(t, s, tb, keyS("x", ""), in)
		in["bin"] = bv([]byte{9, 9, 9})
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if !reflect.DeepEqual(got, item("id", sv("x"), "bin", bv([]byte{1, 2, 3}))) {
			t.Fatalf("mutating the Put input changed the store: %#v", got)
		}
	})

	t.Run("atomic counter 50 goroutines", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("x", ""), item("id", sv("x"), "n", nv("0")))
		var wg sync.WaitGroup
		errs := make(chan error, 50)
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: keyS("x", ""), Fn: func(old attr.Item) (storage.WriteResult, error) {
					n := 0
					if v, ok := old["n"]; ok {
						n, _ = strconv.Atoi(v.N)
					}
					return storage.WriteResult{Op: storage.Put, Item: item("id", sv("x"), "n", nv(strconv.Itoa(n+1)))}, nil
				}})
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("WriteItem: %v", err)
			}
		}
		got, err := s.GetItem(ctx, tb, keyS("x", ""))
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		if got["n"].N != "50" {
			t.Fatalf("counter = %q, want 50", got["n"].N)
		}
	})

	t.Run("transact all or nothing", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeN))
		boom := errors.New("third fails")
		writes := []storage.Write{
			putWrite(tb, keyS("a", "1"), item("id", sv("a"), "sk", nv("1"), "v", sv("1"))),
			putWrite(tb, keyS("a", "2"), item("id", sv("a"), "sk", nv("2"), "v", sv("2"))),
			{Table: tb, Key: keyS("a", "3"), Fn: func(attr.Item) (storage.WriteResult, error) { return storage.WriteResult{}, boom }},
			putWrite(tb, keyS("a", "4"), item("id", sv("a"), "sk", nv("4"), "v", sv("4"))),
		}
		err := s.Transact(ctx, writes)
		var tx *storage.TxError
		if !errors.As(err, &tx) {
			t.Fatalf("got %v, want *TxError", err)
		}
		if tx.Index != 2 {
			t.Fatalf("TxError.Index = %d, want 2", tx.Index)
		}
		if !errors.Is(tx.Err, boom) {
			t.Fatalf("TxError wraps %v, want the fn error", tx.Err)
		}
		for _, k := range []string{"1", "2", "3", "4"} {
			got, err := s.GetItem(ctx, tb, keyS("a", k))
			if err != nil {
				t.Fatalf("GetItem: %v", err)
			}
			if got != nil {
				t.Fatalf("item %s written despite abort: %#v", k, got)
			}
		}
	})

	t.Run("transact concurrent no deadlock", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "n", nv("0")))
		put(t, s, tb, keyS("b", ""), item("id", sv("b"), "n", nv("0")))

		inc := func(name string) storage.Write {
			return storage.Write{Table: tb, Key: keyS(name, ""), Fn: func(old attr.Item) (storage.WriteResult, error) {
				n, _ := strconv.Atoi(old["n"].N)
				return storage.WriteResult{Op: storage.Put, Item: item("id", sv(name), "n", nv(strconv.Itoa(n+1)))}, nil
			}}
		}

		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := 0; i < 20; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				errs <- s.Transact(ctx, []storage.Write{inc("a"), inc("b")})
			}()
			go func() {
				defer wg.Done()
				errs <- s.Transact(ctx, []storage.Write{inc("b"), inc("a")})
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("Transact: %v", err)
			}
		}
		for _, name := range []string{"a", "b"} {
			got, err := s.GetItem(ctx, tb, keyS(name, ""))
			if err != nil {
				t.Fatalf("GetItem: %v", err)
			}
			if got["n"].N != "40" {
				t.Fatalf("counter %s = %q, want 40", name, got["n"].N)
			}
		}
	})
}

// RunReads covers Query and Scan, including GSIs.
func RunReads(t *testing.T, newStore New) {
	// --- Query ---
	t.Run("query hash-only", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("x", ""), item("id", sv("x"), "v", sv("1")))
		put(t, s, tb, keyS("y", ""), item("id", sv("y"), "v", sv("2")))
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0]["id"].S != "x" {
			t.Fatalf("got %#v, want the x item", page.Items)
		}
	})

	t.Run("query range cond eq", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		got := queryN(t, s, tb, "x", rangeCond("=", nv("2")), true)
		if !reflect.DeepEqual(skSet(got), []string{"2"}) {
			t.Fatalf("got %v", skSet(got))
		}
	})

	t.Run("query range cond lt", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		got := queryN(t, s, tb, "x", rangeCond("<", nv("3")), true)
		if !reflect.DeepEqual(skSet(got), []string{"1", "2"}) {
			t.Fatalf("got %v", skSet(got))
		}
	})

	t.Run("query range cond le", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		got := queryN(t, s, tb, "x", rangeCond("<=", nv("2")), true)
		if !reflect.DeepEqual(skSet(got), []string{"1", "2"}) {
			t.Fatalf("got %v", skSet(got))
		}
	})

	t.Run("query range cond gt", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		got := queryN(t, s, tb, "x", rangeCond(">", nv("1")), true)
		if !reflect.DeepEqual(skSet(got), []string{"2", "3"}) {
			t.Fatalf("got %v", skSet(got))
		}
	})

	t.Run("query range cond ge", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		got := queryN(t, s, tb, "x", rangeCond(">=", nv("2")), true)
		if !reflect.DeepEqual(skSet(got), []string{"2", "3"}) {
			t.Fatalf("got %v", skSet(got))
		}
	})

	t.Run("query range cond between", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3", "4")
		got := queryN(t, s, tb, "x", &storage.RangeCond{Op: "BETWEEN", Values: []attr.Value{nv("2"), nv("3")}}, true)
		if !reflect.DeepEqual(skSet(got), []string{"2", "3"}) {
			t.Fatalf("got %v", skSet(got))
		}
	})

	t.Run("query begins_with S", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS))
		for _, sk := range []string{"a", "ab", "ac", "b"} {
			put(t, s, tb, storage.Key{Hash: sv("x"), Range: sv(sk)}, item("id", sv("x"), "sk", sv(sk)))
		}
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Range: &storage.RangeCond{Op: "begins_with", Values: []attr.Value{sv("a")}}, Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !reflect.DeepEqual(skSet(page.Items), []string{"a", "ab", "ac"}) {
			t.Fatalf("got %v", skSet(page.Items))
		}
	})

	t.Run("query begins_with B", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeB))
		for _, sk := range [][]byte{{0x01}, {0x01, 0x02}, {0x01, 0x03}, {0x02}} {
			put(t, s, tb, storage.Key{Hash: sv("x"), Range: bv(sk)}, item("id", sv("x"), "sk", bv(sk)))
		}
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Range: &storage.RangeCond{Op: "begins_with", Values: []attr.Value{bv([]byte{0x01})}}, Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		want := [][]byte{{0x01}, {0x01, 0x02}, {0x01, 0x03}}
		if len(page.Items) != 3 {
			t.Fatalf("got %d items, want 3", len(page.Items))
		}
		for i, w := range want {
			if !reflect.DeepEqual(page.Items[i]["sk"].B, w) {
				t.Fatalf("item %d sk = %v, want %v", i, page.Items[i]["sk"].B, w)
			}
		}
	})

	t.Run("query forward and reverse", func(t *testing.T) {
		s := newStore(t)
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		fwd := queryN(t, s, tb, "x", nil, true)
		if !reflect.DeepEqual(skSet(fwd), []string{"1", "2", "3"}) {
			t.Fatalf("forward = %v", skSet(fwd))
		}
		rev := queryN(t, s, tb, "x", nil, false)
		if !reflect.DeepEqual(skSet(rev), []string{"3", "2", "1"}) {
			t.Fatalf("reverse = %v", skSet(rev))
		}
	})

	t.Run("query limit omits lastkey on final page", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2")
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true, Limit: storage.PageLimit{Items: 2}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("got %d items, want 2", len(page.Items))
		}
		// The limit was reached but there is no further item in the key range,
		// so DynamoDB returns no LastEvaluatedKey.
		if page.LastKey != nil {
			t.Fatalf("LastKey = %#v, want nil on the final page", page.LastKey)
		}
	})

	t.Run("query limit sets lastkey when more items remain", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3")
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true, Limit: storage.PageLimit{Items: 2}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("got %d items, want 2", len(page.Items))
		}
		if page.LastKey == nil {
			t.Fatalf("LastKey is nil even though a third item remains")
		}
		if page.LastKey["sk"].N != "2" {
			t.Fatalf("LastKey sk = %#v, want 2", page.LastKey["sk"])
		}
	})

	t.Run("query startafter continues", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3", "4")
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true, StartAfter: item("id", sv("x"), "sk", nv("2"))})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !reflect.DeepEqual(skSet(page.Items), []string{"3", "4"}) {
			t.Fatalf("got %v", skSet(page.Items))
		}
	})

	t.Run("query paging limit 1 visits all", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := rangeTable(t, s, "id", "sk", attr.TypeN, "1", "2", "3", "4", "5")
		var got []string
		var start attr.Item
		for {
			page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true, StartAfter: start, Limit: storage.PageLimit{Items: 1}})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(page.Items) > 1 {
				t.Fatalf("page has %d items, want at most 1", len(page.Items))
			}
			if len(page.Items) == 0 {
				break
			}
			got = append(got, page.Items[0]["sk"].N)
			start = page.LastKey
			if start == nil {
				break
			}
		}
		if !reflect.DeepEqual(got, []string{"1", "2", "3", "4", "5"}) {
			t.Fatalf("visited %v", got)
		}
	})

	t.Run("query limit bytes returns at least one", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeN))
		// Each item is id + sk + a payload attribute of 100 bytes.
		for _, sk := range []string{"1", "2", "3", "4"} {
			put(t, s, tb, storage.Key{Hash: sv("x"), Range: nv(sk)}, item("id", sv("x"), "sk", nv(sk), "p", sv(string(make([]byte, 100)))))
		}
		sizeOne := attr.ItemSize(item("id", sv("x"), "sk", nv("1"), "p", sv(string(make([]byte, 100)))))
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true, Limit: storage.PageLimit{Bytes: sizeOne*2 + 10}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) < 1 {
			t.Fatalf("bytes limit returned no items")
		}
		if len(page.Items) > 2 {
			t.Fatalf("bytes limit returned %d items, want at most 2", len(page.Items))
		}
		if page.LastKey == nil {
			t.Fatalf("LastKey nil after bytes truncation")
		}
		// A limit smaller than one item still returns one item.
		page, err = s.Query(ctx, storage.QueryInput{Table: tb, Hash: sv("x"), Forward: true, Limit: storage.PageLimit{Bytes: 1}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("bytes limit of 1 returned %d items, want 1", len(page.Items))
		}
	})

	t.Run("query number sort order", func(t *testing.T) {
		s := newStore(t)
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeN))
		for _, sk := range []string{"3", "-10", "25", "0", "-2"} {
			put(t, s, tb, storage.Key{Hash: sv("x"), Range: nv(sk)}, item("id", sv("x"), "sk", nv(sk)))
		}
		got := queryN(t, s, tb, "x", nil, true)
		want := []string{"-10", "-2", "0", "3", "25"}
		if !reflect.DeepEqual(skSet(got), want) {
			t.Fatalf("got %v, want %v", skSet(got), want)
		}
	})

	t.Run("query string sort order", func(t *testing.T) {
		s := newStore(t)
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS))
		for _, sk := range []string{"a", "é", "B"} {
			put(t, s, tb, storage.Key{Hash: sv("x"), Range: sv(sk)}, item("id", sv("x"), "sk", sv(sk)))
		}
		got := queryStr(t, s, tb, "x", nil, true)
		want := []string{"B", "a", "é"}
		if !reflect.DeepEqual(skSet(got), want) {
			t.Fatalf("got %v, want %v", skSet(got), want)
		}
	})

	// --- Scan ---
	t.Run("scan returns every item once", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		var want []string
		for i := 0; i < 7; i++ {
			name := fmt.Sprintf("id%02d", i)
			put(t, s, tb, keyS(name, ""), item("id", sv(name)))
			want = append(want, name)
		}
		var got []string
		var start attr.Item
		for {
			page, err := s.Scan(ctx, storage.ScanInput{Table: tb, StartAfter: start, Limit: storage.PageLimit{Items: 2}})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			for _, it := range page.Items {
				got = append(got, it["id"].S)
			}
			start = page.LastKey
			if start == nil {
				break
			}
		}
		if len(got) != len(want) {
			t.Fatalf("got %d items, want %d", len(got), len(want))
		}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("scan order", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS))
		for _, id := range []string{"a", "b", "c"} {
			put(t, s, tb, storage.Key{Hash: sv(id), Range: sv("1")}, item("id", sv(id), "sk", sv("1")))
		}
		page, err := s.Scan(ctx, storage.ScanInput{Table: tb})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		var ids []string
		for _, it := range page.Items {
			ids = append(ids, it["id"].S)
		}
		// Order must be pk_hash(pk) then pk then sk; verify it is sorted.
		if !sort.SliceIsSorted(page.Items, func(i, j int) bool {
			return cmpScanItems(page.Items[i], page.Items[j], tb) < 0
		}) {
			t.Fatalf("scan not in storage order: %v", ids)
		}
	})

	t.Run("scan segments split", func(t *testing.T) {
		for _, segments := range []int{1, 3, 4} {
			segments := segments
			t.Run(fmt.Sprintf("segments %d", segments), func(t *testing.T) {
				s := newStore(t)
				ctx := context.Background()
				tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS))
				var all []attr.Item
				for i := 0; i < 30; i++ {
					id := fmt.Sprintf("id%02d", i)
					it := item("id", sv(id), "sk", sv(fmt.Sprintf("%d", i)))
					put(t, s, tb, storage.Key{Hash: sv(id), Range: sv(fmt.Sprintf("%d", i))}, it)
					all = append(all, it)
				}
				seen := make(map[string]bool)
				for seg := 0; seg < segments; seg++ {
					page, err := s.Scan(ctx, storage.ScanInput{Table: tb, Segment: seg, Segments: segments})
					if err != nil {
						t.Fatalf("Scan: %v", err)
					}
					lo := scanLo(seg, segments)
					hi := scanLo(seg+1, segments)
					for _, it := range page.Items {
						h := int64(attr.KeyHash(mustEncode(t, it["id"])))
						if h < lo || h >= hi {
							t.Fatalf("segment %d returned item with hash %d outside [%d,%d)", seg, h, lo, hi)
						}
						name := it["id"].S
						if seen[name] {
							t.Fatalf("item %s seen twice", name)
						}
						seen[name] = true
					}
				}
				if len(seen) != len(all) {
					t.Fatalf("segments covered %d of %d items", len(seen), len(all))
				}
			})
		}
	})

	// --- GSIs ---
	t.Run("gsi sparse membership", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeN, gsi("byName", "name", attr.TypeS, "", 0)))
		put(t, s, tb, keyS("a", "1"), item("id", sv("a"), "sk", nv("1"), "name", sv("alice")))
		put(t, s, tb, keyS("b", "1"), item("id", sv("b"), "sk", nv("1")))                  // no name -> not in index
		put(t, s, tb, keyS("c", "1"), item("id", sv("c"), "sk", nv("1"), "name", nv("5"))) // wrong type -> not in index
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("alice"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 1 || page.Items[0]["id"].S != "a" {
			t.Fatalf("got %#v, want only the alice item", page.Items)
		}
	})

	t.Run("gsi query order and ties", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS, gsi("byName", "name", attr.TypeS, "", 0)))
		// Same index hash, different base keys: ties broken by base hash then base range.
		put(t, s, tb, keyS("b", "2"), item("id", sv("b"), "sk", sv("2"), "name", sv("n")))
		put(t, s, tb, keyS("a", "1"), item("id", sv("a"), "sk", sv("1"), "name", sv("n")))
		put(t, s, tb, keyS("a", "0"), item("id", sv("a"), "sk", sv("0"), "name", sv("n")))
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("n"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		var got []string
		for _, it := range page.Items {
			got = append(got, it["id"].S+it["sk"].S)
		}
		want := []string{"a0", "a1", "b2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("gsi lastkey and startafter", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS, gsi("byName", "name", attr.TypeS, "", 0)))
		for _, id := range []string{"a", "b", "c"} {
			put(t, s, tb, keyS(id, "1"), item("id", sv(id), "sk", sv("1"), "name", sv("n")))
		}
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("n"), Forward: true, Limit: storage.PageLimit{Items: 2}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("got %d items, want 2", len(page.Items))
		}
		if page.LastKey == nil {
			t.Fatalf("LastKey is nil")
		}
		if _, ok := page.LastKey["name"]; !ok {
			t.Fatalf("LastKey missing index key: %#v", page.LastKey)
		}
		if _, ok := page.LastKey["id"]; !ok {
			t.Fatalf("LastKey missing table key: %#v", page.LastKey)
		}
		page2, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("n"), Forward: true, StartAfter: page.LastKey, Limit: storage.PageLimit{Items: 10}})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page2.Items) != 1 || page2.Items[0]["id"].S != "c" {
			t.Fatalf("resume got %#v, want the c item", page2.Items)
		}
	})

	t.Run("gsi update index key moves item", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0, gsi("byName", "name", attr.TypeS, "", 0)))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "name", sv("old")))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "name", sv("new")))
		oldPage, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("old"), Forward: true})
		if err != nil {
			t.Fatalf("Query old: %v", err)
		}
		if len(oldPage.Items) != 0 {
			t.Fatalf("item still under old index key: %#v", oldPage.Items)
		}
		newPage, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("new"), Forward: true})
		if err != nil {
			t.Fatalf("Query new: %v", err)
		}
		if len(newPage.Items) != 1 || newPage.Items[0]["id"].S != "a" {
			t.Fatalf("item not under new index key: %#v", newPage.Items)
		}
	})

	t.Run("gsi delete removes", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0, gsi("byName", "name", attr.TypeS, "", 0)))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "name", sv("n")))
		del(t, s, tb, keyS("a", ""))
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("n"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("deleted item still in index: %#v", page.Items)
		}
	})

	t.Run("gsi added via update table backfills", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "name", sv("n")))
		put(t, s, tb, keyS("b", ""), item("id", sv("b"), "name", sv("n")))
		got, err := s.UpdateTable(ctx, "alpha", func(t *storage.Table) error {
			t.GSIs = append(t.GSIs, gsi("byName", "name", attr.TypeS, "", 0))
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateTable: %v", err)
		}
		if len(got.GSIs) != 1 || got.GSIs[0].ID == 0 {
			t.Fatalf("new GSI missing ID: %#v", got.GSIs)
		}
		// The backfill may run in the background: the GSI stays CREATING
		// until it is done.
		deadline := time.Now().Add(10 * time.Second)
		for got.GSIs[0].Status != storage.StatusActive {
			if got.GSIs[0].Status != storage.StatusCreating {
				t.Fatalf("new GSI status = %q, want CREATING or ACTIVE", got.GSIs[0].Status)
			}
			if time.Now().After(deadline) {
				t.Fatalf("new GSI still CREATING after 10 s")
			}
			time.Sleep(5 * time.Millisecond)
			if got, err = s.GetTable(ctx, "alpha"); err != nil {
				t.Fatalf("GetTable: %v", err)
			}
		}
		page, err := s.Query(ctx, storage.QueryInput{Table: got, Index: "byName", Hash: sv("n"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("backfill returned %d items, want 2", len(page.Items))
		}
	})

	t.Run("lsi query order and sparse membership", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		lsi := storage.Index{
			Name:       "byYear",
			Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS, Range: "year", RangeType: attr.TypeN},
			Projection: storage.ProjectAll,
			Local:      true,
		}
		tb := mustCreate(t, s, table("alpha", "id", attr.TypeS, "sk", attr.TypeS, lsi))
		put(t, s, tb, storage.Key{Hash: sv("x"), Range: sv("a")}, item("id", sv("x"), "sk", sv("a"), "year", nv("2000")))
		put(t, s, tb, storage.Key{Hash: sv("x"), Range: sv("b")}, item("id", sv("x"), "sk", sv("b"), "year", nv("1999")))
		put(t, s, tb, storage.Key{Hash: sv("x"), Range: sv("c")}, item("id", sv("x"), "sk", sv("c"), "year", nv("2001")))
		put(t, s, tb, storage.Key{Hash: sv("x"), Range: sv("d")}, item("id", sv("x"), "sk", sv("d"))) // missing year -> not in index
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byYear", Hash: sv("x"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		var got []string
		for _, it := range page.Items {
			got = append(got, it["year"].N)
		}
		if !reflect.DeepEqual(got, []string{"1999", "2000", "2001"}) {
			t.Fatalf("got %v, want [1999 2000 2001]", got)
		}
	})
}

// --- helpers ---

func table(name string, hash string, hashType attr.Type, rng string, rngType attr.Type, gsis ...storage.Index) *storage.Table {
	t := &storage.Table{
		Name:     name,
		Key:      storage.KeySchema{Hash: hash, HashType: hashType, Range: rng, RangeType: rngType},
		AttrDefs: map[string]attr.Type{hash: hashType},
		Status:   storage.StatusActive,
		Billing:  "PAY_PER_REQUEST",
	}
	if rng != "" {
		t.AttrDefs[rng] = rngType
	}
	t.GSIs = gsis
	return t
}

func gsi(name string, hash string, hashType attr.Type, rng string, rngType attr.Type) storage.Index {
	return storage.Index{
		Name:       name,
		Key:        storage.KeySchema{Hash: hash, HashType: hashType, Range: rng, RangeType: rngType},
		Projection: storage.ProjectAll,
	}
}

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

func bv(b []byte) attr.Value { return attr.Value{Type: attr.TypeB, B: append([]byte(nil), b...)} }

func keyS(hash, rng string) storage.Key {
	k := storage.Key{Hash: sv(hash)}
	if rng != "" {
		k.Range = sv(rng)
	}
	return k
}

func mustCreate(t *testing.T, s storage.Store, tb *storage.Table) *storage.Table {
	t.Helper()
	got, err := s.CreateTable(context.Background(), tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return got
}

func put(t *testing.T, s storage.Store, tb *storage.Table, k storage.Key, it attr.Item) {
	t.Helper()
	if _, _, err := s.WriteItem(context.Background(), putWrite(tb, k, it)); err != nil {
		t.Fatalf("put: %v", err)
	}
}

func putWrite(tb *storage.Table, k storage.Key, it attr.Item) storage.Write {
	return storage.Write{Table: tb, Key: k, Fn: func(attr.Item) (storage.WriteResult, error) {
		return storage.WriteResult{Op: storage.Put, Item: it}, nil
	}}
}

func del(t *testing.T, s storage.Store, tb *storage.Table, k storage.Key) {
	t.Helper()
	if _, _, err := s.WriteItem(context.Background(), storage.Write{Table: tb, Key: k, Fn: func(attr.Item) (storage.WriteResult, error) {
		return storage.WriteResult{Op: storage.Delete}, nil
	}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// rangeTable creates a hash+range table with hash "x" and puts one item per
// sort key in vals.
func rangeTable(t *testing.T, s storage.Store, hash, rng string, rngType attr.Type, vals ...string) *storage.Table {
	t.Helper()
	tb := mustCreate(t, s, table("alpha", hash, attr.TypeS, rng, rngType))
	for _, v := range vals {
		var rv attr.Value
		if rngType == attr.TypeN {
			rv = nv(v)
		} else {
			rv = sv(v)
		}
		put(t, s, tb, storage.Key{Hash: sv("x"), Range: rv}, item(hash, sv("x"), rng, rv))
	}
	return tb
}

func rangeCond(op string, vals ...attr.Value) *storage.RangeCond {
	return &storage.RangeCond{Op: op, Values: vals}
}

func queryN(t *testing.T, s storage.Store, tb *storage.Table, hash string, rc *storage.RangeCond, forward bool) []attr.Item {
	t.Helper()
	page, err := s.Query(context.Background(), storage.QueryInput{Table: tb, Hash: sv(hash), Range: rc, Forward: forward})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return page.Items
}

func queryStr(t *testing.T, s storage.Store, tb *storage.Table, hash string, rc *storage.RangeCond, forward bool) []attr.Item {
	return queryN(t, s, tb, hash, rc, forward)
}

// skSet extracts the "sk" attribute as a string (N values keep their text).
func skSet(items []attr.Item) []string {
	var out []string
	for _, it := range items {
		v := it["sk"]
		switch v.Type {
		case attr.TypeN:
			out = append(out, v.N)
		default:
			out = append(out, v.S)
		}
	}
	return out
}

func mustEncode(t *testing.T, v attr.Value) []byte {
	t.Helper()
	b, err := attr.EncodeKey(v)
	if err != nil {
		t.Fatalf("EncodeKey: %v", err)
	}
	return b
}

// scanLo is the contract's segment lower bound, computed in int64.
func scanLo(i, segments int) int64 {
	return int64(math.MinInt32) + (int64(i)*(int64(1)<<32))/int64(segments)
}

// cmpScanItems reproduces Scan order for a base table: pk_hash, then pk, then sk.
func cmpScanItems(a, b attr.Item, tb *storage.Table) int {
	ah := attr.KeyHash(mustEncodeValue(a[tb.Key.Hash]))
	bh := attr.KeyHash(mustEncodeValue(b[tb.Key.Hash]))
	if ah != bh {
		if ah < bh {
			return -1
		}
		return 1
	}
	apk := string(mustEncodeValue(a[tb.Key.Hash]))
	bpk := string(mustEncodeValue(b[tb.Key.Hash]))
	if apk != bpk {
		if apk < bpk {
			return -1
		}
		return 1
	}
	var ask, bsk string
	if tb.Key.Range != "" {
		ask = string(mustEncodeValue(a[tb.Key.Range]))
		bsk = string(mustEncodeValue(b[tb.Key.Range]))
	}
	if ask != bsk {
		if ask < bsk {
			return -1
		}
		return 1
	}
	return 0
}

func mustEncodeValue(v attr.Value) []byte {
	b, _ := attr.EncodeKey(v)
	return b
}

// RunStreams covers the optional StreamStore: stream records, read paging,
// trimming, checkpoints and leases. It skips when the store does not implement
// StreamStore.
func RunStreams(t *testing.T, newStore New) {
	t.Run("stream end", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, streamTable("s", storage.StreamNewImage))
		if end, err := ss.StreamEnd(ctx, tb); err != nil || end != 0 {
			t.Fatalf("StreamEnd of an empty stream = %d, %v; want 0", end, err)
		}
		put(t, s, tb, keyS("a", ""), item("id", sv("a")))
		put(t, s, tb, keyS("b", ""), item("id", sv("b")))
		recs := readStreamAll(t, ss, tb)
		end, err := ss.StreamEnd(ctx, tb)
		if err != nil || len(recs) != 2 || end != recs[1].Seq {
			t.Fatalf("StreamEnd = %d, %v; want the last record's Seq (records %#v)", end, err, recs)
		}
		if more, err := ss.ReadStream(ctx, tb, end, 0); err != nil || len(more) != 0 {
			t.Fatalf("ReadStream after StreamEnd = %d records, %v; want none", len(more), err)
		}
	})
	t.Run("records by view", func(t *testing.T) {
		for _, view := range []string{storage.StreamKeysOnly, storage.StreamNewImage, storage.StreamOldImage, storage.StreamNewAndOld} {
			t.Run(view, func(t *testing.T) {
				s := newStore(t)
				ss, ok := s.(storage.StreamStore)
				if !ok {
					t.Skip("store does not implement StreamStore")
				}
				ctx := context.Background()
				tb := mustCreate(t, s, streamTable("s", view))
				put(t, s, tb, keyS("a", ""), item("id", sv("a"), "v", sv("1"))) // INSERT
				put(t, s, tb, keyS("a", ""), item("id", sv("a"), "v", sv("2"))) // MODIFY
				put(t, s, tb, keyS("a", ""), item("id", sv("a"), "v", sv("2"))) // identical: no record
				del(t, s, tb, keyS("a", ""))                                    // REMOVE
				del(t, s, tb, keyS("b", ""))                                    // missing: no record
				if err := s.Transact(ctx, []storage.Write{
					putWrite(tb, keyS("c", ""), item("id", sv("c"), "v", sv("3"))),
					putWrite(tb, keyS("d", ""), item("id", sv("d"), "v", sv("4"))),
				}); err != nil {
					t.Fatalf("Transact: %v", err)
				}

				recs := readStreamAll(t, ss, tb)
				want := []struct {
					event string
					keys  attr.Item
					old   attr.Item
					new   attr.Item
				}{
					{"INSERT", item("id", sv("a")), nil, item("id", sv("a"), "v", sv("1"))},
					{"MODIFY", item("id", sv("a")), item("id", sv("a"), "v", sv("1")), item("id", sv("a"), "v", sv("2"))},
					{"REMOVE", item("id", sv("a")), item("id", sv("a"), "v", sv("2")), nil},
					{"INSERT", item("id", sv("c")), nil, item("id", sv("c"), "v", sv("3"))},
					{"INSERT", item("id", sv("d")), nil, item("id", sv("d"), "v", sv("4"))},
				}
				if len(recs) != len(want) {
					t.Fatalf("got %d records, want %d: %#v", len(recs), len(want), recs)
				}
				wantsOld := view == storage.StreamOldImage || view == storage.StreamNewAndOld
				wantsNew := view == storage.StreamNewImage || view == storage.StreamNewAndOld
				for i, w := range want {
					r := recs[i]
					if r.Event != w.event {
						t.Fatalf("record %d event = %q, want %q", i, r.Event, w.event)
					}
					if r.Seq <= 0 {
						t.Fatalf("record %d Seq = %d, want > 0", i, r.Seq)
					}
					if i > 0 && r.Seq <= recs[i-1].Seq {
						t.Fatalf("record %d Seq not increasing", i)
					}
					if r.Size <= 0 {
						t.Fatalf("record %d Size = %d, want > 0", i, r.Size)
					}
					if r.TTL {
						t.Fatalf("record %d TTL = true, want false", i)
					}
					if !reflect.DeepEqual(r.Keys, w.keys) {
						t.Fatalf("record %d Keys = %#v, want %#v", i, r.Keys, w.keys)
					}
					checkImage(t, wantsOld, w.old, r.Old)
					checkImage(t, wantsNew, w.new, r.New)
				}
			})
		}
	})

	t.Run("table without stream gets none", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		tb := mustCreate(t, s, table("quiet", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("a", ""), item("id", sv("a")))
		if recs := readStreamAll(t, ss, tb); len(recs) != 0 {
			t.Fatalf("table without stream has %d records", len(recs))
		}
	})

	t.Run("read stream paging", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, streamTable("s", storage.StreamNewImage))
		for i := 0; i < 7; i++ {
			put(t, s, tb, keyS(fmt.Sprintf("id%02d", i), ""), item("id", sv(fmt.Sprintf("id%02d", i))))
		}
		var seen []int64
		after := int64(0)
		for {
			recs, err := ss.ReadStream(ctx, tb, after, 2)
			if err != nil {
				t.Fatalf("ReadStream: %v", err)
			}
			if len(recs) > 2 {
				t.Fatalf("page has %d records, want at most 2", len(recs))
			}
			if len(recs) == 0 {
				break
			}
			for _, r := range recs {
				if r.Seq <= after {
					t.Fatalf("record Seq %d <= after %d", r.Seq, after)
				}
				seen = append(seen, r.Seq)
			}
			after = recs[len(recs)-1].Seq
		}
		if len(seen) != 7 {
			t.Fatalf("visited %d records, want 7", len(seen))
		}
		for i := 1; i < len(seen); i++ {
			if seen[i] <= seen[i-1] {
				t.Fatalf("records not in Seq order: %v", seen)
			}
		}

		// Tables are separate: a second table's records do not leak in.
		tb2 := mustCreate(t, s, streamTable("s2", storage.StreamNewImage))
		put(t, s, tb2, keyS("x", ""), item("id", sv("x")))
		if recs := readStreamAll(t, ss, tb); len(recs) != 7 {
			t.Fatalf("first table now has %d records, want 7", len(recs))
		}
		recs2 := readStreamAll(t, ss, tb2)
		if len(recs2) != 1 || recs2[0].Keys["id"].S != "x" {
			t.Fatalf("second table records = %#v, want one record for x", recs2)
		}
	})

	t.Run("trim before cutoff", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, streamTable("s", storage.StreamNewImage))
		put(t, s, tb, keyS("a", ""), item("id", sv("a")))
		put(t, s, tb, keyS("b", ""), item("id", sv("b")))
		time.Sleep(10 * time.Millisecond)
		cutoff := time.Now()
		time.Sleep(10 * time.Millisecond)
		put(t, s, tb, keyS("c", ""), item("id", sv("c")))
		put(t, s, tb, keyS("d", ""), item("id", sv("d")))

		n, err := ss.TrimStreams(ctx, cutoff, 100)
		if err != nil {
			t.Fatalf("TrimStreams: %v", err)
		}
		if n != 2 {
			t.Fatalf("TrimStreams deleted %d, want 2", n)
		}
		recs := readStreamAll(t, ss, tb)
		if len(recs) != 2 {
			t.Fatalf("got %d records after trim, want 2", len(recs))
		}
	})

	t.Run("trim respects limit", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, streamTable("s", storage.StreamNewImage))
		for i := 0; i < 3; i++ {
			put(t, s, tb, keyS(fmt.Sprintf("k%d", i), ""), item("id", sv(fmt.Sprintf("k%d", i))))
		}
		future := time.Now().Add(time.Hour)
		n1, err := ss.TrimStreams(ctx, future, 2)
		if err != nil {
			t.Fatalf("TrimStreams: %v", err)
		}
		n2, err := ss.TrimStreams(ctx, future, 2)
		if err != nil {
			t.Fatalf("TrimStreams: %v", err)
		}
		n3, err := ss.TrimStreams(ctx, future, 2)
		if err != nil {
			t.Fatalf("TrimStreams: %v", err)
		}
		if n1 != 2 || n2 != 1 || n3 != 0 {
			t.Fatalf("TrimStreams counts = %d, %d, %d; want 2, 1, 0", n1, n2, n3)
		}
		if recs := readStreamAll(t, ss, tb); len(recs) != 0 {
			t.Fatalf("%d records remain after trim", len(recs))
		}
	})

	t.Run("checkpoint", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, streamTable("s", storage.StreamNewImage))
		tb2 := mustCreate(t, s, streamTable("s2", storage.StreamNewImage))

		if cp, err := ss.Checkpoint(ctx, "c1", tb); err != nil || cp != 0 {
			t.Fatalf("first Checkpoint = %d, %v; want 0, nil", cp, err)
		}
		if err := ss.SaveCheckpoint(ctx, "c1", tb, 42); err != nil {
			t.Fatalf("SaveCheckpoint: %v", err)
		}
		if cp, err := ss.Checkpoint(ctx, "c1", tb); err != nil || cp != 42 {
			t.Fatalf("Checkpoint = %d, %v; want 42, nil", cp, err)
		}
		if cp, err := ss.Checkpoint(ctx, "c2", tb); err != nil || cp != 0 {
			t.Fatalf("other consumer Checkpoint = %d, %v; want 0, nil", cp, err)
		}
		if cp, err := ss.Checkpoint(ctx, "c1", tb2); err != nil || cp != 0 {
			t.Fatalf("other table Checkpoint = %d, %v; want 0, nil", cp, err)
		}
		if err := ss.SaveCheckpoint(ctx, "c1", tb, 43); err != nil {
			t.Fatalf("SaveCheckpoint overwrite: %v", err)
		}
		if cp, err := ss.Checkpoint(ctx, "c1", tb); err != nil || cp != 43 {
			t.Fatalf("Checkpoint after overwrite = %d, %v; want 43, nil", cp, err)
		}
	})

	t.Run("lease", func(t *testing.T) {
		s := newStore(t)
		ss, ok := s.(storage.StreamStore)
		if !ok {
			t.Skip("store does not implement StreamStore")
		}
		ctx := context.Background()
		ok, err := ss.TryLease(ctx, "job", "A", time.Hour)
		if err != nil || !ok {
			t.Fatalf("TryLease A = %v, %v; want true, nil", ok, err)
		}
		ok, err = ss.TryLease(ctx, "job", "B", time.Hour)
		if err != nil || ok {
			t.Fatalf("TryLease B while A holds = %v, %v; want false, nil", ok, err)
		}
		ok, err = ss.TryLease(ctx, "job", "A", time.Hour)
		if err != nil || !ok {
			t.Fatalf("TryLease A renew = %v, %v; want true, nil", ok, err)
		}
		ok, err = ss.TryLease(ctx, "job2", "A", 50*time.Millisecond)
		if err != nil || !ok {
			t.Fatalf("TryLease job2 A = %v, %v; want true, nil", ok, err)
		}
		time.Sleep(100 * time.Millisecond)
		ok, err = ss.TryLease(ctx, "job2", "B", time.Hour)
		if err != nil || !ok {
			t.Fatalf("TryLease job2 B after expiry = %v, %v; want true, nil", ok, err)
		}
	})
}

// RunTTL covers the optional TTLStore. It skips when the store does not
// implement TTLStore.
func RunTTL(t *testing.T, newStore New) {
	t.Run("expiry", func(t *testing.T) {
		s := newStore(t)
		ts, ok := s.(storage.TTLStore)
		if !ok {
			t.Skip("store does not implement TTLStore")
		}
		ctx := context.Background()
		now := time.Now()
		past := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
		future := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)

		tb := mustCreate(t, s, ttlTable("ttl", true, gsi("byName", "name", attr.TypeS, "", 0)))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "expires", nv(past), "name", sv("n")))
		put(t, s, tb, keyS("b", ""), item("id", sv("b"), "expires", nv(future)))
		put(t, s, tb, keyS("c", ""), item("id", sv("c"), "expires", sv("not-a-number")))
		put(t, s, tb, keyS("e", ""), item("id", sv("e")))

		n, err := ts.ExpireItems(ctx, now, 0)
		if err != nil {
			t.Fatalf("ExpireItems: %v", err)
		}
		if n != 1 {
			t.Fatalf("ExpireItems deleted %d, want 1", n)
		}
		if got, err := s.GetItem(ctx, tb, keyS("a", "")); err != nil || got != nil {
			t.Fatalf("expired item still present: %#v, %v", got, err)
		}
		for _, k := range []string{"b", "c", "e"} {
			if got, err := s.GetItem(ctx, tb, keyS(k, "")); err != nil || got == nil {
				t.Fatalf("item %s expired or unreadable: %#v, %v", k, got, err)
			}
		}

		// Expiry removed the item from the GSI.
		page, err := s.Query(ctx, storage.QueryInput{Table: tb, Index: "byName", Hash: sv("n"), Forward: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("expired item still in GSI: %#v", page.Items)
		}

		// Expiry wrote a REMOVE stream record with TTL set.
		if ss, ok := s.(storage.StreamStore); ok {
			found := false
			for _, r := range readStreamAll(t, ss, tb) {
				if r.Event == "REMOVE" && r.TTL && r.Keys["id"].S == "a" {
					found = true
				}
			}
			if !found {
				t.Fatalf("no REMOVE record with TTL set for a")
			}
		}
	})

	t.Run("disabled table", func(t *testing.T) {
		s := newStore(t)
		ts, ok := s.(storage.TTLStore)
		if !ok {
			t.Skip("store does not implement TTLStore")
		}
		ctx := context.Background()
		now := time.Now()
		past := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
		tb := mustCreate(t, s, table("off", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("d", ""), item("id", sv("d"), "expires", nv(past)))
		n, err := ts.ExpireItems(ctx, now, 0)
		if err != nil {
			t.Fatalf("ExpireItems: %v", err)
		}
		if n != 0 {
			t.Fatalf("ExpireItems deleted %d on a TTL-disabled table, want 0", n)
		}
		if got, err := s.GetItem(ctx, tb, keyS("d", "")); err != nil || got == nil {
			t.Fatalf("disabled-table item expired: %#v, %v", got, err)
		}
	})

	t.Run("enabling ttl recomputes", func(t *testing.T) {
		s := newStore(t)
		ts, ok := s.(storage.TTLStore)
		if !ok {
			t.Skip("store does not implement TTLStore")
		}
		ctx := context.Background()
		now := time.Now()
		past := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
		tb := mustCreate(t, s, table("en", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "expires", nv(past)))

		got, err := s.UpdateTable(ctx, "en", func(t *storage.Table) error {
			t.TTLAttr = "expires"
			t.TTLEnabled = true
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateTable: %v", err)
		}

		deadline := time.Now().Add(10 * time.Second)
		for {
			n, err := ts.ExpireItems(ctx, now, 0)
			if err != nil {
				t.Fatalf("ExpireItems: %v", err)
			}
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("item not eligible after enabling TTL (10 s)")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if item, err := s.GetItem(ctx, got, keyS("a", "")); err != nil || item != nil {
			t.Fatalf("enabled-table item not expired: %#v, %v", item, err)
		}
	})

	t.Run("limit respected", func(t *testing.T) {
		s := newStore(t)
		ts, ok := s.(storage.TTLStore)
		if !ok {
			t.Skip("store does not implement TTLStore")
		}
		ctx := context.Background()
		now := time.Now()
		past := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
		tb := mustCreate(t, s, ttlTable("l", false))
		for i := 0; i < 3; i++ {
			id := fmt.Sprintf("a%d", i)
			put(t, s, tb, keyS(id, ""), item("id", sv(id), "expires", nv(past)))
		}
		n1, err := ts.ExpireItems(ctx, now, 2)
		if err != nil {
			t.Fatalf("ExpireItems: %v", err)
		}
		n2, err := ts.ExpireItems(ctx, now, 2)
		if err != nil {
			t.Fatalf("ExpireItems: %v", err)
		}
		n3, err := ts.ExpireItems(ctx, now, 2)
		if err != nil {
			t.Fatalf("ExpireItems: %v", err)
		}
		if n1 != 2 || n2 != 1 || n3 != 0 {
			t.Fatalf("ExpireItems counts = %d, %d, %d; want 2, 1, 0", n1, n2, n3)
		}
	})
}

// RunPITR covers the optional PITRStore. It skips when the store does not
// implement PITRStore.
func RunPITR(t *testing.T, newStore New) {
	t.Run("restore to points in time", func(t *testing.T) {
		s := newStore(t)
		ps, ok := s.(storage.PITRStore)
		if !ok {
			t.Skip("store does not implement PITRStore")
		}
		ctx := context.Background()

		tb := mustCreate(t, s, table("pitr", "id", attr.TypeS, "", 0, gsi("byName", "name", attr.TypeS, "", 0)))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "name", sv("n1"), "v", sv("0")))
		put(t, s, tb, keyS("b", ""), item("id", sv("b"), "name", sv("n1"), "v", sv("0")))
		put(t, s, tb, keyS("c", ""), item("id", sv("c"), "name", sv("n1"), "v", sv("0")))

		// Recovery is off before it is enabled.
		if _, _, err := ps.RecoveryWindow(ctx, tb); !errors.Is(err, storage.ErrOutsideRecoveryWindow) {
			t.Fatalf("RecoveryWindow before enabling = %v, want ErrOutsideRecoveryWindow", err)
		}

		// Enable PITR; the caller sets PITRSince.
		enableAt := time.Now()
		got, err := s.UpdateTable(ctx, "pitr", func(t *storage.Table) error {
			t.PITREnabled = true
			t.PITRDays = 35
			t.PITRSince = enableAt
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateTable: %v", err)
		}
		tb = got

		// Wait for the initial copy and a window wide enough to place markers in.
		waitPITR(t, ps, tb)

		change1At := time.Now()
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "name", sv("n1"), "v", sv("1")))
		del(t, s, tb, keyS("b", ""))
		time.Sleep(500 * time.Millisecond)
		change2At := time.Now()
		put(t, s, tb, keyS("b", ""), item("id", sv("b"), "name", sv("n1"), "v", sv("1")))
		put(t, s, tb, keyS("c", ""), item("id", sv("c"), "name", sv("n2"), "v", sv("0")))
		time.Sleep(500 * time.Millisecond)

		// The latest restorable time trails now by 5s; wait for the "after" marker
		// to fall inside the window.
		waitPITRLatest(t, ps, tb, change2At.Add(300*time.Millisecond))

		earliest, _, err := ps.RecoveryWindow(ctx, tb)
		if err != nil {
			t.Fatalf("RecoveryWindow: %v", err)
		}

		before := earliest.Add(time.Second) // after enable, before change1
		between := change1At.Add(250 * time.Millisecond)
		after := change2At.Add(250 * time.Millisecond)

		check := func(name string, at time.Time, want map[string]attr.Item, gsi map[string][]string) {
			t.Helper()
			rt, err := ps.RestoreToTime(ctx, tb, at, restoreTarget(name, tb))
			if err != nil {
				t.Fatalf("RestoreToTime(%s): %v", name, err)
			}
			// The restore may fill the table in the background: it is
			// CREATING until then (storage.go).
			deadline := time.Now().Add(30 * time.Second)
			for rt.Status != storage.StatusActive {
				if rt.Status != storage.StatusCreating || time.Now().After(deadline) {
					t.Fatalf("restored table %s status = %q, want CREATING then ACTIVE", name, rt.Status)
				}
				time.Sleep(20 * time.Millisecond)
				if rt, err = s.GetTable(ctx, rt.Name); err != nil {
					t.Fatalf("GetTable(%s): %v", name, err)
				}
			}
			for id, wantItem := range want {
				gotItem, err := s.GetItem(ctx, rt, keyS(id, ""))
				if err != nil {
					t.Fatalf("GetItem(%s, %s): %v", name, id, err)
				}
				if !reflect.DeepEqual(gotItem, wantItem) {
					t.Fatalf("restore %s item %s = %#v, want %#v", name, id, gotItem, wantItem)
				}
			}
			for gsiHash, wantIDs := range gsi {
				page, err := s.Query(ctx, storage.QueryInput{Table: rt, Index: "byName", Hash: sv(gsiHash), Forward: true})
				if err != nil {
					t.Fatalf("Query(%s, %s): %v", name, gsiHash, err)
				}
				var gotIDs []string
				for _, it := range page.Items {
					gotIDs = append(gotIDs, it["id"].S)
				}
				sort.Strings(gotIDs)
				sort.Strings(wantIDs)
				if !reflect.DeepEqual(gotIDs, wantIDs) {
					t.Fatalf("restore %s GSI %s = %v, want %v", name, gsiHash, gotIDs, wantIDs)
				}
			}
		}

		check("before", before,
			map[string]attr.Item{
				"a": item("id", sv("a"), "name", sv("n1"), "v", sv("0")),
				"b": item("id", sv("b"), "name", sv("n1"), "v", sv("0")),
				"c": item("id", sv("c"), "name", sv("n1"), "v", sv("0")),
			},
			map[string][]string{"n1": {"a", "b", "c"}})

		check("between", between,
			map[string]attr.Item{
				"a": item("id", sv("a"), "name", sv("n1"), "v", sv("1")),
				"c": item("id", sv("c"), "name", sv("n1"), "v", sv("0")),
			},
			map[string][]string{"n1": {"a", "c"}, "n2": nil})

		check("after", after,
			map[string]attr.Item{
				"a": item("id", sv("a"), "name", sv("n1"), "v", sv("1")),
				"b": item("id", sv("b"), "name", sv("n1"), "v", sv("1")),
				"c": item("id", sv("c"), "name", sv("n2"), "v", sv("0")),
			},
			map[string][]string{"n1": {"a", "b"}, "n2": {"c"}})

		// Times outside the window fail.
		if _, err := ps.RestoreToTime(ctx, tb, enableAt.Add(-time.Hour), restoreTarget("too-early", tb)); !errors.Is(err, storage.ErrOutsideRecoveryWindow) {
			t.Fatalf("restore before the window = %v, want ErrOutsideRecoveryWindow", err)
		}
		if _, err := ps.RestoreToTime(ctx, tb, time.Now(), restoreTarget("too-late", tb)); !errors.Is(err, storage.ErrOutsideRecoveryWindow) {
			t.Fatalf("restore after the window = %v, want ErrOutsideRecoveryWindow", err)
		}

		// TrimHistory must not delete versions the window still needs.
		if _, err := ps.TrimHistory(ctx, time.Now(), 0); err != nil {
			t.Fatalf("TrimHistory: %v", err)
		}
		check("after-trim", after,
			map[string]attr.Item{
				"a": item("id", sv("a"), "name", sv("n1"), "v", sv("1")),
				"b": item("id", sv("b"), "name", sv("n1"), "v", sv("1")),
				"c": item("id", sv("c"), "name", sv("n2"), "v", sv("0")),
			},
			map[string][]string{"n1": {"a", "b"}, "n2": {"c"}})
	})

	t.Run("trim history of a disabled table", func(t *testing.T) {
		s := newStore(t)
		ps, ok := s.(storage.PITRStore)
		if !ok {
			t.Skip("store does not implement PITRStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, table("off", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "v", sv("0")))
		enableAt := time.Now()
		got, err := s.UpdateTable(ctx, "off", func(t *storage.Table) error {
			t.PITREnabled = true
			t.PITRDays = 35
			t.PITRSince = enableAt
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateTable: %v", err)
		}
		tb = got
		waitPITR(t, ps, tb)
		put(t, s, tb, keyS("a", ""), item("id", sv("a"), "v", sv("1")))

		if _, err := s.UpdateTable(ctx, "off", func(t *storage.Table) error {
			t.PITREnabled = false
			return nil
		}); err != nil {
			t.Fatalf("UpdateTable disable: %v", err)
		}
		if _, _, err := ps.RecoveryWindow(ctx, tb); !errors.Is(err, storage.ErrOutsideRecoveryWindow) {
			t.Fatalf("RecoveryWindow after disabling = %v, want ErrOutsideRecoveryWindow", err)
		}
		n, err := ps.TrimHistory(ctx, time.Now(), 0)
		if err != nil {
			t.Fatalf("TrimHistory: %v", err)
		}
		if n == 0 {
			t.Fatal("TrimHistory deleted 0 versions of a disabled table, want > 0")
		}
	})
}

// RunBackups covers the optional BackupStore. It skips when the store does not
// implement BackupStore.
func RunBackups(t *testing.T, newStore New) {
	t.Run("backup and restore", func(t *testing.T) {
		s := newStore(t)
		bs, ok := s.(storage.BackupStore)
		if !ok {
			t.Skip("store does not implement BackupStore")
		}
		ctx := context.Background()
		src := mustCreate(t, s, backupTable("src"))

		const n = 1500
		want := make(map[string]attr.Item, n)
		var wantBytes int64
		for i := 0; i < n; i++ {
			it := backupItem(i)
			put(t, s, src, storage.Key{Hash: sv(fmt.Sprintf("id%04d", i)), Range: nv(strconv.Itoa(i))}, it)
			want[it["id"].S] = it
			wantBytes += int64(attr.ItemSize(it))
		}

		b, err := bs.CreateBackup(ctx, src, "bk-src")
		if err != nil {
			t.Fatalf("CreateBackup: %v", err)
		}
		if b.ID == "" || b.Name != "bk-src" || b.Table.Name != "src" {
			t.Fatalf("CreateBackup returned %#v", b)
		}

		// A write made after CreateBackup returned must not be in the snapshot.
		put(t, s, src, storage.Key{Hash: sv("id9999"), Range: nv("9999")}, backupItem(9999))

		b = pollBackup(t, bs, b.ID)
		if b.Items != n {
			t.Fatalf("backup Items = %d, want %d", b.Items, n)
		}
		if b.Bytes != wantBytes {
			t.Fatalf("backup Bytes = %d, want %d", b.Bytes, wantBytes)
		}

		// Restore into a new name: a fresh table ID, the snapshot's items, and
		// every GSI queryable once the table is ACTIVE.
		target := cloneTableDef(&b.Table)
		target.Name = "restored"
		restored, err := bs.RestoreBackup(ctx, b.ID, target)
		if err != nil {
			t.Fatalf("RestoreBackup: %v", err)
		}
		if restored.ID == 0 || restored.ID == src.ID {
			t.Fatalf("restored table ID = %d, want a fresh ID (src %d)", restored.ID, src.ID)
		}
		restored = pollTableActive(t, s, "restored")

		got := scanAll(t, s, restored)
		if len(got) != n {
			t.Fatalf("restored table has %d items, want %d", len(got), n)
		}
		for _, it := range got {
			w, ok := want[it["id"].S]
			if !ok || !reflect.DeepEqual(it, w) {
				t.Fatalf("restored item %s = %#v, want %#v", it["id"].S, it, w)
			}
		}

		if page, err := s.Query(ctx, storage.QueryInput{Table: restored, Index: "byName", Hash: sv("name0000"), Forward: true}); err != nil || len(page.Items) != 1 || page.Items[0]["id"].S != "id0000" {
			t.Fatalf("byName query = %#v, %v", page.Items, err)
		}
		if page, err := s.Query(ctx, storage.QueryInput{Table: restored, Index: "byAge", Hash: nv("5"), Forward: true}); err != nil || len(page.Items) != 15 {
			t.Fatalf("byAge query returned %d items, %v; want 15", len(page.Items), err)
		}
		if page, err := s.Query(ctx, storage.QueryInput{Table: restored, Index: "byCity", Hash: sv("city0010"), Forward: true}); err != nil || len(page.Items) != 1 || page.Items[0]["id"].S != "id0010" {
			t.Fatalf("byCity query = %#v, %v", page.Items, err)
		}

		// Deleting the source table leaves its backup intact.
		if _, err := s.DeleteTable(ctx, "src"); err != nil {
			t.Fatalf("DeleteTable: %v", err)
		}
		again, err := bs.GetBackup(ctx, b.ID)
		if err != nil {
			t.Fatalf("GetBackup after source delete: %v", err)
		}
		if again.Status != "AVAILABLE" || again.Items != n || again.Bytes != wantBytes {
			t.Fatalf("backup after source delete = %#v", again)
		}
	})

	t.Run("list by table range and paging", func(t *testing.T) {
		s := newStore(t)
		bs, ok := s.(storage.BackupStore)
		if !ok {
			t.Skip("store does not implement BackupStore")
		}
		ctx := context.Background()
		t1 := mustCreate(t, s, table("t1", "id", attr.TypeS, "", 0))
		t2 := mustCreate(t, s, table("t2", "id", attr.TypeS, "", 0))
		put(t, s, t1, keyS("a", ""), item("id", sv("a")))
		put(t, s, t2, keyS("b", ""), item("id", sv("b")))

		mk := func(tb *storage.Table, name string) storage.Backup {
			t.Helper()
			b, err := bs.CreateBackup(ctx, tb, name)
			if err != nil {
				t.Fatalf("CreateBackup %s: %v", name, err)
			}
			return *b
		}
		b0 := mk(t1, "b0")
		time.Sleep(20 * time.Millisecond)
		b1 := mk(t2, "b1")
		time.Sleep(20 * time.Millisecond)
		b2 := mk(t1, "b2")
		time.Sleep(20 * time.Millisecond)
		b3 := mk(t2, "b3")

		ids := func(bs []storage.Backup) []string {
			out := make([]string, len(bs))
			for i, b := range bs {
				out[i] = b.ID
			}
			return out
		}
		check := func(got []storage.Backup, want []storage.Backup) {
			t.Helper()
			if !reflect.DeepEqual(ids(got), ids(want)) {
				t.Fatalf("got %v, want %v", ids(got), ids(want))
			}
		}

		all, err := bs.ListBackups(ctx, "", time.Time{}, time.Time{}, "", 0)
		if err != nil {
			t.Fatalf("ListBackups: %v", err)
		}
		check(all, []storage.Backup{b0, b1, b2, b3})

		t1s, err := bs.ListBackups(ctx, "t1", time.Time{}, time.Time{}, "", 0)
		if err != nil {
			t.Fatalf("ListBackups t1: %v", err)
		}
		check(t1s, []storage.Backup{b0, b2})

		t2s, err := bs.ListBackups(ctx, "t2", time.Time{}, time.Time{}, "", 0)
		if err != nil {
			t.Fatalf("ListBackups t2: %v", err)
		}
		check(t2s, []storage.Backup{b1, b3})

		ranged, err := bs.ListBackups(ctx, "", b1.CreatedAt, b3.CreatedAt, "", 0)
		if err != nil {
			t.Fatalf("ListBackups range: %v", err)
		}
		check(ranged, []storage.Backup{b1, b2})

		toOnly, err := bs.ListBackups(ctx, "", time.Time{}, b2.CreatedAt, "", 0)
		if err != nil {
			t.Fatalf("ListBackups to: %v", err)
		}
		check(toOnly, []storage.Backup{b0, b1})

		page1, err := bs.ListBackups(ctx, "", time.Time{}, time.Time{}, "", 2)
		if err != nil {
			t.Fatalf("ListBackups page1: %v", err)
		}
		check(page1, []storage.Backup{b0, b1})

		page2, err := bs.ListBackups(ctx, "", time.Time{}, time.Time{}, b1.ID, 2)
		if err != nil {
			t.Fatalf("ListBackups page2: %v", err)
		}
		check(page2, []storage.Backup{b2, b3})

		page3, err := bs.ListBackups(ctx, "", time.Time{}, time.Time{}, b3.ID, 2)
		if err != nil {
			t.Fatalf("ListBackups page3: %v", err)
		}
		check(page3, nil)
	})

	t.Run("delete backup", func(t *testing.T) {
		s := newStore(t)
		bs, ok := s.(storage.BackupStore)
		if !ok {
			t.Skip("store does not implement BackupStore")
		}
		ctx := context.Background()
		tb := mustCreate(t, s, table("src", "id", attr.TypeS, "", 0))
		put(t, s, tb, keyS("a", ""), item("id", sv("a")))
		b, err := bs.CreateBackup(ctx, tb, "b")
		if err != nil {
			t.Fatalf("CreateBackup: %v", err)
		}
		got, err := bs.DeleteBackup(ctx, b.ID)
		if err != nil {
			t.Fatalf("DeleteBackup: %v", err)
		}
		if got.Status != "DELETED" {
			t.Fatalf("DeleteBackup status = %q, want DELETED", got.Status)
		}
		if _, err := bs.GetBackup(ctx, b.ID); !errors.Is(err, storage.ErrBackupNotFound) {
			t.Fatalf("GetBackup after delete = %v, want ErrBackupNotFound", err)
		}
	})

	t.Run("get missing backup", func(t *testing.T) {
		s := newStore(t)
		bs, ok := s.(storage.BackupStore)
		if !ok {
			t.Skip("store does not implement BackupStore")
		}
		if _, err := bs.GetBackup(context.Background(), "nope"); !errors.Is(err, storage.ErrBackupNotFound) {
			t.Fatalf("GetBackup = %v, want ErrBackupNotFound", err)
		}
	})

	t.Run("restore onto existing name", func(t *testing.T) {
		s := newStore(t)
		bs, ok := s.(storage.BackupStore)
		if !ok {
			t.Skip("store does not implement BackupStore")
		}
		ctx := context.Background()
		src := mustCreate(t, s, table("src", "id", attr.TypeS, "", 0))
		mustCreate(t, s, table("taken", "id", attr.TypeS, "", 0))
		put(t, s, src, keyS("a", ""), item("id", sv("a")))
		b, err := bs.CreateBackup(ctx, src, "b")
		if err != nil {
			t.Fatalf("CreateBackup: %v", err)
		}
		b = pollBackup(t, bs, b.ID)
		target := cloneTableDef(&b.Table)
		target.Name = "taken"
		if _, err := bs.RestoreBackup(ctx, b.ID, target); !errors.Is(err, storage.ErrTableExists) {
			t.Fatalf("RestoreBackup onto existing = %v, want ErrTableExists", err)
		}
	})
}

// waitPITR blocks until t's recovery window is open and at least 2s wide, so the
// test can place write markers inside it with room to spare.
func waitPITR(t *testing.T, ps storage.PITRStore, tb *storage.Table) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		earliest, latest, err := ps.RecoveryWindow(ctx, tb)
		if err == nil && latest.Sub(earliest) >= 2*time.Second {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery window never opened (earliest=%v latest=%v err=%v)", earliest, latest, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitPITRLatest blocks until t's latest restorable time reaches after.
func waitPITRLatest(t *testing.T, ps storage.PITRStore, tb *storage.Table, after time.Time) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, latest, err := ps.RecoveryWindow(ctx, tb)
		if err == nil && !latest.Before(after) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("latest restorable time never reached %v (latest=%v err=%v)", after, latest, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// restoreTarget builds a table that RestoreToTime should create: the same key
// schema, GSIs and attribute definitions as src, but with PITR still off.
func restoreTarget(name string, src *storage.Table) *storage.Table {
	t := table(name, src.Key.Hash, src.Key.HashType, src.Key.Range, src.Key.RangeType)
	for _, g := range src.GSIs {
		t.GSIs = append(t.GSIs, storage.Index{
			Name:        g.Name,
			Key:         g.Key,
			Projection:  g.Projection,
			NonKeyAttrs: g.NonKeyAttrs,
		})
	}
	t.AttrDefs = src.AttrDefs
	return t
}

// backupTable is a table with three GSIs, for backup and restore coverage.
func backupTable(name string) *storage.Table {
	return table(name, "id", attr.TypeS, "sk", attr.TypeN,
		gsi("byName", "name", attr.TypeS, "", 0),
		gsi("byAge", "age", attr.TypeN, "", 0),
		gsi("byCity", "city", attr.TypeS, "", 0))
}

func backupItem(i int) attr.Item {
	return item("id", sv(fmt.Sprintf("id%04d", i)),
		"sk", nv(strconv.Itoa(i)),
		"name", sv(fmt.Sprintf("name%04d", i)),
		"age", nv(strconv.Itoa(i%100)),
		"city", sv(fmt.Sprintf("city%04d", i)),
		"payload", sv(strings.Repeat("x", 32)))
}

// pollBackup waits for a backup to become AVAILABLE.
func pollBackup(t *testing.T, bs storage.BackupStore, id string) *storage.Backup {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := bs.GetBackup(context.Background(), id)
		if err != nil {
			t.Fatalf("GetBackup %s: %v", id, err)
		}
		if b.Status == "AVAILABLE" {
			return b
		}
		if b.Status != storage.StatusCreating {
			t.Fatalf("backup %s status = %q, want CREATING or AVAILABLE", id, b.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s still not AVAILABLE after 30s", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pollTableActive waits for a table and all its GSIs to become ACTIVE.
func pollTableActive(t *testing.T, s storage.Store, name string) *storage.Table {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		tb, err := s.GetTable(context.Background(), name)
		if err != nil {
			t.Fatalf("GetTable %s: %v", name, err)
		}
		if tb.Status == storage.StatusActive {
			ready := true
			for _, g := range tb.GSIs {
				if g.Status != storage.StatusActive {
					ready = false
				}
			}
			if ready {
				return tb
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("table %s not ACTIVE after 30s", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// scanAll reads every item of a table by paging through Scan.
func scanAll(t *testing.T, s storage.Store, tb *storage.Table) []attr.Item {
	t.Helper()
	var out []attr.Item
	var start attr.Item
	for {
		page, err := s.Scan(context.Background(), storage.ScanInput{Table: tb, StartAfter: start, Limit: storage.PageLimit{Items: 100}})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		out = append(out, page.Items...)
		start = page.LastKey
		if start == nil {
			return out
		}
	}
}

// cloneTableDef deep-copies a table definition so a test can derive a restore
// target from a backup's table without sharing its slices.
func cloneTableDef(t *storage.Table) *storage.Table {
	if t == nil {
		return nil
	}
	out := *t
	out.AttrDefs = make(map[string]attr.Type, len(t.AttrDefs))
	for k, v := range t.AttrDefs {
		out.AttrDefs[k] = v
	}
	out.GSIs = make([]storage.Index, len(t.GSIs))
	for i, g := range t.GSIs {
		out.GSIs[i] = g
		out.GSIs[i].NonKeyAttrs = append([]string(nil), g.NonKeyAttrs...)
	}
	out.Tags = make(map[string]string, len(t.Tags))
	for k, v := range t.Tags {
		out.Tags[k] = v
	}
	out.Replicas = append([]storage.Replica(nil), t.Replicas...)
	return &out
}

// streamTable is a table helper with a stream view set.
func streamTable(name, view string) *storage.Table {
	t := table(name, "id", attr.TypeS, "", 0)
	t.StreamView = view
	return t
}

// ttlTable is a table helper with TTL enabled on the "expires" attribute and,
// when stream is true, a stream.
func ttlTable(name string, stream bool, gsis ...storage.Index) *storage.Table {
	t := table(name, "id", attr.TypeS, "", 0, gsis...)
	t.TTLAttr = "expires"
	t.TTLEnabled = true
	if stream {
		t.StreamView = storage.StreamNewAndOld
	}
	return t
}

func readStreamAll(t *testing.T, ss storage.StreamStore, tb *storage.Table) []storage.StreamRecord {
	t.Helper()
	var out []storage.StreamRecord
	after := int64(0)
	for {
		recs, err := ss.ReadStream(context.Background(), tb, after, 1000)
		if err != nil {
			t.Fatalf("ReadStream: %v", err)
		}
		if len(recs) == 0 {
			return out
		}
		out = append(out, recs...)
		after = recs[len(recs)-1].Seq
	}
}

func checkImage(t *testing.T, wants bool, wantItem, got attr.Item) {
	t.Helper()
	if !wants {
		if got != nil {
			t.Fatalf("image = %#v, want nil", got)
		}
		return
	}
	if !reflect.DeepEqual(got, wantItem) {
		t.Fatalf("image = %#v, want %#v", got, wantItem)
	}
}
