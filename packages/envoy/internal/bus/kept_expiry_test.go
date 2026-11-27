package bus

import (
	"testing"

	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/listenerwire"
)

// The attempt a kept copy was kept for is read back from the message id it was stored under
// exactly as Dispatch's outbox wrote it: the first keep is the id the listener keeps the same
// message under, a later one adds its attempt, and every id the outbox did not write that way -
// another key's, another role's, a takeover's own, a malformed attempt - reads as the first.
func TestAKeptCopysAttemptIsReadBackFromItsMessageID(t *testing.T) {
	t.Parallel()
	const key, role = "agent.ses_a.c1:role:reviewer:steer", "reviewer"
	if first := KeptAttemptMsgID(key, role, 1); first != KeptMsgID(key, role) {
		t.Fatalf("the first keep's id is %q, want the listener's own %q", first, KeptMsgID(key, role))
	}
	if none := KeptAttemptMsgID(key, role, 0); none != KeptMsgID(key, role) {
		t.Fatalf("an unset attempt's id is %q, want the first keep's %q", none, KeptMsgID(key, role))
	}
	for attempt := 1; attempt <= 4; attempt++ {
		if got := KeptCopyAttempt(KeptAttemptMsgID(key, role, attempt), key, role); got != attempt {
			t.Errorf("attempt %d's id %q reads as attempt %d", attempt, KeptAttemptMsgID(key, role, attempt), got)
		}
	}
	for _, id := range []string{
		"",
		"takeover-copy",
		KeptAttemptMsgID("dispatch-7", role, 3),
		KeptAttemptMsgID(key, "other", 3),
		KeptMsgID(key, role) + ":attempt:",
		KeptMsgID(key, role) + ":attempt:x",
		KeptMsgID(key, role) + ":attempt:-2",
		KeptMsgID(key, role) + ":frame:42",
	} {
		if got := KeptCopyAttempt(id, key, role); got != 1 {
			t.Errorf("id %q reads as attempt %d, want 1", id, got)
		}
	}
	if got, want := KeptExpiryMsgID(role, 42), "envoy.kept-expired.reviewer.42"; got != want {
		t.Errorf("KeptExpiryMsgID = %q, want %q", got, want)
	}
	if got, want := KeptExpiredSubject(role), "notifications.envoy.kept.expired.reviewer"; got != want {
		t.Errorf("KeptExpiredSubject = %q, want %q", got, want)
	}
}

// The source_event_id the publish route writes for a Dispatch delivery names its post in one
// spelling that reads back as that post, and no source event id any other writer produces reads
// as a post: the decimal id of an outbox event, the 32 hex digits the route mints for every other
// envelope, and a post spelled any other way - another kind, a UUID in upper case, in braces or
// as a URN, no id, no kind.
func TestADeliverysSourceEventIDNamesItsPostInOneSpellingAlone(t *testing.T) {
	t.Parallel()
	for _, post := range []listenerwire.DispatchPost{
		{Kind: listenerwire.DispatchPostComment, ID: "6f1c2b9a-4d3e-4f5a-8b7c-9d0e1f2a3b4c"},
		{Kind: listenerwire.DispatchPostMessage, ID: "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d"},
	} {
		if !ValidDispatchPost(post) {
			t.Fatalf("ValidDispatchPost(%+v) is false, want true", post)
		}
		encoded := DispatchPostEventID(post)
		if got, ok := ParseDispatchPostEventID(encoded); !ok || got != post {
			t.Fatalf("ParseDispatchPostEventID(%q) = %+v, %v; want %+v", encoded, got, ok, post)
		}
	}
	const comment = "6f1c2b9a-4d3e-4f5a-8b7c-9d0e1f2a3b4c"
	for _, sourceEventID := range []string{
		"",
		"42",
		id.New(),
		"dispatch-post:",
		"dispatch-post:comment",
		"dispatch-post:comment:",
		"dispatch-post::" + comment,
		"dispatch-post:ask:" + comment,
		"dispatch-post:comment:6F1C2B9A-4D3E-4F5A-8B7C-9D0E1F2A3B4C",
		"dispatch-post:comment:{" + comment + "}",
		"dispatch-post:comment:urn:uuid:" + comment,
		"dispatch-post:comment:" + comment + ":again",
	} {
		if post, ok := ParseDispatchPostEventID(sourceEventID); ok {
			t.Errorf("ParseDispatchPostEventID(%q) = %+v, want no post", sourceEventID, post)
		}
	}
}
