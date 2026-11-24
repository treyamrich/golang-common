package appbroker_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/treyamrich/golang-common/appbroker"
)

// A worker acting for every user who installed its app. In the cluster,
// FromEnv picks up the projected service-account token that
// app-template's appBroker block mounts.
func Example() {
	c, err := appbroker.FromEnv()
	if err != nil {
		fmt.Println("no broker credential:", err)
		return
	}
	tokens := appbroker.NewTokenCache(c)
	err = c.ForEachInstall(context.Background(), "spotify-playlist-bot", func(ctx context.Context, in appbroker.Install) error {
		tok, err := tokens.UserToken(ctx, in)
		switch {
		case errors.Is(err, appbroker.ErrConsentRevoked), errors.Is(err, appbroker.ErrNotFound):
			return nil // the user must reinstall from the dashboard
		case err != nil:
			return err
		}
		_ = tok.AccessToken // call the provider API as this user
		return nil
	})
	if err != nil {
		fmt.Println(err)
	}
}

// A UI acting for the signed-in user forwards their Authentik ID token
// on the request context (the consumer must declare userSignIn).
func ExampleWithIDToken() {
	c := appbroker.NewClient(appbroker.ClientOptions{
		BaseURL:   appbroker.DefaultBaseURL,
		TokenFile: appbroker.DefaultTokenFile,
	})
	ctx := appbroker.WithIDToken(context.Background(), "eyJhbGc...id-token...")
	if _, err := c.ConnectionsStart(ctx, appbroker.ConnectionsStartRequest{AppID: "spotify-playlist-bot"}); err != nil {
		fmt.Println(err)
	}
}
