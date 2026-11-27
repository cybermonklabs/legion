package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// A dispatch_url whose made-up password holds an unencoded "@" leaves the password's tail where
// the host goes, and the resolver's error names that host: the client's error, and the text the
// outbox stores as a write's last_error (storetext.FarSideError, which the state route serves as
// pendingStatusWrites[].lastError), hold no part of the tail, in its ASCII form or in the `xn--`
// form the client dials a tail that is not ASCII by. A password whose part after its "@" ends in
// ":<digits>", or whose head of digits comes before a "/", leaves those digits where the port goes,
// and the dialer's refusal of the port quotes them: neither error holds them. The error is still a
// TransientError, so a later attempt is still made.
func TestTheDispatchClientsErrorHoldsNoTailOfThePassword(t *testing.T) {
	passwordAt := "legion:p" + "@"
	for name, tc := range map[string]struct{ route, secret string }{
		"an ASCII tail":                   {"http://" + passwordAt + "madeuptail0123.invalid", "madeuptail"},
		"a tail that is not ASCII":        {"http://" + passwordAt + "madeuptail0123\u00f6rd.invalid", "madeuptail"},
		"a tail ending in digits":         {"http://" + passwordAt + "madeuptail0123.invalid:98765432", "98765432"},
		"a head of digits before a slash": {"http://legion:48213579" + "/madeuptail@dispatch.invalid", "48213579"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(tc.route, BearerTransport("dispatch-token", nil)).Handoff(context.Background(), "handoff-1")
			if err == nil {
				t.Fatal("Handoff = nil, want the failed dial")
			}
			var transient *TransientError
			if !errors.As(err, &transient) {
				t.Errorf("Handoff = %v, want a TransientError", err)
			}
			for what, text := range map[string]string{"the error": err.Error(), "the stored last_error": storetext.FarSideError(err.Error())} {
				if lower := strings.ToLower(text); strings.Contains(lower, tc.secret) || strings.Contains(lower, "xn--") {
					t.Errorf("%s %q holds part of the password", what, text)
				}
			}
		})
	}
}
