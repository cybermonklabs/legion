package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var _ supervise.Store = (*Store)(nil)

// claimSelect reads a claim with the delivery pending on it, if any: the two tables are written
// separately (a delivery changes far more often than the claim it rides on) and read together.
// It does not read workspace_lost: nothing carries a disk forward for a launch to find lost
// (INFRA-80). The column stays unread rather than dropped — migrations here are forward-only, and
// the previous release's daemon still reads its own claims table, so dropping it would make
// rolling the image back need a database restore.
// The two marks of how a claim answered its update round are an exception, dropped by
// migration 20261113110610 once no rule read them: an operator who rolls the image back runs the
// statements in docs/kubernetes.md under "Rolling back the release whose update round ends at its
// bound" first, which add them again and let a roll forward run the migration again, and needs no
// restore.
//
// The four activity columns are read here beside the eight step columns and under the same rule
// (putClaim's own comment): PutClaimActivity and PutClaimStep own them, and a machine's write
// leaves them alone, but for a launch's, which clears the four activity columns. Reading them is
// what lets the daemon's minute sweep see what a quiet worker's own session says it is waiting
// on (supervise.Claim's ActivityAt and the three beside it, internal/daemon's
// tellSilentPhaseWorkers).
//
// Whether the delivery is a task of news alone (supervise.Delivery.NewsOnly) is the delivery's own
// column (news_only), written and read beside the news it carries, so no writer of that news can
// drop the mark. What the prompt a confirmation answers carried of that news
// (supervise.Delivery.Carried), and whether that turn's result said it ran (supervise.Delivery.Ran),
// are read beside both (carried_news and carried_ran, design_news_told.go).
const claimSelect = `select c.token, c.project, c.tree, c.issue, c.role, c.generation, c.tree_epoch, c.session,
	c.session_file, c.locator, c.state, c.failure, c.pod_end, c.failure_cause, c.failed_at, c.pending_cause, c.leak_check_cause,
	c.launch_failures, c.deaths, c.prompt_failures, c.prompt_retires,
	c.boot_token_hash, c.capability_hash, c.uncertain_streak, c.last_start_row, c.serving_generation,
	c.given_entry, c.served_entry, c.handshake_entry, c.handshake_generation, c.handshake_queued_at,
	c.handshake_confirmed_at, c.serving_check_look, c.serving_check_opened_at,
	c.agent_secrets_enrollment, c.agent_secrets_enrollment_incarnation, c.suspended_by,
	c.plugin_contract, c.plugin_relaunch_for, c.agent_resume_generation, c.session_agent, c.delivered_rows,
	c.last_turn_start, c.last_turn_end,
	c.last_death_cause, c.last_death_at, c.last_death_digest,
	c.last_start_failure, c.last_start_failure_at, c.last_start_failure_digest, c.last_ready,
	c.turn_outcomes, c.abandoned_session, c.ended_work, c.role_change,
	c.launched_definition, c.launched_generation,
	c.update_round, c.update_reasons, c.update_opened_at, c.update_tasked_at, c.update_signalled_at,
	c.update_head, c.update_told_at,
	c.update_left_round, c.provider_error_run, c.death_charged,
	c.step_at, c.turn_started_at, c.step_tool, c.step_call_id, c.step_args_bytes,
	c.step_args_hash, c.step_started_at, c.step_ended_at,
	c.activity_at, c.activity_background, c.activity_kinds, c.activity_next_reminder_at,
	d.delivery_id, d.task, d.phase, d.generation, d.entry, d.check_look, d.check_opened_at,
	d.queued_at, d.delivered_at, d.marked_by, d.confirmed_at, d.interrupted, d.prompted, d.out_of_memory,
	d.send_failures, d.last_send_failure_at, d.last_send_error,
	d.kept_decisions, d.provider_error_retries, d.provider_error_note, d.design_news, d.carried_news,
	coalesce(d.carried_ran, false), coalesce(d.news_only, false), d.round_named
	from claims c left join pending_task_deliveries d on d.claim_token = c.token`

// PutClaim persists an admitted claim's state: every write a claim's machine makes. A claim enters
// the store through AdmitClaim, which binds it to its tree's open lifecycle first; PutClaim alone
// writes historical state in migrations and test fixtures. The write and any role-holder
// publication it queues share one transaction.
func (s *Store) PutClaim(ctx context.Context, c supervise.Claim) error {
	var wrote error
	if err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		wrote = s.putClaim(ctx, tx, c)
		return wrote
	}); err != nil {
		if wrote == nil {
			return fmt.Errorf("put claim %s: %w", c.Token, err)
		}
		return err
	}
	return nil
}

// PutClaimTx is PutClaim inside a caller's transaction: the stop a project's pause makes writes
// the claim with the pause's records in one transaction (decision 6 of dispatch://INFRA-3759,
// supervise.Deps.PauseStopped), so a stopped claim never stands without them. Like every claim
// write it goes through putClaim, so the publication of the issue's role sessions it queues is in
// the same transaction, and so is the wake of that publication, which runs once the caller's
// transaction commits.
func (s *Store) PutClaimTx(ctx context.Context, tx pgx.Tx, c supervise.Claim) error {
	return s.putClaim(ctx, tx, c)
}

// PutClaimActivity writes the four activity columns of token's claim row and nothing else: what
// that claim's session last reported of what it is doing (INFRA-945). The report is no part of
// the claim's machine state — it arrives between a machine's writes, and the machine's in-memory
// claim does not carry it — so one write of the whole row would erase every report sent since
// that machine read its claim; the claim's own writes carry no report and leave these columns
// alone but for a launch's, which clears them (putClaim), and the idle rule reads them through
// its own view (IdleClaim).
//
// It writes only onto the launch the report belongs to, generation: a report of the claim's
// earlier launch — a dead process's, authenticated just before the relaunch — is never written
// over the launch's clear and the new process's empty report, since the update's own condition
// names the generation.
//
// A report older than the one recorded is dropped rather than written: a session fires its
// reports without an in-flight guard, so an earlier reading can land after a later one, and the
// pod would then be released on a reading the session has already replaced.
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
// The report's own clock is bounded to the daemon's, as the listener's is (decodeListenerWake):
// the session chooses every field of what it sends, and one reading dated ahead of the daemon's
// clock — a session whose host stepped its clock forward, corrected back afterwards — would
// drop every honest report after it under that same ordering guard, while the stale reading it
// left behind went on counting as current, background work and all.
func (s *Store) PutClaimActivity(ctx context.Context, token claim.Token, generation uint64, a claim.SessionActivity) (bool, error) {
	if generation > math.MaxInt64 {
		// No row holds it (putClaim refuses one), so no launch of the claim is this.
		return false, nil
	}
	var reminder *time.Time
	if a.Phase.NextReminderAt != nil && !a.Phase.NextReminderAt.IsZero() {
		at := a.Phase.NextReminderAt.UTC()
		reminder = &at
	}
	at := a.At.UTC()
	if received := time.Now().UTC(); at.After(received) {
		at = received
	}
	var known bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `with reported as (
			update claims set activity_at = $2, activity_background = $3, activity_kinds = $4,
				activity_next_reminder_at = $5, updated_at = now()
			where token = $1 and generation = $6 and (activity_at is null or activity_at <= $2) returning token)
			select exists (select 1 from claims where token = $1 and generation = $6)`,
			// Only the three known kinds are kept (claim.BackgroundWork.KnownKinds): they reach the
			// architect's worker-background-stalled notice and the daemon's log.
			string(token), at, a.BackgroundWork.Running,
			strings.Join(a.BackgroundWork.KnownKinds(), ","), reminder, int64(generation),
		).Scan(&known)
	})
	if err != nil {
		return false, fmt.Errorf("put the activity of claim %s: %w", token, err)
	}
	return known, nil
}

// PutClaimStep writes the eight step columns of token's claim row and nothing else: what the
// daemon last saw that claim's agent do (INFRA-1274). The record is no part of the claim's
// machine state — it is built from the tool frames that arrive between a machine's writes, and
// the machine's in-memory claim does not carry it — so one write of the whole row would erase
// every step seen since that machine read its claim; the claim's own writes leave these columns
// alone, and the backstop reads them back with the claim (supervise.Claim.Step).
//
// A step older than the one recorded is dropped rather than written, as a session's activity
// report is: a connection hands its unflushed frames to the next one, so an earlier step can
// land after a later one, and a worker that has since gone quiet would read as stepping. It
// answers whether the store holds the claim at all, which a step of a claim the daemon has
// forgotten does not. Nothing bounds the step's clock the way PutClaimActivity bounds a
// session's: no frame carries a time of its own, and the daemon stamps every one of them on
// receipt from its own clock.
//
// Three fields of the record say what the write is for, each by its own zero value, which is how
// this package spells "not yet" to SQL (instant):
//
//   - A zero At records the call and leaves the claim's last-step time where it is. That is the
//     fence on a frame of a connection that is no longer the claim's — it has none now, or the
//     one it has was registered after that frame's (supervise's staleStep) — which is a frame of
//     a process the daemon has already replaced: it says what that agent was doing, and nothing
//     at all about what the claim is doing now. The shim's own backlog is not this case: it is
//     held while no daemon is attached and replayed on the next, live connection (internal/shim's
//     outbox), so those frames are stamped at receipt like any other.
//   - A zero TurnStartedAt leaves the stored turn start alone, so an ordinary step does not
//     erase the turn it belongs to. A turn's start writes it.
//   - An empty CallID carries no call, and the six call columns are left exactly as they stand.
//     The record is overwritten by the next call and never cleared, least of all at a turn's
//     end: the recovery this record exists for ends the hung process and starts a turn on the
//     replacement, and a turn's start that cleared the call would erase the one fact anybody
//     wanted — which call was open when the agent stopped. A write that does carry a call
//     writes all six, so no field of one call is ever read beside a field of another.
func (s *Store) PutClaimStep(ctx context.Context, token claim.Token, step supervise.StepRecord) (bool, error) {
	var known bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `with stepped as (
			update claims set step_at = coalesce($2, step_at),
				turn_started_at = coalesce($3, turn_started_at),
				step_tool = case when $5 = '' then step_tool else $4 end,
				step_call_id = case when $5 = '' then step_call_id else $5 end,
				step_args_bytes = case when $5 = '' then step_args_bytes else $6 end,
				step_args_hash = case when $5 = '' then step_args_hash else $7 end,
				step_started_at = case when $5 = '' then step_started_at else $8 end,
				step_ended_at = case when $5 = '' then step_ended_at else $9 end,
				updated_at = now()
			where token = $1 and ($2::timestamptz is null or step_at is null or step_at <= $2)
			returning token)
			select exists (select 1 from claims where token = $1)`,
			string(token), instant(step.At.UTC()), instant(step.TurnStartedAt.UTC()),
			step.Tool, step.CallID, step.ArgsBytes, step.ArgsHash,
			instant(step.StartedAt.UTC()), instant(step.EndedAt.UTC()),
		).Scan(&known)
	})
	if err != nil {
		return false, fmt.Errorf("put the step of claim %s: %w", token, err)
	}
	return known, nil
}

// ClaimStep is the step record of one claim: what the daemon last saw that claim's agent do, as
// the store holds it (PutClaimStep). It is the read behind every operator answer about one claim
// (internal/api's operatorAnswer), where reading every claim to serve one would be the whole
// table; the listing of them all reads them with their claims (Claims).
//
// A token no claim holds answers the zero record, which says the daemon has seen nothing of that
// agent — the same answer a claim that has neither begun a turn nor made a call gives, and the
// truthful one for a row that is gone.
func (s *Store) ClaimStep(ctx context.Context, token claim.Token) (supervise.StepRecord, error) {
	var (
		step                   supervise.StepRecord
		stepAt, turnStarted    *time.Time
		stepStarted, stepEnded *time.Time
	)
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select step_at, turn_started_at, step_tool, step_call_id,
			step_args_bytes, step_args_hash, step_started_at, step_ended_at from claims where token = $1`,
			string(token)).Scan(&stepAt, &turnStarted, &step.Tool, &step.CallID,
			&step.ArgsBytes, &step.ArgsHash, &stepStarted, &stepEnded)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.StepRecord{}, nil
	}
	if err != nil {
		return supervise.StepRecord{}, fmt.Errorf("read the step of claim %s: %w", token, err)
	}
	step.At, step.TurnStartedAt = orZero(stepAt), orZero(turnStarted)
	step.StartedAt, step.EndedAt = orZero(stepStarted), orZero(stepEnded)
	return step, nil
}

// ClaimActivity is the activity report the store holds for launch generation of token's claim
// (PutClaimActivity's columns, the phase reminder aside), and whether it holds one: what a
// machine reads of the process it is letting go, which its own claim carries only as the machine
// read it at boot (supervise.Deps.ClaimActivity, the machine's recordEnd). It is read before the
// launch's own write, which clears the four columns as it moves the generation (putClaim).
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
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select activity_at, activity_background, activity_kinds from claims
			where token = $1 and generation = $2`, string(token), int64(generation)).Scan(&reportedAt, &report.Background, &kinds)
	})
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

// putClaim writes claim c inside tx, the transaction of a claim write and whatever goes with it,
// and queues the publication of c's issue's role sessions in the same transaction when the write
// changes the session the row names as the holder of c's role (storedRoleHolder, roleHolder,
// EnqueueRoleHolders): the agent registers, a new session takes the role, or the claim leaves a
// state that holds it — retired, failed, suspended, or launching again. A write that changes no
// holder queues nothing, and neither does a write that can queue none (queuesRoleHolders), such as
// a write of a claim of another project's issue, whose row only that project's daemon runs.
// A write that queued the publication wakes it once tx commits (wakeRoleHolders), as the hook of
// tx's commit: no caller wakes it. Every claim write goes through here — PutClaim,
// PutClaimAndDelivery, RetireDelivery, ReleaseHolds, AdmitClaim, and the pause's stop through
// PutClaimTx and PutClaimAndDeliveryTx — so whichever decision makes a write, the store holds the
// change and its publication together, or neither, and the publication is woken once for the
// commit that queued it.
func (s *Store) putClaim(ctx context.Context, tx pgx.Tx, c supervise.Claim) error {
	if !s.queuesRoleHolders(c) {
		return upsertClaim(ctx, tx, c)
	}
	stored, err := storedRoleHolder(ctx, tx, c)
	if err != nil {
		return fmt.Errorf("put claim %s: %w", c.Token, err)
	}
	if err := upsertClaim(ctx, tx, c); err != nil {
		return err
	}
	if roleHolder(c.State, c.Session) == stored {
		return nil
	}
	queued, err := EnqueueRoleHolders(ctx, tx, c.Project, c.Issue, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("put claim %s: %w", c.Token, err)
	}
	if queued {
		OnCommit(ctx, func() { s.wakeRoleHolders(c.Issue) })
	}
	return nil
}

// upsertClaim writes the row of claim c inside tx, putClaim's transaction.
//
// The write that records a retirement stamps the claim with retired_at_write_mark, a number
// from outbox_write_mark_seq, and every other write leaves the column where it stands: the mark
// and the state are one write, so no claim is ever read as retired at a moment the store does
// not hold. It is taken from SQL rather than from the claim in memory, on the transition into
// retired alone — the inserted state on the insert, `excluded.state = 'retired' and claims.state
// <> 'retired'` on the update — so a second write of a claim that is already retired leaves the
// stop's own moment untouched. What reads it is admit's stopOutlivedStart, against the write
// mark of a start row the issue owes: outbox rows take their numbers from that same sequence,
// through the column's own default on every insert — the start written in place of one an
// issue already owed among them (record.Store.EnqueueStart) — so one sequence numbers both
// sides.
//
// An upsert of a claim already in the table evaluates its insert's case too, since the row to
// insert is built before the conflict is found, so a write of a retired claim takes a number
// the update then discards. Only the branch that writes the column decides the mark, and a
// number nobody reads costs nothing: the sequence orders writes and counts nothing.
//
// What the mark guarantees is an order over writes, not over what a reader can see. A sequence
// hands its numbers out in the order the statements ask for them and never hands one out twice,
// whatever order the transactions that asked then commit in or whether they commit at all. So a
// write made after this retirement committed took its number after this one and is above the
// mark; a write already committed when this retirement ran took its number before it and is
// below; and a write still in flight when it ran took its number just before or just after,
// with nothing ordering the two, so its row is read on whichever side it landed. A claim an
// earlier release retired carries the column's default, 0, which is below every number the
// sequence hands out, so no owed row of it is ever ended and it behaves as it did before the
// column existed (20261109191929_outbox_write_mark).
func upsertClaim(ctx context.Context, tx pgx.Tx, c supervise.Claim) error {
	if c.Generation > math.MaxInt64 {
		return fmt.Errorf("put claim %s: generation %d does not fit a bigint", c.Token, c.Generation)
	}
	if c.Handshake.Generation > math.MaxInt64 {
		return fmt.Errorf("put claim %s: the generation %d of its handshake does not fit a bigint", c.Token, c.Handshake.Generation)
	}
	if c.AgentResumeGeneration > math.MaxInt64 {
		return fmt.Errorf("put claim %s: the generation %d of its resume by Oh My Pi's own lookup does not fit a bigint", c.Token, c.AgentResumeGeneration)
	}
	var locator []byte
	if c.Locator != nil {
		encoded, err := json.Marshal(c.Locator)
		if err != nil {
			return fmt.Errorf("put claim %s: encode its locator: %w", c.Token, err)
		}
		locator = encoded
	}
	// The window is written as the array it is, never jsonb null: a claim with no report yet reads
	// back as the empty array the column defaults to.
	outcomes := []byte("[]")
	if len(c.TurnOutcomes) > 0 {
		encoded, err := json.Marshal(c.TurnOutcomes)
		if err != nil {
			return fmt.Errorf("put claim %s: encode its turn outcomes: %w", c.Token, err)
		}
		outcomes = encoded
	}
	// Null is a claim that has abandoned no session; one that has records the session, why and
	// when, for the fresh agent's first task to name (supervise's abandonedTask).
	var abandoned []byte
	if c.Abandoned.Session != "" {
		encoded, err := json.Marshal(c.Abandoned)
		if err != nil {
			return fmt.Errorf("put claim %s: encode its abandoned session: %w", c.Token, err)
		}
		abandoned = encoded
	}
	// Null is a claim no let-go of whose process ended anything worth telling; one that did
	// records what it ended, for the next process to be told (supervise's EndedWork, recordEnd).
	var endedWork []byte
	if c.EndedWork.Cause != "" {
		encoded, err := json.Marshal(c.EndedWork)
		if err != nil {
			return fmt.Errorf("put claim %s: encode what its last let-go ended: %w", c.Token, err)
		}
		endedWork = encoded
	}
	// Null is a claim that never took another role; one that did records the role it held before
	// and whether its agent has been told, for the first prompt in the new role to tell
	// (supervise's RoleChange, roleChangedTask).
	var roleChange []byte
	if c.RoleChange.From != "" {
		encoded, err := json.Marshal(c.RoleChange)
		if err != nil {
			return fmt.Errorf("put claim %s: encode the other role it took: %w", c.Token, err)
		}
		roleChange = encoded
	}
	var enrollmentID, enrollmentIncarnation *string
	if c.Enrollment != nil {
		enrollmentID = &c.Enrollment.ID
		if c.Enrollment.Incarnation != "" {
			enrollmentIncarnation = &c.Enrollment.Incarnation
		}
	}
	// The column holds an array, never NULL, and a claim no row has given a task holds no slice at
	// all, which encodes as NULL: it is written as the empty array those two mean.
	delivered := c.DeliveredRows
	if delivered == nil {
		delivered = []int64{}
	}
	_, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, tree_epoch,
		session, session_file, locator, state, failure, launch_failures, deaths, prompt_failures, prompt_retires,
		boot_token_hash, capability_hash, uncertain_streak, last_start_row,
		serving_generation, given_entry, served_entry, agent_secrets_enrollment, agent_secrets_enrollment_incarnation, suspended_by,
		plugin_contract, plugin_relaunch_for, agent_resume_generation, session_agent, delivered_rows,
		last_turn_start, last_turn_end,
		last_death_cause, last_start_failure, last_ready, turn_outcomes, abandoned_session,
		update_round, update_reasons, update_opened_at, update_tasked_at, update_signalled_at,
		update_head, update_told_at,
		update_left_round, provider_error_run, serving_check_look, serving_check_opened_at, death_charged,
		pod_end, failure_cause, failed_at, pending_cause, leak_check_cause, ended_work, role_change,
		handshake_entry, handshake_generation, handshake_queued_at, handshake_confirmed_at,
		launched_definition, launched_generation,
		last_death_at, last_start_failure_at, last_death_digest, last_start_failure_digest,
		retired_at_write_mark)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54,
		$55, $56, $57, $58, $59, $60, $61, $62, $63, $64, $65, $66, $67,
		case when $11 = 'retired' then nextval('outbox_write_mark_seq') else 0 end)
		on conflict (token) do update set project = excluded.project, tree = excluded.tree,
		issue = excluded.issue, role = excluded.role, generation = excluded.generation, tree_epoch = excluded.tree_epoch,
		session = excluded.session, session_file = excluded.session_file,
		locator = excluded.locator, state = excluded.state, failure = excluded.failure,
		pod_end = excluded.pod_end,
		launch_failures = excluded.launch_failures, deaths = excluded.deaths,
		prompt_failures = excluded.prompt_failures,
		prompt_retires = excluded.prompt_retires, boot_token_hash = excluded.boot_token_hash,
		capability_hash = excluded.capability_hash, uncertain_streak = excluded.uncertain_streak,
		last_start_row = excluded.last_start_row,
		-- The number outbox_write_mark_seq hands this write, taken on the transition into
		-- retired and left alone by every other write (this function's doc).
		retired_at_write_mark = case when excluded.state = 'retired' and claims.state <> 'retired'
			then nextval('outbox_write_mark_seq') else claims.retired_at_write_mark end,
		serving_generation = excluded.serving_generation,
		given_entry = excluded.given_entry,
		served_entry = excluded.served_entry,
		agent_secrets_enrollment = excluded.agent_secrets_enrollment,
		agent_secrets_enrollment_incarnation = excluded.agent_secrets_enrollment_incarnation,
		suspended_by = excluded.suspended_by,
		plugin_contract = excluded.plugin_contract,
		plugin_relaunch_for = excluded.plugin_relaunch_for,
		agent_resume_generation = excluded.agent_resume_generation,
		session_agent = excluded.session_agent,
		delivered_rows = (select coalesce(array_agg(distinct delivered order by delivered), '{}'::bigint[])
			from unnest(claims.delivered_rows || excluded.delivered_rows) as delivered),
		last_turn_start = excluded.last_turn_start, last_turn_end = excluded.last_turn_end,
		last_death_cause = excluded.last_death_cause, last_death_at = excluded.last_death_at,
		last_death_digest = excluded.last_death_digest,
		last_start_failure = excluded.last_start_failure, last_start_failure_at = excluded.last_start_failure_at,
		last_start_failure_digest = excluded.last_start_failure_digest,
		last_ready = excluded.last_ready, turn_outcomes = excluded.turn_outcomes,
		abandoned_session = excluded.abandoned_session,
		update_round = excluded.update_round, update_reasons = excluded.update_reasons,
		update_opened_at = excluded.update_opened_at, update_tasked_at = excluded.update_tasked_at,
		update_signalled_at = excluded.update_signalled_at, update_head = excluded.update_head,
		update_told_at = excluded.update_told_at,
		update_left_round = excluded.update_left_round,
		provider_error_run = excluded.provider_error_run,
		serving_check_look = excluded.serving_check_look,
		serving_check_opened_at = excluded.serving_check_opened_at,
		death_charged = excluded.death_charged,
		failure_cause = excluded.failure_cause,
		failed_at = excluded.failed_at,
		pending_cause = excluded.pending_cause,
		leak_check_cause = excluded.leak_check_cause,
		ended_work = excluded.ended_work,
		role_change = excluded.role_change,
		handshake_entry = excluded.handshake_entry,
		handshake_generation = excluded.handshake_generation,
		handshake_queued_at = excluded.handshake_queued_at,
		handshake_confirmed_at = excluded.handshake_confirmed_at,
		launched_definition = excluded.launched_definition,
		launched_generation = excluded.launched_generation,
		-- The four activity columns (activity_at, activity_background, activity_kinds,
		-- activity_next_reminder_at) and the eight step columns (step_at, turn_started_at,
		-- step_tool, step_call_id, step_args_bytes, step_args_hash, step_started_at,
		-- step_ended_at) are no part of a claim's write: PutClaimActivity and PutClaimStep own
		-- them, and a machine's write carries whatever its in-memory claim last read, so a
		-- report the session sent, or a step the stream reported, since that read would be
		-- erased by the next state it persists.
		--
		-- The one exception: a write that moves the generation is a launch (supervise's
		-- launchNow), and it clears the report. The report describes the process that made it,
		-- and the launch replaces that process; a Claude Code worker reports only at its Stop
		-- hook, so its dead process's report would otherwise stand, background work and all.
		-- This is the one place the rule lives. Two readers check the generation as well, each
		-- because it holds a launch apart from the row: ClaimActivity answers only the launch it
		-- is asked of, and the update round (internal/daemon's roundGroup) takes a row's report
		-- onto a machine's view only where the view is of the row's own generation, because a
		-- view of another launch is a process the row's report does not describe.
		activity_at = case when claims.generation <> excluded.generation
			then null else claims.activity_at end,
		activity_background = case when claims.generation <> excluded.generation
			then false else claims.activity_background end,
		activity_kinds = case when claims.generation <> excluded.generation
			then '' else claims.activity_kinds end,
		activity_next_reminder_at = case when claims.generation <> excluded.generation
			then null else claims.activity_next_reminder_at end,
		updated_at = now()`,
		string(c.Token), c.Project, c.Tree, c.Issue, string(c.Role), int64(c.Generation), int64(c.TreeEpoch),
		c.Session, c.SessionFile, locator, string(c.State), c.Failure,
		c.Budgets.LaunchFailures, c.Budgets.Deaths, c.Budgets.PromptFailures, c.Budgets.PromptRetires,
		c.BootTokenHash, c.CapabilityHash, c.UncertainStreak, c.LastStartRow, int64(c.ServingGeneration), c.GivenEntry, c.ServedEntry,
		enrollmentID, enrollmentIncarnation, string(c.SuspendedBy),
		c.PluginContract, c.PluginRelaunchFor, int64(c.AgentResumeGeneration), string(c.SessionAgent), delivered,
		instant(c.LastTurnStart), instant(c.LastTurnEnd),
		c.LastDeathCause, c.LastStartFailure, instant(c.LastReady), outcomes, abandoned,
		c.UpdateRound, c.UpdateReasons, c.UpdateOpenedAt, c.UpdateTaskedAt, c.UpdateSignalledAt,
		c.UpdateHead, c.UpdateToldAt,
		c.UpdateLeftRound, c.ProviderErrorRun, c.ServingCheckLook.Look, instant(c.ServingCheckLook.OpenedAt), c.DeathCharged,
		c.PodEnd, string(c.FailureCause), instant(c.FailedAt), string(c.PendingCause), c.LeakCheckCause, endedWork, roleChange,
		c.Handshake.Entry, int64(c.Handshake.Generation), instant(c.Handshake.QueuedAt), instant(c.Handshake.ConfirmedAt),
		c.LaunchedDefinition, int64(c.LaunchedGeneration),
		instant(c.LastDeathAt), instant(c.LastStartFailureAt),
		causeDigest(c.LastDeathCause, c.LastDeathAt), causeDigest(c.LastStartFailure, c.LastStartFailureAt),
	)
	if err != nil {
		return fmt.Errorf("put claim %s: %w", c.Token, err)
	}
	return nil
}

// Claims reads every claim with its pending delivery, in token order. A stored locator that does
// not validate refuses the whole read, naming the claim: a daemon that loaded it would believe in
// a process it has no way to act on.
func (s *Store) Claims(ctx context.Context) ([]supervise.Claim, error) {
	return ReadOnly(ctx, s.pool, "read claims", readClaims)
}

// ClaimsTx is Claims through tx's snapshot. State reports use it with their durable issue and
// outbox reads, so a claim's state and the queued start that might wake it cannot come from two
// moments.
func (s *Store) ClaimsTx(ctx context.Context, tx pgx.Tx) ([]supervise.Claim, error) {
	return readClaims(ctx, tx)
}

func readClaims(ctx context.Context, tx pgx.Tx) ([]supervise.Claim, error) {
	rows, err := tx.Query(ctx, claimSelect+" order by c.token")
	if err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	defer rows.Close()
	var claims []supervise.Claim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	return claims, nil
}

// ClaimIssue is the issue the claim of token is on, from the daemon's own claims and nothing a pane
// can write: what a tmux pane adopted after a restart is told its own repository by (the daemon's
// claimRepository, the tmux runtime's Options.ClaimRepository), the one its bundle's write token
// reaches, since a token spells its issue lowercased. A token no claim holds is refused.
func (s *Store) ClaimIssue(ctx context.Context, token claim.Token) (string, error) {
	return s.claimColumn(ctx, token, "issue")
}

// ClaimRole is the stored role of the claim of token, the role of the step its agent does now (the
// architect's own at a step the architect does itself), from the daemon's own claims: what a tmux
// pane adopted after a restart holds the App of where the runtime's ActsAs gives no other role
// (the tmux runtime's Options.ClaimRole, read through runtime.SlotRole), since a token names its
// agent and not its role. A token no claim holds is refused.
func (s *Store) ClaimRole(ctx context.Context, token claim.Token) (claim.Role, error) {
	role, err := s.claimColumn(ctx, token, "role")
	return claim.Role(role), err
}

// claimColumn is column of the claim of token (ClaimIssue, ClaimRole), read in a read-only
// transaction of its own; a token no claim holds is refused. column is a column name its callers
// spell, never a value read from outside.
func (s *Store) claimColumn(ctx context.Context, token claim.Token, column string) (string, error) {
	var value string
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select `+column+` from claims where token = $1`, string(token)).Scan(&value)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("no claim holds token %s", token)
	}
	if err != nil {
		return "", fmt.Errorf("read the %s of claim %s: %w", column, token, err)
	}
	return value, nil
}

// claimMayRun is the stored claim that may still hold a process: one in a state that runs, or one
// that still records a locator.
const claimMayRun = `(state not in ('suspended', 'failed', 'retired') or locator is not null)`

// StoredClaimMayRun is whether the stored claim of token may still hold a process and was last
// started no later than row. A stop row that finds no machine for such a claim is a wait: the
// supervisor persists a claim before it publishes its machine, and the stop is for that machine.
func (s *Store) StoredClaimMayRun(ctx context.Context, token claim.Token, row int64) (bool, error) {
	var may bool
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select exists (select 1 from claims where token = $1 and last_start_row <= $2 and `+claimMayRun+`)`,
			string(token), row).Scan(&may)
	})
	if err != nil {
		return false, fmt.Errorf("read stored claim %s: %w", token, err)
	}
	return may, nil
}

// StoredClaimMayRunIn is whether the stored claim of token stands in tree and may still hold a
// process. Its caller can wait for the supervising machine rather than treating no machine as no
// process. Unlike StoredClaimMayRun, it has no start-row term: no start of another tree runs
// against a claim still standing in this tree.
func (s *Store) StoredClaimMayRunIn(ctx context.Context, token claim.Token, tree string) (bool, error) {
	var may bool
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select exists (select 1 from claims where token = $1 and tree = $2 and `+claimMayRun+`)`,
			string(token), tree).Scan(&may)
	})
	if err != nil {
		return false, fmt.Errorf("read stored claim %s in tree %s: %w", token, tree, err)
	}
	return may, nil
}

// ClaimUnretired is whether the store holds token's claim in any state but retired: the claims the
// daemon's orphan sweep knows (knownClaims), read at the moment the runtime decides rather than when
// the sweep read its claims. A token no claim holds is not unretired.
func (s *Store) ClaimUnretired(ctx context.Context, token claim.Token) (bool, error) {
	var unretired bool
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select exists (select 1 from claims where token = $1 and state <> $2)`,
			string(token), string(supervise.StateRetired)).Scan(&unretired)
	})
	if err != nil {
		return false, fmt.Errorf("read whether claim %s is retired: %w", token, err)
	}
	return unretired, nil
}

// runsNothingStates is supervise's runs-nothing set (supervise.RunsNothingStates), as the text
// array a query binds: the store names no state of that set itself.
func runsNothingStates() []string { return claimStateNames(supervise.RunsNothingStates()) }

// goneStates is supervise's gone set (supervise.GoneStates: suspended, failed, retired), as the
// text array a query binds: the store names no state of that set itself.
func goneStates() []string { return claimStateNames(supervise.GoneStates()) }

// claimStateNames is states as the text array a query binds.
func claimStateNames(states []supervise.ClaimState) []string {
	names := make([]string, len(states))
	for i, state := range states {
		names[i] = string(state)
	}
	return names
}

// IssueRunsElsewhere names, as token:state, a stored claim of issue whose tree is not tree and that
// does not run nothing: a claim in a state with a process of its own, running or coming up
// (supervise.RunsNothingStates), or one that still records a locator. It answers "" when none
// does. A child carried into a tree of its own keeps its Sandbox, labelled with the tree it left,
// until a launch of its new tree relabels it: while a claim of the old tree runs there, the relabel
// would end it.
func (s *Store) IssueRunsElsewhere(ctx context.Context, project, issue, tree string) (string, error) {
	var running string
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select token || ':' || state from claims where project = $1 and issue = $2 and tree <> $3
			and not (state = any($4) and locator is null) order by token limit 1`, project, issue, tree, runsNothingStates()).Scan(&running)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the claims of issue %s outside tree %s: %w", issue, tree, err)
	}
	return running, nil
}

// HasLegacySandboxClaims is a raw JSON census used before Claims unmarshals locators. A legacy
// per-claim Sandbox locator lacks podUid/container/generation, so the normal strict locator
// decoder would refuse first without naming the layout migration that is needed.
func (s *Store) HasLegacySandboxClaims(ctx context.Context, project string) (bool, error) {
	var legacy bool
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select exists (
			select 1 from claims where project = $1 and locator->>'runtime' = 'sandbox'
			and (locator->'sandbox'->>'podUid' is null or locator->'sandbox'->>'container' is null
				or locator->'sandbox'->>'generation' is null)
		)`, project).Scan(&legacy)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			// A brand-new database has no claims table until the first migration, and therefore
			// cannot contain a legacy locator. This preserves the no-schema-write-before-census
			// ordering for existing databases.
			return false, nil
		}
		return false, fmt.Errorf("census legacy Sandbox claims for %s: %w", project, err)
	}
	return legacy, nil
}

// ClaimByBootTokenHash finds the claim whose current launch minted the boot token with this hash
// — how a shim's hello and an agent's registration find the claim they belong to.
func (s *Store) ClaimByBootTokenHash(ctx context.Context, hash []byte) (supervise.Claim, bool, error) {
	var c supervise.Claim
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, claimSelect+" where c.boot_token_hash = $1", hash))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Claim{}, false, nil
	}
	if err != nil {
		return supervise.Claim{}, false, err
	}
	return c, true, nil
}

// PutDelivery writes the claim's pending delivery, replacing the one it had: a claim has at most
// one. A token no claim carries is refused. It is one transaction, because the write carries what
// the task names of the decisions a person made while nobody in its root's tree could hear them:
// queued or taken back, the delivery holds those rows; confirmed, it tells them (kept_decisions.go).
// The news of the approved design the task carries is held the other way round: confirmed, the
// row is itself the credit of what the prompt its turn answers carried; final, the write folds that
// credit into the told rows (design_news_told.go).
func (s *Store) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error { return putDelivery(ctx, tx, token, d) })
}

// putDelivery writes a delivery through the caller's transaction: the one PutDelivery opens, or
// the one that carries the claim the delivery is confirmed against. It takes a transaction
// because it reads the row it is about to replace and holds that reading until it has written
// (kept_decisions.go's putBackARetakenConfirmation), which no autocommit statement can do. The
// rows the task names go with it: a delivery still unconfirmed holds them, under this claim and
// this delivery id, taking each from the start row that claimed it and from whatever id the
// claim's previous delivery held it under; the write that records the confirmation tells them.
// The news a prompt of it carried (Delivery.Carried) is the delivery's credit while the row holds
// a confirmation, which the readers of what an audience took read from the row; the write of a
// final credit (Delivery.Ran) folds it into the told rows (settleDesignNews).
// All of it is in the caller's transaction, so no row is ever held by a delivery the store does
// not hold, and no confirmation commits without telling what it confirmed.
func putDelivery(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery) error {
	if d.Generation > math.MaxInt64 {
		return fmt.Errorf("put delivery %s on %s: generation %d does not fit a bigint", d.ID, token, d.Generation)
	}
	kept, news, carried := keptDecisions(d.Kept), designNews(d.News), designNews(d.Carried)
	unconfirmed := d.ConfirmedAt.IsZero()
	if unconfirmed {
		if err := putBackARetakenConfirmation(ctx, tx, token, d.ID, kept); err != nil {
			return err
		}
	}
	if err := settleDesignNews(ctx, tx, token, d, carried); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task,
		phase, generation, entry, check_look, check_opened_at, queued_at, delivered_at, marked_by,
		confirmed_at, interrupted, prompted, out_of_memory,
		send_failures, last_send_failure_at, last_send_error, kept_decisions,
		provider_error_retries, provider_error_note, design_news, carried_news, carried_ran, news_only,
		round_named)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
		coalesce($19::jsonb, '{}'::jsonb), $20, $21, $22::jsonb, $23::jsonb, $24, $25, $26)
		on conflict (claim_token) do update set delivery_id = excluded.delivery_id,
		task = excluded.task, phase = excluded.phase, generation = excluded.generation,
		entry = excluded.entry, check_look = excluded.check_look,
		check_opened_at = excluded.check_opened_at, queued_at = excluded.queued_at,
		delivered_at = excluded.delivered_at, marked_by = excluded.marked_by, confirmed_at = excluded.confirmed_at,
		interrupted = excluded.interrupted, prompted = excluded.prompted,
		out_of_memory = excluded.out_of_memory, send_failures = excluded.send_failures,
		last_send_failure_at = excluded.last_send_failure_at, last_send_error = excluded.last_send_error,
		kept_decisions = excluded.kept_decisions,
		provider_error_retries = excluded.provider_error_retries,
		provider_error_note = excluded.provider_error_note, design_news = excluded.design_news,
		carried_news = excluded.carried_news, carried_ran = excluded.carried_ran, news_only = excluded.news_only,
		round_named = excluded.round_named`,
		string(token), d.ID, d.Task, string(d.Phase), int64(d.Generation), d.Entry,
		d.CheckLook.Look, instant(d.CheckLook.OpenedAt), d.QueuedAt, instant(d.DeliveredAt), d.MarkedBy,
		instant(d.ConfirmedAt), d.Interrupted, d.Prompted, d.OutOfMemory,
		d.SendFailures, instant(d.LastSendFailureAt), d.LastSendError, kept,
		d.ProviderErrorRetries, d.ProviderErrorNote, news, carried, d.Ran, d.NewsOnly, d.RoundNamed,
	)
	if err != nil {
		return fmt.Errorf("put delivery %s on %s: %w", d.ID, token, err)
	}
	if unconfirmed {
		return holdKeptDecisions(ctx, tx, token, d.ID, kept)
	}
	return tellKeptDecisions(ctx, tx, token, d.ID)
}

// PutClaimAndDelivery writes a claim and its pending delivery in one transaction: a confirmation
// records both, and a daemon that died between them would leave a claim recorded as working whose
// delivery nothing retires or re-sends — a task the shim has already answered, so its worker is
// never prompted again and the phase waits. It is also where a confirmation tells the decisions
// its delivery holds, in that same transaction and for the same reason.
func (s *Store) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		return s.PutClaimAndDeliveryTx(ctx, tx, c, d)
	})
}

// PutClaimAndDeliveryTx is PutClaimAndDelivery inside a caller's transaction, for the stop a
// project's pause makes of a claim that holds a pending delivery (PutClaimTx). The publication of
// the issue's role sessions the claim's write queues is woken once the caller's transaction
// commits, as every claim write's is (putClaim).
func (s *Store) PutClaimAndDeliveryTx(ctx context.Context, tx pgx.Tx, c supervise.Claim, d supervise.Delivery) error {
	if err := s.putClaim(ctx, tx, c); err != nil {
		return err
	}
	return putDelivery(ctx, tx, c.Token, d)
}

// RetireDelivery removes the claim's pending delivery, fenced on its id: retiring a delivery the
// claim does not hold is refused, and the one it does hold stays. The claim it is given is written
// in the same transaction, because the run a claim serves is set by the retiring of the task that
// ran: a delete that committed without it would leave a daemon crashed in between with a claim
// that has forgotten the run its worker is still working. What the delivery held is left KEPT in
// that same transaction: a retired task tells nothing, so each row waits for the next start or
// catch-up of its own issue's tree to name it. A confirmed delivery whose turn's result never said
// the turn ran ends its credit with its row (pending_task_deliveries.carried_news,
// design_news_told.go), and writes nothing else: a newer task took its place while that turn was
// still running, or the turn ended with no result this daemon was waiting on, and a provider error
// that ends it would be dropped as the result of a task the claim no longer holds. The news is owed
// again, and a later task carries it: told twice, never not at all. A final credit was folded into
// the told rows by the write that made it final, and stays there.
//
// A delete that removes no row is refused two ways, because pending_task_deliveries is keyed on
// the claim: the row holds no delivery at all, or it holds one under another id. The first is
// the retirement this call already committed and whose answer the caller lost, and it carries
// supervise.ErrNoSuchDelivery so that the caller can read it as its own write made good rather
// than as a fresh failure. The second is a different claim altogether — a write that rotated the
// delivery's id committed after its answer was cut off, so the row holds the new delivery while
// the caller is retiring the old one — and it carries supervise.ErrOtherDelivery, naming both
// ids, which is not ErrNoSuchDelivery and must never be read as one: that would drop the
// delivery memory holds and leave the stored one behind, pending and unretired. Which it is can
// only be read from the row, so it is read here, in the same transaction as the delete that
// found nothing.
func (s *Store) RetireDelivery(ctx context.Context, c supervise.Claim, deliveryID string) error {
	return Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.putClaim(ctx, tx, c); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			"delete from pending_task_deliveries where claim_token = $1 and delivery_id = $2",
			string(c.Token), deliveryID,
		)
		if err != nil {
			return fmt.Errorf("retire delivery %s on %s: %w", deliveryID, c.Token, err)
		}
		if tag.RowsAffected() == 0 {
			var stored string
			switch err := tx.QueryRow(ctx,
				"select delivery_id from pending_task_deliveries where claim_token = $1",
				string(c.Token),
			).Scan(&stored); {
			case errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("retire delivery %s on %s: %w", deliveryID, c.Token, supervise.ErrNoSuchDelivery)
			case err != nil:
				return fmt.Errorf("read the pending delivery of %s: %w", c.Token, err)
			default:
				return fmt.Errorf("retire delivery %s on %s: the row holds delivery %s: %w",
					deliveryID, c.Token, stored, supervise.ErrOtherDelivery)
			}
		}
		return releaseKeptDecisions(ctx, tx, c.Token, deliveryID)
	})
}

func scanClaim(row pgx.Row) (supervise.Claim, error) {
	var (
		c                           supervise.Claim
		token, role, state, by      string
		sessionAgent                string
		cause, pendingCause         string
		failedAt                    *time.Time
		generation, treeEpoch       int64
		agentResumeGeneration       int64
		locator                     []byte
		deliveryID, task            *string
		markedBy                    *string
		deliveryPhase               *string
		deliveryGeneration          *int64
		deliveryEntry               *int64
		deliveryCheckLook           *int
		deliveryCheckOpened         *time.Time
		servingGeneration           int64
		servingCheckLook            int
		servingCheckOpened          *time.Time
		handshakeGeneration         int64
		handshakeQueued             *time.Time
		handshakeConfirmed          *time.Time
		enrollmentID, enrollmentIC  *string
		queuedAt, delivered         *time.Time
		confirmed                   *time.Time
		interrupted, prompted       *bool
		outOfMemory                 *bool
		sendFailures                *int64
		lastSendFailure             *time.Time
		lastSendError               *string
		turnStart, turnEnd          *time.Time
		lastReady                   *time.Time
		launchedGeneration          int64
		deathAt, startFailureAt     *time.Time
		deathDigest, failureDigest  []byte
		outcomes                    []byte
		abandoned                   []byte
		endedWork                   []byte
		roleChange                  []byte
		updateOpened, updateTasked  *time.Time
		updateSignalled, updateTold *time.Time
		kept, news, carried         []byte
		ran, newsOnly               bool
		retries                     *int64
		note                        *string
		roundNamed                  *bool
		stepAt, turnStarted         *time.Time
		stepStarted, stepEnded      *time.Time
		reportedAt, reminderAt      *time.Time
		kinds                       string
	)
	err := row.Scan(&token, &c.Project, &c.Tree, &c.Issue, &role, &generation, &treeEpoch, &c.Session,
		&c.SessionFile, &locator, &state, &c.Failure, &c.PodEnd, &cause, &failedAt, &pendingCause, &c.LeakCheckCause,
		&c.Budgets.LaunchFailures, &c.Budgets.Deaths, &c.Budgets.PromptFailures,
		&c.Budgets.PromptRetires, &c.BootTokenHash, &c.CapabilityHash, &c.UncertainStreak,
		&c.LastStartRow, &servingGeneration, &c.GivenEntry, &c.ServedEntry,
		&c.Handshake.Entry, &handshakeGeneration, &handshakeQueued, &handshakeConfirmed,
		&servingCheckLook, &servingCheckOpened,
		&enrollmentID, &enrollmentIC, &by,
		&c.PluginContract, &c.PluginRelaunchFor, &agentResumeGeneration, &sessionAgent, &c.DeliveredRows,
		&turnStart, &turnEnd,
		&c.LastDeathCause, &deathAt, &deathDigest, &c.LastStartFailure, &startFailureAt, &failureDigest, &lastReady,
		&outcomes, &abandoned, &endedWork, &roleChange,
		&c.LaunchedDefinition, &launchedGeneration,
		&c.UpdateRound, &c.UpdateReasons, &updateOpened, &updateTasked, &updateSignalled,
		&c.UpdateHead, &updateTold,
		&c.UpdateLeftRound, &c.ProviderErrorRun, &c.DeathCharged,
		&stepAt, &turnStarted, &c.Step.Tool, &c.Step.CallID, &c.Step.ArgsBytes,
		&c.Step.ArgsHash, &stepStarted, &stepEnded,
		&reportedAt, &c.ActivityBackground, &kinds, &reminderAt,
		&deliveryID, &task, &deliveryPhase, &deliveryGeneration, &deliveryEntry, &deliveryCheckLook, &deliveryCheckOpened,
		&queuedAt, &delivered, &markedBy, &confirmed, &interrupted, &prompted, &outOfMemory,
		&sendFailures, &lastSendFailure, &lastSendError, &kept, &retries, &note, &news, &carried, &ran, &newsOnly,
		&roundNamed)
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Claim{}, err
	}
	if err != nil {
		return supervise.Claim{}, fmt.Errorf("read claim: %w", err)
	}
	// An empty array is a claim no row has given a task, which the supervisor holds as no slice at
	// all: the two read the same everywhere, and a restored claim equals the one that was written.
	if len(c.DeliveredRows) == 0 {
		c.DeliveredRows = nil
	}
	c.Token, c.Role, c.State, c.SuspendedBy = claim.Token(token), claim.Role(role), supervise.ClaimState(state), supervise.SuspendSource(by)
	c.FailureCause, c.PendingCause = supervise.FailureCause(cause), supervise.FailureCause(pendingCause)
	c.FailedAt = orZero(failedAt)
	c.Generation, c.TreeEpoch, c.ServingGeneration = uint64(generation), uint64(treeEpoch), uint64(servingGeneration)
	c.AgentResumeGeneration, c.SessionAgent = uint64(agentResumeGeneration), plan.AgentID(sessionAgent)
	c.LaunchedGeneration = uint64(launchedGeneration)
	c.ServingCheckLook = supervise.CheckLook{Look: servingCheckLook, OpenedAt: orZero(servingCheckOpened)}
	// The handshake's two moments are nullable (20261114020537_claim_entry_handshake): a claim queued
	// no task carrying an entry has neither, and one whose task of its entry no turn has confirmed
	// has no confirmed moment, each of which the handshake holds as the zero time.
	c.Handshake.Generation = uint64(handshakeGeneration)
	c.Handshake.QueuedAt, c.Handshake.ConfirmedAt = orZero(handshakeQueued), orZero(handshakeConfirmed)
	if len(outcomes) > 0 {
		if err := json.Unmarshal(outcomes, &c.TurnOutcomes); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: decode its turn outcomes: %w", token, err)
		}
		if len(c.TurnOutcomes) == 0 {
			// The column's empty array is a claim no turn has been reported for, which is the
			// claim a machine holds before its first report: it reads back as that claim, not as
			// an empty window of its own.
			c.TurnOutcomes = nil
		}
	}
	if abandoned != nil {
		if err := json.Unmarshal(abandoned, &c.Abandoned); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: decode its abandoned session: %w", token, err)
		}
	}
	// Null is a claim no let-go of whose process ended anything worth telling (putClaim).
	if endedWork != nil {
		if err := json.Unmarshal(endedWork, &c.EndedWork); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: decode what its last let-go ended: %w", token, err)
		}
	}
	// Null is a claim that never took another role (putClaim).
	if roleChange != nil {
		if err := json.Unmarshal(roleChange, &c.RoleChange); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: decode the other role it took: %w", token, err)
		}
	}
	c.LastTurnStart, c.LastTurnEnd, c.LastReady = orZero(turnStart), orZero(turnEnd), orZero(lastReady)
	// A cause's time is read only beside a cause whose digest is the one stored with the time
	// (causeTime), and a start failure's time before last_ready is read as none, since this
	// release drops the start failure with its time at every ready; the migration's header
	// (20261124073512_claim_cause_times) says what the two rules leave after a rollback.
	c.LastDeathAt = causeTime(c.LastDeathCause, deathAt, deathDigest)
	c.LastStartFailureAt = causeTime(c.LastStartFailure, startFailureAt, failureDigest)
	if c.LastStartFailureAt.Before(c.LastReady) {
		c.LastStartFailureAt = time.Time{}
	}
	c.UpdateOpenedAt, c.UpdateTaskedAt, c.UpdateSignalledAt, c.UpdateToldAt = updateOpened, updateTasked, updateSignalled, updateTold
	c.Step.At, c.Step.TurnStartedAt = orZero(stepAt), orZero(turnStarted)
	c.Step.StartedAt, c.Step.EndedAt = orZero(stepStarted), orZero(stepEnded)
	// Both activity moments are nullable (migration 20261101214123_claim_session_activity): a
	// session that has reported nothing has no activity_at, and one that armed no reminder no
	// activity_next_reminder_at. The kinds are stored comma-joined in the order the session sent
	// them, and a claim that named none keeps the nil slice rather than one empty kind.
	c.ActivityAt, c.ActivityNextReminderAt = orZero(reportedAt), orZero(reminderAt)
	if kinds != "" {
		c.ActivityKinds = strings.Split(kinds, ",")
	}
	if locator != nil {
		var loc runtime.Locator
		if err := json.Unmarshal(locator, &loc); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: decode its locator: %w", token, err)
		}
		if err := loc.Validate(); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: %w", token, err)
		}
		c.Locator = &loc
	}
	if enrollmentID != nil && enrollmentIC != nil {
		c.Enrollment = &supervise.Enrollment{ID: *enrollmentID, Incarnation: *enrollmentIC}
	}
	if deliveryID != nil {
		c.Pending = &supervise.Delivery{
			ID:          *deliveryID,
			Task:        *task,
			Phase:       phase.Phase(*deliveryPhase),
			Generation:  uint64(orZeroInt(deliveryGeneration)),
			Entry:       orZeroInt(deliveryEntry),
			CheckLook:   supervise.CheckLook{Look: *deliveryCheckLook, OpenedAt: orZero(deliveryCheckOpened)},
			QueuedAt:    *queuedAt,
			DeliveredAt: orZero(delivered),
			MarkedBy:    *markedBy,
			ConfirmedAt: orZero(confirmed),
			Interrupted: orFalse(interrupted),
			Prompted:    orFalse(prompted),
			OutOfMemory: orFalse(outOfMemory),
			// The three the send failures are diagnosed from: a count, the moment of the last
			// failure — null until a send has failed, which the delivery holds as the zero
			// time — and that failure's error.
			SendFailures:         int(orZeroInt(sendFailures)),
			LastSendFailureAt:    orZero(lastSendFailure),
			LastSendError:        *lastSendError,
			Kept:                 keptDecisions(kept),
			News:                 designNews(news),
			Carried:              designNews(carried),
			Ran:                  ran,
			NewsOnly:             newsOnly,
			ProviderErrorRetries: int(orZeroInt(retries)),
			ProviderErrorNote:    *note,
			// A delivery a release before this one wrote takes the column's default, false: its
			// task names no review round (supervise's Delivery.message).
			RoundNamed: orFalse(roundNamed),
		}
	}
	return c, nil
}

// instant is a time as a nullable column: the zero time is "not yet", which SQL says as null.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// keptDecisions is a delivery's kept-decision record as the column holds it and as the delivery
// carries it: an empty record ("{}", or nothing at all for a claim with no delivery) is a task that
// names no decision, which the delivery holds as no bytes at all, so a restored delivery equals the
// one that was written.
func keptDecisions(kept []byte) []byte {
	if len(kept) == 0 || string(kept) == "{}" {
		return nil
	}
	return kept
}

// orFalse is a nullable boolean column read as false when null: the left join's delivery columns
// are all null for a claim with no pending delivery.
func orFalse(value *bool) bool {
	return value != nil && *value
}

func orZeroInt(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func orZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// causeTime is the time a row holds for cause, or the zero time where the cause is empty, the row
// holds no time, or the digest stored with the time is of another text than the cause's.
func causeTime(cause string, at *time.Time, digest []byte) time.Time {
	if cause == "" || at == nil || !bytes.Equal(digest, causeDigest(cause, *at)) {
		return time.Time{}
	}
	return *at
}

// causeDigest is the digest upsertClaim writes beside a cause's time, in the same statement: nil
// where the time is zero, else the 64-bit FNV-1a of the cause's text as the row stores it, in 8
// bytes. It tells the read whether the time is still the cause's, after a daemon of an earlier
// release rewrote the cause and not the time (causeTime), and it protects nothing: the text it is
// taken of stands in plain beside it in the same row.
func causeDigest(cause string, at time.Time) []byte {
	if at.IsZero() {
		return nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(cause))
	return h.Sum(nil)
}
