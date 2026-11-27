package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// databaseURLSettingWords is how `census --store`, which opens its one connection through
// store.Connect, refuses a DATABASE_URL pgx cannot read (databaseURLRefusal): the setting's name
// before the store's fixed words.
const databaseURLSettingWords = "DATABASE_URL: parse Postgres URL: pgx cannot read its connection settings"

// unreadDatabaseURLs are two DATABASE_URLs pgx refuses, each beside the markers its own error
// quotes: the probe's token as a query key with no "=" (query-key), and the probe's user and
// password as the user information beside a query that carries its token as an access token's
// value and an sslmode pgx refuses (user). pgx masks the password and keeps the rest. refused is
// how pgdsn refuses each, by the setting: Refusal refuses the query key before pgx reads the URL,
// and ConnectError names the URL pgx cannot parse. serverLine is the server's line that prints
// that refusal: resolveBootConfig's, which reads the URL through pgdsn.Refusal at boot, or the
// open's.
var unreadDatabaseURLs = []struct {
	name, url, refused, serverLine string
	markers                        []string
}{
	{"query-key", "postgres://127.0.0.1:1/dispatch?" + redacttest.Token,
		`DATABASE_URL cannot be read: a key of its query holds a character no keyword or parameter name has (only ASCII letters, digits, "_", "." and, after the first, "$"), which Postgres sends the server as a parameter's name and the server can print whole; name each setting of the query by its keyword`,
		`msg="dispatch: resolve boot config"`, []string{redacttest.Token}},
	{"user", "postgres://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/dispatch?access" + "_token=" + redacttest.Token + "&sslmode=not-a-mode",
		"connect to Postgres: DATABASE_URL cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN",
		`msg="dispatch: open database"`, []string{redacttest.User, redacttest.Token}},
}

// TestEveryDatabaseURLCommandNamesTheSettingPgxCannotRead: each command of envoy-dispatch that
// opens the database DATABASE_URL names - the migration census (store.Census) and the store
// census (`census --store`, graphcensus.StoreCensus), versions (store.CheckVersions),
// redeliver-webhooks, stored-order and the database subcommands' open (openMigrated and
// openMigratedWithoutBlockSchema, here backfill-files's) - refuses one pgx cannot read by the
// setting's name, and prints none of the URL: the store census by the setting's name before the
// store's fixed words, "DATABASE_URL: parse Postgres URL: pgx cannot read its connection settings",
// and every other command by pgdsn's words, which name the setting themselves. pgx's
// ParseConfigError quotes the URL with its password masked but its user name and every other query
// pair kept, and the error it wraps quotes a query key that has no "=" alone. The twin shows each
// URL as written, and pgx's own error of it, holds its markers. Captures
// dispatch.database-url-setting/<command>-<case>.
func TestEveryDatabaseURLCommandNamesTheSettingPgxCannotRead(t *testing.T) {
	t.Parallel()
	commands := []struct {
		name string
		want func(refused string) string
		run  func(t *testing.T, url string) string
	}{
		{"census", func(refused string) string { return "census: " + refused + "\n" }, func(t *testing.T, url string) string {
			var out, errOut bytes.Buffer
			if code := census(t.Context(), nil, url, &out, &errOut); code != 2 {
				t.Errorf("census exited %d; want 2, the census could not be taken", code)
			}
			return out.String() + errOut.String()
		}},
		{"store-census", func(string) string { return "census: " + databaseURLSettingWords + "\n" }, func(t *testing.T, url string) string {
			var out, errOut bytes.Buffer
			if code := census(t.Context(), []string{"--store"}, url, &out, &errOut); code != 2 {
				t.Errorf("census --store exited %d; want 2, the census could not be taken", code)
			}
			return out.String() + errOut.String()
		}},
		{"versions", func(refused string) string { return "versions: " + refused + "\n" }, func(t *testing.T, url string) string {
			var out, errOut bytes.Buffer
			if code := versions(t.Context(), url, &out, &errOut); code != 2 {
				t.Errorf("versions exited %d; want 2, the check could not be taken", code)
			}
			return out.String() + errOut.String()
		}},
		{"redeliver-webhooks", func(refused string) string { return "redeliver-webhooks: open database: " + refused + "\n" }, func(t *testing.T, url string) string {
			var out bytes.Buffer
			if code := redeliverWebhooks(t.Context(), []string{"--since", "72h"}, envGetter(map[string]string{"DATABASE_URL": url}), &out); code != 1 {
				t.Errorf("redeliver-webhooks exited %d; want 1", code)
			}
			return out.String()
		}},
		{"stored-order", func(refused string) string { return "stored-order: open database: " + refused + "\n" }, func(t *testing.T, url string) string {
			var out bytes.Buffer
			if code := storedOrder(t.Context(), nil, url, &out); code != 1 {
				t.Errorf("stored-order exited %d; want 1", code)
			}
			return out.String()
		}},
		{"open-database", func(refused string) string { return "backfill-files: open database: " + refused + "\n" }, func(t *testing.T, url string) string {
			var out bytes.Buffer
			if database, ok := openMigratedWithoutBlockSchema(t.Context(), "backfill-files", url, &out); ok {
				database.Pool.Close()
				t.Errorf("the database subcommands' open took %q; want it refused", url)
			}
			return out.String()
		}},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range unreadDatabaseURLs {
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
	for _, command := range commands {
		for _, c := range unreadDatabaseURLs {
			t.Run(command.name+"-"+c.name, func(t *testing.T) {
				t.Parallel()
				text := command.run(t, c.url)
				redacttest.RequireNoPiece(t, "dispatch.database-url-setting/"+command.name+"-"+c.name, text, redacttest.User, redacttest.Password, redacttest.Token)
				if want := command.want(c.refused); text != want {
					t.Errorf("%s printed %q; want %q", command.name, text, want)
				}
			})
		}
	}
}

// TestTheServerNamesTheDatabaseURLPgxCannotRead: the server's boot (main) refuses a DATABASE_URL
// pgx cannot read by pgdsn's words, which name the setting, and prints none of the URL: its query
// key in the "dispatch: resolve boot config" line (resolveBootConfig's pgdsn.Refusal), and a URL
// pgx cannot parse in its "dispatch: open database" line. The lines are written by main, so the
// row runs this test binary as the server (TestMain's DISPATCH_MEMORY_TEST_SERVE), with the
// settings a server boots on and no NATS, and reads its standard error up to that line. The URLs
// are TestEveryDatabaseURLCommandNamesTheSettingPgxCannotRead's, whose twin shows each as written,
// and pgx's own error of it, holds its markers. Captures dispatch.database-url-setting/serve-<case>.
func TestTheServerNamesTheDatabaseURLPgxCannotRead(t *testing.T) {
	t.Parallel()
	for _, c := range unreadDatabaseURLs {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			server := exec.CommandContext(ctx, os.Args[0])
			server.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + t.TempDir(),
				memoryTestServeEnv + "=1",
				"DATABASE_URL=" + c.url,
				"DISPATCH_AGENT_TOKEN=x",
				"DISPATCH_SIGNING_KEY=database-url-setting-signing-key",
				"DISPATCH_IDENTITY=header:X-Dispatch-User",
				"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
				"DISPATCH_NATS_DISABLED=1",
				"DISPATCH_LISTEN_HOST=127.0.0.1",
			}
			said, _ := server.CombinedOutput()
			refusal := c.serverLine
			var line string
			for _, l := range strings.Split(string(said), "\n") {
				if strings.Contains(l, refusal) {
					line = l
					break
				}
			}
			if line == "" {
				t.Fatalf("the server wrote no %s line; its output:\n%s", refusal, said)
			}
			redacttest.RequireNoPiece(t, "dispatch.database-url-setting/serve-"+c.name, line, redacttest.User, redacttest.Password, redacttest.Token)
			if want := "error=" + strconv.Quote(c.refused); !strings.Contains(line, want) {
				t.Errorf("the refusal is %q; want it to hold %q", line, want)
			}
		})
	}
}
