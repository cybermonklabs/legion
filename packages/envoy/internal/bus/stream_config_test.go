package bus

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

// capsAnUnsetStream is what a test tells a start (ensureStreamWithConfig) or a keeper pass
// (keepStream) about the server it writes to, which a deployed program reads off its connection
// (serverDefaultsAnUnsetCap): that this server caps a stream carrying no consumer cap at a
// default of its own, rather than leaving such a stream unlimited. Every server a test of this
// package runs in process is the module's nats-server, v2.15.1 (go.mod), which caps such a
// stream at JSDefaultMaxConsumersPerStream, 1000, so a test of anything but the cap hands this.
// A test of the cap itself names a server version and reads the answer from the predicate
// (TestOwnedStreamRaisesTheConsumerCapWhereItBindsAndKeepsAHigherDeployedOne).
const capsAnUnsetStream = true

// streamInfoJetStream is a stream no server is behind, as a start's reads and writes find it. Its
// state is under mu, since a start calls it from more than one goroutine at once: each send runs on
// a goroutine of its own (Bounded), and the read of the stream's leader that a purge waits on
// (probeLeader, through StreamInfo) runs on beside the start's next update once the purge is
// answered first. A test reads what it holds through held.
type streamInfoJetStream struct {
	nats.JetStreamContext
	mu                        sync.Mutex
	config                    nats.StreamConfig
	purgedSubjects            []string
	roleMessages              int
	injectRoleMessageOnUpdate bool
	updateErr                 error
	// unansweredPurges is how many purges, the first ones, draw no answer: each returns nats.ErrTimeout,
	// as one whose JetStream wait ran out, and purges nothing.
	unansweredPurges int
	// lostPurgeAnswers is how many purges, the first ones after unansweredPurges, land and lose
	// their answer: each purges and returns nats.ErrTimeout.
	lostPurgeAnswers int
	// purgeSends counts every purge sent, answered or not.
	purgeSends int
}

// held is a copy of what js holds, read under its lock: a send the start abandoned, or a read of
// the stream's leader nobody waits for any more, can still run when the start has returned.
func (js *streamInfoJetStream) held() *streamInfoJetStream {
	js.mu.Lock()
	defer js.mu.Unlock()
	return &streamInfoJetStream{
		config:                    js.config,
		purgedSubjects:            slices.Clone(js.purgedSubjects),
		roleMessages:              js.roleMessages,
		injectRoleMessageOnUpdate: js.injectRoleMessageOnUpdate,
		updateErr:                 js.updateErr,
		unansweredPurges:          js.unansweredPurges,
		lostPurgeAnswers:          js.lostPurgeAnswers,
		purgeSends:                js.purgeSends,
	}
}

// StreamInfo answers the stream as it holds it, and a read filtered to a role lane
// (nats.StreamInfoRequest) with the role messages it holds on that lane.
func (js *streamInfoJetStream) StreamInfo(_ string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	info := &nats.StreamInfo{Config: js.config}
	for _, opt := range opts {
		if request, ok := opt.(*nats.StreamInfoRequest); ok && slices.Contains(roleLaneSubjects, request.SubjectsFilter) && js.roleMessages > 0 {
			info.State.Subjects = map[string]uint64{request.SubjectsFilter: uint64(js.roleMessages)}
		}
	}
	return info, nil
}

// AccountInfo answers at once, as the metadata leader of a cluster that has one does: the read a
// write that waits past one read period sends (metadataLeader).
func (js *streamInfoJetStream) AccountInfo(...nats.JSOpt) (*nats.AccountInfo, error) {
	return &nats.AccountInfo{}, nil
}

func (js *streamInfoJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.updateErr != nil {
		return nil, js.updateErr
	}
	if js.injectRoleMessageOnUpdate {
		js.roleMessages++
		js.injectRoleMessageOnUpdate = false
	}
	js.config = *cfg
	return &nats.StreamInfo{Config: js.config}, nil
}

func (js *streamInfoJetStream) PurgeStream(_ string, opts ...nats.JSOpt) error {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.purgeSends++
	if js.unansweredPurges > 0 {
		js.unansweredPurges--
		return nats.ErrTimeout
	}
	for _, opt := range opts {
		request, ok := opt.(*nats.StreamPurgeRequest)
		if !ok || request.Subject == "" {
			continue
		}
		js.purgedSubjects = append(js.purgedSubjects, request.Subject)
		if request.Subject == "notifications.role.>" || request.Subject == "notifications.envoy.exceptions.notifications.role.>" {
			js.roleMessages = 0
		}
	}
	if js.lostPurgeAnswers > 0 {
		js.lostPurgeAnswers--
		return nats.ErrTimeout
	}
	return nil
}

func (js *streamInfoJetStream) ConsumerNames(_ string, _ ...nats.JSOpt) <-chan string {
	names := make(chan string)
	close(names)
	return names
}

func TestConnectWithContextBoundsMultipleUnresponsiveServers(t *testing.T) {
	accepted := make(chan net.Conn, 32)
	urls := make([]string, 0, 2)
	for range 2 {
		listener, err := testport.Listen()
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				accepted <- conn
			}
		}()
		urls = append(urls, "nats://"+listener.Addr().String())
	}
	t.Cleanup(func() {
		for {
			select {
			case conn := <-accepted:
				_ = conn.Close()
			default:
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		conn, err := connectWithContext(ctx, "deadline-test", urls, nil, nil, nil, nil, nil, nil, nil)
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("connect unexpectedly succeeded against unresponsive servers")
		}
		if elapsed := time.Since(started); elapsed > 6*time.Second {
			t.Fatalf("connect returned after %s, want the outer five-second deadline", elapsed)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("connect exceeded the outer deadline while walking NATS URLs")
	}
}

func TestConnectWithContextDoesNotPoisonAutomaticReconnect(t *testing.T) {
	baseCtx := context.Background()
	uri := testnats.URL(t)

	// The property under test is that a connect context that is done by the time the server
	// drops the connection does not poison automatic reconnect; cancel it explicitly instead of
	// racing a short dial deadline against the freshly started server.
	connectCtx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
	defer cancel()
	reconnected := make(chan struct{}, 1)
	conn, err := connectWithContext(connectCtx, "reconnect-dialer-test", []string{uri}, nil, nil, func(*nats.Conn) {
		reconnected <- struct{}{}
	}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("connect with deadline: %v", err)
	}
	defer conn.Close()
	cancel()
	<-connectCtx.Done()

	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("force reconnect: %v", err)
	}
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("connection did not automatically reconnect after its initial context expired")
	}
}

func TestEnsureStreamWithConfig_updatesMaxAgeWhenExistingStreamDiffers(t *testing.T) {
	// Given
	oldConfig := *streamCfg
	oldConfig.MaxAge = time.Minute
	js := &streamInfoJetStream{config: oldConfig}
	desiredConfig := oldConfig
	desiredConfig.MaxAge = 72 * time.Hour

	// When
	if _, err := ensureStreamWithConfig(t.Context(), js, &desiredConfig, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	// Then
	if held := js.held(); held.config.MaxAge != desiredConfig.MaxAge {
		t.Fatalf("stream MaxAge = %s, want %s", held.config.MaxAge, desiredConfig.MaxAge)
	}
}

func TestConnectOwningStreamMigratesExistingStreamDuplicatesWindow(t *testing.T) {
	uri := testnats.URL(t)
	legacyConn := testnats.Connect(t, uri)
	t.Cleanup(legacyConn.Close)
	legacyJS, err := legacyConn.JetStream()
	if err != nil {
		t.Fatalf("open legacy JetStream: %v", err)
	}
	legacyConfig := *streamCfg
	legacyConfig.Duplicates = 0
	if _, err := legacyJS.AddStream(&legacyConfig); err != nil {
		t.Fatalf("create legacy stream: %v", err)
	}

	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect and migrate stream: %v", err)
	}
	t.Cleanup(client.Close)
	info, err := client.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read migrated stream: %v", err)
	}
	if info.Config.Duplicates != 72*time.Hour {
		t.Fatalf("stream duplicate window = %s, want 72h", info.Config.Duplicates)
	}
}

func TestEnsureStreamWithConfigPurgesLegacyRoleMessages(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{config: oldConfig}

	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	wantPurgedSubjects := []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	}
	if held := js.held(); !reflect.DeepEqual(held.purgedSubjects, wantPurgedSubjects) {
		t.Fatalf("purged subjects = %v, want %v", held.purgedSubjects, wantPurgedSubjects)
	}
}

func TestEnsureStreamWithConfigPurgesRoleMessagesPublishedDuringMigration(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{
		config:                    oldConfig,
		injectRoleMessageOnUpdate: true,
	}

	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	held := js.held()
	if held.roleMessages != 0 {
		t.Fatalf("legacy role messages published during migration remained: %d", held.roleMessages)
	}
	wantPurges := []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	}
	if !reflect.DeepEqual(held.purgedSubjects, wantPurges) {
		t.Fatalf("purged subjects = %v, want %v", held.purgedSubjects, wantPurges)
	}
}

// A start that moves the role lanes off a stream that still carries them, and whose first purge of
// those lanes draws no answer, reads the lane back, finds its messages still there, sends the purge
// again and goes on. It once sent each write of the migration once, so the start failed on the one
// lost answer, at the end of the JetStream context's own ten second wait.
func TestALegacyRoleLanePurgeTheServerLeavesUnansweredIsSentAgain(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{config: oldConfig, roleMessages: 1, unansweredPurges: 1}

	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose first purge of the role lanes drew no answer = %v, want it to go on", err)
	}
	held := js.held()
	if held.unansweredPurges != 0 {
		t.Fatalf("unanswered purges left = %d, want the lost one spent: the row held nothing", held.unansweredPurges)
	}
	wantPurges := []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	}
	if !reflect.DeepEqual(held.purgedSubjects, wantPurges) {
		t.Fatalf("purged subjects = %v, want %v: the lost purge sent again, then the purge after the update", held.purgedSubjects, wantPurges)
	}
	if held.purgeSends != len(wantPurges)+1 || held.roleMessages != 0 {
		t.Fatalf("purges sent = %d with %d role messages left, want %d and none: the lost one and the ones that landed", held.purgeSends, held.roleMessages, len(wantPurges)+1)
	}
	if streamCapturesRoleLanes(held.config.Subjects) {
		t.Fatalf("subjects %v still capture the role lanes after the start", held.config.Subjects)
	}
}

// A purge that lands and loses its answer is not sent again: the read of the lane after it finds
// no role message left. Only the read decides it, since a lost answer says nothing of whether the
// purge landed, and sending it again would cost the start one more send.
func TestALegacyRoleLanePurgeThatLandedWithItsAnswerLostIsNotSentAgain(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{config: oldConfig, roleMessages: 1, lostPurgeAnswers: 1}

	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose first purge landed with its answer lost = %v, want it to go on", err)
	}
	held := js.held()
	if held.lostPurgeAnswers != 0 {
		t.Fatalf("lost answers left = %d, want the one spent: the row held nothing", held.lostPurgeAnswers)
	}
	wantPurges := []string{
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
		"notifications.role.>",
		"notifications.envoy.exceptions.notifications.role.>",
	}
	if !reflect.DeepEqual(held.purgedSubjects, wantPurges) || held.purgeSends != len(wantPurges) {
		t.Fatalf("purged subjects = %v in %d sends, want %v in %d: the purge that landed is not sent again", held.purgedSubjects, held.purgeSends, wantPurges, len(wantPurges))
	}
}

func TestConnectOwningStreamPurgesLegacyRoleMessagesBeforeDurableConsumerRestart(t *testing.T) {
	uri := testnats.URL(t)
	legacyConn := testnats.Connect(t, uri)
	legacyJS, err := legacyConn.JetStream()
	if err != nil {
		t.Fatalf("open legacy JetStream: %v", err)
	}
	if _, err := legacyJS.AddStream(&nats.StreamConfig{
		Name:      Stream,
		Subjects:  []string{"notifications.>"},
		Retention: nats.LimitsPolicy,
		MaxAge:    72 * time.Hour,
		Storage:   nats.FileStorage,
		Replicas:  1,
	}); err != nil {
		t.Fatalf("create legacy stream: %v", err)
	}

	var legacyDelivery atomic.Int32
	legacySub, err := legacyJS.Subscribe("notifications.>", func(*nats.Msg) {
		legacyDelivery.Add(1)
	}, nats.Durable("legacy-role-replay"), nats.DeliverAll(), nats.AckExplicit(), nats.ManualAck())
	if err != nil {
		t.Fatalf("start legacy durable consumer: %v", err)
	}
	if err := legacyConn.Flush(); err != nil {
		t.Fatalf("flush legacy durable consumer: %v", err)
	}
	if _, err := legacyJS.Publish("notifications.role.legion-replay", []byte("legacy role message")); err != nil {
		t.Fatalf("publish legacy role message: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for legacyDelivery.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if legacyDelivery.Load() != 1 {
		t.Fatal("legacy durable consumer did not receive the role message")
	}
	_ = legacySub
	legacyConn.Close()

	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("migrate stream: %v", err)
	}
	t.Cleanup(client.Close)
	info, err := client.JS().StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read migrated stream: %v", err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("legacy role messages remained in stream after migration: %d", info.State.Msgs)
	}

	// nats-server releases a push binding on the consumer's own goroutine, after the client's
	// Close is answered, so only the consumer's own report tells this test that the durable may
	// be bound again; Client.Subscribe does not retry a "consumer is already bound" refusal.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		consumer, err := client.JS().ConsumerInfo(Stream, "legacy-role-replay")
		if err != nil {
			t.Fatalf("read the legacy durable consumer: %v", err)
		}
		if !consumer.PushBound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server still reports the legacy durable consumer bound 10s after the legacy connection closed")
		}
	}

	redelivered := make(chan struct{}, 1)
	restartedSub, err := client.Subscribe("notifications.>", func(*nats.Msg) {
		redelivered <- struct{}{}
	}, nats.Durable("legacy-role-replay"), nats.AckExplicit(), nats.ManualAck())
	if err != nil {
		t.Fatalf("restart durable consumer: %v", err)
	}
	t.Cleanup(func() { _ = restartedSub.Unsubscribe() })

	select {
	case <-redelivered:
		t.Fatal("legacy role message was redelivered after the durable consumer restart")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestStreamConfigExcludesRoleLanes(t *testing.T) {
	wantSubjects := []string{
		"notifications.agent.>",
		"notifications.dispatch.>",
		"notifications.github.>",
		"notifications.slack.>",
		"notifications.legion.>",
		"notifications.ghostwispr.>",
		"notifications.whatsapp.>",
		"notifications.team.>",
		"notifications.envoy.exceptions.notifications.agent.>",
		"notifications.envoy.exceptions.notifications.team.>",
		"notifications.envoy.kept.>",
		"notifications.envoy.resume.>",
		"notifications.envoy.takeover.>",
	}
	if !reflect.DeepEqual(streamCfg.Subjects, wantSubjects) {
		t.Fatalf("stream subjects = %v, want %v", streamCfg.Subjects, wantSubjects)
	}
	for _, streamSubject := range streamCfg.Subjects {
		if subjectCapturesRoleLanes(streamSubject) {
			t.Fatalf("stream subject %q captures a role lane", streamSubject)
		}
	}
}

// streamGenerations pins the settings of every stream generation a build has shipped. A row is
// never edited once a build carries it: a build that changes MaxAge, Duplicates or Subjects ships
// a new generation (streamConfigGeneration) and a new row, so a running older build, which leaves a
// stream of a higher generation alone, never reverts the change.
var streamGenerations = map[int]struct {
	maxAge, duplicates time.Duration
	subjects           []string
}{
	1: {
		maxAge:     72 * time.Hour,
		duplicates: 72 * time.Hour,
		subjects: []string{
			"notifications.agent.>",
			"notifications.dispatch.>",
			"notifications.github.>",
			"notifications.slack.>",
			"notifications.legion.>",
			"notifications.ghostwispr.>",
			"notifications.whatsapp.>",
			"notifications.envoy.exceptions.notifications.agent.>",
		},
	},
	// Generation 2 adds the listener's kept lane (streamCarriedSubjects).
	2: {
		maxAge:     72 * time.Hour,
		duplicates: 72 * time.Hour,
		subjects: []string{
			"notifications.agent.>",
			"notifications.dispatch.>",
			"notifications.github.>",
			"notifications.slack.>",
			"notifications.legion.>",
			"notifications.ghostwispr.>",
			"notifications.whatsapp.>",
			"notifications.envoy.exceptions.notifications.agent.>",
			"notifications.envoy.kept.>",
		},
	},
	// Generation 3 adds the listener's resume lane, the subject the stream carries the floor each
	// deleted session consumer stopped at (SessionResumeSubjectPrefix, listenerOwnStreamLanes),
	// which main's #3417 added to the stream's subjects.
	3: {
		maxAge:     72 * time.Hour,
		duplicates: 72 * time.Hour,
		subjects: []string{
			"notifications.agent.>",
			"notifications.dispatch.>",
			"notifications.github.>",
			"notifications.slack.>",
			"notifications.legion.>",
			"notifications.ghostwispr.>",
			"notifications.whatsapp.>",
			"notifications.envoy.exceptions.notifications.agent.>",
			"notifications.envoy.kept.>",
			"notifications.envoy.resume.>",
		},
	},
	// Generation 4 adds a team's own topic and its exception lane, the one namespace an agent
	// names itself (notifications.team.deploy-squad), which main's #2603 added to the stream's
	// subjects: that is a change of Subjects, so under the rule above it ships as a generation
	// of its own rather than as an edit of generation 3's row, and an older build that finds
	// generation 4 deployed leaves those subjects alone instead of reverting them.
	4: {
		maxAge:     72 * time.Hour,
		duplicates: 72 * time.Hour,
		subjects: []string{
			"notifications.agent.>",
			"notifications.dispatch.>",
			"notifications.github.>",
			"notifications.slack.>",
			"notifications.legion.>",
			"notifications.ghostwispr.>",
			"notifications.whatsapp.>",
			"notifications.team.>",
			"notifications.envoy.exceptions.notifications.agent.>",
			"notifications.envoy.exceptions.notifications.team.>",
			"notifications.envoy.kept.>",
			"notifications.envoy.resume.>",
		},
	},
	// Generation 5 adds the listener's takeover lane, the subject the stream carries each role a
	// session took from another on, with how far the copy of the other's role messages has read
	// (TakeoverSubjectPrefix, listenerOwnStreamLanes).
	5: {
		maxAge:     72 * time.Hour,
		duplicates: 72 * time.Hour,
		subjects: []string{
			"notifications.agent.>",
			"notifications.dispatch.>",
			"notifications.github.>",
			"notifications.slack.>",
			"notifications.legion.>",
			"notifications.ghostwispr.>",
			"notifications.whatsapp.>",
			"notifications.team.>",
			"notifications.envoy.exceptions.notifications.agent.>",
			"notifications.envoy.exceptions.notifications.team.>",
			"notifications.envoy.kept.>",
			"notifications.envoy.resume.>",
			"notifications.envoy.takeover.>",
		},
	},
}

// The stream this build ensures has exactly the settings pinned for its generation: a change to
// them without a new generation and a new row fails here.
func TestTheStreamConfigEqualsItsGenerationsPinnedRow(t *testing.T) {
	row, ok := streamGenerations[streamConfigGeneration]
	if !ok {
		t.Fatalf("generation %d has no pinned row; add one with the settings it ships", streamConfigGeneration)
	}
	if streamCfg.MaxAge != row.maxAge || streamCfg.Duplicates != row.duplicates || !slices.Equal(streamCfg.Subjects, row.subjects) {
		t.Fatalf("generation %d ships MaxAge=%s Duplicates=%s Subjects=%v, but its pinned row is MaxAge=%s Duplicates=%s Subjects=%v; a changed setting takes a new generation",
			streamConfigGeneration, streamCfg.MaxAge, streamCfg.Duplicates, streamCfg.Subjects, row.maxAge, row.duplicates, row.subjects)
	}
}

// Every subject of the stream's configuration is classified by whether a topic under it ever
// carries one agent session's private payload: a private one is in privateStreamSubjects, which
// POST /v1/interests/subscribe reads (ReachesPrivateStreamSubject) to refuse a session an
// interest over another session's messages, and every other is named here with the reason its
// publisher gives. Only privateStreamSubjects is production code, read by that one route, so the
// subjects deliberately left out of it live here, where they only have to force the decision. A
// subject added to the stream without one fails here, one in both lists fails, and a private
// subject the stream does not carry is a stale classification and fails too.
func TestEveryStreamSubjectIsClassifiedForInterestRegistration(t *testing.T) {
	notPrivate := []string{
		// Dispatch's outbox: an issue's, a document's or a project's events
		// (internal/dispatch/outbox envelope), each readable on its owner in Dispatch, and
		// Dispatch's start (startup.Subject); a copy for one session goes to that session's agent
		// subject, and an issue-less message has no owner and is not published (outbox publish).
		"notifications.dispatch.>",
		// A repository's GitHub webhook events (internal/webhook github, contracts.GithubEnvelopes)
		// and the CI settlements derived from them (internal/cistore).
		"notifications.github.>",
		// A Slack channel's or thread's messages and app mentions (internal/webhook slack,
		// contracts.SlackEnvelopes).
		"notifications.slack.>",
		// A Ghost Wispr recording's events, under its recording id, not an agent session's
		// (internal/webhook ghostwispr, contracts.GhostWisprEnvelope).
		"notifications.ghostwispr.>",
		// A WhatsApp chat's messages the MCP bridge relays (internal/mcpbridge BuildEnvelope).
		"notifications.whatsapp.>",
		// A team's own topic, published by the listener's route for whoever registered it
		// (cmd/listener publishHandler): a group's lane carries no one session's private payload,
		// as its sender names the team and not a session.
		"notifications.team.>",
	}
	for _, subject := range streamCfg.Subjects {
		private, shared := slices.Contains(privateStreamSubjects, subject), slices.Contains(notPrivate, subject)
		switch {
		case !private && !shared:
			t.Errorf("stream subject %q is classified neither way: add it to privateStreamSubjects when a topic under it carries one session's payload, and to notPrivate here with the reason its publisher gives otherwise", subject)
		case private && shared:
			t.Errorf("stream subject %q is both private and not private", subject)
		}
	}
	for _, subject := range slices.Concat(privateStreamSubjects, notPrivate) {
		if !slices.Contains(streamCfg.Subjects, subject) {
			t.Errorf("the classification names %q, which is no subject of the stream", subject)
		}
	}
}

// The legacy notifications.> catch-all captured the role lanes, so it is the one deployed subject
// start-up replaces outright with the binary's own list.
func TestEnsureStreamWithConfigReplacesTheLegacyRoleLaneCatchAll(t *testing.T) {
	oldConfig := *streamCfg
	oldConfig.Subjects = []string{"notifications.>"}
	js := &streamInfoJetStream{config: oldConfig}
	desiredConfig := oldConfig
	desiredConfig.Subjects = []string{"notifications.github.>"}

	if _, err := ensureStreamWithConfig(t.Context(), js, &desiredConfig, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if held := js.held(); !reflect.DeepEqual(held.config.Subjects, desiredConfig.Subjects) {
		t.Fatalf("stream subjects = %v, want %v", held.config.Subjects, desiredConfig.Subjects)
	}
}

// Several deployments ensure one stream: the listener and Dispatch run from one image but deploy
// separately, and a rollback or a restart during a rollout starts a binary compiled with a
// different subject list. Whichever starts must leave every subject the other still needs.
func TestConnectOwningStreamKeepsTheSubjectsAnotherDeploymentOfTheStreamNeeds(t *testing.T) {
	uri := testnats.URL(t)
	// The other deployment was compiled before notifications.legion.> existed and carries a
	// subject this binary does not know.
	other := *streamCfg
	other.Subjects = []string{"notifications.retired.>"}
	for _, subject := range streamCfg.Subjects {
		if subject != "notifications.legion.>" {
			other.Subjects = append(other.Subjects, subject)
		}
	}
	otherConn := testnats.Connect(t, uri)
	t.Cleanup(otherConn.Close)
	otherJS, err := otherConn.JetStream()
	if err != nil {
		t.Fatalf("open the other deployment's JetStream: %v", err)
	}
	if _, err := ensureStreamWithConfig(t.Context(), otherJS, &other, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("the other deployment creates the stream: %v", err)
	}

	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("this binary connects: %v", err)
	}
	t.Cleanup(client.Close)
	assertStreamSubjectsInclude(t, client.JS(), append([]string{"notifications.retired.>"}, streamCfg.Subjects...))

	// The other deployment restarts while this binary is still live.
	if _, err := ensureStreamWithConfig(t.Context(), otherJS, &other, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("the other deployment restarts: %v", err)
	}
	assertStreamSubjectsInclude(t, client.JS(), append([]string{"notifications.retired.>"}, streamCfg.Subjects...))
	notice := contracts.Envelope{
		EventID:        "notice-skew",
		Source:         "agent",
		SourceEventID:  "notice-skew",
		Topic:          "notifications.legion.omp.LEGION-208",
		DedupeKey:      "legion-outbox:skew",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "notice published after the other deployment restarted",
		TraceID:        "notice-skew",
	}
	if err := client.Publish(notice); err != nil {
		t.Fatalf("publish a Legion notice after the other deployment restarted: %v", err)
	}
}

func assertStreamSubjectsInclude(t *testing.T, js nats.JetStreamContext, want []string) {
	t.Helper()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, subject := range want {
		if !slices.Contains(info.Config.Subjects, subject) {
			t.Fatalf("stream subjects = %v, missing %q", info.Config.Subjects, subject)
		}
	}
}

// JetStream refuses two overlapping subjects in one stream. When one deployment widens, narrows or
// splits a subject another deployment still holds, each start (and a rollback's) must still
// succeed, with the starting binary's shape of that subject in the stream.
//
// Each subtest takes its own testnats.URL, inside the subtest, so each runs in an account of its
// own. Its reset deletes Stream: in a fresh account there is nothing to delete, but in one account
// shared by both, the second subtest's reset would delete the stream the first left, emptying the
// account, and its recreate of that name would race the background removal of the account's
// streams directory that nats-server schedules on every stream delete and that succeeds once that
// directory is empty. The deleted stream's own directory is not what a recreate races:
// nats-server renames it aside and removes the renamed copy, which a recreate never meets.
func TestEnsureStreamWithConfigStartsWhenADeployedSubjectOverlapsItsOwn(t *testing.T) {
	shaped := func(legion string) nats.StreamConfig {
		cfg := *streamCfg
		cfg.Subjects = nil
		for _, subject := range streamCfg.Subjects {
			if subject == "notifications.legion.>" {
				subject = legion
			}
			cfg.Subjects = append(cfg.Subjects, subject)
		}
		return cfg
	}
	wide, narrow := shaped("notifications.legion.>"), shaped("notifications.legion.*.*")

	for _, tc := range []struct {
		name            string
		deployed, start nats.StreamConfig
	}{
		{name: "widen", deployed: narrow, start: wide},
		{name: "narrow", deployed: wide, start: narrow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn := testnats.Connect(t, uri)
			t.Cleanup(conn.Close)
			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("open JetStream: %v", err)
			}
			if err := js.DeleteStream(Stream); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
				t.Fatalf("reset stream: %v", err)
			}
			if _, err := ensureStreamWithConfig(t.Context(), js, &tc.deployed, capsAnUnsetStream, atStart); err != nil {
				t.Fatalf("the deployed binary creates the stream: %v", err)
			}
			for _, step := range []struct {
				name string
				cfg  nats.StreamConfig
			}{
				{name: "the new binary starts", cfg: tc.start},
				{name: "the rollback starts", cfg: tc.deployed},
				{name: "the new binary starts again", cfg: tc.start},
			} {
				if _, err := ensureStreamWithConfig(t.Context(), js, &step.cfg, capsAnUnsetStream, atStart); err != nil {
					t.Fatalf("%s: %v", step.name, err)
				}
				info, err := js.StreamInfo(Stream)
				if err != nil {
					t.Fatalf("%s: read stream: %v", step.name, err)
				}
				got, want := slices.Clone(info.Config.Subjects), slices.Clone(step.cfg.Subjects)
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("%s: stream subjects = %v, want the starting binary's %v", step.name, info.Config.Subjects, step.cfg.Subjects)
				}
			}
		})
	}
}

// capturedLogs collects the default slog logger's JSON records for one test. Its writes and reads
// share a lock because a bus client an earlier test closed can still be logging: nats.go v1.55.0
// queues the disconnected and closed callbacks on its async dispatcher and returns without waiting
// for them. The bus's closed callback logs through the default logger, and every test of the
// package shares one binary.
type capturedLogs struct {
	logtest.Buffer
}

// records decodes every JSON record logged so far.
func (l *capturedLogs) records(t *testing.T) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// recordsWithMsg decodes every JSON record logged so far whose msg is message.
func (l *capturedLogs) recordsWithMsg(t *testing.T, message string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, record := range l.records(t) {
		if record["msg"] == message {
			records = append(records, record)
		}
	}
	return records
}

// captureLogs points the default slog logger at a JSON handler writing into the returned logs
// until the test ends. A test that calls it must not run in parallel: the default is the process's.
func captureLogs(t *testing.T) *capturedLogs {
	t.Helper()
	logs := &capturedLogs{}
	logtest.SetDefault(t, slog.NewJSONHandler(logs, nil))
	return logs
}

// Every subject that can capture a role lane leaves the stream, whichever role it names: the role
// lanes travel over core NATS and a retained copy would be delivered twice.
func TestEnsureStreamWithConfigDropsEveryRoleLaneSubject(t *testing.T) {
	for _, roleSubject := range []string{
		"notifications.role.sre",
		"notifications.role.*",
		"notifications.envoy.exceptions.notifications.role.sre",
	} {
		t.Run(roleSubject, func(t *testing.T) {
			deployed := *streamCfg
			deployed.Subjects = append(slices.Clone(streamCfg.Subjects), roleSubject)
			js := &streamInfoJetStream{config: deployed}
			if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
				t.Fatalf("ensure stream: %v", err)
			}
			held := js.held()
			if slices.Contains(held.config.Subjects, roleSubject) {
				t.Fatalf("stream subjects = %v, still capturing the role lane %q", held.config.Subjects, roleSubject)
			}
			if !slices.Contains(held.purgedSubjects, "notifications.role.>") {
				t.Fatalf("purged subjects = %v, want the role lanes purged", held.purgedSubjects)
			}
		})
	}
}

// A deployed subject split into several of the binary's own names every one it was replaced by.
func TestEnsureStreamWithConfigNamesEverySubjectThatReplacedAnOverlappingOne(t *testing.T) {
	logs := captureLogs(t)
	deployed := *streamCfg
	deployed.Subjects = []string{"notifications.agent.>", "notifications.legion.>"}
	own := *streamCfg
	own.Subjects = []string{"notifications.agent.>", "notifications.legion.*", "notifications.legion.*.*"}
	js := &streamInfoJetStream{config: deployed}
	if _, err := ensureStreamWithConfig(t.Context(), js, &own, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	records := logs.recordsWithMsg(t, "envoy nats stream subject replaced by an overlapping one")
	if len(records) != 1 {
		t.Fatalf("replacement records = %v, want one for notifications.legion.>", records)
	}
	if records[0]["dropped"] != "notifications.legion.>" {
		t.Fatalf("dropped = %v, want notifications.legion.>", records[0]["dropped"])
	}
	kept, _ := records[0]["kept"].([]any)
	if !reflect.DeepEqual(kept, []any{"notifications.legion.*", "notifications.legion.*.*"}) {
		t.Fatalf("kept = %v, want both subjects that replaced it", records[0]["kept"])
	}
}

// A start that keeps deployed subjects it does not compile names them, so the retire step can find
// them; it names them on every start that keeps them, whether or not the stream changed.
func TestEnsureStreamWithConfigNamesTheDeployedSubjectsItKeepsWithoutCompilingThem(t *testing.T) {
	logs := captureLogs(t)
	deployed := *streamCfg
	deployed.Subjects = append(slices.Clone(streamCfg.Subjects), "notifications.retired.>")
	js := &streamInfoJetStream{config: deployed}
	for _, start := range []string{"the stream is already reconciled", "a second start"} {
		if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
			t.Fatalf("%s: ensure stream: %v", start, err)
		}
	}
	records := logs.recordsWithMsg(t, "envoy nats stream keeps subjects this binary does not compile")
	if len(records) != 2 {
		t.Fatalf("records = %v, want one per start", records)
	}
	for _, record := range records {
		if subjects, _ := record["subjects"].([]any); !reflect.DeepEqual(subjects, []any{"notifications.retired.>"}) {
			t.Fatalf("subjects = %v, want [notifications.retired.>]", record["subjects"])
		}
	}
}

// Nothing is reported as replaced or kept when the update that would install it fails.
func TestEnsureStreamWithConfigLogsNoReconciliationWhenTheUpdateFails(t *testing.T) {
	logs := captureLogs(t)
	deployed := *streamCfg
	deployed.Subjects = []string{"notifications.retired.>", "notifications.legion.>"}
	own := *streamCfg
	own.Subjects = []string{"notifications.legion.*"}
	js := &streamInfoJetStream{config: deployed, updateErr: errors.New("update refused")}
	if _, err := ensureStreamWithConfig(t.Context(), js, &own, capsAnUnsetStream, atStart); err == nil {
		t.Fatal("ensure stream succeeded, want the update's error")
	}
	for _, message := range []string{
		"envoy nats stream subject replaced by an overlapping one",
		"envoy nats stream keeps subjects this binary does not compile",
	} {
		if records := logs.recordsWithMsg(t, message); len(records) != 0 {
			t.Fatalf("%q logged %v although the update failed", message, records)
		}
	}
}

// streamTestJetStream is a JetStream context in an account of the test's own, with no stream yet.
func streamTestJetStream(t *testing.T) nats.JetStreamContext {
	t.Helper()
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

func deployedStream(t *testing.T, js nats.JetStreamContext) *nats.StreamInfo {
	t.Helper()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return info
}

// A stream a start creates carries an id of its own: two streams, each in an account of its own,
// get different ones.
func TestEnsureStreamWithConfigCreatesTheStreamWithAFreshID(t *testing.T) {
	var ids []string
	for _, name := range []string{"first stream", "second stream"} {
		t.Run(name, func(t *testing.T) {
			js := streamTestJetStream(t)
			if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
				t.Fatalf("create stream: %v", err)
			}
			id := deployedStream(t, js).Config.Metadata[StreamIDMetadataKey]
			if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
				t.Fatalf("stream id = %q, want 32 hex digits", id)
			}
			ids = append(ids, id)
		})
	}
	if len(ids) == 2 && ids[0] == ids[1] {
		t.Fatalf("both streams got id %q", ids[0])
	}
}

// A deployed stream without an id gets one on the next start, even when nothing else about it
// differs from the binary's configuration, and keeps the rest of its configuration and metadata:
// settings the binary's configuration does not choose (here the message limits and a description;
// in a cluster, the stream's placement) stay as deployed, and so do the ones it would only keep (a
// byte limit, with no limit of its own set) or already matches (DiscardNew, the replicas). The
// start stamps its generation beside the id.
func TestEnsureStreamWithConfigGivesADeployedStreamWithoutAnIDOne(t *testing.T) {
	js := streamTestJetStream(t)
	deployed := *streamCfg
	deployed.Description = "deployed by operations"
	deployed.MaxMsgs = 100_000
	deployed.MaxBytes = 64 << 20
	deployed.MaxMsgsPerSubject = 1_000
	deployed.Metadata = map[string]string{"owner": "operations"}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("create the stream without an id: %v", err)
	}
	before := deployedStream(t, js).Config

	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	info := deployedStream(t, js)
	id := info.Config.Metadata[StreamIDMetadataKey]
	if id == "" {
		t.Fatalf("stream metadata = %v, want an id", info.Config.Metadata)
	}
	want := before
	want.Metadata = maps.Clone(before.Metadata)
	want.Metadata[StreamIDMetadataKey] = id
	want.Metadata[streamGenerationKey] = strconv.Itoa(streamConfigGeneration)
	if !reflect.DeepEqual(info.Config, want) {
		t.Fatalf("stream config = %+v, want the deployed one with only the id and the generation added: %+v", info.Config, want)
	}
	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if again := deployedStream(t, js).Config.Metadata[StreamIDMetadataKey]; again != id {
		t.Fatalf("stream id = %q after a second start, want %q", again, id)
	}
}

// A start that changes the deployed stream's subjects and retention writes the stream's whole
// configuration, and keeps its id and the rest of its metadata in it.
func TestEnsureStreamWithConfigKeepsTheIDAndMetadataWhenItReconcilesTheStream(t *testing.T) {
	js := streamTestJetStream(t)
	deployed := *streamCfg
	deployed.MaxAge = time.Hour
	deployed.Duplicates = time.Hour
	deployed.Subjects = slices.DeleteFunc(slices.Clone(streamCfg.Subjects), func(subject string) bool {
		return subject == "notifications.legion.>"
	})
	deployed.Metadata = map[string]string{StreamIDMetadataKey: "deployed-id", "owner": "operations"}
	if _, err := js.AddStream(&deployed); err != nil {
		t.Fatalf("create the deployed stream: %v", err)
	}

	if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	info := deployedStream(t, js)
	if info.Config.MaxAge != streamCfg.MaxAge || !slices.Contains(info.Config.Subjects, "notifications.legion.>") {
		t.Fatalf("stream config = %+v, want the binary's retention and subjects", info.Config)
	}
	if info.Config.Metadata[StreamIDMetadataKey] != "deployed-id" || info.Config.Metadata["owner"] != "operations" {
		t.Fatalf("stream metadata = %v, want the id deployed-id and owner kept", info.Config.Metadata)
	}
}

// The id a start gives the stream is never replaced: not by the same binary's next start, not by
// another deployment's start that changes the stream's subjects, and not by a start that found no
// stream and created it after another start already had.
func TestEnsureStreamWithConfigNeverReplacesTheStreamsID(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("this binary connects: %v", err)
	}
	t.Cleanup(client.Close)
	id := deployedStream(t, client.JS()).Config.Metadata[StreamIDMetadataKey]
	if id == "" {
		t.Fatal("the created stream has no id")
	}

	otherConn := testnats.Connect(t, uri)
	t.Cleanup(otherConn.Close)
	otherJS, err := otherConn.JetStream()
	if err != nil {
		t.Fatalf("open the other deployment's JetStream: %v", err)
	}
	other := *streamCfg
	other.Subjects = append(slices.Clone(streamCfg.Subjects), "notifications.retired.>")
	if _, err := ensureStreamWithConfig(t.Context(), otherJS, &other, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("the other deployment starts: %v", err)
	}
	if got := deployedStream(t, otherJS).Config.Metadata[StreamIDMetadataKey]; got != id {
		t.Fatalf("stream id = %q after the other deployment's start, want %q", got, id)
	}

	restarted, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("this binary restarts: %v", err)
	}
	t.Cleanup(restarted.Close)
	if got := deployedStream(t, restarted.JS()).Config.Metadata[StreamIDMetadataKey]; got != id {
		t.Fatalf("stream id = %q after a restart, want %q", got, id)
	}

	if _, err := ensureStreamWithConfig(t.Context(), &createdMeanwhileJetStream{JetStreamContext: otherJS}, streamCfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start that found no stream before another created it: %v", err)
	}
	if got := deployedStream(t, otherJS).Config.Metadata[StreamIDMetadataKey]; got != id {
		t.Fatalf("stream id = %q after a start that found no stream, want %q", got, id)
	}
}

// createdMeanwhileJetStream answers its first StreamInfo with no stream, as a start sees it when
// another start creates the stream between this one's read and its create. answered is atomic: a
// start sends each read on a goroutine of its own (Bounded), and a read it abandoned at its limit
// runs on beside the next.
type createdMeanwhileJetStream struct {
	nats.JetStreamContext
	answered atomic.Bool
}

func (js *createdMeanwhileJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.answered.CompareAndSwap(false, true) {
		return nil, nats.ErrStreamNotFound
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

// staleStreamInfoJetStream answers every StreamInfo with the stream as it stood before any update,
// as two starts see it when both read the stream before either writes. No server is behind it.
// Its updates are under mu, since a start sends each update on a goroutine of its own (Bounded),
// and one it abandoned at its limit runs on beside the start's next send and the test; the test
// reads them through sent.
type staleStreamInfoJetStream struct {
	nats.JetStreamContext
	read    nats.StreamInfo
	mu      sync.Mutex
	updates []nats.StreamConfig
}

func (js *staleStreamInfoJetStream) StreamInfo(_ string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	info := js.read
	return &info, nil
}

// AccountInfo answers at once, as the metadata leader of a cluster that has one does: the read an
// update that waits past one read period sends (metadataLeader), which would otherwise reach the
// JetStream context no server is behind.
func (js *staleStreamInfoJetStream) AccountInfo(...nats.JSOpt) (*nats.AccountInfo, error) {
	return &nats.AccountInfo{}, nil
}

func (js *staleStreamInfoJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.updates = append(js.updates, *cfg)
	return &nats.StreamInfo{Config: *cfg}, nil
}

// sent is every update sent through js so far.
func (js *staleStreamInfoJetStream) sent() []nats.StreamConfig {
	js.mu.Lock()
	defer js.mu.Unlock()
	return slices.Clone(js.updates)
}

// Two starts that both find the deployed stream without an id, before either writes one, write the
// same id, so the later write does not replace the earlier one.
func TestEnsureStreamWithConfigRacingStartsWriteOneID(t *testing.T) {
	js := &staleStreamInfoJetStream{read: nats.StreamInfo{
		Config:  *streamCfg,
		Created: time.Date(2026, 10, 27, 9, 30, 0, 123456789, time.UTC),
	}}
	for _, start := range []string{"the listener", "Dispatch's server"} {
		if _, err := ensureStreamWithConfig(t.Context(), js, streamCfg, capsAnUnsetStream, atStart); err != nil {
			t.Fatalf("%s starts: %v", start, err)
		}
	}
	updates := js.sent()
	if len(updates) != 2 {
		t.Fatalf("updates = %d, want one per start", len(updates))
	}
	first, second := updates[0].Metadata[StreamIDMetadataKey], updates[1].Metadata[StreamIDMetadataKey]
	if first == "" || first != second {
		t.Fatalf("racing starts wrote ids %q and %q, want one id", first, second)
	}
}

// The stream this binary compiles carries the consumer cap the issue sizes it at, so a stream it
// creates holds 24000 consumers rather than the 1000 nats-server 2.15 and later apply to a stream
// that sets none, the default the hub's stream filled, after which every new session was refused
// its own consumer and fell back to core delivery.
func TestTheCompiledStreamCarriesTheConsumerCap(t *testing.T) {
	if streamCfg.MaxConsumers != 24000 {
		t.Fatalf("streamCfg.MaxConsumers = %d, want 24000", streamCfg.MaxConsumers)
	}
}

// ListenerOwnLaneOf answers for every lane the stream carries for the listener itself, over the
// exported list of them rather than over the lanes a caller happens to remember: a lane added to
// listenerOwnStreamLanes is on ListenerOwnLanes, and so covered here and wherever a caller asks
// (the publish route's refusal, cmd/listener/api.go). The lane a topic lies under is named as
// every topic of that lane begins, which is what a refusal puts in front of its reader, and a
// topic under no lane is named "". A notification topic, and a wildcard wider than any lane, are
// no lane of the listener's.
func TestListenerOwnLaneCoversEveryLaneTheListenerOwns(t *testing.T) {
	lanes := ListenerOwnLanes()
	if len(lanes) != len(listenerOwnStreamLanes) {
		t.Fatalf("ListenerOwnLanes() = %q, want one entry for each of %q", lanes, listenerOwnStreamLanes)
	}
	for _, lane := range lanes {
		if strings.HasSuffix(lane, ">") || !strings.HasSuffix(lane, ".") {
			t.Fatalf("the listener's lane %q is not the prefix every topic of it begins with", lane)
		}
		for _, topic := range []string{lane + ">", lane + "a-topic", lane + "a.deeper.topic", lane + "*"} {
			if got := ListenerOwnLaneOf(topic); got != lane {
				t.Errorf("ListenerOwnLaneOf(%q) = %q, want the listener's own lane %q it lies under", topic, got, lane)
			}
		}
	}
	for _, topic := range []string{
		"notifications.agent.ses_a",
		"notifications.dispatch.issue.KEY-1.ask.answered",
		ExceptionTopicPrefix + contracts.LegacyAgentSubject("ses_a"),
		"notifications.envoy.deliver.ses_a",
		"notifications.envoy.>",
		"notifications.>",
	} {
		if lane := ListenerOwnLaneOf(topic); lane != "" {
			t.Errorf("ListenerOwnLaneOf(%q) = %q, want \"\": no lane of the listener's holds every subject it matches", topic, lane)
		}
	}
}
