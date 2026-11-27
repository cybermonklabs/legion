package bus

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
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

// A kept copy is of one kind by one rule (ClassifyKeptCopy): a copy from source dispatch with no
// role mark is Dispatch's outbox copy under a key of the outbox's and an event's decimal id, and a
// delivery of a post the publish route kept under the route's key and a post's source event id;
// every other copy is the listener's, owed a dead letter where it names a session.
func TestAKeptCopyIsOfOneKindByOneRule(t *testing.T) {
	t.Parallel()
	postID := DispatchPostEventID(listenerwire.DispatchPost{Kind: listenerwire.DispatchPostComment, ID: "6f1c2b9a-4d3e-4f5a-8b7c-9d0e1f2a3b4c"})
	for _, tc := range []struct {
		name string
		item contracts.Envelope
		want KeptCopyKind
	}{
		{"an outbox copy", contracts.Envelope{Source: "dispatch", SourceEventID: "42", DedupeKey: "dispatch-42"}, KeptCopyOutbox},
		{"an outbox copy that names a session", contracts.Envelope{Source: "dispatch", SourceSession: "ses_author", SourceEventID: "42", DedupeKey: "dispatch-42"}, KeptCopyOutbox},
		{"an outbox copy of an earlier release, with no source event id", contracts.Envelope{Source: "dispatch", DedupeKey: "dispatch-42"}, KeptCopyOutbox},
		{"a delivery the route kept", contracts.Envelope{Source: "dispatch", SourceEventID: postID, DedupeKey: PublishDedupePrefix + "k1"}, KeptCopyDispatchDelivery},
		{"a delivery's copy that names a session", contracts.Envelope{Source: "dispatch", SourceSession: "ses_a", SourceEventID: postID, DedupeKey: PublishDedupePrefix + "k1"}, KeptCopyDispatchDelivery},
		{"a dispatch copy under the outbox's key that names a post", contracts.Envelope{Source: "dispatch", SourceEventID: postID, DedupeKey: "dispatch-42"}, KeptCopyUnanswered},
		{"the same, naming a session", contracts.Envelope{Source: "dispatch", SourceSession: "ses_a", SourceEventID: postID, DedupeKey: "dispatch-42"}, KeptCopySession},
		{"a dispatch copy under the route's key that names no post", contracts.Envelope{Source: "dispatch", SourceEventID: "42", DedupeKey: PublishDedupePrefix + "k2"}, KeptCopyUnanswered},
		{"the same, naming a session", contracts.Envelope{Source: "dispatch", SourceSession: "ses_a", SourceEventID: "42", DedupeKey: PublishDedupePrefix + "k2"}, KeptCopySession},
		{"a send Dispatch marked with a role, under the outbox's key", contracts.Envelope{Source: "dispatch", Role: "reviewer", SourceEventID: "42", DedupeKey: "dispatch-42"}, KeptCopyUnanswered},
		{"a marked delivery's copy that names a session", contracts.Envelope{Source: "dispatch", Role: "reviewer", SourceSession: "ses_a", SourceEventID: postID, DedupeKey: PublishDedupePrefix + "k3"}, KeptCopySession},
		{"an agent's message that names a session", contracts.Envelope{Source: "agent", SourceSession: "ses_a", SourceEventID: postID, DedupeKey: PublishDedupePrefix + "k4"}, KeptCopySession},
		{"an agent's message that names none", contracts.Envelope{Source: "agent", SourceEventID: "42", DedupeKey: "dispatch-42"}, KeptCopyUnanswered},
	} {
		if got := ClassifyKeptCopy(tc.item); got != tc.want {
			t.Errorf("%s: ClassifyKeptCopy(%+v) = %d, want %d", tc.name, tc.item, got, tc.want)
		}
	}
}

// The listener classifies a copy it reads from the lane (ClassifyKeptCopy) and Dispatch's outbox
// classifies the notice of that copy (KeptCopyExpiry.CopyKind) by the one rule, so no copy is an
// outbox copy to one and a delivery to the other. Over every pairing of the dedupe keys and
// source event ids that matter, a copy from source dispatch with no role mark is of a kind its
// notice names alike when it is Dispatch's, and a copy that is not Dispatch's - a key and a source
// event id that disagree on which of Dispatch's copies it is, among them - has a notice that
// names neither kind. The two kinds Dispatch answers for never overlap.
func TestACopyAndItsNoticeNeverReadAsTwoKindsOfDispatchCopy(t *testing.T) {
	t.Parallel()
	postID := DispatchPostEventID(listenerwire.DispatchPost{Kind: listenerwire.DispatchPostMessage, ID: "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d"})
	keys := []string{"", "dispatch-42", "42", PublishDedupePrefix, PublishDedupePrefix + "k1", "forward.publish.k1", id.New()}
	sourceEventIDs := []string{"", "42", "not-a-number", postID, postID + ":again", id.New()}
	for _, key := range keys {
		for _, sourceEventID := range sourceEventIDs {
			for _, session := range []string{"", "ses_a"} {
				item := contracts.Envelope{Source: "dispatch", SourceSession: session, SourceEventID: sourceEventID, DedupeKey: key}
				copyKind := ClassifyKeptCopy(item)
				noticeKind := KeptCopyExpiry{SourceEventID: sourceEventID, DedupeKey: key}.CopyKind()
				answeredFor := copyKind == KeptCopyOutbox || copyKind == KeptCopyDispatchDelivery
				if answeredFor && noticeKind != copyKind {
					t.Errorf("a copy under key %q with source event id %q is kind %d, and its notice names kind %d", key, sourceEventID, copyKind, noticeKind)
				}
				if !answeredFor && (noticeKind == KeptCopyOutbox || noticeKind == KeptCopyDispatchDelivery) {
					t.Errorf("a copy under key %q with source event id %q is kind %d, and its notice names kind %d, one Dispatch answers for", key, sourceEventID, copyKind, noticeKind)
				}
				if _, delivers := ParseDispatchPostEventID(sourceEventID); delivers && copyKind == KeptCopyOutbox {
					t.Errorf("a copy under key %q naming the post %q is an outbox copy", key, sourceEventID)
				}
			}
		}
	}
	crafted := KeptCopyExpiry{SourceEventID: postID, DedupeKey: "dispatch-42"}
	if kind := crafted.CopyKind(); kind == KeptCopyOutbox || kind == KeptCopyDispatchDelivery {
		t.Errorf("a notice under the outbox's key that names a post is kind %d, want neither of Dispatch's", kind)
	}
}
