// Package metrics holds dynamoat's Prometheus metrics. The exposition is
// written by hand in the Prometheus text format, so the server carries no
// client library or third-party dependency.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Buckets are the request-duration histogram boundaries, in seconds.
var Buckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// PoolStats is a snapshot of the Postgres connection pool, read at scrape time.
type PoolStats struct {
	Idle  int32
	InUse int32
	Total int32
	Max   int32
}

// PoolStatsProvider reports the Postgres connection pool's state. The registry
// calls it at scrape time; stores without a pool (memstore) do not implement it
// and so report no pool metrics.
type PoolStatsProvider interface {
	PoolStats() PoolStats
}

// Registry accumulates the counters and histograms the API handler records.
// It is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	counters map[string]uint64     // "op\x00code" -> requests
	hists    map[string]*histogram // op -> duration histogram
	inflight int64

	pool PoolStatsProvider

	jobRuns  map[string]uint64 // "job\x00result" -> runs
	jobItems map[string]uint64 // job -> items processed

	streamDelivered map[string]uint64  // target -> records delivered
	streamFailures  map[string]uint64  // target -> failed delivery attempts
	streamLag       map[string]float64 // "target\x00table" -> lag seconds
}

type histogram struct {
	buckets []uint64 // len(Buckets)+1; the last is +Inf
	sum     float64
	count   uint64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		counters:        map[string]uint64{},
		hists:           map[string]*histogram{},
		jobRuns:         map[string]uint64{},
		jobItems:        map[string]uint64{},
		streamDelivered: map[string]uint64{},
		streamFailures:  map[string]uint64{},
		streamLag:       map[string]float64{},
	}
}

// SetPoolStats installs the connection-pool reporter the /metrics endpoint
// reads at scrape time. nil clears it.
func (r *Registry) SetPoolStats(p PoolStatsProvider) {
	r.mu.Lock()
	r.pool = p
	r.mu.Unlock()
}

// Observe records one completed request: op is the DynamoDB operation name and
// code the error's short name, or "OK".
func (r *Registry) Observe(op, code string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counters[op+"\x00"+code]++
	h, ok := r.hists[op]
	if !ok {
		h = &histogram{buckets: make([]uint64, len(Buckets)+1)}
		r.hists[op] = h
	}
	sec := d.Seconds()
	for i, b := range Buckets {
		if sec <= b {
			h.buckets[i]++
		}
	}
	h.buckets[len(Buckets)]++ // +Inf
	h.sum += sec
	h.count++
}

// IncInflight and DecInflight track the number of requests being processed.
func (r *Registry) IncInflight() {
	r.mu.Lock()
	r.inflight++
	r.mu.Unlock()
}

func (r *Registry) DecInflight() {
	r.mu.Lock()
	r.inflight--
	r.mu.Unlock()
}

// JobRun records one completed run of a background job, ok or error.
func (r *Registry) JobRun(job string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	r.mu.Lock()
	r.jobRuns[job+"\x00"+result]++
	r.mu.Unlock()
}

// JobItems records the number of items a background job processed.
func (r *Registry) JobItems(job string, n int64) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	r.jobItems[job] += uint64(n)
	r.mu.Unlock()
}

// StreamDelivered records the number of stream records delivered to a target.
func (r *Registry) StreamDelivered(target string, n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	r.streamDelivered[target] += uint64(n)
	r.mu.Unlock()
}

// StreamDeliveryFailure records one failed delivery attempt for a target.
func (r *Registry) StreamDeliveryFailure(target string) {
	r.mu.Lock()
	r.streamFailures[target]++
	r.mu.Unlock()
}

// StreamSetLag records a target's delivery lag for one table, in seconds: the
// time since the oldest undelivered record was created, or 0 when caught up.
func (r *Registry) StreamSetLag(target, table string, seconds float64) {
	r.mu.Lock()
	r.streamLag[target+"\x00"+table] = seconds
	r.mu.Unlock()
}

// StreamRemoveLag drops a table's lag gauge once a target no longer matches it,
// keeping the label set bounded.
func (r *Registry) StreamRemoveLag(target, table string) {
	r.mu.Lock()
	delete(r.streamLag, target+"\x00"+table)
	r.mu.Unlock()
}

// WritePrometheus writes every metric in the Prometheus text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.counters) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_requests_total Number of DynamoDB requests, by operation and outcome.")
		fmt.Fprintln(w, "# TYPE dynamoat_requests_total counter")
		keys := make([]string, 0, len(r.counters))
		for k := range r.counters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			op, code := split(k)
			fmt.Fprintf(w, "dynamoat_requests_total{op=%q,code=%q} %d\n", op, code, r.counters[k])
		}
	}

	if len(r.hists) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_request_duration_seconds Duration of DynamoDB requests, by operation.")
		fmt.Fprintln(w, "# TYPE dynamoat_request_duration_seconds histogram")
		ops := make([]string, 0, len(r.hists))
		for op := range r.hists {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			h := r.hists[op]
			for i, b := range Buckets {
				fmt.Fprintf(w, "dynamoat_request_duration_seconds_bucket{op=%q,le=%q} %d\n", op, formatFloat(b), h.buckets[i])
			}
			fmt.Fprintf(w, "dynamoat_request_duration_seconds_bucket{op=%q,le=\"+Inf\"} %d\n", op, h.buckets[len(Buckets)])
			fmt.Fprintf(w, "dynamoat_request_duration_seconds_sum{op=%q} %s\n", op, formatFloat(h.sum))
			fmt.Fprintf(w, "dynamoat_request_duration_seconds_count{op=%q} %d\n", op, h.count)
		}
	}

	fmt.Fprintln(w, "# HELP dynamoat_inflight_requests Number of DynamoDB requests currently being processed.")
	fmt.Fprintln(w, "# TYPE dynamoat_inflight_requests gauge")
	fmt.Fprintf(w, "dynamoat_inflight_requests %d\n", r.inflight)

	r.writeJobs(w)
	r.writeStreams(w)
	r.writePool(w)
	return nil
}

func (r *Registry) writeJobs(w io.Writer) {
	if len(r.jobRuns) == 0 && len(r.jobItems) == 0 {
		return
	}
	if len(r.jobRuns) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_job_runs_total Number of background job runs, by job and result.")
		fmt.Fprintln(w, "# TYPE dynamoat_job_runs_total counter")
		keys := make([]string, 0, len(r.jobRuns))
		for k := range r.jobRuns {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			job, result := split(k)
			fmt.Fprintf(w, "dynamoat_job_runs_total{job=%q,result=%q} %d\n", job, result, r.jobRuns[k])
		}
	}
	if len(r.jobItems) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_job_items_total Number of items processed by background jobs, by job.")
		fmt.Fprintln(w, "# TYPE dynamoat_job_items_total counter")
		jobs := make([]string, 0, len(r.jobItems))
		for j := range r.jobItems {
			jobs = append(jobs, j)
		}
		sort.Strings(jobs)
		for _, j := range jobs {
			fmt.Fprintf(w, "dynamoat_job_items_total{job=%q} %d\n", j, r.jobItems[j])
		}
	}
}

func (r *Registry) writeStreams(w io.Writer) {
	if len(r.streamDelivered) == 0 && len(r.streamFailures) == 0 && len(r.streamLag) == 0 {
		return
	}
	if len(r.streamDelivered) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_stream_delivered_total Stream records delivered to consumers, by target.")
		fmt.Fprintln(w, "# TYPE dynamoat_stream_delivered_total counter")
		targets := make([]string, 0, len(r.streamDelivered))
		for t := range r.streamDelivered {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			fmt.Fprintf(w, "dynamoat_stream_delivered_total{target=%q} %d\n", t, r.streamDelivered[t])
		}
	}
	if len(r.streamFailures) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_stream_delivery_failures_total Failed stream delivery attempts, by target.")
		fmt.Fprintln(w, "# TYPE dynamoat_stream_delivery_failures_total counter")
		targets := make([]string, 0, len(r.streamFailures))
		for t := range r.streamFailures {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		for _, t := range targets {
			fmt.Fprintf(w, "dynamoat_stream_delivery_failures_total{target=%q} %d\n", t, r.streamFailures[t])
		}
	}
	if len(r.streamLag) > 0 {
		fmt.Fprintln(w, "# HELP dynamoat_stream_lag_seconds Seconds since the oldest undelivered stream record was created, by target and table.")
		fmt.Fprintln(w, "# TYPE dynamoat_stream_lag_seconds gauge")
		keys := make([]string, 0, len(r.streamLag))
		for k := range r.streamLag {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			target, table := split(k)
			fmt.Fprintf(w, "dynamoat_stream_lag_seconds{target=%q,table=%q} %s\n", target, table, formatFloat(r.streamLag[k]))
		}
	}
}

func (r *Registry) writePool(w io.Writer) {
	if r.pool == nil {
		return
	}
	p := r.pool.PoolStats()
	fmt.Fprintln(w, "# HELP dynamoat_pg_pool_connections Postgres connections in the pool, by state.")
	fmt.Fprintln(w, "# TYPE dynamoat_pg_pool_connections gauge")
	fmt.Fprintf(w, "dynamoat_pg_pool_connections{state=\"idle\"} %d\n", p.Idle)
	fmt.Fprintf(w, "dynamoat_pg_pool_connections{state=\"in_use\"} %d\n", p.InUse)
	fmt.Fprintf(w, "dynamoat_pg_pool_connections{state=\"total\"} %d\n", p.Total)
	fmt.Fprintln(w, "# HELP dynamoat_pg_pool_max_connections Maximum Postgres connections the pool may hold.")
	fmt.Fprintln(w, "# TYPE dynamoat_pg_pool_max_connections gauge")
	fmt.Fprintf(w, "dynamoat_pg_pool_max_connections %d\n", p.Max)
}

func split(k string) (a, b string) {
	i := strings.IndexByte(k, '\x00')
	return k[:i], k[i+1:]
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
