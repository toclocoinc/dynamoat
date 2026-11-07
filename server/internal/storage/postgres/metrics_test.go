package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres/pgtest"
)

func TestPoolStats(t *testing.T) {
	s, err := OpenWithOptions(context.Background(), pgtest.DB(t), Options{MaxConns: 3, MinConns: 1})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()

	st := s.PoolStats()
	if st.Max != 3 {
		t.Fatalf("PoolStats.Max = %d, want 3", st.Max)
	}
	if st.Idle < 0 || st.InUse < 0 || st.Total < 0 {
		t.Fatalf("PoolStats has negative counts: %+v", st)
	}
	if st.Total < st.InUse+st.Idle {
		t.Fatalf("PoolStats.Total %d < InUse %d + Idle %d", st.Total, st.InUse, st.Idle)
	}
}

func TestWorkerMetrics(t *testing.T) {
	ctx := context.Background()
	m := metrics.New()
	s, err := OpenWithOptions(ctx, pgtest.DB(t), Options{Metrics: m})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	defer s.Close()

	tb := &storage.Table{
		Name:     "wm",
		Key:      storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		AttrDefs: map[string]attr.Type{"id": attr.TypeS, "gid": attr.TypeS},
		Billing:  "PAY_PER_REQUEST",
		Status:   storage.StatusActive,
	}
	tb, err = s.CreateTable(ctx, tb)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	const n = 100
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id%04d", i)
		it := item("id", sv(id), "gid", sv(fmt.Sprintf("g%04d", i)))
		if _, _, err := s.WriteItem(ctx, storage.Write{Table: tb, Key: storage.Key{Hash: sv(id)}, Fn: putItemFn(it)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	updated, err := s.UpdateTable(ctx, "wm", func(t *storage.Table) error {
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
	waitIndexActive(t, s, "wm", g.ID)

	if _, err := s.DeleteTable(ctx, "wm"); err != nil {
		t.Fatalf("DeleteTable: %v", err)
	}

	waitForMetric(t, m, `dynamoat_job_items_total{job="backfill"} `)
	waitForMetric(t, m, `dynamoat_job_runs_total{job="table-delete",result="ok"} `)
}

func waitForMetric(t *testing.T, m *metrics.Registry, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var b strings.Builder
		_ = m.WritePrometheus(&b)
		if strings.Contains(b.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metric %q not recorded:\n%s", want, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
