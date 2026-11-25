package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type nested struct {
	Addr string `env:"ADDR" envDefault:":8080"`
}

type cfg struct {
	Nested   nested
	Interval time.Duration `env:"INTERVAL" envDefault:"10s"`
	Required string        `env:"REQUIRED,required"`
	Secret   string        `env:"SECRET_FILE,file"`
}

func TestLoad(t *testing.T) {
	f := filepath.Join(t.TempDir(), "s")
	_ = os.WriteFile(f, []byte("hunter2"), 0o600)
	t.Setenv("REQUIRED", "yes")
	t.Setenv("INTERVAL", "3s")
	t.Setenv("SECRET_FILE", f)
	c, err := Load[cfg]()
	if err != nil {
		t.Fatal(err)
	}
	if c.Nested.Addr != ":8080" || c.Interval != 3*time.Second || c.Required != "yes" || c.Secret != "hunter2" {
		t.Fatalf("%+v", c)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	os.Unsetenv("REQUIRED")
	if _, err := Load[cfg](); err == nil {
		t.Fatal("want error")
	}
}
