package workflow

// A process an issue pod's memory guard ended is told to the role whose process it was, on that
// role's own topic, and to nobody else (dispatch://INFRA-3557). One the controller pod's guard
// ended, or its microVM's own kernel stopped, is told to the project's controller alone, in a
// controller notice of its own for each kill (dispatch://INFRA-3531, dispatch://INFRA-4567).

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
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
// cause, the process and the pod's memory reading the cause names, and where heavy work runs,
// which every cause says.
func memoryKill(cause, process, pod string) texts.MemoryKillRecord {
	return texts.MemoryKillRecord{RoleHead: texts.RoleHead{Kind: texts.MemoryKill, Issue: "LEGION-209"}, Cause: cause, Process: process,
		PID: 4242, Size: "812.40 MiB", PodMemory: pod, Limit: "4.00 GiB", HeavyWorkRuns: texts.MemoryKillHeavyWork}
}

// controllerKill is the memory-guard-kill notice of LEGION's controller for memoryGuardKillReport's
// sizes, as the workflow queues it: cause, the process and its pid, and the pod's memory reading
// the cause names.
func controllerKill(t *testing.T, cause, process string, pid int, pod string) record.ControllerNotice {
	t.Helper()
	notice, err := record.ControllerNoticeOf(texts.MemoryGuardKillRecord{NoticeHead: texts.NoticeHead{Kind: texts.MemoryGuardKill, Issue: "LEGION"},
		Cause: cause, Process: process, PID: pid, Size: "812.40 MiB", PodMemory: pod, Limit: "4.00 GiB"})
	if err != nil {
		t.Fatalf("the memory-guard-kill notice of %s: %v", process, err)
	}
	return notice
}

// applyControllerKill applies one kill of the controller pod's process, credited to token, under
// the id the daemon applies such a report under.
func applyControllerKill(t *testing.T, pool *pgxpool.Pool, engine *Engine, token claim.Token, report shimwire.LauncherMemoryKill) {
	t.Helper()
	id := fmt.Sprintf("memory-guard-kill:%s:%d", report.Guard, report.Sequence)
	if _, err := intake.ApplyFact(t.Context(), pool, "launcher", id, intake.ControllerMemoryGuardKill{
		Token: token, PodUID: "pod-controller", Report: report,
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the controller's kill %s: %v", id, err)
	}
}

// A kill of one of the controller's own processes is told to the project's controller as one
// controller notice of kind memory-guard-kill, filed under the project key as a tick is. Its record
// names the controller's role, what stopped the process (the guard), the process by its executable
// alone, with none of its arguments, its pid, its size, the pod's reading and its limit, and
// nothing more: what the controller does then is its prompt's, since its skill has no section for
// this notice (dispatch://INFRA-3368/ask/aa99c6ba-8a58-4830-998c-7f8102f65dd0). Nothing else is
// queued. A second kill is a second notice, though the first is still unsent: each is a program
// the controller lost.
func TestMemoryGuardAControllerKillIsOneControllerNoticeUnderTheProjectKey(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	token := claim.ControllerToken("legion")
	first := memoryGuardKillReport()

	applyControllerKill(t, pool, engine, token, first)

	told := controllerKill(t, texts.KillGuard, "python3", 4242, "3.13 GiB")
	if got := noticeRows(t, pool); !reflect.DeepEqual(got, []record.OutboxPayload{told}) {
		t.Fatalf("notice rows = %+v, want the one controller notice %+v", got, told)
	}
	if rows, all := outboxRowsOf(t, pool, "LEGION"), outboxRows(t, pool); rows != 1 || all != 1 {
		t.Fatalf("the outbox holds %d rows under the project key LEGION and %d in all, want the one notice under it and nothing else", rows, all)
	}

	second := first
	second.Sequence, second.PID, second.Executable = 4, 5151, "node"
	applyControllerKill(t, pool, engine, token, second)
	got := noticeRows(t, pool)
	nextTold := controllerKill(t, texts.KillGuard, "node", 5151, "3.13 GiB")
	if !reflect.DeepEqual(got, []record.OutboxPayload{told, nextTold}) {
		t.Fatalf("notice rows after the second kill = %+v, want the first notice and a second one naming node (pid 5151): %+v", got, nextTold)
	}
	if rows := outboxRowsOf(t, pool, "LEGION"); rows != 2 {
		t.Fatalf("the outbox holds %d rows under the project key after two kills, want two", rows)
	}
}

// A kill whose token is not this project's controller's is no kill of this controller's program:
// another project's controller token and an issue role's token each queue nothing, and each says
// so in one line.
func TestMemoryGuardAControllerKillOfATokenThatIsNotTheControllersIsLoggedAndDropped(t *testing.T) {
	pool := migratedPool(t)
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
	report := memoryGuardKillReport()
	for i, token := range []claim.Token{claim.ControllerToken("other"), claim.Token(treeRoleToken(t, "LEGION-209", claim.RoleTester))} {
		report.Sequence = uint64(i + 1)
		applyControllerKill(t, pool, engine, token, report)
	}

	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("the outbox holds %d rows, want none for a token that is not the controller's", rows)
	}
	text := logs.String()
	if got := strings.Count(text, "names a token that is not this project's controller"); got != 2 {
		t.Fatalf("log = %q, want one line for each of the two dropped kills", text)
	}
	if !strings.Contains(text, "role=legion-other-controller") || !strings.Contains(text, "role=legion-legion-legion-209-tester") {
		t.Fatalf("log = %q, want each line to name the token the kill was dropped for", text)
	}
}

// A process the controller's microVM pod's own kernel stopped is told to the controller with the
// kernel's cause and not the guard's, as a role's is
// (TestAKernelKillTellsItsRoleTheKernelStoppedItsProcess,
// TestAKernelKillOfARolesOwnProcessTellsItsRoleItsSessionWasStopped), each in the controller
// notice a guard's kill is told in. One of a command (cause kernel) and one of the processes that
// run the controller's session (cause kernel-protected) each name the process as the launcher
// sent it, its size, the pod's memory counter (not the guard's anonymous reading) and its limit;
// what each means for the controller's session is its prompt's. A report of no cause, the guard's
// of a gVisor controller pod, is still told with the guard's cause, and the line logged for each
// kill names what stopped the process and its cause.
func TestAKernelKillInTheControllersPodTellsTheControllerTheKernelStoppedIt(t *testing.T) {
	pool := migratedPool(t)
	var logs bytes.Buffer
	engine := testEngine(config.DesignGateRootIssues, slog.New(slog.NewTextHandler(&logs, nil)))
	token := claim.ControllerToken("legion")

	var told []record.OutboxPayload
	for i, tc := range []struct {
		cause      shimwire.MemoryKillCause
		executable string
		word       string
		pod        string
		logged     string
	}{
		{shimwire.MemoryKillCauseKernel, "x`y", texts.KillKernel, "3.26 GiB",
			`msg="workflow: the controller whose program the pod's kernel stopped is told why" role=legion-legion-controller pod=pod-controller guard=guard-7f3a sequence=1 pid=4242 cause=kernel`},
		{shimwire.MemoryKillCauseKernelProtected, "omp", texts.KillKernelProtected, "3.26 GiB",
			`msg="workflow: the controller is told why the pod's kernel stopped one of the processes that run its session" role=legion-legion-controller pod=pod-controller guard=guard-7f3a sequence=2 pid=4242 cause=kernel-protected`},
		{shimwire.MemoryKillCauseGuard, "python3", texts.KillGuard, "3.13 GiB",
			`msg="workflow: the controller whose program the memory guard ended is told why" role=legion-legion-controller pod=pod-controller guard=guard-7f3a sequence=3 pid=4242 cause=""`},
	} {
		report := memoryGuardKillReport()
		report.Sequence, report.Cause, report.Executable = uint64(i+1), tc.cause, tc.executable
		applyControllerKill(t, pool, engine, token, report)

		told = append(told, controllerKill(t, tc.word, tc.executable, 4242, tc.pod))
		if got := noticeRows(t, pool); !reflect.DeepEqual(got, told) {
			t.Fatalf("notice rows after the kill of cause %q = %+v\nwant %+v", tc.cause, got, told)
		}
		if text := logs.String(); !strings.Contains(text, tc.logged) {
			t.Fatalf("log = %q, want a line holding %q", text, tc.logged)
		}
	}
	if rows, all := outboxRowsOf(t, pool, "LEGION"), outboxRows(t, pool); rows != 3 || all != 3 {
		t.Fatalf("the outbox holds %d rows under the project key LEGION and %d in all, want the three notices under it and nothing else", rows, all)
	}
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
