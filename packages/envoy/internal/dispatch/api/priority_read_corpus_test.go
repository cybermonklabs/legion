//go:build !race

package api

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// How the corpus of production's counts the priorities read is measured and compared over is
// built (dispatch://INFRA-5791, R1 to R3), and what it is held to: the corpus of
// TestThePrioritiesReadAnswersWithinItsBoundOnEveryPlan,
// TestThePrioritiesReadKeepsItsPlansWithAProjectNamed,
// TestThePrioritiesReadAnswersAsTheMergeBaseAnswers and
// TestThePrioritiesReadWalksIssuesOnceOnTheCorpus. It is a file of its own for the reason
// priority_speed_seed_test.go is: none of it is a reading.
//
// The projects and their first issues are written here, by COPY (priority_fixture_test.go); every
// other row by testdata/priorities-read-corpus.sql, the port of INFRA-1098's R32 corpus to the
// tables this tree reads, whose header says what it holds and why. Nothing is drawn at random, so
// every run builds the same database. The seed is some 680,000 events, heavy work for a pod, so
// these tests carry `//go:build !race` and run in build Jobs and in envoy-go-race-impossible's
// step "Test what the race build cannot run at all or at full size", on the Postgres the job's
// step before it starts.

//go:embed testdata/priorities-read-corpus.sql
var prioritiesReadCorpusSQL string

// The corpus's four projects. The three of readCorpusProjects hold the finished work each and,
// in the second and third, the open work that advances no priority; readCorpusAdvancesProject
// holds the open work that advances a priority and is the largest.
var readCorpusProjects = []string{"PRDA", "PRDB", "PRDC"}

const readCorpusAdvancesProject = "PRDW"

// The counts R1 names, which the corpus prints in its first line and is held to before anything
// is read over it (readCorpusCensus.hold).
const (
	readCorpusIssues         = 13298
	readCorpusLivePriorities = 13
	readCorpusAdvancing      = 7651
	readCorpusStating        = 1600
	// readCorpusDepth is how far below the issue that states its relation the deepest member
	// stands: four levels, at least one chain of which R1 names.
	readCorpusDepth  = 4
	readCorpusParked = 729
	// The spread of events per issue: production's median and mean, and its most, which no issue
	// of the corpus passes.
	readCorpusEventsMedian = 16
	readCorpusEventsMean   = 51
	readCorpusEventsMost   = 937
)

// What the full read and the summary with no project answer over the corpus, by its
// construction: the open pairs of an issue and a priority by status, the claimed ones, the open
// asks on them and the pairs closed in the last seven days, the 115 open issues in flight under
// no priority, and the second fault. Of the 729 parked issues, the second fault holds the 393 that
// state a priority no issue above them carries (priorityBacklogTerms): the 386 roots that stand in
// backlog (269) or icebox (117), which have no parent, and the 7 issues four levels down that
// state a second priority beside the one they inherit, which no issue above them states. Each
// of them states one priority of its own, so the fault's counts by priority add to 393 too.
var readCorpusOpenPairs = map[string]int{
	"in_progress": 972, "todo": 826, "backlog": 525, "triage": 217, "icebox": 211, "needs_review": 119, "testing": 104, "retro": 21,
}

const (
	readCorpusClaimed        = 622
	readCorpusOpenAsks       = 78
	readCorpusClosedRecently = 1461
	readCorpusUnplaced       = 115
	readCorpusBacklog        = 393
	readCorpusBacklogPairs   = 393
)

// readCorpus is the corpus as the four tests address it: its database, whose every connection
// runs with JIT off (withJITOff), and the counts its first line reports.
type readCorpus struct {
	database *store.Store
	census   readCorpusCensus
}

// readCorpusTemplate is the key of the template the corpus is built in (storetest.OpenFrom).
const readCorpusTemplate = "the priorities read's corpus of production's counts (buildPrioritiesReadCorpus)"

// readCorpusBuilt is the template's census, read once when buildPrioritiesReadCorpus builds it.
// Every clone holds the template's rows, so it is every clone's census until a test writes its
// own; storetest.OpenFrom builds the template inside a sync.Once, which every caller passes
// before it reads this.
var readCorpusBuilt readCorpusCensus

// seedPrioritiesReadCorpus is a database of its own holding the corpus, a clone of a template
// built once per test binary (buildPrioritiesReadCorpus), whose every connection runs with JIT
// off (withJITOff), and its counts. The caller prints them as its first line and holds them
// (readCorpusCensus.hold). A clone copies the template's files, so it holds the statistics and
// the visibility map the template's vacuum (analyze) wrote, and its planner reads the corpus as
// the template's would; what a test writes stays in its clone.
func seedPrioritiesReadCorpus(t *testing.T) readCorpus {
	t.Helper()
	database := storetest.OpenFrom(t, readCorpusTemplate, func(databaseURL string) error {
		return buildPrioritiesReadCorpus(t, databaseURL)
	})
	return readCorpus{database: withJITOff(t, database), census: readCorpusBuilt}
}

// buildPrioritiesReadCorpus migrates the template databaseURL names with this tree's runner,
// writes the corpus, fills each issue's newest-event pair by running 20261125144002's file as the
// runner applies it (fillNewestEventPairs), so the corpus stands as a deployment does once this
// release has migrated it, settles it (settleSpeedCorpus) and reads its counts into
// readCorpusBuilt. It closes its pool before it returns, as storetest.OpenFrom requires of a
// template.
func buildPrioritiesReadCorpus(t *testing.T, databaseURL string) error {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open the corpus's template: %w", err)
	}
	defer database.Pool.Close()
	if err := database.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate the corpus's template: %w", err)
	}
	at := time.Now().UTC().Add(-62 * 24 * time.Hour).Truncate(time.Second)
	projects := append(slices.Clone(readCorpusProjects), readCorpusAdvancesProject)
	projectRows := make([][]any, 0, len(projects))
	issueRows := make([][]any, 0, len(projects))
	eventRows := make([][]any, 0, len(projects))
	for _, project := range projects {
		first := seededIssueKey(project, 1)
		projectRows = append(projectRows, seededProject(project, 1, at))
		issueRows = append(issueRows, seededIssueRow{
			key: first, project: project, number: 1, title: "The first issue of " + project,
			status: model.IssueStatusTriage, rank: seededRank(1), at: at, lastSeq: 1,
		}.values())
		eventRows = append(eventRows, seededEvent(first, 1, "issue.created", seededIssuePayload(t, model.IssueStatusTriage), at))
	}
	copySeededRows(t, database, "projects", seededProjectColumns, projectRows)
	copySeededRows(t, database, "issues", seededIssueColumns, issueRows)
	copySeededRows(t, database, "events", seededEventColumns, eventRows)

	// The file reads its projects from two settings and keeps its working tables as temporary
	// ones, both of which belong to one session, so it runs on one connection after them.
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("take the connection the corpus is written on: %w", err)
	}
	began := time.Now()
	if _, err := connection.Exec(ctx,
		`select set_config('priorities_read_corpus.projects', $1, false), set_config('priorities_read_corpus.advances_project', $2, false)`,
		strings.Join(readCorpusProjects, ","), readCorpusAdvancesProject); err != nil {
		connection.Release()
		return fmt.Errorf("name the corpus's projects: %w", err)
	}
	if _, err := connection.Exec(ctx, prioritiesReadCorpusSQL); err != nil {
		connection.Release()
		return fmt.Errorf("write the corpus (testdata/priorities-read-corpus.sql): %w", err)
	}
	connection.Release()
	fillNewestEventPairs(t, database)
	settleSpeedCorpus(t, database)
	readCorpusBuilt = readCorpusCensusOf(t, database)
	readCorpusBuilt.took = time.Since(began)
	return nil
}

// withJITOff is a store on database's database whose every connection runs with JIT off, as
// production's Cloud SQL runs: under the default jit = on a reading times compilation production
// never pays (benchtime.Open). Every pool the readings open is opened from this one's
// configuration (storetest.Reopened, storetest.Traced), so every connection they read on runs
// with it too, which requireReadCorpusJITOff reads back before anything is read.
func withJITOff(t *testing.T, database *store.Store) *store.Store {
	t.Helper()
	return storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.ConnConfig.RuntimeParams["jit"] = "off"
	})
}

// requireReadCorpusJITOff prints `jit = <value>` as show jit reads it on a connection of
// database, and stops the test unless it is off.
func requireReadCorpusJITOff(t *testing.T, database *store.Store, what string) {
	t.Helper()
	var jit string
	if err := database.Pool.QueryRow(context.Background(), "show jit").Scan(&jit); err != nil {
		t.Fatalf("show jit on the connections of %s: %v", what, err)
	}
	t.Logf("jit = %s on the connections of %s (show jit)", jit, what)
	if jit != "off" {
		t.Fatalf("jit = %s on the connections of %s, want off as production's Cloud SQL runs: a reading under JIT times compilation production never pays", jit, what)
	}
}

// readCorpusCensus is what the corpus holds, read from its tables rather than from how it was
// built: the counts R1 names, the spread of events per issue, and the issues whose events do not
// rise in created_at with seq or whose last_seq is not their newest seq.
type readCorpusCensus struct {
	issues, livePriorities, advancing, stating, deepest, parked int
	events, eventsMedian, eventsMost                            int
	eventsMean                                                  float64
	unordered, lastSeqOff                                       int
	took                                                        time.Duration
}

func (c readCorpusCensus) String() string {
	return fmt.Sprintf("corpus: %d issues, %d live priorities, %d issues that advance a priority, %d of them by a relation of their own, "+
		"the deepest %d levels below the issue that states its relation, %d parked issues that advance a priority; "+
		"%d events, per issue median %d, mean %.2f, at most %d; %d issues whose events do not rise in created_at with seq, "+
		"%d whose last_seq is not their newest seq; written in %s",
		c.issues, c.livePriorities, c.advancing, c.stating, c.deepest, c.parked,
		c.events, c.eventsMedian, c.eventsMean, c.eventsMost, c.unordered, c.lastSeqOff, c.took.Round(time.Second))
}

// hold stops the test where a count is not the one R1 names: a reading over a corpus that is not
// production's counts is a reading of another corpus.
func (c readCorpusCensus) hold(t *testing.T) {
	t.Helper()
	faults := []string{}
	for _, count := range []struct {
		what      string
		got, want int
	}{
		{"issues", c.issues, readCorpusIssues},
		{"live priorities", c.livePriorities, readCorpusLivePriorities},
		{"issues that advance a priority", c.advancing, readCorpusAdvancing},
		{"issues that advance one by a relation of their own", c.stating, readCorpusStating},
		{"parked issues that advance a priority", c.parked, readCorpusParked},
		{"events per issue at the median", c.eventsMedian, readCorpusEventsMedian},
		{"events per issue on average, rounded", int(math.Round(c.eventsMean)), readCorpusEventsMean},
		{"issues whose events do not rise in created_at with seq", c.unordered, 0},
		{"issues whose last_seq is not their newest seq", c.lastSeqOff, 0},
	} {
		if count.got != count.want {
			faults = append(faults, fmt.Sprintf("%d %s, want %d", count.got, count.what, count.want))
		}
	}
	if c.deepest < readCorpusDepth {
		faults = append(faults, fmt.Sprintf("its deepest member %d levels below the issue that states its relation, want at least %d", c.deepest, readCorpusDepth))
	}
	if c.eventsMost > readCorpusEventsMost {
		faults = append(faults, fmt.Sprintf("an issue of %d events, want at most %d", c.eventsMost, readCorpusEventsMost))
	}
	if len(faults) > 0 {
		t.Fatalf("the corpus holds %s: it is not production's counts", strings.Join(faults, "; "))
	}
}

// readCorpusCensusQuery reads the census. The members are read by a walk of its own: the newest
// tuple of each pair of an issue and a live priority, kept when it asserts the relation, and every
// issue below one of those down the parent links.
const readCorpusCensusQuery = `with recursive stated as (
	select newest.from_id as key
	from (
		select distinct on (t.from_id, t.to_id) t.from_id, t.to_id, t.asserted
		from priority_tuples t
		where t.from_kind = 'issue' and t.kind = 'advances' and t.to_kind = 'priority'
		order by t.from_id, t.to_id, t.seq desc
	) newest
	join priority_nodes live on live.kind = 'priority' and live.id = newest.to_id and live.state = 'live'
	where newest.asserted
), spread as (
	select key, 0 as depth from stated
	union all
	select c.key, s.depth + 1 from spread s join issues c on c.parent_key = s.key where s.depth < 32
), per_issue as (
	select issue_key, count(*) as events, max(seq) as newest from events where issue_key is not null group by issue_key
)
select (select count(*) from issues),
       (select count(*) from priority_nodes where kind = 'priority' and state = 'live'),
       (select count(distinct key) from spread),
       (select count(distinct key) from stated),
       (select coalesce(max(depth), 0) from spread),
       (select count(distinct s.key) from spread s join issues i on i.key = s.key
        where i.closed_at is null and i.status in ('backlog', 'icebox')),
       (select coalesce(sum(events), 0)::bigint from per_issue),
       (select percentile_disc(0.5) within group (order by events) from per_issue),
       (select avg(events)::float8 from per_issue),
       (select max(events) from per_issue),
       (select count(distinct issue_key) from (
          select issue_key, created_at <= lag(created_at) over (partition by issue_key order by seq) as early
          from events where issue_key is not null) ordered where ordered.early),
       (select count(*) from issues i left join per_issue p on p.issue_key = i.key
        where i.last_seq <> coalesce(p.newest, 0) or coalesce(p.events, 0) <> coalesce(p.newest, 0))`

func readCorpusCensusOf(t *testing.T, database *store.Store) readCorpusCensus {
	t.Helper()
	var c readCorpusCensus
	if err := database.Pool.QueryRow(context.Background(), readCorpusCensusQuery).Scan(
		&c.issues, &c.livePriorities, &c.advancing, &c.stating, &c.deepest, &c.parked,
		&c.events, &c.eventsMedian, &c.eventsMean, &c.eventsMost, &c.unordered, &c.lastSeqOff); err != nil {
		t.Fatalf("read the corpus's counts: %v", err)
	}
	return c
}

// readCorpusLargestProjects is the n projects of database holding the most issues, the largest
// first.
func readCorpusLargestProjects(t *testing.T, database *store.Store, n int) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select project_key from issues group by project_key order by count(*) desc, project_key limit $1`, n)
	if err != nil {
		t.Fatalf("read the largest projects: %v", err)
	}
	defer rows.Close()
	projects := []string{}
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			t.Fatalf("read a project: %v", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil || len(projects) != n {
		t.Fatalf("read %d of the %d largest projects: %v", len(projects), n, err)
	}
	return projects
}

// readCorpusAnswerFaults is every way body, the full read's answer or the summary's with no
// project named, differs from what the corpus was built to answer: a timing or a comparison of a
// wrong answer shows nothing.
func readCorpusAnswerFaults(body model.Priorities, summary bool) []string {
	faults := []string{}
	if body.WorkUnderNoPriority.Count != readCorpusUnplaced {
		faults = append(faults, fmt.Sprintf("%d issues in flight under no priority, want %d", body.WorkUnderNoPriority.Count, readCorpusUnplaced))
	}
	if body.PriorityWorkParked.Count != readCorpusBacklog {
		faults = append(faults, fmt.Sprintf("%d issues in backlog or icebox that state a priority no issue above them carries, want %d",
			body.PriorityWorkParked.Count, readCorpusBacklog))
	}
	if summary {
		if len(body.Priorities) != 0 || len(body.WorkUnderNoPriority.Issues) != 0 || len(body.PriorityWorkParked.Issues) != 0 {
			faults = append(faults, fmt.Sprintf("%d priorities and %d and %d fault rows, want none of them in a summary",
				len(body.Priorities), len(body.WorkUnderNoPriority.Issues), len(body.PriorityWorkParked.Issues)))
		}
		return faults
	}
	if len(body.Priorities) != readCorpusLivePriorities {
		faults = append(faults, fmt.Sprintf("%d priorities, want %d", len(body.Priorities), readCorpusLivePriorities))
	}
	open := map[string]int{}
	claimed, asks, closed, undated := 0, 0, 0, 0
	for _, row := range body.Priorities {
		for status, count := range row.Work.ByStatus {
			open[status] += count
		}
		claimed += row.Work.Claimed
		asks += row.Work.OpenAsks
		closed += row.Work.ClosedLast7Days
		if row.Work.NewestEventAt == nil {
			undated++
		}
	}
	for _, status := range model.IssueStatuses {
		if open[status] != readCorpusOpenPairs[status] {
			faults = append(faults, fmt.Sprintf("%d open pairs in %s, want %d", open[status], status, readCorpusOpenPairs[status]))
		}
	}
	for _, count := range []struct {
		what      string
		got, want int
	}{
		{"claimed pairs", claimed, readCorpusClaimed},
		{"open asks", asks, readCorpusOpenAsks},
		{"pairs closed in the last seven days", closed, readCorpusClosedRecently},
		{"priorities with no newest event", undated, 0},
		{"issues in flight under no priority by project", sumCounts(body.WorkUnderNoPriority.ByProject), readCorpusUnplaced},
		{"pairs in backlog or icebox by priority", sumCounts(body.PriorityWorkParked.ByPriority), readCorpusBacklogPairs},
		{"rows of work in flight under no priority", len(body.WorkUnderNoPriority.Issues), priorityFaultRows},
		{"rows of work in backlog or icebox", len(body.PriorityWorkParked.Issues), priorityFaultRows},
	} {
		if count.got != count.want {
			faults = append(faults, fmt.Sprintf("%d %s, want %d", count.got, count.what, count.want))
		}
	}
	return faults
}

// holdReadCorpusAnswer fails the test where body, form's answer with no project named, is not
// what the corpus was built to answer (readCorpusAnswerFaults); who names the read that answered.
func holdReadCorpusAnswer(t *testing.T, body model.Priorities, form prioritiesReadForm, who string) {
	t.Helper()
	if faults := readCorpusAnswerFaults(body, form.summary); len(faults) > 0 {
		t.Errorf("%s answers %s with no project named: %s", who, form.called, strings.Join(faults, "; "))
	}
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}
