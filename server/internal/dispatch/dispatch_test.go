package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/attr"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

const (
	testRegion = "us-east-1"
	testLabel  = "2026-11-06T12:30:45.123"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newStreamTable(t *testing.T, store *memstore.Store, name string) *storage.Table {
	t.Helper()
	td, err := store.CreateTable(context.Background(), &storage.Table{
		Name:        name,
		Key:         storage.KeySchema{Hash: "id", HashType: attr.TypeS},
		StreamView:  storage.StreamNewAndOld,
		StreamLabel: testLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	return td
}

// writeItems inserts n distinct items with ids start..start+n-1, one stream
// record each. Callers pass distinct starts so later writes do not overwrite
// earlier items.
func writeItems(t *testing.T, store *memstore.Store, td *storage.Table, start, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := strconv.Itoa(start + i)
		_, _, err := store.WriteItem(context.Background(), storage.Write{
			Table: td,
			Key:   storage.Key{Hash: attr.Value{Type: attr.TypeS, S: id}},
			Fn: func(old attr.Item) (storage.WriteResult, error) {
				return storage.WriteResult{
					Op:   storage.Put,
					Item: attr.Item{"id": attr.Value{Type: attr.TypeS, S: id}},
				}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func expectedSeqs(t *testing.T, store *memstore.Store, td *storage.Table) []int64 {
	t.Helper()
	recs, err := store.ReadStream(context.Background(), td, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}

func streamArn(table string) string {
	return "arn:aws:dynamodb:" + testRegion + ":000000000000:table/" + table + "/stream/" + testLabel
}

// received is one POST the fake consumer received.
type received struct {
	event   map[string]any
	records []map[string]any
}

// consumer is an httptest server that records each POST body. respond, when
// set, returns the status and body for the n-th request.
type consumer struct {
	server  *httptest.Server
	respond func(n int) (status int, body string)

	mu    sync.Mutex
	posts []received
}

func newConsumer(t *testing.T, respond func(n int) (int, string)) *consumer {
	t.Helper()
	c := &consumer{respond: respond}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	return c
}

func (c *consumer) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var event map[string]any
	_ = json.Unmarshal(raw, &event)
	var records []map[string]any
	if arr, ok := event["Records"].([]any); ok {
		for _, e := range arr {
			records = append(records, e.(map[string]any))
		}
	}
	c.mu.Lock()
	c.posts = append(c.posts, received{event: event, records: records})
	n := len(c.posts)
	c.mu.Unlock()

	status, body := 200, ""
	if c.respond != nil {
		status, body = c.respond(n)
	}
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (c *consumer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts)
}

func (c *consumer) snapshot() []received {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]received, len(c.posts))
	copy(out, c.posts)
	return out
}

// waitPosts waits until the consumer has at least n posts.
func (c *consumer) waitPosts(t *testing.T, n int, timeout time.Duration) []received {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for c.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("consumer got %d posts, want at least %d", c.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.snapshot()
}

func recordSeq(r map[string]any) int64 {
	ddb := r["dynamodb"].(map[string]any)
	n, _ := strconv.ParseInt(ddb["SequenceNumber"].(string), 10, 64)
	return n
}

func recordARN(r map[string]any) string {
	return r["eventSourceARN"].(string)
}

func seqs(recs []map[string]any) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = recordSeq(r)
	}
	return out
}

func startRun(t *testing.T, store *memstore.Store, targets []Target, owner string) (cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx, store, testRegion, testLogger(), targets, owner, nil)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

func TestDeliversInOrderInLambdaShape(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 5)
	want := expectedSeqs(t, store, td)

	got := newConsumer(t, nil)
	startRun(t, store, []Target{{Name: "c", Tables: "*", URL: got.server.URL}}, "a")

	posts := got.waitPosts(t, 1, 5*time.Second)
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	recs := posts[0].records
	if len(recs) != 5 {
		t.Fatalf("got %d records, want 5", len(recs))
	}
	for i, r := range recs {
		if recordSeq(r) != want[i] {
			t.Fatalf("record %d seq = %d, want %d", i, recordSeq(r), want[i])
		}
		if r["eventSource"] != "aws:dynamodb" {
			t.Errorf("record %d eventSource = %v", i, r["eventSource"])
		}
		if r["eventName"] != "INSERT" {
			t.Errorf("record %d eventName = %v", i, r["eventName"])
		}
		if r["eventVersion"] != "1.1" {
			t.Errorf("record %d eventVersion = %v", i, r["eventVersion"])
		}
		if r["awsRegion"] != testRegion {
			t.Errorf("record %d awsRegion = %v", i, r["awsRegion"])
		}
		if recordARN(r) != streamArn("t") {
			t.Errorf("record %d eventSourceARN = %v", i, recordARN(r))
		}
		if _, ok := r["eventID"].(string); !ok {
			t.Errorf("record %d has no eventID", i)
		}
		ddb := r["dynamodb"].(map[string]any)
		if _, ok := ddb["Keys"]; !ok {
			t.Errorf("record %d has no dynamodb.Keys", i)
		}
		if _, ok := ddb["NewImage"]; !ok {
			t.Errorf("record %d has no NewImage", i)
		}
	}
}

func TestRetriesSameBatchAfterFailure(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 3)
	want := expectedSeqs(t, store, td)

	var mu sync.Mutex
	fails := 0
	got := newConsumer(t, func(n int) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if fails < 2 {
			fails++
			return 500, "boom"
		}
		return 200, ""
	})
	startRun(t, store, []Target{{Name: "c", Tables: "*", URL: got.server.URL}}, "a")

	posts := got.waitPosts(t, 3, 15*time.Second)
	for i, p := range posts {
		if len(p.records) != 3 {
			t.Fatalf("post %d has %d records, want 3", i, len(p.records))
		}
		if got := seqs(p.records); !equalInt64(got, want) {
			t.Fatalf("post %d seqs = %v, want %v", i, got, want)
		}
	}
}

func TestRetriesFunctionErrorBody(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 2)
	want := expectedSeqs(t, store, td)

	var mu sync.Mutex
	first := true
	got := newConsumer(t, func(n int) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		if first {
			first = false
			return 200, `{"errorMessage":"boom","errorType":"RuntimeError"}`
		}
		return 200, ""
	})
	startRun(t, store, []Target{{Name: "c", Tables: "*", URL: got.server.URL}}, "a")

	posts := got.waitPosts(t, 2, 10*time.Second)
	for i, p := range posts {
		if got := seqs(p.records); !equalInt64(got, want) {
			t.Fatalf("post %d seqs = %v, want %v", i, got, want)
		}
	}
}

func TestTwoDispatchersDeliverEachRecordOnce(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 5)
	want := expectedSeqs(t, store, td)

	a := newConsumer(t, nil)
	b := newConsumer(t, nil)
	target := func(c *consumer) Target {
		return Target{Name: "c", Tables: "*", URL: c.server.URL}
	}
	startRun(t, store, []Target{target(a)}, "a")
	startRun(t, store, []Target{target(b)}, "b")

	// Wait until one dispatcher has delivered every record, then stop both and
	// check the records arrived exactly once.
	deadline := time.Now().Add(10 * time.Second)
	for {
		total := 0
		for _, c := range []*consumer{a, b} {
			for _, p := range c.snapshot() {
				total += len(p.records)
			}
		}
		if total == len(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d records, want %d", total, len(want))
		}
		time.Sleep(10 * time.Millisecond)
	}

	var all []int64
	var deliveredA, deliveredB int
	for _, p := range a.snapshot() {
		deliveredA += len(p.records)
		all = append(all, seqs(p.records)...)
	}
	for _, p := range b.snapshot() {
		deliveredB += len(p.records)
		all = append(all, seqs(p.records)...)
	}
	if !equalInt64(all, want) {
		t.Fatalf("delivered seqs = %v, want %v", all, want)
	}
	if deliveredA != 0 && deliveredB != 0 {
		t.Errorf("both dispatchers delivered (a=%d, b=%d), want exactly one", deliveredA, deliveredB)
	}
}

func TestRestartResumesFromCheckpoint(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 3)

	first := newConsumer(t, nil)
	stop := startRun(t, store, []Target{{Name: "c", Tables: "*", URL: first.server.URL}}, "a")
	first.waitPosts(t, 1, 5*time.Second)
	stop()

	writeItems(t, store, td, 3, 2)
	want := expectedSeqs(t, store, td)

	second := newConsumer(t, nil)
	startRun(t, store, []Target{{Name: "c", Tables: "*", URL: second.server.URL}}, "a")

	posts := second.waitPosts(t, 1, 5*time.Second)
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	recs := posts[0].records
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	got := seqs(recs)
	if !equalInt64(got, want[3:]) {
		t.Fatalf("resumed at %v, want %v", got, want[3:])
	}
}

func TestBatchSizeHonoured(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 5)
	want := expectedSeqs(t, store, td)

	got := newConsumer(t, nil)
	startRun(t, store, []Target{{Name: "c", Tables: "*", URL: got.server.URL, BatchSize: 2}}, "a")

	posts := got.waitPosts(t, 3, 5*time.Second)
	sizes := []int{2, 2, 1}
	for i, p := range posts {
		if len(p.records) != sizes[i] {
			t.Fatalf("post %d has %d records, want %d", i, len(p.records), sizes[i])
		}
	}
	var all []int64
	for _, p := range posts {
		all = append(all, seqs(p.records)...)
	}
	if !equalInt64(all, want) {
		t.Fatalf("seqs = %v, want %v", all, want)
	}
}

func TestBatchWindowFillsShortBatch(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 1)

	got := newConsumer(t, nil)
	startRun(t, store, []Target{{Name: "c", Tables: "*", URL: got.server.URL, BatchSize: 10, BatchWindow: time.Second}}, "a")

	// Two more records land while the batch window is still open.
	time.Sleep(100 * time.Millisecond)
	writeItems(t, store, td, 1, 2)

	posts := got.waitPosts(t, 1, 5*time.Second)
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	recs := posts[0].records
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	gotSeqs := seqs(recs)
	if !equalInt64(gotSeqs, expectedSeqs(t, store, td)) {
		t.Fatalf("seqs = %v", gotSeqs)
	}
}

func TestGlobMatching(t *testing.T) {
	store := memstore.New()
	ta := newStreamTable(t, store, "tenant-a")
	tb := newStreamTable(t, store, "other")
	writeItems(t, store, ta, 0, 2)
	writeItems(t, store, tb, 0, 2)

	tenant := newConsumer(t, nil)
	all := newConsumer(t, nil)
	startRun(t, store, []Target{
		{Name: "tenant", Tables: "tenant-*", URL: tenant.server.URL},
		{Name: "all", Tables: "*", URL: all.server.URL},
	}, "a")

	tenant.waitPosts(t, 1, 5*time.Second)
	all.waitPosts(t, 2, 5*time.Second)

	var tenantRecs, allRecs []map[string]any
	for _, p := range tenant.snapshot() {
		tenantRecs = append(tenantRecs, p.records...)
	}
	for _, p := range all.snapshot() {
		allRecs = append(allRecs, p.records...)
	}
	if len(tenantRecs) != 2 {
		t.Fatalf("tenant consumer got %d records, want 2", len(tenantRecs))
	}
	for _, r := range tenantRecs {
		if recordARN(r) != streamArn("tenant-a") {
			t.Errorf("tenant consumer got record for %v", recordARN(r))
		}
	}
	if len(allRecs) != 4 {
		t.Fatalf("all consumer got %d records, want 4", len(allRecs))
	}
}

func TestDispatchRecordsMetrics(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 3)

	m := metrics.New()
	got := newConsumer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx, store, testRegion, testLogger(), []Target{{Name: "c", Tables: "*", URL: got.server.URL}}, "a", m)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		var b strings.Builder
		_ = m.WritePrometheus(&b)
		out := b.String()
		if strings.Contains(out, `dynamoat_stream_delivered_total{target="c"} 3`) &&
			strings.Contains(out, `dynamoat_stream_lag_seconds{target="c",table="t"} 0`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery metrics not recorded:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var b strings.Builder
	_ = m.WritePrometheus(&b)
	if strings.Contains(b.String(), "dynamoat_stream_delivery_failures_total") {
		t.Errorf("unexpected failure metric:\n%s", b.String())
	}
}

func TestDispatchRecordsFailures(t *testing.T) {
	store := memstore.New()
	td := newStreamTable(t, store, "t")
	writeItems(t, store, td, 0, 1)

	m := metrics.New()
	got := newConsumer(t, func(n int) (int, string) { return 500, "boom" })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx, store, testRegion, testLogger(), []Target{{Name: "c", Tables: "*", URL: got.server.URL}}, "a", m)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		var b strings.Builder
		_ = m.WritePrometheus(&b)
		out := b.String()
		if strings.Contains(out, `dynamoat_stream_delivery_failures_total{target="c"} `) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failure metric not recorded:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
