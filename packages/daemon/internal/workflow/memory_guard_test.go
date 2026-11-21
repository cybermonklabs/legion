package workflow

// A process an issue pod's memory guard ended is told to the role whose process it was, on that
// role's own topic, and to nobody else (dispatch://INFRA-3557).

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// memoryGuardKillReport is a kill whose sizes print as 812.40 MiB, 3.13 GiB and 4.00 GiB.
func memoryGuardKillReport() shimwire.LauncherMemoryKill {
	return shimwire.LauncherMemoryKill{Guard: "guard-7f3a", Sequence: 3, PID: 4242, Executable: "python3",
		SizeBytes: 851862979, ReadingBytes: 3360811909, CounterBytes: 3500000000, LimitBytes: 4 << 30,
		At: time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)}
}

// outboxRowsOf is how many outbox rows of any kind issue holds.
func outboxRowsOf(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), "select count(*) from outbox where issue = $1", issue).Scan(&rows); err != nil {
		t.Fatalf("count the outbox rows of %s: %v", issue, err)
	}
	return rows
}

// The kill is told to the role whose connection reported it, as one memory-kill message at med
// addressed to its claim token: the guard's cause, the process by its executable, its pid, its
// size, the pod's anonymous memory reading and its limit. Nothing else is queued for the issue: no
// architect notice, no Dispatch message.
func TestMemoryGuardAKillIsToldToTheReportingRoleAlone(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")
	token := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleTester))
	report := memoryGuardKillReport()

	if _, err := intake.ApplyFact(t.Context(), pool, "launcher", "memory-guard-kill:guard-7f3a:3", intake.MemoryGuardKill{
		Issue: "LEGION-209", Token: token, PodUID: "pod-209", Report: report,
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the kill: %v", err)
	}

	assertRoleMessages(t, pool, "LEGION-209", []roleTold{toldMed(string(token), memoryKill(texts.KillGuard, "python3", "3.13 GiB"))})
	if rows := outboxRowsOf(t, pool, "LEGION-209"); rows != 1 {
		t.Fatalf("the outbox holds %d rows of LEGION-209, want the one role message", rows)
	}
}

// A process a microVM pod's own kernel stopped is told to the role whose connection reported it
// with the kernel's cause, not the guard's: the process as the launcher named it, its size, the
// pod's memory counter (not the guard's anonymous reading) and its limit, in the guard's units.
func TestAKernelKillTellsItsRoleTheKernelStoppedItsProcess(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")
	token := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleTester))
	report := memoryGuardKillReport()
	report.Cause = shimwire.MemoryKillCauseKernel
	report.Executable = "x`y"

	if _, err := intake.ApplyFact(t.Context(), pool, "launcher", "memory-guard-kill:guard-7f3a:3", intake.MemoryGuardKill{
		Issue: "LEGION-209", Token: token, PodUID: "pod-209", Report: report,
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the kill: %v", err)
	}

	assertRoleMessages(t, pool, "LEGION-209", []roleTold{toldMed(string(token), memoryKill(texts.KillKernel, "x`y", "3.26 GiB"))})
	if rows := outboxRowsOf(t, pool, "LEGION-209"); rows != 1 {
		t.Fatalf("the outbox holds %d rows of LEGION-209, want the one role message", rows)
	}
}

// One of the processes that run a role's session, which the microVM pod's own kernel stopped, is
// told to the role whose connection reported it with a cause of its own, beside the process, its
// size, the pod's memory counter and its limit as the kernel's message of a command gives them.
func TestAKernelKillOfARolesOwnProcessTellsItsRoleItsSessionWasStopped(t *testing.T) {
	pool := migratedPool(t)
	seedTestingIssue(t, pool, "LEGION-209")
	token := claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleTester))
	report := memoryGuardKillReport()
	report.Cause = shimwire.MemoryKillCauseKernelProtected
	report.Executable = "omp"

	if _, err := intake.ApplyFact(t.Context(), pool, "launcher", "memory-guard-kill:guard-7f3a:3", intake.MemoryGuardKill{
		Issue: "LEGION-209", Token: token, PodUID: "pod-209", Report: report,
	}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the kill: %v", err)
	}

	assertRoleMessages(t, pool, "LEGION-209", []roleTold{toldMed(string(token), memoryKill(texts.KillKernelProtected, "omp", "3.26 GiB"))})
	if rows := outboxRowsOf(t, pool, "LEGION-209"); rows != 1 {
		t.Fatalf("the outbox holds %d rows of LEGION-209, want the one role message", rows)
	}
}

// A kill that names an issue the daemon does not record has no role of the workflow's to tell:
// it queues nothing and says so in one line.
func TestMemoryGuardAKillOfAnIssueTheDaemonDoesNotRecordIsLoggedAndDropped(t *testing.T) {
	pool := migratedPool(t)
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))

	if _, err := intake.ApplyFact(t.Context(), pool, "launcher", "memory-guard-kill:guard-7f3a:3", intake.MemoryGuardKill{
		Issue: "LEGION-404", Token: claim.Token(treeRoleToken(t, "LEGION-404", claim.RoleTester)), PodUID: "pod-404", Report: memoryGuardKillReport(),
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the kill: %v", err)
	}

	if rows := outboxRowsOf(t, pool, "LEGION-404"); rows != 0 {
		t.Fatalf("the outbox holds %d rows of an issue the daemon does not record, want none", rows)
	}
	if text := logs.String(); !strings.Contains(text, "names an issue the daemon does not record") || !strings.Contains(text, "issue=LEGION-404") {
		t.Fatalf("log = %q, want one line naming the issue the kill was dropped for", text)
	}
}

// memoryKill is the memory-kill message of LEGION-209 for memoryGuardKillReport's sizes, with
// cause, the process and the pod's memory reading the cause names.
func memoryKill(cause, process, pod string) texts.MemoryKillRecord {
	return texts.MemoryKillRecord{RoleHead: texts.RoleHead{Kind: texts.MemoryKill, Issue: "LEGION-209"}, Cause: cause, Process: process,
		PID: 4242, Size: "812.40 MiB", PodMemory: pod, Limit: "4.00 GiB"}
}

// Sizes are written in the largest binary unit they reach, with two decimals, and in whole bytes
// below a KiB; a size two decimals would round up to 1024.00 of a unit is the next unit's 1.00.
func TestMemoryGuardTheNoteWritesSizesInBinaryUnits(t *testing.T) {
	for _, tc := range []struct {
		bytes uint64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{851862979, "812.40 MiB"},
		{3360811909, "3.13 GiB"},
		{4 << 30, "4.00 GiB"},
		{1<<30 - 1, "1.00 GiB"},
		{20401094656, "19.00 GiB"},
	} {
		if got := binarySize(tc.bytes); got != tc.want {
			t.Errorf("binarySize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
