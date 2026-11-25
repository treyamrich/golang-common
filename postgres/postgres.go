// Package postgres is the standard Postgres dependency: a pgx pool that
// plugs into app's lifecycle (Start pings, Check backs /readyz, Close
// drains the pool).
//
// It only manages the connection. Queries live in the service's
// outbound adapter, each wrapped in observe.Call so it is measured.
//
// Credentials: in the homelab the runtime user is a short-lived Vault
// database user, leased by VSO into a Secret and passed as DB_USER /
// DB_PASSWORD. That user can read and write rows but owns no schema;
// migrations run separately (Flyway) as a different user.
package postgres

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is the connection config; embed it in the service config.
type Config struct {
	Host     string `env:"DB_HOST,required"`
	Port     int    `env:"DB_PORT" envDefault:"5432"`
	Name     string `env:"DB_NAME,required"`
	User     string `env:"DB_USER,required"`
	Password string `env:"DB_PASSWORD,required"`
	SSLMode  string `env:"DB_SSLMODE" envDefault:"disable"`
	// SearchPath sets the schema(s) unqualified names resolve to.
	SearchPath string `env:"DB_SEARCH_PATH"`
	// MaxConns caps the pool. Each process holds up to this many
	// server connections, so keep it small.
	MaxConns int32 `env:"DB_MAX_CONNS" envDefault:"5"`
	// MaxConnLifetime recycles connections. Default 30m.
	MaxConnLifetime time.Duration `env:"DB_MAX_CONN_LIFETIME" envDefault:"30m"`
	// StartTimeout bounds the startup ping (with retries). Default 30s:
	// the database may still be coming up when the pod starts.
	StartTimeout time.Duration `env:"DB_START_TIMEOUT" envDefault:"30s"`
}

// URL renders cfg as a postgres:// URL. It contains the password.
func (c Config) URL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:   "/" + c.Name,
	}
	q := url.Values{}
	if c.SSLMode != "" {
		q.Set("sslmode", c.SSLMode)
	}
	if c.SearchPath != "" {
		q.Set("search_path", c.SearchPath)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// DB is a pool with a lifecycle. Use Pool once Start has returned.
type DB struct {
	cfg  Config
	Pool *pgxpool.Pool
}

// New builds the pool without connecting.
func New(cfg Config) (*DB, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL())
	if err != nil {
		return nil, fmt.Errorf("postgres: config: %w", err) // never echo the URL
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: pool: %w", err)
	}
	return &DB{cfg: cfg, Pool: pool}, nil
}

// Start pings until it succeeds or StartTimeout passes.
func (d *DB) Start(ctx context.Context) error {
	timeout := d.cfg.StartTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var err error
	for {
		if err = d.Pool.Ping(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres: %s:%d/%s unreachable: %w", d.cfg.Host, d.cfg.Port, d.cfg.Name, err)
		case <-time.After(time.Second):
		}
	}
}

// Check pings, for /readyz.
func (d *DB) Check(ctx context.Context) error { return d.Pool.Ping(ctx) }

// Close waits for borrowed connections to be returned, then closes.
func (d *DB) Close(context.Context) error {
	d.Pool.Close()
	return nil
}
