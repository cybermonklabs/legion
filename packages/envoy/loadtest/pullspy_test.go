package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// The pull spy counts a session's pull requests in either naming mode: on s-<session id> in the
// fixed mode and on the session's session~<nuid> in the fresh mode (bus.SessionConsumerNamePrefix).
// The listener's durable and Dispatch's consumers count toward the total alone. The control is the
// fixed name, which the spy counted before the fresh mode existed.
func TestThePullSpyCountsASessionsPullsInEitherNamingMode(t *testing.T) {
	for _, row := range []struct {
		consumer string
		session  bool
	}{
		{"s-ses_fixed", true}, // control: the fixed mode's name
		{bus.SessionConsumerNamePrefix + "~K1VbBnCJxFh0NbXfZVkPzB", true},
		{"envoy-listener-1~K1VbBnCJxFh0NbXfZVkPzC", false},
		{"dispatch-delivery", false},
		{"sessionless", false},
	} {
		p := &pullSpy{}
		p.count(pullRequestPrefix + row.consumer)
		want := int64(0)
		if row.session {
			want = 1
		}
		if p.total.Load() != 1 || p.sessions.Load() != want {
			t.Errorf("a pull on %q counted total %d, sessions %d; want 1 and %d", row.consumer, p.total.Load(), p.sessions.Load(), want)
		}
	}
}
