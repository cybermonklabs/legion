package leakguard

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// ghAnswers is a stand-in gh whose every call appends a line to the file reads names, prints
// lines to standard error and exits with exit: what `legion gh` does when it refuses a call before
// any gh runs.
func ghAnswers(reads string, exit int, lines ...string) string {
	return "echo read >> " + shQuote(reads) + "\n" + printsToStderr(lines...) + fmt.Sprintf("exit %d\n", exit)
}

// A push to a repository of an owner the project does not serve: `legion gh` refuses the guard's
// REST read, exit 2, because the gh bundle holds no directory for the owner, and so holds no token
// that could read the repository. The guard then reads the visibility with git's ls-remote and no
// credential, once, as it does for a spent rate limit: the credential helper, the extra headers
// and http.proactiveAuth emptied after the package's pins, GIT_TERMINAL_PROMPT=0, and an empty
// HOME. Where ls-remote lists the repository's refs, the repository is public and the push goes
// on to the scan, which refuses this one's ref name. Where GitHub asks ls-remote for a credential,
// as it does for a private repository and for a name no repository has, and where ls-remote
// fails another way, the push is refused as a setup failure naming both reads. gh is asked once,
// and never through GraphQL, and curl never runs: the guard makes no REST read with no credential,
// whose budget every reader of GitHub from the address shares (readUnservedOwner).
func TestTheVisibilityOfAnOwnerLegionGhDoesNotServeIsReadWithNoCredential(t *testing.T) {
	// owners is the bundle LEGION_GH_OWNERS_DIR names. Nothing reads it: Guard.unservedOwner
	// compares that name, as text, with the one in `legion gh`'s refusal.
	const owners = "/var/run/invented/gh"
	scanned := func(t *testing.T, err error, sources []VisibilitySource) {
		t.Helper()
		if refused := refusal(t, err); refused.By != byRules {
			t.Errorf("the push was refused by %q, want the rules: a public destination is scanned", refused.By)
		}
		if want := []VisibilitySource{VisibilityFromLsRemote}; !slices.Equal(sources, want) {
			t.Errorf("the guard reported the sources %q, want %q", sources, want)
		}
	}
	refusedSaying := func(wants ...string) func(*testing.T, error, []VisibilitySource) {
		return func(t *testing.T, err error, sources []VisibilitySource) {
			t.Helper()
			says(t, setupFailure(t, err), append([]string{
				"the visibility of " + theRepo + " could not be read",
				"gh api repos/" + theRepo + " exited 2: " + ownerNotServedLine("invented-org", owners) +
					", and git ls-remote with no credential could not answer: git ls-remote of " + theRepo + " exited 128",
			}, wants...)...)
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a read that failed, want none", sources)
			}
		}
	}
	for _, tc := range []struct {
		name string
		// git is what the stand-in git does after it records its call.
		git string
		// check reads the push's answer and the sources the guard reported.
		check func(t *testing.T, err error, sources []VisibilitySource)
	}{
		{name: "ls-remote lists the refs", git: answersTheRefs, check: scanned},
		{name: "GitHub asks ls-remote for a credential", git: asksForACredential, check: refusedSaying("could not read Username")},
		{name: "ls-remote fails another way", git: cannotConnect, check: refusedSaying("Could not resolve host")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			reads, graphql, record := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
			curl := filepath.Join(records, "curl")
			writeProgram(t, bin, "gh", ghStandIn(ghAnswers(reads, 2, ownerNotServedLine("invented-org", owners)), graphQLAnswers(graphql, "PRIVATE")))
			writeProgram(t, bin, "curl", curlStandIn(curl, filepath.Join(records, "curl-stdin"), curlAnswers("private")))
			writeProgram(t, bin, "git", leakguardtest.RecordsGitCall(record)+tc.git)
			var sources []VisibilitySource
			guard := New(Options{
				Check:        leakguardtest.Write(t, t.TempDir(), passes),
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir(), "LEGION_GH_OWNERS_DIR="+owners),
			})
			// The guard's curl is the stand-in, where the guard would otherwise run curlProgram:
			// with curlProgram, a curl read would run a curl this test never counts, and the
			// assertion below that curl never runs could not fail.
			guard.curl = filepath.Join(bin, "curl")
			// A ref the push deletes, named for a host the rules refuse: its name is the push's one
			// record, so a scan refuses the push and no scan passes it, and no git reads a commit.
			err := guard.Push(bound(t), PushRequest{
				Dir:     t.TempDir(),
				Remote:  "origin",
				URL:     theURL,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/legion/" + theHost, RemoteSHA: noSHA}},
			})
			tc.check(t, err, sources)
			if got := countLines(t, reads); got != 1 {
				t.Errorf("gh's REST read ran %d times, want once", got)
			}
			if got := len(leakguardtest.ReadCalls(t, graphql)); got != 0 {
				t.Errorf("gh's GraphQL read ran %d times, want none: the pod holds no token for the owner", got)
			}
			if got := countLines(t, curl); got != 0 {
				t.Errorf("curl ran %d times, want never: the read with no credential is ls-remote's, which makes no API request", got)
			}
			calls := leakguardtest.ReadCalls(t, record)
			if len(calls) != 1 {
				t.Fatalf("git ran %d times, want once", len(calls))
			}
			if want := strings.Join(gitPins, " ") + " " + theResets + " ls-remote " + theURL + " HEAD"; calls[0]["argv"] != want {
				t.Errorf("git ran as\n%s\nwant\n%s", calls[0]["argv"], want)
			}
			if calls[0]["prompt"] != "0" || calls[0]["homeholds"] != "" || calls[0]["netrc"] != "unset" {
				t.Errorf("git ran with GIT_TERMINAL_PROMPT=%s, a HOME holding %q and NETRC=%s; want 0, an empty HOME and no NETRC",
					calls[0]["prompt"], calls[0]["homeholds"], calls[0]["netrc"])
			}
		})
	}
}

// Only `legion gh`'s refusal of the repository's owner sends the read to ls-remote: exit 2, and
// that one line for the owner of the repository read, lowercased as `legion gh` names it, and for
// the bundle LEGION_GH_OWNERS_DIR names. An owner spelled in capitals is read with no credential
// at the URL as it was spelled. Every other answer is refused, and git never runs, although the
// stand-in git would answer public: another refusal of `legion gh`; the line for another owner,
// for another bundle, under another exit status, beside another line or with no `legion gh: ` in
// front; the refusal of a configured rename whose new owner the bundle holds no directory for,
// which holds these words inside a longer line; the line where the guard's environment names no
// bundle at all, each of these the setup failure it was; and GitHub's own 404 through gh, which
// the refusal tells as GitHub's answer for the repository.
func TestOnlyLegionGhsRefusalOfTheOwnerIsReadWithNoCredential(t *testing.T) {
	const owners = "/var/run/invented/gh"
	theRefusal := ownerNotServedLine("invented-org", owners)
	renameRefusal := "legion gh: the configured rename invented-org/invented-repo -> moved-org/invented-repo serves invented-org/invented-repo " +
		"from moved-org's gh directory, and " + (&OwnerNotServedError{Owner: "moved-org", OwnersDir: owners}).Error() +
		"; GitHub holds the repository under the name it moved to, so no other token of this bundle reaches it"
	notFound := []string{"HTTP/2.0 404 Not Found", "", `{"message":"Not Found","status":"404"}`, "gh: Not Found (HTTP 404)"}
	for _, tc := range []struct {
		name string
		// slug is the repository read, theRepo where it is empty; exit and lines are what the
		// stand-in gh exits with and prints; env is what the guard's environment holds beyond the
		// test's own, LEGION_GH_OWNERS_DIR=owners where it is nil.
		slug  string
		exit  int
		lines []string
		env   []string
		// public is whether the read answers public through ls-remote; a read that does not fails
		// saying `gh api repos/<slug> <fails>`, or says where it is set.
		public bool
		fails  string
		says   string
	}{
		{name: "the refusal", exit: 2, lines: []string{theRefusal}, public: true},
		{name: "the refusal of an owner spelled in capitals", slug: "Invented-Org/invented-repo", exit: 2, lines: []string{theRefusal}, public: true},
		{
			name: "another refusal of legion gh", exit: 2,
			lines: []string{"legion gh: GH_CONFIG_DIR is unset: a Legion pane or pod sets it, and nothing else runs this gh"},
			fails: "exited 2: legion gh: GH_CONFIG_DIR is unset",
		},
		{name: "the refusal of another owner", exit: 2, lines: []string{ownerNotServedLine("other-owner", owners)}, fails: "exited 2: " + ownerNotServedLine("other-owner", owners)},
		{name: "the refusal for another bundle", exit: 2, lines: []string{ownerNotServedLine("invented-org", "/var/run/other/gh")}, fails: "exited 2: " + ownerNotServedLine("invented-org", "/var/run/other/gh")},
		{name: "the refusal under exit 1", exit: 1, lines: []string{theRefusal}, fails: "exited 1: " + theRefusal},
		{name: "the refusal and another line after it", exit: 2, lines: []string{theRefusal, "usage: legion gh"}, fails: "exited 2: " + theRefusal},
		{name: "another line and the refusal after it", exit: 2, lines: []string{"warning: invented", theRefusal}, fails: "exited 2: warning: invented"},
		{
			name: "the refusal with no legion gh in front", exit: 2, lines: []string{strings.TrimPrefix(theRefusal, "legion gh: ")},
			fails: "exited 2: " + (&OwnerNotServedError{Owner: "invented-org", OwnersDir: owners}).Error(),
		},
		{
			name: "the refusal of a configured rename whose new owner has no directory", exit: 2,
			lines: []string{renameRefusal}, fails: "exited 2: " + renameRefusal,
		},
		{name: "GitHub's own 404", exit: 1, lines: notFound, says: "GitHub answers 404 for " + maskPath(theRepo)},
		{name: "the refusal where no bundle is named", exit: 2, lines: []string{theRefusal}, env: []string{"LEGION_GH_OWNERS_DIR="}, fails: "exited 2: " + theRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slug := cmp.Or(tc.slug, theRepo)
			env := tc.env
			if env == nil {
				env = []string{"LEGION_GH_OWNERS_DIR=" + owners}
			}
			bin, records := t.TempDir(), t.TempDir()
			reads, record := filepath.Join(records, "reads"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", ghAnswers(reads, tc.exit, tc.lines...))
			writeProgram(t, bin, "git", leakguardtest.RecordsGitCall(record)+answersTheRefs)
			visibility, err := New(Options{
				Env: append(append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()), env...),
			}).Visibility(bound(t), slug)
			calls := leakguardtest.ReadCalls(t, record)
			if tc.public {
				if err != nil || visibility != Public {
					t.Fatalf("read %q, %v; want public from ls-remote", visibility, err)
				}
				if len(calls) != 1 || !strings.Contains(calls[0]["argv"], " ls-remote "+githubOrigin+"/"+slug+".git HEAD") {
					t.Errorf("git ran as %q, want one ls-remote of %s", calls, slug)
				}
			} else {
				want := cmp.Or(tc.says, "gh api repos/"+slug+" "+tc.fails)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("read %q, %v; want the read refused saying %q", visibility, err, want)
				}
				if len(calls) != 0 {
					t.Errorf("git ran %d times, want never: only the refusal of the owner is read with no credential", len(calls))
				}
			}
			if got := countLines(t, reads); got != 1 {
				t.Errorf("gh's REST read ran %d times, want once", got)
			}
		})
	}
}

// A push to a repository of an owner the project serves is read as before: gh's REST read answers
// it with the repository's token, its public is scanned and its private is not, and git never runs.
func TestTheVisibilityOfAnOwnerLegionGhServesIsReadAsBefore(t *testing.T) {
	for _, visibility := range []Visibility{Public, Private} {
		t.Run(string(visibility), func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			reads, record := filepath.Join(records, "reads"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", "echo read >> "+shQuote(reads)+"\n"+
				printsResponse("HTTP/2.0 200 OK", []string{"X-Ratelimit-Remaining: 4999"}, string(visibility))+"exit 0\n")
			writeProgram(t, bin, "git", leakguardtest.RecordsGitCall(record)+answersTheRefs)
			var sources []VisibilitySource
			guard := New(Options{
				Check:        leakguardtest.Write(t, t.TempDir(), passes),
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir(), "LEGION_GH_OWNERS_DIR="+t.TempDir()),
			})
			err := guard.Push(bound(t), PushRequest{
				Dir:     t.TempDir(),
				Remote:  "origin",
				URL:     theURL,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/legion/" + theHost, RemoteSHA: noSHA}},
			})
			if visibility == Public {
				if refused := refusal(t, err); refused.By != byRules {
					t.Errorf("the push was refused by %q, want the rules: a public destination is scanned", refused.By)
				}
			} else if err != nil {
				t.Errorf("the push to a private destination: %v; want it passed, unscanned", err)
			}
			if want := []VisibilitySource{VisibilityFromGitHub}; !slices.Equal(sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", sources, want)
			}
			if got := countLines(t, reads); got != 1 {
				t.Errorf("gh's REST read ran %d times, want once", got)
			}
			if calls := leakguardtest.ReadCalls(t, record); len(calls) != 0 {
				t.Errorf("git ran %d times, want never", len(calls))
			}
		})
	}
}
