package main

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/legiontasks"
)

// taskRelayInterval is how often the task relay reads the task bucket again while it has no watch
// of it, and repairs a watch that stopped or no longer follows the bucket: a bucket the Legion
// daemons have not created yet is looked up once an interval, and a grant that refuses the reads
// is reported once an interval. Thirty seconds is the self-health monitor's own interval and the
// session relays' prune interval, so a bucket a daemon creates is relayed from within that long,
// and a listener whose grant lacks the reads costs one STREAM.INFO and one ERROR line each.
const taskRelayInterval = 30 * time.Second

// taskRelayName labels the task relay's KV watcher and its lines (kvwatch.New).
const taskRelayName = "task relay"

// taskFrameReplyToken is the token of every task frame's reply subject under the session relays'
// reply inbox (sessionRelays.replyPrefix). The relay asks nothing of the session, and a client that
// knows task frames answers none; one that does not refuses a frame without the session relay's
// headers by answering it done (packages/envoy-client's consumer delivery). That answer reaches
// sessionRelays.onReply, which acts on a token only when a session frame awaits it, and no session
// frame's token holds a dot (base 36 has none), so the answer finds no frame and is ignored: it acks
// no message of the session's consumer.
const taskFrameReplyToken = "task.frame"

// The lines the task relay writes that an operator, or a test, reads by their text.
const (
	// taskRelayOpenedLine: the bucket opened and its watch started (INFO).
	taskRelayOpenedLine = "task relay opened the task bucket; it relays each claim's task value to the session holding the claim's role"
	// taskRelayWaitLine: the bucket does not exist yet (INFO, once until it opens).
	taskRelayWaitLine = "task relay waits for the task bucket, which the Legion daemons create; it looks the bucket up again every interval, and relays no task value until it exists"
	// taskRelayRefusedLine: the server refused the listener's user a call on the bucket (ERROR, once
	// an interval), naming the subject refused (missing_subject).
	taskRelayRefusedLine = "task relay cannot read the task bucket: the listener's NATS user lacks the bucket's reads (envoy-infra's ListenerPermissions grant them), so no task value is relayed while every other route serves; it tries again every interval"
	// taskRelayFailedLine: any other failure to open or watch the bucket (WARN, once until it opens).
	taskRelayFailedLine = "task relay could not open or watch the task bucket, so no task value is relayed while every other route serves; it tries again every interval"
	// taskRelayRelayedLine: one task frame published (INFO).
	taskRelayRelayedLine = "task relay relayed a claim's task value"
)

// taskRecord is a key's newest record as the task bucket's watch delivered it: a put of value, or a
// delete (a purge reads as one), at revision.
type taskRecord struct {
	op       string
	value    []byte
	revision uint64
}

// taskRelayed is what the relay last relayed of one key: the session it went to, and the record's
// epoch and revision.
type taskRelayed struct {
	session  string
	epoch    string
	revision uint64
}

// taskRelay relays each claim's task value from the Legion task bucket (legiontasks.Bucket) to the
// session that holds the claim's role, as a core frame on that session's relay subject
// (bus.DeliverSubject), beside the session relays (sessionRelays) that relay the session's
// messages on the same subject. A key of the bucket is a claim token, and a claim token is the
// role its worker's session claims, so the session to relay key K to is the holder of role K
// (store.Registry.CachedRoleHolder).
//
// It relays from the listener that relays the holder's own consumer (sessionRelays.wanted): the
// one whose machine id the session's entry names, holding its machine's durable binding, for a
// session on consumer delivery. Every other listener watches the same bucket and relays nothing to
// that session. It relays a key's record when the watch delivers a newer one (each put and delete,
// and every key of a first read), and the key's current record when the role's claim changes in the
// role claim view (store.Registry.OnRoleHolderChange) to a holder it has not relayed that record
// to, so a role claimed after its value was written gets the value then. A claim's renewal, which
// every registration writes, is such a change too, so a session that becomes this listener's after
// its claim was written gets its value at its next registration at the latest. A record superseded
// before the relay reached it is not sent: a worker takes only its claim's current value.
//
// It runs only on a listener that gives its sessions consumer delivery: it opens the bucket once a
// session consumer call of this listener has succeeded (sessionDelivery.consumerGrantHeld), which is
// how the listener learns its NATS user holds the session consumers' calls. Only the grants of the
// listeners no session shares a user with hold those calls, and they hold the task bucket's reads
// beside them (envoy-infra's ListenerPermissions and ListenerHostPermissions), so a listener
// connecting as a host or Dispatch user never opens the bucket, and logs nothing of it.
//
// It opens the bucket and never creates it (bus.OpenKeyValue): the Legion daemons create it. A
// bucket that does not exist yet is waited for quietly, looked up again every interval, with one
// INFO line; a lookup or watch the server refuses under the listener's grant is an ERROR once an
// interval naming the subject refused; any other failure is a WARN once until the watch is up
// again. None of them is in the self-health set that ends the process (listenerCaches), /healthz
// reads none of them, and every other route serves through them all.
type taskRelay struct {
	// client is the main bus connection, which opens and watches the bucket.
	client *bus.Client
	// relays are the session relays: which sessions this listener relays (wanted), the reply inbox
	// a frame's reply subject is under (replyPrefix), and the session-consumer connection a frame
	// is published on (delivery.consumers), as the session relays publish theirs.
	relays   *sessionRelays
	registry *store.Registry
	logger   *logging.Logger
	// granted is closed once this listener gives consumer delivery (consumerGrantHeld).
	granted <-chan struct{}
	// interval is taskRelayInterval, and openWait the JetStream wait of each lookup of the bucket
	// (bus.RequestTimeout); a test shortens both. Set before run starts and read-only after.
	interval time.Duration
	openWait time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	// reconnected wakes run to watch the bucket again on the connection a reconnect brought, and
	// wake to relay the keys pending.
	reconnected chan struct{}
	wake        chan struct{}

	// eventsMu guards pending, the keys whose record or role holder changed since run last relayed.
	eventsMu sync.Mutex
	pending  map[string]struct{}

	// mu guards the fields below it, which the watch's hooks (taskCache) and run share.
	mu sync.Mutex
	// watcher is the bucket's one watcher, made at the first open and repaired by its Rewatch after
	// that; nil until the bucket opened. stopped is set by stop and end, after which no watcher is
	// made. stopRoles unregisters the role change hook the first open registered.
	watcher   *kvwatch.Watcher
	stopped   bool
	stopRoles func()
	// epoch is the bucket's epoch the current read's records belong to (legiontasks.Epoch of its
	// stream's creation time). reading is set from a read's start until it completes, and seen
	// holds the keys that read delivered, so its completion drops the records it found no evidence
	// of (taskCache.CompleteRead).
	epoch   string
	reading bool
	seen    map[string]struct{}
	// records is each key's newest record in the bucket, and relayed what was last relayed of each.
	records map[string]taskRecord
	relayed map[string]taskRelayed

	// What run has logged of the failures since the watch was last up, so each is logged as its
	// kind is meant to be: waitLogged and failLogged once until the watch is up again, and
	// refusalLogged once an interval (run clears it at each tick). Only run reads or writes them.
	waitLogged    bool
	failLogged    bool
	refusalLogged bool
}

// startTaskRelay starts the task relay of the listener whose main bus connection is client, beside
// relays, the relays of its session consumers, reading each role's holder from registry. It opens
// nothing until the listener gives consumer delivery (sessionDelivery.consumerGrantHeld), and then
// reads the bucket again every interval while it has no watch of it, each lookup waiting openWait.
// It ties itself to client: a disconnect stops its copy following the bucket, a reconnect watches
// the bucket again on the new connection in the background, and the connection's drain ends its
// watcher (end).
func startTaskRelay(client *bus.Client, relays *sessionRelays, registry *store.Registry, logger *logging.Logger, interval, openWait time.Duration) *taskRelay {
	r := &taskRelay{
		client:      client,
		relays:      relays,
		registry:    registry,
		logger:      logger,
		granted:     relays.delivery.consumerGrantHeld(),
		interval:    interval,
		openWait:    openWait,
		reconnected: make(chan struct{}, 1),
		wake:        make(chan struct{}, 1),
		pending:     map[string]struct{}{},
		records:     map[string]taskRecord{},
		relayed:     map[string]taskRelayed{},
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	client.AddDisconnectHook(r.disconnected)
	client.AddReconnectHook(r.reconnect)
	client.AddDrainHook(r.end)
	go r.run()
	return r
}

// run waits until the listener gives consumer delivery, then opens and watches the bucket,
// repairs the watch every interval and after each reconnect, and relays the keys pending whenever
// a record or a role's holder changes, until stop.
func (r *taskRelay) run() {
	select {
	case <-r.ctx.Done():
		return
	case <-r.granted:
	}
	r.repair(false)
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
			r.refusalLogged = false
			r.repair(false)
		case <-r.reconnected:
			r.repair(true)
		case <-r.wake:
		}
		r.relayPending()
	}
}

// repair opens the bucket and starts its watch while there is none, and watches it again on the
// current connection when the watcher ended, its copy needs a read nothing is making
// (kvwatch.Watcher.NeedsRead), or reconnected says a reconnect brought a new connection. A failure
// is logged as its kind is (failed), and the next interval or reconnect tries again.
func (r *taskRelay) repair(reconnected bool) {
	conn := r.client.Connection()
	r.mu.Lock()
	w := r.watcher
	r.mu.Unlock()
	var err error
	switch {
	case w == nil:
		err = r.open(conn)
	case reconnected || w.Err() != nil || w.NeedsRead():
		err = w.Rewatch(conn)
	default:
		return
	}
	if err != nil {
		r.failed(conn, err)
		return
	}
	r.waitLogged, r.failLogged = false, false
}

// open opens the bucket on conn, never creating it, makes its watcher, registers the role change
// hook and starts the watch, whose first read delivers every key the bucket holds.
func (r *taskRelay) open(conn *nats.Conn) error {
	js, err := conn.JetStream(nats.MaxWait(r.openWait))
	if err != nil {
		return err
	}
	kv, err := bus.OpenKeyValue(conn, js, legiontasks.Bucket)
	if err != nil {
		return err
	}
	w := kvwatch.New(taskRelayName, kv, taskCache{r}, kvwatch.WithLogger(r.logger.Slog()))
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.watcher = w
	r.stopRoles = r.registry.OnRoleHolderChange(r.enqueue)
	r.mu.Unlock()
	w.Start()
	r.logger.Info(taskRelayOpenedLine,
		slog.String("bucket", legiontasks.Bucket))
	return nil
}

// failed logs err, a failed open or watch of the bucket on conn, as its kind is logged: a bucket
// that does not exist yet with one INFO line until the watch is up, a refusal under the listener's
// grant with one ERROR an interval naming the subject refused, anything else with one WARN until
// the watch is up. A failure an open still in flight at stop brings back is logged by nobody.
func (r *taskRelay) failed(conn *nats.Conn, err error) {
	if r.ctx.Err() != nil {
		return
	}
	if taskBucketAbsent(err) {
		if !r.waitLogged {
			r.waitLogged = true
			r.logger.Info(taskRelayWaitLine,
				slog.String("bucket", legiontasks.Bucket),
				slog.String("interval", r.interval.String()))
		}
		return
	}
	if subject, refused := refusedTaskSubject(err, conn); refused {
		if !r.refusalLogged {
			r.refusalLogged = true
			r.logger.Error(taskRelayRefusedLine,
				slog.String("bucket", legiontasks.Bucket),
				slog.String("missing_subject", subject),
				slog.String("interval", r.interval.String()),
				slog.String("error", err.Error()))
		}
		return
	}
	if !r.failLogged {
		r.failLogged = true
		r.logger.Warn(taskRelayFailedLine,
			slog.String("bucket", legiontasks.Bucket),
			slog.String("interval", r.interval.String()),
			slog.String("error", err.Error()))
	}
}

// taskBucketAbsent reports whether err says the bucket, or its stream, does not exist.
func taskBucketAbsent(err error) bool {
	return errors.Is(err, nats.ErrBucketNotFound) || errors.Is(err, nats.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrStreamNotFound)
}

// serverPublishRefusal reads the subject of the server's refusal of a publish, `Permissions
// Violation for Publish to "<subject>"`, which it writes with %q (bus's publishRefusal).
var serverPublishRefusal = regexp.MustCompile(`Publish to ("(?:[^"\\]|\\.)*")`)

// refusedTaskSubject is the subject of the task bucket the server refused this listener's user,
// and whether err, a failed open or watch on conn, came of such a refusal. The open's lookup names
// its refusal itself (bus.OpenKeyValue answers bus.ErrPublishDenied with the subject); a watch's
// consumer create, whose subject carries a name nats.go makes, draws only silence, so the
// connection's last error is read for it, the server's violation, which counts when it names a
// subject of the task bucket: no other part of the listener sends one.
func refusedTaskSubject(err error, conn *nats.Conn) (string, bool) {
	for _, text := range []string{err.Error(), lastErrorText(conn)} {
		match := serverPublishRefusal.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		subject, unquoteErr := strconv.Unquote(match[1])
		if unquoteErr == nil && taskBucketSubject(subject) {
			return subject, true
		}
	}
	return "", false
}

// lastErrorText is the text of conn's last error when it is the server's permissions violation, and
// "" otherwise.
func lastErrorText(conn *nats.Conn) string {
	if conn == nil {
		return ""
	}
	if last := conn.LastError(); errors.Is(last, nats.ErrPermissionViolation) {
		return last.Error()
	}
	return ""
}

// taskBucketSubject reports whether subject is one a call on the task bucket sends: an API call on
// its stream, a flow-control reply of its watch, or one of its keys.
func taskBucketSubject(subject string) bool {
	return strings.Contains(subject, legiontasks.Stream) || strings.HasPrefix(subject, "$KV."+legiontasks.Bucket+".")
}

// enqueue has run relay key's current record: its record changed, or its role's claim did
// (store.Registry.OnRoleHolderChange, which hands it every role, and a role that is no key of the
// bucket is passed over at once).
func (r *taskRelay) enqueue(key string) {
	r.eventsMu.Lock()
	r.pending[key] = struct{}{}
	r.eventsMu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// relayPending relays the current record of every key pending, until stop.
func (r *taskRelay) relayPending() {
	r.eventsMu.Lock()
	if len(r.pending) == 0 {
		r.eventsMu.Unlock()
		return
	}
	keys := r.pending
	r.pending = map[string]struct{}{}
	r.eventsMu.Unlock()
	for key := range keys {
		if r.ctx.Err() != nil {
			return
		}
		r.relayKey(key)
	}
}

// relayKey relays key's current record to the holder of role key, when the bucket holds a record
// of key, the role has a holder this listener relays (sessionRelays.wanted), and that record has
// not already been relayed to that holder. A frame that could not be published is relayed again at
// the key's next record or the role's next claim write.
func (r *taskRelay) relayKey(key string) {
	r.mu.Lock()
	record, held := r.records[key]
	epoch, last := r.epoch, r.relayed[key]
	r.mu.Unlock()
	if !held {
		return
	}
	holder, err := r.registry.CachedRoleHolder(key)
	if err != nil {
		r.logger.Warn("task relay could not read who holds a claim's role, so it relays the claim's task value at the key's next write or the role's next claim",
			slog.String("claim", key),
			slog.String("error", err.Error()))
		return
	}
	if holder == "" {
		return
	}
	if ours, _ := r.relays.wanted(holder); !ours {
		return
	}
	sent := taskRelayed{session: holder, epoch: epoch, revision: record.revision}
	if last == sent {
		return
	}
	if err := r.publish(key, holder, epoch, record); err != nil {
		if r.ctx.Err() != nil {
			return
		}
		r.logger.Warn("task relay could not publish a claim's task value, so it relays it at the key's next write or the role's next claim",
			slog.String("claim", key),
			slog.String("session_id", holder),
			slog.String("error", err.Error()))
		return
	}
	r.mu.Lock()
	r.relayed[key] = sent
	r.mu.Unlock()
	r.logger.Info(taskRelayRelayedLine,
		slog.String("claim", key),
		slog.String("session_id", holder),
		slog.String("op", record.op),
		slog.String("epoch", epoch),
		slog.Uint64("revision", record.revision))
}

// publish publishes record, key's record of epoch, to holder as a task frame: a core message on
// the holder's relay subject carrying the four legiontasks headers and the value's bytes as the
// bucket holds them, empty for a delete, with a reply subject whose answers are ignored
// (taskFrameReplyToken).
func (r *taskRelay) publish(key, holder, epoch string, record taskRecord) error {
	header := nats.Header{}
	header.Set(legiontasks.HeaderClaim, key)
	header.Set(legiontasks.HeaderEpoch, epoch)
	header.Set(legiontasks.HeaderRevision, strconv.FormatUint(record.revision, 10))
	header.Set(legiontasks.HeaderOp, record.op)
	ctx, cancel := context.WithTimeout(r.ctx, sessionConsumerCallTimeout)
	defer cancel()
	return r.relays.delivery.consumers.Publish(ctx, &nats.Msg{
		Subject: bus.DeliverSubject(holder),
		Reply:   r.relays.replyPrefix + taskFrameReplyToken,
		Header:  header,
		Data:    record.value,
	})
}

// disconnected is the relay's disconnect hook: its copy stops following the bucket
// (kvwatch.Watcher.Disconnected), so a record delivered during the outage waits for the read the
// reconnect's watch makes.
func (r *taskRelay) disconnected() {
	r.mu.Lock()
	w := r.watcher
	r.mu.Unlock()
	if w != nil {
		w.Disconnected()
	}
}

// reconnect is the relay's reconnect hook. It only wakes run, which watches the bucket again on
// the new connection, or opens it there, in the background: so a bucket that is missing or refused
// holds up no other hook and fails no hook run, which the bus would retry without end and /healthz
// would report (bus.Client.HookStall).
func (r *taskRelay) reconnect(*nats.Conn) error {
	select {
	case r.reconnected <- struct{}{}:
	default:
	}
	return nil
}

// stop ends run and retires the watcher for the shutdown (kvwatch.Watcher.Stop), before the
// session-consumer connection the frames go out on drains; the drain of the main connection ends
// the watcher (end). It does not wait for run: a lookup of the bucket in flight takes no context and
// can wait out its whole JetStream wait, ten seconds, which the shutdown's budget cannot spend. Such
// a lookup installs no watcher once stop has run (open reads stopped), a frame run was publishing is
// cut by run's context, and nothing either brings back is logged.
func (r *taskRelay) stop() {
	if r == nil {
		return
	}
	r.cancel()
	r.mu.Lock()
	r.stopped = true
	w, stopRoles := r.watcher, r.stopRoles
	r.mu.Unlock()
	if stopRoles != nil {
		stopRoles()
	}
	if w != nil {
		w.Stop()
	}
}

// end is the relay's drain hook: it ends the watcher within ctx while the connection still carries
// the delete of its consumer (kvwatch.Watcher.End).
func (r *taskRelay) end(ctx context.Context) {
	r.mu.Lock()
	r.stopped = true
	w := r.watcher
	r.mu.Unlock()
	if w != nil {
		w.End(ctx)
	}
}

// taskCache is the relay as the cache its watcher feeds (kvwatch.Cache): the newest record of each
// key, which it hands run to relay. The hooks are its own type's, so nothing but the watcher calls
// them.
type taskCache struct{ r *taskRelay }

// BeginRead starts a read of the bucket: its epoch is the bucket's as the read reads it, and a
// recreated bucket's revisions number from 1 again, so the records of the earlier one are dropped
// and every record the new one holds is relayed.
func (c taskCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if read.Recreated {
		clear(r.records)
	}
	r.epoch = legiontasks.Epoch(read.Created)
	r.reading = true
	r.seen = map[string]struct{}{}
}

// Apply takes one record the watch delivered: one newer than the key's record is the key's record
// now, and run relays it. A record a read already held (a read of the bucket again) relays nothing.
// A record delivered once the copy has stopped following the bucket after a read completed (a
// disconnect, a bucket recreated or deleted under the watch) is passed over: the read that follows
// such a stop delivers it, from the bucket the copy follows then, and only it can say which epoch
// the record's revision belongs to.
func (c taskCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	if !r.reading && !r.watcher.Following() {
		r.mu.Unlock()
		return
	}
	if r.reading {
		r.seen[entry.Key] = struct{}{}
	}
	if held, ok := r.records[entry.Key]; ok && entry.Revision <= held.revision {
		r.mu.Unlock()
		return
	}
	record := taskRecord{op: legiontasks.OpPut, value: entry.Value, revision: entry.Revision}
	if entry.Op != kvwatch.OpPut {
		record = taskRecord{op: legiontasks.OpDelete, revision: entry.Revision}
	}
	r.records[entry.Key] = record
	r.mu.Unlock()
	r.enqueue(entry.Key)
}

// CompleteRead drops the record, and what was relayed, of every key the read delivered nothing
// of: the bucket no longer holds the key at all.
func (c taskCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.records {
		if _, ok := r.seen[key]; !ok {
			delete(r.records, key)
			delete(r.relayed, key)
		}
	}
	r.reading = false
	r.seen = nil
}
