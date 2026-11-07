package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// On Postgres, concurrent writers commit in any order; the dispatcher must
// still deliver every change exactly once, in order per item.
func TestDispatchPostgresConcurrentWriters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := postgres.Open(ctx, pgtest.DB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	tb, err := s.CreateTable(ctx, &storage.Table{
		Name:        "cards",
		Key:         storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs:    map[string]attr.Type{"id": attr.TypeS},
		Billing:     "PAY_PER_REQUEST",
		StreamView:  storage.StreamNewImage,
		StreamLabel: "2026-11-06T00:00:00.000",
	})
	if err != nil {
		t.Fatal(err)
	}

	const items, versions, writers = 20, 25, 8
	var mu sync.Mutex
	seen := map[string][]int{} // item id -> versions in delivery order
	total := 0
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev struct {
			Records []struct {
				Dynamodb struct {
					NewImage map[string]map[string]string
				} `json:"dynamodb"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		for _, rec := range ev.Records {
			id := rec.Dynamodb.NewImage["id"]["S"]
			v, _ := strconv.Atoi(rec.Dynamodb.NewImage["v"]["N"])
			seen[id] = append(seen[id], v)
			total++
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer consumer.Close()

	go Run(ctx, s, "eu-west-2", slog.New(slog.DiscardHandler), []Target{{Name: "test", Tables: "*", URL: consumer.URL, BatchSize: 50}}, nil)

	// Each writer owns some items and writes their versions in order, so
	// per item the committed order is 1..versions while items interleave.
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for v := 1; v <= versions; v++ {
				for i := w; i < items; i += writers {
					item := attr.Item{
						"id": {Type: attr.TypeS, S: fmt.Sprintf("card-%02d", i)},
						"v":  {Type: attr.TypeN, N: strconv.Itoa(v)},
					}
					if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: storage.Key{Hash: item["id"]}, Fn: func(attr.Item) (storage.WriteResult, error) {
						return storage.WriteResult{Op: storage.Put, Item: item}, nil
					}}); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		n := total
		mu.Unlock()
		if n >= items*versions || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // let any duplicate arrive
	mu.Lock()
	defer mu.Unlock()
	if total != items*versions {
		t.Fatalf("delivered %d records, want %d", total, items*versions)
	}
	for i := 0; i < items; i++ {
		id := fmt.Sprintf("card-%02d", i)
		got := seen[id]
		if len(got) != versions {
			t.Fatalf("%s: %d records, want %d", id, len(got), versions)
		}
		for j, v := range got {
			if v != j+1 {
				t.Fatalf("%s: versions delivered as %v, want 1..%d in order", id, got, versions)
			}
		}
	}
}
