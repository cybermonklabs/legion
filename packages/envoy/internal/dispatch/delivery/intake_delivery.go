package delivery

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// intakeDelivery is one subscription of the intake's: the client it subscribed on, the
// subscription, the keyed runner its callback (receive) hands messages to, and the context every
// handler of it runs under. Each subscribe makes one of its own, so stopping one ends that
// subscription and nothing a later subscribe starts.
type intakeDelivery struct {
	nats *bus.Client
	sub  *natsgo.Subscription
	keys *keyedRunner[intakeMessage]
	// handling is the parent of every handler's context (deliver), and abandon cancels it: the stop
	// gives up the handlers still running when its context ends.
	handling context.Context
	abandon  context.CancelFunc
	stopping atomic.Bool
	stopped  sync.Once

	mu sync.Mutex
	// held is what the stop answers once no handler of this delivery is running and the client has
	// a connection to answer over: the messages that were waiting behind their keys, those the
	// callback was handed since, and those whose handler finished before the stop released and
	// whose own acknowledgement the bus refused (acknowledgeOrHold), a refusal that can come before
	// the stop begins. A handler that finishes over a live link acknowledges its message itself and
	// leaves nothing here.
	held []heldMessage
	// released is set once held has been answered; the callback then answers a message at once.
	released bool
}

// intakeMessage is one message on its way to its key's handler, with its envelope when keying
// decoded it, so the handler does not decode it again. Keying decodes every message it wants, a
// pull request's and a workflow run's alike, so the envelope is absent only when that decode
// failed and deliver reports the failure.
type intakeMessage struct {
	msg      *natsgo.Msg
	envelope *contracts.Envelope
}

// heldMessage is one message the stop holds and the answer that message is owed: a negative
// acknowledgement for one this process did not finish handling (giveBack), and an
// acknowledgement for one whose own acknowledgement the bus refused before the stop released
// (acknowledgeOrHold). Both are publishes, and a client with no live connection refuses one, so
// both wait for the connection the stop waits for and are published over it (answerHeld).
type heldMessage struct {
	msg *natsgo.Msg
	// handled is set for a message its handler finished: the stop acknowledges it rather than
	// handing it back, so the next holder does not redo work that is done.
	handled bool
}

// subscribe subscribes the intake's handling to its durable's push deliveries
// (subscribeDurable): at the deliver subject the durable carries when deliverSubject is empty, as
// bind does, or at deliverSubject, for a caller that has just created the durable with it. The
// subscription's callback only routes each message (receive); its handler runs on the goroutine of
// the message's key (keyedRunner) and acknowledges it there, or, when the stop gave its handling
// up, hands it back through this delivery (deliver). StopDelivering stops the subscription this
// made last, so a caller subscribing again stops the one before first. A subscribe that fails
// gives up its handling and the registration the client kept of it
// (bus.Client.ReleaseFailed): a reconnect would bind that registration otherwise, and its
// callback, its handling given up, would hand back every message until the intake's next subscribe
// replaced it.
func (in *Intake) subscribe(deliverSubject string) (*natsgo.Subscription, error) {
	handling, abandon := context.WithCancel(context.Background())
	delivery := &intakeDelivery{nats: in.nats, handling: handling, abandon: abandon}
	delivery.keys = newKeyedRunner(func(message intakeMessage) { in.deliver(delivery, message) })
	wanted := deliveryConsumerConfig()
	sub, err := subscribeDurable(in.nats, wanted, deliverSubject, delivery.receive)
	if err != nil {
		abandon()
		in.nats.ReleaseFailed(wanted.FilterSubject)
		return nil, err
	}
	delivery.sub = sub
	in.mu.Lock()
	in.delivering = delivery
	in.mu.Unlock()
	return sub, nil
}

// intakeStopBudget is the deadline of an intake's stop, counted by each of Run's two stops from
// its own start: from the moment Run's ctx ends (stopContext) for the stop the process stopping
// runs, and from the event itself for the stop a connection event runs (giveUpOnConnectionChange).
// The messages in their handlers have until then to finish and be answered before the stop gives
// them up (StopDelivering), the drain's wait ends by then, and a bind under way as ctx ended ends
// by then too (bindDurableConsumer), so one that binds after ctx ended is stopped under the same
// deadline rather than one of its own. It bounds the stop's wait for a live connection too,
// whatever deadline its caller gave it (awaitLiveConnection). In cmd/dispatch that ctx is the
// signal context, so the stop begins at SIGTERM and runs beside the HTTP drain and the document
// shutdown, which take up to stopGrace (30 s) less a 5 s reserve: httpDrainBeforeDocuments then
// documentShutdownTimeout (cmd/dispatch/shutdown.go); StartIntakes' stop, which the shutdown waits
// for, adds no deadline of its own. The shutdown waits for the intakes after the document service
// and before it deregisters this process and closes the database pool, so that a handler's writes
// never meet a closed pool. 15 s leaves 10 s before the document budget's latest end for what
// follows the deadline -- the handlers given up returning, which they do as soon as their context
// is cancelled, the acknowledgements it holds, the drain's end, which the given-up handlers'
// cancelled context cuts short, and the clients closing -- so the intakes never hold the shutdown
// past what that budget already can, and 15 s before the kubelet's kill. It is also well past what
// a handler GitHub answers needs, a pull request and its commit pages with a page GitHub answered
// 502 asked again (after 1 s, then 2 s: githubPageRetryWait), so an ordinary handler finishes and
// is acknowledged rather than redone. A call GitHub does not answer holds one attempt for up to
// githubapp.RequestTimeout (20 s), and is then asked again, which no budget inside stopGrace could
// wait out. A var so a test can shrink it.
var intakeStopBudget = 15 * time.Second

// stopContext returns the stop deadline of an intake whose Run runs until ctx ends: a context that
// ends intakeStopBudget after ctx does, armed by ctx's end, so everything of the stop -- a bind
// under way at that moment, the handler wait, the drain's wait -- runs under one deadline counted
// from it. release frees it, for Run to defer.
func stopContext(ctx context.Context) (stopping context.Context, release func()) {
	// Read now rather than as ctx ends, so a test restoring intakeStopBudget after Run has
	// returned never races the read.
	budget := intakeStopBudget
	stopping, cancel := context.WithCancel(context.WithoutCancel(ctx))
	disarm := context.AfterFunc(ctx, func() { time.AfterFunc(budget, cancel) })
	return stopping, func() {
		disarm()
		cancel()
	}
}

// StopDelivering stops this intake's subscription so that another process can take the durable
// over with nothing waiting out the ack wait. What it does with the subscription depends on the
// link (bus.Client.DrainSubscription). On a live one it drains it, so the server delivers it
// nothing more while the callback is handed what the client had already buffered. With the link
// down it unsubscribes it instead, and nats.go drops that subscription's pending messages without
// calling the callback at all (waitForMsgs): those messages are not handed back here, and there
// is no drain to wait for. They are the price of leaving the durable free, and the server
// redelivers them to the next holder when their ack wait expires.
//
// Either way it waits for the messages in their handlers until ctx ends: one that finishes by
// then is acknowledged, and held for the stop only where the bus refused that acknowledgement
// (deliver, acknowledgeOrHold), and the handlers still running when ctx ends are given up --
// their context is cancelled, so a GitHub call that does not answer is abandoned -- and their
// messages held for the hand-back the rest are in (deliver, giveBack). Only once no handler is
// running, and the client has a connection to answer over, does it answer what it holds, each
// message by its own kind: an acknowledgement for one whose handler finished and whose own
// acknowledgement was refused, and a negative acknowledgement for every message that was
// waiting behind its key, every message a given-up handler held, and every message the callback
// was handed meanwhile, so the server redelivers those at once -- to the next holder, which can
// then not start a pull request's or run's next message while this process still works on the
// one before it. A message the callback is handed after that is answered at once. It returns
// once a drain it began has ended -- at the latest intakeDrainWait after the last handler
// returned, or as ctx ends if that is sooner, when it unsubscribes instead -- and at once where
// it unsubscribed. Either path gives up the subscription's registration on the client, so that
// no reconnect binds it again and a later subscribe on the same client starts afresh.
//
// Two callers run it, both Run's: as its ctx ends, under its stop deadline (stopContext), and on a
// connection event, under a budget of its own (giveUpOnConnectionChange). The second is the stop
// whose answers begin with the link down, and so the reason the stop waits for a connection at
// all. It is safe from any goroutine and idempotent: a second call returns once the first has
// stopped, and a call with nothing subscribed returns at once. Every call's ctx bounds the handler
// wait of the stop under way, so a caller with a nearer deadline than the first has the handlers
// given up when its own ctx ends.
func (in *Intake) StopDelivering(ctx context.Context) {
	in.mu.Lock()
	delivery := in.delivering
	in.mu.Unlock()
	if delivery != nil {
		delivery.stop(ctx)
	}
}

// stop is StopDelivering for this subscription. The drain or unsubscribe goes first, so the
// server has the subscription's interest gone before any negative acknowledgement reaches it,
// and redelivers what this one gives back to the next subscriber rather than straight back here.
// It is the subscription the client holds for this one's registration that it ends
// (bus.Client.DrainSubscription): a handle of the client's own once it has replaced its
// connection, not sub, which went with the replaced connection while the client's handle went on
// delivering here. The client holds nothing of this stopped callback's from that call on, and
// with the link down that call is an unsubscribe, which leaves no drain to wait for and hands
// this callback none of what the client had buffered.
//
// Between the handler wait and the answers it waits for a live connection (awaitLiveConnection):
// an acknowledgement, negative or not, is a publish, and a stop a connection event runs begins
// while the link is down, where the client refuses one. Each held message is then answered by
// its own kind (heldMessage), over the connection the client holds now (answerHeld):
// acknowledged when its handler finished before the stop released and its own acknowledgement was
// refused, handed back when this process did not finish it.
func (d *intakeDelivery) stop(ctx context.Context) {
	giveUp := context.AfterFunc(ctx, d.abandon)
	defer giveUp()
	d.stopped.Do(func() {
		d.stopping.Store(true)
		draining, err := d.nats.DrainSubscription(d.sub)
		if err != nil {
			slog.Error("dispatch delivery: drain the intake's subscription", "error", err)
		}
		for _, waiting := range d.keys.stop() {
			d.giveBack(waiting.msg)
		}
		// Returns once every handler has, which a handler given up (abandon) does promptly: its
		// GitHub calls and its writes all run under its context.
		d.keys.wait()
		// Only an answer needs a connection. With nothing held and the handle ended rather
		// than drained -- the unsubscribe a give-up with the link down makes, which leaves
		// nats.go delivering this callback nothing more -- there is no acknowledgement to wait
		// for, and waiting would hold this stop's caller through the whole outage: the undo of a
		// bind the server never confirmed runs inside the durable's consumer lease, so no other
		// process could take the durable over for every second of it.
		d.mu.Lock()
		holding := len(d.held)
		d.mu.Unlock()
		if holding > 0 || draining != nil {
			d.awaitLiveConnection(ctx, holding)
		}
		d.mu.Lock()
		d.released = true
		held := d.held
		d.held = nil
		d.mu.Unlock()
		for _, entry := range held {
			d.answerHeld(entry)
		}
		// nats.go ends a drain on a goroutine of its own, once the callback has run every message
		// the client had buffered, and reports it only by the subscription turning invalid.
		waitedFrom := time.Now()
		for draining != nil && draining.IsValid() {
			if d.handling.Err() != nil || time.Since(waitedFrom) >= intakeDrainWait {
				slog.Error("dispatch delivery: the intake's subscription did not finish draining; unsubscribing it", "waited", time.Since(waitedFrom))
				if err := draining.Unsubscribe(); err != nil {
					slog.Error("dispatch delivery: unsubscribe the intake's subscription", "error", err)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		d.abandon()
	})
}

// awaitLiveConnection waits for the client's connection to be live, bounded by ctx and by
// intakeStopBudget. The answers that follow it are publishes, and the bus buffers none while the
// client reconnects (opts.ReconnectBufSize = -1, internal/bus/connect.go): one made over a dead
// link is refused, and the message it would have answered waits out intakeAckWait in the next
// holder's four-slot window (intakeMaxAckPending) rather than being redelivered at once, or, for
// a message whose handler finished, for no reason at all. A stop the process stopping runs has a
// live connection, so its wait returns on the first read; a stop a connection event runs waits
// out the outage, within its own budget.
//
// The budget bounds the wait even where ctx does not. One caller's ctx has no deadline until
// Run's ctx ends: the undo bindDurableConsumer runs for a bind the server never confirmed, which
// runs inside that durable's consumer lease. A link that does not come back would otherwise hold
// that lease for as long as the process lives, and no other process could reconcile the durable.
//
// The signal is taken before the connection's state is read, as ConnectionChanged's own contract
// asks, so a connection installed between the two still wakes the wait. holding is how many
// messages the stop has to answer, for the line a wait the budget ends writes: its caller waits
// at all only while there are messages or a drain that may yet deliver one.
func (d *intakeDelivery) awaitLiveConnection(ctx context.Context, holding int) {
	waiting, cancel := context.WithTimeout(ctx, intakeStopBudget)
	defer cancel()
	waitedFrom := time.Now()
	for {
		changed := d.nats.ConnectionChanged()
		if d.nats.Connected() {
			return
		}
		select {
		case <-waiting.Done():
			slog.Warn("dispatch delivery: no NATS connection to answer the intake's messages over; they wait out the ack wait",
				"messages", holding, "waited", time.Since(waitedFrom), "ack_wait", intakeAckWait())
			return
		case <-changed:
		}
	}
}

// giveBack hands msg back to NATS for the next holder: once no handler of this delivery is
// running, at once; until then it is held, and the stop hands it back with the rest. Handed back
// earlier, a message of a key whose handler is still running here could be redelivered to the
// next holder and run there beside it, and two fetches of one pull request or run at once can
// write the older answer last.
func (d *intakeDelivery) giveBack(msg *natsgo.Msg) {
	d.mu.Lock()
	if !d.released {
		d.held = append(d.held, heldMessage{msg: msg})
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	redeliverAfter(msg, 0)
}

// acknowledgeOrHold acknowledges msg, the message of a handler that has finished, and holds it
// for the stop only when that acknowledgement was refused and this delivery has not released
// what it holds (released). The acknowledgement goes first because holding one protects
// nothing, unlike giveBack, whose hold keeps the next holder from starting a key's next
// message while this process is still on the one before it. A stop with a live link is this
// process shutting down, and there a handler that finished at the first second would otherwise
// wait for the slowest handler or for the budget (intakeStopBudget, up to 15 s) while its
// message holds one of the next holder's four in-flight slots (intakeMaxAckPending) -- during a
// rolling deploy, the slots of the process taking over.
//
// What is held is therefore a refused answer rather than every answer. An acknowledgement is a
// publish, and the stop a connection event runs begins with the link down, where the bus refuses
// one; the message would then stay outstanding for the whole ack wait (intakeAckWait) although
// its handling had finished, and four such handlers would stop the next holder being delivered
// anything. A refused Ack leaves the message unacknowledged -- nats.go sets ackd only where the
// publish succeeded (js.go:3661-3664) -- so the stop can answer it again (answerHeld). Reading
// the refusal rather than the stopping flag also covers the handler that finishes after the link
// has dropped and before the disconnect callback sets stopping, whose acknowledgement is refused
// with no stop yet to hold it.
//
// It covers that handler only once nats.go has noticed the drop, which is all any reading here
// can cover. Until it does, the connection's status is still CONNECTED and the acknowledgement
// is appended to the connection's write buffer, where the publish returns nil unless that append
// flushes a full buffer into the dead socket (nats.go v1.53.1's Conn.publish, nats.go:4649-4661;
// its one refusal before that, atLimitIfUsingPending, is false while the writer holds no pending
// buffer, nats.go:2368-2373). Returning nil, it goes out on a socket that is gone and is lost
// with no error to read, and intakeAckWait is its backstop. From the moment nats.go switches the
// writer to pending and goes to RECONNECTING (processOpErr, nats.go:3556-3574) a publish is
// refused -- the bus sets opts.ReconnectBufSize = -1 (internal/bus/connect.go), so that pending
// buffer is at its limit from the first byte -- and that refusal is the one this reads.
//
// Once the stop has released, there is nobody left to answer the message: the refusal is logged
// and the message handed back, as acknowledge does for every other caller, and the ack wait is
// the backstop when that hand-back is refused too.
func (d *intakeDelivery) acknowledgeOrHold(msg *natsgo.Msg) {
	err := msg.Ack()
	if err == nil {
		return
	}
	d.mu.Lock()
	if !d.released {
		d.held = append(d.held, heldMessage{msg: msg, handled: true})
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	slog.Error("dispatch delivery: acknowledge message", "subject", msg.Subject, "error", err)
	redeliverAfter(msg, 0)
}

// intakeAckBody and intakeNakBody are the bodies NATS reads an acknowledgement and a negative
// acknowledgement as. They are nats.go's own ackAck and ackNak (js.go:3865-3866), which that
// package keeps unexported, so they are named here rather than imported: a held answer is
// published by hand (answerHeld) and cannot go through Msg.Ack or Msg.Nak.
var (
	intakeAckBody = []byte("+ACK")
	intakeNakBody = []byte("-NAK")
)

// answerHeld answers one message the stop held, over the connection the client holds now
// (bus.Client.Connection) rather than over the one the message arrived on. Msg.Ack and Msg.Nak
// publish on the connection of the message's own subscription (sub.conn, js.go:3608-3610), and
// removeSub never changes that field, so where the client closed that connection and dialled a
// replacement (internal/bus/recovery.go's onClosed) the connection awaitLiveConnection returned
// on is the replacement while those answers go out on the closed one and fail with
// ErrConnectionClosed. Every held message, finished or abandoned, would then wait out the whole
// ack wait (intakeAckWait) in the next holder's four-slot window. The answer is published at the
// message's own reply subject, which belongs to the consumer and not to this subscription, so
// any connection of the account can make it and the server takes it after this subscription has
// ended.
//
// An answer this cannot make -- a message with no reply subject, a client with no connection, or
// a publish the connection refuses -- is logged at ERROR with the message and the answer it was:
// this is the last chance the process has to answer, and what follows it is the ack wait.
func (d *intakeDelivery) answerHeld(entry heldMessage) {
	body, answer := intakeNakBody, "hand back"
	if entry.handled {
		body, answer = intakeAckBody, "acknowledge"
	}
	connection := d.nats.Connection()
	switch {
	case entry.msg.Reply == "":
		slog.Error("dispatch delivery: the held message carries no reply subject to answer at",
			"subject", entry.msg.Subject, "answer", answer)
	case connection == nil:
		slog.Error("dispatch delivery: no NATS connection to answer the held message over",
			"subject", entry.msg.Subject, "answer", answer, "ack_wait", intakeAckWait())
	default:
		if err := connection.Publish(entry.msg.Reply, body); err != nil {
			slog.Error("dispatch delivery: answer the held message",
				"subject", entry.msg.Subject, "answer", answer, "ack_wait", intakeAckWait(), "error", err)
		}
	}
}

// intakeDrainWait bounds how long stop waits for the drain to end once every handler has returned.
// By then all that is left is nats.go's: the callback handing back what the client had buffered,
// which never blocks, and the drain's flush to the server. So the bound is the one nats.go itself
// gives a connection's drain, natsgo.DefaultDrainTimeout -- bus connections never set their own --
// rather than intakeMessageTimeout, which bounds a handler, and the handlers are done. The stop's
// context ending cuts it shorter: by then the drain has had the whole handler wait to end.
const intakeDrainWait = natsgo.DefaultDrainTimeout

// receive is the subscription's callback, and only routes: a message once stopping is set is
// handed back (giveBack), a subject intakeKey does not want is acknowledged, and any other message
// goes to its key's handler (deliver).
func (d *intakeDelivery) receive(msg *natsgo.Msg) {
	if d.stopping.Load() {
		d.giveBack(msg)
		return
	}
	key, envelope, wanted := intakeKey(msg)
	if !wanted {
		acknowledge(msg)
		return
	}
	if !d.keys.run(key, intakeMessage{msg: msg, envelope: envelope}) {
		d.giveBack(msg)
	}
}

// intakeKey reads from a message's subject, notifications.github.<owner>.<repo>.<kind>..., and
// from its envelope, whether the intake wants it and the key its handler runs under. Only two
// shapes can carry a kind route handles: a pull request's own subject, pr.<n> and nothing after
// it (pr.<n>.review and pr.<n>.comment carry other kinds, contracts' githubTopic), and a workflow
// run's, workflow.<file>.<action>. Every other subject -- checks, comments, issues, pushes, job
// events -- is not wanted, and is decided with no decode at all.
//
// Every wanted message is decoded, and one whose subject names a repository other than the one
// its payload names is not wanted. That message is the second copy the listener publishes of a
// renamed repository's envelopes, under the repository's former name (internal/ghrename): the
// same event_id, the same dedupe_key and the same payload as the original, differing only in its
// topic. Keyed by its own subject it would key differently from the original, so keyedRunner
// would start a goroutine for each and two handlers would run at once on one pull request's row,
// each doing the full FetchPullRequest and paging the same commits, where the older answer can
// be written last. Dropping the copy loses nothing: the original carries that same event to that
// same handler. The two repositories are compared one subject segment at a time --
// contracts.SanitizeSubjectSegment of the payload's owner and of its name against subject tokens
// 2 and 3, since a name may hold a dot, which the sanitizer replaces -- and without case, as
// GitHub compares repository names.
//
// The envelope decode costs nothing new: the envelope decoded here is handed on (intakeMessage),
// and deliver would otherwise decode it itself. The payload is another matter -- route parses the
// same payload string again for its kind and its fields -- so a pull-request message now pays one
// extra JSON parse, which is what the copy rule costs. A pull request is keyed by its subject's
// repository and number. A workflow run's subject names its file and not its run, so its key is
// its payload's repo and run_id.
//
// A message this cannot judge keeps exactly the behaviour it had before the rule: wanted, and
// keyed as it was keyed then. The canonical repository is the one the payload names, and a
// message whose envelope does not decode, whose payload does not decode, or whose payload names
// no repository that splits into an owner and a name -- an empty repo above all -- names none to
// compare the subject with, so it is handled as it always was rather than dropped on a
// comparison that could not be made. A workflow message whose envelope or payload does not
// decode is keyed by its subject, and its handler logs it; an envelope that does not decode is
// handed on undecoded, so deliver reports the decode failure.
func intakeKey(msg *natsgo.Msg) (key string, envelope *contracts.Envelope, wanted bool) {
	tokens := strings.Split(msg.Subject, ".")
	if len(tokens) < 5 || tokens[0] != "notifications" || tokens[1] != "github" {
		return "", nil, false
	}
	pullRequest := tokens[4] == "pr" && len(tokens) == 6
	workflow := tokens[4] == "workflow" && len(tokens) == 7
	if !pullRequest && !workflow {
		return "", nil, false
	}
	decoded, err := decodeEnvelope(msg.Data)
	if err != nil {
		return subjectKey(msg.Subject, tokens, pullRequest), nil, true
	}
	var event struct {
		Repo  string `json:"repo"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(decoded.Payload), &event); err != nil {
		return subjectKey(msg.Subject, tokens, pullRequest), &decoded, true
	}
	if !subjectNamesRepo(tokens, event.Repo) {
		return "", nil, false
	}
	if workflow {
		return "workflow " + event.Repo + "#" + event.RunID, &decoded, true
	}
	return subjectKey(msg.Subject, tokens, pullRequest), &decoded, true
}

// subjectKey is the key a message keeps when its subject is all there is to key it by: a pull
// request by its subject's repository and number, a workflow run by its whole subject, since
// that subject names the workflow file and not the run. It is built where it is returned, so a
// message pays for the one key it is handled under.
func subjectKey(subject string, tokens []string, pullRequest bool) string {
	if pullRequest {
		return "pr " + tokens[2] + "/" + tokens[3] + "#" + tokens[5]
	}
	return "workflow " + subject
}

// subjectNamesRepo reports whether a GitHub subject's owner and repository tokens (2 and 3) name
// repo, the "<owner>/<name>" a payload carries, each side sanitized as the publisher sanitizes it
// (contracts.SanitizeSubjectSegment) and compared without case. A repo that does not split into
// an owner and a name -- an empty one above all -- names no repository to compare, and answers
// true, so intakeKey wants that message as it always did rather than dropping it.
func subjectNamesRepo(tokens []string, repo string) bool {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return true
	}
	return strings.EqualFold(contracts.SanitizeSubjectSegment(owner), tokens[2]) &&
		strings.EqualFold(contracts.SanitizeSubjectSegment(name), tokens[3])
}

// decodeEnvelope decodes one message's envelope. A var so a test can count the decodes.
var decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
	var envelope contracts.Envelope
	err := json.Unmarshal(data, &envelope)
	return envelope, err
}
