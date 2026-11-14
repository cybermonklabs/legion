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
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// memoryGuardIssue is the issue whose pod's guard reports the kills below.
const memoryGuardIssue = "LEGION-209"

// memoryGuardKill is one kill of memoryGuardIssue's pod, as its launcher reports it.
func memoryGuardKill() shimwire.LauncherMemoryKill {
	return shimwire.LauncherMemoryKill{Guard: "guard-7f3a", Sequence: 3, PID: 4242, Command: "python3 -m pytest tests/",
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

// The holder applies a kill as one fact under the kill's own identity, so the same report sent
// twice is one fact and one note; the note's row, run by the outbox's role message executor, is
// published once, to the role topic of the role whose connection reported the kill, and to no
// other topic.
func TestMemoryGuardAKillReportedTwiceIsOneNoteOnTheReportingRolesTopic(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
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
	report := memoryGuardKill()

	for range 2 {
		if err := kills.killed(ctx, memoryGuardIssue, token, "pod-209", report); err != nil {
			t.Fatalf("apply the kill: %v", err)
		}
	}

	var applied int
	if err := pool.QueryRow(ctx, `select count(*) from processed_events where source = 'launcher' and event_id = $1`,
		"memory-guard-kill:guard-7f3a:3").Scan(&applied); err != nil {
		t.Fatalf("read the applied facts: %v", err)
	}
	if applied != 1 {
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
	for _, part := range []string{"SIGKILL", "`python3 -m pytest tests/` (pid 4242)", "812.40 MiB", "3.13 GiB", "4.00 GiB", "skill://legion-worker/references/tools.md"} {
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

// A holder the boot never bound has no fact path, and says so to the launcher session, which logs
// the report it drops: a boot that skipped the bind shows in the log rather than nowhere.
func TestMemoryGuardAnUnboundHolderRefusesEveryKill(t *testing.T) {
	err := (&memoryGuardKills{}).killed(context.Background(), memoryGuardIssue, "legion-legion-legion-209-tester", "pod-209", memoryGuardKill())
	if !errors.Is(err, errMemoryGuardKillsUnbound) {
		t.Fatalf("killed = %v, want errMemoryGuardKillsUnbound", err)
	}
}

// runtime.kubernetes.memory_guard.issue_pods reaches sandbox.Options as the settings an issue pod's
// launcher is handed, and none at all when the deployment turned the guard off.
func TestMemoryGuardTheIssuePodsSettingsReachTheSandboxRuntime(t *testing.T) {
	cfg := config.Config{Project: "TEST", Projects: map[string]config.Project{"TEST": {}}}
	k := config.Kubernetes{
		Namespace: "legion", Image: "ghcr.io/cybermonklabs/legion-worker@sha256:" + strings.Repeat("a", 64),
		StorageClass: "gp2", IssueVolume: "20Gi", Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
		MemoryGuard: config.MemoryGuardConfig{IssuePods: config.MemoryGuardPodConfig{
			Enabled: true, ActPercent: 80, FloorPercent: 15, WaitSeconds: 8, IntervalMilliseconds: 250,
		}},
	}
	opts, err := sandboxOptions(cfg, k, "test", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions: %v", err)
	}
	want := shimwire.LauncherMemoryGuard{ActPercent: 80, FloorPercent: 15, WaitSeconds: 8, IntervalMilliseconds: 250}
	if opts.MemoryGuard == nil || *opts.MemoryGuard != want {
		t.Fatalf("Options.MemoryGuard = %+v, want %+v", opts.MemoryGuard, want)
	}

	k.MemoryGuard.IssuePods.Enabled = false
	opts, err = sandboxOptions(cfg, k, "test", "", lookup(nil), quietLogger())
	if err != nil {
		t.Fatalf("sandboxOptions with the guard off: %v", err)
	}
	if opts.MemoryGuard != nil {
		t.Fatalf("Options.MemoryGuard = %+v with the guard off, want none", *opts.MemoryGuard)
	}
}
