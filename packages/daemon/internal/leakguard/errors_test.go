package leakguard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// The write's name and a setup failure's text are written as a refusal's lines are: the name can
// hold a remote's URL or a local path, which git hands the pre-push hook and the push's author
// chose, and a setup failure carries what git wrote on its standard error, which can quote such a
// path. An escape sequence and a byte 0x7f in the name, in the reason and in the failure beneath
// are each escaped (termsafe.Escaped); the failure beneath keeps its own line breaks, each of its
// lines escaped. A *NotFound writes its name the same way.
func TestAWritesNameAndASetupFailureWriteNoControlByteRaw(t *testing.T) {
	const name = "the push to /tmp/a\x1b[2Kb.git"
	written := map[string]string{
		"a refusal": (&Refusal{What: name, By: byRules, Lines: []string{"notes.md: " + theClass}}).Error(),
		"a setup failure": (&SetupFailure{
			What: name,
			Why:  "the push's commits could not be read\x7f",
			Err:  errors.New("git diff-tree exited 128: warning: one\nfatal: bad path 'x\x1b]0;title\x07y'"),
		}).Error(),
		"a not-found answer": (&NotFound{What: name, Repo: "invented-org/invented-repo"}).Error(),
	}
	wants := map[string][]string{
		"a refusal": {"LEAK CHECK: refusing the push to /tmp/a\\x1b[2Kb.git: "},
		"a setup failure": {
			"LEAK CHECK SETUP ERROR: the push to /tmp/a\\x1b[2Kb.git: the push's commits could not be read\\x7f: ",
			"git diff-tree exited 128: warning: one\nfatal: bad path 'x\\x1b]0;title\\x07y'",
		},
		"a not-found answer": {"the push to /tmp/a\\x1b[2Kb.git: GitHub answers 404 for "},
	}
	for kind, text := range written {
		for _, want := range wants[kind] {
			if !strings.Contains(text, want) {
				t.Errorf("%s is %q, want it to hold %q", kind, text, want)
			}
		}
		for _, r := range text {
			if r != '\n' && !unicode.IsPrint(r) {
				t.Errorf("%s writes %U raw: %q", kind, r, text)
			}
		}
	}
}

// A push to a URL rather than to a named remote has git hand its hook that URL as the remote's
// name too, so the push's name (PushName) can hold a credential, in the URL's userinfo or in its
// query; a setup failure's Why can quote a caller's argument, such as the path a gh call's
// --body-file names (readGhFile); and a failure underneath or a refusal's line can quote such a
// URL. No answer's Error(), which is what every consumer prints, holds the credential, and each
// still names the push by its URL with no credential. A remote's name that is no URL reads as
// written.
func TestNoAnswerPrintsACredentialThePushsNameCarries(t *testing.T) {
	const token = "FAKEtok3nZQ9x"
	for _, tc := range []struct {
		name, remote, shown string
	}{
		{name: "in the userinfo", remote: "https://x-access-token:" + token + "@github.com/acme/widgets.git", shown: "the push to https://github.com/acme/widgets.git"},
		{name: "as the userinfo, with no password", remote: "https://" + token + "@github.com/acme/widgets.git", shown: "the push to https://github.com/acme/widgets.git"},
		{name: "in the query", remote: "https://github.com/acme/widgets.git?access_token=" + token, shown: "the push to https://github.com/acme/widgets.git"},
		{name: "a remote's name", remote: "origin", shown: "the push to origin"},
	} {
		what := PushName(tc.remote)
		for _, answer := range []error{
			&Refusal{What: what, By: bySharedCheck, Lines: []string{"the remote " + tc.remote + " refused"}},
			&SetupFailure{What: what, Why: "--body-file " + tc.remote + " could not be read", Err: errors.New("fatal: unable to access '" + tc.remote + "/'")},
			&NotFound{What: what, Repo: "acme/widgets"},
		} {
			t.Run(tc.name+": "+fmt.Sprintf("%T", answer), func(t *testing.T) {
				printed := answer.Error()
				if strings.Contains(printed, token) {
					t.Errorf("Error() holds the token: %s", strings.ReplaceAll(printed, token, "<the token>"))
				}
				if !strings.Contains(printed, tc.shown+": ") {
					t.Errorf("Error() = %q, want it to name %q and then its colon", strings.ReplaceAll(printed, token, "<the token>"), tc.shown)
				}
			})
		}
	}
}

// A part of an answer that holds both a credential in a URL and a control byte is written with
// neither: the URL rule runs on the text as it is, and the escaping after it.
func TestAnAnswerWritesNeitherACredentialNorAControlByte(t *testing.T) {
	const token = "FAKEtok3nZQ9x"
	what := PushName("https://x-access-token:" + token + "@github.com/acme/wid\x1b[2Kgets.git")
	for _, answer := range []error{
		&Refusal{What: what, By: byRules, Lines: []string{"notes.md: " + theClass}},
		&SetupFailure{What: what, Why: "the read failed", Err: errors.New("fatal: unable to access 'https://u:" + token + "@github.com/x\x07y/'")},
		&NotFound{What: what, Repo: "acme/widgets"},
	} {
		t.Run(fmt.Sprintf("%T", answer), func(t *testing.T) {
			printed := answer.Error()
			if strings.Contains(printed, token) {
				t.Errorf("Error() holds the token: %s", strings.ReplaceAll(printed, token, "<the token>"))
			}
			if !strings.Contains(printed, "the push to https://github.com/acme/wid\\x1b[2Kgets.git: ") {
				t.Errorf("Error() = %q, want the push named by its URL, its control byte escaped", strings.ReplaceAll(printed, token, "<the token>"))
			}
			for _, r := range printed {
				if r != '\n' && !unicode.IsPrint(r) {
					t.Errorf("Error() writes %U raw", r)
				}
			}
		})
	}
}

// A credential that the URL rule does not see as one in the text as it is, because a tab, a
// no-break space, a U+202E or a control byte stands inside the URL and ends it there, is not
// written either: the escaping writes that character as letters, which joins the URL back up, and
// the URL rule runs again on what the escaping wrote. No four bytes in a row of the planted token
// (partOf) reach any answer's Error(), wherever the part stands: the write's name, a refusal's
// line, a setup failure's reason or the failure beneath it. The control is a URL with no
// credential, followed by a tab and the rest of a line, which keeps that rest.
func TestAnAnswerWritesNoCredentialItsEscapingJoinsIntoAURL(t *testing.T) {
	const token = "FAKEtok3nZQ9x"
	parts := []struct{ name, part string }{
		{name: "a tab inside the userinfo", part: "https://x-access-token:" + token + "\ttok@github.com/a/b"},
		{name: "a no-break space inside the userinfo", part: "https://x-access-token:" + token + "\u00a0tok@github.com/a/b"},
		{name: "a U+202E inside the scheme", part: "https\u202e://u:" + token + "@github.com/a"},
		{name: "a control byte inside the scheme", part: "https\x01://u:" + token + "@github.com/a"},
	}
	answers := func(part string) []error {
		return []error{
			&Refusal{What: PushName(part), By: bySharedCheck, Lines: []string{"the remote " + part + " refused"}},
			&SetupFailure{What: PushName(part), Why: "--body-file " + part + " could not be read", Err: errors.New("fatal: unable to access '" + part + "'")},
			&NotFound{What: PushName(part), Repo: "acme/widgets"},
		}
	}
	for _, tc := range parts {
		for _, answer := range answers(tc.part) {
			t.Run(tc.name+": "+fmt.Sprintf("%T", answer), func(t *testing.T) {
				if printed := answer.Error(); partOf(printed, token) != "" {
					t.Errorf("Error() holds %q of the token: %s", partOf(printed, token), strings.ReplaceAll(printed, token, "<the token>"))
				}
			})
		}
	}
	t.Run("control: a URL with no credential, then a tab and the rest of the line", func(t *testing.T) {
		for _, answer := range answers("https://github.com/a.git\tthe rest of the line") {
			if printed := answer.Error(); !strings.Contains(printed, "the rest of the line") {
				t.Errorf("%T Error() = %q, want it to keep the rest of the line", answer, printed)
			}
		}
	})
}
