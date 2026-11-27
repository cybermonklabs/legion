package leakguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The tests of how the guard reads a child's two streams: gh's REST read of a visibility
// (Guard.ghVisibility) takes its answer from gh's standard output alone and a failure gh or
// `legion gh` reports from its standard error, and a push's git reads (Guard.git) take each record
// from git's standard output alone, so a caller's GH_DEBUG or GIT_TRACE changes no verdict. The gh
// rows read what the real gh and `legion gh` printed, each stream in a file of its own, under
// testdata/gh-rest-read/; the comment of TestEveryReadOfTheGuardAnswersFromItsStandardOutputAlone
// gives the command each was captured with.

// capturedRepo is the repository the debug and transport fixtures read. The not-found and
// unserved-owner fixtures read theRepo.
const capturedRepo = "cybermonklabs/legion"

// capturedRead is the path of what the real gh printed on stream, "stdout" or "stderr", for the
// REST read the fixture name stands for.
func capturedRead(t *testing.T, name, stream string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "gh-rest-read", name+"."+stream))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// printsCaptured is a stand-in gh's REST read that prints the fixture name's standard output on
// its standard output and the fixture's standard error on its standard error, then exits with
// exit.
func printsCaptured(t *testing.T, name string, exit int) string {
	t.Helper()
	return "cat " + shQuote(capturedRead(t, name, "stdout")) + "\n" +
		"cat " + shQuote(capturedRead(t, name, "stderr")) + " >&2\n" +
		fmt.Sprintf("exit %d\n", exit)
}

// capturedLine is the one line the fixture name's stream holds, without its newline.
func capturedLine(t *testing.T, name, stream string) string {
	t.Helper()
	printed, err := os.ReadFile(capturedRead(t, name, stream))
	if err != nil {
		t.Fatal(err)
	}
	line, found := strings.CutSuffix(string(printed), "\n")
	if !found || strings.Contains(line, "\n") {
		t.Fatalf("%s.%s holds %q, want one line", name, stream, printed)
	}
	return line
}

// gh's answer under GH_DEBUG=api, as gh 2.98.0 printed it (the debug fixture): the status line,
// the headers and the body on standard output, and 191 lines of the request and of GitHub's answer
// on standard error. The read answers the public its standard output says, from GitHub, as it
// does with nothing on standard error, and makes no other read. At the base, which read the two
// streams as one, the debug lines ran on after the body, and the read answered no visibility
// GitHub reports.
func TestTheGhReadAnswersTheVisibilityWhateverGhWritesOnStandardError(t *testing.T) {
	for _, tc := range []struct{ name, rest string }{
		{name: "nothing on standard error", rest: "cat " + shQuote(capturedRead(t, "debug", "stdout")) + "\nexit 0\n"},
		{name: "gh's debug lines on standard error", rest: printsCaptured(t, "debug", 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, tc.rest, noGraphQL, curlAnswers("private"), answersTheRefs)
			visibility, err := rig.guard.Visibility(bound(t), capturedRepo)
			if err != nil || visibility != Public {
				t.Fatalf("read %q, %v; want gh's public", visibility, err)
			}
			if want := []VisibilitySource{VisibilityFromGitHub}; !slices.Equal(rig.sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", rig.sources, want)
			}
			if rest, graphql, curl, git := rig.runs(t); rest != 1 || graphql != 0 || curl != 0 || git != 0 {
				t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, 0 and 0", rest, graphql, curl, git)
			}
		})
	}
}

// A REST read that failed on gh's connection to GitHub is read from what gh printed on standard
// error alone: a proxy that refused the connection, as gh 2.98.0 printed it (the transport
// fixture), and a TLS handshake that never finished, the failure of the busy one-CPU pod. Each
// takes ghTransportFailure's one path: the read goes on to curl, and then to ls-remote, and where
// both of those fail too, the setup error names gh's line.
func TestTheGhReadNamesATransportFailureFromStandardError(t *testing.T) {
	tlsLine := `Get "https://api.github.com/repos/` + theRepo + `": net/http: TLS handshake timeout`
	for _, tc := range []struct {
		name, slug, rest string
		// line is what gh printed on standard error, which the setup error names.
		line string
	}{
		{name: "a proxy that refuses the connection", slug: capturedRepo, rest: printsCaptured(t, "transport", 1), line: capturedLine(t, "transport", "stderr")},
		{name: "a TLS handshake timeout", slug: theRepo, rest: printsToStderr(tlsLine) + "exit 1\n", line: tlsLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, tc.rest, noGraphQL, curlTimesOut, asksForACredential,
				CheckVar+"="+leakguardtest.Write(t, t.TempDir(), passes), RulesFileVar+"="+leakguardtest.WriteRules(t, t.TempDir()))
			_, err := rig.guard.GhCall(bound(t), GhRequest{Args: []string{"api", "-X", "PATCH", "repos/" + tc.slug + "/pulls/1", "-f", "title=x"}, Repo: tc.slug})
			says(t, setupFailure(t, err), "the visibility of "+tc.slug+" could not be read: gh could not reach GitHub: "+tc.line+", curl could not reach GitHub")
			if rest, graphql, curl, git := rig.runs(t); rest != 1 || graphql != 0 || curl != 1 || git != 1 {
				t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, 1 and 1", rest, graphql, curl, git)
			}
		})
	}
}

// `legion gh`'s refusal of an owner the project's gh bundle holds no directory for, as it printed
// it (the unserved-owner fixture, with LEGION_GH_OWNERS_DIR=/var/run/legion/gh): nothing on
// standard output, its one line on standard error, and exit 2. The guard reads it as
// Guard.unservedOwner read the two streams together at the base: the read goes on to ls-remote
// with no credential, whose refs answer public, and makes no GraphQL and no curl read. The same
// line beside an answer on standard output is no such refusal, as it was not at the base: the
// read is refused, naming what gh said on standard error, and git never runs.
func TestTheGhReadReadsLegionGhsRefusalFromStandardError(t *testing.T) {
	const owners = "/var/run/legion/gh"
	for _, tc := range []struct {
		name, rest string
		// public is whether the read answers public through ls-remote.
		public bool
	}{
		{name: "the refusal", rest: printsCaptured(t, "unserved-owner", 2), public: true},
		{name: "the refusal beside an answer on standard output", rest: "echo public\n" + printsCaptured(t, "unserved-owner", 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, tc.rest, noGraphQL, curlAnswers("private"), answersTheRefs, "LEGION_GH_OWNERS_DIR="+owners)
			visibility, err := rig.guard.Visibility(bound(t), theRepo)
			rest, graphql, curl, git := rig.runs(t)
			if !tc.public {
				if want := "gh api repos/" + theRepo + " exited 2: " + ownerNotServedLine("invented-org", owners); err == nil || err.Error() != want {
					t.Errorf("read %q, %v; want the read refused saying %q", visibility, err, want)
				}
				if rest != 1 || graphql != 0 || curl != 0 || git != 0 {
					t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, 0 and 0", rest, graphql, curl, git)
				}
				return
			}
			if err != nil || visibility != Public {
				t.Fatalf("read %q, %v; want public from ls-remote", visibility, err)
			}
			if want := []VisibilitySource{VisibilityFromLsRemote}; !slices.Equal(rig.sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", rig.sources, want)
			}
			if rest != 1 || graphql != 0 || curl != 0 || git != 1 {
				t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, 0 and 1", rest, graphql, curl, git)
			}
			if calls := readCalls(t, rig.git); !strings.Contains(calls[0]["argv"], " ls-remote "+theURL+" HEAD") {
				t.Errorf("git ran as %q, want an ls-remote of %s", calls[0]["argv"], theURL)
			}
		})
	}
}

// GitHub's 404 for the REST read, as gh 2.98.0 printed it (the not-found fixture): the status
// line, the headers and GitHub's body on standard output, gh's own words on standard error, and
// exit 1. The status line on standard output makes the read a *notFoundError, which tells what
// GitHub answered, and no other read follows it.
func TestTheGhReadReadsANotFoundFromStandardOutput(t *testing.T) {
	if line := capturedLine(t, "not-found", "stderr"); line != "gh: Not Found (HTTP 404)" {
		t.Fatalf("gh's words on standard error are %q, want gh: Not Found (HTTP 404)", line)
	}
	rig := newTransportRig(t, printsCaptured(t, "not-found", 1), noGraphQL, curlAnswers("private"), answersTheRefs)
	visibility, err := rig.guard.Visibility(bound(t), theRepo)
	var notFound *notFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("read %q, %v; want GitHub's 404", visibility, err)
	}
	if want := notFoundText(shownText(theRepo)); err.Error() != want {
		t.Errorf("the read says %q, want %q", err, want)
	}
	if rest, graphql, curl, git := rig.runs(t); rest != 1 || graphql != 0 || curl != 0 || git != 0 {
		t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, 0 and 0", rest, graphql, curl, git)
	}
}

// A push read under a caller's GIT_TRACE=1, which makes every git the guard runs write a trace of
// its commands on standard error, gets the verdict the same push gets without it (the case
// INFRA-3456's tester met): the guard reads each record from git's standard output alone
// (Guard.git), so no line of a trace is read as a remote's name or a commit's id. A push whose
// commit names a host the rules refuse, in its message and in a line it adds, is refused with the
// same report, and a push whose commit names nothing is passed; neither says `invalid object
// name`, git's words for a trace line read as a commit.
func TestAPushUnderGitTraceIsJudgedAsWithoutIt(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	// The fixture's own git runs under an environment that reaches no repository, remote,
	// credential or configuration but the test's (docs/testing.md), and with no GIT_TRACE.
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	commit := func(t *testing.T, branch, from, content, message string) string {
		t.Helper()
		if from != "" {
			fixture.Run(repo, "git", "checkout", "--quiet", "-B", branch, from)
		}
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	base := commit(t, "main", "", "one\n", "the commit the destination holds")
	// The destination holds the base commit on a remote-tracking ref of this repository, and
	// origin then names it by the URL each push below is made to.
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	fixture.Run(repo, "git", "remote", "set-url", "origin", theURL)
	refused := commit(t, "refused", base, "one\nthe runner answers at "+leakguardtest.RulesHost+"\n", "moved the run to "+leakguardtest.RulesHost)
	clean := commit(t, "clean", base, "one\ntwo\n", "a message of no interest")
	push := func(t *testing.T, head string, extra ...string) error {
		t.Helper()
		return guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, extra...).Push(bound(t), PushRequest{
			Dir:     repo,
			Remote:  "origin",
			URL:     theURL,
			Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}},
		})
	}
	for _, tc := range []struct {
		name, head string
		// refused is whether the push is refused without GIT_TRACE.
		refused bool
	}{
		{name: "a push whose records the rules refuse", head: refused, refused: true},
		{name: "a push whose records the rules clear", head: clean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			without := push(t, tc.head)
			traced := push(t, tc.head, "GIT_TRACE=1")
			for _, err := range []error{without, traced} {
				if err != nil && strings.Contains(err.Error(), "invalid object name") {
					t.Errorf("the push = %v, which reads a line of git's trace as a commit", err)
				}
			}
			if !tc.refused {
				if without != nil || traced != nil {
					t.Fatalf("the push = %v without GIT_TRACE and %v under it; want it passed both ways", without, traced)
				}
				return
			}
			plain, underTrace := refusal(t, without), refusal(t, traced)
			if plain.By != byRules || len(plain.Lines) == 0 {
				t.Fatalf("the push without GIT_TRACE was refused by %q with the report %q, want the rules' report", plain.By, plain.Lines)
			}
			if underTrace.By != plain.By || !slices.Equal(underTrace.Lines, plain.Lines) {
				t.Errorf("the push under GIT_TRACE was refused by %q with the report %q, want %q with %q as without it",
					underTrace.By, underTrace.Lines, plain.By, plain.Lines)
			}
		})
	}
}

// A caller's GH_DEBUG=api changes nothing of a REST read that failed on gh's connection to
// GitHub: the guard runs gh with GH_DEBUG empty (ghReadEnv), so gh writes its one line, and the
// read goes on to curl, whose private is the answer, as it does without GH_DEBUG. The stand-in gh
// records the GH_DEBUG it was given, writes what gh 2.98.0 wrote on standard error under
// GH_DEBUG=api (the debug fixture's standard error) only where its own GH_DEBUG is set and not
// empty, as gh 2.98.0 does, then the line it wrote for a proxy that refused the connection (the
// transport fixture), and exits 1. At the base, which handed gh the caller's GH_DEBUG and read the
// two streams as one, gh wrote the debug lines too, and the read was refused.
func TestATransportFailureUnderGhDebugFallsBackAsWithoutIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		// env is what the guard's environment holds beyond the rig's own.
		env []string
	}{
		{name: "without GH_DEBUG"},
		{name: "under the caller's GH_DEBUG=api", env: []string{"GH_DEBUG=api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := filepath.Join(t.TempDir(), "gh-debug")
			rest := `printf '%s\n' "${GH_DEBUG-unset}" >> ` + shQuote(seen) + "\n" +
				`if [ -n "${GH_DEBUG-}" ]; then cat ` + shQuote(capturedRead(t, "debug", "stderr")) + " >&2; fi\n" +
				"cat " + shQuote(capturedRead(t, "transport", "stderr")) + " >&2\nexit 1\n"
			rig := newTransportRig(t, rest, noGraphQL, curlAnswers("private"), answersTheRefs, tc.env...)
			visibility, err := rig.guard.Visibility(bound(t), capturedRepo)
			if err != nil || visibility != Private {
				t.Fatalf("read %q, %v; want curl's private", visibility, err)
			}
			if want := []VisibilitySource{VisibilityFromCurl}; !slices.Equal(rig.sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", rig.sources, want)
			}
			if rest, graphql, curl, git := rig.runs(t); rest != 1 || graphql != 0 || curl != 1 || git != 0 {
				t.Errorf("gh's REST read ran %d times, GraphQL %d, curl %d and git %d; want 1, 0, 1 and 0", rest, graphql, curl, git)
			}
			if given, err := os.ReadFile(seen); err != nil || string(given) != "\n" {
				t.Errorf("gh ran with GH_DEBUG %q (%v), want it set and empty", given, err)
			}
		})
	}
}

// A gh REST read that the guard's own bound (ghReadContext) stops before GitHub sent a status
// line is a transport failure that names the stop and what gh said on standard error before it,
// so a person who reads the failure reads what gh said. The guard's bound is shortened to 1 s
// inside the caller's minute.
func TestAGhReadStoppedByItsBoundNamesWhatGhSaid(t *testing.T) {
	shorten(t, &ghReadBound, time.Second)
	const line = "invented: the TLS handshake with api.github.com has not finished"
	rig := newTransportRig(t, printsToStderr(line)+"exec sleep 86400\n", noGraphQL, curlAnswers("private"), answersTheRefs)
	var err error
	answered(t, "the REST read whose gh never answers", func() { _, err = rig.guard.ghVisibility(bound(t), theRepo) })
	var unreached *transportError
	if !errors.As(err, &unreached) {
		t.Fatalf("the read = %v, want gh's stop by the guard's own bound as a transport failure", err)
	}
	said := regexp.MustCompile(`^gh could not reach GitHub: gh ran \d+ s and was stopped by its bound of \d+ s: ` + regexp.QuoteMeta(line) + `$`)
	if !said.MatchString(unreached.Error()) {
		t.Errorf("the failure says %q, want it to match %q", unreached, said)
	}
}
