package leakguard

import (
	"errors"
	"strings"
	"testing"
)

// A push to a URL rather than to a named remote has git hand its hook that URL as the remote's
// name too, so the push's name (PushName) can hold a credential, in the URL's userinfo or in its
// query, and a failure underneath can quote such a URL. No answer's Error(), which is what every
// consumer prints, holds the credential, and each still names the push by its URL with no
// credential. A remote's name that is no URL reads as written.
func TestNoAnswerPrintsACredentialThePushsNameCarries(t *testing.T) {
	const token = "FAKEtok3nZQ9x"
	for _, tc := range []struct {
		name, remote, shown string
	}{
		{name: "in the userinfo", remote: "https://x-access-token:" + token + "@github.com/acme/widgets.git", shown: "the push to https://github.com/acme/widgets.git"},
		{name: "in the query", remote: "https://github.com/acme/widgets.git?access_token=" + token, shown: "the push to https://github.com/acme/widgets.git"},
		{name: "a remote's name", remote: "origin", shown: "the push to origin"},
	} {
		what := PushName(tc.remote)
		for _, answer := range []error{
			&Refusal{What: what, By: bySharedCheck, Lines: []string{"the remote " + tc.remote + " refused"}},
			&SetupFailure{What: what, Why: "the push's commits could not be read", Err: errors.New("fatal: unable to access '" + tc.remote + "/'")},
			&NotFound{What: what, Repo: "acme/widgets"},
		} {
			t.Run(tc.name+": "+typeName(answer), func(t *testing.T) {
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

// typeName is the name of err's answer type, for a subtest's name.
func typeName(err error) string {
	switch err.(type) {
	case *Refusal:
		return "Refusal"
	case *SetupFailure:
		return "SetupFailure"
	case *NotFound:
		return "NotFound"
	}
	return "other"
}
