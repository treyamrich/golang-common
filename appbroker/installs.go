package appbroker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Install is one user's installation of an app, as returned by
// GET /v1/apps/{app_id}/installs.
//
// A worker never decides on its own which users to act for: it
// asks the broker who installed its app and acts for exactly those
// users. Installing is the user's grant; uninstalling (revoking the
// connection) withdraws it on the next run.
type Install struct {
	AppID        string
	ConnectionID string
	// UserID is the user's homelab identity (Authentik `sub`). Use it,
	// together with AccountID, as the key for any per-user state the
	// consuming service keeps.
	UserID    string
	AccountID string
	// DisplayLabel is a human-readable label for the linked provider
	// account (e.g. the Spotify display name). For display only.
	DisplayLabel string
	// Status is "active" or "failing" ("failing" connections still hold
	// a refresh token and may recover).
	Status    string
	CreatedAt time.Time
}

type installWire struct {
	AppID        string `json:"app_id"`
	ConnectionID string `json:"connection_id"`
	UserID       string `json:"user_id"`
	AccountID    string `json:"account_id"`
	DisplayLabel string `json:"display_label"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

// Installs maps GET /v1/apps/{app_id}/installs. For services only: the
// broker rejects a request carrying a user sign-in (WithIDToken) with
// ErrForbidden, since a request acting for one signed-in user must not
// enumerate others. Also ErrForbidden when this consumer's
// BrokerConsumer doesn't list the app in userApps.
func (c *Client) Installs(ctx context.Context, appID string) ([]Install, error) {
	if appID == "" {
		return nil, errors.New("app-broker: Installs: appID is required")
	}
	var out struct {
		Installs []installWire `json:"installs"`
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(appID)+"/installs", nil, nil)
	if err != nil {
		return nil, err
	}
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	installs := make([]Install, 0, len(out.Installs))
	for _, w := range out.Installs {
		in := Install{
			AppID:        w.AppID,
			ConnectionID: w.ConnectionID,
			UserID:       w.UserID,
			AccountID:    w.AccountID,
			DisplayLabel: w.DisplayLabel,
			Status:       w.Status,
		}
		if in.AppID == "" {
			in.AppID = appID
		}
		if w.CreatedAt != "" {
			ts, err := time.Parse(time.RFC3339, w.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("app-broker: parse created_at: %w", err)
			}
			in.CreatedAt = ts
		}
		installs = append(installs, in)
	}
	return installs, nil
}

// InstallError reports that the callback passed to ForEachInstall
// failed for one install.
type InstallError struct {
	Install Install
	Err     error
}

func (e *InstallError) Error() string {
	return fmt.Sprintf("app-broker: install %s (account %s): %v", e.Install.ConnectionID, e.Install.AccountID, e.Err)
}

func (e *InstallError) Unwrap() error { return e.Err }

// ForEachInstall lists the users who installed appID and calls fn once
// for each, sequentially, oldest install first.
//
// One user's failure does not stop the others: errors returned by fn
// are wrapped in *InstallError and joined (errors.Join) into the
// returned error, so errors.Is / errors.As work across them. A failure
// to list installs is returned as-is and fn is never called. Context
// cancellation stops the loop and is included in the returned error.
//
//	cache := appbroker.NewTokenCache(c)
//	err := c.ForEachInstall(ctx, "spotify-playlist-bot", func(ctx context.Context, in appbroker.Install) error {
//	    tok, err := cache.UserToken(ctx, in)
//	    if err != nil {
//	        return err
//	    }
//	    return syncPlaylists(ctx, in.UserID, tok.AccessToken)
//	})
func (c *Client) ForEachInstall(ctx context.Context, appID string, fn func(context.Context, Install) error) error {
	installs, err := c.Installs(ctx, appID)
	if err != nil {
		return err
	}
	var errs []error
	for _, in := range installs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := fn(ctx, in); err != nil {
			errs = append(errs, &InstallError{Install: in, Err: err})
		}
	}
	return errors.Join(errs...)
}
