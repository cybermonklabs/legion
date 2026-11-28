package store

import (
	"context"
	"errors"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// afterGetKeyValue reads the real bucket, then runs after once before it returns the answer it
// read: the answer of a claim read made before a concurrent claim landed, which reaches its caller
// after that claim did. update, when it is set, stands in for the bucket's own Update.
type afterGetKeyValue struct {
	natsgo.KeyValue
	after  func()
	update func(string, []byte, uint64) (uint64, error)
}

func (kv *afterGetKeyValue) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	after := kv.after
	kv.after = nil
	if after != nil {
		after()
	}
	return entry, err
}

func (kv *afterGetKeyValue) Update(key string, value []byte, revision uint64) (uint64, error) {
	if kv.update != nil {
		return kv.update(key, value, revision)
	}
	return kv.KeyValue.Update(key, value, revision)
}

// A claim that wrote nothing takes the role's lane topic back off its session's interest record,
// and it decides that on a read of the claim. One interest record serves every process of a
// session, so another process of the same session can claim the role while that read is in flight:
// the rollback must not leave the claim that stands without its topic. The first control is a
// claim that wrote nothing with no winner, whose topic the rollback removes; the second is a winner
// that landed before the rollback's read, which the rollback reads and leaves alone.
func TestAFailedClaimsRollbackKeepsTheTopicOfASameSessionClaimThatLandedMeanwhile(t *testing.T) {
	for _, scenario := range []string{
		"control: a claim that wrote nothing with no winner loses its topic",
		"control: a same-session winner before the rollback's read keeps its topic",
		"a same-session winner that lands while the rollback's read is in flight keeps its topic",
	} {
		t.Run(scenario, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			t.Cleanup(cleanup)
			reg, _ := coldRegistry(t, conn)
			const role, a, b = "rollback-race", "ses_rollback_race_a", "ses_rollback_race_b"
			if _, err := reg.SetRole(a, "machine", role, false); err != nil {
				t.Fatal(err)
			}
			wrapped := &afterGetKeyValue{KeyValue: reg.roleKV.Legacy()}
			reg.roleKV = bus.NewKeyValue(wrapped, reg.roleKV.WatchHandle(), reg.roleKV.Streams())
			win := func() {
				if _, err := reg.SetRoleWithPrevious(context.Background(), RoleClaimRequest{SessionID: b, MachineID: "machine", Role: role, ClientInstanceID: "winner"}); err != nil {
					t.Fatalf("the same-session winner's claim: %v", err)
				}
				if !holdsTopic(t, reg, b, role) {
					t.Fatal("control: the winner's claim landed without its topic")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			refusal := errors.New("the failed claim writes nothing")
			_, err := reg.SetRoleWithPrevious(ctx, RoleClaimRequest{
				SessionID: b, MachineID: "machine", Role: role, ClientInstanceID: "loser",
				BeforeTry: func(context.Context, RoleClaim) error {
					switch scenario {
					case "control: a same-session winner before the rollback's read keeps its topic":
						win()
					case "a same-session winner that lands while the rollback's read is in flight keeps its topic":
						wrapped.after = win
					}
					cancel()
					return refusal
				},
			})
			if !errors.Is(err, refusal) {
				t.Fatalf("the failed claim = %v, want its refusal", err)
			}
			stood := storedRoleClaim(t, reg, role)
			topic := holdsTopic(t, reg, b, role)
			wantTopic := scenario != "control: a claim that wrote nothing with no winner loses its topic"
			if topic != wantTopic {
				t.Fatalf("the claim that stands names %s (process %q); %s's record holds the role's topic: %t, want %t",
					stood.HolderSessionID, stood.ClientInstanceID, b, topic, wantTopic)
			}
		})
	}
}

// The same race on the other failure path: an outright claim whose write met a real conflict and
// whose context then ended, while a claim of another process of the same session lands during the
// rollback's read of the claim.
func TestAContendedClaimsRollbackKeepsTheTopicOfASameSessionClaimThatLandedMeanwhile(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	const role, a, b = "rollback-race-contended", "ses_rollback_race_a", "ses_rollback_race_b"
	if _, err := reg.SetRole(a, "machine", role, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := &afterGetKeyValue{KeyValue: reg.roleKV.Legacy()}
	reg.roleKV = bus.NewKeyValue(wrapped, reg.roleKV.WatchHandle(), reg.roleKV.Streams())
	conflicts := 0
	wrapped.update = func(key string, value []byte, revision uint64) (uint64, error) {
		wrapped.update = nil
		// A real competing write moves the revision, so the claim's own update meets the
		// server's conflict and not one this wrapper made.
		if _, err := wrapped.KeyValue.Update(key, []byte(a), revision); err != nil {
			t.Fatal(err)
		}
		next, err := wrapped.KeyValue.Update(key, value, revision)
		if !errors.Is(err, natsgo.ErrKeyExists) {
			t.Fatalf("control: the competing write did not make the claim's update conflict: %v", err)
		}
		conflicts++
		wrapped.after = func() {
			if _, err := reg.SetRoleWithPrevious(context.Background(), RoleClaimRequest{SessionID: b, MachineID: "machine", Role: role, ClientInstanceID: "winner"}); err != nil {
				t.Fatal(err)
			}
			if !holdsTopic(t, reg, b, role) {
				t.Fatal("control: the winner's claim landed without its topic")
			}
		}
		cancel()
		return next, err
	}
	_, err := reg.SetRoleWithPrevious(ctx, RoleClaimRequest{SessionID: b, MachineID: "machine", Role: role, ClientInstanceID: "loser"})
	stood := storedRoleClaim(t, reg, role)
	if conflicts != 1 || err == nil || stood.HolderSessionID != b || stood.ClientInstanceID != "winner" {
		t.Fatalf("the schedule did not run: conflicts %d, the failed claim's error %v, the claim that stands %s (process %q)",
			conflicts, err, stood.HolderSessionID, stood.ClientInstanceID)
	}
	if !holdsTopic(t, reg, b, role) {
		t.Fatalf("the contended claim's rollback removed the topic the same-session claim that stands holds")
	}
}
