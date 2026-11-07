package jobs

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ttlStore returns a memstore with one TTL-enabled table ("ttl", keyed by id,
// TTL attribute "expires").
func ttlStore(t *testing.T) *memstore.Store {
	t.Helper()
	s := memstore.New()
	tb := &storage.Table{
		Name:       "ttl",
		Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:   map[string]attr.Type{"id": attr.TypeS},
		Status:     storage.StatusActive,
		Billing:    "PAY_PER_REQUEST",
		TTLAttr:    "expires",
		TTLEnabled: true,
	}
	if _, err := s.CreateTable(context.Background(), tb); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return s
}

func putItem(t *testing.T, s *memstore.Store, table, id string, expires time.Time) {
	t.Helper()
	tb, err := s.GetTable(context.Background(), table)
	if err != nil {
		t.Fatalf("GetTable(%q): %v", table, err)
	}
	it := attr.Item{"id": {Type: attr.TypeS, S: id}}
	if !expires.IsZero() {
		it["expires"] = attr.Value{Type: attr.TypeN, N: strconv.FormatInt(expires.Unix(), 10)}
	}
	key := storage.Key{Hash: attr.Value{Type: attr.TypeS, S: id}}
	_, _, err = s.WriteItem(context.Background(), storage.Write{
		Table: tb,
		Key:   key,
		Fn: func(attr.Item) (storage.WriteResult, error) {
			return storage.WriteResult{Op: storage.Put, Item: it}, nil
		},
	})
	if err != nil {
		t.Fatalf("WriteItem(%s): %v", id, err)
	}
}

func hasItem(t *testing.T, s *memstore.Store, table, id string) bool {
	t.Helper()
	tb, err := s.GetTable(context.Background(), table)
	if err != nil {
		t.Fatalf("GetTable(%q): %v", table, err)
	}
	it, err := s.GetItem(context.Background(), tb, storage.Key{Hash: attr.Value{Type: attr.TypeS, S: id}})
	if err != nil {
		t.Fatalf("GetItem(%s): %v", id, err)
	}
	return it != nil
}

func TestExpiredItemDisappears(t *testing.T) {
	s := ttlStore(t)
	putItem(t, s, "ttl", "a", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, s, discardLog(), Options{TTLInterval: 10 * time.Millisecond})

	deadline := time.Now().Add(5 * time.Second)
	for hasItem(t, s, "ttl", "a") {
		if time.Now().After(deadline) {
			t.Fatal("expired item did not disappear within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLeaseHeldByAnotherOwnerStopsJob(t *testing.T) {
	s := ttlStore(t)
	putItem(t, s, "ttl", "a", time.Now().Add(-time.Hour))

	if ok, err := s.TryLease(context.Background(), "ttl", "other-owner", time.Hour); err != nil || !ok {
		t.Fatalf("TryLease(other-owner) = %v, %v; want true, nil", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, s, discardLog(), Options{TTLInterval: 10 * time.Millisecond})

	time.Sleep(100 * time.Millisecond)
	if !hasItem(t, s, "ttl", "a") {
		t.Fatal("item expired even though another owner holds the ttl lease")
	}
}

func TestRunRecordsMetrics(t *testing.T) {
	s := ttlStore(t)
	putItem(t, s, "ttl", "a", time.Now().Add(-time.Hour))

	m := metrics.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, s, discardLog(), Options{TTLInterval: 10 * time.Millisecond, Metrics: m})

	deadline := time.Now().Add(5 * time.Second)
	for hasItem(t, s, "ttl", "a") {
		if time.Now().After(deadline) {
			t.Fatal("expired item did not disappear within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var b strings.Builder
	_ = m.WritePrometheus(&b)
	out := b.String()
	// The job keeps running every TTLInterval, so the ok count grows; the item
	// is expired exactly once, so the item count is pinned at 1.
	if !strings.Contains(out, `dynamoat_job_runs_total{job="ttl",result="ok"} `) {
		t.Errorf("metrics missing ok run:\n%s", out)
	}
	if !strings.Contains(out, `dynamoat_job_items_total{job="ttl"} 1`) {
		t.Errorf("metrics missing item count:\n%s", out)
	}
}

func TestCtxCancelStopsRun(t *testing.T) {
	s := ttlStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, s, discardLog(), Options{TTLInterval: time.Millisecond})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after ctx cancel")
	}
}

func TestStreamRetentionReachesTrimJob(t *testing.T) {
	oldTrimEvery := trimEvery
	trimEvery = 10 * time.Millisecond
	defer func() { trimEvery = oldTrimEvery }()

	s := memstore.New()
	tb := &storage.Table{
		Name:       "s",
		Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:   map[string]attr.Type{"id": attr.TypeS},
		Status:     storage.StatusActive,
		Billing:    "PAY_PER_REQUEST",
		StreamView: storage.StreamNewImage,
	}
	if _, err := s.CreateTable(context.Background(), tb); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	putItem(t, s, "s", "a", time.Time{})
	putItem(t, s, "s", "b", time.Time{})

	// A clock two hours ahead with a one-hour retention makes every record
	// written now older than the trim horizon.
	future := time.Now().Add(2 * time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, s, discardLog(), Options{
			StreamRetention: time.Hour,
			Now:             func() time.Time { return future },
		})
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		tb, _ = s.GetTable(context.Background(), "s")
		recs, err := s.ReadStream(context.Background(), tb, 0, 100)
		if err != nil {
			t.Fatalf("ReadStream: %v", err)
		}
		if len(recs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream records were not trimmed within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestHistoryTrimLoop(t *testing.T) {
	s := memstore.New()
	tb := &storage.Table{
		Name:     "h",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS},
		Status:   storage.StatusActive,
		Billing:  "PAY_PER_REQUEST",
	}
	if _, err := s.CreateTable(context.Background(), tb); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	now := time.Now()
	if _, err := s.UpdateTable(context.Background(), "h", func(t *storage.Table) error {
		t.PITREnabled = true
		t.PITRSince = now
		return nil
	}); err != nil {
		t.Fatalf("enable PITR: %v", err)
	}

	// Two writes produce two history versions.
	putItem(t, s, "h", "a", time.Time{})
	putItem(t, s, "h", "a", time.Time{})

	// Disable recovery so TrimHistory drops every version.
	if _, err := s.UpdateTable(context.Background(), "h", func(t *storage.Table) error {
		t.PITREnabled = false
		return nil
	}); err != nil {
		t.Fatalf("disable PITR: %v", err)
	}

	n, err := historyTrimLoop(context.Background(), s, time.Now())
	if err != nil {
		t.Fatalf("historyTrimLoop: %v", err)
	}
	if n != 2 {
		t.Fatalf("historyTrimLoop deleted %d, want 2", n)
	}
}

func TestTrimLoop(t *testing.T) {
	s := memstore.New()
	tb := &storage.Table{
		Name:       "s",
		Key:        storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:   map[string]attr.Type{"id": attr.TypeS},
		Status:     storage.StatusActive,
		Billing:    "PAY_PER_REQUEST",
		StreamView: storage.StreamNewImage,
	}
	if _, err := s.CreateTable(context.Background(), tb); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	putItem(t, s, "s", "a", time.Time{})
	putItem(t, s, "s", "b", time.Time{})

	// A cutoff in the future removes every record written so far.
	n, err := trimLoop(context.Background(), s, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("trimLoop: %v", err)
	}
	if n != 2 {
		t.Fatalf("trimLoop deleted %d, want 2", n)
	}
	tb, _ = s.GetTable(context.Background(), "s")
	recs, err := s.ReadStream(context.Background(), tb, 0, 100)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("%d stream records remain after trim, want 0", len(recs))
	}
}
