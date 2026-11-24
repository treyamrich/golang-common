package appbroker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Healthz calls GET /healthz. Returns nil on 200, an *APIError otherwise.
func (c *Client) Healthz(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		return err
	}
	drainAndClose(resp)
	return nil
}

// Readyz calls GET /readyz. Returns nil on 200, an *APIError on 503
// (wraps ErrUpstreamUnavailable).
func (c *Client) Readyz(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/readyz", nil, nil)
	if err != nil {
		return err
	}
	drainAndClose(resp)
	return nil
}

// ConnectionsStart calls POST /v1/connections/start. The response's
// Method field discriminates between the OAuth and external_session
// flavours; consumers that only need one should typically check it.
func (c *Client) ConnectionsStart(ctx context.Context, req ConnectionsStartRequest) (ConnectionsStartResponse, error) {
	var out ConnectionsStartResponse
	resp, err := c.do(ctx, http.MethodPost, "/v1/connections/start", nil, req)
	if err != nil {
		return out, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return ConnectionsStartResponse{}, err
	}
	return out, nil
}

// CredentialOAuthUser calls GET /v1/credential/oauth-user. Returns
// ErrNotFound if no connection exists (consumer should call
// ConnectionsStart), ErrConsentRevoked if the upstream refresh
// failed (re-consent required), ErrUpstreamUnavailable on provider 5xx.
func (c *Client) CredentialOAuthUser(ctx context.Context, userID, appID, accountID string) (TokenResponse, error) {
	q := url.Values{}
	q.Set("user_id", userID)
	q.Set("app_id", appID)
	if accountID != "" {
		q.Set("account_id", accountID)
	}
	var out tokenWire
	resp, err := c.do(ctx, http.MethodGet, "/v1/credential/oauth-user", q, nil)
	if err != nil {
		return TokenResponse{}, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return TokenResponse{}, err
	}
	return out.toResponse()
}

// CredentialOAuthClient calls GET /v1/credential/oauth-client. There
// is no user — the broker mints an app-only token via the upstream's
// client_credentials grant.
func (c *Client) CredentialOAuthClient(ctx context.Context, appID, accountID string) (TokenResponse, error) {
	q := url.Values{}
	q.Set("app_id", appID)
	if accountID != "" {
		q.Set("account_id", accountID)
	}
	var out tokenWire
	resp, err := c.do(ctx, http.MethodGet, "/v1/credential/oauth-client", q, nil)
	if err != nil {
		return TokenResponse{}, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return TokenResponse{}, err
	}
	return out.toResponse()
}

// CredentialExternal calls GET /v1/credential/external. Returns the
// stored external_session payload (e.g. AnkiConnect hkey + endpoint)
// for the named connection.
func (c *Client) CredentialExternal(ctx context.Context, userID, appID, accountID string) (ExternalCredential, error) {
	q := url.Values{}
	q.Set("user_id", userID)
	q.Set("app_id", appID)
	if accountID != "" {
		q.Set("account_id", accountID)
	}
	var out ExternalCredential
	resp, err := c.do(ctx, http.MethodGet, "/v1/credential/external", q, nil)
	if err != nil {
		return out, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return ExternalCredential{}, err
	}
	return out, nil
}

// ConnectionsExternalComplete calls POST /v1/connections/external/complete.
// Returns the new connection_id on success.
func (c *Client) ConnectionsExternalComplete(ctx context.Context, req ConnectionsExternalCompleteRequest) (string, error) {
	var out struct {
		ConnectionID string `json:"connection_id"`
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/connections/external/complete", nil, req)
	if err != nil {
		return "", err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return "", err
	}
	return out.ConnectionID, nil
}

// RevokeConnection calls POST /v1/connections/{connection_id}/revoke.
// Idempotent — returns nil even when no connection exists.
func (c *Client) RevokeConnection(ctx context.Context, connectionID string) error {
	resp, err := c.do(ctx, http.MethodPost, "/v1/connections/"+url.PathEscape(connectionID)+"/revoke", nil, nil)
	if err != nil {
		return err
	}
	drainAndClose(resp)
	return nil
}

// Integrations calls GET /v1/integrations.
func (c *Client) Integrations(ctx context.Context, req IntegrationsRequest) (IntegrationsResponse, error) {
	q := url.Values{}
	if req.UserID != "" {
		q.Set("user_id", req.UserID)
	}
	if req.ConsumerApp != "" {
		q.Set("consumer_app", req.ConsumerApp)
	}
	if req.AuthType != "" {
		q.Set("auth_type", req.AuthType)
	}
	if req.Visibility != "" {
		q.Set("visibility", req.Visibility)
	}
	var out IntegrationsResponse
	resp, err := c.do(ctx, http.MethodGet, "/v1/integrations", q, nil)
	if err != nil {
		return out, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return IntegrationsResponse{}, err
	}
	return out, nil
}

// Connections calls GET /v1/connections.
func (c *Client) Connections(ctx context.Context, req ConnectionsRequest) (ConnectionsResponse, error) {
	q := url.Values{}
	if req.UserID != "" {
		q.Set("user_id", req.UserID)
	}
	if req.ConsumerApp != "" {
		q.Set("consumer_app", req.ConsumerApp)
	}
	var out ConnectionsResponse
	resp, err := c.do(ctx, http.MethodGet, "/v1/connections", q, nil)
	if err != nil {
		return out, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return ConnectionsResponse{}, err
	}
	return out, nil
}

// tokenWire mirrors the broker's wire format for credential responses
// (expires_at is RFC3339 string on the wire). We translate to a typed
// time.Time on the boundary.
type tokenWire struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   string `json:"expires_at"`
	Scope       string `json:"scope"`
}

func (t tokenWire) toResponse() (TokenResponse, error) {
	out := TokenResponse{
		AccessToken: t.AccessToken,
		Scope:       t.Scope,
	}
	if t.ExpiresAt != "" {
		ts, err := time.Parse(time.RFC3339, t.ExpiresAt)
		if err != nil {
			return TokenResponse{}, fmt.Errorf("app-broker: parse expires_at: %w", err)
		}
		out.ExpiresAt = ts
	}
	return out, nil
}
