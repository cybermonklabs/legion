package natscallout

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// hubRefusals is the subject a callout server publishes the auth error event of each connection it
// refuses on, in the callout's account (nats-server server/events.go:65, authErrorAccountEventSubj,
// sent for a connection the callout decides at server/auth.go:784).
const hubRefusals = "$SYS.ACCOUNT.CLIENT.AUTH.ERR"

// hubAnswers is the subjects the service's answers are published on: the inbox a server makes for
// each request it sends (server/events.go:3004-3007, newRespInbox), which is the service's own
// publish grant (testnats.CalloutConfig).
const hubAnswers = "$SYS._INBOX.>"

// hubRecordBuffer is how many requests, refusals and answers a hubRecord holds unread, of each: far
// more than those of the largest storm here, a few thousand.
const hubRecordBuffer = 1 << 16

// hubRecord reads a callout server's own record of what became of the authorization requests it
// sends, as a static user of the server's configuration (newHubRecordUser), which the server asks
// no service about. It reads three things: each request, which names the connection it is for (its
// client ID, the cid of the server's log), the nonce the server sent that connection and the inbox
// the answer goes to; each answer the service publishes to such an inbox, which the server reads
// whether or not it still waits for it; and, for each connection the server refuses, an auth error
// event naming the connection and the callout's reason.
//
// The reason is what tells the server's give-up from every other refusal, and the server's log
// cannot: it logs its give-up (hubGaveUp) naming no connection, and every refusal with the same
// "authentication error" line. The callout ends with no reason only when its own timer ended its
// wait for the answer (server/auth_callout.go:457-466); every other way out of it names one, an
// answer that refuses, one the server cannot open or will not take, or a request it could not send,
// and the server sends that string as the event's reason (server/auth.go:762-785). One case reads
// the same: the server reads an answer it refuses in the instant its timer fires, and its wait
// wakes to both and takes the timer.
//
// A timer that ends the wait does not say whether an answer was coming: the server gave up on an
// answer that reached it late and on one that never came alike. So the record counts a give-up
// only where the service's answer to that request reached the server (gaveUpOn).
type hubRecord struct {
	url      string
	public   string
	xkey     nkeys.KeyPair
	conn     *nats.Conn
	requests *nats.Subscription
	refusals *nats.Subscription
	answers  *nats.Subscription
	sent     chan *nats.Msg
	refused  chan *nats.Msg
	answered chan *nats.Msg
	marks    int
}

// newHubRecordUser is the static user a hubRecord reads as, and its seed: an nkey user the server's
// configuration lists, which may read the requests the server sends, the service's answers and the
// server's auth error events.
func newHubRecordUser(t *testing.T) (testnats.StaticUser, string) {
	t.Helper()
	seed, public := testnats.User(t)
	return testnats.StaticUser{NKey: public, Publish: []string{"_INBOX.>"}, Subscribe: []string{authSubject, hubRefusals, hubAnswers}}, seed
}

// hubRecordStep is how long one connect or flush of the record may take: what is left of the test
// binary's deadline, less the time its failure takes to report, so that the deadline is its only
// bound (an hour under -timeout 0).
func hubRecordStep(t *testing.T) time.Duration {
	return testwait.Bounded(t, time.Hour, 30*time.Second)
}

// recordHub reads the record of the server at url as the static user whose seed is seed, from the
// moment it returns, across a restart of the server: it reconnects as a bus client does, and sends
// its subscriptions again (awaitResubscribed). keys' curve key opens the requests the server seals
// to the service.
func recordHub(t *testing.T, url, seed string, keys testnats.CalloutKeys) *hubRecord {
	t.Helper()
	user, err := nkeys.FromSeed([]byte(seed))
	require.NoError(t, err)
	public, err := user.PublicKey()
	require.NoError(t, err)
	xkey, err := nkeys.FromSeed([]byte(keys.XKeySeed))
	require.NoError(t, err)
	r := &hubRecord{url: url, public: public, xkey: xkey, sent: make(chan *nats.Msg, hubRecordBuffer),
		refused: make(chan *nats.Msg, hubRecordBuffer), answered: make(chan *nats.Msg, hubRecordBuffer)}
	r.conn, err = nats.Connect(url, nats.Nkey(public, user.Sign), nats.MaxReconnects(-1),
		nats.ReconnectWait(100*time.Millisecond), nats.Timeout(hubRecordStep(t)))
	require.NoError(t, err, "the record's user is a static auth user, admitted without a callout")
	t.Cleanup(r.conn.Close)
	r.requests, err = r.conn.ChanSubscribe(authSubject, r.sent)
	require.NoError(t, err)
	r.refusals, err = r.conn.ChanSubscribe(hubRefusals, r.refused)
	require.NoError(t, err)
	r.answers, err = r.conn.ChanSubscribe(hubAnswers, r.answered)
	require.NoError(t, err)
	require.NoError(t, r.conn.FlushTimeout(hubRecordStep(t)))
	return r
}

// awaitResubscribed waits until the record has reconnected to a restarted server and its
// subscriptions have reached it: nats.go sends them again as it reconnects, before anything the
// flush queues behind them.
func (r *hubRecord) awaitResubscribed(t *testing.T) {
	t.Helper()
	testwait.UntilPolling(t, 50*time.Millisecond, "the server's record read again after the restart", func() (uint64, bool) {
		reconnects := r.conn.Stats().Reconnects
		return reconnects, reconnects > 0 && r.conn.FlushTimeout(time.Second) == nil
	})
}

// hubConnection is one connection of one server: a server numbers its connections from one, and a
// restarted server, which comes back under a new ID, numbers them afresh.
type hubConnection struct {
	server string
	id     uint64
}

// hubVerdicts is what the server's record says became of its requests: the connection the server
// sent the request on each nonce for and the inbox its answer goes to, the inboxes the service
// published an answer to, and the reason the server refused each connection it refused with.
type hubVerdicts struct {
	connection map[string]hubConnection
	inbox      map[string]string
	answered   map[string]bool
	refused    map[hubConnection]string
}

// read reads the server's record once the replica that answered its requests is gone: it closes
// that replica (closeAnswerer), whose connection server lists under the name answerer, and waits
// until server lists no connection under that name any more.
//
// That is the fact the answers wait for. The server reads a connection's messages in order and
// passes each answer on to the record as it reads it, and it closes the connection only once it has
// read what the replica wrote before its close, which the replica's Close flushes. So once the
// connection is gone every answer the replica published is in the record's connection, ahead of
// the reply to a flush the record sends after that. An answer the replica never published is never
// there, and the read says so at once, rather than at the test binary's deadline.
//
// The requests and the refusals wait for a refusal of the record's own making: it connects as the
// record's own user signing with another key, under a name of its own, which the server refuses by
// itself, asking no service, and publishes an event for like any other. The server queues the
// requests and the events on its one queue of internal messages, which one goroutine sends in order
// (server/events.go:713-751, internalSendLoop :499-673), so once that event has arrived every
// request and every refusal the server queued before it has arrived too.
func (r *hubRecord) read(t *testing.T, server monitoredHub, answerer string, closeAnswerer func()) hubVerdicts {
	t.Helper()
	require.Positive(t, connectionsNamed(t, server, answerer),
		"the server lists no connection named %q, so the wait for its close would see nothing", answerer)
	closeAnswerer()
	testwait.Until(t, "the server to close its connection named "+answerer, func() (int, bool) {
		open := connectionsNamed(t, server, answerer)
		return open, open == 0
	})
	require.NoError(t, r.conn.FlushTimeout(hubRecordStep(t)), "the record's flush behind the answers")

	r.marks++
	mark := fmt.Sprintf("hub record mark %d", r.marks)
	stranger, err := nkeys.CreateUser()
	require.NoError(t, err)
	refused, err := nats.Connect(r.url, nats.Nkey(r.public, stranger.Sign), nats.Name(mark), nats.NoReconnect(),
		nats.Timeout(hubRecordStep(t)))
	if err == nil {
		refused.Close()
	}
	require.ErrorIs(t, err, nats.ErrAuthorization, "the server did not refuse the record's user signing with another key")

	verdicts := hubVerdicts{connection: map[string]hubConnection{}, inbox: map[string]string{},
		answered: map[string]bool{}, refused: map[hubConnection]string{}}
	testwait.Until(t, "the server's auth error event for "+mark, func() (int, bool) {
		for {
			select {
			case msg := <-r.refused:
				var event natsserver.DisconnectEventMsg
				require.NoError(t, json.Unmarshal(msg.Data, &event), "an auth error event the server published")
				if event.Client.Name == mark {
					return len(verdicts.refused), true
				}
				verdicts.refused[hubConnection{event.Server.ID, event.Client.ID}] = event.Reason
			default:
				return len(verdicts.refused), false
			}
		}
	})
	for len(r.sent) > 0 {
		msg := <-r.sent
		claims, _, err := openRequest(r.xkey, msg)
		require.NoError(t, err, "an authorization request the server sent")
		verdicts.connection[claims.ClientInformation.Nonce] = hubConnection{claims.Server.ID, claims.ClientInformation.ID}
		verdicts.inbox[claims.ClientInformation.Nonce] = msg.Reply
	}
	for len(r.answered) > 0 {
		verdicts.answered[(<-r.answered).Subject] = true
	}
	for _, sub := range []*nats.Subscription{r.requests, r.refusals, r.answers} {
		dropped, err := sub.Dropped()
		require.NoError(t, err)
		require.Zero(t, dropped, "the record dropped %d of the server's messages on %s", dropped, sub.Subject)
	}
	return verdicts
}

// monitoredHub is a hub whose connections its monitoring endpoint lists: a testnats.Server in this
// process, or a testnats.HubProcess in a process of its own.
type monitoredHub interface{ MonitorURL() string }

// connectionsNamed is how many open connections server lists under name (its /connz).
func connectionsNamed(t *testing.T, server monitoredHub, name string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.MonitorURL()+"/connz?limit=100000", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err, "read the server's connections")
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, "the server's /connz")
	var connz struct {
		Connections []struct {
			Name string `json:"name"`
		} `json:"connections"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connz), "decode the server's connections")
	named := 0
	for _, connection := range connz.Connections {
		if connection.Name == name {
			named++
		}
	}
	return named
}

// hubOutcome is what the server's record says became of the request on one nonce, other than a
// give-up on an answer that reached the server.
type hubOutcome int

const (
	// hubUnsent: the record holds no request on the nonce.
	hubUnsent hubOutcome = iota
	// hubKept: the server refused nothing on the connection, so it took the request's answer.
	hubKept
	// hubRefused: the server refused the connection for the reason the record holds.
	hubRefused
	// hubUnanswered: the server gave up on the request, and no answer to it reached the server.
	hubUnanswered
)

// hubNonce is one nonce the record does not count as a give-up, and what became of it.
type hubNonce struct {
	nonce      string
	outcome    hubOutcome
	connection uint64
	reason     string
}

func (n hubNonce) String() string {
	switch n.outcome {
	case hubKept:
		return fmt.Sprintf("%s (connection %d): the server took its answer and refused nothing on it", n.nonce, n.connection)
	case hubRefused:
		return fmt.Sprintf("%s (connection %d): the server refused it: %s", n.nonce, n.connection, n.reason)
	case hubUnanswered:
		return fmt.Sprintf("%s (connection %d): the server gave up on it, and no answer to it reached the server", n.nonce, n.connection)
	default:
		return n.nonce + ": the server's record holds no request on it"
	}
}

// gaveUpOn splits nonces into those whose request the server gave up on while the service's
// answer to it reached the server, by its own record, and every other, each with what the record
// says became of it.
func (v hubVerdicts) gaveUpOn(nonces []string) (gaveUp []string, other []hubNonce) {
	for _, nonce := range nonces {
		connection, sent := v.connection[nonce]
		reason, refused := v.refused[connection]
		switch {
		case !sent:
			other = append(other, hubNonce{nonce: nonce, outcome: hubUnsent})
		case !refused:
			other = append(other, hubNonce{nonce: nonce, outcome: hubKept, connection: connection.id})
		case reason != "":
			other = append(other, hubNonce{nonce: nonce, outcome: hubRefused, connection: connection.id, reason: reason})
		case !v.answered[v.inbox[nonce]]:
			other = append(other, hubNonce{nonce: nonce, outcome: hubUnanswered, connection: connection.id})
		default:
			gaveUp = append(gaveUp, nonce)
		}
	}
	return gaveUp, other
}
