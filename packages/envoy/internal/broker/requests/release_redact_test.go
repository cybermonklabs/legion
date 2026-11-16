package requests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// shaped joins parts into one fixture credential. Each fixture is written in pieces so no line of
// this file holds a whole credential shape for a scanner to report, and each holds no real
// credential: its random-looking part spells NotASecret.
func shaped(parts ...string) string { return strings.Join(parts, "") }

// TestACommandHoldingAKnownCredentialShapeIsRedacted: a release redacts a credential of a
// well-known shape wherever it came from — a token an earlier release minted, a key the session
// read from somewhere else — since the values this release handed out are not the only ones a
// command can hold: GitHub tokens, AWS access key ids, Google OAuth access tokens, Slack tokens of
// every xox prefix and a PEM private key. A shape the client's cut ran through at the argument's
// end is redacted too. Text that only begins like one of them, away from the cut, is left as
// written.
func TestACommandHoldingAKnownCredentialShapeIsRedacted(t *testing.T) {
	classic := shaped("gh", "p_", strings.Repeat("NotASecret", 4)[:36])
	installation := shaped("gh", "s_", strings.Repeat("NotASecret", 4)[:36])
	fineGrained := shaped("github", "_pat_", "11NOTASECRET0000000000", "_", strings.Repeat("NotASecret", 6)[:59])
	accessKey := shaped("AK", "IA", "NOTASECRET000000")
	sessionKey := shaped("AS", "IA", "NOTASECRET111111")
	google := shaped("ya", "29.", "NotASecret-token_0")
	slack := shaped("xo", "xb-", "0000-NotASecret")
	slackRefresh := shaped("xo", "xe-", "1-NotASecret0000")
	slackClient := shaped("xo", "xc-", "NotASecret-0000")
	slackCookie := shaped("xo", "xd-", "NotASecret0000")
	// A Slack d cookie's body is base64, which a -b d=… argument usually carries URL-encoded.
	cookieEncoded := shaped("xo", "xd-", "NotASecret%2BNotA%2FSecret%3D%3D")
	cookieDecoded := shaped("xo", "xd-", "NotASecret+NotA/Secret==")
	pem := shaped("-----BEGIN ", "RSA PRIVATE KEY-----\n", "NotASecret\n", "-----END RSA PRIVATE KEY-----")
	pemCut := shaped("-----BEGIN ", "PRIVATE KEY-----\n", "NotASecret")
	for _, row := range []struct {
		name          string
		command, want []string
	}{
		{"a classic GitHub token", []string{"curl", "-H", "Authorization: token " + classic}, []string{"curl", "-H", "Authorization: token [redacted]"}},
		{"a GitHub App installation token", []string{"git", "-c", "http.extraheader=Authorization: Bearer " + installation}, []string{"git", "-c", "http.extraheader=Authorization: Bearer [redacted]"}},
		{"a fine-grained GitHub token", []string{"gh", "--token=" + fineGrained}, []string{"gh", "--token=[redacted]"}},
		{"AWS access key ids", []string{"env", "AWS_ACCESS_KEY_ID=" + accessKey, "OTHER=" + sessionKey}, []string{"env", "AWS_ACCESS_KEY_ID=[redacted]", "OTHER=[redacted]"}},
		{"a Google access token", []string{"curl", "-H", "Authorization: Bearer " + google}, []string{"curl", "-H", "Authorization: Bearer [redacted]"}},
		{"a Slack bot token", []string{"slack", "--token", slack}, []string{"slack", "--token", "[redacted]"}},
		{"a Slack refresh token", []string{"slack", "--refresh", slackRefresh}, []string{"slack", "--refresh", "[redacted]"}},
		{"a Slack client token and cookie", []string{"curl", "-H", "Authorization: Bearer " + slackClient, "-b", "d=" + slackCookie},
			[]string{"curl", "-H", "Authorization: Bearer [redacted]", "-b", "d=[redacted]"}},
		{"an encoded Slack cookie", []string{"curl", "-b", "d=" + cookieEncoded}, []string{"curl", "-b", "d=[redacted]"}},
		{"a decoded Slack cookie", []string{"curl", "-b", "d=" + cookieDecoded + "; x=1"}, []string{"curl", "-b", "d=[redacted]; x=1"}},
		{"an encoded Slack cookie the cut ran through", []string{"echo", strings.Repeat("a", 240) + cookieEncoded[:16]}, []string{"echo", strings.Repeat("a", 240) + "[redacted]"}},
		{"a decoded Slack cookie the cut ran through", []string{"echo", strings.Repeat("a", 240) + cookieDecoded[:17]}, []string{"echo", strings.Repeat("a", 240) + "[redacted]"}},
		{"a PEM private key", []string{"sh", "-c", "key=" + pem + " tail"}, []string{"sh", "-c", "key=[redacted] tail"}},
		{"a PEM private key the cut ran through", []string{"printf", pemCut}, []string{"printf", "[redacted]"}},
		{"a GitHub token the cut ran through", []string{"echo", strings.Repeat("a", 250) + classic[:6]}, []string{"echo", strings.Repeat("a", 250) + "[redac"}},
		{"text that only begins like a credential", []string{"echo", "ghp_short", "AKIA", "xoxb", "ya29"}, []string{"echo", "ghp_short", "AKIA", "xoxb", "ya29"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := loggedCommand(row.command, nil); !reflect.DeepEqual(got, row.want) {
				t.Errorf("logged %d arguments; argument by argument, want %q, got lengths %v", len(got), row.want, argLengths(got))
				for i := range min(len(got), len(row.want)) {
					if got[i] != row.want[i] && !strings.Contains(row.want[i], "[redac") {
						t.Errorf("argument %d is %q, want %q", i, got[i], row.want[i])
					}
				}
			}
		})
	}
}

// argLengths names each argument by its length alone, so a failure prints no part of a fixture
// credential that was left in it.
func argLengths(args []string) []int {
	lengths := make([]int, len(args))
	for i, arg := range args {
		lengths[i] = len(arg)
	}
	return lengths
}

// TestAHandedOutValueIsRedactedOnceAndOnlyWhenLongEnough: two handed-out values that overlap in
// one argument are redacted as one run, leaving no tail of either; a value shorter than eight
// bytes is not redacted, since marking every place its few bytes occur would garble the command
// and show the value by where the marks land; and a value the client's cut ran through, which
// reaches the broker as the start of the value at the end of an argument cut to 256 bytes, leaves
// no part of that start behind.
func TestAHandedOutValueIsRedactedOnceAndOnlyWhenLongEnough(t *testing.T) {
	const value = "v7c1e9a2-stored-0123456789abcdef"
	for _, row := range []struct {
		name                  string
		values, command, want []string
	}{
		{"two values that overlap", []string{"abcdefghij", "efghijklmn"}, []string{"abcdefghijklmn!"}, []string{"[redacted]!"}},
		{"a value shorter than eight bytes", []string{"e"}, []string{"echo", "hello"}, []string{"echo", "hello"}},
		{"a value the client's cut ran through", []string{value}, []string{"echo", strings.Repeat("a", 248) + value[:8]}, []string{"echo", strings.Repeat("a", 248) + "[redacte"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := loggedCommand(row.command, row.values); !reflect.DeepEqual(got, row.want) {
				t.Errorf("logged %q, want %q", got, row.want)
			}
		})
	}
}

// TestALargeCommandCostsTheRedactionNoMoreThanItRecords: a session writes its own body, up to the
// broker's 1 MiB cap, and the redaction runs on every release of it. The redaction is handed each
// argument already cut to the 256 bytes a release records, never the whole argument, so a command
// of 40 arguments of 32 KiB costs it no more than one of 32 arguments of 256 bytes; the rule at
// the cut catches a value that crosses byte 256. The bytes the redaction is handed are counted,
// which no machine's speed changes.
func TestALargeCommandCostsTheRedactionNoMoreThanItRecords(t *testing.T) {
	const value = "v7c1e9a2-stored-0123456789abcdef"
	command := repeated(40, strings.Repeat("a", 32<<10))
	command[1] = strings.Repeat("a", 250) + value + strings.Repeat("b", 32<<10)
	scanned := 0
	got := redactCommand(command, []string{value}, func(arg string, values []string) string {
		scanned += len(arg)
		return redactArgument(arg, values)
	})
	if limit := record.MaxCommandArguments * record.MaxCommandArgumentBytes; scanned > limit {
		t.Errorf("the redaction was handed %d bytes of a %d-byte command, want at most %d: what a release records", scanned, 40*(32<<10), limit)
	}
	if len(got) != record.MaxCommandArguments || got[0] != strings.Repeat("a", 256) || got[1] != strings.Repeat("a", 250)+"[redac" {
		t.Errorf("logged %d arguments, the first two %d and %d bytes; want 32, 256 a's, then 250 a's and the marker's start", len(got), len(got[0]), len(got[1]))
	}
}

// TestAStoredJSONValueIsRedactedLeafByLeaf: a stored value that is a JSON document — a database
// login, an AWS key pair — is redacted string by string, every string of eight or more bytes at
// any depth, since a command takes the fields apart and never holds the whole document. Shorter
// strings, such as a user name or a host, are left as written. (The values are fixtures.)
func TestAStoredJSONValueIsRedactedLeafByLeaf(t *testing.T) {
	const password = "not-a-secret-pw-0001"
	const secretKey = "not-a-secret/aws+secret/key/0000000000"
	accessKey := shaped("AK", "IA", "NOTASECRET000000")
	m, enr, key, _ := newFixture(t)
	// The field names are written in pieces too, as shaped's are, so no line holds a credential
	// field next to its value for a scanner to report.
	passwordField, secretKeyField := shaped("pass", "word"), shaped("secret_", "access_key")
	retag(t, m,
		policytest.Secret("DB_LOGIN", fixtureOperator, policy.TierAgent,
			`{"username":"app","auth":{"`+passwordField+`":"`+password+`"},"host":"db"}`),
		policytest.Secret("AWS_PAIR", fixtureOperator, policy.TierAgent,
			`{"access_key_id":"`+accessKey+`","`+secretKeyField+`":"`+secretKey+`"}`),
	)
	_, grantID := grantOf(t, m, enr, key, "migrate the schema", "AWS_PAIR", "DB_LOGIN")
	command := []string{"psql", "postgresql://app:" + password + "@db/app", "AWS_SECRET_ACCESS_KEY=" + secretKey}
	if _, err := m.Values(t.Context(), grantID, enr, nil, ReleaseCall{Command: command}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	want := []string{"psql", "postgresql://app:[redacted]@db/app", "AWS_SECRET_ACCESS_KEY=[redacted]"}
	if got := loggedCommandOf(t, m, grantID); !reflect.DeepEqual(got, want) {
		t.Errorf("the row's command has argument lengths %v, want %q", argLengths(got), want)
	}
}
