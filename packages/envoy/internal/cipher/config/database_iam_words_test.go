package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnIAMDatabaseURLPgxCannotReadIsRefusedQuotingNoneOfIt: a BROKER_DATABASE_URL that names a
// user and an RDS host, and no password, signs in by IAM token, and one pgx cannot read is refused
// naming the setting and the host, in fixed words, and quoting none of the URL. pgx's own error
// quotes a query pair with no = (parse) and the path of an sslrootcert file it cannot open (file),
// here each holding the probe's user and password, so the refusal prints neither of them nor the
// token the URL's query carries, nor a run of the user or the token; of a file it names the call
// and the system's reason alone. A run of the password is not read: the refusal's fixed words say
// why the host must be verified, "a password to the database", which holds the password's last
// eight characters. Such a URL carries no password in its user information: one with a password
// signs in by it and never reaches pgx's parse here, so the user information holds the user alone.
// The twin shows each URL as written holds every marker. Captures config.database-iam/<case>.
func TestAnIAMDatabaseURLPgxCannotReadIsRefusedQuotingNoneOfIt(t *testing.T) {
	const (
		host    = "broker.cluster-example.us-east-1.rds.amazonaws.com"
		base    = "postgres://" + redacttest.User + "@" + host + ":5432/agent_secrets?sslmode=verify-full&access" + "_token=" + redacttest.Token
		written = redacttest.User + ":" + redacttest.Password
	)
	cases := []struct{ name, url, says string }{
		{"parse", base + "&" + written, "pgx cannot read its connection settings"},
		{"file", base + "&sslrootcert=/nonexistent/" + written + "/ca.pem", "no such file or directory"},
	}
	t.Run("twin", func(t *testing.T) {
		for _, c := range cases {
			for _, marker := range redacttest.Secrets {
				if !strings.Contains(c.url, marker) {
					t.Errorf("%s: the URL as written, %q, holds no %q, so the row cannot see it printed", c.name, c.url, marker)
				}
			}
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolatePostgresDefaults(t)
			iam, err := databaseIAM(c.url)
			if err == nil {
				t.Fatalf("databaseIAM(%q) = %v, nil; want the URL refused as one pgx cannot read", c.url, iam)
			}
			text := err.Error()
			redacttest.RequireNoPiece(t, "config.database-iam/"+c.name, text, redacttest.User, redacttest.Token)
			if !strings.HasPrefix(text, "BROKER_DATABASE_URL signs in to "+host+" ") || !strings.Contains(text, "pgx cannot read") || !strings.Contains(text, c.says) {
				t.Errorf("the refusal %q does not name BROKER_DATABASE_URL, its host %s and pgx's failure to read it, %q", text, host, c.says)
			}
		})
	}
}
