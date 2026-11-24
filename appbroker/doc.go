// Package appbroker is the Go SDK services use to talk to the
// homelab app-broker. It mirrors the broker's HTTP surface as typed
// methods and exposes sentinel errors covering the meaningful 4xx/5xx
// outcomes.
//
// The package depends only on the Go standard library — by design it
// does NOT import the broker's internal packages, so it can be
// imported by external consumers (spotify-service, homelab-dashboard, …)
// without dragging server-side concerns along.
//
// # Authentication
//
// Every caller is a service declared by a BrokerConsumer resource in its
// own chart. In the cluster it authenticates with a projected
// service-account token (audience app-broker), which app-template's
// appBroker block mounts and FromEnv picks up:
//
//	c, err := appbroker.FromEnv()
//
// Off-cluster callers (a CLI) use the static token whose consumer
// declares offClusterToken:
//
//	c := appbroker.NewClient(appbroker.ClientOptions{BaseURL: url, Token: os.Getenv("APP_BROKER_TOKEN")})
//
// To act for a signed-in user (required for install, uninstall and
// listing the user's connections), forward their Authentik ID token on
// the request context; the consumer must declare userSignIn:
//
//	ctx := appbroker.WithIDToken(r.Context(), idToken)
//	resp, err := c.ConnectionsStart(ctx, appbroker.ConnectionsStartRequest{AppID: "spotify-playlist-bot"})
//
// # Endpoint mapping
//
//	ConnectionsStart                → POST /v1/connections/start
//	ConnectionsExternalComplete     → POST /v1/connections/external/complete
//	RevokeConnection                → POST /v1/connections/{connection_id}/revoke
//	CredentialOAuthUser             → GET  /v1/credential/oauth-user
//	CredentialOAuthClient           → GET  /v1/credential/oauth-client
//	CredentialExternal              → GET  /v1/credential/external
//	Integrations                    → GET  /v1/integrations
//	Connections                     → GET  /v1/connections
//	Installs / ForEachInstall       → GET  /v1/apps/{app_id}/installs
//	Healthz / Readyz                → GET  /healthz, /readyz
//
// # Errors
//
// Methods return typed sentinel errors that wrap the HTTP status and
// the broker's standard {"error":"<code>","message":"<...>"} body.
// Use errors.Is to branch:
//
//	tok, err := c.CredentialOAuthUser(ctx, "alice", "spotify-playlist-bot", "")
//	switch {
//	case errors.Is(err, appbroker.ErrConsentRevoked):
//	    // trigger re-consent: call ConnectionsStart
//	case errors.Is(err, appbroker.ErrUpstreamUnavailable):
//	    // retry with backoff
//	case err != nil:
//	    return err
//	}
package appbroker
