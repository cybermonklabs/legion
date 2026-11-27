package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testport"
)

// Open, the census and the version check refuse a DATABASE_URL whose password leaves part of
// itself past its user part, by the setting alone and before anything connects, and name every
// other failure to parse or reach the database without the password. Dispatch logs what Open
// returns and its subcommands print what the other two return, and each printed pgx's text: a
// password `madeuphead@zqtail0123` as a lookup of `zqtail0123@db.invalid`, one
// `48213579/zqtail0123` inside pgx's quote of the URL, and a keyword DSN pgx cannot parse in that
// same quote. Each DSN is built from parts, so no source line holds a URL with userinfo.
func TestTheStorePrintsNoPartOfTheDatabasePassword(t *testing.T) {
	at := "@"
	const refused = "DATABASE_URL cannot be read: it holds an \"@\" after its user name and password"
	rows := map[string]struct{ dsn, want string }{
		"an @ in the password": {
			"postgres://dispatch:madeuphead" + at + "zqtail0123" + at + "db.invalid:5432/dispatch", refused,
		},
		"a / in the password": {
			"postgres://dispatch:48213579/zqtail0123" + at + "db.invalid:5432/dispatch", refused,
		},
		"a keyword DSN pgx cannot parse": {
			"host=db.invalid user=dispatch password = zqtail0123 dbname=dispatch sslmode=requir",
			"connect to Postgres: DATABASE_URL cannot be parsed as a Postgres DSN",
		},
		"a database nothing answers for": {
			"postgres://dispatch:zqtail0123" + at + testport.Refused(t) + "/dispatch",
			"connect to Postgres at 127.0.0.1:",
		},
	}
	for opener, open := range map[string]func(context.Context, string) error{
		"Open": func(ctx context.Context, dsn string) error {
			st, err := Open(ctx, dsn)
			if err == nil {
				st.Pool.Close()
			}
			return err
		},
		"census": func(ctx context.Context, dsn string) error {
			_, err := census(ctx, dsn, migrationFiles, pgmigrate.CensusOptions{})
			return err
		},
		"CheckVersions": func(ctx context.Context, dsn string) error {
			_, err := CheckVersions(ctx, dsn)
			return err
		},
	} {
		for name, row := range rows {
			t.Run(opener+"/"+name, func(t *testing.T) {
				err := open(t.Context(), row.dsn)
				if err == nil {
					t.Fatalf("%s opened %q, want a refusal", opener, name)
				}
				if !strings.Contains(err.Error(), row.want) {
					t.Errorf("%s = %v, want it to name %q", opener, err, row.want)
				}
				for _, part := range []string{"madeuphead", "zqtail0123", "48213579"} {
					if strings.Contains(err.Error(), part) {
						t.Errorf("%s = %v, which holds %q of the password", opener, err, part)
					}
				}
			})
		}
	}
}
