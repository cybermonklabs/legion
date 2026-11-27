package main

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// A takeover's copy carries a minted dedupe key of its own, and an exception about it - the kept
// lane's walk refusing its forward for the holder's delivery mode - names the message's own key,
// which the copy's message id on the kept lane keeps: the Legion daemon's re-hold
// (packages/daemon/internal/daemon/notice_exceptions.go, record.ParseOutboxKey) identifies the
// notice by that key and ignores a key it cannot parse. The controls are a kept copy that is no
// takeover copy, which keeps its key, and a minted-looking key whose message id names no frame.
func TestAnExceptionAboutATakeoverCopyNamesTheMessagesOwnKey(t *testing.T) {
	t.Parallel()
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

// A takeover's copy is judged an outbox copy or the listener's by its own key, which keeps the
// publish route's marker: a copy of a frame the publish route forwarded for source dispatch is the
// listener's, as the frame was, after one takeover and after a second that copies the copy; the
// control is a copy of a frame Dispatch's outbox forwarded under its own key, which is the outbox's
// at both. Each copy key is in the minted form, so every walk forwards it by its key.
func TestATakeoverCopyIsJudgedAnOutboxCopyByItsOwnKeyAfterEveryMove(t *testing.T) {
	t.Parallel()
	const role = "architect"
	for _, row := range []struct {
		name, key string
		outbox    bool
	}{
		{"a copy of a publish-route frame for source dispatch is the listener's", publishDedupePrefix + "0123456789abcdef0123456789abcdef", false},
		{"control: a copy of an outbox frame is the outbox's", "dispatch-0123456789abcdef", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			once := contracts.Envelope{EventID: "evt", Source: "dispatch", Topic: contracts.RoleTopicPrefix + role, DedupeKey: takenFrameDedupeKey(row.key, 7)}
			twice := once
			twice.DedupeKey = takenFrameDedupeKey(once.DedupeKey, 9)
			for move, copied := range []contracts.Envelope{once, twice} {
				if got := isOutboxCopy(copied); got != row.outbox {
					t.Fatalf("isOutboxCopy after takeover %d of key %q (copy key %q) = %v, want %v", move+1, row.key, copied.DedupeKey, got, row.outbox)
				}
				if !regexp.MustCompile(contracts.MintedDedupeKeyPattern).MatchString(copied.DedupeKey) {
					t.Fatalf("copy key %q after takeover %d is not in the minted form", copied.DedupeKey, move+1)
				}
			}
		})
	}
}

// The claim route's warning for a refused takeover keeps a role only while its last line is
// younger than takeoverWarningForget: the next warning forgets a role whose line is older, with the
// lines it held back. The control is a role whose line is a minute old, which stays.
func TestTheTakeoverWarningForgetsRolesItHasNotWarnedOfLately(t *testing.T) {
	// Not parallel: it writes and asserts on entries of the package variable
	// takeoverUnrecordedWarnings (warn-old, warn-recent, warn-now).
	w := &takeoverUnrecordedWarnings
	w.mu.Lock()
	w.last["warn-old"], w.held["warn-old"] = time.Now().Add(-takeoverWarningForget-time.Second), 3
	w.last["warn-recent"], w.held["warn-recent"] = time.Now().Add(-time.Minute), 2
	w.mu.Unlock()
	t.Cleanup(func() {
		w.mu.Lock()
		for _, role := range []string{"warn-old", "warn-recent", "warn-now"} {
			delete(w.last, role)
			delete(w.held, role)
		}
		w.mu.Unlock()
	})

	warnTakeoverUnrecorded(logging.New("test"), "warn-now", "ses_claimant", errors.New("stream refused the record"))

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, kept := w.last["warn-old"]; kept {
		t.Fatalf("the warning kept role warn-old, last warned of %s ago", takeoverWarningForget+time.Second)
	}
	if _, kept := w.held["warn-old"]; kept {
		t.Fatal("the warning kept the held-back count of role warn-old")
	}
	if _, kept := w.last["warn-recent"]; !kept || w.held["warn-recent"] != 2 {
		t.Fatalf("control: the warning forgot role warn-recent, last warned of a minute ago (held %d, want 2)", w.held["warn-recent"])
	}
	if _, kept := w.last["warn-now"]; !kept {
		t.Fatal("the warning did not record role warn-now, which it just warned of")
	}
}
