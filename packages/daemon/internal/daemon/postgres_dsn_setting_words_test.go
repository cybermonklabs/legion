package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// TestTheBootRefusesAPostgresDSNPgxCannotReadByTheSetting: the daemon's boot (run) refuses a
// postgres_dsn pgx cannot read at its first attempt to open the store, by the setting's name
// before the store's fixed words, "postgres_dsn: connect to Postgres at 127.0.0.1:1: pgx cannot
// read its connection settings", which still unwrap to pgx's error, and prints none of the DSN.
// pgx's ParseConfigError quotes the DSN with its password masked but its user name and every
// other query pair kept, and the error it wraps quotes a query key that has no "=" alone. Each
// case holds one marker set: the probe's token as a query key (query-key), and the probe's user
// and password as the user information beside a query that carries its token as an access
// token's value and an sslmode pgx refuses (user). The twin shows each DSN as written, and pgx's
// own error of it, holds its markers. Captures daemon.boot-dsn/<case>.
func TestTheBootRefusesAPostgresDSNPgxCannotReadByTheSetting(t *testing.T) {
	t.Parallel()
	const (
		user, password = "not-a-secret-user", "not-a-secret-password"
		want           = "postgres_dsn: connect to Postgres at 127.0.0.1:1: pgx cannot read its connection settings"
	)
	cases := []struct {
		name, dsn string
		markers   []string
	}{
		{"query-key", "postgres://127.0.0.1:1/legion?" + oauth2calltest.Token, []string{oauth2calltest.Token}},
		{"user", "postgres://" + user + ":" + password + "@127.0.0.1:1/legion?access" + "_token=" + oauth2calltest.Token + "&sslmode=not-a-mode",
			[]string{user, oauth2calltest.Token}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			_, err := pgxpool.ParseConfig(c.dsn)
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
			cfg := testConfig(t)
			cfg.PostgresDSN = c.dsn
			o := fakeRuntime(fake.NewRuntime(), &built{})
			// A DSN pgx cannot read is no connection error the next attempt can clear; the waits
			// are a millisecond so that a boot that tried it again still ends here at once.
			o.storeOpenRetry = &bootprobe.Retry{Initial: time.Millisecond, Max: time.Millisecond, Attempts: storeOpenRetry.Attempts}
			err := run(context.Background(), cfg, quietLogger(), o)
			if err == nil {
				t.Fatalf("run started on the postgres_dsn %q; want it refused as a DSN pgx cannot read", c.dsn)
			}
			text := err.Error()
			oauth2calltest.RequireNoPiece(t, "daemon.boot-dsn/"+c.name, text, user, password, oauth2calltest.Token)
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
