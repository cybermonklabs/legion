package leakguard

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// A cause sentence is one line beginning RefusalPrefix, with the error underneath after a colon,
// and the launcher's own sentence, which already begins with the prefix, is carried as it is.
func TestACauseIsOneLineUnderThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		what string
		err  error
		want string
	}{
		{"what happened alone", "the daemon runs with no GitHub App, so it cannot read " + CheckSlug, nil,
			"the shared public leak check could not be installed: the daemon runs with no GitHub App, so it cannot read theorem-labs/agent-infra"},
		{"with the error underneath", "the served check could not be run", errors.New("fork/exec: permission denied"),
			"the shared public leak check could not be installed: the served check could not be run: fork/exec: permission denied"},
		{"newlines become spaces", "the served check did not answer", errors.New("line one\nline two\r\nline three"),
			"the shared public leak check could not be installed: the served check did not answer: line one line two line three"},
		{"a sentence already prefixed", RefusalPrefix + "the pod's launcher was served leak-check-v16, not its own pin leak-check-v15", nil,
			"the shared public leak check could not be installed: the pod's launcher was served leak-check-v16, not its own pin leak-check-v15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cause(tc.what, tc.err); got != tc.want {
				t.Errorf("Cause = %q, want %q", got, tc.want)
			}
		})
	}
}

// A long error is cut so the sentence is at most 400 bytes, and never inside a rune.
func TestALongCauseIsCutAtARuneBoundary(t *testing.T) {
	got := Cause("GitHub answered the read with HTTP 500", errors.New(strings.Repeat("é", 400)))
	if len(got) > maxCauseBytes {
		t.Errorf("the cause is %d bytes, want at most %d", len(got), maxCauseBytes)
	}
	if len(got) < maxCauseBytes-1 {
		t.Errorf("the cause is %d bytes, want it cut at the limit rather than well short of it", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("the cause %q was cut inside a rune", got)
	}
	if !strings.HasPrefix(got, RefusalPrefix+"GitHub answered the read with HTTP 500: é") {
		t.Errorf("the cause %q lost its beginning", got)
	}
}
