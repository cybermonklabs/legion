package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// followedTopicWake is the kind of the Envoy listener's report of a message on a topic a released
// session follows, as the listener writes it on the wire.
const followedTopicWake = "followed_topic"

// A message on a topic a released session follows wakes the role that kept that session, and the
// woken role's task says the wake was such a message.
func TestAFollowedTopicMessageWakesTheReleasedSession(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("followed-topic", intake.ListenerWake{Kind: followedTopicWake, Session: "session-implementer", At: wakeAt, Report: "report-4"})
	oneWake(t, rig.pool, claim.RoleImplementer, followedTopicWake, "a topic your session follows")
}

// A followed-topic report naming a session no released claim of this daemon's project kept wakes
// nothing: every daemon consumes the subject, and only the one that owns the session acts.
func TestAFollowedTopicMessageForAnotherDaemonsSessionWakesNothing(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("followed-topic", intake.ListenerWake{Kind: followedTopicWake, Session: "somebody-elses-session", At: wakeAt, Report: "report-4"})
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows = %+v, want none for another daemon's session", starts)
	}
}
