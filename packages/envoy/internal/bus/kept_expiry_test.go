package bus

import "testing"

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
