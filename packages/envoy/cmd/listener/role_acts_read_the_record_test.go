// The acts of the role lane and its kept lane that turn on who holds a role - the role message's
// resolution, the kept-role sweep's handover, the drain hold before each kept copy and the
// handover's retry - each read the role's claim record from the role bucket, and none decides from
// the registry's watched copy of that bucket. The tests here make the copy drift from the bucket
// in the listener's own registry, in each direction a copy can drift, and require what the record
// says; and they fail every read of the record and require each act to hold rather than forward.

package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// driftRevision is the revision the two drift helpers write a role's entry at: past any revision a
// test's role bucket reaches, so the copy's fence keeps every record the watcher delivers for that
// role afterwards from undoing the drift (kvwatch.Copy), and the copy stays wrong for the rest of
// the test, as a copy that missed a write or a delete stays wrong until a read of the bucket
// repairs it.
const driftRevision = uint64(1) << 62

// roleClaimCopy reaches registry's copy of the role bucket - its unexported roleHolders, the
// holdersMu that guards it and the roleWatcher whose epoch a write into it names - through the
// reflection seam refuseRoleClaimView reaches the watcher field by, so a test makes the copy drift
// from the bucket with no write the bucket sees.
func roleClaimCopy(t *testing.T, registry *store.Registry) (*sync.RWMutex, *kvwatch.Copy[string], *kvwatch.Watcher) {
	t.Helper()
	fields := reflect.ValueOf(registry).Elem()
	field := func(name string) reflect.Value {
		value := fields.FieldByName(name)
		return reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr()))
	}
	return field("holdersMu").Interface().(*sync.RWMutex),
		field("roleHolders").Interface().(*kvwatch.Copy[string]),
		field("roleWatcher").Elem().Interface().(*kvwatch.Watcher)
}

// dropRoleFromCopy takes role out of registry's copy of the role bucket while the bucket goes on
// holding its claim, as a write or a delivery the copy missed leaves it: the lost direction.
func dropRoleFromCopy(t *testing.T, registry *store.Registry, role string) {
	t.Helper()
	guard, roleCopy, watcher := roleClaimCopy(t, registry)
	guard.Lock()
	defer guard.Unlock()
	if outcome := roleCopy.Evict(watcher, watcher.Epoch(), role, driftRevision); outcome != kvwatch.Applied {
		t.Fatalf("take role %s out of the copy: outcome %d, want it applied", role, outcome)
	}
}

// holdRoleInCopy makes registry's copy of the role bucket name holder for role whatever the bucket
// holds - another session's claim, or none at all - as a write or a delete the copy missed leaves
// it: the surplus direction.
func holdRoleInCopy(t *testing.T, registry *store.Registry, role, holder string) {
	t.Helper()
	guard, roleCopy, watcher := roleClaimCopy(t, registry)
	guard.Lock()
	defer guard.Unlock()
	if outcome := roleCopy.Put(watcher, watcher.Epoch(), role, holder, driftRevision); outcome != kvwatch.Applied {
		t.Fatalf("give %s role %s in the copy: outcome %d, want it applied", holder, role, outcome)
	}
}

// requireDrift fails the test unless registry's copy of the role bucket names copyNames for role
// while its claim record names storeNames (empty for none on either side): the drift a case is
// about is in place before its act runs, so a case that passes does so against a copy that
// disagrees with the record.
func requireDrift(t *testing.T, registry *store.Registry, role, copyNames, storeNames string) {
	t.Helper()
	guard, roleCopy, _ := roleClaimCopy(t, registry)
	guard.RLock()
	inCopy, _ := roleCopy.Get(role)
	guard.RUnlock()
	claim, err := registry.RoleClaim(testwait.Context(t), role)
	if err != nil {
		t.Fatalf("read role %s's claim record: %v", role, err)
	}
	if inCopy != copyNames || claim.HolderSessionID != storeNames {
		t.Fatalf("role %s: the copy names %q and the claim record %q, want the copy naming %q and the record %q",
			role, inCopy, claim.HolderSessionID, copyNames, storeNames)
	}
}

// errRoleBucketReadFailed is what every read answers through the role bucket handle
// failRoleClaimReads installs.
var errRoleBucketReadFailed = errors.New("the role bucket's read did not answer")

// failRoleClaimReads puts a bucketThatFails in front of registry's role bucket
// (setRegistryRoleKV), each of its reads counted and answered errRoleBucketReadFailed, and returns
// its count of reads, so a case shows its act made the read that failed. Every claim record the
// registry reads through it fails, while the role claim watcher, which reads through a handle of
// its own, goes on following the bucket, so the copy still names the holder an act would forward
// to on its word.
func failRoleClaimReads(t *testing.T, registry *store.Registry) *atomic.Int64 {
	t.Helper()
	reads := &atomic.Int64{}
	setRegistryRoleKV(t, registry, bucketThatFails{
		KeyValue: registryRoleKV(t, registry),
		before:   func() { reads.Add(1) },
		err:      errRoleBucketReadFailed,
	})
	return reads
}

// keptLaneHeadReads counts, from here on, the stored-message reads of role's kept lane made over
// client's connection: the read of the lane each pass of a handover opens with, and the one the
// sweep makes before it starts a handover (readKeptLaneHead), so a count above zero is a lane
// that a handover or the sweep read. The count is exact for every read made before the call: the
// reads come on a channel subscription, which the connection's reader fills as it reads each one,
// so once a flush of that connection has returned, every read published on it before the flush
// is on the channel, with no callback left to run.
func keptLaneHeadReads(t *testing.T, client *bus.Client, role string) func() int64 {
	t.Helper()
	lane := fmt.Sprintf("%q", bus.KeptRoleSubject(role))
	requests := make(chan *natsgo.Msg, 1<<12)
	sub, err := client.Conn.ChanSubscribe("$JS.API.STREAM.MSG.GET."+bus.Stream, requests)
	if err != nil {
		t.Fatalf("watch the stored-message reads: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the watch of the stored-message reads: %v", err)
	}
	var reads int64
	return func() int64 {
		t.Helper()
		if err := client.Conn.Flush(); err != nil {
			t.Fatalf("flush the stored-message reads: %v", err)
		}
		for {
			select {
			case request := <-requests:
				if strings.Contains(string(request.Data), lane) {
					reads++
				}
			default:
				return reads
			}
		}
	}
}

// unreadableLaneHead stores a drain cursor no read can decode on role's kept lane, so every
// handover pass's lane-head read fails (readKeptLaneHead) and the handover's retry decides what
// happens next.
func unreadableLaneHead(t *testing.T, client *bus.Client, role string) {
	t.Helper()
	if _, err := client.PublishCursorAt(testwait.Context(t), bus.KeptRoleCursorSubject(role), []byte("{"), 0); err != nil {
		t.Fatalf("write role %s's cursor no read can decode: %v", role, err)
	}
}

// logRecordsNamed is every JSON record in logs whose msg is name and whose role is role.
func logRecordsNamed(logs, name, role string) []map[string]any {
	return linesIn(logs, name, map[string]any{"role": role})
}

// liveRoleActEntry is the record every session here registers with: live and self-subscribed, so a
// resolution of a role it holds forwards to it and a forward to it is refused by nothing.
var liveRoleActEntry = session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}

// roleActRig is one listener's delivery configuration over a harness, with every role forward
// recorded rather than stored: the role lane's, a handover's and a walk's all reach forwards, so a
// case reads which session each message reached.
type roleActRig struct {
	harness  listenerDeliveryHarness
	cfg      listenerDeliveryHandlerConfig
	forwards chan keptHandover
}

func newRoleActRig(t *testing.T) *roleActRig {
	t.Helper()
	rig := &roleActRig{harness: newListenerDeliveryHarness(t), forwards: make(chan keptHandover, 64)}
	cfg := rig.harness.config
	cfg.forwardRole = inlineForward(func(subject string, item contracts.Envelope, msgID string, _ time.Duration) error {
		rig.forwards <- keptHandover{holder: strings.TrimPrefix(subject, contracts.AgentTopicPrefix), key: item.DedupeKey, msgID: msgID}
		return nil
	})
	rig.cfg = wireTestRoleForwarder(t, cfg)
	return rig
}

// register registers sessionID live.
func (r *roleActRig) register(t *testing.T, sessionID string) {
	t.Helper()
	if err := r.harness.sessions.Put(sessionID, liveRoleActEntry); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
}

// claim registers sessionID live and writes its claim of role to the bucket, which the copy then
// names too.
func (r *roleActRig) claim(t *testing.T, sessionID, role string) {
	t.Helper()
	claimRoleWith(t, r.harness, sessionID, role, liveRoleActEntry)
}

// deliver hands the role lane a message to role, as a core message, waits through the shared wait
// for every forward it made to be reported and returns the message.
func (r *roleActRig) deliver(t *testing.T, role, key string) contracts.Envelope {
	t.Helper()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, key)
	coreNATSDeliveryHandler(r.cfg)(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	if !r.cfg.roleForwards.wait(testwait.Context(t)) {
		t.Fatalf("%d role forwards were still outstanding when the test's waits gave up", r.cfg.roleForwards.outstanding())
	}
	return item
}

// keep stores a copy of a message to role on its kept lane, as the listener's give-up paths store
// one (keepRoleMessage), with no handover behind it, and returns the message.
func (r *roleActRig) keep(t *testing.T, role, key string) contracts.Envelope {
	t.Helper()
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, key)
	if _, err := keepRoleMessage(r.cfg, role, item); err != nil {
		t.Fatalf("keep a copy on role %s's lane; the listener log says why:\n%s", role, r.harness.logs.String())
	}
	return item
}

// walk reads role's kept lane head and walks it once for holder (walkKeptRole), as a handover's
// pass that resolved holder does: the drain hold is read before each copy of that walk.
func (r *roleActRig) walk(t *testing.T, role, holder string) (roleKeepDrainResult, error) {
	t.Helper()
	ctx := testwait.Context(t)
	head, err := readKeptLaneHead(ctx, r.harness.client, role)
	if err != nil || !head.left {
		t.Fatalf("read role %s's kept lane head: %+v, %v; want a copy left on it", role, head, err)
	}
	return walkKeptRole(ctx, r.cfg, role, holder, liveRoleActEntry, head, time.Now)
}

// handOver starts a handover of role's kept lane (drainKeptRole) on a copy of the rig's
// configuration whose clock is clk, a fake the case moves: the handover's pauses and its reads'
// bounds run on it (cfg.clock). Its window runs on clk too, and is two of the first pause a failed
// read earns (roleKeepDrainRetryPause): it holds a first read, the pause after it and the read
// behind that pause, and ends before anything that second read arms on clk, its claim read's
// bound and the pause after it. handOver returns what the handover did first (endedOrPaused) - it
// returned, having armed no pause, or it armed that first pause and waits in it - and the channel
// its error comes on once a handover found paused has returned.
func (r *roleActRig) handOver(t *testing.T, role string, clk *clocktest.Fake) (handoverEnd, <-chan error) {
	t.Helper()
	cfg := r.cfg
	cfg.clock = clk
	ctx, cancel := clk.WithTimeout(testwait.Context(t), 2*roleKeepDrainRetryPause)
	t.Cleanup(cancel)
	ended := make(chan error, 1)
	go func() { ended <- drainKeptRole(ctx, cfg, role, time.Now) }()
	return endedOrPaused(t, clk, roleKeepDrainRetryPause, ended), ended
}

// awaitForward waits through the shared wait for the next forward and fails unless it is item's,
// to holder.
func (r *roleActRig) awaitForward(t *testing.T, holder string, item contracts.Envelope) {
	t.Helper()
	forwarded := testwait.Receive(t, fmt.Sprintf("event %s's forward to %s", item.EventID, holder), r.forwards)
	if forwarded.holder != holder || forwarded.key != roleForwardDedupePrefix+item.DedupeKey {
		t.Fatalf("the forward reached %s under %s, want event %s's to %s:\n%s",
			forwarded.holder, forwarded.key, item.EventID, holder, r.harness.logs.String())
	}
}

// requireNoForward fails when any forward has been made, or is still to come from a handover of
// role under way. Every forward the rig records is made by the role lane, whose forwards deliver
// has waited for; by an act the case called and that has returned (a walk, a handover, a sweep
// pass, which waits for the handovers it started); or by a handover a keep started in a goroutine
// of its own (keepForNextHolder, startHandover), counted on the listener's guards before the
// keep's caller returned. So once no handover of role is under way on the listener
// (awaitKeptHandoversIdle), every forward that will come has come, and the forwards are read with
// no window.
func (r *roleActRig) requireNoForward(t *testing.T, role string) {
	t.Helper()
	awaitKeptHandoversIdle(t, &r.cfg, role)
	select {
	case forwarded := <-r.forwards:
		t.Fatalf("a forward reached %s under %s, want none:\n%s", forwarded.holder, forwarded.key, r.harness.logs.String())
	default:
	}
}

// Every act that turns on who holds a role reads that role's claim record, in both directions the
// copy can be wrong in: a surplus copy names a session the record does not (the record names
// another session or none), and a lost copy lacks a claim the record gives a session. Each act is
// required to do what the record says: the role message, the kept-role sweep, the drain hold and
// the handover's retry. The handover's pauses run on a fake clock the case moves (handOver), so a
// handover that retries is one that armed its pause there. The test logs the cases it ran and
// their count.
func TestEachActReadsTheRecordInBothDirections(t *testing.T) {
	t.Parallel()
	const (
		holder = "ses_act_holder"
		other  = "ses_act_other"
	)
	var (
		casesMu sync.Mutex
		cases   []string
	)
	// The cases run beside each other once this function has returned, so the line the test ends
	// with is written once every one of them has ended.
	t.Cleanup(func() {
		casesMu.Lock()
		defer casesMu.Unlock()
		t.Logf("%d cases, each act run on a copy that disagrees with the claim record: %s", len(cases), strings.Join(cases, "; "))
	})
	// leaf records the case it is called from, for the line the test ends with.
	leaf := func(t *testing.T) {
		name := strings.TrimPrefix(t.Name(), "TestEachActReadsTheRecordInBothDirections/")
		casesMu.Lock()
		cases = append(cases, name)
		casesMu.Unlock()
		t.Logf("case: %s", name)
	}

	t.Run("role message", func(t *testing.T) {
		t.Parallel()
		// The copy names the session for a role whose record names another session, or none:
		// the message goes where the record says - to that other session, or onto the kept lane
		// for the role's next holder - and never to the session the copy names.
		t.Run("surplus", func(t *testing.T) {
			t.Parallel()
			for name, storeHolder := range map[string]string{"another session": other, "no claim": ""} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					leaf(t)
					const role = "act-message-surplus"
					rig := newRoleActRig(t)
					rig.register(t, holder)
					if storeHolder != "" {
						rig.claim(t, storeHolder, role)
					}
					holdRoleInCopy(t, rig.harness.registry, role, holder)
					requireDrift(t, rig.harness.registry, role, holder, storeHolder)

					item := rig.deliver(t, role, "act-message-surplus")

					if storeHolder != "" {
						rig.awaitForward(t, storeHolder, item)
					} else {
						waitForKeptRoleMessage(t, rig.harness.client, role, item.EventID)
					}
					rig.requireNoForward(t, role)
				})
			}
		})
		// The record gives the session the role and the copy lost it: the session gets the
		// message, and nothing is kept for a next holder.
		t.Run("lost", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-message-lost"
			rig := newRoleActRig(t)
			rig.claim(t, holder, role)
			dropRoleFromCopy(t, rig.harness.registry, role)
			requireDrift(t, rig.harness.registry, role, "", holder)

			item := rig.deliver(t, role, "act-message-lost")

			rig.awaitForward(t, holder, item)
			assertNothingKept(t, rig.harness.client, role)
		})
	})

	t.Run("kept-role sweep", func(t *testing.T) {
		t.Parallel()
		// The copy names a live session for a role whose lane holds a copy and which no record
		// claims: the sweep hands the lane over, as it hands over every lane, and the handover's
		// resolution reads the record, finds no claim, and forwards nothing. The role's wake
		// report, which the sweep raises again only after a handover that resolved no live
		// holder, says which the resolution read.
		t.Run("surplus", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-sweep-surplus"
			rig := newRoleActRig(t)
			rig.register(t, holder)
			rig.keep(t, role, "act-sweep-surplus")
			holdRoleInCopy(t, rig.harness.registry, role, holder)
			requireDrift(t, rig.harness.registry, role, holder, "")
			reads := keptLaneHeadReads(t, rig.harness.client, role)

			sweepOnce(testwait.Context(t), rig.cfg)

			rig.requireNoForward(t, role)
			if got := reads(); got == 0 {
				t.Fatalf("the sweep read role %s's kept lane %d times; want it read and handed over, as every lane is", role, got)
			}
			if wakes := sweptRoleWakes(t, rig.harness.client, role); len(wakes) != 1 {
				t.Fatalf("the sweep raised %d wake reports for role %s, want one: its handover resolved the role as no claim record names it, unheld:\n%s",
					len(wakes), role, rig.harness.logs.String())
			}
		})
		// The record gives a live session the role and the copy lost it: the sweep hands the
		// lane over, and the copy on it reaches that session.
		t.Run("lost", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-sweep-lost"
			rig := newRoleActRig(t)
			item := rig.keep(t, role, "act-sweep-lost")
			rig.claim(t, holder, role)
			dropRoleFromCopy(t, rig.harness.registry, role)
			requireDrift(t, rig.harness.registry, role, "", holder)
			reads := keptLaneHeadReads(t, rig.harness.client, role)

			sweepOnce(testwait.Context(t), rig.cfg)

			rig.awaitForward(t, holder, item)
			// The same counter the surplus case requires a read on counts the reads this sweep,
			// which handed the lane over, made of it: the pass is over (sweepOnce), so its reads
			// are all counted, and none is a counter that counts nothing.
			if got := reads(); got == 0 {
				t.Fatalf("the sweep that handed role %s's copy over read its kept lane %d times, as the counter counts it; want its reads counted", role, got)
			}
		})
	})

	t.Run("drain hold", func(t *testing.T) {
		t.Parallel()
		// The copy names the walk's holder for a role no record claims: the walk ends before its
		// first copy, as a walk whose holder moved, and forwards nothing.
		t.Run("surplus", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-drain-surplus"
			rig := newRoleActRig(t)
			rig.register(t, holder)
			rig.keep(t, role, "act-drain-surplus")
			holdRoleInCopy(t, rig.harness.registry, role, holder)
			requireDrift(t, rig.harness.registry, role, holder, "")

			result, err := rig.walk(t, role, holder)

			if err != nil || result.end != roleKeepHolderMoved || result.forwarded != 0 {
				t.Fatalf("the walk for %s ended %d having forwarded %d, error %v; want it ended as a moved holder with nothing forwarded: no claim record names %s",
					holder, result.end, result.forwarded, err, holder)
			}
			rig.requireNoForward(t, role)
		})
		// The record names the walk's holder and the copy names another session: the walk goes
		// on and forwards the copy to its holder.
		t.Run("lost", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-drain-lost"
			rig := newRoleActRig(t)
			item := rig.keep(t, role, "act-drain-lost")
			rig.claim(t, holder, role)
			rig.register(t, other)
			holdRoleInCopy(t, rig.harness.registry, role, other)
			requireDrift(t, rig.harness.registry, role, other, holder)

			result, err := rig.walk(t, role, holder)

			if err != nil || result.end != roleKeepLaneDrained || result.forwarded != 1 {
				t.Fatalf("the walk for %s ended %d having forwarded %d, error %v; want the lane drained with its one copy forwarded: the claim record names %s",
					holder, result.end, result.forwarded, err, holder)
			}
			rig.awaitForward(t, holder, item)
		})
	})

	t.Run("handover retry", func(t *testing.T) {
		t.Parallel()
		// The lane-head read fails and the copy names a session for a role no record claims: the
		// handover ends at its first read, reporting nothing and arming no pause, as it does for a
		// role nobody holds.
		t.Run("surplus", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-retry-surplus"
			rig := newRoleActRig(t)
			rig.register(t, holder)
			rig.keep(t, role, "act-retry-surplus")
			unreadableLaneHead(t, rig.harness.client, role)
			holdRoleInCopy(t, rig.harness.registry, role, holder)
			requireDrift(t, rig.harness.registry, role, holder, "")

			end, _ := rig.handOver(t, role, clocktest.New(time.Now()))

			if end.paused || end.err != nil {
				t.Fatalf("the handover of role %s armed its first pause of %s to read the lane again: %v, and reported %v (its clock held %v); want it ended at its first lane read with nothing reported and no pause armed: no claim record names a holder waiting",
					role, roleKeepDrainRetryPause, end.paused, end.err, end.armed)
			}
			rig.requireNoForward(t, role)
		})
		// The lane-head read fails and the record names the session the copy lost: the handover
		// waits the read out and reads the lane again, and reports the read it was retrying. It
		// waits on its own clock, which the case moves past the pause once the handover has armed
		// it, and its window, on that clock too, ends inside what follows the second read.
		t.Run("lost", func(t *testing.T) {
			t.Parallel()
			leaf(t)
			const role = "act-retry-lost"
			rig := newRoleActRig(t)
			rig.keep(t, role, "act-retry-lost")
			unreadableLaneHead(t, rig.harness.client, role)
			rig.claim(t, holder, role)
			dropRoleFromCopy(t, rig.harness.registry, role)
			requireDrift(t, rig.harness.registry, role, "", holder)
			reads := keptLaneHeadReads(t, rig.harness.client, role)
			clk := clocktest.New(time.Now())

			end, ended := rig.handOver(t, role, clk)

			if !end.paused {
				t.Fatalf("the handover of role %s ended reporting %v with no pause armed on its clock (it held %v); want it to wait out a pause of %s, read the lane again and report the read that failed: the claim record names %s",
					role, end.err, end.armed, roleKeepDrainRetryPause, holder)
			}
			read := reads()
			clk.Advance(roleKeepDrainRetryPause)
			testwait.Eventually(t, "the handover's read of role "+role+"'s kept lane after its pause", func() bool { return reads() > read })
			clk.Advance(roleKeepDrainRetryPause)
			if err := testwait.Receive(t, "the handover to end with its window", ended); err == nil {
				t.Fatalf("the handover of role %s waited out its pause of %s and read the lane again, and reported nothing; want it to report the read that failed: the claim record names %s",
					role, roleKeepDrainRetryPause, holder)
			}
		})
	})
}

// No act forwards on a claim record it could not read. Every read of the role bucket's records
// fails, while the copy, which the watcher still follows, names the role's live holder: a role
// message is kept on the role's kept lane and reaches nobody, and a kept copy reaches nobody
// through the drain hold, the handover or the sweep. A registration whose renewal cannot read the
// claim is answered 200 all the same, and its WARN names the role. The control runs the same
// drives with the reads answering: the message and each kept copy reach the holder, and the
// registration renews the claim with no WARN. The handover's pauses run on a fake clock
// (handOver), so a handover that would read again is one that armed its pause there.
func TestTheActHoldsOnAFailedRead(t *testing.T) {
	t.Parallel()
	const holder = "ses_act_held"
	for _, reads := range []struct {
		name string
		fail bool
	}{
		{name: "the claim read fails", fail: true},
		{name: "control: the claim read answers"},
	} {
		t.Run(reads.name, func(t *testing.T) {
			t.Parallel()
			t.Run("role message", func(t *testing.T) {
				t.Parallel()
				const role = "act-held-message"
				rig := newRoleActRig(t)
				rig.claim(t, holder, role)
				var failed *atomic.Int64
				if reads.fail {
					failed = failRoleClaimReads(t, rig.harness.registry)
				}
				probe, err := rig.harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
				if err != nil {
					t.Fatalf("subscribe to the role topic's exception lane: %v", err)
				}
				t.Cleanup(func() { _ = probe.Unsubscribe() })
				if err := rig.harness.client.Conn.Flush(); err != nil {
					t.Fatalf("flush the probe: %v", err)
				}

				item := rig.deliver(t, role, "act-held-message")

				if !reads.fail {
					rig.awaitForward(t, holder, item)
					assertNothingKept(t, rig.harness.client, role)
					noException(t, probe)
					return
				}
				if failed.Load() == 0 {
					t.Fatalf("the role message read no claim record, so no read of it failed")
				}
				waitForKeptRoleMessage(t, rig.harness.client, role, item.EventID)
				rig.requireNoForward(t, role)
				noException(t, probe)
				logs := rig.harness.logs.String()
				kept := logRecordsNamed(logs, "listener kept role message for the role's next holder", role)
				if len(kept) != 1 || kept[0]["reason"] != string(roleClaimUnread) {
					t.Fatalf("the kept lines for role %s are %v, want one naming the reason %s:\n%s", role, kept, roleClaimUnread, logs)
				}
				if failedKeeps := logRecordsNamed(logs, "listener role holder lookup failed", role); len(failedKeeps) != 0 {
					t.Fatalf("the lane logged %v, the line of a keep that failed, for a message it kept:\n%s", failedKeeps, logs)
				}
			})

			t.Run("kept copy", func(t *testing.T) {
				t.Parallel()
				t.Run("drain hold", func(t *testing.T) {
					t.Parallel()
					const role = "act-held-drain"
					rig := newRoleActRig(t)
					rig.claim(t, holder, role)
					item := rig.keep(t, role, "act-held-drain")
					var failed *atomic.Int64
					if reads.fail {
						failed = failRoleClaimReads(t, rig.harness.registry)
					}

					result, err := rig.walk(t, role, holder)

					if !reads.fail {
						if err != nil || result.end != roleKeepLaneDrained || result.forwarded != 1 {
							t.Fatalf("the walk ended %d having forwarded %d, error %v; want its one copy forwarded", result.end, result.forwarded, err)
						}
						rig.awaitForward(t, holder, item)
						return
					}
					if !errors.Is(err, errRoleBucketReadFailed) || result.end != roleKeepDrainFailed || result.forwarded != 0 || failed.Load() == 0 {
						t.Fatalf("the walk ended %d having forwarded %d after %d claim reads, error %v; want it failed on the claim read with nothing forwarded",
							result.end, result.forwarded, failed.Load(), err)
					}
					rig.requireNoForward(t, role)
				})
				t.Run("handover", func(t *testing.T) {
					t.Parallel()
					const role = "act-held-handover"
					rig := newRoleActRig(t)
					rig.claim(t, holder, role)
					item := rig.keep(t, role, "act-held-handover")
					var failed *atomic.Int64
					if reads.fail {
						failed = failRoleClaimReads(t, rig.harness.registry)
					}

					end, _ := rig.handOver(t, role, clocktest.New(time.Now()))

					if !reads.fail {
						if end.paused || end.err != nil {
							t.Fatalf("the handover of role %s armed a pause of %s: %v, and reported %v (its clock held %v); want it ended with nothing reported",
								role, roleKeepDrainRetryPause, end.paused, end.err, end.armed)
						}
						rig.awaitForward(t, holder, item)
						requireLeftLines(t, rig.harness.logs.String(), role, 0, nil, "")
						return
					}
					if end.paused || end.err != nil || failed.Load() == 0 {
						t.Fatalf("the handover of role %s armed its first pause of %s to resolve the holder again: %v, after %d claim reads, reporting %v (its clock held %v); want it ended at its first resolution with nothing reported and no pause armed, leaving the lane to the sweep",
							role, roleKeepDrainRetryPause, end.paused, failed.Load(), end.err, end.armed)
					}
					rig.requireNoForward(t, role)
					// Nothing reported is not nothing said: the handover says at WARN that it left the
					// lane to the role's next claim, naming the resolution that failed.
					requireLeftLines(t, rig.harness.logs.String(), role, 1, map[string]any{
						"read":              "listener kept role message could not resolve the role's holder",
						"bound":             float64(roleKeepHolderWindow),
						"claim_read_failed": true,
					}, errRoleBucketReadFailed.Error())
				})
				t.Run("sweep", func(t *testing.T) {
					t.Parallel()
					const role = "act-held-sweep"
					rig := newRoleActRig(t)
					rig.claim(t, holder, role)
					item := rig.keep(t, role, "act-held-sweep")
					var failed *atomic.Int64
					if reads.fail {
						failed = failRoleClaimReads(t, rig.harness.registry)
					}
					laneReads := keptLaneHeadReads(t, rig.harness.client, role)

					sweepOnce(testwait.Context(t), rig.cfg)

					if !reads.fail {
						rig.awaitForward(t, holder, item)
						return
					}
					rig.requireNoForward(t, role)
					if got := laneReads(); got == 0 || failed.Load() == 0 {
						t.Fatalf("the sweep read role %s's kept lane %d times and its handover made %d claim reads; want the lane handed over and held on the claim read that failed",
							role, got, failed.Load())
					}
					if wakes := sweptRoleWakes(t, rig.harness.client, role); len(wakes) != 0 {
						t.Fatalf("the sweep raised %d wake reports for role %s, want none: a resolution that did not answer says nothing of whether the role has a live holder", len(wakes), role)
					}
				})
			})

			t.Run("registration", func(t *testing.T) {
				t.Parallel()
				const role = "act-held-registration"
				rig := newRoleClaimRenewalRig(t)
				seeded := rig.seed(role, lapsedClaim(holder, holdingProcess))
				var failed *atomic.Int64
				if reads.fail {
					failed = failRoleClaimReads(t, rig.registry)
				}

				rig.register(holder, holdingProcess)

				entry, err := rig.roleKV.Get(role)
				if err != nil {
					t.Fatalf("read role %s's claim from the bucket: %v", role, err)
				}
				stored, err := store.DecodeRoleClaim(entry.Value())
				if err != nil {
					t.Fatalf("decode role %s's claim: %v", role, err)
				}
				var unrenewed []map[string]any
				for _, warning := range warnLinesNaming(rig.logs.String(), holder) {
					if warning["msg"] == "listener subscribe left a role claim unrenewed" && strings.Contains(fmt.Sprint(warning["error"]), role) {
						unrenewed = append(unrenewed, warning)
					}
				}
				if !reads.fail {
					if len(unrenewed) != 0 || stored.RenewedAt <= seeded.RenewedAt {
						t.Fatalf("the registration left the claim renewed at %d (it was %d) and logged %v; want it renewed with no WARN",
							stored.RenewedAt, seeded.RenewedAt, unrenewed)
					}
					return
				}
				if len(unrenewed) != 1 || failed.Load() == 0 {
					t.Fatalf("the registration made %d claim reads and logged %v; want one WARN naming role %s:\n%s",
						failed.Load(), unrenewed, role, rig.logs.String())
				}
				if stored.RenewedAt != seeded.RenewedAt {
					t.Fatalf("the claim is renewed at %d, want it left at %d: no renewal is written over a claim no read answered", stored.RenewedAt, seeded.RenewedAt)
				}
			})
		})
	}
}

// A copy that kept a claim the store dropped gives that claim's session no role delivery. The
// role's claim record is deleted from the bucket behind the listener's copy, as another
// listener's release or the role claim reaper deletes it, and the copy goes on naming the live
// session that held it. The first role message after it does not reach that session: the role is
// unclaimed, so the message is kept for its next holder.
func TestARoleCopySurplusClaimGetsNoRoleDelivery(t *testing.T) {
	t.Parallel()
	const (
		role   = "act-copy-surplus"
		holder = "ses_act_copy_surplus"
	)
	rig := newRoleActRig(t)
	rig.claim(t, holder, role)
	if err := registryRoleKV(t, rig.harness.registry).Delete(role); err != nil {
		t.Fatalf("delete role %s's claim record from the bucket: %v", role, err)
	}
	holdRoleInCopy(t, rig.harness.registry, role, holder)
	requireDrift(t, rig.harness.registry, role, holder, "")
	probe, err := rig.harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role topic's exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := rig.harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the probe: %v", err)
	}

	item := rig.deliver(t, role, "act-copy-surplus")

	waitForKeptRoleMessage(t, rig.harness.client, role, item.EventID)
	rig.requireNoForward(t, role)
	noException(t, probe)
}
