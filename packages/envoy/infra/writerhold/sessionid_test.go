package writerhold

import (
	"strings"
	"testing"

	"github.com/nats-io/nuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
)

// The bucket barrier's key, hold.barrier.<hold id>, is safe to write into a bucket keyed by session
// id only while no session id holds a `.`: a reader that took the barrier's key for a session's
// would read a record nobody wrote. Main refuses a session id holding a `.` before any write, with
// its answer: the listener's delivery route asks bus.SessionConsumerName before it makes or writes
// anything (cmd/listener/session_delivery.go, the 409 `names no session consumer: a session id
// holds no dot, star, angle bracket, slash or whitespace`), and its Legion agents route asks
// legionagents.ValidateSessionID before its put (cmd/listener/legion_agents.go). So each refuses the
// barrier's own key, read as a session id, and a session id with a dot; and the hold's ids
// (newHoldID, a nuid) hold none, so the key is always three tokens. The control is the same id
// without its dot, which both accept. It fails when main starts to accept a dotted session id, or
// when a hold id could hold a dot.
func TestHoldRefusesASessionIdWithADot(t *testing.T) {
	holdID := newHoldID()
	if strings.Contains(holdID, ".") || len(nuid.Next()) == 0 {
		t.Fatalf("a hold id %q holds a dot", holdID)
	}
	barrier := "hold.barrier." + holdID
	for _, id := range []string{barrier, "session.1", "a.b"} {
		if _, ok := bus.SessionConsumerName(id); ok {
			t.Errorf("bus.SessionConsumerName accepts %q, a session id with a dot: the delivery route would write under it", id)
		}
		if err := legionagents.ValidateSessionID(id); err == nil {
			t.Errorf("legionagents.ValidateSessionID accepts %q, a session id with a dot: the agents route would put it", id)
		}
	}
	for _, id := range []string{"session1", "ab", holdID} {
		if _, ok := bus.SessionConsumerName(id); !ok {
			t.Errorf("control: bus.SessionConsumerName refuses %q, a session id with no dot", id)
		}
		if err := legionagents.ValidateSessionID(id); err != nil {
			t.Errorf("control: legionagents.ValidateSessionID refuses %q, a session id with no dot: %v", id, err)
		}
	}
}
