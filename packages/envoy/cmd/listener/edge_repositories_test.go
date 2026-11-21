package main

// edge_repositories_test.go holds the edge listener's GitHub rows on a real bus: which
// repositories' events a session follows and receives, read from the broker's answer for its
// token's projects (edge_repositories.go), how the answer's refresh, its failure and its case
// decide that, and what a registration is told while no answer can be read. Each row runs under
// the same edge-off control as the rows of edge_mode_test.go (edgeTestProjects).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// An edge session may follow the GitHub events of each repository its project's issues
// resolve to, read fresh through the broker at the registration with the listener's own account,
// and an event of each reaches it.
func TestAnEdgeRelaysTheGitHubEventsOfItsProjectsRepositories(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_github"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	before := rig.repositories.calls.Load()
	topics := []string{"notifications.github.acme.pub.>", "notifications.github.acme.two.>"}
	answer := rig.subscribe(t, token, sessionID, topics...)
	requireStatus(t, answer, http.StatusOK, fmt.Sprintf("a registration following %v", topics))
	if rig.repositories.calls.Load() == before {
		t.Fatalf("the registration naming %v read no repository answer; the answer is read fresh at each one", topics)
	}
	if got := rig.repositories.lastBearer(); got != "Bearer "+edgeAccountToken {
		t.Fatalf("the repository read presented %q, want the listener's own account", got)
	}
	pub := rig.storeGithubEvent(t, "acme/pub", "pr.1")
	two := rig.storeGithubEvent(t, "acme/two", "issue.2")
	requireFrames(t, standIn.collect(t, 2, 10*time.Second), pub, two)
}

// A GitHub interest in a repository no issue of the project resolves to, or in no single
// repository, is refused 403 repository_not_in_project, and such a repository's event never
// reaches the session.
func TestAnEdgeRefusesAnotherRepositorysEvents(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_other_repository"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	for _, topic := range []string{"notifications.github.acme.priv.>", "notifications.github.acme.>", "notifications.github.>"} {
		requireTopicRefusal(t, rig.subscribe(t, token, sessionID, topic), "repository_not_in_project", topic)
	}
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "an interest in acme/pub")
	rig.storeGithubEvent(t, "acme/priv", "pr.3")
	pub := rig.storeGithubEvent(t, "acme/pub", "pr.4")
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.Topic != pub.Topic {
		t.Fatalf("the session's first frame is %s, want acme/pub's: the event of acme/priv reached it", frames[0].item.Topic)
	}
	requireFrames(t, frames, pub)
	// Nothing of acme/priv follows either: an agent message stored after both is the next frame.
	agent, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), agent)
}

// A repository that leaves the project's answer stops reaching the session at the first
// refresh that reads the new answer, its event acknowledged undelivered and counted, and the
// session's interest in it is gone at the refresh after. The production refresh runs every 30 s
// and an answer lives 60 s, so both happen within 60 s of the change.
func TestARepositoryThatLeavesTheProjectStopsReachingTheEdge(t *testing.T) {
	if edgeRepositoryRefresh != 30*time.Second || edgeRepositoryAnswerLife != 60*time.Second {
		t.Fatalf("the refresh runs every %s and an answer lives %s, want 30 s and 60 s", edgeRepositoryRefresh, edgeRepositoryAnswerLife)
	}
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_leaving"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>", "notifications.github.acme.two.>"), http.StatusOK, "the registration")
	first := rig.storeGithubEvent(t, "acme/two", "pr.1")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	rig.repositories.answer("PRIV", "acme/pub")
	rig.tick(t)
	left := rig.storeGithubEvent(t, "acme/two", "pr.2")
	stays := rig.storeGithubEvent(t, "acme/pub", "pr.3")
	// The frames go in stream order, so the event after it arriving first is the proof the one
	// before it was not relayed.
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID == left.EventID {
		t.Fatalf("the acme/two event %s stored after acme/two left the answer reached the session", left.EventID)
	}
	requireFrames(t, frames, stays)
	if got := rig.withheld(t, "not_in_project"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="not_in_project"} = %d, want 1: the acme/two event withheld`, got)
	}

	rig.tick(t)
	recorder := rig.call(t, http.MethodGet, token, "/v1/registry/"+sessionID, "")
	requireStatus(t, recorder, http.StatusOK, "GET /v1/registry/"+sessionID)
	var info listenerwire.SessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	for _, topic := range info.Topics {
		if strings.HasPrefix(topic, "notifications.github.acme.two") {
			t.Fatalf("the session still follows %s after acme/two left the answer: %v", topic, info.Topics)
		}
	}
	if !slices.Contains(info.Topics, "notifications.github.acme.pub.>") {
		t.Fatalf("the session no longer follows acme/pub, which stayed: %v", info.Topics)
	}
}

// While no repository answer younger than 60 s stands, a registration naming a GitHub topic
// is 503 repositories_unknown, every GitHub frame is acknowledged undelivered with the gap mark
// on the next frame, named Envoy-Gap-Reason: repository_withheld so a client does not read it as
// the stream's retention gap, and every frame that is not GitHub's flows as before.
func TestAnUnknownRepositoryAnswerWithholdsOnlyGitHubFrames(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_unknown"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")

	rig.repositories.failWith(http.StatusServiceUnavailable)
	requireEdgeRefusal(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusServiceUnavailable, "repositories_unknown",
		"a registration naming acme/pub while the answer is 503")
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.5")
	agent, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != agent.EventID {
		t.Fatalf("the session's next frame is %s on %s, want the agent message: the acme/pub event was delivered with an answer %s old",
			frames[0].item.EventID, frames[0].item.Topic, edgeRepositoryAnswerLife+time.Second)
	}
	if got := frames[0].msg.Header.Get(relayHeaderGap); got != "true" {
		t.Fatalf("the frame after the withheld GitHub event carries %s %q, want true", relayHeaderGap, got)
	}
	if got := frames[0].msg.Header.Get("Envoy-Gap-Reason"); got != "repository_withheld" {
		t.Fatalf("the frame after the withheld GitHub event carries Envoy-Gap-Reason %q, want repository_withheld: a gap with no reason reads as the stream's retention gap", got)
	}
	if got := rig.withheld(t, "answer_unknown"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="answer_unknown"} = %d, want 1`, got)
	}
}

// The gap mark of a GitHub event the edge withheld while no fresh repository answer stood outlives
// the relay run that withheld it: the session registers again under a new delivery epoch before
// its next frame, which restarts its relay at the ack floor, past the withheld event, and that
// frame still carries Envoy-Gap: true with Envoy-Gap-Reason: repository_withheld. Control: with
// edge mode off nothing is withheld and no frame carries the mark.
func TestARepositoryGapOutlivesARelayRestart(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_gap_epoch"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")
	first, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	rig.repositories.failWith(http.StatusServiceUnavailable)
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.1")
	testwait.Eventually(t, "the edge to withhold the acme/pub event while no answer stands", func() bool {
		return rig.withheld(t, "answer_unknown") >= 1
	})
	rig.repositories.failWith(0)
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2", "notifications.github.acme.pub.>")),
		http.StatusOK, "the registration under a new delivery epoch")
	next, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, next)
	if gap, reason := frames[0].msg.Header.Get(relayHeaderGap), frames[0].msg.Header.Get(relayHeaderGapReason); gap != "true" || reason != relayGapRepositoryWithheld {
		t.Fatalf("the first frame after the withheld GitHub event and the relay's restart carries %s %q and %s %q, want true and %s",
			relayHeaderGap, gap, relayHeaderGapReason, reason, relayGapRepositoryWithheld)
	}
}

// The gap mark of a GitHub event the edge withheld while no fresh repository answer stood
// outlives a move of the session's relay to another listener: the mark is kept on the session's
// entry, a listener that took the session over drops what it noted in its own process, and the
// next listener to relay the session - here this one again, with nothing of the mark left in its
// process - sends it on the session's next frame and clears it from the entry. Control: with the
// mark kept in the relaying process alone, the frame after the move carries none.
func TestARepositoryGapOutlivesAMoveToAnotherListener(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_gap_moved"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")
	first, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	rig.repositories.failWith(http.StatusServiceUnavailable)
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.1")
	// The relay marks the entry before it acknowledges and counts the withheld event, so the row
	// waits for both: the move below writes the entry over the marked one, as another listener's
	// registration does.
	testwait.Eventually(t, "the edge to withhold the acme/pub event and mark the session's entry", func() bool {
		return rig.withheld(t, "answer_unknown") >= 1 && storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap != ""
	})

	// Another listener registers the session: its entry names that listener, so this one's relay
	// stops and drops what its process noted.
	moved := storedEdgeSessionEntry(t, rig, sessionID)
	moved.MachineID = "another-listener"
	if err := rig.deps.sessions.Put(sessionID, moved); err != nil {
		t.Fatalf("move %s to another listener: %v", sessionID, err)
	}
	testwait.Eventually(t, "this listener's relay to drop the session", func() bool {
		rig.relays.repositoryGapsMu.Lock()
		defer rig.relays.repositoryGapsMu.Unlock()
		_, noted := rig.relays.repositoryGaps[sessionID]
		return !noted
	})

	rig.repositories.failWith(0)
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-2", "notifications.github.acme.pub.>")),
		http.StatusOK, "the registration that brings the session back to this listener")
	next, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, next)
	if gap, reason := frames[0].msg.Header.Get(relayHeaderGap), frames[0].msg.Header.Get(relayHeaderGapReason); gap != "true" || reason != relayGapRepositoryWithheld {
		t.Fatalf("the first frame after the withheld GitHub event and the move carries %s %q and %s %q, want true and %s",
			relayHeaderGap, gap, relayHeaderGapReason, reason, relayGapRepositoryWithheld)
	}
	testwait.Eventually(t, "the sent mark to be cleared from the session's entry", func() bool {
		return storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap == ""
	})
}

// A registration keeps the gap mark another listener set on the session's entry after this
// listener last read it: the other listener's relay withheld a GitHub event and marked the entry
// (session.SessionRegistry.SetRepositoryGap) while this listener's copy of the session bucket
// still held the entry without the mark, and this listener's next registration of the session,
// built over that copy, leaves the mark on the stored entry for the next listener's frame to
// carry. Here this listener's copy is held behind the bucket by stopping its watch, so the
// registration is made over an entry the bucket no longer holds. The write is every listener's,
// so the row passes with edge mode off by design. Control: with the entry written
// unconditionally (session.SessionRegistry.PutWithin as it was before this row), the mark is lost
// and the row fails.
func TestARegistrationKeepsAGapMarkAnotherListenerSetSinceItsRead(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_gap_raced"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the registration")
	registered := storedEdgeSessionEntry(t, rig, sessionID)

	rig.deps.sessions.StopWatch()
	other, err := session.OpenSessionRegistry(t.Context(), rig.harness.client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the other listener's session registry: %v", err)
	}
	t.Cleanup(other.StopWatch)
	if err := other.SetRepositoryGap(t.Context(), sessionID, "the-other-listeners-mark"); err != nil {
		t.Fatalf("mark %s's entry from the other listener: %v", sessionID, err)
	}
	if storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap == "" {
		t.Fatalf("UNKNOWN: the other listener's mark of %s is not on the stored entry", sessionID)
	}
	if cached, err := rig.deps.sessions.Get(sessionID); err != nil || cached.RepositoryGap != "" {
		t.Fatalf("UNKNOWN: this listener's copy of %s's entry reads %+v (%v), not the entry without the mark, so the registration below is not made over a stale read", sessionID, cached, err)
	}

	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the registration after the other listener's mark")
	stored := storedEdgeSessionEntry(t, rig, sessionID)
	if stored.UpdatedAt <= registered.UpdatedAt {
		t.Fatalf("UNKNOWN: %s's stored entry %+v was not written by the second registration (UpdatedAt %d, first registration's %d)", sessionID, stored, stored.UpdatedAt, registered.UpdatedAt)
	}
	if stored.RepositoryGap != "the-other-listeners-mark" {
		t.Fatalf("after this listener's registration %s's stored entry is %+v: the gap mark the other listener set since this listener's read is lost", sessionID, stored)
	}
}

// A GitHub event the edge withholds while no fresh repository answer stands is acknowledged only
// once the session's entry carries the gap mark it owes (session.SessionRegistry.SetRepositoryGap):
// a mark write that fails gives the event back to the server, as the takeover gives back a frame
// whose copy did not store, so a redelivery writes the mark once the entry can be written, and a
// move or a restart before the session's next frame finds it. Here the mark write fails because
// the bucket holds a value no read can decode under the session's key, behind this listener's
// copy, which still holds the session's projects (its watch is stopped), and the entry is then
// written whole again. The withheld event is counted once, when it is acknowledged. Control: with
// the event acknowledged before the mark is written (sessionRelays.edgeWithholds before this row),
// the consumer's ack floor passes the event while the entry carries no mark, and the row fails.
func TestAWithheldEventIsAcknowledgedOnlyOnceItsGapMarkIsStored(t *testing.T) {
	requireWithheldEventGivenBackUntilMarked(t, "ses_edge_gap_durable", func(bucket natsgo.KeyValue, sessionID string) error {
		_, err := bucket.Put(sessionID, []byte(`{"machine_id":7}`))
		return err
	})
}

// A GitHub event the edge withholds while no fresh repository answer stands is given back to the
// server, as for a mark write that failed, while the bucket holds no entry of the session at all:
// a mark kept in this process alone is lost with a move, and the delete that took the entry
// records a resume floor past an acknowledged event, while the session's next entry carries no
// mark. Here the entry is deleted behind this listener's copy, which still holds the session's
// projects (its watch is stopped), and is then written whole again: the redelivery marks it, and
// only then is the event acknowledged. Control: with an entry the bucket does not hold answered
// as marked (session.SessionRegistry.SetRepositoryGap before this row), the consumer's ack floor
// passes the event while no entry carries its mark, and the row fails.
func TestAWithheldEventIsGivenBackWhileTheSessionHasNoEntry(t *testing.T) {
	requireWithheldEventGivenBackUntilMarked(t, "ses_edge_gap_no_entry", func(bucket natsgo.KeyValue, sessionID string) error {
		return bucket.Delete(sessionID)
	})
}

// requireWithheldEventGivenBackUntilMarked is the give-back rows' body: sessionID registers at the
// edge following acme/pub, spoil makes the bucket's entry of the session one the relay cannot
// mark, behind this listener's copy (its watch is stopped), and an acme/pub event is stored while
// no repository answer stands. The event is given back and not counted while the entry cannot be
// marked, and is marked, acknowledged and counted once after the entry is written whole again.
func requireWithheldEventGivenBackUntilMarked(t *testing.T, sessionID string, spoil func(bucket natsgo.KeyValue, sessionID string) error) {
	t.Helper()
	rig := newEdgeRig(t)
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")
	first, firstSeq := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)
	consumers := observeConsumers(t, rig.url)
	consumer := func() *jetstream.ConsumerInfo {
		t.Helper()
		info, err := consumers.Info(t.Context(), sessionID)
		if err != nil {
			t.Fatalf("read %s's consumer: %v", sessionID, err)
		}
		return info
	}
	testwait.Eventually(t, "the first frame to be acknowledged", func() bool { return consumer().AckFloor.Stream >= firstSeq })

	bucket, err := rig.harness.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	whole, err := bucket.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's entry: %v", sessionID, err)
	}
	rig.deps.sessions.StopWatch()
	if err := spoil(bucket, sessionID); err != nil {
		t.Fatalf("make %s's stored entry one no mark can be written on: %v", sessionID, err)
	}

	rig.repositories.failWith(http.StatusServiceUnavailable)
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.1")
	var seq uint64
	testwait.Eventually(t, "the relay to take the acme/pub event and acknowledge it or give it back", func() bool {
		info := consumer()
		if info.Delivered.Stream <= firstSeq {
			return false
		}
		seq = info.Delivered.Stream
		return info.AckFloor.Stream >= seq || info.NumRedelivered >= 1
	})
	if info := consumer(); info.AckFloor.Stream >= seq {
		t.Fatalf("%s's consumer acknowledged the withheld acme/pub event (seq %d, ack floor %d) while its entry could not carry the gap mark: a move before the next frame loses the mark",
			sessionID, seq, info.AckFloor.Stream)
	}
	if got := rig.withheld(t, "answer_unknown"); got > 0 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="answer_unknown"} = %d while the event is given back, want it counted only when it is acknowledged`, got)
	}

	if _, err := bucket.Put(sessionID, whole.Value()); err != nil {
		t.Fatalf("write %s's entry whole again: %v", sessionID, err)
	}
	testwait.Eventually(t, "the redelivered event to mark the entry and be acknowledged", func() bool {
		return storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap != "" && consumer().AckFloor.Stream >= seq
	})
	if got := rig.withheld(t, "answer_unknown"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="answer_unknown"} = %d, want 1: the event is withheld once, however often it was given back`, got)
	}
}

// Dispatch answers a repository in lower case and the intake keeps GitHub's case, so the
// edge compares owner and name ignoring case: an interest in Acme/Pub is admitted through an
// answer of acme/pub, and the event the intake publishes for Acme/Pub reaches the session.
func TestAnEdgeComparesRepositoriesIgnoringCase(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_case"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	before := rig.repositories.calls.Load()
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.Acme.Pub.>"), http.StatusOK, "an interest in Acme/Pub")
	if rig.repositories.calls.Load() == before {
		t.Fatal("the registration naming Acme/Pub read no repository answer; the answer is read fresh at each one")
	}
	event := rig.storeGithubEvent(t, "Acme/Pub", "pr.9")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), event)
}

// A registration the edge refuses 503 repositories_unknown is told the listener's own sentence:
// nothing of what the broker answered reaches the client, its error code and its text among it.
// It holds both for a GitHub topic the registration names and for one the session's row holds,
// which the edge reads the answer for as well.
func TestAnEdgeRepositoriesRefusalCopiesNoBrokerText(t *testing.T) {
	rig := newEdgeRig(t)
	const named, stored = "ses_edge_unknown_named", "ses_edge_unknown_stored"
	rig.placeElsewhereFollowing(t, stored, []string{}, "notifications.github.acme.pub.>")
	rig.repositories.failWith(http.StatusServiceUnavailable)
	for _, call := range []struct {
		session string
		topics  []string
	}{
		{named, []string{"notifications.github.acme.pub.>"}},
		{stored, nil},
	} {
		recorder := rig.subscribe(t, rig.personToken(call.session, "PRIV"), call.session, call.topics...)
		requireEdgeRefusal(t, recorder, http.StatusServiceUnavailable, "repositories_unknown", fmt.Sprintf("%s's registration naming %v while the broker answers 503", call.session, call.topics))
		for _, broker := range []string{"REPOSITORIES_UNKNOWN", "the stand-in answers"} {
			if strings.Contains(recorder.Body.String(), broker) {
				t.Errorf("%s's 503 copies the broker's answer (%q): %s", call.session, broker, recorder.Body.String())
			}
		}
	}
}

// A subject writes a dot in a repository's name as an underscore, so acme/a.b and acme/a_b
// publish on one subject (notifications.github.acme.a_b): the session follows it through an
// answer that names acme/a.b, and the event of acme/a_b on that subject is withheld
// not_in_project, while the event of acme/a.b reaches it. The relay holds each event to the
// repository its own payload names.
func TestAnEdgeRelaysOnlyTheEventsOfTheRepositoryItsAnswerNames(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_dotted"
	rig.repositories.answer("PRIV", "acme/a.b")
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.a_b.>"), http.StatusOK, "an interest in acme/a.b, which its subject writes acme.a_b")
	other := rig.storeGithubEvent(t, "acme/a_b", "pr.1")
	own := rig.storeGithubEvent(t, "acme/a.b", "pr.2")
	if otherPrefix, ownPrefix := contracts.GithubRepositoryPrefix("acme", "a_b"), contracts.GithubRepositoryPrefix("acme", "a.b"); otherPrefix != ownPrefix {
		t.Fatalf("UNKNOWN: acme/a_b's events are under %s and acme/a.b's under %s, not one subject", otherPrefix, ownPrefix)
	}
	frames := standIn.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID == other.EventID {
		t.Fatalf("the event of acme/a_b reached a session whose answer names acme/a.b alone")
	}
	requireFrames(t, frames, own)
	if got := rig.withheld(t, "not_in_project"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="not_in_project"} = %d, want 1: the acme/a_b event withheld`, got)
	}
}

// A registration the edge refuses 503 for want of a repository answer for the GitHub topic it
// names renews none of the session's claims, and records that against each role the session
// holds, as the 503 for a stored GitHub topic does, so POST /v1/roles/set reads the claim as one
// its holder did not renew.
func TestAnEdgeRepositoriesRefusalRecordsTheUnrenewedClaims(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID, role = "ses_edge_unknown_renewal", "legion-priv-priv-8-tester"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID), http.StatusOK, "the session's registration")
	requireStatus(t, rig.call(t, http.MethodPost, token, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role)),
		http.StatusOK, "the claim of "+role)
	rig.repositories.failWith(http.StatusServiceUnavailable)
	requireEdgeRefusal(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusServiceUnavailable, "repositories_unknown",
		"a registration naming acme/pub while the broker answers 503")
	if _, recorded := rig.deps.registry.RenewalFailedWithin(role, sessionID, time.Minute); !recorded {
		t.Fatalf("the 503 recorded no renewal failure of %s for %s: the claim reads as renewed though the registration renewed nothing", role, sessionID)
	}
}

// edgeBrokerTransport answers each request the edge's repository reader sends with answer, on the
// reader's own goroutine, as the broker would over the network.
type edgeBrokerTransport func(*http.Request) (*http.Response, error)

func (f edgeBrokerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// One project whose broker read hangs holds back no other project's read: a refresh reads each
// served project on its own, so PRIV's answer is read while SLOW's read is still waiting, and
// PRIV's answer is fresh when the pass ends. Read one after the other, PRIV's would wait for
// SLOW's to time out, and with enough slow projects ahead of it would age past its life.
func TestAnEdgeRefreshReadsEachProjectOnItsOwn(t *testing.T) {
	var slowWaiting atomic.Int32
	slowEntered := make(chan struct{})
	release := make(chan struct{})
	var readWhileSlowWaited atomic.Bool
	privRead := make(chan struct{})
	transport := edgeBrokerTransport(func(request *http.Request) (*http.Response, error) {
		project := request.URL.Query().Get("project")
		if project == "SLOW" {
			slowWaiting.Add(1)
			close(slowEntered)
			defer slowWaiting.Add(-1)
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
		}
		// This read waits for SLOW's to have begun, whichever order the reads start in, and then
		// sees whether that one is still waiting. A read that ends before SLOW's begins, its
		// time spent while the refresh runs one read at a time, saw no read beside it.
		defer close(privRead)
		select {
		case <-slowEntered:
			readWhileSlowWaited.Store(slowWaiting.Load() == 1)
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		body := fmt.Sprintf(`{"project":%q,"repositories":["acme/pub"]}`, project)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	reader := newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), []string{"SLOW", "PRIV"},
		newEdgeWithheldCounter(metrics.New()), logging.New("test"))
	reader.client = &http.Client{Transport: transport}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader.refresh(context.Background())
	}()
	select {
	case <-privRead:
	case <-time.After(30 * time.Second):
		t.Fatal("the refresh read no answer of PRIV within 30 s")
	}
	close(release)
	<-done
	if !readWhileSlowWaited.Load() {
		t.Fatal("the refresh read PRIV's answer only once SLOW's read had ended: a project whose read hangs holds back every project after it")
	}
	if repositories, complete := reader.standing([]string{"PRIV"}); !complete || !slices.Equal(repositories, []string{"acme/pub"}) {
		t.Fatalf("after the refresh PRIV stands %v (complete %t), want [acme/pub]", repositories, complete)
	}
}

// A broker whose repository read never answers costs the session no more than one that answers
// 503 at once: the heartbeat of a session following a GitHub topic is refused 503
// repositories_unknown, and its session entry is still renewed, since the read ends
// edgeRegistrationReserve before the registration's budget does. The broker stand-in answers only
// when the read's own context ends, so the read takes all the time it is given, and the renewal is
// told from no write by the entry's revision, with no clock read. Control: with the read given the
// whole budget, the renewal runs on a context that has ended and the entry is not renewed.
func TestAHungRepositoryReadStillRenewsTheEdgeSession(t *testing.T) {
	rig := newEdgeRig(t)
	const sessionID, pub = "ses_edge_hung_broker", "notifications.github.acme.pub.>"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID, pub), http.StatusOK, "the registration following acme/pub")
	_, registered := storedEdgeSessionRecord(t, rig, sessionID)

	var bounded atomic.Bool
	reader := newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), rig.projects,
		newEdgeWithheldCounter(metrics.New()), logging.New("test"))
	reader.client = &http.Client{Transport: edgeBrokerTransport(func(request *http.Request) (*http.Response, error) {
		_, hasDeadline := request.Context().Deadline()
		bounded.Store(hasDeadline)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	rig.deps.repositories = reader
	heartbeat := rig.subscribe(t, token, sessionID, pub)
	requireEdgeRefusal(t, heartbeat, http.StatusServiceUnavailable, "repositories_unknown", "the heartbeat while the broker never answers")
	if !bounded.Load() {
		t.Errorf("the repository read of the heartbeat had no deadline: a broker that never answers holds the registration for as long as its caller waits")
	}
	if _, got := storedEdgeSessionRecord(t, rig, sessionID); got <= registered {
		t.Fatalf("the heartbeat while the broker never answers left the session entry at revision %d, the registration's %d: the entry ages out", got, registered)
	}
}
