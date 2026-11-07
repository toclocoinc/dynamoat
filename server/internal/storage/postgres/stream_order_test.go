package postgres

import (
	"context"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres/pgtest"
)

// A write whose transaction started first but commits last must not be
// skipped by a reader that already moved past the later write.
func TestStreamReaderDoesNotSkipLateCommit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, pgtest.DB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	tb, err := s.CreateTable(ctx, &storage.Table{
		Name:       "orders",
		Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:   map[string]attr.Type{"id": attr.TypeS},
		Billing:    "PAY_PER_REQUEST",
		StreamView: storage.StreamNewImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	put := func(id string) storage.Write {
		item := attr.Item{"id": {Type: attr.TypeS, S: id}}
		return storage.Write{Table: tb, Key: storage.Key{Hash: item["id"]}, Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{Op: storage.Put, Item: item}, nil
		}}
	}

	// The early transaction takes its stream seq first and stays open.
	early, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Rollback(ctx)
	if _, err := early.Exec(ctx,
		`INSERT INTO stream_records (table_id, event, keys, new_image, size) VALUES ($1, 'INSERT', $2, $2, 1)`,
		tb.ID, []byte(`{"id":{"S":"early"}}`)); err != nil {
		t.Fatal(err)
	}

	// A later write commits while the early one is still open.
	if _, _, err := s.WriteItem(ctx, put("late")); err != nil {
		t.Fatal(err)
	}
	recs, err := s.ReadStream(ctx, tb, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("read %d records while an earlier transaction is open; want 0 (held back)", len(recs))
	}

	if err := early.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	recs, err = s.ReadStream(ctx, tb, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("read %d records after both committed, want 2", len(recs))
	}
	if got := recs[0].Keys["id"].S; got != "early" {
		t.Errorf("first record is %q, want early (its transaction came first)", got)
	}
	if recs[0].Seq >= recs[1].Seq {
		t.Errorf("Seq not increasing: %d then %d", recs[0].Seq, recs[1].Seq)
	}

	// A reader checkpointed at the last record sees nothing more, and a new
	// write gets a higher Seq.
	last := recs[1].Seq
	if _, _, err := s.WriteItem(ctx, put("next")); err != nil {
		t.Fatal(err)
	}
	recs, err = s.ReadStream(ctx, tb, last, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Keys["id"].S != "next" || recs[0].Seq <= last {
		t.Fatalf("after checkpoint %d got %+v, want only next with a higher Seq", last, recs)
	}
}
