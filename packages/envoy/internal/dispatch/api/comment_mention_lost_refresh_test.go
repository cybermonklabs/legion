package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A claim refresh whose statement commits and whose answer is lost - the connection drops after
// the commit - leaves the parked send's claim unknown, never assumed: the sender reads the row
// before it acts on the send, records the claim the lost refresh wrote, and goes on writing the
// row. Trusting the claim it recorded before, it would name a claim the row no longer carries,
// no refresh would write the row again, and the claim would lapse with the send still parked
// (TestTheMentionSenderClaimModel finds what follows). The server, its statements and its
// database are the real ones; its connections run through lossyConns, which drops the one that
// carries the refresh's UPDATE once the server has committed it. The control row's answer arrives.
func TestARefreshWhoseAnswerIsLostAfterItCommitsLeavesTheClaimUnknownUntilRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose bool
	}{
		{name: "control: the refresh's answer arrives"},
		{name: "the connection drops after the refresh's statement commits", lose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			lossy := &lossyConns{}
			config := database.Pool.Config()
			config.ConnConfig.DialFunc = lossy.dial
			pool, err := pgxpool.NewWithConfig(context.Background(), config)
			if err != nil {
				t.Fatalf("open a pool through lossy connections: %v", err)
			}
			t.Cleanup(pool.Close)
			listener, _ := newMentionListener(t)
			handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
				store: &store.Store{Pool: store.NewPool(pool)}, envoyURL: listener.URL,
				envoyTimeout: 30 * time.Second, outboxPublishes: true,
			})
			served.mentions.wait()
			sender := newMentionSender(served.lifetime, time.Hour, served.sendCommentMention,
				served.refreshMentionClaims, served.rereadMentionClaims, served.releaseMentionClaims)
			timers := make(chan func(), 4)
			sender.mu.Lock()
			sender.after = func(_ time.Duration, f func()) { timers <- f }
			sender.mu.Unlock()
			served.mentions = sender

			// No outbox runs, so the issue's earlier events stay unsent and the comment's send
			// parks under the claim its first try took, its timer left to the test.
			issue := createInteractionIssue(t, handler, "TEST", "A refresh whose answer is lost", "before")
			comment := postMentionOfS1(t, handler, issue.Key, "the parked one")
			<-timers
			key := mentionKey{comment: comment, target: "session:s1", attempt: 1}
			recorded := func() time.Time {
				sender.mu.Lock()
				defer sender.mu.Unlock()
				return sender.held[key].claimed
			}
			parked := attemptClaimedAt(t, database, comment, "session:s1")
			if !recorded().Equal(parked) {
				t.Fatalf("the parked send's claim is recorded as %v, its row carries %v", recorded(), parked)
			}

			lossy.armed.Store(tc.lose)
			sender.refreshHeldClaims(context.Background())
			lossy.armed.Store(false)
			written := attemptClaimedAt(t, database, comment, "session:s1")
			t.Logf("parked under %v; the refresh wrote %v, and the sender records %v; connections dropped: %d",
				parked, written, recorded(), lossy.dropped.Load())
			if !written.After(parked) {
				t.Fatalf("the refresh's statement did not commit: the row's claim is still %v", parked)
			}
			if want := int32(map[bool]int{false: 0, true: 1}[tc.lose]); lossy.dropped.Load() != want {
				t.Fatalf("%d connections dropped after the refresh's statement, want %d", lossy.dropped.Load(), want)
			}

			sender.refreshHeldClaims(context.Background())
			again := attemptClaimedAt(t, database, comment, "session:s1")
			t.Logf("the next refresh: the row carries %v, and the sender records %v", again, recorded())
			if !again.After(written) {
				t.Fatalf("the next refresh left the row's claim at %v, the one the refresh whose answer was lost wrote: the sender named its claim from before, %v, which the row no longer carries, so no refresh writes the row again and the claim lapses with the send still parked",
					written, parked)
			}
			if !recorded().Equal(again) {
				t.Fatalf("the sender records the parked send's claim as %v, the row carries %v", recorded(), again)
			}
		})
	}
}

// lossyConns dials the database through connections that, once armed, let the next refresh
// statement of the mention sender (refreshMentionClaims, named by its text) commit, and then drop
// its connection before any of its answer reaches the client: what a network failure after the
// commit leaves the statement's caller with. Every other statement, of the server's background
// loops among them, passes through untouched.
type lossyConns struct {
	armed   atomic.Bool
	dropped atomic.Int32
}

// lossyRefreshSQL is the text that names the refresh's statement as the client sends it: the
// sender's other writes of comment_deliveries name the table with no alias.
var lossyRefreshSQL = []byte("update comment_deliveries d set claimed_at = now()")

func (l *lossyConns) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &lossyConn{Conn: conn, lossy: l}, nil
}

// lossyConn hands the server's bytes on as they come until the client writes the refresh's
// statement while its lossyConns is armed. From then on it reads the server's side message by
// message (a type byte and a four-byte length, which every message after the start-up has) and
// holds each back until the server's ReadyForQuery: one that follows an UPDATE's CommandComplete
// is sent once the statement has committed, and it then closes the connection with none of the
// held answer read; one that does not - the round trip that prepares the statement - is handed
// on, and it waits for the execution's.
type lossyConn struct {
	net.Conn
	lossy                     *lossyConns
	dropping, answersAnUpdate bool
	in, out, held             []byte
}

var errLostAfterCommit = errors.New("the connection dropped after the statement committed")

func (c *lossyConn) Write(p []byte) (int, error) {
	if !c.dropping && bytes.Contains(p, lossyRefreshSQL) && c.lossy.armed.CompareAndSwap(true, false) {
		c.dropping = true
	}
	return c.Conn.Write(p)
}

func (c *lossyConn) Read(p []byte) (int, error) {
	if !c.dropping && len(c.out) == 0 {
		return c.Conn.Read(p)
	}
	for len(c.out) == 0 {
		buffer := make([]byte, 32<<10)
		n, err := c.Conn.Read(buffer)
		c.in = append(c.in, buffer[:n]...)
		if c.split() {
			c.lossy.dropped.Add(1)
			_ = c.Conn.Close()
			return 0, errLostAfterCommit
		}
		if err != nil && len(c.out) == 0 {
			return 0, err
		}
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	return n, nil
}

// split moves every whole message read so far on or into held, and answers whether the
// connection drops now.
func (c *lossyConn) split() bool {
	for len(c.in) >= 5 {
		size := 1 + int(binary.BigEndian.Uint32(c.in[1:5]))
		if len(c.in) < size {
			return false
		}
		message := c.in[:size:size]
		c.in = c.in[size:]
		c.held = append(c.held, message...)
		switch message[0] {
		case 'C': // CommandComplete, its tag naming the command
			c.answersAnUpdate = c.answersAnUpdate || bytes.HasPrefix(message[5:], []byte("UPDATE"))
		case 'Z': // ReadyForQuery: the statement has committed
			if c.answersAnUpdate {
				return true
			}
			c.out, c.held = append(c.out, c.held...), nil
		}
	}
	return false
}
