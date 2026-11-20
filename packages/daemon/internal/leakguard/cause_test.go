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

// plantedToken is the credential the URL credential tests plant in a URL, of no real token's
// shape: a text that holds any four bytes of it in a row holds a part of it (partOf).
const plantedToken = "qZxVkWjYpQrTfHbN"

// plantedUserinfo is a userinfo whose password is plantedToken, written apart from any URL.
const plantedUserinfo = "user:" + plantedToken

// partOf is the first four bytes in a row of planted that text holds, and "" where it holds none.
func partOf(text, planted string) string {
	for i := 0; i+4 <= len(planted); i++ {
		if piece := planted[i : i+4]; strings.Contains(text, piece) {
			return piece
		}
	}
	return ""
}

// The query and the userinfo of a URL are removed from the whole sentence before it is cut at
// maxCauseBytes, so a token that the cut of the sentence as written would split leaves no part of
// itself: the cause is the cut of the sentence with the credential gone.
func TestACauseCutAtItsBoundKeepsNoPartOfAURLCredential(t *testing.T) {
	const what = "the daemon could not read " + CheckSlug + " at leak-check-v15"
	const download = "codeload.example/theorem-labs/agent-infra/legacy.tar.gz/refs/tags/leak-check-v15"
	for _, tc := range []struct{ name, url, credential string }{
		{"a token in the query", "https://" + download + "?token=" + plantedToken, "?token=" + plantedToken},
		{"a token in the userinfo", "https://" + plantedUserinfo + "@" + download, plantedUserinfo + "@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := RefusalPrefix + what + ": "
			// The padding puts the token's first eight bytes before the cut of the sentence as
			// written, and the rest after it.
			pad := strings.Repeat("x", maxCauseBytes-8-len(head)-len(` Get "`)-strings.Index(tc.url, plantedToken))
			text := pad + ` Get "` + tc.url + `": dial tcp 127.0.0.1:9: connect: connection refused` + strings.Repeat(", and the words go on", 12)
			if written := head + text; partOf(written[:maxCauseBytes], plantedToken) == "" {
				t.Fatalf("the sentence as written, cut at %d bytes, holds no part of the token: the case tests nothing", maxCauseBytes)
			}

			got := Cause(what, errors.New(text))

			if want := (head + strings.Replace(text, tc.credential, "", 1))[:maxCauseBytes]; got != want {
				t.Errorf("Cause = %q, want %q: the sentence with its URL credential removed, then cut", got, want)
			}
			if part := partOf(got, plantedToken); part != "" {
				t.Errorf("the cause %q holds %q of the planted token", got, part)
			}
		})
	}
}
