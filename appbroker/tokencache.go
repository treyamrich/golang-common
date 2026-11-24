package appbroker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultTokenSkew is how long before expiry a cached token is treated
// as stale, so a token is never handed out moments before it expires.
const DefaultTokenSkew = 30 * time.Second

// TokenCache is an in-process cache of access tokens fetched from the
// broker, so a worker making many upstream calls per user does one
// broker round trip per token lifetime instead of one per call.
//
// The broker itself caches and refreshes upstream tokens; this cache
// only saves the hop to the broker. It is safe for concurrent use.
// Concurrent misses for the same key may each fetch; the last result
// wins, which is harmless because the broker returns its cached token.
type TokenCache struct {
	c    *Client
	skew time.Duration
	now  func() time.Time

	mu     sync.Mutex
	tokens map[tokenKey]TokenResponse
}

type tokenKey struct {
	kind      byte // 'u' user, 'c' client
	userID    string
	appID     string
	accountID string
}

// TokenCacheOption configures a TokenCache.
type TokenCacheOption func(*TokenCache)

// WithTokenSkew overrides DefaultTokenSkew.
func WithTokenSkew(d time.Duration) TokenCacheOption {
	return func(t *TokenCache) { t.skew = d }
}

// WithClock overrides time.Now (tests).
func WithClock(now func() time.Time) TokenCacheOption {
	return func(t *TokenCache) { t.now = now }
}

// NewTokenCache returns a TokenCache that fetches through c.
func NewTokenCache(c *Client, opts ...TokenCacheOption) *TokenCache {
	t := &TokenCache{c: c, skew: DefaultTokenSkew, now: time.Now, tokens: map[tokenKey]TokenResponse{}}
	for _, o := range opts {
		o(t)
	}
	return t
}

// UserToken returns an access token for an install (an oauth_user
// app), from cache when still fresh.
func (t *TokenCache) UserToken(ctx context.Context, in Install) (TokenResponse, error) {
	return t.get(ctx, tokenKey{kind: 'u', userID: in.UserID, appID: in.AppID, accountID: in.AccountID})
}

// ClientToken returns an app-level (oauth_client) access token, from
// cache when still fresh.
func (t *TokenCache) ClientToken(ctx context.Context, appID, accountID string) (TokenResponse, error) {
	return t.get(ctx, tokenKey{kind: 'c', appID: appID, accountID: accountID})
}

// InvalidateUser drops the cached token for an install. Call it when
// the upstream API rejects a token (HTTP 401) so the next UserToken
// asks the broker again.
func (t *TokenCache) InvalidateUser(in Install) {
	t.drop(tokenKey{kind: 'u', userID: in.UserID, appID: in.AppID, accountID: in.AccountID})
}

// InvalidateClient drops the cached app-level token.
func (t *TokenCache) InvalidateClient(appID, accountID string) {
	t.drop(tokenKey{kind: 'c', appID: appID, accountID: accountID})
}

func (t *TokenCache) get(ctx context.Context, k tokenKey) (TokenResponse, error) {
	t.mu.Lock()
	tok, ok := t.tokens[k]
	t.mu.Unlock()
	if ok && t.fresh(tok) {
		return tok, nil
	}

	var err error
	switch k.kind {
	case 'u':
		if k.userID == "" || k.appID == "" {
			return TokenResponse{}, errors.New("app-broker: UserToken: install needs UserID and AppID")
		}
		tok, err = t.c.CredentialOAuthUser(ctx, k.userID, k.appID, k.accountID)
	default:
		if k.appID == "" {
			return TokenResponse{}, errors.New("app-broker: ClientToken: appID is required")
		}
		tok, err = t.c.CredentialOAuthClient(ctx, k.appID, k.accountID)
	}
	if err != nil {
		t.drop(k)
		return TokenResponse{}, err
	}
	if t.fresh(tok) {
		t.mu.Lock()
		t.tokens[k] = tok
		t.mu.Unlock()
	}
	return tok, nil
}

// fresh reports whether tok is usable for at least skew more. A token
// with no expiry is never cached.
func (t *TokenCache) fresh(tok TokenResponse) bool {
	return tok.AccessToken != "" && !tok.ExpiresAt.IsZero() && t.now().Before(tok.ExpiresAt.Add(-t.skew))
}

func (t *TokenCache) drop(k tokenKey) {
	t.mu.Lock()
	delete(t.tokens, k)
	t.mu.Unlock()
}
