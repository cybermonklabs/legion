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
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// memoryGuardKillReport is a kill whose sizes print as 812.40 MiB, 3.13 GiB and 4.00 GiB.
func memoryGuardKillReport() shimwire.LauncherMemoryKill {
	return shimwire.LauncherMemoryKill{Guard: "guard-7f3a", Sequence: 3, PID: 4242, Command: "python3 -m pytest tests/",
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

// The kill is told to the role whose connection reported it, as one role message addressed to its
// claim token: the note names the command, its size, the pod's reading and its limit, and points
// to where heavy work runs. Nothing else is queued for the issue: no architect notice, no Dispatch
// message.
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

	want := "The launcher's memory guard stopped one of your processes with SIGKILL because the pod was near its memory limit: " +
		"`python3 -m pytest tests/` (pid 4242) held 812.40 MiB of anonymous memory while the pod's anonymous memory read 3.13 GiB of its 4.00 GiB limit. " +
		"The pod's other processes and your session go on. " +
		"Heavy work — a build, a whole test suite, anything that needs much memory — runs where your deployment's instructions say, not in this pod: " +
		"read skill://legion-worker/references/tools.md before you run it again."
	assertRoleMessages(t, pool, "LEGION-209", []record.RoleMessage{{Role: string(token), Body: want}})
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

// The command sits in a code span that nothing in it can end or spread over lines: the fence is
// longer than any run of backticks the command holds, a command that begins or ends with a
// backtick is padded, and a character that does not print is written as its escape.
func TestMemoryGuardTheNoteKeepsTheCommandInsideItsCodeSpan(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"python3 grow.py", "`python3 grow.py`"},
		{"sh -c 'echo `id`'", "``sh -c 'echo `id`'``"},
		{"bash -c 'a ``` b'", "````bash -c 'a ``` b'````"},
		{"`id` x", "`` `id` x ``"},
		{"x `id`", "`` x `id` ``"},
		{" padded ", "`  padded  `"},
		{"two\nlines\x1b[31m", "`two\\nlines\\x1b[31m`"},
		{"reorder\u202etext", "`reorder\\u202etext`"},
		{"", "` `"},
	} {
		if got := codeSpan(tc.command); got != tc.want {
			t.Errorf("codeSpan(%q) = %q, want %q", tc.command, got, tc.want)
		}
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
