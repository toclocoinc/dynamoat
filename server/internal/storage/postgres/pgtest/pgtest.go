// Package pgtest gives tests a real, empty Postgres database.
//
// With DYNAMOAT_TEST_PG set (a connection string to a server the tests may
// create databases on, as in CI) it uses that server.
// Otherwise it starts a private Postgres (embedded-postgres: downloaded once
// into ~/.cache/dynamoat, run as the current user on a free local port), so
// every machine, including the build agents and the gate, runs the database
// tests with no Docker and no setup.
//
// A package that uses it must stop the private server when its tests end:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

// Version is the Postgres major the private server runs: 17, the
// recommended version. DYNAMOAT_TEST_PG_VERSION (15, 16, 17 or 18) changes it.
var versions = map[string]embeddedpostgres.PostgresVersion{
	"15": embeddedpostgres.V15, "16": embeddedpostgres.V16,
	"17": embeddedpostgres.V17, "18": embeddedpostgres.V18,
}

var (
	once     sync.Once
	server   *embeddedpostgres.EmbeddedPostgres
	adminURL string
	startErr error
	tmpDir   string
)

// Main runs the package's tests and then stops the private server, if one
// was started.
func Main(m *testing.M) {
	code := m.Run()
	if server != nil {
		_ = server.Stop()
	}
	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
	os.Exit(code)
}

// DB creates a new empty database for this test and returns its connection
// string. The database is dropped when the test ends.
func DB(t testing.TB) string {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Fatalf("pgtest: cannot start Postgres: %v", startErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("pgtest: connect %s: %v", redact(adminURL), err)
	}
	defer admin.Close(ctx)

	name := "dyn_test_" + randomHex(6)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, adminURL); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	return u.String()
}

func start() {
	if s := os.Getenv("DYNAMOAT_TEST_PG"); s != "" {
		adminURL = s
		return
	}
	ver := os.Getenv("DYNAMOAT_TEST_PG_VERSION")
	if ver == "" {
		ver = "17"
	}
	v, ok := versions[ver]
	if !ok {
		startErr = fmt.Errorf("DYNAMOAT_TEST_PG_VERSION=%q: want 15, 16, 17 or 18", ver)
		return
	}
	port, err := freePort()
	if err != nil {
		startErr = err
		return
	}
	home, _ := os.UserHomeDir()
	cache := filepath.Join(home, ".cache", "dynamoat", "embedded-postgres")
	if tmpDir, err = os.MkdirTemp("", "dynamoat-pgtest-"); err != nil {
		startErr = err
		return
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(v).
		Port(uint32(port)).
		Username("postgres").
		Password("pgtest").
		Database("postgres").
		CachePath(cache).
		RuntimePath(filepath.Join(tmpDir, "run")).
		DataPath(filepath.Join(tmpDir, "data")).
		BinariesPath(filepath.Join(tmpDir, "bin")).
		Locale("C").
		Encoding("UTF8").
		StartTimeout(60 * time.Second).
		Logger(io.Discard)
	server = embeddedpostgres.NewDatabase(cfg)
	if err := server.Start(); err != nil {
		server, startErr = nil, err
		return
	}
	adminURL = fmt.Sprintf("postgres://postgres:pgtest@127.0.0.1:%d/postgres?sslmode=disable", port)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func redact(s string) string {
	if u, err := url.Parse(s); err == nil {
		return u.Redacted()
	}
	return "(connection string)"
}
