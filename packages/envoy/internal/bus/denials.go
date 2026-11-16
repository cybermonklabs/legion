package bus

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// publishRefusal reads the subject of the server's refusal of a publish: `Permissions Violation
// for Publish to "<subject>"` (nats-server v2.10.29 client.go:4947-4951), which it reports on the
// connection and never in the publish's own answer. The server writes the subject with %q, as it
// writes the names of a refused subscription (subscriptionRefusal), so it is matched as a whole
// Go-quoted string, escapes included, and unquoted before it is recorded: a subject holding a
// double quote or a backslash reaches the text escaped, and is recorded under the name the
// publisher sent.
var publishRefusal = regexp.MustCompile(`Publish to ("(?:[^"\\]|\\.)*")`)

// publishDenialRetention is how long a recorded violation can still be read (deniedPublish). It
// outlasts, with room to spare, every wait a publish of this package makes for the answer a denied
// publish never gets - publishAcknowledgementTimeout for a window's send, publishBound for
// publishJetStream - so a violation recorded the moment the server refused the message is still
// there when the publish that earned it gives up. It is short enough that a record no publish ever
// claims, because its caller stopped waiting, is dropped long before the same subject is published
// again, so a stale violation never answers a later publish on its own: the publish's own start
// time (deniedPublish) is what rules that out within the retention.
const publishDenialRetention = time.Minute

// publishDenialDepth bounds how many violations of one subject the record keeps, the oldest
// dropped when one past it is recorded. It is measured against how many messages of one subject
// one publisher can have in flight at once: the outbox sends at most one batch per publish window
// and its batch is batchSize (100) events, so a bound comfortably above that cannot drop a
// violation a live publish is still owed, while a server refusing a subject nobody publishes
// again holds no more than this many records of it.
const publishDenialDepth = 256

// publishViolation is the server's refusal of a publish to one subject: the violation it reported
// and the time the client recorded it (recordPublishDenial).
type publishViolation struct {
	violation error
	at        time.Time
}

// recordPublishDenial records violation, a permissions violation nc reported, under the subject it
// names a refused publish to, and reports whether it was a refusal of a publish at all - a refusal
// of a subscription is not, and is the caller's to read (onAsyncError). It goes to the end of that
// subject's queue of violations, which holds them in arrival order: nats-server reports one
// violation per refused message, so one window's several messages to one subject earn a violation
// each, and a record of one violation per subject would answer one of those sends and leave every
// other to its acknowledgement timeout. Each send of the subject takes the violation that answers
// it (deniedPublish).
//
// Nothing is recorded for a connection the client has since replaced, as nothing is marked for one
// (onAsyncError): its publishes are answered already, and the record belongs to the connection in
// Conn. c.mu is held across the check and the record, so a replacement cannot be installed between
// them. Each record carries the time it was made, which bounds how long it can be read, and
// recording one drops every record that retention has spent and, past publishDenialDepth, the
// oldest of the subject's own, so a connection refused publishes to ever new subjects, or ever
// more of one subject, cannot grow the record without bound.
func (c *Client) recordPublishDenial(nc *nats.Conn, violation error) bool {
	match := publishRefusal.FindStringSubmatch(violation.Error())
	if match == nil {
		return false
	}
	subject, err := strconv.Unquote(match[1])
	if err != nil {
		return false
	}
	at := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if nc != c.Conn {
		return true
	}
	if c.deniedPublishes == nil {
		c.deniedPublishes = make(map[string][]publishViolation)
	}
	for recorded, queue := range c.deniedPublishes {
		spent := 0
		for spent < len(queue) && at.Sub(queue[spent].at) > publishDenialRetention {
			spent++
		}
		switch {
		case spent == len(queue):
			delete(c.deniedPublishes, recorded)
		case spent > 0:
			c.deniedPublishes[recorded] = slices.Delete(queue, 0, spent)
		}
	}
	queue := append(c.deniedPublishes[subject], publishViolation{violation: violation, at: at})
	if len(queue) > publishDenialDepth {
		queue = slices.Delete(queue, 0, len(queue)-publishDenialDepth)
	}
	c.deniedPublishes[subject] = queue
	return true
}

// deniedPublish is the server's refusal of the caller's own publish to subject: the oldest
// violation of that subject recorded at or after sentAt, named as ErrPublishDenied
// (deniedPublishError) and taken out of the subject's queue, and nil when the connection reported
// no such refusal. It is what a publish whose own answer cannot name a denial reads to learn that
// a denial, and not the connection, is what it came to: the stream acknowledges a denied JetStream
// publish with nothing (window.acknowledged, publishJetStream), and a window's Request meets a
// flush that one last error per connection can leave unable to confirm the publish (window.request).
//
// sentAt is the time the caller put its own message on the wire, which is what keeps a record from
// naming another publish: a violation recorded before this publish was sent belongs to an earlier
// publish of the subject, whose caller may have stopped waiting, so it is skipped and left where
// it is, for retention to drop, rather than answering this publish, whose own failure then has
// another reason. Taking the oldest violation recorded since, and only that one entry, is what
// answers each refused message with a violation of its own: the server reports one per message, so
// every send of the subject still waiting is owed one of those the queue holds. The subject leaves
// the record once its last violation is read.
func (c *Client) deniedPublish(subject string, sentAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	queue := c.deniedPublishes[subject]
	for index, denial := range queue {
		if denial.at.Before(sentAt) {
			continue
		}
		if len(queue) == 1 {
			delete(c.deniedPublishes, subject)
		} else {
			c.deniedPublishes[subject] = slices.Delete(queue, index, index+1)
		}
		return deniedPublishError(subject, denial.violation)
	}
	return nil
}

// clearPublishDenials drops every recorded violation, because they were the previous session's:
// the grant the server applies to the session a reconnect opened is its own, and a publish on it
// is answered by it. Its caller holds c.mu, where it learns of the reconnect (onReconnect) or
// installs the constructor's connection (install), where the record is still empty.
func (c *Client) clearPublishDenials() {
	clear(c.deniedPublishes)
}

// deniedPublishError names violation, the server's refusal of a publish to subject under the
// connection's own publish permissions, as ErrPublishDenied. It is the one spelling of that error:
// the core path's read of the connection's last error (publishDenial) and the JetStream paths'
// read of the per-subject record (deniedPublish) both return it, so the text an operator greps and
// the errors a caller matches are the same whichever path named the denial.
func deniedPublishError(subject string, violation error) error {
	return fmt.Errorf("bus: %w to %q: %w", ErrPublishDenied, subject, violation)
}

// liveClients holds every client that has been built and has not closed or drained, and
// liveClientsMu guards it. It exists because the record of the server's refusals lives on the
// client (recordPublishDenial) while a caller that must read it may hold only a connection:
// EnsureKeyValue is handed conn and a JetStream context, and nats.go builds the bucket's lookup
// and its create inside itself, so that call has no publish of its own to take an error off and
// nothing but the connection to name the client by. clientOwning is the read.
//
// It holds the clients rather than mapping each connection to its client, so that it is keyed on
// the Conn a client holds now: a client replaces its connection, in the constructor's install and
// again for each one ensureConnWithContext dials after a close, and a map written at every such
// event would both need writing there and hold connections nobody uses until the next one. A
// client's Conn is read under that client's own lock instead (Connection), so a lookup sees the
// connection a client had or the one it has and never a half-installed one.
//
// Nothing takes liveClientsMu while holding a client's own lock, which is what lets clientOwning
// read Connection under it: registerClient runs in install once c.mu is released, and
// forgetClient in Close and Drain outside their own hold of it.
var (
	liveClientsMu sync.Mutex
	liveClients   = map[*Client]struct{}{}
)

// registerClient puts c in the registry of live clients, from install, so a connection the client
// owns can find it for as long as a caller holds the client.
func registerClient(c *Client) {
	liveClientsMu.Lock()
	defer liveClientsMu.Unlock()
	liveClients[c] = struct{}{}
}

// forgetClient takes c out of that registry, where the client closes (Close) and where it drains
// (Drain): its connection is going, and no send on it reads a record of it again.
func forgetClient(c *Client) {
	liveClientsMu.Lock()
	defer liveClientsMu.Unlock()
	delete(liveClients, c)
}

// clientOwning is the live client whose connection is conn, and nil when no client owns it: a
// connection a caller opened for itself, which every testnats.Connect in a test is, finds none
// and reads no record.
func clientOwning(conn *nats.Conn) *Client {
	if conn == nil {
		return nil
	}
	liveClientsMu.Lock()
	defer liveClientsMu.Unlock()
	for client := range liveClients {
		if client.Connection() == conn {
			return client
		}
	}
	return nil
}

// refusedSend is send's answer, with an answer the server never gave (unanswered) replaced by its
// refusal of one of subjects, the subjects send publishes to, when the client owning conn
// recorded one of them at or after the moment this send began. The server reports such a refusal
// on the connection and never in the request's own answer (publishRefusal), so a request it
// refused reads exactly like one it is still working on; the record is what tells the two apart.
//
// A request the connection's grant does not cover is refused however often it is sent, so naming
// the refusal is also what keeps it from being sent again: sentAgain stops at the first answer
// that is not unanswered, so a step wrapped in this makes one send and ends with an error naming
// the subject the server refused. subjects are listed in the order a send publishes them, so the
// refusal a step ends on is the one of the request it stopped at.
//
// The time is read before send, because deniedPublish takes only a violation recorded at or after
// the moment the caller's own message went out and send's message goes out after this. A
// connection no client owns records nothing, so its send is answered as it was before.
func refusedSend[T any](conn *nats.Conn, subjects []string, send func() (T, error)) (T, error) {
	owner := clientOwning(conn)
	if owner == nil {
		return send()
	}
	sentAt := time.Now()
	answer, err := send()
	if !unanswered(err) {
		return answer, err
	}
	for _, subject := range subjects {
		if denial := owner.deniedPublish(subject, sentAt); denial != nil {
			return answer, denial
		}
	}
	return answer, err
}
