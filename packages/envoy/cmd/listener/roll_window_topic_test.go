package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// The rows here play a roll: this build's listener runs against the stream a deployment of this
// build left, and a newer build's start adds a subject this build does not compile
// (rollNewerSubject) while the listener runs.
const (
	// rollNewerSubject is the subject the newer build adds, and rollTopic a topic only it
	// captures.
	rollNewerSubject = "notifications.x.>"
	rollTopic        = "notifications.x.roll"
	// rollRereadGap is bus.streamSubjectsRereadGap, the least time between the end of one read
	// of the deployed subjects a judgement makes and the start of the next.
	rollRereadGap = 250 * time.Millisecond
	// rollPull is how long a pull of a session's consumer waits for its messages.
	rollPull = 300 * time.Millisecond
)

// rollWindowRig is a listener's /v1 routes whose client dials dial (the server at url, or a relay
// to it) and owns the stream a deployment of this build left there, which it leaves as it is, and
// a raw JetStream context of the row's own on url, which plays the newer build and pulls the
// session consumers as a session's relay does.
func rollWindowRig(t *testing.T, url, dial string) (*sessionConsumerRig, natsgo.JetStreamContext) {
	t.Helper()
	if bus.StreamCaptures(rollTopic) {
		t.Fatalf("this build compiles a subject capturing %s; the rows need a topic only a newer build's subject captures", rollTopic)
	}
	deployNewerBuildsStream(t, url)
	client, err := bus.ConnectOwningStream([]string{dial}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, url)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open the row's JetStream: %v", err)
	}
	return rig, js
}

// addStreamSubject plays the newer build's start: it adds subject to the stream.
func addStreamSubject(t *testing.T, js natsgo.JetStreamContext, subject string) {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read %s: %v", bus.Stream, err)
	}
	config := info.Config
	config.Subjects = append(slices.Clone(config.Subjects), subject)
	if _, err := js.UpdateStream(&config); err != nil {
		t.Fatalf("the newer build adds %s: %v", subject, err)
	}
}

// publishOn posts a publish of topic to handler and returns its answer.
func publishOn(handler http.Handler, topic string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish",
		strings.NewReader(`{"topic":"`+topic+`","message":"hello"}`)))
	return recorder
}

// requireStored fails the row unless the stream holds a message on topic.
func requireStored(t *testing.T, js natsgo.JetStreamContext, topic string) {
	t.Helper()
	if _, err := js.GetLastMsg(bus.Stream, topic); err != nil {
		t.Fatalf("the stream holds no message on %s after a publish answered 200: %v", topic, err)
	}
}

// A publish through the older pod on a topic only the subject the newer build added captures is
// accepted and stored when the newer build added that subject inside the 250 ms after the pod's
// last read of the stream's subjects ended: the route refuses a topic only on a read that started
// after the publish asked, so it waits out the rest of the gap and reads (bus.SubjectsAsk).
//
// Before that rule, the route judged a topic uncaptured on the subjects it held inside the gap,
// and refused the publish 400, "no stream subject captures", for a topic the stream stores.
func TestAPublishOnATopicASubjectAddedInsideTheRereadGapCapturesIsNotRefused(t *testing.T) {
	const uncaptured = "notifications.lxspec.probe.gap"
	url := testnats.URL(t)
	rig, js := rollWindowRig(t, url, url)

	// A publish of a topic no stream captures reads the subjects again, and is refused on that
	// read: the read whose end the gap runs from.
	if answer := publishOn(rig.mux, uncaptured); answer.Code != http.StatusBadRequest {
		t.Fatalf("a publish on %s, which no stream captures, was answered %d %s, want 400", uncaptured, answer.Code, answer.Body.String())
	}
	// The gap runs from that read's end (bus.Client.SubjectsRereadEnded), which comes before the
	// answer, so the guard below times the newer build's subject from the read's end and not from
	// the answer: a run slow between the two would otherwise take a subject added past the gap for
	// one inside it, and the publish below would then read the subjects again at once, which proves
	// nothing of the wait.
	read := rig.deps.client.SubjectsRereadEnded()
	if read.IsZero() {
		t.Fatal("the refused publish ended no read of the stream's subjects; the row needs the read whose end the gap runs from")
	}
	addStreamSubject(t, js, rollNewerSubject)
	if rig.deps.client.DeployedStreamCaptures(rollTopic) {
		t.Fatalf("the listener judged %s captured before reading the subjects again; the row needs the subjects it read before the newer build's start", rollTopic)
	}
	if since := time.Since(read); since >= rollRereadGap*4/5 {
		t.Fatalf("the newer build's subject came %s after the listener's last read; the row measures a publish inside the %s gap", since, rollRereadGap)
	}

	answer := publishOn(rig.mux, rollTopic)
	if answer.Code != http.StatusOK {
		t.Fatalf("a publish on %s, which the subject the newer build added inside %s of the listener's last read captures, was answered %d %s: refused on the subjects that read held",
			rollTopic, rollRereadGap, answer.Code, answer.Body.String())
	}
	requireStored(t, js, rollTopic)
}

// A publish through the older pod on a topic only the subject the newer build added captures is
// accepted when the pod's read of the stream's subjects goes unanswered: with no read that started
// after the publish asked, the route refuses nothing (bus.SubjectsAsk), and the publish goes out on
// the subjects the pod holds, over core NATS, where the stream stores it.
//
// Before that rule, a read that failed left the topic judged on the subjects the pod held, and the
// route refused the publish 400 for a topic the stream stores.
func TestAPublishWhoseReadOfTheSubjectsGoesUnansweredIsNotRefused(t *testing.T) {
	url := testnats.URL(t)
	relay := testnats.StartUnansweredRelayOn(t, url, "$JS.API.STREAM.INFO."+bus.Stream)
	rig, js := rollWindowRig(t, url, relay.URL())
	addStreamSubject(t, js, rollNewerSubject)
	if rig.deps.client.DeployedStreamCaptures(rollTopic) {
		t.Fatalf("the listener judged %s captured before reading the subjects again; the row needs the subjects it read before the newer build's start", rollTopic)
	}

	relay.Arm(1)
	answer := publishOn(rig.mux, rollTopic)
	if sent := relay.Sent(); sent != 1 {
		t.Fatalf("the publish sent %d reads of the stream's subjects, want the one the relay left unanswered", sent)
	}
	if answer.Code != http.StatusOK {
		t.Fatalf("a publish on %s, which the stream captures, was answered %d %s after its read of the stream's subjects went unanswered: refused on the subjects the pod held",
			rollTopic, answer.Code, answer.Body.String())
	}
	requireStored(t, js, rollTopic)
}

// A publish through a pod whose NATS user may not read the stream is judged on the subjects the pod
// holds, which are this build's: no read will ever come, so a topic only a newer build's subject
// captures is refused 400 though the stream captures it. It is the one exception to refusing only
// on a read that started after the publish asked, and it stands under either rule.
func TestAPublishByAUserThatMayNotReadTheStreamIsJudgedOnTheSubjectsItHolds(t *testing.T) {
	const machineID = "box-no-stream-info"
	seed, public := testnats.User(t)
	grant := testnats.ListenerGrant(t, machineID)
	streamInfo := "$JS.API.STREAM.INFO." + bus.Stream
	held := len(grant.Publish.Allow)
	grant.Publish.Allow = slices.DeleteFunc(grant.Publish.Allow, func(subject string) bool { return subject == streamInfo })
	if len(grant.Publish.Allow) != held-1 {
		t.Fatalf("the listener's grant does not hold %s exactly once: %q", streamInfo, grant.Publish.Allow)
	}
	granted := testnats.StartGranted(t, testnats.GrantedUser{Public: public, Grant: grant})
	deployNewerBuildsStream(t, granted.AdminURL, rollNewerSubject)
	admin, err := bus.ConnectOwningStream([]string{granted.AdminURL}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the admin: %v", err)
	}
	t.Cleanup(admin.Close)
	registry, err := store.Open(t.Context(), admin.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(t.Context(), admin.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}

	t.Setenv("NATS_NKEY_SEED", seed)
	refused, err := bus.Connect([]string{granted.URL()})
	if err != nil {
		t.Fatalf("connect as the user that may not read the stream: %v", err)
	}
	t.Cleanup(refused.Close)
	if !refused.ReachesNoSession(rollTopic) {
		t.Fatalf("the client judged %s as reaching a session; the row needs a client that holds only the subjects this build compiles", rollTopic)
	}
	handler := publishHandler(&listenerDeps{client: refused, registry: registry, sessions: sessions}, logging.New("test"))

	answer := publishOn(handler, rollTopic)
	if answer.Code != http.StatusBadRequest || !strings.Contains(answer.Body.String(), "no stream subject captures") {
		t.Fatalf("a publish on %s by a user that may not read the stream was answered %d %s, want 400 naming that no stream subject captures it",
			rollTopic, answer.Code, answer.Body.String())
	}
}

// pullOnce sends one pull request to sessionID's consumer, as a session's relay does, acks what it
// delivers, and returns the subjects of those messages.
func pullOnce(t *testing.T, js natsgo.JetStreamContext, sessionID string) []string {
	t.Helper()
	name, ok := bus.SessionConsumerName(sessionID)
	if !ok {
		t.Fatalf("%s names no session consumer", sessionID)
	}
	pull, err := js.PullSubscribe("", name, natsgo.Bind(bus.Stream, name))
	if err != nil {
		t.Fatalf("bind the consumer of %s: %v", sessionID, err)
	}
	defer func() { _ = pull.Unsubscribe() }()
	messages, err := pull.Fetch(10, natsgo.MaxWait(rollPull))
	if err != nil && !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("pull the consumer of %s: %v", sessionID, err)
	}
	subjects := make([]string, 0, len(messages))
	for _, message := range messages {
		subjects = append(subjects, message.Subject)
		if err := message.Ack(); err != nil {
			t.Fatalf("ack %s: %v", message.Subject, err)
		}
	}
	return subjects
}

// A session follows a topic no stream subject captures when it registers; the newer build's start
// then adds the subject, a message is stored on the topic, the session's relay sends one pull,
// and only after it does the session register again. The message is delivered: the registration
// kept the topic in the row and in the consumer's filter (bus.CanFilterOn), which
// nats-server takes for a subject no stream subject captures, so the pull hands it over.
//
// Before the filter kept such a topic, the first registration left it out of the row and the
// filter, the pull found nothing under a filter of the agent subject alone and moved the
// consumer past the message (nats-server v2.15.1 consumer.go:5066-5073), and the next
// registration's wider filter kept that position (consumer.go:2695-2754): the message was never
// delivered.
func TestATopicAddedAfterItsRegistrationIsDeliveredAcrossAnEmptyPull(t *testing.T) {
	const sessionID = "ses_roll_empty_pull"
	url := testnats.URL(t)
	rig, js := rollWindowRig(t, url, url)

	rig.subscribe(t, consumerRegistration(sessionID, "e", rollTopic))
	addStreamSubject(t, js, rollNewerSubject)
	sequence := rig.store(t, rollTopic)
	delivered := pullOnce(t, js, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "e", rollTopic))
	delivered = append(delivered, pullOnce(t, js, sessionID)...)

	if !slices.Contains(delivered, rollTopic) {
		t.Fatalf("the message stored on %s at stream sequence %d, after the newer build added %s, was never delivered to %s across one pull and its next registration: the consumer delivered %v, and filters on %v",
			rollTopic, sequence, rollNewerSubject, sessionID, delivered, rig.filters(t, sessionID))
	}
}

// As TestATopicAddedAfterItsRegistrationIsDeliveredAcrossAnEmptyPull, with a message on the
// session's own agent subject stored after the one on the topic and delivered by that pull before
// the next registration. The message on the topic is delivered too, by the same pull.
//
// Before the filter kept such a topic, the pull delivered the agent subject's message alone, which
// moved the consumer past the message on the topic, and the next registration's wider filter kept
// that position: the message on the topic was never delivered.
func TestATopicAddedAfterItsRegistrationIsDeliveredAcrossAMessageDeliveredBeforeTheNextRegistration(t *testing.T) {
	const sessionID = "ses_roll_delivered_between"
	url := testnats.URL(t)
	rig, js := rollWindowRig(t, url, url)
	own := contracts.LegacyAgentSubject(sessionID)

	rig.subscribe(t, consumerRegistration(sessionID, "e", rollTopic))
	addStreamSubject(t, js, rollNewerSubject)
	sequence := rig.store(t, rollTopic)
	rig.store(t, own)
	delivered := pullOnce(t, js, sessionID)
	if !slices.Contains(delivered, own) {
		t.Fatalf("the pull before the next registration delivered %v, want the message on %s among them; the row needs a message delivered in between", delivered, own)
	}
	rig.subscribe(t, consumerRegistration(sessionID, "e", rollTopic))
	delivered = append(delivered, pullOnce(t, js, sessionID)...)

	if !slices.Contains(delivered, rollTopic) {
		t.Fatalf("the message stored on %s at stream sequence %d, after the newer build added %s, was never delivered to %s across a delivery on %s and its next registration: the consumer delivered %v, and filters on %v",
			rollTopic, sequence, rollNewerSubject, sessionID, own, delivered, rig.filters(t, sessionID))
	}
}

// A topic no stream subject ever captures is kept in the session's interest row and in its
// consumer's filter, with the warning that nothing is delivered on it until a stream subject
// captures it. The consumer is created and updated on that filter like any other, and a core
// publish on the topic, which no stream stores, delivers nothing and fails nothing.
func TestATopicNoStreamCapturesIsKeptInTheRowAndTheFilterAndDeliversNothing(t *testing.T) {
	const (
		sessionID = "ses_never_captured"
		topic     = "notifications.lxspec.probe.never"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("this build compiles a subject capturing %s; the row needs a topic no stream captures", topic)
	}
	url := testnats.URL(t)
	rig, js := rollWindowRig(t, url, url)
	own := contracts.LegacyAgentSubject(sessionID)

	for registration := range 2 {
		answer := rig.subscribe(t, consumerRegistration(sessionID, "e", topic))
		stored, _, err := rig.harness.registry.RefreshAt(t.Context(), sessionID)
		if err != nil {
			t.Fatalf("read %s's interest row: %v", sessionID, err)
		}
		if want := []string{own, topic}; !slices.Equal(stored.Topics, want) {
			t.Fatalf("registration %d stored %v, want %v: the topic no stream subject captures is kept", registration+1, stored.Topics, want)
		}
		// The filter carries every form of each subject (bus.SessionConsumerFilters, 2200c28e #4134
		// added the agent subject's new form, notifications.agent.*.<session>), the kept topic among them.
		if want := bus.SessionConsumerFilters([]string{own, topic}); !slices.Equal(rig.filters(t, sessionID), want) {
			t.Fatalf("registration %d left the consumer of %s filtering on %v, want %v", registration+1, sessionID, rig.filters(t, sessionID), want)
		}
		if want := []string{listenerwire.UncapturedTopicWarning(topic, sessionID)}; !slices.Equal(answer.Warnings, want) {
			t.Fatalf("registration %d warnings = %q, want only %q", registration+1, answer.Warnings, want)
		}
	}

	if err := rig.harness.client.Conn.Publish(topic, []byte("stored by no stream")); err != nil {
		t.Fatalf("publish on %s: %v", topic, err)
	}
	if err := rig.harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if delivered := pullOnce(t, js, sessionID); len(delivered) != 0 {
		t.Fatalf("the consumer of %s delivered %v, want nothing: no stream stores %s", sessionID, delivered, topic)
	}
}

// A followed topic whose second form only a newer build's subject captures is in the session's
// consumer filter in both forms from its registration on, and a message stored on that second form
// once the newer build has added the subject is delivered. The filter is the join of two steps: the
// keep predicate (bus.CanFilterOn) is asked of the topic the session followed
// (sessionConsumerFilter), and the session consumers then filter on every form of each kept topic
// (bus.SessionConsumerFilters, contracts.TopicForms), judging no form. The deployed stream here is
// an older build's whose one issue subject captures the issue's old form,
// notifications.dispatch.issue.INFRA-12.>, and the newer build's start adds
// notifications.dispatch.issue.INFRA.>, which captures the form with the project first that a
// producer moving to the new names (dispatch://INFRA-3884) publishes on.
//
// A filter that judged each form on the deployed stream's subjects as this listener last read them
// would leave the new form out at the registration, and the consumer, which moves past a message
// its filter does not match, would skip what the stream stores on that form after the newer
// build's start until a later write of the filter took the form in.
func TestAFollowedTopicWhoseSecondFormOnlyANewerSubjectCapturesKeepsBothFormsInTheFilter(t *testing.T) {
	const (
		sessionID   = "ses_roll_second_form"
		olderIssues = "notifications.dispatch.issue.INFRA-12.>"
		newerIssues = "notifications.dispatch.issue.INFRA.>"
	)
	followed := contracts.LegacyDispatchIssueSubject("INFRA-12", ">")
	second := contracts.DispatchIssueSubject("INFRA", "INFRA-12", ">")
	if forms := contracts.TopicForms(followed); !slices.Contains(forms, second) {
		t.Fatalf("the forms of %s are %v, want %s among them", followed, forms, second)
	}
	url := testnats.URL(t)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open the row's JetStream: %v", err)
	}
	// The older build's stream: this build's subjects but its issue subject, and the older one
	// that captures the followed topic's first form alone, under a generation this build leaves as
	// it is (deployNewerBuildsStream).
	subjects := slices.DeleteFunc(bus.StreamCarriedSubjects(), func(subject string) bool {
		return strings.HasPrefix(subject, "notifications.dispatch.")
	})
	if _, err := js.AddStream(&natsgo.StreamConfig{
		Name:      bus.Stream,
		Subjects:  append(subjects, olderIssues),
		Retention: natsgo.LimitsPolicy,
		Discard:   natsgo.DiscardNew,
		Storage:   natsgo.FileStorage,
		Metadata:  map[string]string{bus.StreamIDMetadataKey: id.New(), "dispatch.stream-generation": "1000000"},
	}); err != nil {
		t.Fatalf("the older build creates %s: %v", bus.Stream, err)
	}
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	if !client.DeployedStreamCaptures(followed) || client.DeployedStreamCaptures(second) {
		t.Fatalf("the listener judges %s captured %t and %s captured %t; the row needs the first form captured and the second not",
			followed, client.DeployedStreamCaptures(followed), second, client.DeployedStreamCaptures(second))
	}
	rig := newSessionConsumerRigOn(t, client, url)

	rig.subscribe(t, consumerRegistration(sessionID, "e", followed))
	filters := rig.filters(t, sessionID)
	for _, form := range []string{followed, second} {
		if !slices.Contains(filters, form) {
			t.Fatalf("the consumer of %s filters on %v after its registration, want %s among them: each form of %s, the one no stream subject captures yet included",
				sessionID, filters, form, followed)
		}
	}
	addStreamSubject(t, js, newerIssues)
	stored := contracts.DispatchIssueSubject("INFRA", "INFRA-12", "comment.created")
	sequence := rig.store(t, stored)
	if delivered := pullOnce(t, js, sessionID); !slices.Contains(delivered, stored) {
		t.Fatalf("the message stored on %s at stream sequence %d, after the newer build added %s, was not delivered to %s: the consumer delivered %v, and filters on %v",
			stored, sequence, newerIssues, sessionID, delivered, rig.filters(t, sessionID))
	}
}
