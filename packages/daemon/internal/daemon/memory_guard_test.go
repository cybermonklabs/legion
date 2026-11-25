package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// memoryGuardIssue is the issue whose pod's guard reports the kills below.
const memoryGuardIssue = "LEGION-209"

// memoryGuardKill is one kill of memoryGuardIssue's pod, as its launcher reports it.
func memoryGuardKill() shimwire.LauncherMemoryKill {
	return shimwire.LauncherMemoryKill{Guard: "guard-7f3a", Sequence: 3, PID: 4242, Executable: "python3",
		SizeBytes: 851862979, ReadingBytes: 3360811909, CounterBytes: 3500000000, LimitBytes: 4 << 30,
		At: time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)}
}

// roleMessageRows is every role message the outbox holds for issue, oldest first.
func roleMessageRows(t *testing.T, pool *store.Pool, issue string) []record.OutboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and issue = $2 order by id`, string(record.OutboxKindRoleMessage), issue)
	if err != nil {
		t.Fatalf("read the role messages of %s: %v", issue, err)
	}
	defer rows.Close()
	var held []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a role message of %s: %v", issue, err)
		}
		held = append(held, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the role messages of %s: %v", issue, err)
	}
	return held
}

// boundMemoryGuardKills is a holder bound to a workflow over an isolated outbox pool that records
// memoryGuardIssue in testing, and the claim token of that issue's tester.
func boundMemoryGuardKills(t *testing.T) (*store.Pool, *memoryGuardKills, claim.Token) {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, record.Issue{Key: memoryGuardIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: memoryGuardIssue, Project: "LEGION", Title: memoryGuardIssue, Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true})
	}); err != nil {
		t.Fatalf("record %s: %v", memoryGuardIssue, err)
	}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	kills := &memoryGuardKills{}
	kills.bind(pool, []intake.Handler{engine})
	token, err := claim.NewToken("legion", memoryGuardIssue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	return pool, kills, token
}

// appliedMemoryGuardKills is how many facts the launcher source applied under eventID.
func appliedMemoryGuardKills(t *testing.T, pool *store.Pool, eventID string) int {
	t.Helper()
	var applied int
	if err := pool.QueryRow(context.Background(), `select count(*) from processed_events where source = 'launcher' and event_id = $1`,
		eventID).Scan(&applied); err != nil {
		t.Fatalf("read the applied facts: %v", err)
	}
	return applied
}

// The holder applies a kill as one fact under the kill's own identity, so the same report sent
// twice is one fact and one note; the note's row, run by the outbox's role message executor, is
// published once, to the role topic of the role whose connection reported the kill, and to no
// other topic.
func TestMemoryGuardAKillReportedTwiceIsOneNoteOnTheReportingRolesTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, kills, token := boundMemoryGuardKills(t)
	report := memoryGuardKill()

	for range 2 {
		if err := kills.killed(ctx, memoryGuardIssue, token, "pod-209", report); err != nil {
			t.Fatalf("apply the kill: %v", err)
		}
	}

	if applied := appliedMemoryGuardKills(t, pool, "memory-guard-kill:guard-7f3a:3"); applied != 1 {
		t.Fatalf("the kill is applied as %d facts under memory-guard-kill:guard-7f3a:3, want one", applied)
	}

	rows := roleMessageRows(t, pool, memoryGuardIssue)
	if len(rows) != 1 {
		t.Fatalf("the outbox holds %d role messages of %s, want the one note of the one kill", len(rows), memoryGuardIssue)
	}

	publisher := &outboxPublisher{}
	runner := &outbox{log: quietLogger(), notices: publisher}
	if err := runner.execute(ctx, rows[0]); err != nil {
		t.Fatalf("run the note's row: %v", err)
	}
	const topic = "notifications.role.legion-legion-legion-209-tester"
	if topic != notify.RoleTopicPrefix+string(token) {
		t.Fatalf("the tester's topic is %q, want %q", notify.RoleTopicPrefix+string(token), topic)
	}
	if got := publisher.topics(); !slices.Equal(got, []string{topic}) {
		t.Fatalf("the note was published to %q, want once to %q alone", got, topic)
	}
	message := publisher.publishes()[0].message
	for _, part := range []string{"SIGKILL", "`python3` (pid 4242)", "812.40 MiB", "3.13 GiB", "4.00 GiB", "skill://legion-worker/references/tools.md"} {
		if !strings.Contains(message, part) {
			t.Fatalf("the note %q does not name %q", message, part)
		}
	}

	// The guard's next kill is another fact, and another note.
	next := report
	next.Sequence = 4
	if err := kills.killed(ctx, memoryGuardIssue, token, "pod-209", next); err != nil {
		t.Fatalf("apply the next kill: %v", err)
	}
	if rows := roleMessageRows(t, pool, memoryGuardIssue); len(rows) != 2 {
		t.Fatalf("the outbox holds %d role messages of %s after the guard's next kill, want two", len(rows), memoryGuardIssue)
	}
}

// The holder applies a kill of the controller's pod as one fact under the kill's own identity, so
// the same report sent twice is one fact and one controller notice; the outbox's run publishes
// that notice once, to the project's controller topic, as `memory-guard-kill on LEGION`, and to
// no other topic. The notice's reason is the guard's note whole: the process, its pid, its size,
// the pod's reading and the limit, then the advice, with no pointer to the controller's skill,
// which has no section for this notice
// (dispatch://INFRA-3368/ask/aa99c6ba-8a58-4830-998c-7f8102f65dd0).
func TestMemoryGuardAControllerKillReportedTwiceIsOneNoticeOnTheControllerTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	kills := &memoryGuardKills{}
	kills.bind(pool, []intake.Handler{engine})
	token := claim.ControllerToken("legion")
	report := memoryGuardKill()
	report.Guard = "guard-c0de"

	for range 2 {
		if err := kills.controllerKilled(ctx, token, "pod-controller", report); err != nil {
			t.Fatalf("apply the controller's kill: %v", err)
		}
	}

	if applied := appliedMemoryGuardKills(t, pool, "memory-guard-kill:guard-c0de:3"); applied != 1 {
		t.Fatalf("the kill is applied as %d facts under memory-guard-kill:guard-c0de:3, want one", applied)
	}

	publisher := &outboxPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, project: "legion", now: time.Now}
	for run := range 2 {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	const topic = "notifications.legion.legion.controller"
	if topic != notify.ControllerTopic("legion") {
		t.Fatalf("the controller's topic is %q, want %q", notify.ControllerTopic("legion"), topic)
	}
	published := publisher.publishes()
	if len(published) != 1 || published[0].topic != topic || published[0].message != "memory-guard-kill on LEGION" {
		t.Fatalf("published %+v, want one notice on %s as memory-guard-kill on LEGION", published, topic)
	}
	notice, ok := published[0].payload.(record.Notice)
	if !ok || notice.Kind != record.MemoryGuardKillNotice || notice.Role != claim.RoleController {
		t.Fatalf("the published payload is %#v, want a memory-guard-kill notice of the controller", published[0].payload)
	}
	const reason = "The launcher's memory guard stopped one of your processes with SIGKILL because your pod was near its memory limit: " +
		"`python3` (pid 4242) held 812.40 MiB of anonymous memory while the pod's anonymous memory read 3.13 GiB of its 4.00 GiB limit. " +
		"Your session and the pod's other processes go on. " +
		"Do not run the same program again unchanged: bound its output (a page, a limit, a stream), " +
		"or send heavy work where your deployment's instructions say."
	if notice.Reason != reason {
		t.Fatalf("the notice's reason is %q, want %q", notice.Reason, reason)
	}
}

// A holder the boot never bound refuses a kill of the controller's pod as it refuses an issue
// pod's, so the launcher session logs the report it drops.
func TestMemoryGuardAnUnboundHolderRefusesEveryControllerKill(t *testing.T) {
	t.Parallel()
	err := (&memoryGuardKills{}).controllerKilled(context.Background(), claim.ControllerToken("legion"), "pod-controller", memoryGuardKill())
	if !errors.Is(err, errMemoryGuardKillsUnbound) {
		t.Fatalf("controllerKilled = %v, want errMemoryGuardKillsUnbound", err)
	}
}

// A kill the pod's kernel made reaches the workflow through the same holder, with its cause: it is
// applied once under the same identity a guard's kill is, memory-guard-kill:<guard>:<sequence>,
// and the role is told the kernel's note rather than the guard's.
func TestMemoryGuardAKernelKillReachesIntakeWithItsCauseUnderTheSameEventID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, kills, token := boundMemoryGuardKills(t)
	report := memoryGuardKill()
	report.Cause = shimwire.MemoryKillCauseKernel

	if err := kills.killed(ctx, memoryGuardIssue, token, "pod-209", report); err != nil {
		t.Fatalf("apply the kill: %v", err)
	}
	if applied := appliedMemoryGuardKills(t, pool, "memory-guard-kill:guard-7f3a:3"); applied != 1 {
		t.Fatalf("the kernel's kill is applied as %d facts under memory-guard-kill:guard-7f3a:3, want one", applied)
	}
	rows := roleMessageRows(t, pool, memoryGuardIssue)
	if len(rows) != 1 {
		t.Fatalf("the outbox holds %d role messages of %s, want the one note of the one kill", len(rows), memoryGuardIssue)
	}
	publisher := &outboxPublisher{}
	runner := &outbox{log: quietLogger(), notices: publisher}
	if err := runner.execute(ctx, rows[0]); err != nil {
		t.Fatalf("run the note's row: %v", err)
	}
	message := publisher.publishes()[0].message
	if !strings.HasPrefix(message, "The pod's kernel stopped one of your processes because the pod ran out of memory: `python3` (pid 4242)") ||
		strings.Contains(message, "SIGKILL") {
		t.Fatalf("the note is %q, want the kernel's note of `python3` (pid 4242), not the guard's", message)
	}
}

// A holder the boot never bound has no fact path, and says so to the launcher session, which logs
// the report it drops: a boot that skipped the bind shows in the log rather than nowhere.
func TestMemoryGuardAnUnboundHolderRefusesEveryKill(t *testing.T) {
	t.Parallel()
	err := (&memoryGuardKills{}).killed(context.Background(), memoryGuardIssue, "legion-legion-legion-209-tester", "pod-209", memoryGuardKill())
	if !errors.Is(err, errMemoryGuardKillsUnbound) {
		t.Fatalf("killed = %v, want errMemoryGuardKillsUnbound", err)
	}
}

// runtime.kubernetes.memory_guard reaches sandbox.Options each pod kind's block apart: with
// different numbers in the two, Options.MemoryGuard carries issue_pods' settings, which an issue
// pod's launchers are handed, and Options.ControllerMemoryGuard controller_pod's, which the
// controller pod's launcher is handed, neither ever the other's; a kind turned off is none, and
// leaves the other kind's settings as they were.
func TestMemoryGuardEachPodKindsSettingsReachTheSandboxRuntime(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Project: "TEST", Projects: map[string]config.Project{"TEST": {}}}
	issue := config.MemoryGuardPodConfig{Enabled: true, ActPercent: 80, FloorPercent: 15, WaitSeconds: 8, IntervalMilliseconds: 250}
	controller := config.MemoryGuardPodConfig{Enabled: true, ActPercent: 70, FloorPercent: 20, WaitSeconds: 9, IntervalMilliseconds: 400}
	issueWire := &shimwire.LauncherMemoryGuard{ActPercent: 80, FloorPercent: 15, WaitSeconds: 8, IntervalMilliseconds: 250}
	controllerWire := &shimwire.LauncherMemoryGuard{ActPercent: 70, FloorPercent: 20, WaitSeconds: 9, IntervalMilliseconds: 400}
	off := func(g config.MemoryGuardPodConfig) config.MemoryGuardPodConfig {
		g.Enabled = false
		return g
	}
	for _, tc := range []struct {
		name                      string
		guard                     config.MemoryGuardConfig
		wantIssue, wantController *shimwire.LauncherMemoryGuard
	}{
		{"both on", config.MemoryGuardConfig{IssuePods: issue, ControllerPod: controller}, issueWire, controllerWire},
		{"the issue pods' guard off", config.MemoryGuardConfig{IssuePods: off(issue), ControllerPod: controller}, nil, controllerWire},
		{"the controller pod's guard off", config.MemoryGuardConfig{IssuePods: issue, ControllerPod: off(controller)}, issueWire, nil},
		{"both off", config.MemoryGuardConfig{IssuePods: off(issue), ControllerPod: off(controller)}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := config.Kubernetes{
				Namespace: "legion", Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("a", 64),
				StorageClass: "gp2", IssueVolume: "20Gi", Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
				MemoryGuard: tc.guard,
			}
			opts, err := sandboxOptions(cfg, k, "test", sandbox.DispatchCredential{}, lookup(nil), quietLogger())
			if err != nil {
				t.Fatalf("sandboxOptions: %v", err)
			}
			for _, member := range []struct {
				name      string
				got, want *shimwire.LauncherMemoryGuard
			}{
				{"MemoryGuard", opts.MemoryGuard, tc.wantIssue},
				{"ControllerMemoryGuard", opts.ControllerMemoryGuard, tc.wantController},
			} {
				if (member.got == nil) != (member.want == nil) || (member.got != nil && *member.got != *member.want) {
					t.Errorf("Options.%s = %+v, want %+v", member.name, member.got, member.want)
				}
			}
		})
	}
}
