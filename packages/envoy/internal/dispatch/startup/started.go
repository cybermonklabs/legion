// Package startup announces on the bus the build the whole Dispatch server fleet serves
// (LEGION-781, INFRA-555): one server.started envelope each time the commit every live server
// process serves changes. The Legion daemon no longer reads it, since nothing a deployment
// publishes starts a production check's look. "Every live process" is
// the registry's (internal/dispatch/processes): a rolling update that has replaced some pods but
// not all is not yet serving the new build, and a pod that restarts on the build the fleet already
// serves announces nothing.
//
// The announcement is a row of server_build_announcements before it is an envelope: whichever
// process finds the fleet agreeing on the commit it serves itself, and the latest row does not
// name, inserts the row with the relation kinds its build writes as tuples; whichever process
// has a connected bus publishes every row not yet published. So a process whose NATS is down
// announces into the table and leaves the publish to the first process whose bus answers. It
// goes directly through the bus client rather than the outbox: it is no issue's, document's or
// project's event, and the outbox publishes only those.
package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Subject is the one subject Dispatch publishes its fleet's build on, mirrored by
// DISPATCH_SERVER_STARTED_SUBJECT in packages/contracts/src/dispatch-api.ts. A subject of the
// notification stream (bus.StreamSubjects'
// notifications.dispatch.>), so the publish is a JetStream publish with an acknowledgement, and the
// stream's duplicate window recognises a second publish of one announcement.
const Subject = "notifications.dispatch.server.started"

// Source is the envelope source of every Dispatch publish, this one included.
const Source = "dispatch"

// Payload is a server.started envelope's payload, mirrored by DispatchServerStartedPayload in
// packages/contracts/src/dispatch-api.ts: the commit every live server process serves, and when
// the fleet was found serving it (the announcement's announced_at, on the database's clock), in
// UTC to the second. The time is named started_at because the TypeScript schema reads that name.
type Payload struct {
	Commit    string    `json:"commit"`
	StartedAt time.Time `json:"started_at"`
}

// Interval is how often a process looks for a fleet-wide build to announce and for an
// announcement to publish. A variable rather than a constant only so this package's tests can
// shorten it; nothing else sets it.
var Interval = 5 * time.Second

// statementTimeout bounds one pass's announcing transaction, well inside Interval, so a database
// that has stopped answering costs a pass and not the loop: the next tick tries again. It runs on
// the Announcer's clock (Deps.Clock), as publishTimeout does.
const statementTimeout = 3 * time.Second

// publishTimeout bounds one publish: its context, which the bus client gives the reconnect wait,
// the flush and the acknowledgement alike, and once it ends sends nothing. The bus client bounds
// its own publish too, but a client that is still dialling or reconnecting must not hold the
// publishing transaction, and with it every other process's publish, for longer than this: the
// next pass tries again. It runs on the Announcer's clock (Deps.Clock), so a test moves a fake
// past it rather than shortening it.
const publishTimeout = 10 * time.Second

// publishPassTimeout bounds a pass's publishing transaction as a whole, its statements and its
// publishes together, so a database link that stalls mid-pass ends the pass rather than the loop.
// It runs on the Announcer's clock (Deps.Clock), as publishTimeout does.
const publishPassTimeout = 30 * time.Second

// announceLock is the transaction advisory lock a pass takes, with pg_try_advisory_xact_lock,
// before it announces and again before it publishes, so one process at a time does either: two
// processes finding the fleet agreeing at once insert one row, and two with a connected bus
// publish it once. A pass that finds the lock held skips that step, since the holder is doing
// it. It is a single 64-bit key ("DANNOUNC"), apart from every other single-key advisory lock of
// the server for the reason processes' registryLock gives, and apart from that one.
const announceLock int64 = 0x44414E4E4F554E43

// Publisher is what an Announcer publishes through: the bus client.
type Publisher interface {
	// Connected reports whether the client has a live connection. A pass whose publisher has none
	// publishes nothing and takes no lock for it, so a process whose NATS is down never holds the
	// publish back from a process whose NATS answers.
	Connected() bool
	// PublishReportingDuplicateWithin publishes the envelope and returns once the stream has
	// acknowledged it, reporting whether the stream already held it. ctx bounds the whole publish,
	// and a publish whose ctx has ended sends nothing.
	PublishReportingDuplicateWithin(context.Context, contracts.Envelope) (bool, error)
}

// Deps is what an Announcer reads and publishes through.
type Deps struct {
	// Pool is the shared pool the announcements and the registry are read and written through.
	Pool *store.Pool
	// Publisher publishes announcements; nil where NATS is disabled, and such a process announces
	// into the table and publishes nothing.
	Publisher Publisher
	// Clock is the clock every bound of a pass runs on (statementTimeout, publishPassTimeout and
	// publishTimeout): nil is clock.Real(), and a test hands a clocktest.Fake and moves it past a
	// bound.
	Clock clock.Clock
	// Commit is the commit this process serves, the one it registers under (processes.Instance).
	// A process announces a commit only where it is this one, since the kinds it announces are
	// its own build's.
	Commit string
	// TupleKinds answers the relation kinds this build writes as tuples, read when the
	// announcement is inserted: the list graph_edges' tuple arm renders (refs.TupleKinds), which
	// names the installed block schema's pointer kinds and so can change while the process runs.
	// The announcement stores it as tuple_kinds, which is how the release that deletes an old arm
	// of the view reads whether the release a rollback would reach writes that arm's kinds as
	// tuples too; a build of the release before this one inserts no list, and its announcement
	// lists none.
	TupleKinds func() []string
}

// Announcer announces the fleet's build (Run) and remembers the latest announcement's commit
// (FleetCommit) for /healthz.
type Announcer struct {
	deps Deps
	// clock is Deps.Clock, the real clock when that is nil.
	clock clock.Clock
	fleet atomic.Pointer[string]
	// passes counts the passes made (pass, AnnouncerPassed's pass).
	passes atomic.Uint64
}

// AnnouncerPassed is the line an Announcer writes at Debug for each pass it makes (pass), so a test
// can count a process's passes from its log; production's handler, at Info, leaves it out. Its
// fields are pass, the pass's number in this Announcer's life, from 1; found, what the pass found:
// passAnnounceFailed, passNothingUnpublished, passPublished or passPublishFailed; and fleet, the
// latest announcement's commit as the pass left it (FleetCommit), or "" while there is none.
const AnnouncerPassed = "dispatch: the fleet's build announcer made a pass"

// What an announcer pass found (AnnouncerPassed's found).
const (
	// passAnnounceFailed: the announcing transaction failed, so the pass published what it could.
	passAnnounceFailed = "announce failed"
	// passNothingUnpublished: every announcement was published, so the pass published nothing.
	passNothingUnpublished = "nothing unpublished"
	// passPublished: an announcement awaited publishing, and the publish ended without an error
	// (it publishes nothing while the publisher is down or another process holds the lock).
	passPublished = "published"
	// passPublishFailed: an announcement awaited publishing, and the publish failed.
	passPublishFailed = "publish failed"
)

// New returns an Announcer over deps; nothing runs until Run.
func New(deps Deps) *Announcer {
	c := deps.Clock
	if c == nil {
		c = clock.Real()
	}
	return &Announcer{deps: deps, clock: c}
}

// FleetCommit is the commit of the latest announcement as of this process's last pass, nil when
// there is none yet, or before the first pass has read one. It is nil on a nil Announcer.
func (a *Announcer) FleetCommit() *string {
	if a == nil {
		return nil
	}
	return a.fleet.Load()
}

// Run passes at once and then every Interval until ctx ends. Each pass announces the fleet's build
// when the live processes agree on one the latest announcement does not name (announce), then,
// when any announcement is not yet published, publishes every such one (publish). A pass that
// fails is logged and the next one tries again.
func (a *Announcer) Run(ctx context.Context) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		a.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pass is one pass of Run. It writes AnnouncerPassed.
func (a *Announcer) pass(ctx context.Context) {
	found := a.announceAndPublish(ctx)
	n := a.passes.Add(1)
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		fleet := ""
		if commit := a.fleet.Load(); commit != nil {
			fleet = *commit
		}
		slog.Debug(AnnouncerPassed, "pass", n, "found", found, "fleet", fleet)
	}
}

// announceAndPublish is pass's work, and answers what it found.
func (a *Announcer) announceAndPublish(ctx context.Context) string {
	unpublished, err := a.announce(ctx)
	if err != nil && ctx.Err() == nil {
		slog.Warn("dispatch: announce the server fleet's build", "error", err)
	}
	// A fleet with nothing to publish costs no publishing transaction each Interval. A pass whose
	// announce failed cannot tell, so it tries.
	if !unpublished && err == nil {
		return passNothingUnpublished
	}
	found := passPublished
	if err != nil {
		found = passAnnounceFailed
	}
	if err := a.publish(ctx); err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: publish the server fleet's build; the next pass retries", "error", err)
		}
		if found == passPublished {
			found = passPublishFailed
		}
	}
	return found
}

// announce reads the latest announcement and, holding announceLock, inserts a new one when every
// live process serves one non-empty commit that is this process's own and that the latest does
// not name: a fleet that serves two builds is mid-roll, a fleet any of whose processes carries no
// stamp has no build to name, and a fleet still on the announced build has nothing new to say.
// The commit must be this process's own because the row carries the relation kinds this build
// writes as tuples: a process whose heartbeat lapsed is no live process, so the fleet can agree
// on a commit it does not serve, and its row would name that commit with another build's kinds.
// A rollback to a build announced before is a change too, and is announced again under a new
// seq. Without the lock it only reads, since the holder decides. FleetCommit is updated once the
// transaction has committed. It reports whether an announcement awaits publishing: one this pass
// inserted, or one its read found unpublished, which a pass that missed the lock learns as well.
func (a *Announcer) announce(ctx context.Context) (bool, error) {
	ctx, cancel := a.clock.WithTimeout(ctx, statementTimeout)
	defer cancel()
	tx, err := a.deps.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	var locked bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, announceLock).Scan(&locked); err != nil {
		return false, err
	}
	latest, unpublished, err := readAnnouncements(ctx, tx)
	if err != nil {
		return false, err
	}
	if locked {
		live, err := processes.Live(ctx, tx)
		if err != nil {
			return false, err
		}
		if commit, agreed := servedByEvery(live); agreed && commit == a.deps.Commit && (latest == nil || *latest != commit) {
			var seq int64
			if err := tx.QueryRow(ctx, `insert into server_build_announcements (commit, tuple_kinds) values ($1, $2) returning seq`,
				commit, a.deps.TupleKinds()).Scan(&seq); err != nil {
				return false, fmt.Errorf("insert the announcement of %s: %w", commit, err)
			}
			slog.Info("dispatch: every live server process serves one build; announced it", "commit", commit, "seq", seq, "processes", len(live))
			latest, unpublished = &commit, true
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	a.fleet.Store(latest)
	return unpublished, nil
}

// readAnnouncements reads, in one statement, the commit of the announcement with the highest seq
// (nil when there is none) and whether any announcement is unpublished.
func readAnnouncements(ctx context.Context, tx pgx.Tx) (*string, bool, error) {
	var latest *string
	var unpublished bool
	err := tx.QueryRow(ctx, `
		select (select commit from server_build_announcements order by seq desc limit 1),
			exists (select 1 from server_build_announcements where published_at is null)`).Scan(&latest, &unpublished)
	if err != nil {
		return nil, false, fmt.Errorf("read the announcements: %w", err)
	}
	return latest, unpublished, nil
}

// servedByEvery is the commit every live process serves, and whether there is one: at least one
// process is live, and each serves the same non-empty commit.
func servedByEvery(live []processes.Process) (string, bool) {
	if len(live) == 0 || live[0].Commit == "" {
		return "", false
	}
	for _, p := range live[1:] {
		if p.Commit != live[0].Commit {
			return "", false
		}
	}
	return live[0].Commit, true
}

// pending is an announcement not yet published.
type pending struct {
	seq         int64
	commit      string
	announcedAt time.Time
}

// publish publishes every announcement whose published_at is null, oldest first, and sets
// published_at on each one the stream acknowledges, holding announceLock so one process publishes
// at a time. It does nothing while the publisher is absent or disconnected, and nothing when
// another process holds the lock. The first publish that fails ends the pass, so announcements
// reach the stream in their order; the acknowledged ones before it are kept. A publish that
// landed but whose acknowledgement or whose published_at was lost is published again by a later
// pass, and the stream, which keys a Dispatch envelope by its dedupe key, stores it once.
func (a *Announcer) publish(ctx context.Context) error {
	if a.deps.Publisher == nil || !a.deps.Publisher.Connected() {
		return nil
	}
	ctx, cancel := a.clock.WithTimeout(ctx, publishPassTimeout)
	defer cancel()
	tx, err := a.deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	var locked bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, announceLock).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select seq, commit, announced_at from server_build_announcements
		where published_at is null
		order by seq`)
	if err != nil {
		return fmt.Errorf("read the unpublished announcements: %w", err)
	}
	unpublished, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		err := row.Scan(&p.seq, &p.commit, &p.announcedAt)
		return p, err
	})
	if err != nil {
		return fmt.Errorf("read the unpublished announcements: %w", err)
	}
	var failed error
	for _, p := range unpublished {
		item, err := Envelope(p.commit, p.seq, p.announcedAt)
		if err != nil {
			failed = err
			break
		}
		publishCtx, cancelPublish := a.clock.WithTimeout(ctx, publishTimeout)
		duplicate, err := a.deps.Publisher.PublishReportingDuplicateWithin(publishCtx, item)
		cancelPublish()
		if err != nil {
			failed = fmt.Errorf("publish %s: %w", item.EventID, err)
			break
		}
		if _, err := tx.Exec(ctx, `update server_build_announcements set published_at = now() where seq = $1`, p.seq); err != nil {
			failed = fmt.Errorf("record %s published: %w", item.EventID, err)
			break
		}
		slog.Info("dispatch: published the server fleet's build", "commit", p.commit, "seq", p.seq, "event_id", item.EventID, "duplicate", duplicate)
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.Join(failed, err)
	}
	return failed
}

// Envelope is the server.started envelope of announcement seq, which found every live process
// serving commit at announcedAt: its event id, source event id and dedupe key
// dispatch-server-started-<commit>-<seq>, source dispatch, topic Subject, issued at announcedAt,
// the summary `dispatch serves <sha7> on every server process`, the event id as its trace id (as
// the outbox's envelopes carry theirs), and Payload as JSON text. Every field is a function of the
// announcement's row, so every process builds the same envelope for it, and a second publish is
// the same message the stream already holds. Every field the daemon's envelope check requires is
// set; Validate holds it to the contract.
func Envelope(commit string, seq int64, announcedAt time.Time) (contracts.Envelope, error) {
	announcedAt = announcedAt.UTC().Truncate(time.Second)
	payload, err := json.Marshal(Payload{Commit: commit, StartedAt: announcedAt})
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("encode server.started payload: %w", err)
	}
	eventID := fmt.Sprintf("dispatch-server-started-%s-%d", commit, seq)
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         Source,
		SourceEventID:  eventID,
		Topic:          Subject,
		DedupeKey:      eventID,
		IssuedAt:       announcedAt.UnixMilli(),
		PayloadSummary: "dispatch serves " + shortCommit(commit) + " on every server process",
		Payload:        string(payload),
		TraceID:        eventID,
	}
	if err := item.Validate(); err != nil {
		return contracts.Envelope{}, err
	}
	return item, nil
}

// shortCommit is the first seven characters of a commit sha, or the whole of a shorter string.
func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
