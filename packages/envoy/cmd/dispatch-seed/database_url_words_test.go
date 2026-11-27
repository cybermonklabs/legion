package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheSeedNamesTheDatabaseURLFlagPgxCannotRead: dispatch-seed refuses a --database-url pgx
// cannot read by the flag's name, through pgdsn before the store reads it, exits 2 and prints none
// of the URL: a query key no keyword has by pgdsn.Refusal ("dispatch-seed: --database-url cannot
// be read: a key of its query holds …"), and a URL pgx cannot parse by pgdsn.ConnectError
// ("dispatch-seed: connect to Postgres: --database-url cannot be parsed as a Postgres DSN; …").
// pgx's ParseConfigError quotes the URL with its password masked but its user name and every other
// query pair kept, and the error it wraps quotes a query key that has no "=" alone. Each case holds
// one marker set: the probe's token as a query key (query-key), and the probe's user and password
// as the user information beside a query that carries its token as an access token's value and an
// sslmode pgx refuses (user). The twin shows each URL as written, and pgx's own error of it, holds
// its markers. Captures dispatch-seed.database-url/<case>.
func TestTheSeedNamesTheDatabaseURLFlagPgxCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, url, want string
		markers         []string
	}{
		{"query-key", "postgres://127.0.0.1:1/dispatch?" + redacttest.Token,
			`dispatch-seed: --database-url cannot be read: a key of its query holds a character no keyword or parameter name has (only ASCII letters, digits, "_", "." and, after the first, "$"), which Postgres sends the server as a parameter's name and the server can print whole; name each setting of the query by its keyword` + "\n",
			[]string{redacttest.Token}},
		{"user", "postgres://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/dispatch?access" + "_token=" + redacttest.Token + "&sslmode=not-a-mode",
			"dispatch-seed: connect to Postgres: --database-url cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN\n",
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
			if code := run(t.Context(), []string{"--database-url", c.url, "--scale", "0.01"}, &stdout, &stderr); code != 2 {
				t.Errorf("dispatch-seed exited %d; want 2", code)
			}
			text := stdout.String() + stderr.String()
			redacttest.RequireNoPiece(t, "dispatch-seed.database-url/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if text != c.want {
				t.Errorf("dispatch-seed printed %q; want %q", text, c.want)
			}
		})
	}
}
