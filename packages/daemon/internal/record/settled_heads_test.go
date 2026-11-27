package record

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// settledHeadsAt is the clock the settled heads below were read at.
var settledHeadsAt = time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

// settledEntry is head's fence as one settlement of ci, run id, left it: the run and a status of
// deploy stamped with head.
func settledEntry(head string, id int64) Settlement {
	return Settlement{Head: head, CheckRuns: []AttemptRun{{Name: "ci", ID: id, Head: head, Conclusion: RunSucceeded, GitHubConclusion: "success"}},
		Failing: []string{}, Generation: id, Snapshot: head + "-snapshot",
		Statuses: []CommitStatus{{Context: "deploy", State: CommitStatusSuccess, CreatedAt: settledHeadsAt, Head: head}}}
}

// fencedPullRequest is key's pull request open on main at h3, remembering the fences of h1, h2
// and h3, with a head run open from h2 whose anchor remembered those of h1 and h2.
func fencedPullRequest(key string) PullRequest {
	return PullRequest{Issue: key, Repo: "acme/widgets", Number: 7, Branch: "legion/" + key, Base: "main", HeadSHA: "h3",
		HeadUpdatedAt: settledHeadsAt, Failing: []string{}, Cancelled: []string{}, State: PullRequestOpen,
		SettledHeads: []Settlement{settledEntry("h1", 1), settledEntry("h2", 2), settledEntry("h3", 3)},
		Run: &HeadRun{Anchor: Judgement{HeadSHA: "h2", Failing: []string{}, Cancelled: []string{}}, Steps: []RunStep{{Head: "h3", Before: "h2"}},
			Settled: []Settlement{settledEntry("h1", 1), settledEntry("h2", 2)}}}
}

// settledHeadRows is each row pull_request_settled_heads holds for issue's pull requests, as its
// kind, position, head and fence text, by kind and position.
func settledHeadRows(t *testing.T, tx pgx.Tx, issue string) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), `select h.kind || ' ' || h.position || ' ' || h.head || ' ' || h.fence::text
		from pull_request_settled_heads h join pull_requests pr on pr.repo = h.repo and pr.number = h.number
		where pr.issue = $1 order by h.kind, h.position`, issue)
	must(t, err)
	defer rows.Close()
	var held []string
	for rows.Next() {
		var row string
		must(t, rows.Scan(&row))
		held = append(held, row)
	}
	must(t, rows.Err())
	return held
}

// The heads the mixed entries below are of, as long as GitHub's: rowHead is the head each entry
// is remembered for, otherHead another the fence holds a run and a status of.
const (
	rowHead   = "c8fecac2897fa42ffe5f232242508914ec422b0f"
	otherHead = "4859c45994ab7d1e0c3b2a1f0e9d8c7b6a594837"
)

// mixedEntry is rowHead's fence holding a run and a status of rowHead, a run and a status of
// otherHead (classify.HeadRuns reads a run's own head, so a fence can hold one of another head),
// and a run with no head, as a row written before the runs carried one holds it.
func mixedEntry() Settlement {
	return Settlement{Head: rowHead, CheckRuns: []AttemptRun{
		{Name: "ci", ID: 11, Head: rowHead, Conclusion: RunSucceeded, GitHubConclusion: "success"},
		{Name: "deploy", ID: 12, Head: otherHead, Conclusion: RunFailed},
		{Name: "lint", ID: 9}},
		Failing: []string{"deploy"}, Cancelled: []string{}, Generation: 4, Snapshot: "snapshot-4",
		Statuses: []CommitStatus{{Context: "license", State: CommitStatusError, CreatedAt: settledHeadsAt, Head: rowHead},
			{Context: "preview", State: CommitStatusSuccess, CreatedAt: settledHeadsAt.Add(time.Minute), Head: otherHead}}}
}

// mixedPullRequest is fencedPullRequest remembering mixedEntry among its own heads and among its
// head run's, beside an entry whose lists are empty and one whose lists are nil but its runs.
func mixedPullRequest(key string) PullRequest {
	pr := fencedPullRequest(key)
	pr.SettledHeads = []Settlement{settledEntry("h1", 1), mixedEntry(), {Head: "h2", CheckRuns: []AttemptRun{}}}
	pr.Run.Settled = []Settlement{mixedEntry(),
		{Head: "h2", CheckRuns: []AttemptRun{}, Failing: []string{}, Cancelled: []string{}, Statuses: []CommitStatus{}}}
	return pr
}

// What a pull request remembers of its settled heads, its own and its open head run's, reads back
// through each whole read of one pull request exactly as it was put: each run and status with the
// head it was put with - the entry's own, another, or none - and each list nil or empty as it was.
// The issue has no plan rows, so its one pull request is of the task "" and is its merge's.
func TestBothKindsOfSettledHeadReadBackAsTheyWerePut(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-208")
	pr := mixedPullRequest(issue.Key)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, pr))
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, read := range []struct {
			name string
			read func() (*PullRequest, error)
		}{
			{"PullRequestOfTask", func() (*PullRequest, error) { return records.PullRequestOfTask(ctx, tx, issue.Key, "") }},
			{"MergePullRequest", func() (*PullRequest, error) { return records.MergePullRequest(ctx, tx, issue.Key) }},
			{"PullRequestByNumber", func() (*PullRequest, error) { return records.PullRequestByNumber(ctx, tx, pr.Repo, pr.Number) }},
			{"OpenPullRequestsOnBranch", func() (*PullRequest, error) {
				prs, err := records.OpenPullRequestsOnBranch(ctx, tx, pr.Repo, pr.Branch)
				if len(prs) != 1 {
					return nil, err
				}
				return &prs[0], err
			}},
		} {
			got, err := read.read()
			must(t, err)
			if got == nil || !reflect.DeepEqual(got.SettledHeads, pr.SettledHeads) {
				t.Errorf("%s reads the pull request remembering %+v, want %+v", read.name, got, pr.SettledHeads)
				continue
			}
			if got.Run == nil || !reflect.DeepEqual(got.Run.Settled, pr.Run.Settled) {
				t.Errorf("%s reads the head run %+v, want it remembering %+v", read.name, got.Run, pr.Run.Settled)
			}
		}
	})
}

// The fence a row keeps names no head of its own row's: a run or a status of the row's head is
// kept with no head, which the row's head column names once, while one of another head keeps that
// head and a run with no head keeps its empty one. Read as the database holds it, the row's head
// is nowhere in its fence.
func TestAStoredFenceNamesNotTheHeadOfItsRow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-208")
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, mixedPullRequest(issue.Key)))
	})
	rows, err := st.Pool().Query(ctx, `select h.kind, h.fence::text from pull_request_settled_heads h
		join pull_requests pr on pr.repo = h.repo and pr.number = h.number where pr.issue = $1 and h.head = $2 order by h.kind`,
		issue.Key, rowHead)
	must(t, err)
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind, fence string
		must(t, rows.Scan(&kind, &fence))
		kinds = append(kinds, kind)
		if strings.Contains(fence, rowHead) {
			t.Errorf("the %s fence of %s names that head: %s", kind, rowHead, fence)
		}
		var stored struct {
			CheckRuns []map[string]any `json:"checkRuns"`
			Statuses  []map[string]any `json:"statuses"`
		}
		must(t, json.Unmarshal([]byte(fence), &stored))
		heads := map[string]any{}
		for _, run := range stored.CheckRuns {
			heads["run "+run["name"].(string)] = run["head"]
		}
		for _, status := range stored.Statuses {
			heads["status "+status["context"].(string)] = status["head"]
		}
		want := map[string]any{"run ci": nil, "run deploy": otherHead, "run lint": "", "status license": nil, "status preview": otherHead}
		if !reflect.DeepEqual(heads, want) {
			t.Errorf("the %s fence of %s keeps the heads %v, want %v (nil: none kept): %s", kind, rowHead, heads, want, fence)
		}
	}
	must(t, rows.Err())
	if want := []string{"head_run", "pull_request"}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("the store holds the kinds %v of %s, want %v", kinds, rowHead, want)
	}
}

// Only the whole reads of one pull request load what it remembers of its settled heads
// (PullRequestByNumber, OpenPullRequestsOnBranch, and TaskPullRequests with the reads made of it);
// a list read, and a read of the rows alone by their number or their branch, loads none of it. So
// a record such a read made is refused by PutPullRequest, with an error that names the read, and
// the memory the store holds is left as it was: written, it would have deleted every remembered
// head.
func TestAListedPullRequestIsRefusedByPutPullRequest(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-208")
	pr := fencedPullRequest(issue.Key)
	pr.Repo = issue.Repo
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, pr))
	})
	for _, list := range []struct {
		read string
		list func(tx pgx.Tx) ([]PullRequest, error)
	}{
		{"OpenPullRequests", func(tx pgx.Tx) ([]PullRequest, error) { return records.OpenPullRequests(ctx, tx, issue.Project) }},
		{"OpenPullRequestsOnBase", func(tx pgx.Tx) ([]PullRequest, error) {
			return records.OpenPullRequestsOnBase(ctx, tx, pr.Repo, pr.Base)
		}},
		{"PullRequestsOf", func(tx pgx.Tx) ([]PullRequest, error) {
			of, err := records.PullRequestsOf(ctx, tx, []string{issue.Key})
			return []PullRequest{of[issue.Key]}, err
		}},
		{"TaskPullRequestRowsOf", func(tx pgx.Tx) ([]PullRequest, error) {
			of, err := records.TaskPullRequestRowsOf(ctx, tx, []string{issue.Key})
			return of[issue.Key], err
		}},
		{"PullRequestRowByNumber", func(tx pgx.Tx) ([]PullRequest, error) {
			row, err := records.PullRequestRowByNumber(ctx, tx, strings.ToUpper(pr.Repo), pr.Number)
			return rowOf(row), err
		}},
		{"OpenPullRequestRowsOnBranch", func(tx pgx.Tx) ([]PullRequest, error) {
			return records.OpenPullRequestRowsOnBranch(ctx, tx, strings.ToUpper(pr.Repo), pr.Branch)
		}},
	} {
		inTx(t, st, func(tx pgx.Tx) {
			before, err := records.PullRequestOfTask(ctx, tx, issue.Key, "")
			must(t, err)
			listed, err := list.list(tx)
			must(t, err)
			if len(listed) != 1 || listed[0].Issue != issue.Key {
				t.Fatalf("%s lists %+v, want %s's pull request alone", list.read, listed, issue.Key)
			}
			if err := records.PutPullRequest(ctx, tx, listed[0]); err == nil || !strings.Contains(err.Error(), list.read) {
				t.Errorf("PutPullRequest of the record %s made = %v, want it refused naming %s", list.read, err, list.read)
			}
			after, err := records.PullRequestOfTask(ctx, tx, issue.Key, "")
			must(t, err)
			if !reflect.DeepEqual(after.SettledHeads, before.SettledHeads) || !reflect.DeepEqual(after.Run, before.Run) {
				t.Errorf("after the write of the record %s made the memory reads %+v and the run %+v, want %+v and %+v as before",
					list.read, after.SettledHeads, after.Run, before.SettledHeads, before.Run)
			}
		})
	}
	inTx(t, st, func(tx pgx.Tx) {
		if held := settledHeadRows(t, tx, issue.Key); len(held) != 5 {
			t.Errorf("after the refused writes the store holds the remembered heads %v, want the five written", held)
		}
	})
}

// rowOf is the record a read of one row answered as a list of it, none for no row.
func rowOf(pr *PullRequest) []PullRequest {
	if pr == nil {
		return nil
	}
	return []PullRequest{*pr}
}

// A write keeps exactly what the record remembers: an entry it no longer holds is deleted, the
// entries it keeps take their new place, and a record with no head run open leaves no entry of a
// run. A write of the same record again writes no new version of any row.
func TestAWriteDeletesTheSettledHeadsTheRecordNoLongerHolds(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-208")
	pr := fencedPullRequest(issue.Key)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, pr))
	})
	kept := pr
	kept.SettledHeads, kept.Run = pr.SettledHeads[1:], nil
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutPullRequest(ctx, tx, kept)) })
	inTx(t, st, func(tx pgx.Tx) {
		held := settledHeadRows(t, tx, issue.Key)
		var heads []string
		for _, row := range held {
			fields := strings.Fields(row)
			heads = append(heads, strings.Join(fields[:3], " "))
		}
		if want := []string{"pull_request 0 h2", "pull_request 1 h3"}; !reflect.DeepEqual(heads, want) {
			t.Errorf("after a write of h2 and h3 with no head run the store holds %v, want %v", held, want)
		}
		got, err := records.PullRequestOfTask(ctx, tx, issue.Key, "")
		must(t, err)
		if !reflect.DeepEqual(got.SettledHeads, kept.SettledHeads) || got.Run != nil {
			t.Errorf("the pull request remembers %+v with the run %+v, want %+v and none", got.SettledHeads, got.Run, kept.SettledHeads)
		}
	})
	versions := func() string {
		var xmins string
		must(t, st.Pool().QueryRow(ctx, `select string_agg(h.head || ':' || h.xmin::text, ' ' order by h.kind, h.position)
			from pull_request_settled_heads h join pull_requests pr on pr.repo = h.repo and pr.number = h.number
			where pr.issue = $1`, issue.Key).Scan(&xmins))
		return xmins
	}
	written := versions()
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutPullRequest(ctx, tx, kept)) })
	if again := versions(); again != written {
		t.Errorf("a write of the same record again leaves the row versions %s, want %s as they were", again, written)
	}
}

// Each pull request of an issue keeps its own memory of its settled heads, keyed by its repository
// and its number (migration 20261127031536): the pull requests of two tasks of one issue, on two
// branches, each read back their own, and a write of one that drops a head leaves the other's
// whole. Keyed by the issue alone, the second write would delete the first's memory.
func TestEachPullRequestOfAnIssueKeepsItsOwnSettledHeads(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-208")
	first := fencedPullRequest(issue.Key)
	second := fencedPullRequest(issue.Key)
	second.Number, second.Branch, second.HeadSHA, second.Run = 8, "legion/LEGION-208.y", "h5", nil
	second.SettledHeads = []Settlement{settledEntry("h4", 4), settledEntry("h5", 5)}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, first))
		must(t, records.PutPullRequest(ctx, tx, second))
	})
	read := func(tx pgx.Tx, pr PullRequest) *PullRequest {
		t.Helper()
		got, err := records.PullRequestByNumber(ctx, tx, pr.Repo, pr.Number)
		must(t, err)
		if got == nil {
			t.Fatalf("the store holds no pull request of %s on %s", pr.Issue, pr.Branch)
		}
		return got
	}
	holdsFirst := func(tx pgx.Tx) {
		t.Helper()
		got := read(tx, first)
		if !reflect.DeepEqual(got.SettledHeads, first.SettledHeads) || got.Run == nil || !reflect.DeepEqual(got.Run.Settled, first.Run.Settled) {
			t.Errorf("the pull request on %s remembers %+v with the run %+v, want its own %+v and %+v", first.Branch, got.SettledHeads, got.Run,
				first.SettledHeads, first.Run.Settled)
		}
	}
	inTx(t, st, func(tx pgx.Tx) {
		holdsFirst(tx)
		if got := read(tx, second); !reflect.DeepEqual(got.SettledHeads, second.SettledHeads) || got.Run != nil {
			t.Errorf("the pull request on %s remembers %+v with the run %+v, want its own %+v and none", second.Branch, got.SettledHeads, got.Run,
				second.SettledHeads)
		}
	})
	kept := second
	kept.SettledHeads = second.SettledHeads[1:]
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutPullRequest(ctx, tx, kept)) })
	inTx(t, st, func(tx pgx.Tx) {
		holdsFirst(tx)
		if got := read(tx, kept); !reflect.DeepEqual(got.SettledHeads, kept.SettledHeads) {
			t.Errorf("after its write the pull request on %s remembers %+v, want %+v", kept.Branch, got.SettledHeads, kept.SettledHeads)
		}
		if held := settledHeadRows(t, tx, issue.Key); len(held) != 6 {
			t.Errorf("the store holds the remembered heads %v of %s, want the five of %s and the one of %s", held, issue.Key, first.Branch, kept.Branch)
		}
	})
}
