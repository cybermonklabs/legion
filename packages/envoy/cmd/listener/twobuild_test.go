//go:build multiprocess

// Two listener builds for one machine id, as a roll from one release to the next runs them
// (INFRA-557): one build that compiles in a stream subject the other does not know. Each row
// starts the builds' real binaries through multiprocess_harness_test.go's harness.
// TestTwoBuildsSkipOrDoubleNoMessage checks that no message on any subject is skipped, doubled, or
// dropped by both while the machine's delivery passes from one build to the other.
// TestTwoBuildsAnOlderPodRelaysATopicOnlyTheNewerBuildsSubjectCaptures checks that while the older
// build holds the machine's delivery, a session it registered receives a message the newer build
// publishes on a topic only the newer build's subject captures.
// TestTwoBuildsTheOlderPodsRelayKeepsATopicTheNewerPodRegisteredInsideTheRefreshWindow and
// TestTwoBuildsTheOlderPodStoresATopicTheNewerBuildAddedInsideTheRefreshWindow check the same
// delivery before the older pod's periodic read of the stream's subjects has seen the newer
// subject, through a registration with either pod, and
// TestTwoBuildsTheOlderPodStoresAPublishOnATopicTheNewerBuildAddedInsideTheRefreshWindow checks a
// message published through the older pod in that window. Like the rows beside them they use
// nothing a build before INFRA-557 (819dc283) lacks but cmdtest.Build's tags, so they run there
// with that harness beside them.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

const (
	// tbEvery is how often a row publishes, each time on the next of its topics in turn.
	tbEvery = 40 * time.Millisecond
	// tbNewerSubject is the stream subject only the envoy_twobuild build compiles in
	// (internal/bus/stream_extra_twobuild.go).
	tbNewerSubject = "notifications.twobuild.>"
	// tbSubjectsRead bounds how long a plain pod that connected before the other build added
	// tbNewerSubject takes to judge a topic on it captured. A pod of this build judges a topic
	// uncaptured only on a read of the deployed subjects that started after the judgement asked
	// (bus.SubjectsAsk.Uncaptured), so it judges it at once; one built before that reads them only
	// at each ensure of the stream, which runs at connect and after every reconnect, and every
	// streamSubjectsRefresh (internal/bus/nats.go, a minute) from its connect, each read within
	// streamSubjectsRead (10 s). The 5 s past those is the registrations that observe it.
	tbSubjectsRead = 75 * time.Second
	// tbSubjectsRefresh is streamSubjectsRefresh (internal/bus/nats.go), the period of a pod's
	// own read of the deployed stream's subjects, the first of them that long after the pod's
	// client connected: a row that has to run before a pod's first such read runs inside it.
	tbSubjectsRefresh = time.Minute
)

// tbTargets is a topic on every subject the plain build's stream stores (bus.StreamSubjects),
// each with its own session, named for the subject so a report names the subject that lost or
// doubled a message. A subject no session may follow is left out: /v1/interests/subscribe drops
// a topic reaching a private stream subject other than the session's own agent subject
// (bus.ReachesPrivateStreamSubject: the Legion controller lane and the failed-delivery lanes),
// so a session on one would never be relayed.
//
// `notifications.twobuild.>`, the subject only the other build compiles in
// (internal/bus/stream_extra_twobuild.go), gets no session in this row, whose plain pod registers
// every session before a pod of the other build has added the subject to the stream;
// TestTwoBuildsAnOlderPodRelaysATopicOnlyTheNewerBuildsSubjectCaptures measures one. A pod of the
// plain build serves it: a registration stores every followed topic, a session consumer's filter
// carries each one a consumer can filter on whatever the deployed stream's subjects capture
// (sessionConsumerFilter, bus.CanFilterOn), and no build's ensure drops a subject it
// does not compile in. So once a pod of the other build has added the subject, a session
// following a topic on it is served by a pod of either build, whichever build holds the term. A
// pod of this build names such a topic in a warning only while a read of the subjects that
// started after the registration asked leaves it uncaptured (bus.SubjectsAsk.Uncaptured); a pod
// built before this release that has not read the subjects since leaves the topic out of a
// registration and names it in the answer's warning.
func tbTargets() []mpTarget {
	subjects := bus.StreamSubjects()
	var targets []mpTarget
	for _, subject := range subjects {
		prefix := strings.TrimSuffix(subject, ">")
		name := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(prefix, bus.NotificationTopicPrefix), "."), ".", "_")
		sessionID := "ses_tb_" + name
		if prefix == "notifications.agent." {
			targets = append(targets, mpInbox(sessionID))
			continue
		}
		topic := prefix + "mp-twobuild"
		// The target holds no role, so a controller notice lane is private to it as every other
		// private subject is (bus.ReachesPrivateStreamSubject's roles, a parameter 2af83d53fc (#2603) added).
		if bus.ReachesPrivateStreamSubject(sessionID, topic, nil) {
			continue
		}
		targets = append(targets, mpTarget{topic: topic, session: sessionID})
	}
	return targets
}

// tbRegister registers every target's session through the pod, on consumer delivery, subscribed to
// its topic unless the topic is its own inbox.
func tbRegister(t *testing.T, pod *mpPod, serving *mpNATSSession, targets []mpTarget) {
	t.Helper()
	for _, target := range targets {
		if target == mpInbox(target.session) {
			pod.registerNATS(t, serving, target.session)
			continue
		}
		pod.registerNATS(t, serving, target.session, target.topic)
	}
}

// tbKill SIGKILLs the pod between two publishes, once every message published so far has reached
// its session and the durable holds none unacknowledged, and resumes publishing a second after the
// pod has exited. A message a killed pod holds is TestTwoPodsSIGKILLHandsOverInOrderWithin15s's;
// here the kill is the hand-over from one build to the other. Nor is a message sent over core
// NATS, which the server hands the killed pod before it sees that pod's connection close,
// publishable then: a core message is delivered at most once, and the second after the exit is
// the server's to drop the dead pod's subscription. A message the previous steps skipped is
// named here, by awaitAll, before the kill.
func tbKill(t *testing.T, publisher *mpPublisher, serving *mpNATSSession, pod *mpPod) time.Time {
	t.Helper()
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	serving.awaitAll(t, slices.Clone(publisher.sent), 30*time.Second)
	mpEventually(t, 10*time.Second, "the durable to hold no message unacknowledged", func() bool { return publisher.ackPending() == 0 })
	killed := pod.kill(t)
	pod.WaitExit(t, "SIGKILL")
	time.Sleep(time.Second)
	return killed
}

// tbRequireEachOnce waits for every message to reach its session, then past the ack wait for any
// repeat, and fails the test unless each reached it exactly once.
func tbRequireEachOnce(t *testing.T, publisher *mpPublisher, serving *mpNATSSession) {
	t.Helper()
	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireKnown(t, messages)
	serving.requireOnce(t, messages)
	perSession := map[string]int{}
	for _, m := range messages {
		perSession[m.session]++
	}
	t.Logf("%d messages, each delivered exactly once: %v", len(messages), perSession)
}

// No message on any subject is skipped, doubled, or dropped by both while two builds share the
// machine's delivery: a plain build and one built with the envoy_twobuild tag, which compiles in a
// stream subject more, through a graceful and a hard take-over in each direction. The plain build is
// active and the other build standby; a SIGTERM of the plain pod makes the other build active; a
// plain pod starts as its standby; a SIGKILL of the other build makes the plain build active again.
// A message is published every 40 ms throughout, on each subject the plain build's stream stores
// (tbTargets), each to a session of its own: each session takes each of its messages exactly once.
// The subject only the other build adds has no session here: a session following it is
// TestTwoBuildsAnOlderPodRelaysATopicOnlyTheNewerBuildsSubjectCaptures's.
func TestTwoBuildsSkipOrDoubleNoMessage(t *testing.T) {
	plain := cmdtest.Build(t, "envoy-listener")

	t.Run("a build with a stream subject more", func(t *testing.T) {
		extra := cmdtest.Build(t, "envoy-listener-twobuild", "envoy_twobuild")
		uri := mpNATS(t)
		serving := mpStartNATSSessions(t, uri)
		targets := tbTargets()
		plainA := mpStartPod(t, plain, uri, "pod-a")
		mpWaitActive(t, plainA, 30*time.Second)
		tbRegister(t, plainA, serving, targets)
		publisher := mpConnectPublisher(t, uri)
		stop := publisher.everyOn(tbEvery, targets...)
		time.Sleep(3 * time.Second)

		extraB := mpStartPod(t, extra, uri, "pod-b")
		mpWaitStandby(t, extraB, 30*time.Second)
		time.Sleep(3 * time.Second)

		signalled := plainA.Terminate(t)
		// A build before INFRA-557 keeps the durable through its shutdown, and the standby binds
		// only after it.
		mpWaitActive(t, extraB, 60*time.Second)
		t.Logf("%s (envoy_twobuild) took over %s after %s's SIGTERM", extraB.instance, time.Since(signalled).Round(time.Millisecond), plainA.instance)
		plainA.WaitExit(t, "SIGTERM")
		time.Sleep(3 * time.Second)

		plainC := mpStartPod(t, plain, uri, "pod-c")
		mpWaitStandby(t, plainC, 30*time.Second)
		time.Sleep(3 * time.Second)

		killed := tbKill(t, publisher, serving, extraB)
		mpWaitActive(t, plainC, 30*time.Second)
		t.Logf("%s (plain) took over %s after %s's SIGKILL", plainC.instance, time.Since(killed).Round(time.Millisecond), extraB.instance)
		time.Sleep(3 * time.Second)
		stop()

		tbRequireEachOnce(t, publisher, serving)
	})
}

// While two builds serve the machine during a roll, the older build delivers a message on a topic
// only the newer build's stream subject captures: the plain pod A is the machine's active pod and
// so runs the relay of the session's consumer, the envoy_twobuild pod B is its standby, B publishes
// on a topic under notifications.twobuild.> through its own POST /v1/messages/publish, and the
// session A registered takes that message exactly once within mpGracefulBound of the publish, A
// active and B standby at the publish and at the receipt.
//
// B's start puts the subject on the deployed stream: every pod, a standby too, connects through
// bus.ConnectOwningStream, whose ensure (ensureStreamWithConfig) adds the subjects its build
// compiles and keeps every one it finds, which the row reads back from the stream itself. A, which
// connected before B, judges a topic on the subject uncaptured only on a read of the stream's
// subjects that started after the judgement asked (bus.SubjectsAsk.Uncaptured), and a plain build
// from before that rule reads them only at its next streamSubjectsRefresh. That read has no line
// of its own, so the row watches for it where it shows: a registration through A of a probe
// session following another topic under the subject, which A answers with a warning naming the
// topic until it has read the subject (uncapturedTopics), once a second until tbSubjectsRead; a
// plain pod of this build answers the first without one. The session itself then registers
// through A once, which answers 200 storing the topic and with no warning.
//
// Before #2461 the plain build filtered a session's consumer on the subjects it compiles
// (bus.StreamCaptures), so the topic was out of the session consumer's filter and the message
// never reached the session. The filter now carries every followed topic a consumer can filter
// on, whatever any stream's subjects capture (sessionConsumerFilter, bus.CanFilterOn).
func TestTwoBuildsAnOlderPodRelaysATopicOnlyTheNewerBuildsSubjectCaptures(t *testing.T) {
	const (
		sessionID = "ses_tb_newer_subject"
		topic     = "notifications.twobuild.mp-newer-subject"
		probeID   = "ses_tb_newer_subject_probe"
		probe     = "notifications.twobuild.mp-newer-subject-probe"
		event     = "evt-tb-newer-subject"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("the plain build compiles a subject capturing %s; the row needs a topic only the envoy_twobuild build's subject captures", topic)
	}
	plain := cmdtest.Build(t, "envoy-listener")
	newer := cmdtest.Build(t, "envoy-listener-twobuild", "envoy_twobuild")
	uri := mpNATS(t)
	serving := mpStartNATSSessions(t, uri)
	mpJournalOf(t).readsFrom(uri)

	started := time.Now()
	plainA := mpStartPod(t, plain, uri, "pod-a")
	mpWaitActive(t, plainA, 30*time.Second)
	newerB := mpStartPod(t, newer, uri, "pod-b")
	mpWaitStandby(t, newerB, 30*time.Second)
	added := time.Now()
	reader := tbStreamReader(t, uri)
	tbRequireStreamSubject(t, reader, tbNewerSubject)
	reader.Close()

	tbAwaitCaptured(t, plainA, serving, probeID, probe, tbSubjectsRead)
	t.Logf("%s (plain) judged %s captured %s after %s (envoy_twobuild) was standby, %s after %s started",
		plainA.instance, probe, time.Since(added).Round(time.Millisecond), newerB.instance, time.Since(started).Round(time.Millisecond), plainA.instance)
	if topics, warnings := tbRegisterOn(t, plainA, serving, sessionID, topic); !slices.Contains(topics, topic) || len(warnings) > 0 {
		t.Fatalf("%s's registration through %s stored %v with warnings %q; want %s stored and no warning", sessionID, plainA.instance, topics, warnings, topic)
	}

	tbRequireRoles(t, plainA, newerB, "at the publish")
	m := tbPublishThrough(t, newerB, sessionID, topic, event)
	serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
	tbRequireRoles(t, plainA, newerB, "at the receipt")
	if at, took := serving.first(sessionID, event); took {
		t.Logf("%s reached %s %s after %s (envoy_twobuild, standby) published it on %s, relayed by %s (plain, active)",
			event, sessionID, at.Sub(m.published).Round(time.Millisecond), newerB.instance, topic, plainA.instance)
	}
	time.Sleep(mpQuiet)
	serving.requireKnown(t, []mpMessage{m})
	serving.requireOnce(t, []mpMessage{m})
}

// tbRoll is a roll's two pods inside the plain pod's refresh window: A, the plain pod, is the
// machine's active pod and B, the envoy_twobuild pod, its standby, and B's start added
// tbNewerSubject to the stream after A's client last read the stream's subjects.
type tbRoll struct {
	uri     string
	serving *mpNATSSession
	plainA  *mpPod
	newerB  *mpPod
	// aStarted is when the row started A's process, before A's client connected, so the time
	// since it is at least the time since A's connect, from which A's first periodic read of the
	// stream's subjects is tbSubjectsRefresh away.
	aStarted time.Time
}

// tbStartRoll starts the plain pod A, behind an mpLaneRelay so a row can hold one of its
// connections, and waits for it to be active, reads back from the stream, as a client of the
// row's own, that A's start left it without tbNewerSubject, then starts the envoy_twobuild pod B,
// waits for it to be the standby, and reads back that B's start added the subject. A's client read
// the stream's subjects at its ensure, which ran before A was active, and reads them next
// tbSubjectsRefresh after its connect (tbRoll.requireInsideRefresh), unless a judgement of a topic
// it holds uncaptured reads them first (bus.SubjectsAsk.Uncaptured, or a publish's transport,
// bus.Client.PublishTakesJetStream): until either, A holds subjects without tbNewerSubject.
// Nothing here asks A anything that would read them.
func tbStartRoll(t *testing.T) *tbRoll {
	t.Helper()
	plain := cmdtest.Build(t, "envoy-listener")
	newer := cmdtest.Build(t, "envoy-listener-twobuild", "envoy_twobuild")
	roll := &tbRoll{uri: mpNATS(t)}
	roll.serving = mpStartNATSSessions(t, roll.uri)
	mpJournalOf(t).readsFrom(roll.uri)
	roll.aStarted = time.Now()
	roll.plainA = mpStartPodBehindLanes(t, plain, roll.uri, "pod-a")
	mpWaitActive(t, roll.plainA, 30*time.Second)
	reader := tbStreamReader(t, roll.uri)
	defer reader.Close()
	if subjects := tbStreamSubjects(t, reader); slices.Contains(subjects, tbNewerSubject) {
		t.Fatalf("the stream %s carries %v before %s (envoy_twobuild) started; the row needs %s's own read without %s", bus.Stream, subjects, "pod-b", roll.plainA.instance, tbNewerSubject)
	}
	roll.newerB = mpStartPod(t, newer, roll.uri, "pod-b")
	mpWaitStandby(t, roll.newerB, 30*time.Second)
	tbRequireStreamSubject(t, reader, tbNewerSubject)
	t.Logf("%s (envoy_twobuild) is standby and the stream carries %s, %s after %s (plain) started",
		roll.newerB.instance, tbNewerSubject, time.Since(roll.aStarted).Round(time.Millisecond), roll.plainA.instance)
	return roll
}

// requireInsideRefresh fails the test unless the moment when names comes before A's first
// periodic read of the stream's subjects, tbSubjectsRefresh after its client connected, and logs
// how long after A's start it came: an upper bound on the time since A's connect.
func (r *tbRoll) requireInsideRefresh(t *testing.T, when string) {
	t.Helper()
	since := time.Since(r.aStarted)
	if since >= tbSubjectsRefresh {
		t.Fatalf("%s came %s after %s started, past its first periodic read of the stream's subjects (%s after its connect); the row measures the window before it",
			when, since.Round(time.Millisecond), r.plainA.instance, tbSubjectsRefresh)
	}
	t.Logf("%s came %s after %s started, inside its first %s, before its first periodic read of the stream's subjects",
		when, since.Round(time.Millisecond), r.plainA.instance, tbSubjectsRefresh)
}

// publishThroughNewer publishes a message on topic for sessionID through B's own POST
// /v1/messages/publish (tbPublishThrough): B's client holds tbNewerSubject, so it stores the
// message.
func (r *tbRoll) publishThroughNewer(t *testing.T, sessionID, topic, event string) mpMessage {
	t.Helper()
	return tbPublishThrough(t, r.newerB, sessionID, topic, event)
}

// tbPublishThrough publishes a message on topic for sessionID through the pod's own POST
// /v1/messages/publish, as the newer build publishes it, requires a 200, and journals it.
func tbPublishThrough(t *testing.T, pod *mpPod, sessionID, topic, event string) mpMessage {
	t.Helper()
	published := time.Now()
	body := fmt.Sprintf(`{"topic":%q,"message":%q}`, topic, "a message the newer build publishes mp-event="+event)
	if answer := pod.call(http.MethodPost, "/v1/messages/publish", body); answer.status != http.StatusOK {
		t.Fatalf("publish %s through %s: status %d %s %v", body, pod.instance, answer.status, answer.body, answer.err)
	}
	m := mpMessage{event: event, session: sessionID, published: published}
	mpJournalOf(t).publish(m, topic)
	return m
}

// While two builds serve the machine during a roll, a session that registers through the newer
// pod B, following a topic only B's stream subject captures, receives a message on that topic
// relayed by the older pod A before A's first periodic read of the stream's subjects. B, a
// standby, serves /v1, stores the topic on the session's row and answers no warning, since its
// own subjects capture the topic. A, the active pod, runs the session's relay: it hears B's entry
// and restarts the session's consumer on the filter it builds from the row (sessionRelays.restart,
// sessionConsumerFilter).
// A hears the entry only once B's registration has answered: the row holds A's main connection,
// which carries A's watch of the session bucket, while it registers through B, and lets it go
// after, as a watch that lags B's writes does in a roll. B's registration ends on its own repair
// of the consumer from the row (confirmRegistered), which sets the filter from the row again,
// so a restart of A's landing before that repair would leave B's filter standing, and the row
// would measure B rather than A. Held for that registration alone, the connection is lost to
// neither the server nor A, which reads nothing of the stream meanwhile.
// The row waits until A pulls from that consumer, which it does only after that restart has
// written the filter, and logs the filter as the row's own client reads it; it then publishes the
// message on the topic through B, and a second message on the session's agent subject, which the
// filter always keeps, and waits for that second message: A's relay has pulled past the
// first by then. The first takes the session exactly once within mpGracefulBound of its publish,
// all inside A's refresh window (tbRoll.requireInsideRefresh), A active and B standby throughout.
//
// Before A's filter carried every followed topic a consumer can filter on (sessionConsumerFilter,
// bus.CanFilterOn), A built it on the subjects it held, and a build before this release
// on the subjects it read at its connect, without B's, and left the topic out of it. A filter write
// keeps the consumer's position, and the server moves a pulling consumer past a message its filter
// does not match, so the message on the topic was skipped and never reached the session.
func TestTwoBuildsTheOlderPodsRelayKeepsATopicTheNewerPodRegisteredInsideTheRefreshWindow(t *testing.T) {
	const (
		sessionID = "ses_tb_roll_window_relay"
		topic     = "notifications.twobuild.mp-roll-window-relay"
		event     = "evt-tb-roll-window-relay"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("the plain build compiles a subject capturing %s; the row needs a topic only the envoy_twobuild build's subject captures", topic)
	}
	roll := tbStartRoll(t)
	publisher := mpConnectPublisher(t, roll.uri)
	release := roll.plainA.lanes.cut(mpReceiveConnection(roll.plainA.instance))
	topics, warnings := tbRegisterOn(t, roll.newerB, roll.serving, sessionID, topic)
	release()
	if !slices.Contains(topics, topic) || len(warnings) > 0 {
		t.Fatalf("%s's registration through %s stored %v with warnings %q; want %s stored and no warning", sessionID, roll.newerB.instance, topics, warnings, topic)
	}
	filter := tbAwaitRelayPull(t, publisher, sessionID, 15*time.Second)
	t.Logf("%s (plain, active) pulls %s's consumer, which filters on %v", roll.plainA.instance, sessionID, filter)

	tbRequireRoles(t, roll.plainA, roll.newerB, "at the publish")
	roll.requireInsideRefresh(t, "the publish")
	m := roll.publishThroughNewer(t, sessionID, topic, event)
	next := publisher.mustPublish(t, sessionID)
	roll.serving.awaitAll(t, []mpMessage{next}, mpGracefulBound)
	roll.serving.awaitAll(t, []mpMessage{m}, max(time.Until(m.published.Add(mpGracefulBound)), 0))
	roll.requireInsideRefresh(t, "the receipt")
	tbRequireRoles(t, roll.plainA, roll.newerB, "at the receipt")
	if at, took := roll.serving.first(sessionID, event); took {
		t.Logf("%s reached %s %s after %s (envoy_twobuild, standby) published it on %s, relayed by %s (plain, active)",
			event, sessionID, at.Sub(m.published).Round(time.Millisecond), roll.newerB.instance, topic, roll.plainA.instance)
	}
	time.Sleep(mpQuiet)
	roll.serving.requireKnown(t, []mpMessage{m, next})
	roll.serving.requireOnce(t, []mpMessage{m, next})
}

// While two builds serve the machine during a roll, the older pod A, registering a session that
// follows a topic only the newer pod B's stream subject captures before A's first periodic read
// of the stream's subjects, stores the topic and answers no warning, and the session takes a
// message B publishes on it exactly once within mpGracefulBound, relayed by A. A stores every
// followed topic whatever the subjects it holds capture, and names one in a warning only once a
// read of the subjects that started after the registration asked leaves it uncaptured
// (bus.SubjectsAsk.Uncaptured); the registration comes inside A's refresh window
// (tbRoll.requireInsideRefresh), and A is active and B standby throughout.
//
// Before that rule, a build left the topic out of the registration on the subjects it read at its
// connect, with a warning naming it, so the session's row never held it and no message on it
// reached the session.
func TestTwoBuildsTheOlderPodStoresATopicTheNewerBuildAddedInsideTheRefreshWindow(t *testing.T) {
	const (
		sessionID = "ses_tb_roll_window_register"
		topic     = "notifications.twobuild.mp-roll-window-register"
		event     = "evt-tb-roll-window-register"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("the plain build compiles a subject capturing %s; the row needs a topic only the envoy_twobuild build's subject captures", topic)
	}
	roll := tbStartRoll(t)
	roll.requireInsideRefresh(t, "the registration")
	if topics, warnings := tbRegisterOn(t, roll.plainA, roll.serving, sessionID, topic); !slices.Contains(topics, topic) || len(warnings) > 0 {
		t.Fatalf("%s's registration through %s stored %v with warnings %q; want %s stored and no warning", sessionID, roll.plainA.instance, topics, warnings, topic)
	}

	tbRequireRoles(t, roll.plainA, roll.newerB, "at the publish")
	m := roll.publishThroughNewer(t, sessionID, topic, event)
	roll.serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
	roll.requireInsideRefresh(t, "the receipt")
	tbRequireRoles(t, roll.plainA, roll.newerB, "at the receipt")
	if at, took := roll.serving.first(sessionID, event); took {
		t.Logf("%s reached %s %s after %s (envoy_twobuild, standby) published it on %s, relayed by %s (plain, active)",
			event, sessionID, at.Sub(m.published).Round(time.Millisecond), roll.newerB.instance, topic, roll.plainA.instance)
	}
	time.Sleep(mpQuiet)
	roll.serving.requireKnown(t, []mpMessage{m})
	roll.serving.requireOnce(t, []mpMessage{m})
}

// While two builds serve the machine during a roll, a message published through the older pod A's
// own POST /v1/messages/publish, on a topic only the newer pod B's stream subject captures and
// before A's first periodic read of the stream's subjects, is answered 200, stored on the stream,
// and taken exactly once within mpGracefulBound of its publish by a session following the topic,
// relayed by A. A judges the topic on the subjects it holds, read at its connect, without B's, and
// refuses the publish only on a read of them that started after the publish asked
// (bus.SubjectsAsk.Uncaptured); the publish and the receipt come inside A's refresh window
// (tbRoll.requireInsideRefresh), and A is active and B standby throughout.
//
// The topic is registered through B, because nothing but the publish may ask A to read the
// subjects, and a registration through A following the topic has A read them for its warning
// (uncapturedTopics). A relay reads nothing of them (sessionRelays.restart, sessionConsumerFilter).
// So the session first registers through A following its own inbox alone, which A judges with no
// read, and A's relay starts the session's consumer on that filter and pulls. The session then
// registers through B following the topic, and B stores it on the session's row and sets the
// consumer's filter to it in place (bus.SessionConsumers.Ensure). A hears that entry change, and
// its relay, already pulling that consumer, restarts nothing (sessionRelays.apply): the row reads
// the filter with the topic, and A's pull standing on it, as its own client reads the consumer.
//
// Before the publish route refused a topic only on a read of the subjects that started after the
// request asked, A refused the publish 400 on the subjects it read at its connect
// (bus.Client.ReachesNoSession), and nothing was published. The publish's own transport reads
// the subjects again too, which this row does not tell apart: the route's read has updated A's
// subjects by then (internal/bus's TestAPublishJudgesItsTransportOnAFreshReadOfTheSubjects is that
// judgement's row).
func TestTwoBuildsTheOlderPodStoresAPublishOnATopicTheNewerBuildAddedInsideTheRefreshWindow(t *testing.T) {
	const (
		sessionID = "ses_tb_roll_window_publish"
		topic     = "notifications.twobuild.mp-roll-window-publish"
		event     = "evt-tb-roll-window-publish"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("the plain build compiles a subject capturing %s; the row needs a topic only the envoy_twobuild build's subject captures", topic)
	}
	roll := tbStartRoll(t)
	publisher := mpConnectPublisher(t, roll.uri)
	roll.plainA.registerNATS(t, roll.serving, sessionID)
	tbAwaitRelayPull(t, publisher, sessionID, 15*time.Second)
	if topics, warnings := tbRegisterOn(t, roll.newerB, roll.serving, sessionID, topic); !slices.Contains(topics, topic) || len(warnings) > 0 {
		t.Fatalf("%s's registration through %s stored %v with warnings %q; want %s stored and no warning", sessionID, roll.newerB.instance, topics, warnings, topic)
	}
	filter := tbAwaitRelayPull(t, publisher, sessionID, 15*time.Second)
	if !slices.Contains(filter, topic) {
		t.Fatalf("%s's consumer filters on %v after its registration through %s, without %s", sessionID, filter, roll.newerB.instance, topic)
	}
	t.Logf("%s (plain, active) pulls %s's consumer, which filters on %v", roll.plainA.instance, sessionID, filter)

	tbRequireRoles(t, roll.plainA, roll.newerB, "at the publish")
	roll.requireInsideRefresh(t, "the publish")
	published := time.Now()
	body := fmt.Sprintf(`{"topic":%q,"message":%q}`, topic, "a message published through the older pod mp-event="+event)
	answer := roll.plainA.call(http.MethodPost, "/v1/messages/publish", body)
	if answer.status != http.StatusOK {
		t.Fatalf("publish %s through %s (plain): status %d %s %v; want 200, the stream capturing %s since %s (envoy_twobuild) started",
			body, roll.plainA.instance, answer.status, answer.body, answer.err, topic, roll.newerB.instance)
	}
	m := mpMessage{event: event, session: sessionID, published: published}
	mpJournalOf(t).publish(m, topic)
	seq := tbRequireStored(t, publisher, topic, answer.body)
	t.Logf("%s (plain) answered 200 and the stream stored the message on %s at sequence %d", roll.plainA.instance, topic, seq)
	roll.serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
	roll.requireInsideRefresh(t, "the receipt")
	tbRequireRoles(t, roll.plainA, roll.newerB, "at the receipt")
	if at, took := roll.serving.first(sessionID, event); took {
		t.Logf("%s reached %s %s after %s (plain, active) took its publish on %s and relayed it",
			event, sessionID, at.Sub(published).Round(time.Millisecond), roll.plainA.instance, topic)
	}
	time.Sleep(mpQuiet)
	roll.serving.requireKnown(t, []mpMessage{m})
	roll.serving.requireOnce(t, []mpMessage{m})
}

// tbRequireStored fails the test unless the stream's last message on topic, as the row's own
// client reads the stream, is the envelope a publish route answered with answer, and returns its
// stream sequence. The read cannot tell which transport the publish took: the stream stores a core
// NATS publish on a subject it captures as it stores a JetStream one.
func tbRequireStored(t *testing.T, publisher *mpPublisher, topic, answer string) uint64 {
	t.Helper()
	var answered contracts.Envelope
	if err := json.Unmarshal([]byte(answer), &answered); err != nil {
		t.Fatalf("decode the publish's answer %s: %v", answer, err)
	}
	stored, err := publisher.client.JS().GetLastMsg(bus.Stream, topic)
	if err != nil {
		t.Fatalf("the stream %s holds no message on %s after a publish answered 200 with event %s: %v", bus.Stream, topic, answered.EventID, err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(stored.Data, &envelope); err != nil {
		t.Fatalf("decode the stream's last message on %s: %v", topic, err)
	}
	if envelope.EventID != answered.EventID {
		t.Fatalf("the stream's last message on %s is event %s, not the published %s", topic, envelope.EventID, answered.EventID)
	}
	return stored.Sequence
}

// tbAwaitRelayPull waits until a pull request stands on sessionID's consumer, as the row's own
// client reads the consumer, and returns the consumer's filter then. Only the machine's active pod
// relays a session's consumer, and its relay pulls only once its restart has written the filter.
func tbAwaitRelayPull(t *testing.T, publisher *mpPublisher, sessionID string, within time.Duration) []string {
	t.Helper()
	name, ok := bus.SessionConsumerName(sessionID)
	if !ok {
		t.Fatalf("%s names no session consumer", sessionID)
	}
	var filter []string
	mpEventually(t, within, "a pull on "+sessionID+"'s consumer", func() bool {
		info, err := publisher.client.JS().ConsumerInfo(bus.Stream, name)
		if err != nil || info.NumWaiting == 0 {
			return false
		}
		filter = slices.Clone(info.Config.FilterSubjects)
		if info.Config.FilterSubject != "" {
			filter = append(filter, info.Config.FilterSubject)
		}
		slices.Sort(filter)
		return true
	})
	return filter
}

// tbStreamReader is a client of the test's own on natsURL, which reads the deployed stream
// (tbStreamSubjects); the caller closes it.
func tbStreamReader(t *testing.T, natsURL string) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the stream's reader: %v", err)
	}
	return client
}

// tbStreamSubjects is the subjects the deployed stream carries, as reader, a client of the test's
// own (tbStreamReader), reads the stream.
func tbStreamSubjects(t *testing.T, reader *bus.Client) []string {
	t.Helper()
	info, err := reader.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream %s: %v", bus.Stream, err)
	}
	return info.Config.Subjects
}

// tbRequireStreamSubject fails the test unless the deployed stream carries subject, as reader, a
// client of the test's own (tbStreamReader), reads the stream.
func tbRequireStreamSubject(t *testing.T, reader *bus.Client, subject string) {
	t.Helper()
	if subjects := tbStreamSubjects(t, reader); !slices.Contains(subjects, subject) {
		t.Fatalf("the stream %s carries %v, without %s", bus.Stream, subjects, subject)
	}
}

// tbAwaitCaptured registers sessionID through the pod following topic, once a second, until the
// pod answers with no warning, and fails the test when it still warns past within. A pod of this
// build stores the topic and names it in the answer's warning while a read of the subjects that
// started after the registration asked leaves it uncaptured (bus.SubjectsAsk.Uncaptured), and a
// pod built before this release leaves it out of the registration with a warning while the
// subjects it read before the subject capturing topic was added say so. So the first answer
// without one is the pod's read of that subject: at that registration itself for a pod of this
// build, at its next periodic read for one built before it.
func tbAwaitCaptured(t *testing.T, pod *mpPod, serving *mpNATSSession, sessionID, topic string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); ; time.Sleep(time.Second) {
		_, warnings := tbRegisterOn(t, pod, serving, sessionID, topic)
		if len(warnings) == 0 {
			return
		}
		for _, warning := range warnings {
			if !strings.Contains(warning, topic) {
				t.Fatalf("%s's registration through %s warned of more than %s: %q", sessionID, pod.instance, topic, warnings)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still warned that no stream subject captures %s in its answer to %s's registration %s after the other build added its subject: %q", pod.instance, topic, sessionID, within, warnings)
		}
	}
}

// tbRegisterOn registers sessionID through the pod on consumer delivery, following topic, requires
// a 200, and returns the topics the answer stored and its warnings.
func tbRegisterOn(t *testing.T, pod *mpPod, serving *mpNATSSession, sessionID, topic string) (topics, warnings []string) {
	t.Helper()
	answer := pod.subscribe(t, mpNATSRegistration(t, serving, sessionID, topic))
	var registered struct {
		Topics   []string `json:"topics"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(answer.body), &registered); err != nil {
		t.Fatalf("decode %s's registration answer %s: %v", sessionID, answer.body, err)
	}
	return registered.Topics, registered.Warnings
}

// tbRequireRoles fails the test unless active is the machine's active pod and standby its standby,
// at the moment when names.
func tbRequireRoles(t *testing.T, active, standby *mpPod, when string) {
	t.Helper()
	if !active.active() {
		t.Fatalf("%s %s is not the machine's active pod", when, active.instance)
	}
	if !standby.standby() {
		t.Fatalf("%s %s is not the machine's standby", when, standby.instance)
	}
}
