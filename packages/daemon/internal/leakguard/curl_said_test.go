package leakguard

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestACurlAnswerSaysCurlsWordsAsTheRuleReadsThem: a curl that heard GitHub's status line and
// then failed says why on its standard error, which can hold the token the read carried, echoed
// by whatever answered in GitHub's place. CurlResponse.Said, what a caller of its own quotes of
// those words, holds no run of the token and keeps curl's own words. The twin shows Stderr, the
// words as curl printed them, holds the token. Capture leakguard.curl-said/stderr.
func TestACurlAnswerSaysCurlsWordsAsTheRuleReadsThem(t *testing.T) {
	const failure = "curl: (56) Failure when receiving data from the peer"
	guard := wordsGuard(t, "exit 1\n", curlPrintsAndExits("HTTP/2 200\r\n", failure+", after the request sent "+echoedToken, 56))
	response, err := guard.CurlGet(bound(t), "repos/"+theRepo, echoedToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("twin", func(t *testing.T) {
		if response.Exit != 56 || !strings.Contains(response.Stderr, echoedToken) {
			t.Fatalf("the stand-in curl exited %d saying %q, want exit 56 and the read's token in what it said, so the row can see a leak", response.Exit, response.Stderr)
		}
	})
	t.Run("stderr", func(t *testing.T) {
		said := response.Said()
		oauth2calltest.RequireNoPiece(t, "leakguard.curl-said/stderr", said, echoedToken)
		if !strings.HasPrefix(said, failure) {
			t.Errorf("Said is %q, want it to keep curl's own words %q", said, failure)
		}
	})
}
