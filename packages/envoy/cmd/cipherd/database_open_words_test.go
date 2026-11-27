package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerDatabaseURLPgxCannotReadIsRefusedQuotingNoneOfIt: a BROKER_DATABASE_URL that is no
// IAM URL reaches the store unparsed by config, and one pgx cannot read fails openStore, whose error
// the broker's start prints (fatal), naming the setting, in pgdsn's fixed words, and quoting none
// of the URL: no run of the probe's user, password or token. pgx's own error
// (*pgconn.ParseConfigError) quotes the URL with its user and its query, and the query key it could
// not read: a bare query key (key), which pgdsn.Refusal refuses before pgx reads it, and the user of
// a URL whose sslmode pgx refuses (user), which pgdsn.ConnectError names by the setting, each carry
// a marker the twin shows pgx's text holds. Captures cipherd.database-open/<case>.
func TestABrokerDatabaseURLPgxCannotReadIsRefusedQuotingNoneOfIt(t *testing.T) {
	const query = "access" + "_token=" + redacttest.Token
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	cases := []struct {
		name, url, says string
		pgxHolds        []string
	}{
		{"key", "postgres://agent_secrets_broker@127.0.0.1:1/agent_secrets?sslmode=disable&" + redacttest.Token,
			"BROKER_DATABASE_URL cannot be read: a key of its query holds a character no keyword or parameter name has", []string{redacttest.Token}},
		{"user", "postgres://" + userinfo + "127.0.0.1:1/agent_secrets?" + query + "&sslmode=not-a-mode",
			"connect to Postgres: BROKER_DATABASE_URL cannot be parsed as a Postgres DSN", []string{redacttest.User, redacttest.Token}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			_, err := pgxpool.ParseConfig(c.url)
			if err == nil {
				t.Errorf("%s: pgx read %q; want it refused", c.name, c.url)
				continue
			}
			if held := redacttest.Held(err.Error()); !slices.Equal(held, c.pgxHolds) {
				t.Errorf("%s: pgx's own text %q holds %q, want %q, so the row can see it printed", c.name, err, held, c.pgxHolds)
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _, err := openStore(t.Context(), config.Config{DatabaseURL: c.url}, false)
			if err == nil {
				st.Close()
				t.Fatalf("openStore(%q) opened a store; want pgx's refusal", c.url)
			}
			text := err.Error()
			redacttest.RequireNoPiece(t, "cipherd.database-open/"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
			if !strings.HasPrefix(text, "open the database BROKER_DATABASE_URL names: ") || !strings.Contains(text, c.says) {
				t.Errorf("openStore's error %q does not name BROKER_DATABASE_URL and say %q", text, c.says)
			}
		})
	}
}
