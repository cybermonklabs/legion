package store

import (
	"errors"
	"testing"

	natsgo "github.com/nats-io/nats.go"
)

// interestCopyBehindKV is the interest bucket as a direct get of a replicated bucket can answer it:
// a Get of key answers older, a row the bucket has since replaced, or that the bucket holds no row
// when older is nil, as a copy that has not applied the latest write answers; every other call is
// the bucket's own, and a read from the stream's leader does not go through this handle.
type interestCopyBehindKV struct {
	natsgo.KeyValue
	key   string
	older natsgo.KeyValueEntry
}

func (kv interestCopyBehindKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key != kv.key {
		return kv.KeyValue.Get(key)
	}
	if kv.older == nil {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.older, nil
}

// A session's interest deleted right after it was written is deleted even where the copy of the
// interest bucket a direct get reaches is behind: the delete (deleteInterest) reads the record
// from the bucket stream's leader. A copy that has not applied the subscribe answers that the
// record is absent, and the delete deleted nothing; a copy one write behind answers the row
// before the last subscribe, and the delete fenced on that row's revision was refused. Either
// way the session's interest stood, and messages went on to a stopped session, until the
// interest reaper. Control: with a current copy the same delete removes the record, so a row
// fails only for the copy behind.
func TestAnInterestDeleteRemovesTheRecordACopyBehindAnswers(t *testing.T) {
	const sessionID = "ses_interest_copy_behind"
	for _, tc := range []struct {
		name   string
		answer func(first, latest natsgo.KeyValueEntry) natsgo.KeyValueEntry
	}{
		{"a copy that has not applied the subscribe answers the record absent", func(_, _ natsgo.KeyValueEntry) natsgo.KeyValueEntry { return nil }},
		{"a copy one write behind answers the row before the last subscribe", func(first, _ natsgo.KeyValueEntry) natsgo.KeyValueEntry { return first }},
		{"control: a current copy answers the row the bucket holds", func(_, latest natsgo.KeyValueEntry) natsgo.KeyValueEntry { return latest }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			r := openRegistry(t, conn)
			bucket := r.watcher.KV().Legacy()
			putInterest(t, bucket, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications.agent." + sessionID}})
			first, err := bucket.Get(sessionID)
			if err != nil {
				t.Fatalf("read %s's first row: %v", sessionID, err)
			}
			putInterest(t, bucket, Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications.agent." + sessionID, "notifications.dispatch.issue.CB-1.>"}})
			latest, err := bucket.Get(sessionID)
			if err != nil || latest.Revision() <= first.Revision() {
				t.Fatalf("read %s's latest row = %v (%v), want a revision past the first row's %d", sessionID, latest, err, first.Revision())
			}
			answered := tc.answer(first, latest)
			useKV(t, r, interestCopyBehindKV{KeyValue: bucket, key: sessionID, older: answered})
			read, err := r.watcher.KV().Get(sessionID)
			switch {
			case answered == nil && !errors.Is(err, natsgo.ErrKeyNotFound),
				answered != nil && (err != nil || read.Revision() != answered.Revision()):
				t.Fatalf("a direct get of %s through the copy = %v (%v), want %v: the copy is not as the row needs it", sessionID, read, err, answered)
			}
			if err := r.deleteInterest(t.Context(), sessionID); err != nil {
				t.Fatalf("delete %s's interest: %v", sessionID, err)
			}
			if _, err := bucket.Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("the bucket holds %s's interest after its delete (read: %v), want no record: the record the copy behind answered was kept", sessionID, err)
			}
		})
	}
}
