package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An envoy_url whose made-up password holds an unencoded "@" leaves the password's tail where the
// host goes, and the resolver's error names that host: every request of the publisher that got no
// answer (a publish, a role lookup, a registration read, a session delete, a direct send) holds no
// part of the tail, in its ASCII form or in the `xn--` form a tail that is not ASCII is dialed by.
// A password holding a "/" ends the authority there, so url.Parse reads its head as the port and
// no request is built: that error, which quotes the URL whole, holds no part of the password
// either.
func TestThePublishersErrorsHoldNoTailOfThePassword(t *testing.T) {
	passwordAt := "legion:p" + "@"
	for name, address := range map[string]string{
		"an ASCII tail":            "http://" + passwordAt + "madeuptail0123.invalid",
		"a tail that is not ASCII": "http://" + passwordAt + "madeuptail0123\u00f6rd.invalid",
		"a head read as the port":  "http://legion:madeuptail0123/x" + "@listener.invalid",
	} {
		publisher := New(address, "envoy-token")
		ctx := context.Background()
		_, _, lookupErr := publisher.RoleHolder(ctx, "implementer")
		_, readErr := publisher.ReadSessionRegistration(ctx, "ses_x")
		_, deleteErr := publisher.DeleteSession(ctx, "ses_x", DeleteConditions{NotRegisteredAfter: time.Unix(0, 0)})
		for request, err := range map[string]error{
			"a publish":           publisher.Publish(ctx, "notifications.role.implementer", "hello", map[string]string{}, "key-1"),
			"a role lookup":       lookupErr,
			"a registration read": readErr,
			"a session delete":    deleteErr,
			"a direct send":       publisher.Send(ctx, Message{Target: "ses_x", SourceSession: "ses_y", Text: "hello", IdempotencyKey: "key-2"}),
		} {
			t.Run(name+"/"+request, func(t *testing.T) {
				if err == nil {
					t.Fatal("the request = nil, want the failed dial or build")
				}
				if lower := strings.ToLower(err.Error()); strings.Contains(lower, "madeuptail") || strings.Contains(lower, "xn--") {
					t.Errorf("the error %q holds part of the password's tail", err)
				}
			})
		}
	}
}
