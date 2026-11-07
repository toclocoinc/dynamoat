// Package api is DynamoDB's wire protocol: every operation is POST / with
// X-Amz-Target: DynamoDB_20120810.<Operation> and a JSON body
// (application/x-amz-json-1.0). Responses carry x-amzn-RequestId and
// x-amz-crc32 (CRC32 of the body, which the AWS SDKs verify). Errors are a
// JSON body with __type and message; see package ddberr.
//
// Operations are registered by name and receive the raw request body; each
// one decodes and validates its own input.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
)

// TargetPrefix is the X-Amz-Target prefix of every DynamoDB operation.
const TargetPrefix = "DynamoDB_20120810."

// StreamsTargetPrefix is the X-Amz-Target prefix of the DynamoDB Streams API,
// which the SDK sends to AWS_ENDPOINT_URL_DYNAMODB_STREAMS (the same server).
// Its operations are registered as StreamsOp + name, e.g. "Streams.GetRecords".
const StreamsTargetPrefix = "DynamoDBStreams_20120810."

// StreamsOp prefixes the registered name of a DynamoDB Streams operation.
const StreamsOp = "Streams."

// MaxRequestBytes bounds a request body. DynamoDB's largest requests
// (BatchWriteItem, TransactWriteItems) are limited to 16 MB.
const MaxRequestBytes = 16 << 20

// Op handles one operation. body is the request JSON (never empty: an empty
// body is passed as "{}"). The result is marshalled as the response body; an
// error of type *ddberr.Error becomes that error response, any other error an
// InternalServerError.
type Op func(ctx context.Context, body []byte) (any, error)

// Authenticator checks a request's signature. It returns a *ddberr.Error
// (MissingAuthenticationToken, UnrecognizedClient, InvalidSignature) to reject
// it. body is the request body, needed for the payload hash.
type Authenticator interface {
	Authenticate(r *http.Request, body []byte) error
}

type Options struct {
	Logger *slog.Logger
	Auth   Authenticator // nil accepts every request (DYNAMOAT_AUTH=none)
	Ready  func(context.Context) error
	// Metrics, when set, records every DynamoDB request and serves the
	// Prometheus /metrics endpoint. nil disables both.
	Metrics *metrics.Registry
}

type Handler struct {
	ops     map[string]Op
	log     *slog.Logger
	auth    Authenticator
	ready   func(context.Context) error
	metrics *metrics.Registry
}

func New(opts Options) *Handler {
	h := &Handler{ops: map[string]Op{}, log: opts.Logger, auth: opts.Auth, ready: opts.Ready, metrics: opts.Metrics}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	return h
}

// Register adds an operation by its DynamoDB name, e.g. "GetItem", or a
// DynamoDB Streams operation as StreamsOp+name, e.g. "Streams.GetRecords".
func (h *Handler) Register(name string, op Op) { h.ops[name] = op }

type ctxKey int

const requestIDKey ctxKey = 0

// RequestID returns the request ID of the request ctx belongs to.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := newRequestID()
	w.Header().Set("x-amzn-RequestId", id)

	if r.Method == http.MethodGet {
		h.serveGet(w, r)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	op := ""
	if h.metrics != nil {
		h.metrics.IncInflight()
		defer h.metrics.DecInflight()
	}
	status, err := h.serve(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)), &op)
	if h.metrics != nil {
		// Label only registered operations, so made-up targets cannot create
		// unbounded metric series.
		mop := op
		if _, ok := h.ops[mop]; !ok {
			mop = "unknown"
		}
		h.metrics.Observe(mop, outcomeCode(err), time.Since(start))
	}
	attrs := []any{"request_id", id, "op", op, "status", status, "ms", time.Since(start).Milliseconds()}
	var de *ddberr.Error
	if errors.As(err, &de) {
		attrs = append(attrs, "error", de.Code())
		if de.Status >= 500 {
			h.log.Error("request", attrs...)
			return
		}
	}
	h.log.Info("request", attrs...)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, opName *string) (int, error) {
	target := r.Header.Get("X-Amz-Target")
	name, ok := strings.CutPrefix(target, TargetPrefix)
	if !ok {
		if rest, isStreams := strings.CutPrefix(target, StreamsTargetPrefix); isStreams && rest != "" {
			name, ok = StreamsOp+rest, true
		}
	}
	*opName = name
	if !ok || name == "" {
		return writeError(w, ddberr.UnknownOperation())
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return writeError(w, ddberr.Validation("Request size exceeded %d bytes", MaxRequestBytes))
		}
		return writeError(w, ddberr.Serialization("Unable to read the request body"))
	}

	if h.auth != nil {
		if err := h.auth.Authenticate(r, body); err != nil {
			return writeError(w, asDDB(err))
		}
	}

	op, ok := h.ops[name]
	if !ok {
		return writeError(w, ddberr.UnknownOperation())
	}

	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return writeError(w, ddberr.Serialization("Start of structure or map found where not expected."))
	}

	result, err := callOp(r.Context(), op, body)
	if err != nil {
		de := asDDB(err)
		if de.Status >= 500 {
			h.log.Error("operation failed", "request_id", RequestID(r.Context()), "op", name, "err", err)
		}
		return writeError(w, de)
	}
	if result == nil {
		result = struct{}{}
	}
	out, err := json.Marshal(result)
	if err != nil {
		h.log.Error("marshal response", "op", name, "err", err)
		return writeError(w, ddberr.Internal())
	}
	return http.StatusOK, write(w, http.StatusOK, out)
}

// callOp turns a panic in an operation into an InternalServerError.
func callOp(ctx context.Context, op Op, body []byte) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = panicError{p}
		}
	}()
	return op(ctx, body)
}

type panicError struct{ v any }

func (p panicError) Error() string { return "panic: " + toString(p.v) }

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func asDDB(err error) *ddberr.Error {
	var de *ddberr.Error
	if errors.As(err, &de) {
		return de
	}
	return ddberr.Internal()
}

func writeError(w http.ResponseWriter, e *ddberr.Error) (int, error) {
	out, _ := json.Marshal(e.Body())
	_ = write(w, e.Status, out)
	return e.Status, e
}

func write(w http.ResponseWriter, status int, body []byte) error {
	hd := w.Header()
	hd.Set("Content-Type", "application/x-amz-json-1.0")
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	hd.Set("x-amz-crc32", strconv.FormatUint(uint64(crc32.ChecksumIEEE(body)), 10))
	w.WriteHeader(status)
	_, err := w.Write(body)
	return err
}

func (h *Handler) serveGet(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/healthz":
		// DynamoDB answers GET / with a plain-text health line.
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "healthy: dynamoat\n")
	case "/readyz":
		if h.ready != nil {
			if err := h.ready(r.Context()); err != nil {
				// The detail goes to the log only: /readyz is unauthenticated
				// and must not expose Postgres error text or hostnames.
				h.log.Error("readiness check failed", "err", err)
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = io.WriteString(w, "ready\n")
	case "/metrics":
		if h.metrics == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = h.metrics.WritePrometheus(w)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// outcomeCode names the result of a request for metrics: the DynamoDB error's
// short name, "InternalServerError" for any other error, or "OK" on success.
func outcomeCode(err error) string {
	if err == nil {
		return "OK"
	}
	var de *ddberr.Error
	if errors.As(err, &de) {
		return de.Code()
	}
	return "InternalServerError"
}

// newRequestID makes a 52-character ID in DynamoDB's style (upper-case
// letters and digits).
func newRequestID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b [52]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}
