package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// scripted is a scripted answer of the GitHub stand-in (nextBranchGitHub.answer): the request of
// method at path is answered status with body, and every other request as the stand-in answers it.
func scripted(method, path string, status int, body string) func(*http.Request) (int, string, bool) {
	return func(r *http.Request) (int, string, bool) {
		if r.Method != method || r.URL.Path != path {
			return 0, "", false
		}
		return status, body, true
	}
}

// script makes answer the stand-in's scripted answer (nextBranchGitHub.answer), under the lock its
// handler reads it under; nil answers every request as the stand-in does.
func (g *nextBranchGitHub) script(answer func(*http.Request) (int, string, bool)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

// scriptedAnswers is how many requests the scripted answer answered.
func (g *nextBranchGitHub) scriptedAnswers() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.answered
}

// queue writes one branch row of generation 2 into the outbox, due now, and answers its id.
func (r *nextBranchRig) queue(from *record.BranchFrom) int64 {
	r.t.Helper()
	row := mustOutboxRow(r.t, "LEGION-208", record.IssueBranch{Generation: 2, From: from}, time.Now())
	var id int64
	if err := store.TransactWith(context.Background(), r.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = r.runner.records.EnqueueID(ctx, tx, row)
		return err
	}); err != nil {
		r.t.Fatalf("queue the branch row: %v", err)
	}
	return id
}

// drain runs branch row id once as a lane runs it: claimed, run, and settled under its lease
// (runRow), with runner, the rig's own outbox unless a test brings another.
func (r *nextBranchRig) drain(runner *outbox, id int64) {
	r.t.Helper()
	if err := runner.runRow(context.Background(), record.OutboxRow{ID: id, Kind: record.OutboxKindIssueBranch, Issue: "LEGION-208"}); err != nil {
		r.t.Fatalf("run branch row %d: %v", id, err)
	}
}

// branchRow is branch row id as the outbox holds it, and whether it holds it: its attempts and the
// error it last met.
func (r *nextBranchRig) branchRow(id int64) (attempts int, lastError string, held bool) {
	r.t.Helper()
	err := r.pool.QueryRow(context.Background(), `select attempts, coalesce(last_error, '') from outbox where id = $1`, id).Scan(&attempts, &lastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false
	}
	if err != nil {
		r.t.Fatalf("read branch row %d: %v", id, err)
	}
	return attempts, lastError, true
}

// noticesTo is the kind and the role of every notice the rows queued for LEGION-208.
func (r *nextBranchRig) noticesTo() [][2]string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select payload->>'kind', coalesce(payload->>'role', '') from outbox where issue = $1 and kind = $2 order by id`,
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

// refusedInState is LEGION-208's branchRefused as the state route builds it (projection.Project),
// read in a snapshot of its own.
func (r *nextBranchRig) refusedInState() *api.BranchRefusedView {
	r.t.Helper()
	var state api.State
	if err := store.TransactWith(context.Background(), r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			state, err = projection.Project(ctx, projection.Inputs{Tx: tx, Store: r.runner.records, Project: "LEGION", Now: time.Now().UTC()})
			return err
		}); err != nil {
		r.t.Fatalf("build the state: %v", err)
	}
	issue, ok := state.Issues["LEGION-208"]
	if !ok {
		r.t.Fatalf("the state holds no LEGION-208: %+v", state.Issues)
	}
	return issue.BranchRefused
}

// wantRefusedOnce fails the test unless branch row id is gone from the outbox, the issue's
// architect holds exactly one branch-refused notice, and the state names row id with write and
// kind and a reason that carries carries.
func (r *nextBranchRig) wantRefusedOnce(id int64, write string, kind ghbranch.RefusalKind, carries string) {
	r.t.Helper()
	if attempts, lastError, held := r.branchRow(id); held {
		r.t.Fatalf("branch row %d is still queued after %d attempts (last error %q), want it ended as refused for good", id, attempts, lastError)
	}
	if told := r.noticesTo(); len(told) != 1 || told[0] != [2]string{string(record.BranchRefusedNotice), string(claim.RoleArchitect)} {
		r.t.Fatalf("the notices queued are %v, want one %s notice to the architect", told, record.BranchRefusedNotice)
	}
	refused := r.refusedInState()
	if refused == nil {
		r.t.Fatal("the state names no branchRefused of LEGION-208, want the refused row")
	}
	if refused.Row != id || refused.Write != write || refused.Kind != string(kind) || refused.At.IsZero() || !strings.Contains(refused.Reason, carries) {
		r.t.Fatalf("the state's branchRefused = %+v, want row %d, write %s, kind %s and a reason carrying %q", refused, id, write, kind, carries)
	}
}

// A branch row GitHub refuses in a way a retry meets again ends at its first attempt: it is
// finished, not retried, the owning architect is told once with a branch-refused notice, and the
// state names the row, what it wrote, the refusal's kind and GitHub's answer. The cases are the
// refusals the create and the move meet that a retry meets again: an App the GraphQL endpoint
// forbids, a GitHub with no GraphQL endpoint, a repository read that names no node id, updateRefs
// refused as forbidden or as not found, a create GitHub forbids or cannot process, a repository
// with no commit, and a move with neither its base nor the default branch on GitHub.
func TestABranchRowGitHubRefusesForGoodEndsAndTellsItsArchitect(t *testing.T) {
	t.Parallel()
	atMerge := func() map[string]string { return map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead} }
	for _, tc := range []struct {
		name    string
		refs    map[string]string
		from    *record.BranchFrom
		answer  func(*http.Request) (int, string, bool)
		write   string
		kind    ghbranch.RefusalKind
		carries string
	}{
		{name: "an App the GraphQL endpoint forbids", refs: atMerge(), from: mergedFrom("main"), write: record.BranchWriteMove, kind: ghbranch.RefusedGraphQLForbidden,
			answer: scripted(http.MethodPost, "/graphql", http.StatusForbidden, `{"message":"no-graphql-7f3a"}`), carries: "no-graphql-7f3a"},
		{name: "a GitHub with no GraphQL endpoint", refs: atMerge(), from: mergedFrom("main"), write: record.BranchWriteMove, kind: ghbranch.RefusedGraphQLNotFound,
			answer: scripted(http.MethodPost, "/graphql", http.StatusNotFound, `{"message":"no-endpoint-2c9d"}`), carries: "no-endpoint-2c9d"},
		{name: "a repository read that names no node id", refs: atMerge(), from: mergedFrom("main"), write: record.BranchWriteMove, kind: ghbranch.RefusedNoNodeID,
			answer: scripted(http.MethodGet, "/repos/acme/widgets", http.StatusOK, `{"full_name":"acme/widgets","default_branch":"main"}`), carries: "acme/widgets"},
		{name: "updateRefs refused as forbidden", refs: atMerge(), from: mergedFrom("main"), write: record.BranchWriteMove, kind: ghbranch.RefusedGraphQLForbidden,
			answer: scripted(http.MethodPost, "/graphql", http.StatusOK, `{"data":{"updateRefs":null},"errors":[{"type":"FORBIDDEN","message":"forbidden-5e1b"}]}`), carries: "forbidden-5e1b"},
		{name: "updateRefs refused as not found", refs: atMerge(), from: mergedFrom("main"), write: record.BranchWriteMove, kind: ghbranch.RefusedGraphQLNotFound,
			answer: scripted(http.MethodPost, "/graphql", http.StatusOK, `{"data":{"updateRefs":null},"errors":[{"type":"NOT_FOUND","message":"no-node-8a40"}]}`), carries: "no-node-8a40"},
		{name: "a create GitHub forbids", refs: map[string]string{"main": nextBaseHead}, write: record.BranchWriteCreate, kind: ghbranch.RefusedForbidden,
			answer: scripted(http.MethodPost, "/repos/acme/widgets/git/refs", http.StatusForbidden, `{"message":"create-forbidden-31c2","status":"403"}`), carries: "create-forbidden-31c2"},
		{name: "a create GitHub cannot process", refs: map[string]string{"main": nextBaseHead}, write: record.BranchWriteCreate, kind: ghbranch.RefusedUnprocessable,
			answer: scripted(http.MethodPost, "/repos/acme/widgets/git/refs", http.StatusUnprocessableEntity, `{"message":"bad-ref-90fe","status":"422"}`), carries: "bad-ref-90fe"},
		{name: "a repository with no commit", refs: map[string]string{"main": nextBaseHead}, write: record.BranchWriteCreate, kind: ghbranch.RefusedConflict,
			answer: scripted(http.MethodGet, "/repos/acme/widgets/git/ref/heads/main", http.StatusConflict, `{"message":"empty-6d07","status":"409"}`), carries: "empty-6d07"},
		{name: "a move with neither its base nor the default branch", refs: map[string]string{nextBranch: nextMergedHead}, from: mergedFrom("release/v7"),
			write: record.BranchWriteMove, kind: ghbranch.RefusedNoBase, carries: "refs/heads/main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newNextBranchRig(t, tc.refs, nil)
			r.github.script(tc.answer)
			id := r.queue(tc.from)
			r.drain(r.runner, id)
			r.wantRefusedOnce(id, tc.write, tc.kind, tc.carries)
			if creates, moves, _ := r.github.writes(); len(creates) != 0 || len(moves) != 0 {
				t.Fatalf("creates %v and moves %v, want none: GitHub refused the write", creates, moves)
			}
			if met := r.github.scriptedAnswers(); tc.answer != nil && met != 1 {
				t.Fatalf("GitHub answered the refused request %d times, want once", met)
			}
		})
	}
}

// A branch row whose failure a retry may pass keeps retrying on its backoff and tells nobody: the
// row stays queued with the failure as its last error, no notice is queued, and the state names
// no refusal. The cases are GitHub's trouble (a 502 of the GraphQL endpoint, the secondary limit's
// 403, a 429), a token GitHub did not take (401), an updateRefs error of no type while the branch
// still stands at the merged head, a token the daemon could not mint, and a GitHub that answered
// nothing at all.
func TestABranchRowARetryMayPassRetriesAndTellsNobody(t *testing.T) {
	t.Parallel()
	atMerge := func() map[string]string { return map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead} }
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for _, tc := range []struct {
		name   string
		refs   map[string]string
		from   *record.BranchFrom
		answer func(*http.Request) (int, string, bool)
		rig    func(*nextBranchRig)
		cause  string
	}{
		{name: "a 502 of the GraphQL endpoint", refs: atMerge(), from: mergedFrom("main"), cause: "with 502",
			answer: scripted(http.MethodPost, "/graphql", http.StatusBadGateway, `{"message":"Server Error"}`)},
		{name: "the secondary limit's 403 of the create", refs: map[string]string{"main": nextBaseHead}, cause: "secondary rate limit",
			answer: scripted(http.MethodPost, "/repos/acme/widgets/git/refs", http.StatusForbidden,
				`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)},
		{name: "a 429 of the base read", refs: atMerge(), from: mergedFrom("main"), cause: "with 429",
			answer: scripted(http.MethodGet, "/repos/acme/widgets/git/ref/heads/main", http.StatusTooManyRequests, `{"message":"Too Many Requests"}`)},
		{name: "a 401 of the repository read", refs: atMerge(), from: mergedFrom("main"), cause: "with 401",
			answer: scripted(http.MethodGet, "/repos/acme/widgets", http.StatusUnauthorized, `{"message":"Bad credentials"}`)},
		{name: "an updateRefs error of no type", refs: atMerge(), from: mergedFrom("main"), cause: "untyped-4b2e",
			answer: scripted(http.MethodPost, "/graphql", http.StatusOK, `{"data":{"updateRefs":null},"errors":[{"message":"untyped-4b2e"}]}`)},
		{name: "a token the daemon could not mint", refs: atMerge(), from: mergedFrom("main"), cause: "mint the implement App token",
			rig: func(r *nextBranchRig) { r.runner.tokens = refusingOwnerTokens{} }},
		{name: "a GitHub that answered nothing", refs: map[string]string{"main": nextBaseHead}, cause: "connection refused",
			rig: func(r *nextBranchRig) { r.runner.githubAPI = closed.URL }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newNextBranchRig(t, tc.refs, nil)
			r.github.script(tc.answer)
			if tc.rig != nil {
				tc.rig(r)
			}
			id := r.queue(tc.from)
			r.drain(r.runner, id)
			attempts, lastError, held := r.branchRow(id)
			if !held || attempts != 1 || !strings.Contains(lastError, tc.cause) {
				t.Fatalf("branch row %d held %v with %d attempts and last error %q, want it queued again after 1 attempt naming %q",
					id, held, attempts, lastError, tc.cause)
			}
			if told := r.noticesTo(); len(told) != 0 {
				t.Fatalf("the notices queued are %v, want none: a retry may pass", told)
			}
			if refused := r.refusedInState(); refused != nil {
				t.Fatalf("the state's branchRefused = %+v, want none: a retry may pass", refused)
			}
		})
	}
}

// A branch refused for good is told to the issue's architect once for its generation, however
// many rows of that generation meet the refusal and whichever daemon runs them: a second row is
// finished refused and tells nobody, and so does a third, run by an outbox built afresh over the
// same database, as a restarted daemon's is. The state keeps naming the first row. A later row
// whose write GitHub takes clears the refusal from the state.
func TestARefusedBranchIsToldOnceAcrossRowsAndRestarts(t *testing.T) {
	t.Parallel()
	r := newNextBranchRig(t, map[string]string{"main": nextBaseHead, nextBranch: nextMergedHead}, nil)
	r.github.script(scripted(http.MethodPost, "/graphql", http.StatusForbidden, `{"message":"no-graphql-c4d1"}`))
	first := r.queue(mergedFrom("main"))
	r.drain(r.runner, first)
	r.wantRefusedOnce(first, record.BranchWriteMove, ghbranch.RefusedGraphQLForbidden, "no-graphql-c4d1")

	second := r.queue(mergedFrom("main"))
	r.drain(r.runner, second)
	if _, _, held := r.branchRow(second); held {
		t.Fatalf("the second branch row %d is still queued, want it ended as refused for good", second)
	}
	r.wantRefusedOnce(first, record.BranchWriteMove, ghbranch.RefusedGraphQLForbidden, "no-graphql-c4d1")

	restarted := &outbox{log: quietLogger(), pool: r.pool, dispatchProject: "LEGION", records: record.NewStore(), tokens: roleTokens{}, project: "legion",
		githubAPI: r.github.url, now: time.Now}
	third := r.queue(mergedFrom("main"))
	r.drain(restarted, third)
	if _, _, held := r.branchRow(third); held {
		t.Fatalf("the third branch row %d, run after the restart, is still queued, want it ended as refused for good", third)
	}
	r.wantRefusedOnce(first, record.BranchWriteMove, ghbranch.RefusedGraphQLForbidden, "no-graphql-c4d1")

	r.github.script(nil)
	fourth := r.queue(mergedFrom("main"))
	r.drain(restarted, fourth)
	if _, moves, tip := r.github.writes(); len(moves) != 1 || tip != nextBaseHead {
		t.Fatalf("moves %v, the branch at %s; want the fourth row's one move to %s", moves, tip, nextBaseHead)
	}
	if refused := r.refusedInState(); refused != nil {
		t.Fatalf("the state's branchRefused after the branch moved = %+v, want none", refused)
	}
	if told := r.noticesTo(); len(told) != 1 {
		t.Fatalf("the notices queued are %v, want the one branch-refused notice alone", told)
	}
}
