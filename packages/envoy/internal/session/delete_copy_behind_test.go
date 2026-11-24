package session

import (
	"context"
	"errors"
	"testing"

	natsgo "github.com/nats-io/nats.go"
)

// absentCopyKV is a session bucket whose direct get of key answers that the bucket holds no entry,
// as a copy of a replicated bucket that has not applied the session's registration answers; every
// other call is the bucket's own, and a read from the stream's leader does not go through this
// handle.
type absentCopyKV struct {
	natsgo.KeyValue
	key string
}

func (kv absentCopyKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
}

// A stop made right after a registration deletes the entry even where the copy of the bucket a
// direct get reaches has not applied that registration yet: the stop reads the entry from the
// bucket stream's leader. On a three-copy bucket such a copy answers that the entry is absent; the
// conditional stop (DeleteIf, DELETE /v1/sessions with the client's instance) then answered
// EntryDeleted and deleted nothing, and the unconditional one (Delete) deleted nothing either, so
// the session's record stood until it expired. Control: over a session the bucket holds no entry
// for, each stop deletes nothing and DeleteIf answers EntryDeleted, so a row fails only for an
// entry that stayed.
func TestAStopRightAfterARegistrationDeletesTheEntryACopyBehindAnswersAbsent(t *testing.T) {
	const instance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	for _, tc := range []struct {
		name       string
		registered bool
		stop       func(*SessionRegistry, string) error
	}{
		{"a conditional stop of a registered session (DeleteIf)", true, conditionalStop(instance)},
		{"an unconditional stop of a registered session (Delete)", true, unconditionalStop},
		{"control: a conditional stop of a session with no entry (DeleteIf)", false, conditionalStop(instance)},
		{"control: an unconditional stop of a session with no entry (Delete)", false, unconditionalStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := openRegistryForInstanceDelete(t)
			const sessionID = "ses_stop_copy_behind"
			if tc.registered {
				putForInstanceDelete(t, reg, sessionID, instance)
			}
			useKV(t, reg, absentCopyKV{KeyValue: reg.watcher.KV().Legacy(), key: sessionID})
			if _, err := reg.watcher.KV().Get(sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("a direct get of %s through the copy behind = %v, want %v: the copy answers nothing to correct", sessionID, err, natsgo.ErrKeyNotFound)
			}
			if err := tc.stop(reg, sessionID); err != nil {
				t.Fatalf("stop %s: %v", sessionID, err)
			}
			if _, err := reg.watcher.KV().GetFromLeader(context.Background(), sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("the bucket's leader holds %s after its stop (read: %v), want no entry: the entry the copy behind answered absent was kept", sessionID, err)
			}
		})
	}
}

// olderCopyKV is a session bucket whose direct get of key answers older, an entry the bucket's
// stream leader has since replaced, as a copy of a replicated bucket one write behind answers it;
// every other call is the bucket's own, and a read from the stream's leader does not go through
// this handle.
type olderCopyKV struct {
	natsgo.KeyValue
	key   string
	older natsgo.KeyValueEntry
}

func (kv olderCopyKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.key {
		return kv.older, nil
	}
	return kv.KeyValue.Get(key)
}

// A conditional stop (DeleteIf) deletes the entry even where the copy of the bucket a direct get
// reaches is one write behind it: a heartbeat landed after the registration, and the copy still
// answers the registration. The delete fenced on the copy's revision is refused, and the stop's
// next attempt reads the entry from the bucket stream's leader. Read through the same copy again,
// it was refused again and the stop answered EntryKeptAfterLostRace (DELETE /v1/sessions: 200 and
// deleted:false) while the leader still held the entry the stop was asked to delete. Control: with
// a current copy the same stop deletes the entry, so the row fails only for the copy behind.
func TestAConditionalStopDeletesTheEntryACopyOneWriteBehindAnswers(t *testing.T) {
	const (
		instance  = "7a2e9c4b1d8f6a3e7a2e9c4b1d8f6a3e"
		sessionID = "ses_stop_copy_one_write_behind"
	)
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the copy answers the registration a heartbeat replaced", true},
		{"control: the copy answers the heartbeat, the entry the leader holds", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := openRegistryForInstanceDelete(t)
			putForInstanceDelete(t, reg, sessionID, instance)
			registration, err := reg.watcher.KV().Legacy().Get(sessionID)
			if err != nil {
				t.Fatalf("read %s's registration: %v", sessionID, err)
			}
			putForInstanceDelete(t, reg, sessionID, instance)
			heartbeat, err := reg.watcher.KV().GetFromLeader(context.Background(), sessionID)
			if err != nil || heartbeat.Revision() <= registration.Revision() {
				t.Fatalf("the leader's read of %s after its heartbeat = %v (%v), want a revision past the registration's %d", sessionID, heartbeat, err, registration.Revision())
			}
			answered := map[bool]natsgo.KeyValueEntry{true: registration, false: heartbeat}[tc.behind]
			useKV(t, reg, olderCopyKV{KeyValue: reg.watcher.KV().Legacy(), key: sessionID, older: answered})
			if read, err := reg.watcher.KV().Get(sessionID); err != nil || read.Revision() != answered.Revision() {
				t.Fatalf("a direct get of %s through the copy = %v (%v), want revision %d: the copy is not as the row needs it", sessionID, read, err, answered.Revision())
			}
			outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: instance})
			if err != nil || outcome != EntryDeleted {
				t.Errorf("DeleteIf(%s) = %s (%v), want %s", sessionID, outcome, err, EntryDeleted)
			}
			if _, err := reg.watcher.KV().GetFromLeader(context.Background(), sessionID); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("the bucket's leader holds %s after its stop (read: %v), want no entry: the stop kept the entry the copy one write behind answered", sessionID, err)
			}
		})
	}
}

// A conditional stop (DeleteIf) deletes the entry its own client instance registered even where the
// copy of the bucket a direct get reaches still holds another instance's registration under the
// same session id: a new client instance took the session id over and stopped at once, before
// the copy applied its registration. A keep is judged on the bucket stream leader's record, which
// is the stopping instance's. Before, the stop kept the entry on the copy's record, answered
// EntryKeptForAnotherInstance (DELETE /v1/sessions: 200 and deleted:false) and the leader's
// record stood until it expired. Control: with a current copy holding the other instance's
// registration, the stop keeps it.
func TestAConditionalStopDeletesItsOwnEntryWhereACopyBehindHoldsAnotherInstances(t *testing.T) {
	const (
		stopping  = "3c8e1a5f9b2d7c4e3c8e1a5f9b2d7c4e"
		other     = "9d4b2f7a1c6e8b3d9d4b2f7a1c6e8b3d"
		sessionID = "ses_stop_copy_other_instance"
	)
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the copy holds the other instance's registration the stopping instance replaced", true},
		{"control: a current copy holds the other instance's registration", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := openRegistryForInstanceDelete(t)
			putForInstanceDelete(t, reg, sessionID, other)
			others, err := reg.watcher.KV().Legacy().Get(sessionID)
			if err != nil {
				t.Fatalf("read %s's registration by instance %s: %v", sessionID, other, err)
			}
			if tc.behind {
				putForInstanceDelete(t, reg, sessionID, stopping)
			}
			useKV(t, reg, olderCopyKV{KeyValue: reg.watcher.KV().Legacy(), key: sessionID, older: others})
			if read, err := reg.watcher.KV().Get(sessionID); err != nil || read.Revision() != others.Revision() {
				t.Fatalf("a direct get of %s through the copy = %v (%v), want revision %d: the copy is not as the row needs it", sessionID, read, err, others.Revision())
			}
			want := map[bool]DeleteOutcome{true: EntryDeleted, false: EntryKeptForAnotherInstance}[tc.behind]
			outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: stopping})
			if err != nil || outcome != want {
				t.Errorf("DeleteIf(%s) for instance %s = %s (%v), want %s", sessionID, stopping, outcome, err, want)
			}
			_, err = reg.watcher.KV().GetFromLeader(context.Background(), sessionID)
			switch {
			case tc.behind && !errors.Is(err, natsgo.ErrKeyNotFound):
				t.Fatalf("the bucket's leader holds %s after its stop (read: %v), want no entry: the stop kept the stopping instance's entry on the copy's record of another instance", sessionID, err)
			case !tc.behind && err != nil:
				t.Fatalf("the bucket's leader read of %s after the stop = %v, want the other instance's registration kept", sessionID, err)
			}
		})
	}
}

// conditionalStop is DeleteIf naming instance, failing unless it answers EntryDeleted.
func conditionalStop(instance string) func(*SessionRegistry, string) error {
	return func(reg *SessionRegistry, sessionID string) error {
		outcome, err := reg.DeleteIf(context.Background(), sessionID, DeleteConditions{ClientInstanceID: instance})
		if err != nil {
			return err
		}
		if outcome != EntryDeleted {
			return errors.New("DeleteIf answered " + outcome.String() + ", want " + EntryDeleted.String())
		}
		return nil
	}
}

// unconditionalStop is Delete.
func unconditionalStop(reg *SessionRegistry, sessionID string) error {
	return reg.Delete(context.Background(), sessionID)
}
