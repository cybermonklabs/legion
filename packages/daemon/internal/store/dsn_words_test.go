package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestADSNPgxCannotReadPrintsNoneOfIt: a DSN pgx cannot read is refused as its address and fixed
// words, "connect to Postgres at 127.0.0.1:1: pgx cannot read its connection settings", which still
// unwrap to pgx's error, by both roads a refusal of one takes to ConnectError: Open's, through
// PoolConfig, which the daemon's boot and `legion config check` (daemon.CheckStart) read
// postgres_dsn by (open), and ConnectError of pgx.Connect's own error, as Oh My Pi's session
// database is opened (ompsessions.ConnectURL; connect-error). pgx's ParseConfigError quotes the DSN
// with its password masked but its user name and every other query pair kept, and the error it
// wraps quotes a query key that has no "=" alone. Each case holds an sslmode pgx refuses beside one
// marker: the probe's token as a query key (query-key), and the probe's user and password as the
// user information with its token as an access token's value (user). The twin shows each DSN as
// written, and pgx's own error of it, holds its markers. Captures store.dsn/<road>-<case>.
func TestADSNPgxCannotReadPrintsNoneOfIt(t *testing.T) {
	t.Parallel()
	const (
		user, password = "not-a-secret-user", "not-a-secret-password"
		want           = "connect to Postgres at 127.0.0.1:1: pgx cannot read its connection settings"
		queryKey       = "postgres://127.0.0.1:1/legion?sslmode=disable&" + oauth2calltest.Token
		userDSN        = "postgres://" + user + ":" + password + "@127.0.0.1:1/legion?access" + "_token=" + oauth2calltest.Token + "&sslmode=not-a-mode"
	)
	ctx := context.Background()
	open := func(dsn string) error {
		st, err := Open(ctx, dsn)
		if st != nil {
			st.Close()
		}
		return err
	}
	connectURL := func(dsn string) error {
		conn, err := pgx.Connect(ctx, dsn)
		if conn != nil {
			conn.Close(ctx)
			return nil
		}
		return ConnectError(dsn, err)
	}
	poolRead := func(dsn string) error { _, err := pgxpool.ParseConfig(dsn); return err }
	connRead := func(dsn string) error { _, err := pgx.ParseConfig(dsn); return err }
	cases := []struct {
		name, dsn  string
		markers    []string
		read, call func(string) error
	}{
		{"open-query-key", queryKey, []string{oauth2calltest.Token}, poolRead, open},
		{"open-user", userDSN, []string{user, oauth2calltest.Token}, poolRead, open},
		{"connect-error-query-key", queryKey, []string{oauth2calltest.Token}, connRead, connectURL},
		{"connect-error-user", userDSN, []string{user, oauth2calltest.Token}, connRead, connectURL},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			err := c.read(c.dsn)
			if err == nil {
				t.Errorf("%s: pgx read %q; want it refused", c.name, c.dsn)
				continue
			}
			for _, marker := range c.markers {
				if !strings.Contains(c.dsn, marker) || !strings.Contains(err.Error(), marker) {
					t.Errorf("%s: the DSN %q or pgx's error of it, %q, holds no %q, so the row cannot see it printed", c.name, c.dsn, err, marker)
				}
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.call(c.dsn)
			if err == nil {
				t.Fatalf("%s took %q; want it refused as a DSN pgx cannot read", c.name, c.dsn)
			}
			text := err.Error()
			oauth2calltest.RequireNoPiece(t, "store.dsn/"+c.name, text, user, password, oauth2calltest.Token)
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
