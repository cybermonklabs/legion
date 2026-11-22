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
