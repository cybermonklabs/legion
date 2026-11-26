package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// ReadDiff asks for GitHub's diff media type and takes the answer whole within the caller's limit,
// and its bound is DiffTimeout, its own, and not the RequestTimeout every other call has: a diff
// GitHub takes longer to write than a JSON answer still arrives, and a diff it never answers ends
// at DiffTimeout. An answer past the limit is a *ResponseTooLargeError, as ReadWithin answers it.
func TestReadDiffAsksForTheDiffWithinTheCallersLimitAndItsOwnTimeout(t *testing.T) {
	const diff = "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-old\n+new\n"
	var mu sync.Mutex
	var accepts []string
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accepts = append(accepts, r.Header.Get("Accept"))
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/acme/web/commits/c0ffee":
			w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
			fmt.Fprint(w, diff)
		case "/repos/acme/web/compare/a...b":
			fmt.Fprint(w, strings.Repeat("x", 65))
		case "/repos/acme/web/commits/hangs":
			<-r.Context().Done()
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	body, status, header, err := client.ReadDiff(ctx, "installation-token", "/repos/acme/web/commits/c0ffee", 1<<20)
	if err != nil || status != http.StatusOK || string(body) != diff {
		t.Fatalf("ReadDiff: status %d, body %q, %v; want GitHub's 200 and its diff whole", status, body, err)
	}
	if got := header.Get("Content-Type"); !strings.HasPrefix(got, "application/vnd.github.diff") {
		t.Fatalf("ReadDiff: the answer's Content-Type is %q, want GitHub's diff media type", got)
	}
	mu.Lock()
	sent := append([]string(nil), accepts...)
	mu.Unlock()
	if len(sent) != 1 || sent[0] != "application/vnd.github.diff" {
		t.Fatalf("ReadDiff sent Accept %q, want one request asking for application/vnd.github.diff", sent)
	}

	body, status, header, err = client.ReadDiff(ctx, "installation-token", "/repos/acme/web/compare/a...b", 64)
	var tooLarge *ResponseTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != 64 {
		t.Fatalf("ReadDiff of 65 bytes within 64: %v, want a *ResponseTooLargeError at 64", err)
	}
	if body != nil || status != 0 || header != nil {
		t.Fatalf("ReadDiff past its limit: body=%d status=%d header=%v, want no truncated answer", len(body), status, header)
	}

	t.Run("RequestTimeout does not bound it", func(t *testing.T) {
		saved := RequestTimeout
		RequestTimeout = time.Nanosecond
		t.Cleanup(func() { RequestTimeout = saved })
		if _, _, _, err := client.Read(ctx, "installation-token", "/repos/acme/web/commits/c0ffee"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a Read within a RequestTimeout of a nanosecond: %v, want the deadline", err)
		}
		if body, status, _, err := client.ReadDiff(ctx, "installation-token", "/repos/acme/web/commits/c0ffee", 1<<20); err != nil || status != http.StatusOK || string(body) != diff {
			t.Fatalf("ReadDiff under a RequestTimeout of a nanosecond: status %d, %v; want GitHub's diff", status, err)
		}
	})

	t.Run("DiffTimeout bounds it", func(t *testing.T) {
		saved := DiffTimeout
		DiffTimeout = 50 * time.Millisecond
		t.Cleanup(func() { DiffTimeout = saved })
		if _, _, _, err := client.ReadDiff(ctx, "installation-token", "/repos/acme/web/commits/hangs", 1<<20); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadDiff of a diff GitHub never answers: %v, want DiffTimeout's deadline", err)
		}
	})
}

// A limit GitHub answers a diff read with is recorded under the token's installation as ReadWithin
// records one: the answer comes back as GitHub gave it, and every later request under that
// installation, a diff read or any other, is held with no request sent until the wait ends.
func TestReadDiffRecordsALimitAsReadWithinDoes(t *testing.T) {
	fake := startLimitGitHub(t)
	database := storetest.Open(t)
	client := fake.newClient(t, WithLimits(ratelimit.New(database.Pool)))
	ctx := context.Background()
	token, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	fake.limit("/repos/acme/web/commits/c0ffee")
	_, status, _, err := client.ReadDiff(ctx, token, "/repos/acme/web/commits/c0ffee", 1<<20)
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("a diff read GitHub limits: status %d, %v; want GitHub's 403 as ReadWithin answers it", status, err)
	}
	fake.limit("")
	_, _, _, err = client.ReadDiff(ctx, token, "/repos/acme/web/commits/beef", 1<<20)
	requireHeld(t, err, "a diff read inside the installation's limit")
	_, _, _, err = client.ReadWithin(ctx, token, "/repos/acme/web/issues/2", 1<<20)
	requireHeld(t, err, "a read inside the limit a diff read met")
	if got := fake.count("/repos/acme/web/commits/beef"); got != 0 {
		t.Fatalf("GitHub got %d diff reads inside the installation's limit, want none", got)
	}

	passLimits(t, database)
	if _, status, _, err := client.ReadDiff(ctx, token, "/repos/acme/web/commits/beef", 1<<20); err != nil || status != http.StatusOK {
		t.Fatalf("a diff read once the limit passed: status %d, %v; want GitHub's 200", status, err)
	}
}
