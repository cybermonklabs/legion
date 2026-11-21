package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/session"
)

// An edge replica forgets the admission of a session whose record has left its session cache once
// the admission is older than edgeAdmissionGrace (sessionRelays.pruneAdmissions): a session that
// deregistered, and one admitted whose record never arrived. It keeps the admission of a live
// session at every pass, so the prune reads the cache and not only the admission's age. The
// control is the same pass made before the grace has passed, which forgets nothing: a
// registration admits its session before it writes the session's entry (subscribeHandler), so a
// young admission with no record is one whose record may still be on its way. A session admitted
// again after it was forgotten is admitted as before.
func TestAnEdgeForgetsTheAdmissionOfASessionWhoseRecordIsGone(t *testing.T) {
	rig := newSessionConsumerRig(t)
	sessions := rig.harness.sessions
	admissions := &rig.deps.consumers.admissions
	relays := &sessionRelays{delivery: rig.deps.consumers, sessions: sessions}
	const live, deregistered, unwritten = "ses_admitted_live", "ses_admitted_deregistered", "ses_admitted_unwritten"
	for _, sessionID := range []string{live, deregistered} {
		if err := sessions.Put(sessionID, session.SessionEntry{MachineID: "edge-a", SelfSubscribed: true, Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
	}
	for _, sessionID := range []string{live, deregistered, unwritten} {
		admissions.admit(sessionID, []string{"PRIV"})
	}
	if err := sessions.Delete(context.Background(), deregistered); err != nil {
		t.Fatalf("deregister %s: %v", deregistered, err)
	}
	if _, err := sessions.Get(live); err != nil {
		t.Fatalf("the session cache does not hold %s (%v): the kept row would test nothing", live, err)
	}
	for _, sessionID := range []string{deregistered, unwritten} {
		if _, err := sessions.Get(sessionID); err == nil {
			t.Fatalf("the session cache still holds %s: the prune would have nothing to forget", sessionID)
		}
	}
	requireAdmitted := func(when string, want map[string]bool) {
		t.Helper()
		for sessionID, admitted := range want {
			projects, got := admissions.of(sessionID)
			if got != admitted {
				t.Errorf("%s, %s is admitted: %t (projects %v); want %t", when, sessionID, got, projects, admitted)
			}
		}
	}

	relays.pruneAdmissions(time.Now())
	requireAdmitted("after a pass within the grace (control)", map[string]bool{live: true, deregistered: true, unwritten: true})

	relays.pruneAdmissions(time.Now().Add(2 * edgeAdmissionGrace))
	requireAdmitted("after a pass past the grace", map[string]bool{live: true, deregistered: false, unwritten: false})

	admissions.admit(deregistered, []string{"PRIV"})
	if projects, admitted := admissions.of(deregistered); !admitted || len(projects) != 1 || projects[0] != "PRIV" {
		t.Errorf("%s admitted again reads %v, %t; want [PRIV], true", deregistered, projects, admitted)
	}
}
