// Package jobs runs the background maintenance work dynamoat needs on top of a
// storage.Store: TTL expiry, stream trimming and point-in-time-recovery history
// trimming. Each job takes a lease (a storage.StreamStore.TryLease) so only one
// server process runs it at a time.
package jobs

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/metrics"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

const (
	leaseTTL   = 30 * time.Second
	leaseRenew = 10 * time.Second
	ttlBatch   = 1000
	trimBatch  = 10000

	historyTrimBatch = 10000
)

// trimEvery is how often the stream-trim job runs. A var rather than a const
// so tests can shorten it.
var trimEvery = time.Minute

// trimHistoryEvery is how often the point-in-time-recovery history-trim job
// runs. A var rather than a const so tests can shorten it.
var trimHistoryEvery = 10 * time.Minute

// Options configures Run.
type Options struct {
	// TTLInterval is how often the TTL expiry job scans for expired items.
	// Defaults to 1s.
	TTLInterval time.Duration
	// StreamRetention is how long stream records are kept before the trim job
	// deletes them. Defaults to 24h.
	StreamRetention time.Duration
	// Now returns the current time; defaults to time.Now. Exposed so tests can
	// pin the expiry and trim cutoffs with a fake clock.
	Now func() time.Time
	// Metrics, when set, records one run and the items processed for every
	// ttl and stream-trim run.
	Metrics *metrics.Registry
}

// Run starts the TTL expiry and stream-trim jobs and blocks until ctx is
// cancelled, then waits for both jobs to stop. A job for a capability the
// store does not implement is simply not started.
func Run(ctx context.Context, s storage.Store, log *slog.Logger, opt Options) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opt.TTLInterval <= 0 {
		opt.TTLInterval = time.Second
	}
	if opt.StreamRetention <= 0 {
		opt.StreamRetention = 24 * time.Hour
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	owner := hostOwner()

	var wg sync.WaitGroup

	// TTL expiry needs TTLStore to do the work and StreamStore for its lease.
	if ts, ok := s.(storage.TTLStore); ok {
		if ls, ok := s.(storage.StreamStore); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runLeased(ctx, "ttl", owner, ls, log, opt.Metrics, opt.TTLInterval, func(ctx context.Context) (int, error) {
					return expireLoop(ctx, ts, opt.Now())
				})
			}()
		}
	}
	if ss, ok := s.(storage.StreamStore); ok {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLeased(ctx, "stream-trim", owner, ss, log, opt.Metrics, trimEvery, func(ctx context.Context) (int, error) {
				return trimLoop(ctx, ss, opt.Now().Add(-opt.StreamRetention))
			})
		}()
	}
	// History trimming needs PITRStore for the work and StreamStore for its
	// lease, like the other jobs.
	if ps, ok := s.(storage.PITRStore); ok {
		if ls, ok := s.(storage.StreamStore); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runLeased(ctx, "trim-history", owner, ls, log, opt.Metrics, trimHistoryEvery, func(ctx context.Context) (int, error) {
					return historyTrimLoop(ctx, ps, opt.Now())
				})
			}()
		}
	}

	<-ctx.Done()
	wg.Wait()
}

// runLeased holds the named lease while running work every interval, until ctx
// is cancelled or the lease is lost. When another owner holds the lease the
// job does no work at all.
func runLeased(ctx context.Context, name, owner string, ls storage.StreamStore, log *slog.Logger, m *metrics.Registry, interval time.Duration, work func(ctx context.Context) (int, error)) {
	if ok, err := ls.TryLease(ctx, name, owner, leaseTTL); err != nil {
		log.Error("background job: cannot take lease", "job", name, "err", err)
		return
	} else if !ok {
		log.Info("background job: lease held by another owner", "job", name)
		return
	}

	workTicker := time.NewTicker(interval)
	defer workTicker.Stop()
	renewTicker := time.NewTicker(leaseRenew)
	defer renewTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-renewTicker.C:
			ok, err := ls.TryLease(ctx, name, owner, leaseTTL)
			if err != nil {
				log.Error("background job: cannot renew lease", "job", name, "err", err)
				continue
			}
			if !ok {
				log.Info("background job: lease lost", "job", name)
				return
			}
		case <-workTicker.C:
			n, err := work(ctx)
			if m != nil {
				m.JobRun(name, err)
				if err == nil {
					m.JobItems(name, int64(n))
				}
			}
			if err != nil {
				log.Error("background job: failed", "job", name, "err", err)
				continue
			}
			if n > 0 {
				log.Info("background job: ran", "job", name, "count", n)
			}
		}
	}
}

// expireLoop deletes expired items in batches of ttlBatch, repeating while a
// batch is full, and returns the total deleted.
func expireLoop(ctx context.Context, ts storage.TTLStore, now time.Time) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, nil
		}
		n, err := ts.ExpireItems(ctx, now, ttlBatch)
		if err != nil {
			return total, err
		}
		total += n
		if n < ttlBatch {
			return total, nil
		}
	}
}

// trimLoop deletes stream records older than the horizon in batches of
// trimBatch, repeating while a batch is full, and returns the total deleted.
func trimLoop(ctx context.Context, ss storage.StreamStore, before time.Time) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, nil
		}
		n, err := ss.TrimStreams(ctx, before, trimBatch)
		if err != nil {
			return total, err
		}
		total += n
		if n < trimBatch {
			return total, nil
		}
	}
}

// historyTrimLoop deletes item versions no table's recovery window needs any
// more, in batches of historyTrimBatch, repeating while a batch is full, and
// returns the total deleted.
func historyTrimLoop(ctx context.Context, ps storage.PITRStore, now time.Time) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, nil
		}
		n, err := ps.TrimHistory(ctx, now, historyTrimBatch)
		if err != nil {
			return total, err
		}
		total += n
		if n < historyTrimBatch {
			return total, nil
		}
	}
}

// hostOwner is the lease owner: hostname plus pid.
func hostOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}
