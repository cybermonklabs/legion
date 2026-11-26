//go:build basebuild

package api

// This tree's check c-moved-read-cost: no read that R32 moves into the graph store gets slower,
// on this head after the move or on the release before after a rollback.
//
// The test is TestNoMovedReadGetsSlower at the end of this file. It carries the basebuild tag and
// stands in this package beside c-read-cost's tree_read_cost_test.go, whose servers, client,
// database and sampling it shares, because it builds the server of an earlier commit
// (testbaseline.Build), which needs a checkout that holds that commit. It writes its corpus by SQL
// from testdata/moved-read-cost-corpus.sql. It is run on its own, by name, as
//
//	STORES_BASE_SHA=$(git merge-base origin/main HEAD) MOVED_COST_LOOKUP_MERGED=no MOVED_COST_ISSUE_LIST_PLAN_MERGED=no go test -tags basebuild ./internal/dispatch/api/ -run '^TestNoMovedReadGetsSlower$' -timeout 270m -count=1 -v
//
// from packages/envoy, with DISPATCH_TEST_DATABASE_URL naming a Postgres server it may create a
// database on, over TCP and without TLS (the plan proxy relays that connection in the clear), which
// preloads pg_stat_statements (shared_preload_libraries) and whose role may create that extension.
// MOVED_COST_LOOKUP_MERGED and MOVED_COST_ISSUE_LIST_PLAN_MERGED are the check's two inputs, each
// yes or no as the role's line before the Jobs reads them on the base (movedCostSwitches).
// MOVED_READ_COST_SCALE scales the corpus (movedCostScale), MOVED_READ_COST_SEED_ONLY=1, for
// diagnosis, keeps the database the release before seeded and ends the run before any reading
// (movedCostSeedOnly), and MOVED_READ_COST_READS, for diagnosis, names the reads to measure
// (movedCostReadFilter); the check's run leaves all three unset.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove/storesmovetest"
	"github.com/sjawhar/envoy/internal/testbaseline"
	"github.com/sjawhar/envoy/internal/testwait"
)

// movedCostBaseVariable names the release before R32's move: the commit `git merge-base
// origin/main HEAD` answers, which c-stores-moved boots as that release. It is a variable of its
// own and not testbaseline.Variable, because the check's run names it so.
const movedCostBaseVariable = "STORES_BASE_SHA"

// movedCostScaleVariable scales the corpus: unset or 1 is the check's corpus, 0.1 a tenth. Every
// count of issues and of tuples of other kinds scales, and so do production's counts of issues the
// corpus is held to; the 13 priorities do not, and an issue keeps its runs and its children.
const movedCostScaleVariable = "MOVED_READ_COST_SCALE"

// movedCostSeedOnlyVariable, set to 1 for diagnosis, ends the run once the release before has
// seeded the corpus and owes nothing, and keeps that database (movedCostSeedOnly);
// movedCostKeptDatabaseFile is the file in $TMPDIR the kept database's name is written to.
const (
	movedCostSeedOnlyVariable = "MOVED_READ_COST_SEED_ONLY"
	movedCostKeptDatabaseFile = "moved-read-cost-kept-database"
)

// movedCostReadsVariable names, for diagnosis, the reads a run measures, comma-separated by their
// names in movedCostReads, in place of all eight (movedCostReadFilter).
const movedCostReadsVariable = "MOVED_READ_COST_READS"

// movedCostMigrations are the three migrations a release before R32's move holds: the graph store,
// and the two stores R32 moves into it. A base that lacks one is no such release.
var movedCostMigrations = []string{
	"20261109013400_graph_store.up.sql",
	"20261112024320_priority_nodes.up.sql",
	"20261116235202_workflow_nodes.up.sql",
}

// movedCostMigrationsDir is where a commit holds its migrations, from the repository's root.
const movedCostMigrationsDir = "packages/envoy/internal/dispatch/store/migrations"

// movedCostP90Bound is the one bound the run holds on its own: the p90 of each reading on the new
// side and on the third side is at most 200 ms, the owner's bound
// (dispatch://INFRA-1098/ask/88f91e7d-b257-4476-becc-6b5c91ccffef), but where the switch of a
// priority reading reads no (movedCostSwitches). The bounds of p50 and of database time against
// the old side are the check's step's, over the medians of three runs, and the run prints what
// that step reads (logMovedCostReadings).
const movedCostP90Bound = 200 * time.Millisecond

// The two inputs that switch the p90 bound of the three priority reads, each yes or no: whether the
// base holds the merge into main of the pull request that brings those reads under 200 ms, as the
// role's line before the Jobs reads it. MOVED_COST_LOOKUP_MERGED is dispatch://INFRA-5791's, on
// legion/INFRA-5791, and switches the priorities read and its summary read;
// MOVED_COST_ISSUE_LIST_PLAN_MERGED is dispatch://INFRA-5609's cybermonklabs/legion#5361, on
// fix/infra-5609-issue-list-plan, and switches both forms of the issue list with advances
// (dispatch://INFRA-1098/ask/0fa27283-9014-409b-bb22-300cb7747afb).
const (
	movedCostLookupMerged        = "MOVED_COST_LOOKUP_MERGED"
	movedCostIssueListPlanMerged = "MOVED_COST_ISSUE_LIST_PLAN_MERGED"
)

// The projects of the corpus. The steps read and the plans read address the first of the three;
// the open issues that advance a priority stand in a project of their own.
var movedCostProjects = []string{"MOVA", "MOVB", "MOVC"}

const (
	movedCostAdvancesProject = "MOVP"
	// movedCostPriority is the priority the history read and the advances filter name.
	movedCostPriority = "priority-01"
	// movedCostOtherWho is the who of every tuple of other kinds the corpus writes.
	movedCostOtherWho = `{"kind": "session", "id": "moved-read-cost"}`
)

// movedCostCorpusPath is the corpus's SQL, which the test runs whole over its own connection.
var movedCostCorpusPath = filepath.Join("testdata", "moved-read-cost-corpus.sql")

// movedCostEventsShape is production's count of events per issue, read through its API at
// 2026-11-23T23:20Z from the newest event's seq of 120 issues that advance a priority: the mean,
// the p50, the p90, the p99 and the most. The corpus's SQL draws each issue's count from it, its
// top at 900. The count check holds the corpus's mean and percentiles within movedCostShapeSlack
// of production's, and its most at or below production's and above movedCostEventsTail of it: a
// tenth of the issues draws fewer of the top ones, and its most is further below 937.
var movedCostEventsShape = struct{ mean, p50, p90, p99, most float64 }{51, 16, 118, 425, 937}

const movedCostEventsTail = 0.8

// movedCostProduction is production's shape, read through its API at 2026-11-23T23:20Z, which the
// corpus's issues are seeded to. Each figure is at scale 1 and counts issues, or open pairs of an
// issue and a priority it advances, and scales with the corpus; the priorities and the deepest
// chain do not. The corpus's SQL lays its issues out from the same figures (its moved_counts). The
// finished issues Legion never ran of the first project are among these issues; those of the two
// other projects stand beside them and are counted apart (movedCostBeside).
var movedCostProduction = struct {
	issues, members, own, parented float64
	// The open pairs by status, and how many there are in all.
	inProgress, todo, backlog, triage, icebox, needsReview, testing, retro, openPairs float64
	claimed, asks, closedRecently                                                     float64
	// The issues by status, over every issue: done 35%, backlog 32%, todo 17%, in flight 10%, and
	// triage and icebox the other 6%.
	done, backlogAll, todoAll, inFlight, shelved float64
	unplaced, parked                             float64
	priorities, deepest                          int
}{
	issues: 13298, members: 7677, own: 1600, parented: 0.68 * 13298,
	inProgress: 972, todo: 826, backlog: 525, triage: 217, icebox: 211, needsReview: 119, testing: 104, retro: 21, openPairs: 2995,
	claimed: 622, asks: 78, closedRecently: 1461,
	done: 0.35 * 13298, backlogAll: 0.32 * 13298, todoAll: 0.17 * 13298, inFlight: 0.10 * 13298, shelved: 0.06 * 13298,
	unplaced: 115, parked: 729, priorities: 13, deepest: 4,
}

// movedCostShapeSlack is how far a count of production's shape the corpus does not fix may stray
// from production's figure.
const movedCostShapeSlack = 0.05

// movedCostNeverRan is the condition, on an issue i, that it is a finished issue of one of projects
// that Legion never ran: one that no run is part_of.
func movedCostNeverRan(projects []string) string {
	return `(i.project_key in (` + store.QuoteLiterals(projects) + `) and i.status = 'done' and not exists (
		select 1 from workflow_tuples w
		where w.from_kind = 'step' and w.kind = 'part_of' and w.to_kind = 'issue' and w.to_id = i.key))`
}

// movedCostBeside is the condition, on an issue i, that it is one of the finished issues Legion
// never ran that the corpus writes beside production's issues: those of the two other projects.
// The first project's stand among production's issues. The shape counts the issues beside them
// apart, and the events hold them at none: they load only the plans read, which reads no events.
var movedCostBeside = movedCostNeverRan(movedCostProjects[1:])

// movedCostShapeQuery reads the corpus's shape as production's figures count it, over every issue
// of production's shape the database holds, which leaves out the finished issues Legion never ran
// beside them (movedCostBeside), one row of a name and a count each. It also counts by project the
// finished issues Legion never ran of each of the three (movedCostNeverRan): the first project's
// among the issues of production's shape, the two others' beside them. The members are read as
// the priorities read reads them (priorityRelationsQuery, priorityMemberTerms): the newest
// advances tuple of each pair of an issue and a live priority, where it asserts the pair, and
// every issue below such an issue. A pair is an issue and a priority it advances, its depth the
// fewest levels it stands below an issue that states that priority, and an open pair an open
// issue's.
var movedCostShapeQuery = `with recursive never_ran as (
		select i.key, i.project_key from issues i where ` + movedCostNeverRan(movedCostProjects) + `
	), production as (
		select i.* from issues i where not ` + movedCostBeside + `
	), relations as (
		select newest.from_id as key, newest.to_id as priority
		from (select distinct on (t.from_id, t.to_id) t.from_id, t.to_id, t.asserted
		      from priority_tuples t
		      where t.from_kind = 'issue' and t.kind = 'advances' and t.to_kind = 'priority'
		      order by t.from_id, t.to_id, t.seq desc) newest
		join priority_nodes live on live.kind = 'priority' and live.id = newest.to_id and live.state = 'live'
		join production stated on stated.key = newest.from_id
		where newest.asserted
	), spread as (
		select key, priority, 0 as depth from relations
		union all
		select c.key, s.priority, s.depth + 1 from spread s join production c on c.parent_key = s.key
		where s.depth < ` + parentDepthCapSQL + `
	), pairs as (
		select m.key, m.depth, i.status, i.closed_at, i.claimed_by,
		       (select count(*) from asks a where a.issue_key = i.key and a.state = 'open') as asks
		from (select key, priority, min(depth) as depth from spread group by key, priority) m
		join production i on i.key = m.key
	)
	select 'issues', count(*) from production
	union all select 'issues with a parent', count(*) from production where parent_key is not null
	union all select 'live priorities', count(*) from priority_nodes where state = 'live'
	union all select 'issues that advance a priority', count(distinct key) from pairs
	union all select 'issues that state advances', count(distinct key) from relations
	union all select 'levels the deepest member stands below a stated issue', coalesce(max(depth), 0) from pairs
	union all select 'open pairs in ' || status, count(*) from pairs where closed_at is null group by status
	union all select 'open pairs', count(*) from pairs where closed_at is null
	union all select 'claimed open pairs', count(*) from pairs where closed_at is null and claimed_by is not null
	union all select 'open asks on open pairs', coalesce(sum(asks), 0) from pairs where closed_at is null
	union all select 'pairs closed in the last ` + priorityClosedWindow + `', count(*) from pairs
		where closed_at >= now() - interval '` + priorityClosedWindow + `'
	union all select 'issues done', count(*) from production where status = 'done'
	union all select 'issues in backlog', count(*) from production where status = 'backlog'
	union all select 'issues in todo', count(*) from production where status = 'todo'
	union all select 'issues in flight', count(*) from production where status in (` + store.QuoteLiterals(priorityInFlightStatuses) + `)
	union all select 'issues in triage or icebox', count(*) from production where status in ('triage', 'icebox')
	union all select 'unplaced issues', count(*) from production i
		where i.closed_at is null and i.status in (` + store.QuoteLiterals(priorityInFlightStatuses) + `)
		  and not exists (select 1 from pairs p where p.key = i.key)
	union all select 'parked issues', count(distinct key) from pairs
		where closed_at is null and status in (` + store.QuoteLiterals(priorityBacklogStatuses) + `)
	union all select '` + movedCostNeverRanName + `' || project_key, count(*) from never_ran group by project_key`

// movedCostNeverRanName names, before a project's key, the shape's count of the finished issues
// Legion never ran in that project.
const movedCostNeverRanName = "finished issues Legion never ran in "

// movedCostEventsQuery reads the events of the issues the corpus writes in the projects $1, their
// first issues left out, in two rows: first those of production's shape, then the finished issues
// Legion never ran beside them (movedCostBeside), each row led by whether it is theirs. Of each:
// how many events there are and over how many issues, each issue's count of them in nearest-rank
// percentiles, the issues whose events run seq 1 to their last_seq (an issue beside production's
// holds none and a last_seq of 0), and, of the issues parked in backlog or icebox, how many there
// are, those that hold the issue.updated that moved them there as the parked read
// (parkedMoveLateral) finds it, and those whose move is not their newest event.
var movedCostEventsQuery = `with written as (
		select i.key, i.status, i.last_seq, ` + movedCostBeside + ` as beside
		from issues i
		where i.project_key = any($1) and i.number > 1
	), counted as (
		select w.key, w.status, w.last_seq, w.beside, count(e.id) as events, min(e.seq) as low, max(e.seq) as high,
		       max(e.seq) filter (where e.type = 'issue.updated' and e.payload->'changed' @> '"status"'::jsonb
		                          and e.payload->>'status' = w.status) as moved
		from written w
		left join events e on e.issue_key = w.key
		group by w.key, w.status, w.last_seq, w.beside
	)
	select beside, coalesce(sum(events), 0)::bigint, count(*), coalesce(avg(events), 0)::float8,
	       coalesce(percentile_disc(0.5) within group (order by events), 0),
	       coalesce(percentile_disc(0.9) within group (order by events), 0),
	       coalesce(percentile_disc(0.99) within group (order by events), 0),
	       coalesce(max(events), 0),
	       count(*) filter (where events = last_seq and (low = 1 and high = last_seq or beside and events = 0)),
	       count(*) filter (where status in (` + store.QuoteLiterals(priorityBacklogStatuses) + `)),
	       count(*) filter (where status in (` + store.QuoteLiterals(priorityBacklogStatuses) + `) and moved is not null),
	       count(*) filter (where status in (` + store.QuoteLiterals(priorityBacklogStatuses) + `) and moved < last_seq)
	from counted
	group by beside
	order by beside`

// movedCostEvents is one row of movedCostEventsQuery: the events of one group of the issues the
// corpus writes.
type movedCostEvents struct {
	events, issues, p50, p90, p99, most, whole, shelved, moved, movedBefore int64
	mean                                                                    float64
}

// ───── the corpus ─────────────────────────────────────────────────────────

// movedCostCorpus is the corpus's counts at the run's scale, as the corpus's SQL computes them, and
// the issues the reads address.
type movedCostCorpus struct {
	scale float64
	// The workflow side, the check's: in each of the three projects, the finished issues, the
	// issues among them run 30 times, and the others, the finished issues Legion never ran
	// (unran), which in the first project are among production's issues and in the two others
	// stand beside them; and the tuples of other kinds.
	finished, ran, unran int
	others               int64
	// priorities is how many priorities the corpus writes, every one live; issues how many issues
	// of production's shape it writes, the projects' first issues left out; own how many of them
	// state advances, and advances how many advances tuples it writes: one for each, and an assert
	// and a retraction before each eighth.
	priorities, issues, own, advances int
	// busy is the issue of 200 runs and 339 other children; advancing is a parked issue four levels
	// below the issue that states the priority it inherits, which states a second priority of its
	// own.
	busy, advancing string
}

func newMovedCostCorpus(t *testing.T, scale float64) *movedCostCorpus {
	t.Helper()
	scaled := func(full float64) int { return int(math.Round(full * scale)) }
	shape := movedCostProduction
	corpus := &movedCostCorpus{
		scale: scale, finished: scaled(3461), ran: scaled(600), others: int64(math.Round(1_000_000 * scale)),
		priorities: shape.priorities, issues: scaled(shape.issues) - len(movedCostProjects) - 1, own: scaled(shape.own),
		busy: movedCostProjects[0] + "-2", advancing: movedCostAdvancesProject + "-2",
	}
	corpus.unran = corpus.finished - corpus.ran - 1
	corpus.advances = corpus.own + 2*(corpus.own/8)
	// The issues that state a second priority are the parked pairs past the parked issues; the
	// first is the advancing read's issue. The issues run 30 times are numbered up to 5 x ran among
	// each project's finished issues.
	doubled := scaled(shape.backlog) + scaled(shape.icebox) - scaled(shape.parked)
	if doubled < 1 || 5*corpus.ran > corpus.finished {
		t.Fatalf("%s=%v leaves too small a corpus: %d issues that state a second priority, %d issues run of %d finished",
			movedCostScaleVariable, scale, doubled, corpus.ran, corpus.finished)
	}
	return corpus
}

// movedCostScale reads movedCostScaleVariable: unset is 1, the check's corpus.
func movedCostScale(t *testing.T) float64 {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(movedCostScaleVariable))
	if raw == "" {
		return 1
	}
	scale, err := strconv.ParseFloat(raw, 64)
	if err != nil || !(scale > 0 && scale <= 1) {
		t.Fatalf("%s=%q is no scale above 0 and at most 1", movedCostScaleVariable, raw)
	}
	if scale != 1 {
		t.Logf("c-moved-read-cost: %s=%s: the corpus is that share of the check's, so this run is no reading of the check",
			movedCostScaleVariable, raw)
	}
	return scale
}

// movedCostSeedOnly reads movedCostSeedOnlyVariable before anything is built or read. Unset, it
// answers false, and the run is the check's. Set to 1, it answers true: the run builds only the
// release before, writes the corpus, boots that release over it until it owes nothing, keeps the
// database (keepMovedCostSeed) and ends, passing, with no reading. Set to anything else, it fails,
// naming the value it takes.
func movedCostSeedOnly(t *testing.T) bool {
	t.Helper()
	switch named := strings.TrimSpace(os.Getenv(movedCostSeedOnlyVariable)); named {
	case "":
		return false
	case "1":
		return true
	default:
		t.Fatalf("%s=%q: it takes 1, which keeps the seeded database and ends the run before any reading, or is left unset",
			movedCostSeedOnlyVariable, named)
		return false
	}
}

// keepMovedCostSeed keeps the run's database at databaseURL, which the release before seeded and
// no longer serves, past the test's end. readCostDatabase drops the run's database by its name
// when the test ends, so the database is renamed to that name with "_seed" after it, and that drop
// finds nothing; JIT stays off, a setting of the database and not of its name. A rename needs no
// other session on the database, so conn, the test's own, is closed first, and the rename is tried
// again while Postgres still tears down a backend of the stopped server (55006). The kept name is
// printed and written to movedCostKeptDatabaseFile in $TMPDIR.
func keepMovedCostSeed(t *testing.T, conn *pgx.Conn, databaseURL string) {
	t.Helper()
	ctx := context.Background()
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close the test's connection to the seeded database: %v", err)
	}
	run, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("read the run's database URL: %v", err)
	}
	name := strings.TrimPrefix(run.Path, "/")
	kept := name + "_seed"
	adminURL := *run
	adminURL.Path = "/postgres"
	admin, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer admin.Close(ctx)
	testwait.Holds(30*time.Second, 200*time.Millisecond, func() bool {
		_, err = admin.Exec(ctx, "alter database "+name+" rename to "+kept)
		var postgres *pgconn.PgError
		return !errors.As(err, &postgres) || postgres.Code != "55006"
	})
	if err != nil {
		t.Fatalf("rename the seeded database %s to %s, which keeps it past the test's end: %v", name, kept, err)
	}
	t.Logf("c-moved-read-cost: %s keeps the seeded database %s (jit off), so this run is not a reading of the check",
		movedCostSeedOnlyVariable, kept)
	file := filepath.Join(os.TempDir(), movedCostKeptDatabaseFile)
	if err := os.WriteFile(file, []byte(kept+"\n"), 0o644); err != nil {
		t.Fatalf("write the kept database's name %s to %s: %v", kept, file, err)
	}
}

// seedMovedCostTemplates makes, through the release before's own API, the four projects and the
// first issue of each, whose row and node the corpus's SQL copies for every issue it writes.
func seedMovedCostTemplates(t *testing.T, s *readCostServer) {
	t.Helper()
	for _, person := range []string{readCostReader, readCostWriter} {
		s.expect(t, "sign in "+person, http.MethodGet, "/api/v1/whoami", person, nil, http.StatusOK)
	}
	for _, project := range append(slices.Clone(movedCostProjects), movedCostAdvancesProject) {
		s.expect(t, "create the project "+project, http.MethodPost, "/api/v1/projects", readCostWriter,
			map[string]string{"key": project, "name": "The reads R32 moves, project " + project}, http.StatusCreated)
		if key := createReadCostIssue(t, s, project, "The first issue of "+project+", whose row the corpus copies", "", readCostWriter); key != project+"-1" {
			t.Fatalf("the first issue of %s is %s, where the corpus copies %s-1", project, key, project)
		}
	}
}

// seedMovedCostCorpus runs the corpus's SQL over conn with the run's settings, and holds what it
// wrote before anything is measured: the workflow side and the corpus's own writes to the counts
// they fix, the issues of production's shape to it (movedCostProduction), each count printed
// beside production's figure at the run's scale, the finished issues Legion never ran beside them
// apart, and the events to production's events per issue, those beside production's issues held at
// none (holdMovedCostEvents).
func seedMovedCostCorpus(t *testing.T, conn *pgx.Conn, corpus *movedCostCorpus) {
	t.Helper()
	ctx := context.Background()
	text, err := os.ReadFile(movedCostCorpusPath)
	if err != nil {
		t.Fatalf("read the corpus's SQL: %v", err)
	}
	if _, err := conn.Exec(ctx, `select set_config('moved_read_cost.scale', $1, false),
		set_config('moved_read_cost.projects', $2, false), set_config('moved_read_cost.advances_project', $3, false)`,
		strconv.FormatFloat(corpus.scale, 'f', -1, 64), strings.Join(movedCostProjects, ","), movedCostAdvancesProject); err != nil {
		t.Fatalf("set the corpus's settings: %v", err)
	}
	started := time.Now()
	if _, err := conn.Exec(ctx, string(text)); err != nil {
		t.Fatalf("write the corpus by SQL (%s): %v", movedCostCorpusPath, err)
	}
	t.Logf("c-moved-read-cost: wrote the corpus by SQL at scale %v in %s", corpus.scale, time.Since(started).Round(time.Millisecond))

	// corpusCount is one count of the corpus as SQL reads it, held to want within slack.
	type corpusCount struct {
		what  string
		sql   string
		args  []any
		want  int64
		slack int64
	}
	projects := len(movedCostProjects)
	// The issues run 30 times, named: the first issue of each project holds the part_of tuples of
	// the three documents the release before's create states for it (its spec, its issue tree and
	// its decisions query), which the corpus does not write.
	ranIssues := make([]string, 0, projects*corpus.ran)
	for _, project := range movedCostProjects {
		for k := 1; k <= corpus.ran; k++ {
			ranIssues = append(ranIssues, project+"-"+strconv.Itoa(5*k+1))
		}
	}
	counts := []corpusCount{
		{"priorities", `select count(*) from priority_nodes`, nil, int64(corpus.priorities), 0},
		{"advances tuples", `select count(*) from priority_tuples where kind = 'advances'`, nil, int64(corpus.advances), 0},
		{"runs", `select count(*) from workflow_nodes where kind = 'step'`, nil, int64(projects * (30*corpus.ran + 200)), 0},
		{"plans", `select count(*) from workflow_nodes where kind = 'plan'`, nil, int64(projects * (corpus.ran + 1)), 0},
		{"runs part_of " + corpus.busy, `select count(*) from workflow_tuples where from_kind = 'step' and kind = 'part_of' and to_kind = 'issue' and to_id = $1`,
			[]any{corpus.busy}, 200, 0},
		{"other part_of children of " + corpus.busy, `select count(*) from graph_tuples where kind = 'part_of' and to_kind = 'issue' and to_id = $1`,
			[]any{corpus.busy}, 339, 0},
		{"other part_of children of the issues run 30 times", `select count(*) from graph_tuples where kind = 'part_of' and to_kind = 'issue' and to_id = any ($1)`,
			[]any{ranIssues}, int64(projects * 100 * corpus.ran), 0},
		// Each of the 59 terms' shares is rounded on its own, so the sum may stray by half a tuple
		// for each term.
		{"tuples of other kinds", `select count(*) from graph_tuples where who = $1::jsonb`, []any{movedCostOtherWho}, corpus.others, 30},
	}
	for _, project := range movedCostProjects {
		counts = append(counts, corpusCount{"finished issues of " + project,
			`select count(*) from issues where project_key = $1 and status = 'done' and closed_at is not null`,
			[]any{project}, int64(corpus.finished), 0})
	}
	for _, count := range counts {
		var read int64
		if err := conn.QueryRow(ctx, count.sql, count.args...).Scan(&read); err != nil {
			t.Fatalf("count the corpus's %s: %v", count.what, err)
		}
		t.Logf("c-moved-read-cost: the corpus holds %d %s (the corpus at scale %v writes %d)", read, count.what, corpus.scale, count.want)
		if read < count.want-count.slack || read > count.want+count.slack {
			t.Errorf("the corpus holds %d %s, where the corpus at scale %v writes %d", read, count.what, corpus.scale, count.want)
		}
	}
	holdMovedCostShape(t, conn, corpus)
	holdMovedCostEvents(t, conn, corpus)
	var issues, workflow, graph int64
	if err := conn.QueryRow(ctx, `select (select count(*) from issues), (select count(*) from workflow_tuples), (select count(*) from graph_tuples)`).
		Scan(&issues, &workflow, &graph); err != nil {
		t.Fatalf("count the corpus's tables: %v", err)
	}
	t.Logf("c-moved-read-cost: the release before's database holds %d issues (of production's shape and the finished issues Legion "+
		"never ran beside them), %d workflow_tuples and %d graph_tuples", issues, workflow, graph)
	if t.Failed() {
		t.FailNow()
	}
}

// holdMovedCostShape holds the corpus to production's shape (movedCostShapeQuery), each count
// printed beside production's figure at the run's scale: exactly where the corpus fixes the count
// to that figure, and within movedCostShapeSlack where it cannot. The corpus's 4,663 finished
// issues of production's shape, the 1,803 Legion ran and the first project's 2,860 it never ran,
// are 35.1% of its issues where production's done are about 35%, and the issues done, in flight
// and in triage or icebox, the issues that advance a priority and the open pairs in all follow
// from them. The finished issues Legion never ran are counted in each project, exactly the
// corpus's count: the first project's among the issues of production's shape, the two others'
// beside them and apart. A line prints both counts.
func holdMovedCostShape(t *testing.T, conn *pgx.Conn, corpus *movedCostCorpus) {
	t.Helper()
	rows, err := conn.Query(context.Background(), movedCostShapeQuery)
	if err != nil {
		t.Fatalf("read the corpus's shape: %v", err)
	}
	read := map[string]int64{}
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			t.Fatalf("read a count of the corpus's shape: %v", err)
		}
		read[name] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the corpus's shape: %v", err)
	}
	scale := corpus.scale
	scaled := func(full float64) int64 { return int64(math.Round(full * scale)) }
	shape := movedCostProduction
	counts := []struct {
		what       string
		production int64
		exact      bool
	}{
		{"issues", scaled(shape.issues), true},
		{"issues with a parent", scaled(shape.parented), true},
		{"live priorities", int64(shape.priorities), true},
		{"issues that advance a priority", scaled(shape.members), false},
		{"issues that state advances", scaled(shape.own), true},
		{"levels the deepest member stands below a stated issue", int64(shape.deepest), true},
		{"open pairs in in_progress", scaled(shape.inProgress), true},
		{"open pairs in todo", scaled(shape.todo), true},
		{"open pairs in backlog", scaled(shape.backlog), true},
		{"open pairs in triage", scaled(shape.triage), true},
		{"open pairs in icebox", scaled(shape.icebox), true},
		{"open pairs in needs_review", scaled(shape.needsReview), true},
		{"open pairs in testing", scaled(shape.testing), true},
		{"open pairs in retro", scaled(shape.retro), true},
		{"open pairs", scaled(shape.openPairs), false},
		{"claimed open pairs", scaled(shape.claimed), true},
		{"open asks on open pairs", scaled(shape.asks), true},
		{"pairs closed in the last " + priorityClosedWindow, scaled(shape.closedRecently), true},
		{"issues done", scaled(shape.done), false},
		{"issues in backlog", scaled(shape.backlogAll), true},
		{"issues in todo", scaled(shape.todoAll), true},
		{"issues in flight", scaled(shape.inFlight), false},
		{"issues in triage or icebox", scaled(shape.shelved), false},
		{"unplaced issues", scaled(shape.unplaced), true},
		{"parked issues", scaled(shape.parked), true},
	}
	for _, count := range counts {
		held, slack := "exactly", 0.0
		if !count.exact {
			held, slack = fmt.Sprintf("within %.0f%%", 100*movedCostShapeSlack), movedCostShapeSlack*float64(count.production)
		}
		holds := read[count.what]
		t.Logf("c-moved-read-cost: the corpus holds %d %s (production at scale %v: %d, held %s)", holds, count.what, scale, count.production, held)
		if math.Abs(float64(holds-count.production)) > slack {
			t.Errorf("the corpus holds %d %s, where production at scale %v holds %d, held %s", holds, count.what, scale, count.production, held)
		}
	}
	var beside int64
	apart := make([]string, 0, len(movedCostProjects)-1)
	for place, project := range movedCostProjects {
		holds := read[movedCostNeverRanName+project]
		where := "among the issues of production's shape"
		if place > 0 {
			beside += holds
			apart = append(apart, fmt.Sprintf("%d in %s", holds, project))
			where = "beside the issues of production's shape and apart from them"
		}
		t.Logf("c-moved-read-cost: the corpus holds %d %s%s, %s (the corpus at scale %v writes %d, held exactly)",
			holds, movedCostNeverRanName, project, where, scale, corpus.unran)
		if holds != int64(corpus.unran) {
			t.Errorf("the corpus holds %d %s%s, where the corpus at scale %v writes %d", holds, movedCostNeverRanName, project, scale, corpus.unran)
		}
	}
	t.Logf("c-moved-read-cost: the corpus holds %d issues of production's shape, %d of them finished issues of %s Legion never ran, "+
		"and, beside them and counted apart, %d finished issues Legion never ran (%s)", read["issues"],
		read[movedCostNeverRanName+movedCostProjects[0]], movedCostProjects[0], beside, strings.Join(apart, ", "))
}

// holdMovedCostEvents holds the events of the issues the corpus writes (movedCostEventsQuery) to
// production's events per issue (movedCostEventsShape) over the issues of production's shape, and
// holds the finished issues Legion never ran beside them at none: they load only the plans read,
// which reads no events. Every issue of production's shape holds its events seq 1 to its last_seq,
// and every one in backlog or icebox the issue.updated that moved it there, some but not all of
// them before their newest event; every issue beside them holds no event and a last_seq of 0.
func holdMovedCostEvents(t *testing.T, conn *pgx.Conn, corpus *movedCostCorpus) {
	t.Helper()
	rows, err := conn.Query(context.Background(), movedCostEventsQuery, append(slices.Clone(movedCostProjects), movedCostAdvancesProject))
	if err != nil {
		t.Fatalf("count the corpus's events: %v", err)
	}
	groups := map[bool]movedCostEvents{}
	for rows.Next() {
		var beside bool
		var read movedCostEvents
		if err := rows.Scan(&beside, &read.events, &read.issues, &read.mean, &read.p50, &read.p90, &read.p99, &read.most, &read.whole,
			&read.shelved, &read.moved, &read.movedBefore); err != nil {
			t.Fatalf("count the corpus's events: %v", err)
		}
		groups[beside] = read
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the corpus's events: %v", err)
	}
	shape := movedCostEventsShape
	near := func(read, production float64) bool {
		return math.Abs(read-production) <= movedCostShapeSlack*production
	}
	production := groups[false]
	t.Logf("c-moved-read-cost: the corpus holds %d events over %d issues of production's shape, by issue mean %.1f, p50 %d, "+
		"p90 %d, p99 %d and max %d (production: mean %.0f, p50 %.0f, p90 %.0f, p99 %.0f and max %.0f); %d of them hold events "+
		"seq 1 to their last_seq (the corpus at scale %v writes %d such issues)", production.events, production.issues,
		production.mean, production.p50, production.p90, production.p99, production.most, shape.mean, shape.p50, shape.p90,
		shape.p99, shape.most, production.whole, corpus.scale, corpus.issues)
	if production.issues != int64(corpus.issues) || production.whole != production.issues {
		t.Errorf("%d of the corpus's %d issues of production's shape hold events seq 1 to their last_seq, where the corpus at scale %v "+
			"writes %d such issues, each with its events", production.whole, production.issues, corpus.scale, corpus.issues)
	}
	if !near(production.mean, shape.mean) || !near(float64(production.p50), shape.p50) || !near(float64(production.p90), shape.p90) ||
		!near(float64(production.p99), shape.p99) || float64(production.most) > shape.most || float64(production.most) <= movedCostEventsTail*shape.most {
		t.Errorf("the events by issue of the corpus's issues of production's shape, mean %.1f, p50 %d, p90 %d, p99 %d and max %d, are "+
			"not production's: mean %.0f, p50 %.0f, p90 %.0f and p99 %.0f within %.0f%%, and max at most %.0f and above %.0f",
			production.mean, production.p50, production.p90, production.p99, production.most, shape.mean, shape.p50, shape.p90,
			shape.p99, 100*movedCostShapeSlack, shape.most, movedCostEventsTail*shape.most)
	}
	beside := groups[true]
	besideIssues := (len(movedCostProjects) - 1) * corpus.unran
	t.Logf("c-moved-read-cost: the corpus holds %d events over %d finished issues Legion never ran beside them, held at none, and %d of "+
		"them hold no event and a last_seq of 0 (the corpus at scale %v writes %d such issues); %d events in all", beside.events,
		beside.issues, beside.whole, corpus.scale, besideIssues, production.events+beside.events)
	if beside.issues != int64(besideIssues) || beside.events != 0 || beside.whole != beside.issues {
		t.Errorf("the corpus holds %d events over its %d finished issues Legion never ran beside production's issues, and %d of them hold "+
			"no event and a last_seq of 0, where the corpus at scale %v writes %d such issues, none with an event", beside.events,
			beside.issues, beside.whole, corpus.scale, besideIssues)
	}
	t.Logf("c-moved-read-cost: %d of the corpus's %d issues of production's shape in backlog or icebox hold the issue.updated that moved "+
		"them there, %d of them before their newest event", production.moved, production.shelved, production.movedBefore)
	if production.shelved == 0 || production.moved != production.shelved || production.movedBefore == 0 || production.movedBefore == production.moved {
		t.Errorf("%d of the corpus's %d issues in backlog or icebox hold the issue.updated that moved them there, %d of them before "+
			"their newest event, where every one holds it and some but not all before their newest", production.moved, production.shelved,
			production.movedBefore)
	}
}

// ───── what a server owes ─────────────────────────────────────────────────

// The owed wait: every count is read every readCostOwedPoll, as c-read-cost reads them, printed
// every movedCostOwedPrint while one stands above zero, and the run fails once one has stayed
// above zero for movedCostOwedWait. At scale 1 the release before's roll alone reads the corpus's
// events, about 680,000, all of the issues of production's shape, 1,000 a tick, a tick every 2 s,
// about 23 minutes.
const (
	movedCostOwedWait  = 60 * time.Minute
	movedCostOwedPrint = 30 * time.Second
)

// movedCostBaseOwed is what the release before records as owed after a boot, read as its own
// block-index tick reads it (docs' ReconcileBlockIndex at 93625c08). First its roll over the event
// log: how many event ids its build's row of block_index_event_cursors stands behind the newest
// event, or every event where it holds no row yet. Each tick reads the events after that row, at
// most 1,000, marks what they name and moves the row to the last one it read, and while a page is
// full the next tick follows 2 s later (readEventsAfterCursor), so the row reaches the newest event
// once the roll has read the corpus's events. Then the object pass that row owes
// (object_nodes_owed_at), which writes the nodes of the asks the corpus writes; the documents owed
// a block index as that build lists them, by a null version or by a mark its marking rule lists
// (owedDocuments, refs.OwedMarkListed), the rule its row records; and the comments, messages and
// asks owed a block index of their own blocks, and the items owed one with the block grammar or
// the whole grammar. $1 is its commit, the build_commit its processes write.
var movedCostBaseOwed = []readCostOwedCount{
	{"block_index_events_behind", `(select greatest(0, coalesce((select max(id) from events), 0) -
		coalesce((select event_id from block_index_event_cursors where build_commit = $1), 0)))`},
	{"object_nodes_owed", `(select count(*) from block_index_event_cursors where build_commit = $1 and object_nodes_owed_at is not null)`},
	{"document_block_versions_owed", `(select count(*) from document_block_versions b where b.version is null
		or b.store_owed_at is not null and (b.store_owed_rule <= (select marking_rule from block_index_event_cursors where build_commit = $1)
		                                    or b.store_version is null or b.store_version < b.version))`},
	{"comment_block_state_owed", `(select count(*) from comment_block_state where owed_at is not null)`},
	{"message_block_state_owed", `(select count(*) from message_block_state where owed_at is not null)`},
	{"ask_block_state_owed", `(select count(*) from ask_block_state where owed_at is not null)`},
	{"block_grammar_owed", `(select count(*) from block_grammar_owed)`},
	{"refs_grammar_owed", `(select count(*) from refs_grammar_owed)`},
}

// waitMovedCostOwed waits until s owes nothing: every count of counts reads 0, $1 of each being s's
// commit, and s has logged the end of its boot's run of the graph store's backfill
// (readCostBackfillDone). It reads them every readCostOwedPoll, never on a fixed sleep, prints the
// first reading and one every movedCostOwedPrint after it while anything is owed, with the
// database's clock_timestamp(), and then how long the wait took. Past movedCostOwedWait it fails,
// naming each thing still owed.
func waitMovedCostOwed(t *testing.T, conn *pgx.Conn, s *readCostServer, counts []readCostOwedCount) {
	t.Helper()
	columns := make([]string, len(counts))
	for index, count := range counts {
		columns[index] = count.sql
	}
	statement := `select clock_timestamp()::text, ` + strings.Join(columns, ", ")
	started := time.Now()
	var printed time.Time
	var at string
	var owed []string
	settled := testwait.Holds(movedCostOwedWait, readCostOwedPoll, func() bool {
		values := make([]int64, len(counts))
		targets := []any{&at}
		for index := range values {
			targets = append(targets, &values[index])
		}
		if err := conn.QueryRow(context.Background(), statement, s.commit).Scan(targets...); err != nil {
			t.Fatalf("c-moved-read-cost: %s: read what it owes: %v", s.name, err)
		}
		owed = nil
		for index, count := range counts {
			if values[index] != 0 {
				owed = append(owed, fmt.Sprintf("%s=%d", count.name, values[index]))
			}
		}
		if !strings.Contains(s.process.Output.String(), readCostBackfillDone) {
			owed = append(owed, "boot_backfill_done=false")
		}
		if len(owed) > 0 && time.Since(printed) >= movedCostOwedPrint {
			printed = time.Now()
			t.Logf("c-moved-read-cost: %s owes at %s, %s into the wait: %s", s.name, at, time.Since(started).Round(time.Second),
				strings.Join(owed, " "))
		}
		return len(owed) == 0
	})
	if !settled {
		t.Fatalf("c-moved-read-cost: %s still owes after %s: %s", s.name, movedCostOwedWait, strings.Join(owed, ", "))
	}
	t.Logf("c-moved-read-cost: %s owes nothing at %s; the wait took %s", s.name, at, time.Since(started).Round(time.Millisecond))
}

// ───── the eight reads ────────────────────────────────────────────────────

// movedCostRead is one reading of a read R32 moves: its name in the check's reading lines, the
// route it GETs, and how many of what its answer holds, which every side must answer alike before
// it is measured.
type movedCostRead struct {
	name, path, unit string
	count            func(body []byte) (int, error)
	// want is the count the corpus decides, or 0 where the corpus decides only that there is one.
	want int
	// gate is the input that switches the reading's p90 bound (movedCostSwitches), or "" where the
	// run holds it at every value of both.
	gate string
}

// movedCostCount decodes body into a value of T and answers the count measure takes of it.
func movedCostCount[T any](measure func(T) int) func(body []byte) (int, error) {
	return func(body []byte) (int, error) {
		var decoded T
		if err := json.Unmarshal(body, &decoded); err != nil {
			return 0, err
		}
		return measure(decoded), nil
	}
}

type movedCostPage struct {
	Total *int `json:"total"`
}

func movedCostTotal(page movedCostPage) int {
	if page.Total == nil {
		return -1
	}
	return *page.Total
}

// movedCostReads are the eight reads of the check, nine readings: the issue list with advances set
// is read in two forms, each a reading of its own. The issue list with no filter, and the one form
// of the advances list, are read as one page of contracts.DefaultIssuePageLimit: their advances
// terms read the moved relation of every issue the page answers. The other form of the advances
// list names no limit, as the pages send it (advancesIssuesParams in the web app's api/queries.ts),
// and answers every issue that advances the priority. The summary read is the one the sidebar sends
// on every page (prioritySummaryQuery), the two summary statements of the faults.
func movedCostReads(corpus *movedCostCorpus) []movedCostRead {
	type rows = []json.RawMessage
	type fault struct {
		Count int `json:"count"`
	}
	return []movedCostRead{
		{
			name: "priorities", path: "/api/v1/priorities", unit: "live priorities", want: corpus.priorities, gate: movedCostLookupMerged,
			count: movedCostCount(func(v struct {
				Priorities rows `json:"priorities"`
			}) int {
				return len(v.Priorities)
			}),
		},
		{
			name: "priorities_summary", path: "/api/v1/priorities?summary=true", unit: "issues the two faults count",
			gate: movedCostLookupMerged,
			count: movedCostCount(func(v struct {
				Unplaced fault `json:"work_under_no_priority"`
				Parked   fault `json:"priority_work_parked"`
			}) int {
				return v.Unplaced.Count + v.Parked.Count
			}),
		},
		{
			name: "priority_history", path: "/api/v1/priorities/" + movedCostPriority + "/history", unit: "acts",
			count: movedCostCount(func(v rows) int { return len(v) }),
		},
		{
			name: "issue", path: "/api/v1/issues/" + corpus.advancing, unit: "advances",
			count: movedCostCount(func(v struct {
				Advances rows `json:"advances"`
			}) int {
				return len(v.Advances)
			}),
		},
		{
			name: "issue_list", path: "/api/v1/issues?limit=50", unit: "issues in the listing's total",
			count: movedCostCount(movedCostTotal),
		},
		{
			name: "issue_list_by_advances", path: "/api/v1/issues?advances=" + movedCostPriority + "&limit=50", unit: "issues in the listing's total",
			gate:  movedCostIssueListPlanMerged,
			count: movedCostCount(movedCostTotal),
		},
		{
			name: "issue_list_by_advances_unpaged", path: "/api/v1/issues?advances=" + movedCostPriority, unit: "issues the listing answers",
			gate:  movedCostIssueListPlanMerged,
			count: movedCostCount(func(v rows) int { return len(v) }),
		},
		{
			name: "steps", path: "/api/v1/issues/" + corpus.busy + "/steps", unit: "runs", want: 200,
			count: movedCostCount(func(v struct {
				Tasks []struct {
					Steps rows `json:"steps"`
				} `json:"tasks"`
			}) int {
				runs := 0
				for _, task := range v.Tasks {
					runs += len(task.Steps)
				}
				return runs
			}),
		},
		{
			name: "plans", path: "/api/v1/projects/" + movedCostProjects[0] + "/plans", unit: "finished issues", want: corpus.finished,
			count: movedCostCount(func(v struct {
				Issues rows `json:"issues"`
			}) int {
				return len(v.Issues)
			}),
		},
	}
}

// movedCostReadFilter reads MOVED_READ_COST_READS, a comma-separated list of the names of reads,
// before anything is built or read. Unset, it answers reads whole, and every read is measured, as
// a run of the check measures them. Set, it answers the reads it names, in the order of reads, and
// says in one line that the run is not a reading of the check; where it names anything else it
// fails, naming the reads it accepts.
func movedCostReadFilter(t *testing.T, reads []movedCostRead) []movedCostRead {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(movedCostReadsVariable))
	if named == "" {
		return reads
	}
	accepted := make([]string, len(reads))
	for index, read := range reads {
		accepted[index] = read.name
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(named, ",") {
		name = strings.TrimSpace(name)
		if !slices.Contains(accepted, name) {
			t.Fatalf("%s names %q, which is no read of the check: it takes %s, comma-separated, or is left unset",
				movedCostReadsVariable, name, strings.Join(accepted, ", "))
		}
		wanted[name] = true
	}
	kept := make([]movedCostRead, 0, len(wanted))
	for _, read := range reads {
		if wanted[read.name] {
			kept = append(kept, read)
		}
	}
	t.Logf("c-moved-read-cost: %s=%s: the run measures only those reads, so it is no reading of the check", movedCostReadsVariable, named)
	return kept
}

// holdMovedCostReads reads each read once on s and holds its answer to the corpus, and, where old
// is given, to the count the old side answered, before the side is measured.
func holdMovedCostReads(t *testing.T, s *readCostServer, side string, reads []movedCostRead, old map[string]int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, read := range reads {
		answer := s.expect(t, read.name+" on the "+side, http.MethodGet, read.path, readCostReader, nil, http.StatusOK)
		count, err := read.count(answer.body)
		if err != nil {
			t.Errorf("decode %s (%s) on the %s: %v: %s", read.name, read.path, side, err, answer)
			continue
		}
		counts[read.name] = count
		t.Logf("c-moved-read-cost: %s (%s) on the %s answers %d %s", read.name, read.path, side, count, read.unit)
		switch {
		case read.want > 0 && count != read.want:
			t.Errorf("%s (%s) on the %s answers %d %s, where the corpus holds %d", read.name, read.path, side, count, read.unit, read.want)
		case count <= 0:
			t.Errorf("%s (%s) on the %s answers no %s", read.name, read.path, side, read.unit)
		case old != nil && count != old[read.name]:
			t.Errorf("%s (%s) on the %s answers %d %s, where the old side answered %d", read.name, read.path, side, count, read.unit, old[read.name])
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return counts
}

// measureMovedCostSide takes each read on s as c-read-cost takes a read (measureReadCost): one
// warm-up reading that is not counted, then readCostRuns readings over HTTP through the one client,
// with nearest-rank percentiles and the database time of those readings.
func measureMovedCostSide(t *testing.T, statements *readCostStatements, s *readCostServer, side string, reads []movedCostRead) map[string]readCostReading {
	t.Helper()
	readings := map[string]readCostReading{}
	for _, read := range reads {
		readings[read.name] = measureReadCost(t, "c-moved-read-cost", statements, s, read.name+" ("+read.path+")", "the "+side, readCostGets(read.path))
	}
	return readings
}

// ───── the plans of each side ─────────────────────────────────────────────

// movedCostPlanIdle is how long a server booted through the plan proxy stays up after the reads
// are driven, with nothing reading it, so the proxy sees the background work it sends every few
// seconds: the block index's tick every 2 s and its reconcile every 30 s, the process heartbeat
// every 5 s and the leased loops' ticks.
const movedCostPlanIdle = 40 * time.Second

// movedCostPlanModes are the two plans each statement is explained under, by the plan_cache_mode
// that forces each: the custom plan Postgres plans a prepared statement's first five executions
// with, over the values they bind, and the generic plan, over no values, it may switch it to after
// them.
var movedCostPlanModes = []struct{ plan, mode string }{
	{"custom", "force_custom_plan"},
	{"generic", "force_generic_plan"},
}

// movedCostExecutionTime is the line of EXPLAIN ANALYZE's text that states the execution time.
var movedCostExecutionTime = regexp.MustCompile(`(?m)^Execution Time: (\S+ ms)$`)

// movedCostPlanStatement is the name each statement is prepared under on the test's connection
// while it is explained.
const movedCostPlanStatement = "c_moved_read_cost_plan"

// movedCostProxy stands between a server and Postgres while the test drives each read once, and
// keeps each statement that server sends for the read, with the parameters it binds. A statement
// whose text the server also sends while no read is driven, from its boot until it stops, is its
// background work and not the read's (statements). It relays every byte unchanged; it reads the
// frontend's Parse, Bind and Query messages, and nothing the backend answers.
type movedCostProxy struct {
	listener net.Listener
	upstream string
	mu       sync.Mutex
	read     string
	kept     []movedCostStatement
	seen     map[string]bool
	// idle holds the text of each statement the server sent while no read was driven.
	idle map[string]bool
}

// movedCostStatement is one statement a server sent for a read, as it sent it.
type movedCostStatement struct {
	read    string
	sql     string
	oids    []uint32
	formats []int16
	values  [][]byte
}

// movedCostParsed is a statement a connection prepared: its text and the parameter types it named.
type movedCostParsed struct {
	sql  string
	oids []uint32
}

// startMovedCostProxy listens on loopback and relays each connection to the server databaseURL
// names, and answers the URL that reaches the same database through it, in the clear.
func startMovedCostProxy(t *testing.T, databaseURL string) (*movedCostProxy, string) {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Hostname() == "" {
		t.Fatalf("the plan proxy relays to the host the run's database URL names, and it names none (%v)", err)
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the plan proxy: %v", err)
	}
	proxy := &movedCostProxy{listener: listener, upstream: net.JoinHostPort(parsed.Hostname(), port), seen: map[string]bool{}, idle: map[string]bool{}}
	go proxy.accept()
	t.Cleanup(func() { _ = listener.Close() })
	through := *parsed
	through.Host = listener.Addr().String()
	query := through.Query()
	query.Set("sslmode", "disable")
	through.RawQuery = query.Encode()
	return proxy, through.String()
}

func (p *movedCostProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.serve(client)
	}
}

// serve relays one connection: the backend's bytes back as they come, and the frontend's message by
// message, each observed before it is passed on, so a statement is kept before its answer leaves.
func (p *movedCostProxy) serve(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		return
	}
	defer server.Close()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	_ = p.relay(bufio.NewReader(client), client, server)
}

// relay passes the startup phase on, refusing a request for TLS or GSS encryption so the client
// goes on in the clear, and then every typed message.
func (p *movedCostProxy) relay(from *bufio.Reader, client, server net.Conn) error {
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(from, header); err != nil {
			return err
		}
		length := int(binary.BigEndian.Uint32(header[:4]))
		if length < 8 {
			return fmt.Errorf("a startup message of %d bytes", length)
		}
		body := make([]byte, length-8)
		if _, err := io.ReadFull(from, body); err != nil {
			return err
		}
		if code := binary.BigEndian.Uint32(header[4:]); code == 80877103 || code == 80877104 {
			if _, err := client.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		}
		if _, err := server.Write(append(header, body...)); err != nil {
			return err
		}
		break
	}
	parsed := map[string]movedCostParsed{}
	for {
		header := make([]byte, 5)
		if _, err := io.ReadFull(from, header); err != nil {
			return err
		}
		length := int(binary.BigEndian.Uint32(header[1:]))
		if length < 4 {
			return fmt.Errorf("a message of %d bytes", length)
		}
		body := make([]byte, length-4)
		if _, err := io.ReadFull(from, body); err != nil {
			return err
		}
		p.observe(parsed, header[0], body)
		if _, err := server.Write(append(header, body...)); err != nil {
			return err
		}
	}
}

// observe reads one frontend message: a Parse names a statement's text and types, a Bind runs a
// statement with its parameters, and a Query runs a text with none.
func (p *movedCostProxy) observe(parsed map[string]movedCostParsed, kind byte, body []byte) {
	message := &movedCostMessage{body: body}
	switch kind {
	case 'P':
		name, sql := message.cstring(), message.cstring()
		oids := make([]uint32, message.count())
		for index := range oids {
			oids[index] = message.word()
		}
		if !message.bad {
			parsed[name] = movedCostParsed{sql: sql, oids: oids}
		}
	case 'B':
		message.cstring()
		statement, known := parsed[message.cstring()]
		formats := make([]int16, message.count())
		for index := range formats {
			formats[index] = int16(message.half())
		}
		values := make([][]byte, message.count())
		for index := range values {
			if length := int32(message.word()); length >= 0 {
				values[index] = message.take(int(length))
			}
		}
		if known && !message.bad {
			p.keep(statement, formats, values)
		}
	case 'Q':
		if sql := message.cstring(); !message.bad {
			p.keep(movedCostParsed{sql: sql}, nil, nil)
		}
	}
}

// keep keeps a statement the read now driven sent, once for each read, and the text of one sent
// while no read is driven.
func (p *movedCostProxy) keep(statement movedCostParsed, formats []int16, values [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read == "" {
		p.idle[statement.sql] = true
		return
	}
	key := p.read + "\x00" + statement.sql
	if p.seen[key] {
		return
	}
	p.seen[key] = true
	p.kept = append(p.kept, movedCostStatement{read: p.read, sql: statement.sql, oids: statement.oids, formats: formats, values: values})
}

// drive names read as the one the statements that follow belong to, until drive is called again
// with "".
func (p *movedCostProxy) drive(read string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read = read
}

// statements answers the statements kept for read, in the order the server first sent them, and
// apart from them those whose text it also sent while no read was driven.
func (p *movedCostProxy) statements(read string) (sent, background []movedCostStatement) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, statement := range p.kept {
		switch {
		case statement.read != read:
		case p.idle[statement.sql]:
			background = append(background, statement)
		default:
			sent = append(sent, statement)
		}
	}
	return sent, background
}

// movedCostMessage reads the fields of one protocol message in order; a field past its end marks
// the message bad.
type movedCostMessage struct {
	body []byte
	bad  bool
}

func (m *movedCostMessage) take(n int) []byte {
	if n < 0 || n > len(m.body) {
		m.bad = true
		return nil
	}
	field := m.body[:n:n]
	m.body = m.body[n:]
	return field
}

func (m *movedCostMessage) cstring() string {
	end := bytes.IndexByte(m.body, 0)
	if end < 0 {
		m.bad = true
		return ""
	}
	text := string(m.body[:end])
	m.body = m.body[end+1:]
	return text
}

func (m *movedCostMessage) half() uint16 {
	if field := m.take(2); field != nil {
		return binary.BigEndian.Uint16(field)
	}
	return 0
}

func (m *movedCostMessage) word() uint32 {
	if field := m.take(4); field != nil {
		return binary.BigEndian.Uint32(field)
	}
	return 0
}

// count is an Int16 count of the fields that follow; a negative one marks the message bad.
func (m *movedCostMessage) count() int {
	count := int(int16(m.half()))
	if count < 0 {
		m.bad = true
		return 0
	}
	return count
}

// planMovedCostSide prints the plans of the side s serves, before that side is measured: it waits
// until s, a server booted through proxy, owes nothing, as counts record it, vacuums, drives each
// read on s once, leaves s idle for movedCostPlanIdle, stops it, and prints the plans of each
// statement each read sent it (printMovedCostPlans).
func planMovedCostSide(t *testing.T, conn *pgx.Conn, proxy *movedCostProxy, s *readCostServer, counts []readCostOwedCount,
	side string, reads []movedCostRead) {
	t.Helper()
	waitMovedCostOwed(t, conn, s, counts)
	readCostVacuum(t, conn, side)
	for _, read := range reads {
		proxy.drive(read.name)
		_, err := readCostGets(read.path)(s)
		proxy.drive("")
		if err != nil {
			t.Fatalf("drive %s (%s) on the %s through the plan proxy: %v", read.name, read.path, side, err)
		}
	}
	time.Sleep(movedCostPlanIdle)
	s.stop(t)
	printMovedCostPlans(t, conn, proxy, side, reads)
}

// printMovedCostPlans prints, for each read, the two plans (movedCostPlanModes) of each statement
// the server sent for it on side, run over conn with the parameters that server bound
// (explainMovedCost): first a line of the read's name, the statement's number and each plan's
// execution time, then the read's route, the statement's parameters and text, and the two plans.
// The statements whose text the server also sent while no read was driven are named in a line of
// their own, and not explained. Nothing is bound on a plan.
func printMovedCostPlans(t *testing.T, conn *pgx.Conn, proxy *movedCostProxy, side string, reads []movedCostRead) {
	t.Helper()
	ctx := context.Background()
	for _, read := range reads {
		statements, background := proxy.statements(read.name)
		if len(background) > 0 {
			heads := make([]string, len(background))
			for index, statement := range background {
				head := []rune(strings.Join(strings.Fields(statement.sql), " "))
				heads[index] = strconv.Quote(string(head[:min(len(head), readCostStatementText)]))
			}
			t.Logf("c-moved-read-cost: plans of %s on the %s: %d statements it sent are the server's background work too, sent while no read "+
				"was driven, and are not explained: %s", read.name, side, len(background), strings.Join(heads, ", "))
		}
		if len(statements) == 0 {
			t.Errorf("c-moved-read-cost: %s (%s) sent the server on the %s no statement of its own, so it has no plan", read.name, read.path, side)
			continue
		}
		for index, statement := range statements {
			plans, err := explainMovedCost(ctx, conn, statement)
			if err != nil {
				t.Errorf("c-moved-read-cost: explain statement %d of %s (%s) on the %s: %v\n%s", index+1, read.name, read.path, side, err, statement.sql)
				continue
			}
			times := make([]string, len(plans))
			shown := make([]string, len(plans))
			for mode, plan := range plans {
				took := "no execution time"
				if match := movedCostExecutionTime.FindStringSubmatch(plan); match != nil {
					took = match[1]
				}
				times[mode] = movedCostPlanModes[mode].plan + " " + took
				shown[mode] = fmt.Sprintf("%s plan (plan_cache_mode = %s):\n%s", movedCostPlanModes[mode].plan, movedCostPlanModes[mode].mode, plan)
			}
			t.Logf("c-moved-read-cost: plans of %s on the %s: statement %d: %s\nstatement %d of %d of %s, bound %s:\n%s\n%s",
				read.name, side, index+1, strings.Join(times, ", "), index+1, len(statements), read.path, movedCostParameters(statement),
				statement.sql, strings.Join(shown, "\n"))
		}
	}
}

// explainMovedCost prepares statement over conn as pgx prepares one (pgx.Conn.Prepare): a Parse of
// the text and the parameter types the server sent, under a name of its own, and a Describe. It
// answers the statement's EXPLAIN (ANALYZE, BUFFERS) EXECUTE under each plan cache mode of
// movedCostPlanModes, in that order, each in a transaction of its own with JIT off, rolled back
// after; EXECUTE is given the values the server bound, as the literals Postgres quotes them to
// (movedCostLiterals). The statement is deallocated after.
func explainMovedCost(ctx context.Context, conn *pgx.Conn, statement movedCostStatement) (plans []string, err error) {
	pg := conn.PgConn()
	description, err := pg.Prepare(ctx, movedCostPlanStatement, statement.sql, statement.oids)
	if err != nil {
		return nil, fmt.Errorf("prepare: %w", err)
	}
	defer func() {
		if closed := pg.Deallocate(ctx, movedCostPlanStatement); closed != nil && err == nil {
			err = fmt.Errorf("deallocate: %w", closed)
		}
	}()
	literals, err := movedCostLiterals(ctx, pg, statement, description.ParamOIDs)
	if err != nil {
		return nil, err
	}
	execute := "explain (analyze, buffers) execute " + movedCostPlanStatement
	if len(literals) > 0 {
		execute += "(" + strings.Join(literals, ", ") + ")"
	}
	for _, mode := range movedCostPlanModes {
		plan, err := explainMovedCostUnder(ctx, conn, mode.mode, execute)
		if err != nil {
			return nil, fmt.Errorf("the %s plan: %w", mode.plan, err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// explainMovedCostUnder runs execute, an EXPLAIN, over conn in a transaction of its own with JIT
// off and plan_cache_mode set to mode, rolled back after, and answers the plan it prints.
func explainMovedCostUnder(ctx context.Context, conn *pgx.Conn, mode, execute string) (string, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "set local jit = off; set local plan_cache_mode = "+mode); err != nil {
		return "", err
	}
	result := tx.Conn().PgConn().ExecParams(ctx, execute, nil, nil, nil, nil).Read()
	if result.Err != nil {
		return "", result.Err
	}
	lines := make([]string, len(result.Rows))
	for index, row := range result.Rows {
		lines[index] = string(row[0])
	}
	return strings.Join(lines, "\n"), nil
}

// movedCostLiterals answers each value the server bound statement as a literal EXECUTE takes: the
// value read as the type oids names for its parameter, in the format it was bound in, and quoted
// by Postgres (quote_literal), or null.
func movedCostLiterals(ctx context.Context, pg *pgconn.PgConn, statement movedCostStatement, oids []uint32) ([]string, error) {
	if len(oids) != len(statement.values) {
		return nil, fmt.Errorf("the statement takes %d parameters, and the server bound %d", len(oids), len(statement.values))
	}
	literals := make([]string, len(statement.values))
	for index, value := range statement.values {
		if value == nil {
			literals[index] = "null"
			continue
		}
		result := pg.ExecParams(ctx, "select quote_literal($1)", [][]byte{value}, []uint32{oids[index]}, []int16{statement.format(index)}, nil).Read()
		if result.Err != nil {
			return nil, fmt.Errorf("quote parameter $%d: %w", index+1, result.Err)
		}
		literals[index] = string(result.Rows[0][0])
	}
	return literals, nil
}

// format is the format the server bound parameter index in: the one format of every parameter, the
// parameter's own, or text where it named none.
func (s movedCostStatement) format(index int) int16 {
	switch len(s.formats) {
	case 1:
		return s.formats[0]
	case len(s.values):
		return s.formats[index]
	}
	return 0
}

// movedCostParameters is each parameter a statement was bound, text quoted and binary in hex.
func movedCostParameters(statement movedCostStatement) string {
	if len(statement.values) == 0 {
		return "no parameter"
	}
	parts := make([]string, len(statement.values))
	for index, value := range statement.values {
		format := statement.format(index)
		switch {
		case value == nil:
			parts[index] = fmt.Sprintf("$%d=null", index+1)
		case format == 0:
			parts[index] = fmt.Sprintf("$%d=%s", index+1, strconv.Quote(string(value)))
		default:
			parts[index] = fmt.Sprintf(`$%d=\x%s`, index+1, hex.EncodeToString(value))
		}
	}
	return strings.Join(parts, " ")
}

// ───── the run ────────────────────────────────────────────────────────────

// No read that R32 moves gets slower.
//
// Eight reads are measured, each over HTTP through one client against a server process over one
// database: the priorities, their summary, a priority's history, an issue that advances a
// priority, the issue list, the issue list by one priority it advances, an issue's steps and a
// project's plans. The issue list by a priority is read in two forms, one page of 50 and the form
// with no limit, so each side has nine readings (movedCostReads). The run has three sides over one
// database. First, the release before, built from STORES_BASE_SHA, migrates it and makes the four
// projects; the run stops it, writes the corpus by SQL, boots it again and measures the old side
// over the four tables. Second, this head boots, which moves both stores into the graph store, and
// the run measures the new side. Third, as a rollback does, the release before boots again, now
// reading through the four views, and the run measures the third side. Each side answers each
// reading alike before it is measured.
//
// Before each side is measured, the run prints the plans of every statement each read sends on
// that side (planMovedCostSide). A side's readings run each statement past its fifth execution,
// where Postgres may switch a prepared statement from a custom plan to a generic one, so each
// statement is explained under both, with each plan's execution time, and nothing is bound on
// them. They are taken on a boot of the side's build of its own, through a proxy that keeps what
// each read sends, after the wait and the vacuum the side's readings follow, and that boot is
// stopped before the side's measured boot; this head's move runs on it.
//
// No reading is taken while a server's own background work runs over the corpus: after each boot,
// before its first reading or plan, the run waits until that server owes nothing, as it records
// what it owes (waitMovedCostOwed): the release before's block-index roll over the events the
// corpus wrote after its cursor, its object pass and its block index (movedCostBaseOwed), this
// head's backfills (readCostHeadOwed), and each boot's run of the graph store's backfill.
//
// For each reading on the new side and on the third side the run prints the line the check's step
// reads over three runs, `c-moved-read-cost: reading: read=<name> side=<new|third> old_p50=<ms>
// p50=<ms> p90=<ms> old_db=<ms> db=<ms>`, the old figures being this run's old side
// (logMovedCostReadings). The step holds the median of three runs' p50s and of their database time
// against the old side's; the run holds neither. It holds each reading's p90 on both sides to at
// most 200 ms (movedCostP90Bound), failing on each it breaks, naming the reading, the side and the
// bound (holdMovedCostP90). The p90 of the priorities read and of its summary read is held only
// where MOVED_COST_LOOKUP_MERGED is yes, and that of both forms of the issue list with advances
// only where MOVED_COST_ISSUE_LIST_PLAN_MERGED is yes; where one is no, the run prints those
// readings' p90 beside the bound and holds it to nothing. The run prints both inputs on one line at
// its start (movedCostSwitches). The database time is read from pg_stat_statements as c-read-cost
// reads it (readCostStatements), and printed with the statements behind it and the database time
// of an idle interval after it. A reading whose database time the run could not read on either
// side, where no snapshot of its batch was read or pg_stat_statements held no statement that ran
// once per reading of it, fails on that side, naming the reading. Each p95 and p99 is printed and
// bound at nothing.
//
// MOVED_READ_COST_SEED_ONLY=1, for diagnosis, builds only the release before, writes the corpus,
// boots that release over it until it owes nothing, stops it and keeps the database, writing its
// name to $TMPDIR/moved-read-cost-kept-database (keepMovedCostSeed); the run ends there, passing,
// with no reading, and is no reading of the check.
//
// MOVED_READ_COST_READS, for diagnosis, names the reads to measure, by their names in
// movedCostReads, and every plan and reading of the run is of those alone; the run is no reading
// of the check (movedCostReadFilter).
//
// It fails, never skips, before it builds anything: where MOVED_READ_COST_SEED_ONLY is set to
// anything but 1; where MOVED_READ_COST_READS names anything but the name of a read; unless
// MOVED_READ_COST_SEED_ONLY is 1, where MOVED_COST_LOOKUP_MERGED or
// MOVED_COST_ISSUE_LIST_PLAN_MERGED is unset or holds anything but yes or no, naming it; where
// STORES_BASE_SHA is unset, names no 40-character commit, or names a commit that lacks one of the
// three migrations; and, unless MOVED_READ_COST_SEED_ONLY is 1, where pg_stat_statements cannot be
// read, naming why and each reading whose database time it cannot read
// (requireMovedCostStatements).
func TestNoMovedReadGetsSlower(t *testing.T) {
	began := time.Now()
	t.Cleanup(func() {
		t.Logf("c-moved-read-cost: TestNoMovedReadGetsSlower took %s of wall time", time.Since(began).Round(time.Second))
	})
	seedOnly := movedCostSeedOnly(t)
	var merged map[string]bool
	if !seedOnly {
		merged = movedCostSwitches(t)
	}
	baseCommit := movedCostBaseCommit(t)
	corpus := newMovedCostCorpus(t, movedCostScale(t))
	var headCommit, headBinary string
	if !seedOnly {
		headCommit = gitOutput(t, ".", "rev-parse", "HEAD")
		if !readCostFullCommit.MatchString(headCommit) || headCommit == baseCommit {
			t.Fatalf("this head's commit %q must be a whole commit other than %s=%s, or the two builds cannot be told apart",
				headCommit, movedCostBaseVariable, baseCommit)
		}
	}
	reads := movedCostReadFilter(t, movedCostReads(corpus))
	databaseURL := readCostDatabase(t, false)
	var statements *readCostStatements
	if !seedOnly {
		statements = requireMovedCostStatements(t, databaseURL, reads)
	}
	baseBinary := testbaseline.Build(t, baseCommit, movedCostBaseVariable)
	if seedOnly {
		t.Logf("c-moved-read-cost: %s=1: the run builds only the release before (%s), the commit %s names, seeds the "+
			"corpus with it and ends before any reading", movedCostSeedOnlyVariable, baseCommit, movedCostBaseVariable)
	} else {
		headBinary = cmdtest.BuildCommand(t, cmdtest.Command{
			Name:   "envoy-dispatch-head",
			Dir:    envoyModuleRoot(t),
			Target: "./cmd/dispatch",
			Flags:  []string{"-ldflags", "-X main.buildCommit=" + headCommit},
		})
		t.Logf("c-moved-read-cost: the old side and the third side run on the build of %s, the commit %s names; the new side "+
			"on the build of this checkout's working copy, stamped with its HEAD %s", baseCommit, movedCostBaseVariable, headCommit)
	}
	client := readCostClient()
	listener := readCostListener(t)
	conn := readCostConnect(t, databaseURL)
	holdReadCostJITOff(t, "c-moved-read-cost", conn)
	baseName := func(what string) string { return "the release before (" + baseCommit + ")" + what }

	// ───── first: the corpus and the old side, on the release before; the plans first ─────
	first := startReadCostServer(t, client, baseName(", migrating"), baseBinary, baseCommit, databaseURL, listener)
	seedMovedCostTemplates(t, first)
	first.stop(t)
	seedMovedCostCorpus(t, conn, corpus)
	if !seedOnly {
		proxy, throughProxy := startMovedCostProxy(t, databaseURL)
		planned := startReadCostServer(t, client, baseName(" through the plan proxy, over the four tables"), baseBinary, baseCommit, throughProxy, listener)
		planMovedCostSide(t, conn, proxy, planned, movedCostBaseOwed, "old side", reads)
	}
	base := startReadCostServer(t, client, baseName(""), baseBinary, baseCommit, databaseURL, listener)
	waitMovedCostOwed(t, conn, base, movedCostBaseOwed)
	if seedOnly {
		base.stop(t)
		keepMovedCostSeed(t, conn, databaseURL)
		return
	}
	t.Logf("c-moved-read-cost: before the old side is measured: %s", storesmovetest.ReadKindsStatistics(t, conn))
	counts := holdMovedCostReads(t, base, "old side", reads, nil)
	old := measureMovedCostSide(t, statements, base, "old side", reads)
	base.stop(t)

	// ───── second: this head, which moves both stores at boot; the plans first ─────
	proxy, throughProxy := startMovedCostProxy(t, databaseURL)
	moved := startReadCostServer(t, client, "this head ("+headCommit+") through the plan proxy", headBinary, headCommit, throughProxy, listener)
	for _, line := range strings.Split(moved.process.Output.String(), "\n") {
		if strings.Contains(line, `msg="dispatch: the boot's migrations begin"`) ||
			strings.Contains(line, `msg="dispatch: the move of the priority store and the workflow store into the graph store ended"`) {
			t.Logf("c-moved-read-cost: this head's boot: %s", line)
		}
	}
	var relkind string
	if err := conn.QueryRow(context.Background(), `select relkind::text from pg_class where oid = 'priority_nodes'::regclass`).Scan(&relkind); err != nil {
		t.Fatalf("read what priority_nodes is after this head's boot: %v", err)
	}
	if relkind != "v" {
		t.Fatalf("after this head's boot priority_nodes is of relkind %q, where the move leaves a view", relkind)
	}
	planMovedCostSide(t, conn, proxy, moved, readCostHeadOwed, "new side", reads)
	head := startReadCostServer(t, client, "this head ("+headCommit+")", headBinary, headCommit, databaseURL, listener)
	waitMovedCostOwed(t, conn, head, readCostHeadOwed)
	t.Logf("c-moved-read-cost: before the new side is measured: %s", storesmovetest.ReadKindsStatistics(t, conn))
	holdMovedCostReads(t, head, "new side", reads, counts)
	now := measureMovedCostSide(t, statements, head, "new side", reads)
	head.stop(t)

	// ───── third: the release before again, through the views; the plans first ─────
	proxy, throughProxy = startMovedCostProxy(t, databaseURL)
	planned := startReadCostServer(t, client, baseName(" through the plan proxy, over the four views"), baseBinary, baseCommit, throughProxy, listener)
	planMovedCostSide(t, conn, proxy, planned, movedCostBaseOwed, "third side", reads)
	rollback := startReadCostServer(t, client, baseName(" after the rollback"), baseBinary, baseCommit, databaseURL, listener)
	waitMovedCostOwed(t, conn, rollback, movedCostBaseOwed)
	t.Logf("c-moved-read-cost: before the third side is measured: %s", storesmovetest.ReadKindsStatistics(t, conn))
	holdMovedCostReads(t, rollback, "third side", reads, counts)
	third := measureMovedCostSide(t, statements, rollback, "third side", reads)
	rollback.stop(t)

	t.Logf("c-moved-read-cost: old side and third side %s, new side %s; %d readings of each read on each side; corpus scale %v",
		baseCommit, headCommit, readCostRuns, corpus.scale)
	t.Log("c-moved-read-cost: read\troute\t" + strings.Join(readCostHeads("old", "new", "third"), "\t") +
		"\tnew/old db\tthird/old db\tnew/old p50\tthird/old p50\tbound in the run")
	for _, read := range reads {
		before, after, rolled := old[read.name], now[read.name], third[read.name]
		cells := append(readCostTableCells(before.cells(), after.cells(), rolled.cells()),
			readCostDatabaseRatio(before.database, after.database), readCostDatabaseRatio(before.database, rolled.database),
			fmt.Sprintf("%.2f", float64(after.p50)/float64(before.p50)), fmt.Sprintf("%.2f", float64(rolled.p50)/float64(before.p50)),
			movedCostP90Held(read, merged))
		t.Logf("c-moved-read-cost: %s\t%s\t%s", read.name, read.path, strings.Join(cells, "\t"))
	}
	sides := []struct {
		name     string
		readings map[string]readCostReading
	}{{"old side", old}, {"new side", now}, {"third side", third}}
	for _, read := range reads {
		for _, side := range sides {
			logReadCostStatements(t, "c-moved-read-cost", read.name+" ("+read.path+")", side.name, side.readings[read.name].database)
		}
	}
	for _, read := range reads {
		for _, side := range sides {
			t.Logf("c-moved-read-cost: %s, %s, every reading in order, in ms: %v", read.name, side.name, readCostMilliseconds(side.readings[read.name].runs))
		}
	}
	logMovedCostReadings(t, reads, old, now, third)

	// The bound, one subtest for each reading on each side, so a reading that breaks it names
	// itself and its side and the rest still report theirs.
	for _, read := range reads {
		before := old[read.name]
		for _, side := range sides[1:] {
			t.Run(read.name+" on the "+side.name, func(t *testing.T) {
				holdMovedCostP90(t, read, side.name, before, side.readings[read.name], merged)
			})
		}
	}
}

// logMovedCostReadings prints, for each reading on the new side and on the third side, the line
// the check's step reads from each run: the reading's name and its side, the old side's p50, its
// p50 and its p90, and the old side's database time per reading and its own, each in ms, a
// database time that could not be read as none (movedCostDatabaseUnread).
func logMovedCostReadings(t *testing.T, reads []movedCostRead, old, now, third map[string]readCostReading) {
	t.Helper()
	database := func(spent *readCostDatabaseTime) string {
		if movedCostDatabaseUnread(spent) != "" {
			return "none"
		}
		return fmt.Sprintf("%.3f", spent.ms)
	}
	for _, read := range reads {
		before := old[read.name]
		for _, side := range []struct {
			name     string
			readings map[string]readCostReading
		}{{"new", now}, {"third", third}} {
			reading := side.readings[read.name]
			t.Logf("c-moved-read-cost: reading: read=%s side=%s old_p50=%.2f p50=%.2f p90=%.2f old_db=%s db=%s", read.name, side.name,
				msOf(before.p50), msOf(reading.p50), msOf(reading.p90), database(before.database), database(reading.database))
		}
	}
}

// movedCostP90Holds says whether the run holds read's p90 to movedCostP90Bound: where read has no
// gate, and where its gate reads yes (movedCostSwitches).
func movedCostP90Holds(read movedCostRead, merged map[string]bool) bool {
	return read.gate == "" || merged[read.gate]
}

// movedCostP90Held is the table's cell of the bound the run holds read to.
func movedCostP90Held(read movedCostRead, merged map[string]bool) string {
	if !movedCostP90Holds(read, merged) {
		return "p90 held to nothing, " + read.gate + "=no"
	}
	return fmt.Sprintf("p90 at most %.0f ms", msOf(movedCostP90Bound))
}

// holdMovedCostP90 holds reading, of read on side, to movedCostP90Bound where the run holds read
// to it (movedCostP90Holds): it fails where the p90 is over the bound, naming the reading, the side
// and the bound, and says otherwise that it keeps it. Where read's gate reads no, it prints the p90
// beside the bound and holds it to nothing. A database time it could not read on either side
// fails, naming the reading and why, since the step's bound of database time cannot be read.
func holdMovedCostP90(t *testing.T, read movedCostRead, side string, before, reading readCostReading, merged map[string]bool) {
	t.Helper()
	what := read.name + " (" + read.path + ") on the " + side
	switch {
	case !movedCostP90Holds(read, merged):
		t.Logf("%s: p90 %.1f ms beside its bound of %.0f ms, held to nothing, since %s=no (the old side's p90 is %.1f ms)",
			what, msOf(reading.p90), msOf(movedCostP90Bound), read.gate, msOf(before.p90))
	case reading.p90 > movedCostP90Bound:
		t.Errorf("%s: p90 %.1f ms is over its bound of %.0f ms (the old side's p90 is %.1f ms)",
			what, msOf(reading.p90), msOf(movedCostP90Bound), msOf(before.p90))
	default:
		t.Logf("%s: p90 %.1f ms is within its bound of %.0f ms (the old side's p90 is %.1f ms)",
			what, msOf(reading.p90), msOf(movedCostP90Bound), msOf(before.p90))
	}
	for _, database := range []struct {
		side  string
		spent *readCostDatabaseTime
	}{{"the old side", before.database}, {"the " + side, reading.database}} {
		if why := movedCostDatabaseUnread(database.spent); why != "" {
			t.Errorf("%s: its database time per reading on %s could not be read, so the check's bound of database time against the "+
				"old side's cannot be read: %s", what, database.side, why)
		}
	}
}

// movedCostDatabaseUnread says why database, a side's database time of a reading, cannot bear the
// check's bound on database time, or "" where it can: no snapshot of its batch was read, or
// pg_stat_statements held no statement that ran once per reading of it.
func movedCostDatabaseUnread(database *readCostDatabaseTime) string {
	switch {
	case database == nil:
		return "no snapshot of pg_stat_statements was read around its batch"
	case database.statements == 0:
		return fmt.Sprintf("pg_stat_statements held no statement that ran at least once per reading of its batch of %d", database.readings)
	}
	return ""
}

// requireMovedCostStatements opens what database time is read with (openReadCostStatements)
// before anything is built, and fails the run at once where pg_stat_statements cannot be read,
// naming why and each reading whose database time, on the new side and on the third side, it then
// cannot read.
func requireMovedCostStatements(t *testing.T, databaseURL string, reads []movedCostRead) *readCostStatements {
	t.Helper()
	statements, err := openReadCostStatements(t, "c-moved-read-cost", databaseURL)
	if err != nil {
		named := make([]string, len(reads))
		for index, read := range reads {
			named[index] = read.name + " (" + read.path + ")"
		}
		t.Fatalf("c-moved-read-cost: database time cannot be read: %v; the run's Postgres must preload pg_stat_statements "+
			"(shared_preload_libraries) and its role must be able to create that extension, or no reading's database time, which "+
			"the check's step bounds against the old side's, can be read on the new side or the third side: %s", err, strings.Join(named, ", "))
	}
	return statements
}

// movedCostSwitches reads the two inputs that switch the priority readings' p90 bound
// (movedCostLookupMerged, movedCostIssueListPlanMerged) before anything is built or read, and
// prints them on the line the check's step reads from each run, `c-moved-read-cost: switches:
// MOVED_COST_LOOKUP_MERGED=<value> MOVED_COST_ISSUE_LIST_PLAN_MERGED=<value>`. It fails, never
// skips, naming each input that is unset or holds anything but yes or no. It answers whether each
// holds yes.
func movedCostSwitches(t *testing.T) map[string]bool {
	t.Helper()
	merged := map[string]bool{}
	named := make([]string, 0, 2)
	for _, input := range []struct{ name, branch string }{
		{movedCostLookupMerged, "legion/INFRA-5791"},
		{movedCostIssueListPlanMerged, "fix/infra-5609-issue-list-plan"},
	} {
		value, set := os.LookupEnv(input.name)
		switch {
		case !set:
			t.Errorf("c-moved-read-cost: %s is unset: it takes yes where the base holds the merge into main of %s's pull request, "+
				"and no where it does not, as the role's line before the Jobs reads it", input.name, input.branch)
		case value != "yes" && value != "no":
			t.Errorf("c-moved-read-cost: %s=%q holds neither yes nor no: it takes yes where the base holds the merge into main of %s's "+
				"pull request, and no where it does not, as the role's line before the Jobs reads it", input.name, value, input.branch)
		}
		merged[input.name] = value == "yes"
		named = append(named, input.name+"="+value)
	}
	if t.Failed() {
		t.FailNow()
	}
	t.Logf("c-moved-read-cost: switches: %s", strings.Join(named, " "))
	return merged
}

// movedCostBaseCommit reads STORES_BASE_SHA and fails, never skips, where it is unset, names no
// 40-character commit this checkout holds, or names a commit that lacks one of the three
// migrations, naming each it lacks, before anything is built or read.
func movedCostBaseCommit(t *testing.T) string {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(movedCostBaseVariable))
	if named == "" {
		t.Fatalf("%s is unset: c-moved-read-cost reads the old side and the third side on the server of the commit it names, "+
			"the release before R32's move; set it to the commit `git merge-base origin/main HEAD` answers", movedCostBaseVariable)
	}
	if !readCostFullCommit.MatchString(named) {
		t.Fatalf("%s=%q names no 40-character commit: c-moved-read-cost reads the commit the run names, whole", movedCostBaseVariable, named)
	}
	out, err := exec.Command("git", "rev-parse", "--verify", "--quiet", named+"^{commit}").Output()
	if commit := strings.TrimSpace(string(out)); err != nil || commit != named {
		t.Fatalf("%s=%s names no commit this checkout holds (git rev-parse --verify: %v); a shallow clone lacks it", movedCostBaseVariable, named, err)
	}
	for _, migration := range movedCostMigrations {
		if err := exec.Command("git", "cat-file", "-e", named+":"+movedCostMigrationsDir+"/"+migration).Run(); err != nil {
			t.Errorf("%s=%s lacks the migration %s, so it is no release before R32's move", movedCostBaseVariable, named, migration)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return named
}
