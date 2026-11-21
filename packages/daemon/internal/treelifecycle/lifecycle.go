package treelifecycle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// Authority names the only producers that may open runnable work for a tree or an issue. A
// workflow lifecycle is opened by its admission fact; an operator's is opened only by the
// authenticated operator route. The empty value is never authority.
type Authority string

const (
	AuthorityWorkflow Authority = "workflow"
	AuthorityOperator Authority = "operator"
)

// Unit is what one lifecycle guards, and the table its rows stand in: a tree (Trees, the rows of
// tree_lifecycles, keyed by the tree's root) or an issue (Issues, the rows of issue_lifecycles,
// keyed by the issue itself). The barrier is the same for both, and every operation of it is the
// unit's method, so the two share this one implementation (decision 16 of
// dispatch://INFRA-4730). The daemon's decisions bind and check the issue's; the tree's rows stay
// written for the release before, whose claims bind them (R22 of dispatch://INFRA-3320).
type Unit struct {
	// noun names the unit in every message ("tree", "issue"); table and column are where its
	// rows stand and the column that holds their key.
	noun, table, column string
}

var (
	// Trees is the lifecycle of a tree, keyed by its root: the barrier of the release before.
	Trees = Unit{noun: "tree", table: "tree_lifecycles", column: "tree"}
	// Issues is the lifecycle of one issue, keyed by the issue: the barrier a claim of this
	// release binds, and the one an issue's cleanup reserves.
	Issues = Unit{noun: "issue", table: "issue_lifecycles", column: "issue"}
)

// Lifecycle is the independent durable admission barrier for one project's tree or issue. Its
// epoch binds claims to one open lifetime. Cleanup reserves that epoch until API-confirmed
// release; only an explicit fresh authority opens the next epoch.
type Lifecycle struct {
	Project string
	// Tree is the key of what the lifecycle guards: the tree's root for a lifecycle of Trees, the
	// issue itself for one of Issues.
	Tree               string
	Epoch              uint64
	Authority          Authority
	CleanupStarted     bool
	CleanupConfirmedAt time.Time
	// Created is this call's own insert of the row: true only where Open created the lifecycle,
	// false for every read of one and for every later epoch of it. The draw of the declared
	// experiments (internal/experiment) is made exactly once, when the row is first created, so
	// it is the one caller that asks; Read answers false, since it creates nothing.
	Created bool
}

var (
	// ErrCleanupReserved is a wait: whatever it refuses runs once the cleanup confirmed and a fresh
	// admission opened the next epoch.
	ErrCleanupReserved = wait.New("lifecycle cleanup is reserved")
	ErrLifecycleAbsent = errors.New("lifecycle is absent")
	ErrLifecycleClosed = errors.New("lifecycle cleanup is confirmed")
	// ErrAuthorityHeld is an open lifecycle of the other authority: a workflow root whose key names
	// a lifecycle the operator opened, or the reverse. It refuses only that tree or issue.
	ErrAuthorityHeld = errors.New("lifecycle is open under another authority")
)

// Serialize is the same global fact serializer intake.ApplyFact uses. Facts promote other trees
// and issues under one transaction, so a per-key lock would introduce a cross-key lock order.
// Every non-fact lifecycle operation, of either unit, takes this lock too; pg_advisory_xact_lock
// is re-entrant in the fact transaction and is released with it.
func Serialize(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('legion.apply_fact'))"); err != nil {
		return fmt.Errorf("serialize lifecycle: %w", err)
	}
	return nil
}

// columns is the select list scan reads, the key column named as the unit names it.
func (u Unit) columns() string {
	return "project, " + u.column + ", epoch, authority, cleanup_started, cleanup_confirmed_at"
}

func (u Unit) scan(row pgx.Row) (Lifecycle, error) {
	var lifecycle Lifecycle
	var epoch int64
	var authority string
	var confirmed *time.Time
	if err := row.Scan(&lifecycle.Project, &lifecycle.Tree, &epoch, &authority, &lifecycle.CleanupStarted, &confirmed); err != nil {
		return Lifecycle{}, err
	}
	if confirmed != nil {
		lifecycle.CleanupConfirmedAt = *confirmed
	}
	if epoch <= 0 {
		return Lifecycle{}, fmt.Errorf("%s lifecycle %s/%s has invalid epoch %d", u.noun, lifecycle.Project, lifecycle.Tree, epoch)
	}
	lifecycle.Epoch, lifecycle.Authority = uint64(epoch), Authority(authority)
	return lifecycle, nil
}

func (u Unit) read(ctx context.Context, tx pgx.Tx, project, key string) (Lifecycle, error) {
	return u.scan(tx.QueryRow(ctx, `select `+u.columns()+` from `+u.table+`
		where project = $1 and `+u.column+` = $2 for update`, project, key))
}

// Read returns the lifecycle of key as the table holds it now, and whether it has a row at all.
// It opens, reserves and confirms nothing.
func (u Unit) Read(ctx context.Context, tx pgx.Tx, project, key string) (Lifecycle, bool, error) {
	lifecycle, err := u.scan(tx.QueryRow(ctx, `select `+u.columns()+` from `+u.table+`
		where project = $1 and `+u.column+` = $2`, project, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lifecycle{}, false, nil
	}
	if err != nil {
		return Lifecycle{}, false, fmt.Errorf("read %s lifecycle %s: %w", u.noun, key, err)
	}
	return lifecycle, true, nil
}

// ReadMany is the lifecycle of each of keys under project, by key, in one read: the lifecycles a
// look over many trees or issues takes at once, rather than a Read per key. A key with no row has
// no entry, and no keys reads nothing. Like Read it opens, reserves and confirms nothing, and it
// takes no row lock: the caller is judging, not writing.
func (u Unit) ReadMany(ctx context.Context, tx pgx.Tx, project string, keys []string) (map[string]Lifecycle, error) {
	lifecycles := make(map[string]Lifecycle, len(keys))
	if len(keys) == 0 {
		return lifecycles, nil
	}
	rows, err := tx.Query(ctx, `select `+u.columns()+` from `+u.table+`
		where project = $1 and `+u.column+` = any($2) order by `+u.column, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read the %s lifecycles of %d %ss: %w", u.noun, len(keys), u.noun, err)
	}
	defer rows.Close()
	for rows.Next() {
		lifecycle, err := u.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("read the %s lifecycles of %d %ss: %w", u.noun, len(keys), u.noun, err)
		}
		lifecycles[lifecycle.Tree] = lifecycle
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the %s lifecycles of %d %ss: %w", u.noun, len(keys), u.noun, err)
	}
	return lifecycles, nil
}

// HeldByOperator says whether the operator owns this lifetime: it opened it and its cleanup has
// not confirmed, so the workflow may record nothing that acts on its claims or its pod.
func (l Lifecycle) HeldByOperator() bool {
	return l.Authority == AuthorityOperator && l.CleanupConfirmedAt.IsZero()
}

// OpenUnderWorkflow says whether the workflow owns this lifetime and it is still running: an
// admission fact opened it, and no cleanup has been reserved on it. A lifetime ends by reserving
// its cleanup (ReserveCleanup), and only a fresh Open clears that reservation, so CleanupStarted is
// the mark of a lifetime that has ended whether or not its release has confirmed yet.
func (l Lifecycle) OpenUnderWorkflow() bool {
	return l.Authority == AuthorityWorkflow && !l.CleanupStarted
}

// Open starts a fresh authorized lifetime of key or returns its current open epoch. It is called
// in the admission fact transaction or before an explicit operator claim is created; a cleanup
// reservation is a named wait, never an implicit zero authority.
func (u Unit) Open(ctx context.Context, tx pgx.Tx, project, key string, authority Authority) (Lifecycle, error) {
	if authority != AuthorityWorkflow && authority != AuthorityOperator {
		return Lifecycle{}, fmt.Errorf("open %s lifecycle %s: invalid authority %q", u.noun, key, authority)
	}
	if err := Serialize(ctx, tx); err != nil {
		return Lifecycle{}, err
	}
	lifecycle, err := u.read(ctx, tx, project, key)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := tx.Exec(ctx, `insert into `+u.table+` (project, `+u.column+`, epoch, authority) values ($1, $2, 1, $3)`,
			project, key, string(authority))
		if err != nil {
			return Lifecycle{}, fmt.Errorf("create %s lifecycle %s: %w", u.noun, key, err)
		}
		return Lifecycle{Project: project, Tree: key, Epoch: 1, Authority: authority, Created: true}, nil
	}
	if err != nil {
		return Lifecycle{}, fmt.Errorf("read %s lifecycle %s: %w", u.noun, key, err)
	}
	if lifecycle.CleanupStarted && lifecycle.CleanupConfirmedAt.IsZero() {
		return Lifecycle{}, fmt.Errorf("open %s lifecycle %s: %w", u.noun, key, ErrCleanupReserved)
	}
	if lifecycle.CleanupStarted {
		if lifecycle.Epoch == math.MaxInt64 {
			return Lifecycle{}, fmt.Errorf("open %s lifecycle %s: epoch overflows", u.noun, key)
		}
		lifecycle.Epoch++
		lifecycle.Authority, lifecycle.CleanupStarted, lifecycle.CleanupConfirmedAt = authority, false, time.Time{}
		_, err = tx.Exec(ctx, `update `+u.table+` set epoch = $3, authority = $4, cleanup_started = false,
			cleanup_confirmed_at = null, updated_at = now() where project = $1 and `+u.column+` = $2`,
			project, key, int64(lifecycle.Epoch), string(authority))
		if err != nil {
			return Lifecycle{}, fmt.Errorf("open next %s lifecycle %s: %w", u.noun, key, err)
		}
		return lifecycle, nil
	}
	if lifecycle.Authority != authority {
		return Lifecycle{}, fmt.Errorf("open %s lifecycle %s as %s: %w: current authority is %s", u.noun, key, authority, ErrAuthorityHeld, lifecycle.Authority)
	}
	return lifecycle, nil
}

// BindWork binds a claim before its first durable write. Authority was established by the opener;
// this shared path rejects absent, reserved and confirmed lifecycles before a runnable row commits.
func (u Unit) BindWork(ctx context.Context, tx pgx.Tx, project, key string) (uint64, error) {
	if err := Serialize(ctx, tx); err != nil {
		return 0, err
	}
	lifecycle, err := u.read(ctx, tx, project, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("bind work of %s %s: %w", u.noun, key, ErrLifecycleAbsent)
	}
	if err != nil {
		return 0, fmt.Errorf("bind work of %s %s: %w", u.noun, key, err)
	}
	if lifecycle.CleanupStarted {
		if lifecycle.CleanupConfirmedAt.IsZero() {
			return 0, fmt.Errorf("bind work of %s %s: %w", u.noun, key, ErrCleanupReserved)
		}
		return 0, fmt.Errorf("bind work of %s %s: %w", u.noun, key, ErrLifecycleClosed)
	}
	return lifecycle.Epoch, nil
}

// CheckWork confirms a bound claim may start or resume. It is deliberately a short transaction
// separate from runtime work: a cleanup reservation makes the outbox retry the start without
// consuming a launch-failure budget.
func (u Unit) CheckWork(ctx context.Context, tx pgx.Tx, project, key string, epoch uint64) error {
	if epoch == 0 {
		return fmt.Errorf("check work of %s %s: unbound lifecycle epoch", u.noun, key)
	}
	bound, err := u.BindWork(ctx, tx, project, key)
	if err != nil {
		return err
	}
	if bound != epoch {
		return fmt.Errorf("check work of %s %s: bound epoch %d is stale; current is %d", u.noun, key, epoch, bound)
	}
	return nil
}

// ReserveCleanup is the irreversible durable reservation. A retry returns the existing
// unconfirmed reservation; a confirmed lifecycle cannot be selected by a delayed close.
func (u Unit) ReserveCleanup(ctx context.Context, tx pgx.Tx, project, key string, authority Authority) (Lifecycle, error) {
	if err := Serialize(ctx, tx); err != nil {
		return Lifecycle{}, err
	}
	lifecycle, err := u.read(ctx, tx, project, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lifecycle{}, fmt.Errorf("reserve %s cleanup %s: %w", u.noun, key, ErrLifecycleAbsent)
	}
	if err != nil {
		return Lifecycle{}, fmt.Errorf("reserve %s cleanup %s: %w", u.noun, key, err)
	}
	if lifecycle.Authority != authority {
		return Lifecycle{}, fmt.Errorf("reserve %s cleanup %s as %s: current authority is %s", u.noun, key, authority, lifecycle.Authority)
	}
	if lifecycle.CleanupStarted {
		if lifecycle.CleanupConfirmedAt.IsZero() {
			return lifecycle, nil
		}
		return Lifecycle{}, fmt.Errorf("reserve %s cleanup %s: %w", u.noun, key, ErrLifecycleClosed)
	}
	_, err = tx.Exec(ctx, `update `+u.table+` set cleanup_started = true, updated_at = now()
		where project = $1 and `+u.column+` = $2`, project, key)
	if err != nil {
		return Lifecycle{}, fmt.Errorf("reserve %s cleanup %s: %w", u.noun, key, err)
	}
	lifecycle.CleanupStarted = true
	return lifecycle, nil
}

// CheckReservation proves the physical cleanup caller holds the current reservation epoch. It takes
// the shared serializer before the lifecycle row, as every lifecycle operation does. The authority
// was checked when the reservation was taken through its own entry point; the epoch names it.
func (u Unit) CheckReservation(ctx context.Context, tx pgx.Tx, project, key string, epoch uint64) error {
	if epoch == 0 {
		return fmt.Errorf("check cleanup reservation of %s %s: unbound lifecycle epoch", u.noun, key)
	}
	if err := Serialize(ctx, tx); err != nil {
		return err
	}
	lifecycle, err := u.read(ctx, tx, project, key)
	if err != nil {
		return fmt.Errorf("check cleanup reservation of %s %s: %w", u.noun, key, err)
	}
	if lifecycle.Epoch != epoch || !lifecycle.CleanupStarted || !lifecycle.CleanupConfirmedAt.IsZero() {
		return fmt.Errorf("check cleanup reservation of %s %s: reservation is not current", u.noun, key)
	}
	return nil
}

// ConfirmCleanup records complete API-confirmed release. The next explicit admission, not a
// delayed claim or old outbox row, opens the following epoch.
func (u Unit) ConfirmCleanup(ctx context.Context, tx pgx.Tx, project, key string, epoch uint64) error {
	if err := Serialize(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `update `+u.table+` set cleanup_confirmed_at = now(), updated_at = now()
		where project = $1 and `+u.column+` = $2 and epoch = $3 and cleanup_started = true and cleanup_confirmed_at is null`,
		project, key, int64(epoch))
	if err != nil {
		return fmt.Errorf("confirm %s cleanup %s: %w", u.noun, key, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("confirm %s cleanup %s: reservation is not current", u.noun, key)
	}
	return nil
}

// Open, ReserveCleanup and ConfirmCleanup are Trees' operations under the names the tests of
// admission and of the daemon still seed the release before's tree barrier with; each moves to
// the unit it means with the code it tests (dispatch://INFRA-4730's t5 to t7), and these go with
// the last of them.
func Open(ctx context.Context, tx pgx.Tx, project, tree string, authority Authority) (Lifecycle, error) {
	return Trees.Open(ctx, tx, project, tree, authority)
}

func ReserveCleanup(ctx context.Context, tx pgx.Tx, project, tree string, authority Authority) (Lifecycle, error) {
	return Trees.ReserveCleanup(ctx, tx, project, tree, authority)
}

func ConfirmCleanup(ctx context.Context, tx pgx.Tx, project, tree string, epoch uint64) error {
	return Trees.ConfirmCleanup(ctx, tx, project, tree, epoch)
}
