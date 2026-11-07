package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
)

const (
	typValidation      = "com.amazon.coral.validate#ValidationException"
	typConditional     = "com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException"
	typResourceMissing = "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException"
)

// newOps wires the operations onto an in-memory store behind an HTTP handler.
func newOps(t *testing.T) (*Ops, *api.Handler, *memstore.Store) {
	t.Helper()
	store := memstore.New()
	o := &Ops{Store: store}
	h := api.New(api.Options{})
	o.Register(h)
	return o, h, store
}

func makeTable(t *testing.T, store *memstore.Store, name string, key storage.KeySchema) {
	t.Helper()
	if _, err := store.CreateTable(context.Background(), &storage.Table{Name: name, Key: key}); err != nil {
		t.Fatalf("CreateTable(%q): %v", name, err)
	}
}

func post(t *testing.T, h http.Handler, op, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+op)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func bodyJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q is not JSON (status %d): %v", rec.Body.String(), rec.Code, err)
	}
	return m
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) map[string]any {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, code, rec.Body.String())
	}
	return bodyJSON(t, rec)
}

func wantErr(t *testing.T, rec *httptest.ResponseRecorder, typ, msg string) {
	t.Helper()
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	m := bodyJSON(t, rec)
	if m["__type"] != typ {
		t.Errorf("__type = %v, want %s", m["__type"], typ)
	}
	if msg != "" && m["message"] != msg {
		t.Errorf("message = %v, want %s", m["message"], msg)
	}
}

func equalJSON(t *testing.T, got, want any, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", label, got, want)
	}
}
