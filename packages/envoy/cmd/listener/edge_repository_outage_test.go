package main

// edge_repository_outage_test.go pins what a session at the edge keeps while the broker cannot
// answer which repositories its projects' issues resolve to. The edge withholds every GitHub
// frame of the session meanwhile (sessionRelays.edgeWithholds), so the outage needs to cost the
// session nothing else: its registration is still renewed by its heartbeat, and its agent
// messages still reach it. A heartbeat names the topics the session's row already holds, and a
// row that holds a GitHub topic is judged against the repository answer, so a heartbeat that the
// outage refuses leaves the session's record to age out: past the session bucket's maximum age
// (session.BucketMaxAge, five minutes, which a deployment that sets no ENVOY_SESSION_MAX_AGE
// keeps) the session is gone, a send to it is answered 404, and no agent message reaches it.

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// While the broker answers 503 for the repositories of the session's projects, the heartbeat of
// a session at the edge whose row follows a GitHub topic renews its session entry, so the
// session stays registered for as long as the outage lasts; and the outage still withholds the
// session's GitHub frames and admits no GitHub topic the last answer did not name. Control: the
// same heartbeat from a session that follows no GitHub topic renews its entry (it does at
// ca2555e4fb), so the renewal this row asks for is the one every other session gets.
func TestAnEdgeSessionStaysRegisteredThroughARepositoryOutage(t *testing.T) {
	rig := newEdgeRig(t)
	if !rig.edgeOn() {
		t.Fatal("this row tests an edge listener: ENVOY_EDGE_PROJECTS is set empty")
	}
	const sessionID, control = "ses_edge_outage", "ses_edge_outage_control"
	const pub, privIssues = "notifications.github.acme.pub.>", "notifications.dispatch.issue.PRIV.>"
	token, controlToken := rig.personToken(sessionID, "PRIV"), rig.personToken(control, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, pub), http.StatusOK, "the registration following acme/pub")
	requireStatus(t, rig.subscribe(t, controlToken, control, privIssues), http.StatusOK, "the control's registration")
	// A write of the entry moves its bucket revision, so the heartbeat's renewal is told from no
	// write by the revision, whatever millisecond the clock reads.
	_, registered := storedEdgeSessionRecord(t, rig, sessionID)
	_, controlRegistered := storedEdgeSessionRecord(t, rig, control)

	rig.repositories.failWith(http.StatusServiceUnavailable)
	heartbeat := rig.subscribe(t, token, sessionID, pub)
	controlBeat := rig.subscribe(t, controlToken, control, privIssues)
	requireStatus(t, controlBeat, http.StatusOK, "the control's heartbeat during the outage")
	if _, got := storedEdgeSessionRecord(t, rig, control); got <= controlRegistered {
		t.Fatalf("the control's heartbeat during the outage left its entry at revision %d, the registration's %d", got, controlRegistered)
	}
	if entry, got := storedEdgeSessionRecord(t, rig, sessionID); got <= registered {
		t.Errorf("the heartbeat of %s during the repository outage (answered %d %s) left its session entry at revision %d (UpdatedAt %d), the registration's %d: "+
			"the entry ages out at the session bucket's maximum age (%s by default) however long the session keeps beating, and the session is then gone, "+
			"so a send to it is 404 and no agent message reaches it, while a session that follows no GitHub topic is renewed",
			sessionID, heartbeat.Code, heartbeat.Body.String(), got, entry.UpdatedAt, registered, session.BucketMaxAge)
	}

	// The outage still costs the session its GitHub frames, and nothing of another project's.
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.outage")
	agent, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != agent.EventID {
		t.Fatalf("the session's next frame is %s on %s, want the agent message: a GitHub event was delivered with no fresh repository answer",
			frames[0].item.EventID, frames[0].item.Topic)
	}
	priv := rig.subscribe(t, token, sessionID, pub, "notifications.github.acme.priv.>")
	if row := bucketInterestRow(t, rig.harness.client, sessionID); slices.Contains(row.Topics, "notifications.github.acme.priv.>") {
		t.Fatalf("a registration naming acme/priv during the outage (answered %d %s) stored it: %v", priv.Code, priv.Body.String(), row.Topics)
	}
}

// A GitHub event of a repository the session follows, stored while no fresh repository answer
// stands, is withheld and not lost: the relay leaves it unacknowledged, and once an answer naming
// the repository stands again the consumer delivers it once more and the relay hands the session
// that very event. While the answer is unknown the session's agent message, stored after it, is
// relayed first, so the event did not reach the session then. The control is an event of the same
// repository relayed before the outage.
func TestAGitHubEventWaitsOutAnUnknownRepositoryAnswer(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_outage"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")
	before := rig.storeGithubEvent(t, "acme/pub", "pr.1")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), before)

	rig.repositories.failWith(http.StatusServiceUnavailable)
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	held := rig.storeGithubEvent(t, "acme/pub", "pr.2")
	agent, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	if frames := standIn.collect(t, 1, 10*time.Second); frames[0].item.EventID != agent.EventID {
		t.Fatalf("the session's next frame is %s on %s, want the agent message: the acme/pub event %s was relayed with no fresh answer",
			frames[0].item.EventID, frames[0].item.Topic, held.EventID)
	}

	rig.repositories.failWith(0)
	rig.tick(t)
	recovered := standIn.collect(t, 1, 2*edgeRepositoryRefresh+10*time.Second)
	if recovered[0].item.EventID != held.EventID {
		t.Fatalf("the session's next frame after the answer stood again is %s on %s, want the acme/pub event %s withheld while it was unknown",
			recovered[0].item.EventID, recovered[0].item.Topic, held.EventID)
	}
}
