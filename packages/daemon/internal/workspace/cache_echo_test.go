package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// echoedCacheToken is the bearer of the cases whose stand-in at the repository cache's address
// writes it back into what it streams. It holds oauth2calltest.Token, the made-up credential the
// check's markers name, so what Fetch leaves of it shows in the output a case captures.
const echoedCacheToken = "cache-" + oauth2calltest.Token + "-7f3a9c2e"

// requireNoCacheEcho is oauth2calltest.RequireNoPiece of text under capture for echoedCacheToken,
// and the same of text percent-decoded (unescaped) under capture/decoded.
func requireNoCacheEcho(t *testing.T, capture, text string) {
	t.Helper()
	oauth2calltest.RequireNoPiece(t, capture, text, echoedCacheToken)
	oauth2calltest.RequireNoPiece(t, capture+"/decoded", unescaped(text), echoedCacheToken)
}

// A stand-in at the repository cache's address holds the bearer the /tar request carried, and can
// write it into what it streams: the name of a tar entry, which unpackEntry refuses naming it -
// one that leaves the feed, one that is neither a regular file nor a directory, one that is no ref
// or pack (the Authorization header whole, or the token alone) - or fails to write naming it - one
// whose body is cut short, one a directory of its name stands in place of - and a ref, which fsck
// names when the history is not whole and a line names when the feed drops it. It can write the
// token as it is or percent-encoded, every byte a %XX escape (the -encoded cases), which a reading
// of the text as it is finds no run of. Neither Fetch's error nor its log holds a run of the token,
// as written or percent-decoded. The controls: each still names what failed, with the token's
// place redacted. Captures workspace.cache-echo/<case>.
func TestACacheThatEchoesItsTokenIntoWhatItStreamsPrintsNoneOfIt(t *testing.T) {
	const ref = "refs/heads/" + echoedCacheToken
	encoded := percentEncoded(echoedCacheToken, func(int) bool { return true }, true)
	encodedRef := "refs/heads/" + encoded
	// fsckBody streams HEAD naming main and the ref named, pointing at an object nobody streamed.
	fsckBody := func(named string) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			return tarOf(t, [2]string{"HEAD", "ref: refs/heads/main\n"}, [2]string{named, "0123456789abcdef0123456789abcdef01234567\n"})
		}
	}
	// fsckLines is the line fsck's failure names the ref in, for each case that reaches fsck.
	fsckLines := map[string]string{
		"fsck":         "error: refs/heads/[redacted]: invalid sha1 pointer",
		"fsck-encoded": "error: refs/heads/***: invalid sha1 pointer",
	}
	for _, tc := range []struct {
		name string
		body func(t *testing.T) []byte
		says string
	}{
		{"outside", func(t *testing.T) []byte { return tarOf(t, [2]string{"../" + echoedCacheToken, "escaped\n"}) },
			`the repository cache's tar holds "../[redacted]", which is not a path inside the feed`},
		{"link", func(t *testing.T) []byte { return tarOf(t, [2]string{ref + "@"}) },
			`the repository cache's tar holds "refs/heads/[redacted]", which is neither a regular file nor a directory`},
		{"bearer", func(t *testing.T) []byte { return tarOf(t, [2]string{"Bearer " + echoedCacheToken, "x"}) },
			`the repository cache's tar holds "Bearer [redacted]", which is not one of a repository's refs or packs`},
		{"bare", func(t *testing.T) []byte { return tarOf(t, [2]string{echoedCacheToken, "x"}) },
			`the repository cache's tar holds "[redacted]", which is not one of a repository's refs or packs`},
		{"bare-encoded", func(t *testing.T) []byte { return tarOf(t, [2]string{encoded, "x"}) },
			`the repository cache's tar holds "***", which is not one of a repository's refs or packs`},
		{"cut", func(t *testing.T) []byte {
			// One header block and the first 100 bytes of the entry's 1 KiB body.
			return tarOf(t, [2]string{ref, strings.Repeat("x", 1024)})[:512+100]
		}, "read refs/heads/[redacted] from the repository cache's tar: unexpected EOF"},
		{"cut-encoded", func(t *testing.T) []byte {
			return tarOf(t, [2]string{encodedRef, strings.Repeat("x", 1024)})[:512+100]
		}, "read refs/heads/*** from the repository cache's tar: unexpected EOF"},
		{"directory", func(t *testing.T) []byte { return tarOf(t, [2]string{ref + "/"}, [2]string{ref, "x"}) },
			"write refs/heads/[redacted] from the repository cache's tar: open: is a directory"},
		{"fsck", fsckBody(ref), "the repository cache's history is not whole: command failed"},
		{"fsck-encoded", fsckBody(encodedRef), "the repository cache's history is not whole: command failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "main", "history.txt")
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = (&fakeCache{body: tc.body(t), token: echoedCacheToken}).serve(t), echoedCacheToken
			_, err := Fetch(context.Background(), run, req)
			said := ""
			if err != nil {
				said = err.Error()
			}
			requireNoCacheEcho(t, "workspace.cache-echo/"+tc.name, said)
			if !strings.Contains(said, tc.says) {
				t.Errorf("Fetch = %v, want the cache refused naming %q", err, tc.says)
			}
			if line, ok := fsckLines[tc.name]; ok && !strings.Contains(said, line) {
				t.Errorf("Fetch = %v, want fsck's line naming the ref with the token's place redacted (%q)", err, line)
			}
		})
	}
	for _, tc := range []struct{ name, ref, says string }{
		{"log", ref, "removed [redacted] from the feed: GitHub no longer has it"},
		{"log-encoded", encodedRef, "removed *** from the feed: GitHub no longer has it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "main", "history.txt")
			cache := cacheOf(t, run.remote)
			runSetup(t, cache, "git", "--git-dir="+cache, "update-ref", tc.ref, "refs/heads/main")
			req := fetchRequest(t)
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			req.CacheURL, req.CacheToken = (&fakeCache{body: cacheTar(t, cache), token: echoedCacheToken}).serve(t), echoedCacheToken
			if _, err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			said := strings.Join(logged, "\n")
			requireNoCacheEcho(t, "workspace.cache-echo/"+tc.name, said)
			if !strings.Contains(said, tc.says) {
				t.Errorf("Fetch logged %q, want the ref it dropped named with the token's place redacted (%q)", logged, tc.says)
			}
		})
	}
}
