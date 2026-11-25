package main

// parent_status_fix_boot_test.go boots the real binary with DISPATCH_PARENT_STATUS_RULE and holds
// R25's one-time fix at boot (api.RunParentStatusFixAtBoot) where the test can see it: the
// process answers no request until the fix's changes and its record are stored, and a process
// that finds the fix's lease held waits for the record that the holder writes.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// fixBootIssues are the issues of each boot's database, which break R24 twice: ACME-2 is open
// under ACME-1, which is done, and ACME-4 is in work under ACME-3, which nobody started. R25's fix
// unlinks ACME-2 and moves ACME-3 to in_progress.
var fixBootIssues = []struct{ key, status, parent string }{
	{"ACME-1", "done", ""},
	{"ACME-2", "backlog", "ACME-1"},
	{"ACME-3", "backlog", ""},
	{"ACME-4", "in_progress", "ACME-3"},
}

// fixBootRuleOn is the environment that turns the rule on, so the boot runs the fix.
var fixBootRuleOn = map[string]string{"DISPATCH_PARENT_STATUS_RULE": "on"}

// fixBootAsked are the requests the test sends a booting process: the health probe, and a read of
// the issue that the fix unlinks.
var fixBootAsked = []string{"/healthz", "/api/v1/issues/ACME-2"}

// fixBootAskBound is how long one request waits for its answer, and fixBootAskPause how long an
// asker waits after an answer before it asks again.
const (
	fixBootAskBound = 250 * time.Millisecond
	fixBootAskPause = 50 * time.Millisecond
)

// TestTheServerAnswersOnlyAfterTheParentStatusFix boots the real binary three times, one process
// at a time, each on a fresh database whose issues break R24 (fixBootIssues) and that holds no
// record of R25's fix. With DISPATCH_PARENT_STATUS_RULE on, main runs the fix before the process
// serves, so no request is answered until the fix's changes and its record are stored: the first
// boot holds the fix on a row that it writes and then on the table of its record, and the second
// holds the fix's lease, as another process that runs the fix holds it, until the test writes the
// record itself. With the setting off, the process answers and changes nothing. Each boot then
// shows that the rule the boot answered reached the server: a write that breaks the rule is
// refused with the setting on and taken with it off.
func TestTheServerAnswersOnlyAfterTheParentStatusFix(t *testing.T) {
	t.Run("on, with a row that the fix writes locked", func(t *testing.T) {
		database := breakTheRule(t)
		// The fix's first step unlinks ACME-2, so its write waits for this lock of the row.
		row := holdInTransaction(t, database, `select key from issues where key = 'ACME-2' for update`)
		process := startFixBoot(t, database, fixBootRuleOn)
		asker := askWhileBooting(t, process)
		waitForLockWaiter(t, process, row, "the fix to wait for the row lock of ACME-2")
		asker.waitUnanswered(t, process, "requests sent while the fix waits for the row lock")

		// The table of the record, in exclusive mode: a read of it passes, and the insert of the
		// record waits.
		record := holdInTransaction(t, database, `lock table parent_status_fix in exclusive mode`)
		release(t, row)
		waitForLockWaiter(t, process, record, "the record of the fix to wait for the lock of its table")
		// Each change of the fix and its comment are stored, and the record is not.
		requireFixBootIssues(t, database, fixBootFixed())
		requireFixBootComments(t, database, "ACME-2", "ACME-3")
		asker.waitUnanswered(t, process, "requests sent while the record of the fix waits")

		recordReleased := time.Now()
		release(t, record)
		answers := asker.untilAnswered(t, process)
		for _, answer := range answers {
			if answer.at.Before(recordReleased) {
				t.Errorf("GET %s was answered %s before the record of the fix could be stored: %d %s",
					answer.path, recordReleased.Sub(answer.at), answer.status, answer.body)
			}
		}
		// The first answer of the read already shows the fix's change.
		read := answers[slices.IndexFunc(answers, func(answer fixBootAnswer) bool { return answer.path == fixBootAsked[1] })]
		var issue struct {
			Parent *string `json:"parent"`
		}
		if err := json.Unmarshal(read.body, &issue); err != nil || read.status != http.StatusOK || issue.Parent != nil {
			t.Errorf("the first answer of GET %s is %d %s (%v), want ACME-2 with no parent", read.path, read.status, read.body, err)
		}
		requireFixBootIssues(t, database, fixBootFixed())
		requireFixBootComments(t, database, "ACME-2", "ACME-3")
		if got := fixBootRecords(t, database); got != "1 1" {
			t.Errorf("the records of the fix (unlinked, moved) are %q, want %q", got, "1 1")
		}
		requireTheRule(t, process, true)
		stopFixBoot(t, process)
	})

	t.Run("on, with the lease of the fix held", func(t *testing.T) {
		database := breakTheRule(t)
		before := fixBootIssueRecord(t, database)
		endPass := holdFixLease(t, database)
		process := startFixBoot(t, database, fixBootRuleOn)
		asker := askWhileBooting(t, process)
		testwait.Eventually(t, "the boot to wait for the record of the fix that another process runs", func() bool {
			requireBooting(t, process)
			return strings.Contains(process.Output.String(), "the boot waits for the parent status fix")
		})
		asker.waitUnanswered(t, process, "requests sent while the boot waits for the record of the fix")

		recorded := time.Now()
		if _, err := database.Pool.Exec(context.Background(), `insert into parent_status_fix (unlinked, moved) values (0, 0)`); err != nil {
			t.Fatalf("write the record of the fix, as the process that holds its lease does: %v", err)
		}
		for _, answer := range asker.untilAnswered(t, process) {
			if answer.at.Before(recorded) {
				t.Errorf("GET %s was answered %s before the record of the fix was written: %d %s",
					answer.path, recorded.Sub(answer.at), answer.status, answer.body)
			}
		}
		// The boot also writes its dashboard assets, its registry row, its loops' duty_passes rows
		// and the eval registry's seed of the project, so the record compares the issues alone.
		if after := fixBootIssueRecord(t, database); after != before {
			t.Errorf("the boot changed the issues, their comments or their events while another process held the lease of the fix:\nbefore %s\nafter  %s", before, after)
		}
		if got := fixBootRecords(t, database); got != "0 0" {
			t.Errorf("the records of the fix (unlinked, moved) are %q, want only the test's %q", got, "0 0")
		}
		requireTheRule(t, process, true)
		if !endPass() {
			t.Error("the test's pass under the lease of the fix did not run")
		}
		stopFixBoot(t, process)
	})

	t.Run("off", func(t *testing.T) {
		database := breakTheRule(t)
		before := fixBootIssueRecord(t, database)
		process := startFixBoot(t, database, nil)
		process.waitHealthy(t)
		if after := fixBootIssueRecord(t, database); after != before {
			t.Errorf("the boot with the setting off changed the issues, their comments or their events:\nbefore %s\nafter  %s", before, after)
		}
		requireFixBootIssues(t, database, fixBootLoaded())
		if got := fixBootRecords(t, database); got != "" {
			t.Errorf("the records of the fix (unlinked, moved) are %q, want none", got)
		}
		requireTheRule(t, process, false)
		stopFixBoot(t, process)
	})
}

// breakTheRule is a fresh database whose issues are fixBootIssues and that holds no record of the
// fix.
func breakTheRule(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee, next_number) values ('ACME', 'Acme', 'alice', $1)`,
		len(fixBootIssues)+1); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	for index, issue := range fixBootIssues {
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, parent_key, created_by, rank, closed_at)
			values ($1, 'ACME', $2, $3, $4::text, nullif($5::text, ''), '{"kind":"user","id":"alice"}', $6,
			        case when $4::text = 'done' then now() end)
		`, issue.key, index+1, "Issue "+issue.key, issue.status, issue.parent, fmt.Sprintf("U%06d", index+1)); err != nil {
			t.Fatalf("load %s: %v", issue.key, err)
		}
	}
	if got := fixBootRecords(t, database); got != "" {
		t.Fatalf("the fresh database holds the records %q of the fix, want none", got)
	}
	return database
}

// fixBootLoaded is each issue as breakTheRule loads it, as "<status> <parent>", with "-" for no
// parent.
func fixBootLoaded() map[string]string {
	loaded := make(map[string]string, len(fixBootIssues))
	for _, issue := range fixBootIssues {
		loaded[issue.key] = issue.status + " " + cmp.Or(issue.parent, "-")
	}
	return loaded
}

// fixBootFixed is each issue as the fix leaves it: ACME-2 with no parent, and ACME-3 in work.
func fixBootFixed() map[string]string {
	fixed := fixBootLoaded()
	fixed["ACME-2"] = "backlog -"
	fixed["ACME-3"] = "in_progress -"
	return fixed
}

// requireFixBootIssues fails t unless the database holds each issue as want gives it.
func requireFixBootIssues(t *testing.T, database *store.Store, want map[string]string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select key, status || ' ' || coalesce(parent_key, '-') from issues`)
	if err != nil {
		t.Fatalf("read the issues: %v", err)
	}
	got := map[string]string{}
	var key, issue string
	if _, err := pgx.ForEachRow(rows, []any{&key, &issue}, func() error {
		got[key] = issue
		return nil
	}); err != nil {
		t.Fatalf("read the issues: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("the issues are %v, want %v", got, want)
	}
}

// requireFixBootComments fails t unless the comments that Dispatch's system actor wrote are one
// on each of keys, as the fix's two steps leave them.
func requireFixBootComments(t *testing.T, database *store.Store, keys ...string) {
	t.Helper()
	var commented []string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(array_agg(issue_key order by issue_key), '{}') from comments
		where author ->> 'kind' = 'system' and author ->> 'id' = 'parent-status-rule'
	`).Scan(&commented); err != nil {
		t.Fatalf("read the fix's comments: %v", err)
	}
	if !slices.Equal(commented, keys) {
		t.Errorf("Dispatch's system actor commented on %q, want one comment on each of %q", commented, keys)
	}
}

// fixBootRecords is each record of the fix in the database, as "<unlinked> <moved>", joined by
// commas: empty when there is none.
func fixBootRecords(t *testing.T, database *store.Store) string {
	t.Helper()
	var records string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(string_agg(unlinked || ' ' || moved, ','), '') from parent_status_fix`).Scan(&records); err != nil {
		t.Fatalf("read the records of the fix: %v", err)
	}
	return records
}

// fixBootIssueRecord is what a boot that runs no fix leaves as it found it, as JSON text: each
// issue's status, parent and last sequence, every comment, and the issues' own events, the events
// whose issue_key is set. A project's events, which carry artifact_id (a project document's) or
// project_key instead, are left out: the server's own writes to a project at boot, such as the
// eval registry's seed of the project's evals-md document, are no change of the issues.
func fixBootIssueRecord(t *testing.T, database *store.Store) string {
	t.Helper()
	var record string
	if err := database.Pool.QueryRow(context.Background(), `
		select jsonb_build_object(
			'issues', (select coalesce(jsonb_agg(jsonb_build_array(key, status, parent_key, last_seq) order by key), '[]') from issues),
			'comments', (select coalesce(jsonb_agg(to_jsonb(c) order by c.id), '[]') from comments c),
			'events', (select coalesce(jsonb_agg(to_jsonb(e) order by e.id), '[]') from events e where e.issue_key is not null)
		)::text
	`).Scan(&record); err != nil {
		t.Fatalf("read the issues, their comments and their events: %v", err)
	}
	return record
}

// holdInTransaction runs statement in a transaction of the test's own, which holds what the
// statement locks until release ends it, or the test ends.
func holdInTransaction(t *testing.T, database *store.Store, statement string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction for %q: %v", statement, err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return holder
}

// release ends holder's transaction, and with it every lock that it holds.
func release(t *testing.T, holder pgx.Tx) {
	t.Helper()
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("end the transaction that holds the lock: %v", err)
	}
}

// waitForLockWaiter waits until pg_locks shows a session that waits for a lock that holder's
// transaction holds. The database is the process's alone, and the test's own sessions wait for
// nothing, so the waiter is the process.
func waitForLockWaiter(t *testing.T, process *dispatchProcess, holder pgx.Tx, what string) {
	t.Helper()
	testwait.Eventually(t, what, func() bool {
		requireBooting(t, process)
		var waiting bool
		if err := holder.QueryRow(context.Background(), `
			select exists (select 1 from pg_locks where not granted and pg_backend_pid() = any(pg_blocking_pids(pid)))
		`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		return waiting
	})
}

// holdFixLease takes the lease parent-status-fix (api's parentStatusFixLease) in a pass of the
// test's own, as a process that runs the fix holds it. The pass holds it until the returned end
// runs, or the test ends, and end answers whether the pass ran. A try of the lease that ends
// before its pass runs fails the test, and so does a pass that does not start within a minute.
func holdFixLease(t *testing.T, database *store.Store) (end func() bool) {
	t.Helper()
	leases := storetest.Leases(t, database.Pool)
	entered, done, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var passed bool
	go func() {
		defer close(finished)
		passed = duty.Exclusive(context.Background(), leases, "parent-status-fix", time.Time{}, 0, func(context.Context) {
			close(entered)
			<-done
		})
	}()
	var once sync.Once
	end = func() bool {
		once.Do(func() { close(done) })
		<-finished
		return passed
	}
	t.Cleanup(func() { end() })
	bound := testwait.Bounded(t, time.Minute, time.Second)
	select {
	case <-entered:
	case <-finished:
		t.Fatalf("the lease parent-status-fix answered %t before the test's pass under it ran", passed)
	case <-time.After(bound):
		t.Fatalf("the test's pass under the lease parent-status-fix did not start within %v", bound)
	}
	return end
}

// startFixBoot starts Dispatch on database with env added to its environment, and logs its output
// when the test fails.
func startFixBoot(t *testing.T, database *store.Store, env map[string]string) *dispatchProcess {
	t.Helper()
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: env})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s\n%s", dispatchOutputHeading, process.Output.String())
		}
	})
	return process
}

// requireBooting fails t when the process has exited.
func requireBooting(t *testing.T, process *dispatchProcess) {
	t.Helper()
	select {
	case <-process.Exited:
		t.Fatalf("Dispatch exited during its boot. %s", process.exitCause())
	default:
	}
}

// requireTheRule fails t unless the server holds the rule as on says: a change of ACME-3 to
// triage while ACME-4 is in work under it breaks R24, so with the rule on it is refused 409
// PARENT_STATUS_CONFLICT, and with it off it is taken.
func requireTheRule(t *testing.T, process *dispatchProcess, on bool) {
	t.Helper()
	answer := process.send(t, http.MethodPatch, "/api/v1/issues/ACME-3", `{"status":"triage"}`, asUser(dispatchTestLogin))
	switch {
	case on && (answer.status != http.StatusConflict || answer.code() != "PARENT_STATUS_CONFLICT"):
		t.Errorf("with the rule on, a change of ACME-3 to triage over ACME-4 in work answered %s, want 409 PARENT_STATUS_CONFLICT", answer)
	case !on && answer.status != http.StatusOK:
		t.Errorf("with the rule off, a change of ACME-3 to triage over ACME-4 in work answered %s, want 200", answer)
	}
}

// stopFixBoot stops the process as a deploy does, and fails t unless it exits 0.
func stopFixBoot(t *testing.T, process *dispatchProcess) {
	t.Helper()
	process.Terminate(t)
	process.WaitExit(t, "SIGTERM")
	if code := process.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}
}

// fixBootAnswer is one answer of a booting process: the path it answered, when it came, and its
// status and body.
type fixBootAnswer struct {
	path   string
	at     time.Time
	status int
	body   []byte
}

// fixBootAsker sends a booting process each of fixBootAsked over and over, from a goroutine for
// each, every request bounded by fixBootAskBound, and keeps each answer with the moment it came.
// A process that does not serve yet answers nothing: the kernel queues each connection on the
// socket that the process holds (cmdtest.HoldPort), and the request waits there until its bound
// ends it.
type fixBootAsker struct {
	stop    context.CancelFunc
	running sync.WaitGroup

	mu sync.Mutex
	// unanswered counts, by path, the requests whose bound ended them.
	unanswered map[string]int
	answers    []fixBootAnswer
}

// askWhileBooting starts an asker of process, which runs until untilAnswered or the test ends.
func askWhileBooting(t *testing.T, process *dispatchProcess) *fixBootAsker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	asker := &fixBootAsker{stop: cancel, unanswered: map[string]int{}}
	t.Cleanup(asker.halt)
	for _, path := range fixBootAsked {
		request, err := http.NewRequest(http.MethodGet, process.url(path), nil)
		if err != nil {
			t.Fatalf("build GET %s: %v", path, err)
		}
		request.Header = asUser(dispatchTestLogin)
		asker.running.Go(func() {
			for ctx.Err() == nil {
				asker.ask(ctx, request, path)
			}
		})
	}
	return asker
}

// ask sends request, the request of path, once, and keeps what came of it.
func (a *fixBootAsker) ask(ctx context.Context, request *http.Request, path string) {
	bounded, cancel := context.WithTimeout(ctx, fixBootAskBound)
	defer cancel()
	response, err := http.DefaultClient.Do(request.Clone(bounded))
	if err != nil {
		if ctx.Err() == nil {
			a.mu.Lock()
			a.unanswered[path]++
			a.mu.Unlock()
		}
		return
	}
	at := time.Now()
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	a.mu.Lock()
	a.answers = append(a.answers, fixBootAnswer{path: path, at: at, status: response.StatusCode, body: body})
	a.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(fixBootAskPause):
	}
}

// state is what the asker has kept so far.
func (a *fixBootAsker) state() (map[string]int, []fixBootAnswer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.unanswered), slices.Clone(a.answers)
}

// halt stops the asker and waits for its goroutines.
func (a *fixBootAsker) halt() {
	a.stop()
	a.running.Wait()
}

// waitUnanswered waits until two more requests of each path have gone unanswered, and fails t
// when the process answers any request meanwhile: the test holds the boot before it serves.
func (a *fixBootAsker) waitUnanswered(t *testing.T, process *dispatchProcess, what string) {
	t.Helper()
	before, _ := a.state()
	testwait.Eventually(t, what, func() bool {
		requireBooting(t, process)
		unanswered, answers := a.state()
		if len(answers) > 0 {
			t.Fatalf("GET %s was answered while the test held the fix: %d %s", answers[0].path, answers[0].status, answers[0].body)
		}
		for _, path := range fixBootAsked {
			if unanswered[path] < before[path]+2 {
				return false
			}
		}
		return true
	})
}

// untilAnswered waits until the process has joined the registry of live server processes and
// answered each path 200, then stops the asker and answers every answer it kept, in the order
// they came.
func (a *fixBootAsker) untilAnswered(t *testing.T, process *dispatchProcess) []fixBootAnswer {
	t.Helper()
	testwait.Eventually(t, "an answer of 200 to each request, once the process has joined the registry", func() bool {
		requireBooting(t, process)
		if !strings.Contains(process.Output.String(), `msg="dispatch: registered this server process"`) {
			return false
		}
		_, answers := a.state()
		for _, path := range fixBootAsked {
			if !slices.ContainsFunc(answers, func(answer fixBootAnswer) bool { return answer.path == path && answer.status == http.StatusOK }) {
				return false
			}
		}
		return true
	})
	a.halt()
	_, answers := a.state()
	slices.SortFunc(answers, func(x, y fixBootAnswer) int { return x.at.Compare(y.at) })
	return answers
}
