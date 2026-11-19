package config

import (
	"strings"
	"testing"
)

// The RDS IAM check reads BROKER_DATABASE_URL with net/url, which ends the host at a ?, # or \ in
// the user part that pgx keeps in it: a value whose user part holds an RDS host's name before such
// a character, with a user in the query net/url reads, was named by that piece of its user part in
// the check's refusal. The check now decides nothing of such a value, so it names none of it. The
// control is the honest form of an RDS URL with a second host, which the check still refuses
// naming its RDS host. The token is made for this test.
func TestTheIAMCheckNamesNoPartOfAUserPartNetURLReadsApart(t *testing.T) {
	const token = "not-a-secret-tok5f2e"
	for _, raw := range []string{
		"postgres://" + rdsHost + "?user=" + token + "@other.example.net:5432/agent_secrets",
		"postgres://" + rdsHost + "#user=" + token + "@other.example.net:5432/agent_secrets",
	} {
		iam, err := databaseIAM(raw)
		if err != nil || iam {
			t.Errorf("%d characters: databaseIAM = %v, %v, want no IAM sign-in and no refusal", len(raw), iam, err)
		}
		if err != nil && (strings.Contains(err.Error(), rdsHost) || strings.Contains(err.Error(), token)) {
			t.Errorf("%d characters: the refusal names a part of the user part: %q", len(raw), err)
		}
	}
	_, err := databaseIAM("postgres://agent_secrets_broker@" + rdsHost + ":5432,other.example.net:5432/agent_secrets?sslmode=verify-full&sslrootcert=" + rdsBundle)
	if err == nil || !strings.Contains(err.Error(), rdsHost) || !strings.Contains(err.Error(), "it names another host too") {
		t.Fatalf("UNKNOWN: the control is not refused naming its RDS host, so this row saw nothing: %v", err)
	}
}
