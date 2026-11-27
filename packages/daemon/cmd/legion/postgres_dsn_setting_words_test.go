package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
)

// TestConfigCheckNamesThePostgresDSNPgxCannotRead: `legion config check`'s reading of postgres_dsn
// (daemon.CheckStart, which it runs on the configuration it loaded) refuses one pgx cannot read by
// the setting's name and the store's fixed words (pgdsn.ConnectError), "connect to Postgres:
// postgres_dsn (or LEGION_POSTGRES_DSN) cannot be parsed as a Postgres DSN; …", and the command
// prints that refusal as it is. pgx's ParseConfigError quotes the DSN with its password masked but
// its user name and every other query pair kept. The case holds the probe's user and password as
// the user information beside a query that carries its token as an access token's value and an
// sslmode pgx refuses (user); a query key no keyword has is refused as the configuration loads, by
// pgdsn.Refusal, whose own tests read it. The capture is CheckStart's own error, which no printer
// has read yet. The twin shows the DSN as written, and pgx's own error of it, holds its markers.
// Captures legion.config-check-dsn/<case>.
func TestConfigCheckNamesThePostgresDSNPgxCannotRead(t *testing.T) {
	legionState(t)
	const (
		user, password = "not-a-secret-user", "not-a-secret-password"
		want           = "connect to Postgres: postgres_dsn (or LEGION_POSTGRES_DSN) cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN"
	)
	cases := []struct {
		name, dsn string
		markers   []string
	}{
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
			path, _ := workflowConfig(t, 13370, "")
			replaceLine(t, path, "postgres_dsn: ", "postgres_dsn: "+c.dsn)
			cfg, err := config.LoadForValidation(path, nil)
			if err != nil {
				t.Fatalf("load the configuration: %v", err)
			}
			applyLinkedPlugins(&cfg)
			_, _, err = daemon.CheckStart(cfg, os.LookupEnv)
			if err == nil {
				t.Fatalf("CheckStart took the postgres_dsn %q; want it refused as a DSN pgx cannot read", c.dsn)
			}
			text := err.Error()
			oauth2calltest.RequireNoPiece(t, "legion.config-check-dsn/"+c.name, text, user, password, oauth2calltest.Token)
			if text != want {
				t.Errorf("CheckStart's refusal is %q; want %q", text, want)
			}
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "config", "check", "--config", path}, &out, &errb)
			if printed := "legion config check: " + want + "\n"; code != 1 || out.Len() != 0 || errb.String() != printed {
				t.Errorf("exit code = %d, stdout %q, stderr %q; want 1 and stderr %q", code, out.String(), errb.String(), printed)
			}
		})
	}
}
