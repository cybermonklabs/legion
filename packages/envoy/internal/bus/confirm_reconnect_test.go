package bus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// denyThenDrop serves a NATS client: it answers the handshake, then answers the first publish's
// flush with the server's permissions violation for subject and the PONG, and closes the
// connection. That is what a server restarted just behind a denied publish looks like to the
// client. With reconnectable false it closes its listener too, so the client stays reconnecting;
// with it true it serves the client's next connection, so the reconnect completes.
func denyThenDrop(t *testing.T, subject string, reconnectable bool) string {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serveFake(conn, subject)
		if !reconnectable {
			listener.Close()
			return
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFake(conn, "")
		}
	}()
	return "nats://" + listener.Addr().String()
}

// serveFake speaks just enough of the NATS protocol to one client: INFO, then a PONG for every
// PING. When deny names a subject, the first publish's flush is answered with the permissions
// violation for it and the PONG, and the connection is closed.
func serveFake(conn net.Conn, deny string) {
	defer conn.Close()
	fmt.Fprintf(conn, "INFO {\"server_id\":\"fake\",\"version\":\"2.10.29\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
	lines := bufio.NewReader(conn)
	published := false
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(line, "PUB "):
			if _, err := lines.ReadString('\n'); err != nil {
				return
			}
			published = true
		case strings.HasPrefix(line, "PING") && published && deny != "":
			fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to %q'\r\nPONG\r\n", deny)
			return
		case strings.HasPrefix(line, "PING"):
			fmt.Fprint(conn, "PONG\r\n")
		}
	}
}

// A denied publish whose connection drops right behind the flush's PONG is not confirmed: the
// reconnect clears the recorded violation before it has counted itself, so the connection's last
// error and reconnect count read exactly as they did before the publish.
func TestConfirmPublishedRefusesADenialAReconnectCleared(t *testing.T) {
	subject := "notifications.role.reviewer"
	var once sync.Once
	disconnected := make(chan struct{})
	conn, err := nats.Connect(denyThenDrop(t, subject, false),
		nats.ReconnectWait(time.Hour),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { once.Do(func() { close(disconnected) }) }),
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	before := observe(conn)
	if err := conn.Publish(subject, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// nats.go queues the disconnect callback after the reconnect has cleared the last error
	// (doReconnect), so once it runs the violation is gone and the reconnect has not counted.
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never saw the server drop the connection")
	}
	if err := confirmPublished(conn, subject, before); !errors.Is(err, ErrPublishUnconfirmed) {
		t.Fatalf("a denied publish read as %v, want unconfirmed: last error %v, reconnecting %t",
			err, conn.LastError(), conn.IsReconnecting())
	}
}

// A denied publish whose connection drops behind the flush's PONG and reconnects before the check
// is not confirmed either: the connection reads as connected again with no error, and only its
// reconnect count shows the violation may have gone with the old connection.
func TestConfirmPublishedRefusesADenialACompletedReconnectCleared(t *testing.T) {
	subject := "notifications.role.reviewer"
	var once sync.Once
	reconnected := make(chan struct{})
	conn, err := nats.Connect(denyThenDrop(t, subject, true),
		nats.ReconnectWait(10*time.Millisecond),
		nats.MaxReconnects(-1),
		nats.ReconnectHandler(func(*nats.Conn) { once.Do(func() { close(reconnected) }) }),
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	before := observe(conn)
	if err := conn.Publish(subject, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// nats.go queues the reconnect callback once the new connection is marked connected.
	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the client never reconnected")
	}
	if err := confirmPublished(conn, subject, before); !errors.Is(err, ErrPublishUnconfirmed) {
		t.Fatalf("a denied publish read as %v, want unconfirmed: before %+v, after %+v", err, before, observe(conn))
	}
}

// sliceError is an error whose value cannot be compared: its interface field holds a slice, though
// its type reads as comparable.
type sliceError struct{ cause any }

func (sliceError) Error() string { return "slice error" }

// sameError never panics on a value that cannot be compared, and counts it as a change.
func TestSameErrorTreatsAValueThatCannotBeComparedAsChanged(t *testing.T) {
	if sameError(sliceError{[]int{1}}, sliceError{[]int{1}}) {
		t.Fatal("two incomparable error values read as the same error")
	}
	shared := errors.New("shared")
	if !sameError(shared, shared) {
		t.Fatal("one error value read as a change")
	}
}

// answeringServer serves one NATS client and stays up: it answers every flush that follows a
// publish with the permissions violation for deny, when deny is not empty, and then the PONG,
// and, when ack is not empty, then sends each line of ack, in order, to the publish's reply
// subject as a reply of its own, as the notification stream's acknowledgement of a captured agent
// subject answers a forward (storedAck is a stored copy's). A publish with headers (HPUB, as a
// forward with its Nats-Msg-Id is) is read whole by its length, since its header block spans
// lines.
func answeringServer(t *testing.T, deny string, ack string) *nats.Conn {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "INFO {\"server_id\":\"fake\",\"version\":\"2.10.29\",\"proto\":1,\"headers\":true,\"max_payload\":1048576}\r\n")
		lines := bufio.NewReader(conn)
		sids := map[string]string{}
		published, reply := false, ""
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			switch {
			case len(fields) == 3 && fields[0] == "SUB":
				sids[fields[1]] = fields[2]
			case len(fields) >= 3 && fields[0] == "PUB":
				if _, err := lines.ReadString('\n'); err != nil {
					return
				}
				published, reply = true, ""
				if len(fields) == 4 {
					reply = fields[2]
				}
			case len(fields) >= 4 && fields[0] == "HPUB":
				total, err := strconv.Atoi(fields[len(fields)-1])
				if err != nil {
					return
				}
				if _, err := io.ReadFull(lines, make([]byte, total+len("\r\n"))); err != nil {
					return
				}
				published, reply = true, ""
				if len(fields) == 5 {
					reply = fields[2]
				}
			case len(fields) == 1 && fields[0] == "PING" && published:
				if deny != "" {
					fmt.Fprintf(conn, "-ERR 'Permissions Violation for Publish to %q'\r\n", deny)
				}
				fmt.Fprint(conn, "PONG\r\n")
				if sid, ok := sids[reply]; ok && ack != "" {
					for _, frame := range strings.Split(ack, "\n") {
						fmt.Fprintf(conn, "MSG %s %s %d\r\n%s\r\n", reply, sid, len(frame), frame)
					}
				}
				published = false
			case len(fields) == 1 && fields[0] == "PING":
				fmt.Fprint(conn, "PONG\r\n")
			}
		}
	}()
	conn, err := nats.Connect("nats://"+listener.Addr().String(), nats.MaxReconnects(0))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// The state a publish is judged against is read before the publish: a violation the server reports
// while the publish call is still running is this publish's denial, not a state it started from.
func TestPublishConfirmedReadsTheStateBeforeThePublish(t *testing.T) {
	subject := "notifications.role.reviewer"
	conn := answeringServer(t, subject, "")
	client := &Client{Conn: conn}
	// A publish whose own flush has the server's violation recorded before it returns.
	answered := func(msg *nats.Msg) error {
		if err := conn.PublishMsg(msg); err != nil {
			return err
		}
		return conn.Flush()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: []byte("{}")}, answered, false)
	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("a publish the server denied while it ran returned %v, want ErrPublishDenied", err)
	}
}

// A forward publishConfirmed cannot confirm - the server reported another subject's violation
// during it - still counts as delivered when the stream's acknowledgement arrives, since it
// proves the forward reached the stream.
func TestAnUnconfirmedForwardTheStreamStoredIsDelivered(t *testing.T) {
	conn := answeringServer(t, "notifications.other", storedAck)
	client := &Client{Conn: conn}

	forward, err := client.StartRequestStoredTo(context.Background(), "notifications.agent.holder", contracts.Envelope{EventID: "e", Topic: "notifications.role.r"}, ForwardMsgID("e", "notifications.agent.holder"), 2*time.Second)
	if err != nil {
		t.Fatalf("an unconfirmed forward was refused outright: %v", err)
	}
	if _, err := forward.Wait(); err != nil {
		t.Fatalf("an unconfirmed forward the stream acknowledged returned %v, want nil", err)
	}
}

// An unconfirmed forward the stream never acknowledged is not stored, and says why it is
// unconfirmed: the listener reports it delivery_failed rather than delivered.
func TestAnUnconfirmedForwardWithoutTheStreamsAcknowledgementIsNotStored(t *testing.T) {
	conn := answeringServer(t, "notifications.other", "")
	client := &Client{Conn: conn}

	forward, err := client.StartRequestStoredTo(context.Background(), "notifications.agent.holder", contracts.Envelope{EventID: "e", Topic: "notifications.role.r"}, ForwardMsgID("e", "notifications.agent.holder"), 300*time.Millisecond)
	if err != nil {
		t.Fatalf("an unconfirmed forward was refused outright: %v", err)
	}
	_, err = forward.Wait()
	if !errors.Is(err, ErrPublishUnconfirmed) || !errors.Is(err, ErrForwardNotStored) {
		t.Fatalf("an unconfirmed forward with no acknowledgement returned %v, want the unconfirmed error wrapped by ErrForwardNotStored", err)
	}
}

// storedAck is the notification stream's acknowledgement of a copy it stored at sequence 1.
const storedAck = `{"stream":"ENVOY_NOTIFICATIONS","seq":1}`

// A forward's Wait returns the stream's acknowledgement: a duplicate's sequence and verdict, which
// a role send answers the stored forward by. A stream's duplicate that names no sequence is no
// stored forward: Wait says so at once, as ErrForwardNotStored wrapping
// ErrDuplicateWithoutSequence, rather than wait out the forward's window for an answer that does
// not come; the check is that Wait returned before the forward's own deadline, which a wait for
// the window's end cannot. A reply that names no stream is no acknowledgement, whatever else it
// says, and Wait passes it over for the stream's acknowledgement after it.
func TestAForwardsWaitReturnsTheStreamsVerdict(t *testing.T) {
	// window is long beside any answer's time on a loaded runner, so only a wait that does not end
	// on the answer reaches it.
	const window = 30 * time.Second
	forward := func(t *testing.T, ack string) (StoredAck, bool, error) {
		t.Helper()
		client := &Client{Conn: answeringServer(t, "", ack)}
		deadline := time.Now().Add(window)
		pending, err := client.StartRequestStoredTo(context.Background(), "notifications.agent.holder", contracts.Envelope{EventID: "e", Topic: "notifications.role.r"}, ForwardMsgID("e", "notifications.agent.holder"), window)
		if err != nil {
			t.Fatalf("the forward was refused outright: %v", err)
		}
		got, err := pending.Wait()
		return got, time.Now().Before(deadline), err
	}
	if got, _, err := forward(t, `{"stream":"ENVOY_NOTIFICATIONS","seq":7,"duplicate":true}`); err != nil || got != (StoredAck{Sequence: 7, Duplicate: true}) {
		t.Fatalf("a duplicate forward's wait = %+v (%v), want sequence 7 and Duplicate", got, err)
	}
	got, beforeDeadline, err := forward(t, `{"stream":"ENVOY_NOTIFICATIONS","seq":0,"duplicate":true}`)
	if !errors.Is(err, ErrForwardNotStored) || !errors.Is(err, ErrDuplicateWithoutSequence) || got != (StoredAck{}) {
		t.Fatalf("a duplicate with no sequence = %+v (%v), want ErrForwardNotStored wrapping ErrDuplicateWithoutSequence", got, err)
	}
	if !beforeDeadline {
		t.Fatalf("the wait for a duplicate with no sequence ended at its %s window's deadline, want it to end on that answer", window)
	}
	got, _, err = forward(t, `{"stream":"","seq":7,"duplicate":true}`+"\n"+`{"seq":0,"duplicate":true}`+"\n"+`{"stream":"ENVOY_NOTIFICATIONS","seq":7,"duplicate":true}`)
	if err != nil || got != (StoredAck{Sequence: 7, Duplicate: true}) {
		t.Fatalf("a duplicate forward's wait behind a reply that names no stream = %+v (%v), want the stream's sequence 7 and Duplicate", got, err)
	}
}

// The stream's acknowledgement settles a forward whose wait begins after its caller's context was
// cancelled, when the acknowledgement was delivered before: nats.go answers a context that has
// ended before it reads a reply already delivered (Subscription.NextMsgWithContext), and a wait
// that read only that answer would call a stored forward not stored, which the role lane's stop
// then holds and parks. The row waits until the acknowledgement is delivered to the forward's
// reply subscription, a count nats.go keeps (Subscription.Pending), then cancels, then waits. The
// control is a forward no acknowledgement answers, whose wait after the same cancel is
// ErrForwardNotStored wrapping the cancel.
func TestAForwardsAcknowledgementDeliveredBeforeItsCancelSettlesIt(t *testing.T) {
	start := func(t *testing.T, ack string) (*PendingForward, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		client := &Client{Conn: answeringServer(t, "", ack)}
		pending, err := client.StartRequestStoredTo(ctx, "notifications.agent.holder", contracts.Envelope{EventID: "e", Topic: "notifications.role.r"}, ForwardMsgID("e", "notifications.agent.holder"), 30*time.Second)
		if err != nil {
			t.Fatalf("the forward was refused outright: %v", err)
		}
		return pending, cancel
	}

	acknowledged, cancel := start(t, storedAck)
	testwait.Eventually(t, "the stream's acknowledgement to reach the forward's reply subscription", func() bool {
		delivered, _, err := acknowledged.replies.Pending()
		return err == nil && delivered > 0
	})
	cancel()
	if got, err := acknowledged.Wait(); err != nil || got != (StoredAck{Sequence: 1}) {
		t.Fatalf("a forward whose acknowledgement was delivered before its caller's cancel = %+v (%v), want the stream's sequence 1", got, err)
	}

	unanswered, cancel := start(t, "")
	cancel()
	if got, err := unanswered.Wait(); !errors.Is(err, ErrForwardNotStored) || !errors.Is(err, context.Canceled) || got != (StoredAck{}) {
		t.Fatalf("a forward no acknowledgement answered, after its caller's cancel = %+v (%v), want ErrForwardNotStored wrapping context.Canceled", got, err)
	}
}

// A no-responders answer that nats.go returns in the instant the forward's window ends is the
// server's answer, not the window's end: next returns it, so Wait reports the forward not stored
// with no responders, which leaves the message free to go to another holder (cmd/listener's
// forwardMayBeStored), and not the window's error, which pins it to a holder nobody answered for.
// No run can force that instant, so the row hands next a read that ends the window and returns
// nats.ErrNoResponders. The control is a read that returns the window's own error, which next
// answers as the window's end once no delivered reply is left.
func TestAForwardsNoRespondersAnswerAtItsWindowsEndIsKept(t *testing.T) {
	conn := answeringServer(t, "", "")
	pending := func(t *testing.T) (*PendingForward, context.CancelFunc) {
		t.Helper()
		replies, err := conn.SubscribeSync(conn.NewInbox())
		if err != nil {
			t.Fatalf("subscribe the forward's reply inbox: %v", err)
		}
		t.Cleanup(func() { _ = replies.Unsubscribe() })
		window, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Cleanup(cancel)
		return &PendingForward{replies: replies, window: window, stopped: context.Background(), cancel: cancel}, cancel
	}

	answered, end := pending(t)
	if _, err := answered.next(func(context.Context) (*nats.Msg, error) {
		end()
		return nil, nats.ErrNoResponders
	}); !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("next with no responders read as the window ended = %v, want nats.ErrNoResponders", err)
	}

	ended, end := pending(t)
	if _, err := ended.next(func(ctx context.Context) (*nats.Msg, error) {
		end()
		return nil, ctx.Err()
	}); !errors.Is(err, context.Canceled) || errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("next with the window's own end read = %v, want context.Canceled", err)
	}
}
