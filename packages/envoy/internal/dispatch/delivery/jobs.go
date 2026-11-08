package delivery

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// JobIntake records the jobs of a deploy run Dispatch already tracks as each job finishes, from the
// job events Envoy publishes for a run with no pull request, so a deploy shows once its production
// job finishes rather than when its run ends (LEGION-690). It reads a run's jobs from GitHub, never
// from the event, once per burst: a run's events within jobQuiet of each other are one read, and a
// burst never waits longer than jobMaxWait. A run it does not track costs no GitHub call.
//
// It has a durable consumer of its own rather than a kind in Intake's route: Intake handles at most
// intakeMaxAckPending messages at once with their GitHub calls inline, and a deploy run's thirty
// jobs finishing together would queue every merge behind them. Intake's own durable filters every
// GitHub subject, so it is handed job messages too, and acks them by their subject with no decode
// (intakeKey).
type JobIntake struct {
	nats   *bus.Client
	pool   *store.Pool
	github *githubapp.Client
	clock  jobClock
	// settings is the delivery settings a job event is checked against, shared with the intake
	// (StartIntakes): a message reads them from memory, not from delivery_settings.
	settings *SettingsCache
	// leases is the pool bind takes this durable's consumer lease on (consumerLease), so that one
	// process at a time reconciles this durable's configuration and subscribes to it
	// (bindDurableConsumer). It is the delivery intake's lease pool, but not its lease: a lease
	// name per durable, so that a reconciliation stuck on the other intake's durable does not keep
	// this one from creating its own.
	leases duty.Leases
	// throttle is this intake's own bind-poll state: how recently it reconciled its durable and
	// how recently it said it was standing by for it (bindThrottle).
	throttle *bindThrottle

	// ctx is Run's context: every read a timer starts runs under it, so a stopped process stops them.
	ctx context.Context

	// reads holds a slot for each read in flight. When many runs' bursts end at once, as on the
	// first start after an outage drains its backlog, at most reconcilePartialConcurrency runs'
	// jobs are read from GitHub at a time, the bound the reconcile completes pull requests under;
	// one listing per run at once would pass GitHub's 100 concurrent requests and rate-limit the
	// App.
	reads chan struct{}

	mu      sync.Mutex
	pending map[runKey]*pendingRun
}

// runKey names one run of one repository.
type runKey struct {
	repo  string
	runID int64
}

// pendingRun is one run's burst: when its first event arrived, the production rule the newest
// event read, and the timer that reads the run's jobs once the burst goes quiet or has waited
// jobMaxWait.
type pendingRun struct {
	first      time.Time
	production Production
	timer      jobTimer
}

const (
	jobSubject       = "notifications.github.*.*.job.>"
	jobQuiet         = 2 * time.Second
	jobMaxWait       = 10 * time.Second
	jobMaxAckPending = 64
	// jobAckWait bounds a message the handler never reached, and one the callback was handed
	// after the stop whose hand-back the link refused (deliver, which Naks such a message rather
	// than leaving it unanswered); every other message is acked on receipt, before its checks.
	jobAckWait = 30 * time.Second
	// jobCheckTimeout bounds one message's indexed read (RunTracked).
	jobCheckTimeout = 10 * time.Second
)

// jobClock is the time JobIntake schedules by; a test hands it a clock it moves itself. Its timer
// calls f once the clock reaches at, at once when it already has, so a clock that moves between the
// read and the arm fires the timer all the same (api's hubClock has the same shape).
type jobClock interface {
	Now() time.Time
	At(at time.Time, f func()) jobTimer
}

type jobTimer interface{ Stop() bool }

type realJobClock struct{}

func (realJobClock) Now() time.Time { return time.Now() }

func (realJobClock) At(at time.Time, f func()) jobTimer { return time.AfterFunc(time.Until(at), f) }

// newJobIntake builds a JobIntake on natsClient, a connected client StartIntakes opens for it alone,
// scheduling its reads by clock and checking each job event against settings.
func newJobIntake(natsClient *bus.Client, pool *store.Pool, leases duty.Leases, github *githubapp.Client, clock jobClock, settings *SettingsCache) *JobIntake {
	return &JobIntake{
		nats: natsClient, pool: pool, github: github, clock: clock, settings: settings, leases: leases,
		throttle: &bindThrottle{},
		ctx:      context.Background(), pending: map[runKey]*pendingRun{},
		reads: make(chan struct{}, reconcilePartialConcurrency),
	}
}

// Run binds the durable job consumer and handles job events until ctx is cancelled. An attempt
// that binds nothing -- a failure, or the durable being another process's (bindStandby) -- is
// made again every durableBindInterval, for the life of the process: a standby that outlives its
// holder takes the durable over seconds later. A bind under way as ctx ends ends by the stop
// deadline armed then (stopContext, bindDurableConsumer), as the delivery intake's does.
//
// It binds only while its client has a connection, and gives the subscription up at a connection
// event, for the delivery intake's reasons (Intake.Run): a bind with no connection spends the
// client's whole JetStream request timeout on a ConsumerInfo nothing answers, and a subscription
// left in place over an outage comes back -- re-sent by nats.go inside its own reconnect, or
// bound again by the client on a replacement connection -- beside the process that took the
// durable over meanwhile, so both would deliver every job event of it (INFRA-1551). The give-up
// needs no budget of the delivery intake's kind: this intake acknowledges each message on receipt
// (deliver), so at either give-up it holds nothing in flight to hand back and has nothing to wait
// for. What a drain does hand the callback is what the client had already buffered, after the
// give-up has returned, and there the two give-ups part: at a connection event Run goes on, so
// deliver handles those messages as it does any other, while at the shutdown j.ctx is done and
// deliver hands each of them back, which gives the next holder of the durable them at once.
func (j *JobIntake) Run(ctx context.Context) {
	j.ctx = ctx
	defer j.stopPending()
	stopping, release := stopContext(ctx)
	defer release()
	binds := time.NewTicker(durableBindInterval)
	defer binds.Stop()
	for {
		// Taken before the connection's state is read, as ConnectionChanged's own contract asks,
		// so an event during the bind is not missed.
		connection := j.nats.ConnectionChanged()
		var poll <-chan time.Time
		if j.nats.Connected() {
			sub, outcome := j.bind(ctx, stopping)
			if outcome == bindBound {
				// Nothing polls while this process holds the durable; a give-up starts the ticker
				// again, a full durableBindInterval from the Reset, as Intake.Run's does.
				binds.Stop()
				select {
				case <-ctx.Done():
					j.giveUp(sub)
					return
				case <-connection:
					slog.Info(intakeConnectionChangedMessage, "name", bus.DispatchJobDurable)
					j.giveUp(sub)
					binds.Reset(durableBindInterval)
					continue
				}
			}
			poll = binds.C
		}
		select {
		case <-ctx.Done():
			return
		case <-connection:
		case <-poll:
		}
	}
}

// giveUp ends this process's job subscription through the client, which is what gives up the
// registration the client keeps of it (bus.Client.DrainSubscription): a bare Unsubscribe would
// leave a registration the next reconnect binds again, on a durable this process gave up, and
// sub is itself stale once the client has replaced its connection. The same undo bind hands
// bindDurableConsumer, for the same reason.
func (j *JobIntake) giveUp(sub *natsgo.Subscription) {
	if _, err := j.nats.DrainSubscription(sub); err != nil {
		slog.Error("dispatch delivery: drain the job intake's subscription", "name", bus.DispatchJobDurable, "error", err)
	}
}

// jobConsumerConfig is the durable's policy: deliveryConsumerConfig's shape with this consumer's
// name and filter, starting at new messages, since a job event older than the process says nothing
// the run's end event and the reconcile do not.
func jobConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchJobDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  jobSubject,
		DeliverPolicy:  natsgo.DeliverNewPolicy,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        jobAckWait,
		MaxAckPending:  jobMaxAckPending,
	}
}

// bind creates (on first run) or resumes the durable and subscribes to it, throttling its own
// reconciliation and standby line on this intake's own bindThrottle. stopping is Run's stop
// deadline (bindDurableConsumer).
//
// The undo it hands back is giveUp, and not a bare Unsubscribe, for giveUp's reason: a bind the
// server never confirms must leave the client holding no registration of it.
func (j *JobIntake) bind(ctx, stopping context.Context) (*natsgo.Subscription, bindOutcome) {
	wanted := jobConsumerConfig()
	return bindDurableConsumer(ctx, stopping, j.nats, j.leases, j.throttle, wanted, func() (*natsgo.Subscription, func(), error) {
		sub, err := subscribeDurable(j.nats, wanted, "", j.deliver)
		if err != nil {
			return nil, nil, err
		}
		return sub, func() { j.giveUp(sub) }, nil
	})
}

// deliver acks a job message on receipt, then hands its payload to receive. Acking first is safe:
// the run's end event (Intake) and the reconcile both read every job of a run, so a process that
// dies with a read pending loses only the early signal, and a message never waits on GitHub.
//
// A message that arrives once j.ctx is done is the exception: it is handed back, neither acked
// nor read. The stop drains this subscription (giveUp), so the client hands this callback what
// it had already buffered after Run has returned, where acking would lose the early signal for
// good and the read that follows it would fail under the cancelled context with an ERROR line
// of its own.
//
// What the drain hands this callback does not come back here, because the drain's UNSUB is on
// the wire ahead of it. nats.go v1.53.1's Conn.unsubscribe (:5416-5476) holds the connection's
// lock for the whole of a drain's start (:5427-5429): it marks the subscription draining
// (:5447-5453) and appends the UNSUB to the connection's write buffer, kicking the flusher
// (:5457-5460). The callback is handed what the client had buffered one message at a time after
// that, and this Nak is a publish appended to that same write buffer behind the UNSUB. The
// server therefore sees the unsubscribe first, and redelivers those messages to the next
// subscriber of the durable rather than to this one; the delivery intake's stop turns on the
// same order (intake_delivery.go's stop, which drains before it answers anything).
//
// A message handed to this callback while the subscription is still bound is the other case:
// one the server delivers after j.ctx is done and before Run reaches giveUp (Run's ctx case),
// or during a bind confirmation under a context already cancelled. Its Nak reaches the server
// with this subscriber's interest still in place, so the server may hand it here once more
// before the UNSUB lands, and it is handed back again. That is bounded -- the hand-back after
// the UNSUB goes to the next subscriber -- and loses nothing, since neither pass reads the
// message.
//
// A Nak the link refuses -- which is the state a give-up over an outage begins in -- leaves the
// message outstanding, as the bare unsubscribe this give-up replaced left it: the next holder is
// handed it when jobAckWait expires, and the run's end event and the five-minute reconcile both
// cover it in any case.
func (j *JobIntake) deliver(msg *natsgo.Msg) {
	if j.ctx.Err() != nil {
		negativelyAcknowledge(msg)
		return
	}
	if err := msg.Ack(); err != nil {
		slog.Error("dispatch delivery: acknowledge job message", "subject", msg.Subject, "error", err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		slog.Error("dispatch delivery: decode job envelope", "subject", msg.Subject, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(j.ctx, jobCheckTimeout)
	defer cancel()
	j.receive(ctx, envelope.Payload)
}

// receive schedules a read of the run a job event names, when it is a run of the deploy repository
// Dispatch already tracks: the repository is checked against the cached settings, and any other
// event costs at most one indexed read (RunTracked).
func (j *JobIntake) receive(ctx context.Context, payload string) {
	var job struct {
		Kind  string `json:"kind"`
		Repo  string `json:"repo"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(payload), &job); err != nil || job.Kind != "job" {
		return
	}
	settings := j.settings.Load()
	if settings == nil || job.Repo != settings.DeployRepo {
		return
	}
	runID, err := strconv.ParseInt(job.RunID, 10, 64)
	if err != nil {
		slog.Warn("dispatch delivery: job event with a malformed run_id", "run_id", job.RunID)
		return
	}
	tracked, err := RunTracked(ctx, j.pool, job.Repo, runID)
	if err != nil {
		slog.Error("dispatch delivery: read whether a job's run is tracked", "repo", job.Repo, "run_id", runID, "error", err)
		return
	}
	if !tracked {
		return
	}
	j.schedule(runKey{repo: job.Repo, runID: runID}, settings.Production())
}

// schedule starts a run's burst or moves its read to jobQuiet after this event, but never past
// jobMaxWait after the burst's first event. A later event replaces production so a settings update
// observed before that event governs the deferred read.
func (j *JobIntake) schedule(key runKey, production Production) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.clock.Now()
	run, ok := j.pending[key]
	if !ok {
		run = &pendingRun{first: now}
		j.pending[key] = run
	} else {
		run.timer.Stop()
	}
	run.production = production
	due := now.Add(jobQuiet)
	if limit := run.first.Add(jobMaxWait); limit.Before(due) {
		due = limit
	}
	run.timer = j.clock.At(due, func() { j.fire(key, run) })
}

// fire ends a run's burst and reads its jobs once a read slot is free (reads). An event that
// arrives during the read starts the next burst; two reads of one run that overlap are safe, since
// a read never undoes a later one (UpsertRunJobs). The read's deadline starts once it has its slot.
func (j *JobIntake) fire(key runKey, run *pendingRun) {
	j.mu.Lock()
	if j.pending[key] != run {
		j.mu.Unlock()
		return
	}
	delete(j.pending, key)
	j.mu.Unlock()
	select {
	case j.reads <- struct{}{}:
	case <-j.ctx.Done():
		return
	}
	defer func() { <-j.reads }()
	ctx, cancel := context.WithTimeout(j.ctx, intakeMessageTimeout)
	defer cancel()
	j.read(ctx, key, run.production)
}

// read lists the run's jobs from GitHub and stores that listing. It marks the run's jobs complete
// only when the same listing shows every job finished, so a listing taken before the run ended
// never marks the run complete because another consumer recorded its end meanwhile.
func (j *JobIntake) read(ctx context.Context, key runKey, production Production) {
	owner, repo, err := splitRepo(key.repo)
	if err != nil {
		slog.Error("dispatch delivery: job run repository", "repo", key.repo, "error", err)
		return
	}
	fetched, err := ListWorkflowRunJobs(ctx, j.github, owner, repo, key.runID)
	if err != nil {
		slog.Warn("dispatch delivery: read the jobs of a running deploy run; its end reads them again", "repo", key.repo, "run_id", key.runID, "error", err)
		return
	}
	jobs := RunJobs(key.repo, key.runID, fetched, production)
	if err := UpsertRunJobs(ctx, j.pool, key.repo, key.runID, jobs, JobsComplete(jobs)); err != nil {
		slog.Error("dispatch delivery: store the jobs of a running deploy run", "repo", key.repo, "run_id", key.runID, "error", err)
	}
}

// stopPending stops every burst's timer once Run ends.
func (j *JobIntake) stopPending() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for key, run := range j.pending {
		run.timer.Stop()
		delete(j.pending, key)
	}
}
