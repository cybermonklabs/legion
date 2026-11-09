package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// stop sends sessionID's clean stop as every normal end of a session does, DELETE
// /v1/sessions/<session_id> with query, which carries the stop's conditions or is empty for a stop
// that names none, and returns what the route answered.
func (rig *sessionConsumerReapRig) stop(t *testing.T, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	return stopSession(t, rig.mux, sessionID, query)
}

// stopSession sends sessionID's clean stop to the routes mux holds, with query carrying the stop's
// conditions or empty for a stop that names none, and returns what the route answered. It is a
// func of its own because the rows below drive the same route over two rigs: the one that reads
// the stream's consumers, and the one that relays them to a session.
func stopSession(t *testing.T, mux *http.ServeMux, sessionID, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/sessions/" + sessionID
	if query != "" {
		path += "?" + query
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stop %s: status %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	return recorder
}

// requireNoConsumer fails the test unless the stream holds no consumer for sessionID, when saying
// when it was read.
func (rig *sessionConsumerReapRig) requireNoConsumer(t *testing.T, sessionID, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info, err := rig.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer of %s %s: %+v, %v; want it deleted with the session's record",
			sessionID, when, info, err)
	}
}

// A clean stop takes the session's consumer with the record it deletes. Every normal end of a
// session comes through this route and never through the reaper - a plugin's deregistration, the
// Legion daemon's stop of a role, a /new, a fork, a branch delete - so a consumer this route left
// behind would stand for the whole of the stream's inactive threshold with no session to deliver
// to. Another session's consumer is untouched: the stop names one session.
func TestACleanStopDeletesTheStoppedSessionsConsumer(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	rig.register(t, "ses_clean_stop")
	rig.register(t, "ses_still_here")
	rig.requireConsumer(t, "ses_clean_stop", "after its registration")
	rig.requireConsumer(t, "ses_still_here", "after its registration")

	answer := rig.stop(t, "ses_clean_stop", "")

	if answer.Body.Len() != 0 {
		t.Fatalf("the stop answered with a body: %s", answer.Body.String())
	}
	if entry, err := rig.sessions.Get("ses_clean_stop"); err == nil {
		t.Fatalf("the stopped session's record stands: %+v", entry)
	}
	rig.requireNoConsumer(t, "ses_clean_stop", "after the stop that deleted its record")
	rig.requireConsumer(t, "ses_still_here", "after another session's clean stop")
}

// A clean stop's own restore reports no filter left behind. The stop deletes the session's
// record and then its consumer, and the restore that follows that delete finds no record on
// its first pass: it plans nothing, writes nothing, and the line that names a filter standing
// on the consumer of a session that is gone would name a filter nothing wrote
// (deleteConsumer, repairRegistered, filterWriteSessionGone).
//
// Every clean stop ends this way, and so does every reap, so the guard that line is written
// under is what keeps a WARN off the ordinary end of a session: one logged here would have an
// operator look for messages lost to an older row's filter on every session that ever stopped.
func TestACleanStopsRestoreReportsNoFilterLeftOnTheSessionItTookAway(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_clean_stop_restore"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", "notifications.dispatch.issue.CLEANSTOPRESTORE.>"))
	// The passes of the restore alone: the registration's own repair is behind this hook.
	passes := 0
	rig.deps.consumers.beforeRepairPlan = func() { passes++ }

	stopSession(t, rig.mux, sessionID, "")

	rig.deps.consumers.beforeRepairPlan = nil
	if passes != 1 {
		t.Fatalf("the stop's restore made %d passes, want the one pass that finds the record the stop itself deleted", passes)
	}
	if _, err := rig.deps.sessions.Refresh(context.Background(), sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("the bucket answers %v for %s's record, want no record at all: the restore's pass must have found it deleted", err, sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info, err := rig.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer of %s after its clean stop: %+v, %v; want it deleted with the record, which is why its restore wrote no filter", sessionID, info, err)
	}
	if logs := rig.harness.logs.String(); strings.Contains(logs, repairRecordGoneLine) {
		t.Errorf("the clean stop of %s reports a filter of an older row left on its consumer; the restore wrote none, and this line on a stop that wrote nothing is a WARN on every clean stop and every reap:\n%s", sessionID, logs)
	}
}

// A conditional stop removes the session's consumer exactly when it removed the session's record.
// The daemon's stop of a role names the moment its probe judged the process gone: a registration
// written after that moment is the relaunch's, and it keeps the record and the consumer that
// session is receiving through, while a stop whose moment the stored registration is covered by
// takes both.
func TestAConditionalStopDeletesTheConsumerOnlyWhenItDeletesTheRecord(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	rig.register(t, "ses_relaunched")
	registered, err := rig.sessions.Get("ses_relaunched")
	if err != nil {
		t.Fatalf("read the relaunch's registration: %v", err)
	}
	rig.requireConsumer(t, "ses_relaunched", "after its registration")

	outlived := rig.stop(t, "ses_relaunched", fmt.Sprintf("if_not_registered_after=%d", registered.UpdatedAt-1))

	kept := stopAnswer(t, outlived)
	if kept.Deleted || kept.Reason != "the registration was written after the moment the delete belongs to" {
		t.Fatalf("answer = %+v, want the registration kept with its reason", kept)
	}
	if _, err := rig.sessions.Get("ses_relaunched"); err != nil {
		t.Fatalf("the kept registration is gone: %v", err)
	}
	rig.requireConsumer(t, "ses_relaunched", "after a stop its registration outlived")

	covered := rig.stop(t, "ses_relaunched", fmt.Sprintf("if_not_registered_after=%d", registered.UpdatedAt))

	deleted := stopAnswer(t, covered)
	if !deleted.Deleted || deleted.Reason != "" {
		t.Fatalf("answer = %+v, want the registration deleted", deleted)
	}
	if entry, err := rig.sessions.Get("ses_relaunched"); err == nil {
		t.Fatalf("the stopped session's record stands: %+v", entry)
	}
	rig.requireNoConsumer(t, "ses_relaunched", "after the stop that deleted its record")
}

// stopAnswer is the body a stop that named a condition answered.
func stopAnswer(t *testing.T, recorder *httptest.ResponseRecorder) deleteSessionResponse {
	t.Helper()
	var answer deleteSessionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&answer); err != nil {
		t.Fatalf("decode the stop's answer: %v", err)
	}
	return answer
}

// keptStopSilence is how long the session of a kept stop is watched for a frame it must not
// receive: one it already holds, sent to it again. A stop that deleted the consumer leaves the
// relay pulling one the stream no longer holds, and the relay restarts at the ack floor
// relayRetryFirst after that pull failed, so this window is several times that wait. Nothing
// sends the session a frame inside it otherwise: the frame it holds is answered, so the relay
// does not take it for unreachable (relayReceiptWait), and the consumer's ack wait is a minute
// (bus.SessionConsumerAckWait).
const keptStopSilence = 4 * relayRetryFirst

// A conditional stop that keeps the session's record leaves that session's consumer exactly as it
// found it, whichever of the stop's conditions kept the record. The record stands, so the session
// is still there, and the consumer is what it receives through: the stream must hold the very
// consumer it held before the stop, the frames it has sent and has no ack for still in flight,
// and the next message published for the session must reach it through that consumer.
//
// Deleting a kept consumer and giving it back is not the same thing, which is why this row reads
// the consumer's own creation and what the session receives rather than only whether a consumer
// exists: a delete restores the consumer of every session whose record is in the bucket
// (sessionDelivery.deleteConsumer), and a kept record is one, so a stop that deleted it leaves
// a consumer behind it. The replacement is a new consumer created at the sequence after the ack
// floor, so every frame the session holds unacked is sent to it again, and the relay pulling the
// consumer that went relays nothing until its retry.
func TestAConditionalStopThatKeepsTheRecordLeavesTheConsumerAlone(t *testing.T) {
	const storedInstance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	const stoppingInstance = "0b7a4c1e9d3f5a2b0b7a4c1e9d3f5a2b"
	for _, kept := range []struct {
		name         string
		registration func(t *testing.T, sessionID string) string
		query        func(stored session.SessionEntry) string
		reason       string
	}{
		{
			name: "the record is another client instance's",
			registration: func(t *testing.T, sessionID string) string {
				return instanceRegistration(t, sessionID, "epoch-1", storedInstance)
			},
			query:  func(session.SessionEntry) string { return "client_instance_id=" + stoppingInstance },
			reason: "the registration belongs to another client instance",
		},
		{
			name: "the record was written after the moment the stop belongs to",
			registration: func(t *testing.T, sessionID string) string {
				return consumerRegistration(sessionID, "epoch-1")
			},
			query: func(stored session.SessionEntry) string {
				return fmt.Sprintf("if_not_registered_after=%d", stored.UpdatedAt-1)
			},
			reason: "the registration was written after the moment the delete belongs to",
		},
	} {
		t.Run(kept.name, func(t *testing.T) {
			rig := newRelayRig(t)
			sessionID := "ses_kept_stop_" + id.New()
			agent := contracts.AgentSubject(sessionID)
			standIn := startSessionStandIn(t, rig.url, sessionID, answerReceived)
			rig.subscribe(t, kept.registration(t, sessionID))
			held, _ := rig.storeEvent(t, agent)
			requireFrames(t, standIn.collect(t, 1, 10*time.Second), held)
			created := consumerCreatedAt(t, rig, sessionID, "before the stop")

			answer := stopAnswer(t, stopSession(t, rig.mux, sessionID, kept.query(rig.entry(t, sessionID))))

			if answer.Deleted || answer.Reason != kept.reason {
				t.Fatalf("answer = %+v, want the registration kept for the reason %q", answer, kept.reason)
			}
			if _, err := rig.harness.sessions.Get(sessionID); err != nil {
				t.Fatalf("the kept registration of %s is gone: %v", sessionID, err)
			}
			standIn.none(t, keptStopSilence)

			next, _ := rig.storeEvent(t, agent)

			requireFrames(t, standIn.collect(t, 1, 10*time.Second), next)
			if again := consumerCreatedAt(t, rig, sessionID, "after the stop that kept its record"); !again.Equal(created) {
				t.Fatalf("the consumer of %s was created at %s and the stop that kept its record left one created at %s; want the session's own consumer untouched",
					sessionID, created, again)
			}
		})
	}
}

// A clean stop deletes the consumer of a session whose caller hung up the moment its record was
// gone. That caller is the ordinary one: a plugin sends its deregistration and exits, and the
// connection it drops a moment later cancels the request's context. The consumer delete runs on a
// context detached from the request for that reason (context.WithoutCancel in
// deleteSessionConsumer), and the cancellation protects nothing by then: the record is already
// deleted, so the session is gone whatever happens next, while a consumer left behind stands for
// the whole of the stream's inactive threshold with no session to deliver to.
//
// The hang-up is placed rather than raced. The test holds the session's lock, which the consumer
// delete takes before its first call (sessionDelivery.deleteConsumer), so the handler reaches that
// lock only once it has deleted the record, and the request's context is cancelled there, between
// the record delete and the consumer delete the route exists to make.
func TestACleanStopDeletesTheConsumerOfACallerThatHungUpOnceItsRecordWasGone(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	const sessionID = "ses_hung_up"
	rig.register(t, sessionID)
	rig.requireConsumer(t, sessionID, "after its registration")
	hangUp, hungUp := context.WithCancel(context.Background())
	defer hungUp()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/sessions/"+sessionID, nil).WithContext(hangUp)
	unlock := rig.delivery.locks.lock(sessionID)
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.mux.ServeHTTP(recorder, request)
	}()
	awaitDeleteWaitingOrDone(t, &rig.delivery.locks, sessionID, served, 2*time.Second)
	rig.waitForRecordDeleted(t, sessionID)

	hungUp()
	unlock()
	<-served

	if recorder.Code != http.StatusOK {
		t.Fatalf("the stop of %s answered %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	rig.requireNoConsumer(t, sessionID, "after the stop whose caller hung up on it")
}

// A relay whose session was deregistered tells an operator that nobody is delivered the
// messages it held, and never that they are delivered again at the consumer's next pull. The
// stop deletes the session's record, which cancels the relay (sessionRelays.apply), and that
// same stop deletes the session's consumer (deleteSessionConsumer), so the naks of the
// hand-over reach a consumer the server no longer holds and it drops them: a nak is a publish
// the server answers nothing to, so every one of them is reported returned while nothing
// delivers those messages, at once or after the consumer's ack wait. This is where most relays
// end, a clean stop being where most sessions end, so the line that promises a further
// delivery would have an operator wait for one that never comes. That line names the lapse
// beside this case, the run having nothing to tell the two apart by (relayHandOverLines), and
// TestARelayHandsOverALapsedSessionsMessagesToTheConsumerThatStands holds the half of it this
// row does not.
//
// The row reads the line's words, its reason and its count (requireHandOverOfAGoneSession),
// since those are what a reader and an alert key on. What is its own is the absence of either
// unqualified promise of a further delivery from the whole log, and the consumer after the stop,
// which is the fact behind the half of the line this case falls on.
func TestACleanStopsRelayHandOverNeverPromisesAFurtherDelivery(t *testing.T) {
	rig, sessionID, _, _ := relayHoldingOneMessage(t, "ses_stopped_handover_")

	stopSession(t, rig.mux, sessionID, "")

	// The promises are read from the log the hand-over line was read from, taken once that line
	// is in it: before that the run has written no line at all, and a missing promise of a
	// further delivery would say nothing.
	logs := requireHandOverOfAGoneSession(t, rig, sessionID, "stopped")
	const againAtOnce = "session relay stopped; it gave the server back the messages it held, so they are delivered again at once rather than after the consumer's ack wait"
	const afterTheAckWait = "session relay stopped; it could not give the server back every message it held, and each one it did not is delivered again only after the consumer's ack wait"
	for _, promise := range []string{againAtOnce, afterTheAckWait} {
		if strings.Contains(logs, promise) {
			t.Fatalf("the hand-over of the stopped session %s reports its messages delivered again; the stop deleted the consumer they were naked into, so nothing delivers them:\n%s",
				sessionID, logs)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info, err := rig.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer of %s after the stop: %+v, %v; want it deleted with the session's record, which is why nothing delivers the naked messages",
			sessionID, info, err)
	}
}

// handOverLineNaming is the one hand-over record of sessionID in logs, whatever level it was
// written at: the line a cancelled run writes once it has given back what it held
// (sessionRelays.relayNATS). It fails the test unless there is exactly one, since a run writes
// one and a row that read the first of several would say nothing about the others.
func handOverLineNaming(t *testing.T, logs, sessionID string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["session_id"] != sessionID {
			continue
		}
		if msg, ok := record["msg"].(string); ok && strings.HasPrefix(msg, "session relay stopped;") {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d hand-over lines name session %s, want exactly 1:\n%s", len(found), sessionID, logs)
	}
	return found[0]
}

// handOverRecordGoneLine is what the one hand-over line of a cancelled relay reads for a session
// the bucket holds no record of, whichever way that record went - a stop, a reap or a lapse
// (relayHandOverLines, relayCancelSessionGone). The two rows hold this copy of the wording rather
// than reading relayHandOverLines, so a change to what the listener writes is read against what
// each of them says the line means.
const handOverRecordGoneLine = "session relay stopped; the session has no record, so nothing relays it; the messages it held stand on the session's consumer, which delivers them when the session registers again, and nobody is delivered them where a stop or a reap deleted that consumer with the record"

// relayHoldingOneMessage registers a session in consumer mode on a new relay rig and leaves its
// relay holding one message delivered and unacked, which is where both hand-over rows start: the
// session's record goes next, and that one message is what the hand-over line counts returned.
// A nil answer leaves the stand-in silent, so the frame stays outstanding and its message stays
// delivered and unacked, and the rig's receipt wait is longer than either row, so the relay
// cannot pause, and so forget the frame it holds, before the record goes. It returns the rig, the
// session, its stand-in and the message the relay holds.
func relayHoldingOneMessage(t *testing.T, prefix string) (*relayRig, string, *sessionStandIn, contracts.Envelope) {
	t.Helper()
	rig := newRelayRigReceipt(t, relayPauseFirst, relayPauseMax, 30*time.Second)
	sessionID := prefix + id.New()
	standIn := startSessionStandIn(t, rig.url, sessionID, nil)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	stored, _ := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), stored)
	awaitAckState(t, rig.consumers, sessionID, 1, 0)
	return rig, sessionID, standIn, stored
}

// requireHandOverOfAGoneSession fails the test unless the one hand-over line sessionID's
// cancelled relay wrote is the line of a session with no record: its words, the reason
// session_gone, and the one message the relay held counted returned. went is how that record
// went, which the failures name. It answers the log it read the line from, taken once the line is
// in it, so a row reads what else the run wrote from the same log.
func requireHandOverOfAGoneSession(t *testing.T, rig *relayRig, sessionID, went string) string {
	t.Helper()
	awaitLogLine(t, rig, "session relay stopped;", 15*time.Second)
	logs := rig.harness.logs.String()
	report := handOverLineNaming(t, logs, sessionID)
	if report["msg"] != handOverRecordGoneLine {
		t.Fatalf("the hand-over line of the %s session %s reads %q, want %q", went, sessionID, report["msg"], handOverRecordGoneLine)
	}
	if report["reason"] != "session_gone" {
		t.Fatalf("the hand-over line of the %s session %s names reason %#v, want session_gone: the cache holds no record of it", went, sessionID, report["reason"])
	}
	// A number of a log record decodes from JSON as a float64, so the count reads as one here.
	if returned, ok := report["returned"].(float64); !ok || returned != 1 {
		t.Fatalf("the hand-over line of the %s session %s counts returned %#v, want the number 1: the relay held one message:\n%v", went, sessionID, report["returned"], report)
	}
	return logs
}

// answerReceived answers a frame received and nothing more: the session holds it and has not
// written it, so the consumer still holds that message for an ack. A frame answered at all is one
// the relay does not take for unreachable, so the session goes on being relayed.
func answerReceived(relayedFrame) []string { return []string{relayReplyReceived} }

// instanceRegistration is the registration of a session in consumer mode at epoch whose client
// minted the instance id instanceID, as every client that mints one sends on every registration:
// what a conditional stop naming another instance compares against (session.InstanceMatches).
func instanceRegistration(t *testing.T, sessionID, epoch, instanceID string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(consumerRegistration(sessionID, epoch)), &body); err != nil {
		t.Fatalf("decode the registration of %s: %v", sessionID, err)
	}
	body["client_instance_id"] = instanceID
	registration, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the registration of %s: %v", sessionID, err)
	}
	return string(registration)
}

// consumerCreatedAt is when the stream created sessionID's consumer, read through the test's own
// view of the session consumers, when saying when it was read. A delete and the restore that
// follows it give the session a consumer created later; nothing else the listener does to a
// consumer changes it.
func consumerCreatedAt(t *testing.T, rig *relayRig, sessionID, when string) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := rig.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("the consumer of %s %s: %v; want the stream holding it", sessionID, when, err)
	}
	return info.Created
}
