package leakguard

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// The leak check's read is one of the daemon's own calls to GitHub: with no Client of its own it
// goes through the REST client's transport (githubrest.SetTransport), where the daemon's rate
// counter counts it, under the caller kind leak_check, and its token's mint is asked under that
// kind too. Here the counter knows the read's token as installation 77's, and GitHub's answer
// names the core counter.
func TestALeakCheckReadCountsUnderTheLeakCheckCallerKind(t *testing.T) {
	counter := githubrate.NewCounter()
	counter.Register(sourceToken, "77")
	githubrest.SetTransport(counter.Transport(nil))
	t.Cleanup(func() { githubrest.SetTransport(nil) })
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	fake := serveCheck(t, func(w http.ResponseWriter, _ *http.Request, tag string) {
		w.Header().Set("X-RateLimit-Resource", githubrate.Core)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "1")
		w.Header().Set("X-RateLimit-Reset", reset)
		_, _ = w.Write(wholeCheck(t, marked(tag)))
	})
	var mintedAs string
	source := &Source{Repository: checkRepository, API: fake.url, Token: func(ctx context.Context) (string, error) {
		mintedAs = githubrate.CallerOf(ctx)
		return sourceToken, nil
	}}
	if _, err := source.Get(bound(t), theTag); err != nil {
		t.Fatalf("read the check at %s: %v", theTag, err)
	}
	if mintedAs != "leak_check" {
		t.Errorf("the read's token was minted as caller kind %q, want leak_check", mintedAs)
	}
	core, _ := counter.Own("77")
	if core["leak_check"] != 1 || len(core) != 1 {
		t.Fatalf("installation 77's own core calls = %v, want the one read counted as leak_check", core)
	}
}
