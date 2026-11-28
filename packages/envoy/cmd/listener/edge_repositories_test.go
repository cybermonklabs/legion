package main

// edge_repositories_test.go holds the edge listener's GitHub rows on a real bus: which
// repositories' events a session follows and receives, read from the broker's answer for its
// token's projects (edge_repositories.go), how the answer's refresh, its failure and its case
// decide that, and what a registration is told while no answer can be read. Each row runs under
// the same edge-off control as the rows of edge_mode_test.go (edgeTestProjects).

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/listenerwire"
)

// An edge session may follow the GitHub events of each repository its project's issues
// resolve to, read fresh through the broker at the registration with the listener's own account,
// and an event of each reaches it.
func TestAnEdgeRelaysTheGitHubEventsOfItsProjectsRepositories(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	awaitConsumerSettled(t, rig, sessionID)
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
	t.Parallel()
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

	awaitConsumerSettled(t, rig, sessionID)
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

// The end of a receive term forgets every gap mark the relay set noted in its process (stopAll),
// and a term that begins after it starts with none: the session's entry keeps the mark for the pod
// that relays the session next, which may send and clear it before this pod relays again. Here the
// edge withholds a GitHub event while no repository answer stands, so the relay notes the event's
// mark; the term ends (durableReceiver.end, sessionRelays.setOwned), the mark is sent and cleared
// from the entry while this listener relays nothing, as the pod that relayed the session meanwhile
// would, and a term begins again, whose first frame carries no gap mark. Control: with stopAll
// keeping the noted marks, the mark outlives the term's end and the row fails.
func TestAReceiveTermsEndForgetsTheGapMarksItsRelaysNoted(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_gap_term_end"
	token := rig.personToken(sessionID, "PRIV")
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>"), http.StatusOK, "the registration")
	first, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	noted := func() string {
		rig.relays.repositoryGapsMu.Lock()
		defer rig.relays.repositoryGapsMu.Unlock()
		return rig.relays.repositoryGaps[sessionID]
	}
	rig.repositories.failWith(http.StatusServiceUnavailable)
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.1")
	testwait.Eventually(t, "the edge to withhold the acme/pub event and note its gap mark", func() bool {
		return rig.withheld(t, "answer_unknown") >= 1 && noted() != ""
	})
	mark := noted()

	rig.relays.setOwned(nil)
	if !testwait.Holds(10*time.Second, testwait.Poll, func() bool { return noted() == "" }) {
		t.Fatalf("the relay set still notes the gap mark %q for %s 10 s after its receive term ended: a mark noted under a term outlives it", noted(), sessionID)
	}
	// The pod that relays the session meanwhile sends the mark and clears it from the entry.
	if err := rig.deps.sessions.ClearRepositoryGap(t.Context(), sessionID, mark); err != nil {
		t.Fatalf("clear %s's mark %q as the next pod's relay does: %v", sessionID, mark, err)
	}
	rig.repositories.failWith(0)
	awaitConsumerSettled(t, rig, sessionID)
	rig.relays.setOwned(endlessTerm())
	if again := noted(); again != "" {
		t.Fatalf("the receive term that began after the end notes the gap mark %q for %s", again, sessionID)
	}

	next, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frames := standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, next)
	if gap, reason := frames[0].msg.Header.Get(relayHeaderGap), frames[0].msg.Header.Get(relayHeaderGapReason); gap != "" || reason != "" {
		t.Fatalf("the first frame of the term that began after the end carries %s %q and %s %q: the gap mark %q, which the next pod sent and cleared, is sent again",
			relayHeaderGap, gap, relayHeaderGapReason, reason, mark)
	}
}

// awaitConsumerSettled waits for the server to take every ack of what sessionID's consumer has
// delivered - the session's done for a frame, the relay's own for a GitHub event it withheld - and
// then for the consumer's stored state (awaitSessionConsumerStored), before a row restarts the
// relay at the ack floor: a registration under a new delivery epoch, or a receive term that begins
// again. That restart then finds nothing awaiting an ack, so it keeps the consumer: it updates it
// in place and sends no delete and no create (bus.SessionConsumers.RestartAtFloor). A restart that
// finds a delivery still awaiting its ack deletes the consumer and creates it again at the floor,
// which sends the withheld event once more, and a delete that soon after a delivery or an ack can
// meet the consumer's state write still under way, which fails the create.
func awaitConsumerSettled(t *testing.T, rig *edgeRig, sessionID string) {
	t.Helper()
	info, err := rig.consumers.Info(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("read %s's consumer: %v", sessionID, err)
	}
	awaitAcked(t, rig.consumers, sessionID, info.Delivered.Stream)
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)
}

// The gap mark a later withheld event owes outlives another relay's clear of an earlier mark:
// while the session moves, its old relay (A) noted a mark for a GitHub event it withheld while no
// repository answer stood, the new relay (B) adopted the mark the entry carries
// (owedRepositoryGap) and published a frame carrying it, and before B clears that mark A withholds
// a later event. A's mark for the later event is an id of its own (noteRepositoryGap), so B's
// clear, which removes only the mark its frame carried (session.SessionRegistry.ClearRepositoryGap),
// leaves it on the entry for the next frame to send. The relays' marks are every listener's, so
// the row passes with edge mode off by design. Control: with noteRepositoryGap writing the mark it
// noted before again, B's clear removes the later event's mark and the row fails.
func TestAClearAfterAHandoverLeavesTheMarkOfALaterWithheldEvent(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_gap_handed_over"
	requireStatus(t, rig.subscribe(t, rig.personToken(sessionID, "PRIV"), sessionID), http.StatusOK, "the registration")
	ctx := t.Context()
	relayA := rig.relays
	relayB := &sessionRelays{sessions: rig.deps.sessions, logger: rig.harness.config.logger}
	first, err := relayA.noteRepositoryGap(ctx, sessionID)
	if err != nil {
		t.Fatalf("relay A's mark of the first withheld event: %v", err)
	}
	carried := relayB.owedRepositoryGap(sessionID)
	if carried != first {
		t.Fatalf("UNKNOWN: relay B owes the mark %q, not relay A's %q on the entry, so its frame carries no adopted mark", carried, first)
	}
	later, err := relayA.noteRepositoryGap(ctx, sessionID)
	if err != nil {
		t.Fatalf("relay A's mark of the later withheld event: %v", err)
	}
	relayB.repositoryGapSent(ctx, sessionID, carried)
	if stored := storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap; stored == "" || stored != later {
		t.Fatalf("after relay B cleared the mark its frame carried (%q) the entry carries %q, not relay A's mark of the later withheld event (%q): the later event's gap, which no frame has carried, is lost",
			carried, stored, later)
	}
	if owed := relayA.owedRepositoryGap(sessionID); owed != later {
		t.Fatalf("relay A owes %q after relay B's clear, want its mark of the later withheld event, %q", owed, later)
	}
}

// A relay's write of the gap mark, and its clear of one, end at their deadline
// (repositoryGapWriteTimeout) while the session bucket does not answer them, whatever the relay
// run's own context allows, so a bucket that stops answering holds the session's frames no longer
// than that: the mark's write is then the error, on which the relay gives the withheld event back
// (giveBackWithheld), and the clear's is logged. The mark and the clear each read the entry from
// the stream's leader (session.SessionRegistry.SetRepositoryGap and ClearRepositoryGap), a read no
// bucket handle carries, and then write it back, so a bucket can stop answering at either step:
//   - the entry's conditional write stalls until the test ends (stalledSessionEntryKV), with the
//     leader's read answering;
//   - the leader's read stalls: the session bucket's handle reads the stream through a connection
//     to a server that has stopped answering (testnats.Server.Freeze), with the bucket's own reads
//     and writes answering.
//
// Each must end within its own bound, well before bus.RequestTimeout, the bound a read of the stream
// with no deadline of its own takes, so the row tells the relay's own bound from the handle's. The
// relays' writes are every listener's, so the row passes with edge mode off by design. Control: with
// both written under the relay run's context, which has no deadline (noteRepositoryGap and
// repositoryGapSent before this row), the stalled write never ends and the stalled leader read ends
// only at bus.RequestTimeout, and both subtests fail.
func TestARelaysGapMarkWritesEndWhileTheSessionBucketDoesNotAnswer(t *testing.T) {
	t.Parallel()
	// The row's limit lies between the relay's own bound and a read's own, so it can tell them apart
	// only while the first is the shorter: a bound raised past bus.RequestTimeout would otherwise set
	// a limit as long as the bound it guards.
	if repositoryGapWriteTimeout >= bus.RequestTimeout {
		t.Fatalf("repositoryGapWriteTimeout is %s, not under bus.RequestTimeout (%s): the relay's gap writes are bounded no tighter than a read with no deadline of its own", repositoryGapWriteTimeout, bus.RequestTimeout)
	}
	for _, stall := range []struct {
		name string
		// apply makes the session bucket stop answering the step the subtest names, for sessionID.
		apply func(t *testing.T, rig *edgeRig, sessionID string)
	}{
		{"the entry's write", func(t *testing.T, rig *edgeRig, sessionID string) {
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			swapSessionBucket(t, rig.deps.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
				return stalledSessionEntryKV{stalledSessionGetKV{KeyValue: kv, key: sessionID, released: released}}
			})
		}},
		{"the stream leader's read", func(t *testing.T, rig *edgeRig, _ string) {
			server, uri := testnats.Start(t)
			frozen, err := jetstream.New(testnats.Connect(t, uri))
			if err != nil {
				t.Fatalf("a jetstream context on the server that stops answering: %v", err)
			}
			t.Cleanup(server.Freeze(t))
			swapSessionHandle(t, rig.deps.sessions, func(old bus.KeyValue) bus.KeyValue {
				return bus.NewKeyValue(old.Legacy(), old.WatchHandle(), frozen)
			})
		}},
	} {
		t.Run(stall.name, func(t *testing.T) {
			rig := newEdgeRig(t)
			const sessionID = "ses_edge_gap_unanswered"
			requireStatus(t, rig.subscribe(t, rig.personToken(sessionID, "PRIV"), sessionID), http.StatusOK, "the registration")
			// The clear reads the bucket only for an entry this listener's copy holds marked.
			if err := rig.deps.sessions.SetRepositoryGap(t.Context(), sessionID, "an-earlier-mark"); err != nil {
				t.Fatalf("mark %s's entry: %v", sessionID, err)
			}
			stall.apply(t, rig, sessionID)
			// Each write must end well before bus.RequestTimeout: halfway between that and the
			// relay's own bound, so a write the relay's bound ends passes with seconds to spare, and
			// one only a read's own bound ends fails with seconds to spare.
			within := (repositoryGapWriteTimeout + bus.RequestTimeout) / 2
			ends := func(what string, write func()) {
				t.Helper()
				done := make(chan struct{})
				started := time.Now()
				go func() {
					defer close(done)
					write()
				}()
				select {
				case <-done:
					t.Logf("%s ended after %s", what, time.Since(started).Round(time.Millisecond))
				case <-time.After(within):
					t.Fatalf("%s while %s does not answer is still waiting after %s, past its own bound (%s): it holds the session's frames as long as the bucket does not answer, or until a read's own bound (%s)",
						what, stall.name, within, repositoryGapWriteTimeout, bus.RequestTimeout)
				}
			}
			var markErr error
			ends("the relay's mark of a withheld event", func() { _, markErr = rig.relays.noteRepositoryGap(t.Context(), sessionID) })
			if !errors.Is(markErr, context.DeadlineExceeded) {
				t.Errorf("the relay's mark of a withheld event while %s does not answer ended with %v, want its deadline", stall.name, markErr)
			}
			ends("the relay's clear of the mark a frame carried", func() { rig.relays.repositoryGapSent(t.Context(), sessionID, "an-earlier-mark") })
		})
	}
}

// stalledSessionEntryKV is stalledSessionGetKV whose conditional write of key does not answer
// either until released is closed, as a bucket that stopped answering the entry's reads and
// writes; every other call is the bucket's own.
type stalledSessionEntryKV struct {
	stalledSessionGetKV
}

func (kv stalledSessionEntryKV) Update(key string, value []byte, last uint64) (uint64, error) {
	if key != kv.key {
		return kv.stalledSessionGetKV.Update(key, value, last)
	}
	<-kv.released
	return 0, errors.New("the session bucket's write of the entry never answered")
}

// A gap mark another relay of the session writes on the entry while this listener's relay run is
// live, for a GitHub event it withheld after this run started, rides the first frame this run
// sends once this listener's copy of the entry shows the mark: the run reads the mark it owes
// from that copy before each frame (owedRepositoryGap), not only when it starts, and clears the
// mark once that frame is published. Here the mark is written through this listener's own
// registry, so its copy shows it at once and the next frame carries it.
// The relays' marks are every listener's, so the row passes with edge mode off by design.
// Control: with the run reading the entry's mark only when it starts (relayNATS before this row),
// the next frame carries no gap mark and the entry keeps the mark for a later run, and the row
// fails.
func TestAMarkAnotherRelayWritesDuringARunReachesTheRunsNextFrame(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const sessionID = "ses_edge_gap_live_run"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.call(t, http.MethodPost, rig.personToken(sessionID, "PRIV"), "/v1/interests/subscribe",
		consumerRegistration(sessionID, "epoch-1", "notifications.dispatch.issue.PRIV.>")), http.StatusOK, "the registration")
	first, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)
	other := &sessionRelays{sessions: rig.deps.sessions, logger: rig.harness.config.logger}
	mark, err := other.noteRepositoryGap(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("the other relay's mark: %v", err)
	}
	next, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-2.comment.created")
	frames := standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, next)
	if gap, reason := frames[0].msg.Header.Get(relayHeaderGap), frames[0].msg.Header.Get(relayHeaderGapReason); gap != "true" || reason != relayGapRepositoryWithheld {
		t.Fatalf("the live run's frame after the other relay's mark %q carries %s %q and %s %q, want true and %s; the entry holds %q for a later run",
			mark, relayHeaderGap, gap, relayHeaderGapReason, reason, relayGapRepositoryWithheld, storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap)
	}
	testwait.Eventually(t, "the mark the frame carried to be cleared from the session's entry", func() bool {
		return storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap == ""
	})
}

// A relay run carries a mark once: a mark its frame carried is not taken again from this
// listener's copy of the entry while the copy trails the clear, which would send the session the
// same gap notice twice. Here another listener's relay sent the mark and cleared it on the bucket
// while this listener's copy, whose watch is stopped, still holds it, so this run's own clear finds
// nothing to clear and the copy keeps the mark. The run's first frame after the mark carries it, a
// notice the copy cannot yet tell is the other relay's, and its next frame carries none. The
// relays' marks are every listener's, so the row passes with edge mode off by design. Control:
// with the run taking any mark the copy shows before each frame (relayNATS before this row), the
// next frame carries the same mark again, and the row fails.
func TestARelayRunCarriesAMarkOnceWhileItsCopyTrailsTheClear(t *testing.T) {
	t.Parallel()
	rig := newEdgeRig(t)
	const sessionID, mark = "ses_edge_gap_carried_once", "the-other-listeners-mark"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	requireStatus(t, rig.call(t, http.MethodPost, rig.personToken(sessionID, "PRIV"), "/v1/interests/subscribe",
		consumerRegistration(sessionID, "epoch-1", "notifications.dispatch.issue.PRIV.>")), http.StatusOK, "the registration")
	first, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-1.comment.created")
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)

	other, err := session.OpenSessionRegistry(t.Context(), rig.harness.client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the other listener's session registry: %v", err)
	}
	t.Cleanup(other.StopWatch)
	if err := other.SetRepositoryGap(t.Context(), sessionID, mark); err != nil {
		t.Fatalf("the other listener's mark: %v", err)
	}
	testwait.Eventually(t, "this listener's copy of the entry to show the other listener's mark", func() bool {
		entry, _ := rig.deps.sessions.Lookup(sessionID)
		return entry.RepositoryGap == mark
	})
	rig.deps.sessions.StopWatch()
	if err := other.ClearRepositoryGap(t.Context(), sessionID, mark); err != nil {
		t.Fatalf("the other listener's clear: %v", err)
	}
	if stored := storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap; stored != "" {
		t.Fatalf("UNKNOWN: the bucket's entry carries %q after the other listener's clear, want none", stored)
	}

	carried, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-2.comment.created")
	frames := standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, carried)
	if gap := frames[0].msg.Header.Get(relayHeaderGap); gap != "true" {
		t.Fatalf("UNKNOWN: the frame after the other listener's mark carries %s %q, not the mark this listener's copy shows, so the row tests nothing", relayHeaderGap, gap)
	}
	if entry, _ := rig.deps.sessions.Lookup(sessionID); entry.RepositoryGap != mark {
		t.Fatalf("UNKNOWN: this listener's copy of the entry carries %q after the run's clear, not the mark %q it trails the clear of", entry.RepositoryGap, mark)
	}
	next, _ := rig.storeEvent(t, "notifications.dispatch.issue.PRIV.PRIV-3.comment.created")
	frames = standIn.collect(t, 1, 10*time.Second)
	requireFrames(t, frames, next)
	if gap, reason := frames[0].msg.Header.Get(relayHeaderGap), frames[0].msg.Header.Get(relayHeaderGapReason); gap != "" || reason != "" {
		t.Fatalf("the run's frame after the one that carried mark %q carries %s %q and %s %q: the same gap notice is sent twice while this listener's copy trails the clear",
			mark, relayHeaderGap, gap, relayHeaderGapReason, reason)
	}
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	requireWithheldEventGivenBackUntilMarked(t, "ses_edge_gap_no_entry", func(bucket natsgo.KeyValue, sessionID string) error {
		return bucket.Delete(sessionID)
	})
}

// requireWithheldEventGivenBackUntilMarked is the give-back rows' body: the event withheld while
// the session's entry cannot be marked (giveBackWithheldEvent) is given back and not counted, and is
// marked, acknowledged and counted once after the entry is written whole again.
func requireWithheldEventGivenBackUntilMarked(t *testing.T, sessionID string, spoil func(bucket natsgo.KeyValue, sessionID string) error) {
	t.Helper()
	rig := newEdgeRig(t)
	event := giveBackWithheldEvent(t, rig, sessionID, spoil)
	if info := event.consumer(); info.AckFloor.Stream >= event.seq {
		t.Fatalf("%s's consumer acknowledged the withheld acme/pub event (seq %d, ack floor %d) while its entry could not carry the gap mark: a move before the next frame loses the mark",
			sessionID, event.seq, info.AckFloor.Stream)
	}
	if got := rig.withheld(t, "answer_unknown"); got > 0 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="answer_unknown"} = %d while the event is given back, want it counted only when it is acknowledged`, got)
	}

	event.restore()
	testwait.Eventually(t, "the redelivered event to mark the entry and be acknowledged", func() bool {
		return storedEdgeSessionEntry(t, rig, sessionID).RepositoryGap != "" && event.consumer().AckFloor.Stream >= event.seq
	})
	if got := rig.withheld(t, "answer_unknown"); got != 1 {
		t.Fatalf(`envoy_edge_github_withheld_total{reason="answer_unknown"} = %d, want 1: the event is withheld once, however often it was given back`, got)
	}
}

// givenBackEvent is a withheld GitHub event the relay took while the session's entry could not be
// marked (giveBackWithheldEvent): consumer reads the session's consumer now, seq is the event's
// stream sequence, before is the consumer's delivered sequence before the event was stored, so
// each delivery of the event moves it on by one, and restore writes the session's entry whole
// again.
type givenBackEvent struct {
	consumer func() *jetstream.ConsumerInfo
	seq      uint64
	before   uint64
	restore  func()
}

// giveBackWithheldEvent is the give-back rows' setup on rig: sessionID registers at the edge
// following acme/pub, spoil makes the bucket's entry of the session one the relay cannot mark,
// behind this listener's copy (its watch is stopped), and an acme/pub event is stored while no
// repository answer stands. It returns once the relay has taken the event and acknowledged it or
// given it back.
func giveBackWithheldEvent(t *testing.T, rig *edgeRig, sessionID string, spoil func(bucket natsgo.KeyValue, sessionID string) error) givenBackEvent {
	t.Helper()
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
	before := consumer().Delivered.Consumer
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
	return givenBackEvent{consumer: consumer, seq: seq, before: before, restore: func() {
		t.Helper()
		if _, err := bucket.Put(sessionID, whole.Value()); err != nil {
			t.Fatalf("write %s's entry whole again: %v", sessionID, err)
		}
	}}
}

// A withheld GitHub event the edge gives back because the session's entry cannot record its gap
// mark is logged once, when the give-backs start, however often the server delivers it again
// meanwhile (1 s, 2 s, 4 s ... after each give-back, relayNakDelayFor), and once more when a mark
// write of the session lands again, with the count of give-backs between. Control: with a WARN at
// each give-back and no line at the end, as before this row, the second give-back logs a second
// WARN and the row fails.
func TestAGivenBackEventIsLoggedOnceUntilItsSessionsEntryTakesAMark(t *testing.T) {
	t.Parallel()
	const gaveBack, marked = "edge listener gave a withheld GitHub event back to the server", "edge listener recorded a gap mark on the session's entry again"
	const sessionID = "ses_edge_gap_logged_once"
	rig := newEdgeRigServing(t, []string{"PRIV"})
	event := giveBackWithheldEvent(t, rig, sessionID, func(bucket natsgo.KeyValue, sessionID string) error {
		_, err := bucket.Put(sessionID, []byte(`{"machine_id":7}`))
		return err
	})
	// A delivery follows the give-back of the one before it, so by the third the first two were
	// given back.
	testwait.Eventually(t, "the server to deliver the given-back event a third time", func() bool {
		return event.consumer().Delivered.Consumer >= event.before+3
	})
	if lines := edgeLogLines(rig.harness.logs.String(), "WARN", gaveBack); len(lines) != 1 || lines[0]["session_id"] != sessionID {
		t.Fatalf("an event given back at each of its deliveries logged %d WARN lines, want 1 naming %s, when the give-backs began:\n%s",
			len(lines), sessionID, rig.harness.logs.String())
	}
	if lines := edgeLogLines(rig.harness.logs.String(), "INFO", marked); len(lines) != 0 {
		t.Fatalf("the entry still takes no mark, and the log says it does:\n%s", rig.harness.logs.String())
	}

	event.restore()
	testwait.Eventually(t, "the redelivered event to mark the entry and be acknowledged", func() bool {
		return event.consumer().AckFloor.Stream >= event.seq
	})
	lines := edgeLogLines(rig.harness.logs.String(), "INFO", marked)
	if len(lines) != 1 || lines[0]["session_id"] != sessionID {
		t.Fatalf("the mark that landed after the give-backs logged %d lines saying the entry takes marks again, want 1 naming %s:\n%s",
			len(lines), sessionID, rig.harness.logs.String())
	}
	if given, _ := lines[0]["given_back"].(float64); given < 2 {
		t.Errorf("the line that ends the give-backs counts %v of them, want at least the 2 before the third delivery", lines[0]["given_back"])
	}
}

// A run of give-backs also ends when a fresh repository answer decides the session's GitHub events
// again while its entry still takes no mark: the given-back event comes back, a fresh answer that
// names its repository relays it, and that ends the run with its INFO line. A later outage in which
// the entry still takes no mark starts a new run, logged with its own WARN. Control: with the run
// ended only by a mark write that lands or the session leaving, as at 3031034d82, the count of the
// first run stands, the second run's give-back logs nothing, and the row fails.
func TestARunOfGiveBacksEndsWhenAFreshAnswerRelaysTheEvent(t *testing.T) {
	t.Parallel()
	const gaveBack, decided = "edge listener gave a withheld GitHub event back to the server", "edge listener decides the session's GitHub events by a fresh repository answer again"
	const sessionID = "ses_edge_gap_answer_back"
	rig := newEdgeRigServing(t, []string{"PRIV"})
	event := giveBackWithheldEvent(t, rig, sessionID, func(bucket natsgo.KeyValue, sessionID string) error {
		_, err := bucket.Put(sessionID, []byte(`{"machine_id":7}`))
		return err
	})
	if lines := edgeLogLines(rig.harness.logs.String(), "WARN", gaveBack); len(lines) != 1 {
		t.Fatalf("UNKNOWN: the first give-back logged %d WARN lines, want 1:\n%s", len(lines), rig.harness.logs.String())
	}

	// The broker answers again, and the refresh reads a fresh answer naming acme/pub, so the
	// given-back event is relayed when it comes back, while the entry still takes no mark.
	rig.repositories.failWith(0)
	rig.tick(t)
	testwait.Eventually(t, "the given-back event to be relayed and acknowledged", func() bool {
		return event.consumer().AckFloor.Stream >= event.seq
	})
	ends := edgeLogLines(rig.harness.logs.String(), "INFO", decided)
	if len(ends) != 1 || ends[0]["session_id"] != sessionID {
		t.Errorf("the relay of the given-back event by a fresh answer logged %d lines ending the run, want 1 naming %s", len(ends), sessionID)
	} else if given, _ := ends[0]["given_back"].(float64); given < 1 {
		t.Errorf("the line that ends the run counts %v give-backs, want at least 1", ends[0]["given_back"])
	}

	// A second outage, the entry still taking no mark: its first give-back starts a new run. The
	// event is delivered again only after its give-back, which logs at once.
	rig.repositories.failWith(http.StatusServiceUnavailable)
	rig.clock.advance(edgeRepositoryAnswerLife + time.Second)
	rig.storeGithubEvent(t, "acme/pub", "pr.2")
	testwait.Eventually(t, "the second outage's event to be given back and delivered again", func() bool {
		info := event.consumer()
		return info.Delivered.Stream > event.seq && info.NumRedelivered >= 1
	})
	if lines := edgeLogLines(rig.harness.logs.String(), "WARN", gaveBack); len(lines) != 2 {
		t.Fatalf("a give-back of a second outage, after a fresh answer ended the first, logged %d WARN lines in all, want 2:\n%s", len(lines), rig.harness.logs.String())
	}
}

// A drop that no repository answer decided does not end a run of give-backs: a GitHub event whose
// payload names no repository is dropped not_in_project whatever the answer says, so it ends no run
// and logs no end line, and the given-back event's next give-back starts no new run. Control: with
// the run ended at every drop, as at e263b68294, the no-repository event writes the end line, the
// next give-back writes a second WARN, and the row fails.
func TestADropNoAnswerDecidedLeavesARunOfGiveBacksOpen(t *testing.T) {
	t.Parallel()
	const gaveBack, decided = "edge listener gave a withheld GitHub event back to the server", "edge listener decides the session's GitHub events by a fresh repository answer again"
	const sessionID = "ses_edge_gap_unnamed_drop"
	rig := newEdgeRigServing(t, []string{"PRIV"})
	event := giveBackWithheldEvent(t, rig, sessionID, func(bucket natsgo.KeyValue, sessionID string) error {
		_, err := bucket.Put(sessionID, []byte(`{"machine_id":7}`))
		return err
	})
	subject := contracts.GithubRepositoryPrefix("acme", "pub") + ".pr.unnamed"
	item := listenerTestEnvelope(subject, "edge-github-"+id.New())
	item.Source = "github"
	item.Payload = `{"kind":"pr","action":"opened"}`
	if _, err := rig.harness.client.JS().Publish(subject, marshalListenerEnvelope(t, item)); err != nil {
		t.Fatalf("store a GitHub event that names no repository: %v", err)
	}
	// The given-back event, the unnamed one, then the given-back event twice more: by its last
	// delivery the one before it was given back.
	testwait.Eventually(t, "the given-back event to be delivered twice more after the unnamed event", func() bool {
		return event.consumer().Delivered.Consumer >= event.before+4
	})
	if lines := edgeLogLines(rig.harness.logs.String(), "INFO", decided); len(lines) != 0 {
		t.Errorf("a drop of an event that names no repository logged %d lines ending the run, want 0: no answer decided it", len(lines))
	}
	if lines := edgeLogLines(rig.harness.logs.String(), "WARN", gaveBack); len(lines) != 1 {
		t.Fatalf("the give-backs around a drop no answer decided logged %d WARN lines, want 1: the run goes on:\n%s", len(lines), rig.harness.logs.String())
	}
}

// nakAcker is a pulled message's answers whose NakWithDelay ends with err.
type nakAcker struct{ err error }

func (nakAcker) Ack() error                         { return nil }
func (nakAcker) InProgress() error                  { return nil }
func (a nakAcker) NakWithDelay(time.Duration) error { return a.err }

// A give-back whose nak the server does not take is logged at every give-back, the first of a run
// or not: the event then waits out the consumer's ack wait rather than the nak's delay, which is not
// the repeated "the entry takes no mark" a run logs once. The repeated mark error is still logged
// once a run. Control: with the nak's error logged only at a run's first give-back, as at
// 3031034d82, the second give-back's failed nak is logged nowhere and the row fails.
func TestAGiveBackWhoseNakFailsIsLoggedEveryTime(t *testing.T) {
	t.Parallel()
	const gaveBack, unNaked = "edge listener gave a withheld GitHub event back to the server", "edge listener could not give a withheld GitHub event back to the server"
	logs := &cmdtest.LockedBuffer{}
	m := &sessionRelays{logger: logging.NewWithWriter("test", logs)}
	marking := errors.New("mark session ses_nak's repository gap: the bucket holds no entry of it")
	refused := errors.New("nats: connection closed")
	const subject = "notifications.github.acme.pub.pr.1"
	m.giveBackWithheld("ses_nak", bus.SessionMessage{Subject: subject, StreamSeq: 7, Delivered: 1, SessionAcker: nakAcker{}}, marking)
	m.giveBackWithheld("ses_nak", bus.SessionMessage{Subject: subject, StreamSeq: 7, Delivered: 2, SessionAcker: nakAcker{err: refused}}, marking)
	m.giveBackWithheld("ses_nak", bus.SessionMessage{Subject: subject, StreamSeq: 7, Delivered: 3, SessionAcker: nakAcker{}}, marking)
	if lines := edgeLogLines(logs.String(), "WARN", gaveBack); len(lines) != 1 {
		t.Fatalf("three give-backs of one run logged %d WARN lines starting the run, want 1:\n%s", len(lines), logs.String())
	}
	lines := edgeLogLines(logs.String(), "WARN", unNaked)
	if len(lines) != 1 {
		t.Fatalf("the second give-back, whose nak failed, logged %d lines naming the failed nak, want 1:\n%s", len(lines), logs.String())
	}
	if text, _ := lines[0]["error"].(string); lines[0]["session_id"] != "ses_nak" || lines[0]["stream_seq"] != float64(7) || !strings.Contains(text, refused.Error()) {
		t.Errorf("the failed nak's line names session %v, stream_seq %v, error %q; want ses_nak, 7 and %q", lines[0]["session_id"], lines[0]["stream_seq"], text, refused)
	}
}

// Dispatch answers a repository in lower case and the intake keeps GitHub's case, so the
// edge compares owner and name ignoring case: an interest in Acme/Pub is admitted through an
// answer of acme/pub, and the event the intake publishes for Acme/Pub reaches the session.
func TestAnEdgeComparesRepositoriesIgnoringCase(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	case <-time.After(listenerWaitBound(t)):
		t.Fatal("the refresh read no answer of PRIV in time")
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

// A run of failed reads of one project's repositories is logged once, when it starts, and once
// more when a read succeeds again, however many refreshes the broker stays away: a WARN at every
// refresh would log a line per project every 30 s for the whole outage. The WARN names when the
// project's sessions lose their GitHub events (withheld_from): the last answer still decides them
// until it is edgeRepositoryAnswerLife old, so the first failed read after a fresh answer withholds
// nothing, and with no answer standing they are withheld from the failed read on. Control: with a
// WARN at each failed read and no line at the end, as before this row, the three failed refreshes
// log three WARN lines and the row fails.
func TestAnEdgeRefreshLogsARunOfFailedReadsOnceAndItsEnd(t *testing.T) {
	t.Parallel()
	const failed, again = "edge listener could not read its project's repositories", "edge listener read its project's repositories again"
	var refusing atomic.Bool
	newReader := func() (*edgeRepositories, *cmdtest.LockedBuffer, *listenerClock) { return refusableReader(&refusing) }

	reader, logs, clock := newReader()
	answered := clock.Now()
	reader.refresh(t.Context())
	refusing.Store(true)
	for pass := range 3 {
		clock.Advance(edgeRepositoryRefresh)
		reader.refresh(t.Context())
		if _, complete := reader.standing([]string{"PRIV"}); pass == 0 && !complete {
			t.Fatalf("one failed read %s after PRIV's answer left PRIV no fresh answer, want the answer, which lives %s, to stand", edgeRepositoryRefresh, edgeRepositoryAnswerLife)
		}
	}
	warnings := edgeLogLines(logs.String(), "WARN", failed)
	if len(warnings) != 1 {
		t.Fatalf("three failed refreshes of PRIV logged %d WARN lines, want 1, when the run of failed reads began:\n%s", len(warnings), logs.String())
	}
	if warnings[0]["project"] != "PRIV" {
		t.Errorf("the WARN names project %v, want PRIV", warnings[0]["project"])
	}
	requireLoggedTime(t, warnings[0], "withheld_from", answered.Add(edgeRepositoryAnswerLife))
	if ends := edgeLogLines(logs.String(), "INFO", again); len(ends) != 0 {
		t.Fatalf("the reads of PRIV still fail, and the log says they succeed again:\n%s", logs.String())
	}

	refusing.Store(false)
	clock.Advance(edgeRepositoryRefresh)
	reader.refresh(t.Context())
	reader.refresh(t.Context())
	ends := edgeLogLines(logs.String(), "INFO", again)
	if len(ends) != 1 {
		t.Fatalf("two good refreshes of PRIV after three failed ones logged %d lines saying its reads succeed again, want 1:\n%s", len(ends), logs.String())
	}
	if ends[0]["project"] != "PRIV" || ends[0]["failed_reads"] != float64(3) {
		t.Errorf("the end of the run names project %v and failed_reads %v, want PRIV and 3", ends[0]["project"], ends[0]["failed_reads"])
	}

	// A reader whose first read fails holds no answer, so the project's GitHub events are
	// withheld from that read on.
	fresh, freshLogs, freshClock := newReader()
	refusing.Store(true)
	fresh.refresh(t.Context())
	warnings = edgeLogLines(freshLogs.String(), "WARN", failed)
	if len(warnings) != 1 {
		t.Fatalf("a failed first read of PRIV logged %d WARN lines, want 1:\n%s", len(warnings), freshLogs.String())
	}
	requireLoggedTime(t, warnings[0], "withheld_from", freshClock.Now())
}

// refusableReader is the edge's repository reader of PRIV on a clock of its own, logging to logs,
// whose broker answers 503 while refusing holds and PRIV's answer of acme/pub otherwise.
func refusableReader(refusing *atomic.Bool) (reader *edgeRepositories, logs *cmdtest.LockedBuffer, clock *listenerClock) {
	logs = &cmdtest.LockedBuffer{}
	clock = &listenerClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	reader = newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), []string{"PRIV"},
		newEdgeWithheldCounter(metrics.New()), logging.NewWithWriter("test", logs))
	reader.client = &http.Client{Transport: edgeBrokerTransport(func(request *http.Request) (*http.Response, error) {
		if refusing.Load() {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`)), Request: request}, nil
		}
		body := fmt.Sprintf(`{"project":%q,"repositories":["acme/pub"]}`, request.URL.Query().Get("project"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	reader.now = clock.Now
	return reader, logs, clock
}

// A good read of a project ends its run of failed refresh reads whoever makes it: a registration's
// read (readFresh) keeps an answer as a refresh's does, so it writes the run's end line, and when
// that answer ages out while the refreshes still fail, the next failed refresh starts a new run
// whose WARN names that answer's age-out as withheld_from. Control: with the run ended only by a
// refresh's good read, as at e263b68294, the registration's good read logs nothing, the next failed
// refresh logs no WARN, and the row fails.
func TestAGoodRegistrationReadEndsARunOfFailedRefreshes(t *testing.T) {
	t.Parallel()
	const failed, again = "edge listener could not read its project's repositories", "edge listener read its project's repositories again"
	var refusing atomic.Bool
	reader, logs, clock := refusableReader(&refusing)
	refusing.Store(true)
	reader.refresh(t.Context())

	clock.Advance(edgeRepositoryRefresh)
	refusing.Store(false)
	answered := clock.Now()
	if repositories, err := reader.readFresh(t.Context(), []string{"PRIV"}); err != nil || !slices.Equal(repositories, []string{"acme/pub"}) {
		t.Fatalf("UNKNOWN: the registration's read of PRIV answered %v, %v; want [acme/pub]", repositories, err)
	}
	ends := edgeLogLines(logs.String(), "INFO", again)
	if len(ends) != 1 || ends[0]["failed_reads"] != float64(1) {
		t.Errorf("a registration's good read after one failed refresh logged %d lines ending the run (%v), want 1 with failed_reads 1", len(ends), ends)
	}

	refusing.Store(true)
	clock.Advance(edgeRepositoryRefresh)
	reader.refresh(t.Context())
	warnings := edgeLogLines(logs.String(), "WARN", failed)
	if len(warnings) != 2 {
		t.Fatalf("a failed refresh after a registration's good read logged %d WARN lines in all, want 2: the good read ended the first run:\n%s", len(warnings), logs.String())
	}
	requireLoggedTime(t, warnings[1], "withheld_from", answered.Add(edgeRepositoryAnswerLife))
}

// A session whose interest in a repository that left its projects cannot be removed is logged
// once, when its removals start failing, however many refresh passes they fail at, and the
// removal that then succeeds is logged as every removal is. Here a removal fails because its pass
// runs on a context that has ended, which the removal's bus calls read at once. Control: with a
// WARN at each failed removal, as before this row, three failed passes log three WARN lines and
// the row fails.
func TestAnEdgePruneLogsARunOfFailedRemovalsOnce(t *testing.T) {
	t.Parallel()
	const failed, removed = "edge listener could not remove the interests of a repository that left the session's projects", "edge listener removed the interests of a repository that left the session's projects"
	rig := newEdgeRigServing(t, []string{"PRIV"})
	const sessionID, two = "ses_edge_prune_failing", "notifications.github.acme.two.>"
	token := rig.personToken(sessionID, "PRIV")
	requireStatus(t, rig.subscribe(t, token, sessionID, "notifications.github.acme.pub.>", two), http.StatusOK, "the registration")
	// The pass the tick runs reads the answer that no longer names acme/two, after its own prune.
	rig.repositories.answer("PRIV", "acme/pub")
	rig.tick(t)

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	for range 3 {
		pruneEdgeInterests(ended, rig.deps, "test-machine")
	}
	warnings := edgeLogLines(rig.harness.logs.String(), "WARN", failed)
	if len(warnings) != 1 {
		t.Fatalf("three passes that could not remove %s's interest in acme/two logged %d WARN lines, want 1, when its removals began to fail:\n%s",
			sessionID, len(warnings), rig.harness.logs.String())
	}
	if warnings[0]["session_id"] != sessionID {
		t.Errorf("the WARN names session %v, want %s", warnings[0]["session_id"], sessionID)
	}

	pruneEdgeInterests(t.Context(), rig.deps, "test-machine")
	if lines := edgeLogLines(rig.harness.logs.String(), "INFO", removed); len(lines) != 1 || lines[0]["session_id"] != sessionID {
		t.Fatalf("a pass that removed %s's interest in acme/two logged %d removal lines, want 1 naming it:\n%s", sessionID, len(lines), rig.harness.logs.String())
	}
	if row := bucketInterestRow(t, rig.harness.client, sessionID); slices.Contains(row.Topics, two) {
		t.Fatalf("the pass that logged the removal left %s on %s's row: %v", two, sessionID, row.Topics)
	}
}

// A relay socket logs a run of GitHub frames it answers nak for want of a fresh repository answer
// once, when the run starts, however often the relay delivers them again (1 s, 2 s ... 30 s
// apart), and once more when a fresh answer decides a GitHub frame of its session again, here by
// withholding one whose repository the answer does not name. Control: with a line at each nak, as at
// e263b68294, three naks log three lines and the row fails.
func TestARelaySocketLogsARunOfAnswerUnknownNaksOnceAndItsEnd(t *testing.T) {
	t.Parallel()
	const ended = "listener: the relay socket decides its session's GitHub frames by a fresh repository answer again"
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	var refusing atomic.Bool
	reader, _, _ := refusableReader(&refusing)
	logs := &cmdtest.LockedBuffer{}
	const session = "ses_edge_socket_naks"
	socket := &relaySocket{sockets: &relaySockets{client: client, logger: logging.NewWithWriter("test", logs)}, session: session,
		admitted: ciphertoken.Caller{Projects: []string{"PRIV"}}, repositories: reader}
	frame := func(repository string) *natsgo.Msg {
		owner, name, _ := strings.Cut(repository, "/")
		subject := contracts.GithubRepositoryPrefix(owner, name) + ".pr"
		item := listenerTestEnvelope(subject, "socket-"+repository)
		item.Source = "github"
		item.Payload = fmt.Sprintf(`{"kind":"pr","action":"opened","repo":%q}`, repository)
		return &natsgo.Msg{Subject: "relay." + session, Reply: bus.RelayReplyPrefix + "m1.n1." + session + ".t1",
			Header: natsgo.Header{relayHeaderSubject: []string{subject}}, Data: marshalListenerEnvelope(t, item)}
	}
	naks := func() int {
		count := 0
		for _, line := range edgeLogLines(logs.String(), "INFO", "listener: the relay socket") {
			if line["answer"] == relayReplyNak {
				count++
			}
		}
		return count
	}

	for range 3 {
		socket.forward(1, frame("acme/pub"))
	}
	if got := naks(); got != 1 {
		t.Fatalf("three naks of a GitHub frame while the replica has no fresh answer logged %d lines, want 1, when the run began:\n%s", got, logs.String())
	}
	if _, err := reader.readFresh(t.Context(), []string{"PRIV"}); err != nil {
		t.Fatalf("UNKNOWN: read PRIV's answer: %v", err)
	}
	socket.forward(1, frame("acme/other"))
	lines := edgeLogLines(logs.String(), "INFO", ended)
	if len(lines) != 1 || lines[0]["session_id"] != session || lines[0]["given_back"] != float64(3) {
		t.Fatalf("a GitHub frame a fresh answer decided after three naks logged %d lines ending the run (%v), want 1 naming %s with given_back 3:\n%s",
			len(lines), lines, session, logs.String())
	}
}

// startFailedRemovalRun registers sessionID at an edge rig following acme/pub and acme/two, makes
// PRIV's answer name acme/pub alone, and runs one prune whose removal of acme/two fails, its context
// having ended: the session's run of failed removals has begun, with its one WARN.
func startFailedRemovalRun(t *testing.T, sessionID string) *edgeRig {
	t.Helper()
	rig := newEdgeRigServing(t, []string{"PRIV"})
	requireStatus(t, rig.subscribe(t, rig.personToken(sessionID, "PRIV"), sessionID, "notifications.github.acme.pub.>", edgeRowTwo), http.StatusOK, "the registration")
	rig.repositories.answer("PRIV", "acme/pub")
	rig.tick(t)
	done, cancel := context.WithCancel(t.Context())
	cancel()
	pruneEdgeInterests(done, rig.deps, "test-machine")
	if lines := edgeLogLines(rig.harness.logs.String(), "WARN", "edge listener could not remove the interests of a repository that left the session's projects"); len(lines) != 1 {
		t.Fatalf("UNKNOWN: the failed removal logged %d WARN lines, want 1:\n%s", len(lines), rig.harness.logs.String())
	}
	return rig
}

// edgeRowTwo is the topic of acme/two the removal-run rows follow, the repository that leaves.
const edgeRowTwo = "notifications.github.acme.two.>"

// requireRemovalRunEnd fails the test unless the next prune of rig logs exactly one end of
// sessionID's run of failed removals, with why, and no removal.
func requireRemovalRunEnd(t *testing.T, rig *edgeRig, sessionID, why string) {
	t.Helper()
	pruneEdgeInterests(t.Context(), rig.deps, "test-machine")
	logged := rig.harness.logs.String()
	lines := edgeLogLines(logged, "INFO", "edge listener no longer removes the interests of a repository that left the session's projects")
	if len(lines) != 1 || lines[0]["session_id"] != sessionID || lines[0]["why"] != why {
		t.Fatalf("the pass after %s's run of failed removals ended logged %d end lines (%v), want 1 naming the session and why %q:\n%s", sessionID, len(lines), lines, why, logged)
	}
	if removals := edgeLogLines(logged, "INFO", "edge listener removed the interests of a repository that left the session's projects"); len(removals) != 0 {
		t.Fatalf("UNKNOWN: a removal of %s's interests succeeded, which ends the run by its own line: %v", sessionID, removals)
	}
}

// noneOutsideWhy is the why of a run of failed removals ended because no interest of the session
// is outside its projects' repositories any more, whichever of the session and the answer changed.
const noneOutsideWhy = "no interest of the session is outside its projects' repositories now"

// A run of failed removals that ends with no removal is logged too: here the repository comes back
// to the session's projects while its removals fail, so the next pass finds nothing leaving and
// writes the run's end line, naming why it ended. The new answer is read by a refresh with no prune
// before it, so no removal succeeds between. Control: with a run ended silently when nothing is left
// to remove, as at e263b68294, the pass logs no end line and the row fails.
func TestAnEdgePruneLogsTheEndOfARunWhenTheRepositoryComesBack(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_edge_prune_comes_back"
	rig := startFailedRemovalRun(t, sessionID)
	rig.repositories.answer("PRIV", "acme/pub", "acme/two")
	rig.reader.refresh(t.Context())
	requireRemovalRunEnd(t, rig, sessionID, noneOutsideWhy)
	if row := bucketInterestRow(t, rig.harness.client, sessionID); !slices.Contains(row.Topics, edgeRowTwo) {
		t.Fatalf("UNKNOWN: %s's row lost %s, which came back, so a removal happened: %v", sessionID, edgeRowTwo, row.Topics)
	}
}

// A run of failed removals that the session ends itself, by unsubscribing from the topic that left,
// is logged with a why that says what the pass found, nothing outside the answer any more, and not
// that the repository came back: acme/two is still outside PRIV's answer. Control: with the why
// "the repository is the session's again", as at 845bd3766e, the row fails on the why.
func TestAnEdgePruneLogsTheEndOfARunTheSessionEndsByUnsubscribing(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_edge_prune_unsubscribed"
	rig := startFailedRemovalRun(t, sessionID)
	requireStatus(t, rig.call(t, http.MethodPost, rig.personToken(sessionID, "PRIV"), "/v1/interests/unsubscribe",
		fmt.Sprintf(`{"session_id":%q,"topics":[%q]}`, sessionID, edgeRowTwo)), http.StatusOK, "the session's unsubscribe from acme/two")
	testwait.Eventually(t, "the listener's copy of the interests to lose acme/two", func() bool {
		items, err := rig.deps.registry.List()
		return err == nil && !slices.ContainsFunc(items, func(item store.Interest) bool {
			return item.SessionID == sessionID && slices.Contains(item.Topics, edgeRowTwo)
		})
	})
	if _, complete := rig.reader.standing([]string{"PRIV"}); !complete {
		t.Fatalf("UNKNOWN: PRIV's answer is not fresh, so the pass would weigh nothing")
	}
	requireRemovalRunEnd(t, rig, sessionID, noneOutsideWhy)
}

// A run of failed removals ends, logged, when the session's interest record becomes another
// listener's, as when the session registers again through it. Control: with that end's line removed
// (the ended call for another listener's record), the pass logs no end line and the row fails.
func TestAnEdgePruneLogsTheEndOfARunWhenTheSessionMovesToAnotherListener(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_edge_prune_moved"
	rig := startFailedRemovalRun(t, sessionID)
	if _, err := rig.deps.registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "shared-listener"}, []string{edgeRowTwo}); err != nil {
		t.Fatalf("write %s's interest row as another listener's: %v", sessionID, err)
	}
	testwait.Eventually(t, "the listener's copy of the interests to name the other listener", func() bool {
		items, err := rig.deps.registry.List()
		return err == nil && slices.ContainsFunc(items, func(item store.Interest) bool {
			return item.SessionID == sessionID && item.MachineID == "shared-listener"
		})
	})
	requireRemovalRunEnd(t, rig, sessionID, "the session's interests are another listener's")
}

// A run of failed removals ends, logged, when the session's interest record is gone, as after its
// deregistration. Control: with that end's line removed (the ended call for a session the list no
// longer names), the pass logs no end line and the row fails.
func TestAnEdgePruneLogsTheEndOfARunWhenTheSessionsRecordIsGone(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_edge_prune_gone"
	rig := startFailedRemovalRun(t, sessionID)
	if _, err := rig.deps.registry.Remove(t.Context(), sessionID, nil, store.RoleReleaseAuthority{Force: true}); err != nil {
		t.Fatalf("deregister %s: %v", sessionID, err)
	}
	testwait.Eventually(t, "the listener's copy of the interests to lose the session", func() bool {
		items, err := rig.deps.registry.List()
		return err == nil && !slices.ContainsFunc(items, func(item store.Interest) bool { return item.SessionID == sessionID })
	})
	requireRemovalRunEnd(t, rig, sessionID, "the session's interest record is gone")
}

// A refresh pass that cannot list the interest records, because the listener's copy of the
// interest bucket has not followed the bucket for longer than its stale bound, logs that once,
// when its passes start failing to list, and once more when a list succeeds again, however many
// passes fail between. Control: with a WARN at each failed list and no line at the end, as before
// this row, three failed passes log three WARN lines and the row fails.
func TestAnEdgePruneLogsARunOfFailedListsOnceAndItsEnd(t *testing.T) {
	t.Parallel()
	const failed, again = "edge listener could not list the interests a repository may have left", "edge listener lists the interests again"
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest store: %v", err)
	}
	if err := registry.WaitForCacheReady(t.Context()); err != nil {
		t.Fatalf("warm the interest cache: %v", err)
	}
	clock := &listenerClock{now: time.Now()}
	replaceWatcher(t, registry, "watcher", "interest registry", unwrappedKV, kvwatch.WithNow(clock.Now))
	t.Cleanup(registry.StopWatch)
	testwait.Eventually(t, "the interest copy's first read", func() bool { return registry.Servable() == nil })
	logs := &cmdtest.LockedBuffer{}
	deps := &listenerDeps{registry: registry, repositories: newEdgeRepositories("http://broker.example", listenerBrokerAccount(edgeAccountToken), []string{"PRIV"},
		newEdgeWithheldCounter(metrics.New()), logging.NewWithWriter("test", logs))}

	registry.Disconnected()
	clock.Advance(staleBound + time.Second)
	if _, err := registry.List(); err == nil {
		t.Fatalf("the interest copy lists its records %s after it stopped following its bucket, want it refused", staleBound+time.Second)
	}
	for range 3 {
		pruneEdgeInterests(t.Context(), deps, "test-machine")
	}
	if lines := edgeLogLines(logs.String(), "WARN", failed); len(lines) != 1 {
		t.Fatalf("three passes that could not list the interests logged %d WARN lines, want 1, when the passes began to fail:\n%s", len(lines), logs.String())
	}

	if err := registry.Rewatch(t.Context(), client.Conn); err != nil {
		t.Fatalf("read the interest bucket again: %v", err)
	}
	testwait.Eventually(t, "the interest copy to list again", func() bool { _, err := registry.List(); return err == nil })
	pruneEdgeInterests(t.Context(), deps, "test-machine")
	pruneEdgeInterests(t.Context(), deps, "test-machine")
	if lines := edgeLogLines(logs.String(), "INFO", again); len(lines) != 1 || lines[0]["failed_passes"] != float64(3) {
		t.Fatalf("two passes that listed the interests after three that could not logged %d lines saying they list again, want 1 with failed_passes 3:\n%s", len(lines), logs.String())
	}
}

// A broker whose repository read never answers costs the session no more than one that answers
// 503 at once: the heartbeat of a session following a GitHub topic is refused 503
// repositories_unknown, and its session entry is still renewed, since the read ends
// edgeRegistrationReserve before the registration's budget does. The broker stand-in answers only
// when the read's own context ends, so the read takes all the time it is given, and the renewal is
// told from no write by the entry's revision, with no clock read. Control: with the read given the
// whole budget, the renewal runs on a context that has ended and the entry is not renewed.
// It must not run in parallel: the renewal has edgeRegistrationReserve (1 s) after the read for a
// bucket read and a write, which the -race run of the whole package outlasted beside its other rows.
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
