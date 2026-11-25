package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The commits of a next change's branch: the head its last pull request merged, the base's head
// after the squash of that merge, a commit pushed on the branch after the merge, and a commit that
// holds the base's head.
var (
	nextMergedHead = strings.Repeat("1", 40)
	nextBaseHead   = strings.Repeat("2", 40)
	nextLater      = strings.Repeat("3", 40)
	nextHolding    = strings.Repeat("4", 40)
)

// nextBranch is the branch of the next change of LEGION-208.
const nextBranch = "legion/LEGION-208"

// nextBranchGitHub is acme/widgets's REST API as a next change's branch row calls it: each branch
// of refs by its exact name, a create that adds a branch, a move that writes one, and a compare of
// two commits answered with the status compared names for "base...head". Every create and move is
// recorded; any other request fails the test.
type nextBranchGitHub struct {
	url      string
	mu       sync.Mutex
	refs     map[string]string
	compared map[string]string
	creates  []map[string]string
	moves    []map[string]any
}

func newNextBranchGitHub(t *testing.T, refs, compared map[string]string) *nextBranchGitHub {
	t.Helper()
	g := &nextBranchGitHub{refs: refs, compared: compared}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		read, isRead := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/git/ref/heads/")
		moved, isMove := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/git/refs/heads/")
		basehead, isCompare := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/compare/")
		switch {
		case r.Method == http.MethodGet && isRead:
			commit, found := g.refs[read]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found","status":"404"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ref":"refs/heads/` + read + `","object":{"sha":"` + commit + `","type":"commit"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/git/refs":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the create: %v", err)
			}
			name := strings.TrimPrefix(body["ref"], "refs/heads/")
			if _, exists := g.refs[name]; exists {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"Reference already exists","status":"422"}`))
				return
			}
			g.creates = append(g.creates, body)
			g.refs[name] = body["sha"]
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPatch && isMove:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the move: %v", err)
			}
			g.moves = append(g.moves, body)
			g.refs[moved], _ = body["sha"].(string)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && isCompare && g.compared[basehead] != "":
			_, _ = w.Write([]byte(`{"status":"` + g.compared[basehead] + `"}`))
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	return g
}

// writes is every create and every move GitHub answered, and where the branch stands now.
func (g *nextBranchGitHub) writes() (creates []map[string]string, moves []map[string]any, tip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]string(nil), g.creates...), append([]map[string]any(nil), g.moves...), g.refs[nextBranch]
}

// nextBranchRig is an outbox over a fresh database that holds LEGION-208 at generation 2, a root
// on acme/widgets whose recorded default branch is main, with GitHub answered by github.
type nextBranchRig struct {
	t      *testing.T
	pool   *store.Pool
	runner *outbox
	github *nextBranchGitHub
}

func newNextBranchRig(t *testing.T, refs, compared map[string]string) *nextBranchRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	github := newNextBranchGitHub(t, refs, compared)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Admitted, Generation: 2, Status: "in_progress", Rank: "U"})
	return &nextBranchRig{t: t, pool: pool, github: github, runner: &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION",
		records: records, tokens: roleTokens{}, project: "legion", githubAPI: github.url, now: time.Now}}
}

// run executes one branch row of generation 2.
func (r *nextBranchRig) run(from *record.BranchFrom) {
	r.t.Helper()
	if err := r.runner.execute(context.Background(), mustOutboxRow(r.t, "LEGION-208", record.IssueBranch{Generation: 2, From: from}, time.Now())); err != nil {
		r.t.Fatalf("execute the branch row: %v", err)
	}
}

// told is the kind and the reason of every notice the rows queued for LEGION-208.
func (r *nextBranchRig) told() [][2]string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select payload->>'kind', payload->>'reason' from outbox where issue = $1 and kind = $2 order by id`,
		"LEGION-208", string(record.OutboxKindNotice))
	if err != nil {
		r.t.Fatalf("read the notices: %v", err)
	}
	defer rows.Close()
	var notices [][2]string
	for rows.Next() {
		var notice [2]string
		if err := rows.Scan(&notice[0], &notice[1]); err != nil {
			r.t.Fatalf("read a notice: %v", err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// mergedFrom is the branch row's start of a next change whose last pull request merged
// nextMergedHead into base.
func mergedFrom(base string) *record.BranchFrom {
	return &record.BranchFrom{Base: base, MergedHead: nextMergedHead}
}

// The branch of a next change that stands at the head its last pull request merged moves to the
// head of the base that pull request merged into, with one forced move, and nobody is told.
func TestABranchAtTheMergedHeadMovesToTheBase(t *testing.T) {
	t.Parallel()
	r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead}, nil)
	r.run(mergedFrom("main"))
	creates, moves, tip := r.github.writes()
	if len(creates) != 0 || len(moves) != 1 || moves[0]["sha"] != nextBaseHead || moves[0]["force"] != true || tip != nextBaseHead {
		t.Fatalf("creates %v and moves %v, the branch at %s; want one forced move to %s", creates, moves, tip, nextBaseHead)
	}
	if told := r.told(); len(told) != 0 {
		t.Fatalf("told %v, want nobody", told)
	}
}

// A next change whose branch GitHub no longer has gets it made at the head of the base its last
// pull request merged into, which need not be the default branch.
func TestAMissingBranchIsMadeAtTheBase(t *testing.T) {
	t.Parallel()
	r := newNextBranchRig(t, map[string]string{"main": nextHolding, "release/v7": nextBaseHead}, nil)
	r.run(mergedFrom("release/v7"))
	creates, moves, tip := r.github.writes()
	if len(creates) != 1 || creates[0]["ref"] != "refs/heads/"+nextBranch || creates[0]["sha"] != nextBaseHead || len(moves) != 0 || tip != nextBaseHead {
		t.Fatalf("creates %v and moves %v, the branch at %s; want one create at %s", creates, moves, tip, nextBaseHead)
	}
}

// A branch that holds commits after the merge is someone's work: the row leaves it where it
// stands, writes nothing, and tells the owning architect, naming the branch's tip, to merge the
// base forward before the next change's first task.
func TestABranchWithCommitsAfterTheMergeIsLeftAndTold(t *testing.T) {
	t.Parallel()
	r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: nextLater},
		map[string]string{nextBaseHead + "..." + nextLater: "diverged"})
	r.run(mergedFrom("main"))
	creates, moves, tip := r.github.writes()
	if len(creates) != 0 || len(moves) != 0 || tip != nextLater {
		t.Fatalf("creates %v and moves %v, the branch at %s; want it left at %s", creates, moves, tip, nextLater)
	}
	told := r.told()
	if len(told) != 1 || told[0][0] != "next-change-workspace" || !strings.Contains(told[0][1], nextLater) || !strings.Contains(told[0][1], nextBranch) {
		t.Fatalf("told %v, want one next-change-workspace notice naming %s at %s", told, nextBranch, nextLater)
	}
}

// A base GitHub no longer has is replaced by the issue's recorded default branch, and the branch
// moves to that branch's head.
func TestAGoneBaseFallsBackToTheDefaultBranch(t *testing.T) {
	t.Parallel()
	r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead}, nil)
	r.run(mergedFrom("release/v7"))
	if _, moves, tip := r.github.writes(); len(moves) != 1 || tip != nextBaseHead {
		t.Fatalf("moves %v, the branch at %s; want one move to main's head %s", moves, tip, nextBaseHead)
	}
}

// A row of a generation that is no next change carries no start, and creates the branch at the
// default branch, as every branch row did before, or leaves the one GitHub has.
func TestARowWithNoFromCreatesAsToday(t *testing.T) {
	t.Parallel()
	r := newNextBranchRig(t, map[string]string{"main": nextBaseHead}, nil)
	r.run(nil)
	creates, moves, _ := r.github.writes()
	if len(creates) != 1 || creates[0]["sha"] != nextBaseHead || len(moves) != 0 {
		t.Fatalf("creates %v and moves %v; want one create at the default branch and no move", creates, moves)
	}
	r.run(nil)
	if creates, moves, _ := r.github.writes(); len(creates) != 1 || len(moves) != 0 {
		t.Fatalf("creates %v and moves %v after a second row; want the branch GitHub has left as it is", creates, moves)
	}
}

// A second branch row of the same next change finds the branch already at the base's head, or at
// a commit that holds it, and leaves it as it stands: nothing is written and nobody is told.
func TestASecondRowOfTheGenerationChangesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		tip      string
		compared map[string]string
	}{
		{name: "at the base's head", tip: nextBaseHead},
		{name: "holding the base's head", tip: nextHolding, compared: map[string]string{nextBaseHead + "..." + nextHolding: "ahead"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: tc.tip}, tc.compared)
			r.run(mergedFrom("main"))
			if creates, moves, tip := r.github.writes(); len(creates) != 0 || len(moves) != 0 || tip != tc.tip {
				t.Fatalf("creates %v and moves %v, the branch at %s; want it left at %s", creates, moves, tip, tc.tip)
			}
			if told := r.told(); len(told) != 0 {
				t.Fatalf("told %v, want nobody", told)
			}
		})
	}

	t.Run("a row after the first moved it", func(t *testing.T) {
		t.Parallel()
		r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead}, nil)
		r.run(mergedFrom("main"))
		r.run(mergedFrom("main"))
		if _, moves, tip := r.github.writes(); len(moves) != 1 || tip != nextBaseHead {
			t.Fatalf("moves %v, the branch at %s; want the first row's one move to %s", moves, tip, nextBaseHead)
		}
		if told := r.told(); len(told) != 0 {
			t.Fatalf("told %v, want nobody", told)
		}
	})
}
