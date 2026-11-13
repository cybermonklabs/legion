package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
//
// The four activity columns are read here beside the eight step columns and under the same rule
// (putClaim's own comment): PutClaimActivity and PutClaimStep own them, and a machine's write
// leaves them alone, but for a launch's, which clears the four activity columns. Reading them is
// what lets the daemon's minute sweep see what a quiet worker's own session says it is waiting
// on (supervise.Claim's ActivityAt and the three beside it, internal/daemon's
// tellSilentPhaseWorkers).
const claimSelect = `select c.token, c.project, c.tree, c.issue, c.role, c.generation, c.tree_epoch, c.session,
	c.session_file, c.locator, c.state, c.failure, c.failure_cause, c.failed_at, c.pending_cause, c.leak_check_cause,
	c.launch_failures, c.deaths, c.prompt_failures, c.prompt_retires, c.pod_ends_not_ours,
	c.boot_token_hash, c.capability_hash, c.uncertain_streak, c.last_start_row, c.serving_generation,
	c.given_entry, c.served_entry, c.serving_check_look, c.serving_check_opened_at,
	c.agent_secrets_enrollment, c.agent_secrets_enrollment_incarnation, c.suspended_by,
	c.plugin_contract, c.plugin_relaunch_for, c.delivered_rows,
	c.last_turn_start, c.last_turn_end,
	c.last_death_cause, c.last_start_failure, c.last_ready, c.turn_outcomes, c.abandoned_session,
	c.update_round, c.update_reasons, c.update_opened_at, c.update_tasked_at, c.update_signalled_at,
	c.update_head, c.update_told_at, c.update_answered_by_turn_end, c.update_reasked_for_push_check,
	c.update_left_round, c.provider_error_run, c.death_charged,
	c.step_at, c.turn_started_at, c.step_tool, c.step_call_id, c.step_args_bytes,
	c.step_args_hash, c.step_started_at, c.step_ended_at,
	c.activity_at, c.activity_background, c.activity_kinds, c.activity_next_reminder_at,
	d.delivery_id, d.task, d.phase, d.generation, d.entry, d.check_look, d.check_opened_at,
	d.queued_at, d.delivered_at, d.marked_by, d.confirmed_at, d.interrupted, d.prompted, d.out_of_memory,
	d.send_failures, d.last_send_failure_at, d.last_send_error,
	d.kept_decisions, d.provider_error_retries, d.provider_error_note
	from claims c left join pending_task_deliveries d on d.claim_token = c.token`

// PutClaim persists an admitted claim's state: every write a claim's machine makes. A claim enters
// the store through AdmitClaim, which binds it to its tree's open lifecycle first; PutClaim alone
// writes historical state in migrations and test fixtures.
func (s *Store) PutClaim(ctx context.Context, c supervise.Claim) error {
	return putClaim(ctx, s.pool, c)
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
// earlier launch — a dead process's, authenticated just before the relaunch and written just
// after the launch's clear — is refused rather than written over the new process's empty report,
// and answered as unknown, as the route answers a secret of a launch that is gone.
//
// A report older than the one recorded is dropped rather than written: a session fires its
// reports without an in-flight guard, so an earlier reading can land after a later one, and the
// pod would then be released on a reading the session has already replaced. It answers whether
// the store holds the claim at generation at all, which a later report of a claim the daemon has
// forgotten or relaunched does not.
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
	err := s.pool.QueryRow(ctx, `with reported as (
		update claims set activity_at = $2, activity_background = $3, activity_kinds = $4,
			activity_next_reminder_at = $5, updated_at = now()
		where token = $1 and generation = $6 and (activity_at is null or activity_at <= $2) returning token)
		select exists (select 1 from claims where token = $1 and generation = $6)`,
		// Only the three known kinds are kept (claim.BackgroundWork.KnownKinds): they reach the
		// architect's worker-background-stalled notice and the daemon's log.
		string(token), at, a.BackgroundWork.Running,
		strings.Join(a.BackgroundWork.KnownKinds(), ","), reminder, int64(generation),
	).Scan(&known)
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
	err := s.pool.QueryRow(ctx, `with stepped as (
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
	err := s.pool.QueryRow(ctx, `select step_at, turn_started_at, step_tool, step_call_id,
		step_args_bytes, step_args_hash, step_started_at, step_ended_at from claims where token = $1`,
		string(token)).Scan(&stepAt, &turnStarted, &step.Tool, &step.CallID,
		&step.ArgsBytes, &step.ArgsHash, &stepStarted, &stepEnded)
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

// putClaim writes a claim through whichever executor the caller has: the pool, or a transaction
// that carries the delivery write that goes with it.
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
func putClaim(ctx context.Context, db execer, c supervise.Claim) error {
	if c.Generation > math.MaxInt64 {
		return fmt.Errorf("put claim %s: generation %d does not fit a bigint", c.Token, c.Generation)
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
	_, err := db.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, tree_epoch,
		session, session_file, locator, state, failure, launch_failures, deaths, prompt_failures, prompt_retires, pod_ends_not_ours,
		boot_token_hash, capability_hash, uncertain_streak, last_start_row,
		serving_generation, given_entry, served_entry, agent_secrets_enrollment, agent_secrets_enrollment_incarnation, suspended_by,
		plugin_contract, plugin_relaunch_for, delivered_rows,
		last_turn_start, last_turn_end,
		last_death_cause, last_start_failure, last_ready, turn_outcomes, abandoned_session,
		update_round, update_reasons, update_opened_at, update_tasked_at, update_signalled_at,
		update_head, update_told_at, update_answered_by_turn_end, update_reasked_for_push_check,
		update_left_round, provider_error_run, serving_check_look, serving_check_opened_at, death_charged,
		failure_cause, failed_at, pending_cause, leak_check_cause,
		retired_at_write_mark)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55,
		case when $11 = 'retired' then nextval('outbox_write_mark_seq') else 0 end)
		on conflict (token) do update set project = excluded.project, tree = excluded.tree,
		issue = excluded.issue, role = excluded.role, generation = excluded.generation, tree_epoch = excluded.tree_epoch,
		session = excluded.session, session_file = excluded.session_file,
		locator = excluded.locator, state = excluded.state, failure = excluded.failure,
		launch_failures = excluded.launch_failures, deaths = excluded.deaths,
		prompt_failures = excluded.prompt_failures,
		prompt_retires = excluded.prompt_retires, pod_ends_not_ours = excluded.pod_ends_not_ours,
		boot_token_hash = excluded.boot_token_hash,
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
		delivered_rows = (select coalesce(array_agg(distinct delivered order by delivered), '{}'::bigint[])
			from unnest(claims.delivered_rows || excluded.delivered_rows) as delivered),
		last_turn_start = excluded.last_turn_start, last_turn_end = excluded.last_turn_end,
		last_death_cause = excluded.last_death_cause, last_start_failure = excluded.last_start_failure,
		last_ready = excluded.last_ready, turn_outcomes = excluded.turn_outcomes,
		abandoned_session = excluded.abandoned_session,
		update_round = excluded.update_round, update_reasons = excluded.update_reasons,
		update_opened_at = excluded.update_opened_at, update_tasked_at = excluded.update_tasked_at,
		update_signalled_at = excluded.update_signalled_at, update_head = excluded.update_head,
		update_told_at = excluded.update_told_at,
		update_answered_by_turn_end = excluded.update_answered_by_turn_end,
		update_reasked_for_push_check = excluded.update_reasked_for_push_check,
		update_left_round = excluded.update_left_round,
		provider_error_run = excluded.provider_error_run,
		serving_check_look = excluded.serving_check_look,
		serving_check_opened_at = excluded.serving_check_opened_at,
		death_charged = excluded.death_charged,
		failure_cause = excluded.failure_cause,
		failed_at = excluded.failed_at,
		pending_cause = excluded.pending_cause,
		leak_check_cause = excluded.leak_check_cause,
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
		-- This is the one place the rule lives: no reader of the columns checks anything.
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
		c.Budgets.LaunchFailures, c.Budgets.Deaths, c.Budgets.PromptFailures, c.Budgets.PromptRetires, c.Budgets.PodEndsNotOurs,
		c.BootTokenHash, c.CapabilityHash, c.UncertainStreak, c.LastStartRow, int64(c.ServingGeneration), c.GivenEntry, c.ServedEntry,
		enrollmentID, enrollmentIncarnation, string(c.SuspendedBy),
		c.PluginContract, c.PluginRelaunchFor, delivered,
		instant(c.LastTurnStart), instant(c.LastTurnEnd),
		c.LastDeathCause, c.LastStartFailure, instant(c.LastReady), outcomes, abandoned,
		c.UpdateRound, c.UpdateReasons, c.UpdateOpenedAt, c.UpdateTaskedAt, c.UpdateSignalledAt,
		c.UpdateHead, c.UpdateToldAt, c.UpdateAnsweredByTurnEnd, c.UpdateReaskedForPushCheck,
		c.UpdateLeftRound, c.ProviderErrorRun, c.ServingCheckLook.Look, instant(c.ServingCheckLook.OpenedAt), c.DeathCharged,
		string(c.FailureCause), instant(c.FailedAt), string(c.PendingCause), c.LeakCheckCause,
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
	return readClaims(ctx, s.pool)
}

// ClaimsTx is Claims through tx's snapshot. State reports use it with their durable issue and
// outbox reads, so a claim's state and the queued start that might wake it cannot come from two
// moments.
func (s *Store) ClaimsTx(ctx context.Context, tx pgx.Tx) ([]supervise.Claim, error) {
	return readClaims(ctx, tx)
}

type claimQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readClaims(ctx context.Context, q claimQuerier) ([]supervise.Claim, error) {
	rows, err := q.Query(ctx, claimSelect+" order by c.token")
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
	var issue string
	err := s.pool.QueryRow(ctx, `select issue from claims where token = $1`, string(token)).Scan(&issue)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("no claim holds token %s", token)
	}
	if err != nil {
		return "", fmt.Errorf("read the issue of claim %s: %w", token, err)
	}
	return issue, nil
}

// claimMayRun is the stored claim that may still hold a process: one in a state that runs, or one
// that still records a locator.
const claimMayRun = `(state not in ('suspended', 'failed', 'retired') or locator is not null)`

// StoredClaimMayRun is whether the stored claim of token may still hold a process and was last
// started no later than row. A stop row that finds no machine for such a claim is a wait: the
// supervisor persists a claim before it publishes its machine, and the stop is for that machine.
func (s *Store) StoredClaimMayRun(ctx context.Context, token claim.Token, row int64) (bool, error) {
	var may bool
	err := s.pool.QueryRow(ctx, `select exists (select 1 from claims where token = $1 and last_start_row <= $2 and `+claimMayRun+`)`,
		string(token), row).Scan(&may)
	if err != nil {
		return false, fmt.Errorf("read stored claim %s: %w", token, err)
	}
	return may, nil
}

// ClaimUnretired is whether the store holds token's claim in any state but retired: the claims the
// daemon's orphan sweep knows (knownClaims), read at the moment the runtime decides rather than when
// the sweep read its claims. A token no claim holds is not unretired.
func (s *Store) ClaimUnretired(ctx context.Context, token claim.Token) (bool, error) {
	var unretired bool
	err := s.pool.QueryRow(ctx, `select exists (select 1 from claims where token = $1 and state <> $2)`,
		string(token), string(supervise.StateRetired)).Scan(&unretired)
	if err != nil {
		return false, fmt.Errorf("read whether claim %s is retired: %w", token, err)
	}
	return unretired, nil
}

// runsNothingStates is supervise's runs-nothing set (supervise.RunsNothingStates), as the text
// array a query binds: the store names no state of that set itself.
func runsNothingStates() []string {
	states := supervise.RunsNothingStates()
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
	err := s.pool.QueryRow(ctx, `select token || ':' || state from claims where project = $1 and issue = $2 and tree <> $3
		and not (state = any($4) and locator is null) order by token limit 1`, project, issue, tree, runsNothingStates()).Scan(&running)
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
	err := s.pool.QueryRow(ctx, `select exists (
		select 1 from claims where project = $1 and locator->>'runtime' = 'sandbox'
		and (locator->'sandbox'->>'podUid' is null or locator->'sandbox'->>'container' is null
			or locator->'sandbox'->>'generation' is null)
	)`, project).Scan(&legacy)
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
	c, err := scanClaim(s.pool.QueryRow(ctx, claimSelect+" where c.boot_token_hash = $1", hash))
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
func (s *Store) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	return s.Tx(ctx, func(tx pgx.Tx) error { return putDelivery(ctx, tx, token, d) })
}

// putDelivery writes a delivery through the caller's transaction: the one PutDelivery opens, or
// the one that carries the claim the delivery is confirmed against. It takes a transaction
// because it reads the row it is about to replace and holds that reading until it has written
// (kept_decisions.go's putBackARetakenConfirmation), which no autocommit statement can do. The
// rows the task names go with it: a delivery still unconfirmed holds them, under this claim and
// this delivery id, taking each from the start row that claimed it and from whatever id the
// claim's previous delivery held it under; the write that records the confirmation tells them.
// All of it is in the caller's transaction, so no row is ever held by a delivery the store does
// not hold, and no confirmation commits without telling what it confirmed.
func putDelivery(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery) error {
	if d.Generation > math.MaxInt64 {
		return fmt.Errorf("put delivery %s on %s: generation %d does not fit a bigint", d.ID, token, d.Generation)
	}
	kept := keptDecisions(d.Kept)
	unconfirmed := d.ConfirmedAt.IsZero()
	if unconfirmed {
		if err := putBackARetakenConfirmation(ctx, tx, token, d.ID, kept); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task,
		phase, generation, entry, check_look, check_opened_at, queued_at, delivered_at, marked_by,
		confirmed_at, interrupted, prompted, out_of_memory,
		send_failures, last_send_failure_at, last_send_error, kept_decisions,
		provider_error_retries, provider_error_note)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
		coalesce($19::jsonb, '{}'::jsonb), $20, $21)
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
		provider_error_note = excluded.provider_error_note`,
		string(token), d.ID, d.Task, string(d.Phase), int64(d.Generation), d.Entry,
		d.CheckLook.Look, instant(d.CheckLook.OpenedAt), d.QueuedAt, instant(d.DeliveredAt), d.MarkedBy,
		instant(d.ConfirmedAt), d.Interrupted, d.Prompted, d.OutOfMemory,
		d.SendFailures, instant(d.LastSendFailureAt), d.LastSendError, kept,
		d.ProviderErrorRetries, d.ProviderErrorNote,
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
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if err := putClaim(ctx, tx, c); err != nil {
			return err
		}
		return putDelivery(ctx, tx, c.Token, d)
	})
}

// RetireDelivery removes the claim's pending delivery, fenced on its id: retiring a delivery the
// claim does not hold is refused, and the one it does hold stays. The claim it is given is written
// in the same transaction, because the run a claim serves is set by the retiring of the task that
// ran: a delete that committed without it would leave a daemon crashed in between with a claim
// that has forgotten the run its worker is still working. What the delivery held is left KEPT in
// that same transaction: a retired task tells nothing, so each row waits for the next start or
// catch-up of its own issue's tree to name it.
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
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if err := putClaim(ctx, tx, c); err != nil {
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
		cause, pendingCause         string
		failedAt                    *time.Time
		generation, treeEpoch       int64
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
		outcomes                    []byte
		abandoned                   []byte
		updateOpened, updateTasked  *time.Time
		updateSignalled, updateTold *time.Time
		kept                        []byte
		retries                     *int64
		note                        *string
		stepAt, turnStarted         *time.Time
		stepStarted, stepEnded      *time.Time
		reportedAt, reminderAt      *time.Time
		kinds                       string
	)
	err := row.Scan(&token, &c.Project, &c.Tree, &c.Issue, &role, &generation, &treeEpoch, &c.Session,
		&c.SessionFile, &locator, &state, &c.Failure, &cause, &failedAt, &pendingCause, &c.LeakCheckCause,
		&c.Budgets.LaunchFailures, &c.Budgets.Deaths, &c.Budgets.PromptFailures,
		&c.Budgets.PromptRetires, &c.Budgets.PodEndsNotOurs, &c.BootTokenHash, &c.CapabilityHash, &c.UncertainStreak,
		&c.LastStartRow, &servingGeneration, &c.GivenEntry, &c.ServedEntry, &servingCheckLook, &servingCheckOpened,
		&enrollmentID, &enrollmentIC, &by,
		&c.PluginContract, &c.PluginRelaunchFor, &c.DeliveredRows,
		&turnStart, &turnEnd,
		&c.LastDeathCause, &c.LastStartFailure, &lastReady, &outcomes, &abandoned,
		&c.UpdateRound, &c.UpdateReasons, &updateOpened, &updateTasked, &updateSignalled,
		&c.UpdateHead, &updateTold, &c.UpdateAnsweredByTurnEnd, &c.UpdateReaskedForPushCheck,
		&c.UpdateLeftRound, &c.ProviderErrorRun, &c.DeathCharged,
		&stepAt, &turnStarted, &c.Step.Tool, &c.Step.CallID, &c.Step.ArgsBytes,
		&c.Step.ArgsHash, &stepStarted, &stepEnded,
		&reportedAt, &c.ActivityBackground, &kinds, &reminderAt,
		&deliveryID, &task, &deliveryPhase, &deliveryGeneration, &deliveryEntry, &deliveryCheckLook, &deliveryCheckOpened,
		&queuedAt, &delivered, &markedBy, &confirmed, &interrupted, &prompted, &outOfMemory,
		&sendFailures, &lastSendFailure, &lastSendError, &kept, &retries, &note)
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
	c.ServingCheckLook = supervise.CheckLook{Look: servingCheckLook, OpenedAt: orZero(servingCheckOpened)}
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
	c.LastTurnStart, c.LastTurnEnd, c.LastReady = orZero(turnStart), orZero(turnEnd), orZero(lastReady)
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
			ProviderErrorRetries: int(orZeroInt(retries)),
			ProviderErrorNote:    *note,
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
