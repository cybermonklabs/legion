package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// readAttributionCounts reads GET /api/v1/delivery/attribution as a person. It fails the test
// unless the route answers 200.
func readAttributionCounts(t *testing.T, handler http.Handler) delivery.AttributionCounts {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/attribution", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/delivery/attribution: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[delivery.AttributionCounts](t, response)
}

// storeAttributionRow stores acme/widgets#number, merged on branch, through the writer of a read of
// GitHub, with texts that are current. Its title and its body name no issue. messagesRead false
// stores the texts with the commit messages not read.
func storeAttributionRow(t *testing.T, database *store.Store, number int, branch string, messagesRead bool) {
	t.Helper()
	at := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	texts := &delivery.PullRequestTexts{HeadRepo: "acme/widgets", BaseRef: "main", Title: "A change", Body: ""}
	if messagesRead {
		texts.CommitMessages, texts.MessagesRead = []string{}, true
	}
	if err := delivery.UpsertPullRequestHead(context.Background(), database.Pool, delivery.PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: fmt.Sprintf("%040x", number), HeadRef: branch, BaseRef: "main",
		State: "merged", Title: "A change", URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		EventAt: &at, Texts: texts,
	}); err != nil {
		t.Fatalf("store acme/widgets#%d with its texts: %v", number, err)
	}
}

// putTextsOnReading puts the texts row of acme/widgets#number on reading, with the trigger off, as
// a row that a release of another reading stored stands.
func putTextsOnReading(t *testing.T, database *store.Store, number int, reading string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `update pull_request_texts set reading = $2 where repo = 'acme/widgets' and number = $1`, number, reading); err != nil {
		t.Fatalf("put the texts of acme/widgets#%d on reading %q: %v", number, reading, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// The count route of the attribution answers on a server with no delivery settings and no App
// key. The test reads GET /api/v1/delivery/attribution after each step of its fixture:
//
//   - a row that GitHub proved absent is in no count;
//   - a refused row with no texts is in facts_left, reads_left and facts_refused;
//   - a row with current texts, messages not read and a branch that names a held issue is in
//     reads_left only;
//   - a texts row on another reading is in keys_on_another_reading;
//   - a queued move is in moves_queued;
//   - two issues that hold one URL in two letter cases make one tie, and one issue makes none.
//
// reading is attribution_reading() at each step. The test fails where the route needs delivery
// settings or an App key, or where a count reads the wrong rows. Each step whose name starts with
// control shows that the count of the step after it can fail.
func TestTheAttributionCountsNeedNoDeliverySettings(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if deps.GitHub != nil {
		t.Fatal("control_no_app_key: the test server holds an App key, want none")
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/measures", nil, "alice"); response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), `"code":"DELIVERY_NOT_CONFIGURED"`) {
		t.Fatalf("control_no_delivery_settings: GET /api/v1/delivery/measures answers %d %s, want 404 DELIVERY_NOT_CONFIGURED",
			response.Code, response.Body.String())
	}
	var reading string
	if err := database.Pool.QueryRow(ctx, `select attribution_reading()`).Scan(&reading); err != nil {
		t.Fatalf("read attribution_reading(): %v", err)
	}
	if reading == "" {
		t.Fatal("attribution_reading() answers an empty text, so the reading of the route proves nothing")
	}
	exec := func(step, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	want := delivery.AttributionCounts{Reading: reading}
	check := func(step string) {
		t.Helper()
		if got := readAttributionCounts(t, handler); got != want {
			t.Errorf("%s: the route counts %+v, want %+v", step, got, want)
		}
	}
	check("an_empty_database")

	held := issueInRepository(t, handler, "ATTR", "acme/widgets", "Held").Key
	newIssue := func(title string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": "ATTR", "title": title, "spec": "A spec\n"}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %q of ATTR: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).Key
	}

	// #1 has no texts. Then GitHub proves that #1 does not exist.
	writeReadMergedRow(t, database, "acme/widgets", 1, fmt.Sprintf("%040x", 1), "")
	want.FactsLeft, want.ReadsLeft = 1, 1
	check("control_a_row_with_no_texts_before_its_proof_of_absence")
	exec("prove #1 absent", `update pull_request_heads set attribution_absent_at = clock_timestamp() where repo = 'acme/widgets' and number = 1`)
	want.FactsLeft, want.ReadsLeft = 0, 0
	check("a_row_proven_absent")

	writeReadMergedRow(t, database, "acme/widgets", 2, fmt.Sprintf("%040x", 2), "")
	exec("refuse #2", `update pull_request_heads set attribution_refused_at = clock_timestamp() where repo = 'acme/widgets' and number = 2`)
	want.FactsLeft, want.ReadsLeft, want.FactsRefused = 1, 1, 1
	check("a_refused_row_with_no_texts")

	// The branch of #3 names a held issue, so its messages that are not read decide nothing.
	storeAttributionRow(t, database, 3, "legion/"+held, false)
	want.ReadsLeft = 2
	check("a_branch_that_names_a_held_issue_and_messages_not_read")
	// No source of #4 above its messages names a held issue, so the answer of #4 is not known.
	storeAttributionRow(t, database, 4, "work/none", false)
	want.FactsLeft, want.ReadsLeft = 2, 3
	check("control_a_branch_that_names_no_held_issue_and_messages_not_read")

	storeAttributionRow(t, database, 5, "work/none", true)
	check("control_texts_with_messages_read_on_the_reading_that_runs")
	putTextsOnReading(t, database, 5, "old")
	want.KeysOnAnotherReading = 1
	check("texts_on_another_reading")

	exec("queue a move of #3", `
		insert into pull_request_attribution_moves (repo, number, from_key, to_key, source)
		values ('acme/widgets', 3, null, $1, 'branch')`, held)
	want.MovesQueued = 1
	check("a_queued_move")

	// The PATCH of an issue refuses a URL that another issue holds in another case. So a tie
	// stands only where a release before stored it, and the test stores the links by SQL.
	second, third := newIssue("Second"), newIssue("Third")
	exec("link one issue to one URL in two cases", `
		insert into issue_external_links (issue_key, url, kind)
		values ($1, 'https://example.com/Design-Note', 'url'), ($1, 'https://example.com/design-note', 'url')`, second)
	check("control_one_issue_holds_a_url_in_two_cases")
	exec("link two issues to one URL in two cases", `
		insert into issue_external_links (issue_key, url, kind)
		values ($1, 'https://example.com/PLAN', 'url'), ($2, 'https://example.com/plan', 'url')`, third, held)
	want.LinksInTwoCases = 1
	check("two_issues_hold_one_url_in_two_cases")
}

// The count route names the pull requests whose head is not read while a read is owed: the
// failures, which GitHub did not answer, and the refusals, which GitHub refuses. The control comes
// first: with nothing owed, both counts are 0, and the body holds neither time. Then
// acme/widgets#61 is owed a failure since 2 hours ago. acme/widgets#62 is owed a refusal, first
// refused 3 hours ago and owed again 1 hour ago. So a count of the refusals that reads owed_at
// in place of refused_since fails. Want owed_failures 1 since the owe of #61, and owed_refusals 1
// since the first refusal of #62.
func TestTheAttributionCountsNameTheReadsThatAreOwed(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	read := func(step string) (delivery.AttributionCounts, map[string]json.RawMessage) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/attribution", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/delivery/attribution: status=%d body=%s", step, response.Code, response.Body.String())
		}
		t.Logf("%s: %s", step, response.Body.String())
		var members map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
			t.Fatalf("%s: decode the members of %s: %v", step, response.Body.String(), err)
		}
		return decodeBody[delivery.AttributionCounts](t, response), members
	}

	counts, members := read("control_nothing_owed")
	for _, member := range []string{"owed_failures", "owed_refusals"} {
		if got := string(members[member]); got != "0" {
			t.Errorf("control_nothing_owed: %s is %q, want 0", member, got)
		}
	}
	for _, member := range []string{"owed_failures_oldest_at", "owed_refusals_since"} {
		if got, present := members[member]; present {
			t.Errorf("control_nothing_owed: the body holds %s %s, want no such member", member, got)
		}
	}
	if counts.OwedFailuresOldestAt != nil || counts.OwedRefusalsSince != nil {
		t.Errorf("control_nothing_owed: the times are %v and %v, want none", counts.OwedFailuresOldestAt, counts.OwedRefusalsSince)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	failureOwed, refusalOwed, firstRefused := now.Add(-2*time.Hour), now.Add(-time.Hour), now.Add(-3*time.Hour)
	if _, err := database.Pool.Exec(ctx, `
		insert into pull_request_reads_owed (repo, number, reason, owed_at, refused_as, refused_since)
		values ('acme/widgets', 61, 'failure', $1, null, null),
		       ('acme/widgets', 62, 'refusal', $2, $3, $4)
	`, failureOwed, refusalOwed, model.RefusedAsAppNotInstalled, firstRefused); err != nil {
		t.Fatalf("owe a failure of #61 and a refusal of #62: %v", err)
	}
	counts, _ = read("one_failure_and_one_refusal_owed")
	if counts.OwedFailures != 1 || counts.OwedFailuresOldestAt == nil || !counts.OwedFailuresOldestAt.Equal(failureOwed) {
		t.Errorf("owed_failures is %d since %v, want 1 since %s, the owe of #61", counts.OwedFailures, counts.OwedFailuresOldestAt, failureOwed.Format(time.RFC3339Nano))
	}
	if counts.OwedRefusals != 1 || counts.OwedRefusalsSince == nil || !counts.OwedRefusalsSince.Equal(firstRefused) {
		t.Errorf("owed_refusals is %d since %v, want 1 since %s, the first refusal of #62 and not its last owe at %s",
			counts.OwedRefusals, counts.OwedRefusalsSince, firstRefused.Format(time.RFC3339Nano), refusalOwed.Format(time.RFC3339Nano))
	}
}
