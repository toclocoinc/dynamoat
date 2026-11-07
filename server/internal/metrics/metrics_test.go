package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCounter(t *testing.T) {
	r := New()
	r.Observe("GetItem", "OK", time.Millisecond)
	r.Observe("GetItem", "OK", time.Millisecond)
	r.Observe("GetItem", "ResourceNotFoundException", time.Millisecond)

	var b strings.Builder
	if err := r.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`dynamoat_requests_total{op="GetItem",code="OK"} 2`,
		`dynamoat_requests_total{op="GetItem",code="ResourceNotFoundException"} 1`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output missing %q:\n%s", want, b.String())
		}
	}
}

func TestHistogram(t *testing.T) {
	r := New()
	// 50µs falls in the smallest bucket, 2s in the 2.5s bucket.
	r.Observe("GetItem", "OK", 50*time.Microsecond)
	r.Observe("GetItem", "OK", 2*time.Second)

	var b strings.Builder
	_ = r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`dynamoat_request_duration_seconds_bucket{op="GetItem",le="0.001"} 1`,
		`dynamoat_request_duration_seconds_bucket{op="GetItem",le="1"} 1`,
		`dynamoat_request_duration_seconds_bucket{op="GetItem",le="2.5"} 2`,
		`dynamoat_request_duration_seconds_bucket{op="GetItem",le="+Inf"} 2`,
		`dynamoat_request_duration_seconds_count{op="GetItem"} 2`,
		"dynamoat_request_duration_seconds_sum{op=\"GetItem\"} " + formatFloat(r.hists["GetItem"].sum),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInflight(t *testing.T) {
	r := New()
	r.IncInflight()
	r.IncInflight()
	r.DecInflight()

	var b strings.Builder
	_ = r.WritePrometheus(&b)
	if !strings.Contains(b.String(), "dynamoat_inflight_requests 1\n") {
		t.Errorf("inflight = %q", b.String())
	}
}

func TestJobMetrics(t *testing.T) {
	r := New()
	r.JobRun("ttl", nil)
	r.JobRun("ttl", nil)
	r.JobRun("ttl", errors.New("boom"))
	r.JobItems("ttl", 7)
	r.JobItems("ttl", 3)

	var b strings.Builder
	_ = r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`dynamoat_job_runs_total{job="ttl",result="ok"} 2`,
		`dynamoat_job_runs_total{job="ttl",result="error"} 1`,
		`dynamoat_job_items_total{job="ttl"} 10`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStreamMetrics(t *testing.T) {
	r := New()
	r.StreamDelivered("c", 3)
	r.StreamDeliveryFailure("c")
	r.StreamDeliveryFailure("c")
	r.StreamSetLag("c", "t", 0)
	r.StreamSetLag("c", "t", 12.5)

	var b strings.Builder
	_ = r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`dynamoat_stream_delivered_total{target="c"} 3`,
		`dynamoat_stream_delivery_failures_total{target="c"} 2`,
		`dynamoat_stream_lag_seconds{target="c",table="t"} 12.5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	r.StreamRemoveLag("c", "t")
	b.Reset()
	_ = r.WritePrometheus(&b)
	if strings.Contains(b.String(), `dynamoat_stream_lag_seconds{target="c",table="t"}`) {
		t.Errorf("lag gauge not removed:\n%s", b.String())
	}
}

type fakePool struct{ stats PoolStats }

func (f fakePool) PoolStats() PoolStats { return f.stats }

func TestPoolMetrics(t *testing.T) {
	r := New()
	r.SetPoolStats(fakePool{stats: PoolStats{Idle: 2, InUse: 1, Total: 3, Max: 10}})

	var b strings.Builder
	_ = r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`dynamoat_pg_pool_connections{state="idle"} 2`,
		`dynamoat_pg_pool_connections{state="in_use"} 1`,
		`dynamoat_pg_pool_connections{state="total"} 3`,
		`dynamoat_pg_pool_max_connections 10`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPoolMetricsAbsentWithoutProvider(t *testing.T) {
	r := New()
	var b strings.Builder
	_ = r.WritePrometheus(&b)
	if strings.Contains(b.String(), "dynamoat_pg_pool_") {
		t.Errorf("pool metrics emitted without a provider:\n%s", b.String())
	}
}
