package leakguard

import (
	"context"
	"errors"
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
// credential and answers password, as `legion gh` answers from the bundle's hosts.yml.
func transportGh(reads, rest, graphql, credential, password string) string {
	return "if [ \"$1\" = auth ]; then\n" +
		"cat > " + shQuote(credential) + "\n" +
		"printf 'protocol=https\\nhost=github.com\\nusername=x-access-token\\npassword=%s\\n' " + shQuote(password) + "\nexit 0\nfi\n" +
		ghStandIn("echo read >> "+shQuote(reads)+"\n"+rest, graphql)
}

// noGraphQL is a GraphQL read that only records that it ran, to record, and fails.
func noGraphQL(record string) string {
	return "echo ran >> " + shQuote(record) + "\nexit 1\n"
}

// curlStandIn is a stand-in curl. It appends a line to record with its arguments and with the
// GH_TOKEN, GITHUB_TOKEN and SSLKEYLOGFILE it was given (readCalls reads it), writes its standard
// input to stdin, and then does what then says.
func curlStandIn(record, stdin, then string) string {
	return `printf 'argv=%s\tghtoken=%s\tgithubtoken=%s\tsslkeylog=%s\thttpsproxy=%s\tallproxy=%s\tcertfile=%s\tcertdir=%s\tcabundle=%s\n' "$*" "${GH_TOKEN-unset}" "${GITHUB_TOKEN-unset}" "${SSLKEYLOGFILE-unset}" ` +
		`"${HTTPS_PROXY-unset}" "${ALL_PROXY-unset}" "${SSL_CERT_FILE-unset}" "${SSL_CERT_DIR-unset}" "${CURL_CA_BUNDLE-unset}" >> ` +
		shQuote(record) + "\n" + "cat > " + shQuote(stdin) + "\n" + then
}

// curlPrints is a curl that prints a response as `curl --include` prints one (printsResponse) and
// exits 0, which curl does whatever the status.
func curlPrints(status string, headers []string, body string) string {
	return printsResponse(status, headers, body) + "exit 0\n"
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

// curlHeardThenFailed is a curl that heard GitHub's status line and headers and then lost the
// connection while it received the body: exit 56, which is also an exit of a curl that heard
// nothing (curlTransportExits).
var curlHeardThenFailed = printsResponse("HTTP/2 200", []string{"content-type: application/json; charset=utf-8"}, `{"full_name":"`) +
	printsToStderr("curl: (56) Recv failure: Connection reset by peer") + "exit 56\n"

// transportRig is one guard whose gh, curl and git are stand-ins, the directory they are in, the
// files they record to, and the sources the guard reported.
type transportRig struct {
	guard                           *Guard
	sources                         []VisibilitySource
	bin                             string
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
		bin:        bin,
		reads:      filepath.Join(records, "reads"),
		graphql:    filepath.Join(records, "graphql"),
		credential: filepath.Join(records, "credential"),
		git:        filepath.Join(records, "git"),
		curl:       filepath.Join(records, "curl"),
		curlStdin:  filepath.Join(records, "curl-stdin"),
	}
	writeProgram(t, bin, "gh", transportGh(rig.reads, rest, graphql(rig.graphql), rig.credential, theToken))
	writeProgram(t, bin, "curl", curlStandIn(rig.curl, rig.curlStdin, curl))
	writeProgram(t, bin, "git", recordsItsCall(rig.git)+git)
	rig.guard = New(Options{
		OnVisibility: func(source VisibilitySource) { rig.sources = append(rig.sources, source) },
		Env:          append(append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()), extra...),
	})
	// The guard's curl is the stand-in, where the guard would otherwise run curlProgram.
	rig.guard.curl = filepath.Join(bin, "curl")
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
// makes no GraphQL read, which goes through gh too, unless curl meets a spent core limit. curl's
// public, private and internal are the answer, reported as rest-curl, and its public is kept as
// gh's is. A rate limit that curl meets goes on to the rate-limit reads, whose first is GraphQL's
// (here it answers). A curl that fails on its connection too sends the read on to ls-remote, whose
// refs read public and whose `could not read Username` at exit 128 refuses the call. Any other curl
// answer refuses the call with no ls-remote: GitHub's 404, which the refusal tells as GitHub's
// answer for the repository, and a curl that heard GitHub's status line and then lost its
// connection among them.
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
				`gh could not reach GitHub: Get "https://api.github.com/repos/` + theRepo + `": net/http: TLS handshake timeout`,
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
			fails:   []string{"gh could not reach GitHub", "curl could not answer", "GitHub answers 404 for " + theRepo},
		},
		{
			name:    "curl hears GitHub and then loses its connection",
			graphql: noGraphQL,
			curl:    curlHeardThenFailed,
			git:     answersTheRefs,
			fails:   []string{"gh could not reach GitHub", "curl could not answer", "exited 56", "Recv failure"},
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
// read on to curl. Every other failure of gh's REST read is refused, and runs neither curl, nor
// GraphQL, nor git: a GitHub error body (a 404, whose refusal tells it as GitHub's answer for the
// repository), a 403 that is not a rate limit, a malformed answer, a refusal by `legion gh`, a
// failed DNS lookup, and a transport failure's words beside a status line, beside another line,
// for another repository's URL or at exit 0.
func TestOnlyATransportFailureOfGhIsReadThroughCurl(t *testing.T) {
	included := func(status string, body string, headers ...string) string {
		return printsResponse(status, append([]string{"Content-Type: application/json; charset=utf-8"}, headers...), body)
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
			fails: "GitHub answers 404 for " + theRepo,
		},
		{
			name: "a 403 that is not a rate limit",
			rest: included("HTTP/2.0 403 Forbidden", `{"message":"Resource not accessible by integration","status":"403"}`, "X-Ratelimit-Remaining: 4000") +
				printsToStderr("gh: Resource not accessible by integration (HTTP 403)") + "exit 1\n",
			fails: "Resource not accessible by integration",
		},
		{name: "a malformed answer", rest: included("HTTP/2.0 200 OK", "banana") + "exit 0\n", fails: "answered no visibility GitHub reports"},
		{
			// `legion gh`'s refusal of an owner the bundle holds no directory for stays a refusal
			// that runs no curl and no git here: the rig's environment (fixtureenv.Isolated) names
			// no LEGION_GH_OWNERS_DIR, so Guard.unservedOwner does not read it as an unserved owner.
			name:  "a refusal by legion gh",
			rest:  printsToStderr(ownerNotServedLine("invented-org", "/invented")) + "exit 2\n",
			fails: (&OwnerNotServedError{Owner: "invented-org", OwnersDir: "/invented"}).Error(),
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
// is asked only then. curl reads no .curlrc (-q first) and no netrc, takes https alone, reads no
// range or set in the URL (--globoff), so one read is one request, and asks GitHub's REST API for
// the repository with GitHub's media type. Of the caller's environment its proxy reaches it, so a
// host that reaches GitHub through one reaches it the same way, and no trust store and no TLS key
// log do: a proxy the caller names with a CA curl trusted would read the token's header, where
// with the system's own CAs it sees only a TLS tunnel.
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
			keys := "SSLKEYLOGFILE=" + filepath.Join(t.TempDir(), "keys")
			mitm := []string{"HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "SSL_CERT_FILE=/dev/null", "SSL_CERT_DIR=/dev/null", "CURL_CA_BUNDLE=/dev/null"}
			rig := newTransportRig(t, ghTLSTimeout, noGraphQL, curlAnswers("private"), answersTheRefs, append(append(tc.extra, keys), mitm...)...)
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
				" --include --suppress-connect-headers ", " --globoff ", " --proto =https ", " --max-time ", " --header @- ",
				" --header Accept: application/vnd.github+json ", " https://api.github.com/repos/" + theRepo,
			} {
				if !strings.Contains(" "+argv, want) {
					t.Errorf("curl ran as %q, want %q in it", argv, want)
				}
			}
			for _, key := range []string{"ghtoken", "githubtoken", "sslkeylog", "certfile", "certdir", "cabundle"} {
				if got := calls[0][key]; got != "unset" {
					t.Errorf("curl ran with %s=%q, want it unset", key, got)
				}
			}
			for _, key := range []string{"httpsproxy", "allproxy"} {
				if got := calls[0][key]; got != "http://127.0.0.1:1" {
					t.Errorf("curl ran with %s=%q, want the caller's proxy, http://127.0.0.1:1", key, got)
				}
			}
			if stdin, err := os.ReadFile(rig.curlStdin); err != nil || string(stdin) != "Authorization: Bearer "+tc.token+"\n" {
				t.Errorf("curl's standard input held %q (%v), want the token's one header", stdin, err)
			}
		})
	}
}

// A token that holds a line break, a carriage return included, is never sent: curl reads the
// header on its standard input a line at a time, so the break would put a header of the token's
// own on the request. The caller's GH_TOKEN or GITHUB_TOKEN, and the password `gh auth
// git-credential get` answers, are each refused, and the read goes on to ls-remote as it does for
// a read with no token. GitHub asks ls-remote for a credential here, so the call is refused, naming
// the line break and never the token.
func TestTransportTokenWithALineBreakIsNeverSent(t *testing.T) {
	const injected = "X-Injected: not-a-secret"
	for _, tc := range []struct {
		name  string
		extra []string
		// password is what gh auth git-credential get answers; fails what the refusal says.
		password, fails string
	}{
		{name: "a newline in GH_TOKEN", extra: []string{"GH_TOKEN=caller-not-a-secret\n" + injected}, password: theToken, fails: "GH_TOKEN holds a line break"},
		{name: "a carriage return in GITHUB_TOKEN", extra: []string{"GITHUB_TOKEN=other-not-a-secret\r" + injected}, password: theToken, fails: "GITHUB_TOKEN holds a line break"},
		{
			name:     "a carriage return in the credential's password",
			password: "invented-not-a-secret\r" + injected,
			fails:    "gh auth git-credential get answered a password that holds a line break",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTransportRig(t, ghTLSTimeout, noGraphQL, curlAnswers("private"), asksForACredential, tc.extra...)
			writeProgram(t, rig.bin, "gh", transportGh(rig.reads, ghTLSTimeout, noGraphQL(rig.graphql), rig.credential, tc.password))
			visibility, err := rig.guard.Visibility(bound(t), theRepo)
			if err == nil {
				t.Fatalf("read %q, want the call refused", visibility)
			}
			for _, want := range []string{tc.fails, "the read through curl could not be made", "git ls-remote could not answer"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), injected) {
				t.Errorf("the failure %q quotes the token", err)
			}
			if _, graphql, curl, git := rig.runs(t); curl != 0 || graphql != 0 || git != 1 {
				t.Errorf("curl ran %d times, GraphQL %d and git %d; want 0, 0 and 1", curl, graphql, git)
			}
		})
	}
}

// With no curl at the guard's curlProgram, the read after gh's transport failure goes on to
// ls-remote, whose refs read public.
func TestTransportFailureWithNoCurlIsReadWithLsRemote(t *testing.T) {
	bin, git := t.TempDir(), filepath.Join(t.TempDir(), "git")
	writeProgram(t, bin, "gh", ghTLSTimeout)
	writeProgram(t, bin, "git", "echo ran >> "+shQuote(git)+"\n"+answersTheRefs)
	var sources []VisibilitySource
	guard := New(Options{
		OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
		Env:          append(fixtureenv.Isolated(t.TempDir()), "PATH="+bin, "XDG_STATE_HOME="+t.TempDir()),
	})
	guard.curl = filepath.Join(t.TempDir(), "curl")
	visibility, err := guard.Visibility(bound(t), theRepo)
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

// The curl read runs the guard's own curl and never the first curl on the caller's PATH, which the
// caller put there and which would be handed the token on its standard input: a curl first on
// PATH, ahead of the stand-in's own directory, never runs, and the guard's curl answers.
func TestCurlReadNeverRunsTheCurlFirstOnPath(t *testing.T) {
	rig := newTransportRig(t, ghTLSTimeout, noGraphQL, curlAnswers("private"), answersTheRefs)
	hostile, record := t.TempDir(), filepath.Join(t.TempDir(), "hostile")
	writeProgram(t, hostile, "curl", "cat > "+shQuote(record)+"\n"+curlAnswers("private"))
	rig.guard.opts.Env = append(fixtureenv.Isolated(t.TempDir()), "PATH="+hostile+":"+rig.bin, "XDG_STATE_HOME="+t.TempDir())
	if visibility, err := rig.guard.Visibility(bound(t), theRepo); err != nil || visibility != Private {
		t.Fatalf("read %q, %v; want the guard's curl's private", visibility, err)
	}
	if got, err := os.ReadFile(record); err == nil {
		t.Errorf("the curl first on PATH ran and was handed %q", got)
	}
	if calls := readCalls(t, rig.curl); len(calls) != 1 {
		t.Errorf("the guard's curl ran %d times, want once", len(calls))
	}
}

// A gh REST read that the caller's own bound stops is no transport failure: the call is refused
// at once, naming gh's stop, and neither curl nor git runs, since no bound is left for them. The
// caller cancels, or its deadline of 2 s, which leaves less than curlReserve and so sets no bound
// of the guard's own, comes first. Each caller's context ends only once the stand-in gh has
// started, so it is a running gh the end stops on a loaded machine too.
func TestTransportPathLeavesTheCallersStopAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		// caller is the caller's context, given the file the stand-in gh writes before it hangs.
		caller func(t *testing.T, started string) context.Context
		// stop checks what the failure says of gh's stop.
		stop func(t *testing.T, err error)
	}{
		{
			name: "the caller cancels",
			caller: func(t *testing.T, started string) context.Context {
				ctx, cancel := context.WithCancel(bound(t))
				t.Cleanup(cancel)
				whenWritten(ctx, started, cancel)
				return ctx
			},
			stop: func(t *testing.T, err error) {
				if want := "and was stopped by a cancel"; !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			},
		},
		{
			name: "the caller's deadline comes first",
			caller: func(t *testing.T, started string) context.Context {
				deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				t.Cleanup(cancel)
				ctx := &deadlineOnceStarted{Context: deadline, done: make(chan struct{})}
				whenWritten(bound(t), started, func() {
					<-deadline.Done()
					close(ctx.done)
				})
				return ctx
			},
			stop: func(t *testing.T, err error) {
				if got := stopBound(t, err); got > 2 {
					t.Errorf("gh was stopped by a bound of %d s, want the caller's deadline of 2 s", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := filepath.Join(t.TempDir(), "started")
			rig := newTransportRig(t, ": > "+shQuote(started)+"\nsleep 30\n", noGraphQL, curlAnswers("private"), answersTheRefs)
			ctx := tc.caller(t, started)
			began := time.Now()
			visibility, err := rig.guard.Visibility(ctx, theRepo)
			answeredWithin10s(t, began)
			if err == nil {
				t.Fatalf("read %q, want the call refused", visibility)
			}
			var stopped *stoppedError
			if !errors.As(err, &stopped) {
				t.Fatalf("the failure %q is no stop of gh, want gh's stop", err)
			}
			tc.stop(t, err)
			if _, _, curl, git := rig.runs(t); curl != 0 || git != 0 {
				t.Errorf("curl ran %d times and git %d, want neither", curl, git)
			}
		})
	}
}

// shorten sets *bound to d for the test, and back after it.
func shorten(t *testing.T, bound *time.Duration, d time.Duration) {
	t.Helper()
	saved := *bound
	*bound = d
	t.Cleanup(func() { *bound = saved })
}

// A gh REST read that hangs, as gh did for about 42 s in the busy one-CPU pod, is stopped by the
// guard's own bound, which leaves the rest of the caller's bound to curl: curl answers private
// within the caller's bound, and gets what is left of it as its --max-time. The guard's bound is
// ghReadBound inside a caller's 15 s (the bound `legion gh` gives the shared check's own read),
// here shortened to 1 s; or the caller's deadline less curlReserve where that is sooner, here a
// deadline of 6 s and a reserve of 3 s.
func TestTransportBoundStopsAHangingGhAndCurlAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		// caller is the caller's bound; ghBound and reserve are ghReadBound and curlReserve for the
		// case; stoppedAt is when the guard's bound stops gh.
		caller, ghBound, reserve, stoppedAt time.Duration
	}{
		{name: "the read's own cap", caller: 15 * time.Second, ghBound: time.Second, reserve: curlReserve, stoppedAt: time.Second},
		{name: "the caller's deadline less the reserve", caller: 6 * time.Second, ghBound: ghReadBound, reserve: 3 * time.Second, stoppedAt: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shorten(t, &ghReadBound, tc.ghBound)
			shorten(t, &curlReserve, tc.reserve)
			rig := newTransportRig(t, "sleep 30\n", noGraphQL, curlAnswers("private"), answersTheRefs)
			ctx, cancel := context.WithTimeout(context.Background(), tc.caller)
			t.Cleanup(cancel)
			began := time.Now()
			visibility, err := rig.guard.Visibility(ctx, theRepo)
			if took := time.Since(began); took >= tc.caller {
				t.Errorf("the call answered after %s, want within the caller's %s", took.Round(time.Millisecond), tc.caller)
			}
			if err != nil || visibility != Private {
				t.Fatalf("read %q, %v; want curl's private", visibility, err)
			}
			if want := []VisibilitySource{VisibilityFromCurl}; !slices.Equal(rig.sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", rig.sources, want)
			}
			if rest, graphql, curl, git := rig.runs(t); rest != 1 || curl != 1 || graphql != 0 || git != 0 {
				t.Errorf("gh's REST read ran %d times, curl %d, GraphQL %d and git %d; want 1, 1, 0 and 0", rest, curl, graphql, git)
			}
			calls := readCalls(t, rig.curl)
			if len(calls) != 1 {
				t.Fatalf("curl ran %d times, want once", len(calls))
			}
			_, after, _ := strings.Cut(calls[0]["argv"], "--max-time ")
			word, _, _ := strings.Cut(after, " ")
			maxTime, err := time.ParseDuration(word + "s")
			if err != nil {
				t.Fatalf("curl ran with --max-time %q: %v", word, err)
			}
			if left := tc.caller - tc.stoppedAt; maxTime < curlLeast || maxTime >= left {
				t.Errorf("curl ran with --max-time %s, want at least %s and less than the %s the caller's bound has left after gh's stop",
					maxTime, curlLeast, left)
			}
		})
	}
}

// A gh REST read that heard GitHub's status line and then hung is no transport failure when the
// guard's own bound stops it: GitHub answered, so the call is refused as gh's stop, and neither
// curl nor git runs. The guard's bound is shortened to 1 s inside the caller's minute.
func TestTransportBoundRefusesAGhThatHeardGitHub(t *testing.T) {
	shorten(t, &ghReadBound, time.Second)
	rig := newTransportRig(t, "printf 'HTTP/2.0 200 OK\\r\\n'\nsleep 30\n", noGraphQL, curlAnswers("private"), answersTheRefs)
	began := time.Now()
	visibility, err := rig.guard.Visibility(bound(t), theRepo)
	answeredWithin10s(t, began)
	if err == nil {
		t.Fatalf("read %q, want the call refused", visibility)
	}
	if got := stopBound(t, err); got > 1 {
		t.Errorf("gh was stopped by a bound of %d s, want the guard's own 1 s", got)
	}
	if _, graphql, curl, git := rig.runs(t); curl != 0 || graphql != 0 || git != 0 {
		t.Errorf("curl ran %d times, GraphQL %d and git %d, want none", curl, graphql, git)
	}
}

// Where less than curlLeast of the caller's bound is left after gh's transport failure, the call
// is refused as it was before the reads after gh existed, naming gh's failure, and neither the
// token read, nor curl, nor git runs. The caller's deadline is 1 s away, and its context does not
// end before the guard answers, so the refusal is the guard's own and not a stop.
func TestTransportFailureWithNoTimeLeftForCurlRefuses(t *testing.T) {
	rig := newTransportRig(t, ghTLSTimeout, noGraphQL, curlAnswers("private"), answersTheRefs)
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	visibility, err := rig.guard.Visibility(&deadlineOnceStarted{Context: deadline, done: make(chan struct{})}, theRepo)
	if err == nil {
		t.Fatalf("read %q, want the call refused", visibility)
	}
	for _, want := range []string{"gh could not reach GitHub", "TLS handshake timeout", "too little of the caller's bound is left for the read through curl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not say %q", err, want)
		}
	}
	if _, graphql, curl, git := rig.runs(t); curl != 0 || graphql != 0 || git != 0 {
		t.Errorf("curl ran %d times, GraphQL %d and git %d, want none", curl, graphql, git)
	}
	if _, err := os.Stat(rig.credential); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gh auth git-credential get was asked (%v), want no token read with no time left for curl", err)
	}
}

// ghTransportFailure names the failure in gh's own line, with the repository as it was written.
func TestTransportFailureSaysGhsLine(t *testing.T) {
	line := `Get "https://api.github.com/repos/` + theRepo + `": net/http: TLS handshake timeout`
	unreached := ghTransportFailure(1, line+"\n", theRepo)
	if unreached == nil {
		t.Fatal("gh's TLS handshake timeout is no transport failure")
	}
	if want := "gh could not reach GitHub: " + line; unreached.Error() != want {
		t.Errorf("the failure says %q, want %q", unreached, want)
	}
}
