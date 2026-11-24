package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// valueDigest names a value by its length and the first sixteen hex digits of its SHA-256, which
// is how every row below reads a released value, a stored value and a minter's input: a test says
// which value it got, and says so in its failure too, without printing one byte of it. Two values
// of the same length and digest are the same value for a test's purposes.
func valueDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%d bytes, sha256 %x", len(value), sum[:8])
}

// grantAndRelease puts secret into the fixture's store, asks the fixture's session for name and
// releases the grant, answering the value the release handed over. Every secret the rows below
// build is the operator's own and agent tier, so Create grants it at once and the release needs
// no approval; one Create and one Values means the shape rule reads the stored value once, which
// is what the log rows count.
func grantAndRelease(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, secret memory.Secret, name string) string {
	t.Helper()
	retag(t, m, secret)
	request, err := m.Create(t.Context(), enrollmentID, signRequest(t, m, key, "read "+name, name), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(%s) = state %q, grant %v, %v; want an automatic grant", name, request.State, request.GrantID, err)
	}
	release, err := m.Values(t.Context(), *request.GrantID, enrollmentID, ReleaseCall{})
	if err != nil {
		t.Fatalf("Values(%s): %v", name, err)
	}
	value, released := release.Values[name]
	if !released {
		t.Fatalf("Values released %d names, none of them %s", len(release.Values), name)
	}
	return value
}

// trimLines are the lines a capture holds of TrailingLineEndingRemovedMessage, each without its
// newline, in the order the broker wrote them.
func trimLines(captured string) []string {
	var lines []string
	for line := range strings.Lines(captured) {
		if strings.Contains(line, TrailingLineEndingRemovedMessage) {
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
	}
	return lines
}

// trimLinesNaming counts those lines whose name attribute is secret.
func trimLinesNaming(captured, secret string) int {
	count := 0
	for _, line := range trimLines(captured) {
		if strings.Contains(line, "name="+secret) {
			count++
		}
	}
	return count
}

// wantTrimLog holds a capture to one line for each secret whose value the shape rule changed and
// to none for each it left alone: the line is at INFO, carries TrailingLineEndingRemovedMessage
// and the one attribute name holding the secret's name, and nothing else — no grant, no
// enrollment and no part of any value — which the exact line it is compared against pins.
func wantTrimLog(t *testing.T, captured string, changed, unchanged []string) {
	t.Helper()
	lines := trimLines(captured)
	for _, secret := range changed {
		want, got := "INFO "+TrailingLineEndingRemovedMessage+" name="+secret, 0
		for _, line := range lines {
			if line == want {
				got++
			}
		}
		if got != 1 {
			t.Errorf("the log holds %d lines %q, want exactly one; its lines of the shape rule are:\n%s",
				got, want, strings.Join(lines, "\n"))
		}
	}
	for _, secret := range unchanged {
		if got := trimLinesNaming(captured, secret); got != 0 {
			t.Errorf("the log holds %d lines of the shape rule naming %s, want none: the rule changed no value of it", got, secret)
		}
	}
	if len(lines) != len(changed) {
		t.Errorf("the log holds %d lines of the shape rule, want one for each changed value (%d):\n%s",
			len(lines), len(changed), strings.Join(lines, "\n"))
	}
}

// TestTheBrokerTrimsAStoredValuesTrailingLineEnding is what a release of a secret hands over: the
// broker reads the value for itself (policy.TrimLineEnding), so a
// one-line credential loses its whole trailing run of carriage returns and line feeds, however
// many and in whichever order, while a value holding a line ending inside it, and one that is
// nothing but line endings, are released byte for byte. The rule reaches the input a minter is
// given as well, and never the token a minter answers, and each value it changed is logged once
// for the operator, naming the secret and nothing else. The rows share one fixture and one log
// capture and none is parallel, so the log row reads exactly what the rows before it wrote.
// (The values below are fixtures, not credentials; each is read as a length and a digest all
// the same.)
func TestTheBrokerTrimsAStoredValuesTrailingLineEnding(t *testing.T) {
	const pem = "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"
	logged := policytest.CaptureLog(t)
	m, enrollmentID, key, _ := newAppFixture(t, "MINTED_INPUT_KEY")

	for _, c := range []struct{ row, secret, stored, want string }{
		{"token-and-a-line-feed", "LINE_FEED_KEY", "tok\n", "tok"},
		{"token-and-a-carriage-return-and-a-line-feed", "CARRIAGE_RETURN_LINE_FEED_KEY", "tok\r\n", "tok"},
		{"token-and-a-carriage-return", "CARRIAGE_RETURN_KEY", "tok\r", "tok"},
		{"token-and-two-line-feeds", "TWO_LINE_FEEDS_KEY", "tok\n\n", "tok"},
		{"token-with-no-line-ending", "NO_LINE_ENDING_KEY", "tok", "tok"},
		// A PEM document holds line endings inside it, so the untagged reading leaves the one
		// that ends it alone; so does a value whose last line ending is not its last byte's
		// neighbour alone.
		{"a-pem-document", "PEM_DOCUMENT_KEY", pem, pem},
		{"a-line-feed-inside-and-a-carriage-return-at-the-end", "LINE_FEED_INSIDE_KEY", "a\nb\r", "a\nb\r"},
		// A value that is only line endings keeps them: the empty string reads to a caller as a
		// credential that is not set.
		{"one-line-feed-alone", "ONE_LINE_FEED_KEY", "\n", "\n"},
		{"two-line-feeds-and-then-a-token", "LEADING_LINE_FEEDS_KEY", "\n\ntok", "\n\ntok"},
	} {
		t.Run(c.row, func(t *testing.T) {
			released := grantAndRelease(t, m, enrollmentID, key,
				policytest.Secret(c.secret, fixtureOperator, policy.TierAgent, c.stored), c.secret)
			if got, want := valueDigest(released), valueDigest(c.want); got != want {
				t.Fatalf("%s released %s, want %s", c.secret, got, want)
			}
		})
	}

	t.Run("a-minted-types-input", func(t *testing.T) {
		// The input is a value the github-app decoder reads, which the policy serves; the stored
		// value is that with a line ending, which the rule removes before the minter sees it.
		const secret, token = "MINTED_INPUT_KEY", "minted-token\n"
		input := policytest.TypeValue(policy.TypeGitHubApp)
		stored := input + "\n"
		given := ""
		m.Minters = map[string]Minter{
			policy.TypeGitHubApp: func(_ context.Context, request secrets.MintRequest) (secrets.Minted, error) {
				given = request.Value
				return secrets.Minted{Value: token}, nil
			},
		}
		released := grantAndRelease(t, m, enrollmentID, key,
			policytest.TypeSecret(secret, fixtureOperator, policy.TierAgent, policy.TypeGitHubApp, stored), secret)
		if got, want := valueDigest(given), valueDigest(input); got != want {
			t.Errorf("the minter was given %s, want the stored value with its trailing line ending removed, %s", got, want)
		}
		if got, want := valueDigest(released), valueDigest(token); got != want {
			t.Errorf("the minted token was released as %s, want the minter's own answer, %s: the rule reaches the stored value alone", got, want)
		}
	})

	t.Run("the-log-of-every-changed-value", func(t *testing.T) {
		wantTrimLog(t, logged.String(),
			[]string{"LINE_FEED_KEY", "CARRIAGE_RETURN_LINE_FEED_KEY", "CARRIAGE_RETURN_KEY", "TWO_LINE_FEEDS_KEY", "MINTED_INPUT_KEY"},
			[]string{"NO_LINE_ENDING_KEY", "PEM_DOCUMENT_KEY", "LINE_FEED_INSIDE_KEY", "ONE_LINE_FEED_KEY", "LEADING_LINE_FEEDS_KEY"})
	})
}

// TestTheBrokerReleasesTheTrimmedValue: the broker reads a stored value by the one line-ending
// rule (policy.TrimLineEnding) and by nothing else, so a secret a writer left a `shape` tag on
// is released exactly as the same secret without the tag is: `tok\n` goes out as `tok` whichever
// word the tag holds, and the log holds one line for each of them. The rows share one fixture
// and one log capture and none is parallel. (The values below are fixtures, not credentials.)
func TestTheBrokerReleasesTheTrimmedValue(t *testing.T) {
	logged := policytest.CaptureLog(t)
	m, enrollmentID, key, _ := newFixture(t)
	tagged := func(name, leftover, stored string) memory.Secret {
		s := policytest.Secret(name, fixtureOperator, policy.TierAgent, stored)
		if leftover != "" {
			s.Tags["shape"] = leftover
		}
		return s
	}

	for _, c := range []struct{ row, secret, leftover string }{
		{"no-leftover-tag", "NO_LEFTOVER_KEY", ""},
		{"a-leftover-document-tag", "DOCUMENT_SHAPE_KEY", "document"},
		{"a-leftover-token-tag", "TOKEN_SHAPE_KEY", "token"},
		{"a-leftover-tag-of-no-word-at-all", "BOGUS_SHAPE_KEY", "bogus"},
	} {
		t.Run(c.row, func(t *testing.T) {
			const stored, spent = "tok\n", "tok"
			released := grantAndRelease(t, m, enrollmentID, key, tagged(c.secret, c.leftover, stored), c.secret)
			if got, want := valueDigest(released), valueDigest(spent); got != want {
				t.Fatalf("%s released %s, want the trailing line feed gone, %s", c.secret, got, want)
			}
		})
	}

	t.Run("the-log-of-every-changed-value", func(t *testing.T) {
		wantTrimLog(t, logged.String(),
			[]string{"NO_LEFTOVER_KEY", "DOCUMENT_SHAPE_KEY", "TOKEN_SHAPE_KEY", "BOGUS_SHAPE_KEY"},
			nil)
	})
}
