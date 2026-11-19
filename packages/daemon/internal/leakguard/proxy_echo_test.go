package leakguard

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// A proxy in GitHub's place can answer the read with a page of its own that echoes the request
// back, the read-only token included. That body is not GitHub's error JSON, so the refusal names
// it in one line with the token and any Authorization or Bearer value redacted, where GitHub's own
// answer is still quoted by its first line (TestARefusedReadIsRefusedWithGitHubsAnswerAndNotKept).
func TestARefusedReadNamesAProxysPageWithoutTheToken(t *testing.T) {
	fake := serveCheck(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		authorization := r.Header.Get("Authorization")
		_, sent, _ := strings.Cut(authorization, " ")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "<html><body><h1>502 Bad Gateway</h1><pre>Authorization: %s\nseen=%s</pre>\n</body></html>", authorization, sent)
	})
	var mints atomic.Int32
	_, err := sourceOver(fake.url, &mints).Get(bound(t), theTag)

	want := fmt.Sprintf("%sGitHub answered the read of %s at %s with HTTP 502: %s", RefusalPrefix, CheckSlug, theTag,
		`a body that is not GitHub's error JSON ("502 Bad Gateway Authorization: Bearer [redacted] seen=[redacted]")`)
	if err == nil || err.Error() != want || strings.Contains(err.Error(), sourceToken) {
		t.Fatalf("the refused read = %v, want %q", err, want)
	}
}
