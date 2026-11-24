package dispatchread_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADispatchReadRedirectPrintsNoLocationOfIt: whatever answers at Dispatch's address answers
// the broker's read, which carries its project-access token, with a redirect whose Location quotes
// that token. One net/http cannot parse (unparsed) is net/http's error whatever the client's
// redirect policy, since Go parses a Location before it asks CheckRedirect, and the read's error,
// which the broker logs, is ErrNoAnswer. One that parses names an address on another host nothing
// listens at (elsewhere), which a following client would ask, naming the Location in its error;
// the reader's client follows no redirect, so the read hands back Dispatch's 302 by its status,
// with no error (TestADispatchReadFollowsNoRedirect). Neither outcome, its error or its status,
// holds a run of the token or a word of the Location. Captures dispatchread.redirect/<case>.
func TestADispatchReadRedirectPrintsNoLocationOfIt(t *testing.T) {
	const token = "tq9xb-not-a-secret-RDX-81e4"
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	_ = closed.Close()
	for _, c := range []struct {
		name     string
		location func(bearer string) string
		answered bool
	}{
		{"unparsed", func(bearer string) string { return "/moved%zz" + bearer }, false},
		{"elsewhere", func(bearer string) string { return "http://" + dead + "/moved?access_token=" + bearer }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", c.location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
				w.WriteHeader(http.StatusFound)
			}))
			defer server.Close()
			reader := dispatchread.New(server.URL, ciphertoken.NewAccountToken(file), nil, 10*time.Second, 1<<20)
			answer, err := reader.Get(t.Context(), "/api/v1/projects/PRIV/repositories", nil)
			// What a caller of the read prints of it: the status it handed back, or its error.
			printed := fmt.Sprintf("%d %v", answer.Status, err)
			redacttest.RequireNoPiece(t, "dispatchread.redirect/"+c.name, printed, token)
			if strings.Contains(printed, "moved") {
				t.Errorf("the read's outcome names the Location: %q", printed)
			}
			switch {
			case c.answered && (err != nil || answer.Status != http.StatusFound):
				t.Errorf("Get = %d, %v, want Dispatch's 302 handed back by its status", answer.Status, err)
			case !c.answered && !errors.Is(err, dispatchread.ErrNoAnswer):
				t.Errorf("Get = %v, want ErrNoAnswer", err)
			}
		})
	}
}
