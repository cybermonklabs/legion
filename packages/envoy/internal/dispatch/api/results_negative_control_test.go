package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// negativeControlChecks is a Verification document holding a check of each `when`: nc-default
// names none and so runs before the merge, the schema's default; nc-before names before_merge;
// nc-after runs after the merge and nc-trunk on the default branch.
const negativeControlChecks = ":::check{#nc-default surface=\"cli\" run=\"go test ./...\"}\n" +
	"A pass states the negative control that would have caught its failure.\n" +
	":::\n\n" +
	":::check{#nc-before when=\"before_merge\" surface=\"cli\" run=\"go test ./...\"}\n" +
	"A pre-merge check that says when it runs.\n" +
	":::\n\n" +
	":::check{#nc-after when=\"after_merge\"}\n" +
	"Production answers at the merged commit.\n" +
	":::\n\n" +
	":::check{#nc-trunk when=\"default_branch\"}\n" +
	"The trunk answers at the merged commit.\n" +
	":::\n"

// statedNegativeControl is a negative control a result states.
const statedNegativeControl = "Removed the refusal from recordCheckResult and the test read 201."

// negativeControlInput is what a result's input carries as its negative control: nothing at all
// when sent is false, value when it is true.
type negativeControlInput struct {
	name  string
	sent  bool
	value string
}

// emptyNegativeControls are the negative controls a pass of a before_merge check is refused for:
// the field absent, the empty string, and whitespace alone.
var emptyNegativeControls = []negativeControlInput{
	{name: "no negative_control", sent: false},
	{name: "an empty negative_control", sent: true, value: ""},
	{name: "a negative_control of whitespace alone", sent: true, value: " \t\n "},
}

// negativeControlBody is resultBody for one check and verdict at headCommit, carrying control
// as its negative control.
func negativeControlBody(check, verdict string, control negativeControlInput, fields map[string]any) map[string]any {
	body := resultBody(check, verdict, headCommit, fields)
	delete(body, "negative_control")
	if control.sent {
		body["negative_control"] = control.value
	}
	return body
}

// checkResultRowsOf counts the check_results rows recorded against one check block.
func checkResultRowsOf(t *testing.T, database *store.Store, blockID string) int {
	t.Helper()
	return int(countRows(t, database, `select count(*) from check_results where check_block_id = $1`, blockID))
}

// storedCheckResultRow is one check_results row as stored, every column and the row version
// (xmin) beside it, so a test can tell that nothing rewrote the row.
func storedCheckResultRow(t *testing.T, database *store.Store, id string) string {
	t.Helper()
	var row string
	if err := database.Pool.QueryRow(context.Background(),
		`select r.xmin::text || ' ' || row_to_json(r)::text from check_results r where r.id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("read the stored result %s: %v", id, err)
	}
	return row
}

// A passing before_merge result states the negative control that would have caught its failure.
// A pass of a before_merge check whose input carries an empty negative_control is refused 400
// NEGATIVE_CONTROL_REQUIRED, and so is one that is whitespace alone or absent; the error states the
// field and the check's block id, and nothing is stored. The same call with a non-empty negative
// control is recorded, status 201. A pass on an after_merge or default_branch check, and a fail on
// a before_merge check, are each recorded with the field empty. A row stored before this rule,
// whose negative_control is the empty string the column defaults to, still reads back with its
// verdict, its commit and its role, its check still lists it, and no row is rewritten.
func TestResultRefusesABeforeMergePassWithNoNegativeControl(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "RESNC", "Negative controls", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", negativeControlChecks)

	// A pass of the pre-merge check stored before the rule, by SQL: its negative_control is the
	// column's default, the empty string.
	var version int
	var digest string
	if err := database.Pool.QueryRow(ctx, `
		select d.changed_version, d.text_sha256 from document_blocks d
		where `+refs.DocumentRoomBlocksSQL("d", "$1::uuid")+` and d.block_id = 'nc-default' and d.type = 'check'
	`, verification.Artifact.ID).Scan(&version, &digest); err != nil {
		t.Fatalf("read the indexed check nc-default: %v", err)
	}
	var storedID string
	if err := database.Pool.QueryRow(ctx, `
		insert into check_results (
			issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256,
			role, commit_sha, verdict, actor
		)
		values ($1, $1, $2::uuid, 'nc-default', $3, $4, 'tester', $5, 'pass', '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue.Key, verification.Artifact.ID, version, digest, mergeCommit).Scan(&storedID); err != nil {
		t.Fatalf("store a pass with no negative control as the release before this rule stored one: %v", err)
	}
	storedBefore := storedCheckResultRow(t, database, storedID)

	for _, check := range []string{"nc-default", "nc-before"} {
		rowsBefore := checkResultRowsOf(t, database, check)
		for _, control := range emptyNegativeControls {
			refused := recordResult(t, handler, issue.Key, negativeControlBody(check, "pass", control, nil))
			if refused.Code != http.StatusBadRequest {
				t.Fatalf("a pass of before_merge check %s with %s: status=%d body=%s, want 400 %s",
					check, control.name, refused.Code, refused.Body.String(), codeNegativeControlRequired)
			}
			code, message := refusalOf(t, refused)
			if code != codeNegativeControlRequired {
				t.Fatalf("a pass of before_merge check %s with %s: code=%s body=%s, want %s",
					check, control.name, code, refused.Body.String(), codeNegativeControlRequired)
			}
			if !strings.Contains(message, "negative_control") || !strings.Contains(message, check) {
				t.Fatalf("the refusal of a pass of %s with %s says %q, want it to name the field negative_control and the check %s",
					check, control.name, message, check)
			}
		}
		if rows := checkResultRowsOf(t, database, check); rows != rowsBefore {
			t.Fatalf("the refused passes of %s took its rows from %d to %d, want none stored", check, rowsBefore, rows)
		}

		stated := recordResult(t, handler, issue.Key,
			negativeControlBody(check, "pass", negativeControlInput{sent: true, value: statedNegativeControl}, nil))
		if stated.Code != http.StatusCreated {
			t.Fatalf("a pass of before_merge check %s stating its negative control: status=%d body=%s, want 201",
				check, stated.Code, stated.Body.String())
		}
		if recorded := decodeBody[recordedResult](t, stated).Result; recorded.NegativeControl != statedNegativeControl {
			t.Fatalf("the recorded pass of %s holds negative_control %q, want %q", check, recorded.NegativeControl, statedNegativeControl)
		}
	}
	// Only the two passes that stated a negative control appended an event.
	if events := resultEventsOn(t, database, issue.Key); events != 2 {
		t.Fatalf("%s holds %d result.recorded events, want the two of the passes that stated a negative control", issue.Key, events)
	}

	for _, row := range []struct {
		what    string
		check   string
		verdict string
		fields  map[string]any
	}{
		{what: "a pass of the after_merge check", check: "nc-after", verdict: "pass"},
		{what: "a pass of the default_branch check", check: "nc-trunk", verdict: "pass"},
		{what: "a fail of the check that runs before the merge by default", check: "nc-default", verdict: "fail", fields: map[string]any{"failed": 1}},
		{what: "a fail of the check that names before_merge", check: "nc-before", verdict: "fail", fields: map[string]any{"failed": 1}},
	} {
		for _, control := range emptyNegativeControls {
			recorded := recordResult(t, handler, issue.Key, negativeControlBody(row.check, row.verdict, control, row.fields))
			if recorded.Code != http.StatusCreated {
				t.Fatalf("%s with %s: status=%d body=%s, want 201", row.what, control.name, recorded.Code, recorded.Body.String())
			}
			if result := decodeBody[recordedResult](t, recorded).Result; result.Verdict != row.verdict ||
				result.NegativeControl != control.value {
				t.Fatalf("%s with %s was recorded as verdict %q, negative_control %q; want %q and %q",
					row.what, control.name, result.Verdict, result.NegativeControl, row.verdict, control.value)
			}
		}
	}

	// The row stored before the rule reads back under its check with its verdict, its commit and
	// its role, and nothing - neither the read nor any record since - rewrote it.
	var stored *model.CheckResult
	results := checkOf(t, readResults(t, handler, issue.Key), "nc-default").Results
	for index := range results {
		if results[index].ID == storedID {
			stored = &results[index]
		}
	}
	if stored == nil {
		t.Fatalf("nc-default's results = %+v, want the pass stored before the rule among them", results)
	}
	if stored.Verdict != "pass" || stored.CommitSHA != mergeCommit || stored.Role != "tester" || stored.NegativeControl != "" {
		t.Fatalf("the pass stored before the rule reads back as %+v, want a pass at %s by tester with no negative control",
			*stored, mergeCommit)
	}
	if after := storedCheckResultRow(t, database, storedID); after != storedBefore {
		t.Fatalf("the pass stored before the rule was rewritten:\nbefore %s\nafter  %s", storedBefore, after)
	}
}

// A closed issue answers 409 ISSUE_CLOSED first: the refusal of a pass that states no negative
// control sits after the closed-issue refusal in recordCheckResult, so a pass of a before_merge
// check on a closed issue is refused ISSUE_CLOSED whatever its negative_control holds, and
// nothing is stored. The closed issue still takes a pass of its after_merge and default_branch
// checks with the field empty.
func TestAClosedIssueRefusesABeforeMergeResultFirst(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RESNX", "Closed negative controls", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", negativeControlChecks)
	if closed := dispatchRequest(t, handler, http.MethodPatch, owner, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}

	controls := append(append([]negativeControlInput{}, emptyNegativeControls...),
		negativeControlInput{name: "a stated negative_control", sent: true, value: statedNegativeControl})
	for _, check := range []string{"nc-default", "nc-before"} {
		for _, control := range controls {
			refused := recordResult(t, handler, issue.Key, negativeControlBody(check, "pass", control, nil))
			if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
				t.Fatalf("a pass of before_merge check %s with %s on a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED first",
					check, control.name, refused.Code, refused.Body.String())
			}
		}
		if rows := checkResultRowsOf(t, database, check); rows != 0 {
			t.Fatalf("the refused passes of %s on a closed issue stored %d rows, want none", check, rows)
		}
	}
	if events := resultEventsOn(t, database, issue.Key); events != 0 {
		t.Fatalf("%s holds %d result.recorded events after refusals alone, want none", issue.Key, events)
	}

	for _, check := range []string{"nc-after", "nc-trunk"} {
		recorded := recordResult(t, handler, issue.Key, negativeControlBody(check, "pass", emptyNegativeControls[0], nil))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("a pass of %s with no negative_control on a closed issue: status=%d body=%s, want 201",
				check, recorded.Code, recorded.Body.String())
		}
	}
}
