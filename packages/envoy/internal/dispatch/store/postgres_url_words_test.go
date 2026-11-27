package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAPostgresURLPgxCannotReadPrintsNoneOfIt: each read of a database URL this package makes -
// Census, CheckVersions, Connect, and Open's two, declaresPoolSize's first and then the pool's own
// - refuses one pgx cannot read in fixed words that quote none of it. Connect's are "parse Postgres
// URL: pgx cannot read its connection settings"; every other read goes through pgdsn, which
// refuses a query key no keyword has before pgx reads the URL (Refusal: "DATABASE_URL cannot be
// read: a key of its query holds …") and names one pgx cannot parse by the setting
// (ConnectError: "connect to Postgres: DATABASE_URL cannot be parsed as a Postgres DSN; …"). Each
// of those still unwraps to pgx's error where pgx read the URL. pgx's ParseConfigError quotes the
// URL with its password masked but its user name and every other query pair kept, and the error it
// wraps quotes a query key that has no "=" alone. Each site's two cases hold an sslmode or a pool
// key pgx refuses beside one marker: the probe's token as a query key (query-key), and the probe's
// user and password as the user information with its token as an access token's value (user). Open's
// pool parse is reached only by a URL pgx.ParseConfig reads, since declaresPoolSize reads it first,
// so its query key carries a value. The twin shows each URL as written, and pgx's own error of it,
// holds its markers. Captures store.postgres-url/<site>-<case>.
func TestAPostgresURLPgxCannotReadPrintsNoneOfIt(t *testing.T) {
	t.Parallel()
	const (
		connectWords = "parse Postgres URL: pgx cannot read its connection settings"
		keyRefused   = DSNSetting + " cannot be read: a key of its query holds a character no keyword or parameter name has (only ASCII letters, digits, \"_\", \".\" and, after the first, \"$\"), which Postgres sends the server as a parameter's name and the server can print whole; name each setting of the query by its keyword"
		unparsed     = "connect to Postgres: " + DSNSetting + " cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN"
		userinfo     = redacttest.User + ":" + redacttest.Password + "@"
		token        = "access" + "_token=" + redacttest.Token
		queryKey     = "postgres://127.0.0.1:1/dispatch?sslmode=disable&" + redacttest.Token
		user         = "postgres://" + userinfo + "127.0.0.1:1/dispatch?" + token + "&sslmode=not-a-mode"
		poolKey      = "postgres://127.0.0.1:1/dispatch?sslmode=disable&" + redacttest.Token + "=1&pool_min_conns=not-a-number"
		poolUser     = "postgres://" + userinfo + "127.0.0.1:1/dispatch?" + token + "&sslmode=disable&pool_min_conns=not-a-number"
	)
	ctx := context.Background()
	pgxRead := func(raw string) error { _, err := pgx.ParseConfig(raw); return err }
	poolRead := func(raw string) error { _, err := pgxpool.ParseConfig(raw); return err }
	census := func(raw string) error { _, err := Census(ctx, raw); return err }
	versions := func(raw string) error { _, err := CheckVersions(ctx, raw); return err }
	connect := func(raw string) error {
		conn, err := Connect(ctx, raw, "store url words")
		if conn != nil {
			conn.Close(ctx)
		}
		return err
	}
	open := func(raw string) error {
		st, err := Open(ctx, raw)
		if st != nil {
			st.Pool.Close()
		}
		return err
	}
	cases := []struct {
		name, url string
		markers   []string
		// pgxRead is the parse whose error the site printed, and a pool case's read is the one
		// declaresPoolSize makes first, which must take the URL so that Open reaches the pool's.
		read, call func(string) error
		pool       bool
		// want is the refusal's whole text, and unwraps whether it still unwraps to pgx's error:
		// pgdsn.Refusal refuses a query key before pgx reads the URL.
		want    string
		unwraps bool
	}{
		{"census-query-key", queryKey, []string{redacttest.Token}, pgxRead, census, false, keyRefused, false},
		{"census-user", user, []string{redacttest.User, redacttest.Token}, pgxRead, census, false, unparsed, true},
		{"versions-query-key", queryKey, []string{redacttest.Token}, pgxRead, versions, false, keyRefused, false},
		{"versions-user", user, []string{redacttest.User, redacttest.Token}, pgxRead, versions, false, unparsed, true},
		{"connect-query-key", queryKey, []string{redacttest.Token}, pgxRead, connect, false, connectWords, true},
		{"connect-user", user, []string{redacttest.User, redacttest.Token}, pgxRead, connect, false, connectWords, true},
		{"open-declares-query-key", queryKey, []string{redacttest.Token}, pgxRead, open, false, keyRefused, false},
		{"open-declares-user", user, []string{redacttest.User, redacttest.Token}, pgxRead, open, false, unparsed, true},
		{"open-pool-query-key", poolKey, []string{redacttest.Token}, poolRead, open, true, keyRefused, false},
		{"open-pool-user", poolUser, []string{redacttest.User, redacttest.Token}, poolRead, open, true, unparsed, true},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			err := c.read(c.url)
			if err == nil {
				t.Errorf("%s: pgx read %q; want it refused", c.name, c.url)
				continue
			}
			for _, marker := range c.markers {
				if !strings.Contains(c.url, marker) || !strings.Contains(err.Error(), marker) {
					t.Errorf("%s: the URL %q or pgx's error of it, %q, holds no %q, so the row cannot see it printed", c.name, c.url, err, marker)
				}
			}
			if c.pool {
				if err := pgxRead(c.url); err != nil {
					t.Errorf("%s: pgx.ParseConfig refused %q (%v); want it taken, so Open reaches the pool's parse", c.name, c.url, err)
				}
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.call(c.url)
			if err == nil {
				t.Fatalf("%s took %q; want it refused as a URL pgx cannot read", c.name, c.url)
			}
			text := err.Error()
			redacttest.RequireNoPiece(t, "store.postgres-url/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if text != c.want {
				t.Errorf("the refusal is %q; want %q", text, c.want)
			}
			var unread *pgconn.ParseConfigError
			if c.unwraps && !errors.As(err, &unread) {
				t.Errorf("the refusal %q does not unwrap to pgx's *pgconn.ParseConfigError", text)
			}
		})
	}
}
