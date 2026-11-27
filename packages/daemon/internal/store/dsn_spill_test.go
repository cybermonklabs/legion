package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// spilledDSN is a postgres URL whose made-up password leaves part of itself after pgx's user part,
// built from parts so no source line holds a URL with userinfo and its "@". The rule itself and
// every shape it refuses are pgdsn's rows; these hold Open to it.
func spilledDSN(password string) string {
	return "postgres://legion:" + password + "@" + "db.invalid:5432/legion"
}

// Open of a DSN whose password spills, which the loader refuses before a connection is tried,
// names "the configured Postgres" and no detail of the driver's, whose text names the host and the
// database: an "@" with nothing after the host, one before a "/", one before a "?" and a "/" after
// digits.
func TestOpenOfASpilledDSNPrintsNoPartOfThePassword(t *testing.T) {
	t.Parallel()
	for name, password := range map[string]string{
		"a password holding an @":             "madeuphead0123@zqtail0123",
		"a password holding an @ and then /":  "madeuphead0123@zqtailone/zqtailtwo",
		"a password holding an @ and then ?":  "madeuphead0123@zqtailone?zqtailtwo",
		"a password holding a / after digits": "48213579/zqtail0123",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := Open(ctx, spilledDSN(password))
			if err == nil {
				t.Fatal("Open = nil, want the failed connection")
			}
			if !strings.HasPrefix(err.Error(), "connect to the configured Postgres: ") {
				t.Errorf("Open = %v, want the configured Postgres named", err)
			}
			for _, part := range []string{"madeuphead", "zqtail", "48213"} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("Open = %v, which holds %q of the password", err, part)
				}
			}
		})
	}
}

// Open of a keyword DSN pgx cannot parse names the daemon's setting alone, none of pgx's text,
// which quotes the DSN and masks its password only when written `password=<value>`; the error is
// still a *pgconn.ParseConfigError, which OpenRetryable reads as one no attempt clears.
func TestOpenOfAKeywordDSNPgxCannotParseNamesTheSettingAlone(t *testing.T) {
	t.Parallel()
	_, err := Open(context.Background(), "host=db.invalid user=legion password = zqtailsecret0123 dbname=legion sslmode=requir")
	if want := "connect to Postgres: " + DSNSetting + " cannot be parsed as a Postgres DSN; the driver's error is not printed, since it quotes the DSN"; err == nil || err.Error() != want {
		t.Fatalf("Open = %v, want %q", err, want)
	}
	var unparsed *pgconn.ParseConfigError
	if !errors.As(err, &unparsed) || OpenRetryable(err) {
		t.Errorf("Open = %v, want a *pgconn.ParseConfigError OpenRetryable does not retry", err)
	}
}
