package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestURL(t *testing.T) {
	u := Config{Host: "db", Port: 5432, Name: "tasks", User: "v-k8s-x", Password: "p@ss/w:rd", SSLMode: "disable", SearchPath: "tasks"}.URL()
	if !strings.HasPrefix(u, "postgres://v-k8s-x:p%40ss%2Fw%3Ard@db:5432/tasks?") || !strings.Contains(u, "search_path=tasks") {
		t.Fatal(u)
	}
}

func TestStartTimesOutOnUnreachable(t *testing.T) {
	db, err := New(Config{Host: "127.0.0.1", Port: 1, Name: "x", User: "u", Password: "p", SSLMode: "disable", StartTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	err = db.Start(context.Background())
	if err == nil || strings.Contains(err.Error(), "p@") {
		t.Fatalf("err=%v", err)
	}
}
