package appbroker_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treyamrich/golang-common/appbroker"
)

func modeB(fb *fakeBroker) *appbroker.Client {
	return appbroker.NewClient(appbroker.ClientOptions{BaseURL: fb.srv.URL, Token: "tok"})
}

func installsHandler(created string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"installs": []map[string]any{
			{"app_id": "spotify-playlist-bot", "connection_id": "c1", "user_id": "sub-a", "account_id": "default", "display_label": "alice", "status": "active", "created_at": created},
			{"app_id": "spotify-playlist-bot", "connection_id": "c2", "user_id": "sub-b", "account_id": "work", "status": "failing", "created_at": created},
		}})
	}
}

// --- Installs ---------------------------------------------------------

func TestInstalls(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("GET /v1/apps/spotify-playlist-bot/installs", installsHandler("2026-11-24T10:00:00Z"))
	got, err := modeB(fb).Installs(context.Background(), "spotify-playlist-bot")
	if err != nil {
		t.Fatal(err)
	}
	if fb.gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", fb.gotAuth)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	want0 := appbroker.Install{
		AppID: "spotify-playlist-bot", ConnectionID: "c1", UserID: "sub-a", AccountID: "default",
		DisplayLabel: "alice", Status: "active", CreatedAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
	}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v, want %+v", got[0], want0)
	}
	if got[1].AccountID != "work" || got[1].Status != "failing" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestInstalls_EscapesAppID(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("/", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"installs": []any{}}) })
	if _, err := modeB(fb).Installs(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
	if fb.gotPath != "/v1/apps/a%2Fb/installs" {
		t.Errorf("path = %q", fb.gotPath)
	}
}

func TestInstalls_Forbidden(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("GET /v1/apps/x/installs", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 403, "not_allowed", "consumer not authorised for this app")
	})
	_, err := modeB(fb).Installs(context.Background(), "x")
	if !errors.Is(err, appbroker.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestInstalls_RequiresAppID(t *testing.T) {
	fb := newFakeBroker(t)
	if _, err := modeB(fb).Installs(context.Background(), ""); err == nil {
		t.Fatal("want error")
	}
	if fb.hits.Load() != 0 {
		t.Error("no request should be sent")
	}
}

// --- ForEachInstall ------------------------------------------------------

func TestForEachInstall_ContinuesPastFailures(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("GET /v1/apps/spotify-playlist-bot/installs", installsHandler("2026-11-24T10:00:00Z"))
	boom := errors.New("boom")
	var seen []string
	err := modeB(fb).ForEachInstall(context.Background(), "spotify-playlist-bot", func(_ context.Context, in appbroker.Install) error {
		seen = append(seen, in.UserID)
		if in.UserID == "sub-a" {
			return boom
		}
		return nil
	})
	if len(seen) != 2 || seen[0] != "sub-a" || seen[1] != "sub-b" {
		t.Fatalf("seen = %v", seen)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("want boom in joined error, got %v", err)
	}
	var ie *appbroker.InstallError
	if !errors.As(err, &ie) || ie.Install.ConnectionID != "c1" {
		t.Fatalf("want *InstallError for c1, got %v", err)
	}
}

func TestForEachInstall_AllSucceed(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("GET /v1/apps/spotify-playlist-bot/installs", installsHandler("2026-11-24T10:00:00Z"))
	if err := modeB(fb).ForEachInstall(context.Background(), "spotify-playlist-bot", func(context.Context, appbroker.Install) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestForEachInstall_ListErrorSkipsCallback(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("GET /v1/apps/x/installs", func(w http.ResponseWriter, r *http.Request) { writeErr(w, 503, "not_ready", "") })
	called := false
	err := modeB(fb).ForEachInstall(context.Background(), "x", func(context.Context, appbroker.Install) error { called = true; return nil })
	if !errors.Is(err, appbroker.ErrUpstreamUnavailable) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestForEachInstall_StopsOnCancel(t *testing.T) {
	fb := newFakeBroker(t)
	fb.handle("GET /v1/apps/spotify-playlist-bot/installs", installsHandler("2026-11-24T10:00:00Z"))
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := modeB(fb).ForEachInstall(ctx, "spotify-playlist-bot", func(context.Context, appbroker.Install) error {
		calls++
		cancel()
		return nil
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

// --- TokenCache ------------------------------------------------------------

func tokenHandler(hits *atomic.Int32, token string, expiresAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeJSON(w, 200, map[string]string{"access_token": token, "expires_at": expiresAt.UTC().Format(time.RFC3339)})
	}
}

func TestTokenCache_UserTokenCachesUntilSkew(t *testing.T) {
	fb := newFakeBroker(t)
	var hits atomic.Int32
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	fb.handle("GET /v1/credential/oauth-user", tokenHandler(&hits, "at", now.Add(time.Hour)))
	cache := appbroker.NewTokenCache(modeB(fb), appbroker.WithClock(func() time.Time { return now }))
	in := appbroker.Install{AppID: "spotify-playlist-bot", UserID: "sub-a", AccountID: "default"}

	for i := 0; i < 3; i++ {
		tok, err := cache.UserToken(context.Background(), in)
		if err != nil || tok.AccessToken != "at" {
			t.Fatalf("tok=%+v err=%v", tok, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("want 1 broker hit, got %d", hits.Load())
	}
	if fb.gotPath != "/v1/credential/oauth-user?account_id=default&app_id=spotify-playlist-bot&user_id=sub-a" {
		t.Errorf("path = %q", fb.gotPath)
	}

	// Within the skew window of expiry → refetch.
	now = now.Add(time.Hour - 10*time.Second)
	if _, err := cache.UserToken(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("want refetch inside skew window, hits=%d", hits.Load())
	}
}

func TestTokenCache_KeysAreIndependent(t *testing.T) {
	fb := newFakeBroker(t)
	var hits atomic.Int32
	fb.handle("GET /v1/credential/oauth-user", tokenHandler(&hits, "at", time.Now().Add(time.Hour)))
	cache := appbroker.NewTokenCache(modeB(fb))
	a := appbroker.Install{AppID: "app", UserID: "sub-a", AccountID: "default"}
	b := appbroker.Install{AppID: "app", UserID: "sub-b", AccountID: "default"}
	aWork := appbroker.Install{AppID: "app", UserID: "sub-a", AccountID: "work"}
	for _, in := range []appbroker.Install{a, b, aWork, a, b, aWork} {
		if _, err := cache.UserToken(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("want one fetch per (user, app, account), got %d", hits.Load())
	}
}

func TestTokenCache_Invalidate(t *testing.T) {
	fb := newFakeBroker(t)
	var hits atomic.Int32
	fb.handle("GET /v1/credential/oauth-user", tokenHandler(&hits, "at", time.Now().Add(time.Hour)))
	cache := appbroker.NewTokenCache(modeB(fb))
	in := appbroker.Install{AppID: "app", UserID: "sub-a"}
	_, _ = cache.UserToken(context.Background(), in)
	cache.InvalidateUser(in)
	_, _ = cache.UserToken(context.Background(), in)
	if hits.Load() != 2 {
		t.Fatalf("want refetch after invalidate, hits=%d", hits.Load())
	}
}

func TestTokenCache_ErrorsAreNotCached(t *testing.T) {
	fb := newFakeBroker(t)
	var hits atomic.Int32
	fb.handle("GET /v1/credential/oauth-user", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeErr(w, 401, "consent_revoked", "re-consent")
	})
	cache := appbroker.NewTokenCache(modeB(fb))
	in := appbroker.Install{AppID: "app", UserID: "sub-a"}
	for i := 0; i < 2; i++ {
		if _, err := cache.UserToken(context.Background(), in); !errors.Is(err, appbroker.ErrConsentRevoked) {
			t.Fatalf("want ErrConsentRevoked, got %v", err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("errors must not be cached, hits=%d", hits.Load())
	}
}

func TestTokenCache_ClientToken(t *testing.T) {
	fb := newFakeBroker(t)
	var hits atomic.Int32
	fb.handle("GET /v1/credential/oauth-client", tokenHandler(&hits, "cc", time.Now().Add(time.Hour)))
	cache := appbroker.NewTokenCache(modeB(fb))
	for i := 0; i < 2; i++ {
		tok, err := cache.ClientToken(context.Background(), "spotify-catalog", "")
		if err != nil || tok.AccessToken != "cc" {
			t.Fatalf("tok=%+v err=%v", tok, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestTokenCache_RejectsIncompleteInstall(t *testing.T) {
	fb := newFakeBroker(t)
	cache := appbroker.NewTokenCache(modeB(fb))
	if _, err := cache.UserToken(context.Background(), appbroker.Install{AppID: "app"}); err == nil {
		t.Fatal("want error for missing UserID")
	}
	if fb.hits.Load() != 0 {
		t.Error("no request should be sent")
	}
}
