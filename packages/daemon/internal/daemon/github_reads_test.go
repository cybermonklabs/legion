package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pullStandIn is GitHub's REST API for acme/widgets as the per-issue read sees it: pull request 86
// answered with pull, and no review on it.
func pullStandIn(t *testing.T, pull string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull))
		case "/repos/acme/widgets/pulls/86/reviews":
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// mergeableStandIn answers pull request 86 on base main with state as the literal JSON of GitHub's
// `mergeable` field, and no merge state at all.
func mergeableStandIn(t *testing.T, state string) *httptest.Server {
	t.Helper()
	return pullStandIn(t, `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":`+state+`}`)
}

// mergeStateStandIn answers pull request 86 on base main with state as GitHub's `mergeable_state`,
// beside the `mergeable: true` GitHub computes with it: GitHub answers true for every state its
// own rules leave rather than a conflict, a head it blocks on a required check included, which is
// what makes the merge state the only answer that tells those apart.
func mergeStateStandIn(t *testing.T, state string) *httptest.Server {
	t.Helper()
	return pullStandIn(t, `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"`+state+`"}`)
}

// readRuntime is a workflow runtime over pool whose GitHub reads go to api, logging to log. Its
// engine stands in for the whole workflow and is given the runtime's own merge-state queues, of
// one pull request and of a base's batch, so a fact a test applies asks for the read it asks for
// in production; the queues are empty and nothing reads them until a test starts readMergeStates.
// Its reads of a pull request and of a base's rules are conditional, as the daemon's are.
func readRuntime(pool *store.Pool, api string, log *slog.Logger) *workflowRuntime {
	w := &workflowRuntime{
		pool: pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"},
		tokens: outboxTokens{}, dispatchProject: "CAPTURE", bootID: "test-boot", log: log, githubAPI: api,
		mergeStates: newWakeSet[string](), mergeStateRunning: map[string]*mergeStateRun{}, baseWake: newWakeSet[string](),
		etags: githubrest.NewETags(etagBytes),
	}
	w.engineOver(record.NewStore())
	return w
}

// engineOver gives w the engine readRuntime gives it, writing through records, and applies the
// facts w's reads make with it (workflowRuntime.handlers).
func (w *workflowRuntime) engineOver(records record.Store) {
	w.engine = workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories,
		ReadMergeState: w.queueMergeState, ReadBaseMergeStates: w.queueBaseMergeStates}, w.log)
	w.handlers = []intake.Handler{w.engine}
}

// servedAcme is the repository state of a project that has prepared acme/widgets alone, both
// role Apps installed on it, the one a read's own spelling of a repository is resolved against.
func servedAcme() *repositories {
	served := newRepositories("CAPTURE", ghrepo.Renames{})
	served.fill(ghrepo.MustParse("acme/widgets"))
	return served
}

func capturePhase(t *testing.T, pool *store.Pool) (phase.Phase, *record.PullRequest) {
	t.Helper()
	var issue *record.Issue
	var pr *record.PullRequest
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if issue, err = record.NewStore().Issue(context.Background(), tx, "CAPTURE-1"); err != nil {
			return err
		}
		pr, err = record.NewStore().PullRequest(context.Background(), tx, "CAPTURE-1")
		return err
	}); err != nil {
		t.Fatalf("read CAPTURE-1: %v", err)
	}
	return issue.Phase, pr
}

// readCapture runs the per-issue read of CAPTURE-1's pull request, as the daemon's start and each
// handoff completion do, failing the test on an error.
func readCapture(t *testing.T, w *workflowRuntime) {
	t.Helper()
	if _, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
		t.Fatalf("read CAPTURE-1's pull request: %v", err)
	}
}

// captureHeadUpdated is the clock CAPTURE-1's record holds for the lifecycle events applied to
// its pull request (record.PullRequest.HeadUpdatedAt), which is GitHub's own updated_at for it.
var captureHeadUpdated = time.Date(2026, 10, 8, 22, 15, 0, 0, time.UTC)

// seedCapture records CAPTURE-1 in from with its pull request 86 at head code, whose required
// check passed there and which GitHub's last read found clean, and its phases. A test about what
// an unread or another merge state leaves undone sets its own (clearCaptureMergeState).
func seedCapture(t *testing.T, pool *store.Pool, from phase.Phase, status string, phases ...record.PhaseRow) {
	t.Helper()
	records := record.NewStore()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root",
			Phase: from, Generation: 1, Status: status, Rank: "U"}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets",
			Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "code", HeadUpdatedAt: captureHeadUpdated,
			CheckedHead: "code", Failing: []string{},
			CheckRuns:  []record.AttemptRun{{Name: "pr-checks-result", ID: 1}, {Name: "review", ID: 2}},
			Generation: 1, Snapshot: "settled", Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}); err != nil {
			return err
		}
		for _, row := range append([]record.PhaseRow{{Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim"}}, phases...) {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
}

// noticePayloads is the payload of every notice of kind in the outbox, decoded as JSON.
func noticePayloads(t *testing.T, pool *store.Pool, kind string) []map[string]any {
	t.Helper()
	var notices []map[string]any
	rows, err := pool.Query(context.Background(), "select payload from outbox where kind = 'notice' and payload->>'kind' = $1 order by id", kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var notice map[string]any
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatalf("decode a %s notice %s: %v", kind, payload, err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return notices
}

// GitHub sends no event when it computes a pull request's mergeability, so a head that starts
// conflicting with its base - someone merged another pull request into it - is learned only from
// the daemon's own read of /pulls/{number}. The read applies what it found, and the issue keeps
// its phase whatever that is (dispatch://INFRA-4374 R1). The first read that finds the head
// conflicting records itself, queues the read that confirms it a minute later
// (record.ConflictConfirm) and acts on nothing (R2). A read that finds `mergeable` still null -
// GitHub has not computed it yet - or true records only that. The read must apply the fact and
// decide even when it only confirms what is already stored: a conflict can be confirmed while the
// issue is still in testing or reviewing, where it needed no merge yet, and the first read after
// the issue reaches merging has to ask for the merge forward then, with no change in the GitHub
// answer to key off.
func TestThePerIssueReadCatchesAHeadThatStartsConflictingWithItsBase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		state        string
		held         string
		mergeability record.Mergeability
		firstRead    bool
		asked        bool
	}{
		{"conflicting", "false", "", record.MergeabilityConflicting, true, false},
		{"not yet computed", "null", "", record.MergeabilityUnknown, false, false},
		{"mergeable", "true", "", record.MergeabilityMergeable, false, false},
		{"conflict already recorded", "false", "code", record.MergeabilityConflicting, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Merging, "retro",
				record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "code", Summary: "READY #86 at code"})
			if tc.held != "" {
				if _, err := pool.Exec(context.Background(), "update pull_requests set mergeability = $1, conflicting_head = $2 where issue = 'CAPTURE-1'",
					string(record.MergeabilityConflicting), tc.held); err != nil {
					t.Fatalf("seed the confirmed conflict: %v", err)
				}
			}
			w := readRuntime(pool, mergeableStandIn(t, tc.state).URL, quietLogger())
			if _, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
				t.Fatalf("read the issue's pull request: %v", err)
			}
			got, pr := capturePhase(t, pool)
			if got != phase.Merging {
				t.Fatalf("after the read the issue is in %s, want merging: no read of GitHub's mergeability moves an issue", got)
			}
			if pr.Mergeability != tc.mergeability {
				t.Fatalf("recorded mergeability = %q, want %q", pr.Mergeability, tc.mergeability)
			}
			confirms := outboxRowsOfKind(t, pool, "CAPTURE-1", record.OutboxKindConflictConfirm)
			wantConfirms := 0
			if tc.firstRead {
				wantConfirms = 1
			}
			if first := pr.ConflictRead != nil; first != tc.firstRead || confirms != wantConfirms {
				t.Fatalf("after the read the record holds first read %+v and the outbox %d second reads, want a first read %t and %d second reads",
					pr.ConflictRead, confirms, tc.firstRead, wantConfirms)
			}
			asked := pr.MergeForward != nil && pr.MergeForward.Head == "code" && pr.MergeForward.Phase == phase.Merging
			if asked != tc.asked {
				t.Fatalf("after the read the record's merge forward is %+v, want one asked of code in merging %t", pr.MergeForward, tc.asked)
			}
			notices := noticePayloads(t, pool, "checks-red")
			if !tc.asked {
				if len(notices) != 0 {
					t.Fatalf("checks-red notices %v, want none: nothing was acted on", notices)
				}
				return
			}
			if len(notices) != 1 || notices[0]["phase"] != string(phase.Merging) || notices[0]["conflicts_with"] != "main" ||
				notices[0]["head"] != "code" || notices[0]["why"] != string(record.ConflictMergeNext) || notices[0]["reason"] != nil {
				t.Fatalf("checks-red notices %v, want one naming the head code conflicting with main, merging as the phase the issue stays in, merge-next, and no reason", notices)
			}
		})
	}
}

// The same one read carries GitHub's merge state for the head, which is the answer the spec's
// mergeable is written in: clean, unstable or has_hooks, and never blocked. The daemon records
// GitHub's own spelling, since the tri-state mergeability it decodes from the same response
// cannot tell the two apart - a head GitHub holds on a check or a review the repository requires
// answers `mergeable: true` exactly as a clean one does - and recording it moves no issue: what
// the merge state decides is a later change's.
//
// It records when that answer was in hand with it (record.PullRequest.StateReadAt), from the
// reader's own clock, and the next read moves the stamp although it answers the very same state:
// what the column records is the read, so a head read a minute ago reads differently from one
// nothing has read for a day (applyMergeability).
func TestThePerIssueReadRecordsGitHubsMergeStateForTheHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		state     string
		want      record.MergeState
		mergeable bool
	}{
		{"blocked on what the repository requires", "blocked", record.MergeStateBlocked, false},
		{"clean", "clean", record.MergeStateClean, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Merging, "retro",
				record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "code", Summary: "READY #86 at code"})
			w := readRuntime(pool, mergeStateStandIn(t, tc.state).URL, quietLogger())
			// The column holds whole microseconds, as every timestamp the store writes does, so
			// the moment before the read is read to the same precision before it is compared.
			opened := time.Now().UTC().Truncate(time.Microsecond)
			if _, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
				t.Fatalf("read the issue's pull request: %v", err)
			}
			got, pr := capturePhase(t, pool)
			if pr.MergeState != tc.want {
				t.Fatalf("recorded merge state = %q, want %q", pr.MergeState, tc.want)
			}
			if pr.MergeState.Mergeable() != tc.mergeable {
				t.Fatalf("%q reads as mergeable = %t, want %t", pr.MergeState, pr.MergeState.Mergeable(), tc.mergeable)
			}
			if got != phase.Merging {
				t.Fatalf("after the read the issue is in %s, want merging: recording the merge state moves nothing", got)
			}
			if pr.StateReadAt.Before(opened) || pr.StateReadAt.After(time.Now().UTC()) {
				t.Fatalf("the read is recorded as made at %v, want a moment of the read itself (after %v)", pr.StateReadAt, opened)
			}
			if _, err := w.readIssuePullRequest(context.Background(), "CAPTURE-1"); err != nil {
				t.Fatalf("read the issue's pull request again: %v", err)
			}
			_, again := capturePhase(t, pool)
			if again.MergeState != tc.want || !again.StateReadAt.After(pr.StateReadAt) {
				t.Fatalf("the second read left %q read at %v, want the same state read later than %v: a read that confirms what is recorded still moves the read time",
					again.MergeState, again.StateReadAt, pr.StateReadAt)
			}
		})
	}
}
