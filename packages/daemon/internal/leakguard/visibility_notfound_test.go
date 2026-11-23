package leakguard

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The tests of the guard's answer to GitHub's 404 for its REST visibility read: the read through
// gh, and the same read through curl that the guard makes after gh could not reach GitHub. A 404
// is GitHub saying that no repository of that name exists or that the read's credential cannot
// see it. The guard still refuses the write, since it read no visibility, but it says what GitHub
// answered (NotFound): the write is neither refused for its text (*Refusal) nor stopped by a check
// that could not run (*SetupFailure), and a person who reads either marker looks for the wrong
// fault. Every other failure of the read keeps the setup failure it was, a 500 among them.

// ghPrints is a gh REST read that prints answer, gh's standard output and standard error as the
// two print together, and exits 1.
func ghPrints(answer string) string {
	return "printf '%s' " + shQuote(answer) + "\nexit 1\n"
}

// curlServerError is a curl to which GitHub answered 500.
var curlServerError = curlPrints("HTTP/2 500", []string{"content-type: application/json; charset=utf-8"}, `{"message":"Server Error"}`)

// notFoundWords is the whole of what the guard says of a write refused because GitHub answered
// its visibility read of theRepo with 404, what being the write in the entry point's words.
func notFoundWords(what string) string {
	return what + ": GitHub answers 404 for " + maskPath(theRepo) + ", so the repository does not exist or this credential cannot read it"
}

// The two markers no not-found answer holds: the one a setup failure starts with and the one a
// refusal starts with.
const (
	setupMarker   = "LEAK CHECK SETUP ERROR"
	refusalMarker = "LEAK CHECK: refusing"
)

// patchCall is the gh call the rows that check one make: a write to theRepo, which the guard
// checks whole, the visibility read included.
var patchCall = []string{"api", "-X", "PATCH", "repos/" + theRepo + "/pulls/1", "-f", "title=x"}

// GitHub's 404 for the REST visibility read refuses the write with words that say so, and no
// other read runs after it: GitHub answered, so neither GraphQL, nor curl, nor ls-remote could
// answer otherwise. The rows: gh's 404 (leakguardtest.GhReadNotFound, as gh 2.98.0 printed it)
// for a gh call (Guard.GhCall) and for a push (Guard.Push), each a *NotFound naming the write; the
// 404 that curl reads after gh could not reach GitHub, for a gh call, also a *NotFound, after
// which no ls-remote runs; and gh's 404 for the shared check's own read (Guard.FreshVisibility),
// which goes through no write and answers the read's own *notFoundError. None of them is a
// *SetupFailure or a *Refusal, or holds either marker, and none reports a source or keeps an
// answer.
func TestAVisibilityRead404IsToldAsNotFound(t *testing.T) {
	ghNotFound := ghPrints(leakguardtest.GhReadNotFound)
	for _, tc := range []struct {
		name string
		// rest is what gh's REST read does, and curl what curl does (newTransportRig); read makes
		// the read through guard and answers its error.
		rest, curl string
		read       func(t *testing.T, guard *Guard) error
		// target points at the type the answer is or wraps, for errors.As; words is the whole of
		// its text, where it is set; curlRuns is how often curl ran.
		target   any
		words    string
		curlRuns int
	}{
		{
			name: "a gh call", rest: ghNotFound, curl: curlAnswers("private"),
			read: func(t *testing.T, guard *Guard) error {
				_, err := guard.GhCall(bound(t), GhRequest{Args: patchCall, Repo: theRepo})
				return err
			},
			target: new(*NotFound), words: notFoundWords(whatGhCall),
		},
		{
			name: "a push", rest: ghNotFound, curl: curlAnswers("private"),
			read: func(t *testing.T, guard *Guard) error {
				// A ref the push deletes, named for a host the rules refuse: its name is the push's
				// one record, so a scan refuses the push and no scan passes it, and no git reads a
				// commit.
				return guard.Push(bound(t), PushRequest{
					Dir:     t.TempDir(),
					Remote:  "origin",
					URL:     theURL,
					Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/legion/" + theHost, RemoteSHA: noSHA}},
				})
			},
			target: new(*NotFound), words: notFoundWords(PushName("origin")),
		},
		{
			name: "the curl read after gh could not reach GitHub", rest: ghTLSTimeout, curl: curlNotFound,
			read: func(t *testing.T, guard *Guard) error {
				_, err := guard.GhCall(bound(t), GhRequest{Args: patchCall, Repo: theRepo})
				return err
			},
			target: new(*NotFound), words: notFoundWords(whatGhCall), curlRuns: 1,
		},
		{
			name: "the shared check's own read", rest: ghNotFound, curl: curlAnswers("private"),
			read: func(t *testing.T, guard *Guard) error {
				visibility, err := guard.FreshVisibility(bound(t), theRepo)
				if err == nil {
					t.Errorf("read %q, want the read refused", visibility)
				}
				return err
			},
			target: new(*notFoundError),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, tc.rest, noGraphQL, tc.curl, answersTheRefs,
				CheckVar+"="+leakguardtest.Write(t, t.TempDir(), passes))
			err := tc.read(t, rig.guard)
			if err == nil {
				t.Fatal("the write was cleared, want it refused")
			}
			if !errors.As(err, tc.target) {
				t.Errorf("the answer %q is a %T, want a %s", err, err, reflect.TypeOf(tc.target).Elem())
			}
			if errors.As(err, new(*SetupFailure)) || errors.As(err, new(*Refusal)) {
				t.Errorf("the answer %q is a setup failure or a refusal, want GitHub's 404 told as such", err)
			}
			if tc.words != "" && err.Error() != tc.words {
				t.Errorf("the answer says\n%q\nwant\n%q", err, tc.words)
			}
			if want := "GitHub answers 404 for invented-org/invented-repo"; !strings.Contains(err.Error(), want) {
				t.Errorf("the answer %q does not say %q", err, want)
			}
			for _, marker := range []string{setupMarker, refusalMarker} {
				if strings.Contains(err.Error(), marker) {
					t.Errorf("the answer %q holds %q", err, marker)
				}
			}
			rest, graphql, curl, git := rig.runs(t)
			if rest != 1 || graphql != 0 || curl != tc.curlRuns || git != 0 {
				t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, %d and 0", rest, graphql, curl, git, tc.curlRuns)
			}
			if len(rig.sources) != 0 || rig.kept(t) {
				t.Errorf("the guard reported the sources %q and kept an answer: %v; want neither for a refused read", rig.sources, rig.kept(t))
			}
		})
	}
}

// Every other failure of the REST visibility read stays the setup failure it was, with the words
// it had: a 500 that gh reads (leakguardtest.GhReadServerError), which runs no other read, and a
// 500 that curl reads after gh could not reach GitHub, which runs no ls-remote.
func TestAVisibilityRead500IsASetupFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rest, curl string
		// says is what the setup failure holds beside the words every row's does; curlRuns is
		// how often curl ran.
		says     []string
		curlRuns int
	}{
		{
			name: "gh's REST read", rest: ghPrints(leakguardtest.GhReadServerError), curl: curlAnswers("private"),
			says: []string{"gh api repos/" + maskPath(theRepo) + " exited 1: HTTP/2.0 500 Internal Server Error"},
		},
		{
			name: "the curl read after gh could not reach GitHub", rest: ghTLSTimeout, curl: curlServerError,
			says:     []string{"gh could not reach GitHub", "curl could not answer", "answered the status 500", "Server Error"},
			curlRuns: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, tc.rest, noGraphQL, tc.curl, answersTheRefs,
				CheckVar+"="+leakguardtest.Write(t, t.TempDir(), passes))
			_, err := rig.guard.GhCall(bound(t), GhRequest{Args: patchCall, Repo: theRepo})
			setupFailure(t, err)
			wants := append([]string{"LEAK CHECK SETUP ERROR: the gh call: the visibility of " + maskPath(theRepo) + " could not be read"}, tc.says...)
			for _, want := range wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the setup failure %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "GitHub answers 404") {
				t.Errorf("the setup failure %q says GitHub answered 404", err)
			}
			rest, graphql, curl, git := rig.runs(t)
			if rest != 1 || graphql != 0 || curl != tc.curlRuns || git != 0 {
				t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, %d and 0", rest, graphql, curl, git, tc.curlRuns)
			}
		})
	}
}
