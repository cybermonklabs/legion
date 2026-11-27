package dispatchread_test

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
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
	dead := redirectstest.Dead(t)
	for _, c := range []struct {
		name     string
		location func(bearer string) string
		answered bool
	}{
		{"unparsed", redirectstest.Unparsed, false},
		{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			reader := dispatchread.New(redirectstest.EchoBearer(t, c.location), ciphertoken.NewAccountToken("BROKER_PROJECT_ACCESS_TOKEN_FILE", file), nil, 10*time.Second, 1<<20)
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
