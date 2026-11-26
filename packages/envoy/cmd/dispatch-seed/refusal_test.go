package main

import (
	"bytes"
	"strings"
	"testing"
)

// A --database-url whose password leaves part of itself past its user part (an "@" in it) is
// refused by the flag's own name, before the store, whose refusal names the server's DATABASE_URL,
// and before anything connects; the refusal holds no part of the password. The URL is built from
// parts, so no source line holds a URL with userinfo.
func TestRunRefusesASpillingDatabaseURLByTheFlagsName(t *testing.T) {
	at := "@"
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--database-url", "postgres://seed:madeuphead" + at + "zqtail0123" + at + "db.invalid:5432/dispatch"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr %q", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "dispatch-seed: --database-url cannot be read: it holds an \"@\" after its user name and password") {
		t.Errorf("stderr = %q, want the refusal naming --database-url", stderr.String())
	}
	for _, part := range []string{"madeuphead", "zqtail0123"} {
		if strings.Contains(stderr.String(), part) {
			t.Errorf("stderr = %q, which holds %q of the password", stderr.String(), part)
		}
	}
}

// A --database-url the store's pool cannot parse is named by the flag, the one the operator set,
// and not by DATABASE_URL, which the store names, and with none of pgx's text, which quotes the
// password: one pgx itself cannot parse (a keyword DSN with spaces around password's "="), and one
// whose pool_min_conns or pool_max_conns the pool refuses, which pgx alone reads as a runtime
// parameter.
func TestRunNamesADatabaseURLPgxCannotParseByTheFlag(t *testing.T) {
	at := "@"
	for name, dsn := range map[string]string{
		"spaces around password's =": "host=db.invalid user=seed password = zqsecret0123 dbname=dispatch sslmode=requir",
		"a pool_min_conns of -x":     "host=127.0.0.1 port=1 user=seed password=zqsecret0123 pool_min_conns=-x",
		"a pool_max_conns of abc":    "postgres://seed:zqsecret0123" + at + "127.0.0.1:1/dispatch?pool_max_conns=abc",
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"--database-url", dsn}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("run = %d, want 2; stderr %q", code, stderr.String())
			}
			if want := "dispatch-seed: connect to Postgres: --database-url cannot be parsed as a Postgres DSN"; !strings.HasPrefix(stderr.String(), want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), want)
			}
			if strings.Contains(stderr.String(), "DATABASE_URL") || strings.Contains(stderr.String(), "zqsecret") {
				t.Errorf("stderr = %q, which names DATABASE_URL or holds part of the password", stderr.String())
			}
		})
	}
}

// A database URL given as an argument, not as --database-url, is refused as an unexpected argument
// without being quoted, since it holds the password, and the refusal points to --database-url.
func TestRunRefusesAnArgumentWithoutQuotingIt(t *testing.T) {
	at := "@"
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"postgres://dispatch:zqsecret0123" + at + "db.example/dispatch"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr %q", code, stderr.String())
	}
	if want := "dispatch-seed: 1 unexpected argument(s), not printed"; !strings.HasPrefix(stderr.String(), want) || !strings.Contains(stderr.String(), "--database-url") {
		t.Errorf("stderr = %q, want %q and a pointer to --database-url", stderr.String(), want)
	}
	if strings.Contains(stderr.String(), "zqsecret") || strings.Contains(stderr.String(), "db.example") {
		t.Errorf("stderr = %q, which quotes the argument", stderr.String())
	}
}

// A database URL Go's flag package refuses is named by the flag, never quoted: one given -scale as
// its value (`--scale --database-url=<dsn>`), one behind three dashes, one behind a dash alone,
// whose flag name is the URL, and one given a boolean flag as its value. Go's own output, which
// quotes the argument, is discarded.
func TestRunNamesAFlagItRefusesWithoutItsValue(t *testing.T) {
	at := "@"
	dsn := "postgres://dispatch:zqsecret0123" + at + "db.test/dispatch?sslmode=disable"
	for name, row := range map[string]struct {
		args []string
		want string
	}{
		"a DSN given -scale as its value": {[]string{"--scale", "--database-url=" + dsn}, "dispatch-seed: -scale was given a value it cannot read"},
		"three dashes":                    {[]string{"---database-url=" + dsn}, `dispatch-seed: "---database-url" has no flag's syntax`},
		"a DSN as a flag's name":          {[]string{"-" + dsn}, `dispatch-seed: an argument beginning with "-" is no flag dispatch-seed takes`},
		"a DSN given -no-migrate":         {[]string{"-no-migrate=" + dsn}, "dispatch-seed: -no-migrate was given a value it cannot read"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), row.args, &stdout, &stderr); code != 2 {
				t.Fatalf("run = %d, want 2; stderr %q", code, stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), row.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), row.want)
			}
			if strings.Contains(stderr.String(), "zqsecret") || strings.Contains(stderr.String(), "db.test") {
				t.Errorf("stderr = %q, which quotes the argument", stderr.String())
			}
		})
	}
}
