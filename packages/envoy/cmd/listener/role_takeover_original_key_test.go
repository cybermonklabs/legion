package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A takeover's copy carries a minted dedupe key of its own, and an exception about it - the kept
// lane's walk refusing its forward for the holder's delivery mode - names the message's own key,
// which the copy's message id on the kept lane keeps: the Legion daemon's re-hold
// (packages/daemon/internal/daemon/notice_exceptions.go, record.ParseOutboxKey) identifies the
// notice by that key and ignores a key it cannot parse. The controls are a kept copy that is no
// takeover copy, which keeps its key, and a minted-looking key whose message id names no frame.
func TestAnExceptionAboutATakeoverCopyNamesTheMessagesOwnKey(t *testing.T) {
	const role = "architect"
	const original = "legion.notice.ISSUE-1.0123456789abcdef"
	copied := contracts.Envelope{EventID: "evt-notice", Topic: contracts.RoleTopicPrefix + role, DedupeKey: takenFrameDedupeKey(original, 41)}
	if got := takenFrameOriginal(copied, takenFrameMsgID(original, role, 41), role); got.DedupeKey != original || got.EventID != copied.EventID {
		t.Fatalf("the exception's envelope for a takeover copy = %+v, want the message's own key %q and the same event", got, original)
	}

	kept := contracts.Envelope{EventID: "evt-kept", Topic: contracts.RoleTopicPrefix + role, DedupeKey: original}
	if got := takenFrameOriginal(kept, takenFrameMsgID(original, role, 41), role); got.DedupeKey != original {
		t.Fatalf("control: a kept copy that is no takeover copy names %q, want its own key %q", got.DedupeKey, original)
	}
	if got := takenFrameOriginal(copied, "some-other-message-id", role); got.DedupeKey != copied.DedupeKey {
		t.Fatalf("control: a copy whose message id names no frame names %q, want the copy's own key %q", got.DedupeKey, copied.DedupeKey)
	}
}

// A takeover's copy is judged an outbox copy or the listener's by the message's own key: a copy of a
// frame the publish route forwarded for source dispatch, whose key the route made under
// publishDedupePrefix, is the listener's, as the frame was; the control is a copy of a frame
// Dispatch's outbox forwarded under its own key, which is the outbox's.
func TestATakeoverCopyIsJudgedAnOutboxCopyByTheMessagesOwnKey(t *testing.T) {
	const role = "architect"
	for _, row := range []struct {
		name, key string
		outbox    bool
	}{
		{"a copy of a publish-route frame for source dispatch is the listener's", publishDedupePrefix + "dispatch.0123456789abcdef", false},
		{"control: a copy of an outbox frame is the outbox's", "dispatch-0123456789abcdef", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			copied := contracts.Envelope{EventID: "evt", Source: "dispatch", Topic: contracts.RoleTopicPrefix + role, DedupeKey: takenFrameDedupeKey(row.key, 7)}
			if got := isOutboxCopy(copied, takenFrameMsgID(row.key, role, 7), role); got != row.outbox {
				t.Fatalf("isOutboxCopy of a takeover copy of key %q = %v, want %v", row.key, got, row.outbox)
			}
		})
	}
}
