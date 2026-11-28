package api

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// How the corpus a priority's issue list is measured and compared over is built
// (TestAPriorityListAnswersWithinItsBoundOnEveryPlan, TestAPriorityListAnswersAsTheMergeBaseAnswers,
// TestNoListingRequestReadsMoreOnTheGenericPlan).
// It is a file of its own for the reason priority_speed_seed_test.go is: none of it is a reading.
//
// The corpus holds production's counts as dispatch://INFRA-5609 gives them: 13,298 issues, 13
// live priorities, 7,677 issues that advance one, and 598 open issues under the largest. Every row
// is written by COPY (priority_fixture_test.go), and nothing is drawn at random: an issue's
// project, status, parent and relations follow from its place in the corpus, so every run builds
// the same database.
//
//   - Issue n (from 0) is in project PRD0(n % 6 + 1), was made n minutes after the corpus's start,
//     and stands in the status its place draws from production's shares (listCorpusStatuses),
//     each done issue closed an hour after it was made. Every issue under priority-01 is open and
//     draws from the open statuses' shares alone, so the corpus is a third done rather than
//     production's 35%.
//   - The issues that advance a priority are an even spread of 7,677 places. The k-th of them
//     belongs to priority k % 13, so seven priorities hold 591 and six hold 590, and each
//     priority's issues, in order, are cut into trees of the sizes listCorpusTreeSizes repeats.
//     A tree is a binary heap: its first issue states the priority, its own relation, and each
//     other issue hangs under the one at half its position, so a tree of 30 reaches four levels
//     down and the rest inherit. 1,586 trees state 1,586 relations.
//   - One leaf four levels down in the first tree of 30 of each of priority-02 to priority-08
//     states priority-01 beside the priority it inherits: that brings priority-01 to 598, and the
//     issues that state a relation of their own to 1,593 (production's 1,600).
//   - The root of every eighth tree first asserts and then retracts the next priority, which it
//     therefore does not advance.
//   - Of the 5,621 issues that advance none, as many hang under another of them as bring the
//     corpus to production's 68% with a parent. 25 of their roots state a relation to a priority
//     that is gone, and 25 others assert and retract one to a live priority, so all of them and
//     the issues below them still advance none.
//   - Every fifth issue of a project carries the label Alpha and every seventh beta and ALPHA, an
//     issue's priority is its number % 4 (3 being none), alice pins every eleventh issue and
//     holds an unpinned row on every eleventh but five, and every thirteenth issue is blocked by
//     the one before it: so the comparison's filters each have something to cut.
const (
	listCorpusIssues     = 13298
	listCorpusProjects   = 6
	listCorpusPriorities = 13
	listCorpusAdvancing  = 7677
	listCorpusLargest    = 598
	// listCorpusParentedPercent is the share of the corpus's issues that have a parent.
	listCorpusParentedPercent = 68
	// listCorpusLeaf is the position, from 0, of the leaf of a tree of listCorpusLeafTree issues
	// that states priority-01 beside the priority it inherits: position 16 from 1, whose children
	// would stand at 32 and 33, four levels below the tree's root.
	listCorpusLeaf     = 15
	listCorpusLeafTree = 30
	// listCorpusUnadvancedStated is how many roots of the issues advancing none state a relation
	// to the gone priority, and as many again assert and retract one to a live priority.
	listCorpusUnadvancedStated = 25
)

// listCorpusTreeSizes are the sizes of a priority's trees, repeated in order: 1,586 trees over
// the 7,677 issues, as production's 1,600 stated relations carry its 7,677 issues.
var listCorpusTreeSizes = []int{1, 2, 1, 3, 2, 1, 4, 2, 3, listCorpusLeafTree}

// listCorpusStatuses are production's shares of the statuses, in thousandths: an issue's place
// times a prime, modulo 1,000, picks the first status whose running total it is below.
var listCorpusStatuses = []struct {
	status    string
	thousands int
}{
	{"done", 350}, {"in_progress", 100}, {"todo", 170}, {"backlog", 150}, {"triage", 100},
	{"icebox", 80}, {"needs_review", 20}, {"testing", 20}, {"retro", 10},
}

// listCorpus is the corpus as the two tests address it: its database, the live priorities with
// the largest first, the gone one, the parents the comparison filters by, the moments its issues
// were made at, and the counts its first line reports.
type listCorpus struct {
	database   *store.Store
	priorities []string
	gone       string
	// parents are the root of priority-01's first tree of 30 and the first root of the issues
	// advancing none, each with issues below it.
	parents []string
	// start is the moment the corpus's first issue was made; issue n was made n minutes later.
	start    time.Time
	done     int
	parented int
	stating  int
	trees    int
}

// String is the corpus's own report, which each test prints as its first line.
func (c listCorpus) String() string {
	return fmt.Sprintf("corpus: %d issues over %d projects, %d done and %d with a parent; %d live priorities and 1 gone; "+
		"%d issues advancing a live priority, %d of them stating one of their own over %d trees at most 4 levels deep; "+
		"%d open under %s, the largest",
		listCorpusIssues, listCorpusProjects, c.done, c.parented, listCorpusPriorities,
		listCorpusAdvancing, c.stating, c.trees, listCorpusLargest, c.priorities[0])
}

// listCorpusStatus is the status the issue at place stands in: open alone draws from the open
// statuses' shares only.
func listCorpusStatus(place int, open bool) string {
	total := 0
	for _, share := range listCorpusStatuses {
		if !open || share.status != "done" {
			total += share.thousands
		}
	}
	drawn, reach := place*7919%total, 0
	for _, share := range listCorpusStatuses {
		if open && share.status == "done" {
			continue
		}
		reach += share.thousands
		if drawn < reach {
			return share.status
		}
	}
	panic("the status shares do not reach their total")
}

// seedListCorpus writes the corpus, settles it (settleSpeedCorpus) and holds it to its own counts.
func seedListCorpus(t *testing.T) listCorpus {
	t.Helper()
	database := storetest.Open(t)
	start := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	corpus := listCorpus{database: database, gone: "priority-gone", start: start}
	for index := range listCorpusPriorities {
		corpus.priorities = append(corpus.priorities, fmt.Sprintf("priority-%02d", index+1))
	}

	projects := make([]string, listCorpusProjects)
	numbers := make([]int, listCorpusProjects)
	for index := range projects {
		projects[index] = fmt.Sprintf("PRD%02d", index+1)
	}
	keys := make([]string, listCorpusIssues)
	for place := range keys {
		project := place % listCorpusProjects
		numbers[project]++
		keys[place] = seededIssueKey(projects[project], numbers[project])
	}
	var advancing, unadvanced []int
	for place := range listCorpusIssues {
		if (place+1)*listCorpusAdvancing/listCorpusIssues > place*listCorpusAdvancing/listCorpusIssues {
			advancing = append(advancing, place)
		} else {
			unadvanced = append(unadvanced, place)
		}
	}
	if len(advancing) != listCorpusAdvancing {
		t.Fatalf("the corpus spreads %d issues advancing a priority, want %d", len(advancing), listCorpusAdvancing)
	}

	parents := make(map[int]int, listCorpusIssues)
	members := make([][]int, listCorpusPriorities)
	for order, place := range advancing {
		members[order%listCorpusPriorities] = append(members[order%listCorpusPriorities], place)
	}
	doubled := listCorpusLargest - len(members[0])
	underLargest := make(map[int]bool, listCorpusLargest)
	for _, place := range members[0] {
		underLargest[place] = true
	}
	if doubled < 1 || doubled >= listCorpusPriorities {
		t.Fatalf("priority-01 holds %d issues of its own, so %d leaves of other priorities would bring it to %d", len(members[0]), doubled, listCorpusLargest)
	}
	at := start.Add(-time.Hour)
	tuples := [][]any{}
	for priority, group := range members {
		stated := false
		for tree, first := 0, 0; first < len(group); tree++ {
			size := min(listCorpusTreeSizes[tree%len(listCorpusTreeSizes)], len(group)-first)
			heap := group[first : first+size]
			first += size
			root := keys[heap[0]]
			if corpus.trees%8 == 7 {
				other := corpus.priorities[(priority+1)%listCorpusPriorities]
				tuples = append(tuples, seededAdvancesTuple(root, other, at), retractedAdvancesTuple(root, other, at))
			}
			tuples = append(tuples, seededAdvancesTuple(root, corpus.priorities[priority], at))
			corpus.trees++
			corpus.stating++
			for position := 1; position < size; position++ {
				parents[heap[position]] = heap[(position+1)/2-1]
			}
			if size != listCorpusLeafTree {
				continue
			}
			if priority == 0 && len(corpus.parents) == 0 {
				corpus.parents = append(corpus.parents, root)
			}
			if priority >= 1 && priority <= doubled && !stated {
				tuples = append(tuples, seededAdvancesTuple(keys[heap[listCorpusLeaf]], corpus.priorities[0], at))
				underLargest[heap[listCorpusLeaf]] = true
				corpus.stating++
				stated = true
			}
		}
		if priority >= 1 && priority <= doubled && !stated {
			t.Fatalf("%s has no tree of %d issues to put a leaf stating %s in", corpus.priorities[priority], listCorpusLeafTree, corpus.priorities[0])
		}
	}
	if len(underLargest) != listCorpusLargest {
		t.Fatalf("%d issues are under %s, want %d", len(underLargest), corpus.priorities[0], listCorpusLargest)
	}

	children := listCorpusIssues*listCorpusParentedPercent/100 - (listCorpusAdvancing - corpus.trees)
	roots := len(unadvanced) - children
	if children < 1 || roots < 1 {
		t.Fatalf("the issues advancing none are %d, too few for %d of them to hang under the others", len(unadvanced), children)
	}
	for order := roots; order < len(unadvanced); order++ {
		parents[unadvanced[order]] = unadvanced[order-roots]
	}
	corpus.parents = append(corpus.parents, keys[unadvanced[0]])
	for order := range listCorpusUnadvancedStated {
		gone, retracted := keys[unadvanced[2*order*roots/(2*listCorpusUnadvancedStated)]], keys[unadvanced[(2*order+1)*roots/(2*listCorpusUnadvancedStated)]]
		live := corpus.priorities[order%listCorpusPriorities]
		tuples = append(tuples, seededAdvancesTuple(gone, corpus.gone, at),
			seededAdvancesTuple(retracted, live, at), retractedAdvancesTuple(retracted, live, at))
	}

	issues := make([][]any, 0, listCorpusIssues)
	clear(numbers)
	for place, key := range keys {
		project := place % listCorpusProjects
		numbers[project]++
		status := listCorpusStatus(place, underLargest[place])
		made := start.Add(time.Duration(place) * time.Minute)
		var parent *string
		if above, found := parents[place]; found {
			parent = &keys[above]
			corpus.parented++
		}
		var closed *time.Time
		if status == "done" {
			closing := made.Add(time.Hour)
			closed = &closing
			corpus.done++
		}
		issues = append(issues, seededIssueRow{
			key: key, project: projects[project], number: numbers[project],
			title: fmt.Sprintf("Production-shaped work %05d", place), status: status, parent: parent,
			rank: seededRank(place), at: made, closedAt: closed,
		}.values())
	}

	priorityRows := make([][]any, 0, listCorpusPriorities+1)
	for index, id := range corpus.priorities {
		priorityRows = append(priorityRows, seededPriorityNode(t, id, fmt.Sprintf("Priority %02d", index+1),
			"What this priority is for.", "alice", fmt.Sprintf("s%02d", index), at))
	}
	gone := seededPriorityNode(t, corpus.gone, "A priority that is gone", "What it was for.", "alice", "z99", at)
	gone[slices.Index(seededNodeColumns, "state")] = prioritystore.StateGone
	priorityRows = append(priorityRows, gone)

	projectRows := make([][]any, 0, listCorpusProjects)
	for index, key := range projects {
		projectRows = append(projectRows, seededProject(key, numbers[index], at))
	}
	copySeededRows(t, database, "projects", seededProjectColumns, projectRows)
	copySeededRows(t, database, "issues", seededIssueColumns, issues)
	copySeededRows(t, database, "priority_nodes", seededNodeColumns, priorityRows)
	copySeededRows(t, database, "priority_tuples", seededTupleColumns, tuples)
	for _, statement := range []string{
		`update issues set
			labels = case when number % 5 = 0 then array['Alpha'] when number % 7 = 0 then array['beta', 'ALPHA'] else '{}' end,
			priority = case when number % 4 = 3 then null else (number % 4)::smallint end`,
		`insert into user_issue_state (login, issue_key, pinned)
			select 'alice', key, number % 11 = 0 from issues where number % 11 in (0, 5)`,
		`insert into issue_links (issue_key, kind, target_key)
			select i.key, 'blocked_by', b.key from issues i
			join issues b on b.project_key = i.project_key and b.number = i.number - 1
			where i.number % 13 = 0`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("write the corpus's labels, priorities, pins and blockers: %v", err)
		}
	}
	settleSpeedCorpus(t, database)

	// The one count copySeededRows cannot check: that the gone priority's state landed as gone.
	live := 0
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from priority_nodes where state = 'live'`).Scan(&live); err != nil {
		t.Fatalf("count the corpus's live priorities: %v", err)
	}
	if live != listCorpusPriorities {
		t.Fatalf("the corpus holds %d live priorities, want %d", live, listCorpusPriorities)
	}
	return corpus
}
