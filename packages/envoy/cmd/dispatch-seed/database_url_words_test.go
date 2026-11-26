package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheSeedNamesTheDatabaseURLFlagPgxCannotRead: dispatch-seed refuses a --database-url pgx
// cannot read by the flag's name before the store's fixed words, "dispatch-seed: open database:
// --database-url: parse Postgres URL: pgx cannot read its connection settings", and prints none of
// the URL. pgx's ParseConfigError quotes the URL with its password masked but its user name and
// every other query pair kept, and the error it wraps quotes a query key that has no "=" alone.
// Each case holds one marker set: the probe's token as a query key (query-key), and the probe's
// user and password as the user information beside a query that carries its token as an access
// token's value and an sslmode pgx refuses (user). The twin shows each URL as written, and pgx's
// own error of it, holds its markers. Captures dispatch-seed.database-url/<case>.
func TestTheSeedNamesTheDatabaseURLFlagPgxCannotRead(t *testing.T) {
	t.Parallel()
	const want = "dispatch-seed: open database: --database-url: parse Postgres URL: pgx cannot read its connection settings\n"
	cases := []struct {
		name, url string
		markers   []string
	}{
		{"query-key", "postgres://127.0.0.1:1/dispatch?" + redacttest.Token, []string{redacttest.Token}},
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
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), []string{"--database-url", c.url, "--scale", "0.01"}, &stdout, &stderr); code != 1 {
				t.Errorf("dispatch-seed exited %d; want 1", code)
			}
			text := stdout.String() + stderr.String()
			redacttest.RequireNoPiece(t, "dispatch-seed.database-url/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if text != want {
				t.Errorf("dispatch-seed printed %q; want %q", text, want)
			}
		})
	}
}
