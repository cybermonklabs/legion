package writerhold

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// probe dials url with the hold probe's credential and answers the dial's error.
func (h *heldStack) probe(url string) error {
	h.t.Helper()
	return h.op.Probe(context.Background(), url)
}

// The probe: outside a hold it connects, which shows its credential and its non-empty grant are
// valid, so a refusal in a hold cannot come from a bad credential. With the callout's hold rule
// off (a broker whose BROKER_NATS_HOLD_READ is false), it connects in a hold too: that is the
// control, the hub the rule protects without it. With the rule, it is refused on the hub and on
// each server directly, before and after the hold's restart; and a row the auth service cannot read
// refuses it too. It fails when the auth service admits the probe under a held row.
func TestHoldTheProbe(t *testing.T) {
	t.Run("the control: with the hold rule off, the probe connects in a hold", func(t *testing.T) {
		h := newHeldStackWith(t, holdtest.Options{NoHoldRead: true})
		require.NoError(t, h.probe(h.s.Cluster.URL()), "the probe was refused outside a hold")
		id := h.prepared(holdrecord.WorkflowBucketsCreate)
		h.heldWithoutTheSteps(id)
		require.NoError(t, h.probe(h.s.Cluster.URL()), "with the hold rule off the probe was refused in a hold")
	})

	h := newHeldStack(t)
	require.NoError(t, h.probe(h.s.Cluster.URL()), "the probe was refused outside a hold, so its credential or grant is bad")
	h.seedProtectedStreams()
	code, _, _ := h.run(actionSet, "--workflow", string(holdrecord.WorkflowBucketsCreate), "--reason", "the row's hold")
	require.Zero(t, code, "hold set, whose step 2 and step 4 require the probe refused on the hub and on each restarted server")
	require.ErrorIs(t, h.probe(h.s.Cluster.URL()), nats.ErrAuthorization, "the probe was admitted to the hub under a held row")
	for i := range h.s.Cluster.Servers {
		url, stop, err := h.op.Hub.ClientURL(context.Background(), holdtest.Pod(i))
		require.NoError(t, err)
		require.ErrorIs(t, h.probe(url), nats.ErrAuthorization, "the probe was admitted to %s's own client port", holdtest.Pod(i))
		stop()
	}

	_, err := h.s.Broker.Pool.Exec(context.Background(), `delete from nats_hold`)
	require.NoError(t, err, "the test's admin deletes the row")
	require.ErrorIs(t, h.probe(h.s.Cluster.URL()), nats.ErrAuthorization, "the probe was admitted while the row could not be read")
}

// A held connection is closed at its user's expiry, and its reconnect after the release is decided
// again, under released: Operator(OperatorPhaseReleased) alone. The control is the same connection
// before its expiry, still answered on a held-only subject: so it is the expiry and the new decision
// that take the held grant away, not anything else. It fails when the auth service answers the
// operator with a user that outlives its token, or keeps the held grant across a decision under
// released.
func TestHoldAHeldConnectionExpires(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	id := h.prepared(holdrecord.WorkflowBucketsCreate)
	h.heldWithoutTheSteps(id)
	// The stack's tokens are issued 5 s before their reading, so a life of 10 s ends 5 s after the
	// decision.
	h.s.SetTokenLife(10 * time.Second)

	disconnected := make(chan error, 4)
	nc, err := nats.Connect(h.s.Cluster.URL(), append(h.s.OperatorCredential(), nats.MaxReconnects(-1),
		nats.ReconnectWait(200*time.Millisecond), nats.Timeout(10*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { disconnected <- err }))...)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	names := func() error {
		_, err := nc.Request("$JS.API.STREAM.NAMES", []byte(`{}`), 5*time.Second)
		return err
	}
	require.NoError(t, names(), "the control: the held connection is answered on STREAM.NAMES, a held-only subject")
	// The held connection's user is decided; its reconnect is decided again with a token of the
	// ordinary life.
	h.s.SetTokenLife(50 * time.Minute)

	code, _, _ := h.run(actionRelease, "--hold", id, "--reason", "release before the expiry")
	require.Zero(t, code)
	select {
	case err := <-disconnected:
		t.Logf("the server closed the held connection at its expiry: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the held connection was not closed within 60 s of its user's 5 s life")
	}
	testwait.Eventually(t, "the operator's reconnect after the release", nc.IsConnected)
	require.Error(t, names(), "the reconnect after the release kept the held grant")
	_, err = nc.Request("$JS.API.STREAM.INFO."+bus.Stream, nil, 5*time.Second)
	require.NoError(t, err, "the reconnect after the release lost released's own reads")
}

// spyConn is a client connection that records, from what its client writes, the subject and the
// reply subject of every publish (PUB and HPUB): the operator's requests, as the server reads them.
type spyConn struct {
	net.Conn
	mu      *sync.Mutex
	pending *bytes.Buffer
	seen    *[][2]string
}

func (c spyConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.pending.Write(p)
	for {
		line, err := c.pending.ReadString('\n')
		if err != nil {
			// A part of a line: keep it for the rest.
			rest := line
			c.pending.Reset()
			c.pending.WriteString(rest)
			break
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 4 && fields[0] == "PUB":
			*c.seen = append(*c.seen, [2]string{fields[1], fields[2]})
		case len(fields) == 5 && fields[0] == "HPUB":
			*c.seen = append(*c.seen, [2]string{fields[1], fields[2]})
		}
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

// spyDialer dials as nats.go does, through spyConn.
type spyDialer struct {
	mu   sync.Mutex
	seen [][2]string
}

func (d *spyDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial(network, address)
	if err != nil {
		return nil, err
	}
	return spyConn{Conn: conn, mu: &d.mu, pending: &bytes.Buffer{}, seen: &d.seen}, nil
}

func (d *spyDialer) requests() [][2]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][2]string(nil), d.seen...)
}

// The reply subject: a spy on the operator's own connection sees every request `hold set` sends
// carry a reply under _INBOX_envoy_operator., and the spy is seen to see them (more than a
// handful of requests). A raw client holding the operator's grant can still aim a JetStream answer
// at $KV.envoy_sessions.x, which the server stores: the stated residual, recorded here so a change to
// it is seen. It fails when an operator request asks for its answer anywhere else.
func TestHoldTheReplySubject(t *testing.T) {
	h := newHeldStack(t)
	h.seedProtectedStreams()
	spy := &spyDialer{}
	connect := h.op.Connect
	h.op.Connect = func(context.Context) (*nats.Conn, error) {
		return nats.Connect(h.s.Cluster.URL(), append(h.s.OperatorCredential(), nats.Name("envoy-infra hold"),
			nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond), nats.Timeout(10*time.Second),
			nats.SetCustomDialer(spy))...)
	}
	code, _, _ := h.run(actionSet, "--workflow", string(holdrecord.WorkflowBucketsCreate), "--reason", "the row's hold")
	require.Zero(t, code)
	h.op.Connect = connect

	requests := 0
	for _, pub := range spy.requests() {
		if pub[1] == "" {
			continue
		}
		requests++
		require.True(t, strings.HasPrefix(pub[1], natsgrant.OperatorInbox+"."), "the request on %s asked for its answer on %s", pub[0], pub[1])
	}
	require.Greater(t, requests, 10, "the spy saw %d requests: it is blind to the hold's", requests)
	t.Logf("the spy saw %d requests, each with its reply under %s.", requests, natsgrant.OperatorInbox)

	// The residual: under the held grant, a raw client aims $JS.API.INFO's answer at a KV key, and
	// the bucket's stream stores it: its last_seq moves by one.
	nc, err := h.op.Connect(context.Background())
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	lastSeq := func() uint64 {
		stream, err := js.Stream(context.Background(), kvStreamPrefix+bus.SessionBucket)
		require.NoError(t, err)
		return stream.CachedInfo().State.LastSeq
	}
	before := lastSeq()
	require.NoError(t, nc.PublishRequest("$JS.API.INFO", "$KV."+bus.SessionBucket+".x", nil))
	require.NoError(t, nc.Flush())
	testwait.Eventually(t, "the JetStream answer stored at $KV.envoy_sessions.x", func() bool { return lastSeq() == before+1 })
}
