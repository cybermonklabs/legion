package natscallout

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The storms of this package (storm_test.go) run the hub and their clients in a process of their
// own, this test binary run again (stormPeerEnv), and keep the auth service in the test's own
// process. The three are what a deployment separates: the hub holds two CPUs of its own, each
// broker replica 200m of CPU with a 500m limit, and the sessions are elsewhere entirely. Run in
// one process, as these storms were until dispatch://INFRA-3904, a storm measures that process
// rather than the service: the clients' signatures, the hub's work and the service's decisions
// run on one scheduler and stop together at the same garbage collection, and under the race
// detector every one of them costs several times what it costs deployed, the clients and the hub
// included. In a process of their own the clients and the hub take their own scheduler and their
// own collections, and the operating system shares the machine's CPUs between the three as it
// shares them between any three programs.
//
// What that cost, measured at main ca20a512fc on 2026-11-26: with the hub, the service and the
// clients in one process under the race detector on two CPUs, the restart storm failed 9 of 60
// runs and the waiting-bound storm 3 of 60. The same storm with the three roles in processes of
// their own, each at the CPU its deployment holds, passed 20 of 20 runs at 500 and at 1,400
// connections, the service answering each request 0.10 to 0.42 s after it reached it against the
// hub's two-second authorization timeout; with the service held to 100m of CPU, a fifth of its
// deployed limit, the hub gave up on 73 requests of one run, which is the control that such a run
// can fail at all.
//
// The service stays in the test's own process, so every assertion about it still reads it here -
// the nonces it decided, the log it wrote, the slowest decision it made - and the race detector
// still watches the code under test.

// stormPeerEnv names the file descriptor the peer writes its answers on, and marks a run of this
// binary as a peer rather than a test run. Answers go there rather than to the peer's standard
// output, which carries whatever its test and the libraries it runs write.
const stormPeerEnv = "ENVOY_STORM_PEER_ANSWERS"

// peerCommand is one thing a test asks its peer to do.
type peerCommand struct {
	// Do is the command: start, connect, restart, await, report or quit.
	Do string `json:"do"`
	// Sessions is how many sessions start makes, and Debug whether its hub logs at debug level,
	// the level the hub's give-up line is written at (hubGaveUp).
	Sessions int  `json:"sessions,omitempty"`
	Debug    bool `json:"debug,omitempty"`
	// Mode is how connect connects: reconnecting, as a bus client does, one connect at a time in
	// waves of connectingAtOnce; once, one connect each with no reconnect, all at once; or
	// nocredential, one connect each presenting nothing at all, all at once.
	Mode string `json:"mode,omitempty"`
	// Count is how many connections connect makes, the sessions it has when it is zero.
	Count int `json:"count,omitempty"`
	// GiveUpOnRefusal leaves nats.IgnoreAuthErrorAbort off the connections connect makes, which
	// the restart storm's control needs: such a client closes for good on its second refusal.
	// Retry adds nats.RetryOnFailedConnect, so a client the hub refuses at its timeout keeps the
	// connection and tries again, which the late-admission control needs.
	GiveUpOnRefusal bool `json:"give_up_on_refusal,omitempty"`
	Retry           bool `json:"retry,omitempty"`
	// Until is what await waits for, connected or closed, and Bound how long it waits.
	Until string        `json:"until,omitempty"`
	Bound time.Duration `json:"bound,omitempty"`
}

// peerAnswer is what the peer answers a command with. Error is its failure, and every other field
// is empty then.
type peerAnswer struct {
	Error string `json:"error,omitempty"`
	// URL is the hub's client URL and Keys the three keys the hub and the service share, both
	// from start; Answers is what the broker knows of each session's token, by token, which the
	// test's service introspects from (peerIntrospection).
	URL     string                             `json:"url,omitempty"`
	Keys    testnats.CalloutKeys               `json:"keys,omitempty"`
	Answers map[string]oauthwire.Introspection `json:"answers,omitempty"`
	// Kinds is what became of each connect, in the order connect made them: admitted, refused
	// (the hub's authorization violation, its own refusal and the service's alike) or failed.
	// Errors is each one's error text, empty where it was admitted, and Took how long it took.
	Kinds  []string        `json:"kinds,omitempty"`
	Errors []string        `json:"errors,omitempty"`
	Took   []time.Duration `json:"took,omitempty"`
	// Connected and Closed are how many of the clients hold a connection and how many have closed
	// for good; Reconnects how many reconnects they have made in all.
	Connected  int   `json:"connected,omitempty"`
	Closed     int   `json:"closed,omitempty"`
	Reconnects int64 `json:"reconnects,omitempty"`
	// Log is the hub's whole log, and Logged its length in bytes, which restart answers so a test
	// reads the log from the restart on.
	Log    string `json:"log,omitempty"`
	Logged int    `json:"logged,omitempty"`
	// Last is the nonce each client signed last, by session id, and Signed when each nonce was
	// signed, by nonce (lastNonces).
	Last   map[string]string    `json:"last,omitempty"`
	Signed map[string]time.Time `json:"signed,omitempty"`
}

// stormPeer is a peer process and the pipe its answers come back on.
type stormPeer struct {
	t       *testing.T
	command *exec.Cmd
	in      io.WriteCloser
	answers *bufio.Reader
	// output is whatever the peer wrote to its standard output and error, which a failure here
	// names. It is a locked buffer: the peer's copier goroutine writes it while this one reads.
	output *logtest.Buffer
	mu     sync.Mutex
}

// startStormPeer starts a peer: this test binary run again. It is killed when the test ends.
func startStormPeer(t *testing.T) *stormPeer {
	t.Helper()
	read, write, err := os.Pipe()
	require.NoError(t, err, "the pipe the peer answers on")
	// -test.run names the peer's own test, which every other run of this binary skips, and
	// -test.timeout 0 leaves the peer no deadline of its own: its test bounds every wait it makes.
	command := exec.Command(os.Args[0], "-test.run=^TestTheStormsPeerServesItsTest$", "-test.count=1", "-test.timeout=0")
	command.Env = append(os.Environ(), stormPeerEnv+"=3")
	command.ExtraFiles = []*os.File{write}
	in, err := command.StdinPipe()
	require.NoError(t, err)
	output := &logtest.Buffer{}
	command.Stdout, command.Stderr = output, output
	require.NoError(t, command.Start(), "the storm's peer did not start")
	require.NoError(t, write.Close())
	peer := &stormPeer{t: t, command: command, in: in, answers: bufio.NewReaderSize(read, 1<<20), output: output}
	t.Cleanup(func() {
		_ = in.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		_ = read.Close()
	})
	return peer
}

// peerBound is how long a test waits for one answer of its peer, beside the command's own wait: a
// peer that does not answer fails the test here, naming the command and what the peer wrote,
// rather than at the test binary's own deadline.
const peerBound = 2 * time.Minute

// do runs one command and answers what the peer answered, failing the test when the peer failed,
// did not answer or ended.
func (p *stormPeer) do(command peerCommand) peerAnswer {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	encoded, err := json.Marshal(command)
	require.NoError(p.t, err)
	_, err = p.in.Write(append(encoded, '\n'))
	require.NoError(p.t, err, "the peer did not take the command %s; what it wrote:\n%s", command.Do, p.output.String())
	type read struct {
		line []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := p.answers.ReadBytes('\n')
		done <- read{line, err}
	}()
	bound := command.Bound + peerBound
	select {
	case answer := <-done:
		require.NoError(p.t, answer.err, "the peer's answer to %s; what it wrote:\n%s", command.Do, p.output.String())
		var decoded peerAnswer
		require.NoError(p.t, json.Unmarshal(answer.line, &decoded), "the peer's answer to %s: %s", command.Do, answer.line)
		require.Empty(p.t, decoded.Error, "the peer failed the command %s", command.Do)
		return decoded
	case <-time.After(bound):
		p.t.Fatalf("the peer did not answer %s within %s; what it wrote:\n%s", command.Do, bound, p.output.String())
		return peerAnswer{}
	}
}

// awaitConnected waits until the peer reports every one of count clients connected, and fails the
// test when one closed for good or the bound passed first.
func (p *stormPeer) awaitConnected(count int, bound time.Duration) peerAnswer {
	p.t.Helper()
	answer := p.do(peerCommand{Do: "await", Until: "connected", Bound: bound})
	require.Zero(p.t, answer.Closed, "%d connections closed for good while the service was away", answer.Closed)
	require.Equal(p.t, count, answer.Connected,
		"only %d of %d connections were admitted again within %s", answer.Connected, count, bound)
	return answer
}

// peerIntrospection answers what the broker knows of token, from what the peer's sessions carry:
// a token no session of the peer holds is not live, as the broker answers one it never issued.
// wait is what the answer takes, as a broker's database read does, and it gives up with ctx.
func peerIntrospection(answers map[string]oauthwire.Introspection, wait time.Duration) func(context.Context, string) (oauthwire.Introspection, error) {
	return func(ctx context.Context, token string) (oauthwire.Introspection, error) {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return oauthwire.Introspection{}, ctx.Err()
			case <-time.After(wait):
			}
		}
		return answers[token], nil
	}
}

// TestTheStormsPeerServesItsTest is the peer itself: with stormPeerEnv set it serves its test's
// commands until the pipe closes, and with it unset, which is every other run of this binary, it
// is skipped. It is a test so that it holds the fixtures a test holds: the hub
// (testnats.StartWithCalloutServer, with its restart and its log) and the sessions its clients
// present (sessionsNamed, reconnectingSession).
func TestTheStormsPeerServesItsTest(t *testing.T) {
	descriptor := os.Getenv(stormPeerEnv)
	if descriptor == "" {
		t.Skip("not a peer run: each storm starts its own peer (stormpeer_test.go)")
	}
	number, err := strconv.Atoi(descriptor)
	require.NoError(t, err, "%s names no file descriptor", stormPeerEnv)
	answers := os.NewFile(uintptr(number), "answers")
	require.NotNil(t, answers, "no file descriptor %d", number)
	peer := &stormPeerState{t: t}
	commands := bufio.NewReaderSize(os.Stdin, 1<<16)
	for {
		line, err := commands.ReadBytes('\n')
		if len(line) == 0 && errors.Is(err, io.EOF) {
			return
		}
		require.NoError(t, err, "reading a command")
		var command peerCommand
		require.NoError(t, json.Unmarshal(line, &command), "a command: %s", line)
		if command.Do == "quit" {
			return
		}
		encoded, err := json.Marshal(peer.run(command))
		require.NoError(t, err)
		_, err = answers.Write(append(encoded, '\n'))
		require.NoError(t, err, "answering %s", command.Do)
	}
}

// stormPeerState is the peer's hub and clients between commands.
type stormPeerState struct {
	t          *testing.T
	keys       testnats.CalloutKeys
	server     *testnats.Server
	url        string
	debug      bool
	sessions   []*session
	conns      []*nats.Conn
	nonces     *lastNonces
	reconnects atomic.Int64
}

// run is one command, answered with what it found.
func (s *stormPeerState) run(command peerCommand) peerAnswer {
	switch command.Do {
	case "start":
		return s.start(command)
	case "connect":
		return s.connect(command)
	case "restart":
		return s.restart()
	case "await":
		return s.await(command)
	case "report":
		return s.report()
	default:
		return peerAnswer{Error: "unknown command " + command.Do}
	}
}

// start runs the hub and makes the sessions, and answers the hub's URL, the keys the service
// needs and what the broker knows of each session's token.
func (s *stormPeerState) start(command peerCommand) peerAnswer {
	s.keys = testnats.NewCalloutKeys(s.t)
	s.debug = command.Debug
	if command.Debug {
		s.server, s.url = startDebugCalloutServer(s.t, s.keys)
	} else {
		s.server, s.url = testnats.StartWithCalloutServer(s.t, s.keys)
	}
	s.sessions = sessionsNamed(s.t, "ses_storm", command.Sessions)
	s.nonces = newLastNonces()
	answers := make(map[string]oauthwire.Introspection, len(s.sessions))
	for _, session := range s.sessions {
		answers[session.token] = session.answer
	}
	return peerAnswer{URL: s.url, Keys: s.keys, Answers: answers}
}

// connectingAtOnce is how many of the restart storm's first connections are made at once: they
// set the storm up and are not the storm.
const connectingAtOnce = 20

// connect connects every session the command asks for, and answers what became of each.
func (s *stormPeerState) connect(command peerCommand) peerAnswer {
	count := command.Count
	if count == 0 {
		count = len(s.sessions)
	}
	if count > len(s.sessions) && command.Mode != "nocredential" {
		return peerAnswer{Error: "the peer holds " + strconv.Itoa(len(s.sessions)) + " sessions, not " + strconv.Itoa(count)}
	}
	s.conns = make([]*nats.Conn, count)
	errs := make([]error, count)
	took := make([]time.Duration, count)
	gate := make(chan struct{}, connectingAtOnce)
	var wait sync.WaitGroup
	for index := range count {
		wait.Go(func() {
			if command.Mode == "reconnecting" {
				gate <- struct{}{}
				defer func() { <-gate }()
			}
			began := time.Now()
			s.conns[index], errs[index] = s.connectOne(index, command)
			took[index] = time.Since(began)
			if errs[index] == nil && command.Mode != "reconnecting" {
				s.conns[index].Close()
			}
		})
	}
	wait.Wait()
	kinds := make([]string, count)
	texts := make([]string, count)
	for index, err := range errs {
		switch {
		case err == nil:
			kinds[index] = "admitted"
		case errors.Is(err, nats.ErrAuthorization):
			kinds[index] = "refused"
		default:
			kinds[index] = "failed"
		}
		if err != nil {
			texts[index] = err.Error()
		}
	}
	return peerAnswer{Kinds: kinds, Errors: texts, Took: took}
}

// connectOne is one connection of the mode the command names.
func (s *stormPeerState) connectOne(index int, command peerCommand) (*nats.Conn, error) {
	switch command.Mode {
	case "reconnecting":
		options := []nats.Option{nats.ReconnectHandler(func(*nats.Conn) { s.reconnects.Add(1) })}
		if command.Retry {
			options = append(options, nats.RetryOnFailedConnect(true))
		}
		session := s.sessions[index]
		return reconnectingSession(s.t, s.url, session, command.GiveUpOnRefusal, s.nonces.signer(session.id), options...)
	case "nocredential":
		return nats.Connect(s.url, nats.NoReconnect(), nats.Timeout(10*time.Second))
	default:
		return connectSession(s.t, s.url, s.sessions[index], nil)
	}
}

// restart restarts the hub on the same configuration and URL, as an operator's restart does, and
// answers how long its log was when it did, so its test reads the log from there on.
func (s *stormPeerState) restart() peerAnswer {
	logged := len(s.server.Log())
	config := testnats.CalloutConfig(s.keys)
	if s.debug {
		config = debugCalloutConfig(s.keys)
	}
	s.server.Restart(s.t, config)
	return peerAnswer{Logged: logged}
}

// await waits until every client holds a connection, or until every client has closed for good,
// and answers how many of each there are then. It answers rather than failing: its test reads the
// counts and says what they mean.
func (s *stormPeerState) await(command peerCommand) peerAnswer {
	end := time.Now().Add(command.Bound)
	for {
		connected, closed := s.counts()
		reached := (command.Until == "connected" && connected == len(s.conns)) ||
			(command.Until == "closed" && closed == len(s.conns))
		if reached || time.Now().After(end) {
			return peerAnswer{Connected: connected, Closed: closed, Reconnects: s.reconnects.Load()}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// counts is how many of the clients hold a connection and how many have closed for good.
func (s *stormPeerState) counts() (connected, closed int) {
	for _, conn := range s.conns {
		if conn == nil {
			continue
		}
		if conn.IsConnected() {
			connected++
		}
		if conn.IsClosed() {
			closed++
		}
	}
	return connected, closed
}

// report is the hub's whole log and every nonce its clients signed.
func (s *stormPeerState) report() peerAnswer {
	connected, closed := s.counts()
	last, signed := s.nonces.read()
	return peerAnswer{Log: s.server.Log(), Connected: connected, Closed: closed,
		Reconnects: s.reconnects.Load(), Last: last, Signed: signed}
}
