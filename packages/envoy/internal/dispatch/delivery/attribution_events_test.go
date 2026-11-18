package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// attributionEvent is one stored issue.pull_request_attribution_changed event, as the API and the
// issue's timeline read it.
type attributionEvent struct {
	Seq     int
	Actor   model.Actor
	Notify  bool
	Payload struct {
		Repo   string  `json:"repo"`
		Number int     `json:"number"`
		URL    string  `json:"url"`
		From   *string `json:"from"`
		To     *string `json:"to"`
		Source string  `json:"source"`
	}
}

// attributionEvents reads one issue's stored attribution events, oldest first.
func attributionEvents(t *testing.T, ctx context.Context, pool *store.Pool, issue string) []attributionEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select seq, actor, notify, payload from events where issue_key = $1 and type = $2 order by id
	`, issue, AttributionEventType)
	if err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	defer rows.Close()
	var found []attributionEvent
	for rows.Next() {
		var event attributionEvent
		var actor, payload []byte
		if err := rows.Scan(&event.Seq, &actor, &event.Notify, &payload); err != nil {
			t.Fatalf("scan an event of %s: %v", issue, err)
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			t.Fatalf("decode an event actor: %v", err)
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			t.Fatalf("decode an event payload: %v", err)
		}
		found = append(found, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	return found
}

// attributionEventCount is how many attribution events issue carries, and how many numbers of
// pull requests they name.
func attributionEventCount(t *testing.T, ctx context.Context, pool *store.Pool, issue string) (found, numbers int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		select count(*), count(distinct payload->>'number') from events where issue_key = $1 and type = $2
	`, issue, AttributionEventType).Scan(&found, &numbers); err != nil {
		t.Fatalf("count the events of %s: %v", issue, err)
	}
	return found, numbers
}

// attributedHeadRow is the head of an open pull request whose texts name issue by source: the
// branch for sourceBranch, else the title. Its messages are read, so the statement gives the
// issue from those facts. A nil issue names none.
func attributedHeadRow(number int, issue *string, source attributionSource) PullRequestHead {
	branch, title := "work/none", "feat: the work"
	if issue != nil && source == sourceBranch {
		branch = "legion/" + *issue
	} else if issue != nil {
		title = *issue + " the work"
	}
	return PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a",
		HeadRef: branch, BaseRef: "main", State: "open", Title: title,
		URL:   "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
		Texts: &PullRequestTexts{HeadRepo: "acme/widgets", BaseRef: "main", Title: title, CommitMessages: []string{}, MessagesRead: true},
	}
}

// attributionPass is the part of the head pass that records each answer: SyncAttributions, then
// AppendAttributionEvents (reconcilePullRequestHeads). It answers the sync, and the count of events
// that the append appended.
func attributionPass(t *testing.T, ctx context.Context, pool *store.Pool, broker *events.Broker) (AttributionSync, int) {
	t.Helper()
	synced := syncAttributions(t, ctx, pool)
	appended, err := AppendAttributionEvents(ctx, pool, broker)
	if err != nil {
		t.Fatalf("AppendAttributionEvents: %v", err)
	}
	return synced, appended
}

// syncAttributions is SyncAttributions, and it stops the test where the sync fails.
func syncAttributions(t *testing.T, ctx context.Context, pool *store.Pool) AttributionSync {
	t.Helper()
	synced, err := SyncAttributions(ctx, pool)
	if err != nil {
		t.Fatalf("SyncAttributions: %v", err)
	}
	return synced
}

// nobodyOr is key, or "nobody" where key is nil.
func nobodyOr(key *string) string {
	if key == nil {
		return "nobody"
	}
	return *key
}

// queuedMoves is each row of pull_request_attribution_moves in the order of its key, as
// "<repo>#<number> <from> -> <to> by <source>".
func queuedMoves(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `select repo, number, from_key, to_key, source from pull_request_attribution_moves order by repo, number`)
	if err != nil {
		t.Fatalf("read the queued moves: %v", err)
	}
	defer rows.Close()
	moves := []string{}
	for rows.Next() {
		var repo, source string
		var number int
		var from, to *string
		if err := rows.Scan(&repo, &number, &from, &to, &source); err != nil {
			t.Fatalf("scan a queued move: %v", err)
		}
		moves = append(moves, fmt.Sprintf("%s#%d %s -> %s by %s", repo, number, nobodyOr(from), nobodyOr(to), source))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the queued moves: %v", err)
	}
	return moves
}

// queuedMoveCount is the count of rows of pull_request_attribution_moves.
func queuedMoveCount(t *testing.T, ctx context.Context, pool *store.Pool) int {
	t.Helper()
	var queued int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_attribution_moves`).Scan(&queued); err != nil {
		t.Fatalf("count the queued moves: %v", err)
	}
	return queued
}

// storedAttributionOf is what the results row of repo#number stores, as "<issue> by <source>": the
// columns issue_key and attribution_source, which the sync writes and no reader reads for a pull
// request with facts.
func storedAttributionOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	var issue, source *string
	if err := pool.QueryRow(ctx, `select issue_key, attribution_source from pull_request_heads where repo = $1 and number = $2`,
		repo, number).Scan(&issue, &source); err != nil {
		t.Fatalf("read the stored issue of %s#%d: %v", repo, number, err)
	}
	return nobodyOr(issue) + " by " + nobodyOr(source)
}

// timelineIssueOf is the issue_key of the timeline row of repo#number, or "nobody".
func timelineIssueOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	var issue *string
	if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&issue); err != nil {
		t.Fatalf("read the timeline issue of %s#%d: %v", repo, number, err)
	}
	return nobodyOr(issue)
}

// rowVersionOf is the transaction that last wrote the results row of repo#number (xmin). A write
// of any column of the row changes it.
func rowVersionOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	var version string
	if err := pool.QueryRow(ctx, `select xmin::text from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&version); err != nil {
		t.Fatalf("read the version of %s#%d: %v", repo, number, err)
	}
	return version
}

// owedReadsOf is the count of reads that pull_request_reads_owed owes for repo#number.
func owedReadsOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) int {
	t.Helper()
	var owed int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed where repo = $1 and number = $2`, repo, number).Scan(&owed); err != nil {
		t.Fatalf("count the reads owed for %s#%d: %v", repo, number, err)
	}
	return owed
}

// A pass records a move of a pull request on both issues. It appends one event on the issue that
// the pull request left and one on the issue that it reached. Each event names both sides and the
// source of the new issue. A move from nobody, or to nobody, has one event. The write of the facts
// appends no event: the pass appends it, after the sync records the answer. A move changes the
// heads of both issues (HeadsAttributedTo), and so the results of both. It fails where
// AppendAttributionEvents appends no event, appends on one side only, or names the wrong sides, and
// where a write of facts appends an event.
func TestAnAttributionThatMovesIsRecordedOnBothIssues(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	left := seedHeadIssue(t, ctx, pool, 70)
	reached := seedHeadIssue(t, ctx, pool, 71)

	if err := UpsertPullRequestHead(ctx, pool, attributedHeadRow(170, &left, sourceTitleBody)); err != nil {
		t.Fatalf("store the head attributed to %s: %v", left, err)
	}
	if found := attributionEvents(t, ctx, pool, left); len(found) != 0 {
		t.Fatalf("%s carries %d events after the write, want 0: no write appends an event", left, len(found))
	}
	if _, appended := attributionPass(t, ctx, pool, broker); appended != 1 {
		t.Errorf("the first pass appended %d events, want 1: the move from nobody to %s", appended, left)
	}
	// Reaching an issue from nobody is a move too: one event, on the issue it reached.
	first := attributionEvents(t, ctx, pool, left)
	if len(first) != 1 || first[0].Payload.From != nil || first[0].Payload.To == nil || *first[0].Payload.To != left {
		t.Fatalf("%s carries %+v, want one event reaching it from nobody", left, first)
	}
	if first[0].Actor != AttributionEventActor || first[0].Notify || first[0].Seq <= 0 {
		t.Fatalf("the event's actor %+v, notify %v, seq %d; want the delivery-attribution system actor, quiet, a positive sequence",
			first[0].Actor, first[0].Notify, first[0].Seq)
	}

	if err := UpsertPullRequestHead(ctx, pool, attributedHeadRow(170, &reached, sourceBranch)); err != nil {
		t.Fatalf("move the head to %s: %v", reached, err)
	}
	if before, beside := attributionEvents(t, ctx, pool, left), attributionEvents(t, ctx, pool, reached); len(before) != 1 || len(beside) != 0 {
		t.Fatalf("after the write and before the pass, %s carries %d events and %s %d, want 1 and 0: no write appends an event",
			left, len(before), reached, len(beside))
	}
	if _, appended := attributionPass(t, ctx, pool, broker); appended != 2 {
		t.Errorf("the second pass appended %d events, want 2: one on %s and one on %s", appended, left, reached)
	}
	moved := attributionEvents(t, ctx, pool, left)
	if len(moved) != 2 {
		t.Fatalf("%s carries %d events, want 2: the one that credited it and the one that moved the pull request away", left, len(moved))
	}
	reachedEvents := attributionEvents(t, ctx, pool, reached)
	if len(reachedEvents) != 1 {
		t.Fatalf("%s carries %d events, want 1: the move that reached it", reached, len(reachedEvents))
	}
	for _, event := range []attributionEvent{moved[1], reachedEvents[0]} {
		if event.Payload.From == nil || *event.Payload.From != left {
			t.Errorf("the move's `from` = %v, want %s", event.Payload.From, left)
		}
		if event.Payload.To == nil || *event.Payload.To != reached {
			t.Errorf("the move's `to` = %v, want %s", event.Payload.To, reached)
		}
		if event.Payload.Source != string(sourceBranch) {
			t.Errorf("the move's source = %q, want %q", event.Payload.Source, sourceBranch)
		}
		if event.Payload.Repo != "acme/widgets" || event.Payload.Number != 170 {
			t.Errorf("the move names %s#%d, want acme/widgets#170", event.Payload.Repo, event.Payload.Number)
		}
		if event.Actor != AttributionEventActor || event.Notify || event.Seq <= 0 {
			t.Errorf("the move's actor %+v, notify %v, seq %d; want the delivery-attribution system actor, quiet, a positive sequence",
				event.Actor, event.Notify, event.Seq)
		}
	}

	// A pull request that reaches nobody records that too, on the issue it left alone.
	if err := UpsertPullRequestHead(ctx, pool, attributedHeadRow(170, nil, sourceNone)); err != nil {
		t.Fatalf("uncredit the head: %v", err)
	}
	if _, appended := attributionPass(t, ctx, pool, broker); appended != 1 {
		t.Errorf("the third pass appended %d events, want 1: the move from %s to nobody", appended, reached)
	}
	uncredited := attributionEvents(t, ctx, pool, reached)
	if len(uncredited) != 2 || uncredited[1].Payload.To != nil || uncredited[1].Payload.From == nil || *uncredited[1].Payload.From != reached {
		t.Fatalf("%s carries %+v, want a second event leaving it credited to nobody", reached, uncredited)
	}
	if found := attributionEvents(t, ctx, pool, left); len(found) != 2 {
		t.Errorf("%s carries %d events after the move to nobody, want still 2", left, len(found))
	}
}

// A pass records nothing for an answer that stays. A write of the same facts, a write that gives
// the same issue from the title, and a write whose messages are not read but whose branch names the
// issue each leave the answer. The pass after each appends nothing, and leaves no move queued. The
// reconcile reads every open row every five minutes, so an event at each pass would bury the
// timeline of each issue under the pass's own heartbeat. It fails where AppendAttributionEvents
// appends an event for a move whose two keys are the same, or where a sync moves a stay.
func TestAnAttributionThatStaysIsRecordedNowhere(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	issue := seedHeadIssue(t, ctx, pool, 72)

	if err := UpsertPullRequestHead(ctx, pool, attributedHeadRow(172, &issue, sourceBranch)); err != nil {
		t.Fatalf("store the head attributed to %s: %v", issue, err)
	}
	if _, appended := attributionPass(t, ctx, pool, broker); appended != 1 {
		t.Errorf("the first pass appended %d events, want 1: the move from nobody to %s", appended, issue)
	}
	notRead := attributedHeadRow(172, &issue, sourceBranch)
	notRead.Texts.CommitMessages, notRead.Texts.MessagesRead = nil, false
	for _, again := range []struct {
		what string
		head PullRequestHead
	}{
		{"the same facts", attributedHeadRow(172, &issue, sourceBranch)},
		// The same issue from another source is no move either: the row credits the same issue.
		{"the same issue from the title", attributedHeadRow(172, &issue, sourceTitleBody)},
		// A higher source names a held issue, so the messages decide nothing.
		{"the branch's issue with the messages not read", notRead},
	} {
		if err := UpsertPullRequestHead(ctx, pool, again.head); err != nil {
			t.Fatalf("write %s: %v", again.what, err)
		}
		if _, appended := attributionPass(t, ctx, pool, broker); appended != 0 {
			t.Errorf("the pass after a write of %s appended %d events, want 0", again.what, appended)
		}
		if moves := queuedMoves(t, ctx, pool); len(moves) != 0 {
			t.Errorf("after the pass that followed a write of %s, the queue holds %v, want none", again.what, moves)
		}
	}
	if found := attributionEvents(t, ctx, pool, issue); len(found) != 1 {
		t.Fatalf("%s carries %d events, want 1: only the pass that first credited it moved anything", issue, len(found))
	}
	if stored := storedAttributionOf(t, ctx, pool, "acme/widgets", 172); stored != issue+" by branch" {
		t.Errorf("the results row stores %s, want %s by branch", stored, issue)
	}
	if stored := readHead(t, ctx, pool, "acme/widgets", 172); stored.IssueKey == nil || *stored.IssueKey != issue {
		t.Fatalf("the row = %+v, want it still credited to %s", stored, issue)
	}
}

// storeOlderRuleRows stores count merged pull requests of acme/widgets from number first on, each
// with current texts and messages read. The branch of each names rule, and its title names older.
// Each results row stores older from the title, as an older rule that read the title first stored it.
func storeOlderRuleRows(t *testing.T, ctx context.Context, pool *store.Pool, first, count int, older, rule string) {
	t.Helper()
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, title, url, event_at, checked_at, github_read_at)
		select 'acme/widgets', n, pg_catalog.lpad(pg_catalog.to_hex(n), 40, '0'), 'legion/' || $4::text, 'main', 'merged', $3::text, 'title_body',
			$3::text || ' the work', 'https://github.com/acme/widgets/pull/' || n, $5::timestamptz, now(), now()
		from pg_catalog.generate_series($1::int, $1::int + $2::int - 1) n
	`, first, count, older, rule, at); err != nil {
		t.Fatalf("store %d results rows: %v", count, err)
	}
	if _, err := pool.Exec(ctx, `
		insert into pull_request_texts (repo, number, head_sha, head_ref, head_repo, base_ref, title, body, commit_messages, updated_at)
		select h.repo, h.number, h.head_sha, h.head_ref, h.repo, h.base_ref, h.title, '', '{}', h.event_at
		from pull_request_heads h where h.repo = 'acme/widgets' and h.number between $1::int and $1::int + $2::int - 1
	`, first, count); err != nil {
		t.Fatalf("store the texts of %d rows: %v", count, err)
	}
}

// One pass records the answer of every pull request, and appends the events of at most 500 moves.
// 1,200 merged rows store the issue that an older rule gave, and their facts give another issue.
// After one pass each results row holds the statement's answer. Each side carries 500 events, and
// 700 moves stay queued. Two more passes append the rest: then each side carries 1,200 events, one
// for each pull request, and no move stays queued. It fails where the sync caps its writes, where
// the append takes another count of moves in a pass, or where it appends one side of a move alone.
func TestAPassRecordsEveryChangeAndAppendsAtMostFiveHundredEvents(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	older := seedHeadIssue(t, ctx, pool, 82)
	rule := seedHeadIssue(t, ctx, pool, 83)
	const first, rows = 1001, 1200
	storeOlderRuleRows(t, ctx, pool, first, rows, older, rule)

	wantSides := func(when string, want int) {
		t.Helper()
		for _, issue := range []string{older, rule} {
			if found, numbers := attributionEventCount(t, ctx, pool, issue); found != want || numbers != want {
				t.Errorf("%s: %s carries %d events of %d pull requests, want %d of %d", when, issue, found, numbers, want, want)
			}
		}
		var unmatched int
		if err := pool.QueryRow(ctx, `
			select count(*) from (select payload->>'number' n from events where issue_key = $1 and type = $3) l
			full join (select payload->>'number' n from events where issue_key = $2 and type = $3) r using (n)
			where l.n is null or r.n is null
		`, older, rule, AttributionEventType).Scan(&unmatched); err != nil {
			t.Fatalf("compare the two sides' events: %v", err)
		}
		if unmatched != 0 {
			t.Errorf("%s: %d pull requests have an event on one side alone, want 0", when, unmatched)
		}
	}

	synced, appended := attributionPass(t, ctx, pool, broker)
	if synced.Changes != rows {
		t.Errorf("the first sync upserted %d moves, want %d: one for each pull request", synced.Changes, rows)
	}
	if appended != 1000 {
		t.Errorf("the first pass appended %d events, want 1,000: 500 moves, with an event on each side", appended)
	}
	var answered int
	if err := pool.QueryRow(ctx, `
		select count(*) from pull_request_heads
		where repo = 'acme/widgets' and number between $1::int and $1::int + $2::int - 1 and issue_key = $3 and attribution_source = $4
	`, first, rows, rule, string(sourceBranch)).Scan(&answered); err != nil {
		t.Fatalf("count the rows that hold the answer: %v", err)
	}
	if answered != rows {
		t.Errorf("after one pass %d of %d results rows store %s by branch, want all", answered, rows, rule)
	}
	wantSides("after one pass", 500)
	if queued := queuedMoveCount(t, ctx, pool); queued != 700 {
		t.Errorf("after one pass %d moves are queued, want 700", queued)
	}

	for pass, want := range []int{1000, 400} {
		synced, appended := attributionPass(t, ctx, pool, broker)
		if synced.Changes != 0 {
			t.Errorf("pass %d upserted %d moves, want 0: every row holds its answer", pass+2, synced.Changes)
		}
		if appended != want {
			t.Errorf("pass %d appended %d events, want %d", pass+2, appended, want)
		}
	}
	wantSides("after three passes", rows)
	if queued := queuedMoveCount(t, ctx, pool); queued != 0 {
		t.Errorf("after three passes %d moves are queued, want 0", queued)
	}
}

// A pass appends nothing where each answer stays. A pull request whose stored issue and source are
// the rule's answer, one whose stored source alone differs, one with no facts, and one with a
// timeline row alone each keep their issue. Control in the same test: a pull request whose answer
// moves gets its two events, so the count of events can fail. A second pass upserts no move. It
// fails where AppendAttributionEvents appends an event for a move whose two keys are the same, or
// where a sync moves a pull request whose answer is its stored issue.
func TestAPassAppendsNothingForAnAnswerThatStays(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	stays := seedHeadIssue(t, ctx, pool, 80)
	leaves := seedHeadIssue(t, ctx, pool, 81)
	timeline := &statementTimeline{issue: &stays}
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 280, branch: "legion/" + stays, title: "no key",
		stored: &stays, texts: readTexts(""), timeline: timeline})
	seedStoredIssue(t, ctx, pool, "acme/widgets", 280, &stays, string(sourceBranch))
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 281, branch: "legion/" + stays, title: "no key",
		stored: &stays, texts: readTexts("")})
	seedStoredIssue(t, ctx, pool, "acme/widgets", 281, &stays, string(sourceTitleBody))
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 282, branch: "main", title: "no key", stored: &stays, timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 283, title: "no key", noHead: true, timeline: timeline})
	// The control: #284 stores another issue, and its branch names the issue that stays.
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 284, branch: "legion/" + stays, title: "no key",
		stored: &leaves, texts: readTexts("")})

	if _, appended := attributionPass(t, ctx, pool, broker); appended != 2 {
		t.Errorf("the pass appended %d events, want 2: the control's move alone", appended)
	}
	for issue, want := range map[string]int{stays: 1, leaves: 1} {
		found := attributionEvents(t, ctx, pool, issue)
		if len(found) != want {
			t.Errorf("%s carries %d events, want %d", issue, len(found), want)
			continue
		}
		if event := found[0]; event.Payload.Number != 284 || nobodyOr(event.Payload.From) != leaves || nobodyOr(event.Payload.To) != stays {
			t.Errorf("%s carries the event %+v, want the control's move of #284 from %s to %s", issue, event.Payload, leaves, stays)
		}
	}
	if moves := queuedMoves(t, ctx, pool); len(moves) != 0 {
		t.Errorf("after the pass the queue holds %v, want none", moves)
	}
	for number, want := range map[int]string{280: stays + " by branch", 281: stays + " by branch", 282: stays + " by none", 284: stays + " by branch"} {
		if stored := storedAttributionOf(t, ctx, pool, "acme/widgets", number); stored != want {
			t.Errorf("#%d stores %s, want %s", number, stored, want)
		}
	}
	for _, number := range []int{280, 282, 283} {
		if issue := timelineIssueOf(t, ctx, pool, "acme/widgets", number); issue != stays {
			t.Errorf("the timeline row of #%d names %s, want %s", number, issue, stays)
		}
	}
	if synced, appended := attributionPass(t, ctx, pool, broker); synced.Changes != 0 || appended != 0 {
		t.Errorf("a second pass upserted %d moves and appended %d events, want 0 and 0", synced.Changes, appended)
	}
}

// A sync writes nothing for a pull request whose answer is not known. The rows are those of
// TestAMoveAwayFromAPullRequestWhoseAnswerIsNotKnown, and one pass of the sync stores PRNT-581 in
// place of the update there. After the move away, one sync queues no move and writes no column of
// the results row. Then the facts line reads the messages, which name PRNT-582, and the next syncs
// queue the move from PRNT-581 to PRNT-582 once. Control in the same test: with the messages read,
// the same move away queues a move to nobody, so the check of the queue can fail. It fails where
// SyncAttributions writes the not-known answer, nobody, over the stored issue, or queues its move.
func TestASyncWritesNothingForAnAnswerThatIsNotKnown(t *testing.T) {
	for _, read := range []bool{false, true} {
		name := map[bool]string{false: "messages_not_read", true: "control_messages_read"}[read]
		t.Run(name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			broker := events.NewBroker()
			seedProjectRepository(t, ctx, pool, "PRNT", "PRNT", "acme/prnt-main")
			mapRepository(t, ctx, pool, "acme/prnt-side", "PRNT", false, nil)
			insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-180", 180)
			insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-581", 581)
			insertProjectIssue(t, ctx, pool, "PRNT", "PRNT-582", 582)
			execDispatchChange(t, ctx, pool, "give PRNT-180 its own repository", `update issues set repo = 'acme/prnt-side' where key = 'PRNT-180'`)
			execDispatchChange(t, ctx, pool, "give PRNT-582 that repository", `update issues set repo = 'acme/prnt-side' where key = 'PRNT-582'`)
			execDispatchChange(t, ctx, pool, "make PRNT-581 a child of PRNT-180", `update issues set parent_key = 'PRNT-180' where key = 'PRNT-581'`)
			texts := &statementTexts{}
			if read {
				texts = readTexts("")
			}
			storeStatementRow(t, ctx, pool, statementRow{repo: "acme/prnt-side", number: 581, branch: "legion/PRNT-581", title: "no key", texts: texts})
			if _, appended := attributionPass(t, ctx, pool, broker); appended != 1 {
				t.Errorf("the first pass appended %d events, want 1: the move from nobody to PRNT-581", appended)
			}
			if stored := storedAttributionOf(t, ctx, pool, "acme/prnt-side", 581); stored != "PRNT-581 by branch" {
				t.Fatalf("after the first pass the results row stores %s, want PRNT-581 by branch", stored)
			}
			execDispatchChange(t, ctx, pool, "move PRNT-581 away from PRNT-180", `update issues set parent_key = null where key = 'PRNT-581'`)
			version := rowVersionOf(t, ctx, pool, "acme/prnt-side", 581)

			synced := syncAttributions(t, ctx, pool)
			moves := queuedMoves(t, ctx, pool)
			stored := storedAttributionOf(t, ctx, pool, "acme/prnt-side", 581)
			if read {
				if want := []string{"acme/prnt-side#581 PRNT-581 -> nobody by none"}; synced.Changes != 1 || !slices.Equal(moves, want) {
					t.Errorf("the control: after the move away one sync upserted %d moves and queued %v, want 1 and %v", synced.Changes, moves, want)
				}
				if stored != "nobody by none" {
					t.Errorf("the control: after the move away the results row stores %s, want nobody by none", stored)
				}
				return
			}
			if synced.Changes != 0 || len(moves) != 0 {
				t.Errorf("after the move away one sync upserted %d moves and queued %v, want none: the answer is not known", synced.Changes, moves)
			}
			if stored != "PRNT-581 by branch" {
				t.Errorf("after the move away the results row stores %s, want PRNT-581 by branch, as before the sync", stored)
			}
			if after := rowVersionOf(t, ctx, pool, "acme/prnt-side", 581); after != version {
				t.Errorf("the sync wrote the results row (version %s, then %s), want no write", version, after)
			}

			fake := newFakeGitHub(t)
			github := serveBatchGitHub(t, fake)
			github.set("acme/prnt-side", 581, batchPull{
				state: "merged", head: headOf(581), branch: "legion/PRNT-581", title: "no key",
				updatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), messages: []string{"Work for PRNT-582."},
			})
			if _, err := NewReconcile(pool, fake.newTestClient(), broker).readFactsLine(ctx, &passInstallations{}, &passRateLimit{}); err != nil {
				t.Fatalf("readFactsLine: %v", err)
			}
			wantAnswer(t, ctx, pool, "acme/prnt-side", 581, "PRNT-582 from commit_message")
			want := []string{"acme/prnt-side#581 PRNT-581 -> PRNT-582 by commit_message"}
			for i, wantChanges := range []int{1, 0} {
				if synced := syncAttributions(t, ctx, pool); synced.Changes != wantChanges {
					t.Errorf("sync %d after the facts line upserted %d moves, want %d", i+1, synced.Changes, wantChanges)
				}
				if moves := queuedMoves(t, ctx, pool); !slices.Equal(moves, want) {
					t.Errorf("after sync %d of the facts line the queue holds %v, want %v", i+1, moves, want)
				}
			}
			if appended, err := AppendAttributionEvents(ctx, pool, broker); err != nil || appended != 2 {
				t.Errorf("AppendAttributionEvents = %d, %v; want 2 events: one on PRNT-581 and one on PRNT-582", appended, err)
			}
			if found := attributionEvents(t, ctx, pool, "PRNT-582"); len(found) != 1 || nobodyOr(found[0].Payload.From) != "PRNT-581" {
				t.Errorf("PRNT-582 carries %+v, want one event of the move from PRNT-581", found)
			}
			if found := attributionEvents(t, ctx, pool, "PRNT-581"); len(found) != 2 {
				t.Errorf("PRNT-581 carries %d events, want 2: the move to it, and the move away from it", len(found))
			}
		})
	}
}

// A move and the move back before the append records nothing. A link of HEAD-91 moves #290 away
// from HEAD-90, and the sync queues that move. The link goes, and the next sync moves #290 back.
// The queued row keeps its first from_key, HEAD-90, and its to_key returns to HEAD-90. So the
// append appends no event, and the queue is empty after it. It fails where the upsert of a move
// writes from_key again, or where the sync or the write of the link appends an event.
func TestAMoveAndBackBeforeItsEventAppendsNothing(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	home := seedHeadIssue(t, ctx, pool, 90)
	away := seedHeadIssue(t, ctx, pool, 91)
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 290, branch: "legion/" + home, title: "no key",
		stored: &home, texts: readTexts("")})
	seedStoredIssue(t, ctx, pool, "acme/widgets", 290, &home, string(sourceBranch))
	const url = "https://github.com/acme/widgets/pull/290"

	execDispatchChange(t, ctx, pool, "link #290 to "+away, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')`, away, url)
	if synced := syncAttributions(t, ctx, pool); synced.Changes != 1 {
		t.Errorf("the sync after the link upserted %d moves, want 1", synced.Changes)
	}
	if want, moves := []string{"acme/widgets#290 " + home + " -> " + away + " by external_link"}, queuedMoves(t, ctx, pool); !slices.Equal(moves, want) {
		t.Fatalf("after the link the queue holds %v, want %v", moves, want)
	}

	execDispatchChange(t, ctx, pool, "drop the link of "+away, `delete from issue_external_links where issue_key = $1 and url = $2`, away, url)
	if synced := syncAttributions(t, ctx, pool); synced.Changes != 1 {
		t.Errorf("the sync after the link went upserted %d moves, want 1", synced.Changes)
	}
	if want, moves := []string{"acme/widgets#290 " + home + " -> " + home + " by branch"}, queuedMoves(t, ctx, pool); !slices.Equal(moves, want) {
		t.Errorf("after the move back the queue holds %v, want %v: the first from_key stays", moves, want)
	}
	if stored := storedAttributionOf(t, ctx, pool, "acme/widgets", 290); stored != home+" by branch" {
		t.Errorf("after the move back the results row stores %s, want %s by branch", stored, home)
	}

	if appended, err := AppendAttributionEvents(ctx, pool, broker); err != nil || appended != 0 {
		t.Errorf("AppendAttributionEvents = %d, %v; want 0 events: the pull request is where it was", appended, err)
	}
	for _, issue := range []string{home, away} {
		if found := attributionEvents(t, ctx, pool, issue); len(found) != 0 {
			t.Errorf("%s carries %+v, want no event", issue, found)
		}
	}
	if queued := queuedMoveCount(t, ctx, pool); queued != 0 {
		t.Errorf("after the append %d moves are queued, want 0", queued)
	}
}

// A pass writes one answer to both rows of a pull request with no facts: the stored issue of its
// results row. #200 has a results row read from GitHub that stores ATTR-2, and a timeline row that
// names ATTR-1. #201 has an unread results row that stores ATTR-3, and a timeline row that names
// ATTR-1. After one pass both rows of #200 name ATTR-2, and both rows of #201 name ATTR-3. The
// results rows do not change, so no move is queued. A second sync writes nothing. It fails where
// the sync writes no timeline row, or gives the timeline's issue to a pull request with a results
// row, read or unread.
func TestAPassWritesOneAnswerToBothRowsOfAPullRequestWithNoFacts(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-1", "ATTR-2", "ATTR-3"}, nil)
	timeline := &statementTimeline{issue: new("ATTR-1")}
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 200, branch: "main", title: "no key", stored: new("ATTR-2"), timeline: timeline})
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 201, branch: "main", title: "no key", stored: new("ATTR-3"), unread: true, timeline: timeline})

	synced, appended := attributionPass(t, ctx, pool, broker)
	if synced.Changes != 0 || synced.TimelineRows != 2 || appended != 0 {
		t.Errorf("the pass upserted %d moves, wrote %d timeline rows and appended %d events; want 0, 2 and 0", synced.Changes, synced.TimelineRows, appended)
	}
	for number, want := range map[int]string{200: "ATTR-2", 201: "ATTR-3"} {
		if stored := storedAttributionOf(t, ctx, pool, "acme/widgets", number); stored != want+" by none" {
			t.Errorf("the results row of #%d stores %s, want %s by none", number, stored, want)
		}
		if issue := timelineIssueOf(t, ctx, pool, "acme/widgets", number); issue != want {
			t.Errorf("the timeline row of #%d names %s, want %s: the results row's answer", number, issue, want)
		}
	}
	if synced := syncAttributions(t, ctx, pool); synced.Changes != 0 || synced.TimelineRows != 0 {
		t.Errorf("a second sync upserted %d moves and wrote %d timeline rows, want 0 and 0", synced.Changes, synced.TimelineRows)
	}
}

// A pass writes the rule's answer to the timeline row of a pull request with facts. #210 and #212
// store ATTR-4, as an older rule stored it, in both rows, and the branch of each names ATTR-5. The
// timeline row of #212 spells the repository as GitHub can, Acme/Widgets. After one pass the
// results rows and the timeline rows of both name ATTR-5, and each move has its two events. #211
// has a timeline row alone, which is its answer, so it keeps ATTR-4. It fails where the sync writes
// the results rows alone, joins the timeline by the exact name, or writes a timeline-only row.
func TestAPassWritesTheTimelineRowOfAPullRequestWithFacts(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-4", "ATTR-5"}, nil)
	older := "ATTR-4"
	for _, number := range []int{210, 212} {
		row := statementRow{repo: "acme/widgets", number: number, branch: "legion/ATTR-5", title: "no key", stored: &older, texts: readTexts("")}
		if number == 210 {
			row.timeline = &statementTimeline{issue: &older}
		}
		storeStatementRow(t, ctx, pool, row)
		seedStoredIssue(t, ctx, pool, "acme/widgets", number, &older, string(sourceTitleBody))
	}
	execDispatchChange(t, ctx, pool, "store the timeline row of #212 as GitHub spells its repository",
		`insert into delivery_pull_requests (repo, number, title, url, author, issue_key)
		 values ('Acme/Widgets', 212, 'no key', 'https://github.com/Acme/Widgets/pull/212', 'octocat', $1)`, older)
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 211, title: "no key", noHead: true, timeline: &statementTimeline{issue: &older}})

	synced, appended := attributionPass(t, ctx, pool, broker)
	if synced.Changes != 2 || synced.TimelineRows != 2 || appended != 4 {
		t.Errorf("the pass upserted %d moves, wrote %d timeline rows and appended %d events; want 2, 2 and 4", synced.Changes, synced.TimelineRows, appended)
	}
	for _, number := range []int{210, 212} {
		if stored := storedAttributionOf(t, ctx, pool, "acme/widgets", number); stored != "ATTR-5 by branch" {
			t.Errorf("the results row of #%d stores %s, want ATTR-5 by branch", number, stored)
		}
	}
	for name, want := range map[PullRequestKey]string{
		{Repo: "acme/widgets", Number: 210}: "ATTR-5",
		{Repo: "Acme/Widgets", Number: 212}: "ATTR-5",
		{Repo: "acme/widgets", Number: 211}: "ATTR-4",
	} {
		if issue := timelineIssueOf(t, ctx, pool, name.Repo, name.Number); issue != want {
			t.Errorf("the timeline row of %s#%d names %s, want %s", name.Repo, name.Number, issue, want)
		}
	}
	for _, issue := range []string{"ATTR-4", "ATTR-5"} {
		if found, numbers := attributionEventCount(t, ctx, pool, issue); found != 2 || numbers != 2 {
			t.Errorf("%s carries %d events of %d pull requests, want 2 of 2: the moves of #210 and #212", issue, found, numbers)
		}
	}
}

// A pass owes no read for its write of the issue, and keeps the mark of the row. #220 was read from
// GitHub, stores ATTR-6, and its branch names ATTR-7. After one pass it stores ATTR-7. No read is
// owed in pull_request_reads_owed, and github_read_at and checked_at are as before. That is the
// narrowed trigger of migration 20261117111040. Control in the same test: a write of the title
// owes a read and takes the mark, so the checks can fail. It fails where the sync writes a fact of
// the row, or where the trigger counts issue_key or attribution_source as a fact.
func TestAPassOwesNoReadAndKeepsTheMark(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	broker := events.NewBroker()
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-6", "ATTR-7"}, nil)
	older := "ATTR-6"
	storeStatementRow(t, ctx, pool, statementRow{repo: "acme/widgets", number: 220, branch: "legion/ATTR-7", title: "no key", stored: &older, texts: readTexts("")})
	marks := func() (readAt, checkedAt *time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select github_read_at, checked_at from pull_request_heads where repo = 'acme/widgets' and number = 220`).Scan(&readAt, &checkedAt); err != nil {
			t.Fatalf("read the marks of #220: %v", err)
		}
		return readAt, checkedAt
	}
	readBefore, checkedBefore := marks()
	if readBefore == nil || checkedBefore == nil {
		t.Fatalf("the fixture's row has github_read_at %v and checked_at %v, want both set", readBefore, checkedBefore)
	}

	attributionPass(t, ctx, pool, broker)
	if stored := storedAttributionOf(t, ctx, pool, "acme/widgets", 220); stored != "ATTR-7 by branch" {
		t.Fatalf("after one pass #220 stores %s, want ATTR-7 by branch: the sync wrote no issue", stored)
	}
	if owed := owedReadsOf(t, ctx, pool, "acme/widgets", 220); owed != 0 {
		t.Errorf("the sync's write of the issue owes %d reads of #220, want 0", owed)
	}
	if readAfter, checkedAfter := marks(); readAfter == nil || !readAfter.Equal(*readBefore) || checkedAfter == nil || !checkedAfter.Equal(*checkedBefore) {
		t.Errorf("after the sync #220 has github_read_at %v and checked_at %v, want %v and %v as before", readAfter, checkedAfter, readBefore, checkedBefore)
	}

	execDispatchChange(t, ctx, pool, "the control: retitle #220", `update pull_request_heads set title = 'retitled' where repo = 'acme/widgets' and number = 220`)
	if owed := owedReadsOf(t, ctx, pool, "acme/widgets", 220); owed != 1 {
		t.Errorf("the control: a write of the title owes %d reads of #220, want 1", owed)
	}
	if readAt, _ := marks(); readAt != nil {
		t.Errorf("the control: after a write of the title #220 has github_read_at %v, want null", readAt)
	}
}
