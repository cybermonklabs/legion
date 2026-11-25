package intake

//ci:reads packages/contracts/fixtures/github-envelopes/* TestEnvoyGoldenSubjectsReachTheirRepositorysConsumer globs it and reads each file; TestAWorkflowRunEnvelopeIsNoFact reads workflow-run-completed.json

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

type handlerFunc func(context.Context, pgx.Tx, Fact) (Result, error)

func (f handlerFunc) Apply(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
	return f(ctx, tx, fact)
}

func TestApplyFactCommitsRefusalAndLaterHandlers(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()
	refusal := &Refusal{Status: 409, Code: "DESIGN_GATE_CLOSED", Message: "approve version 3"}

	result, err := ApplyFact(ctx, pool, "dispatch", "event-refusal", DispatchIssue{Key: "LEGION-208"},
		writeHandler("first", refusal),
		writeHandler("later", nil),
	)
	if err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}
	if !reflect.DeepEqual(result.Refusal, refusal) {
		t.Fatalf("refusal = %#v, want %#v", result.Refusal, refusal)
	}
	if got := writeCount(t, pool); got != 2 {
		t.Fatalf("committed writes = %d, want 2", got)
	}
	if got, want := writeHandlers(t, pool), []string{"first", "later"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("handler order = %#v, want %#v", got, want)
	}
}

func TestApplyFactDeduplicatesBeforeHandlers(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()

	if _, err := ApplyFact(ctx, pool, "dispatch", "event-duplicate", DispatchIssue{Key: "LEGION-208"}, writeHandler("first", nil)); err != nil {
		t.Fatalf("first ApplyFact: %v", err)
	}
	result, err := ApplyFact(ctx, pool, "dispatch", "event-duplicate", DispatchIssue{Key: "LEGION-208"}, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		return Result{}, errors.New("duplicate reached a handler")
	}))
	if err != nil {
		t.Fatalf("duplicate ApplyFact: %v", err)
	}
	if result.Duplicate != true || result.Refusal != nil {
		t.Fatalf("duplicate result = %#v, want a duplicate with no refusal", result)
	}
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("writes after duplicate = %d, want 1", got)
	}
}

// Applied names, of the ids asked about, those ApplyFact applied from that source: an id applied
// under another source, or not applied at all, is not named, and no ids reads nothing.
func TestAppliedNamesOnlyTheFactsApplyFactAppliedFromTheSource(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	for _, event := range []struct{ source, id string }{{"supervise", "ready-1"}, {"supervise", "failed-2"}, {"dispatch", "ready-3"}} {
		if _, err := ApplyFact(ctx, pool, event.source, event.id, DispatchIssue{Key: "LEGION-208"}); err != nil {
			t.Fatalf("apply %s/%s: %v", event.source, event.id, err)
		}
	}
	applied, err := Applied(ctx, pool, "supervise", []string{"ready-1", "failed-2", "ready-3", "never-4"})
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if want := map[string]bool{"ready-1": true, "failed-2": true}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("Applied = %v, want %v", applied, want)
	}
	if none, err := Applied(ctx, pool, "supervise", nil); err != nil || len(none) != 0 {
		t.Fatalf("Applied of no ids = %v, %v; want none and no error", none, err)
	}
}

// An event id is built of what the fact's source sent — the API's from a reported commit, the
// daemon's reads from what GitHub answered — so it can hold a NUL, and the store records the id
// as storetext.Text makes it. Applied answers each id under the id its caller gave, so a caller
// that looks its own id up finds the fact applied: a completion the workflow already took reads
// as taken, not as new.
func TestAppliedAnswersAnIDHoldingNULThatApplyFactRecordedAsApplied(t *testing.T) {
	t.Parallel()
	pool := store.NewPool(migratedPool(t))
	ctx := context.Background()
	const id = "handoff:LEGION-208:1:1:1:implementer:implementing:1:commit\x00sha:"
	if _, err := ApplyFact(ctx, pool, "api", id, DispatchIssue{Key: "LEGION-208"}); err != nil {
		t.Fatalf("apply %q: %v", id, err)
	}
	applied, err := Applied(ctx, pool, "api", []string{id, "never\x00applied", "never-applied"})
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if want := map[string]bool{id: true}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("Applied = %#v, want %#v", applied, want)
	}
	if result, err := ApplyFact(ctx, pool, "api", id, DispatchIssue{Key: "LEGION-208"}); err != nil || !result.Duplicate {
		t.Fatalf("ApplyFact of %q again = %+v, %v; want the duplicate it is", id, result, err)
	}
}

func TestApplyFactRollsBackHandlerAndDeduplicationOnError(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()

	_, err := ApplyFact(ctx, pool, "dispatch", "event-rollback", DispatchIssue{Key: "LEGION-208"},
		writeHandler("rolled-back", nil),
		handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, errors.New("handler failed") }),
	)
	if err == nil || !strings.Contains(err.Error(), "handler failed") {
		t.Fatalf("ApplyFact error = %v, want handler error", err)
	}
	if got := writeCount(t, pool); got != 0 {
		t.Fatalf("writes after rollback = %d, want 0", got)
	}

	if _, err := ApplyFact(ctx, pool, "dispatch", "event-rollback", DispatchIssue{Key: "LEGION-208"}, writeHandler("retry", nil)); err != nil {
		t.Fatalf("retry ApplyFact: %v", err)
	}
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("writes after retry = %d, want 1", got)
	}
}

// An Observed fact, Dispatch's view of an issue the workflow did not record when the fact was
// prepared, reaches the handlers as a plain observation while the issue stays unrecorded, with
// neither field of the view. Once the issue is recorded, the handlers get the event the view was
// prepared in place of, carrying the view's Ancestry and Reread, which the preparation read
// for the view alone, or nothing for release_children's view, prepared in place of none.
func TestApplyFactSettlesAViewByWhetherItsIssueIsRecorded(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	var seen []DispatchIssue
	recorder := handlerFunc(func(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
		seen = append(seen, fact.(DispatchIssue))
		return Result{}, nil
	})
	apply := func(id string, fact Fact) {
		t.Helper()
		if _, err := ApplyFact(ctx, pool, "test", id, fact, recorder); err != nil {
			t.Fatalf("ApplyFact %s: %v", id, err)
		}
	}
	event := DispatchIssue{Key: "LEGION-208", Seq: 5, Status: "todo", HandedOver: true, ActorSession: "session", ResolvedRepo: "acme/widgets"}
	reread := map[string][]string{"LEGION-214": nil}
	ancestry := []string{"LEGION-169", "LEGION-168"}
	view := DispatchIssue{Key: "LEGION-208", Seq: 7, Status: "backlog", ResolvedRepo: "acme/widgets", Ancestry: ancestry, Reread: reread, Observed: true, Delivered: &event}
	apply("unrecorded", view)
	if want := (DispatchIssue{Key: "LEGION-208", Seq: 7, Status: "backlog", ResolvedRepo: "acme/widgets", Ancestry: ancestry, Reread: reread}); len(seen) != 1 || !reflect.DeepEqual(seen[0], want) {
		t.Fatalf("the handlers saw %+v for an unrecorded issue's view, want only %+v", seen, want)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted,
			Generation: 1, Status: "in_progress", Rank: "U", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
	}); err != nil {
		t.Fatalf("record LEGION-208: %v", err)
	}
	seen = nil
	apply("recorded", view)
	apply("recorded-release", DispatchIssue{Key: "LEGION-208", Seq: 8, Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets", Observed: true})
	delivered := event
	delivered.Ancestry, delivered.Reread = ancestry, reread
	if len(seen) != 1 || !reflect.DeepEqual(seen[0], delivered) {
		t.Fatalf("the handlers saw %+v once the issue was recorded, want only the delivered event %+v", seen, delivered)
	}
}

// A handler's OnCommit hook runs once ApplyFact's transaction has committed, in the order handlers
// registered them, and sees what the fact wrote; a fact whose later handler fails rolls back and
// runs none. A duplicate runs no handler, so it registers none.
func TestOnCommitRunsOnlyAfterTheFactCommits(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()
	var ran []string
	hooked := func(name string) Handler {
		return handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
			if _, err := writeHandler(name, nil).Apply(ctx, tx, fact); err != nil {
				return Result{}, err
			}
			store.OnCommit(ctx, func() { ran = append(ran, fmt.Sprintf("%s saw %d", name, writeCount(t, pool))) })
			return Result{}, nil
		})
	}

	if _, err := ApplyFact(ctx, pool, "dispatch", "event-hooks-rolled-back", DispatchIssue{Key: "LEGION-208"},
		hooked("first"),
		handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, errors.New("handler failed") }),
	); err == nil {
		t.Fatal("ApplyFact with a failing handler succeeded")
	}
	if len(ran) != 0 {
		t.Fatalf("hooks %v ran for a fact that rolled back", ran)
	}

	for range 2 {
		if _, err := ApplyFact(ctx, pool, "dispatch", "event-hooks", DispatchIssue{Key: "LEGION-208"}, hooked("first"), hooked("second")); err != nil {
			t.Fatalf("ApplyFact: %v", err)
		}
	}
	if fmt.Sprint(ran) != "[first saw 2 second saw 2]" {
		t.Fatalf("hooks ran %v; want both once, in order, each seeing both committed writes", ran)
	}
}

// A cancel of the caller's context that lands as ApplyFact commits does not abandon the commit
// (store.Transact): the fact commits, ApplyFact reports it applied, and its OnCommit hooks run. A
// commit abandoned after it was sent can still land, and then ApplyFact would report a committed
// fact as failed and skip the hooks that say what it did, such as admission's "slot taken" line
// for a slot the store holds.
func TestACancelAtCommitStillCommitsTheFactAndRunsItsHooks(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran []string
	hooked := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if _, err := writeHandler("first", nil).Apply(ctx, tx, fact); err != nil {
			return Result{}, err
		}
		store.OnCommit(ctx, func() { ran = append(ran, fmt.Sprintf("saw %d", writeCount(t, pool))) })
		return Result{}, nil
	})

	if _, err := ApplyFact(ctx, cancelAtCommit{TxPool: pool, cancel: cancel}, "dispatch", "event-cancel-at-commit", DispatchIssue{Key: "LEGION-208"}, hooked); err != nil {
		t.Fatalf("ApplyFact with its context canceled at commit: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the caller's context was not canceled at commit, so this proves nothing")
	}
	if fmt.Sprint(ran) != "[saw 1]" {
		t.Fatalf("hooks ran %v; want one, seeing the committed write", ran)
	}
}

// A fact whose context ends while its handlers run, before ApplyFact commits, does not commit and
// runs no hook, though every handler returned nil: that is how consumeMessage cuts a handling past
// its limit whose step ignored the context, and how a stop ends a fact still being applied.
func TestAFactWhoseContextEndedBeforeItsCommitDoesNotCommit(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran []string
	cutWhileRunning := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if _, err := writeHandler("first", nil).Apply(ctx, tx, fact); err != nil {
			return Result{}, err
		}
		store.OnCommit(ctx, func() { ran = append(ran, "hook") })
		cancel()
		return Result{}, nil
	})

	if _, err := ApplyFact(ctx, pool, "dispatch", "event-cut-before-commit", DispatchIssue{Key: "LEGION-208"}, cutWhileRunning); !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyFact with its context canceled before its commit = %v, want context.Canceled", err)
	}
	if len(ran) != 0 {
		t.Fatalf("hooks %v ran for a fact whose context ended before its commit", ran)
	}
	if got := writeCount(t, pool); got != 0 {
		t.Fatalf("%d writes committed for a fact whose context ended before its commit, want none", got)
	}
}

// A fact whose context ends after its transaction's id is read, before the commit, does not
// commit (store.Transact): no commit reaches its transaction, nothing is written and no hook runs.
// Its retry on a live context applies, and its hook runs once.
func TestAFactCancelledAfterItsIDReadDoesNotCommit(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran atomic.Int32
	cut := testdb.CancelAfterScan(pool, "pg_current_xact_id_if_assigned", cancel)

	if _, err := ApplyFact(ctx, cut, "dispatch", "event-cancel-after-id", DispatchIssue{Key: "LEGION-208"}, hooked(&ran)); !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyFact cancelled after its id read = %v, want context.Canceled", err)
	}
	if n := cut.Commits(); n != 0 {
		t.Errorf("%d commits reached the fact's transaction, want none", n)
	}
	if n := writeCount(t, pool); n != 0 {
		t.Errorf("%d writes committed for a fact cancelled before its commit, want none", n)
	}
	if n := ran.Load(); n != 0 {
		t.Errorf("the fact's hook ran %d times, want none: nothing committed", n)
	}

	retry, err := ApplyFact(context.Background(), pool, "dispatch", "event-cancel-after-id", DispatchIssue{Key: "LEGION-208"}, hooked(&ran))
	if err != nil {
		t.Fatalf("the retry = %v, want nil", err)
	}
	if retry.Duplicate {
		t.Errorf("the retry = %+v, want it applied: the first never committed", retry)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("the hook ran %d times after the retry, want once", n)
	}
}

// cancelAtCommit is a pool whose transactions cancel the context of the fact they apply as they
// commit: the cancel a stopping daemon sends while a fact's commit is on its way to the server.
type cancelAtCommit struct {
	store.TxPool
	cancel context.CancelFunc
}

func (p cancelAtCommit) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.TxPool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return cancelingTx{Tx: tx, cancel: p.cancel}, nil
}

type cancelingTx struct {
	pgx.Tx
	cancel context.CancelFunc
}

func (tx cancelingTx) Commit(ctx context.Context) error {
	tx.cancel()
	return tx.Tx.Commit(ctx)
}

func TestDecodeCapturedProducerEnvelopes(t *testing.T) {
	t.Parallel()
	updatedAt := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	// mark is a captured Dispatch event's place in its issue's log: its seq and created_at.
	mark := func(seq int64, at string) record.EventMark {
		created, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			t.Fatalf("parse captured created_at %q: %v", at, err)
		}
		return record.EventMark{Seq: seq, At: created}
	}
	cases := []struct {
		name    string
		subject string
		file    string
		want    Fact
	}{
		{
			name:    "Dispatch issue created",
			subject: "notifications.dispatch.issue.CAPTURE-4.issue.created",
			file:    "dispatch/issue-created.json",
			want:    DispatchIssue{Key: "CAPTURE-4", Seq: 1, Type: "issue.created", Status: "triage", Title: "Captured approval issue", Rank: "UUUU", Labels: []string{}},
		},
		{
			name:    "Dispatch issue updated",
			subject: "notifications.dispatch.issue.CAPTURE-3.issue.updated",
			file:    "dispatch/issue-updated.json",
			want:    DispatchIssue{Key: "CAPTURE-3", Seq: 2, Type: "issue.updated", Status: "todo", Title: "Captured workflow issue", Rank: "UUU", ResolvedRepo: "sjawhar/legion", Labels: []string{}},
		},
		{
			name:    "Dispatch issue closed",
			subject: "notifications.dispatch.issue.CAPTURE-4.issue.closed",
			file:    "dispatch/issue-closed.json",
			want:    DispatchIssue{Key: "CAPTURE-4", Seq: 6, Type: "issue.closed", Status: "done", Title: "Captured approval issue", Rank: "UUUU", ResolvedRepo: "sjawhar/legion", Labels: []string{}},
		},
		{
			name:    "Dispatch artifact version",
			subject: "notifications.dispatch.issue.CAPTURE-4.artifact.version",
			file:    "dispatch/artifact-version.json",
			want: DispatchArtifact{Key: "CAPTURE-4", ArtifactID: "0544d460-0931-4374-b20b-790408519edd", Kind: DispatchArtifactVersion, Version: 2,
				Mark: mark(2, "2026-09-23T00:46:19.000081Z"), Actor: dispatch.Actor{Kind: "user", ID: "smoke"}},
		},
		{
			name:    "Dispatch artifact approved",
			subject: "notifications.dispatch.issue.CAPTURE-4.artifact.approved",
			file:    "dispatch/artifact-approved.json",
			want: DispatchArtifact{Key: "CAPTURE-4", ArtifactID: "0544d460-0931-4374-b20b-790408519edd", Kind: DispatchArtifactApproved, Version: 2,
				Actor: dispatch.Actor{Kind: "user", ID: "smoke"}, Mark: mark(5, "2026-09-23T00:46:26.997872Z")},
		},
		{
			name:    "Dispatch artifact changes requested",
			subject: "notifications.dispatch.issue.CAPTURE-3.artifact.changes_requested",
			file:    "dispatch/artifact-changes-requested.json",
			want: DispatchArtifact{Key: "CAPTURE-3", ArtifactID: "e7860036-ca1a-4ec6-8bd0-51d5f1b6fbd8", Kind: DispatchArtifactChangesRequested, Version: 2, Reason: "Captured reviewer reason",
				Actor: dispatch.Actor{Kind: "user", ID: "smoke"}, Mark: mark(7, "2026-09-23T00:45:50.37209Z")},
		},
		// The ask and claim envelopes below are built by hand from Dispatch's own types
		// (model.AskEventPayload, model.CommentEventPayload with ask_state and ask_waiting_on, and
		// the issue payload with claim and previous_claim), not captured from a running Dispatch as
		// the envelopes above were; ask_events_decode_test.go covers the fields they leave out.
		{
			name:    "Dispatch ask opened (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.ask.opened",
			file:    "dispatch/ask-opened.json",
			want:    DispatchAskOpened{Key: "CAPTURE-4", Ask: "6c83cb7f-8f4e-4820-9e05-7c94fa725007", Kind: "question", AuthorSession: "session-architect", Mark: mark(6, "2026-09-23T00:46:31Z")},
		},
		{
			name:    "Dispatch ask answered (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.ask.answered",
			file:    "dispatch/ask-answered.json",
			want: DispatchAskClosed{Key: "CAPTURE-4", Ask: "6c83cb7f-8f4e-4820-9e05-7c94fa725007", Kind: "question", State: "answered", Actor: dispatch.Actor{Kind: "user", ID: "human"},
				Mark: mark(9, "2026-09-23T00:46:34Z")},
		},
		{
			name:    "Dispatch ask resolved (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.ask.resolved",
			file:    "dispatch/ask-resolved.json",
			want: DispatchAskClosed{Key: "CAPTURE-4", Ask: "6c83cb7f-8f4e-4820-9e05-7c94fa725007", Kind: "question", State: "resolved", Actor: dispatch.Actor{Kind: "session", ID: "session-architect"},
				Mark: mark(10, "2026-09-23T00:46:35Z")},
		},
		// A hand-on moves which person's Inbox the ask sits in, and no root waits on a person,
		// so the daemon reads the type by ignoring it (decode.go's default arm): no fact, no
		// error, and nothing of the workflow moves.
		{
			name:    "Dispatch ask handed to another person (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.ask.asked_of",
			file:    "dispatch/ask-asked-of.json",
			want:    nil,
		},
		{
			name:    "Dispatch ask resolved as a person's decline (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.ask.resolved",
			file:    "dispatch/ask-resolved-declined.json",
			want: DispatchAskClosed{Key: "CAPTURE-4", Ask: "9f2b7c41-6d0a-4e3b-8c57-1ab2d3e4f506", Kind: "question", State: "resolved",
				Decline: &record.AskDecline{Note: "The vendor owns that date; I do not.", Words: declineDispatchWords},
				Actor:   dispatch.Actor{Kind: "user", ID: "human"}, Mark: mark(13, "2026-09-23T00:46:38Z")},
		},
		{
			name:    "Dispatch ask reply returning the turn to the agent (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.comment.created",
			file:    "dispatch/comment-ask-agent.json",
			want: DispatchAskReply{Key: "CAPTURE-4", Ask: "6c83cb7f-8f4e-4820-9e05-7c94fa725007", WaitingOn: "agent", Actor: dispatch.Actor{Kind: "user", ID: "human"},
				Mark: mark(7, "2026-09-23T00:46:32Z"), Comment: "4c4b2a5f-93df-4aeb-a2ca-b5c74187c8bf"},
		},
		{
			name:    "Dispatch ask reply leaving the turn with the human (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.comment.created",
			file:    "dispatch/comment-ask-human.json",
			want: DispatchAskReply{Key: "CAPTURE-4", Ask: "6c83cb7f-8f4e-4820-9e05-7c94fa725007", WaitingOn: "human", Actor: dispatch.Actor{Kind: "session", ID: "session-architect"},
				Mark: mark(8, "2026-09-23T00:46:33Z"), Comment: "65f690c1-bd91-4a42-81aa-aaf136fcc51c"},
		},
		{
			name:    "Dispatch issue claimed (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.issue.claimed",
			file:    "dispatch/issue-claimed.json",
			want:    DispatchClaim{Key: "CAPTURE-4", Seq: 11, ClaimedBy: "user:human"},
		},
		{
			name:    "Dispatch issue released (hand-built)",
			subject: "notifications.dispatch.issue.CAPTURE-4.issue.released",
			file:    "dispatch/issue-released.json",
			want:    DispatchClaim{Key: "CAPTURE-4", Seq: 12},
		},
		{
			name:    "pull request opened",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-opened.json",
			want: PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main", HeadSHA: "head-captured", Body: "Dispatch: LEGION-208",
				URL: "https://github.com/sjawhar/legion/pull/42", UpdatedAt: updatedAt},
		},
		{
			name:    "pull request synchronized",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-synchronized.json",
			want: PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Before: "head-before", Sender: "legion-implementer[bot]",
				HeadSHA: "head-captured", Base: "main", Body: "Dispatch: LEGION-208", UpdatedAt: updatedAt},
		},
		{
			name:    "pull request closed",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-closed.json",
			want:    PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-captured", UpdatedAt: updatedAt},
		},
		{
			name:    "pull request merged",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-merged.json",
			want:    PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-captured", HeadSHA: "head-captured"},
		},
		{
			name:    "pull request review",
			subject: "notifications.github.sjawhar.legion.pr.42.review",
			file:    "github/review.json",
			want:    PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head-captured", HeadSHA: "head-captured", Author: "reviewer", Body: "Captured review"},
		},
		{
			name:    "checks settlement",
			subject: "notifications.github.sjawhar.legion.pr.42.checks",
			file:    "github/checks.json",
			want:    PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "abcdef1234567890abcdef1234567890abcdef12", CheckRuns: []CheckRun{{Name: "unit", ID: 73}}, Snapshot: "ed3e3bafc46f498bca65fe879fcd1765a90fecbb1fcd62579e46a94707c0bacd", Failing: []string{"unit"}, Cancelled: []string{}},
		},
		{
			name:    "branch push",
			subject: "notifications.github.sjawhar.legion.push.branch.legion/LEGION-208",
			file:    "github/push.json",
			want:    Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: "before-captured", After: "head-captured", Pusher: "author", Sender: "author"},
		},
		{
			name:    "base branch push",
			subject: "notifications.github.sjawhar.legion.push.branch.main",
			file:    "github/push-base.json",
			want:    BasePush{Repo: "sjawhar/legion", Branch: "main", After: "base-captured"},
		},
		{
			name:    "comment",
			subject: "notifications.github.sjawhar.legion.pr.42.comment",
			file:    "github/comment.json",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatalf("read captured envelope: %v", err)
			}
			got, err := decodeMessage(tc.subject, "CAPTURE", capturedRepositories, data)
			if err != nil {
				t.Fatalf("decode captured envelope: %v", err)
			}
			if !reflect.DeepEqual(got.Fact, tc.want) {
				t.Fatalf("fact = %#v, want %#v", got.Fact, tc.want)
			}
		})
	}
}

func TestCapturedIssueUpdatedEnvelopeDecodes(t *testing.T) {
	t.Parallel()
	data := capturedIssueUpdatedEnvelope(t)
	if _, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, data); err != nil {
		t.Fatalf("decode captured issue.updated envelope: %v", err)
	}
}

// Dispatch can add issue payload fields, or start filling one it has always sent empty, before a
// deployed daemon consumes them: the old decoder must keep accepting those events until its
// matching workflow change rolls out. The daemon reads no key the captured fixture lacks, which a
// decoder rejecting unknown fields would refuse; it does read the priority, which the filled
// fixture carries as P1 and the fact carries beside everything the original says.
func TestIssueEventIgnoresFieldsTheDaemonDoesNotConsume(t *testing.T) {
	t.Parallel()
	original := string(capturedIssueCreatedEnvelope(t))
	filled := strings.Replace(original, `\"priority\":null`, `\"priority\":1`, 1)
	if filled == original {
		t.Fatal("captured issue.created fixture has no priority field to fill")
	}
	widened := strings.Replace(filled, `\"labels\":[]`, `\"some_future_field\":1,\"labels\":[]`, 1)
	if widened == filled {
		t.Fatal("captured issue.created fixture has no empty labels field to widen beside")
	}
	subject := "notifications.dispatch.issue.CAPTURE-4.issue.created"
	want, err := decodeMessage(subject, "CAPTURE", capturedRepositories, []byte(original))
	if err != nil {
		t.Fatalf("decode captured issue.created: %v", err)
	}
	got, err := decodeMessage(subject, "CAPTURE", capturedRepositories, []byte(widened))
	if err != nil {
		t.Fatalf("decode issue.created with a priority and a field the fixture lacks: %v", err)
	}
	carried, ok := want.Fact.(DispatchIssue)
	if !ok {
		t.Fatalf("captured issue.created decodes to %#v, want a Dispatch issue", want.Fact)
	}
	carried.Priority = record.P1
	if !reflect.DeepEqual(got.Fact, carried) {
		t.Fatalf("widened fact = %#v, want %#v", got.Fact, carried)
	}
}

// A listener released before it forwarded a synchronize's before and sender sends the synchronize
// without them, and a push without its sender: the daemon decodes the synchronize as an arrival
// that names no move (PullRequestSynchronized.Before and Sender empty), which classify judges as the
// release before this one did, the push with no sender, and every other field as before. A listener
// that sends before but not the sender gives an arrival naming its before alone (design (c′)).
func TestASynchronizeFromAListenerWithoutBeforeNamesNoMove(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/github/pr-synchronized.json")
	if err != nil {
		t.Fatalf("read captured envelope: %v", err)
	}
	noSender := strings.Replace(string(data), `\"sender\":\"legion-implementer[bot]\",`, "", 1)
	older := strings.Replace(noSender, `\"before\":\"head-before\",`, "", 1)
	if older == noSender || noSender == string(data) {
		t.Fatal("captured synchronize fixture carries no before or no sender to take out")
	}
	updated := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	want := PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-captured", Base: "main", Body: "Dispatch: LEGION-208",
		UpdatedAt: updated}
	for _, tc := range []struct {
		name, data string
		before     string
	}{{"neither", older, ""}, {"before alone", noSender, "head-before"}} {
		got, err := decodeMessage("notifications.github.sjawhar.legion.pr.42", "CAPTURE", capturedRepositories, []byte(tc.data))
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		want.Before = tc.before
		if !reflect.DeepEqual(got.Fact, want) {
			t.Fatalf("%s: fact = %#v, want %#v", tc.name, got.Fact, want)
		}
	}
	push, err := os.ReadFile("testdata/github/push.json")
	if err != nil {
		t.Fatalf("read captured push: %v", err)
	}
	unsent := strings.Replace(string(push), `,\"sender\":\"author\"`, "", 1)
	if unsent == string(push) {
		t.Fatal("captured push fixture carries no sender to take out")
	}
	got, err := decodeMessage("notifications.github.sjawhar.legion.push", "CAPTURE", capturedRepositories, []byte(unsent))
	if err != nil {
		t.Fatalf("decode a push without sender: %v", err)
	}
	if p, ok := got.Fact.(Push); !ok || p.Sender != "" || p.Pusher != "author" {
		t.Fatalf("push fact = %#v, want one with no sender and its pusher", got.Fact)
	}
}

// GitHub sends the pull_request action `dequeued` when a pull request leaves the merge queue, and
// the listener publishes it with the keys it writes for every pull_request action (Envoy's
// normalizer, packages/envoy/internal/contracts/normalize.go): GitHub's reason for the removal is
// not one of them. On an issue's branch the event is the pull request and the head it left the
// queue at. One with no head names no head, so it is no fact, as a synchronize with none is no fact.
// One on a branch that is no issue's is no fact either, as every pull request event there is
// (decodePullRequest), so it is acknowledged without a transaction.
func TestDecodeADequeuedPullRequest(t *testing.T) {
	t.Parallel()
	const subject = "notifications.github.sjawhar.legion.pr.42"
	envelope := func(branch, head string) []byte {
		t.Helper()
		payload, err := json.Marshal(map[string]string{
			"kind": "pr", "action": "dequeued", "repo": "sjawhar/legion", "number": "42", "title": "Captured GitHub event",
			"author": "author", "url": "https://github.com/sjawhar/legion/pull/42", "head_sha": head, "before": "",
			"sender": "github-merge-queue[bot]", "head_ref": branch, "head_repo": "sjawhar/legion", "base_ref": "main",
			"merged": "false", "merge_commit_sha": "", "merged_by": "", "updated_at": "2026-09-23T00:00:00Z",
			"body": "Dispatch: LEGION-208",
		})
		if err != nil {
			t.Fatalf("marshal the dequeued payload: %v", err)
		}
		data, err := json.Marshal(map[string]any{
			"event_id": "dequeued-event", "source": "github", "source_event_id": "dequeued-delivery", "topic": subject,
			"dedupe_key": "github.dequeued-delivery", "issued_at": int64(1_790_124_795_391),
			"payload_summary": "pr dequeued: sjawhar/legion#42 Captured GitHub event", "payload": string(payload),
			"trace_id": "dequeued-trace",
		})
		if err != nil {
			t.Fatalf("marshal the dequeued envelope: %v", err)
		}
		return data
	}
	for _, tc := range []struct {
		name, branch, head string
		want               Fact
	}{
		{name: "on an issue's branch, with its head", branch: "legion/LEGION-208", head: "head-captured",
			want: PullRequestDequeued{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-captured"}},
		{name: "on an issue's branch, with no head", branch: "legion/LEGION-208"},
		{name: "on a branch that is no issue's", branch: "feature/faster-builds", head: "head-captured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeMessage(subject, "CAPTURE", capturedRepositories, envelope(tc.branch, tc.head))
			if err != nil {
				t.Fatalf("decode the dequeued event: %v", err)
			}
			if !reflect.DeepEqual(got.Fact, tc.want) {
				t.Fatalf("fact = %#v, want %#v", got.Fact, tc.want)
			}
		})
	}
}

// Dispatch sends an issue's blocked_by keys on every issue event, already ordered: the decoder
// carries that order onto the fact, and an event retained from a Dispatch that sent no such field
// decodes as an issue waiting on nothing does.
func TestADispatchIssueEventCarriesTheKeysItIsBlockedBy(t *testing.T) {
	t.Parallel()
	subject := "notifications.dispatch.issue.CAPTURE-3.issue.updated"
	original := string(capturedIssueUpdatedEnvelope(t))
	blocked := strings.Replace(original, `\"labels\":[]`, `\"blocked_by\":[\"CAPTURE-9\",\"CAPTURE-2\"],\"labels\":[]`, 1)
	if blocked == original {
		t.Fatal("captured issue.updated fixture has no empty labels field to widen")
	}
	got, err := decodeMessage(subject, "CAPTURE", capturedRepositories, []byte(blocked))
	if err != nil {
		t.Fatalf("decode issue.updated with blocked_by: %v", err)
	}
	want := []string{"CAPTURE-9", "CAPTURE-2"}
	if blockers := got.Fact.(DispatchIssue).Blockers; !reflect.DeepEqual(blockers, want) {
		t.Fatalf("blockers = %#v, want %#v in Dispatch's own order", blockers, want)
	}
	got, err = decodeMessage(subject, "CAPTURE", capturedRepositories, []byte(original))
	if err != nil {
		t.Fatalf("decode captured issue.updated: %v", err)
	}
	if blockers := got.Fact.(DispatchIssue).Blockers; len(blockers) != 0 {
		t.Fatalf("blockers = %#v, want none: the payload carries no blocked_by", blockers)
	}
}

// An issue event's labels say whether it is handed to Legion; decodeDispatchFact resolves that
// once, into HandedOver, matching the label in any case among any others, and every decision reads
// that bool. The raw list rides on the fact too (Labels), for the copy of Dispatch's issues, which
// is its one reader.
func TestADispatchIssueEventResolvesHandedOverFromItsLabels(t *testing.T) {
	t.Parallel()
	data := strings.Replace(string(capturedIssueUpdatedEnvelope(t)), `\"labels\":[]`, `\"labels\":[\"frontend\",\"Legion\"]`, 1)
	got, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, []byte(data))
	if err != nil {
		t.Fatalf("decode labeled issue.updated envelope: %v", err)
	}
	issue, ok := got.Fact.(DispatchIssue)
	if !ok || !issue.HandedOver {
		t.Fatalf("fact = %#v, want a Dispatch issue with HandedOver true (labels included Legion)", got.Fact)
	}
}

// An event JetStream retained from before Dispatch resolved an issue's repository carries no
// resolved_repo. It is never refused: it carries its actor and sequence, which the workflow's
// fences read, so it decodes with ResolvedRepoMissing set, its repository empty and every other
// field as the event says. The consumer's PrepareRepository fills the repository before the fact
// is applied. A null resolved_repo is a Dispatch that resolves the issue to none.
func TestADispatchIssueEventWithoutResolvedRepoDecodesWithItsRepositoryMissing(t *testing.T) {
	t.Parallel()
	original := string(capturedIssueUpdatedEnvelope(t))
	older := strings.Replace(original, `\"repo\":null,\"resolved_repo\":\"sjawhar/legion\",`, "", 1)
	if older == original {
		t.Fatal("captured issue.updated fixture carries no resolved_repo to remove")
	}
	decoded, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, []byte(older))
	if err != nil {
		t.Fatalf("decode of an event without resolved_repo = %v, want it decoded", err)
	}
	want := DispatchIssue{Key: "CAPTURE-3", Seq: 2, Type: "issue.updated", Status: "todo", Title: "Captured workflow issue", Rank: "UUU", ResolvedRepoMissing: true, Labels: []string{}}
	if !reflect.DeepEqual(decoded.Fact, want) {
		t.Fatalf("fact = %#v, want %#v", decoded.Fact, want)
	}
	none := strings.Replace(original, `\"resolved_repo\":\"sjawhar/legion\"`, `\"resolved_repo\":null`, 1)
	decoded, err = decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, []byte(none))
	if err != nil || decoded.Fact.(DispatchIssue).ResolvedRepoMissing || decoded.Fact.(DispatchIssue).ResolvedRepo != "" {
		t.Fatalf("decode of an event resolving to none = %#v, %v; want an empty repository, not missing", decoded.Fact, err)
	}
}

// GitHub owner and repository names are case-insensitive, so an event of a repository GitHub
// intake hears in another spelling is the daemon's: the decoder matches its repository against the
// heard set case-insensitively, and the fact names the heard repository.
func TestAGitHubEventOfAHeardRepositoryInAnotherCaseIsDecoded(t *testing.T) {
	t.Parallel()
	heard := []ghrepo.Repository{ghrepo.MustParse("SJawhar/Legion")}
	decoded, err := decodeMessage("notifications.github.sjawhar.legion.pr.42", "CAPTURE", heard, capturedGitHubEnvelope(t, "pr-opened.json"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	opened, ok := decoded.Fact.(PullRequestOpened)
	if !ok || opened.Repo != "SJawhar/Legion" {
		t.Fatalf("fact = %#v, want a PullRequestOpened on SJawhar/Legion", decoded.Fact)
	}
}

// settingsSubject is the subject Dispatch publishes its project settings event
// settings.repo_project.updated on, for project: Dispatch's own event of its repo-projects table,
// which no consumer of the daemon filters on. It is named only to prove that, and
// dispatch://INFRA-3532's own change deletes it and its test, with the rename pass.
func settingsSubject(project string) string {
	return "notifications.dispatch.project." + project + ".settings.repo_project.updated"
}

// A boot narrows the Dispatch consumer an earlier daemon made to the issue events alone: the
// consumer that heard the subject of Dispatch's settings.repo_project.updated beside them is
// updated in place, its position kept, and its filters read back exactly
// notifications.dispatch.issue.>. An event Dispatch then publishes on that settings subject is
// never delivered: once an issue event published after it is acknowledged, nothing is pending
// or awaiting its acknowledgement, and processed_events holds a row for the issue events alone.
//
// The read-back of the filters is the control: with the settings subject put back among them, and
// no decode of its event left (A8: no branch stays for it), the event is delivered and terminated
// as poison, so the counts and the rows below read the same, and only the filters tell the two
// apart (INFRA-4401 P4; dispatch://INFRA-4401 check a-go, item 2). dispatch://INFRA-3532's own
// change deletes this test, which names Dispatch's settings event only to prove the daemon hears
// none of it, with the rename pass.
func TestTheBootNarrowsTheDispatchConsumerToTheIssueEvents(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	backlog := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	if _, err := stream.CreateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:        dispatchConsumerName(spec.Project),
		FilterSubjects: []string{"notifications.dispatch.issue.>", settingsSubject(spec.Project)},
		AckPolicy:      jetstream.AckExplicitPolicy, AckWait: spec.AckWait,
	}); err != nil {
		t.Fatalf("create the earlier daemon's consumer: %v", err)
	}
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers over the earlier daemon's consumer: %v", err)
	}
	narrowed := consumerInfo(t, stream, dispatchConsumerName(spec.Project))
	if want := []string{"notifications.dispatch.issue.>"}; narrowed.Config.FilterSubject != "" || !slices.Equal(narrowed.Config.FilterSubjects, want) {
		t.Fatalf("Dispatch consumer filters = %q %v, want exactly %v", narrowed.Config.FilterSubject, narrowed.Config.FilterSubjects, want)
	}
	if narrowed.NumPending != 1 || narrowed.Delivered.Stream != 0 {
		t.Fatalf("the narrowed consumer has %d pending and delivered through stream sequence %d, want its backlog of 1 (stream sequence %d) kept and nothing delivered: the boot updates the consumer in place",
			narrowed.NumPending, narrowed.Delivered.Stream, backlog.Sequence)
	}
	stop := runConsumers(t, consumers, writeHandler("dispatch", nil))
	defer stop()

	settings := envelopeJSON(t, "dispatch-70-"+spec.Project, "dispatch", fmt.Sprintf(`{"id":70,"issue_key":null,"artifact_id":null,"project":%q,"seq":0,"type":"settings.repo_project.updated","actor":{"kind":"user","id":"smoke"},"notify":true,"created_at":"2026-10-16T18:00:00Z","payload":{"mapping":{"repo":"acme/widgets","project":%q,"default":false},"deleted":false}}`, spec.Project, spec.Project))
	publish(t, js, settingsSubject(spec.Project), settings)
	last := publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatchConsumerName(spec.Project), last)
	if info := consumerInfo(t, stream, dispatchConsumerName(spec.Project)); info.NumPending != 0 || info.NumAckPending != 0 {
		t.Fatalf("the Dispatch consumer has %d pending and %d awaiting acknowledgement, want none", info.NumPending, info.NumAckPending)
	}
	if got := writeCount(t, pool); got != 2 {
		t.Fatalf("%d facts applied, want the two issue events alone", got)
	}
	rows, err := pool.Query(context.Background(), `select event_id from processed_events where source = 'dispatch' order by event_id`)
	if err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	marks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	if want := []string{"dispatch-15", "dispatch-24"}; !slices.Equal(marks, want) {
		t.Fatalf("processed_events = %q, want the two issue events alone, %q, and no row for the settings event", marks, want)
	}
}

// The repositories GitHub intake hears change at runtime (SetRepositories): the
// GitHub consumer's filters and the decoder's set change together, so a newly listed repository's
// event is applied and an unlisted one's is acknowledged untouched.
func TestSetRepositoriesChangesTheRepositoriesGitHubIntakeHears(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	stop := runConsumers(t, consumers, writeHandler("github", nil))
	defer stop()

	widgets := []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("acme/widgets")}
	if err := consumers.SetRepositories(context.Background(), widgets); err != nil {
		t.Fatalf("SetRepositories: %v", err)
	}
	if got, want := consumerInfo(t, stream, githubConsumerName(spec.Project)).Config.FilterSubjects, githubFilters(widgets); !slices.Equal(got, want) {
		t.Fatalf("GitHub consumer filters = %v, want %v", got, want)
	}
	opened := strings.ReplaceAll(string(capturedGitHubEnvelope(t, "pr-opened.json")), "sjawhar/legion", "acme/widgets")
	publish(t, js, "notifications.github.acme.widgets.pr.42", []byte(opened))
	testwait.Eventually(t, "the newly listed repository's event applied", func() bool { return writeCount(t, pool) == 1 })

	if err := consumers.SetRepositories(context.Background(), capturedRepositories); err != nil {
		t.Fatalf("SetRepositories: %v", err)
	}
	if decoded, err := decodeMessage("notifications.github.acme.widgets.pr.42", spec.Project, consumers.repositories(), []byte(opened)); err != nil || decoded.Fact != nil {
		t.Fatalf("decode of an unlisted repository's event = %#v, %v; want no fact", decoded.Fact, err)
	}
	// A daemon serving no repository hears none: the consumer filters on a subject no repository's
	// events are published on, never on none, which would hear every subject of the stream.
	if err := consumers.SetRepositories(context.Background(), nil); err != nil {
		t.Fatalf("SetRepositories of none: %v", err)
	}
	if got := consumerInfo(t, stream, githubConsumerName(spec.Project)).Config.FilterSubjects; !slices.Equal(got, []string{"notifications.github.-.-"}) {
		t.Fatalf("GitHub consumer filters hearing none = %v, want the one no repository publishes on", got)
	}
	if decoded, err := decodeMessage("notifications.github.sjawhar.legion.pr.42", spec.Project, consumers.repositories(), capturedGitHubEnvelope(t, "pr-opened.json")); err != nil || decoded.Fact != nil {
		t.Fatalf("decode while hearing none = %#v, %v; want no fact", decoded.Fact, err)
	}
}

// A SetRepositories whose consumer update fails changes nothing: the decoder's set stays the
// repositories the GitHub consumer still filters on, so the caller can retry it and nothing decodes
// an event of a repository the consumer was never told to hear.
func TestAFailedSetRepositoriesChangesNothing(t *testing.T) {
	t.Parallel()
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	consumers, err := OpenConsumers(context.Background(), js, migratedPool(t), spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	widgets := []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("acme/widgets")}
	if err := consumers.SetRepositories(canceled, widgets); err == nil {
		t.Fatal("SetRepositories with a failed consumer update reported no failure")
	}
	if got := consumers.repositories(); !slices.Equal(got, capturedRepositories) {
		t.Fatalf("the decoder's set after a failed update = %v, want it unchanged, %v", got, capturedRepositories)
	}
	if got, want := consumerInfo(t, stream, githubConsumerName(spec.Project)).Config.FilterSubjects, githubFilters(capturedRepositories); !slices.Equal(got, want) {
		t.Fatalf("GitHub consumer filters = %v, want them unchanged, %v", got, want)
	}
}

// The consumer has the daemon prepare an issue event's resolved repository before the fact is
// applied (ConsumerSpec.PrepareRepository): the workflow performs no I/O, so what it reads of the
// repository is read here, and a failed read is retried rather than applied without it. The fact
// the preparation returns is the one applied: a retained event's missing repository is filled
// there.
func TestConsumePreparesAnIssueEventsRepositoryBeforeApplyingIt(t *testing.T) {
	// Not parallel: its two preparations hold only while no copy is delivered again inside consumerSpec's 200 ms window, a margin the load of tests beside it can eat.
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	var mu sync.Mutex
	var prepared []string
	var applied []DispatchIssue
	spec.PrepareRepository = func(_ context.Context, issue DispatchIssue) (DispatchIssue, error) {
		mu.Lock()
		defer mu.Unlock()
		prepared = append(prepared, issue.Key+" "+issue.ResolvedRepo)
		if len(prepared) == 1 {
			return issue, errors.New("GitHub is slow")
		}
		issue.ResolvedRepo = "acme/widgets"
		return issue, nil
	}
	record := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if issue, ok := fact.(DispatchIssue); ok {
			mu.Lock()
			applied = append(applied, issue)
			mu.Unlock()
		}
		return Result{}, nil
	})
	stop := startConsume(t, js, spec, pool, writeHandler("prepared", nil), record)
	defer stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	testwait.Eventually(t, "the event applied once its repository was prepared", func() bool { return writeCount(t, pool) == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"CAPTURE-3 sjawhar/legion", "CAPTURE-3 sjawhar/legion"}; !reflect.DeepEqual(prepared, want) {
		t.Fatalf("prepared %v, want %v (a failed preparation, then the retry)", prepared, want)
	}
	if len(applied) != 1 || applied[0].ResolvedRepo != "acme/widgets" {
		t.Fatalf("applied %#v, want the prepared fact", applied)
	}
}

// Envoy's goldens (packages/contracts/fixtures/github-envelopes, which its golden test writes from
// its webhook fixtures) are the subjects its listener publishes. For each, the consumer configured
// with the payload's repository filters on a prefix of the golden's topic and decodes the workflow
// fact the golden carries: the two sides spell a repository's subject segments alike, a dotted name
// included. The golden push, which is to main, decodes to the BasePush of that base, and the same
// push to an issue's branch decodes to its Push. A pull request event decodes to a fact only on an
// issue's branch, so the golden pull requests, on a branch of their own, decode to none, and the
// same event on an issue's branch decodes to its fact. The push golden's fact is compared whole
// with the golden's own payload, so a listener change that stops sending a key the daemon reads
// fails here.
func TestEnvoyGoldenSubjectsReachTheirRepositorysConsumer(t *testing.T) {
	t.Parallel()
	goldens, err := filepath.Glob(filepath.Join("..", "..", "..", "contracts", "fixtures", "github-envelopes", "*.json"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("Envoy golden envelopes: %v, %d found", err, len(goldens))
	}
	for _, path := range goldens {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var golden struct {
				Topic          string         `json:"topic"`
				PayloadSummary string         `json:"payload_summary"`
				Payload        map[string]any `json:"payload"`
			}
			if err := json.Unmarshal(data, &golden); err != nil {
				t.Fatalf("decode golden: %v", err)
			}
			repository := ghrepo.MustParse(golden.Payload["repo"].(string))
			filter := githubFilters([]ghrepo.Repository{repository})[0]
			if !strings.HasPrefix(golden.Topic, strings.TrimSuffix(filter, ">")) {
				t.Fatalf("%s's consumer filters %s, which does not match Envoy's %s", repository, filter, golden.Topic)
			}
			decode := func() decodedMessage {
				t.Helper()
				payload, err := json.Marshal(golden.Payload)
				if err != nil {
					t.Fatal(err)
				}
				envelope, err := json.Marshal(map[string]any{
					"event_id": "golden", "source": "github", "source_event_id": "golden", "topic": golden.Topic,
					"dedupe_key": "golden", "issued_at": 1, "payload_summary": golden.PayloadSummary,
					"payload": string(payload), "trace_id": "golden",
				})
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decodeMessage(golden.Topic, "GOLDEN", []ghrepo.Repository{repository}, envelope)
				if err != nil {
					t.Fatalf("decode %s: %v", golden.Topic, err)
				}
				return decoded
			}
			decoded := decode()
			kind := golden.Payload["kind"]
			issueBranch := claim.IssueBranch("GOLDEN-1")
			switch kind {
			case "push":
				if base, ok := decoded.Fact.(BasePush); !ok || base.Branch != "main" {
					t.Fatalf("%s's push to %v decoded %#v, want the base push of main", repository, golden.Payload["ref"], decoded.Fact)
				}
				golden.Payload["ref"] = "refs/heads/" + issueBranch
				decoded = decode()
				if push, ok := decoded.Fact.(Push); !ok || push.Branch != claim.IssueBranch("GOLDEN-1") {
					t.Fatalf("%s's push to an issue's branch decoded %#v, want its push", repository, decoded.Fact)
				}
			case "pr":
				if decoded.Fact != nil {
					t.Fatalf("%s's pull request on %v decoded %#v, want no fact for a branch that is no issue's", repository, golden.Payload["head_ref"], decoded.Fact)
				}
				golden.Payload["head_ref"] = issueBranch
				decoded = decode()
			case "sub_issue":
				if decoded.Fact != nil {
					t.Fatalf("%s's sub-issue event decoded %#v, want no fact: the workflow reads no sub-issue", repository, decoded.Fact)
				}
			}
			if (kind == "pr" || kind == "review" || kind == "push") && decoded.Fact == nil {
				t.Fatalf("%s's %s event decoded no fact", repository, kind)
			}
			if kind == "push" {
				// decodePush reads before, pusher and sender with `, _`, so a listener that stopped
				// sending one of them would leave the decode above green and lose the field. The
				// golden is the listener's own output, so comparing the whole decoded fact with the
				// golden's payload is the cross-package contract: a key the daemon reads may not go.
				field := func(key string) string {
					t.Helper()
					value, ok := golden.Payload[key].(string)
					if !ok {
						t.Fatalf("the push golden carries no string %q, which decodePush reads", key)
					}
					return value
				}
				want := Push{
					Repo:   repository.String(),
					Branch: issueBranch,
					Before: field("before"),
					After:  field("after"),
					Pusher: field("pusher"),
					Sender: field("sender"),
				}
				if got, ok := decoded.Fact.(Push); !ok || got != want {
					t.Fatalf("%s's push decoded %#v, want %#v from the golden's own payload", repository, decoded.Fact, want)
				}
			}
		})
	}
}

// A completed workflow run, as Envoy's own normalizer writes it from a real sjawhar/legion
// release.yaml push run on main (the workflow-run-completed golden), is no fact: no run of any
// workflow wakes a production check, so the GitHub durable that carries the envelope
// acknowledges it without a transaction, and it is never poison.
func TestAWorkflowRunEnvelopeIsNoFact(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "github-envelopes", "workflow-run-completed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Topic          string         `json:"topic"`
		PayloadSummary string         `json:"payload_summary"`
		Payload        map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	text, err := json.Marshal(golden.Payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"event_id": "golden", "source": "github", "source_event_id": "golden", "topic": golden.Topic,
		"dedupe_key": "golden", "issued_at": 1, "payload_summary": golden.PayloadSummary,
		"payload": string(text), "trace_id": "golden",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMessage(golden.Topic, "GOLDEN", capturedRepositories, envelope)
	if err != nil {
		t.Fatalf("decode %s: %v, want no poison", golden.Topic, err)
	}
	if decoded.Fact != nil {
		t.Fatalf("the golden's fact = %#v, want none", decoded.Fact)
	}
}

// A checks settlement names its pull request in its subject, where a dotted repository name is one
// segment, a dot as `_`: the settlement decodes for the repository its payload names.
func TestDecodeChecksForADottedRepository(t *testing.T) {
	t.Parallel()
	data := capturedGitHubEnvelope(t, "checks.json")
	data = bytes.ReplaceAll(data, []byte("sjawhar/legion"), []byte("sjawhar/legion.x"))
	data = bytes.ReplaceAll(data, []byte("sjawhar.legion."), []byte("sjawhar.legion_x."))
	decoded, err := decodeMessage("notifications.github.sjawhar.legion_x.pr.42.checks", "CAPTURE", []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion.x")}, data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if checks, ok := decoded.Fact.(PullRequestChecks); !ok || checks.Repo != "sjawhar/legion.x" || checks.Number != 42 {
		t.Fatalf("fact = %#v, want sjawhar/legion.x#42's checks", decoded.Fact)
	}
}

// The daemon reads nothing from a settlement's settled_at, so a settled_at that is not a millisecond
// count still decodes the settlement the captured one does, rather than losing its CI verdict.
func TestDecodeChecksIgnoresAMalformedSettledAt(t *testing.T) {
	t.Parallel()
	const subject = "notifications.github.sjawhar.legion.pr.42.checks"
	captured := capturedGitHubEnvelope(t, "checks.json")
	want, err := decodeMessage(subject, "CAPTURE", capturedRepositories, captured)
	if err != nil || want.Fact == nil {
		t.Fatalf("decode the captured settlement: fact %#v, err %v", want.Fact, err)
	}
	for _, settledAt := range []string{`\"yesterday\"`, `-5`} {
		data := bytes.Replace(captured, []byte(`\"settled_at\":1790124840596`), []byte(`\"settled_at\":`+settledAt), 1)
		if bytes.Equal(data, captured) {
			t.Fatal("the captured settlement carries no settled_at to replace")
		}
		got, err := decodeMessage(subject, "CAPTURE", capturedRepositories, data)
		if err != nil {
			t.Fatalf("settled_at %s: decode: %v", settledAt, err)
		}
		if !reflect.DeepEqual(got.Fact, want.Fact) {
			t.Fatalf("settled_at %s: fact = %#v, want %#v", settledAt, got.Fact, want.Fact)
		}
	}
}

// Envoy publishes a finished GitHub Actions job on the default branch, of a run with no pull
// request, on `notifications.github.<owner>.<repo>.job.<run_id>.completed`, with `kind: "job"`,
// inside the repository's filter. It changes no workflow record, so it decodes to no fact even for
// a configured repository, which consumeMessage acknowledges.
func TestAJobEnvelopeIsNoFact(t *testing.T) {
	t.Parallel()
	const subject = "notifications.github.sjawhar.legion.job.38230245571.completed"
	payload := `{"kind":"job","repo":"sjawhar/legion","run_id":"38230245571","job_id":"114758907253",` +
		`"name":"release","status":"completed","conclusion":"success","started_at":"2026-10-12T10:01:21Z",` +
		`"completed_at":"2026-10-12T10:02:02Z","head_sha":"f15b4562bb3b9ba82cf4c25a82ff0a312fc3e80b",` +
		`"url":"https://github.com/sjawhar/legion/actions/runs/38230245571/job/114758907253"}`
	if filter := githubFilters(capturedRepositories)[0]; !strings.HasPrefix(subject, strings.TrimSuffix(filter, ">")) {
		t.Fatalf("the repository's filter %s does not take the job subject %s, so this proves nothing", filter, subject)
	}
	decoded, err := decodeMessage(subject, "CAPTURE", capturedRepositories, envelopeJSON(t, "job-1", "github", payload))
	if err != nil {
		t.Fatalf("decode a job envelope: %v", err)
	}
	if decoded.Fact != nil || len(decoded.Unread) != 0 {
		t.Fatalf("decoded = %#v, want no fact and no unread field", decoded)
	}
}

// Dispatch's announcement that every server process serves a build (server.started) is no fact:
// no build is compared against a production check, so the daemon has no decoder for the subject
// and no durable that carries it (TestABootedDaemonOpensNoDispatchServerDurable). A message on it
// is refused as a subject no durable of this daemon reads.
func TestADispatchServerStartedMessageIsNoFact(t *testing.T) {
	t.Parallel()
	const build = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
	decoded, err := decodeMessage("notifications.dispatch.server.started", "CAPTURE", capturedRepositories,
		envelopeJSON(t, "dispatch-server-started-1", "dispatch", `{"commit":"`+build+`","started_at":"2026-10-19T06:00:00Z"}`))
	if decoded.Fact != nil {
		t.Fatalf("fact = %#v, want none", decoded.Fact)
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported durable subject") {
		t.Fatalf("decode = %v, want the subject refused as one no durable reads", err)
	}
}

// deployedEvent is Dispatch's issue.deployed event of issue as its delivery timeline appends it,
// with payload as the event's payload object.
func deployedEvent(issue, payload string) string {
	return `{"id":31,"issue_key":"` + issue + `","seq":7,"type":"issue.deployed","actor":{"kind":"system","id":"delivery-timeline"},"notify":true,"payload":` + payload + `}`
}

// Dispatch's issue.deployed is no fact: its delivery timeline's first sight of the production run
// that shipped a pull request woke the issue's production check once, and nothing reads it now.
// The event still arrives on the Dispatch durable, so it is decoded to no fact rather than
// refused, as every other event type the daemon does not know is.
func TestADispatchIssueDeployedEventIsNoFact(t *testing.T) {
	t.Parallel()
	const payload = `{"repo":"sjawhar/legion","number":1300,"run_id":38230245571,"completed_at":"2026-10-19T06:12:00Z"}`
	decoded, err := decodeMessage("notifications.dispatch.issue.CAPTURE-4.issue.deployed", "CAPTURE", capturedRepositories,
		envelopeJSON(t, "dispatch-31", "dispatch", deployedEvent("CAPTURE-4", payload)))
	if err != nil {
		t.Fatalf("decode issue.deployed: %v, want no poison", err)
	}
	if decoded.Fact != nil {
		t.Fatalf("fact = %#v, want none", decoded.Fact)
	}
}

// The daemon opens no durable on Dispatch's announcement that every server process serves a build
// (notifications.dispatch.server.started): no build wakes a production check any more, so the
// subject is nobody's. A boot opens the Dispatch, GitHub and listener-wake durables and no
// fourth; a durable an older daemon left on the stream is neither read nor deleted.
func TestABootedDaemonOpensNoDispatchServerDurable(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	want := []string{dispatchConsumerName(spec.Project), githubConsumerName(spec.Project), wakeConsumerName(spec.Project)}
	if got := lagNames(consumers.Lag(context.Background())); !slices.Equal(got, want) {
		t.Fatalf("the boot's consumers = %q, want %q", got, want)
	}
	var opened []string
	names := stream.ConsumerNames(context.Background())
	for name := range names.Name() {
		opened = append(opened, name)
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the stream's durables: %v", err)
	}
	slices.Sort(opened)
	slices.Sort(want)
	if !slices.Equal(opened, want) {
		t.Fatalf("the stream's durables = %q, want %q: none carries notifications.dispatch.server.started", opened, want)
	}
}

// A message that does not decode is given up at its first delivery: one give-up line at ERROR
// with the reason undecodable and one given-up record each, and it is terminated, so neither is
// delivered again.
func TestConsumeTermsPoisonMessagesOnce(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	stop := startConsume(t, js, spec, pool, writeHandler("applied", nil))
	defer stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-208.issue.updated", []byte(`{`))
	publish(t, js, "notifications.dispatch.issue.CAPTURE-208.issue.updated", envelopeJSON(t, "dispatch-poison", "dispatch", `{"id":1,"issue_key":"CAPTURE-208","seq":1,"notify":true,"type":"issue.updated","payload":{"key":"CAPTURE-999","status":"todo","title":"wrong key"}}`))
	const giveUp = "intake: a message was given up"
	testwait.Eventually(t, "two give-up lines", func() bool { return strings.Count(logs.String(), giveUp) == 2 })
	time.Sleep(3 * spec.AckWait)
	if got := strings.Count(logs.String(), giveUp); got != 2 {
		t.Fatalf("give-up lines after ack wait = %d, want 2", got)
	}
	if got := strings.Count(logs.String(), "reason=undecodable"); got != 2 {
		t.Fatalf("%d give-up lines name the reason undecodable, want both:\n%s", got, logs)
	}
	records := intakeRecords(t, pool)
	if len(records) != 2 || !records[0].GivenUp() || !records[1].GivenUp() ||
		records[0].GiveUpReason != record.GiveUpUndecodable || records[1].GiveUpReason != record.GiveUpUndecodable {
		t.Fatalf("intake_failures = %+v, want one record of each message given up as undecodable", records)
	}
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

// A GitHub envelope with no payload, as an Envoy listener published every commit status on its
// repository's comment topic (testdata/github/status-no-payload.json is such a message's stored
// bytes, with this test's repository and ids of its own), is given up once, its give-up line at
// ERROR, and terminated, and the GitHub durable goes on: the push stored after it is applied, and
// nothing waits for an acknowledgement.
func TestConsumeTermsAGitHubEnvelopeWithNoPayloadAndAppliesTheNext(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	stop := startConsume(t, js, spec, pool, writeHandler("push", nil))
	defer stop()

	poison := publish(t, js, "notifications.github.sjawhar.legion.comment", capturedGitHubEnvelope(t, "status-no-payload.json"))
	last := publish(t, js, "notifications.github.sjawhar.legion.push.branch.main", capturedGitHubEnvelope(t, "push-base.json"))
	assertAcknowledgedThrough(t, stream, githubConsumerName(spec.Project), last)
	if got := writeHandlers(t, pool); !slices.Equal(got, []string{"push"}) {
		t.Fatalf("handler writes = %q, want the push after the poison applied", got)
	}
	requireLogLine(t, logs, "level=ERROR", "intake: a message was given up", "reason=undecodable", "Envoy envelope payload is not JSON",
		"subject=notifications.github.sjawhar.legion.comment", "stream_seq="+strconv.FormatUint(poison.Sequence, 10))
	time.Sleep(3 * spec.AckWait)
	if got := strings.Count(logs.String(), "intake: a message was given up"); got != 1 {
		t.Fatalf("give-up lines after ack wait = %d, want 1: a terminated message is not delivered again", got)
	}
	assertNoAckPending(t, stream, githubConsumerName(spec.Project))
}

// One NATS stream carries every Dispatch project's issue events. A daemon acts only on its own
// project's: another project's event is acknowledged and never reaches a handler, as the shipped
// daemon drops it (events.ts: a subject key outside the configured projects returns).
func TestConsumeAcknowledgesAnotherProjectsDispatchEventWithoutApplyingIt(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	spec.Project = "LEGION"
	stop := startConsume(t, js, spec, pool, writeHandler("foreign", nil))
	defer stop()

	sent := publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	assertAcknowledgedThrough(t, stream, dispatchConsumerName(spec.Project), sent)
	if got := writeCount(t, pool); got != 0 {
		t.Fatalf("handler writes for another project's event = %d, want 0", got)
	}
}

// A subject inside a repository's filter does not prove the event is that repository's: a
// repository whose name holds a dot (`sjawhar/legion.x`) publishes under more segments than an owner
// and a name, which `sjawhar/legion`'s filter matches. The payload's repository decides, as the
// shipped daemon's does (reducers.ts registerPrFenced: a pull request whose repository is not its
// issue's is not registered), so another repository's pull request on a `legion/<KEY>` branch is
// acknowledged and never reaches a handler.
func TestConsumeAcknowledgesAnotherRepositorysGitHubEventWithoutApplyingIt(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool, writeHandler("foreign", nil))
	defer stop()

	foreign := bytes.ReplaceAll(capturedGitHubEnvelope(t, "pr-opened.json"), []byte("sjawhar/legion"), []byte("sjawhar/legion.x"))
	sent := publish(t, js, "notifications.github.sjawhar.legion.x.pr.42", foreign)
	assertAcknowledgedThrough(t, stream, githubConsumerName(spec.Project), sent)
	if got := writeCount(t, pool); got != 0 {
		t.Fatalf("handler writes for another repository's pull request = %d, want 0", got)
	}
}

// A push to an issue's branch, legion/<KEY>, is the Push the workflow applies to the pull request it
// records there; a push to any other branch is the BasePush that says a pull request's base may have
// moved. Both are applied once and marked once. A push to anything that is not a branch - a tag -
// is no fact of either kind, and is acknowledged untouched, with no transaction (no
// processed_events mark, no handler).
func TestConsumeAppliesAPushToAnIssuesBranchAndToABase(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool, writeHandler("push", nil))
	defer stop()

	captured := capturedGitHubEnvelope(t, "push.json")
	tagged := bytes.ReplaceAll(captured, []byte("refs/heads/legion/LEGION-208"), []byte("refs/tags/v1.0.0"))
	tagged = bytes.ReplaceAll(tagged, []byte(`"task315-push"`), []byte(`"tag-push"`))
	publish(t, js, "notifications.github.sjawhar.legion.push.tag.v1.0.0", tagged)
	base := capturedGitHubEnvelope(t, "push-base.json")
	publish(t, js, "notifications.github.sjawhar.legion.push.branch.main", base)
	last := publish(t, js, "notifications.github.sjawhar.legion.push.branch.legion/LEGION-208", captured)
	assertAcknowledgedThrough(t, stream, githubConsumerName(spec.Project), last)
	if got := writeHandlers(t, pool); !slices.Equal(got, []string{"push", "push"}) {
		t.Fatalf("handler writes = %q, want one for the push to the base and one for the push to the issue's branch", got)
	}
	rows, err := pool.Query(context.Background(), `select event_id from processed_events where source = 'github' order by event_id`)
	if err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	marks, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read processed_events: %v", err)
	}
	if !slices.Equal(marks, []string{"github.task315-push", "github.task714-push-base"}) {
		t.Fatalf("processed_events = %q, want the push to the issue's branch and the push to the base marked, and the tag push unmarked", marks)
	}
}

// A push to a branch that is legion/ followed by an issue key (claim.BranchIssue) decodes to the
// Push the workflow applies to the pull request it records there; a push to a branch of any other
// shape decodes to the BasePush that says a pull request's base may have moved, since that is the
// shape of a branch pull requests sit on. A push to a ref that is no branch at all decodes to
// neither. GitHub's deletion of an issue's branch, a push whose after is record.NoCommit, names no
// head and decodes to no fact; the deletion of a base is still the BasePush that says the base
// moved.
func TestDecodeAPushIsAPushOnAnIssuesBranchAndABasePushOnAnyOther(t *testing.T) {
	t.Parallel()
	captured := capturedGitHubEnvelope(t, "push.json")
	for _, tc := range []struct {
		ref string
		// after replaces the push's after, head-captured, when it is set.
		after string
		// branch is the issue branch the push decodes to a Push on, base the branch it decodes to a
		// BasePush on; both empty is a ref that decodes to no fact.
		branch string
		base   string
	}{
		{ref: "refs/heads/legion/LEGION-208", branch: "legion/LEGION-208"},
		{ref: "refs/heads/legion/ACME-19", branch: "legion/ACME-19"},
		{ref: "refs/heads/main", base: "main"},
		{ref: "refs/heads/legion/main", base: "legion/main"},
		{ref: "refs/heads/legion/legion-208", base: "legion/legion-208"},
		{ref: "refs/heads/legion/LEGION-208/x", base: "legion/LEGION-208/x"},
		{ref: "refs/heads/bot/LEGION-208", base: "bot/LEGION-208"},
		{ref: "refs/tags/legion/LEGION-208"},
		{ref: "refs/heads/"},
		{ref: "refs/heads/legion/LEGION-208", after: record.NoCommit},
		{ref: "refs/heads/main", after: record.NoCommit, base: "main"},
	} {
		t.Run(strings.TrimSpace(tc.ref+" "+tc.after), func(t *testing.T) {
			data := bytes.ReplaceAll(captured, []byte("refs/heads/legion/LEGION-208"), []byte(tc.ref))
			after := "head-captured"
			if tc.after != "" {
				after = tc.after
				replaced := bytes.ReplaceAll(data, []byte(`\"after\":\"head-captured\"`), []byte(`\"after\":\"`+after+`\"`))
				if bytes.Equal(replaced, data) {
					t.Fatal("the captured push carries no after to replace")
				}
				data = replaced
			}
			decoded, err := decodeMessage("notifications.github.sjawhar.legion.push.branch.x", "CAPTURE", capturedRepositories, data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			push, isPush := decoded.Fact.(Push)
			basePush, isBase := decoded.Fact.(BasePush)
			switch {
			case tc.branch == "" && tc.base == "" && decoded.Fact != nil:
				t.Fatalf("fact = %#v, want none for a ref that is no branch or a deletion of an issue's branch", decoded.Fact)
			case tc.branch != "" && (!isPush || push.Branch != tc.branch):
				t.Fatalf("fact = %#v, want a push to %s", decoded.Fact, tc.branch)
			case tc.base != "" && (!isBase || basePush.Branch != tc.base || basePush.After != after):
				t.Fatalf("fact = %#v, want a base push to %s at %s", decoded.Fact, tc.base, after)
			}
		})
	}
}

func TestConsumeDeduplicatesOneEventAcrossDeliveries(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool, writeHandler("applied", nil))
	defer stop()

	message := capturedIssueUpdatedEnvelope(t)
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", message)
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", message)
	testwait.Eventually(t, "one deduplicated write", func() bool { return writeCount(t, pool) == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

func TestConsumeNaksRollbackAndAppliesRedeliveryOnce(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	var calls atomic.Int32
	handler := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if _, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ('retry')`); err != nil {
			return Result{}, err
		}
		if calls.Add(1) == 1 {
			return Result{}, errors.New("transient handler failure")
		}
		return Result{}, nil
	})
	stop := startConsume(t, js, spec, pool, handler)
	defer stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	testwait.Eventually(t, "redelivery committed once", func() bool { return calls.Load() >= 2 && writeCount(t, pool) == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

func TestConsumeRestartResumesAfterAcknowledgedMessage(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, _ := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool, writeHandler("restart", nil))

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	testwait.Eventually(t, "first committed message", func() bool { return writeCount(t, pool) == 1 })
	stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	stop = startConsume(t, js, spec, pool, writeHandler("restart", nil))
	defer stop()
	testwait.Eventually(t, "durable consumer resumes after ack", func() bool { return writeCount(t, pool) == 2 })
}

// A daemon's first boot on a NATS server already holding history — production's stream keeps
// 72 hours of ENVOY_NOTIFICATIONS — must not replay it into admission: its consumers, created now,
// start at the next message. Boot reads its Dispatch listing after creating them, and the listing
// covers the state before them.
func TestAFreshConsumerStartsAtTheNextMessage(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.github.sjawhar.legion.pr.42", capturedGitHubEnvelope(t, "pr-opened.json"))

	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	for _, name := range []string{dispatchConsumerName(spec.Project), githubConsumerName(spec.Project)} {
		if pending := consumerInfo(t, stream, name).NumPending; pending != 0 {
			t.Errorf("%s was created with %d messages of history pending, want 0", name, pending)
		}
	}
	stop := runConsumers(t, consumers, writeHandler("fresh", nil))
	defer stop()
	publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	testwait.Eventually(t, "the next message committed", func() bool { return writeCount(t, pool) >= 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
	if got := writeCount(t, pool); got != 1 {
		t.Errorf("%d facts committed, want only the message published after the consumers were created", got)
	}
}

// A consumer that already exists keeps its position, whatever policy created it: an earlier daemon's
// consumer (created delivering all) still owes its unacknowledged backlog, and a boot that updates
// it still applies the configuration that may change, the repositories it filters on.
func TestAnExistingConsumerKeepsItsPosition(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.github.sjawhar.legion.pr.42", capturedGitHubEnvelope(t, "pr-opened.json"))
	for _, config := range []jetstream.ConsumerConfig{
		{Durable: dispatchConsumerName(spec.Project), FilterSubject: "notifications.dispatch.issue.>", AckPolicy: jetstream.AckExplicitPolicy, AckWait: spec.AckWait},
		{Durable: githubConsumerName(spec.Project), FilterSubjects: githubFilters(spec.Repositories), AckPolicy: jetstream.AckExplicitPolicy, AckWait: spec.AckWait},
	} {
		if _, err := stream.CreateConsumer(context.Background(), config); err != nil {
			t.Fatalf("create the earlier daemon's consumer %s: %v", config.Durable, err)
		}
	}

	spec.Repositories = []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion"), ghrepo.MustParse("acme/widgets")}
	if _, err := OpenConsumers(context.Background(), js, pool, spec); err != nil {
		t.Fatalf("OpenConsumers over existing consumers: %v", err)
	}
	for _, name := range []string{dispatchConsumerName(spec.Project), githubConsumerName(spec.Project)} {
		if pending := consumerInfo(t, stream, name).NumPending; pending != 1 {
			t.Errorf("%s has %d messages pending after the boot, want its backlog of 1", name, pending)
		}
	}
	if got, want := consumerInfo(t, stream, githubConsumerName(spec.Project)).Config.FilterSubjects, githubFilters(spec.Repositories); !slices.Equal(got, want) {
		t.Errorf("GitHub consumer filters = %v, want the boot's %v", got, want)
	}

	stop := startConsume(t, js, spec, pool, writeHandler("existing", nil))
	defer stop()
	testwait.Eventually(t, "every backlog delivered and acknowledged", func() bool {
		for _, name := range []string{dispatchConsumerName(spec.Project), githubConsumerName(spec.Project)} {
			if info := consumerInfo(t, stream, name); info.NumPending != 0 || info.NumAckPending != 0 || info.Delivered.Consumer == 0 {
				return false
			}
		}
		return true
	})
	if writeCount(t, pool) == 0 {
		t.Error("the Dispatch backlog was acknowledged without being applied")
	}
}

// consumerInfo reads the named consumer's info. A failed read fails the row, so the read waits
// for the server until the row's hang guards end, the test binary's deadline less 30 s
// (testwait.Remaining), in place of the five seconds nats.go gives a request whose context has no
// deadline (its defaultAPITimeout), which would let a NATS server the machine holds decide it.
func consumerInfo(t *testing.T, stream jetstream.Stream, name string) *jetstream.ConsumerInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t))
	defer cancel()
	consumer, err := stream.Consumer(ctx, name)
	if err != nil {
		t.Fatalf("look up consumer %s: %v", name, err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer %s info: %v", name, err)
	}
	return info
}

func capturedGitHubEnvelope(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatalf("read captured GitHub envelope %s: %v", name, err)
	}
	return data
}

func TestConsumeCommitsRefusalAndAcknowledges(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	stop := startConsume(t, js, spec, pool, writeHandler("refusal", &Refusal{Status: 409, Code: "DESIGN_GATE_CLOSED", Message: "approve version 3"}))
	defer stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	testwait.Eventually(t, "refusal committed", func() bool { return writeCount(t, pool) == 1 && strings.Count(logs.String(), "committed refusal") == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

// Every carrier of a ghrepo.Repository refuses the zero value, and intake is one: a zero
// repository would subscribe to `notifications.github...>`, a subject no repository publishes on,
// and intake would wait on it silently. The spec is refused by name before any consumer opens.
func TestOpenConsumersRefusesAZeroRepository(t *testing.T) {
	t.Parallel()
	spec := consumerSpec(&lockedBuffer{})
	spec.Repositories = append(spec.Repositories, ghrepo.Repository{})
	if _, err := normalizedSpec(spec); err == nil || err.Error() != "intake consumer repository is required" {
		t.Fatalf("normalizedSpec with a zero repository = %v, want \"intake consumer repository is required\"", err)
	}
}

// capturedRepositories is the repository every captured GitHub envelope names.
var capturedRepositories = []ghrepo.Repository{ghrepo.MustParse("sjawhar/legion")}

func consumerSpec(logs *lockedBuffer) ConsumerSpec {
	return ConsumerSpec{
		Project:      "CAPTURE",
		Repositories: capturedRepositories,
		AckWait:      200 * time.Millisecond,
		NakDelay:     25 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	}
}

func startConsume(t *testing.T, js jetstream.JetStream, spec ConsumerSpec, pool *pgxpool.Pool, handlers ...Handler) func() {
	t.Helper()
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	return runConsumers(t, consumers, handlers...)
}

func runConsumers(t *testing.T, consumers *Consumers, handlers ...Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumers.Run(ctx, handlers...) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
}

func testJetStream(t *testing.T) (jetstream.JetStream, jetstream.Stream) {
	t.Helper()
	js := testnats.JetStream(t)
	return js, testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
}

// publish publishes data on subject and returns its acknowledgement. The publish waits for the
// server's answer until the row's hang guards end, the test binary's deadline less 30 s
// (testwait.Remaining), in place of the five seconds nats.go gives a request whose context has no
// deadline (its defaultAPITimeout), so a NATS server the machine holds is waited out.
func publish(t *testing.T, js jetstream.JetStream, subject string, data []byte) *jetstream.PubAck {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t))
	defer cancel()
	ack, err := js.Publish(ctx, subject, data)
	if err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
	return ack
}

// assertAcknowledgedThrough waits for consumer to have acknowledged every message of its filter up
// to the one last names, with none left to deliver. It reads the stream position, not a count of
// deliveries, because one message can be delivered more than once (the delivery mechanism:
// ConsumerSpec.HandlerLimit's comment in consume.go). That is at-least-once delivery the consumer
// deduplicates and which a delivery count would read as more. Its reads wait for the server until
// the row's hang guards end (testwait.Remaining), as assertNoAckPending's do.
func assertAcknowledgedThrough(t *testing.T, stream jetstream.Stream, consumer string, last *jetstream.PubAck) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t))
	defer cancel()
	testwait.Eventually(t, fmt.Sprintf("%s has acknowledged through stream sequence %d", consumer, last.Sequence), func() bool {
		item, err := stream.Consumer(ctx, consumer)
		if err != nil {
			return false
		}
		info, err := item.Info(ctx)
		return err == nil && info.AckFloor.Stream == last.Sequence && info.NumPending == 0 && info.NumAckPending == 0
	})
}

// assertNoAckPending waits for consumer to owe no acknowledgement. Its reads wait for the server
// until the row's hang guards end (testwait.Remaining), in place of the five seconds nats.go gives
// a request whose context has no deadline: a read that fails only makes the next poll read again,
// but each held read spends five seconds of the wait's own bound and the last one it makes decides
// the row.
func assertNoAckPending(t *testing.T, stream jetstream.Stream, consumer string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t))
	defer cancel()
	testwait.Eventually(t, consumer+" has no acknowledgement pending", func() bool {
		item, err := stream.Consumer(ctx, consumer)
		if err != nil {
			return false
		}
		info, err := item.Info(ctx)
		return err == nil && info.NumAckPending == 0
	})
}

func capturedIssueUpdatedEnvelope(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read captured issue.updated envelope: %v", err)
	}
	return data
}

func capturedIssueCreatedEnvelope(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/dispatch/issue-created.json")
	if err != nil {
		t.Fatalf("read captured issue.created envelope: %v", err)
	}
	return data
}

func envelopeJSON(t *testing.T, eventID, source, payload string) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"event_id":        eventID,
		"source":          source,
		"source_event_id": eventID + "-source",
		"topic":           "test",
		"dedupe_key":      eventID,
		"issued_at":       1,
		"payload_summary": "test event",
		"payload":         payload,
		"trace_id":        eventID + "-trace",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

func writeHandler(name string, refusal *Refusal) Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, _ Fact) (Result, error) {
		if _, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, name); err != nil {
			return Result{}, err
		}
		return Result{Refusal: refusal}, nil
	})
}

func createWrites(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `create table intake_test_writes (id bigserial primary key, handler text not null)`); err != nil {
		t.Fatalf("create write table: %v", err)
	}
}

func writeCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from intake_test_writes`).Scan(&count); err != nil {
		t.Fatalf("count writes: %v", err)
	}
	return count
}

func writeHandlers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select handler from intake_test_writes order by id`)
	if err != nil {
		t.Fatalf("list handler writes: %v", err)
	}
	defer rows.Close()
	var handlers []string
	for rows.Next() {
		var handler string
		if err := rows.Scan(&handler); err != nil {
			t.Fatalf("scan handler write: %v", err)
		}
		handlers = append(handlers, handler)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate handler writes: %v", err)
	}
	return handlers
}

// migratedPool is a pool on a database carrying every migration, cloned from the package's
// template (testpg.ClonePool).
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testpg.ClonePool(t, "legion_intake_test")
}

type lockedBuffer = testlog.Buffer

// Facts apply one at a time: the handlers read and rewrite whole records (an issue, the admission
// slots that span trees), so a second fact running beside the first would act on what the first is
// about to change and overwrite it. The second fact waits for the first to commit.
func TestApplyFactSerializesConcurrentFacts(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `create table intake_test_counter (n integer not null); insert into intake_test_counter values (0)`); err != nil {
		t.Fatalf("create counter: %v", err)
	}
	increment := func(read, release chan struct{}) Handler {
		return handlerFunc(func(ctx context.Context, tx pgx.Tx, _ Fact) (Result, error) {
			var n int
			if err := tx.QueryRow(ctx, `select n from intake_test_counter`).Scan(&n); err != nil {
				return Result{}, err
			}
			if read != nil {
				close(read)
				<-release
			}
			_, err := tx.Exec(ctx, `update intake_test_counter set n = $1`, n+1)
			return Result{}, err
		})
	}
	read, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letFirstFinish := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing wait must still let the first fact finish, or its open transaction holds the pool.
	defer letFirstFinish()
	first := make(chan error, 1)
	go func() {
		_, err := ApplyFact(ctx, pool, "test", "first", DispatchIssue{Key: "LEGION-208"}, increment(read, release))
		first <- err
	}()
	<-read
	second := make(chan error, 1)
	go func() {
		_, err := ApplyFact(ctx, pool, "test", "second", DispatchIssue{Key: "LEGION-209"}, increment(nil, nil))
		second <- err
	}()
	testwait.Eventually(t, "the second fact to wait for the first", func() bool {
		var waiting int
		if err := pool.QueryRow(ctx, `select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting == 1
	})
	letFirstFinish()
	if err := <-first; err != nil {
		t.Fatalf("first ApplyFact: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second ApplyFact: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `select n from intake_test_counter`).Scan(&n); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if n != 2 {
		t.Fatalf("counter = %d, want both facts applied in turn", n)
	}
}

// A fact takes its pool's turn before it takes a connection (store.Pool.FactTurn), so a burst of
// facts waiting on the fact lock holds one connection, not one each: with one fact inside its
// handler and twice the pool's size of facts waiting behind it, a read of the pool still answers
// at once. Before the turn every waiting fact held a connection, the burst held them all, and the
// read waited for the whole burst (the boot replay's read of which facts are applied, which ran out
// its budget so in 1 of 20 race-soak runs of internal/daemon).
func TestABurstOfFactsWaitingOnTheLockLeavesThePoolToOtherReads(t *testing.T) {
	t.Parallel()
	raw := migratedPool(t)
	pool := store.NewPool(raw)
	ctx := context.Background()
	inside, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letFirstFinish := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing read must still let the first fact finish, or its open transaction holds the pool.
	defer letFirstFinish()
	var applied sync.WaitGroup
	errs := make(chan error, 1+2*int(raw.Config().MaxConns))
	applied.Go(func() {
		_, err := ApplyFact(ctx, pool, "test", "burst-0", DispatchConsumerPosition{AckFloorStream: 0}, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
			close(inside)
			<-release
			return Result{}, nil
		}))
		errs <- err
	})
	<-inside
	var asked sync.WaitGroup
	for i := range 2 * int(raw.Config().MaxConns) {
		asked.Add(1)
		applied.Go(func() {
			asked.Done()
			_, err := ApplyFact(ctx, pool, "test", fmt.Sprintf("burst-%d", i+1), DispatchConsumerPosition{AckFloorStream: int64(i + 1)})
			errs <- err
		})
	}
	asked.Wait()
	// Without the turn the burst takes every connection of the pool, each waiting on the lock; with
	// it, only the fact inside its handler holds one. The read goes once the pool is full or a second
	// has passed, whichever is first, so it never runs ahead of a burst that would starve it.
	testwait.Holds(time.Second, testwait.Poll, func() bool { return raw.Stat().AcquiredConns() == raw.Config().MaxConns })
	read, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(read, `select 1`).Scan(&one); err != nil {
		t.Fatalf("a read of the pool beside %d facts waiting on the fact lock = %v, want it answered at once", 2*raw.Config().MaxConns, err)
	}
	letFirstFinish()
	applied.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a fact of the burst: %v", err)
		}
	}
}

// A fact whose ctx ends while it waits for its pool's turn leaves the line holding nothing: it
// takes no connection, runs no handler and records nothing, and answers ctx's error naming the
// wait. The line moves on: once the turn is free, the same fact applies.
func TestAFactWhoseContextEndsWhileItWaitsForItsTurnTakesNoConnection(t *testing.T) {
	t.Parallel()
	raw := migratedPool(t)
	pool := store.NewPool(raw)
	ctx := context.Background()
	held, err := pool.FactTurn(ctx)
	if err != nil {
		t.Fatalf("take the pool's turn: %v", err)
	}
	ran := false
	handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		ran = true
		return Result{}, nil
	})
	acquired := raw.Stat().AcquireCount()
	waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err = ApplyFact(waiting, pool, "test", "turn-ends", DispatchConsumerPosition{AckFloorStream: 1}, handler)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "wait for the pool's turn to apply fact test/turn-ends") {
		t.Fatalf("ApplyFact while another holds the turn = %v, want the wait for the turn ended by its context", err)
	}
	if got := raw.Stat().AcquireCount(); got != acquired || ran {
		t.Fatalf("the fact that left the line took %d connections and ran its handler %t, want none and false", got-acquired, ran)
	}
	held()
	if _, err := ApplyFact(ctx, pool, "test", "turn-ends", DispatchConsumerPosition{AckFloorStream: 1}, handler); err != nil || !ran {
		t.Fatalf("ApplyFact once the turn is free = %v, handler ran %t; want it applied", err, ran)
	}
}

// A fact hands its pool's turn on at its COMMIT, before its other commit hooks run: a hook that
// waits (a NATS publish, say) holds up no other fact. Here the first fact's hook waits until a
// second fact has applied; a turn held through the hooks would leave the second waiting on the
// first's hook, and its bound would end it.
func TestAFactsCommitHookHoldsUpNoOtherFact(t *testing.T) {
	t.Parallel()
	pool := store.NewPool(migratedPool(t))
	ctx := context.Background()
	second := make(chan error, 1)
	hooked := handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		store.OnCommit(ctx, func() {
			bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := ApplyFact(bounded, pool, "test", "after-the-commit", DispatchConsumerPosition{AckFloorStream: 2})
			second <- err
		})
		return Result{}, nil
	})
	if _, err := ApplyFact(ctx, pool, "test", "hooked", DispatchConsumerPosition{AckFloorStream: 1}, hooked); err != nil {
		t.Fatalf("the hooked fact: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("a fact applied from the first fact's commit hook = %v, want it applied: the turn was handed on at the first's COMMIT", err)
	}
}

// A reopened pull request is open again, recorded as when it opened: a closed pull request's record
// is dropped at the next re-admission, so one reopened in between must not stay closed. The fact
// says it is a reopen, since GitHub sends opened only once and a second one is a redelivery.
func TestDecodeReopenedPullRequestAsOpened(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/github/pr-opened.json")
	if err != nil {
		t.Fatalf("read captured opened envelope: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode captured envelope: %v", err)
	}
	payload := envelope["payload"].(string)
	if !strings.Contains(payload, `"action":"opened"`) {
		t.Fatalf("captured payload %s has no opened action", payload)
	}
	envelope["payload"] = strings.Replace(payload, `"action":"opened"`, `"action":"reopened"`, 1)
	reopened, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMessage("notifications.github.sjawhar.legion.pr.42", "CAPTURE", capturedRepositories, reopened)
	if err != nil {
		t.Fatalf("decode reopened: %v", err)
	}
	if opened, ok := got.Fact.(PullRequestOpened); !ok || opened.Number != 42 || opened.Branch != "legion/LEGION-208" || opened.HeadSHA != "head-captured" || !opened.Reopened {
		t.Fatalf("reopened fact = %#v, want the pull request opened again", got.Fact)
	}
}

// A Dispatch event names who wrote it. A session actor's id is decoded, so the workflow can tell a
// write by an agent holding a claim, or by an outside session, from a person's move. A user actor
// decodes none, and neither does the daemon's own session, legion-daemon:<PROJECT>: both are the
// writes the workflow acts on.
func TestDecodeDispatchIssueNamesASessionActor(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read captured issue.updated envelope: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode captured envelope: %v", err)
	}
	payload := envelope["payload"].(string)
	if !strings.Contains(payload, `"actor":{"kind":"user","id":"smoke"}`) {
		t.Fatalf("captured payload %s has no user actor", payload)
	}
	envelope["payload"] = strings.Replace(payload, `"actor":{"kind":"user","id":"smoke"}`, `"actor":{"kind":"session","id":"ses-impl"}`, 1)
	byAgent, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, byAgent)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if issue, ok := got.Fact.(DispatchIssue); !ok || issue.ActorSession != "ses-impl" {
		t.Fatalf("fact = %#v, want the session actor ses-impl", got.Fact)
	}
	human, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, data)
	if err != nil {
		t.Fatalf("decode the captured event: %v", err)
	}
	if issue, ok := human.Fact.(DispatchIssue); !ok || issue.ActorSession != "" {
		t.Fatalf("fact = %#v, want no session actor for a user", human.Fact)
	}
	envelope["payload"] = strings.Replace(payload, `"actor":{"kind":"user","id":"smoke"}`, `"actor":{"kind":"session","id":"legion-daemon:CAPTURE"}`, 1)
	byDaemon, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", "CAPTURE", capturedRepositories, byDaemon)
	if err != nil {
		t.Fatalf("decode the daemon's own write: %v", err)
	}
	if issue, ok := daemon.Fact.(DispatchIssue); !ok || issue.ActorSession != "" || !issue.DaemonActor {
		t.Fatalf("fact = %#v, want the daemon's own actor", daemon.Fact)
	}
}

// GitHub's review id and the review's submission time reach the facts: the workflow orders reviews
// by submission time then id, so each must survive decoding exactly. A review id that is not a
// positive integer is refused rather than read as none. A time that cannot be read is taken as none
// and reported, since a review without one is still ordered, by its id.
func TestDecodingCarriesTheReviewOrder(t *testing.T) {
	t.Parallel()
	withPayload := func(t *testing.T, name, subject string, set map[string]any) (decodedMessage, error) {
		t.Helper()
		var envelope map[string]any
		if err := json.Unmarshal(capturedGitHubEnvelope(t, name), &envelope); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// The envelope carries its payload as JSON text.
		var payload map[string]any
		if err := json.Unmarshal([]byte(envelope["payload"].(string)), &payload); err != nil {
			t.Fatalf("parse %s's payload: %v", name, err)
		}
		for key, value := range set {
			payload[key] = value
		}
		text, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode %s's payload: %v", name, err)
		}
		envelope["payload"] = string(text)
		data, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		return decodeMessage(subject, "CAPTURE", capturedRepositories, data)
	}
	reviewSubject := "notifications.github.sjawhar.legion.pr.42.review"

	decoded, err := withPayload(t, "review.json", reviewSubject, map[string]any{"review_id": "5325101010",
		"submitted_at": "2026-09-26T12:03:00+02:00"})
	submitted := time.Date(2026, 9, 26, 10, 3, 0, 0, time.UTC)
	if review, ok := decoded.Fact.(PullRequestReview); err != nil || !ok || review.ID != 5325101010 ||
		!review.SubmittedAt.Equal(submitted) || len(decoded.Unread) != 0 {
		t.Fatalf("review with an id and a submission time = %#v, unread %q, %v", decoded.Fact, decoded.Unread, err)
	}
	if _, err := withPayload(t, "review.json", reviewSubject, map[string]any{"review_id": "not-a-number"}); err == nil {
		t.Fatal("a review id that is not a number decoded")
	}
	for _, unreadable := range []any{"yesterday", 1.72735218e+09, true, map[string]any{"t": "2026-09-26T12:03:00Z"}} {
		decoded, err = withPayload(t, "review.json", reviewSubject, map[string]any{"review_id": "5325101010", "submitted_at": unreadable})
		if review, ok := decoded.Fact.(PullRequestReview); err != nil || !ok || review.ID != 5325101010 || !review.SubmittedAt.IsZero() ||
			len(decoded.Unread) != 1 || !strings.Contains(decoded.Unread[0], "submitted_at") {
			t.Fatalf("review with the time %#v = %#v, unread %q, %v; want it untimed and the field reported", unreadable, decoded.Fact, decoded.Unread, err)
		}
	}
	for _, absent := range []any{nil, ""} {
		decoded, err = withPayload(t, "review.json", reviewSubject, map[string]any{"review_id": "5325101010", "submitted_at": absent})
		if review, ok := decoded.Fact.(PullRequestReview); err != nil || !ok || !review.SubmittedAt.IsZero() || len(decoded.Unread) != 0 {
			t.Fatalf("review with the time %#v = %#v, unread %q, %v; want it untimed and nothing reported", absent, decoded.Fact, decoded.Unread, err)
		}
	}
	// A capped review's body is marked (`body_truncated: "true"`), so a truncated review-App review
	// can be told apart from one whose full body simply carries no Legion footer.
	decoded, err = withPayload(t, "review.json", reviewSubject, map[string]any{"body_truncated": "true"})
	if review, ok := decoded.Fact.(PullRequestReview); err != nil || !ok || !review.BodyTruncated {
		t.Fatalf("review with body_truncated true = %#v, %v; want BodyTruncated set", decoded.Fact, err)
	}
	decoded, err = withPayload(t, "review.json", reviewSubject, map[string]any{"body_truncated": "false"})
	if review, ok := decoded.Fact.(PullRequestReview); err != nil || !ok || review.BodyTruncated {
		t.Fatalf("review with body_truncated false = %#v, %v; want BodyTruncated clear", decoded.Fact, err)
	}
	decoded, err = withPayload(t, "review.json", reviewSubject, nil)
	if review, ok := decoded.Fact.(PullRequestReview); err != nil || !ok || review.BodyTruncated {
		t.Fatalf("review with no body_truncated field = %#v, %v; want BodyTruncated clear", decoded.Fact, err)
	}
}

// The workflow decides a review round inside a transaction and performs no I/O, so whether the
// review's author may write to the repository is read before the fact is applied, and only for a
// review that decides a round and names its author: a comment costs no GitHub call, and neither
// does a review the producer gave no author. A request for changes in GitHub's upper case is
// looked up as the workflow counts it (PullRequestReview.Decides). A read that fails is returned,
// so the delivery is retried rather than applied with a permission nobody read, and a daemon with
// no reader leaves every review's write access false.
func TestADecidingReviewCarriesItsAuthorsWriteAccessBeforeItIsApplied(t *testing.T) {
	t.Parallel()
	review := PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: "a-writer", CommitID: "head"}
	var asked []string
	answering := func(canWrite bool, err error) ConsumerSpec {
		return ConsumerSpec{AckWait: time.Second, ReviewPermission: func(_ context.Context, review PullRequestReview) (bool, error) {
			asked = append(asked, fmt.Sprintf("%s#%d %s", review.Repo, review.Number, review.Author))
			return canWrite, err
		}}
	}
	withAuthor := func(review PullRequestReview, author string) PullRequestReview {
		review.Author = author
		return review
	}
	for _, tc := range []struct {
		name     string
		fact     Fact
		spec     ConsumerSpec
		want     bool
		wantAsks []string
	}{
		{name: "an approval by an account with write access", fact: review, spec: answering(true, nil), want: true,
			wantAsks: []string{"acme/widgets#42 a-writer"}},
		{name: "an approval by an account without it", fact: review, spec: answering(false, nil),
			wantAsks: []string{"acme/widgets#42 a-writer"}},
		{name: "a request for changes", fact: withState(review, "changes_requested"), spec: answering(true, nil), want: true,
			wantAsks: []string{"acme/widgets#42 a-writer"}},
		{name: "a request for changes in upper case", fact: withState(review, "CHANGES_REQUESTED"), spec: answering(true, nil), want: true,
			wantAsks: []string{"acme/widgets#42 a-writer"}},
		{name: "a comment, which decides nothing whoever writes it", fact: withState(review, "commented"), spec: answering(true, nil),
			wantAsks: nil},
		{name: "an approval with no author", fact: withAuthor(review, ""), spec: answering(true, nil), wantAsks: nil},
		{name: "a daemon with no reader", fact: review, spec: ConsumerSpec{}, wantAsks: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked = nil
			got, err := resolveReviewPermission(context.Background(), tc.spec, tc.fact)
			if err != nil {
				t.Fatalf("resolve the review's permission: %v", err)
			}
			if resolved, ok := got.(PullRequestReview); !ok || resolved.AuthorCanWrite != tc.want {
				t.Fatalf("the review = %#v, want AuthorCanWrite %v", got, tc.want)
			}
			if !slices.Equal(asked, tc.wantAsks) {
				t.Fatalf("GitHub was asked %v, want %v", asked, tc.wantAsks)
			}
		})
	}

	if _, err := resolveReviewPermission(context.Background(), answering(false, errors.New("GitHub answered 502")), review); err == nil {
		t.Fatal("a failed permission read resolved; want the error, so the delivery is retried")
	}
	other := PullRequestChecks{Repo: "acme/widgets", Number: 42}
	if got, err := resolveReviewPermission(context.Background(), answering(true, nil), other); err != nil || !reflect.DeepEqual(got, Fact(other)) {
		t.Fatalf("a fact that is not a review = %#v, %v; want it unchanged", got, err)
	}
}

// resolveReviewBody restores a truncated review-App review's body from GitHub
// (ConsumerSpec.ReviewBody) before resolveReviewPermission reads its author's permission, since a
// restored body carries the Legion footer the workflow needs. It is read only for a review the
// normalizer capped, with an id and an author; a review with no id keeps its capped body and
// BodyTruncated true, and a failed read naks the delivery the same way a failed permission read
// does.
func TestResolveReviewBodyRestoresATruncatedReviewsBody(t *testing.T) {
	t.Parallel()
	truncated := PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 7, State: "approved", Author: "legion-reviewer[bot]",
		Body: "a capped body", BodyTruncated: true}
	var asked []PullRequestReview
	answering := func(body string, err error) ConsumerSpec {
		return ConsumerSpec{AckWait: time.Second, ReviewBody: func(_ context.Context, review PullRequestReview) (string, error) {
			asked = append(asked, review)
			return body, err
		}}
	}
	t.Run("a truncated review is replaced with the answer and BodyTruncated is cleared", func(t *testing.T) {
		asked = nil
		got, err := resolveReviewBody(context.Background(), answering("the full body with a footer", nil), truncated)
		if err != nil {
			t.Fatalf("resolve the review's body: %v", err)
		}
		resolved, ok := got.(PullRequestReview)
		if !ok || resolved.Body != "the full body with a footer" || resolved.BodyTruncated {
			t.Fatalf("the review = %#v, want the restored body with BodyTruncated cleared", got)
		}
		if len(asked) != 1 {
			t.Fatalf("GitHub was asked %d times, want once", len(asked))
		}
	})
	t.Run("a review with no id is not looked up", func(t *testing.T) {
		asked = nil
		noID := truncated
		noID.ID = 0
		got, err := resolveReviewBody(context.Background(), answering("ignored", nil), noID)
		if err != nil {
			t.Fatalf("resolve the review's body: %v", err)
		}
		resolved, ok := got.(PullRequestReview)
		if !ok || resolved.Body != "a capped body" || !resolved.BodyTruncated || len(asked) != 0 {
			t.Fatalf("a review with no id = %#v, asked %v; want its capped body kept and no call made", got, asked)
		}
	})
	t.Run("a review that was not truncated is not looked up", func(t *testing.T) {
		asked = nil
		full := truncated
		full.BodyTruncated = false
		got, err := resolveReviewBody(context.Background(), answering("ignored", nil), full)
		if err != nil {
			t.Fatalf("resolve the review's body: %v", err)
		}
		resolved, ok := got.(PullRequestReview)
		if !ok || resolved.Body != "a capped body" || len(asked) != 0 {
			t.Fatalf("an untruncated review = %#v, asked %v; want it unchanged and no call made", got, asked)
		}
	})
	t.Run("a daemon with no reader leaves the capped body", func(t *testing.T) {
		got, err := resolveReviewBody(context.Background(), ConsumerSpec{}, truncated)
		if err != nil {
			t.Fatalf("resolve the review's body: %v", err)
		}
		resolved, ok := got.(PullRequestReview)
		if !ok || resolved.Body != "a capped body" || !resolved.BodyTruncated {
			t.Fatalf("with no reader = %#v, want the capped body kept", got)
		}
	})
	t.Run("a failed read naks the delivery", func(t *testing.T) {
		if _, err := resolveReviewBody(context.Background(), answering("", errors.New("GitHub answered 502")), truncated); err == nil {
			t.Fatal("a failed body read resolved; want the error, so the delivery is retried")
		}
	})
	other := PullRequestChecks{Repo: "acme/widgets", Number: 42}
	if got, err := resolveReviewBody(context.Background(), answering("ignored", nil), other); err != nil || !reflect.DeepEqual(got, Fact(other)) {
		t.Fatalf("a fact that is not a review = %#v, %v; want it unchanged", got, err)
	}
}

// prepareArtifact has the daemon ready a Dispatch review event before it is applied
// (ConsumerSpec.PrepareArtifact), as prepareRepository does an issue event's: the workflow
// performs no I/O, so Dispatch's view of the issue a person's approval is on is read here, and the
// fact the preparation returns is the one applied. A failed preparation is the error, so the
// delivery is naked and retried rather than applied without the view; a daemon with no preparer,
// and every fact that is not a review, pass through untouched.
func TestPrepareArtifactReadiesAReviewEventBeforeItIsApplied(t *testing.T) {
	t.Parallel()
	approval := DispatchArtifact{Key: "LEGION-755", ArtifactID: "art-1", Kind: DispatchArtifactApproved, Version: 3, Actor: dispatch.Actor{Kind: "user", ID: "sami"}}
	view := dispatch.Issue{Key: "LEGION-755", Status: "backlog", HandedOver: true, PrimaryArtifactID: "art-1"}
	var asked []DispatchArtifact
	answering := func(issue *dispatch.Issue, err error) ConsumerSpec {
		return ConsumerSpec{AckWait: time.Second, PrepareArtifact: func(_ context.Context, artifact DispatchArtifact) (DispatchArtifact, error) {
			asked = append(asked, artifact)
			artifact.Issue = issue
			return artifact, err
		}}
	}
	t.Run("a review is the fact the preparer returns", func(t *testing.T) {
		asked = nil
		got, err := prepareArtifact(context.Background(), answering(&view, nil), approval)
		if err != nil {
			t.Fatalf("prepare the review's issue: %v", err)
		}
		prepared, ok := got.(DispatchArtifact)
		if !ok || prepared.Issue == nil || prepared.Issue.Key != view.Key || prepared.Issue.Status != view.Status {
			t.Fatalf("the review = %#v, want it carrying Dispatch's view of %s", got, view.Key)
		}
		if len(asked) != 1 || !reflect.DeepEqual(asked[0], approval) {
			t.Fatalf("the preparer was asked %#v, want once, with the decoded review", asked)
		}
	})
	t.Run("a daemon with no preparer applies the review as decoded", func(t *testing.T) {
		got, err := prepareArtifact(context.Background(), ConsumerSpec{AckWait: time.Second}, approval)
		if err != nil || !reflect.DeepEqual(got, Fact(approval)) {
			t.Fatalf("with no preparer = %#v, %v; want the review unchanged", got, err)
		}
	})
	t.Run("a failed preparation naks the delivery", func(t *testing.T) {
		if _, err := prepareArtifact(context.Background(), answering(nil, errors.New("Dispatch answered 502")), approval); err == nil || !strings.Contains(err.Error(), "Dispatch answered 502") {
			t.Fatalf("a failed preparation = %v; want the preparer's error, so the delivery is retried", err)
		}
	})
	asked = nil
	for _, other := range []Fact{
		DispatchIssue{Key: "LEGION-755", Seq: 1, Type: "issue.updated", Status: "triage", HandedOver: true},
		PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: "a-writer"},
	} {
		if got, err := prepareArtifact(context.Background(), answering(&view, nil), other); err != nil || !reflect.DeepEqual(got, other) {
			t.Fatalf("a fact that is not a review = %#v, %v; want it unchanged", got, err)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("the preparer was asked %#v for facts that are not reviews, want nothing", asked)
	}
}

// A permission read GitHub answers with its rate limit names how long GitHub asks to be left alone
// (RetryLater), and the review's delivery waits that long before it is read again, not the
// consumer's nak delay: every review retried each NakDelay would keep calling an API that already
// said no until its reset. A failure that names no wait is retried after NakDelay. Either way the
// review of the pull request the daemon records is applied once its read passes.
func TestARateLimitedPermissionReadWaitsTheTimeGitHubNames(t *testing.T) {
	t.Parallel()
	// rateLimitWait is far longer than the 25 ms nak delay the other row is retried after, so the
	// delay each row's nak asks for tells the two apart.
	const rateLimitWait = 2 * time.Second
	for _, tc := range []struct {
		name    string
		failure error
		limited bool
	}{
		{name: "a rate limit", failure: &RetryLater{After: rateLimitWait, Err: errors.New("GitHub answered 403: API rate limit exceeded")}, limited: true},
		{name: "a 502", failure: errors.New("GitHub answered 502")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			createWrites(t, pool)
			recordPullRequest(t, pool, "LEGION-208", 42)
			js, _ := testJetStream(t)
			logs := &lockedBuffer{}
			spec := consumerSpec(logs)
			// The permission reads this test counts are the handler's own, so nothing but the
			// failed read and its retry may read the permission. AckWait bounds each read the
			// handler makes (recordedPullRequest, resolveReviewPermission), and any fixed window
			// lets the machine decide whether one is cut: consumerSpec's 200 ms is the length of
			// this handler's own work on a loaded runner - two pool reads and the fact's
			// transaction - and a minute cuts a permission read of 61 s, whose nak brings a third
			// read. So the window, and the row's wait for the review to be applied, are what is left
			// of the test binary's deadline less 30 s (testwait.Remaining): a read however
			// slow is waited out, leaving the failed read and its retry as the only two reads, and
			// only one that never answers fails the row, by name, before the binary's own timeout.
			// The window is also what makes the count of two sound: a third read needs the whole
			// window to pass before a handling's nak or acknowledgement reaches the server. The
			// campaign of dispatch://INFRA-1113 measured that trip at
			// 516.8 ms at its largest, under 16 busy loops on 2 CPUs, and the handler's own work and
			// that trip together, about 717 ms, are far below this window. The progress keeper
			// reports nothing here: it ticks at a third of the window (spec.AckWait/3 in
			// consume.go), and each handling of this row ends, in a nak or in an acknowledgement, in
			// about 200 ms, long before the first tick is due (keepInProgress, ackprogress.go).
			// Where the margin closes, a third read can be made no earlier than a second delivery
			// of the message (the delivery mechanism: ConsumerSpec.HandlerLimit's comment in
			// consume.go).
			spec.AckWait = testwait.Remaining(t)
			var mu sync.Mutex
			var asks []time.Time
			spec.ReviewPermission = func(context.Context, PullRequestReview) (bool, error) {
				mu.Lock()
				defer mu.Unlock()
				asks = append(asks, time.Now())
				if len(asks) == 1 {
					return false, tc.failure
				}
				return true, nil
			}
			stop := startConsume(t, js, spec, pool, writeHandler("applied", nil))
			defer stop()

			publish(t, js, "notifications.github.sjawhar.legion.pr.42.review", capturedGitHubEnvelope(t, "review.json"))
			testwait.WithinPolling(t, testwait.Remaining(t), testwait.Poll, "the review applied once its read passed", func() bool {
				return writeCount(t, pool) == 1
			})
			mu.Lock()
			defer mu.Unlock()
			if len(asks) != 2 {
				t.Fatalf("the permission was read %d times, want twice", len(asks))
			}
			// What the row holds is the delay the intake asked JetStream for: nakResolve logs it with
			// the failure, from the value it passes to NakWithDelay. The time between the two
			// permission reads is no measure of that delay, since it also holds the redelivery's own
			// handling up to its permission read (the pull-request read among it), which a loaded
			// machine or a slow store stretches; it is held from below alone, since no redelivery
			// comes before the delay asked.
			want := spec.NakDelay
			if tc.limited {
				want = rateLimitWait
			}
			const failed = `msg="read the reviewer's repository permission"`
			written := logs.String()
			if naks, line := strings.Count(written, failed), logLine(written, "level=WARN", failed); naks != 1 || !strings.Contains(line, " retry_in="+want.String()+" ") {
				t.Fatalf("the intake logged %d naks of the failed permission read, the first %q, want one asking a delay of %s:\n%s", naks, line, want, written)
			}
			if gap := asks[1].Sub(asks[0]); gap < want {
				t.Fatalf("the failed read was made again after %s, sooner than the %s its nak asked", gap, want)
			}
		})
	}
}

func withState(review PullRequestReview, state string) PullRequestReview {
	review.State = state
	return review
}
