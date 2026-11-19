package processes

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/pgsetting"
)

// The registry opens a pool of its own from the store's configuration (Register), which dials when
// the registry first asks it for a connection, and again after a connection is lost, so each of
// its statements can be the one whose connect fails, and pgx's error names DATABASE_URL's user,
// where a token can stand. So the registration, a heartbeat and the deregistration each name such
// a failure without it, and errors.As still finds the ConnectError under it. The control is a pool
// of the same configuration, asked the same, whose error names the token. The token and the
// password are made at each run, so no credential is written in this file.
func TestTheRegistryNamesAFailedConnectWithoutTheUser(t *testing.T) {
	token, password := "made-up-token-"+rand.Text(), "made-up-"+rand.Text()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword(token, password), Host: "127.0.0.1:1", Path: "/x", RawQuery: "sslmode=disable&connect_timeout=3"}).String()
	config, err := pgsetting.ParsePoolConfig("DATABASE_URL", dsn)
	if err != nil {
		t.Fatalf("ParsePoolConfig: %v", err)
	}
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	pool := store.NewPool(opened)
	defer pool.Close()
	control, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	defer control.Close()
	if _, err := control.Exec(ctx, "select 1"); err == nil || !strings.Contains(err.Error(), token) {
		t.Fatalf("UNKNOWN: a pool of the store's configuration does not name the user when its connect fails, so this test saw nothing: %v", err)
	}
	registered := func() *Registration {
		own, err := pgxpool.NewWithConfig(ctx, pool.Config())
		if err != nil {
			t.Fatalf("NewWithConfig: %v", err)
		}
		t.Cleanup(own.Close)
		stopped := make(chan struct{})
		close(stopped)
		return &Registration{ID: "made-up-instance", pool: own, stop: func() {}, stopped: stopped, lost: make(chan error, 1)}
	}
	for _, statement := range []struct {
		name string
		run  func() error
	}{
		{"Register", func() error {
			_, err := Register(ctx, pool, Instance{SigningKeys: []string{"made-up-key"}})
			return err
		}},
		{"a heartbeat", func() error { _, err := registered().renew(ctx); return err }},
		{"Deregister", func() error { return registered().Deregister(ctx) }},
	} {
		t.Run(statement.name, func(t *testing.T) {
			err := statement.run()
			var connect *pgconn.ConnectError
			if !errors.As(err, &connect) {
				t.Fatalf("%v, want a failed connect", err)
			}
			for _, part := range []string{token, password} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("the failed connect holds a part of the user part: %q", err)
				}
			}
			if !strings.Contains(err.Error(), "127.0.0.1:1") {
				t.Errorf("%q, want what failed at 127.0.0.1:1", err)
			}
		})
	}
}
