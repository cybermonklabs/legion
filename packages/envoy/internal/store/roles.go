package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// RoleClaim is the durable ownership record for a role lane. Role claims are
// independent of interest records: a listener restart can temporarily age an
// interest out before its still-running holder re-registers.
type RoleClaim struct {
	HolderSessionID   string `json:"holder_session_id"`
	ClaimedAt         int64  `json:"claimed_at"`
	PreviousSessionID string `json:"previous_session_id"`
	// GrantID names the grant that wrote this claim: an id each grant draws at random for the
	// record it writes (SetRoleWithPrevious), which the holder's renewals, the adoption of a
	// claim that names no process and the hand-back's mark carry over untouched. It is what
	// tells a claim granted again from the claim a release read before its write lost - the
	// delete that frees the claim and the hand-back's mark alike (RoleClaim.sameGrant,
	// Registry.releaseReadRoleClaim, Registry.markRoleClaimHandedBack) - which ClaimedAt cannot:
	// a grant stamped in the millisecond of the claim before it carries that claim's moment.
	// Empty on a claim an earlier build wrote: that build draws no grant id, and its renewal and
	// its mark write back a claim decoded without the field, so a record it rewrites loses the
	// id. Such a claim renews, is adopted, released and marked as it was.
	GrantID string `json:"grant_id,omitempty"`
	// ClientInstanceID is the client instance of the process that holds the claim, which the
	// claimant named when it took the role. A session id alone does not name that process: a
	// second process can resume a live session's id, and it carries a client instance of its
	// own, so this field is what tells the two apart and keeps the second from taking,
	// releasing or reaping the first's role. Empty where the claimant named none - a claim an
	// earlier build wrote, or one whose caller has no instance - and an empty instance is
	// never evidence that two claims come from the same process.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// RenewedAt is the wall clock in milliseconds at which the holder's process last answered
	// for this claim; a claim write stamps it with that write's own ClaimedAt. Zero means the
	// record carries no renewal, which a claim an earlier build wrote does not, and such a
	// claim is read at the time it was claimed rather than at the epoch: every reader of a
	// renewal reads it through LastAnsweredAt, which says so.
	RenewedAt int64 `json:"renewed_at,omitempty"`
	// RenewedWithoutProcess is the note a registration that states no process leaves on a
	// claim that names none when it renews it (Registry.renewRoleClaim): the claim's holder
	// answers for it without saying which process it is, so from then on no registration that
	// states a process adopts it. It is absent on every other claim. Every grant writes a fresh
	// record without it (SetRoleWithPrevious), and a claim an earlier build stored has none
	// either, so the first registration of its session that states a process adopts it: the
	// one heartbeat in which a claim that names no process is open to any process of its
	// session. A note rather than a comparison of RenewedAt with ClaimedAt, because a clock
	// that does not move, and two listeners whose clocks differ, make those two equal or
	// reversed for a claim that was renewed.
	RenewedWithoutProcess bool `json:"renewed_without_process,omitempty"`
	// HandedBack is the mark a hand-back left on this claim: the release named this role's own
	// lane topic and stated no process, so it was not authorised to free the claim
	// (RoleReleaseAuthority.frees) and marked it instead (Registry.markRoleClaimHandedBack),
	// whether the claim names a process or none. It is written by that release alone - never by
	// the administrator's force, which frees the claim outright, and never by the empty-topics
	// deregistration. Its readers are the renewal, which renews and adopts no marked claim
	// whoever registers (Registry.renewRoleClaim), so the role claim reaper frees the role once
	// that renewal has lapsed, a live window and up to one reap interval later, and GET
	// /v1/roles/<role>, which reports it as handed_back. Absent means no hand-back of that shape
	// has reached this claim: a claim granted since clears the mark, because every grant writes
	// a fresh record (SetRoleWithPrevious).
	HandedBack bool `json:"handed_back,omitempty"`
	// Revision is the role bucket revision the claim was read at, which RoleClaim stamps on
	// every claim it decodes and nothing stores: it belongs to the entry, not to the record,
	// so it is never marshalled back into the bucket. Zero means no claim record was read -
	// an unclaimed role, or a claim built to be written. It is what a conditional
	// claim names as its precondition (SetRoleWithPrevious's ifClaimRevision) and what GET
	// /v1/roles/<role> reports as claim_revision, so a caller can read a role and then claim
	// it only while no other claim has landed.
	Revision uint64 `json:"-"`
}

// LastAnsweredAt is the wall clock in milliseconds at which this claim's holder last answered for
// it: RenewedAt where the record carries one, and ClaimedAt, the moment the claim was written,
// where it does not. It is the one reading of a claim's renewal, so a claim stored before this
// build, which carries no RenewedAt, is read at the time it was claimed rather than at the epoch,
// where every window would already have passed. A claim carrying neither is zero, which is older
// than any window.
func (c RoleClaim) LastAnsweredAt() int64 {
	if c.RenewedAt > 0 {
		return c.RenewedAt
	}
	return c.ClaimedAt
}

// HeldByProcess reports whether instance names the very process this claim records as the
// holder's. It is the one strict test of a claim against a process, which every decision that
// weighs the two reads through: the release (RoleReleaseAuthority.frees), a claim of a role held
// under the claimant's own session id (POST /v1/roles/set), and the renewal
// (Registry.renewRoleClaim, which also adopts a claim that names no process at its first
// heartbeat).
//
// An absent id on either side names no process, so it matches nothing: a claim whose claimant
// stated none names none, and a caller that states none says nothing about which process it is.
// That is the opposite of session.InstanceMatches, the loose test the predecessor rule reads,
// where an absent id on either side is a client predating the field and the claim goes through.
func (c RoleClaim) HeldByProcess(instance string) bool {
	return instance != "" && c.ClientInstanceID == instance
}

// sameGrant reports whether c is the grant read is, as the holder's renewals and the adoption
// leave it: the question a release asks of the claim it reads again after a write of its own lost
// its compare-and-swap, the delete that frees the claim (Registry.releaseReadRoleClaim) and the
// mark a hand-back leaves on it (Registry.markRoleClaimHandedBack) alike. Where c carries a grant
// id, that id decides, so a grant made since read, which drew an id of its own, is never read's,
// whatever millisecond it was stamped in. Where c carries none, an earlier build wrote it - its
// grant draws no id, and its renewal drops the one it read - so ClaimedAt decides, as it did in
// that build. Two grants in read's own millisecond therefore read as read while such a build
// writes the bucket: one that build wrote, and one this build wrote that such a build renewed
// before this read, which has lost its id. No code here can tell either apart; both end when no
// such build writes the bucket.
func (c RoleClaim) sameGrant(read RoleClaim) bool {
	if c.GrantID != "" {
		return c.GrantID == read.GrantID
	}
	return c.ClaimedAt == read.ClaimedAt
}

// ExpiredRoleClaimRelease reports what releaseReadExpiredRoleClaim's conditional delete did to
// an expired role claim: ExpiredRoleClaimReleased is the delete that landed, the one outcome the
// sweep counts as reaped (ReapRoleClaims); ExpiredRoleClaimMissing a role whose record was gone
// before the delete, or that never had one; ExpiredRoleClaimSuperseded a claim written again
// between the caller's read and the delete, which the delete's own revision kept; and
// ExpiredRoleClaimRetained a claim left standing, either for the restart grace of a listener
// that has just opened (sessionTTL) or by a delete that failed, which the error beside it names.
type ExpiredRoleClaimRelease uint8

const (
	ExpiredRoleClaimReleased ExpiredRoleClaimRelease = iota
	ExpiredRoleClaimMissing
	ExpiredRoleClaimSuperseded
	ExpiredRoleClaimRetained
)

// roleRevisions reads the revision of every role claim kv holds, for the grace a restored claim's
// holder gets to register again: one run of the role claim scan Open makes (scanRoleClaims). It
// takes them from one scan over the bucket's existing keys (kvwatch.ScanExistingKeys), the way the
// interest, session and CI caches read theirs, so readiness costs one pass over the bucket rather
// than a round trip per stored claim. Each run starts from an empty result, so a claim an earlier
// run read reaches the start only if this run reads it too. A claim whose key this build cannot
// read gets a revision here; it still gets no grace, because the grace is read by
// releaseReadExpiredRoleClaim, the only reader of these revisions, over a claim its caller has read
// from the bucket, and no caller can read that one.
//
// A snapshot short of the bucket is missing claims that are in it, and each one it misses is a
// restored holder that loses its grace and is released a session TTL early, so an incomplete run
// returns an error and no revisions. A run that delivers nothing for idle is one, and so is one
// whose ctx ended, which is how scanRoleClaims ends a run, and one whose watch could not be made.
// Every error it returns names the role claim scan (roleClaimScanName): kvwatch.ScanExistingKeys
// puts that name at the head of each error of a watch it made, and returns the error of a watch it
// could not make as nats.go gave it, which this names. Beside the revisions it returns what the
// scan delivered (kvwatch.KeyScan), whose count of delete markers the log line of the run Open
// keeps carries. handed, which only this package's tests set (roleScanSeams), is called after each
// claim the run is handed with how many it has been handed so far.
func roleRevisions(ctx context.Context, kv bus.KeyValue, idle time.Duration, log *slog.Logger, handed func(claims int)) (map[string]uint64, kvwatch.KeyScan, error) {
	revisions := map[string]uint64{}
	claims := 0
	scan, err := kvwatch.ScanExistingKeys(ctx, kv, jetstream.AllKeys, roleClaimScanName, idle, log, func(entry kvwatch.Entry) error {
		revisions[entry.Key] = entry.Revision
		claims++
		if handed != nil {
			handed(claims)
		}
		return nil
	}, jetstream.MetaOnly())
	if err != nil {
		if !strings.HasPrefix(err.Error(), roleClaimScanName+": ") {
			err = fmt.Errorf("%s: the bucket's watch could not be made: %w", roleClaimScanName, err)
		}
		return nil, kvwatch.KeyScan{}, err
	}
	return revisions, scan, nil
}

// roleClaimScanName is the name every error of the role claim scan begins with, so the listener's
// exit on a failed start says which read of the start failed.
const roleClaimScanName = "role claim scan"

// roleScanSeams are the role claim scan's test seams (scanRoleClaims), each nil, and so off,
// unless this package's tests set them through Open's options (withRoleScanSeams, in
// role_claim_scan_test.go). The listener and every other caller of Open set none.
type roleScanSeams struct {
	// watch wraps the role claim bucket's handle every run reads, so a test paces or stalls the
	// runs' watches.
	watch func(bus.KeyValue) bus.KeyValue
	// run is called as each run starts, with the run's number, the first run's 1, and the scan's
	// start (roleClaimScan.started), the one clock read its deadline is roleClaimScanDeadline
	// after, so a test measures the scan's bounds from that read rather than from a read of its
	// own, which comes later.
	run func(n int, started time.Time)
	// handed is called on a run's own goroutine after each claim the run is handed, with the run's
	// number and how many claims it has been handed, so a test acts at a point of a run. A run the
	// scan ended can still call it while its teardown finishes (roleClaimScan.run).
	handed func(run, claims int)
	// noticed is called on the scan's goroutine when the scan first takes run n, while it is in
	// progress, for one the connection left its server or reconnected during.
	noticed func(run int)
}

// The role claim scan's bounds (scanRoleClaims). roleClaimScanRuns is the most runs it makes.
// roleClaimScanDeadline, counted from the first run's start, bounds the path a reconnect takes: a
// wait for the connection, and a run during which the connection left its server or reconnected.
// It is 37 times the slowest whole scan of the deployment's bucket measured on 2026-11-22 (1,863 ms
// for 17,454 entries), so it holds four such runs and the waits between them. roleClaimScanPoll is
// how often the scan reads the connection (readConn), well inside the second in which it must
// notice a reconnect while nothing holds the connection's lock; the connection's own notices
// (followConn) are what it notices one by while a handshake holds it.
const (
	roleClaimScanRuns     = 4
	roleClaimScanDeadline = 70 * time.Second
	roleClaimScanPoll     = 250 * time.Millisecond
)

// errRoleClaimScanDeadline is what a start fails with when the role claim scan's deadline
// (roleClaimScanDeadline) ends it: a wait for the connection still under way there, or a run that
// the connection left its server or reconnected during and that no run read the whole bucket after
// by then.
var errRoleClaimScanDeadline = errors.New("the scan's deadline passed")

// errRoleClaimScanLateReconnect is what a start fails with when the connection left its server or
// reconnected during a run of the role claim scan after its deadline: the scan ends that run at
// once and makes no other.
var errRoleClaimScanLateReconnect = errors.New("the NATS connection reconnected, or began to, after the scan's deadline")

// scanRoleClaims is the role claim scan Open makes once it has opened the buckets: the revision of
// each role claim, read by runs of roleRevisions over kv through conn, the connection Open was
// given. The scan that failed the webhook listener's start on 2026-11-22 met a reconnect: the
// Service in front of the hub switched the connection to another server a second in, and no further
// entry came before the idle bound. A run that spans a reconnect is not shown to have read the
// whole bucket, complete or not: either the idle bound fires first, or nats.go's activity check
// makes the watch's ordered consumer again at the next stream sequence on whatever server the
// connection now reaches (nats.go v1.55.0 js.go, activityCheck), where that sequence need not name
// the same entry. So such a run is made again, rather than trusted or failed.
//
// A reconnect is a rise in the connection's own count of reconnects (nats.Conn.Stats), which the
// scan reads with the connection's status before each run, after it, and every roleClaimScanPoll
// while it is in progress (readConn). Each of those readings waits on the connection's lock, and
// nats.go holds that lock through a reconnect attempt's dial and handshake (nats.go v1.55.0
// doReconnect), so behind a server that does not answer the handshake no reading comes for as long
// as the connection's Timeout, 5 s for a bus client. So the scan also takes the connection's own
// notices of its status (nats.Conn.StatusChanged, followConn), which nats.go sends without
// waiting, and which reach the scan whatever holds the lock: a notice that the connection has left
// its server (RECONNECTING or DISCONNECTED) during a run counts as a reconnect of that run, since
// the run either ends while the connection is not connected or spans the reconnect that follows,
// and either makes it again. So the scan notices a reconnect during a run within a second, before
// nats.go even raises the count. nats.go raises the count once a reconnect's TCP connection
// succeeds, before its handshake, so a rise is judged together with the status. What ends a run
// decides what follows, in this order:
//
//   - On a closed connection the start fails: a closed connection never reconnects, and the bus
//     client's replacement of one (bus.Client) is a new *nats.Conn, not a reconnect of this one.
//   - A run that ended, complete or not, while the connection was not connected makes the scan
//     wait for it to be connected again (awaitConnected), and then run again.
//   - A run during which the connection left its server or reconnected, or that began while it
//     was not connected, is made again.
//   - A run that ended incomplete, with no reconnect and the connection connected, fails the start
//     with its own error, as the scan did when it made one run only.
//   - A run that ended complete, with no reconnect and the connection connected, gives the start
//     its revisions, and its size is what the restore's log line gives.
//
// Each run starts from an empty result (roleRevisions), and the scan makes at most
// roleClaimScanRuns: a last run it does not keep fails the start. The path a reconnect takes ends
// roleClaimScanDeadline after the first run started: a wait for the connection ends there, and so
// does a run in progress during which the connection has already left its server or reconnected,
// each failing the start (errRoleClaimScanDeadline). A run with no reconnect so far goes on with
// the idle bound alone, as the scan's one run did before; if the connection leaves its server or
// reconnects during it after the deadline, the scan ends it at once and the start fails
// (errRoleClaimScanLateReconnect). So a run that meets no reconnect, on a connection that stays
// connected, keeps the idle bound and no other however long it lasts, and the reconnect path ends
// within roleClaimScanDeadline of the first run's start, or within a second of a reconnect after
// it. Every error it returns names the role claim scan.
func scanRoleClaims(conn *nats.Conn, kv bus.KeyValue, idle time.Duration, log *slog.Logger, seams roleScanSeams) (map[string]uint64, error) {
	if seams.watch != nil {
		kv = seams.watch(kv)
	}
	// The notices are followed before the first reading, so no change between the two is missed.
	changes, unfollow := followConn(conn)
	defer unfollow()
	stop := make(chan struct{})
	defer close(stop)
	scan := &roleClaimScan{kv: kv, idle: idle, log: log, seams: seams, readings: readConn(conn, stop), changes: changes}
	before := <-scan.readings
	scan.started = time.Now()
	scan.deadline = scan.started.Add(roleClaimScanDeadline)
	for n := 1; ; n++ {
		run, err := scan.run(n, before)
		if err != nil {
			return nil, err
		}
		var again string
		switch {
		case run.after.status == nats.CLOSED:
			return nil, roleClaimScanClosed(n, run.err)
		case run.after.status != nats.CONNECTED:
			again = "the run ended while the NATS connection was not connected"
		case !run.reconnected.IsZero():
			again = "the NATS connection left its server or reconnected while the run was in progress"
		case run.err != nil:
			return nil, run.err
		default:
			log.Info("restored role claims", slog.Int("restored", len(run.revisions)), slog.Int("delete_markers", run.scan.Markers))
			return run.revisions, nil
		}
		if n == roleClaimScanRuns {
			if run.err != nil {
				return nil, fmt.Errorf("role claim scan: none of its %d runs ended complete with no reconnect of the NATS connection; in the last, %s: %w", roleClaimScanRuns, again, run.err)
			}
			return nil, fmt.Errorf("role claim scan: none of its %d runs ended complete with no reconnect of the NATS connection; in the last, %s", roleClaimScanRuns, again)
		}
		if !run.reconnected.IsZero() && !run.reconnected.Before(scan.deadline) {
			return nil, scan.lateReconnect(n)
		}
		before = run.after
		if run.after.status != nats.CONNECTED {
			if before, err = scan.awaitConnected(n); err != nil {
				return nil, err
			}
		} else if !time.Now().Before(scan.deadline) {
			return nil, scan.spannedPastDeadline(n)
		}
		attrs := []any{slog.Int("run", n), slog.String("reason", again)}
		if run.err != nil {
			attrs = append(attrs, slog.String("error", run.err.Error()))
		}
		log.Warn("the role claim scan runs again", attrs...)
	}
}

// roleClaimScan is one role claim scan across its runs (scanRoleClaims).
type roleClaimScan struct {
	kv    bus.KeyValue
	idle  time.Duration
	log   *slog.Logger
	seams roleScanSeams
	// readings carries the readings of the connection readConn takes, one every roleClaimScanPoll.
	readings <-chan connReading
	// changes is what the connection's own notices of its status have said (followConn).
	changes *connChanges
	// started is the first run's start: the scan's one clock read for its bounds.
	started time.Time
	// deadline is roleClaimScanDeadline after started.
	deadline time.Time
}

// roleClaimRun is how one run of the scan ended: the run's own result, the first reading of the
// connection begun after the run ended, and when the scan first saw the connection leave its server
// or reconnect while the run was in progress, zero when it saw neither.
type roleClaimRun struct {
	revisions   map[string]uint64
	scan        kvwatch.KeyScan
	err         error
	after       connReading
	reconnected time.Time
}

// run makes run n of the scan, starting from before, the reading of the connection the run starts
// from, and returns how it ended (roleClaimRun). The scan ends a run itself in three cases only, and
// returns the start's error for it: a closed connection, which no run can read through; the
// deadline, when the connection has already left its server or reconnected during the run; and
// either of those after the deadline, as soon as a notice or a reading shows it. A run the scan ends
// returns at once, and its watch's teardown finishes on the run's own goroutine (end). A run that
// begins while the connection is not connected is taken for one a reconnect spans, since the
// reconnect that ends that state can raise the count before the run starts.
func (s *roleClaimScan) run(n int, before connReading) (roleClaimRun, error) {
	if s.seams.run != nil {
		s.seams.run(n, s.started)
	}
	var handed func(int)
	if s.seams.handed != nil {
		handed = func(claims int) { s.seams.handed(n, claims) }
	}
	left, _ := s.changes.read()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan roleClaimRun, 1)
	go func() {
		revisions, scan, err := roleRevisions(ctx, s.kv, s.idle, s.log, handed)
		ended <- roleClaimRun{revisions: revisions, scan: scan, err: err}
	}()
	// end ends the run in progress with err, the start's error, and returns at once. Cancelling ctx
	// ends the run's scan, which then stops its watch and drains it (kvwatch.ScanExistingKeys), and
	// the stop unsubscribes through the connection, so it waits on the connection's lock, which
	// nats.go holds through a reconnect's dial and handshake, each up to the connection's Timeout.
	// The run's goroutine is left to that teardown and ends with it, handing its result to ended,
	// which holds one and which nothing reads any more, so the start fails when the scan says it
	// does, not when a handshake lets the lock go, and the goroutine outlives it by no more than
	// the attempt that holds the lock.
	end := func(err error) (roleClaimRun, error) {
		cancel()
		return roleClaimRun{}, err
	}
	var reconnected time.Time
	// spanned records that the connection left its server or reconnected during the run, and
	// reports whether that came after the deadline.
	spanned := func() bool {
		reconnected = time.Now()
		if s.seams.noticed != nil {
			s.seams.noticed(n)
		}
		return !reconnected.Before(s.deadline)
	}
	if before.status != nats.CONNECTED {
		spanned()
	}
	deadline := time.NewTimer(time.Until(s.deadline))
	defer deadline.Stop()
	for {
		select {
		case run := <-ended:
			run.reconnected = reconnected
			return s.after(n, before, run)
		case <-s.changes.changed:
			now, closed := s.changes.read()
			if closed {
				return end(roleClaimScanClosed(n, nil))
			}
			if !reconnected.IsZero() || now == left {
				continue
			}
			if spanned() {
				return end(s.lateReconnect(n))
			}
		case reading := <-s.readings:
			if reading.status == nats.CLOSED {
				return end(roleClaimScanClosed(n, nil))
			}
			if !reconnected.IsZero() || reading.reconnects <= before.reconnects {
				continue
			}
			if spanned() {
				return end(s.lateReconnect(n))
			}
		case <-deadline.C:
			if !reconnected.IsZero() {
				return end(s.spannedPastDeadline(n))
			}
		}
	}
}

// after completes run, which has just ended: it waits for the first reading of the connection
// begun after the run ended, which decides what follows, and records a reconnect that reading or
// any before it shows. A run the connection left its server or reconnected during is held to the
// deadline while it waits, since a reading waits on the connection's lock, which nats.go holds
// through a dial and a handshake; a notice that the connection closed ends the wait at once.
func (s *roleClaimScan) after(n int, before connReading, run roleClaimRun) (roleClaimRun, error) {
	endedAt := time.Now()
	var deadline <-chan time.Time
	if !run.reconnected.IsZero() {
		timer := time.NewTimer(time.Until(s.deadline))
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case reading := <-s.readings:
			if run.reconnected.IsZero() && reading.reconnects > before.reconnects {
				run.reconnected = time.Now()
			}
			if reading.at.Before(endedAt) {
				continue
			}
			run.after = reading
			return run, nil
		case <-s.changes.changed:
			if _, closed := s.changes.read(); closed {
				return roleClaimRun{}, roleClaimScanClosed(n, run.err)
			}
		case <-deadline:
			return roleClaimRun{}, s.spannedPastDeadline(n)
		}
	}
}

// awaitConnected waits, after run n ended while the connection was not connected, for a reading
// that shows it connected again, which the next run starts from, until the deadline. A notice that
// the connection closed ends the wait at once.
func (s *roleClaimScan) awaitConnected(n int) (connReading, error) {
	notConnected := fmt.Errorf("role claim scan: run %d ended while the NATS connection was not connected, and it was not connected again %s after the first run started: %w", n, roleClaimScanDeadline, errRoleClaimScanDeadline)
	deadline := time.NewTimer(time.Until(s.deadline))
	defer deadline.Stop()
	for {
		select {
		case reading := <-s.readings:
			switch {
			case reading.status == nats.CLOSED:
				return connReading{}, roleClaimScanClosed(n, nil)
			case !time.Now().Before(s.deadline):
				return connReading{}, notConnected
			case reading.status == nats.CONNECTED:
				return reading, nil
			}
		case <-s.changes.changed:
			if _, closed := s.changes.read(); closed {
				return connReading{}, roleClaimScanClosed(n, nil)
			}
		case <-deadline.C:
			return connReading{}, notConnected
		}
	}
}

// spannedPastDeadline is the error of a start whose run n the connection left its server or
// reconnected during, and whose deadline came before any run read the whole bucket.
func (s *roleClaimScan) spannedPastDeadline(n int) error {
	return fmt.Errorf("role claim scan: the NATS connection left its server or reconnected during run %d, and no run had read the whole bucket %s after the first run started: %w", n, roleClaimScanDeadline, errRoleClaimScanDeadline)
}

// lateReconnect is the error of a start whose connection left its server or reconnected during run
// n after the deadline: the scan ends that run at once if it is still in progress, and makes no run
// after it.
func (s *roleClaimScan) lateReconnect(n int) error {
	return fmt.Errorf("role claim scan: the NATS connection left its server or reconnected during run %d, more than %s after the first run started, and no run may follow a reconnect past the deadline: %w", n, roleClaimScanDeadline, errRoleClaimScanLateReconnect)
}

// roleClaimScanClosed is the error of a start whose connection closed during run n. runErr is the
// run's own error, when the run ended before the scan saw the close.
func roleClaimScanClosed(n int, runErr error) error {
	if runErr != nil {
		return fmt.Errorf("role claim scan: run %d ended on a closed NATS connection, which never reconnects (the run: %v): %w", n, runErr, nats.ErrConnectionClosed)
	}
	return fmt.Errorf("role claim scan: run %d ended on a closed NATS connection, which never reconnects: %w", n, nats.ErrConnectionClosed)
}

// connReading is one reading of the connection the role claim scan reads through: its status, then
// its count of reconnects (nats.Conn.Stats), read in that order, and the time the reading began.
// nats.go raises the count before it marks a reconnected connection connected, so a reading that
// shows the connection connected shows every reconnect that completed before it.
type connReading struct {
	status     nats.Status
	reconnects uint64
	at         time.Time
}

// readConn reads conn every roleClaimScanPoll and hands each reading on, until stop closes. It
// reads on a goroutine of its own because a reading waits on the connection's lock, which nats.go
// holds through a reconnect's dial and handshake, each up to the connection's Timeout (5 s for a
// bus client): read inline, a reading could hold the scan past its deadline. For the same reason no
// reading comes while a handshake holds the lock, and the connection's own notices (followConn)
// are what the scan notices a reconnect by then.
func readConn(conn *nats.Conn, stop <-chan struct{}) <-chan connReading {
	readings := make(chan connReading)
	go func() {
		for {
			at := time.Now()
			status := conn.Status()
			reading := connReading{status: status, reconnects: conn.Stats().Reconnects, at: at}
			select {
			case readings <- reading:
			case <-stop:
				return
			}
			select {
			case <-time.After(roleClaimScanPoll):
			case <-stop:
				return
			}
		}
	}()
	return readings
}

// connChanges is what the connection's own notices of its status have said since the role claim
// scan began to follow them (followConn): how many times it has left its server, and whether it
// has closed. changed carries a signal after each notice, so a waiter reads the two again.
type connChanges struct {
	mu      sync.Mutex
	left    int
	closed  bool
	changed chan struct{}
}

// read returns how many times the connection has left its server, and whether it has closed.
func (c *connChanges) read() (left int, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.left, c.closed
}

// followConn follows conn's own notices of its status for the role claim scan
// (nats.Conn.StatusChanged): that it has left its server (RECONNECTING, DISCONNECTED) and that it
// has closed (CLOSED), until the func it returns is called. nats.go sends each notice under the
// connection's lock without waiting, into a channel of ten from which a notice is dropped when it
// is full, so a notice reaches the scan while a reconnect's handshake holds that lock, which no
// reading of the connection does (readConn); a goroutine of its own takes each at once, so none
// is dropped. The listener is the scan's own: the connection's handlers (nats.Options'
// DisconnectedErrCB and the rest) are the bus client's. Registering it takes the connection's
// lock, as a reading does. Removing it does too, so that is left to a goroutine of its own: a scan
// that ends while a handshake holds the lock, as one ended by a reconnect after the deadline can,
// does not wait for that handshake.
func followConn(conn *nats.Conn) (*connChanges, func()) {
	notices := conn.StatusChanged(nats.RECONNECTING, nats.DISCONNECTED, nats.CLOSED)
	c := &connChanges{changed: make(chan struct{}, 1)}
	go func() {
		for status := range notices {
			c.mu.Lock()
			if status == nats.CLOSED {
				c.closed = true
			} else {
				c.left++
			}
			c.mu.Unlock()
			select {
			case c.changed <- struct{}{}:
			default:
			}
		}
	}()
	return c, func() { go conn.RemoveStatusListener(notices) }
}

// scannedRoleClaim is one role claim record as a scan of the role bucket delivered it: the key it
// is stored under, the claim decoded from its value with the entry's revision stamped on it, and
// the error DecodeRoleClaim answered, nil when the record decoded. A sweep reading from the scan
// has of each record exactly what a Get of its key gave it before - the value and the revision -
// so the decode failure travels with the record rather than failing the scan: a stored record this
// build cannot read costs its own role and no more, as it did when each key was read on its own.
type scannedRoleClaim struct {
	Role  string
	Claim RoleClaim
	Err   error
}

// roleClaimsUntil lists every role claim the role bucket holds, sorted by role, from one scan of
// its existing keys (kvwatch.ScanExistingKeys) with delete markers left out: nats.go's Keys runs
// its legacy watcher (bus.KeyValue says why none may run). An empty bucket lists no claim. A scan
// short of the bucket fails the caller rather than list part of it, where nats.go's Keys would
// hand over what had arrived when its watcher gave up, so a sweep never takes a partial list for
// every claim.
//
// The scan carries each record's value, and not its metadata alone, because its callers want the
// claim: a listing of keys costs every caller a Get of its own per key, which is a round trip per
// stored claim per pass against the bucket whose read load is what this reads one pass of instead.
//
// It refuses at the first key it is handed once ctx has ended, so a listing its caller walked
// away from ends there. The scan must end on its own account, because it has a context of its
// own (kvwatch.ScanExistingKeys builds one): one left running goes on holding an ordered
// consumer and appending to a map nobody reads until the bucket's last key arrives or its idle
// bound elapses (kvwatch.ScanIdleTimeout, 10 s), while the client's retry starts a second scan
// of the whole bucket - against the bucket already too far behind to serve the claim view, which
// is the only reason the deregistration's listing runs at all. The error a visit returns ends
// the scan, and the scan then stops and drains its watcher (kvwatch.drainScan), so the listing
// costs the bucket nothing past the answer its caller already has.
//
// The claims are collected into a map keyed by the stored key, the higher revision winning, and
// only then sorted into a slice. The map is what makes a key delivered twice one claim: an ordered
// consumer delivers a key's last-per-subject entry and then any update to it that lands while the
// scan runs, and a sweep handed that key twice would act on the role twice, the second time at the
// older of the two revisions. The slice is sorted by role because that is the order a listing of
// the bucket's keys returned, which the sweeps' callers read their logs and their fixtures in.
//
// Nothing an entry's value aliases is retained: kvwatch.Entry.Value shares the read entry's bytes
// (kvwatch/entry.go), while RoleClaim holds only strings encoding/json allocated out of them. What
// is held to the end of the scan is one scannedRoleClaim per stored claim - a role string, the
// claim's two strings and its two numbers - which at the fleet's ~5,000 claims is under about 1 MB.
func (r *Registry) roleClaimsUntil(ctx context.Context) ([]scannedRoleClaim, error) {
	claims := map[string]scannedRoleClaim{}
	_, err := kvwatch.ScanExistingKeys(context.Background(), r.roles(), jetstream.AllKeys, "role claims", kvwatch.ScanIdleTimeout, r.logger(), func(entry kvwatch.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scanned, ok := claims[entry.Key]; ok && scanned.Claim.Revision >= entry.Revision {
			return nil
		}
		claim, decodeErr := DecodeRoleClaim(entry.Value)
		claim.Revision = entry.Revision
		claims[entry.Key] = scannedRoleClaim{Role: entry.Key, Claim: claim, Err: decodeErr}
		return nil
	}, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(maps.Values(claims), func(a, b scannedRoleClaim) int { return strings.Compare(a.Role, b.Role) }), nil
}

// ErrMalformedRoleClaim is a role claim record the bucket holds and this build cannot read: one
// that names no holder, or JSON that is not a claim. The bus answered it, so it is not a read that
// failed, and reading it again returns the same record; each error wrapping it names which.
var ErrMalformedRoleClaim = errors.New("malformed role claim")

// DecodeRoleClaim reads a stored role claim record: a structured claim naming a holder, or a bare
// session id an earlier build stored. Any other record is ErrMalformedRoleClaim. Dispatch's outbox
// reads the role bucket through it too (roles.Buckets), so the two readers cannot drift.
//
// A record that carries no client instance id, no renewal, or neither is read, not refused: a
// claim an earlier build wrote names no process and no renewal, and both fields are left zero, as
// they are for the bare session id. A missing field is never ErrMalformedRoleClaim, which is a
// record this build cannot read at all.
func DecodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	err := json.Unmarshal(value, &claim)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("%w: it names no holder", ErrMalformedRoleClaim)
		}
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("%w: %w", ErrMalformedRoleClaim, err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("%w: a legacy record with an empty holder", ErrMalformedRoleClaim)
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

// newRoleHolderCopy is an empty copy of the role claim bucket, which keeps the session each claim
// it can read names. A claim this build cannot read (ErrMalformedRoleClaim) gives its role to
// nobody, so the copy keeps no entry for it, as a delete marker leaves none. Each entry it removes
// is handed to r's repair (roleClaimRepair.swept), which counts the ones a repairing read's sweep
// took out.
func newRoleHolderCopy(r *Registry) kvwatch.Copy[string] {
	return kvwatch.NewCopy(kvwatch.Rows[string]{Decode: func(entry kvwatch.Entry) (string, bool, error) {
		claim, err := DecodeRoleClaim(entry.Value)
		if err != nil {
			return "", false, err
		}
		return claim.HolderSessionID, true, nil
	}, Removed: r.roleRepair.swept})
}

// roleCache is the registry as the role claim cache its watcher feeds (kvwatch.Cache). The hooks
// are its own type's, not the registry's methods, so nothing but the watcher calls them. Beside the
// copy they count what each whole read of the bucket repaired in it (roleClaimRepair), which
// RoleClaimRepair reports.
type roleCache struct{ r *Registry }

// BeginRead starts a read of the role claim bucket (kvwatch.Copy.BeginRead). Every holder stays.
// Whether the read repairs the copy, and so counts what it repairs, is the repair's to say
// (roleClaimRepair.begin).
func (c roleCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.BeginRead(read)
	r.roleRepair.begin(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a claim it could
// not read, which gave its role to nobody, as the interest watcher logs a value it cannot decode.
// What the record did to the role's entry is handed to the repair while a read that repairs is
// under way (roleClaimRepair.delivered); a record delivered while the copy follows its bucket
// costs no look at the entry beside its own write.
func (c roleCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	var malformed error
	if r.roleRepair.repairing {
		before, held := r.roleHolders.Get(entry.Key)
		malformed = r.roleHolders.Delivered(entry)
		after, holds := r.roleHolders.Get(entry.Key)
		r.roleRepair.delivered(entry.Key, before, held, after, holds)
	} else {
		malformed = r.roleHolders.Delivered(entry)
	}
	r.holdersMu.Unlock()
	if malformed != nil {
		r.logger().Warn("role claim watcher found a claim it cannot read; it names no holder",
			slog.String("role", entry.Key),
			slog.Uint64("revision", entry.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// CompleteRead removes every holder and fence entry the read found no evidence of: the bucket no
// longer holds that role's claim. The repair runs that sweep (roleClaimRepair.sweep), so a read
// that repairs counts each holder the sweep removes as taken out (roleClaimRepair.swept), and no
// removal outside the sweep is counted so. Every completed read, whether it repaired or filled the
// copy, is recorded as the last on the registry's clock, with what it repaired
// (roleClaimRepair.complete); a read that repaired anything logs it at INFO, with the counts and
// the first roleClaimRepairLoggedRoles of the roles it repaired.
func (c roleCache) CompleteRead(read kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	r.roleRepair.sweep(&r.roleHolders)
	found := r.roleRepair.complete(read, r.now())
	r.holdersMu.Unlock()
	if found.putBack > 0 || found.tookOut > 0 {
		r.logger().Info(fmt.Sprintf("role claim repair put back %d claims and took out %d", found.putBack, found.tookOut),
			slog.Int("put_back", found.putBack),
			slog.Int("took_out", found.tookOut),
			slog.Any("put_back_roles", found.putBackRoles),
			slog.Any("took_out_roles", found.tookOutRoles),
		)
	}
}

// setOwnRoleHolder records a claim write this registry made through a handle of epoch
// (kvwatch.Copy.Put): a write through a handle on a bucket since recreated is dropped rather than
// fencing out the new bucket's lower revisions, and so is one the fence finds older than the
// holder the copy already took.
func (r *Registry) setOwnRoleHolder(role, holder string, revision, epoch uint64) {
	if r.roleWatcher == nil {
		return
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.Put(r.roleWatcher, epoch, role, holder, revision)
}

// forgetRoleHolder records that the claim of role at revision was deleted through a handle of
// epoch. A delete does not return its marker's revision, so the view is fenced one past the
// revision deleted, as deleteInterest fences the interest cache: a claim written since is later
// still, and applies.
func (r *Registry) forgetRoleHolder(role string, revision, epoch uint64) {
	if r.roleWatcher == nil {
		return
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.Evict(r.roleWatcher, epoch, role, revision+1)
}

// roleViewEpoch is the epoch a claim write through the role handle carries, and 0 when no watcher
// follows the claims, which only a registry a unit test built leaves: such a write reaches no
// view (setOwnRoleHolder, forgetRoleHolder).
func (r *Registry) roleViewEpoch() uint64 {
	if r.roleWatcher == nil {
		return 0
	}
	_, epoch := r.roleWatcher.Handle()
	return epoch
}

// roleClaimWrite is one conditional write of one role's claim, as its caller means it: which
// records the write may be made over, the write itself, and what the view owes a write that
// landed. Three callers make such a write - the release's delete (releaseReadRoleClaim), the
// hand-back's mark (markRoleClaimHandedBack) and the renewal (renewRoleClaim) - and what they
// share is the protocol, not the meaning: the bounded call, the bucket's answers read one way,
// the bounded re-read, and the bound on attempts. writeRoleClaim runs that, each caller states
// the rest here, and nothing is flattened: a lost race means something different to each of
// them, which each says by what it does with the outcome.
type roleClaimWrite struct {
	// role is the claim's key: what the write is sent against and what a re-read reads.
	role string
	// attempts is how many conditional writes may be sent in all, counting the first. One is a
	// write that is never retried, which is what a lost race means to the renewal: the record
	// is another claimant's now, and a renewal written into it would stamp this process's
	// heartbeat on a claim it does not hold.
	attempts int
	// writes reports whether this write may be made over claim, the record as it was last
	// read. It is asked before every attempt, so a retry is made only over a record its caller
	// still means to write, and a caller comparing the re-read against the claim it first read
	// closes over that one.
	writes func(claim RoleClaim) bool
	// send sends the write itself, conditional on the revision claim was read at, and answers
	// the revision the record stands at after it, as the bucket's writes do; a delete answers
	// none and its caller reads the revision it deleted off claim.
	send func(claim RoleClaim) (uint64, error)
	// landed moves this registry's own copy of the role's holder to what the write left
	// (setOwnRoleHolder, forgetRoleHolder), over the claim the write was made at, the revision
	// it answered and the epoch of the handle it was sent through, read before the write.
	landed func(claim RoleClaim, revision, epoch uint64)
}

// roleClaimWriteOutcome is what one run of writeRoleClaim did to the role's record, for the
// callers that answer for each of them differently. A failure is the zero value and comes with
// the error that caused it; every other outcome carries none.
type roleClaimWriteOutcome uint8

const (
	// roleClaimWriteFailed is a bucket call that did not answer, a re-read that failed, or an
	// answer that is neither a landed write nor a lost race. Nothing was written.
	roleClaimWriteFailed roleClaimWriteOutcome = iota
	// roleClaimWriteLanded is the conditional write the bucket took; landed has been called.
	roleClaimWriteLanded
	// roleClaimWriteStood is a record this write may not be made over: the caller's own test
	// refused it, or no record claims the role at all (a zero revision), which no conditional
	// write can be made over either. The claim writeRoleClaim answers beside it is the record
	// it stopped at, which is the caller's to read.
	roleClaimWriteStood
	// roleClaimWriteMissing is a write the bucket answered nats.ErrKeyNotFound: the record was
	// gone when it arrived.
	roleClaimWriteMissing
	// roleClaimWriteRaced is every attempt lost to a write that landed since the read it was
	// conditional on (bus.IsCASConflict), the last of them at the attempt bound.
	roleClaimWriteRaced
)

// writeRoleClaim sends w over claim, the record its caller read, conditional on the revision
// that read returned, and sends it again over a re-read of the record while the write loses
// that condition and w allows another attempt. Each bucket call waits no longer than ctx
// (bus.Bounded); a call ctx abandoned may still land after this answer, and it is conditional on
// the revision it was sent at, so it touches no record written since - each caller says what
// that costs it.
//
// It answers what became of the record (roleClaimWriteOutcome), the claim it stopped at, and the
// error of a call that failed. The claim is the caller's own when nothing was re-read, and the
// re-read's record otherwise, so a caller that must weigh the record it left reads it here
// rather than reading the bucket again.
func (r *Registry) writeRoleClaim(ctx context.Context, claim RoleClaim, w roleClaimWrite) (roleClaimWriteOutcome, RoleClaim, error) {
	for attempt := 1; ; attempt++ {
		if claim.Revision == 0 || !w.writes(claim) {
			return roleClaimWriteStood, claim, nil
		}
		at := claim
		epoch := r.roleViewEpoch()
		revision, answered, err := bus.Bounded(ctx, r.roles().Conn(), func() (uint64, error) { return w.send(at) })
		switch {
		case !answered:
			return roleClaimWriteFailed, claim, err
		case err == nil:
			w.landed(at, revision, epoch)
			return roleClaimWriteLanded, at, nil
		case errors.Is(err, nats.ErrKeyNotFound):
			return roleClaimWriteMissing, claim, nil
		case !bus.IsCASConflict(err):
			return roleClaimWriteFailed, claim, err
		case attempt == w.attempts:
			return roleClaimWriteRaced, claim, nil
		}
		if claim, err = r.RoleClaim(ctx, w.role); err != nil {
			return roleClaimWriteFailed, claim, err
		}
	}
}

// ErrRoleViewNotReady is RolesHeldBy's answer when its view of the role claims cannot be served,
// so it cannot say which roles a session holds: the watcher has not completed a read of the bucket,
// its copy has not followed the bucket for longer than the stale bound
// (kvwatch.Watcher.Servable), the watcher has ended and not been replaced, or no watcher follows
// the claims. It is a read that did not answer, never the answer that a session holds no roles.
var ErrRoleViewNotReady = errors.New("the role claim view cannot be served")

// RolesHeldBy returns the roles whose claims name sessionID, sorted, from the role claim
// watcher's copy of the bucket, so it makes no bucket call. A session holds a role exactly when
// the role's claim names it; its interest record's topics say nothing about that. It answers only
// while that copy can be served (kvwatch.Watcher.Servable): a read of the bucket has completed and
// the copy follows it, or stopped following it no longer ago than the stale bound. Otherwise it
// returns ErrRoleViewNotReady, wrapped, with the reason RoleViewErr gives, the one test of the
// view's state, which /healthz makes too. A read whose scan timed out is read again by the
// watcher itself, so a view it refuses becomes servable without a Rewatch.
//
// The copy keeps every holder through a read and removes the ones the read found no evidence of
// only once it completes (roleCache), so a read of it during a refill answers the copy it had, not
// a part of the bucket, and no answer is ever a session's roles stripped by a read in flight.
func (r *Registry) RolesHeldBy(sessionID string) ([]string, error) {
	if err := r.RoleViewErr(); err != nil {
		return nil, err
	}
	roles := []string{}
	r.holdersMu.RLock()
	for role, holder := range r.roleHolders.All() {
		if holder == sessionID {
			roles = append(roles, role)
		}
	}
	r.holdersMu.RUnlock()
	sort.Strings(roles)
	return roles, nil
}

// RolesBySession is RolesHeldBy for every session at once: each holder's roles, sorted, read in
// one pass over the claim copy under one lock, for a caller that would otherwise ask per row
// (GET /v1/sessions, whose answer carries every listed session's roles). It refuses on the same
// terms, and a session no claim names is simply absent from the map, which a caller reads as no
// roles.
func (r *Registry) RolesBySession() (map[string][]string, error) {
	if err := r.RoleViewErr(); err != nil {
		return nil, err
	}
	held := map[string][]string{}
	r.holdersMu.RLock()
	for role, holder := range r.roleHolders.All() {
		held[holder] = append(held[holder], role)
	}
	r.holdersMu.RUnlock()
	for _, roles := range held {
		sort.Strings(roles)
	}
	return held, nil
}

// RoleViewErr is nil while the role claim view would answer a read, and otherwise the wrapped
// ErrRoleViewNotReady that RolesHeldBy refuses that read with, naming the same reason: it is the
// one test of the view's state, so a report that the view answers and a read it refuses can never
// disagree. It makes no bucket call, so a caller reports the view without waiting on the bus.
//
// /healthz reports it as a degraded dependency, and it is deliberately not part of Ping or
// WatchErr: those feed the listener's self-health monitor, which restarts the listener over a
// terminal failure that survives its recovery attempts, and no rebuild completes a read the
// bucket keeps timing out on. What does complete it is the role claim watcher's own re-read,
// which needs nothing from the listener.
func (r *Registry) RoleViewErr() error {
	if r.roleWatcher == nil {
		return fmt.Errorf("%w: no watcher follows the role claims", ErrRoleViewNotReady)
	}
	if err := r.roleWatcher.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	if err := r.roleWatcher.Servable(); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	return nil
}

// ErrRoleHeld reports a soft claim that found the role held by another session.
// The holder is carried on the error so callers can report who has it.
type ErrRoleHeld struct {
	Role   string
	Holder string
}

func (e *ErrRoleHeld) Error() string {
	return fmt.Sprintf("role %s is held by %s", e.Role, e.Holder)
}

// ErrRoleClaimRevisionMismatch reports a conditional claim the role bucket did not hold the
// revision for: the caller named the revision it had read the role's claim at
// (SetRoleWithPrevious's ifClaimRevision) and the bucket holds another one, either when the claim
// was read or by the time its conditional write reached the bucket. Expected is the revision the
// caller named, Actual the revision the bucket holds as this call last read it - zero on either
// side names no claim record at all - and Holder the session the stored claim names, empty when
// there is none. No claim record was written: the role is left exactly as the claim that stands
// left it, and a claim that lost the write's compare-and-swap rolled its own topic back unless
// the winning claim names the claimant itself.
type ErrRoleClaimRevisionMismatch struct {
	Role     string
	Expected uint64
	Actual   uint64
	Holder   string
}

func (e *ErrRoleClaimRevisionMismatch) Error() string {
	return fmt.Sprintf("role %s's claim is at revision %d, not the %d this claim required", e.Role, e.Actual, e.Expected)
}

// RoleClaim reads role's claim from the bucket, waiting no longer than ctx (bus.Bounded). The
// claim it returns carries the entry's revision (RoleClaim.Revision), which is what a conditional
// claim and GET /v1/roles/<role> compare against; a role with no claim record returns a zero
// claim without an error, so a zero revision names a role no record claims, and a record that is
// not a claim returns ErrMalformedRoleClaim.
//
// That revision is the whole of what the entry the read answered says beyond the claim itself,
// which is why this hands back no entry: a caller testing whether the bucket holds a record for
// the role reads claim.Revision == 0, and one writing conditionally over the record it read names
// claim.Revision, as releaseReadRoleClaim already does.
func (r *Registry) RoleClaim(ctx context.Context, role string) (RoleClaim, error) {
	roles := r.roles()
	entry, answered, err := bus.Bounded(ctx, roles.Conn(), func() (nats.KeyValueEntry, error) { return roles.Get(role) })
	if !answered {
		return RoleClaim{}, fmt.Errorf("read role %s's claim: %w", role, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil
	}
	if err != nil {
		return RoleClaim{}, err
	}
	claim, err := DecodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, err
	}
	claim.Revision = entry.Revision()
	return claim, nil
}

// RoleClaimFromLeader reads role's claim as RoleClaim does, from the role bucket stream's leader
// (bus.KeyValue.ReadFromLeader) rather than by a direct get, which on a replicated bucket any copy
// near enough to current answers: a copy that has not applied a takeover answers the previous
// holder's claim. It is the read for a decision a stale claim would get wrong for good, such as the
// session relay's judgement of whether a role message still belongs to the session it was stored
// for. A role with no claim, or a delete or purge marker, is the zero claim without an error; a
// message the server wrote under the role's key is ErrMalformedRoleClaim, as a value no claim
// decodes from is.
func (r *Registry) RoleClaimFromLeader(ctx context.Context, role string) (RoleClaim, error) {
	leader, err := r.roles().ReadFromLeader(ctx, role)
	switch {
	case err != nil:
		return RoleClaim{}, fmt.Errorf("read role %s's claim: %w", role, err)
	case leader.Kind == bus.LeaderAbsent || leader.Kind == bus.LeaderMarker:
		return RoleClaim{}, nil
	case leader.Kind == bus.LeaderServerWritten:
		return RoleClaim{}, fmt.Errorf("%w: role %s's key holds a message the server wrote", ErrMalformedRoleClaim, role)
	}
	claim, err := DecodeRoleClaim(leader.Value)
	if err != nil {
		return RoleClaim{}, err
	}
	claim.Revision = leader.Revision
	return claim, nil
}

// RoleHolder returns the authoritative session ID for role, read with no deadline of its own. Only
// tests call it, to read what a claim left behind; the listener reads RoleClaim within a deadline,
// its routes and its role lane through the one resolution (roles.ResolveHolder, internal/roles)
// and its kept lane directly. An unclaimed role returns an empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(context.Background(), role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}
