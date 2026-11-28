package store_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/testport"
)

// Open refuses a BROKER_DATABASE_URL whose password leaves part of itself past its user part, by
// the setting alone and before anything connects, and names every other failure to parse or reach
// the database without the password. cipherd logs what Open returns, and Open printed pgx's text:
// a password `madeuphead@zqtail0123` as a lookup of `zqtail0123@db.invalid`, one
// `48213579/zqtail0123` inside pgx's quote of the URL, and a keyword DSN pgx cannot parse in that
// same quote. Each DSN is built from parts, so no source line holds a URL with userinfo.
func TestOpenPrintsNoPartOfTheDatabasePassword(t *testing.T) {
	at := "@"
	for name, row := range map[string]struct{ dsn, want string }{
		"an @ in the password": {
			"postgres://broker:madeuphead" + at + "zqtail0123" + at + "db.invalid:5432/cipher",
			"BROKER_DATABASE_URL cannot be read: it holds an \"@\" after its user name and password",
		},
		"a / in the password": {
			"postgres://broker:48213579/zqtail0123" + at + "db.invalid:5432/cipher",
			"BROKER_DATABASE_URL cannot be read: it holds an \"@\" after its user name and password",
		},
		"a keyword DSN pgx cannot parse": {
			"host=db.invalid user=broker password = zqtail0123 dbname=cipher sslmode=requir",
			"connect to Postgres: BROKER_DATABASE_URL cannot be parsed as a Postgres DSN",
		},
		"a database nothing answers for": {
			"postgres://broker:zqtail0123" + at + testport.Refused(t) + "/cipher",
			"connect to Postgres at 127.0.0.1:",
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(t.Context(), row.dsn)
			if err == nil {
				st.Pool.Close()
				t.Fatalf("Open opened %q, want a refusal", name)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("Open = %v, want it to name %q", err, row.want)
			}
			for _, part := range []string{"madeuphead", "zqtail0123", "48213579"} {
				if strings.Contains(err.Error(), part) {
					t.Errorf("Open = %v, which holds %q of the password", err, part)
				}
			}
		})
	}
}
