package leakguard

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// echoedToken is the GitHub token the visibility reads of this file carry, which whatever answers
// them in GitHub's place echoes back: gh's REST and GraphQL reads carry it in GH_TOKEN, and the
// curl read sends it on curl's standard input. It is a token in GitHub's shape with the gate's
// marker in it, so the text a row captures shows the gate where an echo reached a refusal.
var echoedToken = githubtest.InstallationToken("leakguard visibility words") + "-RDX-"

// echoedAnswer is a GraphQL or REST answer of a stand-in that wrote the read's token into a
// member of its own, beside the answer GitHub would give for theRepo.
var echoedAnswer = `{"data":{"repository":{"nameWithOwner":"` + theRepo + `","visibility":"PUBLIC"}},"echoed":"` + echoedToken + `"}`

// included200 is body as `gh api --include` and `curl --include` print a 200 carrying it.
func included200(body string) string {
	return "HTTP/2.0 200 OK\r\nContent-Type: application/json; charset=utf-8\r\n\r\n" + body
}

// included500 is body as they print a 500 carrying it.
func included500(body string) string {
	return "HTTP/2.0 500 Internal Server Error\r\nContent-Type: application/json; charset=utf-8\r\n\r\n" + body
}

// printsAndExits is a stand-in program that prints out on its standard output, says stderr on its
// standard error where there is one, and exits exit.
func printsAndExits(out, stderr string, exit int) string {
	script := "printf '%s' " + shQuote(out) + "\n"
	if stderr != "" {
		script += printsToStderr(stderr)
	}
	return script + fmt.Sprintf("exit %d\n", exit)
}

// curlPrintsAndExits is printsAndExits for the stand-in curl, which reads the header on its
// standard input first, as the guard's curl is given the token there.
func curlPrintsAndExits(out, stderr string, exit int) string {
	return "cat > /dev/null\n" + printsAndExits(out, stderr, exit)
}

// wordsGuard is a Guard whose gh and curl are the stand-ins given, with echoedToken as its
// GH_TOKEN, which is the token gh's reads carry and the one the curl read is handed
// (Guard.ghToken).
func wordsGuard(t *testing.T, gh, curl string) *Guard {
	t.Helper()
	bin := t.TempDir()
	writeProgram(t, bin, "gh", gh)
	writeProgram(t, bin, "curl", curl)
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), pathWith(bin),
		"XDG_STATE_HOME="+t.TempDir(), "GH_TOKEN="+echoedToken)})
	guard.curl = filepath.Join(bin, "curl")
	return guard
}

// TestTheVisibilityReadsPrintNoRunOfTheTokenTheyCarried: whatever answers a visibility read in
// GitHub's place holds the token the read carried, and writes it back into the body it answers or
// into what gh or curl prints on its standard error. Every refusal these reads make quotes those
// words (said, shownSaid), and the pod's start, the claim's cause and `legion claims list` print
// the refusal, so none may hold a run of the token.
//
// A row per print: the five of the GraphQL read (graphQLAnswer), its quote of the errors GitHub
// named, of the repository GraphQL answered for and of the visibility it gave, the REST read's own
// exit (Guard.ghVisibility), the three of the curl read (curlAnswer) and the curl answer a caller
// of its own reads (CurlResponse.Unreached). The twin of each shows the stand-in's echo is in the
// words that print quotes, so a print that quoted them as they came would hold the token.
// Captures leakguard.visibility-words/<case>.
func TestTheVisibilityReadsPrintNoRunOfTheTokenTheyCarried(t *testing.T) {
	const failedCurl = "exit 1\n"
	// errorsBody, namedBody and visibilityBody are GraphQL answers whose words GitHub itself
	// would write and a stand-in echoes the token into: an error's message, the repository's
	// nameWithOwner and its visibility.
	errorsBody := `{"errors":[{"type":"NOT_FOUND","message":"Could not resolve a repository for ` + echoedToken + `"}]}`
	namedBody := `{"data":{"repository":{"nameWithOwner":"` + echoedToken + `/widgets","visibility":"PUBLIC"}}}`
	echoedVisibility := `{"data":{"repository":{"nameWithOwner":"` + theRepo + `","visibility":"` + echoedToken + `"}}}`
	for _, c := range []struct {
		name string
		// said is what the stand-in printed that the refusal quotes, which the twin reads.
		said string
		// read runs the one read whose print the row is of.
		read func(t *testing.T) error
		// says is what the refusal names beside those words, so no row passes on a refusal
		// made elsewhere.
		says string
	}{
		{
			name: "graphql-not-whole", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included200(echoedAnswer+echoedAnswer), "", 1)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "without one whole GraphQL answer",
		},
		{
			name: "graphql-no-status-line", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(echoedAnswer, "", 0)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "printed no status line this reads",
		},
		{
			name: "graphql-status", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included500(echoedAnswer), "", 0)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "answered the status 500",
		},
		{
			name: "graphql-exit", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included200(echoedAnswer), "", 1)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "exited 1",
		},
		{
			name: "graphql-no-repository", said: echoedToken,
			read: func(t *testing.T) error {
				body := `{"data":{"repository":null},"echoed":"` + echoedToken + `"}`
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included200(body), "", 0)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "GraphQL answered no repository",
		},
		{
			name: "graphql-errors", said: errorsBody,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included200(errorsBody), "", 0)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "NOT_FOUND",
		},
		{
			name: "graphql-repository-name", said: namedBody,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included200(namedBody), "", 0)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "GraphQL answered for the repository",
		},
		{
			name: "graphql-visibility", said: echoedVisibility,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(failedCurl, printsAndExits(included200(echoedVisibility), "", 0)), failedCurl)
				_, err := guard.graphQLVisibility(bound(t), theRepo)
				return err
			},
			says: "which is none GitHub defines",
		},
		{
			name: "rest-exit", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, ghStandIn(printsAndExits(included200(echoedAnswer), "", 1), failedCurl), failedCurl)
				_, err := guard.ghVisibility(bound(t), theRepo)
				return err
			},
			says: "gh api repos/",
		},
		{
			name: "curl-unreached", said: "curl: (7) could not connect, sent " + echoedToken,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, failedCurl, curlPrintsAndExits("", "curl: (7) could not connect, sent "+echoedToken, 7))
				_, err := guard.curlVisibility(bound(t), theRepo)
				return err
			},
			says: "curl could not reach GitHub",
		},
		{
			name: "curl-exit", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, failedCurl, curlPrintsAndExits(included500(echoedAnswer), "", 1))
				_, err := guard.curlVisibility(bound(t), theRepo)
				return err
			},
			says: "exited 1",
		},
		{
			name: "curl-status", said: echoedAnswer,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, failedCurl, curlPrintsAndExits(included500(echoedAnswer), "", 0))
				_, err := guard.curlVisibility(bound(t), theRepo)
				return err
			},
			says: "answered the status 500",
		},
		{
			name: "curl-caller-unreached", said: "curl: (35) the handshake failed after " + echoedToken,
			read: func(t *testing.T) error {
				guard := wordsGuard(t, failedCurl, curlPrintsAndExits("", "curl: (35) the handshake failed after "+echoedToken, 35))
				response, err := guard.CurlGet(bound(t), "repos/"+theRepo, echoedToken)
				if err != nil {
					return err
				}
				return response.Unreached()
			},
			says: "curl could not reach GitHub",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Run("twin", func(t *testing.T) {
				if !strings.Contains(c.said, echoedToken) {
					t.Fatalf("the stand-in's words %q hold no echo of the read's token, so the row cannot see a leak", c.said)
				}
			})
			err := c.read(t)
			if err == nil {
				t.Fatalf("the read answered, want it refused over %q", c.said)
			}
			said := err.Error()
			oauth2calltest.RequireNoPiece(t, "leakguard.visibility-words/"+c.name, said, echoedToken)
			if !strings.Contains(said, c.says) {
				t.Errorf("the refusal is %q, want it to say %q", said, c.says)
			}
		})
	}
}
