package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// captureRecords points the default logger at a recorder until t ends, so a test reads the
// records the broker logs as its handler receives them, attributes and all.
func captureRecords(t *testing.T) *logtest.Recorder {
	t.Helper()
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	return capture
}

// releasedRecords are the attributes of each ReleasedMessage line capture holds (loggedLines),
// each checked to be at INFO: a test reads the values the release logged rather than one
// handler's rendering of them, since the broker's own handler (logging.NewCloudHandler) writes
// JSON and no test's does.
func releasedRecords(t *testing.T, capture *logtest.Recorder) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range loggedLines(t, capture, ReleasedMessage) {
		if line.level != slog.LevelInfo {
			t.Errorf("%q was logged at %s, want INFO", ReleasedMessage, line.level)
		}
		records = append(records, line.attrs)
	}
	return records
}

// loggedText is every record capture holds, its message and each attribute's value as Go prints
// it, one record a line: what a check that no byte of a value was logged searches, whatever line
// held it.
func loggedText(capture *logtest.Recorder) string {
	var text strings.Builder
	for _, r := range capture.Records() {
		text.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&text, " %s=%v", a.Key, a.Value.Any())
			return true
		})
		text.WriteByte('\n')
	}
	return text.String()
}

// with is fields added to base, in a new map.
func with(base, fields map[string]any) map[string]any {
	merged := maps.Clone(base)
	maps.Copy(merged, fields)
	return merged
}

// asJSON is args as a grant.used row's detail decodes them.
func asJSON(args []string) []any {
	out := make([]any, len(args))
	for i, arg := range args {
		out[i] = arg
	}
	return out
}

// loggedCommandOf is the command the last grant.used row of grantID records, as the strings it
// holds.
func loggedCommandOf(t *testing.T, m *Machine, grantID string) []string {
	t.Helper()
	details := grantUsedDetails(t, m, grantID)
	if len(details) == 0 {
		t.Fatalf("grant %s has no grant.used row", grantID)
	}
	held, ok := details[len(details)-1]["command"].([]any)
	if !ok {
		t.Fatalf("the last grant.used row of %s holds no command: %v", grantID, details[len(details)-1])
	}
	command := make([]string, len(held))
	for i, arg := range held {
		command[i], _ = arg.(string)
	}
	return command
}

// TestAReleaseLogsTheGrantsReasonAndItsCommand: a session's release records, in its grant.used
// row and in one log record, the reason its request gave and the command the session said the
// release starts. The proxy's release of the same grant records the reason too, and marks itself
// as the airlock's in place of a command, since it starts none.
func TestAReleaseLogsTheGrantsReasonAndItsCommand(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, _ := newPlaceholderFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", "")}})
	retag(t, m, policytest.Secret("SECOND_TOKEN", fixtureOperator, policy.TierAgent, "second-v1"))
	const reason = "publish the docs site"
	requestID, grantID := grantOf(t, m, enr, key, reason, "AUTO_TOKEN", "SECOND_TOKEN")

	if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{Command: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatalf("Values: %v", err)
	}
	if _, _, err := m.AirlockValues(t.Context(), grantID); err != nil {
		t.Fatalf("AirlockValues: %v", err)
	}

	want := []map[string]any{
		{"names": []any{"AUTO_TOKEN", "SECOND_TOKEN"}, "reason": reason, "command": []any{"sh", "-c", "true"}},
		{"names": []any{"AUTO_TOKEN"}, "airlock": true, "reason": reason},
	}
	if details := grantUsedDetails(t, m, grantID); !reflect.DeepEqual(details, want) {
		t.Errorf("grant.used details = %v; want %v: each release records its grant's reason, and the session's the command it starts", details, want)
	}
	ids := map[string]any{"grant": grantID, "request": requestID, "enrollment": enr}
	wantRecords := []map[string]any{
		with(ids, map[string]any{"names": []string{"AUTO_TOKEN", "SECOND_TOKEN"}, "reason": reason, "command": []string{"sh", "-c", "true"}}),
		with(ids, map[string]any{"names": []string{"AUTO_TOKEN"}, "reason": reason, "airlock": true}),
	}
	if records := releasedRecords(t, capture); !reflect.DeepEqual(records, wantRecords) {
		t.Errorf("the release records are\n%v\nwant\n%v", records, wantRecords)
	}
}

// TestAReleaseLogsTheReasonItsOwnCallGave: a session that asks again for the names it holds a
// live grant of gets that grant back (reuseLiveGrant), and the reason its own call gave goes into
// no request row. Its release records that reason as call_reason, beside the reason of the request
// the grant came of, in the row and the record, when the two differ, and records nothing more when
// they are the same or the session sent none, as a client from before the field does. The broker
// holds the call's reason to a request's own rules (record.Sanitize), dropping the runes no reason
// may carry and cutting it to record.MaxReasonRunes, since nothing refused it before the release.
func TestAReleaseLogsTheReasonItsOwnCallGave(t *testing.T) {
	capture := captureRecords(t)
	m, enr, key, _ := newFixture(t)
	const reason, again = "publish the docs site", "rebuild the search index"
	requestID, grantID := grantOf(t, m, enr, key, reason, "AUTO_TOKEN")
	if _, reused := grantOf(t, m, enr, key, again, "AUTO_TOKEN"); reused != grantID {
		t.Fatalf("the second ask got grant %s, want the live grant %s back", reused, grantID)
	}
	ids := map[string]any{"grant": grantID, "request": requestID, "enrollment": enr}
	for _, row := range []struct {
		name       string
		call       ReleaseCall
		callReason string
		command    []string
	}{
		{"its call's own reason", ReleaseCall{Reason: again, Command: []string{"make", "index"}}, again, []string{"make", "index"}},
		{"the grant's own reason", ReleaseCall{Reason: reason, Command: []string{"make"}}, "", []string{"make"}},
		{"no reason of its own", ReleaseCall{}, "", []string{}},
		{"a reason no request could sign", ReleaseCall{Reason: "fix\u202eit" + strings.Repeat("é", 500)}, "fixit" + strings.Repeat("é", 395), []string{}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := m.Values(t.Context(), grantID, enr, row.call); err != nil {
				t.Fatalf("Values: %v", err)
			}
			wantDetail := map[string]any{"names": []any{"AUTO_TOKEN"}, "reason": reason, "command": asJSON(row.command)}
			wantRecord := with(ids, map[string]any{"names": []string{"AUTO_TOKEN"}, "reason": reason, "command": row.command})
			if row.callReason != "" {
				wantDetail["call_reason"], wantRecord["call_reason"] = row.callReason, row.callReason
			}
			details := grantUsedDetails(t, m, grantID)
			if got := details[len(details)-1]; !reflect.DeepEqual(got, wantDetail) {
				t.Errorf("the last grant.used detail = %.200v; want %.200v", got, wantDetail)
			}
			records := releasedRecords(t, capture)
			if len(records) == 0 || !reflect.DeepEqual(records[len(records)-1], wantRecord) {
				t.Errorf("the last release record is %.300v; want %.300v", records, wantRecord)
			}
		})
	}
}

// TestAReleaseCommandHoldingAValueIsRedacted: a command that holds a value the release handed out
// is recorded with "[redacted]" in its place, in the row and in the record, and no log line holds
// one byte of it that could say what it was: a stored value, a token inside the document a
// github-app minter answered, and a placeholder the airlock handed out for a name it holds back.
// The value is redacted before the argument is clipped to its 256 bytes, so a value that crosses
// the cut leaves no prefix of itself behind. (The values below are fixtures, not credentials.)
func TestAReleaseCommandHoldingAValueIsRedacted(t *testing.T) {
	// Each value opens with eight bytes no other line of the log or the rows holds, a test's
	// name included, since what the rows and the log must not hold is those eight bytes.
	const storedValue = "v7c1e9a2-stored-0123456789abcdef"
	const mintedToken = "ghs_m4t9k0123456789abcdefghij"

	// hidden fails t when the grant's rows or the log hold the first eight bytes of probe: the
	// value, or the part of it that is the secret.
	hidden := func(t *testing.T, m *Machine, grantID, logged, probe string) {
		t.Helper()
		rows, err := json.Marshal(grantUsedDetails(t, m, grantID))
		if err != nil {
			t.Fatal(err)
		}
		for _, held := range []struct{ where, text string }{{"the grant.used rows", string(rows)}, {"the log", logged}} {
			if strings.Contains(held.text, probe[:8]) {
				t.Errorf("%s hold the first eight bytes of the released value (%s)", held.where, valueDigest(probe))
			}
		}
	}

	t.Run("a-stored-value", func(t *testing.T) {
		capture := captureRecords(t)
		m, enr, key, _ := newFixture(t)
		retag(t, m, policytest.Secret("STORED_KEY", fixtureOperator, policy.TierAgent, storedValue))
		_, grantID := grantOf(t, m, enr, key, "call the API", "STORED_KEY")
		command := []string{"curl", "-H", "Authorization: Bearer " + storedValue, "https://api.example.com/v1"}
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{Command: command}); err != nil {
			t.Fatalf("Values: %v", err)
		}
		want := []string{"curl", "-H", "Authorization: Bearer [redacted]", "https://api.example.com/v1"}
		if got := loggedCommandOf(t, m, grantID); !reflect.DeepEqual(got, want) {
			t.Errorf("the row's command = %q, want %q", got, want)
		}
		if records := releasedRecords(t, capture); len(records) != 1 || !reflect.DeepEqual(records[0]["command"], want) {
			t.Errorf("the release records' commands = %v, want one record naming %q", records, want)
		}
		hidden(t, m, grantID, loggedText(capture), storedValue)
	})

	t.Run("a-token-in-a-minted-document", func(t *testing.T) {
		capture := captureRecords(t)
		m, enr, key, _ := newAppFixture(t, "APP_KEY")
		retag(t, m, requestTypeSecret("APP_KEY", policy.TypeGitHubApp, policy.TierAgent, ""))
		m.Minters = map[string]Minter{
			policy.TypeGitHubApp: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
				return secrets.Minted{Value: `{"installations":{"ada":{"installation_id":7,"token":"` + mintedToken + `","permissions":{"contents":"write"}}}}`}, nil
			},
		}
		_, grantID := grantOf(t, m, enr, key, "open a pull request", "APP_KEY")
		command := []string{"gh", "api", "-H", "Authorization: token " + mintedToken, "-f", "permission=write", "repos/ada/widgets"}
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{Command: command}); err != nil {
			t.Fatalf("Values: %v", err)
		}
		// The token is the document's secret; the document's other strings, such as the
		// permission it grants, are no value of it and are left as written.
		want := []string{"gh", "api", "-H", "Authorization: token [redacted]", "-f", "permission=write", "repos/ada/widgets"}
		if got := loggedCommandOf(t, m, grantID); !reflect.DeepEqual(got, want) {
			t.Errorf("the row's command = %q, want %q", got, want)
		}
		hidden(t, m, grantID, loggedText(capture), mintedToken)
	})

	t.Run("a-placeholder-the-airlock-handed-out", func(t *testing.T) {
		capture := captureRecords(t)
		m, enr, key, _ := newPlaceholderFixture(t, proxyrules.Rules{Secrets: []proxyrules.SecretRule{rule("AUTO_TOKEN", "")}})
		_, grantID := grantOf(t, m, enr, key, "call the API", "AUTO_TOKEN")
		first, err := m.Values(t.Context(), grantID, enr, ReleaseCall{})
		if err != nil || first.Airlock == nil {
			t.Fatalf("Values = %+v, %v; want the airlock's placeholder", first, err)
		}
		placeholder := first.Airlock.Names["AUTO_TOKEN"].Value
		// A second release of the grant hands out the same placeholder, which a command written
		// after the first can hold.
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{Command: []string{"curl", "-u", "x:" + placeholder}}); err != nil {
			t.Fatalf("Values: %v", err)
		}
		if got, want := loggedCommandOf(t, m, grantID), []string{"curl", "-u", "x:[redacted]"}; !reflect.DeepEqual(got, want) {
			t.Errorf("the row's command = %q, want %q", got, want)
		}
		hidden(t, m, grantID, loggedText(capture), strings.TrimPrefix(placeholder, PlaceholderPrefix))
	})

	t.Run("a-value-across-the-cut", func(t *testing.T) {
		capture := captureRecords(t)
		m, enr, key, _ := newFixture(t)
		retag(t, m, policytest.Secret("STORED_KEY", fixtureOperator, policy.TierAgent, storedValue))
		_, grantID := grantOf(t, m, enr, key, "call the API", "STORED_KEY")
		// The value starts eight bytes before the 256th: clipping first would keep those eight.
		argument := strings.Repeat("a", 248) + storedValue
		if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{Command: []string{"echo", argument}}); err != nil {
			t.Fatalf("Values: %v", err)
		}
		got := loggedCommandOf(t, m, grantID)
		if want := strings.Repeat("a", 248) + "[redacte"; len(got) != 2 || got[1] != want {
			t.Errorf("the row's command = %q, want echo and %q", got, want)
		}
		hidden(t, m, grantID, loggedText(capture), storedValue)
	})
}

// TestAReleaseCommandIsClipped: the command a release records is its first 32 arguments, each
// cut to its first 256 bytes on a character's boundary, so a session cannot make one release
// write a line of any length; and an argument holding a NUL byte, which no argument vector a
// process runs can hold and a database's JSON refuses, is recorded with U+FFFD in its place
// rather than failing the release.
func TestAReleaseCommandIsClipped(t *testing.T) {
	for _, row := range []struct {
		name    string
		command []string
		want    []string
	}{
		{
			name:    "40 arguments of 1 KiB",
			command: repeated(40, strings.Repeat("a", 1024)),
			want:    repeated(32, strings.Repeat("a", 256)),
		},
		{
			name:    "a character across the 256th byte",
			command: []string{"echo", strings.Repeat("a", 255) + "é"},
			want:    []string{"echo", strings.Repeat("a", 255)},
		},
		{
			name:    "a NUL byte",
			command: []string{"printf", "a\x00b"},
			want:    []string{"printf", "a\uFFFDb"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, enr, key, _ := newFixture(t)
			_, grantID := grantOf(t, m, enr, key, "read it", "AUTO_TOKEN")
			if _, err := m.Values(t.Context(), grantID, enr, ReleaseCall{Command: row.command}); err != nil {
				t.Fatalf("Values: %v", err)
			}
			got := loggedCommandOf(t, m, grantID)
			if !reflect.DeepEqual(got, row.want) {
				t.Errorf("the row's command holds %d arguments, want %d: %.80q", len(got), len(row.want), got)
			}
			for i, arg := range got {
				if len(arg) > 256 || !utf8.ValidString(arg) {
					t.Errorf("argument %d is %d bytes, valid UTF-8 %t; want at most 256 bytes of valid UTF-8", i, len(arg), utf8.ValidString(arg))
				}
			}
		})
	}
}

// repeated is n copies of arg.
func repeated(n int, arg string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = arg
	}
	return out
}
