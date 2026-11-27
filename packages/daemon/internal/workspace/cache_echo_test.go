package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// echoedCacheToken is the bearer of the cases whose stand-in at the repository cache's address
// writes it back into what it streams. It holds oauth2calltest.Token, the made-up credential the
// check's markers name, so what the fill leaves of it shows in the output a case captures.
const echoedCacheToken = "cache-" + oauth2calltest.Token + "-7f3a9c2e"

// echoedFetchToken is the provisioning token of the same cases, which a ref's name can hold too.
// It holds the check's marker -RDX- and shares no run of oauth2calltest.TokenPiece characters with
// echoedCacheToken, so what the fill leaves of either token shows as that token's.
const echoedFetchToken = "ghs_pv7q2-RDX-d58e1b"

// requireNoFetchEcho is oauth2calltest.RequireNoPiece of text under capture for both tokens of
// these cases, and the same of text percent-decoded (unescaped) under capture/decoded.
func requireNoFetchEcho(t *testing.T, capture, text string) {
	t.Helper()
	oauth2calltest.RequireNoPiece(t, capture, text, echoedCacheToken, echoedFetchToken)
	oauth2calltest.RequireNoPiece(t, capture+"/decoded", unescaped(text), echoedCacheToken, echoedFetchToken)
}

// A stand-in at the repository cache's address holds the bearer the /tar request carried, and can
// write it into what it streams: the name of a tar entry, which unpackEntry refuses naming it -
// one that leaves the clone's git directory, one that is neither a regular file nor a directory,
// one that is no ref or pack (the Authorization header whole, or the token alone) - or fails to
// write naming it - one whose body is cut short, one a directory of its name stands in place of -
// and a ref, which fsck names when the history is not whole. It can write the token as it is or
// percent-encoded, every byte a %XX escape (the -encoded cases), which a reading of the text as it
// is finds no run of. A ref fsck names can hold the issue pod's provisioning token too, which the
// stream's filter holds beside the bearer (CacheStream.Held): fsck-encoded's refs hold both
// tokens, the provisioning token in lower-case escapes. The issue pod's fill (ensureRepoClone)
// fails holding no run of either token in its error, as written or percent-decoded. The branch
// GitHub's HEAD names, which the repository cache's own fetch logs when HEAD follows it, can hold
// the provisioning token percent-encoded (log-encoded), and Fetch's log holds no run of it. The
// controls: each still names what failed, with each token's place redacted. Captures
// workspace.cache-echo/<case>.
func TestACacheThatEchoesItsTokenIntoWhatItStreamsPrintsNoneOfIt(t *testing.T) {
	const ref = "refs/heads/" + echoedCacheToken
	every := func(int) bool { return true }
	encoded, encodedFetch := percentEncoded(echoedCacheToken, every, true), percentEncoded(echoedFetchToken, every, false)
	encodedRef, encodedFetchRef := "refs/heads/"+encoded, "refs/heads/"+encodedFetch
	// fsckBody streams HEAD naming main and each ref named, pointing at an object nobody streamed.
	fsckBody := func(named ...string) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			entries := [][2]string{{"HEAD", "ref: refs/heads/main\n"}}
			for _, name := range named {
				entries = append(entries, [2]string{name, "0123456789abcdef0123456789abcdef01234567\n"})
			}
			return tarOf(t, entries...)
		}
	}
	// fsckNames is how many refs fsck's failure names, each on the line fsckLine, for each case
	// that reaches fsck.
	const fsckLine = "error: refs/heads/***: invalid sha1 pointer"
	fsckNames := map[string]int{"fsck": 1, "fsck-encoded": 2}
	for _, tc := range []struct {
		name string
		body func(t *testing.T) []byte
		says string
	}{
		{"outside", func(t *testing.T) []byte { return tarOf(t, [2]string{"../" + echoedCacheToken, "escaped\n"}) },
			`the repository cache's tar holds "../***", which is not a path inside the clone's git directory`},
		{"link", func(t *testing.T) []byte { return tarOf(t, [2]string{ref + "@"}) },
			`the repository cache's tar holds "refs/heads/***", which is neither a regular file nor a directory`},
		{"bearer", func(t *testing.T) []byte { return tarOf(t, [2]string{"Bearer " + echoedCacheToken, "x"}) },
			`the repository cache's tar holds "Bearer ***", which is not one of a repository's refs or packs`},
		{"bare", func(t *testing.T) []byte { return tarOf(t, [2]string{echoedCacheToken, "x"}) },
			`the repository cache's tar holds "***", which is not one of a repository's refs or packs`},
		{"bare-encoded", func(t *testing.T) []byte { return tarOf(t, [2]string{encoded, "x"}) },
			`the repository cache's tar holds "***", which is not one of a repository's refs or packs`},
		{"cut", func(t *testing.T) []byte {
			// One header block and the first 100 bytes of the entry's 1 KiB body.
			return tarOf(t, [2]string{ref, strings.Repeat("x", 1024)})[:512+100]
		}, "read refs/heads/*** from the repository cache's tar: unexpected EOF"},
		{"cut-encoded", func(t *testing.T) []byte {
			return tarOf(t, [2]string{encodedRef, strings.Repeat("x", 1024)})[:512+100]
		}, "read refs/heads/*** from the repository cache's tar: unexpected EOF"},
		{"directory", func(t *testing.T) []byte { return tarOf(t, [2]string{ref + "/"}, [2]string{ref, "x"}) },
			"write refs/heads/*** from the repository cache's tar: open: is a directory"},
		{"fsck", fsckBody(ref), "the repository cache's history is not whole: command failed"},
		{"fsck-encoded", fsckBody(encodedRef, encodedFetchRef), "the repository cache's history is not whole: command failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "main", "history.txt")
			url := (&fakeCache{body: tc.body(t), token: echoedCacheToken}).serve(t)
			clock := newTestClock()
			_, err := fillIssueClone(t, run, clock, issuePodRemoteHolding(t, staticToken(echoedFetchToken), url, echoedCacheToken, clock, func(string) {}))
			said := ""
			if err != nil {
				said = err.Error()
			}
			requireNoFetchEcho(t, "workspace.cache-echo/"+tc.name, said)
			if !strings.Contains(said, tc.says) {
				t.Errorf("the fill = %v, want the cache refused naming %q", err, tc.says)
			}
			if names, ok := fsckNames[tc.name]; ok && strings.Count(said, fsckLine) != names {
				t.Errorf("the fill = %v, want fsck's line naming each of its %d refs with the token's place redacted (%q)", err, names, fsckLine)
			}
		})
	}
	t.Run("log-encoded", func(t *testing.T) {
		run := newLocalRunner(t)
		pushRemoteBranch(t, run.remote, "main", "history.txt")
		req := fetchRequest(t)
		req.Token = staticToken(echoedFetchToken)
		if err := Fetch(context.Background(), run, req); err != nil {
			t.Fatalf("Fetch filling the cache: %v", err)
		}
		// The branch GitHub's HEAD names once the cache is made, which HEAD follows.
		head := "refs/heads/default/" + encodedFetch
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", head, "refs/heads/main")
		runSetup(t, run.remote, "git", "--git-dir="+run.remote, "symbolic-ref", "HEAD", head)
		var logged []string
		req.Log = func(line string) { logged = append(logged, line) }
		if err := Fetch(context.Background(), run, req); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		said := strings.Join(logged, "\n")
		requireNoFetchEcho(t, "workspace.cache-echo/log-encoded", said)
		if line := "the default branch of acme/widgets is now default/*** (HEAD named refs/heads/main)"; strings.Count(said, line) != 1 {
			t.Errorf("Fetch logged %q, want one line naming the branch with the token's place redacted (%q)", logged, line)
		}
	})
}
