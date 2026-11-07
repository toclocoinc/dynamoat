package api

import (
	"context"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

func post(t *testing.T, h http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	return m
}

func checkEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-amz-json-1.0" {
		t.Errorf("Content-Type = %q", ct)
	}
	want := strconv.FormatUint(uint64(crc32.ChecksumIEEE(rec.Body.Bytes())), 10)
	if got := rec.Header().Get("x-amz-crc32"); got != want {
		t.Errorf("x-amz-crc32 = %q, want %q", got, want)
	}
	if id := rec.Header().Get("x-amzn-RequestId"); len(id) != 52 {
		t.Errorf("x-amzn-RequestId = %q, want 52 characters", id)
	}
}

func TestRoutesByTarget(t *testing.T) {
	h := New(Options{})
	var got string
	h.Register("ListTables", func(ctx context.Context, body []byte) (any, error) {
		got = string(body)
		if RequestID(ctx) == "" {
			t.Error("operation context has no request ID")
		}
		return map[string]any{"TableNames": []string{"a", "b"}}, nil
	})
	rec := post(t, h, "DynamoDB_20120810.ListTables", `{"Limit":2}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	checkEnvelope(t, rec)
	if got != `{"Limit":2}` {
		t.Errorf("operation got body %q", got)
	}
	if names := decode(t, rec)["TableNames"]; len(names.([]any)) != 2 {
		t.Errorf("TableNames = %v", names)
	}
}

func TestEmptyBodyIsEmptyObject(t *testing.T) {
	h := New(Options{})
	h.Register("ListTables", func(_ context.Context, body []byte) (any, error) {
		if string(body) != "{}" {
			t.Errorf("body %q, want {}", body)
		}
		return nil, nil
	})
	rec := post(t, h, "DynamoDB_20120810.ListTables", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
}

func TestErrors(t *testing.T) {
	h := New(Options{})
	h.Register("GetItem", func(context.Context, []byte) (any, error) {
		return nil, ddberr.ResourceNotFound("")
	})
	h.Register("PutItem", func(context.Context, []byte) (any, error) {
		return nil, ddberr.ConditionalCheckFailed(map[string]any{"id": map[string]any{"S": "x"}})
	})
	h.Register("Query", func(context.Context, []byte) (any, error) { return nil, errors.New("boom") })
	h.Register("Scan", func(context.Context, []byte) (any, error) { panic("bad") })

	cases := []struct {
		name, target, body string
		status             int
		typ, message       string
	}{
		{"no target", "", "{}", 400, "com.amazon.coral.service#UnknownOperationException", ""},
		{"wrong prefix", "DynamoDB_20111205.GetItem", "{}", 400, "com.amazon.coral.service#UnknownOperationException", ""},
		{"unknown op", "DynamoDB_20120810.Nope", "{}", 400, "com.amazon.coral.service#UnknownOperationException", ""},
		{"bad JSON", "DynamoDB_20120810.GetItem", "{", 400, "com.amazon.coral.service#SerializationException", ""},
		{"JSON array", "DynamoDB_20120810.GetItem", "[]", 400, "com.amazon.coral.service#SerializationException", ""},
		{"ddb error", "DynamoDB_20120810.GetItem", "{}", 400, "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException", "Requested resource not found"},
		{"conditional", "DynamoDB_20120810.PutItem", "{}", 400, "com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException", "The conditional request failed"},
		{"plain error", "DynamoDB_20120810.Query", "{}", 500, "com.amazonaws.dynamodb.v20120810#InternalServerError", "Internal server error"},
		{"panic", "DynamoDB_20120810.Scan", "{}", 500, "com.amazonaws.dynamodb.v20120810#InternalServerError", "Internal server error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(t, h, c.target, c.body)
			if rec.Code != c.status {
				t.Errorf("status %d, want %d", rec.Code, c.status)
			}
			checkEnvelope(t, rec)
			m := decode(t, rec)
			if m["__type"] != c.typ {
				t.Errorf("__type %v, want %s", m["__type"], c.typ)
			}
			if c.message != "" && m["message"] != c.message {
				t.Errorf("message %v, want %s", m["message"], c.message)
			}
		})
	}

	m := decode(t, post(t, h, "DynamoDB_20120810.PutItem", "{}"))
	if _, ok := m["Item"]; !ok {
		t.Error("ConditionalCheckFailed lost its Item")
	}
}

type denyAll struct{}

func (denyAll) Authenticate(*http.Request, []byte) error { return ddberr.MissingAuthenticationToken() }

func TestAuthRunsBeforeOperation(t *testing.T) {
	h := New(Options{Auth: denyAll{}})
	h.Register("GetItem", func(context.Context, []byte) (any, error) {
		t.Error("operation ran without authentication")
		return nil, nil
	})
	rec := post(t, h, "DynamoDB_20120810.GetItem", "{}")
	if m := decode(t, rec); m["__type"] != "com.amazon.coral.service#MissingAuthenticationTokenException" {
		t.Errorf("__type %v", m["__type"])
	}
}

func TestInternalErrorDoesNotLeakPostgresDetails(t *testing.T) {
	h := New(Options{})
	h.Register("GetItem", func(context.Context, []byte) (any, error) {
		return nil, &pgconn.PgError{
			Severity:      "ERROR",
			Code:          "42P01",
			Message:       `connection to server at "db.internal.example.com" failed: relation "items" does not exist`,
			InternalQuery: `SELECT pk, sk, item FROM items WHERE table_id = $1`,
			SchemaName:    "public",
			TableName:     "items",
			File:          "parse_relation.c",
			Line:          1376,
			Routine:       "parserOpenTable",
		}
	})

	rec := post(t, h, "DynamoDB_20120810.GetItem", "{}")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	checkEnvelope(t, rec)
	m := decode(t, rec)
	if m["__type"] != "com.amazonaws.dynamodb.v20120810#InternalServerError" {
		t.Errorf("__type %v, want InternalServerError", m["__type"])
	}
	if m["message"] != "Internal server error" {
		t.Errorf("message %v, want generic Internal server error", m["message"])
	}

	body := rec.Body.String()
	for _, leak := range []string{
		"db.internal.example.com", // internal hostname
		"does not exist",          // Postgres error text
		"parse_relation.c",        // source file path
		"SQLSTATE",                // pgconn error formatting
		"SELECT pk, sk, item",     // SQL
		"items",                   // table name
	} {
		if strings.Contains(body, leak) {
			t.Errorf("response body leaks %q: %s", leak, body)
		}
	}
}

func TestRequestTooLarge(t *testing.T) {
	h := New(Options{})
	h.Register("BatchWriteItem", func(context.Context, []byte) (any, error) { return nil, nil })
	big := `{"x":"` + strings.Repeat("a", MaxRequestBytes) + `"}`
	if m := decode(t, post(t, h, "DynamoDB_20120810.BatchWriteItem", big)); m["__type"] != "com.amazon.coral.validate#ValidationException" {
		t.Errorf("__type %v", m["__type"])
	}
}

func TestGetEndpoints(t *testing.T) {
	notReady := errors.New(`failed to connect to "db.internal.example.com": connection refused`)
	ready := error(nil)
	h := New(Options{Ready: func(context.Context) error { return ready }})
	for path, want := range map[string]int{"/": 200, "/healthz": 200, "/readyz": 200, "/nope": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	ready = notReady
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 503 {
		t.Errorf("GET /readyz while not ready = %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "db.internal.example.com") || strings.Contains(body, "connection refused") {
		t.Errorf("GET /readyz leaks the readiness error: %q", body)
	}
}

func TestRoutesStreamsTarget(t *testing.T) {
	h := New(Options{})
	called := ""
	h.Register("GetRecords", func(ctx context.Context, body []byte) (any, error) { called = "dynamodb"; return map[string]any{}, nil })
	h.Register(StreamsOp+"GetRecords", func(ctx context.Context, body []byte) (any, error) { called = "streams"; return map[string]any{}, nil })
	if rec := post(t, h, "DynamoDBStreams_20120810.GetRecords", `{}`); rec.Code != 200 || called != "streams" {
		t.Fatalf("streams target: status %d, called %q", rec.Code, called)
	}
	for _, target := range []string{"DynamoDBStreams_20120810.", "DynamoDBStreams_20120810.Nope"} {
		if rec := post(t, h, target, `{}`); rec.Code != 400 {
			t.Errorf("%s: status %d, want 400 UnknownOperation", target, rec.Code)
		}
	}
}
