// Command dynamoat serves the DynamoDB API.
//
// Configuration is by environment:
//
//	DYNAMOAT_LISTEN   listen address (default :8000, DynamoDB Local's port)
//	DYNAMOAT_AUTH     "sigv4" (default) or "none" for local development
//	DYNAMOAT_KEYS     access key file, or directory of one file per key (required for sigv4).
//	                  Re-read on SIGHUP and every 30s when its modification time
//	                  (or, for a directory, any file's) changes, so keys rotate
//	                  without a restart.
//	DYNAMOAT_REGION   pin the region accepted in the credential scope (default: any)
//	DYNAMOAT_STORE    "memory" or "postgres"; default postgres when DYNAMOAT_PG is set,
//	                  memory otherwise
//	DYNAMOAT_PG       Postgres connection string for the postgres store
//	DYNAMOAT_PG_AUTH  "password" (default) or "azure-workload-identity"; with
//	                  azure-workload-identity each connection's password is an
//	                  Entra access token obtained with the Azure Workload
//	                  Identity federated token flow (the AZURE_* variables)
//	DYNAMOAT_PG_MAX_CONNS  maximum Postgres connections in the pool (default 10)
//	DYNAMOAT_PG_MIN_CONNS  minimum Postgres connections in the pool (default 0)
//	DYNAMOAT_PG_STATEMENT_TIMEOUT  statement_timeout applied to new connections
//	                  (a Go duration, default 30s)
//	DYNAMOAT_PG_IDLE_TX_TIMEOUT  idle_in_transaction_session_timeout applied to
//	                  new connections (a Go duration, default 10s)
//	DYNAMOAT_LOCK_TIMEOUT  how long a write waits for an item lock before the
//	                  request is throttled (a Go duration, default 5s)
//	DYNAMOAT_STREAM_TARGETS  a JSON array of stream consumer targets, or
//	                  "@<path>" to read it from a file (see docs/streams.md)
//	DYNAMOAT_TTL_INTERVAL  how often the TTL expiry job scans for expired items
//	                  (a Go duration, default 1s)
//	DYNAMOAT_STREAM_RETENTION  how long stream records are kept before the
//	                  stream-trim job deletes them (a Go duration, default 24h)
//	DYNAMOAT_LOG      "json" (default) or "text"
//	DYNAMOAT_METRICS  "off" disables the /metrics Prometheus endpoint
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/dispatch"
	"github.com/toclocoinc/dynamoat/server/internal/jobs"
	"github.com/toclocoinc/dynamoat/server/internal/metrics"
	"github.com/toclocoinc/dynamoat/server/internal/ops"
	"github.com/toclocoinc/dynamoat/server/internal/sigv4"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/storage/memstore"
	"github.com/toclocoinc/dynamoat/server/internal/storage/postgres"
)

func main() {
	if code := handleArgs(os.Args[1:], os.Stdout, os.Stderr); code >= 0 {
		os.Exit(code)
	}
	log := newLogger(os.Getenv("DYNAMOAT_LOG"))
	addr := envOr("DYNAMOAT_LISTEN", ":8000")

	var auth api.Authenticator // nil means no authentication
	var keyStore *sigv4.KeyStore
	switch mode := envOr("DYNAMOAT_AUTH", "sigv4"); mode {
	case "sigv4":
		path := os.Getenv("DYNAMOAT_KEYS")
		if path == "" {
			log.Error("DYNAMOAT_AUTH=sigv4 requires DYNAMOAT_KEYS (key file or directory)")
			os.Exit(1)
		}
		keys, err := sigv4.NewKeyStore(path)
		if err != nil {
			log.Error("cannot load access keys", "path", path, "err", err)
			os.Exit(1)
		}
		keyStore = keys
		region := os.Getenv("DYNAMOAT_REGION")
		auth = &sigv4.Verifier{Keys: keys, Region: region}
		log.Info("authentication: sigv4", "keys", keys.Len(), "region", region)
	case "none":
		log.Warn("authentication: none (every request is accepted)")
	default:
		log.Error("DYNAMOAT_AUTH must be \"sigv4\" or \"none\"", "value", mode)
		os.Exit(1)
	}

	var m *metrics.Registry
	if os.Getenv("DYNAMOAT_METRICS") != "off" {
		m = metrics.New()
	}

	store, closeStore := openStore(log, m)
	defer closeStore()
	if m != nil {
		if p, ok := store.(metrics.PoolStatsProvider); ok {
			m.SetPoolStats(p)
		}
	}

	targets, err := dispatch.ParseTargets(os.Getenv("DYNAMOAT_STREAM_TARGETS"))
	if err != nil {
		log.Error("invalid DYNAMOAT_STREAM_TARGETS", "err", err)
		os.Exit(1)
	}

	h := api.New(api.Options{Logger: log, Auth: auth, Ready: store.Ready, Metrics: m})
	o := &ops.Ops{Store: store, Region: os.Getenv("DYNAMOAT_REGION")}
	o.Register(h)

	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// Bounds the whole request read, so a client trickling a body (up to
		// the 16 MB limit) cannot hold a connection open indefinitely.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if keyStore != nil {
		go watchKeys(ctx, keyStore, log)
	}

	go dispatch.Run(ctx, store, os.Getenv("DYNAMOAT_REGION"), log, targets, m)

	ttlInterval := time.Second
	if v := os.Getenv("DYNAMOAT_TTL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Error("DYNAMOAT_TTL_INTERVAL must be a Go duration (e.g. 1s, 200ms)", "value", v)
			os.Exit(1)
		}
		ttlInterval = d
	}
	streamRetention := 24 * time.Hour
	if v := os.Getenv("DYNAMOAT_STREAM_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Error("DYNAMOAT_STREAM_RETENTION must be a Go duration (e.g. 24h, 1h)", "value", v)
			os.Exit(1)
		}
		streamRetention = d
	}
	go jobs.Run(ctx, store, log, jobs.Options{TTLInterval: ttlInterval, StreamRetention: streamRetention, Metrics: m})

	log.Info("dynamoat listening", "addr", addr, "version", version, "commit", buildCommit())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// watchKeys reloads the access keys on SIGHUP and every 30s when the key
// file(s) changed on disk. A failed reload keeps the current keys and is
// logged; a successful one logs only the key IDs added and removed (never the
// secrets).
func watchKeys(ctx context.Context, ks *sigv4.KeyStore, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	reload := func() {
		added, removed, err := ks.ReloadIfChanged()
		if err != nil {
			log.Error("access key reload failed; keeping current keys", "err", err)
			return
		}
		if len(added) == 0 && len(removed) == 0 {
			return
		}
		log.Info("access keys reloaded", "added", added, "removed", removed)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			// SIGHUP forces a reload even when the file's mtime is unchanged.
			added, removed, err := ks.Reload()
			if err != nil {
				log.Error("access key reload failed; keeping current keys", "err", err)
				continue
			}
			if len(added) != 0 || len(removed) != 0 {
				log.Info("access keys reloaded", "added", added, "removed", removed)
			}
		case <-ticker.C:
			reload()
		}
	}
}

// openStore builds the storage backend from DYNAMOAT_STORE and DYNAMOAT_PG,
// and returns it with a close function for the postgres store.
func openStore(log *slog.Logger, m *metrics.Registry) (storage.Store, func()) {
	name := os.Getenv("DYNAMOAT_STORE")
	if name == "" {
		if os.Getenv("DYNAMOAT_PG") != "" {
			name = "postgres"
		} else {
			name = "memory"
		}
	}
	switch name {
	case "memory":
		log.Info("store: memory")
		return memstore.New(), func() {}
	case "postgres":
		connString := os.Getenv("DYNAMOAT_PG")
		if connString == "" {
			log.Error("DYNAMOAT_STORE=postgres requires DYNAMOAT_PG (the connection string)")
			os.Exit(1)
		}
		lockTimeout := 5 * time.Second
		if v := os.Getenv("DYNAMOAT_LOCK_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Error("DYNAMOAT_LOCK_TIMEOUT must be a Go duration (e.g. 5s, 200ms)", "value", v)
				os.Exit(1)
			}
			lockTimeout = d
		}
		stmtTimeout := 30 * time.Second
		if v := os.Getenv("DYNAMOAT_PG_STATEMENT_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Error("DYNAMOAT_PG_STATEMENT_TIMEOUT must be a Go duration (e.g. 30s, 200ms)", "value", v)
				os.Exit(1)
			}
			stmtTimeout = d
		}
		idleTxTimeout := 10 * time.Second
		if v := os.Getenv("DYNAMOAT_PG_IDLE_TX_TIMEOUT"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Error("DYNAMOAT_PG_IDLE_TX_TIMEOUT must be a Go duration (e.g. 10s, 200ms)", "value", v)
				os.Exit(1)
			}
			idleTxTimeout = d
		}
		maxConns, err := intEnv("DYNAMOAT_PG_MAX_CONNS", 10, 1)
		if err != nil {
			log.Error("DYNAMOAT_PG_MAX_CONNS must be a positive integer", "value", os.Getenv("DYNAMOAT_PG_MAX_CONNS"))
			os.Exit(1)
		}
		minConns, err := intEnv("DYNAMOAT_PG_MIN_CONNS", 0, 0)
		if err != nil {
			log.Error("DYNAMOAT_PG_MIN_CONNS must be a non-negative integer", "value", os.Getenv("DYNAMOAT_PG_MIN_CONNS"))
			os.Exit(1)
		}
		if minConns > maxConns {
			log.Error("DYNAMOAT_PG_MIN_CONNS cannot exceed DYNAMOAT_PG_MAX_CONNS", "min", minConns, "max", maxConns)
			os.Exit(1)
		}

		var tokenSource postgres.TokenSource
		switch authMode := envOr("DYNAMOAT_PG_AUTH", "password"); authMode {
		case "password":
		case "azure-workload-identity":
			ts, err := postgres.NewWorkloadIdentity()
			if err != nil {
				log.Error("cannot configure Entra workload identity", "err", err)
				os.Exit(1)
			}
			tokenSource = ts
		default:
			log.Error("DYNAMOAT_PG_AUTH must be \"password\" or \"azure-workload-identity\"", "value", authMode)
			os.Exit(1)
		}

		s, err := postgres.OpenWithOptions(context.Background(), connString, postgres.Options{
			LockTimeout:      lockTimeout,
			StatementTimeout: stmtTimeout,
			IdleTxTimeout:    idleTxTimeout,
			Log:              log,
			MaxConns:         int32(maxConns),
			MinConns:         int32(minConns),
			TokenSource:      tokenSource,
			Metrics:          m,
		})
		if err != nil {
			log.Error("cannot open postgres store", "err", redactConnString(err.Error(), connString))
			os.Exit(1)
		}
		log.Info("store: postgres")
		return s, s.Close
	default:
		log.Error("DYNAMOAT_STORE must be \"memory\" or \"postgres\"", "value", name)
		os.Exit(1)
	}
	panic("unreachable")
}

var keywordPasswordRE = regexp.MustCompile(`(?i)(password\s*=\s*)[^ \t\n]+`)

// redactConnString removes the password from an error message by replacing any
// verbatim occurrence of the connection string with its redacted form.
func redactConnString(msg, connString string) string {
	red := redactedConnString(connString)
	if red == connString {
		return msg
	}
	return strings.ReplaceAll(msg, connString, red)
}

func redactedConnString(connString string) string {
	if u, err := url.Parse(connString); err == nil && u.Scheme != "" && u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
			return u.String()
		}
	}
	return keywordPasswordRE.ReplaceAllString(connString, `${1}REDACTED`)
}

func newLogger(format string) *slog.Logger {
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// intEnv reads a non-negative integer from k, returning def when it is unset.
// An error is returned when the value is not an integer or is below min.
func intEnv(k string, def, min int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return 0, errors.New("invalid integer")
	}
	return n, nil
}
