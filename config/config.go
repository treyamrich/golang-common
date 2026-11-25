// Package config loads a service's config from environment variables.
//
// Config is a struct with env tags (github.com/caarlos0/env):
//
//	type Config struct {
//	    API  api.Config      // nested structs are walked; their tags apply
//	    DB   postgres.Config
//	    Auth auth.OIDCConfig
//	    PollInterval time.Duration `env:"POLL_INTERVAL" envDefault:"10s"`
//	    APIKey       string        `env:"API_KEY,required,file"` // ,file: the env var holds a path
//	}
//
// Env vars only (12-factor). Secrets come from Kubernetes Secrets as env
// vars, or as files (Vault Agent, projected volumes) with ",file".
// Never log a config struct: it holds secrets.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// Load parses T from the environment, applying defaults and failing on
// missing required values or unparsable ones.
func Load[T any]() (T, error) {
	cfg, err := env.ParseAs[T]()
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
