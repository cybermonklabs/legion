package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADatabaseURLPgxCannotReadIsNamedByItsSetting: dev sign-in's check of DATABASE_URL
// (loopbackDatabase) refuses one pgx cannot read by the setting, in fixed words, "DATABASE_URL: pgx
// cannot read its connection settings", which still unwrap to pgx's error. pgx's ParseConfigError
// quotes the URL with its password masked but its user name and every other query pair kept, and
// the error it wraps quotes a query key that has no "=" alone. Each case holds an sslmode pgx
// refuses beside one marker: the probe's token as a query key (query-key), and the probe's user and
// password as the user information with its token as an access token's value (user). The twin shows
// each URL as written, and pgx's own error of it, holds its markers. Captures
// dispatch.database-url/<case>.
func TestADatabaseURLPgxCannotReadIsNamedByItsSetting(t *testing.T) {
	t.Parallel()
	const want = "DATABASE_URL: pgx cannot read its connection settings"
	cases := []struct {
		name, url string
		markers   []string
	}{
		{"query-key", "postgres://127.0.0.1:1/dispatch?sslmode=disable&" + redacttest.Token, []string{redacttest.Token}},
		{"user", "postgres://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/dispatch?access" + "_token=" + redacttest.Token + "&sslmode=not-a-mode",
			[]string{redacttest.User, redacttest.Token}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			_, err := pgx.ParseConfig(c.url)
			if err == nil {
				t.Errorf("%s: pgx read %q; want it refused", c.name, c.url)
				continue
			}
			for _, marker := range c.markers {
				if !strings.Contains(c.url, marker) || !strings.Contains(err.Error(), marker) {
					t.Errorf("%s: the URL %q or pgx's error of it, %q, holds no %q, so the row cannot see it printed", c.name, c.url, err, marker)
				}
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := loopbackDatabase(c.url)
			if err == nil {
				t.Fatalf("loopbackDatabase took %q; want it refused as a URL pgx cannot read", c.url)
			}
			text := err.Error()
			redacttest.RequireNoPiece(t, "dispatch.database-url/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if text != want {
				t.Errorf("the refusal is %q; want %q", text, want)
			}
			var unread *pgconn.ParseConfigError
			if !errors.As(err, &unread) {
				t.Errorf("the refusal %q does not unwrap to pgx's *pgconn.ParseConfigError", text)
			}
		})
	}
}
