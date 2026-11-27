package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// The set of messages a relay passed as too large to relay keeps one entry for each message and
// session, so each gets one WARN however often a restart hands it back. It holds no entry the
// session's consumer can no longer hand back: a start drops the session's entries at or below the
// ack floor it read, and those of another stream, and leaves other sessions' entries alone. So a
// relay that keeps passing such messages holds only those still above its last start's floor.
func TestTheTooLargeSetKeepsOnlyWhatIsAboveTheFloor(t *testing.T) {
	t.Parallel()
	const stream = "stream-a"
	held := func(set *relayPassedSet, sessionID string) int {
		set.mu.Lock()
		defer set.mu.Unlock()
		return len(set.bySession[sessionID])
	}

	var set relayPassedSet
	for seq := uint64(1); seq <= 10; seq++ {
		if !set.first("ses_a", stream, seq) {
			t.Fatalf("the first pass of message %d was not its first: it would write no WARN", seq)
		}
	}
	if !set.first("ses_b", stream, 5) {
		t.Fatal("another session's first pass of message 5 was not its first: that session would get no WARN")
	}
	if set.first("ses_a", stream, 7) {
		t.Fatal("a second pass of message 7 was a first: it would write a second WARN")
	}

	// A start at floor 6: messages 7 to 10 are still above it, and a restart may hand them back.
	set.prune("ses_a", bus.SessionConsumerStart{StreamID: stream, AckFloor: 6})
	if got := held(&set, "ses_a"); got != 4 {
		t.Fatalf("the set holds %d entries of ses_a after a start at floor 6, want 4: messages 7 to 10", got)
	}
	if set.first("ses_a", stream, 8) {
		t.Fatal("a pass of message 8, above the floor, was a first again: it would write a second WARN")
	}
	if got := held(&set, "ses_b"); got != 1 {
		t.Fatalf("the set holds %d entries of ses_b after ses_a's start, want its one", got)
	}

	// A start past every passed message leaves the session nothing in the set.
	set.prune("ses_a", bus.SessionConsumerStart{StreamID: stream, AckFloor: 10})
	if got := held(&set, "ses_a"); got != 0 {
		t.Fatalf("the set holds %d entries of ses_a after a start at floor 10, want none", got)
	}

	// A start on another stream: no entry of the old stream can come back.
	set.prune("ses_b", bus.SessionConsumerStart{StreamID: "stream-b"})
	if got := held(&set, "ses_b"); got != 0 {
		t.Fatalf("the set holds %d entries of ses_b after a start on another stream, want none", got)
	}

	// A long-lived relay that passes a message and starts again past it, a thousand times over,
	// holds no more than the one message above its floor.
	for seq := uint64(11); seq <= 1010; seq++ {
		set.first("ses_a", stream, seq)
		if got := held(&set, "ses_a"); got > 1 {
			t.Fatalf("the set holds %d entries of ses_a after passing message %d, want at most the one above the floor", got, seq)
		}
		set.prune("ses_a", bus.SessionConsumerStart{StreamID: stream, AckFloor: seq})
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if len(set.bySession) != 0 {
		t.Fatalf("the set holds sessions %v after every start passed their messages, want none", set.bySession)
	}
}
