package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
)

func TestMetricsEndpoint(t *testing.T) {
	h := New(Options{Metrics: metrics.New()})
	h.Register("GetItem", func(context.Context, []byte) (any, error) {
		return map[string]any{"Item": map[string]any{}}, nil
	})
	h.Register("PutItem", func(context.Context, []byte) (any, error) {
		return nil, ddberr.ConditionalCheckFailed(nil)
	})

	post(t, h, "DynamoDB_20120810.GetItem", "{}")
	post(t, h, "DynamoDB_20120810.PutItem", "{}")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`dynamoat_requests_total{op="GetItem",code="OK"} 1`,
		`dynamoat_requests_total{op="PutItem",code="ConditionalCheckFailedException"} 1`,
		`dynamoat_request_duration_seconds_count{op="GetItem"} 1`,
		`dynamoat_request_duration_seconds_count{op="PutItem"} 1`,
		"dynamoat_inflight_requests 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GET /metrics missing %q:\n%s", want, out)
		}
	}
}

func TestMetricsDisabled(t *testing.T) {
	h := New(Options{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics without a registry = %d, want 404", rec.Code)
	}
}
