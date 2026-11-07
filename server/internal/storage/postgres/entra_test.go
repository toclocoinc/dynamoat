package postgres

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkloadIdentityTokenRequest(t *testing.T) {
	var got struct {
		method string
		path   string
		ctype  string
		form   url.Values
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got.method = r.Method
		got.path = r.URL.Path
		got.ctype = r.Header.Get("Content-Type")
		got.form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"secret-token","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer srv.Close()

	w, err := newWorkloadIdentity(workloadIdentityConfig{
		authorityHost: srv.URL + "/",
		tenantID:      "tenant-123",
		clientID:      "client-456",
		tokenFile:     writeTokenFile(t, "the-assertion-jwt"),
		client:        srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := w.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "secret-token" {
		t.Fatalf("token = %q, want %q", tok, "secret-token")
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/tenant-123/oauth2/v2.0/token" {
		t.Errorf("path = %q, want /tenant-123/oauth2/v2.0/token", got.path)
	}
	if got.ctype != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q, want application/x-www-form-urlencoded", got.ctype)
	}
	for field, want := range map[string]string{
		"grant_type":            "client_credentials",
		"client_id":             "client-456",
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"client_assertion":      "the-assertion-jwt",
		"scope":                 entraScope,
	} {
		if got.form.Get(field) != want {
			t.Errorf("form %s = %q, want %q", field, got.form.Get(field), want)
		}
	}
}

func TestWorkloadIdentityCachesAndRefreshes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":3600}`, calls)
	}))
	defer srv.Close()

	var cur time.Time
	w, err := newWorkloadIdentity(workloadIdentityConfig{
		authorityHost: srv.URL + "/",
		tenantID:      "tenant",
		clientID:      "client",
		tokenFile:     writeTokenFile(t, "assertion"),
		client:        srv.Client(),
		now:           func() time.Time { return cur },
	})
	if err != nil {
		t.Fatal(err)
	}

	// First call fetches; the token expires at 1000 + 3600 = 4600.
	cur = time.Unix(1000, 0)
	if tok, err := w.Token(context.Background()); err != nil || tok != "tok-1" {
		t.Fatalf("first Token = %q, %v; want tok-1, nil", tok, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	// Ten minutes later the token is still valid: no new request.
	cur = time.Unix(1600, 0)
	if tok, err := w.Token(context.Background()); err != nil || tok != "tok-1" {
		t.Fatalf("cached Token = %q, %v; want tok-1, nil", tok, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (still cached)", calls)
	}

	// Within five minutes of expiry the token is refreshed.
	cur = time.Unix(4310, 0)
	if tok, err := w.Token(context.Background()); err != nil || tok != "tok-2" {
		t.Fatalf("refreshed Token = %q, %v; want tok-2, nil", tok, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (refreshed)", calls)
	}
}

func TestWorkloadIdentityErrorDoesNotLeakAssertion(t *testing.T) {
	const assertion = "super-secret-assertion-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid_client","error_description":"bad client"}`)
	}))
	defer srv.Close()

	w, err := newWorkloadIdentity(workloadIdentityConfig{
		authorityHost: srv.URL + "/",
		tenantID:      "tenant",
		clientID:      "client",
		tokenFile:     writeTokenFile(t, assertion),
		client:        srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.Token(context.Background())
	if err == nil {
		t.Fatal("want an error when the token endpoint rejects the request")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should mention the HTTP status", err)
	}
	if strings.Contains(err.Error(), assertion) {
		t.Errorf("error %q leaks the assertion", err)
	}
}

func TestWorkloadIdentityRequiresConfig(t *testing.T) {
	for _, cfg := range []workloadIdentityConfig{
		{},
		{tenantID: "t", clientID: "c"},
		{tenantID: "t", tokenFile: "f"},
		{clientID: "c", tokenFile: "f"},
	} {
		if _, err := newWorkloadIdentity(cfg); err == nil {
			t.Errorf("newWorkloadIdentity(%+v) = nil error, want a missing-config error", cfg)
		}
	}
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "federated-token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
