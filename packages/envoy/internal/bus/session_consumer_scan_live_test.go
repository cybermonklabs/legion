//go:build natslive

// A session consumer's own messages, in both forms of its agent subject, behind other sessions'
// traffic, against a real nats-server image; and the listener's refusal of a server that can skip
// them. The module links nats-server v2.15.1 in process, whose scan of a consumer's stored
// messages sees every filter, so only an image can run a server whose scan does not: nats-server
// v2.10.29, the copy of nats:2.10 in envoy's natslive matrix (natsversion says why, and which
// servers).
//
// The file sits behind the natslive tag for the reason stream_max_consumers_live_test.go gives, and
// the envoy-go CI job runs it in the same step.

package bus

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsversion"
)

// otherSessionsTraffic is how many messages for other sessions each case stores between the
// consumer's position and its own messages: more than the distinct subjects a message block holds
// once they are stored, which is where nats-server v2.10.29 stops reading a multi-filter
// consumer's stored messages one by one and reads them by subject instead
// (server/filestore.go:2267, firstMatchingMulti, `mb.fss.Size() < lseq-start`). Measured on
// v2.10.29: with none of this traffic the old-form message was handed over, and with 20 messages
// over two other sessions' subjects it was skipped.
const otherSessionsTraffic = 24

// The consumer a session gets from the listener is handed every message stored on its own agent
// subject, in the old form and under a project, however much other sessions' traffic stands
// between its position and that message, on the server the hub runs (nats-server v2.15.1). Each
// case drives the calls cmd/listener makes, in its order: a registration's Ensure, the relay's
// RestartAtFloor and Pull; a release's Delete; and a registration again.
//
//   - "stored while the session was released" is the Legion daemon's idle release: the
//     registration's create starts the consumer at the session's resume record, below the
//     traffic and the messages stored while it had none.
//   - "sent while the pull waits" is a live session: the pull has already found the stream's end,
//     and the traffic is stored without waking the consumer, since none of it is on its filters.
//
// Its control is TestASessionConsumerOnAServerThatSkipsAFilterLosesItsOldFormMessage, the same
// first case on nats-server v2.10.29 with the refusal stepped past.
func TestASessionConsumerHandsOverItsOwnMessagesInBothFormsBehindOtherSessionsTraffic(t *testing.T) {
	t.Run("stored while the session was released", func(t *testing.T) {
		owner, consumers := liveSessionConsumers(t, testnats.HubImage)
		pull, own := releasedAndRegisteredAgain(t, owner, consumers)
		requireHandedOver(t, consumers, pull, scanSession, own)
	})
	t.Run("sent while the pull waits", func(t *testing.T) {
		owner, consumers := liveSessionConsumers(t, testnats.HubImage)
		ctx := context.Background()
		publishOthers(t, owner, 2)
		if _, err := consumers.Ensure(ctx, scanSession, scanSubjects, 0); err != nil {
			t.Fatalf("ensure the consumer of %s: %v", scanSession, err)
		}
		if _, err := consumers.RestartAtFloor(ctx, scanSession, scanSubjects); err != nil {
			t.Fatalf("restart the consumer of %s at its floor: %v", scanSession, err)
		}
		pull := pullSession(t, consumers, scanSession)
		// The pull request reaches the server, which then counts it waiting, and the consumer
		// reads to the stream's end before the traffic is stored: nothing is handed over meanwhile.
		testwait.Eventually(t, "the pull of "+scanSession+" to wait at the server", func() bool {
			info, err := consumers.Info(ctx, scanSession)
			return err == nil && info.NumWaiting > 0
		})
		requireNoMessage(t, consumers, pull, scanSession)
		publishOthers(t, owner, otherSessionsTraffic)
		requireHandedOver(t, consumers, pull, scanSession, publishOwn(t, owner, scanSession))
	})
}

// The listener refuses to start against a nats-server that can skip a message on a consumer
// filtering on a wildcard and a literal at the same token: its start connects its session
// consumers (cmd/listener connectSessionDelivery, ConnectSessionConsumers) and ends the process on
// the error, which names the server's version and the fixed releases (natsversion.Refusal). The
// control is the hub's v2.15.1, which it connects to.
func TestTheListenerRefusesAServerThatSkipsAFilter(t *testing.T) {
	_, refused := testnats.StartImageConfig(t, testnats.Image, "jetstream {}\n", liveServerArgs, true)
	consumers, err := ConnectSessionConsumers([]string{refused}, 0, nil, WithEnvironment(liveServerEnvironment))
	if err == nil {
		consumers.Close()
		t.Fatal("ConnectSessionConsumers to nats:2.10 connected, want a refusal naming its version")
	}
	if !strings.Contains(err.Error(), "2.10.29") || !strings.Contains(err.Error(), natsversion.FixedReleases) {
		t.Fatalf("the refusal = %q, want it to name 2.10.29 and %q", err, natsversion.FixedReleases)
	}
	_, taken := testnats.StartImageConfig(t, testnats.HubImage, "jetstream {}\n", liveServerArgs, true)
	control, err := ConnectSessionConsumers([]string{taken}, 0, nil, WithEnvironment(liveServerEnvironment))
	if err != nil {
		t.Fatalf("ConnectSessionConsumers to the hub's image: %v, want a connection", err)
	}
	control.Close()
}

// The control the refusal leans on: the session consumers stepped past it (sessionConsumersOn) on
// nats-server v2.10.29 lose the session's old-form message stored while it was released, and hand
// over the new-form one alone. The server reads the block by subject (server/sublist.go:1750-1784,
// intersectStree), follows the wildcard notifications.agent.*.<session> and never the literal
// notifications.agent.<session> beside it, so it finds nothing on the old form and moves the
// consumer past it (server/consumer.go:3700-3708, getNextMsg). If this stops failing that way on
// the pinned image, the refusal's reason has to be measured again.
func TestASessionConsumerOnAServerThatSkipsAFilterLosesItsOldFormMessage(t *testing.T) {
	_, uri := testnats.StartImageConfig(t, testnats.Image, "jetstream {}\n", liveServerArgs, true)
	owner, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	client, err := Connect([]string{uri}, WithName("envoy-session-consumers"), WithEnvironment(liveServerEnvironment))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	consumers := sessionConsumersOn(client, 0, nil)
	t.Cleanup(consumers.Close)
	version := client.Connection().ConnectedServerVersion()
	if version == "" {
		t.Fatal("the connection reports no server version: this control needs the pinned nats:2.10 image's own, which natsversion refuses")
	}
	if err := natsversion.Refusal(version); err == nil {
		t.Fatalf("the pinned nats:2.10 image reports %s, which natsversion takes; this control needs a server it refuses", version)
	}
	pull, _ := releasedAndRegisteredAgain(t, owner, consumers)
	got := next(t, pull)
	if got.Subject != contracts.AgentSubject("INFRA", scanSession) {
		t.Fatalf("handed over %d on %s first, want the new-form message: the old-form one stored before it is skipped on this server", got.StreamSeq, got.Subject)
	}
}

// scanSession is the session every case of this file follows its own messages for.
const scanSession = "ses_scan"

// scanSubjects is what scanSession registers: its own agent subject in the old form, as a plugin
// does on every heartbeat.
var scanSubjects = []string{contracts.LegacyAgentSubject(scanSession)}

// releasedAndRegisteredAgain drives the idle release the Legion daemon makes: a registration
// (Ensure, RestartAtFloor, Pull), a release (Delete), other sessions' traffic and one message on
// the session's agent subject in each form stored while it had no consumer, more traffic, and a
// registration again. It returns the new pull and the session's own messages, in stream order.
func releasedAndRegisteredAgain(t *testing.T, owner *Client, consumers *SessionConsumers) (*SessionPull, []ownMessage) {
	t.Helper()
	ctx := context.Background()
	register := func() *SessionPull {
		t.Helper()
		if _, err := consumers.Ensure(ctx, scanSession, scanSubjects, 0); err != nil {
			t.Fatalf("ensure the consumer of %s: %v", scanSession, err)
		}
		if _, err := consumers.RestartAtFloor(ctx, scanSession, scanSubjects); err != nil {
			t.Fatalf("restart the consumer of %s at its floor: %v", scanSession, err)
		}
		return pullSession(t, consumers, scanSession)
	}
	publishOthers(t, owner, 2)
	register().Stop()
	if _, err := consumers.Delete(ctx, scanSession, nil); err != nil {
		t.Fatalf("delete the consumer of %s at its release: %v", scanSession, err)
	}
	publishOthers(t, owner, otherSessionsTraffic)
	own := publishOwn(t, owner, scanSession)
	publishOthers(t, owner, 4)
	return register(), own
}

// liveSessionConsumers starts image, owns the stream on it and connects SessionConsumers beside
// the owner, both closed when the test ends.
func liveSessionConsumers(t *testing.T, image string) (*Client, *SessionConsumers) {
	t.Helper()
	_, uri := testnats.StartImageConfig(t, image, "jetstream {}\n", liveServerArgs, true)
	owner, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil, WithEnvironment(liveServerEnvironment))
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	return owner, consumers
}

// publishOthers stores n messages on two other sessions' agent subjects, in the old form, in turn.
func publishOthers(t *testing.T, owner *Client, n int) {
	t.Helper()
	for i := range n {
		publishStored(t, owner, contracts.LegacyAgentSubject(fmt.Sprintf("ses_other_%d", i%2)), "another session's message")
	}
}

// ownMessage is one message stored for the session: its stream sequence and subject.
type ownMessage struct {
	seq     uint64
	subject string
}

// publishOwn stores one message on session's agent subject in each form, the old form first, and
// returns them in stream order.
func publishOwn(t *testing.T, owner *Client, session string) []ownMessage {
	t.Helper()
	var own []ownMessage
	for _, subject := range []string{contracts.LegacyAgentSubject(session), contracts.AgentSubject("INFRA", session)} {
		own = append(own, ownMessage{seq: publishStored(t, owner, subject, "the session's own message"), subject: subject})
	}
	return own
}

// requireHandedOver requires pull to hand over want, in order, and nothing after it: once the
// last of want is acknowledged, session's consumer holds nothing more to deliver and the pull
// holds nothing it has not handed over (requireNoMessage).
func requireHandedOver(t *testing.T, consumers *SessionConsumers, pull *SessionPull, session string, want []ownMessage) {
	t.Helper()
	for _, message := range want {
		got := next(t, pull)
		if got.StreamSeq != message.seq || got.Subject != message.subject {
			t.Fatalf("handed over %d on %s, want %d on %s", got.StreamSeq, got.Subject, message.seq, message.subject)
		}
		if err := got.Ack(); err != nil {
			t.Fatalf("ack %d: %v", got.StreamSeq, err)
		}
	}
	requireNoMessage(t, consumers, pull, session)
}
