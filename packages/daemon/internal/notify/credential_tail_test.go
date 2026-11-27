package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An envoy_url whose made-up password holds an unencoded "@" leaves the password's tail where the
// host goes, and the resolver's error names that host: every request of the publisher that got no
// answer (a publish, a role lookup, a registration read, a session delete) holds no part of the
// tail, in its ASCII form or in the `xn--` form a tail that is not ASCII is dialed by.
func TestThePublishersErrorsHoldNoTailOfThePassword(t *testing.T) {
	passwordAt := "legion:p" + "@"
	for name, tail := range map[string]string{
		"an ASCII tail":            "madeuptail0123.invalid",
		"a tail that is not ASCII": "madeuptail0123\u00f6rd.invalid",
	} {
		publisher := New("http://"+passwordAt+tail, "envoy-token")
		ctx := context.Background()
		_, _, lookupErr := publisher.RoleHolder(ctx, "implementer")
		_, readErr := publisher.ReadSessionRegistration(ctx, "ses_x")
		_, deleteErr := publisher.DeleteSession(ctx, "ses_x", DeleteConditions{NotRegisteredAfter: time.Unix(0, 0)})
		// Publish answers the session the listener stored the message for as well as its error
		// (Publisher.Publish); a dial that got no answer names no holder, and this test reads the
		// error alone.
		_, publishErr := publisher.Publish(ctx, "notifications.role.implementer", "hello", map[string]string{}, "key-1")
		for request, err := range map[string]error{
			"a publish":           publishErr,
			"a role lookup":       lookupErr,
			"a registration read": readErr,
			"a session delete":    deleteErr,
		} {
			t.Run(name+"/"+request, func(t *testing.T) {
				if err == nil {
					t.Fatal("the request = nil, want the failed dial")
				}
				if lower := strings.ToLower(err.Error()); strings.Contains(lower, "madeuptail") || strings.Contains(lower, "xn--") {
					t.Errorf("the error %q holds part of the password's tail", err)
				}
			})
		}
	}
}
