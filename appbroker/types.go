package appbroker

import "time"

// Request/response types — one per §6.x endpoint of the post-PR-2A
// surface. JSON tags mirror the broker's wire format exactly (see
// internal/http/handlers.go).

// ConnectionsStartRequest is the body of POST /v1/connections/start.
//
// UserID is required for oauth_user and external_session apps. It is
// omitted (and the broker returns 400) for oauth_client apps, which
// have no user. AccountID defaults to "default" server-side when empty.
type ConnectionsStartRequest struct {
	UserID    string `json:"user_id,omitempty"`
	AppID     string `json:"app_id"`
	AccountID string `json:"account_id,omitempty"`
}

// ConnectionsStartResponse is the JSON returned by /v1/connections/start.
//
// Method discriminates: "oauth_user" → RedirectURL is the upstream
// authorize URL; "external_session" → RedirectURL is the catalog's
// form_url_template with the signed state token substituted in.
type ConnectionsStartResponse struct {
	Method      string `json:"method"`
	RedirectURL string `json:"redirect_url"`
	State       string `json:"state"`
}

// TokenResponse is the JSON returned by /v1/credential/oauth-user and
// /v1/credential/oauth-client. Scope is populated for oauth_user and
// empty for oauth_client.
type TokenResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	Scope       string    `json:"scope,omitempty"`
}

// ExternalCredential is the JSON returned by /v1/credential/external.
// Kind is the discriminator (e.g. "anki"); Payload merges the public
// catalog fields with the secret material from Vault.
type ExternalCredential struct {
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload"`
}

// ConnectionsExternalCompleteRequest is the body of
// POST /v1/connections/external/complete.
type ConnectionsExternalCompleteRequest struct {
	State        string         `json:"state"`
	Payload      map[string]any `json:"payload"`
	DisplayLabel string         `json:"display_label,omitempty"`
}

// IntegrationsRequest is the query-param input to GET /v1/integrations.
type IntegrationsRequest struct {
	UserID      string // when no sign-in is forwarded
	ConsumerApp string
	AuthType    string // "oauth_user" | "oauth_client" | "external_session"; empty = no filter
	Visibility  string // "user" (default) | "system"
}

// Integration is one item in IntegrationsResponse.Integrations.
type Integration struct {
	AppID       string   `json:"app_id"`
	DisplayName string   `json:"display_name"`
	Provider    string   `json:"provider"`
	AuthType    string   `json:"auth_type"`
	Scopes      []string `json:"scopes"`
	Visibility  string   `json:"visibility"`
	Description string   `json:"description"`
	IconURL     string   `json:"icon_url"`
}

// IntegrationsResponse is the JSON returned by GET /v1/integrations.
type IntegrationsResponse struct {
	Integrations []Integration `json:"integrations"`
}

// ConnectionsRequest is the query-param input to GET /v1/connections.
type ConnectionsRequest struct {
	UserID      string // when no sign-in is forwarded
	ConsumerApp string
}

// Connection is one item in ConnectionsResponse.Connections.
type Connection struct {
	ConnectionID  string   `json:"connection_id"`
	AppID         string   `json:"app_id"`
	DisplayName   string   `json:"display_name,omitempty"`
	DisplayLabel  string   `json:"display_label,omitempty"`
	IconURL       string   `json:"icon_url,omitempty"`
	AccountID     string   `json:"account_id"`
	AuthMethod    string   `json:"auth_method,omitempty"`
	ExternalID    string   `json:"external_id,omitempty"`
	Status        string   `json:"status"`
	Scopes        []string `json:"scopes"`
	CreatedAt     string   `json:"created_at"`
	LastRefreshAt string   `json:"last_refresh_at,omitempty"`
	FailureCount  int      `json:"failure_count"`
}

// ConnectionsResponse is the JSON returned by GET /v1/connections.
type ConnectionsResponse struct {
	Connections []Connection `json:"connections"`
}
