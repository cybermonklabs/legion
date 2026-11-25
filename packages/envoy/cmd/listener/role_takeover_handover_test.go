package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

const (
	// handoverOld is the session a claim replaces, handoverNew the session that replaces it,
	// handoverNext a session that claims a role later with no session named. handoverPerson is a
	// person's session and handoverAgent the agent session that replaces it, each with the client
	// instance of its own process (handoverPersonProcess, handoverAgentProcess).
	handoverOld           = "ses_handover_old"
	handoverNew           = "ses_handover_new"
	handoverNext          = "ses_handover_next"
	handoverPerson        = "ses_handover_person"
	handoverAgent         = "ses_handover_agent"
	handoverPersonProcess = "process-of-the-person"
	handoverAgentProcess  = "process-of-the-agent"
	// handoverOldProcess is the replaced session's process, which a hand-back names: the release
	// route frees a claim naming the process that releases it.
	handoverOldProcess = "process-of-the-old-session"
	// handoverRole and handoverOther are two roles the replaced session held.
	handoverRole  = "handover-role"
	handoverOther = "handover-other"
)

// postClaimReplacing is the rig's answer to an outright claim of role by sessionID through POST
// /v1/roles/set that names replaced as the session it replaces (previous_session_id), as a Legion
// session's claim of its role does once its claim's daemon names the session it abandoned, with
// instance as its process when it is not empty.
func (rig *copiesRig) postClaimReplacing(sessionID, role, replaced, instance string) *httptest.ResponseRecorder {
	fields := map[string]any{"session_id": sessionID, "role": role, "previous_session_id": replaced}
	if instance != "" {
		fields["client_instance_id"] = instance
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, postJSON("/v1/roles/set", string(body)))
	return recorder
}

// claimReplacing is postClaimReplacing, and fails the row unless the claim is granted.
func (rig *copiesRig) claimReplacing(t *testing.T, sessionID, role, replaced, instance string) {
	t.Helper()
	if recorder := rig.postClaimReplacing(sessionID, role, replaced, instance); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s naming %s as the session it replaces: status %d %s; want 200", sessionID, role, replaced, recorder.Code, recorder.Body.String())
	}
}

// sendDirect stores on sessionID's own subject a message to sessionID itself, in the shape the send
// route and Dispatch's queueing of a person's message store one (contracts.AgentSendEnvelope: the
// session's own topic, an agent-send dedupe key), from source, with text as its summary, and
// returns its event id.
func (rig *copiesRig) sendDirect(t *testing.T, sessionID, source, text string) string {
	t.Helper()
	subject := contracts.LegacyAgentSubject(sessionID)
	item := listenerTestEnvelope(subject, contracts.AgentSendDedupeKey(sessionID, id.New()))
	item.Source = source
	item.PayloadSummary = text
	if _, err := rig.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, item)); err != nil {
		t.Fatalf("store a message to %s on its own subject: %v", sessionID, err)
	}
	return item.EventID
}

// stopCleanly ends sessionID the way a session that shuts down does: it hands each of roles back,
// as instance, deregisters, and its process ends; its consumer is deleted, with a record of its
// floor (the session resume lane), and no claim of any of roles stands.
func (rig *copiesRig) stopCleanly(t *testing.T, sessionID, instance string, standIn *sessionStandIn, roles ...string) {
	t.Helper()
	for _, role := range roles {
		releaseRoleThrough(t, rig.mux, rig.harness.registry, sessionID, role, instance)
	}
	rig.deregister(t, sessionID, "")
	standIn.close()
	rig.awaitNoConsumer(t, sessionID)
	if _, recorded := rig.resumeRecord(t, sessionID); !recorded {
		t.Fatalf("%s's consumer was deleted with no record of its floor", sessionID)
	}
}

// handedOverCopies is how many messages carrying eventID the stream holds on sessionID's own
// subject, each of which is a message it was sent or a copy handed over to it.
func (rig *copiesRig) handedOverCopies(t *testing.T, sessionID, eventID string) int {
	t.Helper()
	return copiesOf(rig.stored(t, contracts.LegacyAgentSubject(sessionID)), eventID)
}

// handoverUnread starts sessionID's process, registers it as instance and has it claim each of
// roles; stores a message to it that it reads, then direct messages to it from source, then one
// role message of each of roles, none of which it reads; and waits until its consumer holds the
// read one acknowledged and the rest not. It returns the process and the event ids of the unread
// direct messages and role messages, in order.
func (rig *copiesRig) handoverUnread(t *testing.T, sessionID, instance, source string, directs int, roles ...string) (*sessionStandIn, []string, []string) {
	t.Helper()
	standIn := startCopiesStandIn(t, rig.url, sessionID, copiesHolderAnswer)
	fields := map[string]any(nil)
	if instance != "" {
		fields = map[string]any{"client_instance_id": instance}
	}
	rig.register(t, sessionID, fields)
	for _, role := range roles {
		grantRoleClaim(t, rig.mux, sessionID, role, instance, false)
	}
	read := rig.sendDirect(t, sessionID, source, copiesReadMark+"a message to "+sessionID+" it read")
	var direct, role []string
	for range directs {
		direct = append(direct, rig.sendDirect(t, sessionID, source, "an unread message to "+sessionID))
	}
	for _, name := range roles {
		role = append(role, rig.publish(t, name, "", "an unread role message of "+name, sessionID))
	}
	all := append(append([]string{read}, direct...), role...)
	seqs := copiesSeqs(t, standIn.collect(t, len(all), copiesWatchBound), all...)
	awaitAckState(t, rig.consumers, sessionID, len(all)-1, seqs[read])
	return standIn, direct, role
}

// A session that replaces another one names it in its claim of its role (previous_session_id),
// as a Legion session does once its claim's daemon names the session the claim abandoned
// (registration's `replaces`). Each direct message and each role message the replaced session's
// consumer had not acknowledged reaches the new session at least once, counted by event id over a
// watch that outlasts the claim's handover, whether the replaced session's claim of the role still
// stood at the claim (row 1) or the role was free (row 2); no message the replaced session read
// reaches it. The case rows fail on the base this change builds on, where an outright claim reads
// no previous_session_id and no handover record exists: the new session receives none of the
// direct messages, and in row 2 none of the role messages either.
//
// The session relay's own copy of a role message whose role another session holds is held back
// (copiesRelayCopyHeld), so what the new session receives is what the passes copied.
func TestASessionThatReplacesAnotherReceivesItsUnreadMessages(t *testing.T) {
	t.Run("1 the replaced session's claim still stands: it crashed and is past its live window", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(handoverRole)})
		old, direct, role := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 2, handoverRole)
		rig.crash(t, handoverOld, old)
		// Stored once the replaced session's process stopped: a message it never read.
		direct = append(direct, rig.sendDirect(t, handoverOld, "agent", "a message to "+handoverOld+" stored after its crash"))
		rig.sessionClock.advance(session.LiveWindow + 30*time.Second)
		if _, state := rig.harness.sessions.Lookup(handoverOld); state != session.Unresponsive {
			t.Fatalf("%s reads %s past its live window; want %s", handoverOld, state, session.Unresponsive)
		}

		taker := rig.startTaker(t, handoverNew)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, append(direct, role...)...)

		t.Run("case: the new session receives each unread direct message of the replaced session", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, direct...)
		})
		t.Run("case: the new session receives each unread role message of the replaced session", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, role...)
		})
		t.Run("control: a copy is a message to the new session, with the message's own event id and sender", func(t *testing.T) {
			for _, eventID := range direct {
				held, ok := copiesStoredEvent(rig.stored(t, contracts.LegacyAgentSubject(handoverNew)), eventID)
				if !ok {
					t.Errorf("the stream holds no copy of %s on %s's own subject", eventID, handoverNew)
					continue
				}
				if held.item.Topic != contracts.LegacyAgentSubject(handoverNew) || held.item.Source != "agent" || !mintedKey(held.item.DedupeKey) {
					t.Errorf("the copy of %s has topic %q, source %q, dedupe key %q; want %s's own topic, the sender's source, and a minted key", eventID, held.item.Topic, held.item.Source, held.item.DedupeKey, handoverNew)
				}
			}
		})
	})

	t.Run("2 the role was free: the replaced session handed it back and stopped", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(handoverRole)})
		old, direct, role := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 2, handoverRole)
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole)

		taker := rig.startTaker(t, handoverNew)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, append(direct, role...)...)

		t.Run("case: the new session receives each unread direct message of the replaced session", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, direct...)
		})
		t.Run("case: the new session receives each unread role message of the replaced session", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, role...)
		})
	})
}

// The handover holds for a session that held more than one role, and for a person's session that
// an agent session replaces. A role message of each role the replaced session held goes to that
// role's kept lane, which hands it to the role's next live holder: the new session for each role
// it claims (row 1), the next session to claim it for a role the new session does not (row 2). A
// direct message reaches the new session once however many of its claims name the replaced
// session. A person's session's unread messages reach the agent session that names it once it
// has stopped (row 3), and not while it is live (row 4): a session that answers reads its own
// messages. The case rows fail on the base this change builds on, where nothing is copied from a
// session whose claim did not stand and no direct message is copied at all.
//
// Every session here registers and claims on no credential, so each record and each claim names no
// principal, and the rule that hands over only between sessions of one principal (handoverRefusal)
// lets each handover stand. A person's session registered on its person's credential is handed to
// no agent session on a credential of its own service, by design
// (TestTheHandoverGateHandsOverOnlyBetweenSessionsOfOnePrincipal).
func TestTheHandoverHoldsForEachRoleAndForAPersonsSession(t *testing.T) {
	t.Run("1 the replaced session held two roles, and the new session claims both", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: func(role string, seq uint64) error {
			return errors.Join(copiesRelayCopyHeld(handoverRole)(role, seq), copiesRelayCopyHeld(handoverOther)(role, seq))
		}})
		old, direct, role := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 1, handoverRole, handoverOther)
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole, handoverOther)

		taker := rig.startTaker(t, handoverNew)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		rig.claimReplacing(t, handoverNew, handoverOther, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, append(direct, role...)...)
		rig.awaitHandoverIdle(t, handoverOther)

		t.Run("case: the new session receives the unread role message of each of the two roles", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, role...)
		})
		t.Run("case: the new session receives the unread direct message, and the stream holds one copy of it", func(t *testing.T) {
			copiesRequireEach(t, received, handoverNew, direct...)
			for _, eventID := range direct {
				if n := rig.handedOverCopies(t, handoverNew, eventID); n != 1 {
					t.Errorf("%s's own subject holds %d copies of %s after two claims naming %s; want exactly one", handoverNew, n, eventID, handoverOld)
				}
			}
		})
	})

	t.Run("2 the replaced session held two roles, and the new session claims one of them", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: func(role string, seq uint64) error {
			return errors.Join(copiesRelayCopyHeld(handoverRole)(role, seq), copiesRelayCopyHeld(handoverOther)(role, seq))
		}})
		old, _, role := rig.handoverUnread(t, handoverOld, handoverOldProcess, "agent", 1, handoverRole, handoverOther)
		other := role[1]
		rig.stopCleanly(t, handoverOld, handoverOldProcess, old, handoverRole, handoverOther)

		taker := rig.startTaker(t, handoverNew)
		rig.claimReplacing(t, handoverNew, handoverRole, handoverOld, "")
		received := rig.watch(t, taker, handoverNew, handoverRole, role[0])
		rig.awaitHandoverIdle(t, handoverOther)

		t.Run("control: the new session does not receive the role message of the role it did not claim", func(t *testing.T) {
			copiesRequireNone(t, received, handoverNew, other)
		})
		t.Run("case: the role message of the role the new session did not claim waits on that role's kept lane", func(t *testing.T) {
			if n := rig.keptCopiesOf(t, handoverOther, other); n != 1 {
				t.Errorf("role %s's kept lane holds %d copies of %s; want exactly one", handoverOther, n, other)
			}
		})

		next := rig.startTaker(t, handoverNext)
		rig.claim(t, handoverNext, handoverOther, false)
		later := rig.watch(t, next, handoverNext, handoverOther, other)
		t.Run("case: the next session to claim that role receives it", func(t *testing.T) {
			copiesRequireEach(t, later, handoverNext, other)
		})
	})

	t.Run("3 an agent session replaces a person's session that handed its role back and stopped", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(handoverRole)})
		person, direct, role := rig.handoverUnread(t, handoverPerson, handoverPersonProcess, "dispatch", 2, handoverRole)
		rig.stopCleanly(t, handoverPerson, handoverPersonProcess, person, handoverRole)

		agent := startCopiesStandIn(t, rig.url, handoverAgent, answerDone)
		rig.register(t, handoverAgent, map[string]any{"client_instance_id": handoverAgentProcess})
		rig.claimReplacing(t, handoverAgent, handoverRole, handoverPerson, handoverAgentProcess)
		received := rig.watch(t, agent, handoverAgent, handoverRole, append(direct, role...)...)

		t.Run("case: the agent session receives each unread message a person sent the person's session", func(t *testing.T) {
			copiesRequireEach(t, received, handoverAgent, direct...)
		})
		t.Run("case: the agent session receives the person's session's unread role message", func(t *testing.T) {
			copiesRequireEach(t, received, handoverAgent, role...)
		})
	})

	t.Run("4 an agent session names a person's session that is still live", func(t *testing.T) {
		rig := newCopiesRig(t)
		setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{relayCopy: copiesRelayCopyHeld(handoverRole)})
		person, direct, role := rig.handoverUnread(t, handoverPerson, handoverPersonProcess, "dispatch", 1, handoverRole)
		seqs := map[string]uint64{}
		for _, message := range rig.stored(t, contracts.LegacyAgentSubject(handoverPerson)) {
			seqs[message.item.EventID] = message.seq
		}

		agent := startCopiesStandIn(t, rig.url, handoverAgent, answerDone)
		rig.register(t, handoverAgent, map[string]any{"client_instance_id": handoverAgentProcess})
		rig.claimReplacing(t, handoverAgent, handoverRole, handoverPerson, handoverAgentProcess)
		// The outright claim takes the role from the live person's session, whose role message the
		// role's takeover record copies: once it has arrived the claim's passes have run.
		received := rig.watch(t, agent, handoverAgent, handoverRole, role...)

		t.Run("control: while the person's session is live, the agent session receives none of its direct messages", func(t *testing.T) {
			copiesRequireNone(t, received, handoverAgent, direct...)
			for _, eventID := range direct {
				if n := rig.handedOverCopies(t, handoverAgent, eventID); n != 0 {
					t.Errorf("%s's own subject holds %d copies of %s; want none", handoverAgent, n, eventID)
				}
			}
			rig.requireUnacknowledged(t, handoverPerson, seqs, direct...)
		})

		// The person's session stops; the sweep's next pass runs the handover record's passes.
		rig.deregister(t, handoverPerson, "")
		person.close()
		rig.awaitNoConsumer(t, handoverPerson)
		rig.sweepPass()
		after := rig.watch(t, agent, handoverAgent, handoverRole, direct...)
		t.Run("case: once the person's session has stopped, the agent session receives its unread direct message", func(t *testing.T) {
			copiesRequireEach(t, after, handoverAgent, direct...)
		})
	})
}

// mintedKey reports whether key is in the minted form every host reads as naming its event
// (contracts.MintedDedupeKeyPattern), which a takeover's copy carries.
func mintedKey(key string) bool {
	return contracts.DedupeKeyNamesTheUpstreamEvent(contracts.Envelope{DedupeKey: key})
}
