// priority_newest_event_test.go holds the newest event under each priority
// (priorityNewestEventQuery) to what it reads and to what it answers. It reads the event of
// last_seq of each member issue through the unique (issue_key, seq) of events, and no other event.
// Beside the walk over the members it reads every table through an index, and its walk, which is
// main's, reads issues once and no more shared buffers than main's walk. Its time is main's
// (mainPriorityNewestEventQuery) where no two writes of an issue overlap. Where a write of an
// issue began before another write of that issue committed, its time can be earlier, but by less
// than the time from that start to that commit.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainPriorityNewestEventQuery is the whole text that priorityNewestEventQuery sent at commit
// cdb2baa0c3f9f6392aa3fb1aeb8cbb9c812af5b1, before the statement read the event of last_seq of
// each member, with priorityMemberTerms (its seed priorityRelationSeed), parentDepthCapSQL and
// priorityProjectFilter written out as they were at that commit. It is the oracle of the tests
// below, held letter for letter. A text built from the package's own constants would move with
// them, and the two sides would then agree however the read changed. Its walk over the members
// is mainNewestEventWalk, the frozen text of that walk, which it is built from: so the oracle
// carries main's walk by construction, and the walk has one frozen text.
const mainPriorityNewestEventQuery = `with recursive members_seed as (
		select ($1::text[])[i] as key, ($2::text[])[i] as priority
		from generate_subscripts($1::text[], 1) as seed(i)
	), ` + mainNewestEventWalk + `, members as (
		select distinct key, priority from members_spread
	)
	select m.priority, max(e.created_at)
	from members m
	join issues i on i.key = m.key
	join events e on e.issue_key = i.key
	where (cardinality($3::text[]) = 0 or i.project_key = any($3::text[]))
	group by m.priority`

// mainNewestEventWalk is main's walk over the members of each priority, the recursive CTE that
// newestEventWalkCTE names, frozen letter for letter as it stood at that commit. It is the walk
// that mainPriorityNewestEventQuery is built from, so the buffers the plan test reads off the walk
// of main's plans (requireWalkWithinMains) are this walk's. Each step joins issues on parent_key,
// and the planner hashes issues for it from one read of the table. It decides the holds a text of
// the newest event is put to (requireNewestEventPlan): a text that carries it takes main's walk,
// and any other text takes a walk of its own, such as the walk keyed through issues_parent_key
// (priorityNewestEventKeyedWalkQuery) that a reading on production may call for.
const mainNewestEventWalk = `members_spread as (
		select s.key, s.priority, 0 as depth from members_seed s
		union all
		select c.key, m.priority, m.depth + 1
		from members_spread m join issues c on c.parent_key = m.key
		where m.depth < 32
	)`

// newestEventIndex is the unique (issue_key, seq) of events: the one index the newest event under
// a priority reads, once for each member issue.
const newestEventIndex = "events_issue_key_seq_key"

// The two projects of the corpus the newest event is read over (seedNewestEventCorpus). The read
// with one project names the first, and leaves out the issues of the second.
const (
	newestEventProject      = "NEWA"
	newestEventOtherProject = "NEWB"
)

// The priorities of the corpus that no time is read for: one stated on an issue that has no
// event, and one that no issue states.
const (
	newestEventSilentPriority   = "silent-work"
	newestEventUnstatedPriority = "nothing-stated"
)

// The issues of the corpus that advance no priority: newestEventBystanderRoots roots, each with
// newestEventBystanderChildren children, 1,000 issues in all. Each has events, and they are many
// times the member issues the read joins, so a plan that probes the events of every issue before
// it joins the members runs its probe many more times than a plan that probes once for each
// member issue (requireOneEventOfEachMember). Most of them have a parent. The planner sizes the
// walk over the members from the 1,000 rows it plans for every seed (priorityMemberTerms) and from
// how many issues have a parent, so it plans many more member rows than there are issues: 16,498
// against 1,029 (PostgreSQL 18.7, 2026-11-24). A plan that probes every issue first is then the
// cheaper one by its costs. When none of these issues had a parent, the planner probed once for
// each member row even where the probe named the issue alone, so the check passed such a probe.
const (
	newestEventBystanderRoots    = 40
	newestEventBystanderChildren = 24
)

// newestEventCorpus is what seedNewestEventCorpus wrote: the server that reads it, its store, the
// priorities it holds in rank order, and of those the four stated on the trees.
type newestEventCorpus struct {
	served     *server
	database   *store.Store
	priorities []string
	trees      []string
}

// seedNewestEventCorpus writes the corpus that the newest event is compared and planned over, in
// bulk (priority_fixture_test.go). For each depth from 1 to 4 there is a priority stated on the
// root of a tree of that depth, and each issue of the tree above its last level has two children.
// The issues go to the two projects in turn. Each issue holds 30 to 60 events whose seq and time
// rise together, one minute apart from a start of its own, as appends that each commit before
// the next begins write them. So the event of last_seq of each issue is its newest, and the
// newest event of a priority is on the member whose events go on longest. Beside the trees:
//
//   - the depth-3 tree holds one more child of its root, with no event, among members that have
//     events;
//   - one more child of the depth-2 tree's root also states the depth-4 priority, so one issue
//     advances two priorities;
//   - the depth-3 tree's root also states the depth-1 and the depth-2 priorities, so each issue
//     of that tree, in both projects, advances three. In each read the member issues are then
//     fewer than the member rows, the (issue, priority) pairs, so a plan that probes once for
//     each member row probes more times than the check permits (requireOneEventOfEachMember);
//   - newestEventSilentPriority is stated on one issue of its own, which has no event;
//   - newestEventUnstatedPriority is stated on no issue;
//   - the issues of newestEventBystanderRoots trees advance no priority. Each holds 1 to 5
//     events, which begin a day after the start of the members' events and so are later than all
//     of them.
//
// The random source has a fixed seed, so every run writes the same rows.
func seedNewestEventCorpus(t *testing.T) newestEventCorpus {
	t.Helper()
	_, database, deps := newTestServer(t, testServerOptions{})
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	random := rand.New(rand.NewPCG(1, 2))
	eventsOf := func() int { return 30 + random.IntN(31) }

	numbers := map[string]int{}
	var issues, eventRows, nodes, tuples [][]any
	// eventsFrom is the earliest start of the events of the next issue that addIssue writes.
	eventsFrom := base
	addIssue := func(parent *string, count int) string {
		project := newestEventProject
		if len(issues)%2 == 1 {
			project = newestEventOtherProject
		}
		numbers[project]++
		key := seededIssueKey(project, numbers[project])
		issues = append(issues, seededIssueRow{
			key: key, project: project, number: numbers[project], title: "Newest event work " + key,
			status: "todo", parent: parent, rank: seededRank(len(issues)), at: base, lastSeq: count,
		}.values())
		start := eventsFrom.Add(time.Duration(random.IntN(600)) * time.Minute)
		for seq := 1; seq <= count; seq++ {
			eventRows = append(eventRows, seededEvent(key, seq, "comment.created", `{}`, start.Add(time.Duration(seq)*time.Minute)))
		}
		return key
	}

	corpus := newestEventCorpus{served: directServer(deps), database: database}
	roots := map[int]string{}
	for depth := 1; depth <= 4; depth++ {
		priority := fmt.Sprintf("tree-depth-%d", depth)
		corpus.trees = append(corpus.trees, priority)
		root := addIssue(nil, eventsOf())
		roots[depth] = root
		tuples = append(tuples, seededAdvancesTuple(root, priority, base))
		level := []string{root}
		for range depth - 1 {
			var next []string
			for _, parent := range level {
				for range 2 {
					next = append(next, addIssue(&parent, eventsOf()))
				}
			}
			level = next
		}
	}
	addIssue(new(roots[3]), 0)
	shared := addIssue(new(roots[2]), eventsOf())
	tuples = append(tuples, seededAdvancesTuple(shared, corpus.trees[3], base))
	tuples = append(tuples, seededAdvancesTuple(roots[3], corpus.trees[0], base), seededAdvancesTuple(roots[3], corpus.trees[1], base))
	tuples = append(tuples, seededAdvancesTuple(addIssue(nil, 0), newestEventSilentPriority, base))
	eventsFrom = base.Add(24 * time.Hour)
	for range newestEventBystanderRoots {
		root := addIssue(nil, 1+random.IntN(5))
		for range newestEventBystanderChildren {
			addIssue(&root, 1+random.IntN(5))
		}
	}

	corpus.priorities = append(slices.Clone(corpus.trees), newestEventSilentPriority, newestEventUnstatedPriority)
	for index, id := range corpus.priorities {
		nodes = append(nodes, seededPriorityNode(t, id, "Priority "+id, "Work the newest event is read under.",
			"alice", fmt.Sprintf("s%02d", index), base))
	}
	copySeededRows(t, database, "projects", seededProjectColumns, [][]any{
		seededProject(newestEventProject, numbers[newestEventProject], base),
		seededProject(newestEventOtherProject, numbers[newestEventOtherProject], base),
	})
	copySeededRows(t, database, "issues", seededIssueColumns, issues)
	copySeededRows(t, database, "events", seededEventColumns, eventRows)
	copySeededRows(t, database, "priority_nodes", seededNodeColumns, nodes)
	copySeededRows(t, database, "priority_tuples", seededTupleColumns, tuples)
	return corpus
}

// priorityCorpusSeed reads the relations of a corpus's priorities, served by served, as the read
// does (readPriorityRelations): the pair of arrays that every statement of the read takes as its
// seed.
func priorityCorpusSeed(t *testing.T, served *server, priorities []string) (keys, ids []string) {
	t.Helper()
	keys, ids, err := served.readPriorityRelations(context.Background(), priorities)
	if err != nil {
		t.Fatalf("read the relations of the corpus's priorities: %v", err)
	}
	return keys, ids
}

// newestEventScopes are the two reads each test makes: with no project named, as the page reads
// it, and with one project named.
var newestEventScopes = []struct {
	name     string
	projects []string
}{
	{"every project", []string{}},
	{"project " + newestEventProject, []string{newestEventProject}},
}

// newestEventTimes runs statement, one of the two texts of the newest event under each priority,
// with the seed and projects of a read, and answers the time of each priority by its id. Neither
// text orders its rows, and the read keeps them by id (scanPriorityNewestEvents), so the two are
// compared as sets of rows.
func newestEventTimes(t *testing.T, database *store.Store, statement string, seedKeys, seedIDs, projects []string) map[string]time.Time {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, seedKeys, seedIDs, projects)
	if err != nil {
		t.Fatalf("run the newest event statement: %v", err)
	}
	defer rows.Close()
	times := map[string]time.Time{}
	for rows.Next() {
		var id string
		var newest *time.Time
		if err := rows.Scan(&id, &newest); err != nil {
			t.Fatalf("read a row of the newest event statement: %v", err)
		}
		if newest == nil {
			t.Fatalf("the newest event statement answers %s with no time", id)
		}
		if _, twice := times[id]; twice {
			t.Fatalf("the newest event statement answers %s twice", id)
		}
		times[id] = *newest
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows of the newest event statement: %v", err)
	}
	return times
}

// TestPriorityNewestEventAnswersMainsTime holds the newest event under each priority to main's
// time where no two writes of an issue overlap. Over the corpus of seedNewestEventCorpus, whose
// events rise in time with their seq, priorityNewestEventQuery answers main's (priority, time)
// rows, read with no project named and with one. The read that the route answers from
// (readPriorities) carries main's time on each priority, and null where main answers none: a
// priority whose members have no event, or none in the project named.
func TestPriorityNewestEventAnswersMainsTime(t *testing.T) {
	t.Parallel()
	corpus := seedNewestEventCorpus(t)
	seedKeys, seedIDs := priorityCorpusSeed(t, corpus.served, corpus.priorities)
	answered := make([]map[string]time.Time, 0, len(newestEventScopes))
	for _, scope := range newestEventScopes {
		want := newestEventTimes(t, corpus.database, mainPriorityNewestEventQuery, seedKeys, seedIDs, scope.projects)
		for _, id := range []string{newestEventSilentPriority, newestEventUnstatedPriority} {
			if at, has := want[id]; has {
				t.Fatalf("main answers %v for %s over %s, which has no member with an event", at, id, scope.name)
			}
		}
		for _, id := range corpus.trees {
			if _, has := want[id]; !has && len(scope.projects) == 0 {
				t.Fatalf("main answers no time for %s over %s, so the comparison holds nothing of it", id, scope.name)
			}
		}
		answered = append(answered, want)

		got := newestEventTimes(t, corpus.database, priorityNewestEventQuery, seedKeys, seedIDs, scope.projects)
		if !maps.EqualFunc(got, want, time.Time.Equal) {
			t.Errorf("the newest event over %s answers\n%v\nwant main's\n%v", scope.name, got, want)
		}

		body, err := corpus.served.readPriorities(context.Background(), scope.projects, false)
		if err != nil {
			t.Fatalf("read the priorities over %s: %v", scope.name, err)
		}
		if ids := priorityIDsOf(body); !slices.Equal(ids, corpus.priorities) {
			t.Fatalf("the read over %s answers the priorities %v, want %v", scope.name, ids, corpus.priorities)
		}
		for _, row := range body.Priorities {
			wantAt, has := want[row.ID]
			switch newest := row.Work.NewestEventAt; {
			case !has && newest != nil:
				t.Errorf("the read over %s answers %v for %s, want null, as main answers no time for it", scope.name, *newest, row.ID)
			case has && newest == nil:
				t.Errorf("the read over %s answers null for %s, want main's %v", scope.name, row.ID, wantAt)
			case has && !newest.Equal(wantAt):
				t.Errorf("the read over %s answers %v for %s, want main's %v", scope.name, *newest, row.ID, wantAt)
			}
		}
	}
	if maps.EqualFunc(answered[0], answered[1], time.Time.Equal) {
		t.Fatalf("main answers the same times over %s and over %s, so the read with one project holds nothing the other does not:\n%v",
			newestEventScopes[0].name, newestEventScopes[1].name, answered[0])
	}
}

// TestPriorityNewestEventReadsOneEventOfEachMember holds the plans of the newest event under each
// priority to what they read. The plans are two, each run and measured at the planner's default
// settings, with no enable_* setting changed, which are the settings the server's connections
// plan under: the generic plan that a prepared statement settles on
// (explaintest.AnalyzedGenericPlan) and the plan made for the values of the read
// (explaintest.AnalyzedPlanUnder). Each is read with no project named and with one. Sequential
// scans stay on: with them off, the planner reads a table it would scan whole through the whole
// of one of its indexes instead, with no index condition, which is the same read under another
// name, in a plan the server does not run. Each plan of priorityNewestEventQuery
// (requireNewestEventPlan):
//
//   - reads one event of each member issue it joins (requireOneEventOfEachMember), and not one
//     for each member row: an issue that advances three priorities is read once. The corpus holds
//     many more issues with events than member issues, and fewer member issues than member rows
//     (seedNewestEventCorpus), so a plan that probes the events of every issue before it joins the
//     members, or that probes once for each member row, reads events more times than the check
//     permits;
//   - while its walk over the members is main's (mainNewestEventWalk), reads every table through
//     an index beside the walk (requireKeyedBesideTheWalk), runs one read of issues in the walk
//     (requireWalkReadsIssuesOnce), and reads no more shared buffers in the walk than main's walk
//     reads under the same plan over the same scope (requireWalkWithinMains). Main's walk reads
//     the whole of issues, with no index condition, but once for the whole walk, where a walk
//     keyed through issues_parent_key probes the table once for each member row;
//   - where its walk is any other, reads every table through an index, the walk's read included
//     (requireEveryScanKeyed).
//
// The same holds are run on the statement with its walk keyed through issues_parent_key
// (priorityNewestEventKeyedWalkQuery), which takes the second branch, so each branch holds a
// plan the test reads.
//
// Each check must fail a control under each plan over each scope, and the test fails when one
// passes, since the check would then be one that cannot fail (explaintest.RequireControlFails):
//
//   - main's text fails the read of events and the reads beside the walk: it joins every event of
//     every member issue, and its plans read events in a Seq Scan and the members' issues rows
//     with no index condition, in a Seq Scan or a whole Index Only Scan of issues_pkey;
//   - the keyed walk fails the two holds of main's walk: it probes issues once for each member
//     row, and on this corpus those probes read more shared buffers than main's one read of the
//     table (68 against 42, PostgreSQL 18.7, 2026-11-25);
//   - priorityNewestEventQuery fails the index condition on every scan, since its walk, main's,
//     reads issues in a Seq Scan.
func TestPriorityNewestEventReadsOneEventOfEachMember(t *testing.T) {
	t.Parallel()
	if strings.Contains(priorityNewestEventKeyedWalkQuery, mainNewestEventWalk) {
		t.Fatalf("the statement with its walk keyed (priorityNewestEventKeyedWalkQuery) carries main's walk, so no plan the test reads takes the keyed branch")
	}
	corpus := seedNewestEventCorpus(t)
	settleSpeedCorpus(t, corpus.database)
	seedKeys, seedIDs := priorityCorpusSeed(t, corpus.served, corpus.priorities)
	ctx := context.Background()
	keys := explaintest.NewIndexKeys(corpus.database.Pool)
	connection, err := corpus.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plans: %v", err)
	}
	defer connection.Release()

	const mains, statement, keyed = 0, 1, 2
	texts := []struct{ name, statement string }{
		mains:     {"main's newest event", mainPriorityNewestEventQuery},
		statement: {"the newest event", priorityNewestEventQuery},
		keyed:     {"the newest event with its walk keyed", priorityNewestEventKeyedWalkQuery},
	}
	for scopeNumber, scope := range newestEventScopes {
		members := newestEventMemberIssues(t, corpus.database, seedKeys, seedIDs, scope.projects, scope.name)
		arguments := []any{seedKeys, seedIDs, scope.projects}
		parameters, literals := benchtime.PreparedArguments(t, "the newest event over "+scope.name, arguments...)
		plans := []struct {
			name  string
			plans []json.RawMessage
		}{{name: "the generic plan"}, {name: "the plan for its values"}}
		for textNumber, text := range texts {
			plans[0].plans = append(plans[0].plans, explaintest.AnalyzedGenericPlan(t, ctx, connection,
				fmt.Sprintf("newest_event_%d_%d", scopeNumber, textNumber), text.statement, parameters, literals))
			plans[1].plans = append(plans[1].plans, explaintest.AnalyzedPlanUnder(t, ctx, corpus.database.Pool,
				text.statement, nil, arguments...))
		}
		for _, plan := range plans {
			subjects := make([]string, len(texts))
			for textNumber, text := range texts {
				subjects[textNumber] = fmt.Sprintf("%s under %s over %s", text.name, plan.name, scope.name)
			}
			mainsWalk := newestEventWalkBuffers(t, plan.plans[mains], subjects[mains])
			if mainsWalk == 0 {
				t.Fatalf("%s reads no shared buffer in its walk over the members, so the bound on the walk holds nothing:\n%s",
					subjects[mains], plan.plans[mains])
			}
			for _, held := range []int{statement, keyed} {
				requireNewestEventPlan(t, keys, texts[held].statement, plan.plans[held], subjects[held], members, mainsWalk)
			}

			for _, control := range []struct {
				text       int
				cannotTell string
				check      func(t testing.TB, plan json.RawMessage, subject string)
			}{
				{mains, "a read of every event from one read of a member", func(t testing.TB, plan json.RawMessage, subject string) {
					requireOneEventOfEachMember(t, keys, plan, subject, members)
				}},
				{mains, "a read of a whole table beside the walk from a read through an index", requireKeyedBesideTheWalk},
				{keyed, "a walk that reads issues for each member row from one that reads it once", requireWalkReadsIssuesOnce},
				{keyed, "a walk that reads more shared buffers than main's from one that reads no more", func(t testing.TB, plan json.RawMessage, subject string) {
					requireWalkWithinMains(t, plan, subject, mainsWalk)
				}},
				{statement, "a walk that reads the whole of issues from one keyed through an index", requireEveryScanKeyed},
			} {
				subject, controlPlan := subjects[control.text], plan.plans[control.text]
				explaintest.RequireControlFails(t, subject, "a control", control.cannotTell, controlPlan,
					func(t testing.TB) { control.check(t, controlPlan, subject) })
			}
		}
	}
}

// newestEventMembersQuery counts, over the seed and projects of a read, the member rows of the
// newest event under each priority: the distinct (issue, priority) rows of the walk
// (priorityMemberTerms) whose issue the project filter keeps (priorityProjectFilter). Beside them
// it counts the member issues of those rows, the distinct issues, which the read joins and
// probes the events of, and the issues with an event that the project filter keeps, which are
// what a read that probes the events of every issue probes.
var newestEventMembersQuery = `with recursive ` + priorityMemberTerms + `
	select
		(select count(*) from members m join issues i on i.key = m.key where ` + priorityProjectFilter + `),
		(select count(distinct m.key) from members m join issues i on i.key = m.key where ` + priorityProjectFilter + `),
		(select count(*) from issues i where i.last_seq > 0 and ` + priorityProjectFilter + `)`

// newestEventMemberIssues is how many member issues the newest event under each priority joins
// over the seed and projects of a read, scope (newestEventMembersQuery). It logs them beside the
// member rows and the issues with an event in the same projects. A corpus whose member issues
// are as many as its member rows fails the test: there no issue advances two priorities, and the
// bound on the member issues (requireOneEventOfEachMember) would pass a plan that probes once
// for each member row.
func newestEventMemberIssues(t *testing.T, database *store.Store, seedKeys, seedIDs, projects []string, scope string) int {
	t.Helper()
	var rows, members, issues int
	if err := database.Pool.QueryRow(context.Background(), newestEventMembersQuery, seedKeys, seedIDs, projects).
		Scan(&rows, &members, &issues); err != nil {
		t.Fatalf("count the member issues over %s: %v", scope, err)
	}
	t.Logf("over %s the read joins %d member issues in %d member rows, and %d issues with an event stand in its projects",
		scope, members, rows, issues)
	if members >= rows {
		t.Fatalf("over %s the read joins %d member issues in %d member rows, so no issue advances two priorities and the check would pass a probe for each member row",
			scope, members, rows)
	}
	return members
}

// requireOneEventOfEachMember holds plan, an analyzed plan of a newest event statement, to reading
// one event of each member issue it joins, the member's event of last_seq, and no other event.
// Every scan of events is an Index Scan of newestEventIndex whose Index Cond names both columns
// of the index: issue_key, its first (IndexKeys.RequireIndexKeyed), and seq. Each such scan
// answers at most one row each time it runs, and at least one of them ran, so a plan that read no
// event shows nothing. A scan of events of any other kind is a read beyond the one event of a
// member. The scans of events run at most members times in all, the member issues the read joins
// (newestEventMemberIssues): a plan that probes the events of an issue before it joins the issue
// to the members probes every issue the project filter keeps, and a plan that probes once for
// each member row, an (issue, priority) pair, probes an issue once for every priority it
// advances. Each failure names subject and prints the plan through t.Errorf, which is what a
// control reads (explaintest.RequireControlFails).
func requireOneEventOfEachMember(t testing.TB, keys *explaintest.IndexKeys, plan json.RawMessage, subject string, members int) {
	t.Helper()
	keys.RequireIndexKeyed(t, plan, newestEventIndex, subject, explaintest.CondRequired)
	var loops float64
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != "events" {
			continue
		}
		loops += node.Loops
		if node.Type != "Index Scan" || node.Index != newestEventIndex {
			t.Errorf("%s reads events by %s, want only Index Scans of %s:\n%s", subject, node, newestEventIndex, plan)
			continue
		}
		if !explaintest.CondNamesFirstKey(node.IndexCond, "seq") {
			t.Errorf("%s reads %s on `%s`, which names no seq, so it reads every event of an issue, want its one event of last_seq:\n%s",
				subject, node, node.IndexCond, plan)
		}
		if node.Rows > 1 {
			t.Errorf("%s reads %v rows of events through %s each of the %v times it runs, want at most one:\n%s",
				subject, node.Rows, node, node.Loops, plan)
		}
	}
	if loops == 0 {
		t.Errorf("%s ran no read of events, so its plan shows nothing of how it reads them:\n%s", subject, plan)
	}
	t.Logf("%s reads events %v times in all, for the %d member issues it joins", subject, loops, members)
	if loops > float64(members) {
		t.Errorf("%s reads events %v times in all, more than the %d member issues it joins, want at most one read for each member issue:\n%s",
			subject, loops, members, plan)
	}
}

// requireNewestEventPlan holds plan, an analyzed plan of statement, a text of the newest event
// under each priority, to every hold of TestPriorityNewestEventReadsOneEventOfEachMember: one read
// of the event of each member issue (requireOneEventOfEachMember), and the holds of its walk over
// the members, which its text decides. A text that carries main's walk (mainNewestEventWalk)
// reads every table beside the walk through an index, runs one read of issues in the walk, and
// reads no more shared buffers in the walk than mainsWalk, those main's walk reads under the same
// plan over the same scope. A text whose walk is any other reads every table through an index,
// the walk's read included. members is how many member issues the read joins
// (newestEventMemberIssues). Each failure names subject and prints the plan.
func requireNewestEventPlan(t testing.TB, keys *explaintest.IndexKeys, statement string, plan json.RawMessage, subject string, members int, mainsWalk float64) {
	t.Helper()
	requireOneEventOfEachMember(t, keys, plan, subject, members)
	if !strings.Contains(statement, mainNewestEventWalk) {
		t.Logf("%s takes a walk over the members other than main's, so every read of it is held to an index condition", subject)
		requireEveryScanKeyed(t, plan, subject)
		return
	}
	t.Logf("%s takes main's walk over the members, so every read beside the walk is held to an index condition, and the walk to one read of issues and to main's shared buffers", subject)
	requireKeyedBesideTheWalk(t, plan, subject)
	requireWalkReadsIssuesOnce(t, plan, subject)
	requireWalkWithinMains(t, plan, subject, mainsWalk)
}

// newestEventWalk is the plan of the walk over the members in plan, an analyzed plan of a text of
// the newest event: the one node that begins the subplan newestEventWalkCTE, with every node
// under it. The walk is told by its place in the plan rather than by the tables it reads, so a
// read of issues beside the walk is never taken for the walk's own. A plan that holds no such
// node, or more than one, fails the test, since what is held of the walk would then be read off
// nothing.
func newestEventWalk(t testing.TB, plan json.RawMessage, subject string) json.RawMessage {
	t.Helper()
	var walks []json.RawMessage
	var visit func(node json.RawMessage)
	visit = func(node json.RawMessage) {
		var read struct {
			SubplanName string            `json:"Subplan Name"`
			Plans       []json.RawMessage `json:"Plans"`
		}
		if err := json.Unmarshal(node, &read); err != nil {
			t.Fatalf("decode a node of the plan of %s: %v", subject, err)
		}
		if read.SubplanName == newestEventWalkCTE {
			walks = append(walks, node)
		}
		for _, child := range read.Plans {
			visit(child)
		}
	}
	visit(plan)
	if len(walks) != 1 {
		t.Fatalf("%s holds %d plans of %s, want one:\n%s", subject, len(walks), newestEventWalkCTE, plan)
	}
	return walks[0]
}

// newestEventWalkBuffers is the shared buffers, hit plus read, that the walk over the members in
// plan reads (newestEventWalk): those of the walk's top node, which count every node under it.
func newestEventWalkBuffers(t testing.TB, plan json.RawMessage, subject string) float64 {
	t.Helper()
	top := explaintest.Nodes(t, newestEventWalk(t, plan, subject))[0]
	return top.SharedHitBlocks + top.SharedReadBlocks
}

// requireKeyedBesideTheWalk holds plan to reading every table through an index beside the walk
// over the members: each scan that explaintest.ScansWithoutIndexCond names in plan is one of the
// walk's (newestEventWalk). The walk's scans are taken out of the plan's one for one, so a read
// beside the walk is still named where the walk reads the same table the same way.
func requireKeyedBesideTheWalk(t testing.TB, plan json.RawMessage, subject string) {
	t.Helper()
	unkeyed := explaintest.ScansWithoutIndexCond(t, plan)
	for _, scan := range explaintest.ScansWithoutIndexCond(t, newestEventWalk(t, plan, subject)) {
		if at := slices.Index(unkeyed, scan); at >= 0 {
			unkeyed = slices.Delete(unkeyed, at, at+1)
		}
	}
	if len(unkeyed) > 0 {
		t.Errorf("%s reads %s with no index condition beside its walk over the members (%s), want every read beside the walk through an index:\n%s",
			subject, strings.Join(unkeyed, ", "), newestEventWalkCTE, plan)
	}
}

// requireWalkReadsIssuesOnce holds the walk over the members in plan (newestEventWalk) to one
// read of issues: one scan of the table, which runs once. Main's walk hashes issues from that one
// read and joins each step's members to the hash; a walk that probes issues for each member row
// runs its scan once for each.
func requireWalkReadsIssuesOnce(t testing.TB, plan json.RawMessage, subject string) {
	t.Helper()
	var reads []explaintest.Scan
	for _, scan := range explaintest.Scans(t, newestEventWalk(t, plan, subject)) {
		if scan.Relation == "issues" {
			reads = append(reads, scan)
		}
	}
	if len(reads) != 1 {
		t.Errorf("%s scans issues %d times in its walk over the members (%s), want one scan:\n%s",
			subject, len(reads), newestEventWalkCTE, plan)
		return
	}
	if reads[0].Loops != 1 {
		t.Errorf("%s runs its scan of issues in its walk over the members (%s) %v times, want once:\n%s",
			subject, newestEventWalkCTE, reads[0].Loops, plan)
	}
}

// requireWalkWithinMains holds the walk over the members in plan (newestEventWalk) to reading no
// more shared buffers, hit plus read, than mainsWalk, those main's walk reads under the same plan
// over the same scope.
func requireWalkWithinMains(t testing.TB, plan json.RawMessage, subject string, mainsWalk float64) {
	t.Helper()
	walk := newestEventWalkBuffers(t, plan, subject)
	t.Logf("%s reads %v shared buffers in its walk over the members, and main's walk %v", subject, walk, mainsWalk)
	if walk > mainsWalk {
		t.Errorf("%s reads %v shared buffers in its walk over the members (%s), more than the %v of main's walk under the same plan, want at most main's:\n%s",
			subject, walk, newestEventWalkCTE, mainsWalk, plan)
	}
}

// requireEveryScanKeyed holds plan to reading every table through an index, the walk over the
// members included: explaintest.ScansWithoutIndexCond names no scan of it.
func requireEveryScanKeyed(t testing.TB, plan json.RawMessage, subject string) {
	t.Helper()
	if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
		t.Errorf("%s reads %s with no index condition, want every read through an index, the walk's over the members included:\n%s",
			subject, strings.Join(unkeyed, ", "), plan)
	}
}

// TestPriorityNewestEventIsEarlierOnlyInsideItsBound holds the newest event under each priority,
// where two writes of an issue overlap, to a time earlier than main's by less than the time from
// the start of the write that began first to the other write's commit. Each write is an append
// through the broker (events.Broker.Append) on a transaction of the test's own pool, as Dispatch
// appends an event. In each case, the transaction that began first commits last, so its event is
// the issue's event of last_seq, and its created_at, its own start, is earlier than the other
// write's. The newest event is then that earlier time, main's is the later one, and the two stand
// apart by less than the time from the first write's start to the other write's commit. The
// commit is read as the server's clock_timestamp() once the commit has returned, which is after
// the commit.
//
// Each case holds a priority of its own, whose members no other case writes to. The cases run one
// after another, since the second counts the backends of the test's database that wait on a lock.
func TestPriorityNewestEventIsEarlierOnlyInsideItsBound(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	broker := events.NewBroker()
	at := time.Now().UTC().Add(-time.Hour)
	const project = "OVER"
	one, waits, twoRoot, twoChild := seededIssueKey(project, 1), seededIssueKey(project, 2),
		seededIssueKey(project, 3), seededIssueKey(project, 4)
	issues := [][]any{}
	for number, key := range []string{one, waits, twoRoot, twoChild} {
		row := seededIssueRow{
			key: key, project: project, number: number + 1, title: "Overlapping writes " + key,
			status: "todo", rank: seededRank(number), at: at,
		}
		if key == twoChild {
			row.parent = new(twoRoot)
		}
		issues = append(issues, row.values())
	}
	copySeededRows(t, database, "projects", seededProjectColumns, [][]any{seededProject(project, len(issues), at)})
	copySeededRows(t, database, "issues", seededIssueColumns, issues)
	// The seed the read sends for the relations of the three priorities: each is stated on one
	// issue, and the third on the root whose child is its second member.
	seedKeys := []string{one, waits, twoRoot}
	seedIDs := []string{"overlap-one", "overlap-waits", "overlap-two-members"}

	t.Run("A begins, B appends and commits, then A appends", func(t *testing.T) {
		a := beginOverlapWrite(t, database)
		b := beginOverlapWrite(t, database)
		later := b.append(t, broker, one)
		b.commit(t)
		committed := serverClock(t, database)
		earlier := a.append(t, broker, one)
		a.commit(t)
		requireOverlap(t, a, earlier, later)
		requireNewestInsideBound(t, database, seedKeys, seedIDs, "overlap-one", earlier.CreatedAt, later.CreatedAt,
			committed.Sub(a.began))
	})

	t.Run("A begins, B appends, A's append waits on the issue's lock until B commits", func(t *testing.T) {
		ctx := t.Context()
		a := beginOverlapWrite(t, database)
		b := beginOverlapWrite(t, database)
		later := b.append(t, broker, waits)
		type appended struct {
			event model.Event
			err   error
		}
		waited, done := make(chan appended, 1), make(chan struct{})
		go func() {
			defer close(done)
			event, err := broker.Append(ctx, a.tx, overlapEvent(waits))
			waited <- appended{event, err}
		}()
		// B's end lets A's append through, and A's append ends before the cleanups roll A
		// back, so the two never use A's connection at once.
		t.Cleanup(func() {
			_ = b.tx.Rollback(context.Background())
			<-done
		})
		awaitLockWaiters(t, database, 1)
		b.commit(t)
		committed := serverClock(t, database)
		var earlier model.Event
		select {
		case result := <-waited:
			if result.err != nil {
				t.Fatalf("append A's event once B committed: %v", result.err)
			}
			earlier = result.event
		case <-time.After(15 * time.Second):
			t.Fatal("A's append still waits 15 s after B committed")
		}
		a.commit(t)
		requireOverlap(t, a, earlier, later)
		requireNewestInsideBound(t, database, seedKeys, seedIDs, "overlap-waits", earlier.CreatedAt, later.CreatedAt,
			committed.Sub(a.began))
	})

	t.Run("two members, each with an overlap of its own length", func(t *testing.T) {
		// The child's overlap holds the root's whole overlap inside it, so it is the longer.
		childA := beginOverlapWrite(t, database)

		rootA := beginOverlapWrite(t, database)
		rootB := beginOverlapWrite(t, database)
		rootLater := rootB.append(t, broker, twoRoot)
		rootB.commit(t)
		rootCommitted := serverClock(t, database)
		rootEarlier := rootA.append(t, broker, twoRoot)
		rootA.commit(t)
		requireOverlap(t, rootA, rootEarlier, rootLater)

		childB := beginOverlapWrite(t, database)
		childLater := childB.append(t, broker, twoChild)
		childB.commit(t)
		childCommitted := serverClock(t, database)
		childEarlier := childA.append(t, broker, twoChild)
		childA.commit(t)
		requireOverlap(t, childA, childEarlier, childLater)

		shorter, longer := rootCommitted.Sub(rootA.began), childCommitted.Sub(childA.began)
		if longer <= shorter {
			t.Fatalf("the child's overlap is %v and the root's %v, want the child's the longer, so the case is not the one it names", longer, shorter)
		}
		// The newest event is the root's earlier write, which began after the child's, and
		// main's is the child's later write, which began after every other. The two stand
		// further apart than the root's overlap, so only the longer overlap bounds them.
		if apart := childLater.CreatedAt.Sub(rootEarlier.CreatedAt); apart <= shorter {
			t.Fatalf("the two times stand %v apart, within the shorter overlap of %v, so the case does not tell the two overlaps apart", apart, shorter)
		}
		requireNewestInsideBound(t, database, seedKeys, seedIDs, "overlap-two-members", rootEarlier.CreatedAt,
			childLater.CreatedAt, longer)
	})
}

// overlapWrite is one writing transaction of an overlap: the transaction, and its start, now(),
// which is the created_at of every event it appends (store/migrations/0001_init.up.sql).
type overlapWrite struct {
	tx    pgx.Tx
	began time.Time
}

// beginOverlapWrite begins a transaction on database's pool and reads its start. A transaction
// that the test does not commit rolls back when the test ends.
func beginOverlapWrite(t *testing.T, database *store.Store) overlapWrite {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a write: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var began time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&began); err != nil {
		t.Fatalf("read the write's start: %v", err)
	}
	return overlapWrite{tx: tx, began: began}
}

// overlapEvent is the event each write appends on the issue key.
func overlapEvent(key string) model.Event {
	return model.Event{
		IssueKey: new(key),
		Type:     "test.overlap",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload:  map[string]any{},
	}
}

// append appends overlapEvent on key in w's transaction, through broker.
func (w overlapWrite) append(t *testing.T, broker *events.Broker, key string) model.Event {
	t.Helper()
	appended, err := broker.Append(t.Context(), w.tx, overlapEvent(key))
	if err != nil {
		t.Fatalf("append an event on %s: %v", key, err)
	}
	return appended
}

// commit commits w's transaction.
func (w overlapWrite) commit(t *testing.T) {
	t.Helper()
	if err := w.tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit a write: %v", err)
	}
}

// serverClock is the server's clock now, clock_timestamp(), the clock that now() reads a
// transaction's start from.
func serverClock(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(t.Context(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the server's clock: %v", err)
	}
	return now
}

// requireOverlap holds one overlap to the case it names, before any time is read. The earlier
// event, which first wrote, carries first's start as its created_at, and that is before the later
// event's. It took the issue's next seq after the later event, since the issue's lock let first
// take its seq only after the other write committed.
func requireOverlap(t *testing.T, first overlapWrite, earlier, later model.Event) {
	t.Helper()
	if !earlier.CreatedAt.Equal(first.began) {
		t.Fatalf("the event of the write that began first was created at %v, want its transaction's start %v", earlier.CreatedAt, first.began)
	}
	if !earlier.CreatedAt.Before(later.CreatedAt) {
		t.Fatalf("the write that began first was created at %v, not before the other write's %v", earlier.CreatedAt, later.CreatedAt)
	}
	if earlier.Seq != later.Seq+1 {
		t.Fatalf("the write that began first took seq %d, want %d, the one after the other write's", earlier.Seq, later.Seq+1)
	}
}

// requireNewestInsideBound holds priority's two times to that bound after an overlap. The newest
// event is wantNew, the created_at of the write that began first and committed last. Main's is
// wantMain, the created_at of the other write. The newest event is never later than main's, and
// main's is later by less than bound, the time from that start to that commit.
func requireNewestInsideBound(t *testing.T, database *store.Store, seedKeys, seedIDs []string, priority string, wantNew, wantMain time.Time, bound time.Duration) {
	t.Helper()
	newest, has := newestEventTimes(t, database, priorityNewestEventQuery, seedKeys, seedIDs, []string{})[priority]
	if !has {
		t.Fatalf("the newest event answers no time for %s", priority)
	}
	mains, has := newestEventTimes(t, database, mainPriorityNewestEventQuery, seedKeys, seedIDs, []string{})[priority]
	if !has {
		t.Fatalf("main answers no time for %s", priority)
	}
	if !newest.Equal(wantNew) {
		t.Errorf("the newest event under %s is %v, want %v, the created_at of the write that began first and committed last, whose event is of last_seq",
			priority, newest, wantNew)
	}
	if !mains.Equal(wantMain) {
		t.Errorf("main's newest event under %s is %v, want %v, the created_at of the write that began last", priority, mains, wantMain)
	}
	if newest.After(mains) {
		t.Errorf("the newest event under %s is %v, later than main's %v", priority, newest, mains)
	}
	if apart := mains.Sub(newest); apart >= bound {
		t.Errorf("main's newest event under %s is %v later than the newest event, want less than the %v from the first write's start to the other write's commit",
			priority, apart, bound)
	}
}
