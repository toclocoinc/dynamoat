// Package dispatch delivers a table's stream records, in order, to HTTP
// consumers in the Lambda event shape. It is dynamoat's replacement for a
// Lambda event source mapping: an application's stream Lambdas run unchanged in a
// container whose Runtime Interface Emulator takes the event on
// POST /2015-03-31/functions/function/invocations (any endpoint taking the
// same body works).
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/metrics"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/streams"
)

const (
	// leaseTTL is how long a delivery loop's lease lasts between renewals.
	leaseTTL = 30 * time.Second
	// discoverEvery is how often each target re-lists its tables.
	discoverEvery = 5 * time.Second
	// pollEmpty is how long a delivery loop waits before polling again when
	// its stream has no records.
	pollEmpty = 250 * time.Millisecond
	// retryFirst is the first retry delay after a failed delivery; it doubles
	// up to retryMax.
	retryFirst = time.Second
	retryMax   = time.Minute
	// defaultBatch and maxBatch bound a target's BatchSize.
	defaultBatch = 100
	maxBatch     = 1000
	// batchPoll is how often a delivery loop re-reads while a BatchWindow is
	// open, waiting for a short batch to fill.
	batchPoll = 100 * time.Millisecond
	// postTimeout bounds one consumer invocation, so a hung consumer becomes a
	// retryable failure rather than blocking the table forever.
	postTimeout = 30 * time.Second
)

// Target is one stream consumer. Run holds a lease and a checkpoint for every
// (target, table) pair it finds, so each table is delivered by exactly one
// server process at a time.
type Target struct {
	Name        string // the consumer name, for checkpoints and leases
	Tables      string // path.Match glob over table names, e.g. "*" or "tenant-*"
	URL         string
	BatchSize   int           // default 100, max 1000
	BatchWindow time.Duration // wait up to this long to fill a batch (default 0)
	Headers     map[string]string
	Timeout     time.Duration // one invocation; default 30 s, at most 15 min (a Lambda's limit)
}

// Run delivers stream records for every target until ctx is cancelled, then
// waits for its delivery loops to stop. A store that does not implement
// storage.StreamStore is logged and skipped. Metrics, when non-nil, records
// delivery counts and lag.
func Run(ctx context.Context, s storage.Store, region string, log *slog.Logger, targets []Target, m *metrics.Registry) {
	run(ctx, s, region, log, targets, leaseOwner(), m)
}

// leaseOwner names this process for leases: hostname and pid, so two servers
// never share an owner.
func leaseOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// run is Run with an explicit lease owner, so tests can run two dispatchers in
// one process with distinct owners.
func run(ctx context.Context, s storage.Store, region string, log *slog.Logger, targets []Target, owner string, m *metrics.Registry) {
	if log == nil {
		log = slog.Default()
	}
	ss, ok := s.(storage.StreamStore)
	if !ok {
		log.Warn("stream dispatcher: store does not support streams, not dispatching")
		return
	}
	if len(targets) == 0 {
		return
	}
	if region == "" {
		region = "us-east-1"
	}

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			sup := &supervisor{
				store:   s,
				ss:      ss,
				region:  region,
				log:     log,
				target:  t,
				owner:   owner,
				client:  &http.Client{Timeout: t.timeout()},
				metrics: m,
				runners: make(map[int32]context.CancelFunc),
			}
			sup.supervise(ctx)
		}(t)
	}
	<-ctx.Done()
	wg.Wait()
}

// timeout is the target's invocation timeout, defaulting to postTimeout.
func (t Target) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return postTimeout
}

// supervisor runs one target: it re-lists the target's tables and keeps one
// delivery loop per matching table with a stream.
type supervisor struct {
	store  storage.Store
	ss     storage.StreamStore
	region string
	log    *slog.Logger
	target Target
	owner  string
	client *http.Client

	metrics *metrics.Registry

	mu      sync.Mutex
	runners map[int32]context.CancelFunc
	wg      sync.WaitGroup
}

func (s *supervisor) supervise(ctx context.Context) {
	s.reconcile(ctx)
	ticker := time.NewTicker(discoverEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile aligns the running delivery loops with the target's tables that
// currently match the glob and have a stream, starting new loops and stopping
// loops whose table is gone or no longer streaming.
func (s *supervisor) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	names, err := s.store.ListTables(ctx, "", 0)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("stream dispatcher: listing tables", "target", s.target.Name, "err", err)
		}
		return
	}
	want := make(map[int32]*storage.Table)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		if !matches(s.target.Tables, name) {
			continue
		}
		t, err := s.store.GetTable(ctx, name)
		if err != nil {
			if !errors.Is(err, storage.ErrTableNotFound) && ctx.Err() == nil {
				s.log.Error("stream dispatcher: getting table", "target", s.target.Name, "table", name, "err", err)
			}
			continue
		}
		if t.StreamView == "" {
			continue
		}
		want[t.ID] = t
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cancel := range s.runners {
		if _, ok := want[id]; !ok {
			cancel()
			delete(s.runners, id)
		}
	}
	for id, t := range want {
		if _, ok := s.runners[id]; ok {
			continue
		}
		dctx, cancel := context.WithCancel(ctx)
		s.runners[id] = cancel
		s.wg.Add(1)
		go s.deliverLoop(id, t, dctx, cancel)
	}
}

// deliverLoop runs one delivery loop and removes itself from the runners map
// when it stops.
func (s *supervisor) deliverLoop(id int32, t *storage.Table, ctx context.Context, cancel context.CancelFunc) {
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.runners, id)
		s.mu.Unlock()
		s.wg.Done()
	}()
	s.deliver(ctx, t)
}

// deliver holds the lease for one (target, table) and delivers its records in
// Seq order until the loop is cancelled, the table is deleted, its stream is
// disabled, or the lease is lost.
func (s *supervisor) deliver(ctx context.Context, t *storage.Table) {
	defer s.removeLag(t)

	batchSize := s.target.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatch
	}
	if batchSize > maxBatch {
		batchSize = maxBatch
	}

	lease := fmt.Sprintf("dispatch:%s:%d", s.target.Name, t.ID)
	got, err := s.ss.TryLease(ctx, lease, s.owner, leaseTTL)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("stream dispatcher: acquiring lease", "target", s.target.Name, "table", t.Name, "err", err)
		}
		return
	}
	if !got {
		return
	}

	// dctx also cancels when the lease is lost, so another server taking over
	// stops this loop promptly.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.renew(dctx, cancel, lease)

	after, err := s.ss.Checkpoint(dctx, s.target.Name, t)
	if err != nil {
		if dctx.Err() == nil {
			s.log.Error("stream dispatcher: reading checkpoint", "target", s.target.Name, "table", t.Name, "err", err)
		}
		return
	}

	delay := retryFirst
	for {
		if dctx.Err() != nil {
			return
		}
		recs, err := s.readBatch(dctx, t, after, batchSize)
		if err != nil {
			if dctx.Err() != nil {
				return
			}
			s.log.Error("stream dispatcher: reading stream", "target", s.target.Name, "table", t.Name, "err", err)
			delay = sleepBackoff(dctx, delay)
			continue
		}
		if len(recs) == 0 {
			s.setLag(t, 0)
			if !sleep(dctx, pollEmpty) {
				return
			}
			delay = retryFirst
			continue
		}

		s.setLag(t, time.Since(recs[0].At).Seconds())

		first, last := recs[0].Seq, recs[len(recs)-1].Seq
		ok, err := s.post(dctx, t, recs)
		if err != nil {
			if dctx.Err() != nil {
				return
			}
			s.recordFailure()
			s.log.Warn("stream dispatcher: delivering batch failed", "target", s.target.Name, "table", t.Name, "seqFirst", first, "seqLast", last, "err", err)
			delay = sleepBackoff(dctx, delay)
			continue
		}
		if !ok {
			if dctx.Err() != nil {
				return
			}
			s.recordFailure()
			s.log.Warn("stream dispatcher: consumer reported a function error", "target", s.target.Name, "table", t.Name, "seqFirst", first, "seqLast", last)
			delay = sleepBackoff(dctx, delay)
			continue
		}
		if !s.saveCheckpoint(dctx, t, last) {
			return
		}
		s.recordDelivered(len(recs))
		s.log.Debug("stream dispatcher: delivered batch", "target", s.target.Name, "table", t.Name, "seqFirst", first, "seqLast", last, "count", len(recs))
		after = last
		delay = retryFirst
	}
}

// readBatch reads up to limit records after `after`, waiting up to the
// target's BatchWindow for a short batch to fill.
func (s *supervisor) readBatch(ctx context.Context, t *storage.Table, after int64, limit int) ([]storage.StreamRecord, error) {
	recs, err := s.ss.ReadStream(ctx, t, after, limit)
	if err != nil {
		return recs, err
	}
	if s.target.BatchWindow <= 0 || len(recs) == 0 || len(recs) >= limit {
		return recs, nil
	}
	seen := recs[len(recs)-1].Seq
	deadline := time.Now().Add(s.target.BatchWindow)
	for len(recs) < limit {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := batchPoll
		if remaining < wait {
			wait = remaining
		}
		if !sleep(ctx, wait) {
			return recs, ctx.Err()
		}
		more, err := s.ss.ReadStream(ctx, t, seen, limit-len(recs))
		if err != nil {
			return recs, err
		}
		recs = append(recs, more...)
		if len(more) > 0 {
			seen = recs[len(recs)-1].Seq
		}
	}
	return recs, nil
}

// post sends one batch. It returns ok=false with a nil error when the consumer
// reported a Lambda function error: a 2xx whose body is a JSON object with an
// errorType or errorMessage field. Any transport error or non-2xx status is a
// non-nil error.
func (s *supervisor) post(ctx context.Context, t *storage.Table, recs []storage.StreamRecord) (bool, error) {
	records := make([]map[string]any, len(recs))
	for i, r := range recs {
		m := streams.Record(t, s.region, r)
		m["eventSourceARN"] = streams.StreamARN(s.region, t.Name, t.StreamLabel)
		records[i] = m
	}
	body, err := json.Marshal(map[string]any{"Records": records})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.target.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.target.Headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("consumer returned status %d", resp.StatusCode)
	}
	if isFunctionError(respBody) {
		return false, nil
	}
	return true, nil
}

// saveCheckpoint persists the last delivered Seq, retrying until it sticks or
// the loop is cancelled. The batch has already been delivered, so it must not
// be POSTed again; the checkpoint alone restores at-least-once delivery after
// a crash.
func (s *supervisor) saveCheckpoint(ctx context.Context, t *storage.Table, seq int64) bool {
	delay := retryFirst
	for {
		err := s.ss.SaveCheckpoint(ctx, s.target.Name, t, seq)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		s.log.Error("stream dispatcher: saving checkpoint", "target", s.target.Name, "table", t.Name, "seq", seq, "err", err)
		delay = sleepBackoff(ctx, delay)
	}
}

// renew keeps the lease alive while the delivery loop runs and cancels the
// loop when another owner takes the lease.
func (s *supervisor) renew(ctx context.Context, stop func(), lease string) {
	ticker := time.NewTicker(leaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			got, err := s.ss.TryLease(ctx, lease, s.owner, leaseTTL)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Error("stream dispatcher: renewing lease", "target", s.target.Name, "lease", lease, "err", err)
				}
				continue
			}
			if !got {
				s.log.Warn("stream dispatcher: lost lease", "target", s.target.Name, "lease", lease)
				stop()
				return
			}
		}
	}
}

// matches reports whether name matches the target's table glob.
func matches(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// setLag records the target's delivery lag for one table, in seconds.
func (s *supervisor) setLag(t *storage.Table, seconds float64) {
	if s.metrics != nil {
		s.metrics.StreamSetLag(s.target.Name, t.Name, seconds)
	}
}

// removeLag drops a table's lag gauge once the loop stops, keeping the label
// set bounded to tables the target currently matches.
func (s *supervisor) removeLag(t *storage.Table) {
	if s.metrics != nil {
		s.metrics.StreamRemoveLag(s.target.Name, t.Name)
	}
}

// recordDelivered records stream records delivered to the target.
func (s *supervisor) recordDelivered(n int) {
	if s.metrics != nil {
		s.metrics.StreamDelivered(s.target.Name, n)
	}
}

// recordFailure records one failed delivery attempt for the target.
func (s *supervisor) recordFailure() {
	if s.metrics != nil {
		s.metrics.StreamDeliveryFailure(s.target.Name)
	}
}

// isFunctionError reports whether body is a Lambda function error: a JSON
// object with an errorType or errorMessage field.
func isFunctionError(body []byte) bool {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return false
	}
	_, hasType := obj["errorType"]
	_, hasMsg := obj["errorMessage"]
	return hasType || hasMsg
}

// sleep waits d, returning false if ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// sleepBackoff sleeps delay and returns the next delay, doubled to at most
// retryMax.
func sleepBackoff(ctx context.Context, delay time.Duration) time.Duration {
	sleep(ctx, delay)
	next := delay * 2
	if next > retryMax {
		next = retryMax
	}
	return next
}
