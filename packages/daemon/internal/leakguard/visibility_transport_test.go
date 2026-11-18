package leakguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// The tests of the reads the guard makes when gh's REST read fails on its connection to GitHub
// (Guard.readWithoutGh): curl's REST read, then git's ls-remote.

// ghFailsOn is what gh 2.98.0 printed on standard error, exit 1, for a REST read of theRepo whose
// request failed with cause: Go's error for the request, naming its URL. Measured on 2026-11-18
// through a local proxy for a TLS handshake that never finished, a connection reset and a refused
// connection.
func ghFailsOn(cause string) string {
	return printsToStderr(`Get "https://api.github.com/repos/`+theRepo+`": `+cause) + "exit 1\n"
}

// ghTLSTimeout is gh's REST read failing as every gh call failed in the busy one-CPU pod.
var ghTLSTimeout = ghFailsOn("net/http: TLS handshake timeout")

// theToken is the token the stand-in `gh auth git-credential get` answers. Each invented token here
// says not-a-secret, which the shared leak check reads as no credential.
const theToken = "invented-token-not-a-secret"

// transportGh is a stand-in gh. Each REST read appends a line to reads and then does what rest
// says. The GraphQL read does what graphql says. `gh auth git-credential get` writes its request to
// credential and answers theToken, as `legion gh` answers from the bundle's hosts.yml.
func transportGh(reads, rest, graphql, credential string) string {
	return "if [ \"$1\" = auth ]; then\n" +
		"cat > " + shQuote(credential) + "\n" +
		"printf 'protocol=https\\nhost=github.com\\nusername=x-access-token\\npassword=%s\\n' " + shQuote(theToken) + "\nexit 0\nfi\n" +
		ghStandIn("echo read >> "+shQuote(reads)+"\n"+rest, graphql)
}

// noGraphQL is a GraphQL read that only records that it ran, to record, and fails.
func noGraphQL(record string) string {
	return "echo ran >> " + shQuote(record) + "\nexit 1\n"
}

// curlStandIn is a stand-in curl. It appends a line to record with its arguments and with the
// GH_TOKEN and GITHUB_TOKEN it was given (readCalls reads it), writes its standard input to stdin,
// and then does what then says.
func curlStandIn(record, stdin, then string) string {
	return `printf 'argv=%s\tghtoken=%s\tgithubtoken=%s\n' "$*" "${GH_TOKEN-unset}" "${GITHUB_TOKEN-unset}" >> ` + shQuote(record) + "\n" +
		"cat > " + shQuote(stdin) + "\n" + then
}

// curlPrints is a curl that prints the status line, the headers, a blank line and body, as
// `curl --include` prints a response, and exits 0, which curl does whatever the status.
func curlPrints(status string, headers []string, body string) string {
	lines := append(append([]string{status}, headers...), "")
	quoted := make([]string, len(lines))
	for i, line := range lines {
		quoted[i] = shQuote(line)
	}
	return "printf '%s\\r\\n' " + strings.Join(quoted, " ") + "\nprintf '%s' " + shQuote(body) + "\nexit 0\n"
}

// curlAnswers is a curl to which GitHub answered 200 with visibility as theRepo's visibility.
func curlAnswers(visibility string) string {
	return curlPrints("HTTP/2 200", []string{"content-type: application/json; charset=utf-8", "x-ratelimit-remaining: 4999"},
		`{"full_name":"`+theRepo+`","visibility":"`+visibility+`"}`)
}

// curlRateLimited is a curl to which GitHub answered as it answers once the installation's core
// limit is spent: 403, X-RateLimit-Remaining 0 and the reset.
var curlRateLimited = curlPrints("HTTP/2 403", []string{"content-type: application/json; charset=utf-8",
	"x-ratelimit-limit: 15000", "x-ratelimit-remaining: 0", "x-ratelimit-reset: 1794636000", "x-ratelimit-resource: core"},
	`{"message":"API rate limit exceeded for installation ID 1.","status":"403"}`)

// curlNotFound is a curl to which GitHub answered 404.
var curlNotFound = curlPrints("HTTP/2 404", []string{"content-type: application/json; charset=utf-8"},
	`{"message":"Not Found","documentation_url":"https://docs.github.com/rest/repos/repos#get-a-repository","status":"404"}`)

// curlTimesOut is a curl whose TLS handshake did not finish within its --max-time.
var curlTimesOut = printsToStderr("curl: (28) SSL connection timeout") + "exit 28\n"

// transportRig is one guard whose gh, curl and git are stand-ins, the files they record to, and
// the sources the guard reported.
type transportRig struct {
	guard                           *Guard
	sources                         []VisibilitySource
	reads, graphql, credential, git string
	curl, curlStdin                 string
}

// newTransportRig is a transportRig whose gh REST read does what rest says, whose GraphQL read
// what graphql says (given the file it records to), whose curl what curl says, and whose git what
// git says, with extra on top of the guard's environment.
func newTransportRig(t *testing.T, rest string, graphql func(record string) string, curl, git string, extra ...string) *transportRig {
	t.Helper()
	bin, records := t.TempDir(), t.TempDir()
	rig := &transportRig{
		reads:      filepath.Join(records, "reads"),
		graphql:    filepath.Join(records, "graphql"),
		credential: filepath.Join(records, "credential"),
		git:        filepath.Join(records, "git"),
		curl:       filepath.Join(records, "curl"),
		curlStdin:  filepath.Join(records, "curl-stdin"),
	}
	writeProgram(t, bin, "gh", transportGh(rig.reads, rest, graphql(rig.graphql), rig.credential))
	writeProgram(t, bin, "curl", curlStandIn(rig.curl, rig.curlStdin, curl))
	writeProgram(t, bin, "git", recordsItsCall(rig.git)+git)
	rig.guard = New(Options{
		OnVisibility: func(source VisibilitySource) { rig.sources = append(rig.sources, source) },
		Env:          append(append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()), extra...),
	})
	return rig
}

// runs is how many times each stand-in ran: gh's REST read, gh's GraphQL read, curl and git.
func (r *transportRig) runs(t *testing.T) (rest, graphql, curl, git int) {
	t.Helper()
	return countLines(t, r.reads), countLines(t, r.graphql), countLines(t, r.curl), len(readCalls(t, r.git))
}

// kept is whether the guard kept an answer for theRepo.
func (r *transportRig) kept(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(r.guard.visibilityCachePath(theRepo))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// When gh's REST read fails with `net/http: TLS handshake timeout`, the guard makes the same REST
// read through curl, with the token `gh auth git-credential get` answers for the repository, and
// makes no GraphQL read, which goes through gh too. curl's public, private and internal are the
// answer, reported as rest-curl, and its public is kept as gh's is. A rate limit that curl meets
// goes on to the rate-limit reads (here GraphQL answers). A curl that fails on its connection too
// sends the read on to ls-remote, whose refs read public and whose `could not read Username` at
// exit 128 refuses the call. Any other curl answer refuses the call with no ls-remote.
func TestVisibilityAfterAGhTransportFailureIsReadThroughCurl(t *testing.T) {
	for _, tc := range []struct {
		name    string
		graphql func(record string) string
		curl    string
		git     string
		// want is the answer, empty where the call is refused, and source where it came from;
		// fails is what a refusal says; graphqlRuns and gitRuns how often each read ran.
		want                 Visibility
		source               VisibilitySource
		fails                []string
		graphqlRuns, gitRuns int
	}{
		{name: "curl answers private", graphql: noGraphQL, curl: curlAnswers("private"), git: answersTheRefs, want: Private, source: VisibilityFromCurl},
		{name: "curl answers public", graphql: noGraphQL, curl: curlAnswers("public"), git: answersTheRefs, want: Public, source: VisibilityFromCurl},
		{name: "curl answers internal", graphql: noGraphQL, curl: curlAnswers("internal"), git: answersTheRefs, want: Internal, source: VisibilityFromCurl},
		{
			name:        "curl meets the spent core limit",
			graphql:     func(r string) string { return graphQLAnswers(r, "PRIVATE") },
			curl:        curlRateLimited,
			git:         answersTheRefs,
			want:        Private,
			source:      VisibilityFromGraphQL,
			graphqlRuns: 1,
		},
		{
			name:    "curl cannot connect and ls-remote lists the refs",
			graphql: noGraphQL,
			curl:    curlTimesOut,
			git:     answersTheRefs,
			want:    Public,
			source:  VisibilityFromLsRemote,
			gitRuns: 1,
		},
		{
			name:    "curl cannot connect and GitHub asks ls-remote for a credential",
			graphql: noGraphQL,
			curl:    curlTimesOut,
			git:     asksForACredential,
			fails: []string{
				`gh could not reach GitHub: Get "https://api.github.com/repos/` + maskPath(theRepo) + `": net/http: TLS handshake timeout`,
				"curl could not reach GitHub: exit 28: curl: (28) SSL connection timeout",
				"git ls-remote could not answer",
				"could not read Username",
			},
			gitRuns: 1,
		},
		{
			name:    "curl cannot connect and ls-remote fails another way",
			graphql: noGraphQL,
			curl:    curlTimesOut,
			git:     cannotConnect,
			fails:   []string{"git ls-remote could not answer", "Could not resolve host"},
			gitRuns: 1,
		},
		{
			name:    "GitHub answers curl 404",
			graphql: noGraphQL,
			curl:    curlNotFound,
			git:     answersTheRefs,
			fails:   []string{"gh could not reach GitHub", "curl could not answer", "answered the status 404", "Not Found"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, ghTLSTimeout, tc.graphql, tc.curl, tc.git)
			visibility, err := rig.guard.Visibility(bound(t), theRepo)
			rest, graphql, curl, git := rig.runs(t)
			if rest != 1 || curl != 1 || graphql != tc.graphqlRuns || git != tc.gitRuns {
				t.Errorf("gh's REST read ran %d times, curl %d, GraphQL %d and git %d; want 1, 1, %d and %d",
					rest, curl, graphql, git, tc.graphqlRuns, tc.gitRuns)
			}
			if request, err := os.ReadFile(rig.credential); err != nil || string(request) != "protocol=https\nhost=github.com\npath="+theRepo+".git\n\n" {
				t.Errorf("gh auth git-credential get was asked %q (%v), want the repository's URL", request, err)
			}
			if len(tc.fails) > 0 {
				if err == nil {
					t.Fatalf("read %q, want the call refused", visibility)
				}
				for _, want := range tc.fails {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the failure %q does not say %q", err, want)
					}
				}
				if strings.Contains(err.Error(), theToken) {
					t.Errorf("the failure %q holds the token", err)
				}
				if len(rig.sources) != 0 || rig.kept(t) {
					t.Errorf("the guard reported the sources %q and kept an answer: %v; want neither for a refused call", rig.sources, rig.kept(t))
				}
				return
			}
			if err != nil || visibility != tc.want {
				t.Fatalf("read %q, %v; want %s", visibility, err, tc.want)
			}
			if want := []VisibilitySource{tc.source}; !slices.Equal(rig.sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", rig.sources, want)
			}
			if kept := rig.kept(t); kept != (tc.want == Public) {
				t.Errorf("an answer was kept: %v; want one for public alone", kept)
			}
			if tc.want != Public {
				return
			}
			// A kept public answers the next call, which then needs no gh.
			if visibility, err := rig.guard.Visibility(bound(t), theRepo); err != nil || visibility != Public {
				t.Fatalf("the second call read %q, %v; want public", visibility, err)
			}
			if rest, _, curl, git := rig.runs(t); rest != 1 || curl != 1 || git != tc.gitRuns {
				t.Errorf("after the second call gh's REST read ran %d times, curl %d and git %d; want 1, 1 and %d", rest, curl, git, tc.gitRuns)
			}
			if want := []VisibilitySource{tc.source, VisibilityFromCache}; !slices.Equal(rig.sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", rig.sources, want)
			}
		})
	}
}

// Each of gh's transport failures (ghTransportFailures), in the line gh prints for it, sends the
// read on to curl. Every other failure of gh's REST read is refused as before, and runs neither
// curl, nor GraphQL, nor git: a GitHub error body, a 403 that is not a rate limit, a malformed
// answer, a refusal by `legion gh`, a failed DNS lookup, and a transport failure's words beside a
// status line, beside another line, for another repository's URL or at exit 0.
func TestOnlyATransportFailureOfGhIsReadThroughCurl(t *testing.T) {
	included := func(status string, body string, headers ...string) string {
		lines := append(append([]string{status, "Content-Type: application/json; charset=utf-8"}, headers...), "")
		quoted := make([]string, len(lines))
		for i, line := range lines {
			quoted[i] = shQuote(line)
		}
		return "printf '%s\\r\\n' " + strings.Join(quoted, " ") + "\nprintf '%s\\n' " + shQuote(body) + "\n"
	}
	tlsLine := `Get "https://api.github.com/repos/` + theRepo + `": net/http: TLS handshake timeout`
	for _, tc := range []struct {
		name string
		rest string
		// transport is whether the read goes on to curl; fails what a refusal says.
		transport bool
		fails     string
	}{
		{name: "a TLS handshake timeout", rest: ghTLSTimeout, transport: true},
		{name: "a dial timeout", rest: ghFailsOn("dial tcp 140.82.112.5:443: i/o timeout"), transport: true},
		{name: "a connection reset", rest: ghFailsOn("read tcp 10.42.0.17:51234->140.82.112.5:443: read: connection reset by peer"), transport: true},
		{name: "a proxy that refuses the connection", rest: ghFailsOn("proxyconnect tcp: dial tcp 127.0.0.1:3128: connect: connection refused"), transport: true},
		{name: "the HTTP client's own timeout", rest: ghFailsOn("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), transport: true},
		{
			name: "a GitHub error body",
			rest: included("HTTP/2.0 404 Not Found", `{"message":"Not Found","status":"404"}`, "X-Ratelimit-Remaining: 4999") +
				printsToStderr("gh: Not Found (HTTP 404)") + "exit 1\n",
			fails: "exited 1",
		},
		{
			name: "a 403 that is not a rate limit",
			rest: included("HTTP/2.0 403 Forbidden", `{"message":"Resource not accessible by integration","status":"403"}`, "X-Ratelimit-Remaining: 4000") +
				printsToStderr("gh: Resource not accessible by integration (HTTP 403)") + "exit 1\n",
			fails: "Resource not accessible by integration",
		},
		{name: "a malformed answer", rest: included("HTTP/2.0 200 OK", "banana") + "exit 0\n", fails: "answered no visibility GitHub reports"},
		{
			name:  "a refusal by legion gh",
			rest:  printsToStderr(`legion gh: "invented-org" is not an owner of this project: /invented holds no gh directory for it`) + "exit 2\n",
			fails: "is not an owner of this project",
		},
		{
			name:  "a failed DNS lookup",
			rest:  printsToStderr("error connecting to api.github.com", "check your internet connection or https://githubstatus.com") + "exit 1\n",
			fails: "error connecting to api.github.com",
		},
		{
			name:  "a TLS handshake timeout beside a status line",
			rest:  included("HTTP/2.0 502 Bad Gateway", `{"message":"Server Error"}`) + printsToStderr(tlsLine) + "exit 1\n",
			fails: "TLS handshake timeout",
		},
		{name: "a TLS handshake timeout beside another line", rest: printsToStderr("warning: invented", tlsLine) + "exit 1\n", fails: "TLS handshake timeout"},
		{
			name:  "a TLS handshake timeout for another repository",
			rest:  printsToStderr(`Get "https://api.github.com/repos/other-org/other-repo": net/http: TLS handshake timeout`) + "exit 1\n",
			fails: "TLS handshake timeout",
		},
		{name: "a TLS handshake timeout at exit 0", rest: printsToStderr(tlsLine) + "exit 0\n", fails: "answered no visibility GitHub reports"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, tc.rest, noGraphQL, curlAnswers("private"), answersTheRefs)
			visibility, err := rig.guard.Visibility(bound(t), theRepo)
			rest, graphql, curl, git := rig.runs(t)
			if tc.transport {
				if err != nil || visibility != Private {
					t.Fatalf("read %q, %v; want curl's private", visibility, err)
				}
				if rest != 1 || curl != 1 || graphql != 0 || git != 0 {
					t.Errorf("gh's REST read ran %d times, curl %d, GraphQL %d and git %d; want 1, 1, 0 and 0", rest, curl, graphql, git)
				}
				return
			}
			if err == nil {
				t.Fatalf("read %q, want the call refused", visibility)
			}
			if !strings.Contains(err.Error(), tc.fails) {
				t.Errorf("the failure %q does not say %q", err, tc.fails)
			}
			if strings.Contains(err.Error(), "curl") {
				t.Errorf("the failure %q names curl, want gh's own refusal", err)
			}
			if rest != 1 || curl != 0 || graphql != 0 || git != 0 {
				t.Errorf("gh's REST read ran %d times, curl %d, GraphQL %d and git %d; want 1, 0, 0 and 0", rest, curl, graphql, git)
			}
		})
	}
}

// The curl read carries the token on its standard input, as the one header `--header @-` reads,
// and never in its argv or its environment. The token is the caller's GH_TOKEN, else its
// GITHUB_TOKEN, which gh would have sent, and else what `gh auth git-credential get` answers, which
// is asked only then. curl reads no .curlrc (-q first) and no netrc, takes https alone, and asks
// GitHub's REST API for the repository with GitHub's media type.
func TestCurlCarriesTheTokenOnItsStandardInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		// token is the token curl must send; asked whether gh auth git-credential get is asked.
		token string
		asked bool
	}{
		{name: "the token gh auth git-credential get answers", token: theToken, asked: true},
		{name: "the caller's GH_TOKEN", extra: []string{"GH_TOKEN=caller-not-a-secret", "GITHUB_TOKEN=other-not-a-secret"}, token: "caller-not-a-secret"},
		{name: "the caller's GITHUB_TOKEN", extra: []string{"GITHUB_TOKEN=other-not-a-secret"}, token: "other-not-a-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, ghTLSTimeout, noGraphQL, curlAnswers("private"), answersTheRefs, tc.extra...)
			if visibility, err := rig.guard.Visibility(bound(t), theRepo); err != nil || visibility != Private {
				t.Fatalf("read %q, %v; want curl's private", visibility, err)
			}
			if _, err := os.Stat(rig.credential); (err == nil) != tc.asked {
				t.Errorf("gh auth git-credential get was asked: %v; want %v", err == nil, tc.asked)
			}
			calls := readCalls(t, rig.curl)
			if len(calls) != 1 {
				t.Fatalf("curl ran %d times, want once", len(calls))
			}
			argv := calls[0]["argv"]
			if strings.Contains(argv, tc.token) || strings.Contains(argv, "Authorization") {
				t.Errorf("curl's argv %q holds the token", argv)
			}
			if !strings.HasPrefix(argv, "-q --no-netrc ") {
				t.Errorf("curl ran as %q, want -q first and then --no-netrc", argv)
			}
			for _, want := range []string{
				" --include --suppress-connect-headers ", " --proto =https ", " --max-time ", " --header @- ",
				" --header Accept: application/vnd.github+json ", " https://api.github.com/repos/" + theRepo,
			} {
				if !strings.Contains(" "+argv, want) {
					t.Errorf("curl ran as %q, want %q in it", argv, want)
				}
			}
			for _, key := range []string{"ghtoken", "githubtoken"} {
				if got := calls[0][key]; got != "unset" {
					t.Errorf("curl ran with %s=%q, want it unset", key, got)
				}
			}
			if stdin, err := os.ReadFile(rig.curlStdin); err != nil || string(stdin) != "Authorization: Bearer "+tc.token+"\n" {
				t.Errorf("curl's standard input held %q (%v), want the token's one header", stdin, err)
			}
		})
	}
}

// With no curl on PATH, the read after gh's transport failure goes on to ls-remote, whose refs read
// public. PATH holds the stand-ins alone, so the test's own curl cannot answer.
func TestTransportFailureWithNoCurlIsReadWithLsRemote(t *testing.T) {
	bin, git := t.TempDir(), filepath.Join(t.TempDir(), "git")
	writeProgram(t, bin, "gh", ghTLSTimeout)
	writeProgram(t, bin, "git", "echo ran >> "+shQuote(git)+"\n"+answersTheRefs)
	var sources []VisibilitySource
	visibility, err := New(Options{
		OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
		Env:          append(fixtureenv.Isolated(t.TempDir()), "PATH="+bin, "XDG_STATE_HOME="+t.TempDir()),
	}).Visibility(bound(t), theRepo)
	if err != nil || visibility != Public {
		t.Fatalf("read %q, %v; want public", visibility, err)
	}
	if got := countLines(t, git); got != 1 {
		t.Errorf("git ran %d times, want once", got)
	}
	if want := []VisibilitySource{VisibilityFromLsRemote}; !slices.Equal(sources, want) {
		t.Errorf("the guard reported the sources %q, want %q", sources, want)
	}
}

// A gh REST read that the caller's own bound stops is no transport failure: the call is refused
// at once, naming gh's stop, and neither curl nor git runs, since no bound is left for them.
func TestTransportPathLeavesTheCallersStopAnError(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	rig := newTransportRig(t, ": > "+shQuote(started)+"\nsleep 30\n", noGraphQL, curlAnswers("private"), answersTheRefs)
	ctx, cancel := context.WithCancel(bound(t))
	t.Cleanup(cancel)
	whenWritten(ctx, started, cancel)
	began := time.Now()
	visibility, err := rig.guard.Visibility(ctx, theRepo)
	answeredWithin10s(t, began)
	if err == nil {
		t.Fatalf("read %q, want the call refused", visibility)
	}
	var stopped *stoppedError
	if !errors.As(err, &stopped) {
		t.Errorf("the failure %q is no stop of gh, want gh's stop", err)
	}
	if _, _, curl, git := rig.runs(t); curl != 0 || git != 0 {
		t.Errorf("curl ran %d times and git %d, want neither", curl, git)
	}
}

// ghTransportFailure names the failure with the repository's name masked, and reads nothing else
// as one.
func TestTransportFailureSaysGhsLine(t *testing.T) {
	line := `Get "https://api.github.com/repos/` + theRepo + `": net/http: TLS handshake timeout`
	unreached := ghTransportFailure(1, line+"\n", theRepo)
	if unreached == nil {
		t.Fatal("gh's TLS handshake timeout is no transport failure")
	}
	if want := fmt.Sprintf("gh could not reach GitHub: Get %q: net/http: TLS handshake timeout", "https://api.github.com/repos/"+maskPath(theRepo)); unreached.Error() != want {
		t.Errorf("the failure says %q, want %q", unreached, want)
	}
}
