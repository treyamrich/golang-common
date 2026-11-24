package appbroker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treyamrich/golang-common/appbroker"
)

// fakeBroker is a tiny test double over httptest.NewServer.
type fakeBroker struct {
	t        *testing.T
	srv      *httptest.Server
	mux      *http.ServeMux
	gotAuth  string
	gotIDTok string
	gotPath  string
	gotBody  []byte
	hits     atomic.Int32
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{t: t, mux: http.NewServeMux()}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.hits.Add(1)
		fb.gotAuth = r.Header.Get("Authorization")
		fb.gotIDTok = r.Header.Get("X-Homelab-Identity")
		fb.gotPath = r.URL.RequestURI()
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			fb.gotBody = b
		}
		fb.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBroker) handle(pattern string, h http.HandlerFunc) {
	fb.mux.Handle(pattern, h)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// ---------------------------------------------------------------------
// constructor
// ---------------------------------------------------------------------

func TestNewClient_DefaultHTTPClient(t *testing.T) {
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: "http://x", Token: "t"})
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL + "/", Token: "tok"})
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatalf("healthz: %v", err)
	}
}

func TestNewClient_CustomHTTPClient(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, nil) })
	custom := &http.Client{Timeout: 5 * time.Second}
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t", HTTPClient: custom})
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------
// happy paths
// ---------------------------------------------------------------------

func TestHealthz_OK(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, nil) })
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if err := c.Healthz(context.Background()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.HasPrefix(fb.gotAuth, "Bearer t") {
		t.Errorf("missing/wrong bearer: %q", fb.gotAuth)
	}
}

func TestReadyz_503(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 503, "not_ready", "broker not ready")
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if err := c.Readyz(context.Background()); !errors.Is(err, appbroker.ErrUpstreamUnavailable) {
		t.Fatalf("want ErrUpstreamUnavailable, got %v", err)
	}
}

func TestReadyz_OK(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if err := c.Readyz(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------
// ConnectionsStart
// ---------------------------------------------------------------------

func TestConnectionsStart_OAuthUser(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/connections/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("want POST, got %s", r.Method)
		}
		writeJSON(w, 200, map[string]string{
			"method":       "oauth_user",
			"redirect_url": "https://accounts.spotify.com/auth?...",
			"state":        "signed-state",
		})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	out, err := c.ConnectionsStart(context.Background(), appbroker.ConnectionsStartRequest{
		AppID: "spotify-playlist-bot", UserID: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Method != "oauth_user" || out.State != "signed-state" || out.RedirectURL == "" {
		t.Errorf("bad response: %+v", out)
	}
	if !strings.Contains(string(fb.gotBody), `"user_id":"alice"`) {
		t.Errorf("body missing user_id: %s", fb.gotBody)
	}
	if !strings.Contains(string(fb.gotBody), `"app_id":"spotify-playlist-bot"`) {
		t.Errorf("body missing app_id: %s", fb.gotBody)
	}
}

func TestConnectionsStart_OAuthClient_OmitsUserID(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/connections/start", func(w http.ResponseWriter, r *http.Request) {
		// Even sending the request unsupported here, we just verify
		// the wire format respects omitempty.
		writeJSON(w, 400, map[string]string{"error": "wrong_auth_type"})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	_, err := c.ConnectionsStart(context.Background(), appbroker.ConnectionsStartRequest{
		AppID: "spotify-catalog",
	})
	if err == nil {
		t.Fatal("expected wrong_auth_type 400")
	}
	if strings.Contains(string(fb.gotBody), "user_id") {
		t.Errorf("user_id should be omitted when empty: %s", fb.gotBody)
	}
}

// ---------------------------------------------------------------------
// CredentialOAuthUser
// ---------------------------------------------------------------------

func TestCredentialOAuthUser_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/credential/oauth-user", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("user_id") != "alice" || q.Get("app_id") != "spotify-playlist-bot" {
			t.Errorf("query=%v", q)
		}
		if q.Has("account_id") {
			t.Errorf("empty account_id should be omitted")
		}
		writeJSON(w, 200, map[string]string{
			"access_token": "at",
			"expires_at":   "2026-05-08T02:00:00Z",
			"scope":        "playlist-read",
		})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	tok, err := c.CredentialOAuthUser(context.Background(), "alice", "spotify-playlist-bot", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" || tok.Scope != "playlist-read" {
		t.Errorf("tok=%+v", tok)
	}
	if tok.ExpiresAt.IsZero() {
		t.Errorf("expires_at not parsed: %+v", tok)
	}
}

func TestCredentialOAuthUser_AccountIDPropagates(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/credential/oauth-user", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("account_id") != "secondary" {
			t.Errorf("account_id=%q", r.URL.Query().Get("account_id"))
		}
		writeJSON(w, 200, map[string]string{"access_token": "x", "expires_at": ""})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if _, err := c.CredentialOAuthUser(context.Background(), "alice", "app", "secondary"); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialOAuthUser_BadExpiresAt(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/credential/oauth-user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"access_token": "x", "expires_at": "garbage"})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if _, err := c.CredentialOAuthUser(context.Background(), "u", "a", ""); err == nil {
		t.Fatal("expected parse error")
	}
}

// ---------------------------------------------------------------------
// CredentialOAuthClient
// ---------------------------------------------------------------------

func TestCredentialOAuthClient_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/credential/oauth-client", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("app_id") != "spotify-catalog" || q.Has("user_id") {
			t.Errorf("query=%v", q)
		}
		writeJSON(w, 200, map[string]string{
			"access_token": "ct",
			"expires_at":   "2026-05-08T02:00:00Z",
		})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	tok, err := c.CredentialOAuthClient(context.Background(), "spotify-catalog", "")
	if err != nil || tok.AccessToken != "ct" {
		t.Fatalf("err=%v tok=%+v", err, tok)
	}
	if tok.Scope != "" {
		t.Errorf("scope should be empty for oauth_client: %q", tok.Scope)
	}
}

// ---------------------------------------------------------------------
// CredentialExternal
// ---------------------------------------------------------------------

func TestCredentialExternal_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/credential/external", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("user_id") != "alice" || q.Get("app_id") != "anki-app" {
			t.Errorf("query=%v", q)
		}
		writeJSON(w, 200, map[string]any{
			"kind": "anki",
			"payload": map[string]any{
				"hkey":     "secret-key",
				"endpoint": "http://anki:8765",
			},
		})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	cr, err := c.CredentialExternal(context.Background(), "alice", "anki-app", "")
	if err != nil {
		t.Fatal(err)
	}
	if cr.Kind != "anki" || cr.Payload["hkey"] != "secret-key" {
		t.Errorf("got %+v", cr)
	}
}

// ---------------------------------------------------------------------
// ConnectionsExternalComplete
// ---------------------------------------------------------------------

func TestConnectionsExternalComplete_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/connections/external/complete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"connection_id": "uuid-1"})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	id, err := c.ConnectionsExternalComplete(context.Background(), appbroker.ConnectionsExternalCompleteRequest{
		State:        "signed",
		Payload:      map[string]any{"hkey": "k"},
		DisplayLabel: "Phone",
	})
	if err != nil || id != "uuid-1" {
		t.Fatalf("err=%v id=%q", err, id)
	}
	if !strings.Contains(string(fb.gotBody), `"display_label":"Phone"`) {
		t.Errorf("body missing display_label: %s", fb.gotBody)
	}
}

// ---------------------------------------------------------------------
// RevokeConnection
// ---------------------------------------------------------------------

func TestRevokeConnection_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/connections/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("want POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/revoke") {
			t.Errorf("path=%q", r.URL.Path)
		}
		w.WriteHeader(204)
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if err := c.RevokeConnection(context.Background(), "abc-123"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fb.gotPath, "/v1/connections/abc-123/revoke") {
		t.Errorf("path=%q", fb.gotPath)
	}
}

func TestRevokeConnection_PathEscapesID(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/connections/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if err := c.RevokeConnection(context.Background(), "weird id/with slash"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fb.gotPath, "weird id/with slash") {
		t.Errorf("ID should be escaped: %s", fb.gotPath)
	}
}

// ---------------------------------------------------------------------
// Integrations / Connections
// ---------------------------------------------------------------------

func TestIntegrations_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"integrations": []map[string]any{
				{"app_id": "spotify-playlist-bot", "display_name": "Spotify Bot", "provider": "spotify",
					"auth_type": "oauth_user", "scopes": []string{"playlist-read"},
					"visibility": "user", "description": "", "icon_url": ""},
			},
		})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	out, err := c.Integrations(context.Background(), appbroker.IntegrationsRequest{
		UserID: "alice", AuthType: "oauth_user", Visibility: "user", ConsumerApp: "homelab-dashboard",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Integrations) != 1 || out.Integrations[0].AppID != "spotify-playlist-bot" {
		t.Fatalf("bad: %+v", out)
	}
	if !strings.Contains(fb.gotPath, "auth_type=oauth_user") {
		t.Errorf("query missing auth_type: %s", fb.gotPath)
	}
}

func TestConnections_Happy(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/connections", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"connections": []map[string]any{
				{"connection_id": "uuid-1", "app_id": "spotify-playlist-bot", "account_id": "default",
					"status": "active", "scopes": []string{"playlist-read"},
					"created_at": "2026-05-08T00:00:00Z", "failure_count": 0},
			},
		})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	out, err := c.Connections(context.Background(), appbroker.ConnectionsRequest{UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Connections) != 1 || out.Connections[0].ConnectionID != "uuid-1" {
		t.Fatalf("bad: %+v", out)
	}
}

// ---------------------------------------------------------------------
// sentinel error mapping
// ---------------------------------------------------------------------

func TestSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{"401 generic", 401, "unauthorized", appbroker.ErrUnauthorized},
		{"401 invalid_id_token", 401, "invalid_id_token", appbroker.ErrUnauthorized},
		{"401 consent_revoked", 401, "consent_revoked", appbroker.ErrConsentRevoked},
		{"403 mode", 403, "mode_not_allowed", appbroker.ErrForbidden},
		{"404 not_consented", 404, "not_consented", appbroker.ErrNotFound},
		{"409 conflict", 409, "conflict", appbroker.ErrConflict},
		{"502 provider_unavailable", 502, "provider_unavailable", appbroker.ErrUpstreamUnavailable},
		{"503 unavailable", 503, "not_ready", appbroker.ErrUpstreamUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.handle("/v1/credential/oauth-user", func(w http.ResponseWriter, r *http.Request) {
				writeErr(w, tc.status, tc.code, "msg")
			})
			c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
			_, err := c.CredentialOAuthUser(context.Background(), "u", "a", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			var apiErr *appbroker.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *APIError, got %T", err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Errorf("APIError fields: %+v", apiErr)
			}
			if apiErr.Error() == "" {
				t.Error("Error() empty")
			}
		})
	}
}

func TestUnmappedStatus(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 418, "teapot", "i am one")
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	err := c.Healthz(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, sentinel := range []error{
		appbroker.ErrUnauthorized, appbroker.ErrForbidden, appbroker.ErrNotFound,
		appbroker.ErrConflict, appbroker.ErrConsentRevoked, appbroker.ErrUpstreamUnavailable,
	} {
		if errors.Is(err, sentinel) {
			t.Errorf("418 must not match %v", sentinel)
		}
	}
}

func TestErrorBody_NotJSON(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("not json"))
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if err := c.Healthz(context.Background()); !errors.Is(err, appbroker.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestAPIError_NoMessage(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	err := c.Healthz(context.Background())
	var apiErr *appbroker.APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("not APIError")
	}
	if apiErr.Message != "" {
		t.Errorf("expected empty message, got %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Error(), "unauthorized") {
		t.Errorf("Error()=%q", apiErr.Error())
	}
}

// ---------------------------------------------------------------------
// Sign-in (X-Homelab-Identity) forwarding
// ---------------------------------------------------------------------

func TestForwardsIDToken(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"integrations": []any{}})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{
		BaseURL: fb.srv.URL, Token: "t",
	})
	ctx := appbroker.WithIDToken(context.Background(), "id-token-jwt")
	if _, err := c.Integrations(ctx, appbroker.IntegrationsRequest{}); err != nil {
		t.Fatal(err)
	}
	if fb.gotIDTok != "id-token-jwt" {
		t.Errorf("ID token not forwarded: %q", fb.gotIDTok)
	}
}

// A context carrying a sign-in always forwards it.
func TestIDTokenForwardedWithStaticToken(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"integrations": []any{}})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	ctx := appbroker.WithIDToken(context.Background(), "user-jwt")
	if _, err := c.Integrations(ctx, appbroker.IntegrationsRequest{}); err != nil {
		t.Fatal(err)
	}
	if fb.gotIDTok != "user-jwt" {
		t.Errorf("X-Homelab-Identity = %q", fb.gotIDTok)
	}
}

func TestTokenFile(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"integrations": []any{}})
	})
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("projected.sa.token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, TokenFile: f, Token: "ignored"})
	if _, err := c.Integrations(context.Background(), appbroker.IntegrationsRequest{}); err != nil {
		t.Fatal(err)
	}
	if fb.gotAuth != "Bearer projected.sa.token" {
		t.Errorf("auth = %q", fb.gotAuth)
	}
}

func TestTokenFileMissing(t *testing.T) {
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: "http://127.0.0.1:1", TokenFile: "/nonexistent"})
	if _, err := c.Integrations(context.Background(), appbroker.IntegrationsRequest{}); err == nil {
		t.Fatal("want error")
	}
}

func TestFromEnv(t *testing.T) {
	f := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(f, []byte("tok"), 0o600)
	t.Setenv("APP_BROKER_URL", "http://broker.test")
	t.Setenv("APP_BROKER_TOKEN_FILE", f)
	if _, err := appbroker.FromEnv(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_BROKER_TOKEN_FILE", "")
	t.Setenv("APP_BROKER_TOKEN", "static")
	if _, err := appbroker.FromEnv(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_BROKER_TOKEN", "")
	if _, err := os.Stat(appbroker.DefaultTokenFile); err != nil {
		if _, err := appbroker.FromEnv(); err == nil {
			t.Fatal("want error without any credential")
		}
	}
}

func TestNoIDTokenInContext_NoHeader(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/integrations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"integrations": []any{}})
	})
	c := appbroker.NewClient(appbroker.ClientOptions{
		BaseURL: fb.srv.URL, Token: "t",
	})
	if _, err := c.Integrations(context.Background(), appbroker.IntegrationsRequest{}); err != nil {
		t.Fatal(err)
	}
	if fb.gotIDTok != "" {
		t.Errorf("expected no ID token header, got %q", fb.gotIDTok)
	}
}

// ---------------------------------------------------------------------
// context cancel + timeout + URL edge cases
// ---------------------------------------------------------------------

func TestContextCancel(t *testing.T) {
	fb := newFakeBroker(t)
	block := make(chan struct{})
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	t.Cleanup(func() { close(block) })

	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := c.Healthz(ctx)
	if err == nil {
		t.Fatal("expected error from cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestHTTPClientTimeout(t *testing.T) {
	fb := newFakeBroker(t)
	block := make(chan struct{})
	fb.handle("/healthz", func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	t.Cleanup(func() { close(block) })

	c := appbroker.NewClient(appbroker.ClientOptions{
		BaseURL:    fb.srv.URL,
		Token:      "t",
		HTTPClient: &http.Client{Timeout: 30 * time.Millisecond},
	})
	if err := c.Healthz(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestBaseURL_Invalid(t *testing.T) {
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: "http://[::1", Token: "t"})
	if err := c.Healthz(context.Background()); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestDecodeError_BadJSON(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/v1/credential/oauth-user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{not json"))
	})
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	if _, err := c.CredentialOAuthUser(context.Background(), "u", "a", ""); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

// Hit decode-error branches on every method that consumes a JSON body.
func TestDecodeError_AllJSONMethods(t *testing.T) {
	bad := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{not"))
	}
	fb := newFakeBroker(t)
	for _, p := range []string{
		"/v1/connections/start", "/v1/credential/oauth-client",
		"/v1/credential/external", "/v1/integrations",
		"/v1/connections", "/v1/connections/external/complete",
	} {
		fb.handle(p, bad)
	}
	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "t"})
	ctx := context.Background()
	if _, err := c.ConnectionsStart(ctx, appbroker.ConnectionsStartRequest{AppID: "a"}); err == nil {
		t.Error("ConnectionsStart: expected decode err")
	}
	if _, err := c.CredentialOAuthClient(ctx, "a", ""); err == nil {
		t.Error("CredentialOAuthClient: expected decode err")
	}
	if _, err := c.CredentialExternal(ctx, "u", "a", ""); err == nil {
		t.Error("CredentialExternal: expected decode err")
	}
	if _, err := c.Integrations(ctx, appbroker.IntegrationsRequest{}); err == nil {
		t.Error("Integrations: expected decode err")
	}
	if _, err := c.Connections(ctx, appbroker.ConnectionsRequest{UserID: "u"}); err == nil {
		t.Error("Connections: expected decode err")
	}
	if _, err := c.ConnectionsExternalComplete(ctx, appbroker.ConnectionsExternalCompleteRequest{State: "s"}); err == nil {
		t.Error("ConnectionsExternalComplete: expected decode err")
	}
}

// Compile-time evidence that APIError implements error and unwraps.
var _ error = (*appbroker.APIError)(nil)
