package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// The daemon subscribes to its own project's role-lane exceptions alone,
// notifications.envoy.exceptions.notifications.role.<project>.*, the one subject a grant that holds
// it to its projects names: a report on another project's lane, on a lane of no project, or on a
// lane's old form, which names no project, never reaches it. A report on its own project's lane
// does, which shows the subscription hears at all; it is published last, so the reports before it
// had every chance to arrive first.
func TestTheExceptionSubscriptionHearsOnlyItsOwnProjectsLanes(t *testing.T) {
	conn := testnats.Connect(t, testnats.URL(t))
	heard := make(chan string, 8)
	sub, err := subscribeNoticeExceptions(conn, "LEGION", func(data []byte) { heard <- string(data) })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	for _, lane := range []string{
		"notifications.role.OTHER.legion-other-other-1-architect",
		"notifications.role._.lead-reviewer",
		"notifications.role.legion-legion-legion-1-architect",
		"notifications.role.LEGION.legion-legion-legion-1-architect",
	} {
		if err := conn.Publish("notifications.envoy.exceptions."+lane, []byte(lane)); err != nil {
			t.Fatalf("publish the report on %s: %v", lane, err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the reports: %v", err)
	}
	select {
	case got := <-heard:
		if got != "notifications.role.LEGION.legion-legion-legion-1-architect" {
			t.Fatalf("the subscription heard the report on %s, want only its own project's lane", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription heard nothing, not even the report on its own project's lane, so this shows nothing")
	}
	select {
	case got := <-heard:
		t.Fatalf("the subscription also heard the report on %s", got)
	case <-time.After(500 * time.Millisecond):
	}
}
