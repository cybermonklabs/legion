package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// handoverRecords is how many of role's takeover records hand previous's messages to successor
// (bus.HandoverSubject).
func (rig *copiesRig) handoverRecords(t *testing.T, role, previous, successor string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, err := rig.harness.client.TakeoverRecords(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's takeover records: %v", role, err)
	}
	n := 0
	for _, stored := range records {
		if got, ok := bus.TakeoverSuccessor(stored.Subject); ok && got == successor && stored.Record.Previous == previous {
			n++
		}
	}
	return n
}

// The claim route's handover record (publishHandover): one for a claimant and the session it names,
// however often it claims, and a claim whose record did not store is answered 503 so its client
// claims again, which writes the record.
func TestAClaimNamingTheSessionItReplacesWritesOneHandoverRecord(t *testing.T) {
	t.Parallel()
	t.Run("a claim made again naming the same session", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(handoverRole)})
		old, direct, _ := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 2, handoverRole)
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole)

		taker := rig.startTaker(t, handoverNew)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, direct...)
		copiesRequireEach(t, received, handoverNew, direct...)
		// The plugin claims its role again at each registration, naming the same session, and the
		// sweep runs the record's passes again.
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		rig.awaitHandoverIdle(t, handoverRole)
		rig.sweepPass()
		rig.awaitHandoverIdle(t, handoverRole)

		t.Run("case: the role holds one handover record from the replaced session to the claimant", func(t *testing.T) {
			if n := rig.handoverRecords(t, handoverRole, handoverOld, handoverNew); n != 1 {
				t.Errorf("role %s holds %d handover records from %s to %s; want exactly one", handoverRole, n, handoverOld, handoverNew)
			}
		})
		t.Run("case: the stream holds one copy of each direct message on the claimant's own subject", func(t *testing.T) {
			for _, eventID := range direct {
				if n := rig.handedOverCopies(t, handoverNew, eventID); n != 1 {
					t.Errorf("%s's own subject holds %d copies of %s; want exactly one", handoverNew, n, eventID)
				}
			}
		})
	})

	t.Run("the record of the handover did not store", func(t *testing.T) {
		rig := newCopiesRig(t)
		var refusing atomic.Bool
		refusing.Store(true)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
			relayCopy: copiesRelayCopyHeld(handoverRole),
			handoverRecordWrite: func(role, previous, successor string) error {
				if refusing.Load() && role == handoverRole && previous == handoverOld && successor == handoverNew {
					return errors.New("the row refuses the handover record's write")
				}
				return nil
			},
		})
		old, direct, _ := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 1, handoverRole)
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole)

		taker := rig.startTaker(t, handoverNew)
		refused := rig.postClaimReplacing(handoverNew, handoverRole, handoverOld, "")
		t.Run("case: the claim is answered 503 with Retry-After and the code that says it stands", func(t *testing.T) {
			var answer listenerwire.APIError
			if err := json.Unmarshal(refused.Body.Bytes(), &answer); err != nil || refused.Code != http.StatusServiceUnavailable || refused.Header().Get("Retry-After") != "1" || answer.Code != listenerwire.CodeHandoverUnrecorded {
				t.Errorf("the claim whose handover record did not store answered %d (Retry-After %q) %s; want 503 with Retry-After: 1 and the code %s", refused.Code, refused.Header().Get("Retry-After"), refused.Body.String(), listenerwire.CodeHandoverUnrecorded)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if claim, err := rig.harness.registry.RoleClaim(ctx, handoverRole); err != nil || claim.HolderSessionID != handoverNew {
				t.Errorf("role %s's claim names %q (%v); want %s", handoverRole, claim.HolderSessionID, err, handoverNew)
			}
		})

		refusing.Store(false)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, direct...)
		t.Run("case: the claim made again writes the record, and the claimant receives the unread direct message", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, direct...)
		})
	})
}

// A previous_session_id no session consumer can be named for (bus.SessionConsumerName: a ".", "*",
// ">", "/", "\" or whitespace) names no session a handover could copy from, and some such ids are
// no key the known-session bucket reads at all, whose read the handover's rule would answer 503 on
// every try while the claim stands, a loop with no end. So the claim route answers 400 naming the
// field before it writes anything: the role stays unclaimed. Each case fails where such a claim is
// judged by the rule (503, or 200 with a refusal) or lands; the control is the same claim naming a
// session id a consumer can be named for, which no session registered, so it lands and is answered
// the rule's refusal as its warning.
func TestAClaimNamingAnIDNoConsumerCanBeNamedForIsRefused400(t *testing.T) {
	t.Parallel()
	rig := newCopiesRig(t)
	rig.startTaker(t, handoverNew)
	for _, row := range []struct {
		name     string
		replaced string
		status   int
	}{
		{"case: an id with a space, which no known-session key takes", "ses with space", http.StatusBadRequest},
		{"case: an id with a wildcard", "ses_wild*", http.StatusBadRequest},
		{"case: an id with a dot, a key the bucket takes but no consumer name", "ses.dotted", http.StatusBadRequest},
		{"control: an id a consumer can be named for, which no session registered", "ses_never_registered", http.StatusOK},
	} {
		t.Run(row.name, func(t *testing.T) {
			recorder := rig.postClaimReplacing(handoverNew, handoverRole, row.replaced, "")
			var answer listenerwire.APIError
			_ = json.Unmarshal(recorder.Body.Bytes(), &answer)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			claim, err := rig.harness.registry.RoleClaim(ctx, handoverRole)
			if row.status == http.StatusBadRequest {
				if recorder.Code != http.StatusBadRequest || !slices.Equal(answer.Expected, []string{"previous_session_id"}) {
					t.Errorf("a claim naming %q answered %d %s; want 400 naming previous_session_id", row.replaced, recorder.Code, recorder.Body.String())
				}
				if err == nil && claim.HolderSessionID != "" {
					t.Errorf("role %s's claim names %q after the refused claim; want none written", handoverRole, claim.HolderSessionID)
				}
				return
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "no message of "+row.replaced+" is handed over") {
				t.Errorf("a claim naming %q answered %d %s; want 200 with the rule's refusal as its warning", row.replaced, recorder.Code, recorder.Body.String())
			}
			if err != nil || claim.HolderSessionID != handoverNew {
				t.Errorf("role %s's claim names %q (%v); want %s", handoverRole, claim.HolderSessionID, err, handoverNew)
			}
		})
	}
}

// A handover record's pass copies a message that is no role message to the session that replaced
// the previous one only where its topic is the previous session's own agent subject, in either
// form; a record of a role taken with no session named copies no such message, and no pass copies
// a message to another session or a topic the previous session followed.
func TestAHandoverPassTellsAMessageToTheReplacedSessionByEitherFormOfItsTopic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, topic string
		want        takenKind
	}{
		{"the old form of its agent subject", "notifications.agent.ses_gone", takenDirectCopy},
		{"the new form under a project", "notifications.agent.ACME.ses_gone", takenDirectCopy},
		{"another session's agent subject", "notifications.agent.ses_other", takenNone},
		{"another session's agent subject in the new form", "notifications.agent.ACME.ses_other", takenNone},
		{"a topic the session followed", "notifications.github.owner.repo.push", takenNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := []byte(`{"event_id":"evt-handover","topic":"` + c.topic + `","message":"m"}`)
			if _, kind := takenFrame(data, "some-role", "ses_gone", true); kind != c.want {
				t.Errorf("a handover pass takes a message on %s as %v; want %v", c.topic, kind, c.want)
			}
			if _, kind := takenFrame(data, "some-role", "ses_gone", false); kind != takenNone {
				t.Errorf("a takeover pass of a record that names no successor takes a message on %s as %v; want it copied nowhere", c.topic, kind)
			}
		})
	}
}

// A message to a session is one message however many handovers carry it, so two sessions that
// each replaced the other copy it to and fro once each and then no more: the stream drops a copy
// under a message id it already holds (handedOverMsgID). A message that names no event id, which
// a reader of the agent subject accepts, is given one on its first copy (handedOverEventID), and
// each later copy keeps it, so the copy that comes back round to a session carries the id its
// first copy there was stored under. The case fails where an event-less copy is named by the
// frame each hop reads it from, which is new at every hop. The control is a message with an event
// id, which every copy keeps as it is.
func TestAHandedOverMessageKeepsOneIdentityAcrossHops(t *testing.T) {
	t.Parallel()
	type published struct {
		item  contracts.Envelope
		msgID string
	}
	hop := func(t *testing.T, data []byte, previous, successor string, seq uint64) published {
		t.Helper()
		frame, kind := takenFrame(data, "some-role", previous, true)
		if kind != takenDirectCopy {
			t.Fatalf("a handover pass of %s takes %s as %v; want a direct copy", previous, data, kind)
		}
		var got published
		cfg := listenerDeliveryHandlerConfig{keepPublish: func(_ context.Context, subject string, item contracts.Envelope, msgID string) (bus.StoredAck, error) {
			got = published{item: item, msgID: msgID}
			return bus.StoredAck{Sequence: seq + 100}, nil
		}}
		if err := handOverDirectFrame(t.Context(), cfg, previous, successor, frame, seq); err != nil {
			t.Fatalf("hand %s's frame %d over to %s: %v", previous, seq, successor, err)
		}
		return got
	}
	stored := func(t *testing.T, item contracts.Envelope) []byte {
		t.Helper()
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, c := range []struct {
		name, original string
	}{
		{"case: a message that names no event id", `{"topic":"notifications.agent.ses_hop_a","message":"m"}`},
		{"control: a message with an event id", `{"event_id":"evt-hop","topic":"notifications.agent.ses_hop_a","message":"m"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			toB := hop(t, []byte(c.original), "ses_hop_a", "ses_hop_b", 5)
			if toB.item.EventID == "" {
				t.Fatalf("the first copy names no event id: %+v", toB.item)
			}
			backToA := hop(t, stored(t, toB.item), "ses_hop_b", "ses_hop_a", 9)
			againToB := hop(t, stored(t, backToA.item), "ses_hop_a", "ses_hop_b", 12)
			if againToB.msgID != toB.msgID {
				t.Errorf("the copy that came back round to ses_hop_b is stored under %q, its first copy under %q; want one id, which the stream drops", againToB.msgID, toB.msgID)
			}
			if backToA.item.EventID != toB.item.EventID || againToB.item.EventID != toB.item.EventID {
				t.Errorf("the copies name event ids %q, %q, %q; want the first copy's at every hop", toB.item.EventID, backToA.item.EventID, againToB.item.EventID)
			}
		})
	}
}
