package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// killsProject is the Dispatch project the kills below are kept for: the project of
// memoryGuardIssue, whose pod reports them.
const killsProject = "LEGION"

// keptKillsRig is the daemon's own path of an issue pod's kill report, from the launcher
// session's receiver to the store: the memory guards' kill holder (memoryGuardKills) bound, as the
// boot binds it, to the handlers the workflow applies every fact through (admit.Handlers), over an
// isolated pool that records memoryGuardIssue in testing. now is admission's clock, which keeps a
// kill and forgets the kills an hour before it.
type keptKillsRig struct {
	t       *testing.T
	pool    *store.Pool
	records *record.Postgres
	kills   *memoryGuardKills
	token   claim.Token
	now     time.Time
}

func newKeptKillsRig(t *testing.T, now time.Time) *keptKillsRig {
	t.Helper()
	ctx := context.Background()
	rig := &keptKillsRig{t: t, pool: isolatedOutboxPool(t), records: record.NewStore(), kills: &memoryGuardKills{}, now: now}
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		return rig.records.PutIssue(ctx, tx, record.Issue{Key: memoryGuardIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: memoryGuardIssue, Project: killsProject, Title: memoryGuardIssue, Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
	}); err != nil {
		t.Fatalf("record %s: %v", memoryGuardIssue, err)
	}
	engine := workflow.New(rig.records, workflow.Config{Project: killsProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(rig.records, engine, 1, killsProject, quietLogger())
	admission.UseClock(func() time.Time { return rig.now })
	rig.kills.bind(rig.pool, admit.Handlers(rig.records, killsProject, engine, admission))
	token, err := claim.NewToken("legion", memoryGuardIssue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	rig.token = token
	return rig
}

// killed hands the holder report as the launcher session of the tester's connection to
// memoryGuardIssue's pod does (sandbox's memoryKilled).
func (r *keptKillsRig) killed(issue string, report shimwire.LauncherMemoryKill) {
	r.t.Helper()
	if err := r.kills.killed(context.Background(), issue, r.token, "pod-209", report); err != nil {
		r.t.Fatalf("apply the kill %s:%d: %v", report.Guard, report.Sequence, err)
	}
}

// kept is every kill the store keeps of the project, by guard and sequence, as
// `<guard>:<sequence> at=<RFC 3339> cause=<cause> noRole=<bool>`.
func (r *keptKillsRig) kept() []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select guard, sequence, at, cause, no_role from memory_kills
		where project = $1 order by guard, sequence`, killsProject)
	if err != nil {
		r.t.Fatalf("read the kept kills: %v", err)
	}
	kept, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var guard, cause string
		var sequence int64
		var at time.Time
		var noRole bool
		err := row.Scan(&guard, &sequence, &at, &cause, &noRole)
		return fmt.Sprintf("%s:%d at=%s cause=%s noRole=%t", guard, sequence, at.UTC().Format(time.RFC3339Nano), cause, noRole), err
	})
	if err != nil {
		r.t.Fatalf("read the kept kills: %v", err)
	}
	return kept
}

// killsAt is the moment of each kill the kills signal reads at at: the store's kills after the
// hour before at, as the live reading takes them (record.Store.MemoryKillsAfter).
func (r *keptKillsRig) killsAt(at time.Time) []time.Time {
	r.t.Helper()
	var kills []time.Time
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		kills, err = r.records.MemoryKillsAfter(ctx, tx, killsProject, at.Add(-signals.KillSpan))
		return err
	}); err != nil {
		r.t.Fatalf("read the kills of the hour before %s: %v", at, err)
	}
	return kills
}

// keptLine is the row kept says a kill of guard and sequence at at, of cause and mark, is.
func keptLine(guard string, sequence uint64, at time.Time, cause shimwire.MemoryKillCause, noRole bool) string {
	return fmt.Sprintf("%s:%d at=%s cause=%s noRole=%t", guard, sequence, at.UTC().Format(time.RFC3339Nano), cause, noRole)
}

// killReportKind is one kind of kill report an issue pod's launcher sends: its cause, and whether
// the process was of no role.
type killReportKind struct {
	cause  shimwire.MemoryKillCause
	noRole bool
}

// Each kind of kill report an issue pod's launcher sends is kept once for the project, at the
// report's own moment, with its cause and its mark, whatever its role is told: the memory guard's
// kill of a role's process and of a process of no role, and the pod's kernel's kill of a role's
// command, of a process that runs a role's session, and of a process of no role of either kind.
// The report reaches the store through the holder's own fact path, twice, as a launcher that
// lost the connection it wrote it on sends it again, and is one fact and one row. The role whose
// connection carried a report of its own process is told once; a report of no role tells no role.
// The kept kill is the one the kills signal reads in the hour after its moment.
func TestSignalInputGuardKill(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		kind killReportKind
	}{
		{"guard-kill/role", killReportKind{shimwire.MemoryKillCauseGuard, false}},
		{"guard-kill/no-role", killReportKind{shimwire.MemoryKillCauseGuard, true}},
		{"kernel-kill/role-command", killReportKind{shimwire.MemoryKillCauseKernel, false}},
		{"kernel-kill/role-session", killReportKind{shimwire.MemoryKillCauseKernelProtected, false}},
		{"kernel-kill/no-role", killReportKind{shimwire.MemoryKillCauseKernel, true}},
		{"kernel-kill/no-role-protected", killReportKind{shimwire.MemoryKillCauseKernelProtected, true}},
	}
	// Every cause the wire knows, of a role's process and of one of no role, has a row.
	for _, cause := range []shimwire.MemoryKillCause{shimwire.MemoryKillCauseGuard, shimwire.MemoryKillCauseKernel, shimwire.MemoryKillCauseKernelProtected} {
		for _, noRole := range []bool{false, true} {
			kind := killReportKind{cause, noRole}
			if !slices.ContainsFunc(rows, func(row struct {
				name string
				kind killReportKind
			}) bool {
				return row.kind == kind
			}) {
				t.Fatalf("no row keeps a report of cause %q with noRole %t", cause, noRole)
			}
		}
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			report := memoryGuardKill()
			report.Cause, report.NoRole = row.kind.cause, row.kind.noRole
			rig := newKeptKillsRig(t, report.At.Add(time.Minute))
			for range 2 {
				rig.killed(memoryGuardIssue, report)
			}
			if applied := appliedMemoryGuardKills(t, rig.pool, "memory-guard-kill:guard-7f3a:3"); applied != 1 {
				t.Fatalf("the kill is applied as %d facts under memory-guard-kill:guard-7f3a:3, want one", applied)
			}
			if got, want := rig.kept(), []string{keptLine("guard-7f3a", 3, report.At, report.Cause, report.NoRole)}; !slices.Equal(got, want) {
				t.Fatalf("the store keeps %q, want %q", got, want)
			}
			if got := rig.killsAt(rig.now); !slices.EqualFunc(got, []time.Time{report.At}, time.Time.Equal) {
				t.Fatalf("the kills signal at %s reads the kills %v, want the one at %s", rig.now, got, report.At)
			}
			want := 1
			if report.NoRole {
				want = 0
			}
			if told := len(roleMessageRows(t, rig.pool, memoryGuardIssue)); told != want {
				t.Fatalf("the outbox holds %d role messages of %s, want %d: a role is told of its own process's kill alone, once", told, memoryGuardIssue, want)
			}
		})
	}
}

// A kill of an issue pod whose issue the daemon keeps no record of, which the workflow tells no
// role of, is kept all the same: the kills signal counts every kill of the project's issue pods,
// as their launchers' lines do, whatever the workflow could do with it.
func TestSignalInputGuardKillOfAnUnrecordedIssueIsKept(t *testing.T) {
	t.Parallel()
	report := memoryGuardKill()
	rig := newKeptKillsRig(t, report.At.Add(time.Minute))
	rig.killed("LEGION-777", report)
	if got, want := rig.kept(), []string{keptLine("guard-7f3a", 3, report.At, shimwire.MemoryKillCauseGuard, false)}; !slices.Equal(got, want) {
		t.Fatalf("the store keeps %q, want %q", got, want)
	}
	if told := len(roleMessageRows(t, rig.pool, "LEGION-777")); told != 0 {
		t.Fatalf("the outbox holds %d role messages of an issue the daemon does not record, want none", told)
	}
}

// A kill of the controller's pod is not kept: the kills signal counts the issue pods' kills alone.
// The controller is told of its own program's kill as before, and of a report marked as of no
// role, which the controller pod's launcher is never told the daemon knows, nothing.
func TestSignalInputGuardKillOfTheControllersPodIsNotKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	report := memoryGuardKill()
	rig := newKeptKillsRig(t, report.At.Add(time.Minute))
	report.Guard = "guard-c0de"
	if err := rig.kills.controllerKilled(ctx, claim.ControllerToken("legion"), "pod-controller", report); err != nil {
		t.Fatalf("apply the controller's kill: %v", err)
	}
	noRole := report
	noRole.Sequence, noRole.NoRole = 4, true
	if err := rig.kills.controllerKilled(ctx, claim.ControllerToken("legion"), "pod-controller", noRole); err != nil {
		t.Fatalf("apply the controller pod's report of no role: %v", err)
	}
	if applied := appliedMemoryGuardKills(t, rig.pool, "memory-guard-kill:guard-c0de:4"); applied != 1 {
		t.Fatalf("the report of no role is applied as %d facts, want one", applied)
	}
	if got := rig.kept(); len(got) != 0 {
		t.Fatalf("the store keeps %q of the controller's pod, want nothing", got)
	}
	var notices int
	if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where kind = $1 and payload->>'kind' = $2`,
		string(record.OutboxKindControllerNotice), string(record.MemoryGuardKillNotice)).Scan(&notices); err != nil {
		t.Fatalf("read the controller's notices: %v", err)
	}
	if notices != 1 {
		t.Fatalf("the outbox holds %d memory-guard-kill notices, want the one of the controller's own program", notices)
	}
}

// A report a reconnected launcher sends late counts at its own moment, the moment the watch named
// the kill, and not at the moment the daemon received it: kept twenty minutes after the kill, it
// counts from the kill's moment to an hour later, and no longer. The store forgets each kill of the
// project once it is an hour or more older than the moment a later kill is kept at, and a report
// older than that when it arrives is never read.
func TestSignalInputGuardKillReportedLateCountsAtItsOwnMoment(t *testing.T) {
	t.Parallel()
	late := memoryGuardKill()
	received := late.At.Add(20 * time.Minute)
	rig := newKeptKillsRig(t, received)
	rig.killed(memoryGuardIssue, late)
	kills := rig.killsAt(received)
	for _, tc := range []struct {
		at   time.Time
		want float64
	}{
		{received, 1},
		{late.At.Add(signals.KillSpan - time.Second), 1},
		{late.At.Add(signals.KillSpan), 0},
	} {
		if got := signals.KillsAt(signals.Coverage{}, kills, tc.at); got != (signals.Value{Recorded: true, Value: tc.want}) {
			t.Fatalf("the kills signal at %s over the kept kills %v = %+v, want %v", tc.at, kills, got, tc.want)
		}
	}

	stale := memoryGuardKill()
	stale.Sequence, stale.At = 4, received.Add(-signals.KillSpan)
	next := memoryGuardKill()
	next.Sequence, next.At = 5, late.At.Add(90*time.Minute)
	rig.now = next.At
	rig.killed(memoryGuardIssue, stale)
	rig.killed(memoryGuardIssue, next)
	if got, want := rig.kept(), []string{keptLine("guard-7f3a", 5, next.At, shimwire.MemoryKillCauseGuard, false)}; !slices.Equal(got, want) {
		t.Fatalf("the store keeps %q, want the kill of the last hour alone, %q", got, want)
	}
}

// The first boot of the release that keeps the kills records the moment it began keeping them for
// the project, from which the kills signal counts an hour, and a later boot leaves that moment.
func TestSignalInputKillsAreKeptFromTheFirstBoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	rig := newKeptKillsRig(t, first)
	since := func() (time.Time, bool) {
		t.Helper()
		var at time.Time
		var recorded bool
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			at, recorded, _, err = rig.records.SignalInputStatus(ctx, tx, killsProject, signals.Kills)
			return err
		}); err != nil {
			t.Fatalf("read since when the kills are kept: %v", err)
		}
		return at, recorded
	}
	if at, recorded := since(); recorded {
		t.Fatalf("before any boot the kills are kept since %s, want no moment", at)
	}
	for boot := range int64(2) {
		if _, err := intake.ApplyFact(ctx, rig.pool, "daemon", fmt.Sprintf("boot:%s:%d", killsProject, boot+1), intake.DaemonBooted{Boot: boot + 1}, rig.kills.handlers...); err != nil {
			t.Fatalf("apply boot %d: %v", boot+1, err)
		}
		if at, recorded := since(); !recorded || !at.Equal(first) {
			t.Fatalf("after boot %d the kills are kept since %s (recorded %t), want the first boot's %s", boot+1, at, recorded, first)
		}
		rig.now = rig.now.Add(time.Hour)
	}
}
