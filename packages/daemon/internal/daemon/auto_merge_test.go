package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghautomerge"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// An auto_merge_disable row decides by the record when it runs, as the implement App: while the
// issue awaits its merge and the pull request's head on GitHub is the merger's READY commit, the
// arm is that READY's and is kept; otherwise auto-merge is disabled. A row queued long before, by
// a withdrawal the issue has since come back from, so never undoes the READY that followed it. A
// READY a person merges (its by-hand note) covers no arm at all, so whoever armed it is undone, and
// neither does one in a tree that lingers after its root left the workflow, which beginLinger
// disarms.
func TestAnAutoMergeDisableRowKeepsOnlyTheAwaitedREADYHeadsArm(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name string
		seed readyMergeSeed
		want int
	}{
		{"awaiting its merge at the READY head", awaitingREADY, 0},
		{"awaiting its merge, the head past READY", awaitingREADY.with(func(s *readyMergeSeed) { s.ready = "older-head" }), 1},
		{"back in implementing", awaitingREADY.with(func(s *readyMergeSeed) { s.at = phase.Implementing }), 1},
		{"awaiting a person's merge at the READY head", awaitingREADY.with(func(s *readyMergeSeed) { s.mergeNote = byHand }), 1},
		{"awaiting its merge at the READY head in a lingering tree", awaitingREADY.with(func(s *readyMergeSeed) { s.lingers = true }), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bearers []string
			disables := 0
			api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path != "/graphql" {
					http.NotFound(w, r)
					return
				}
				bearers = append(bearers, r.Header.Get("Authorization"))
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "disablePullRequestAutoMerge") {
					disables++
					_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
					return
				}
				_, _ = w.Write([]byte(armedPullJSON("ready-head", disables == 0)))
			}})
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", tc.seed)
			runner := mergeRunner(pool, records, api, quietLogger(), time.Now)
			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())); err != nil {
				t.Fatalf("execute the disarm: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if disables != tc.want {
				t.Fatalf("auto-merge disables %d, want %d", disables, tc.want)
			}
			for _, bearer := range bearers {
				if bearer != "Bearer implement-app-token-for-acme" {
					t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
				}
			}
		})
	}
}

// The row reads GitHub before the record. The daemon arms only after the READY commits, so an arm
// the row's GitHub read finds is already covered by the record it reads next. Read the other way
// round, a READY committing and its merge being armed between the two reads would leave the row
// keeping nothing and disabling that legitimate arm. Here the READY commits while GitHub answers
// the row's read, and the arm it reports is kept.
func TestAnAutoMergeDisableRowReadsGitHubBeforeTheRecord(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Merging,
		Generation: 1, Status: "retro", Rank: "U"})
	var mu sync.Mutex
	disables := 0
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "disablePullRequestAutoMerge") {
			disables++
			_, _ = w.Write([]byte(`{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`))
			return
		}
		// The READY commits, and its merge is armed, as GitHub answers this read.
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), "update issues set phase = 'awaiting_merge' where key = 'LEGION-208'"); err != nil {
				return err
			}
			return records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head"})
		}); err != nil {
			t.Errorf("commit the READY: %v", err)
		}
		_, _ = w.Write([]byte(armedPullJSON("ready-head", true)))
	}})
	runner := mergeRunner(pool, records, api, quietLogger(), time.Now)
	if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, time.Now())); err != nil {
		t.Fatalf("execute the disarm: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if disables != 0 {
		t.Fatalf("auto-merge disables %d, want the READY's arm kept", disables)
	}
}

// A ready_merge row submits the merge as the implement App only while the record still awaits that
// exact READY head on an open pull request. A READY a person merges, a READY the issue has left, a
// tree that lingers, and a pull request the record holds closed are stale; GitHub's expected-head
// check guards the head between the row's read and its mutation.
func TestAREADYMergeRowSubmitsOnlyTheAwaitedREADYHead(t *testing.T) {
	const byHand = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	for _, tc := range []struct {
		name   string
		seed   readyMergeSeed
		merges int
	}{
		{"awaiting its merge at the READY head", awaitingREADY, 1},
		{"awaiting another READY head", awaitingREADY.with(func(s *readyMergeSeed) { s.ready = "older-head" }), 0},
		{"back in implementing", awaitingREADY.with(func(s *readyMergeSeed) { s.at = phase.Implementing }), 0},
		{"awaiting a person's merge", awaitingREADY.with(func(s *readyMergeSeed) { s.mergeNote = byHand }), 0},
		{"awaiting its merge in a lingering tree", awaitingREADY.with(func(s *readyMergeSeed) { s.lingers = true }), 0},
		{"awaiting its merge on a pull request the record holds closed", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, mergedAnswer)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", tc.seed)
			runner := mergeRunner(pool, records, github.url, quietLogger(), time.Now)
			if err := runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())); err != nil {
				t.Fatalf("execute the READY merge: %v", err)
			}
			if merges, bearers := github.merged(); merges != tc.merges {
				t.Fatalf("merges %d, want %d", merges, tc.merges)
			} else {
				for _, bearer := range bearers {
					if bearer != "Bearer implement-app-token-for-acme" {
						t.Fatalf("a GitHub call carried %q, want the implement App's token", bearer)
					}
				}
			}
		})
	}
}

// The record can trust a head GitHub has already left: a late push whose move the record made
// before is spent, so a branch re-pushed to the same next commit is not seen on its way until its
// synchronize arrives (classify.unspentPushes). No merge lands that commit meanwhile: the READY merge
// row submits only READY's head, read from GitHub first (ghautomerge.Submit), and every mutation
// carries it as GitHub's expected head. A read that shows GitHub's head re-pushed submits nothing;
// a re-push between the read and the mutation is refused by GitHub ("Head branch was modified").
// Either way the issue stays in awaiting_merge and no merge is recorded.
func TestAREADYMergeNeverLandsAHeadReplacedAfterTheREADY(t *testing.T) {
	for _, tc := range []struct {
		name   string
		read   func(int) string
		merges int
	}{
		{"GitHub's read shows the head re-pushed", func(int) string { return pullJSON("OPEN", "re-pushed", "CLEAN") }, 0},
		{"the head is re-pushed between the read and the merge", func(n int) string {
			if n == 1 {
				return pullJSON("OPEN", "ready-head", "CLEAN")
			}
			return pullJSON("OPEN", "re-pushed", "CLEAN")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, tc.read, refusedAnswer("Head branch was modified. Review and try the merge again."))
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			runner := mergeRunner(pool, records, github.url, quietLogger(), time.Now)
			_ = runner.execute(context.Background(), mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now()))
			if merges, _ := github.merged(); merges != tc.merges {
				t.Fatalf("merge mutations %d, want %d", merges, tc.merges)
			}
			var at string
			var state record.PullRequestState
			if err := pool.QueryRow(context.Background(), `select i.phase, p.state from issues i join pull_requests p on p.issue = i.key where i.key = 'LEGION-208'`).Scan(&at, &state); err != nil {
				t.Fatal(err)
			}
			if at != string(phase.AwaitingMerge) || state == record.PullRequestMerged {
				t.Fatalf("after the READY merge row the issue is in %s and its pull request %s, want awaiting_merge and not merged", at, state)
			}
		})
	}
}

// GitHub refusing a ready_merge row's submission where a retry would meet the same refusal (squash
// merges disabled, a permission the App lacks) fails loudly: the issue's architect is told once,
// with GitHub's message, and the row ends rather than retrying forever. A transient failure
// (GitHub's server error) tells nobody and keeps the row on its backoff.
func TestAPermanentREADYMergeRefusalTellsTheArchitectOnce(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	for _, tc := range []struct {
		name           string
		answer         func(http.ResponseWriter)
		ready, notices int
		reasonMentions []string
	}{
		{"refused for good", refusedAnswer(squash), 0, 1, []string{squash, "#42"}},
		{"a server error", func(w http.ResponseWriter) { http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway) }, 1, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, tc.answer)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now()))
			if err := mergeRunner(pool, records, github.url, quietLogger(), time.Now).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			reasons := assertMergeRows(t, pool, tc.ready, "merge-refused", tc.notices)
			for _, mention := range tc.reasonMentions {
				if !strings.Contains(reasons[0], mention) {
					t.Fatalf("merge-refused reason %q, want it to name %q", reasons[0], mention)
				}
			}
		})
	}
}

// A permanent ready_merge refusal writes its architect notice and finishes the row in one
// transaction. When FinishLeasedOutbox loses its connection, the notice rolls back with it and the
// row stays leased; once its lease expires, the row runs again and leaves exactly one notice and no
// ready_merge row. Writing the notice first and finishing later would send it twice.
func TestARetriedPermanentREADYMergeRefusalPostsOneNotice(t *testing.T) {
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, refusedAnswer("Squash merges are not allowed on this repository."))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	flaky := &flakyOutboxStore{Store: records, finishFailures: 1}
	clock := now
	runner := mergeRunner(pool, flaky, github.url, quietLogger(), func() time.Time { return clock })

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if got := flaky.failedFinishes(); got != 1 {
		t.Fatalf("failed finishes = %d, want the injected one", got)
	}

	clock = clock.Add(outboxLease + time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("retried RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 1)
}

// When a slow worker's lease expires, another worker owns the ready_merge row. The stale worker's
// permanent refusal rolls its notice back with its finish, which no longer holds the lease; only
// the worker holding the lease leaves the one architect notice.
func TestOnlyTheOwningREADYMergeLeasePostsAPermanentRefusal(t *testing.T) {
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, refusedAnswer("Squash merges are not allowed on this repository."))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))

	if err := mergeRunner(pool, leaseStealer{records}, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
		t.Fatalf("stale RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)

	later := now.Add(3 * outboxLease)
	if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return later }).RunOnce(context.Background()); err != nil {
		t.Fatalf("owning RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 1)
}

// A ready_merge row only GitHub's rules hold back (a required check still running or a required
// review, BLOCKED; GitHub still computing, UNKNOWN; an arm or merge-queue entry at READY's head)
// waits routinely, logged at debug however long it waits, so a person supplying the review GitHub
// requires sees no warning flood. One waiting on a branch update (BEHIND) or a draft (DRAFT) waits
// on someone, so it warns once it has waited long.
func TestAWaitingREADYMergeWarnsOnlyWhenItWaitsOnSomeone(t *testing.T) {
	for state, level := range map[string]string{"BLOCKED": "DEBUG", "UNKNOWN": "DEBUG", "BEHIND": "WARN", "DRAFT": "WARN", "armed": "DEBUG"} {
		t.Run(state, func(t *testing.T) {
			url := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", state) }, mergedAnswer).url
			if state == "armed" {
				url = newArmableGitHub(t).url
			}
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())
			row.Attempts = waitWarnAttempts
			enqueueOutbox(t, pool, records, row)
			var logged bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if err := mergeRunner(pool, records, url, log, time.Now).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			assertMergeRows(t, pool, 1, "merge-refused", 0)
			var levels []string
			for line := range strings.Lines(logged.String()) {
				var entry struct{ Level, Kind string }
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatalf("decode log line %q: %v", line, err)
				}
				if entry.Kind == string(record.OutboxKindReadyMerge) {
					levels = append(levels, entry.Level)
				}
			}
			if len(levels) != 1 || levels[0] != level {
				t.Fatalf("the waiting row logged at %v, want one line at %s", levels, level)
			}
		})
	}
}

// A ready_merge row ends as stale only on the record's word. While the record still awaits READY's
// head on an open pull request, GitHub showing the pull request closed or at another head is a
// webhook the record has not applied yet, or a GitHub read behind the record: the row waits and
// decides again. A reopen at READY's head takes the row back from any worker that read the record
// before it, so it is never lost: one landing while the row reads the close, however long the row
// waited before, or between the row's record read and its finish, leaves the merge submitted on the
// next run. A close the record has applied ends the row quietly, the architect already told of the
// close. The bound is GitHub's own clock, never the row's retries: a disagreement ends the row only
// once GitHub's last change to the pull request has stood longer than ghautomerge.LagBound, far
// longer than a webhook takes, and then tells the architect once (merge-stalled). So one
// disagreement after a long routine wait never ends it.
func TestAREADYMergeRowWaitsForGitHubAndTheRecordToAgree(t *testing.T) {
	for _, waited := range []int{0, waitWarnAttempts} {
		t.Run(fmt.Sprintf("a reopen lands while a row that waited %d times reads the close", waited), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }))
			github := newMergeGitHub(t, func(read int) string {
				if read > 1 {
					return pullJSON("OPEN", "ready-head", "CLEAN")
				}
				// The reopen's transaction commits as GitHub answers the row's read of the close.
				reopen(t, pool, records)
				return pullJSON("CLOSED", "ready-head", "UNKNOWN")
			}, mergedAnswer)
			now := time.Now().UTC()
			row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
			row.Attempts = waited
			enqueueOutbox(t, pool, records, row)
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("first RunOnce: %v", err)
			}
			assertMergeRows(t, pool, 1, "merge-stalled", 0)
			clock = clock.Add(time.Minute)
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("second RunOnce: %v", err)
			}
			assertMergeRows(t, pool, 0, "merge-stalled", 0)
			if merges, _ := github.merged(); merges != 1 {
				t.Fatalf("merges %d, want READY's merge submitted once the reopen reached both", merges)
			}
		})
	}
	t.Run("a reopen lands between the row's record read and its finish", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }))
		github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, mergedAnswer)
		now := time.Now().UTC()
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
		clock := now
		reopening := &reopensBeforeFinish{Store: records, t: t, pool: pool}
		if err := mergeRunner(pool, reopening, github.url, quietLogger(), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
			t.Fatalf("first RunOnce: %v", err)
		}
		if !reopening.reopened {
			t.Fatal("the row finished without the reopen landing first")
		}
		assertMergeRows(t, pool, 1, "merge-stalled", 0)
		clock = clock.Add(time.Minute)
		if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		assertMergeRows(t, pool, 0, "merge-stalled", 0)
		if merges, _ := github.merged(); merges != 1 {
			t.Fatalf("merges %d, want READY's merge submitted after the reopen", merges)
		}
	})
	t.Run("one disagreement after a long routine wait", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		now := time.Now().UTC()
		clock := now
		github := newMergeGitHub(t, func(read int) string {
			if read <= waitWarnAttempts {
				return pullJSON("OPEN", "ready-head", "BLOCKED")
			}
			// A close GitHub made just now, whose webhook has not reached the record.
			return pullJSONAt("CLOSED", "ready-head", "UNKNOWN", clock)
		}, mergedAnswer)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
		for run := 0; run <= waitWarnAttempts; run++ {
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce %d: %v", run, err)
			}
			clock = clock.Add(2 * time.Minute)
		}
		assertMergeRows(t, pool, 1, "merge-stalled", 0)
	})
	t.Run("GitHub and the record both hold the pull request closed", func(t *testing.T) {
		github := newMergeGitHub(t, func(int) string { return pullJSON("CLOSED", "ready-head", "UNKNOWN") }, mergedAnswer)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY.with(func(s *readyMergeSeed) { s.pr = record.PullRequestClosed }))
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now()))
		if err := mergeRunner(pool, records, github.url, quietLogger(), time.Now).RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		assertMergeRows(t, pool, 0, "merge-stalled", 0)
	})
	t.Run("a disagreement that has stood on GitHub past the bound", func(t *testing.T) {
		now := time.Now().UTC()
		closed := now.Add(-9 * time.Minute)
		github := newMergeGitHub(t, func(int) string { return pullJSONAt("CLOSED", "ready-head", "UNKNOWN", closed) }, mergedAnswer)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		// However many times the row tried before, only GitHub's clock bounds the disagreement.
		row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
		row.Attempts = 100
		enqueueOutbox(t, pool, records, row)
		clock := now
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("first RunOnce: %v", err)
		}
		assertMergeRows(t, pool, 1, "merge-stalled", 0)
		clock = clock.Add(2 * time.Minute)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("second RunOnce: %v", err)
		}
		reasons := assertMergeRows(t, pool, 0, "merge-stalled", 1)
		for _, want := range []string{"#42 CLOSED", "ready-head", closed.Format(time.RFC3339)} {
			if !strings.Contains(reasons[0], want) {
				t.Fatalf("merge-stalled reason %q, want it to name %q", reasons[0], want)
			}
		}
	})
}

// A merge Legion submitted that GitHub has not completed within mergeWaitWarn of its queueing
// (record.ReadyMerge.Queued, which workflow's queueReadyMerge stamps on every submission it
// queues) tells the issue's architect once (merge-waiting), naming the pull request, READY's head
// and what GitHub last answered, and then goes on waiting: nothing is withdrawn, the row stays and
// still waits, the issue stays in awaiting_merge, and every later run of the same row, however
// much longer it waits, tells nobody again.
func TestAREADYMergeGitHubLeavesWaitingTellsTheArchitectOnce(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	// A head GitHub will not merge: a required check holds the pull request back, and the
	// repository allows no auto-merge, so the row waits on GitHub however long it runs.
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "BLOCKED") }, mergedAnswer)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 0)
	clock = clock.Add(mergeWaitWarn + time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	reasons := assertMergeRows(t, pool, 1, "merge-waiting", 1)
	for _, want := range []string{"#42", "ready-head", "awaiting_merge"} {
		if !strings.Contains(reasons[0], want) {
			t.Fatalf("merge-waiting reason %q, want it to name %q", reasons[0], want)
		}
	}
	// The row goes on waiting on GitHub's answer, recorded as having told its architect.
	nextAt, lastError, warned, _ := readyMergeState(t, pool)
	if !nextAt.After(clock) || !strings.Contains(lastError, "BLOCKED") || !warned {
		t.Fatalf("the ready_merge row runs again at %s with %q, warned %v; want it still waiting on GitHub and recorded as told", nextAt, lastError, warned)
	}
	var at string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.AwaitingMerge) {
		t.Fatalf("LEGION-208 in %q (%v), want awaiting_merge", at, err)
	}
	clock = clock.Add(mergeWaitWarn)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 1)
}

// The compatibility path of that wait: a ready_merge row a daemon before record.ReadyMerge.Queued
// enqueued carries no queue time, and rememberWait measures its wait from the row's created_at,
// which is what it was measured from then — reading the zero time as the wait itself would tell
// the architect of every such row on its first run after the upgrade, naming the decades since the
// zero time. Production writes no such row any more: workflow's queueReadyMerge stamps Queued on
// every submission it queues, READY's acceptance and a reopen at READY's head alike, so the only
// rows of this shape are the ones the upgrade found already queued, and the fallback drains with
// the release that carries them off.
func TestAREADYMergeQueuedBeforeTheQueueTimeWarnsFromItsCreatedAt(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "BLOCKED") }, mergedAnswer)
	now := time.Now().UTC()
	// The row the upgrade finds: its payload carries no Queued, and the daemon before the field
	// queued it an hour and two minutes ago.
	row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
	row.CreatedAt = now.Add(-mergeWaitWarn - 2*time.Minute)
	enqueueOutbox(t, pool, records, row)
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the first run of the row the upgrade found: %v", err)
	}
	reasons := assertMergeRows(t, pool, 1, "merge-waiting", 1)
	if !strings.Contains(reasons[0], "1h2m0s ago") {
		t.Fatalf("merge-waiting reason %q, want it to name the hour and two minutes since the row's created_at", reasons[0])
	}
	if _, _, warned, _ := readyMergeState(t, pool); !warned {
		t.Fatal("the ready_merge row is not recorded as having told its architect")
	}
	// It waits on, and however much longer it waits it tells nobody again.
	clock = clock.Add(mergeWaitWarn)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run an hour on: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 1)
}

// The one merge-waiting notice is spent on a wait and on nothing else. readyMerge hands
// rememberWait every failure ghautomerge.Submit leaves it — a GitHub read that failed, a mutation
// refusal a retry may not meet again, the record read that decides the row — and one of those past
// mergeWaitWarn must spend neither the notice nor the row's Warned flag: the architect would be
// told GitHub has not completed the merge, with the daemon's own error rendered as GitHub's last
// answer, and the genuine wait that follows would then never be told at all. Here GitHub answers
// the row's read with a server error for the first hour, then answers as a pull request it will not
// merge, and the architect is told once: of the wait.
func TestAREADYMergeFailureThatIsNoWaitSpendsNoWaitingNotice(t *testing.T) {
	var mu sync.Mutex
	broken := true
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		failing := broken
		mu.Unlock()
		if failing {
			http.Error(w, "GitHub is having trouble", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, pullJSON("OPEN", "ready-head", "BLOCKED"))
	}})
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	clock := now.Add(mergeWaitWarn + time.Minute)
	runner := mergeRunner(pool, records, api, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run GitHub failed: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 0)
	nextAt, lastError, warned, _ := readyMergeState(t, pool)
	if !nextAt.After(clock) || !strings.Contains(lastError, "500") || warned {
		t.Fatalf("the ready_merge row runs again at %s with %q, warned %v; want it retrying on GitHub's failure with its notice unspent", nextAt, lastError, warned)
	}
	// GitHub answers again, as a pull request a required check holds back: a wait, and the notice
	// the failed run must have left unspent is the one it carries.
	mu.Lock()
	broken = false
	mu.Unlock()
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run GitHub left waiting: %v", err)
	}
	reasons := assertMergeRows(t, pool, 1, "merge-waiting", 1)
	if !strings.Contains(reasons[0], "BLOCKED") || strings.Contains(reasons[0], "500") {
		t.Fatalf("merge-waiting reason %q, want GitHub's own answer as what GitHub last answered", reasons[0])
	}
	if _, _, warned, _ := readyMergeState(t, pool); !warned {
		t.Fatal("the waiting row is not recorded as having told its architect, want the telling and its notice written together")
	}
}

// The disagreement is no wait the architect is told of either. GitHub showing the pull request
// closed, or at another head than the record awaits, is a wait (ghautomerge.Lagging) until it has
// stood past ghautomerge.LagBound, so a row past mergeWaitWarn that meets one answers every other
// term of the warn gate: the architect would be told the merge has not been withdrawn and the
// daemon goes on submitting it, under a cause whose own words are that GitHub shows the pull
// request closed, and would then be told the opposite within the bound, when the same
// disagreement stalls and the row's merge-stalled says the daemon stopped submitting. That one
// telling (readyMergeEnded) is the disagreement's, so the waiting notice stays unspent for the
// genuine wait that follows. Here GitHub answers the row's read as a pull request closed just
// now, then as one a required check holds back, and the architect is told once: of the wait.
func TestAREADYMergeDisagreementSpendsNoWaitingNotice(t *testing.T) {
	var mu sync.Mutex
	disagreeing := true
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		closed := disagreeing
		mu.Unlock()
		if closed {
			// A close GitHub made just now, whose webhook has not reached the record: a
			// disagreement the next run reads again, far from the bound that stalls it.
			_, _ = io.WriteString(w, pullJSON("CLOSED", "ready-head", "UNKNOWN"))
			return
		}
		_, _ = io.WriteString(w, pullJSON("OPEN", "ready-head", "BLOCKED"))
	}})
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	// The submission was queued an hour and a minute ago, so every run of it has waited longer
	// than mergeWaitWarn, while GitHub's own change to the pull request is seconds old.
	queued := now.Add(-mergeWaitWarn - time.Minute)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: queued}, now))
	clock := now
	runner := mergeRunner(pool, records, api, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run GitHub answered as closed: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 0)
	nextAt, lastError, warned, _ := readyMergeState(t, pool)
	if !nextAt.After(clock) || !strings.Contains(lastError, "CLOSED") || warned {
		t.Fatalf("the ready_merge row runs again at %s with %q, warned %v; want it waiting for GitHub and the record to agree with its notice unspent", nextAt, lastError, warned)
	}
	// GitHub answers as the record has it again, a pull request a required check holds back: a
	// wait, and the notice the disagreement must have left unspent is the one it carries.
	mu.Lock()
	disagreeing = false
	mu.Unlock()
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run GitHub left waiting: %v", err)
	}
	reasons := assertMergeRows(t, pool, 1, "merge-waiting", 1)
	if !strings.Contains(reasons[0], "BLOCKED") || strings.Contains(reasons[0], "CLOSED") {
		t.Fatalf("merge-waiting reason %q, want the wait's own answer as what GitHub last answered", reasons[0])
	}
	if _, _, warned, _ := readyMergeState(t, pool); !warned {
		t.Fatal("the waiting row is not recorded as having told its architect, want the telling and its notice written together")
	}
}

// The arm gone from GitHub is no wait the architect is told of either. Once the row has seen
// READY's head armed, an open pull request at that head with nothing armed and no removal on its
// timeline is a wait (ghautomerge.Disappeared, removal's last answer) whose own words are that the
// arm the daemon placed is gone and GitHub has yet to say who took it, so a row past
// mergeWaitWarn that meets one answers every other term of the warn gate: the architect would be
// told the merge has not been withdrawn and the daemon goes on submitting it, under a cause that
// says it may have been, and the removal GitHub names next, whose own telling ends the row
// (merge-removed), would find the one notice spent. Only GitHub not having completed the merge
// carries it (waitingOnGitHub), so the notice stays unspent for the genuine wait that follows.
// Here GitHub shows READY's head armed, then loses the arm with its timeline naming no removal,
// then shows it armed again, and the architect is told once: of the wait.
//
// On the gate before waitingOnGitHub the disappearance warns, and the assertion that breaks is the
// assertMergeRows of no merge-waiting notice after that run: a Disappeared is a wait
// (wait.ErrWaiting), is no ghautomerge.Lagging, and is no permanentRefusal, so every term of that
// gate held and the notice was written on it.
func TestAREADYMergeArmGoneFromGitHubSpendsNoWaitingNotice(t *testing.T) {
	github := newArmableGitHub(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run that saw READY's head armed: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 0)
	// GitHub's read of the pull request runs ahead of its timeline: the arm the row saw is gone,
	// with nothing on the timeline yet to name what took it, an hour and a minute after the
	// submission was queued.
	github.set(func(g *armableGitHub) { g.armed = false })
	clock = now.Add(mergeWaitWarn + time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run whose arm had disappeared: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 0)
	nextAt, lastError, warned, _ := readyMergeState(t, pool)
	if !nextAt.After(clock) || !strings.Contains(lastError, "no longer armed or queued") || warned {
		t.Fatalf("the ready_merge row runs again at %s with %q, warned %v; want it waiting for GitHub to record the removal with its notice unspent", nextAt, lastError, warned)
	}
	// GitHub's read catches its timeline up, the arm standing where the row saw it: a wait, and
	// the notice the disappearance must have left unspent is the one it carries.
	github.set(func(g *armableGitHub) { g.armed = true })
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run GitHub left waiting on its arm: %v", err)
	}
	reasons := assertMergeRows(t, pool, 1, "merge-waiting", 1)
	if !strings.Contains(reasons[0], "armed for auto-merge") || strings.Contains(reasons[0], "no longer armed") {
		t.Fatalf("merge-waiting reason %q, want the arm's own answer as what GitHub last answered", reasons[0])
	}
	if _, _, warned, _ := readyMergeState(t, pool); !warned {
		t.Fatal("the waiting row is not recorded as having told its architect, want the telling and its notice written together")
	}
	if _, arms := github.counts(); arms != 0 {
		t.Fatalf("arms %d while the arm was gone from GitHub, want none: the row reads the removal rather than submitting again", arms)
	}
}

// A reopen at READY's head starts the wait over with the telling. The reopen's queue of the same
// issue and head replaces the payload (record's readyMergeConflict), which clears
// record.ReadyMerge.Warned, while leaving the row's created_at at the original queueing: a wait
// measured from created_at would warn a second time on the reopened submission's very first run,
// naming hours a submission seconds old has not waited. Measured from record.ReadyMerge.Queued,
// which the reopen's payload carries, the architect hears again only once the reopened submission
// has itself waited mergeWaitWarn, and hears of that hour rather than of the row's whole life.
func TestAReopenedREADYMergeWaitsItsOwnHourBeforeWarningAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "BLOCKED") }, mergedAnswer)
	now := time.Now().UTC()
	row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now)
	row.CreatedAt = now
	enqueueOutbox(t, pool, records, row)
	clock := now.Add(mergeWaitWarn + time.Minute)
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the run that had waited an hour: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 1)
	_, _, warned, createdAt := readyMergeState(t, pool)
	if !warned {
		t.Fatal("the ready_merge row is not recorded as having told its architect")
	}
	// The pull request is reopened at READY's head: the workflow queues the same issue and head
	// again, its payload carrying the clock as its queue time (workflow's queueReadyMerge).
	clock = clock.Add(5 * time.Minute)
	requeued := clock
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: requeued}, requeued))
	if _, _, warned, after := readyMergeState(t, pool); warned || !after.Equal(createdAt) {
		t.Fatalf("the requeued row is warned %v at created_at %s; want it unwarned, its created_at left at %s by the upsert", warned, after, createdAt)
	}
	clock = clock.Add(2 * time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the reopened submission's first run: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 1)
	if _, _, warned, _ := readyMergeState(t, pool); warned {
		t.Fatal("the reopened submission told its architect minutes after it was queued, want it told once it has waited an hour of its own")
	}
	clock = clock.Add(mergeWaitWarn)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("the reopened submission's run an hour on: %v", err)
	}
	reasons := assertMergeRows(t, pool, 1, "merge-waiting", 2)
	if !strings.Contains(reasons[1], "1h2m0s ago") {
		t.Fatalf("the second merge-waiting reason %q, want it to name the hour and two minutes since the reopen's queue", reasons[1])
	}
	if _, _, warned, after := readyMergeState(t, pool); !warned || !after.Equal(createdAt) {
		t.Fatalf("the reopened row is warned %v at created_at %s; want it recorded as told, its created_at still %s", warned, after, createdAt)
	}
}

// The delayed disarm: an auto_merge_disable row sent its disable while the issue was still merging,
// and its runner stopped before GitHub answered. READY has been accepted at that head since, so the
// replacement runner's disarm keeps the arm it finds, and the READY merge row finds the pull request
// armed. The outstanding disable then lands. An arm is not the merge: the row waits on it, and once
// the arm is gone while the record still awaits READY it arms READY's head again, ending only once
// GitHub reports the pull request merged at that head.
func TestADelayedDisarmNeverLosesTheREADYMerge(t *testing.T) {
	github := newArmableGitHub(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.AutoMergeDisable{Number: 42}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	run := func(step string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	run("with READY's arm standing")
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if disables, arms := github.counts(); disables != 0 || arms != 0 {
		t.Fatalf("disables %d and arms %d while READY's arm stands, want the arm kept and none sent", disables, arms)
	}
	github.remove(manuallyDisabledEvent(implementApp))
	run("after the earlier disable lands")
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("arms %d after the arm disappeared, want READY's head armed again", arms)
	}
	github.set(func(g *armableGitHub) { g.state = "MERGED" })
	run("after GitHub merges READY's head")
	assertMergeRows(t, pool, 0, "merge-refused", 0)
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("arms %d after GitHub merged READY's head, want no other arm", arms)
	}
}

// A merge the merge queue fails is the transition back to implementing, never a second submission
// (LEGION-800): once the row has seen READY's head in the queue, GitHub taking it out for
// failed_checks, its merge_group run red on the commit the queue built, sends the issue from
// awaiting_merge to implementing as a red head does. The implementer's task and the architect's
// checks-red name the failing run, the READY's withdrawal is posted with its disarm, and the row
// ends without enqueueing the head again, which would rebuild every entry queued behind it and fail
// the same way.
func TestAMergeTheQueueFailsSendsTheIssueBackToImplementing(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
	run := func(step string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	run("READY's submission")
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("enables %d, want READY's head enqueued once", arms)
	}
	github.remove(dequeuedEvent("failed_checks", "github-merge-queue", failedMergeGroup))
	run("the retry after the merge queue failed the merge")
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("enables %d after the merge queue failed READY's merge, want no second submission", arms)
	}
	assertMergeRows(t, pool, 0, "merge-removed", 0)
	var at string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.Implementing) {
		t.Fatalf("LEGION-208 in %q (%v), want implementing", at, err)
	}
	// A phase worker's start is one row, which carries the task and waits for the issue's
	// admission slot (workflow.StartOrOwe). The task that row carries is what this asserts about.
	var task string
	if err := pool.QueryRow(context.Background(), `select payload->>'task' from outbox where kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'implementer' and coalesce(payload->>'task', '') <> ''`).Scan(&task); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	for _, want := range []string{failedMergeGroupRun, "integration", "failed_checks", "#42"} {
		if !strings.Contains(task, want) {
			t.Fatalf("the implementer's task %q, want it to name %q", task, want)
		}
	}
	if strings.Contains(task, "lint") {
		t.Fatalf("the implementer's task %q names the merge_group job that passed", task)
	}
	var reason string
	if err := pool.QueryRow(context.Background(), `select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'`).Scan(&reason); err != nil || !strings.Contains(reason, failedMergeGroupRun) {
		t.Fatalf("the architect's checks-red reason %q (%v), want it to name the failing run", reason, err)
	}
	var disarms int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'auto_merge_disable'`).Scan(&disarms); err != nil || disarms != 1 {
		t.Fatalf("auto_merge_disable rows %d (%v), want the withdrawal's one", disarms, err)
	}
}

// A merge failure whose fact the workflow already applied (intake.ApplyFact's Duplicate) changes
// nothing, and the row's log says so rather than that the issue goes back to implementing.
func TestAMergeFailureAlreadyAppliedIsNotLoggedAsSendingTheIssueBack(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
	var logged bytes.Buffer
	runner := mergeRunner(pool, records, github.url, slog.New(slog.NewJSONHandler(&logged, nil)), func() time.Time { return clock })
	runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce READY's submission: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		_, err := records.MarkProcessed(context.Background(), tx, "outbox", "merge-failed:LEGION-208:ready-head:RFMQE_1")
		return err
	}); err != nil {
		t.Fatalf("record the merge failure's fact as applied: %v", err)
	}
	github.remove(dequeuedEvent("failed_checks", "github-merge-queue", failedMergeGroup))
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce after the merge queue failed the merge: %v", err)
	}
	var at string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.AwaitingMerge) {
		t.Fatalf("LEGION-208 in %q (%v), want awaiting_merge: the fact was already applied", at, err)
	}
	told := false
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Msg string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if strings.Contains(entry.Msg, "goes back to implementing") {
			t.Fatalf("the row logged %q for a merge failure already applied", entry.Msg)
		}
		told = told || strings.Contains(entry.Msg, "already applied")
	}
	if !told {
		t.Fatalf("the row's log %s, want a line saying the merge failure was already applied", logged.String())
	}
}

// The failure the issue goes back to implementing on names what each failing job's log says
// failed (INFRA-1442), and the row reads those logs where it is about to tell of the removal:
// after the decision, never while it is made. So the implementer's task and the architect's
// checks-red carry the tests the log states failed, and a run that meets a failure already
// applied — an apply that failed after its mark stood, a worker whose settle was lost, the row
// run again at the same head — reads no log a second time for a decision already made, and
// tells nobody twice.
func TestAMergeTheQueueFailedHasItsJobLogsReadOnceForOneDecision(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.jobLogs = map[int64]string{failedMergeGroupJob: failedMergeGroupLog} })
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
	run := func(step string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	logReads := func() int {
		github.mu.Lock()
		defer github.mu.Unlock()
		return github.logReads
	}
	run("READY's submission")
	if reads := logReads(); reads != 0 {
		t.Fatalf("job logs read %d while READY's head was submitted, want none: nothing has failed", reads)
	}
	github.remove(dequeuedEvent("failed_checks", "github-merge-queue", failedMergeGroupWithJob))
	run("the retry after the merge queue failed the merge")
	if reads := logReads(); reads != 1 {
		t.Fatalf("job logs read %d for the removal, want the one failing job's", reads)
	}
	var task string
	if err := pool.QueryRow(context.Background(), `select payload->>'task' from outbox where kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'implementer' and coalesce(payload->>'task', '') <> ''`).Scan(&task); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	var reason string
	if err := pool.QueryRow(context.Background(), `select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'`).Scan(&reason); err != nil {
		t.Fatalf("read the architect's checks-red notice: %v", err)
	}
	for _, want := range []string{"integration", "its log names TestTheQueueTakesTheEntryOut as failing",
		`its first failure line "--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)"`} {
		if !strings.Contains(task, want) {
			t.Fatalf("the implementer's task %q, want it to name %q", task, want)
		}
		if !strings.Contains(reason, want) {
			t.Fatalf("the architect's checks-red notice %q, want it to name %q", reason, want)
		}
	}
	// The record awaits that READY again and the row runs once more against the removal it
	// already applied: the fact is the same event's, so nothing moves and nothing is read.
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Armed: true}, clock))
	run("the row run again against a failure already applied")
	if reads := logReads(); reads != 1 {
		t.Fatalf("job logs read %d once the row met a failure already applied, want the first run's one", reads)
	}
	var starts, notices int
	if err := pool.QueryRow(context.Background(), `select
		(select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer' and coalesce(payload->>'task', '') <> ''),
		(select count(*) from outbox where kind = 'notice')`).Scan(&starts, &notices); err != nil {
		t.Fatalf("count what the second run told: %v", err)
	}
	if starts != 1 || notices != 1 {
		t.Fatalf("implementer starts %d and notices %d, want the one apply's: a failure already applied tells nobody again", starts, notices)
	}
	var at string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.AwaitingMerge) {
		t.Fatalf("LEGION-208 in %q (%v), want awaiting_merge: the fact was already applied", at, err)
	}
}

// A queued pull request whose merge GitHub's merge queue cannot make goes back to implementing
// (INFRA-1442), and the entry's own state is what says so: while an entry is queued GitHub answers
// the pull request's mergeable UNKNOWN, so a daemon reading that field alone leaves a conflicting
// pull request sitting in the queue for hours. The row acts on the one state in which the entry's
// own head is the cause — UNMERGEABLE while the queue has built the entry on the tip of the pull
// request's own base branch, where nothing ahead of it in the queue can be to blame — and only
// once that has stood queueConflictBound, so one odd read decides nothing. Every other entry
// state, an entry the queue has built nothing for, and an entry built on another commit than the
// base branch's tip neither write the marks nor clear them, and an entry GitHub rebuilds onto
// another base starts the clock again. What GitHub answers for the pull request's own mergeability
// changes none of it. An entry the row has not decided also leaves the record where it was and
// sends no mutation at all: it neither submits the head again nor takes the entry out of the
// queue, which only a withdrawal's disarm does. The entries the row reads no conflict from —
// one in another state than UNMERGEABLE, one built on another entry's commit, one the queue has
// built nothing for — stand here on the database too, and that branch of the row stands on its
// own below (TestTheMergeRowLeavesAnEntryItHasNotDecidedAlone); which states those are is read
// off the entry alone, in ghautomerge, since each is this one branch of the row.
func TestAnUnmergeableEntryOnItsBaseBranchesTipSendsTheIssueBackToImplementing(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		queueState, queueBase string
		// mergeable and mergeState are GitHub's own answers for the pull request, which this
		// decision never reads: every row has the queue's entry decide it.
		mergeable, mergeState string
		// gap is how long after the first read the row reads again; a zero gap reads once.
		gap time.Duration
		// rebuilt is the commit GitHub rebuilds the entry onto, moving the base branch to it,
		// between the two reads.
		rebuilt string
		want    phase.Phase
		// wantBase is the entry base commit the row's mark stands at afterwards, "" for no mark,
		// and markLate says the mark stands at the second read rather than the first.
		wantBase string
		markLate bool
	}{
		{name: "unmergeable on the base branch's tip, read once", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			want: phase.AwaitingMerge, wantBase: queueBaseTip},
		{name: "unmergeable on the base branch's tip, read again at the bound", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			gap: queueConflictBound, want: phase.Implementing},
		{name: "unmergeable on the base branch's tip, read again past the bound", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			gap: queueConflictBound + time.Minute, want: phase.Implementing},
		{name: "unmergeable on the base branch's tip, read twice inside the bound", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			gap: queueConflictBound - time.Minute, want: phase.AwaitingMerge, wantBase: queueBaseTip},
		{name: "unmergeable on another entry's commit", queueState: "UNMERGEABLE", queueBase: "entry-ahead",
			gap: queueConflictBound + time.Minute, want: phase.AwaitingMerge},
		{name: "unmergeable with no commit the queue has built", queueState: "UNMERGEABLE",
			gap: queueConflictBound + time.Minute, want: phase.AwaitingMerge},
		{name: "queued on the base branch's tip", queueState: "QUEUED", queueBase: queueBaseTip,
			gap: queueConflictBound + time.Minute, want: phase.AwaitingMerge},
		{name: "unmergeable while GitHub reports the pull request mergeable and clean", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			mergeable: "true", mergeState: "CLEAN", gap: queueConflictBound + time.Minute, want: phase.Implementing},
		{name: "unmergeable while GitHub has yet to compute the pull request's mergeability", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			mergeable: "null", mergeState: "UNKNOWN", gap: queueConflictBound + time.Minute, want: phase.Implementing},
		{name: "unmergeable on a base the queue rebuilds the entry onto", queueState: "UNMERGEABLE", queueBase: queueBaseTip,
			gap: queueConflictBound + time.Minute, rebuilt: "rebuilt-tip", want: phase.AwaitingMerge, wantBase: "rebuilt-tip", markLate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mergeState := tc.mergeState
			if mergeState == "" {
				mergeState = "UNKNOWN"
			}
			github := newQueueGitHub(t, mergeState)
			github.set(func(g *armableGitHub) {
				g.queued, g.queueState, g.queueBase, g.mergeable = true, tc.queueState, tc.queueBase, tc.mergeable
			})
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Now().UTC().Truncate(time.Second)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("the row's first read of the entry: %v", err)
			}
			if tc.gap != 0 {
				if tc.rebuilt != "" {
					github.set(func(g *armableGitHub) { g.queueBase, g.baseTip = tc.rebuilt, tc.rebuilt })
				}
				clock = now.Add(tc.gap)
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("the row's read %s on: %v", tc.gap, err)
				}
			}
			var at string
			if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(tc.want) {
				t.Fatalf("LEGION-208 in %q (%v), want %s", at, err, tc.want)
			}
			mark, base, standing := unmergeableMark(t, pool)
			if tc.want == phase.Implementing {
				if standing {
					t.Fatalf("the ready_merge row still stands (mark %s at %s), want it ended by the return", mark, base)
				}
				var reason string
				if err := pool.QueryRow(context.Background(), `select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'`).Scan(&reason); err != nil {
					t.Fatalf("read the architect's checks-red notice: %v", err)
				}
				for _, want := range []string{"conflicts with " + queueBaseBranch, "merge " + queueBaseBranch + " forward", "merge queue", queueBaseTip} {
					if !strings.Contains(reason, want) {
						t.Fatalf("the checks-red reason %q, want it to name %q", reason, want)
					}
				}
				return
			}
			if !standing {
				t.Fatal("the ready_merge row is gone, want it still waiting on GitHub's queue")
			}
			// The fake fails a merge or a dequeue itself: these are the enables and disables it
			// counts, and the only mutations left for the row to have sent.
			if disables, arms := github.counts(); disables != 0 || arms != 0 {
				t.Fatalf("disables %d and arms %d, want no mutation at all for an entry the row has not decided", disables, arms)
			}
			var head string
			var rounds int
			if err := pool.QueryRow(context.Background(), `select p.head_sha, coalesce((select sum(rounds) from phases where issue = 'LEGION-208'), 0)
				from issues i join pull_requests p on p.issue = i.key where i.key = 'LEGION-208'`).Scan(&head, &rounds); err != nil {
				t.Fatalf("read LEGION-208's record: %v", err)
			}
			if head != "ready-head" || rounds != 0 {
				t.Fatalf("LEGION-208 at %q with %d rounds counted, want it awaiting its merge at ready-head with none", head, rounds)
			}
			want := time.Time{}
			if tc.wantBase != "" {
				want = now
				if tc.markLate {
					want = now.Add(tc.gap)
				}
			}
			if base != tc.wantBase || !mark.Equal(want) {
				t.Fatalf("the row's first unmergeable read is %s on %q, want %s on %q", mark, base, want, tc.wantBase)
			}
		})
	}
}

// An entry the row has not decided leaves everything where it stands: the merge-queue entry GitHub
// holds is a wait like any other, so the row sends no mutation at all — it neither submits the
// head again nor takes the entry out of the queue, which only a withdrawal's disarm does — writes
// no mark of a conflict, tells nobody, and leaves the issue awaiting its merge at its recorded
// head with no round counted against anyone. Which entry states the row reads a conflict from at
// all is read off the entry itself, beside the wait that carries it
// (ghautomerge's TestAnUnmergeableEntryOnItsBaseBranchesTipIsTheHeadsOwnConflict); what stands
// here is the one branch of the row on the database (INFRA-1442).
func TestTheMergeRowLeavesAnEntryItHasNotDecidedAlone(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		queueState, queueBase string
	}{
		{name: "an entry the queue is still working on", queueState: "QUEUED", queueBase: queueBaseTip},
		{name: "an entry the queue cannot merge, built on the commit it made for the entry ahead",
			queueState: "UNMERGEABLE", queueBase: "entry-ahead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newQueueGitHub(t, "UNKNOWN")
			github.set(func(g *armableGitHub) {
				g.queued, g.queueState, g.queueBase = true, tc.queueState, tc.queueBase
			})
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Now().UTC().Truncate(time.Second)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now })
			runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("the row's read of the entry: %v", err)
			}
			assertMergeRows(t, pool, 1, "merge-removed", 0)
			if kinds := noticeCount(t, pool); kinds != 0 {
				t.Fatalf("notices of every kind %d, want none: an entry the row has not decided tells nobody", kinds)
			}
			if disables, arms := github.counts(); disables != 0 || arms != 0 {
				t.Fatalf("disables %d and arms %d, want no mutation at all for an entry the row has not decided", disables, arms)
			}
			mark, base, standing := unmergeableMark(t, pool)
			if !standing || !mark.IsZero() || base != "" {
				t.Fatalf("the ready_merge row stands %t with the mark %s on %q, want it waiting with no conflict marked", standing, mark, base)
			}
			var at, head string
			var rounds int
			if err := pool.QueryRow(context.Background(), `select i.phase, p.head_sha, coalesce((select sum(rounds) from phases where issue = 'LEGION-208'), 0)
				from issues i join pull_requests p on p.issue = i.key where i.key = 'LEGION-208'`).Scan(&at, &head, &rounds); err != nil {
				t.Fatalf("read LEGION-208's record: %v", err)
			}
			if at != string(phase.AwaitingMerge) || head != "ready-head" || rounds != 0 {
				t.Fatalf("LEGION-208 in %q at %q with %d rounds counted, want it awaiting its merge at ready-head with none", at, head, rounds)
			}
		})
	}
}

// unmergeableMark is the first UNMERGEABLE read the one ready_merge row holds and the commit the
// merge queue had built its entry on at that read (record.ReadyMerge.Unmergeable,
// UnmergeableBase), the zero time and "" where the row holds no mark, and standing false where no
// ready_merge row is left at all.
func unmergeableMark(t *testing.T, pool *pgxpool.Pool) (mark time.Time, base string, standing bool) {
	t.Helper()
	var at, on *string
	if err := pool.QueryRow(context.Background(),
		`select payload->>'unmergeable', payload->>'unmergeableBase' from outbox where kind = 'ready_merge'`).Scan(&at, &on); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, "", false
		}
		t.Fatalf("read the ready_merge row's unmergeable mark: %v", err)
	}
	if at != nil {
		read, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			t.Fatalf("parse the row's unmergeable read %q: %v", *at, err)
		}
		mark = read.UTC()
	}
	if on != nil {
		base = *on
	}
	return mark, base, true
}

// Any other removal of an arm or queue entry the row saw at READY's head (a person dequeuing the
// pull request, a person turning its auto-merge off) is not the row's to undo (LEGION-800): the row
// ends, submitting nothing more, and tells the architect once (merge-removed), naming what was
// removed, GitHub's reason and who removed it. The issue stays in awaiting_merge.
func TestARemovalByAPersonEndsTheREADYMergeAndTellsTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		github  func(*testing.T) *armableGitHub
		removal string
		names   []string
	}{
		{"a person dequeues it", func(t *testing.T) *armableGitHub { return newQueueGitHub(t, "CLEAN") },
			dequeuedEvent("manual", "a-maintainer", "null"), []string{"#42", "merge queue", "manual", "a-maintainer", "ready-head"}},
		{"a person turns its auto-merge off", func(t *testing.T) *armableGitHub {
			g := newArmableGitHub(t)
			g.set(func(g *armableGitHub) { g.armed, g.timeline = false, nil })
			return g
		}, manuallyDisabledEvent("a-maintainer"), []string{"#42", "auto-merge", "manually_disabled", "a-maintainer", "ready-head"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := tc.github(t)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			clock := time.Now().UTC()
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			run := func(step string) {
				t.Helper()
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("RunOnce %s: %v", step, err)
				}
				clock = clock.Add(time.Minute)
			}
			run("READY's submission")
			github.remove(tc.removal)
			run("the retry after the removal")
			run("a later drain")
			if _, arms := github.counts(); arms != 1 {
				t.Fatalf("enables %d after a person's removal, want READY's one submission and no other", arms)
			}
			reasons := assertMergeRows(t, pool, 0, "merge-removed", 1)
			for _, want := range tc.names {
				if !strings.Contains(reasons[0], want) {
					t.Fatalf("merge-removed reason %q, want it to name %q", reasons[0], want)
				}
			}
			var at string
			if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&at); err != nil || at != string(phase.AwaitingMerge) {
				t.Fatalf("LEGION-208 in %q (%v), want awaiting_merge", at, err)
			}
		})
	}
}

// A removal whose job-log reads the daemon's own stop cut short tells nobody and ends no row.
// Every line those reads wrote then says the daemon stopped rather than what the job's log held,
// and a *Removed ends its row with merge-removed written once and never again, so the row would
// spend its one telling on lines about the stop. The row fails instead, with a failure that is
// no removal and no refusal: nothing is told, nothing is marked, and the row reads the same
// removal from GitHub when the daemon comes back (INFRA-1442).
func TestARemovalWhoseLogReadsADaemonStopCutShortTellsNobody(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	arrived := make(chan struct{})
	github.set(func(g *armableGitHub) {
		g.jobLogs, g.logArrived = map[int64]string{failedMergeGroupJob: failedMergeGroupLog}, arrived
	})
	github.remove(dequeuedEvent("manual", "a-maintainer", failedMergeGroupWithJob))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	payload := record.ReadyMerge{Number: 42, Head: "ready-head", Armed: true, Queued: now}
	row := mustOutboxRow(t, "LEGION-208", payload, now)
	enqueueOutbox(t, pool, records, row)
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now })
	lane, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		<-arrived
		stop()
	}()
	err := runner.readyMerge(lane, row, payload)
	var removed *ghautomerge.Removed
	switch {
	case err == nil || !errors.Is(err, context.Canceled):
		t.Fatalf("the row under a daemon stopping = %v, want the stop's own error", err)
	case errors.As(err, &removed):
		t.Fatalf("the row = %v, want no removal: a removal told from lines about the stop is told once and never again", err)
	case permanentRefusal(row, err, now):
		t.Fatalf("the row = %v, want a failure it retries rather than one it is finished on", err)
	}
	assertMergeRows(t, pool, 1, "merge-removed", 0)
	if kinds := noticeCount(t, pool); kinds != 0 {
		t.Fatalf("notices of every kind %d, want none: the daemon stopping tells nobody", kinds)
	}
	if posts := messagePosts(t, pool); len(posts) != 0 {
		t.Fatalf("Dispatch messages %q, want none", posts)
	}
}

// GitHub's merge queue takes a pull request out with the one reason merge_conflict for two
// different things, and the merge state GitHub computes once the entry is no longer queued tells
// them apart (INFRA-1442). Where GitHub still merges the head, what the queue could not merge was
// this head against an entry ahead of it: no check failed, nobody withdrew anything, and nothing
// else in Legion would ever submit that merge again, so the row submits it once more at the same
// head, marks the submission spent on its payload and posts what it did on the issue, with no
// notice written at all. A head that conflicts with the base itself (DIRTY) ends the row with the
// architect told, as any other removal does, and so does a second conflict removal of a head
// whose submission is spent: the row's own re-queue of the pull request never clears that mark,
// so a third submission is never made. A merge state GitHub has not computed (UNKNOWN) decides
// nothing — the row asks again on its own backoff and ends only once conflictUnknownBound has
// passed from the first such read. The issue stays in awaiting_merge at its recorded head on
// every path, with no round counted against anyone.
func TestAMergeTheQueueTookOutForAConflictIsSubmittedOnceMoreOrEnded(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mergeState is what GitHub answers for the pull request once its entry is gone, and gap
		// how long after the row's read of the removal it reads again; a zero gap reads once.
		mergeState string
		gap        time.Duration
		// removedAgain is the merge queue taking the pull request out for a conflict a second
		// time, after the row's further submission put it back in the queue.
		removedAgain bool
		// arms is every enable the row sent, READY's own submission among them; messages the
		// Dispatch messages posted on the issue, notices the notices written for its architect,
		// and standing whether the ready_merge row is left to run again.
		arms, messages, notices int
		standing                bool
		// spent is the mark of the one further submission this head has had
		// (record.ReadyMerge.Resubmitted), and unknown whether the row holds the moment of its
		// first read of an uncomputed merge state (record.ReadyMerge.ConflictUnknown).
		spent, unknown bool
	}{
		{name: "GitHub merges the head cleanly", mergeState: "CLEAN", arms: 2, messages: 1, standing: true, spent: true},
		{name: "GitHub merges the head cleanly and the queue takes it out again", mergeState: "CLEAN", removedAgain: true, arms: 2, messages: 1, notices: 1},
		{name: "the head conflicts with the base itself", mergeState: "DIRTY", arms: 1, notices: 1},
		{name: "GitHub has computed no merge state yet", mergeState: "UNKNOWN", gap: conflictUnknownBound - time.Minute, arms: 1, standing: true, unknown: true},
		{name: "GitHub has computed no merge state within five minutes", mergeState: "UNKNOWN", gap: conflictUnknownBound, arms: 1, notices: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A merge-queue base GitHub reports mergeable: READY's own submission enqueues the
			// pull request, as the further submission does once the queue has taken it out.
			github := newQueueGitHub(t, "CLEAN")
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Now().UTC().Truncate(time.Second)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			// The workflow is wired in so that a fact this path applied would move the issue: it
			// applies none, and the record below is what says so.
			runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
			run := func(step string) {
				t.Helper()
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("RunOnce %s: %v", step, err)
				}
				clock = clock.Add(time.Minute)
			}
			run("READY's submission")
			github.set(func(g *armableGitHub) { g.mergeState = tc.mergeState })
			github.remove(dequeuedEvent("merge_conflict", "github-merge-queue", "null"))
			read := clock
			run("the read that finds the removal")
			if tc.gap != 0 {
				clock = read.Add(tc.gap)
				run("the read after the gap")
			}
			if tc.removedAgain {
				github.set(func(g *armableGitHub) { g.mergeState = tc.mergeState })
				github.remove(dequeuedEvent("merge_conflict", "github-merge-queue", "null"))
				run("the read that finds the second removal")
				run("a later drain")
			}
			if _, arms := github.counts(); arms != tc.arms {
				t.Fatalf("enables %d, want %d: READY's own submission and what the conflict removal left", arms, tc.arms)
			}
			left := 0
			if tc.standing {
				left = 1
			}
			reasons := assertMergeRows(t, pool, left, "merge-removed", tc.notices)
			for _, want := range reasons {
				for _, name := range []string{"#42", "merge queue", "merge_conflict", "ready-head"} {
					if !strings.Contains(want, name) {
						t.Fatalf("merge-removed reason %q, want it to name %q", want, name)
					}
				}
			}
			if kinds := noticeCount(t, pool); kinds != tc.notices {
				t.Fatalf("notices of every kind %d, want %d: a further submission tells the architect nothing", kinds, tc.notices)
			}
			posts := messagePosts(t, pool)
			if len(posts) != tc.messages {
				t.Fatalf("Dispatch messages %q, want %d", posts, tc.messages)
			}
			for _, post := range posts {
				for _, name := range []string{"#42", "ready-head", "merge_conflict", "once more", tc.mergeState} {
					if !strings.Contains(post, name) {
						t.Fatalf("the message posted on the issue %q, want it to name %q", post, name)
					}
				}
			}
			spent, unknown, standing := conflictMarks(t, pool)
			switch {
			case standing != tc.standing:
				t.Fatalf("the ready_merge row stands %t, want %t", standing, tc.standing)
			case standing && (spent != tc.spent || unknown.IsZero() == tc.unknown):
				t.Fatalf("the row's further submission is spent %t and its first uncomputed read %s, want %t and a mark %t", spent, unknown, tc.spent, tc.unknown)
			}
			var at, head string
			var rounds int
			if err := pool.QueryRow(context.Background(), `select i.phase, p.head_sha, coalesce((select sum(rounds) from phases where issue = 'LEGION-208'), 0)
				from issues i join pull_requests p on p.issue = i.key where i.key = 'LEGION-208'`).Scan(&at, &head, &rounds); err != nil {
				t.Fatalf("read LEGION-208's record: %v", err)
			}
			if at != string(phase.AwaitingMerge) || head != "ready-head" || rounds != 0 {
				t.Fatalf("LEGION-208 in %q at %q with %d rounds counted, want it awaiting its merge at ready-head with none", at, head, rounds)
			}
		})
	}
}

// conflictMarks is what the one ready_merge row holds of the merge queue's conflict removal: the
// further submission spent on its head (record.ReadyMerge.Resubmitted) and the moment of its first
// read of a merge state GitHub had not computed (ConflictUnknown), with standing false where no
// ready_merge row is left at all.
func conflictMarks(t *testing.T, pool *pgxpool.Pool) (spent bool, unknown time.Time, standing bool) {
	t.Helper()
	var at *string
	if err := pool.QueryRow(context.Background(),
		`select coalesce((payload->>'resubmitted')::boolean, false), payload->>'conflictUnknown' from outbox where kind = 'ready_merge'`).Scan(&spent, &at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, false
		}
		t.Fatalf("read the ready_merge row's conflict marks: %v", err)
	}
	if at != nil {
		read, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			t.Fatalf("parse the row's uncomputed merge state read %q: %v", *at, err)
		}
		unknown = read.UTC()
	}
	return spent, unknown, true
}

// noticeCount is every notice the outbox holds, whatever its kind.
func noticeCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var notices int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'notice'`).Scan(&notices); err != nil {
		t.Fatalf("count the notices: %v", err)
	}
	return notices
}

// messagePosts is every Dispatch message the outbox holds, oldest first.
func messagePosts(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload->>'body' from outbox where kind = 'dispatch_message' order by id`)
	if err != nil {
		t.Fatalf("read the Dispatch messages: %v", err)
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan a Dispatch message: %v", err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// On a base with a merge queue, enabling auto-merge before GitHub reports the pull request mergeable
// arms it rather than enqueueing it, and an armed pull request fails a required check that judges
// the arm (LEGION-800). The row waits instead, quietly at debug however long, while a required check
// runs (BLOCKED), and enters the queue once GitHub reports the pull request mergeable: auto-merge is
// never armed.
func TestAREADYMergeOnAMergeQueueBaseNeverArmsAutoMerge(t *testing.T) {
	github := newQueueGitHub(t, "BLOCKED")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	clock := time.Now().UTC()
	row := mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock)
	row.Attempts = waitWarnAttempts
	enqueueOutbox(t, pool, records, row)
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runner := mergeRunner(pool, records, github.url, log, func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce with a required check running: %v", err)
	}
	if _, arms := github.counts(); arms != 0 {
		t.Fatalf("enables %d while a required check runs on a merge-queue base, want none: GitHub would arm auto-merge", arms)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	var levels []string
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Level, Kind string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry.Kind == string(record.OutboxKindReadyMerge) {
			levels = append(levels, entry.Level)
		}
	}
	if len(levels) != 1 || levels[0] != "DEBUG" {
		t.Fatalf("the waiting row logged at %v, want one line at DEBUG", levels)
	}
	github.set(func(g *armableGitHub) { g.mergeState = "CLEAN" })
	clock = clock.Add(time.Hour)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce once the required checks passed: %v", err)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if github.arms != 1 || !github.queued || github.armed {
		t.Fatalf("enables %d, queued %t, armed %t once GitHub reports the pull request mergeable; want one enable that enqueues it and never an arm", github.arms, github.queued, github.armed)
	}
}

// A READY merge row waiting on an arm at READY's head ends once the merged pull request's webhook
// moves the record past awaiting its merge, even while GitHub's read still shows the arm.
func TestAREADYMergeRowWaitingOnAnArmEndsOnceTheMergeMovesTheRecord(t *testing.T) {
	github := newArmableGitHub(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	if _, err := pool.Exec(context.Background(), "update issues set phase = 'done', status = 'done' where key = 'LEGION-208'"); err != nil {
		t.Fatalf("apply the merge to the record: %v", err)
	}
	clock = clock.Add(time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 0)
	if _, arms := github.counts(); arms != 0 {
		t.Fatalf("arms %d, want none sent for an arm that stood", arms)
	}
}

// armableGitHub is GitHub's GraphQL API for acme/widgets#42 at ready-head as a ready_merge row and a
// disarm read and change it: the pull request's state and merge state, whether its base requires a
// merge queue (its repository allowing auto-merge either way), whether it is armed for auto-merge or
// sits in the merge queue, what state the queue has that entry in and the commit the queue built it
// on (queueState, queueBase: QUEUED on nothing built yet when empty), the branch the pull request
// merges into with that branch's tip (baseName, baseTip: queueBaseBranch at queueBaseTip when
// empty), what GitHub answers for the pull request's own mergeability (mergeable: null when
// empty), and its timeline of arms and removals. An enablePullRequestAutoMerge at ready-head
// enqueues it on a merge-queue base GitHub reports mergeable now and arms auto-merge otherwise,
// as GitHub does, and a disablePullRequestAutoMerge turns auto-merge off; each lands on
// the timeline as the implement App's (implementApp), and both are counted (arms counts every
// enable). A removal outside the daemon is remove's. jobLogs is the log of each Actions job of
// a merge group by its id, as GET /actions/jobs/{id}/logs answers it — a job it holds none for
// is GitHub's 404, as a check run no Actions job backs would answer — and logReads counts the
// reads of them the daemon made. logArrived, where a test gives one, is closed as the first
// such read reaches the fake, which then holds the answer open until the request's own context
// ends: the shape a daemon stopping while a removal's logs are read is proved in. The fake
// holds its own lock while it waits there, so a test that gives one reads nothing of the fake
// until the read it stopped has answered.
type armableGitHub struct {
	url                   string
	mu                    sync.Mutex
	state, mergeState     string
	mergeable             string
	queueRequired         bool
	armed, queued         bool
	queueState, queueBase string
	baseName, baseTip     string
	timeline              []string
	disables, arms        int
	jobLogs               map[int64]string
	logReads              int
	logArrived            chan struct{}
	// changedFiles is what GET /pulls/42/files answers, the paths a merge hold is matched
	// against. filesCut has GitHub answer a list cut at githubrest.PullRequestFilesLimit instead,
	// the list a pull request of three thousand files or more gets: its names are the fake's own
	// and match no hold any test sets, so a row held on such a list is held by the cut alone.
	changedFiles []string
	filesCut     bool
	// queue is the merge queue GitHub answers for the base branch, oldest entry first, and
	// queueTotal GitHub's own totalCount for the whole queue, which is above the entries listed
	// where the queue is longer than one page. queueRefused has GitHub refuse the read.
	queue        []queueEntry
	queueTotal   int
	queueRefused bool
	// enqueueRefusal is GitHub's answer to the jump's enqueue where GitHub refuses it, empty
	// where GitHub takes it, and enqueueStatus the HTTP status GitHub answers that same enqueue
	// with instead of any GraphQL answer, which is GitHub deciding the request itself — a 403
	// the token is refused by, which settles the jump, as much as the 502 GitHub's edge sends
	// for an API that decided nothing, which settles nothing (ghautomerge's jumpEnded).
	// enqueueNoData has GitHub answer that enqueue 2xx with no data in the document at all,
	// which is GitHub answering and saying nothing of the mutation
	// (githubrest.ErrGraphQLNoData). enqueueUnanswered leaves that enqueue with no answer at
	// all: the fake takes the connection over and closes it, so the client is left with a
	// transport error and nothing of GitHub's to read. enqueues is how many enqueues were sent.
	enqueueRefusal    string
	enqueueStatus     int
	enqueueNoData     bool
	enqueueUnanswered bool
	enqueues          int
	// atEnqueue, where a test gives one, runs as the jump's enqueue reaches the fake and before
	// the fake answers it: the one moment from which a test can read what Legion has written
	// about a jump whose mutation nothing has answered yet, since the transaction that spends
	// the mark writes the record and the issue's message and only then is this request sent
	// (readyMergeMarks.Jump). It reads nothing of the fake, which holds its own lock here.
	atEnqueue func()
	// requests is every request the fake answered, in order, each named by what it was: "read",
	// "files", "queue", "enqueue", "enable", "disable" and "timeline". A test that must know one
	// read happened before one mutation reads it here, since the order is the whole of what it
	// asserts.
	requests []string
}

// queueEntry is one entry of the merge queue the fake answers: the pull request it is for, its
// place in the queue, whether it jumped and the state GitHub has it in.
type queueEntry struct {
	number   int
	position int
	jump     bool
	state    string
}

// queueBaseBranch is the branch acme/widgets#42 merges into and queueBaseTip the commit that
// branch is at, the commit the merge queue builds an entry on that nothing stands before.
const (
	queueBaseBranch = "main"
	queueBaseTip    = "base-tip"
)

// implementApp is the implement App's login as GitHub's timeline names an actor: its bot login
// without the [bot] suffix that GraphQL's viewer gives it.
const implementApp = "legion-implementer"

// newArmableGitHub is armableGitHub for a repository that allows auto-merge and has no merge queue,
// the pull request open, armed and BLOCKED on its required checks.
func newArmableGitHub(t *testing.T) *armableGitHub {
	t.Helper()
	return serveArmable(t, &armableGitHub{state: "OPEN", mergeState: "BLOCKED", armed: true, timeline: []string{timelineEvent("AutoMergeEnabledEvent")}})
}

// newQueueGitHub is armableGitHub for a base that requires a merge queue, the pull request open in
// mergeState with nothing armed.
func newQueueGitHub(t *testing.T, mergeState string) *armableGitHub {
	t.Helper()
	return serveArmable(t, &armableGitHub{state: "OPEN", mergeState: mergeState, queueRequired: true})
}

func serveArmable(t *testing.T, g *armableGitHub) *armableGitHub {
	t.Helper()
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		defer g.mu.Unlock()
		// A job's log is the one read a merge row makes outside GraphQL: GET
		// /actions/jobs/{id}/logs under the repository's REST base (githubrest.RepositoryAPI).
		if job, found := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/actions/jobs/"); found {
			g.logReads++
			if g.logArrived != nil {
				close(g.logArrived)
				g.logArrived = nil
				<-r.Context().Done() // the read is held open until whatever stopped it ends the request
				return
			}
			id, err := strconv.ParseInt(strings.TrimSuffix(job, "/logs"), 10, 64)
			log, held := g.jobLogs[id]
			switch {
			case r.Method != http.MethodGet || !strings.HasSuffix(job, "/logs") || err != nil:
				t.Errorf("unexpected job log request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
			case !held:
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			default:
				_, _ = io.WriteString(w, log)
			}
			return
		}
		// The files a pull request changes are the other REST read a merge row makes: GET
		// /pulls/{number}/files under the same repository base, which the merge holds are
		// matched against (githubrest.PullRequestPaths).
		if r.URL.Path == "/repos/acme/widgets/pulls/42/files" {
			g.requests = append(g.requests, "files")
			_, _ = io.WriteString(w, changedFilesJSON(g))
			return
		}
		switch {
		case strings.Contains(string(body), "disablePullRequestAutoMerge"):
			g.requests = append(g.requests, "disable")
			g.disables++
			g.armed = false
			g.timeline = append(g.timeline, manuallyDisabledEvent(implementApp))
			_, _ = io.WriteString(w, `{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`)
		case strings.Contains(string(body), "enablePullRequestAutoMerge"):
			g.requests = append(g.requests, "enable")
			if !strings.Contains(string(body), `"head":"ready-head"`) {
				t.Errorf("arm request %s, want READY's expected head", body)
			}
			g.arms++
			if g.queueRequired && (g.mergeState == "CLEAN" || g.mergeState == "HAS_HOOKS" || g.mergeState == "UNSTABLE") {
				g.queued = true
				g.timeline = append(g.timeline, timelineEvent("AddedToMergeQueueEvent"))
			} else {
				g.armed = true
				g.timeline = append(g.timeline, timelineEvent("AutoMergeEnabledEvent"))
			}
			_, _ = io.WriteString(w, `{"data":{}}`)
		case strings.Contains(string(body), "enqueuePullRequest"):
			g.requests = append(g.requests, "enqueue")
			g.enqueues++
			if !strings.Contains(string(body), `"jump":true`) && !strings.Contains(string(body), "jump: true") {
				t.Errorf("enqueue request %s, want GitHub's enqueue with jump", body)
			}
			if !strings.Contains(string(body), `"head":"ready-head"`) {
				t.Errorf("enqueue request %s, want READY's expected head", body)
			}
			if g.atEnqueue != nil {
				g.atEnqueue()
			}
			if g.enqueueUnanswered {
				closeWithoutAnswering(t, w)
				return
			}
			if g.enqueueStatus != 0 {
				http.Error(w, `{"message":"Server Error"}`, g.enqueueStatus)
				return
			}
			if g.enqueueNoData {
				_, _ = io.WriteString(w, `{"data":null}`)
				return
			}
			if g.enqueueRefusal != "" {
				_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":"FORBIDDEN","message":%q}]}`, g.enqueueRefusal)
				return
			}
			g.queued = true
			g.timeline = append(g.timeline, timelineEvent("AddedToMergeQueueEvent"))
			_, _ = io.WriteString(w, `{"data":{}}`)
		case strings.Contains(string(body), "MergeQueueEntries"):
			g.requests = append(g.requests, "queue")
			if g.queueRefused {
				_, _ = io.WriteString(w, `{"data":null,"errors":[{"type":"SERVICE_UNAVAILABLE","message":"the merge queue could not be read"}]}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"mergeQueue":{"entries":{"totalCount":%d,"nodes":[%s]}}}}}`,
				max(g.queueTotal, len(g.queue)), queueNodesJSON(g.queue))
		case strings.Contains(string(body), "mergePullRequest"), strings.Contains(string(body), "dequeuePullRequest"):
			t.Errorf("request %s, want only an enable: GitHub merges an armed or queued pull request itself", body)
		case strings.Contains(string(body), "timelineItems"):
			g.requests = append(g.requests, "timeline")
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":%q},"repository":{"pullRequest":{"timelineItems":{"nodes":[%s]}}}}}`,
				implementApp+"[bot]", strings.Join(g.timeline, ","))
		default:
			g.requests = append(g.requests, "read")
			autoMerge, entry := "null", "null"
			if g.armed {
				autoMerge = `{"enabledAt":"2026-10-11T00:00:00Z"}`
			}
			if g.queued {
				queueState, queueBase := g.queueState, "null"
				if queueState == "" {
					queueState = "QUEUED"
				}
				if g.queueBase != "" {
					queueBase = fmt.Sprintf(`{"oid":%q}`, g.queueBase)
				}
				entry = fmt.Sprintf(`{"enqueuedAt":"2026-10-11T00:05:00Z","state":%q,"baseCommit":%s}`, queueState, queueBase)
			}
			mergeable, baseName, baseTip := g.mergeable, g.baseName, g.baseTip
			if mergeable == "" {
				mergeable = "null"
			}
			if baseName == "" {
				baseName = queueBaseBranch
			}
			if baseTip == "" {
				baseTip = queueBaseTip
			}
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"autoMergeAllowed":true,"pullRequest":{"id":"PR_42","state":%q,"title":"feat(daemon): merge the accepted READY head","mergeable":%s,"headRefOid":"ready-head","mergeStateStatus":%q,"updatedAt":%q,"isMergeQueueEnabled":%t,"autoMergeRequest":%s,"mergeQueueEntry":%s,"baseRef":{"name":%q,"target":{"oid":%q}}}}}}`,
				g.state, mergeable, g.mergeState, time.Now().UTC().Format(time.RFC3339), g.queueRequired, autoMerge, entry, baseName, baseTip)
		}
	}})
	return g
}

// closeWithoutAnswering ends a request with no answer of GitHub's at all: the fake takes the
// connection over (http.Hijacker) and closes it without writing a status line, so the client is
// left with a transport error and no answer of GitHub's to read. It is the one shape a request
// nothing came back for has in a test; a 5xx would not do, since GitHub answering 502 is still
// GitHub answering (ghautomerge's jumpEnded).
func closeWithoutAnswering(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Errorf("the fake's ResponseWriter hijacks nothing, so no request of it can go unanswered")
		return
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		t.Errorf("take the connection over to leave the request unanswered: %v", err)
		return
	}
	_ = connection.Close()
}

// set changes GitHub's state, as a mutation landing outside the daemon or GitHub's own merge does.
func (g *armableGitHub) set(change func(*armableGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

// remove takes the pull request out of the merge queue and turns its auto-merge off without a merge,
// as someone other than the daemon's submission does, leaving event, GitHub's record of it, on the
// timeline.
func (g *armableGitHub) remove(event string) {
	g.set(func(g *armableGitHub) {
		g.armed, g.queued = false, false
		g.timeline = append(g.timeline, event)
	})
}

func (g *armableGitHub) counts() (disables, arms int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.disables, g.arms
}

// enqueued is how many jumping enqueues the fake was sent.
func (g *armableGitHub) enqueued() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enqueues
}

// requestLog is every request the fake answered, in the order it answered them: the one way a
// test can say a read happened before a mutation.
func (g *armableGitHub) requestLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

// changedFilesJSON is GitHub's answer to GET /pulls/42/files: the fake's own file names, or a
// list of githubrest.PullRequestFilesLimit names where the fake answers a cut list, which is
// what a pull request GitHub stops listing at answers.
func changedFilesJSON(g *armableGitHub) string {
	names := g.changedFiles
	if g.filesCut {
		names = make([]string, 0, githubrest.PullRequestFilesLimit)
		for index := range githubrest.PullRequestFilesLimit {
			names = append(names, fmt.Sprintf("cut/file-%d.txt", index))
		}
	}
	files := make([]string, 0, len(names))
	for _, name := range names {
		files = append(files, fmt.Sprintf(`{"filename":%q}`, name))
	}
	return "[" + strings.Join(files, ",") + "]"
}

// queueNodesJSON is a merge queue's entries as GitHub's own entries(first: 50) answer lists them.
func queueNodesJSON(entries []queueEntry) string {
	nodes := make([]string, 0, len(entries))
	for _, entry := range entries {
		state := entry.state
		if state == "" {
			state = "QUEUED"
		}
		nodes = append(nodes, fmt.Sprintf(`{"position":%d,"jump":%t,"state":%q,"enqueuedAt":"2026-10-16T04:00:00Z","pullRequest":{"number":%d}}`,
			entry.position, entry.jump, state, entry.number))
	}
	return strings.Join(nodes, ",")
}

// timelineEvent is a timeline item of kind (AddedToMergeQueueEvent or AutoMergeEnabledEvent) as
// GitHub lists it.
func timelineEvent(kind string) string {
	return fmt.Sprintf(`{"__typename":%q,"createdAt":"2026-10-16T05:00:00Z"}`, kind)
}

// manuallyDisabledEvent is GitHub's record of the pull request's auto-merge turned off by actor, a
// person or the implement App's disarm: reason code manually_disabled, with GitHub's sentence for it.
func manuallyDisabledEvent(actor string) string {
	return fmt.Sprintf(`{"__typename":"AutoMergeDisabledEvent","id":"AMDE_1","createdAt":"2026-10-16T05:10:00Z","reason":"Manually disabled by user","reasonCode":"manually_disabled","actor":{"login":%q}}`,
		actor)
}

// dequeuedEvent is GitHub's record of the pull request taken out of the merge queue by actor for
// reason, the merge group commit the queue built for it (failedMergeGroup) or null.
func dequeuedEvent(reason, actor, commit string) string {
	return fmt.Sprintf(`{"__typename":"RemovedFromMergeQueueEvent","id":"RFMQE_1","createdAt":"2026-10-16T05:10:00Z","reason":%q,"actor":{"login":%q},"beforeCommit":%s}`,
		reason, actor, commit)
}

// failedMergeGroupRun is the URL of the merge_group run that failed on the commit the merge queue
// built for #42 (failedMergeGroup).
const failedMergeGroupRun = "https://github.com/acme/widgets/actions/runs/7"

// failedMergeGroup is the commit the merge queue built for #42, on which its merge_group run's
// integration job failed and its lint job passed.
const failedMergeGroup = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedMergeGroupRun + `/job/1","checkSuite":{"workflowRun":{"url":"` + failedMergeGroupRun + `"}}},` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedMergeGroupRun + `/job/2","checkSuite":{"workflowRun":{"url":"` + failedMergeGroupRun + `"}}}]}}}`

// failedMergeGroupJob is the Actions job the integration check run of failedMergeGroupWithJob
// ran as, GitHub's databaseId for that check run and the id its log is read by, and
// failedMergeGroupLog that job's log as GitHub's job-log route carries it, a Go test binary's
// end with one failure in it.
const (
	failedMergeGroupJob = 128479266994
	failedMergeGroupLog = "2026-11-07T18:04:23.8502586Z --- FAIL: TestTheQueueTakesTheEntryOut (0.01s)\n" +
		"2026-11-07T18:04:24.8502586Z FAIL\n"
)

// failedMergeGroupWithJob is failedMergeGroup with the Actions job GitHub names the failing
// check run's: the merge group whose failing job has a log to read.
const failedMergeGroupWithJob = `{"oid":"group-sha","statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"` + failedMergeGroupRun + `/job/1","databaseId":1,"checkSuite":{"workflowRun":{"url":"` + failedMergeGroupRun + `"}}},` +
	`{"__typename":"CheckRun","name":"integration","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"` + failedMergeGroupRun + `/job/2","databaseId":128479266994,"checkSuite":{"workflowRun":{"url":"` + failedMergeGroupRun + `"}}}]}}}`

// A reopen at READY's head that lands while a worker is recording GitHub's permanent refusal takes
// the row from that worker: the worker's notice rolls back with its finish, and the run that holds
// the row afterwards tells the architect, so the refusal is told once. The worker whose finish
// rolled back logs the refusal it could not record beside its finish's failure.
func TestAReopenDuringAPermanentRefusalTellsTheArchitectOnce(t *testing.T) {
	const squash = "Squash merges are not allowed on this repository."
	github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, refusedAnswer(squash))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	clock := now
	reopening := &reopensBeforeFinish{Store: records, t: t, pool: pool}
	var logged bytes.Buffer
	if err := mergeRunner(pool, reopening, github.url, slog.New(slog.NewJSONHandler(&logged, nil)), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if !reopening.reopened {
		t.Fatal("the refusal finished without the reopen landing first")
	}
	assertMergeRows(t, pool, 1, "merge-refused", 0)
	logsCause := false
	for line := range strings.Lines(logged.String()) {
		var entry struct{ Level, Msg, Cause string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		logsCause = logsCause || entry.Level == "ERROR" && strings.Contains(entry.Msg, "finish not recorded") && strings.Contains(entry.Cause, squash)
	}
	if !logsCause {
		t.Fatalf("the worker whose finish rolled back logged %s, want its failed finish logged at ERROR with the refusal as its cause", logged.String())
	}
	clock = clock.Add(time.Minute)
	if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock }).RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	assertMergeRows(t, pool, 0, "merge-refused", 1)
}

// reopen applies a reopen of LEGION-208's pull request at READY's head as the workflow does: the
// record holds it open, and READY's merge is queued again (queueReadyMerge).
func reopen(t *testing.T, pool *pgxpool.Pool, records record.Store) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutPullRequest(context.Background(), tx, readyPullRequest("LEGION-208", record.PullRequestOpen)); err != nil {
			return err
		}
		row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, time.Now())
		if err != nil {
			return err
		}
		return records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		t.Errorf("apply the reopen: %v", err)
	}
}

// reopensBeforeFinish is a record store whose first finish of a row, plain or lease-owned, lets a
// reopen at READY's head (reopen) commit first, as one landing after the row's worker read the
// record and before that worker finishes the row.
type reopensBeforeFinish struct {
	record.Store
	t        *testing.T
	pool     *pgxpool.Pool
	reopened bool
}

func (s *reopensBeforeFinish) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	s.reopenOnce()
	return s.Store.FinishOutbox(ctx, tx, id, leaseToken)
}

func (s *reopensBeforeFinish) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	s.reopenOnce()
	return s.Store.FinishLeasedOutbox(ctx, tx, id, leaseToken)
}

func (s *reopensBeforeFinish) reopenOnce() {
	if !s.reopened {
		s.reopened = true
		reopen(s.t, s.pool, s.Store)
	}
}

// A rate-limited ready_merge waits as long as GitHub asks, longer than the outbox's own backoff,
// whether GitHub answers the GraphQL mutation with RATE_LIMITED and 200 or with a 429.
func TestARateLimitedREADYMergeWaitsGitHubsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter)
		wait   time.Duration
	}{
		{"GraphQL's RATE_LIMITED", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "180")
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		}, 180 * time.Second},
		{"a 429", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "120")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
		}, 120 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newMergeGitHub(t, func(int) string { return pullJSON("OPEN", "ready-head", "CLEAN") }, tc.answer)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
			if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if nextAt, _ := outboxRowState(t, pool, "LEGION-208"); !nextAt.Equal(now.Add(tc.wait)) {
				t.Fatalf("rate-limited READY merge next_at = %s, want GitHub's retry-after %s", nextAt, now.Add(tc.wait))
			}
		})
	}
}

// A GitHub rate limit defers the rows after it only where they would spend the same budget.
// The implement App's REST core budget (an issue branch's create) and its GraphQL budget (a disarm,
// a READY merge) are separate, so a REST core limit never holds back a disarm, the effect that keeps
// an unreviewed head from merging, nor a GraphQL limit an issue branch. GitHub's secondary limit
// (a Retry-After) covers every call the token makes. A deferred row is not run in the batch at all,
// and its last error names the row whose limit deferred it.
func TestARateLimitDefersOnlyTheRowsThatSpendItsBudget(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	primary := func(resource string) map[string]string {
		return map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": resource}
	}
	for _, tc := range []struct {
		name          string
		first, second record.OutboxPayload
		restLimit     map[string]string
		mergeLimit    map[string]string
		deferred      bool
		wait          time.Duration
	}{
		{name: "a REST core limit leaves a disarm behind it to run", first: record.IssueBranch{Generation: 1}, second: record.AutoMergeDisable{Number: 43},
			restLimit: primary("core")},
		{name: "a GraphQL limit defers a disarm behind it", first: record.ReadyMerge{Number: 42, Head: "ready-head"}, second: record.AutoMergeDisable{Number: 43},
			mergeLimit: primary("graphql"), deferred: true, wait: 9 * time.Minute},
		{name: "a GraphQL limit leaves an issue branch behind it to run", first: record.ReadyMerge{Number: 42, Head: "ready-head"}, second: record.IssueBranch{Generation: 1},
			mergeLimit: primary("graphql")},
		{name: "a secondary limit defers an issue branch behind it", first: record.ReadyMerge{Number: 42, Head: "ready-head"}, second: record.IssueBranch{Generation: 1},
			mergeLimit: map[string]string{"Retry-After": "180"}, deferred: true, wait: 180 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newBudgetGitHub(t, tc.restLimit, tc.mergeLimit)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-209", Title: "Disarmed", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-210", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-210", Title: "Branch", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})
			issueOf := func(payload record.OutboxPayload) string {
				switch payload.(type) {
				case record.ReadyMerge:
					return "LEGION-208"
				case record.AutoMergeDisable:
					return "LEGION-209"
				default:
					return "LEGION-210"
				}
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issueOf(tc.first), tc.first, now))
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issueOf(tc.second), tc.second, now))
			var firstID int64
			if err := pool.QueryRow(context.Background(), "select id from outbox where issue = $1", issueOf(tc.first)).Scan(&firstID); err != nil {
				t.Fatalf("read the first row's id: %v", err)
			}
			if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			second := issueOf(tc.second)
			ran := github.calledFor(second)
			if ran == tc.deferred {
				t.Fatalf("GitHub calls for %s's row: %t, want deferred %t", second, ran, tc.deferred)
			}
			if !tc.deferred {
				return
			}
			nextAt, lastError := outboxRowState(t, pool, second)
			if nextAt.Before(now.Add(tc.wait)) || !strings.Contains(lastError, fmt.Sprintf("row %d", firstID)) {
				t.Fatalf("the deferred row waits until %s with last error %q; want no earlier than %s, naming row %d", nextAt, lastError, now.Add(tc.wait), firstID)
			}
		})
	}
}

// The REST core and GraphQL budgets are exhausted independently, so the runner remembers each limit
// its rows met: a REST core limit met after a GraphQL one leaves the GraphQL limit standing, and a
// disarm behind both is deferred rather than sent against an exhausted budget.
func TestARateLimitedBatchRemembersEveryBudgetItExhausted(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	primary := func(resource string) map[string]string {
		return map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": resource}
	}
	github := newBudgetGitHub(t, primary("core"), primary("graphql"))
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-209", Title: "Disarmed", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-210", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-210", Title: "Branch", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-210", record.IssueBranch{Generation: 1}, now))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.AutoMergeDisable{Number: 43}, now))
	if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !github.calledFor("LEGION-208") || !github.calledFor("LEGION-210") {
		t.Fatal("the READY merge and the issue branch, each the first of its budget, did not run")
	}
	if github.calledFor("LEGION-209") {
		t.Fatal("the disarm called GraphQL in the batch whose first row met GraphQL's limit")
	}
}

// A GitHub rate limit stands until its deadline, not until its batch ends, and a row that spends
// several limited budgets waits for the latest of their deadlines. A disarm queued after the batch
// that met GitHub's secondary limit is deferred to that limit's end, then runs once it has passed;
// and an issue branch behind a 30-second REST core limit and a 180-second secondary limit waits the
// 180 seconds.
func TestARateLimitStandsUntilTheLatestDeadlineOfTheBudgetsARowSpends(t *testing.T) {
	secondary := map[string]string{"Retry-After": "180"}
	t.Run("across batches", func(t *testing.T) {
		github := newBudgetGitHub(t, nil, secondary)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-209", Title: "Disarmed", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
		start := time.Now().UTC().Truncate(time.Microsecond)
		clock := start
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, clock))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("the batch that meets the limit: %v", err)
		}
		clock = start.Add(time.Minute)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.AutoMergeDisable{Number: 43}, clock))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("the next batch: %v", err)
		}
		if github.calledFor("LEGION-209") {
			t.Fatal("the disarm called GitHub inside the secondary limit a batch before it met")
		}
		if nextAt, _ := outboxRowState(t, pool, "LEGION-209"); !nextAt.Equal(start.Add(180 * time.Second)) {
			t.Fatalf("the deferred disarm is due at %s, want the limit's end %s", nextAt, start.Add(180*time.Second))
		}
		// The READY merge no longer has a READY to submit, so its next run calls no mutation.
		if _, err := pool.Exec(context.Background(), "update issues set phase = 'implementing' where key = 'LEGION-208'"); err != nil {
			t.Fatalf("withdraw the READY: %v", err)
		}
		clock = start.Add(181 * time.Second)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("the batch after the limit: %v", err)
		}
		if !github.calledFor("LEGION-209") {
			t.Fatal("the disarm did not run once the limit had passed")
		}
	})
	t.Run("overlapping REST core and secondary limits", func(t *testing.T) {
		core := map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10), "X-RateLimit-Resource": "core"}
		github := newBudgetGitHub(t, core, secondary)
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		for _, key := range []string{"LEGION-210", "LEGION-211"} {
			putOutboxIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: key, Title: "Branch", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-210", record.IssueBranch{Generation: 1}, now))
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now))
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-211", record.IssueBranch{Generation: 1}, now))
		if err := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }).RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if nextAt, _ := outboxRowState(t, pool, "LEGION-211"); !nextAt.Equal(now.Add(180 * time.Second)) {
			t.Fatalf("the issue branch behind both limits is due at %s, want the later one's end %s", nextAt, now.Add(180*time.Second))
		}
	})
}

// replacePayload is the one lease-fenced payload write a merge row makes, and a run can owe more
// than one row beside it: a run a merge hold holds back writes the payload, posts one message on
// the Dispatch issue naming the hold and enqueues one notice for the architect. Every told row
// goes into that one transaction, in the order given, so the payload and the rows that tell of it
// commit together. A replacement whose lease has moved to another worker writes nothing at all:
// neither the payload nor one told row, since that worker reads GitHub itself and would otherwise
// find the telling already spent.
func TestAMergePayloadWriteEnqueuesEveryToldRowUnderTheRowsLease(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	runner := mergeRunner(pool, records, "", quietLogger(), func() time.Time { return now })
	row := record.OutboxRow{Issue: "LEGION-208"}
	if err := pool.QueryRow(ctx, `select id from outbox where kind = 'ready_merge'`).Scan(&row.ID); err != nil {
		t.Fatalf("read the ready_merge row: %v", err)
	}
	row.LeaseToken = leaseOutboxRow(t, pool, row.ID)

	held := record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now, HeldSince: now, HeldTold: true}
	message := mustOutboxRow(t, "LEGION-208", record.MessagePost{Body: "A merge hold holds this merge back."}, now)
	notice := mustOutboxRow(t, "LEGION-208", record.Notice{Kind: "merge-held", Role: claim.RoleArchitect, Reason: "a merge hold holds the merge back"}, now)
	replaced, err := runner.replacePayload(ctx, row, held, message, notice)
	if err != nil || !replaced {
		t.Fatalf("replacePayload = %v, %v; want the replacement to take", replaced, err)
	}
	if kinds := toldRowKinds(t, pool); kinds != "dispatch_message,notice" {
		t.Fatalf("the told rows are %q, want the message and the notice in the order they were given", kinds)
	}
	var toldMark bool
	if err := pool.QueryRow(ctx, `select coalesce((payload->>'heldTold')::boolean, false) from outbox where kind = 'ready_merge'`).Scan(&toldMark); err != nil {
		t.Fatalf("read the replaced payload: %v", err)
	}
	if !toldMark {
		t.Fatal("the ready_merge payload does not carry the telling the told rows were written for")
	}

	// The same write under a lease another worker holds now: the payload stands as it was and
	// neither told row is enqueued.
	stolen := row
	stolen.LeaseToken = "another-worker"
	second := mustOutboxRow(t, "LEGION-208", record.MessagePost{Body: "A second merge hold holds this merge back."}, now)
	third := mustOutboxRow(t, "LEGION-208", record.Notice{Kind: "merge-waiting", Role: claim.RoleArchitect, Reason: "a second merge hold holds the merge back"}, now)
	replaced, err = runner.replacePayload(ctx, stolen, record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, second, third)
	if err != nil || replaced {
		t.Fatalf("replacePayload under a lease that moved = %v, %v; want it to take nothing", replaced, err)
	}
	if kinds := toldRowKinds(t, pool); kinds != "dispatch_message,notice" {
		t.Fatalf("the told rows are %q, want the two the owning lease wrote and no more", kinds)
	}
	if err := pool.QueryRow(ctx, `select coalesce((payload->>'heldTold')::boolean, false) from outbox where kind = 'ready_merge'`).Scan(&toldMark); err != nil {
		t.Fatalf("read the payload the replacement left alone: %v", err)
	}
	if !toldMark {
		t.Fatal("the replacement that did not take wrote the payload of the worker that did not hold the lease")
	}
}

// toldRowKinds is the kind of every outbox row a merge payload write told, oldest first and comma
// separated: every row of the issue but the ready_merge row the write replaced.
func toldRowKinds(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select kind from outbox where kind <> 'ready_merge' order by id`)
	if err != nil {
		t.Fatalf("read the told rows: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan a told row: %v", err)
		}
		kinds = append(kinds, kind)
	}
	return strings.Join(kinds, ",")
}

// budgetGitHub is GitHub for TestARateLimitDefersOnlyTheRowsThatSpendItsBudget: acme/widgets' REST
// API, which answers an issue branch's read of main with restLimit's headers and a 403 when it is
// set, and GraphQL, where #42 is a READY merge's pull request, mergeable now, whose merge GitHub
// rate-limits with mergeLimit's headers when it is set, and #43 a pull request armed at another
// head, whose disarm GitHub accepts. It records which issue's row called it.
type budgetGitHub struct {
	url    string
	mu     sync.Mutex
	called map[string]bool
}

func newBudgetGitHub(t *testing.T, restLimit, mergeLimit map[string]string) *budgetGitHub {
	t.Helper()
	g := &budgetGitHub{called: map[string]bool{}}
	disabled := false
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/git/") {
			g.called["LEGION-210"] = true
			switch {
			case restLimit != nil:
				for name, value := range restLimit {
					w.Header().Set(name, value)
				}
				http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			case r.Method == http.MethodGet:
				_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+branchBaseCommit+`"}}`)
			default:
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{}`)
			}
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode GraphQL request: %v", err)
		}
		switch {
		case strings.Contains(request.Query, "disablePullRequestAutoMerge"):
			g.called["LEGION-209"] = true
			disabled = true
			_, _ = io.WriteString(w, `{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`)
		case strings.Contains(request.Query, "mergePullRequest"):
			g.called["LEGION-208"] = true
			for name, value := range mergeLimit {
				w.Header().Set(name, value)
			}
			if mergeLimit == nil {
				_, _ = io.WriteString(w, `{"data":{}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
		case request.Variables["number"] == float64(43):
			g.called["LEGION-209"] = true
			_, _ = io.WriteString(w, armedPullJSON("other-head", !disabled))
		default:
			g.called["LEGION-208"] = true
			_, _ = io.WriteString(w, pullJSON("OPEN", "ready-head", "CLEAN"))
		}
	}})
	return g
}

func (g *budgetGitHub) calledFor(issue string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.called[issue]
}

// mergeGitHub is GitHub's GraphQL API as a ready_merge row calls it: read answers the row's read of
// pull request #42 (given how many reads came before it), and mutation answers its merge or arm. It
// counts the merges sent and records every call's bearer, and fails the test on a merge that names
// another expected head than ready-head.
type mergeGitHub struct {
	url     string
	mu      sync.Mutex
	merges  int
	bearers []string
}

func newMergeGitHub(t *testing.T, read func(int) string, mutation func(http.ResponseWriter)) *mergeGitHub {
	t.Helper()
	g := &mergeGitHub{}
	reads := 0
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bearers = append(g.bearers, r.Header.Get("Authorization"))
		isMerge := strings.Contains(string(body), "mergePullRequest") || strings.Contains(string(body), "enablePullRequestAutoMerge")
		if isMerge {
			g.merges++
			if !strings.Contains(string(body), `"head":"ready-head"`) {
				t.Errorf("merge request %s, want READY's expected head", body)
			}
		} else {
			reads++
		}
		count := reads
		g.mu.Unlock()
		if isMerge {
			mutation(w)
			return
		}
		_, _ = io.WriteString(w, read(count))
	}})
	return g
}

func (g *mergeGitHub) merged() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.merges, append([]string(nil), g.bearers...)
}

// mergedAnswer is GitHub accepting a merge; refusedAnswer is GitHub refusing it with message, an
// UNPROCESSABLE a retry meets again.
func mergedAnswer(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"data":{}}`) }

func refusedAnswer(message string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, `{"data":null,"errors":[{"type":"UNPROCESSABLE","message":%q}]}`, message)
	}
}

// pullJSON is ghautomerge's read of acme/widgets#42 as GitHub answers it in a repository that allows
// no auto-merge: the pull request in state at head, in mergeState, with nothing armed, GitHub's last
// change to it made just now (pullJSONAt).
func pullJSON(state, head, mergeState string) string {
	return pullJSONAt(state, head, mergeState, time.Now())
}

// pullJSONAt is pullJSON with GitHub's last change to the pull request (its updatedAt) made at
// updated.
func pullJSONAt(state, head, mergeState string, updated time.Time) string {
	return fmt.Sprintf(`{"data":{"repository":{"autoMergeAllowed":false,"pullRequest":{"id":"PR_42","state":%q,"title":"feat(daemon): merge the accepted READY head","headRefOid":%q,"mergeStateStatus":%q,"updatedAt":%q,"isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null}}}}`,
		state, head, mergeState, updated.UTC().Format(time.RFC3339))
}

// armedPullJSON is a disarm's read of an open pull request at head, its auto-merge enabled while
// armed.
func armedPullJSON(head string, armed bool) string {
	autoMerge := "null"
	if armed {
		autoMerge = `{"enabledAt":"2026-10-11T00:00:00Z"}`
	}
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"id":"PR","state":"OPEN","headRefOid":%q,"autoMergeRequest":%s,"mergeQueueEntry":null}}}}`, head, autoMerge)
}

// readyMergeSeed is the record a ready_merge row of an issue's pull request #42 at ready-head runs
// against: the issue's phase, the merger's READY head and by-hand note, whether the issue's tree
// lingers, and the pull request's recorded state.
type readyMergeSeed struct {
	at        phase.Phase
	ready     string
	mergeNote string
	lingers   bool
	pr        record.PullRequestState
}

// awaitingREADY is the issue awaiting its merge at ready-head, its pull request open.
var awaitingREADY = readyMergeSeed{at: phase.AwaitingMerge, ready: "ready-head", pr: record.PullRequestOpen}

// with is the seed changed by change.
func (s readyMergeSeed) with(change func(*readyMergeSeed)) readyMergeSeed {
	change(&s)
	return s
}

// seedReadyMerge records issue as seed has it.
func seedReadyMerge(t *testing.T, pool *pgxpool.Pool, records record.Store, issue string, seed readyMergeSeed) {
	t.Helper()
	child := record.Issue{Key: issue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: issue, Title: "Workflow", Phase: seed.at, Generation: 1, Status: "retro", Rank: "U"}
	if seed.lingers {
		until, parent := time.Now().Add(time.Hour), "LEGION-207"
		putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-207", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-207", Title: "Root", Phase: phase.Done,
			Generation: 1, Status: "done", Rank: "T", LingerUntil: &until})
		child.Tree, child.Parent = "LEGION-207", &parent
	}
	putOutboxIssue(t, pool, records, child)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: issue, Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: seed.ready, MergeNote: seed.mergeNote}); err != nil {
			return err
		}
		return records.PutPullRequest(context.Background(), tx, readyPullRequest(issue, seed.pr))
	}); err != nil {
		t.Fatalf("seed %s's READY: %v", issue, err)
	}
}

// readyPullRequest is issue's pull request #42 at ready-head, recorded in state.
func readyPullRequest(issue string, state record.PullRequestState) record.PullRequest {
	return record.PullRequest{State: state, Issue: issue, Repo: "acme/widgets", Number: 42, Branch: "legion/" + issue, HeadSHA: "ready-head", Failing: []string{}}
}

// mergeRunner is the outbox the merge tests drive: as the implement App (roleTokens), for the
// repository each issue's record names (acme/widgets), against GitHub at api, logging to log, on
// the clock now.
func mergeRunner(pool *pgxpool.Pool, records record.Store, api string, log *slog.Logger, now func() time.Time) *outbox {
	return &outbox{log: log, pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		githubAPI: api, now: now}
}

// leaseStealer is a record store whose ClaimRow lets another worker take every row it hands out, as
// a worker too slow for its lease meets: it claims the row again in the same transaction, as of the
// moment its lease expires, so the row it returns holds a stale lease.
type leaseStealer struct{ record.Store }

func (s leaseStealer) ClaimRow(ctx context.Context, tx pgx.Tx, id int64, now time.Time, lease time.Duration) (record.OutboxRow, bool, error) {
	row, ok, err := s.Store.ClaimRow(ctx, tx, id, now, lease)
	if err != nil || !ok {
		return row, ok, err
	}
	if _, _, err := s.Store.ClaimRow(ctx, tx, id, now.Add(lease), lease); err != nil {
		return record.OutboxRow{}, false, err
	}
	return row, true, nil
}

// assertMergeRows checks the outbox holds ready ready_merge rows and notices notices of noticeKind,
// and returns those notices' reasons, oldest first.
func assertMergeRows(t *testing.T, pool *pgxpool.Pool, ready int, noticeKind string, notices int) []string {
	t.Helper()
	var readyRows int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'ready_merge'`).Scan(&readyRows); err != nil {
		t.Fatalf("count ready_merge rows: %v", err)
	}
	rows, err := pool.Query(context.Background(), `select payload->>'role', payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = $1 order by id`, noticeKind)
	if err != nil {
		t.Fatalf("read %s notices: %v", noticeKind, err)
	}
	defer rows.Close()
	var reasons []string
	for rows.Next() {
		var role, reason string
		if err := rows.Scan(&role, &reason); err != nil {
			t.Fatalf("scan a %s notice: %v", noticeKind, err)
		}
		if role != string(claim.RoleArchitect) {
			t.Fatalf("a %s notice for %q, want the architect", noticeKind, role)
		}
		reasons = append(reasons, reason)
	}
	if readyRows != ready || len(reasons) != notices {
		t.Fatalf("ready_merge rows %d and %s notices %q; want %d and %d", readyRows, noticeKind, reasons, ready, notices)
	}
	return reasons
}

// readyMergeState is the one ready_merge row's state: when it runs next, the error it last failed
// with, whether it is recorded as having told its architect (record.ReadyMerge.Warned), and when it
// was first queued into the outbox (created_at, which a reopen's upsert leaves where it is).
func readyMergeState(t *testing.T, pool *pgxpool.Pool) (time.Time, string, bool, time.Time) {
	t.Helper()
	var nextAt, createdAt time.Time
	var lastError string
	var warned bool
	if err := pool.QueryRow(context.Background(),
		`select next_at, last_error, coalesce((payload->>'warned')::boolean, false), created_at from outbox where kind = 'ready_merge'`).
		Scan(&nextAt, &lastError, &warned, &createdAt); err != nil {
		t.Fatalf("read the ready_merge row: %v", err)
	}
	return nextAt, lastError, warned, createdAt
}

// outboxRowState is when issue's one outbox row runs next and the error it last failed with.
func outboxRowState(t *testing.T, pool *pgxpool.Pool, issue string) (time.Time, string) {
	t.Helper()
	var nextAt time.Time
	var lastError string
	if err := pool.QueryRow(context.Background(), `select next_at, last_error from outbox where issue = $1`, issue).Scan(&nextAt, &lastError); err != nil {
		t.Fatalf("read %s's outbox row: %v", issue, err)
	}
	return nextAt, lastError
}

// The controller's jump mark sends READY's merge to the head of GitHub's merge queue: the one
// mutation of such a run is GitHub's enqueue with jump, carrying the head the row names, and the
// mark is spent at that mutation and nowhere else. Every other submission is what it was before
// marks existed: a merge-queue base GitHub does not report mergeable yet sends nothing at all, a
// base with no merge queue sends the arm it always sent and leaves the mark where it is for the
// submission that can spend it, and an unmarked issue sends today's enable with today's
// variables. A mark is spent whether or not GitHub takes the jump (a refusal falls back to the
// enable in the same attempt), and a run whose lease moved to another worker sends no mutation at
// all and spends nothing: the worker that holds the row makes that submission, mark and all.
func TestAJumpMarkSendsREADYsMergeToTheHeadOfTheMergeQueue(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mergeState is what GitHub answers for the pull request, queue whether its base requires
		// a merge queue, and marked whether the controller marked the issue.
		mergeState string
		queue      bool
		marked     bool
		// refuseEnqueue has GitHub refuse the jumping enqueue, and steal has another worker take
		// the row's lease before the spend transaction runs.
		refuseEnqueue bool
		steal         bool
		// runs is how many times the row is drained, enqueues how many jumping enqueues GitHub
		// was sent, arms how many enables, and standing whether the mark is still in the record.
		runs           int
		enqueues, arms int
		standing       bool
	}{
		{name: "a marked merge GitHub reports mergeable now", mergeState: "CLEAN", queue: true, marked: true, runs: 1, enqueues: 1},
		{name: "a marked merge GitHub does not report mergeable yet", mergeState: "BLOCKED", queue: true, marked: true, runs: 1, standing: true},
		{name: "a mark spent at a mutation GitHub refused", mergeState: "CLEAN", queue: true, marked: true, refuseEnqueue: true, runs: 2, enqueues: 1, arms: 1},
		{name: "an unmarked merge", mergeState: "CLEAN", queue: true, runs: 1, arms: 1},
		{name: "a marked merge on a base with no merge queue", mergeState: "BLOCKED", marked: true, runs: 1, arms: 1, standing: true},
		{name: "a marked merge whose spend finds the lease gone", mergeState: "CLEAN", queue: true, marked: true, steal: true, runs: 1, standing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := serveArmable(t, &armableGitHub{state: "OPEN", mergeState: tc.mergeState, queueRequired: tc.queue})
			if tc.refuseEnqueue {
				github.set(func(g *armableGitHub) { g.enqueueRefusal = "Only administrators can jump the merge queue." })
			}
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Now().UTC().Truncate(time.Second)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
			if tc.marked {
				setMergeJump(t, pool, records, "LEGION-208", "the release waits on this merge", now)
			}
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			if tc.steal {
				// Every claim this runner makes hands the row to another worker in the same
				// transaction, so the lease the run holds is stale by the time it spends the mark.
				runner.records = leaseStealer{records}
			}
			for run := range tc.runs {
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("RunOnce %d: %v", run+1, err)
				}
				clock = clock.Add(time.Minute)
			}
			_, arms := github.counts()
			if github.enqueued() != tc.enqueues || arms != tc.arms {
				t.Fatalf("enqueues %d and enables %d, want %d and %d: %v", github.enqueued(), arms, tc.enqueues, tc.arms, github.requestLog())
			}
			if standing := mergeJumpStands(t, pool, "LEGION-208"); standing != tc.standing {
				t.Fatalf("the jump mark of LEGION-208 stands %t, want %t: the mark is spent at the mutation and nowhere else", standing, tc.standing)
			}
		})
	}
}

// A conflict resubmission never jumps. The mark is the controller's instruction about READY's
// merge, and the further submission the merge queue's conflict removal calls for is made with no
// marks at all (readyMerge's resubmit): it sends today's enable, and a mark set while the row ran
// is left where it is for a submission that can spend it.
func TestAConflictResubmissionNeverJumpsTheMergeQueue(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	run := func(step string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	run("READY's own submission")
	// The controller marks the issue while the merge sits in the queue, and the queue then takes
	// the entry out for a conflict with an entry ahead of it.
	setMergeJump(t, pool, records, "LEGION-208", "the release waits on this merge", clock)
	github.remove(dequeuedEvent("merge_conflict", "github-merge-queue", "null"))
	run("the read that finds the removal")
	_, arms := github.counts()
	if github.enqueued() != 0 || arms != 2 {
		t.Fatalf("enqueues %d and enables %d, want none and two: a resubmission sends today's mutation", github.enqueued(), arms)
	}
	if !mergeJumpStands(t, pool, "LEGION-208") {
		t.Fatalf("the jump mark of LEGION-208 is gone: a resubmission spends no mark")
	}
}

// The record of a jump is the account of the queue the merge was asked to go ahead of, and it
// can be taken at one moment only: before the enqueue lands, since the queue as it stood is gone
// once it has. So the queue is read before the mutation, and what that read answered — or that
// it answered nothing at all — is written with the issue, the repository, the pull request, the
// head, the time and the controller's reason. One record stands per repository: the newest jump
// replaces the one before it. Nothing in it says where the merge then stood, since at the moment
// it is written nothing has been sent.
func TestAJumpRecordNamesTheQueueItAskedToGoAheadOf(t *testing.T) {
	const reason = "the release waits on this merge"
	t.Run("the queue is read before the enqueue and recorded with the jump", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		github.set(func(g *armableGitHub) {
			g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}, {number: 12, position: 2, state: "QUEUED"}}
		})
		pool, records, runner, clock := jumpRunner(t, github, reason)
		var logs bytes.Buffer
		runner.log = slog.New(slog.NewJSONHandler(&logs, nil))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if order := requestOrder(github.requestLog(), "queue", "enqueue"); !order {
			t.Fatalf("requests %v, want the queue read before the enqueue: once the enqueue lands the queue as it stood is gone", github.requestLog())
		}
		jumped := onlyJumpRecord(t, pool, records)
		switch {
		case jumped.Issue != "LEGION-208" || jumped.Repository != "acme/widgets" || jumped.Number != 42 || jumped.Head != "ready-head":
			t.Fatalf("the jump record names %s, %s, #%d at %s, want LEGION-208, acme/widgets, #42 at ready-head", jumped.Issue, jumped.Repository, jumped.Number, jumped.Head)
		case !jumped.At.Equal(clock):
			t.Fatalf("the jump record was made at %s, want the daemon's clock %s", jumped.At, clock)
		case jumped.Reason != reason:
			t.Fatalf("the jump record's reason %q, want the controller's %q", jumped.Reason, reason)
		case jumped.Outcome != record.MergeJumpAccepted || jumped.Answer != "" || jumped.EntriesUnknown:
			t.Fatalf("the jump record ended %s with %q and unknown entries %t, want GitHub's acceptance, nothing quoted and the queue it read", jumped.Outcome, jumped.Answer, jumped.EntriesUnknown)
		case len(jumped.Entries) != 2 || jumped.Entries[0].Number != 11 || jumped.Entries[1].Number != 12 || jumped.EntriesTotal != 2:
			t.Fatalf("the jump record holds %v of %d entries, want #11 and #12", jumped.Entries, jumped.EntriesTotal)
		case jumped.Entries[0].State != "MERGEABLE" || jumped.Entries[0].Position != 1:
			t.Fatalf("the first recorded entry is %+v, want GitHub's own position and state", jumped.Entries[0])
		}
		posts := messagePosts(t, pool)
		if len(posts) != 1 {
			t.Fatalf("Dispatch messages %q, want the one that posts the jump", posts)
		}
		for _, name := range []string{"#42", "ready-head", reason, "#11", "#12"} {
			if !strings.Contains(posts[0], name) {
				t.Fatalf("the message posted on the issue %q, want it to name %q", posts[0], name)
			}
		}
		if lines := strings.Count(logs.String(), "the controller's jump mark, which this run spends"); lines != 1 {
			t.Fatalf("the daemon wrote %d lines for the jump, want one: %s", lines, logs.String())
		}
	})

	t.Run("a second jump of the same repository replaces its record", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		pool, records, runner, clock := jumpRunner(t, github, reason)
		seedReadyMerge(t, pool, records, "LEGION-209", awaitingREADY)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce for LEGION-208: %v", err)
		}
		// GitHub merges the first entry, so the second issue's own submission is made against a
		// pull request with nothing armed, as the daemon always meets one.
		github.set(func(g *armableGitHub) { g.queued = false })
		// The second row is queued at the runner's own clock, which jumpRunner holds still: a row
		// queued at the wall clock instead is due a second later than the runner reads the time
		// whenever a second boundary falls between the two readings, and the run below then finds
		// nothing due and leaves the first jump's record standing.
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-209", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: clock}, clock))
		setMergeJump(t, pool, records, "LEGION-209", "the other release waits on this one", clock)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce for LEGION-209: %v", err)
		}
		jumped := onlyJumpRecord(t, pool, records)
		if jumped.Issue != "LEGION-209" {
			t.Fatalf("the repository's jump record names %s, want the newest jump, LEGION-209", jumped.Issue)
		}
	})

	t.Run("a queue longer than one page is recorded as far as it was read", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		github.set(func(g *armableGitHub) {
			for number := range 50 {
				g.queue = append(g.queue, queueEntry{number: number + 1, position: number + 1})
			}
			g.queueTotal = 73
		})
		pool, records, runner, _ := jumpRunner(t, github, reason)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		jumped := onlyJumpRecord(t, pool, records)
		if len(jumped.Entries) != 50 || jumped.EntriesTotal != 73 {
			t.Fatalf("the jump record holds %d entries of %d, want the 50 one page carries of a queue of 73", len(jumped.Entries), jumped.EntriesTotal)
		}
	})

	t.Run("a queue GitHub would not read leaves the entries unknown", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		github.set(func(g *armableGitHub) { g.queueRefused = true })
		pool, records, runner, clock := jumpRunner(t, github, reason)
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if github.enqueued() != 1 {
			t.Fatalf("enqueues %d, want one: a queue nobody could read is no failure of the jump", github.enqueued())
		}
		jumped := onlyJumpRecord(t, pool, records)
		switch {
		case !jumped.EntriesUnknown || len(jumped.Entries) != 0 || jumped.EntriesTotal != 0:
			t.Fatalf("the jump record holds %v of %d with unknown %t, want no entries and unknown", jumped.Entries, jumped.EntriesTotal, jumped.EntriesUnknown)
		case jumped.Issue != "LEGION-208" || jumped.Number != 42 || jumped.Head != "ready-head" || jumped.Reason != reason || !jumped.At.Equal(clock):
			t.Fatalf("the jump record is %+v, want every other field standing", jumped)
		}
	})
}

// GitHub answering the jump's own mutation with anything but the jump spends the mark all the
// same, and the submission chosen follows in the same attempt: the merge was submitted. The
// issue is told what Legion asked for and what GitHub answered, the record of the jump carries
// those words with the entries the jump had asked to go ahead of, and the controller gets a
// notice of its own — the jump was its instruction. The row is not ended: a jump that did not
// take is no refusal of the merge.
//
// Neither text says where the merge then stood. The daemon asked, read GitHub's answer and
// submitted the plain merge, and GitHub alone knows what its queue did with it.
func TestAJumpRefusedByGitHubFallsBackAndTellsTheController(t *testing.T) {
	const refusal = "Only administrators can jump the merge queue."
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) {
		g.enqueueRefusal = refusal
		g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
	})
	pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	_, arms := github.counts()
	if github.enqueued() != 1 || arms != 1 {
		t.Fatalf("enqueues %d and enables %d, want one of each: the submission chosen follows the refused jump in the same attempt", github.enqueued(), arms)
	}
	jumped := onlyJumpRecord(t, pool, records)
	switch {
	case jumped.Outcome != record.MergeJumpRefused || !strings.Contains(jumped.Answer, refusal):
		t.Fatalf("the jump record ended %s with %q, want it refused with GitHub's own answer", jumped.Outcome, jumped.Answer)
	case len(jumped.Entries) != 1 || jumped.Entries[0].Number != 11:
		t.Fatalf("the refused jump record holds %v, want the entries it had asked to go ahead of", jumped.Entries)
	}
	posts := messagePosts(t, pool)
	if len(posts) != 2 {
		t.Fatalf("Dispatch messages %q, want the jump's and the failure's", posts)
	}
	for _, said := range []string{"Legion asked GitHub to send the merge", "GitHub answered: ", refusal, "Legion submits the merge without the jump in this same attempt"} {
		if !strings.Contains(posts[1], said) {
			t.Fatalf("the failure posted on the issue %q, want it to say %q: what Legion asked, what came back, and what Legion does next", posts[1], said)
		}
	}
	notices := controllerNotices(t, pool)
	if len(notices) != 1 || notices[0].kind != string(record.MergeJumpRefusedNotice) {
		t.Fatalf("controller notices %v, want one merge-jump-refused", notices)
	}
	for _, name := range []string{"LEGION-208", "#42", "ready-head", refusal} {
		if !strings.Contains(notices[0].reason, name) {
			t.Fatalf("the controller notice %q, want it to name %q", notices[0].reason, name)
		}
	}
	// What neither text may say: the daemon never read where the merge stands, and a 502 of
	// GitHub's own edge reaches these same words, so "GitHub refused" would be a verdict nobody
	// has.
	for _, told := range []string{posts[1], notices[0].reason} {
		for _, claimed := range []string{"GitHub refused", "waits in the queue", "behind what was already", "went ahead of", "was not sent to the head"} {
			if strings.Contains(told, claimed) {
				t.Fatalf("%q says %q, which is a reading of GitHub's queue the daemon never made", told, claimed)
			}
		}
	}
	// The row still stands and no merge-refused notice was written: a refused jump never ends
	// the row.
	assertMergeRows(t, pool, 1, "merge-refused", 0)
}

// A jump nothing came back for — the enqueue whose connection closed before any status arrived,
// as a dial that failed or a timeout leaves it — falls back exactly as a refusal does and spends
// the mark the same way, and everything written about it says what it was: Legion asked, and
// does not know what GitHub did with the request. Neither the Dispatch post nor the controller's
// notice puts an answer in GitHub's mouth, and the record of the jump carries the same words
// under record.MergeJumpUnknown, which is what came back deciding nothing. A controller told
// GitHub refused would read the repository's merge-queue rules for a failure of the network, and
// one told where the merge stands would be reading a queue nobody looked at.
func TestAJumpGitHubNeverAnsweredFallsBackAndReadsAsNoAnswerLegionCouldRead(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) {
		g.enqueueUnanswered = true
		g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
	})
	pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	_, arms := github.counts()
	if github.enqueued() != 1 || arms != 1 {
		t.Fatalf("enqueues %d and enables %d, want one of each: the submission chosen follows the unanswered jump in the same attempt", github.enqueued(), arms)
	}
	jumped := onlyJumpRecord(t, pool, records)
	switch {
	case jumped.Outcome != record.MergeJumpUnknown:
		t.Fatalf("the jump record ended %s, want it unknown: nothing of GitHub's came back to decide it", jumped.Outcome)
	case !strings.Contains(jumped.Answer, "No answer Legion could read came back from GitHub") || !strings.Contains(jumped.Answer, "/graphql"):
		t.Fatalf("the jump record carries %q, want the failure named as one no answer came back for", jumped.Answer)
	case len(jumped.Entries) != 1 || jumped.Entries[0].Number != 11:
		t.Fatalf("the record of the unanswered jump holds %v, want the entries it had asked to go ahead of", jumped.Entries)
	}
	posts := messagePosts(t, pool)
	if len(posts) != 2 {
		t.Fatalf("Dispatch messages %q, want the jump's and the failure's", posts)
	}
	for _, said := range []string{"Legion asked GitHub to send the merge", "does not know whether GitHub took the request", "Legion submits the merge without the jump in this same attempt"} {
		if !strings.Contains(posts[1], said) {
			t.Fatalf("the failure posted on the issue %q, want it to say %q: what Legion asked, that nothing came back, and what Legion does next", posts[1], said)
		}
	}
	notices := controllerNotices(t, pool)
	if len(notices) != 1 || notices[0].kind != string(record.MergeJumpRefusedNotice) {
		t.Fatalf("controller notices %v, want one merge-jump-refused", notices)
	}
	for _, name := range []string{"LEGION-208", "#42", "ready-head", "Legion does not know whether GitHub took the request", "/graphql"} {
		if !strings.Contains(notices[0].reason, name) {
			t.Fatalf("the controller notice %q, want it to name %q", notices[0].reason, name)
		}
	}
	// Nothing GitHub never said is written as GitHub's, and nothing is said of where the merge
	// stands: neither text calls this a refusal of GitHub's, neither quotes it as GitHub's
	// answer, and neither places the merge in the queue.
	for _, told := range []string{posts[1], notices[0].reason} {
		for _, claimed := range []string{"GitHub refused", "GitHub answered:", "waits in the queue", "behind what was already", "went ahead of", "did not reach GitHub's merge queue"} {
			if strings.Contains(told, claimed) {
				t.Fatalf("%q says %q, which is either GitHub's word it never gave or a reading of its queue nobody made", told, claimed)
			}
		}
	}
	// The row still stands and no merge-refused notice was written: a jump that failed is no
	// refusal of the merge.
	assertMergeRows(t, pool, 1, "merge-refused", 0)
}

// An HTTP answer is GitHub answering as much as a GraphQL one, and what it decides is what the
// status says. A 4xx — a 401 or a 403 the token is refused by, a secondary rate limit, a 429 —
// is GitHub deciding this very request before any document of it is read, which settles the
// jump: record.MergeJumpRefused. The 500 or 502 GitHub's own edge sends for an API that decided
// nothing reached Legion all the same and settles nothing: record.MergeJumpUnknown, the same
// outcome a connection closed with no status leaves, which the answer's own words keep apart
// from it.
//
// Both quote the answer with the status it came with, and neither draws a verdict from it: a
// 502 is GitHub's edge saying its API decided nothing, so "GitHub refused" over it would tell a
// controller of a decision GitHub never made, and a 403 written as a request nothing came back
// for would sit beside the very words naming GitHub's own status.
func TestAJumpGitHubAnsweredWithAnHTTPStatusIsRefusedByA4xxAndUnknownByA5xx(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome record.MergeJumpOutcome
		// said is what the two texts lead with for this outcome, and quiet what neither may say.
		said, quiet string
	}{
		{status: http.StatusForbidden, outcome: record.MergeJumpRefused,
			said: "GitHub answered: ", quiet: "does not know whether GitHub took the request"},
		{status: http.StatusBadGateway, outcome: record.MergeJumpUnknown,
			said: "does not know whether GitHub took the request", quiet: "GitHub answered: "},
	} {
		t.Run(fmt.Sprintf("GitHub answered %d", tc.status), func(t *testing.T) {
			named := strconv.Itoa(tc.status)
			github := newQueueGitHub(t, "CLEAN")
			github.set(func(g *armableGitHub) {
				g.enqueueStatus = tc.status
				g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
			})
			pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			_, arms := github.counts()
			if github.enqueued() != 1 || arms != 1 {
				t.Fatalf("enqueues %d and enables %d, want one of each: the submission chosen follows the jump in the same attempt", github.enqueued(), arms)
			}
			jumped := onlyJumpRecord(t, pool, records)
			switch {
			case jumped.Outcome != tc.outcome || !strings.Contains(jumped.Answer, named):
				t.Fatalf("the jump record ended %s with %q, want %s carrying GitHub's own %s", jumped.Outcome, jumped.Answer, tc.outcome, named)
			case strings.Contains(jumped.Answer, "No answer Legion could read"):
				t.Fatalf("the jump record carries %q, want GitHub's answer and not the words of one nothing came back for", jumped.Answer)
			case len(jumped.Entries) != 1 || jumped.Entries[0].Number != 11:
				t.Fatalf("the jump record holds %v, want the entries it had asked to go ahead of", jumped.Entries)
			}
			posts := messagePosts(t, pool)
			if len(posts) != 2 {
				t.Fatalf("Dispatch messages %q, want the jump's and its end's", posts)
			}
			notices := controllerNotices(t, pool)
			if len(notices) != 1 || notices[0].kind != string(record.MergeJumpRefusedNotice) {
				t.Fatalf("controller notices %v, want one merge-jump-refused", notices)
			}
			for _, told := range []string{posts[1], notices[0].reason} {
				switch {
				case !strings.Contains(told, named):
					t.Fatalf("%q does not name the %s GitHub answered the jump with", told, named)
				case !strings.Contains(told, tc.said):
					t.Fatalf("%q does not say %q, which is what a %s came to", told, tc.said, named)
				case strings.Contains(told, tc.quiet):
					t.Fatalf("%q says %q, which is the other outcome's own words over a %s", told, tc.quiet, named)
				case !strings.Contains(told, "asked GitHub to send the merge"):
					t.Fatalf("%q does not say what Legion asked GitHub for before it quotes the answer", told)
				case strings.Contains(told, "reached Legion"), strings.Contains(told, "did not reach GitHub"):
					t.Fatalf("%q reads as a jump nothing of GitHub's answered, where GitHub answered %s", told, named)
				case strings.Contains(told, "GitHub refused"), strings.Contains(told, "waits in the queue"), strings.Contains(told, "went ahead of"):
					t.Fatalf("%q draws a verdict from the %s GitHub answered, which the daemon has no reading of the queue for", told, named)
				}
			}
			// The row still stands and no merge-refused notice was written: a jump that did not
			// take is no refusal of the merge.
			assertMergeRows(t, pool, 1, "merge-refused", 0)
		})
	}
}

// A 2xx GitHub carried no data in is GitHub answering and saying nothing at all of the mutation
// (githubrest.ErrGraphQLNoData), which settles nothing: the record ends unknown, with GitHub's
// own words, beside the jump nothing came back for and apart from the 4xx that decided one. The
// fallback goes out in the same attempt as it does for every jump that did not come back
// accepted.
func TestAJumpGitHubAnsweredWithoutDataLeavesTheOutcomeUnknown(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) {
		g.enqueueNoData = true
		g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
	})
	pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	_, arms := github.counts()
	if github.enqueued() != 1 || arms != 1 {
		t.Fatalf("enqueues %d and enables %d, want one of each: the submission chosen follows the jump in the same attempt", github.enqueued(), arms)
	}
	jumped := onlyJumpRecord(t, pool, records)
	switch {
	case jumped.Outcome != record.MergeJumpUnknown:
		t.Fatalf("the jump record ended %s, want it unknown: GitHub answered and said nothing of the mutation", jumped.Outcome)
	case !strings.Contains(jumped.Answer, "without data"):
		t.Fatalf("the jump record carries %q, want GitHub's own words for an answer that held no data", jumped.Answer)
	case strings.Contains(jumped.Answer, "No answer Legion could read"):
		t.Fatalf("the jump record carries %q, want GitHub's answer and not the words of one nothing came back for: GitHub answered this one", jumped.Answer)
	}
	notices := controllerNotices(t, pool)
	if len(notices) != 1 || notices[0].kind != string(record.MergeJumpRefusedNotice) {
		t.Fatalf("controller notices %v, want one merge-jump-refused", notices)
	}
}

// A jump GitHub takes is written down as taken: the record ends accepted, which is the one thing
// the mutation answering says, and the answer column stays empty because there is nothing to
// quote. Nobody is told — no second message on the issue and no controller notice — since the
// mark was spent on exactly what it was set for and the merge is in the queue.
//
// What is never written is where the merge then stands. Acceptance is GitHub taking the request
// and the queue's own order after it is read on GitHub, so the account keeps the queue it read
// before asking and claims no place in it.
func TestAJumpGitHubTakesIsRecordedAccepted(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) {
		g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
	})
	pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if github.enqueued() != 1 {
		t.Fatalf("enqueues %d, want the one the jump sends", github.enqueued())
	}
	jumped := onlyJumpRecord(t, pool, records)
	switch {
	case jumped.Outcome != record.MergeJumpAccepted:
		t.Fatalf("the jump record ended %s, want it accepted: GitHub took the request", jumped.Outcome)
	case jumped.Answer != "":
		t.Fatalf("the jump record carries %q, want nothing quoted: an accepted mutation answered with no words", jumped.Answer)
	case len(jumped.Entries) != 1 || jumped.Entries[0].Number != 11:
		t.Fatalf("the accepted jump record holds %v, want the queue it read before asking", jumped.Entries)
	}
	posts := messagePosts(t, pool)
	if len(posts) != 1 {
		t.Fatalf("Dispatch messages %q, want the jump's own alone: an accepted jump owes nobody a second message", posts)
	}
	if notices := controllerNotices(t, pool); len(notices) != 0 {
		t.Fatalf("controller notices %v, want none: the mark was spent on what it was set for", notices)
	}
	// Acceptance is the mutation answering and no place in a queue: nothing written of this run
	// may read as where the merge stands.
	for _, claimed := range []string{"went ahead of", "at the head of the queue", "waits in the queue", "behind what was already"} {
		if strings.Contains(posts[0], claimed) {
			t.Fatalf("the message %q says %q of a queue the daemon never read again", posts[0], claimed)
		}
	}
}

// The write that records what became of a jump is fenced on the row's lease, as the spend
// itself was, and it is best-effort: a lease that moved to another worker while the enqueue was
// in flight leaves the record saying Legion asked, which is what is then true of this worker,
// and fails nothing. The merge is in GitHub's queue by then, and a row failed here would come
// back and submit the same merge again over an account it could not write.
//
// The lease is taken here at the one moment that leaves the spend committed and the second
// write to come: as the enqueue reaches GitHub and before it is answered (atEnqueue).
func TestAJumpWhoseLeaseMovesBeforeTheSecondWriteStaysAsked(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) {
		g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}}
	})
	pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
	var stolen error
	github.set(func(g *armableGitHub) {
		g.atEnqueue = func() {
			_, stolen = pool.Exec(context.Background(),
				`update outbox set lease_token = 'another-worker' where kind = 'ready_merge'`)
		}
	})
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v, want a run that fails nothing: the merge is queued and only its account was lost", err)
	}
	if stolen != nil {
		t.Fatalf("take the row's lease while the enqueue was in flight: %v", stolen)
	}
	if github.enqueued() != 1 {
		t.Fatalf("enqueues %d, want the one the jump sends", github.enqueued())
	}
	jumped := onlyJumpRecord(t, pool, records)
	if jumped.Outcome != record.MergeJumpAsked || jumped.Answer != "" {
		t.Fatalf("the jump record ended %s with %q, want it still asked: this worker never wrote down what it learned", jumped.Outcome, jumped.Answer)
	}
}

// Nothing Legion writes when it spends a jump mark says the merge went to the head of the queue,
// because when it is written nothing has been sent: the record and the issue's message are
// written in the transaction that spends the mark, which commits before the enqueue goes out
// (requirement R2). That window is read here from GitHub's own side — the fake runs the read as
// the enqueue reaches it and before it answers (atEnqueue) — and what stands in it is a request:
// the message says Legion asked for the jump and names the queue as what the merge is asked to go
// ahead of, and the record's outcome is record.MergeJumpAsked, which is the state of every jump
// between its spend and its answer. A sentence written there as a merge that jumped would be
// false for every jump GitHub then refuses or never answers, and would stay false until
// JumpEnded's own write repaired it.
func TestWhatAJumpWritesBeforeItsMutationIsSentSaysOnlyThatLegionAsked(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.queue = []queueEntry{{number: 11, position: 1, state: "MERGEABLE"}} })
	pool, records, runner, _ := jumpRunner(t, github, "the release waits on this merge")
	var posts []string
	var jumps []record.MergeJumpRecord
	var read error
	github.set(func(g *armableGitHub) {
		g.atEnqueue = func() { posts, jumps, read = jumpWindow(pool, records) }
	})
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// The fake's own lock is taken here before the window's reads are, which is what hands this
	// goroutine what the handler wrote.
	if github.enqueued() != 1 {
		t.Fatalf("enqueues %d, want the one the jump sends: the window below is read as it arrives", github.enqueued())
	}
	if read != nil {
		t.Fatalf("read what the jump had written when its enqueue reached GitHub: %v", read)
	}
	if len(posts) != 1 || len(jumps) != 1 {
		t.Fatalf("the window held %d messages and %d jump records, want the jump's own of each", len(posts), len(jumps))
	}
	switch {
	case !strings.Contains(posts[0], "Legion asked GitHub to send the merge of pull request #42"):
		t.Fatalf("the message written before the enqueue was sent %q, want it to say Legion asked for the jump", posts[0])
	case !strings.Contains(posts[0], "which this merge is asked to go ahead of"):
		t.Fatalf("the message %q names the queue as one the merge has gone ahead of, want what it is asked to go ahead of", posts[0])
	case strings.Contains(posts[0], "Legion sent the merge"), strings.Contains(posts[0], "now goes ahead of"):
		t.Fatalf("the message written before the enqueue was sent %q says the merge went to the head of the queue, which has not happened", posts[0])
	}
	if jumps[0].Issue != "LEGION-208" || jumps[0].Outcome != record.MergeJumpAsked || jumps[0].Answer != "" {
		t.Fatalf("the record in that window = %+v, want LEGION-208's request, asked and nothing learned", jumps[0])
	}
}

// jumpWindow is every Dispatch message the outbox holds and every jump record of LEGION at the
// moment it is called, with the failure of either read. It takes no *testing.T: it is called from
// the fake's own handler (atEnqueue), where a t.Fatalf would stop the goroutine that owes GitHub's
// answer rather than the test, so its caller reads the error back and fails there.
func jumpWindow(pool *pgxpool.Pool, records record.Store) ([]string, []record.MergeJumpRecord, error) {
	ctx := context.Background()
	var posts []string
	var jumps []record.MergeJumpRecord
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select payload->>'body' from outbox where kind = 'dispatch_message' order by id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				return err
			}
			posts = append(posts, body)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		jumps, err = records.MergeJumpRecords(ctx, tx, "LEGION")
		return err
	})
	return posts, jumps, err
}

// jumpRunner is the common setup of the jump tests: LEGION-208 awaiting its merge at ready-head,
// its ready_merge row queued, the controller's mark on it for reason, and the outbox that runs it
// against github on a clock held still. It answers the pool, the store, the runner and that clock.
func jumpRunner(t *testing.T, github *armableGitHub, reason string) (*pgxpool.Pool, record.Store, *outbox, time.Time) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	setMergeJump(t, pool, records, "LEGION-208", reason, now)
	return pool, records, mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now }), now
}

// setMergeJump marks issue for the jump, as the controller's route does. The write lands only
// while the issue is still one the daemon runs (record.Store.SetMergeJump), so the answer is
// read: a seed that wrote nothing would leave the run with no mark to spend.
func setMergeJump(t *testing.T, pool *pgxpool.Pool, records record.Store, issue, reason string, at time.Time) {
	t.Helper()
	var wrote bool
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		wrote, err = records.SetMergeJump(context.Background(), tx, record.MergeJump{Issue: issue, Reason: reason, Session: "ses_controller", SetAt: at})
		return err
	}); err != nil {
		t.Fatalf("set the jump mark of %s: %v", issue, err)
	}
	if !wrote {
		t.Fatalf("the jump mark of %s was not written, want it on an issue still in the workflow", issue)
	}
}

// mergeJumpStands says whether issue still carries its jump mark.
func mergeJumpStands(t *testing.T, pool *pgxpool.Pool, issue string) bool {
	t.Helper()
	var marks int
	if err := pool.QueryRow(context.Background(), `select count(*) from merge_jumps where issue = $1`, issue).Scan(&marks); err != nil {
		t.Fatalf("read the jump mark of %s: %v", issue, err)
	}
	return marks == 1
}

// onlyJumpRecord is the project's one jump record, and fails where it holds any other number.
func onlyJumpRecord(t *testing.T, pool *pgxpool.Pool, records record.Store) record.MergeJumpRecord {
	t.Helper()
	var jumps []record.MergeJumpRecord
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		jumps, err = records.MergeJumpRecords(context.Background(), tx, "LEGION")
		return err
	}); err != nil {
		t.Fatalf("read the jump records of LEGION: %v", err)
	}
	if len(jumps) != 1 {
		t.Fatalf("jump records %+v, want one: the record keeps the newest jump of each repository", jumps)
	}
	return jumps[0]
}

// controllerNotice is one controller notice the outbox holds: its kind and its reason.
type controllerNotice struct{ kind, reason string }

// controllerNotices is every controller notice the outbox holds, oldest first.
func controllerNotices(t *testing.T, pool *pgxpool.Pool) []controllerNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload->>'kind', payload->>'reason' from outbox where kind = 'controller_notice' order by id`)
	if err != nil {
		t.Fatalf("read the controller notices: %v", err)
	}
	defer rows.Close()
	var notices []controllerNotice
	for rows.Next() {
		var notice controllerNotice
		if err := rows.Scan(&notice.kind, &notice.reason); err != nil {
			t.Fatalf("scan a controller notice: %v", err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// requestOrder says whether the fake answered first before second, both of them once at least.
func requestOrder(requests []string, first, second string) bool {
	return slices.Index(requests, first) >= 0 && slices.Index(requests, first) < slices.Index(requests, second)
}

// heldFile is the one file acme/widgets#42 changes in the merge hold tests, and heldPattern a
// hold's pattern that matches it: an unanchored regular expression, as a project's by_hand merge
// rules are. missPattern matches nothing the pull request changes.
const (
	heldFile    = "packages/daemon/internal/daemon/auto_merge.go"
	heldPattern = "packages/daemon/"
	missPattern = "^migrations/"
)

// A merge hold keeps the submission back while it is live and matches a file the pull request
// changes: nothing at all is sent to GitHub, the issue stays in awaiting_merge, the row stands
// and runs again when the hold ends, and whatever else the controller marked the issue with is
// left where it is. A hold that has ended, one the controller took off, one whose paths match
// nothing the pull request changes and one naming another repository hold nothing. A file list
// GitHub cut holds the merge, since the daemon cannot tell whether what it never saw is the
// hold's. A project with no hold at all reads no file list: the holds are read first, and a
// project holding nothing costs its merges no GitHub call.
func TestAMergeHoldKeepsTheMergeBackUntilItEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// holds is the project's hold list, built around the moment the row first runs, cleared
		// whether the controller takes the list off before the second run, and filesCut whether
		// GitHub cuts the file list at its own limit.
		holds    func(now time.Time) []record.MergeHold
		cleared  bool
		filesCut bool
		// marked is a jump mark set on the issue, which a held run must leave where it is.
		marked bool
		// runs is how many times the row is drained, arms how many submissions GitHub was sent,
		// heldUntil the moment the row is next due, zero where the row was not held, and
		// readsFiles whether the pull request's changed files were read at all: only a hold whose
		// repository covers this one, and which is still live, costs that read.
		runs       int
		arms       int
		heldUntil  func(now time.Time) time.Time
		readsFiles bool
	}{
		{
			name: "a live hold whose paths match a changed file",
			holds: func(now time.Time) []record.MergeHold {
				return []record.MergeHold{windowHold(now.Add(30*time.Minute), heldPattern)}
			},
			marked:     true,
			runs:       1,
			heldUntil:  func(now time.Time) time.Time { return now.Add(30 * time.Minute) },
			readsFiles: true,
		},
		{
			name: "a hold whose end time has passed",
			holds: func(now time.Time) []record.MergeHold {
				return []record.MergeHold{windowHold(now.Add(-time.Minute), heldPattern)}
			},
			runs: 1,
			arms: 1,
		},
		{
			name: "a hold the controller took off before the next run",
			holds: func(now time.Time) []record.MergeHold {
				return []record.MergeHold{windowHold(now.Add(30*time.Minute), heldPattern)}
			},
			cleared:    true,
			runs:       2,
			arms:       1,
			readsFiles: true,
		},
		{
			name: "a hold whose paths match no changed file",
			holds: func(now time.Time) []record.MergeHold {
				return []record.MergeHold{windowHold(now.Add(30*time.Minute), missPattern)}
			},
			runs:       1,
			arms:       1,
			readsFiles: true,
		},
		{
			name: "a hold naming another repository the project serves",
			holds: func(now time.Time) []record.MergeHold {
				hold := windowHold(now.Add(30*time.Minute), heldPattern)
				hold.Repository = "acme/gadgets"
				return []record.MergeHold{hold}
			},
			runs: 1,
			arms: 1,
		},
		{
			name: "a file list GitHub cut at its own limit",
			holds: func(now time.Time) []record.MergeHold {
				return []record.MergeHold{windowHold(now.Add(30*time.Minute), missPattern)}
			},
			filesCut:   true,
			runs:       1,
			heldUntil:  func(now time.Time) time.Time { return now.Add(30 * time.Minute) },
			readsFiles: true,
		},
		{
			name:  "no hold at all",
			holds: func(time.Time) []record.MergeHold { return nil },
			runs:  1,
			arms:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newQueueGitHub(t, "CLEAN")
			github.set(func(g *armableGitHub) {
				g.changedFiles, g.filesCut = []string{heldFile}, tc.filesCut
			})
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Now().UTC().Truncate(time.Second)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
			setMergeHolds(t, pool, records, tc.holds(now)...)
			if tc.marked {
				setMergeJump(t, pool, records, "LEGION-208", "the release waits on this merge", now)
			}
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			for run := range tc.runs {
				if run == 1 && tc.cleared {
					setMergeHolds(t, pool, records)
				}
				// A held row is due again when its hold ends, so a test that drains it twice
				// brings it forward, as the daemon's own clock does by reaching that moment.
				readyMergeDue(t, pool, clock.Add(-time.Second))
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("RunOnce %d: %v", run+1, err)
				}
				clock = clock.Add(time.Minute)
			}
			disables, arms := github.counts()
			if arms != tc.arms || github.enqueued() != 0 || disables != 0 {
				t.Fatalf("submissions %d, enqueues %d and disarms %d, want %d, none and none: %v",
					arms, github.enqueued(), disables, tc.arms, github.requestLog())
			}
			if read := slices.Contains(github.requestLog(), "files"); read != tc.readsFiles {
				t.Fatalf("the pull request's files were read %t, want %t: %v", read, tc.readsFiles, github.requestLog())
			}
			var at string
			if err := pool.QueryRow(context.Background(), `select phase from issues where key = 'LEGION-208'`).Scan(&at); err != nil {
				t.Fatalf("read LEGION-208's phase: %v", err)
			}
			if at != string(phase.AwaitingMerge) {
				t.Fatalf("LEGION-208 is in %q, want it awaiting its merge: a hold moves no issue", at)
			}
			nextAt, _, _, _ := readyMergeState(t, pool)
			if tc.heldUntil == nil {
				// The row was not held, so there is no next run to check: the one case that
				// marks the issue is held, and its mark is read below.
				return
			}
			if want := tc.heldUntil(now); !nextAt.Equal(want) {
				t.Fatalf("the held row runs again at %s, want the hold's end %s", nextAt, want)
			}
			if tc.marked && !mergeJumpStands(t, pool, "LEGION-208") {
				t.Fatalf("the jump mark of LEGION-208 is gone: a held run spends nothing at all")
			}
		})
	}
}

// A held row tells the issue once and its architect once, however many times it runs under the
// hold, and the time it spent held is not time GitHub was slow: the merge-waiting notice is
// measured from the submission's queueing less every held stretch, so a row held for two hours
// and waiting on GitHub for one does not warn, and the same row warns once its unheld wait passes
// the hour.
func TestAMergeHoldTellsTheIssueOnceAndIsNoWaitOnGitHub(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	ends := now.Add(2 * time.Hour)
	setMergeHolds(t, pool, records, windowHold(ends, heldPattern))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	run := func(step string) {
		t.Helper()
		readyMergeDue(t, pool, clock.Add(-time.Second))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
	}
	for held := range 10 {
		run(fmt.Sprintf("held run %d", held+1))
		clock = clock.Add(time.Minute)
	}
	if _, arms := github.counts(); arms != 0 {
		t.Fatalf("submissions %d, want none: the hold holds every run", arms)
	}
	posts := messagePosts(t, pool)
	reasons := assertMergeRows(t, pool, 1, "merge-held", 1)
	if len(posts) != 1 {
		t.Fatalf("Dispatch messages %q, want the one the first held run posts", posts)
	}
	for _, name := range []string{"#42", "ready-head", heldPattern, "the release window is open", "ses_controller", ends.Format(time.RFC3339)} {
		if !strings.Contains(posts[0], name) {
			t.Fatalf("the message posted on the issue %q, want it to name %q", posts[0], name)
		}
		if !strings.Contains(reasons[0], name) {
			t.Fatalf("the merge-held notice %q, want it to name %q", reasons[0], name)
		}
	}
	// The hold ends, and the submission is made on the run after it: the held stretch is closed
	// on that same run, so the wait the architect could be told of starts from there.
	clock = ends.Add(time.Minute)
	run("the submission once the hold ends")
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("submissions %d, want the one made once the hold ended", arms)
	}
	held, since, told := heldMarks(t, pool)
	if want := clock.Sub(now); held != want || !since.IsZero() || !told {
		t.Fatalf("the row holds %s held since %s told %t, want %s closed and told", held, since, told, want)
	}
	// GitHub has had the entry for less than an hour, while the row has stood for three: nothing
	// is told, since none of the held time is GitHub's.
	clock = clock.Add(55 * time.Minute)
	run("the wait inside the hour")
	if _, _, warned, _ := readyMergeState(t, pool); warned {
		t.Fatalf("the row is recorded as warned, want the held time taken off the wait before the hour is judged")
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 0)
	// An hour of unheld waiting passes, and the architect is told once.
	clock = clock.Add(10 * time.Minute)
	run("the wait past the hour")
	if _, _, warned, _ := readyMergeState(t, pool); !warned {
		t.Fatalf("the row is not recorded as warned after an hour of unheld waiting")
	}
	assertMergeRows(t, pool, 1, "merge-waiting", 1)
	if held, _, _ := heldMarks(t, pool); held != ends.Add(time.Minute).Sub(now) {
		t.Fatalf("the row holds %s of held time, want the one stretch the hold kept open", held)
	}
}

// A re-READY of a held row starts the hold's account over: the workflow's upsert replaces the
// whole payload, so the held time, the open stretch and the telling go together, and the next
// submission tells its own architect again. The row's next_at is deliberately kept by that upsert
// (record's readyMergeConflict), so a row held until tomorrow is still due tomorrow.
func TestAReREADYOfAMergeHoldHeldRowStartsItsHoldOver(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	ends := now.Add(2 * time.Hour)
	setMergeHolds(t, pool, records, windowHold(ends, heldPattern))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if held, since, told := heldMarks(t, pool); held != 0 || since.IsZero() || !told {
		t.Fatalf("the held row holds %s since %s told %t, want an open stretch told once", held, since, told)
	}
	clock = clock.Add(time.Minute)
	// The workflow queues the same issue and head again, as a reopen does.
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: clock}, clock))
	held, since, told := heldMarks(t, pool)
	if held != 0 || !since.IsZero() || told {
		t.Fatalf("the re-queued row holds %s since %s told %t, want all three reset", held, since, told)
	}
	if nextAt, _, _, _ := readyMergeState(t, pool); !nextAt.Equal(ends) {
		t.Fatalf("the re-queued row runs again at %s, want the hold's end %s: the upsert keeps next_at on purpose", nextAt, ends)
	}
}

// The merge queue's removal for a conflict is answered inside the submission before it chooses
// anything, so the hold is asked once more where that removal would have the row submit again: a
// matching hold spends nothing, posts nothing of the resubmission and leaves the row due when the
// hold ends, and the run after it makes that further submission.
func TestAMergeHoldHoldsTheConflictResubmission(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	runner.handlers = []intake.Handler{workflow.New(records, workflow.Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps()}, quietLogger())}
	run := func(step string) {
		t.Helper()
		readyMergeDue(t, pool, clock.Add(-time.Second))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}
	run("READY's own submission")
	ends := clock.Add(30 * time.Minute)
	setMergeHolds(t, pool, records, windowHold(ends, heldPattern))
	github.remove(dequeuedEvent("merge_conflict", "github-merge-queue", "null"))
	run("the read that finds the removal while the hold stands")
	if _, arms := github.counts(); arms != 1 {
		t.Fatalf("submissions %d, want only READY's own: a hold holds the further submission too", arms)
	}
	spent, _, standing := conflictMarks(t, pool)
	nextAt, _, _, _ := readyMergeState(t, pool)
	switch {
	case !standing || spent:
		t.Fatalf("the ready_merge row stands %t with its further submission spent %t, want a standing row that spent nothing", standing, spent)
	case !nextAt.Equal(ends):
		t.Fatalf("the held row runs again at %s, want the hold's end %s", nextAt, ends)
	}
	for _, post := range messagePosts(t, pool) {
		if strings.Contains(post, "once more") {
			t.Fatalf("the issue was told of a further submission the hold held back: %q", post)
		}
	}
	// The hold ends, and the removal is decided again: this time the merge is submitted once more.
	clock = ends.Add(time.Minute)
	run("the run after the hold ends")
	spent, _, _ = conflictMarks(t, pool)
	if _, arms := github.counts(); arms != 2 || !spent {
		t.Fatalf("submissions %d with the further submission spent %t, want two and spent", arms, spent)
	}
}

// A live hold changes nothing about what the submission answers for a row GitHub shows armed or
// queued: the hold is asked only where a mutation would be sent, so an entry GitHub still holds
// is the wait it always was, a removal still ends the row with its architect told, and a pull
// request GitHub shows closed is still the disagreement the row waits on. No mutation is
// sent on any of the three.
func TestAMergeHoldLeavesAnArmedRowsOwnPathsAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// github is the state GitHub is put into after READY's own submission queued the pull
		// request, removed whether a merge-removed notice is written, and standing whether the
		// ready_merge row is left to run again.
		github   func(g *armableGitHub)
		removed  int
		standing bool
	}{
		{name: "an entry the merge queue still holds", github: func(*armableGitHub) {}, standing: true},
		{
			name: "an entry a person took out",
			github: func(g *armableGitHub) {
				g.armed, g.queued = false, false
				g.timeline = append(g.timeline, manuallyDisabledEvent("alice"))
			},
			removed: 1,
		},
		{
			name:     "a pull request GitHub shows closed",
			github:   func(g *armableGitHub) { g.state = "CLOSED" },
			standing: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newQueueGitHub(t, "CLEAN")
			github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
			now := time.Now().UTC().Truncate(time.Second)
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
			clock := now
			runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce for READY's own submission: %v", err)
			}
			clock = clock.Add(time.Minute)
			setMergeHolds(t, pool, records, windowHold(clock.Add(30*time.Minute), heldPattern))
			github.set(tc.github)
			readyMergeDue(t, pool, clock.Add(-time.Second))
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce under the hold: %v", err)
			}
			if _, arms := github.counts(); arms != 1 {
				t.Fatalf("submissions %d, want only READY's own: nothing is sent under a live hold", arms)
			}
			left := 0
			if tc.standing {
				left = 1
			}
			assertMergeRows(t, pool, left, "merge-removed", tc.removed)
		})
	}
}

// windowHold is one live merge hold of the controller's: the paths it covers, its reason, its
// session and when it ends.
func windowHold(ends time.Time, paths ...string) record.MergeHold {
	return record.MergeHold{Paths: paths, Reason: "the release window is open", EndsAt: ends,
		Session: "ses_controller", SetAt: ends.Add(-time.Hour)}
}

// setMergeHolds replaces the project's whole merge hold list, as the controller's route does.
func setMergeHolds(t *testing.T, pool *pgxpool.Pool, records record.Store, holds ...record.MergeHold) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.ReplaceMergeHolds(context.Background(), tx, "LEGION", holds)
	}); err != nil {
		t.Fatalf("replace the merge holds of LEGION: %v", err)
	}
}

// heldMarks is what the one ready_merge row records of the merge holds that held it: the time
// already counted (record.ReadyMerge.Held), the moment an open stretch began (HeldSince), and
// whether the row has told of a hold (HeldTold).
func heldMarks(t *testing.T, pool *pgxpool.Pool) (held time.Duration, since time.Time, told bool) {
	t.Helper()
	var nanoseconds *int64
	var at *string
	if err := pool.QueryRow(context.Background(),
		`select (payload->>'held')::bigint, payload->>'heldSince', coalesce((payload->>'heldTold')::boolean, false) from outbox where kind = 'ready_merge'`).
		Scan(&nanoseconds, &at, &told); err != nil {
		t.Fatalf("read the ready_merge row's held marks: %v", err)
	}
	if nanoseconds != nil {
		held = time.Duration(*nanoseconds)
	}
	if at != nil {
		began, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			t.Fatalf("parse the row's open held stretch %q: %v", *at, err)
		}
		since = began.UTC()
	}
	return held, since, told
}

// readyMergeDue brings the one ready_merge row forward to at, as the daemon's own clock does by
// reaching the moment a held row named: a held row is due when its hold ends, and a test that
// drains it again before then would otherwise list no row at all.
func readyMergeDue(t *testing.T, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update outbox set next_at = $1 where kind = 'ready_merge'`, at); err != nil {
		t.Fatalf("bring the ready_merge row forward: %v", err)
	}
}
