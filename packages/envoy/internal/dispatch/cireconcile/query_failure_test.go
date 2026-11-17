package cireconcile

import (
	"net/http"
	"strings"
	"testing"
)

// A query GitHub answers with an error status is logged with GitHub's body shown to a bound
// (githubapp.ShownBody), and the page is read over REST: a 5xx body may be the 1 MiB a response
// may hold, and a line that carried it whole carried 1,048,697 bytes in one field (review of
// cybermonklabs/legion#4209 at def907052c, H3). The start of the body, where GitHub writes its
// message, is kept.
func TestAQueryGitHubFailsIsLoggedWithItsBodyToABound(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	const start = `{"message":"Bad Gateway","filler":"`
	h.github.failQueries(http.StatusBadGateway, start+strings.Repeat("x", 1<<20-len(start)-len(`"}`))+`"}`)

	h.pass()

	lines := h.lines("ci reconcile read a repository over REST: GitHub did not answer the query")
	if len(lines) == 0 {
		t.Fatal("the pass logged no line for the query GitHub answered 502")
	}
	text, _ := lines[0]["error"].(string)
	t.Logf("bytes=%d", len(text))
	if len(text) > 1024 || !strings.Contains(text, "status 502") || !strings.Contains(text, start) || !strings.Contains(text, "1048576 bytes") {
		t.Errorf("the line's error is %d bytes and begins %.200q, want at most 1,024 that name status 502, keep the start of GitHub's body and name its 1048576 bytes", len(text), text)
	}
}
