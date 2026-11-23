package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// forcedPointModeEnv names what forcedProxy does at its armed point: "" passes the operation on (the
// control); "drop" closes the connection without writing it; "stall" writes nothing more on that
// connection - the operation and every byte after it are swallowed - until the client gives the
// connection up, as a listener whose pings go unanswered does (bus.WithSilentCutDetection); and
// "late" holds the operation, and every byte after it, for lateHold and then writes them all, so
// the server stores a forward whose window ended before its flush was answered.
const forcedPointModeEnv = "ROLE_LANE_FORCED_MODE"

// lateHold is how long the "late" forced point holds a forward: past its 2 s window
// (roleForwardTimeout), and inside the 3 s after which the listener calls a connection with two
// pings unanswered stale (bus.WithSilentCutDetection), so the connection stays up.
const lateHold = roleForwardTimeout + 200*time.Millisecond

// forcedProxy relays NATS client connections to one server and acts once, at the first PUB or HPUB
// to the subject it was armed with, on whichever connection sends it.
type forcedProxy struct {
	listener net.Listener
	target   string
	url      string
	mode     string

	mu      sync.Mutex
	subject string
	armed   bool
	fired   int
	conns   map[net.Conn]struct{}
}

func startForcedProxy(t *testing.T, uri, mode string) *forcedProxy {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q: %v", uri, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the forced-point proxy: %v", err)
	}
	proxied := *server
	proxied.Host = listener.Addr().String()
	p := &forcedProxy{listener: listener, target: server.Host, url: proxied.String(), mode: mode, conns: map[net.Conn]struct{}{}}
	go p.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for conn := range p.conns {
			_ = conn.Close()
		}
	})
	return p
}

// arm makes the next PUB or HPUB to subject the forced point.
func (p *forcedProxy) arm(subject string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subject, p.armed = subject, true
}

// take reports whether a publish to subject is the forced point, and disarms the proxy if it is.
func (p *forcedProxy) take(subject string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.armed || subject != p.subject {
		return false
	}
	p.armed = false
	p.fired++
	return true
}

func (p *forcedProxy) firedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fired
}

func (p *forcedProxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range conns {
		p.conns[conn] = struct{}{}
	}
}

func (p *forcedProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.join(client)
	}
}

// join relays one client connection. The server's bytes are copied as they come; the client's are
// read one protocol operation at a time (readNATSClientOp), so the armed publish is found whole,
// and this proxy's fault, its mode, is applied to that one publish.
func (p *forcedProxy) join(client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	p.track(client, upstream)
	closeBoth := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	defer closeBoth()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeBoth()
	}()
	reader := bufio.NewReaderSize(client, 64<<10)
	for {
		op, err := readNATSClientOp(reader)
		if err != nil {
			return
		}
		if op.publishes() && p.take(op.subject()) {
			switch p.mode {
			case "drop":
				return
			case "stall":
				// Nothing more reaches the server on this connection: the client's pings go
				// unanswered until it declares the connection stale and closes it.
				_, _ = io.Copy(io.Discard, reader)
				return
			case "late":
				time.Sleep(lateHold)
			}
		}
		if _, err := upstream.Write(op.bytes()); err != nil {
			return
		}
	}
}

// roleOutcomeLines are the lines that end the role lane's handling of one message.
var roleOutcomeLines = map[string]bool{
	"listener role forwarded":                               true,
	"listener role forward failed":                          true,
	"listener kept role message for the role's next holder": true,
	"listener role has no holder":                           true,
	"listener role holder is not live":                      true,
	"listener role dedupe skip":                             true,
	"listener skip role echo":                               true,
}

// eventLines returns the listener's JSON records that name eventID.
func eventLines(listener *listenerProcess, eventID string) []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(listener.Output.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["event_id"] != eventID {
			continue
		}
		out = append(out, record)
	}
	return out
}

// awaitRoleOutcome waits, up to a minute, for the listener to log how it ended eventID, and returns
// that line's msg (and error, if any).
func awaitRoleOutcome(t *testing.T, listener *listenerProcess, eventID string) string {
	t.Helper()
	outcome := ""
	ended := testwait.Holds(time.Minute, 20*time.Millisecond, func() bool {
		for _, record := range eventLines(listener, eventID) {
			if msg, _ := record["msg"].(string); roleOutcomeLines[msg] {
				outcome = msg
				if errText, ok := record["error"].(string); ok {
					outcome = msg + ": " + errText
				}
				return true
			}
		}
		return false
	})
	if !ended {
		t.Fatalf("the listener logged no outcome for %s in a minute; its lines for it: %v", eventID, eventLines(listener, eventID))
	}
	return outcome
}

// awaitOutput waits, up to a minute, for pattern to match the listener's output past offset.
func awaitOutput(t *testing.T, listener *listenerProcess, offset int, pattern *regexp.Regexp, what string) {
	t.Helper()
	if !testwait.Holds(time.Minute, 20*time.Millisecond, func() bool {
		output := listener.Output.String()
		return len(output) > offset && pattern.MatchString(output[offset:])
	}) {
		t.Fatalf("%s: not in the listener's output after a minute", what)
	}
}

// storedOnSubjects sums the messages the notification stream holds on the subjects filter matches
// whose name contains part.
func storedOnSubjects(t *testing.T, client *bus.Client, filter, part string) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: filter})
	if err != nil {
		t.Fatalf("read the stream's messages on %s: %v", filter, err)
	}
	var total uint64
	for subject, count := range info.State.Subjects {
		if strings.Contains(subject, part) {
			total += count
		}
	}
	return total
}

// A role message the listener's role lane took reaches its holder once, through the copy the
// stream stores on the holder's agent subject and the holder's session consumer relays, even when
// the listener's NATS connection fails under the forward of that one message and comes back.
//
// ROLE_LANE_FORCED_MODE picks the forced point at message forcedAt's forward: "" is the control,
// which passes the forward on; "drop" closes the listener's connection as it sends the forward;
// "stall" swallows the forward and everything after it on that connection until the listener calls
// the connection stale; "late" holds the forward past its window and then lets it reach the
// server. The messages are published one at a time, each after the listener logged how it ended
// the one before, and after a drop or a stall the next waits for the listener's reconnect, so
// every message after it is handled on the recovered connection.
//
// A listener that reports a message delivery_failed tells its sender the message is lost, so the
// test also fails on a delivery exception for a message the holder was handed.
//
// Delivery is counted once every message is settled: the listener logged it forwarded, or it
// logged the forward failed, kept no copy of it and holds no forward
// (envoy_role_forwards_outstanding 0), so nothing it took can still be stored. The stream's count
// on the holder's agent subject is then final, and the test waits for the relay to hand the holder
// that many messages, so no count is read before delivery ends.
func TestRoleLaneForcedPointDeliversEveryRoleMessage(t *testing.T) {
	mode := os.Getenv(forcedPointModeEnv)
	switch mode {
	case "", "drop", "stall", "late":
	default:
		t.Fatalf("%s=%q: want empty, drop, stall or late", forcedPointModeEnv, mode)
	}
	const (
		messages = 12
		forcedAt = 6
	)
	uri := testnats.URL(t)
	proxy := startForcedProxy(t, uri, mode)
	standIn := startBurstStandIn(t, uri, 8*messages)
	listener := startRoleLaneListeners(t, proxy.url, nil, "role-lane-forced")[0]
	role := "role-lane-forced-" + id.New()
	holderID := claimRoleThrough(t, listener, role)
	holders := connectPublisher(t, uri)
	liveFrames := receiveRoleForwards(t, holders.Conn, holderID)
	exceptions := make(chan string, 8*messages)
	exceptionSub, err := holders.Conn.Subscribe(bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+">", func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if json.Unmarshal(msg.Data, &item) == nil {
			exceptions <- item.SourceEventID
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the role lanes' delivery exceptions: %v", err)
	}
	t.Cleanup(func() { _ = exceptionSub.Unsubscribe() })
	if err := holders.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)
	agentSubject := contracts.LegacyAgentSubject(holderID)
	reconnected := regexp.MustCompile(`envoy nats reconnected name=envoy url=`)

	// A message is settled once the listener forwarded it, or gave it up: a failed forward with no
	// kept copy, while it holds no forward at all. A message given up is dropped: nothing the
	// listener holds can still bring it to the holder.
	settle := func(eventID string) (settled, dropped bool) {
		forwarded, kept, failed := false, false, false
		for _, record := range eventLines(listener, eventID) {
			switch record["msg"] {
			case "listener role forwarded":
				forwarded = true
			case "listener kept role message for the role's next holder":
				kept = true
			case "listener role forward failed":
				failed = true
			}
		}
		if forwarded {
			return true, false
		}
		given := failed && !kept && metricValue(t, listener, "envoy_role_forwards_outstanding") == 0
		return given, given
	}
	deadline := time.Now().Add(time.Minute)
	awaitSettled := func(eventID string) (dropped bool) {
		if !testwait.Holds(time.Until(deadline), 20*time.Millisecond, func() bool {
			var settled bool
			settled, dropped = settle(eventID)
			return settled
		}) {
			t.Fatalf("%s is not settled within a minute; the listener's lines for it: %v", eventID, eventLines(listener, eventID))
		}
		return dropped
	}

	published := make([]contracts.Envelope, 0, messages)
	for i := range messages {
		if i == forcedAt {
			proxy.arm(agentSubject)
		}
		before := len(listener.Output.String())
		item := publishRoleMessage(t, publisher, role, "forced point message "+strconv.Itoa(i))
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush role message %d: %v", i, err)
		}
		published = append(published, item)
		t.Logf("message %d %s: %s", i, item.EventID, awaitRoleOutcome(t, listener, item.EventID))
		if i == forcedAt && (mode == "drop" || mode == "stall") {
			awaitOutput(t, listener, before, reconnected, "the listener's main connection reconnected after the forced point")
			t.Logf("the listener's main connection reconnected after message %d", i)
		}
		if i == forcedAt {
			deadline = time.Now().Add(time.Minute)
			if awaitSettled(item.EventID) {
				t.Errorf("message %d %s: the listener gave it up - its forward failed, it kept no copy and holds no forward (envoy_role_forwards_outstanding 0)", i, item.EventID)
			}
		}
	}
	deadline = time.Now().Add(time.Minute)
	for _, item := range published {
		awaitSettled(item.EventID)
	}
	stored := storedOnSubjects(t, holders, agentSubject, holderID)
	keptLane := storedOnSubjects(t, holders, bus.KeptRoleSubject(">"), role)

	// The relay hands the holder each stored copy; wait until it has handed over as many as the
	// stream holds.
	relayed := map[string]int{}
	deadline = time.Now().Add(time.Minute)
	for uint64(len(relayed)) < stored {
		select {
		case frame := <-standIn.frames:
			if frame.sessionID == holderID {
				relayed[frame.eventID]++
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("the relay handed the holder %d of the %d copies the stream stores", len(relayed), stored)
		}
	}
	// The live frames are the core copies of the forwards' publishes, which arrive with the publish
	// and before its acknowledgement; they are counted for the report only.
	live := map[string]int{}
	liveDeadline := time.After(5 * time.Second)
collectLive:
	for len(live) < int(stored) {
		select {
		case frame := <-liveFrames:
			live[frame.item.EventID]++
		case <-liveDeadline:
			break collectLive
		}
	}
	for drained := false; !drained; {
		select {
		case frame := <-liveFrames:
			live[frame.item.EventID]++
		case frame := <-standIn.frames:
			if frame.sessionID == holderID {
				relayed[frame.eventID]++
			}
		default:
			drained = true
		}
	}
	excepted := map[string]bool{}
	for drained := false; !drained; {
		select {
		case eventID := <-exceptions:
			excepted[eventID] = true
		default:
			drained = true
		}
	}

	topic := contracts.RoleTopicPrefix + role
	onRole := func(record map[string]any) bool { return record["topic"] == topic }
	received := countRecords(listener, "listener received", onRole)
	forwarded := countRecords(listener, "listener role forwarded", onRole)
	failed := countRecords(listener, "listener role forward failed", onRole)
	kept := countRecords(listener, "listener kept role message for the role's next holder", onRole)
	t.Logf("mode %q, forced point reached %d time(s): published %d; listener received %d, forwarded %d, forward failed %d, kept %d; stream stores %d on the holder's agent subject and %d on the role's kept lane; relay delivered %d, live frames %d, delivery exceptions %d; envoy_role_forwards_outstanding %d",
		mode, proxy.firedCount(), messages, received, forwarded, failed, kept, stored, keptLane, len(relayed), len(live), len(excepted), metricValue(t, listener, "envoy_role_forwards_outstanding"))
	for i, item := range published {
		if relayed[item.EventID] == 1 && !excepted[item.EventID] {
			continue
		}
		var lines []string
		for _, record := range eventLines(listener, item.EventID) {
			lines = append(lines, fmt.Sprint(record["msg"], " ", record["error"]))
		}
		t.Errorf("message %d %s: relayed %d time(s), live %d, delivery exception %v; the listener's lines for it: %q", i, item.EventID, relayed[item.EventID], live[item.EventID], excepted[item.EventID], lines)
	}
	if proxy.firedCount() != 1 {
		t.Fatalf("the forced point was reached %d times, want 1: the proxy did not see the forward of message %d", proxy.firedCount(), forcedAt)
	}
	if received != messages || stored != messages || len(relayed) != messages {
		t.Fatalf("the holder was delivered %d of %d role messages (listener received %d, the stream stores %d)", len(relayed), messages, received, stored)
	}
}
