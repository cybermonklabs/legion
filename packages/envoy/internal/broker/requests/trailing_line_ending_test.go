package requests

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
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
func grantAndRelease(t *testing.T, m *Machine, enrollmentID string, key *ecdsa.PrivateKey, secret secrets.LocalSecret, name string) string {
	t.Helper()
	retag(t, m, secret)
	request, err := m.Create(t.Context(), enrollmentID, signRequest(t, m, key, "read "+name, name), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create(%s) = state %q, grant %v, %v; want an automatic grant", name, request.State, request.GrantID, err)
	}
	release, err := m.Values(t.Context(), *request.GrantID, enrollmentID, nil)
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

// TestTheBrokerTrimsAStoredValuesTrailingLineEnding is what a release of a secret carrying no
// shape tag hands over: the broker reads the value for itself (policy.TrimForShape), so a
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
	m, enrollmentID, key, _ := newFixture(t)

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

	t.Run("a-minted-kinds-input", func(t *testing.T) {
		const secret, stored, input, token = "MINTED_INPUT_KEY", "app-key\n", "app-key", "minted-token\n"
		given := ""
		m.Minters = map[string]Minter{
			policy.KindGitHubApp: func(_ context.Context, request secrets.MintRequest) (secrets.Minted, error) {
				given = request.Value
				return secrets.Minted{Value: token}, nil
			},
		}
		released := grantAndRelease(t, m, enrollmentID, key,
			policytest.KindSecret(secret, fixtureOperator, policy.TierAgent, policy.KindGitHubApp, stored), secret)
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

// TestTheShapeTagDecidesWhatTheBrokerReleases is what the shape tag does to a release: a secret
// its owner tagged a document is released byte for byte, line ending and all, and one tagged a
// token loses its whole trailing run of carriage returns and line feeds while keeping every line
// ending inside it. The log holds one line for each value the broker changed and none for the
// document. A token that is nothing but line endings is released as stored, whatever the tag
// says, since an empty value reads to a caller as a credential that is not set, and it writes no
// log line — which is why that row stands after the log row. The rows share one fixture and one
// log capture and none is parallel. (The values below are fixtures, not credentials.)
func TestTheShapeTagDecidesWhatTheBrokerReleases(t *testing.T) {
	logged := policytest.CaptureLog(t)
	m, enrollmentID, key, _ := newFixture(t)
	shaped := func(name, shape, stored string) secrets.LocalSecret {
		s := policytest.Secret(name, fixtureOperator, policy.TierAgent, stored)
		s.Tags[policy.TagShape] = shape
		return s
	}

	t.Run("a-document-keeps-its-line-feed", func(t *testing.T) {
		const secret, stored = "DOCUMENT_SHAPE_KEY", "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"
		released := grantAndRelease(t, m, enrollmentID, key, shaped(secret, policy.ShapeDocument, stored), secret)
		if got, want := valueDigest(released), valueDigest(stored); got != want {
			t.Fatalf("%s released %s, want the stored value byte for byte, %s", secret, got, want)
		}
	})

	t.Run("a-token-of-two-lines", func(t *testing.T) {
		const secret, stored, spent = "TWO_LINE_TOKEN_KEY", "a\nb\n", "a\nb"
		released := grantAndRelease(t, m, enrollmentID, key, shaped(secret, policy.ShapeToken, stored), secret)
		if got, want := valueDigest(released), valueDigest(spent); got != want {
			t.Fatalf("%s released %s, want the trailing line feed gone and the one inside kept, %s", secret, got, want)
		}
	})

	t.Run("a-token-and-a-carriage-return-and-a-line-feed", func(t *testing.T) {
		const secret, stored, spent = "TOKEN_CRLF_KEY", "tok\r\n", "tok"
		released := grantAndRelease(t, m, enrollmentID, key, shaped(secret, policy.ShapeToken, stored), secret)
		if got, want := valueDigest(released), valueDigest(spent); got != want {
			t.Fatalf("%s released %s, want both the carriage return and the line feed gone, %s", secret, got, want)
		}
	})

	t.Run("the-log-of-every-changed-value", func(t *testing.T) {
		wantTrimLog(t, logged.String(),
			[]string{"TWO_LINE_TOKEN_KEY", "TOKEN_CRLF_KEY"},
			[]string{"DOCUMENT_SHAPE_KEY"})
	})

	t.Run("a-token-that-is-only-line-feeds", func(t *testing.T) {
		const secret, stored = "ONLY_LINE_FEEDS_KEY", "\n\n"
		released := grantAndRelease(t, m, enrollmentID, key, shaped(secret, policy.ShapeToken, stored), secret)
		if got, want := valueDigest(released), valueDigest(stored); got != want {
			t.Fatalf("%s released %s, want the stored value as it is, %s: an empty value reads as a credential that is not set", secret, got, want)
		}
		if got := trimLinesNaming(logged.String(), secret); got != 0 {
			t.Errorf("the log holds %d lines of the shape rule naming %s, want none: the broker changed nothing", got, secret)
		}
	})
}
