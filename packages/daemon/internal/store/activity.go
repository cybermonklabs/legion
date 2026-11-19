package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// PutClaimActivity writes the activity columns of token's claim row (activity_at,
// activity_sent_at, activity_sent_generation, activity_background, activity_kinds and
// activity_next_reminder_at) and its updated_at, and nothing else: what that claim's session last
// reported of what it is doing. The report is no part of the claim's machine state — it arrives
// between a machine's writes, and the machine's in-memory claim does not carry it — so one write of
// the whole row would erase every report sent since that machine read its claim; the claim's own
// writes carry no report and leave these columns alone but for a launch's, which clears all of them
// but activity_sent_generation (putClaim), and the idle rule reads them through its own view
// (IdleClaim).
//
// It writes only onto the launch the report belongs to, generation: a report of the claim's
// earlier launch — a dead process's, authenticated just before the relaunch — is never written
// over the launch's clear and the new process's empty report, since the update's own condition
// names the generation.
//
// A report older than the one recorded is dropped rather than written: a session fires its
// reports without an in-flight guard, so an earlier reading can land after a later one, and the
// pod would then be released on a reading the session has already replaced. Older is by the
// session's own clock (activity_sent_at), the one clock that orders its readings, and only
// within one launch (activity_sent_generation): a launch the release before this column made
// leaves the time the process before it reported at, which says nothing of the launched one.
//
// A session's clock can also step backwards within one launch, and then every reading it takes
// after the step is dated before the one recorded. The daemon's receipt tells such a reading from
// an earlier one overtaken in flight. A reading's receipt runs ahead of its session time by the
// session clock's offset from the daemon's and the time the report spent in flight: a reading
// overtaken on the same clock runs further ahead than the one recorded only by how much longer
// it was in flight, and one taken after a step back runs further ahead by the whole step. So a
// reading dated before the one recorded is written when its receipt runs further ahead of its
// session time than the recorded one's by more than reportOvertakenWithin, and is dropped
// otherwise. A step back longer than that bound drops no reading, and a shorter one drops the
// session's readings until its clock passes the recorded time again, for no longer than the step.
//
// It answers whether the store held the claim at generation when the statement began, which is
// not always whether it wrote. The answer is the outer select's, read from the snapshot the
// statement took as it began, while an update that waits on a row lock checks its condition
// again against the row the lock's holder committed (Postgres's READ COMMITTED). So a report of
// the earlier launch whose statement began after the launch's write committed finds no row at
// generation and is answered unknown, as the route answers a secret of a launch that is gone;
// and one whose update waited behind that write's row lock writes nothing, since the row it
// reads again is at the next generation, and is answered known all the same, so the route
// answers it 204. A report dropped as older than the one recorded is answered known too.
//
// The report is dated by received, the moment the daemon received it on its own clock (the
// route's, api's claims/activity), never by the session's clock, which the rules that read the
// report compare with the daemon's own times: a pod whose clock runs more than 20 minutes behind
// would otherwise send reports that read as 20 minutes old the moment they arrive, and the
// relaunch guard's turn report rule would let a relaunch cut a turn whose process still runs
// (supervise.RelaunchGuard). The session's own time orders its reports alone (activity_sent_at),
// and is bounded to the receipt, as the listener's is (decodeListenerWake): the session chooses
// every field of what it sends, and one reading dated ahead of the daemon's clock — a session
// whose host stepped its clock forward, corrected back afterwards — would drop every honest report
// after it under that ordering guard, while the stale reading it left behind went on counting as
// current, background work and all.
func (s *Store) PutClaimActivity(ctx context.Context, token claim.Token, generation uint64, a claim.SessionActivity, received time.Time) (bool, error) {
	if generation > math.MaxInt64 {
		// No row holds it (putClaim refuses one), so no launch of the claim is this.
		return false, nil
	}
	var reminder *time.Time
	if a.Phase.NextReminderAt != nil && !a.Phase.NextReminderAt.IsZero() {
		at := a.Phase.NextReminderAt.UTC()
		reminder = &at
	}
	received = received.UTC()
	sent := a.At.UTC()
	if sent.After(received) {
		sent = received
	}
	var known bool
	err := s.pool.QueryRow(ctx, `with reported as (
		update claims set activity_at = $2, activity_sent_at = $7, activity_sent_generation = $6, activity_background = $3,
			activity_kinds = $4, activity_next_reminder_at = $5, updated_at = now()
		where token = $1 and generation = $6
			and (activity_sent_at is null or activity_sent_generation is distinct from $6 or activity_sent_at <= $7
				or $2 - $7 > activity_at - activity_sent_at + $8::interval) returning token)
		select exists (select 1 from claims where token = $1 and generation = $6)`,
		// Only the three known kinds are kept (claim.BackgroundWork.KnownKinds): they reach the
		// architect's worker-background-stalled notice and the daemon's log.
		string(token), received, a.BackgroundWork.Running,
		strings.Join(a.BackgroundWork.KnownKinds(), ","), reminder, int64(generation), sent, reportOvertakenWithin,
	).Scan(&known)
	if err != nil {
		return false, fmt.Errorf("put the activity of claim %s: %w", token, err)
	}
	return known, nil
}

// reportOvertakenWithin is the most by which an earlier reading of a session's, overtaken in
// flight by a later one, can have taken longer to reach the daemon than the reading that
// overtook it, and still be dropped as that earlier reading (PutClaimActivity); a reading dated
// before the one recorded that ran further behind its receipt is taken as the session's clock
// stepped back. It is the period of a turn's heartbeat report (pi-shared's
// TURN_ACTIVITY_HEARTBEAT_MS), so a shorter step drops the readings of no more than one heartbeat,
// inside the 20 minutes the relaunch guard reads a turn's report for (supervise.UnreachableAfter),
// while a reading overtaken by more would be one a report took minutes longer to send than the
// reading after it.
const reportOvertakenWithin = 5 * time.Minute

// ClaimActivity is the activity report the store holds for launch generation of token's claim
// (PutClaimActivity's columns, the phase reminder aside), and whether it holds one: what a
// machine reads of the process it is letting go, which its own claim carries only as the machine
// read it at boot (supervise.Deps.ClaimActivity, the machine's recordEnd). It is read before the
// launch's own write, which clears the report as it moves the generation (putClaim).
//
// Only a report of that launch is answered. The columns hold the report of the launch the row
// records and of no other — a report is written onto the launch it was authenticated for alone
// (PutClaimActivity), and the write that moves the generation clears them — so a row at another
// generation, a row whose launch has reported nothing (activity_at null) and a token no claim
// holds all answer no report.
func (s *Store) ClaimActivity(ctx context.Context, token claim.Token, generation uint64) (supervise.ActivityReport, bool, error) {
	if generation > math.MaxInt64 {
		// No row holds it (putClaim refuses one), so no launch of the claim is this.
		return supervise.ActivityReport{}, false, nil
	}
	var (
		reportedAt *time.Time
		report     supervise.ActivityReport
		kinds      string
	)
	err := s.pool.QueryRow(ctx, `select activity_at, activity_background, activity_kinds from claims
		where token = $1 and generation = $2`, string(token), int64(generation)).Scan(&reportedAt, &report.Background, &kinds)
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.ActivityReport{}, false, nil
	}
	if err != nil {
		return supervise.ActivityReport{}, false, fmt.Errorf("read the activity of claim %s at generation %d: %w", token, generation, err)
	}
	if reportedAt == nil {
		return supervise.ActivityReport{}, false, nil
	}
	report.At = *reportedAt
	// Kept comma-joined in the session's order, as scanClaim reads them: no kind is no slice.
	if kinds != "" {
		report.Kinds = strings.Split(kinds, ",")
	}
	return report, true, nil
}
