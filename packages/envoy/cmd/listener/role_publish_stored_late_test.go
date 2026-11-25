package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/listenerwire"
)

// A publish-route forward that the stream stores later than the fence and the route's own budget
// after the resolution that chose its holder (roles.ForwardStoredTooLate) is answered by what its
// withdrawal finds (withdrawLateForward): 504 when its withdrawal mark landed first, 200 when a
// takeover's copy landed first, and 503 maybe_stored when no mark can be published at all. In each
// row the route resolves A, passes the fence, and its forward waits inside the listener's forward
// (holdForward) while B takes the role and the listener's clock moves on; released, the stream
// stores the forward on A's own subject, where it stays.
//
//   - "withdrawn", at 56 and at 65 minutes: the mark stores first, the send is answered 504
//     naming A and roles.ErrForwardStoredTooLate, and the mark stays on the kept lane. The
//     sender's retry reaches B, and B receives the message exactly once through two sweeps after
//     it. At 56 minutes B's takeover record still copies from A's subject, so a copy of the forward
//     beside the retry would give B the message twice under two event ids: the head before the
//     withdrawal did.
//   - "taken first": a sweep copies the forward to the kept lane and hands it to B before the
//     withdrawal, which finds the copy there, so the send is answered 200, and B has it once.
//   - "not withdrawable": every publish of the mark fails, so the send is answered 503
//     maybe_stored with Retry-After, and no mark is on the lane. The sweeps' copy reaches B once.
//     The sender's retry, which that answer invites, then reaches B a second time under a new
//     event id: the duplicate this branch accepts in place of a loss nothing reports.
func TestAPublishForwardStoredPastItsFenceIsAnsweredByWhatItsWithdrawalFinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall time.Duration
		// takenFirst has a sweep run before the withdrawal; unmarkable fails every mark publish.
		takenFirst, unmarkable bool
		wantStatus             int
	}{
		{name: "withdrawn at 56 minutes", stall: 56 * time.Minute, wantStatus: http.StatusGatewayTimeout},
		{name: "withdrawn at 65 minutes", stall: 65 * time.Minute, wantStatus: http.StatusGatewayTimeout},
		{name: "taken first", stall: 56 * time.Minute, takenFirst: true, wantStatus: http.StatusOK},
		{name: "not withdrawable", stall: 56 * time.Minute, unmarkable: true, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCrashRig(t)
			const a, b, role = "ses_publate_a", "ses_publate_b", "publate"
			const message = "a publish whose forward waits past its fence"
			rig.gone(a)
			rig.claim(rig.away, a, role)
			next := rig.relayed(b, answerDone)
			body, err := json.Marshal(map[string]any{
				"topic": contracts.RoleTopicPrefix + role, "source": "agent", "source_session": crashSender,
				"message": message, "idempotency_key": uuid.NewString(),
			})
			if err != nil {
				t.Fatal(err)
			}
			var faults takeoverFaultSet
			withdrawals := 0
			faults.beforeWithdraw = func(string, uint64) {
				withdrawals++
				if tc.takenFirst {
					rig.sweep(rig.cfg(), role)
				}
			}
			if tc.unmarkable {
				faults.withdrawPublish = func(string) error { return errors.New("the stream did not answer the mark") }
			}
			setTakeoverFaults(t, rig.cfg(), faults)

			held, release := rig.holdForward(message)
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() { answered <- rig.send(rig.mux, http.MethodPost, "/v1/messages/publish", string(body)) }()
			crashAwait(t, held, 10*time.Second, "the publish's forward to "+a+" passing its fence and waiting")
			rig.claim(rig.mux, b, role)
			rig.clock.advance(tc.stall)
			release()
			var first *httptest.ResponseRecorder
			select {
			case first = <-answered:
			case <-time.After(2 * time.Minute):
				t.Fatal("the held publish was not answered within 2 minutes of its release")
			}
			if withdrawals != 1 {
				t.Fatalf("the route withdrew the late forward %d times, want once: the row tests nothing", withdrawals)
			}
			if first.Code != tc.wantStatus {
				t.Fatalf("the held publish answered %d %s, want %d", first.Code, first.Body.String(), tc.wantStatus)
			}
			// The forward the answer judges stays on A's own subject in every row.
			var eventID string
			switch tc.wantStatus {
			case http.StatusGatewayTimeout:
				var failure listenerwire.RoleSendFailure
				if err := json.Unmarshal(first.Body.Bytes(), &failure); err != nil || failure.Reason != string(sendDeliveryFailed) ||
					failure.Holder != a || !strings.Contains(failure.Error, roles.ErrForwardStoredTooLate.Error()) {
					t.Fatalf("the held publish answered %s, want delivery_failed naming %s and %q", first.Body.String(), a, roles.ErrForwardStoredTooLate)
				}
				eventID = failure.EventID
			case http.StatusServiceUnavailable:
				var refusal listenerwire.APIError
				if err := json.Unmarshal(first.Body.Bytes(), &refusal); err != nil || refusal.Code != listenerwire.CodeMaybeStored ||
					first.Header().Get("Retry-After") != "1" || !strings.Contains(refusal.Error, roles.ErrForwardStoredTooLate.Error()) {
					t.Fatalf("the held publish answered %s (Retry-After %q), want code %s naming %q with Retry-After 1",
						first.Body.String(), first.Header().Get("Retry-After"), listenerwire.CodeMaybeStored, roles.ErrForwardStoredTooLate)
				}
				for _, held := range rig.stored(contracts.LegacyAgentSubject(a)) {
					if held.item.PayloadSummary == message {
						eventID = held.item.EventID
					}
				}
			default:
				var delivered listenerwire.PublishResponse
				if err := json.Unmarshal(first.Body.Bytes(), &delivered); err != nil {
					t.Fatalf("decode the held publish's answer %s: %v", first.Body.String(), err)
				}
				eventID = delivered.EventID
			}
			rig.awaitStored(contracts.LegacyAgentSubject(a), eventID, "the late forward on "+a+"'s own subject")
			marks := 0
			for _, kept := range rig.stored(bus.KeptRoleSubject(role)) {
				if isWithdrawnFrame(kept.item) && kept.item.EventID == eventID {
					marks++
				}
			}
			if wantMarks := map[bool]int{true: 1, false: 0}[tc.wantStatus == http.StatusGatewayTimeout]; marks != wantMarks {
				t.Fatalf("the kept lane holds %d withdrawal marks of %s, want %d", marks, eventID, wantMarks)
			}
			if tc.wantStatus == http.StatusGatewayTimeout {
				retry := rig.send(rig.mux, http.MethodPost, "/v1/messages/publish", string(body))
				var delivered listenerwire.PublishResponse
				if err := json.Unmarshal(retry.Body.Bytes(), &delivered); err != nil || retry.Code != http.StatusOK || delivered.Holder != b {
					t.Fatalf("the sender's retry answered %d %s, want 200 naming the new holder %s", retry.Code, retry.Body.String(), b)
				}
			}
			rig.sweep(rig.cfg(), role)
			rig.sweep(rig.cfg(), role)
			got := crashDrain(next, 3*time.Second)
			received := 0
			for _, frame := range got {
				if frame.item.PayloadSummary == message {
					received++
				}
			}
			if received != 1 {
				t.Fatalf("the role's holder %s received the message %d times (frames %v), want exactly once", b, received, frameEvents(got))
			}
			if tc.wantStatus == http.StatusServiceUnavailable {
				retry := rig.send(rig.mux, http.MethodPost, "/v1/messages/publish", string(body))
				if retry.Code != http.StatusOK {
					t.Fatalf("the sender's retry answered %d %s, want 200", retry.Code, retry.Body.String())
				}
				again := crashDrain(next, 3*time.Second)
				events := map[string]bool{}
				for _, frame := range append(got, again...) {
					if frame.item.PayloadSummary == message {
						events[frame.item.EventID] = true
					}
				}
				if len(events) != 2 {
					t.Fatalf("after the retry the role's holder received the message under %d event ids, want 2: the accepted duplicate of the 503 branch", len(events))
				}
			}
		})
	}
}
