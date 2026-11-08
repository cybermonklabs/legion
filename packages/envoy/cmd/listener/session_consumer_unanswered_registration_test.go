package main

// A registration filters the session's consumer on the plan it is about to store, before it
// stores it, and the repair that follows its session entry is what judges that filter against
// the bucket's newest record and row (consumerFor, confirmRegistered). A registration that
// reaches no entry runs neither: it leaves the filter of a plan nothing stored on the consumer,
// over the filter the registration that won the row set. Both exits are here - every plan
// outrun, and the session entry's write failing - and each ends with the consumer filtered on
// the row the bucket holds (registerSession's deferred settle).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/store"
)

// A registration whose row is rewritten under every plan it makes is answered the
// bus-unavailable 503, and the last plan it filtered the consumer on is one it never stored:
// the writer that won the row holds topics that filter leaves out, and a message stored on one
// of them would be lost once the consumer's position moves past it, which the delivery of
// anything stored after it does. The registration sets the filter back to the row the bucket
// holds before it answers.
func TestARegistrationEveryPlanOfWhichIsOutrunLeavesTheConsumerFilteredOnTheStoredRow(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_outrun_filter"
	// Every pass's plan is outrun by a writer that stores a topic and sets no consumer filter,
	// as a role claim does, so the last plan's filter holds neither the topic stored under it
	// nor the one stored under the pass before it.
	passes := 0
	rig.deps.consumers.beforeRegister = func() {
		passes++
		topic := fmt.Sprintf("notifications.dispatch.issue.OUTRUNFILTER-%d.>", passes)
		if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{topic}); err != nil {
			t.Errorf("store %s under the registration's plan: %v", topic, err)
		}
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(consumerRegistration(sessionID, "epoch-1", "notifications.team.outrunfilter"))))
	rig.deps.consumers.beforeRegister = nil

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	requireFilteredOnStoredRow(t, rig, sessionID)
	// The topic the last plan was outrun by is the consumer's to deliver, and the registration's
	// own, which nothing stored, is not.
	rig.store(t, "notifications.dispatch.issue.OUTRUNFILTER-3.comment")
	rig.store(t, "notifications.team.outrunfilter")
	rig.awaitPending(t, sessionID, 1)
}

// A registration whose session entry could not be written leaves its interest row stored and
// nothing telling another reader the session is here, so the repair that reads that record
// makes none: the filter it set stands over whatever row landed after it. It holds that filter
// to the row the bucket holds before it answers, as the confirm of an answered registration
// does.
func TestARegistrationWhoseEntryWriteFailsLeavesTheConsumerFilteredOnTheStoredRow(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_entry_write_filter"
	const followed = "notifications.dispatch.issue.ENTRYFAILKEPT.>"
	const added = "notifications.dispatch.issue.ENTRYFAILADD.>"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", followed))

	// The entry write of the registration below fails, and a writer that sets no consumer filter
	// stores the session's row in the window between that registration's row and its entry,
	// which is the window the repair after the entry covers for a registration that is answered.
	var once sync.Once
	swapSessionBucket(t, rig.harness.sessions, func(kv nats.KeyValue) nats.KeyValue {
		return refusingEntryKV{KeyValue: kv, before: func() {
			once.Do(func() {
				if _, err := rig.harness.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{added}); err != nil {
					t.Errorf("store %s on the session's row: %v", added, err)
				}
			})
		}}
	})

	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
		strings.NewReader(consumerRegistration(sessionID, "epoch-1", followed))))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "session registry unavailable") {
		t.Fatalf("status = %d, body = %s; want the 503 of a session entry that could not be written", recorder.Code, recorder.Body.String())
	}

	requireFilteredOnStoredRow(t, rig, sessionID)
	rig.store(t, "notifications.dispatch.issue.ENTRYFAILADD.comment")
	rig.awaitPending(t, sessionID, 1)
}

// refusingEntryKV is a session bucket whose writes all fail, after before has run: the write of
// a registration's session entry, and the window before it where another writer's interest row
// lands.
type refusingEntryKV struct {
	nats.KeyValue
	before func()
}

func (kv refusingEntryKV) Put(string, []byte) (uint64, error) {
	kv.before()
	return 0, fmt.Errorf("injected session entry write failure")
}

// requireFilteredOnStoredRow fails the test unless sessionID's consumer filters on exactly what
// the row the bucket holds builds (sessionConsumerFilter).
func requireFilteredOnStoredRow(t *testing.T, rig *sessionConsumerRig, sessionID string) {
	t.Helper()
	stored, _, err := rig.harness.registry.RefreshAt(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the stored interest row of %s: %v", sessionID, err)
	}
	want := slices.Compact(slices.Sorted(slices.Values(sessionConsumerFilter(sessionID, stored.Topics))))
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("the consumer of %s filters on %v, want %v, the topics of the row the bucket holds", sessionID, got, want)
	}
}
