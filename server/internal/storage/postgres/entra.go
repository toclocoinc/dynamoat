package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies a short-lived access token. When OpenWithOptions is
// given one, every new connection's password is the token it returns, so a
// managed identity can sign in without a static password in the connection
// string. nil means use the connection string's password as given.
type TokenSource interface {
	// Token returns an access token, refreshing it shortly before it expires.
	Token(ctx context.Context) (string, error)
}

// entraScope is the Azure resource the token is requested for (Azure Database
// for PostgreSQL Flexible Server).
const entraScope = "https://ossrdbms-aad.database.windows.net/.default"

// defaultAuthorityHost is Microsoft's OAuth 2.0 endpoint host.
const defaultAuthorityHost = "https://login.microsoftonline.com/"

// refreshBefore is how long before expiry a cached token is refreshed.
const refreshBefore = 5 * time.Minute

// workloadIdentityConfig holds the Azure Workload Identity parameters. In
// production these are the AZURE_* variables injected by the AKS workload
// identity webhook.
type workloadIdentityConfig struct {
	authorityHost string           // AZURE_AUTHORITY_HOST, default defaultAuthorityHost
	tenantID      string           // AZURE_TENANT_ID
	clientID      string           // AZURE_CLIENT_ID
	tokenFile     string           // AZURE_FEDERATED_TOKEN_FILE
	scope         string           // token scope, default entraScope
	client        *http.Client     // nil means a client with a 30 s timeout
	now           func() time.Time // nil means time.Now
}

// workloadIdentity is a TokenSource for the Azure Workload Identity federated
// token flow: the Kubernetes service account token in tokenFile is exchanged
// for an Entra access token.
type workloadIdentity struct {
	cfg workloadIdentityConfig

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewWorkloadIdentity builds a TokenSource from the Azure Workload Identity
// environment variables. It returns an error when a required variable is
// missing.
func NewWorkloadIdentity() (TokenSource, error) {
	return newWorkloadIdentity(workloadIdentityConfig{
		authorityHost: envDefault("AZURE_AUTHORITY_HOST", defaultAuthorityHost),
		tenantID:      os.Getenv("AZURE_TENANT_ID"),
		clientID:      os.Getenv("AZURE_CLIENT_ID"),
		tokenFile:     os.Getenv("AZURE_FEDERATED_TOKEN_FILE"),
		scope:         entraScope,
	})
}

func newWorkloadIdentity(cfg workloadIdentityConfig) (*workloadIdentity, error) {
	if cfg.authorityHost == "" {
		cfg.authorityHost = defaultAuthorityHost
	}
	if cfg.tenantID == "" {
		return nil, errors.New("entra workload identity: AZURE_TENANT_ID is required")
	}
	if cfg.clientID == "" {
		return nil, errors.New("entra workload identity: AZURE_CLIENT_ID is required")
	}
	if cfg.tokenFile == "" {
		return nil, errors.New("entra workload identity: AZURE_FEDERATED_TOKEN_FILE is required")
	}
	if cfg.scope == "" {
		cfg.scope = entraScope
	}
	if cfg.client == nil {
		// A hung token endpoint must not block new connections forever.
		cfg.client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &workloadIdentity{cfg: cfg}, nil
}

// Token returns a cached access token, fetching a fresh one when the cached
// token is within refreshBefore of expiry.
func (w *workloadIdentity) Token(ctx context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.token != "" && w.cfg.now().Before(w.expiresAt.Add(-refreshBefore)) {
		return w.token, nil
	}
	token, expiresIn, err := w.fetch(ctx)
	if err != nil {
		return "", err
	}
	w.token = token
	w.expiresAt = w.cfg.now().Add(expiresIn)
	return w.token, nil
}

// fetch exchanges the federated token for an Entra access token. The error it
// returns never contains the assertion.
func (w *workloadIdentity) fetch(ctx context.Context) (string, time.Duration, error) {
	assertion, err := os.ReadFile(w.cfg.tokenFile)
	if err != nil {
		return "", 0, fmt.Errorf("entra: read federated token file: %w", err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {w.cfg.clientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {string(assertion)},
		"scope":                 {w.cfg.scope},
	}
	endpoint := w.cfg.authorityHost + w.cfg.tenantID + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("entra: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := w.cfg.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("entra: token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("entra: read token response: %w", err)
	}

	var tr struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
		Error       string      `json:"error"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("entra: token endpoint returned HTTP %d with an unparseable body", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("entra: token endpoint returned HTTP %d: %s", resp.StatusCode, tr.Error)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("entra: token endpoint returned no access_token")
	}
	seconds, err := tr.ExpiresIn.Int64()
	if err != nil {
		return "", 0, fmt.Errorf("entra: token endpoint returned an invalid expires_in")
	}
	if seconds <= 0 {
		return "", 0, fmt.Errorf("entra: token endpoint returned a non-positive expires_in")
	}
	return tr.AccessToken, time.Duration(seconds) * time.Second, nil
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
