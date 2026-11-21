//go:build twobuild

// What the steps of the two-build suite's attribution row (twobuild_attribution_test.go) run with:
// the gate on the delivery reconcile's lease, which lets one pass run at a time, the hold on the
// passes of the previous build, and the reads and writes of the issues, the results and the stored
// pull request rows that each step makes and checks.

package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// reconcileLeaseNamespace is the first key of every loop lease: duty's leaseNamespace, which
// stays unexported, the four bytes of "LOOP" (memory_harness_test.go names it too). The second
// key is the lease's name, which Postgres hashes with hashtext.
const reconcileLeaseNamespace int32 = 0x4C4F4F50

// reconcileGate holds the delivery reconcile's lease (delivery.ReconcileLease) on a connection of
// the test. While the test holds it, each tick of either build finds the lease held and runs no
// pass (duty.Exclusive). onePass lets one pass of a build run.
type reconcileGate struct {
	pool *store.Pool
	conn *pgxpool.Conn
	held bool
}

// closedReconcileGate takes the lease before either build starts, and gives it back when the
// test ends. It fails the test when the database does not then show the lease held, because the
// gate would then take another lock than the reconcile takes.
func closedReconcileGate(t *testing.T, database *store.Store) *reconcileGate {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the gate of the reconcile: %v", err)
	}
	gate := &reconcileGate{pool: database.Pool, conn: conn}
	t.Cleanup(func() {
		if gate.held {
			unlock, cancel := duty.LeaseCall()
			_, err := conn.Exec(unlock, `select pg_advisory_unlock($1, hashtext($2))`, reconcileLeaseNamespace, delivery.ReconcileLease)
			cancel()
			if err != nil {
				// Postgres gives a session's locks back when the session ends.
				closing, cancel := duty.LeaseCall()
				_ = conn.Hijack().Close(closing)
				cancel()
				return
			}
		}
		conn.Release()
	})
	gate.close(t)
	if held, err := duty.LeaseHeld(ctx, database.Pool, delivery.ReconcileLease); err != nil || !held {
		t.Fatalf("the gate took the lease %s, and the database shows it held=%v (%v): the gate takes another lock than the reconcile",
			delivery.ReconcileLease, held, err)
	}
	return gate
}

// close takes the lease, and waits behind a pass that holds it.
func (g *reconcileGate) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := g.conn.Exec(ctx, `select pg_advisory_lock($1, hashtext($2))`, reconcileLeaseNamespace, delivery.ReconcileLease); err != nil {
		t.Fatalf("take the lease %s: %v", delivery.ReconcileLease, err)
	}
	g.held = true
}

// open gives the lease back.
func (g *reconcileGate) open(t *testing.T) {
	t.Helper()
	ctx, cancel := duty.LeaseCall()
	defer cancel()
	var released bool
	if err := g.conn.QueryRow(ctx, `select pg_advisory_unlock($1, hashtext($2))`, reconcileLeaseNamespace, delivery.ReconcileLease).Scan(&released); err != nil || !released {
		t.Fatalf("give the lease %s back: released=%v, %v", delivery.ReconcileLease, released, err)
	}
	g.held = false
}

// onePass lets exactly one pass of p run, and answers the attributes of its head pass's line. It
// gives the lease back. Once a tick holds the lease, it queues for the lease, so the database
// gives it to the gate when that tick gives it back, before the next tick of any process.
//
// A tick of the previous build can hold the lease too: it finds its period served and runs no
// pass (holdThePreviousBuildsPasses). p then logged no pass, and the gate opens again.
func (g *reconcileGate) onePass(t *testing.T, p *dispatchProcess, step string) map[string]string {
	t.Helper()
	before := len(headPasses(p))
	passed := func() bool { return len(headPasses(p)) > before }
	for attempt := 1; ; attempt++ {
		g.open(t)
		deadline := time.Now().Add(2 * time.Minute)
		for {
			held, err := duty.LeaseHeld(context.Background(), g.pool, delivery.ReconcileLease)
			if err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if held || passed() {
				break
			}
			select {
			case <-p.Exited:
				t.Fatalf("%s: the current build exited while the gate was open:\n%s", step, p.Output.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no tick took the lease %s within 2 minutes of the gate opening:\n%s", step, delivery.ReconcileLease, p.Output.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		g.close(t)
		// The head pass logs its line before the pass gives the lease back. The test reads the
		// process's output through a pipe, so it waits a while for the line.
		if testwait.Holds(10*time.Second, 20*time.Millisecond, passed) {
			break
		}
		if attempt == 5 {
			t.Fatalf("%s: five ticks held the lease, and the current build logged no pass:\n%s", step, p.Output.String())
		}
		t.Logf("%s: a tick that ran no pass of the current build held the lease; the gate opens again", step)
	}
	lines := headPasses(p)
	if len(lines) != before+1 {
		t.Fatalf("%s: the current build logged %d passes while the gate was open, want 1:\n%s", step, len(lines)-before, strings.Join(lines[before:], "\n"))
	}
	t.Logf("%s: %s", step, strings.TrimSpace(lines[before]))
	attributes := map[string]string{}
	for _, match := range attributionLogAttribute.FindAllStringSubmatch(lines[before], -1) {
		attributes[match[1]] = strings.Trim(match[2], `"`)
	}
	return attributes
}

// headPasses is each whole line of a head pass that p has logged, without its newline. A line
// that the pipe has not yet carried to its end is left out, so no reader parses half of its
// counts. logLines cannot tell the two apart, because it trims the newline.
func headPasses(p *dispatchProcess) []string {
	var whole []string
	for line := range strings.Lines(p.Output.String()) {
		if strings.Contains(line, headPassLine) && strings.HasSuffix(line, "\n") {
			whole = append(whole, strings.TrimSuffix(line, "\n"))
		}
	}
	return whole
}

// wantPassCounts fails the test for each count of a head pass's line that is not its want.
func wantPassCounts(t *testing.T, step string, line map[string]string, want map[string]int) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got, err := strconv.Atoi(line[name]); err != nil || got != want[name] {
			t.Errorf("%s: the pass logged %s=%q, want %d", step, name, line[name], want[name])
		}
	}
}

// previousReconcilePeriod is the period of the previous build's delivery reconcile: its
// delivery.ReconcileInterval, as no test setting shortens it there.
const previousReconcilePeriod = 5 * time.Minute

// holdThePreviousBuildsPasses records a pass of the previous build's period that ends in the year
// 2999. Each tick of the previous build then finds its period served and runs no pass (duty's
// readPeriod). So each step reads the previous build before its next pass. The previous build
// keys its record <lease>/<period_ns>. This tree's build runs at 2 s, so the record serves none of
// its ticks. It answers the key of the record.
func holdThePreviousBuildsPasses(t *testing.T, database *store.Store) string {
	t.Helper()
	period := previousReconcilePeriod.Nanoseconds()
	key := delivery.ReconcileLease + "/" + strconv.FormatInt(period, 10)
	if _, err := database.Pool.Exec(context.Background(), `insert into duty_passes (lease, boundary, period_ns) values ($1, $2, $3)`,
		key, time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC), period); err != nil {
		t.Fatalf("record the previous build's period as served: %v", err)
	}
	return key
}

// mapRepositoryTo makes repo the default repository of project, through p.
func mapRepositoryTo(t *testing.T, p *dispatchProcess, repo, project string) {
	t.Helper()
	p.call(t, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, jsonBody(t, map[string]any{"project": project, "default": true}), http.StatusOK)
}

// issueWithCheck creates an issue of project through p, gives it attributionCheck, and fails the
// test when its key is not want.
func issueWithCheck(t *testing.T, p *dispatchProcess, project, title, want string) {
	t.Helper()
	if key, _ := p.createIssueIn(t, project, title, "A spec.\n"); key != want {
		t.Fatalf("the issue %q is %s, want %s", title, key, want)
	}
	p.call(t, http.MethodPost, "/api/v1/issues/"+want+"/artifacts",
		jsonBody(t, map[string]string{"name": "verification.md", "content": attributionCheck}), http.StatusCreated)
}

// recordPass records a pass of the check k1 of issue at commit through p.
func recordPass(t *testing.T, p *dispatchProcess, issue, commit string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/results",
		jsonBody(t, map[string]any{"check": "k1", "role": "implementer", "verdict": "pass", "commit": commit}), http.StatusCreated)
}

// basesOf is each pull request that the check k1 of issue compares its results with, as p
// answers GET /api/v1/issues/{key}/results: its number, with the head commit of the basis.
func basesOf(t *testing.T, p *dispatchProcess, issue string) map[int]string {
	t.Helper()
	var read struct {
		Checks []model.CheckBlock `json:"checks"`
	}
	getJSON(t, p, "/api/v1/issues/"+issue+"/results", &read)
	for _, check := range read.Checks {
		if check.BlockID != "k1" {
			continue
		}
		bases := map[int]string{}
		for _, basis := range check.ComparedWith {
			if basis.PullRequest != nil {
				bases[*basis.PullRequest] = basis.Commit
			}
		}
		return bases
	}
	t.Fatalf("the results of %s hold no check k1: %+v", issue, read.Checks)
	return nil
}

// wantBases fails the test where the check k1 of issue, as p answers it, compares with other pull
// requests than want.
func wantBases(t *testing.T, p *dispatchProcess, name, issue string, want []int, when string) {
	t.Helper()
	got := basesOf(t, p, issue)
	var missing, extra []int
	for _, number := range want {
		if _, found := got[number]; !found {
			missing = append(missing, number)
		}
	}
	for number := range got {
		if !slices.Contains(want, number) {
			extra = append(extra, number)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		slices.Sort(extra)
		t.Errorf("%s: the %s compares %s with %d pull requests, want %d: %s",
			when, name, issue, len(got), len(want), numbersDiff(missing, extra))
	}
}

// numbersDiff names the first few numbers of missing and of extra.
func numbersDiff(missing, extra []int) string {
	first := func(numbers []int) []int { return numbers[:min(len(numbers), 8)] }
	return fmt.Sprintf("%d missing %v, %d more %v", len(missing), first(missing), len(extra), first(extra))
}

// numbersFrom is count numbers from first, in order.
func numbersFrom(first, count int) []int {
	numbers := make([]int, count)
	for i := range numbers {
		numbers[i] = first + i
	}
	return numbers
}

// statementAnswers is the statement's answer for each stored results row, by number, computed in
// the test's own process (delivery.PullRequestIssues) over the database that both builds serve.
func statementAnswers(t *testing.T, database *store.Store) map[int]delivery.PullRequestAttribution {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select repo, number from pull_request_heads`)
	if err != nil {
		t.Fatalf("read the stored results rows: %v", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery.PullRequestKey, error) {
		var key delivery.PullRequestKey
		return key, row.Scan(&key.Repo, &key.Number)
	})
	if err != nil {
		t.Fatalf("read the stored results rows: %v", err)
	}
	answers, err := delivery.PullRequestIssues(ctx, database.Pool, keys)
	if err != nil {
		t.Fatalf("read the statement's answers: %v", err)
	}
	byNumber := make(map[int]delivery.PullRequestAttribution, len(answers))
	for key, answer := range answers {
		byNumber[key.Number] = answer
	}
	return byNumber
}

// heldBy is each number whose answer is issue, in order.
func heldBy(answers map[int]delivery.PullRequestAttribution, issue string) []int {
	var numbers []int
	for number, answer := range answers {
		if answer.IssueKey != nil && *answer.IssueKey == issue {
			numbers = append(numbers, number)
		}
	}
	slices.Sort(numbers)
	return numbers
}

// wantStatement fails the test where the statement's answer of number is not issue, has_facts
// and known.
func wantStatement(t *testing.T, answers map[int]delivery.PullRequestAttribution, number int, issue string, hasFacts bool, when string) {
	t.Helper()
	answer, found := answers[number]
	if !found || answer.IssueKey == nil || *answer.IssueKey != issue || answer.HasFacts != hasFacts || !answer.Known {
		t.Errorf("%s: the statement answers #%d %s, want %s with has_facts %v and known", when, number, describeAnswer(answer, found), issue, hasFacts)
	}
}

func describeAnswer(answer delivery.PullRequestAttribution, found bool) string {
	if !found {
		return "nothing"
	}
	issue := "nobody"
	if answer.IssueKey != nil {
		issue = *answer.IssueKey
	}
	return fmt.Sprintf("%s from %s, has_facts %v, known %v", issue, answer.Source, answer.HasFacts, answer.Known)
}

// wantStoredIsStatement fails the test for each results row whose stored issue, which the
// previous build reads, is not the statement's answer.
func wantStoredIsStatement(t *testing.T, database *store.Store, answers map[int]delivery.PullRequestAttribution, when string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select number, issue_key from pull_request_heads`)
	if err != nil {
		t.Fatalf("read the stored issues: %v", err)
	}
	var differ []string
	stored := 0
	for rows.Next() {
		var number int
		var issue *string
		if err := rows.Scan(&number, &issue); err != nil {
			rows.Close()
			t.Fatalf("read the stored issues: %v", err)
		}
		stored++
		answer := answers[number]
		if !answer.Known || (issue == nil) != (answer.IssueKey == nil) || issue != nil && *issue != *answer.IssueKey {
			differ = append(differ, fmt.Sprintf("#%d stores %v, the statement answers %s", number, nobodyOr(issue), describeAnswer(answer, true)))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored issues: %v", err)
	}
	if len(differ) > 0 {
		t.Errorf("%s: %d of %d results rows store another issue than the statement's answer: %v", when, len(differ), stored, differ[:min(len(differ), 8)])
	}
}

func nobodyOr(issue *string) string {
	if issue == nil {
		return "nobody"
	}
	return *issue
}

// storedPullRequest is the head and the stored issue of the results row of number of repo, and
// the head of its texts row, "" where it has none.
func storedPullRequest(t *testing.T, database *store.Store, repo string, number int) (head, issue, textsHead string) {
	t.Helper()
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `
		select h.head_sha, h.issue_key, coalesce(t.head_sha, '') from pull_request_heads h
		left join pull_request_texts t on t.repo = h.repo and t.number = h.number
		where h.repo = $1 and h.number = $2`, repo, number).Scan(&head, &stored, &textsHead); err != nil {
		t.Fatalf("read the results row of %s#%d: %v", repo, number, err)
	}
	return head, nobodyOr(stored), textsHead
}

// attributionCount runs one count statement over the builds' database.
func attributionCount(t *testing.T, database *store.Store, what, statement string, args ...any) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), statement, args...).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", what, err)
	}
	return count
}

// queuedMoves is the moves that wait for their events (pull_request_attribution_moves).
func queuedMoves(t *testing.T, database *store.Store) int {
	t.Helper()
	return attributionCount(t, database, "the queued moves", `select count(*) from pull_request_attribution_moves`)
}

// attributionEventsOn is the events of a moved attribution that issue carries.
func attributionEventsOn(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	return attributionCount(t, database, "the attribution events of "+issue,
		`select count(*) from events where issue_key = $1 and type = $2`, issue, delivery.AttributionEventType)
}

// storeBacklog stores the backlog of step 5: backlogRows merged rows of backlogRepo, each with
// current texts and its messages read. The branch of each names ACME-2 and its title names
// ACME-1. Each results row stores ACME-1 from the title, as an older rule stored it. The stand-in
// holds the same pull requests.
func storeBacklog(t *testing.T, database *store.Store, fake *attributionFake) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at, github_read_at)
		select $1::text, n, pg_catalog.lpad(pg_catalog.to_hex(n), 40, '0'), 'legion/ACME-2', 'main', 'merged', 'ACME-1', 'title_body',
			'ACME-1 the work', 'https://github.com/' || $1::text || '/pull/' || n, $4::timestamptz, now(), now()
		from pg_catalog.generate_series($2::int, $2::int + $3::int - 1) n
	`, backlogRepo, backlogFirst, backlogRows, backlogUpdate); err != nil {
		t.Fatalf("store the backlog's results rows: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		select h.repo, h.number, h.head_sha, h.head_ref, h.repo, h.base_ref, h.title, '', array['Commit 1.'], h.event_at
		from pull_request_heads h where h.repo = $1 and h.number between $2::int and $2::int + $3::int - 1
	`, backlogRepo, backlogFirst, backlogRows); err != nil {
		t.Fatalf("store the backlog's texts: %v", err)
	}
	for _, number := range numbersFrom(backlogFirst, backlogRows) {
		head := fmt.Sprintf("%040x", number)
		fake.put(backlogRepo, fakePull{number: number, head: head, branch: "legion/ACME-2", title: "ACME-1 the work", updatedAt: backlogUpdate, commits: []string{head}})
	}
}

// storeLaterRows stores the rows of step 6: laterRows merged rows of backlogRepo with no texts, as
// the release before stores a row. Each stores ACME-1, and its title names ACME-1, so a read of
// its facts gives the issue it stores and queues no move. The stand-in holds the same pull
// requests.
func storeLaterRows(t *testing.T, database *store.Store, fake *attributionFake) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at)
		select $1::text, n, pg_catalog.lpad(pg_catalog.to_hex(n), 40, '0'), 'work/no-key', 'main', 'merged', 'ACME-1', 'title_body',
			'ACME-1: an older change', 'https://github.com/' || $1::text || '/pull/' || n, $4::timestamptz, now()
		from pg_catalog.generate_series($2::int, $2::int + $3::int - 1) n
	`, backlogRepo, laterFirst, laterRows, backlogUpdate); err != nil {
		t.Fatalf("store the rows with no facts: %v", err)
	}
	for _, number := range numbersFrom(laterFirst, laterRows) {
		head := fmt.Sprintf("%040x", number)
		fake.put(backlogRepo, fakePull{number: number, head: head, branch: "work/no-key", title: "ACME-1: an older change", updatedAt: backlogUpdate, commits: []string{head}})
	}
}
