package bus_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	grantedRole = "notifications.role.granted"
	deniedRole  = "notifications.role.denied"
)

// grantedClient connects a bus client as a user NATS lets publish only to the granted role topic
// and to inboxes, and returns it with a plain connection of the same user that the tests subscribe
// through.
func grantedClient(t *testing.T) (*bus.Client, *natsgo.Conn) {
	t.Helper()
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, grantedRole, "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subscriber, err := bus.Dial("role-subscriber", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	t.Cleanup(subscriber.Close)
	return client, subscriber
}

func roleEnvelope(topic string) contracts.Envelope {
	return contracts.Envelope{
		EventID:       "evt-" + topic,
		Source:        "agent",
		SourceEventID: "source-" + topic,
		Topic:         topic,
		DedupeKey:     "dedupe-" + topic,
		IssuedAt:      contracts.NowMillis(),
	}
}

// A core publish the server denies returns the denial, naming the subject, where core NATS itself
// reports nothing to the publisher; a granted one after it is still accepted and delivered, so
// the connection's recorded violation does not fail every later publish. Publish takes the core
// transport for these role lanes (bus.Client.UsesCoreTransport).
func TestPublishReportsACorePublishNATSDenies(t *testing.T) {
	client, subscriber := grantedClient(t)
	received, err := subscriber.SubscribeSync(grantedRole)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	err = client.Publish(roleEnvelope(deniedRole))
	if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("denied publish returned %v, want ErrPublishDenied wrapping the permissions violation", err)
	}
	if !strings.Contains(err.Error(), `Permissions Violation for Publish to "`+deniedRole+`"`) {
		t.Fatalf("denied publish error %q does not carry the server's violation for %s", err, deniedRole)
	}

	if err := client.Publish(roleEnvelope(grantedRole)); err != nil {
		t.Fatalf("granted publish after a denial returned %v", err)
	}
	if _, err := received.NextMsgWithContext(testwait.Context(t)); err != nil {
		t.Fatalf("granted publish was not delivered: %v", err)
	}
}

// jetStreamGrantedClient connects a stream-owning bus client as a user NATS lets publish only to
// the subjects allow names, beside the JetStream API and the inboxes the client needs to create
// its stream and to be answered on it.
func jetStreamGrantedClient(t *testing.T, allow ...string) *bus.Client {
	t.Helper()
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, append([]string{"$JS.API.>", "_INBOX.>"}, allow...)...).URL
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect as the granted user: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// A JetStream publish the server denies returns the denial, naming the subject, rather than an
// unreachable NATS: the stream acknowledges a denied publish with nothing, so a publisher reading
// the acknowledgement alone meets the window running out and reports an outage. A publish of the
// same stream subject under a grant that covers it is accepted and retained.
func TestPublishReportsAJetStreamPublishNATSDenies(t *testing.T) {
	const topic = "notifications.dispatch.issue.ACME-1.message.created"
	envelope := contracts.Envelope{
		EventID:       "evt-jetstream-denied",
		Source:        "dispatch",
		SourceEventID: "source-jetstream-denied",
		Topic:         topic,
		DedupeKey:     "publish.jetstream.denied",
		IssuedAt:      contracts.NowMillis(),
	}

	err := jetStreamGrantedClient(t).Publish(envelope)
	if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("denied JetStream publish returned %v, want ErrPublishDenied wrapping the permissions violation", err)
	}
	if errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("denied JetStream publish returned %v, which reads as an unreachable NATS", err)
	}
	if !strings.Contains(err.Error(), `Permissions Violation for Publish to "`+topic+`"`) {
		t.Fatalf("denied JetStream publish error %q does not carry the server's violation for %s", err, topic)
	}

	granted := jetStreamGrantedClient(t, "notifications.dispatch.>")
	if err := granted.Publish(envelope); err != nil {
		t.Fatalf("granted JetStream publish returned %v", err)
	}
	if _, err := granted.JS().GetLastMsg(bus.Stream, topic); err != nil {
		t.Fatalf("the stream did not retain the granted publish: %v", err)
	}
}

// A role-lane forward the server denies returns the denial at once, before any wait: the listener
// reports it delivery_failed rather than holding the role send open for its whole window.
func TestStartRequestStoredToReportsAForwardNATSDenies(t *testing.T) {
	client, _ := grantedClient(t)
	started := time.Now()
	_, err := client.StartRequestStoredTo(deniedRole, roleEnvelope(deniedRole), bus.ForwardMsgID("denied-forward", deniedRole), 2*time.Second)
	if !errors.Is(err, bus.ErrPublishDenied) {
		t.Fatalf("denied forward returned %v, want ErrPublishDenied", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("denied forward waited %s, the whole forward window", elapsed)
	}
}

// A window's Request the server denies answers the denial at once, not as a Request nobody
// answered.
func TestAWindowRequestReportsAForwardNATSDenies(t *testing.T) {
	client, _ := grantedClient(t)
	started := time.Now()
	answered := client.WindowUntil(nil).Request(deniedRole, roleEnvelope(deniedRole), bus.RequestOptions{MsgID: bus.ForwardMsgID("denied", deniedRole), Timeout: 2 * time.Second})
	if err := testwait.Receive(t, "the denied Request to be answered", answered); !errors.Is(err, bus.ErrPublishDenied) || errors.Is(err, bus.ErrNotAcknowledged) {
		t.Fatalf("denied Request answered %v, want ErrPublishDenied", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("denied Request waited %s, the whole timeout", elapsed)
	}
}

// The connection probe's request (RequestCore) the server denies answers the denial at once, not
// as a subject that kept silent until the deadline: the send route counts the session connected
// and logs the denial (cmd/listener), and a probe that waited would spend the send's budget.
func TestARequestCoreNATSDeniesAnswersTheDenialAtOnce(t *testing.T) {
	client, _ := grantedClient(t)
	// The bound under test, not a wait: a denial answers at once, and a probe that waits for an
	// answer instead runs out these 2 s, which the row reads below.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := client.RequestCore(ctx, deniedRole, natsgo.NewInbox(), natsgo.Header{"Envoy-Probe": []string{"1"}})
	if !errors.Is(err, bus.ErrPublishDenied) {
		t.Fatalf("denied request answered %v, want ErrPublishDenied", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("denied request waited %s, the whole deadline", elapsed)
	}
}

// The server's refusal of the core subscription the client sent again on reconnecting reads as an
// inactive subscription until a server that grants it accepts it again. nats.go keeps a refused
// subscription valid and only records the refusal, so the listener's role lane, reconnected onto
// a server restarted without its grant on notifications.role.>, would deliver nothing while its
// /healthz answered healthy and subscribed. CoreSubOK is what that check reads
// (cmd/listener/main.go, "role lane subscription inactive").
func TestCoreSubOKIsFalseWhileTheServerRefusesTheResentSubscription(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "refusal-test"
		topic   = "notifications.team.refusal-test"
	)
	seed, public := testnats.User(t)
	granted := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granted, []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	received := make(chan string, 1)
	held, err := client.SubscribeCore(subject, func(msg *natsgo.Msg) { received <- msg.Subject }, queue)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	if err := client.Conn.LastError(); err != nil {
		t.Fatalf("the server that grants %s refused it: %v", subject, err)
	}
	if !client.CoreSubOK() {
		t.Fatal("CoreSubOK is false on a server that grants the subscription")
	}

	server.Restart(t, testnats.SubscribeGrantConfig(public, "_INBOX.>"))
	waitFor(t, "CoreSubOK to read the restarted server's refusal of the re-sent subscription", func() bool {
		return client.Connected() && !client.CoreSubOK()
	})
	if !held.IsValid() {
		t.Fatal("nats.go dropped the refused subscription; the test proves nothing about a refusal it keeps valid")
	}
	if err := client.Conn.LastError(); !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("the restarted server's last error is %v, want its refusal of %s", err, subject)
	}

	server.Restart(t, granted)
	waitFor(t, "CoreSubOK once a server that grants the subscription has it again", func() bool {
		return client.Connected() && client.CoreSubOK()
	})
	// The flush returns once the server has handled every protocol line the client sent before it,
	// the re-sent subscription among them.
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the re-sent subscription: %v", err)
	}
	publisher, err := bus.Dial("refusal-publisher", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	if err := publisher.Publish(topic, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the publish: %v", err)
	}
	if got := testwait.Receive(t, "the subscription the server granted again to receive the publish", received); got != topic {
		t.Fatalf("the subscription received %s, want %s", got, topic)
	}
	if !client.CoreSubOK() {
		t.Fatal("CoreSubOK is false after the server accepted the subscription and delivered to it")
	}
}

// A reload that takes the grant away revokes the core subscription the server had accepted, and
// the server says so in a refusal of its own form (`Permissions Violation for Subscription to
// "<subject>" using queue "<queue>" (sid "<sid>")`, which from nats-server v2.15.0 names the
// queue group a revoked queue subscription has and before it named none): CoreSubOK reads it as
// refused while the connection stays up, though nats.go keeps the subscription valid and leaves
// the refusal of a queue subscription unrecorded. A restart onto a configuration that grants it
// again, to which the client re-sends the subscription, makes it OK again.
func TestCoreSubOKIsFalseOnceAReloadRevokesTheSubscription(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "revocation-test"
	)
	seed, public := testnats.User(t)
	granted := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granted, []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	held, err := client.SubscribeCore(subject, func(*natsgo.Msg) {}, queue)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	if !client.CoreSubOK() {
		t.Fatal("CoreSubOK is false on a server that grants the subscription")
	}

	if err := server.Reload(t, testnats.SubscribeGrantConfig(public, "_INBOX.>")); err != nil {
		t.Fatalf("reload without the grant: %v", err)
	}
	waitFor(t, "CoreSubOK to read the reload's revocation of the subscription", func() bool {
		return !client.CoreSubOK()
	})
	if !client.Connected() {
		t.Fatal("the client lost its connection to a server that only reloaded")
	}
	if !held.IsValid() {
		t.Fatal("nats.go dropped the revoked subscription; the test proves nothing about a refusal it keeps valid")
	}
	// The server's own wording for the refusal has changed between versions (v2.15.1 names the
	// queue group between the subject and the sid), so what this asserts is the refusal and the
	// subject it names, not the sentence.
	if err := client.Conn.LastError(); !errors.Is(err, natsgo.ErrPermissionViolation) ||
		!strings.Contains(err.Error(), `Permissions Violation for Subscription to "`+subject+`"`) {
		t.Fatalf("the server's last error is %v, want its revocation of %s", err, subject)
	}

	server.Restart(t, granted)
	waitFor(t, "CoreSubOK once a restarted server that grants the subscription has it again", func() bool {
		return client.Connected() && client.CoreSubOK()
	})
}

// ResendRefusedCore sends a revoked core subscription again only once the handler has taken every
// message the server delivered to it before the revocation: the unsubscribe that follows the send
// would drop them. While the handler holds the first message and the second waits behind it, it
// sends nothing; once the handler has taken both, it sends one SUB, and the handler was handed
// both messages.
func TestResendRefusedCoreWaitsForTheMessagesTheRevokedSubscriptionHolds(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "resend-test"
		topic   = "notifications.team.resend-test"
	)
	seed, public := testnats.User(t)
	granted := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granted, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	handled := make(chan string, 2)
	if _, err := client.SubscribeCore(subject, func(msg *natsgo.Msg) {
		entered <- struct{}{}
		<-release
		handled <- msg.Subject
	}, queue); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	publisher, err := bus.Dial("resend-publisher", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	for range 2 {
		if err := publisher.Publish(topic, []byte("{}")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the publishes: %v", err)
	}
	testwait.Receive(t, "the handler to be handed a message", entered)
	// The server queued both deliveries before the PONG this flush waits for, so the second one
	// is in the subscription's pending messages once it returns.
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the client: %v", err)
	}

	if err := server.Reload(t, testnats.SubscribeGrantConfig(public, "_INBOX.>")); err != nil {
		t.Fatalf("reload without the grant: %v", err)
	}
	waitFor(t, "CoreSubOK to read the reload's revocation", func() bool { return !client.CoreSubOK() })
	sent := func(from int) int { return strings.Count(server.Log()[from:], "<<- [SUB "+subject+" ") }
	from := len(server.Log())
	if err := client.ResendRefusedCore(); err != nil {
		t.Fatalf("resend with a message pending: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush after the resend: %v", err)
	}
	if n := sent(from); n != 0 {
		t.Fatalf("the client sent the subscription %d times while a message delivered before the revocation was pending, want 0", n)
	}
	if client.CoreSubOK() {
		t.Fatal("CoreSubOK is true for a revoked subscription the client did not send again")
	}

	close(release)
	waitFor(t, "the client to send the subscription again once the handler took both messages", func() bool {
		if err := client.ResendRefusedCore(); err != nil {
			t.Fatalf("resend: %v", err)
		}
		if err := client.Conn.Flush(); err != nil {
			t.Fatalf("flush after the resend: %v", err)
		}
		return sent(from) > 0
	})
	if n := sent(from); n != 1 {
		t.Fatalf("the client sent the subscription %d times, want 1", n)
	}
	for range 2 {
		if got := testwait.Receive(t, "the handler to be handed both messages delivered before the revocation", handled); got != topic {
			t.Fatalf("the handler was handed %s, want %s", got, topic)
		}
	}
}

// revokedCoreClient connects a client as a user the server first grants subject, subscribes it in
// queue, and then has the server reload without the grant, which revokes the subscription. It
// returns the client, the server, its granting configuration and a count of the subscriptions to
// subject the server received after a point in its log.
func revokedCoreClient(t *testing.T, subject, queue string) (*bus.Client, *testnats.Server, string, func(from int) int) {
	t.Helper()
	seed, public := testnats.User(t)
	granted := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granted, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if _, err := client.SubscribeCore(subject, func(*natsgo.Msg) {}, queue); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	if err := server.Reload(t, testnats.SubscribeGrantConfig(public, "_INBOX.>")); err != nil {
		t.Fatalf("reload without the grant: %v", err)
	}
	waitFor(t, "CoreSubOK to read the reload's revocation", func() bool { return !client.CoreSubOK() })
	sent := func(from int) int { return strings.Count(server.Log()[from:], "<<- [SUB "+subject+" ") }
	return client, server, granted, sent
}

// ResendRefusedCore keeps the refusal mark until the server has answered what it sent: right after
// a send the server refuses, CoreSubOK still reads false. Cleared before the answer, it read true
// for about one round trip after each send, and /healthz answered 200 for a lane the server
// refuses. Each of the ten sends here is counted in the server's trace, so a pass is not a send
// that never happened.
func TestCoreSubOKStaysFalseThroughAResendTheServerRefuses(t *testing.T) {
	const sends = 10
	client, server, _, sent := revokedCoreClient(t, "notifications.>", "resend-answer-test")
	from := len(server.Log())
	for attempt := 1; attempt <= sends; attempt++ {
		waitFor(t, "CoreSubOK to read the refusal before the next send", func() bool { return !client.CoreSubOK() })
		if err := client.ResendRefusedCore(); err != nil {
			t.Fatalf("send %d: %v", attempt, err)
		}
		if client.CoreSubOK() {
			t.Fatalf("CoreSubOK read true right after send %d, which the server refuses", attempt)
		}
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n := sent(from); n != sends {
		t.Fatalf("the server received %d subscriptions, want the %d the client sent", n, sends)
	}
}

// A refusal of the session before a reconnect is not answered. Once nats.go has reconnected, it has
// sent the subscription again to the new server, but the mark it set on the old session stays until
// the reconnect callback clears it (countConnectionEvent). A send in that gap sent a second SUB and
// unsubscribed the one the new server had just accepted. Here a disconnect hook holds nats.go's
// callback goroutine, so the reconnect callback waits while the client is connected to the
// restarted server, which grants the subscription: ResendRefusedCore sends nothing, and the server
// receives nats.go's SUB alone and no UNSUB.
func TestResendRefusedCoreSendsNothingForTheRefusalOfTheSessionBeforeAReconnect(t *testing.T) {
	const subject = "notifications.>"
	client, server, granted, sent := revokedCoreClient(t, subject, "resend-session-test")
	held, release := make(chan struct{}), make(chan struct{})
	var hold, free sync.Once
	let := func() { free.Do(func() { close(release) }) }
	t.Cleanup(let)
	client.AddDisconnectHook(func() {
		hold.Do(func() {
			close(held)
			<-release
		})
	})

	from := len(server.Log())
	server.Restart(t, granted)
	testwait.Receive(t, "the restart's disconnect to reach the hook", held)
	waitFor(t, "the client to reconnect while its reconnect callback waits", func() bool {
		return client.Conn.Status() == natsgo.CONNECTED && client.Conn.Stats().Reconnects == 1
	})
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the reconnected connection: %v", err)
	}
	if client.CoreSubOK() {
		t.Fatal("CoreSubOK reads true before the reconnect callback ran; the test proves nothing about the gap")
	}
	if err := client.ResendRefusedCore(); err != nil {
		t.Fatalf("resend in the gap: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush after the resend: %v", err)
	}
	subs, unsubs := sent(from), strings.Count(server.Log()[from:], "<<- [UNSUB ")
	let()
	if subs != 1 || unsubs != 0 {
		t.Fatalf("after the reconnect and before its callback ran, the server received %d SUBs and %d UNSUBs; want nats.go's one SUB and no UNSUB", subs, unsubs)
	}
	waitFor(t, "CoreSubOK once the reconnect callback ran", client.CoreSubOK)
}
