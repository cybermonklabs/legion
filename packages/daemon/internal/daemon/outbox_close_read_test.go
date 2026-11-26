package daemon

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// closeReadBoard is outboxDispatch answering the reads of an issue's close: its results (and its
// results at one commit, each commit asked kept in askedAt), its children, and its Verification
// document's approval and blocks at each version. resultsErr is what the results read answers in
// place of all that.
type closeReadBoard struct {
	*outboxDispatch
	mu         sync.Mutex
	results    dispatch.IssueResults
	atHead     dispatch.IssueResults
	askedAt    []string
	approved   int
	blocks     map[int][]dispatch.DocumentBlock
	resultsErr error
}

func (d *closeReadBoard) IssueResults(context.Context, string) (dispatch.IssueResults, error) {
	return d.results, d.resultsErr
}

func (d *closeReadBoard) IssueResultsAt(_ context.Context, _, commit string) (dispatch.IssueResults, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.askedAt = append(d.askedAt, commit)
	return d.atHead, nil
}

func (d *closeReadBoard) ApprovedVersionBefore(context.Context, string, int) (int, error) {
	return d.approved, nil
}

func (d *closeReadBoard) PlanDocument(_ context.Context, artifactID string, version int) (dispatch.DocumentVersion, error) {
	blocks, held := d.blocks[version]
	if !held {
		return dispatch.DocumentVersion{}, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "the document has no such version"}
	}
	return dispatch.DocumentVersion{ArtifactID: artifactID, IssueKey: "LEGION-208", Version: version, Blocks: blocks}, nil
}

// The read of an issue's checks for its close reads Dispatch's results, the results at the newest
// merge's final head where a before_merge check has no subject at that head, the issue's
// children, and the Verification document's approved and current check blocks, and carries them
// back as one CloseChecksRead fact of the issue: the generation the row was queued for, the
// moment the row began its first Dispatch call, and the answer in the close's stored layout
// (record.CheckReadAnswer). A check with a subject at the merge head is read once. A client's
// error fails the row, so the outbox retries it, and the fact says why the read failed. An
// upstream error page is kept as the daemon stores a far side's error text, cut to its start.
func TestACloseReadCarriesTheStatesBack(t *testing.T) {
	t.Parallel()
	const mergeHead = "1111111111111111111111111111111111111111"
	const openHead = "2222222222222222222222222222222222222222"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	since := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	putOutboxIssue(t, pool, records, waitingIssue(t, since, record.StoredWait{Entered: since}))
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := records.PutMergeSnapshot(ctx, tx, "LEGION-208", record.Merge{Number: 70, Head: mergeHead, SHA: "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0",
			Base: "main", Repo: "acme/widgets", MergedAt: since})
		return err
	}); err != nil {
		t.Fatalf("store the merge snapshot: %v", err)
	}
	beforeMerge := func(head string) dispatch.CheckBlock {
		return dispatch.CheckBlock{BlockID: "k1", When: dispatch.CheckBeforeMerge, PassStands: dispatch.PassStands{Answer: dispatch.PassStandsNo,
			Subjects: []dispatch.PassStandsSubject{{Basis: dispatch.CheckBasis{Kind: dispatch.BasisPullRequest, Commit: head}, Answer: dispatch.PassStandsNo,
				Reason: dispatch.PassStandsReasonNoResult}}}}
	}
	check := []dispatch.DocumentBlock{{ID: "k1", Type: "check", Attributes: map[string]string{"when": "before_merge"}, Text: "The change answers."}}
	board := &closeReadBoard{
		outboxDispatch: &outboxDispatch{
			issue:    dispatch.Issue{Key: "LEGION-208", Status: "testing", Children: []dispatch.IssueChild{{Key: "LEGION-209", Status: "backlog"}}},
			approval: dispatch.Approval{IssueKey: "LEGION-208", State: "approved", LatestVersion: 3},
		},
		results: dispatch.IssueResults{Issue: "LEGION-208", VerificationArtifactID: "verification-208", Checks: []dispatch.CheckBlock{beforeMerge(openHead)}},
		atHead: dispatch.IssueResults{Issue: "LEGION-208", VerificationArtifactID: "verification-208", Checks: []dispatch.CheckBlock{{BlockID: "k1",
			When: dispatch.CheckBeforeMerge, ComparedWith: []dispatch.CheckBasis{{Kind: dispatch.BasisNamed, Commit: mergeHead}},
			PassStands: dispatch.PassStands{Answer: dispatch.PassStandsYes}}}},
		approved: 2,
		blocks:   map[int][]dispatch.DocumentBlock{2: check, 3: check},
	}
	handler := &outboxFactHandler{}
	readAt := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	runner := &outbox{log: quietLogger(), pool: pool, records: records, dispatch: board, handlers: []intake.Handler{handler},
		now: func() time.Time { return readAt }}
	run := func(id int64) error {
		t.Helper()
		row := mustOutboxRow(t, "LEGION-208", record.CloseRead{Generation: 3}, time.Now())
		row.ID = id
		return runner.execute(context.Background(), row)
	}

	if err := run(81); err != nil {
		t.Fatalf("the read: %v", err)
	}
	want := intake.CloseChecksRead{Issue: "LEGION-208", Generation: 3, ReadAt: readAt, Answer: &record.CheckReadAnswer{
		Results: board.results, AtMergeHead: &board.atHead, Children: board.issue.Children,
		Approval: &record.CheckApproval{Version: 2, Approved: check, Current: check}}}
	if facts := handler.facts(); len(facts) != 1 || !reflect.DeepEqual(facts[0], want) {
		t.Fatalf("the read carried %#v, want one fact %#v", facts, want)
	}
	if !slices.Equal(board.askedAt, []string{mergeHead}) {
		t.Fatalf("the read asked the results at %q, want once at the merge head %s", board.askedAt, mergeHead)
	}

	board.results.Checks = []dispatch.CheckBlock{beforeMerge(mergeHead)}
	if err := run(82); err != nil {
		t.Fatalf("the read of a check with a subject at the merge head: %v", err)
	}
	if facts := handler.facts(); len(facts) != 2 || facts[1].(intake.CloseChecksRead).Answer.AtMergeHead != nil || len(board.askedAt) != 1 {
		t.Fatalf("a check with a subject at the merge head was read at %q, carrying %#v; want no read at one commit", board.askedAt, facts)
	}

	board.resultsErr = &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is down"}
	row := mustOutboxRow(t, "LEGION-208", record.CloseRead{Generation: 3}, time.Now())
	row.ID = 83
	err := runner.execute(context.Background(), row)
	var refusal *dispatch.Error
	if err == nil || !errors.As(err, &refusal) || permanentRefusal(row, err) {
		t.Fatalf("a read Dispatch failed = %v, want a failure the row runs again on", err)
	}
	facts := handler.facts()
	failed, ok := facts[len(facts)-1].(intake.CloseChecksRead)
	if len(facts) != 3 || !ok || failed.Answer != nil || failed.Failure == "" || !failed.ReadAt.Equal(readAt) || failed.Generation != 3 {
		t.Fatalf("the failed read carried %#v, want a fact naming why it failed and no answer", facts)
	}

	page := "<html><body>" + strings.Repeat("<p>502 Bad Gateway</p>", 400) + "</body></html>"
	board.resultsErr = &dispatch.Error{Status: http.StatusBadGateway, Message: page}
	row = mustOutboxRow(t, "LEGION-208", record.CloseRead{Generation: 3}, time.Now())
	row.ID = 84
	if err := runner.execute(context.Background(), row); err == nil {
		t.Fatal("a read Dispatch answered with an error page succeeded, want a failure the row runs again on")
	}
	facts = handler.facts()
	cut, ok := facts[len(facts)-1].(intake.CloseChecksRead)
	want = intake.CloseChecksRead{Issue: "LEGION-208", Generation: 3, ReadAt: readAt,
		Failure: storetext.FarSideError("read the results of LEGION-208: " + page)}
	if len(facts) != 4 || !ok || !reflect.DeepEqual(cut, want) || utf8.RuneCountInString(cut.Failure) > storetext.FarSideErrorRunes {
		t.Fatalf("the read answered by an error page of %d bytes carried %d runes of failure, want it cut to its first %d: %#v",
			len(page), utf8.RuneCountInString(cut.Failure), storetext.FarSideErrorRunes, cut)
	}
}
