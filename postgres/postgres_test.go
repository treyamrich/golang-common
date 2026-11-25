package postgres

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestURL(t *testing.T) {
	// Reserved URL characters in the credential must survive escaping.
	cred := strings.Join([]string{"a", "b", "c"}, "@/:")
	raw := Config{Host: "db", Port: 5432, Name: "tasks", User: "v-k8s-x", Password: cred, SSLMode: "disable", SearchPath: "tasks"}.URL()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := u.User.Password()
	if u.User.Username() != "v-k8s-x" || got != cred || u.Host != "db:5432" || u.Path != "/tasks" ||
		u.Query().Get("search_path") != "tasks" || u.Query().Get("sslmode") != "disable" {
		t.Fatalf("parsed %+v", u)
	}
}

func TestStartTimesOutOnUnreachable(t *testing.T) {
	db, err := New(Config{Host: "127.0.0.1", Port: 1, Name: "x", User: "u", SSLMode: "disable", StartTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	err = db.Start(context.Background())
	if err == nil || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("err=%v", err)
	}
}
