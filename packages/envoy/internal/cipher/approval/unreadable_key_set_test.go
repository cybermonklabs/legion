package approval_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestASignInThroughAnUnreadableKeySetAnswersSignInUnavailableWithRetryAfter: a sign-in that could
// not reach its provider is answered 503 "Sign-in unavailable" with Retry-After, and is no refusal
// of the person. At the callback, a key set that cannot be read when the ID token is checked
// reached no verdict on the token: the wait is that key set's FailureSpacing, 5 s by default, and
// the sign-in status reports issuer_unreachable. At the sign-in start, a discovery document that
// cannot be read carries no key set's wait, and the page asks for the default, the same 5 s.
// An ID token signed by another key under the published key id, while the set reads, is a verdict:
// 403 SIGN_IN_UNVERIFIED with no Retry-After, and the status reports bad_signature.
func TestASignInThroughAnUnreadableKeySetAnswersSignInUnavailableWithRetryAfter(t *testing.T) {
	// A browser with no live session reads no record when it starts a sign-in, and no row's
	// sign-in completes, so the record the sign-in returns to need not exist.
	recordID := strings.Repeat("a", 64)
	for _, row := range []struct {
		name string
		// drive takes a sign-in through r and answers the broker's last answer to the browser.
		drive  func(t *testing.T, r *rig) ciphertest.Response
		status int
		// heading is the page's heading, code the refusal its answer's record names ("" for an
		// answer that refuses nothing) and retryAfter its Retry-After ("" for none).
		heading, code, retryAfter string
		// reason is what the sign-in status reports afterwards, "" for no failure written.
		reason string
	}{
		{"callback unreadable", func(t *testing.T, r *rig) ciphertest.Response {
			b := r.browser(t)
			authURL := b.StartSignIn(t, "/approve/"+recordID)
			// The start read the provider's discovery document and no key set: the code flow
			// reads its keys for the first ID token it checks, the callback's.
			r.issuer.FailKeys(http.StatusServiceUnavailable)
			return b.Get(t, b.Authorize(t, authURL, person(ada)))
		}, http.StatusServiceUnavailable, "Sign-in unavailable", "", "5", "issuer_unreachable"},
		{"start undiscovered", func(t *testing.T, r *rig) ciphertest.Response {
			r.issuer.FailDiscovery(http.StatusServiceUnavailable)
			return r.browser(t).Get(t, "/approve/"+recordID)
		}, http.StatusServiceUnavailable, "Sign-in unavailable", "", "5", ""},
		{"callback refused", func(t *testing.T, r *rig) ciphertest.Response {
			return r.signInCallback(t, recordID, nil, true, nil)
		}, http.StatusForbidden, "Sign-in refused", "SIGN_IN_UNVERIFIED", "", "bad_signature"},
	} {
		t.Run(row.name, func(t *testing.T) {
			capture := &logtest.Recorder{}
			logtest.SetDefault(t, capture)
			r := newRig(t)
			res := row.drive(t, r)
			if res.Status != row.status || !strings.Contains(res.Body, row.heading) {
				t.Fatalf("%s = %d, want %d, the page saying %s:\n%s", row.name, res.Status, row.status, row.heading, res.Body)
			}
			if got := res.Header.Get("Retry-After"); got != row.retryAfter {
				t.Errorf("%s answered Retry-After %q, want %q", row.name, got, row.retryAfter)
			}
			id := res.Header.Get(answer.Header)
			var codes []any
			for _, record := range capture.Records() {
				if fields := logFields(record); record.Message == answer.RefusalMessage && fields["answer_id"] == id {
					codes = append(codes, fields["code"])
				}
			}
			switch {
			case row.code == "" && len(codes) != 0:
				t.Errorf("%s wrote refusal records naming the codes %v, want none: it refuses nothing", row.name, codes)
			case row.code != "" && (len(codes) != 1 || codes[0] != row.code):
				t.Errorf("%s wrote refusal records naming the codes %v, want one naming %s", row.name, codes, row.code)
			}
			status, body := r.signInStatus(t)
			if row.reason == "" {
				if status.LastFailedSignInReason != nil || status.LastFailedSignInAt != nil {
					t.Errorf("after %s the sign-in status = %s, want no failure written", row.name, body)
				}
			} else if status.LastFailedSignInReason == nil || *status.LastFailedSignInReason != row.reason {
				t.Errorf("after %s the sign-in status = %s, want the reason %q", row.name, body, row.reason)
			}
			if r.sessions(t) != 0 {
				t.Errorf("%s made a session", row.name)
			}
		})
	}
}
