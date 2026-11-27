package bus

import (
	"fmt"
	"log/slog"
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

// refusedPublishSubject is the subject of the publish violation, the server's refusal of a
// publish, says it refused (publishRefusal), unquoted; false when its text names no such subject.
// The record of refusals (recordPublishDenial) and the keeper's record of a refused bucket
// request (keepBuckets) both read a refusal's subject through it.
func refusedPublishSubject(violation error) (string, bool) {
	match := publishRefusal.FindStringSubmatch(violation.Error())
	if match == nil {
		return "", false
	}
	subject, err := strconv.Unquote(match[1])
	if err != nil {
		return "", false
	}
	return subject, true
}

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
	subject, ok := refusedPublishSubject(violation)
	if !ok {
		return false
	}
	at := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if nc != c.Conn && nc != c.installing {
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
	c.signalRefusal()
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
	return c.deniedPublishLocked(subject, sentAt)
}

// deniedPublishLocked is deniedPublish with c.mu held.
func (c *Client) deniedPublishLocked(subject string, sentAt time.Time) error {
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

// deniedPublishOf is the server's refusal of the caller's own publish to the first of subjects that
// has one recorded at or after sentAt (deniedPublish), and nil when none has. Each subject is
// matched as the request sends it, a full wildcard at its end included: the create of a consumer
// filtered on `notifications.github.>` goes to a subject ending in `.>`, and only that subject's
// refusal answers it, not the refusal of a narrower create of the same consumer. A send whose
// subject holds a part nats.go picks as it sends reads deniedPublishMatching instead.
func (c *Client) deniedPublishOf(subjects []string, sentAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, subject := range subjects {
		if denial := c.deniedPublishLocked(subject, sentAt); denial != nil {
			return denial
		}
	}
	return nil
}

// deniedPublishMatching is the server's refusal of the caller's own publish to a subject matches
// accepts, recorded at or after sentAt (deniedPublish), and nil when none is: the read of a send
// whose subject holds a part nats.go picks as it sends, which the caller cannot name and so
// describes, such as the name of a watch's consumer (watchCreate.matches). matches should accept
// only a subject whose refusal the caller's own publish would meet too, since the refusal this
// takes is then gone from the record of the send that earned it. watchCreate.matches has one
// exception: a grant that refuses one consumer by name refuses only that consumer's create, so its
// refusal can answer a watch of the same shape whose own create the grant allows, and takes the
// refusal from the send that earned it. That needs a named consumer on a bucket's KV_ stream, and
// this module creates none: every named consumer it creates is on Stream (the listener's durable,
// the session consumers, Dispatch's delivery and job durables), whose creates no watch matches.
func (c *Client) deniedPublishMatching(matches func(subject string) bool, sentAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for subject := range c.deniedPublishes {
		if !matches(subject) {
			continue
		}
		if denial := c.deniedPublishLocked(subject, sentAt); denial != nil {
			return denial
		}
	}
	return nil
}

// refusalSignal is a channel that is closed when the next refusal of a publish on c's connection is
// recorded (recordPublishDenial, signalRefusal). A send that waits on an answer takes it before its
// message goes out and selects on it beside its own answer (refusedBy, window.acknowledged,
// window.request), so a refusal answers the send the moment nats.go hands the server's report
// over, where a send that read the record only once its own wait ran out spent that whole wait on
// a request the server had refused at once. The signal says only that some refusal arrived: the
// woken send takes the signal again, then reads the record (deniedPublishOf,
// deniedPublishMatching), and goes on waiting when the refusal was another send's; a refusal
// recorded between the two is in the record it reads, since the record is written before the
// signal is replaced. Taking the signal is one atomic load and allocates nothing, so a send the
// server answers pays nothing for it.
func (c *Client) refusalSignal() <-chan struct{} {
	for {
		if signal := c.refusal.Load(); signal != nil {
			return *signal
		}
		fresh := make(chan struct{})
		if c.refusal.CompareAndSwap(nil, &fresh) {
			return fresh
		}
	}
}

// signalRefusal wakes every send waiting on refusalSignal and puts a new signal in its place. Its
// caller holds c.mu and has just written the record, so two refusals recorded at once replace
// the signal one after the other. A refusal allocates one channel; a request allocates none.
func (c *Client) signalRefusal() {
	fresh := make(chan struct{})
	if old := c.refusal.Swap(&fresh); old != nil {
		close(*old)
	}
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

// loggerOwning is the logger of the client that owns conn (WithLogger), and followsDefault, which
// writes where slog.Default() points at each line, when no client owns it: where a function given
// a connection and no client, such as EnsureKeyValue, writes its lines.
func loggerOwning(conn *nats.Conn) *slog.Logger {
	if owner := clientOwning(conn); owner != nil {
		return owner.log()
	}
	return followsDefault
}

// refusedBy is send's answer, or the server's refusal of one of subjects, the subjects send
// publishes to, when owner, the client that owns the connection send goes out on, records one of
// them at or after the moment this send began, whichever comes first. The server reports such a
// refusal on the connection and never in the request's own answer (publishRefusal), so a request
// it refused reads exactly like one it is still working on until its wait runs out; the record is
// what tells the two apart, and the signal the record gives (refusalSignal) is what answers the
// send as soon as the refusal arrives, where reading the record only after send returned spent the
// request's whole wait, RequestTimeout for a start, on a request refused at once.
//
// A request the connection's grant does not cover is refused however often it is sent, so naming
// the refusal is also what keeps it from being sent again: sentAgain stops at the first answer
// that is not unanswered, so a step wrapped in this makes one send and ends with an error naming
// the subject the server refused. send is one request, and subjects are the subjects that request
// can be sent to, each matched as it is sent (deniedPublishOf). A call that makes several requests
// one after another is made one request at a time, each under its own subject (refusalReads's
// AddConsumer, createBucketStream): listed for the whole call, a later request's subject would
// take another send's refusal of it while the call still waits on an earlier one, and take it out
// of the record that send reads.
//
// send runs on a goroutine of its own, so a refusal can answer before it returns; that goroutine
// ends at send's own wait, and its answer is then dropped, as Bounded drops one, since what
// these sends answer holds nothing (refusedByRecord releases an answer that does). That costs a
// goroutine, a channel and two closures, under a microsecond, which is why it serves the requests
// of a start and the other requests made once in a while (readsRefusals, the KeyValue handle's
// Status and stream reads, and a watch's create through refusedByRecord, watchRefused); the reads
// and writes of a key made on every request the bus serves take refusedAfter, which costs nothing
// when the server answers. The signal is taken and the time read before send, because
// deniedPublish takes only a violation recorded at or after the moment the caller's own message
// went out and send's message goes out after this.
//
// The caller names owner, found once for the many requests it makes (readsRefusals and the
// KeyValue handle look it up with clientOwning); the client's own JetStream context and stream
// ensure name the client itself (Client.readsOwnRefusals), since the constructor ensures the
// stream before the client is registered (install). owner nil, a connection no client owns,
// records nothing, so its send is answered as it was before.
func refusedBy[T any](owner *Client, subjects []string, send func() (T, error)) (T, error) {
	if owner == nil {
		return send()
	}
	return refusedByRecord(owner, func(sentAt time.Time) error { return owner.deniedPublishOf(subjects, sentAt) }, send, nil)
}

// refusedByRecord is refusedBy for a send whose refusal denied reads out of owner's record, given
// the moment the send began, where refusedBy reads the refusal of the subjects it is given: a
// watch's create, whose subject holds a name nats.go picks as it sends (watchRefused). release,
// when it is not nil, takes the answer of a send a refusal answered first, once that send returns
// it without an error: an answer that holds something, such as a watcher, is let go there rather
// than dropped with nobody holding it. owner is not nil.
func refusedByRecord[T any](owner *Client, denied func(sentAt time.Time) error, send func() (T, error), release func(T)) (T, error) {
	signal := owner.refusalSignal()
	sentAt := time.Now()
	type answer struct {
		value T
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		value, err := send()
		answered <- answer{value: value, err: err}
	}()
	for {
		select {
		case got := <-answered:
			if unanswered(got.err) {
				// The refusal can be recorded after the send's wait ran out, nats.go handing the
				// server's report over on another goroutine, so the record is read once more.
				if denial := denied(sentAt); denial != nil {
					return got.value, denial
				}
			}
			return got.value, got.err
		case <-signal:
			signal = owner.refusalSignal()
			if denial := denied(sentAt); denial != nil {
				if release != nil {
					go func() {
						if got := <-answered; got.err == nil {
							release(got.value)
						}
					}()
				}
				var none T
				return none, denial
			}
		}
	}
}

// refusedAfter is send, made on the caller's goroutine, with the server's refusal of one of the
// subjects in place of its answer when the server left it unanswered and owner's record holds that
// refusal (deniedPublishOf): the reads and writes of a key the bus makes on every request it
// serves (KeyValue's Get, Put and the rest, GetFromLeader), which this keeps exactly as fast as a
// send made without it. A refused one waits out its own wait, as it always has, and then names the
// refusal where it named only the silence. subjects is called only then, so a send the server
// answers builds no subject either. owner nil sends as send does.
func refusedAfter[T any](owner *Client, subjects func() []string, send func() (T, error)) (T, error) {
	if owner == nil {
		return send()
	}
	sentAt := time.Now()
	value, err := send()
	if unanswered(err) {
		if denial := owner.deniedPublishOf(subjects(), sentAt); denial != nil {
			return value, denial
		}
	}
	return value, err
}
