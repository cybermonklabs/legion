package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The near_copy_checks record: one row for each answer the near-copy gate gives a create, read
// back through GET /api/v1/near-copy-checks. Its fixtures are the gate tests' own, which score at
// the extremes or between the two bars wherever those bars stand.

func nearCopyChecksOf(t *testing.T, handler http.Handler, query url.Values) model.NearCopyChecksPage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/near-copy-checks?"+query.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("near-copy checks %v: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return decodeBody[model.NearCopyChecksPage](t, response)
}

func nearCopyCheckOf(t *testing.T, handler http.Handler, id string) model.NearCopyCheck {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/near-copy-checks/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("near-copy check %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[model.NearCopyCheck](t, response)
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Every state of the gate leaves its row: the refusal, which writes nothing else anywhere, keeps
// the text it refused and what it named; each create that went in names its issue.
func TestNearCopyChecksRecordEveryAnswerOfTheGate(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	title := "Astrolabe calibration drifts after a reseat"

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))
	requireDuplicateRefusal(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))
	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe, "force": true,
	}))
	plain := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))

	page := nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}})
	if page.Project != "TEST" || page.NextAfter != nil {
		t.Fatalf("page project = %q, next_after = %v", page.Project, page.NextAfter)
	}
	type want struct {
		outcome string
		issue   string
		force   bool
	}
	var got []want
	for _, check := range page.Checks {
		issue := ""
		if check.Issue != nil {
			issue = *check.Issue
		}
		got = append(got, want{check.Outcome, issue, check.Force})
	}
	expected := []want{
		{nearCopyPassed, first.Key, false},
		{nearCopyRefused, "", false},
		{nearCopyForced, forced.Key, true},
		{nearCopyPassed, plain.Key, false},
	}
	if len(got) != len(expected) {
		t.Fatalf("rows = %+v, want %+v", got, expected)
	}
	for index := range expected {
		if got[index] != expected[index] {
			t.Fatalf("row %d = %+v, want %+v (all rows %+v)", index, got[index], expected[index], got)
		}
	}

	refused := page.Checks[1]
	if refused.Spec != nil || refused.Title != title {
		t.Fatalf("refused row on the page = title %q, spec %v, want its title and no spec", refused.Title, refused.Spec)
	}
	if refused.SpecSHA256 != sha256Hex(duplicateSpecAstrolabe) || refused.Reason != nil || refused.Parent != nil {
		t.Fatalf("refused row = %+v", refused)
	}
	if one := nearCopyCheckOf(t, handler, refused.ID); one.Spec == nil || *one.Spec != duplicateSpecAstrolabe ||
		one.ID != refused.ID || one.Outcome != nearCopyRefused {
		t.Fatalf("refused row read by id = %+v, want it with the spec it refused", one)
	}
	if one := nearCopyCheckOf(t, handler, page.Checks[3].ID); one.Spec != nil || one.Issue == nil || *one.Issue != plain.Key {
		t.Fatalf("passed row read by id = %+v, want %s and no spec, which its issue holds", one, plain.Key)
	}
	if len(refused.Candidates) != 1 || refused.Candidates[0].Key != first.Key ||
		refused.Candidates[0].Title != title || refused.Candidates[0].Score < refused.RefuseScore {
		t.Fatalf("refused row's candidates = %+v, want %s at or above %v", refused.Candidates, first.Key, refused.RefuseScore)
	}
	if refused.RefuseScore != duplicateRefuseScore || refused.NudgeScore != duplicateNudgeScore {
		t.Fatalf("refused row's bars = %v and %v", refused.RefuseScore, refused.NudgeScore)
	}
	if refused.ActorKind == "" || refused.ActorID == "" {
		t.Fatalf("refused row names no actor: %+v", refused)
	}
	forcedRow := page.Checks[2]
	if forcedRow.Spec != nil || forcedRow.SpecSHA256 != sha256Hex(duplicateSpecAstrolabe) ||
		len(forcedRow.Candidates) == 0 || forcedRow.Candidates[0].Key != first.Key {
		t.Fatalf("forced row = %+v, want no spec and the issue it went past", forcedRow)
	}
	if plainRow := page.Checks[3]; len(plainRow.Candidates) != 0 || plainRow.Spec != nil {
		t.Fatalf("passed row of a text sharing nothing = %+v", plainRow)
	}
}

// A nudge is recorded as one, with the closest issue below the refusal bar.
func TestNearCopyChecksRecordANudge(t *testing.T) {
	handler := newTestHandler(t)
	fixture := newDuplicateVocabulary(t, handler, "NUDGE", (duplicateNudgeScore+duplicateRefuseScore)/2)

	nudged := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": fixture.project, "title": fixture.title, "spec": fixture.spec,
	}))
	page := nearCopyChecksOf(t, handler, url.Values{"project": {fixture.project}})
	if len(page.Checks) == 0 {
		t.Fatalf("project %s holds no near-copy check rows, want the nudged create's last", fixture.project)
	}
	last := page.Checks[len(page.Checks)-1]
	if last.Outcome != nearCopyNudged || last.Issue == nil || *last.Issue != nudged.Key {
		t.Fatalf("last row = %+v, want %s nudged", last, nudged.Key)
	}
	if len(last.Candidates) == 0 || last.Candidates[0].Key != fixture.candidate.Key ||
		last.Candidates[0].Score < duplicateNudgeScore || last.Candidates[0].Score >= duplicateRefuseScore {
		t.Fatalf("nudged row's candidates = %+v, want %s between the bars", last.Candidates, fixture.candidate.Key)
	}
}

// A create whose check did not answer is recorded as not made, with why; a create naming a
// GitHub issue runs no gate and leaves no row.
func TestNearCopyChecksRecordACheckNotMadeAndNoneForAnExternalCreate(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))
	mapping := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice")
	if mapping.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapping.Code, mapping.Body.String())
	}
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"external": "owner/repo#41", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe,
	}))

	served.duplicateBound = 200 * time.Millisecond
	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	unchecked := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration drifts after a reseat",
		"spec": duplicateSpecAstrolabe, "force": true,
	}))
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release search_term_stats: %v", err)
	}

	page := nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}})
	if len(page.Checks) != 2 {
		t.Fatalf("rows = %+v, want the first create's and the unchecked one's, none for the external create", page.Checks)
	}
	row := page.Checks[1]
	if row.Outcome != nearCopyNotMade || row.Reason == nil || *row.Reason != nearCopyNotMadeBound ||
		!row.Force || row.Issue == nil || *row.Issue != unchecked.Key || len(row.Candidates) != 0 {
		t.Fatalf("unchecked row = %+v", row)
	}
}

// createWaitsForNearCopyChecks reports whether a create's transaction waits for a lock on this
// test's near_copy_checks: a backend that holds the lock its issue insert took on issues and waits
// on near_copy_checks. A refusal's write runs outside any create transaction and holds no lock on
// issues, so it is never this backend.
func createWaitsForNearCopyChecks(t *testing.T, database *store.Store) bool {
	t.Helper()
	var waiting bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (
		  select 1 from pg_locks l
		   where not l.granted and l.relation = 'near_copy_checks'::regclass
		     and l.database = (select oid from pg_database where datname = current_database())
		     and exists (select 1 from pg_locks i
		                  where i.pid = l.pid and i.granted and i.relation = 'issues'::regclass))`,
	).Scan(&waiting); err != nil {
		t.Fatalf("read the waiters on near_copy_checks: %v", err)
	}
	return waiting
}

// A refusal is answered only once its row is written, so a count of refused rows misses no 409.
// While near_copy_checks cannot take a row, a near copy that would be refused goes in instead,
// unchecked: its answer names the issue the refusal would have named, and its row, written in the
// create's own transaction, waits for the table and commits with the create.
func TestNearCopyChecksARefusalThatCannotBeRecordedRefusesNothing(t *testing.T) {
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")
	title := "Astrolabe calibration drifts after a reseat"
	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table near_copy_checks in access exclusive mode"); err != nil {
		t.Fatalf("lock near_copy_checks: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- createIssueRequest(t, handler, map[string]any{
			"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
		})
	}()
	// The refusal's write waits for the table until its bound ends it, and only then does the
	// create's transaction begin; the table is let go once that transaction's write waits, so the
	// refusal's write cannot have landed and the create's write is the one that lands.
	deadline := time.Now().Add(30 * time.Second)
	for !createWaitsForNearCopyChecks(t, database) {
		if time.Now().After(deadline) {
			t.Fatalf("the create's transaction did not wait for near_copy_checks in 30 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release near_copy_checks: %v", err)
	}
	response := <-answered

	created := requireCreatedIssue(t, response)
	suggestions := decodeBody[struct {
		Advice struct {
			Suggestions model.Suggestions `json:"suggestions"`
		} `json:"advice"`
	}](t, response).Advice.Suggestions
	if suggestions.NearCopyCheck != nearCopyCheckUnrecorded(model.DuplicateCandidate{Key: first.Key, Title: title}) {
		t.Fatalf("create %s's near_copy_check = %q, want the unrecorded refusal of %s", created.Key,
			suggestions.NearCopyCheck, first.Key)
	}
	if suggestions.LikelyDuplicate != nil || len(suggestions.TitleMatches) != 0 {
		t.Fatalf("an unchecked create named issues as a nudge or a force: %#v %#v",
			suggestions.LikelyDuplicate, suggestions.TitleMatches)
	}
	if _, ok := rawCreatedPayload(t, handler, created.Key)["forced"]; ok {
		t.Fatalf("an unforced create carries a forced record")
	}
	var rows []model.NearCopyCheck
	for _, row := range nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}}).Checks {
		if row.Outcome == nearCopyRefused {
			t.Fatalf("a refused row was written for a create that went in: %+v", row)
		}
		if row.Issue != nil && *row.Issue == created.Key {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 || rows[0].Outcome != nearCopyNotMade || rows[0].Reason == nil ||
		*rows[0].Reason != nearCopyNotMadeUnrecorded || len(rows[0].Candidates) == 0 ||
		rows[0].Candidates[0].Key != first.Key {
		t.Fatalf("rows of %s = %+v, want one, not made and unrecorded, naming %s", created.Key, rows, first.Key)
	}
}

// A refusal whose insert landed though the server never learned it did is not answered: the create
// goes in, and its row, written in its transaction under the same id, replaces the refused one. So
// a committed create never keeps a refused row. The refused row put in place under the create's
// own id stands for that insert, and the refusal's write, which errs, for an answer the server
// could not read (putUnseenRefusal).
func TestNearCopyChecksARefusalRowTheServerCannotConfirmGivesWayToTheCreate(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")
	title := "Astrolabe calibration drifts after a reseat"
	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))

	const id = "6f1e2d3c-4b5a-4978-8a1b-2c3d4e5f6a7b"
	putUnseenRefusal(t, database, id, title)
	served.nearCopyRowID = func() string { return id }

	created := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))

	var rows []model.NearCopyCheck
	for _, row := range nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}}).Checks {
		if row.Outcome == nearCopyRefused {
			t.Fatalf("a refused row stands beside the create it did not stop: %+v", row)
		}
		if row.ID == id {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("rows under %s = %+v, want the one create row that replaced the refusal", id, rows)
	}
	row := rows[0]
	if row.Outcome != nearCopyNotMade || row.Reason == nil || *row.Reason != nearCopyNotMadeUnrecorded ||
		row.Issue == nil || *row.Issue != created.Key || row.Spec != nil {
		t.Fatalf("row under %s = %+v, want %s not made, unrecorded, with no spec", id, row, created.Key)
	}
	if len(row.Candidates) == 0 || row.Candidates[0].Key != first.Key {
		t.Fatalf("row under %s names candidates %+v, want %s, the issue the refusal named", id, row.Candidates, first.Key)
	}
}

// putUnseenRefusal puts a refused row of title under id in project TEST and makes every later
// refusal's write err, as a refusal's insert that landed though the server never read its answer:
// the row stands, and the write the create waited on answers an error, not the row. A write that
// found the row and read it back as its own would rightly answer the 409.
func putUnseenRefusal(t *testing.T, database *store.Store, id, title string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into near_copy_checks
			(id, project_key, actor_kind, actor_id, outcome, force, title, spec, spec_sha256,
			 candidates, refuse_score, nudge_score)
		values ($1::uuid, 'TEST', 'user', 'alice', 'refused', false, $2, $3, $4, '[]', $5, $6)`,
		id, title, duplicateSpecAstrolabe, sha256Hex(duplicateSpecAstrolabe),
		duplicateRefuseScore, duplicateNudgeScore); err != nil {
		t.Fatalf("put the unseen refusal's row in place: %v", err)
	}
	for _, statement := range []string{
		`create function near_copy_checks_lose_refusal() returns trigger language plpgsql as $$
		begin raise exception 'the refusal''s answer was lost'; end $$`,
		`create trigger near_copy_checks_lose_refusal before insert on near_copy_checks
		  for each row when (new.outcome = 'refused') execute function near_copy_checks_lose_refusal()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make the refusal's write err: %v", err)
		}
	}
}

// The create let in past a refusal it could not confirm writes its row in its own transaction:
// when that row cannot replace the refused one, the create is not admitted at all, and the refused
// row stands beside no issue. So the refusal and the issue never both stand.
func TestNearCopyChecksACreateWhoseRowCannotReplaceTheRefusalIsNotAdmitted(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")
	title := "Astrolabe calibration drifts after a reseat"
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	}))

	const id = "7a2b3c4d-5e6f-4a1b-9c2d-3e4f5a6b7c8d"
	putUnseenRefusal(t, database, id, title)
	for _, statement := range []string{
		`create function near_copy_checks_keep_refusal() returns trigger language plpgsql as $$
		begin raise exception 'the refused row cannot be replaced'; end $$`,
		`create trigger near_copy_checks_keep_refusal before update on near_copy_checks
		  for each row execute function near_copy_checks_keep_refusal()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make the refused row's replacement fail: %v", err)
		}
	}
	served.nearCopyRowID = func() string { return id }

	response := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": title, "spec": duplicateSpecAstrolabe,
	})
	if response.Code == http.StatusCreated || response.Code == http.StatusConflict {
		t.Fatalf("create whose row could not replace the refusal: status=%d body=%s, want an error",
			response.Code, response.Body.String())
	}
	var issues int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from issues where project_key = 'TEST'`).Scan(&issues); err != nil {
		t.Fatalf("count TEST's issues: %v", err)
	}
	if issues != 1 {
		t.Fatalf("TEST holds %d issues, want only the first: the create was admitted", issues)
	}
	var outcome string
	var issue *string
	if err := database.Pool.QueryRow(context.Background(),
		`select outcome, issue_key from near_copy_checks where id = $1::uuid`, id).Scan(&outcome, &issue); err != nil {
		t.Fatalf("read the row under %s: %v", id, err)
	}
	if outcome != nearCopyRefused || issue != nil {
		t.Fatalf("row under %s = %s naming %v, want the refusal, naming no issue", id, outcome, issue)
	}
}

// The route pages by id, refuses a cursor that is no row of the project, and needs a project.
func TestNearCopyChecksPagesAndRefusesBadQueries(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	createDuplicateTestProject(t, handler, "OTHER")
	for _, title := range []string{"Astrolabe drifts", "Weighbridge loses power", "Orchard crates pile up"} {
		requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{"project": "TEST", "title": title}))
	}
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{"project": "OTHER", "title": "Gnomon reseated"}))

	all := nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}})
	if len(all.Checks) != 3 {
		t.Fatalf("TEST rows = %d, want 3", len(all.Checks))
	}
	first := nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}, "limit": {"2"}})
	if len(first.Checks) != 2 || first.NextAfter == nil || *first.NextAfter != all.Checks[1].ID {
		t.Fatalf("first page = %+v", first)
	}
	second := nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}, "limit": {"2"}, "after": {*first.NextAfter}})
	if len(second.Checks) != 1 || second.Checks[0].ID != all.Checks[2].ID || second.NextAfter != nil {
		t.Fatalf("second page = %+v", second)
	}
	future := nearCopyChecksOf(t, handler, url.Values{"project": {"TEST"}, "from": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}})
	if len(future.Checks) != 0 || future.From == nil {
		t.Fatalf("a window after every row = %+v", future)
	}

	other := nearCopyChecksOf(t, handler, url.Values{"project": {"OTHER"}})
	if len(other.Checks) != 1 {
		t.Fatalf("OTHER rows = %d, want 1", len(other.Checks))
	}
	for name, query := range map[string]url.Values{
		"no project":            {},
		"another project's id":  {"project": {"TEST"}, "after": {other.Checks[0].ID}},
		"an id that is no id":   {"project": {"TEST"}, "after": {"TEST-1"}},
		"an empty window":       {"project": {"TEST"}, "from": {"2026-11-15T00:00:00Z"}, "to": {"2026-11-15T00:00:00Z"}},
		"a limit past the most": {"project": {"TEST"}, "limit": {"1001"}},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/near-copy-checks?"+query.Encode(), nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", name, response.Code, response.Body.String())
		}
	}
	for id, want := range map[string]struct {
		status int
		code   string
	}{
		"6f1e2d3c-4b5a-4978-8a1b-2c3d4e5f6a7b": {http.StatusNotFound, "NEAR_COPY_CHECK_NOT_FOUND"},
		"TEST-1":                               {http.StatusBadRequest, "NEAR_COPY_CHECK_ID_INPUT"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/near-copy-checks/"+id, nil, "alice")
		if response.Code != want.status || !strings.Contains(response.Body.String(), `"code":"`+want.code+`"`) {
			t.Errorf("near-copy check %s: status=%d body=%s, want %d %s", id, response.Code, response.Body.String(), want.status, want.code)
		}
	}
}

// nearCopyOutcomesOf is the outcome of each near_copy_checks row that names issue, read from the
// table itself.
func nearCopyOutcomesOf(t *testing.T, database *store.Store, issue string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select outcome from near_copy_checks where issue_key = $1 order by created_at, id`, issue)
	if err != nil {
		t.Fatalf("read the rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			t.Fatalf("read a row of %s: %v", issue, err)
		}
		outcomes = append(outcomes, outcome)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows of %s: %v", issue, err)
	}
	return outcomes
}

// A create's row held up past its second, here by a lock on near_copy_checks such as a migration
// takes on a table the insert locks, is tried once more in the background: the create answers at
// once, and the row is written once the lock is released. The lock is held until the first
// attempt's insert is cancelled, so the row is the retry's.
func TestNearCopyChecksARowHeldUpPastItsSecondIsWrittenOnceTheLockIsReleased(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")

	lockTx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table near_copy_checks in access exclusive mode"); err != nil {
		t.Fatalf("lock near_copy_checks: %v", err)
	}
	poll := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 10s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- createIssueRequest(t, handler, map[string]any{
			"project": "TEST", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
		})
	}()
	// The first attempt is the backend whose insert waits on the lock before the create answers;
	// the retry's insert, which may run on the same connection, starts a second after it.
	var firstPID int
	var firstStart time.Time
	poll("the row's first attempt waiting on the lock", func() bool {
		err := database.Pool.QueryRow(context.Background(), `
			select pid, query_start from pg_stat_activity
			 where datname = current_database() and state = 'active' and pid <> pg_backend_pid()
			   and query like '%insert into near_copy_checks%'
			 order by query_start limit 1
		`).Scan(&firstPID, &firstStart)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatalf("read the waiting inserts: %v", err)
		}
		return true
	})
	created := requireCreatedIssue(t, <-answered)
	if got := served.relatedRecordRetrying.Load(); got != 1 {
		t.Fatalf("records being retried after the create answered = %d, want 1 (its row's first attempt held up by the lock)", got)
	}
	poll("the first attempt's insert cancelled", func() bool {
		var waiting bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select from pg_stat_activity
			 where pid = $1 and query_start = $2 and state = 'active')
		`, firstPID, firstStart).Scan(&waiting); err != nil {
			t.Fatalf("read the first attempt: %v", err)
		}
		return !waiting
	})
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release near_copy_checks: %v", err)
	}
	var outcomes []string
	poll("the create's row written after the lock's release", func() bool {
		outcomes = nearCopyOutcomesOf(t, database, created.Key)
		return len(outcomes) > 0
	})
	if len(outcomes) != 1 || outcomes[0] != nearCopyPassed {
		t.Fatalf("rows of %s = %v, want one, passed", created.Key, outcomes)
	}
}

// A write that finds a row under its id holding the very answer it writes - an earlier attempt that
// landed though nobody read its answer - has landed. Another answer under that id has not.
func TestNearCopyChecksAWriteThatFindsItsOwnRowHasLanded(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")
	row := nearCopyCheckRow{
		id: "8b3c4d5e-6f7a-4b2c-8d3e-4f5a6b7c8d9e", project: "TEST", actor: model.Actor{Kind: "user", ID: "alice"},
		title: "Orchard pickers cannot log a crate", spec: duplicateSpecHarvest, issue: "TEST-9",
	}
	passed := &nearCopyCheck{outcome: nearCopyPassed}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := served.writeNearCopyCheck(context.Background(), database.Pool, row, passed); err != nil {
			t.Fatalf("attempt %d of one write: %v", attempt, err)
		}
	}
	if err := served.writeNearCopyCheck(context.Background(), database.Pool, row, &nearCopyCheck{outcome: nearCopyNudged}); err == nil {
		t.Fatalf("a write of another outcome under %s answered that it landed", row.id)
	}
	other := row
	other.issue = "TEST-10"
	if err := served.writeNearCopyCheck(context.Background(), database.Pool, other, passed); err == nil {
		t.Fatalf("a write naming another issue under %s answered that it landed", row.id)
	}
	if outcomes := nearCopyOutcomesOf(t, database, "TEST-9"); len(outcomes) != 1 || outcomes[0] != nearCopyPassed {
		t.Fatalf("rows of TEST-9 = %v, want the one passed row", outcomes)
	}
	if outcomes := nearCopyOutcomesOf(t, database, "TEST-10"); len(outcomes) != 0 {
		t.Fatalf("rows of TEST-10 = %v, want none", outcomes)
	}
}

// A create's row that cannot be written for a reason no retry cures is logged and dropped: the
// create still answers 201 and stands, with no row and no retry.
func TestNearCopyChecksARowThatCannotBeWrittenIsLoggedAndTheCreateStands(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createDuplicateTestProject(t, handler, "TEST")
	for _, statement := range []string{
		`create function near_copy_checks_refuse_rows() returns trigger language plpgsql as $$
		begin raise exception 'near_copy_checks takes no row'; end $$`,
		`create trigger near_copy_checks_refuse_rows before insert on near_copy_checks
		  for each row execute function near_copy_checks_refuse_rows()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make near_copy_checks refuse every row: %v", err)
		}
	}
	logs := captureLogs(t)

	created := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}))
	if got := served.relatedRecordRetrying.Load(); got != 0 {
		t.Fatalf("records being retried = %d, want 0: a row the table refuses is not tried again", got)
	}
	if outcomes := nearCopyOutcomesOf(t, database, created.Key); len(outcomes) != 0 {
		t.Fatalf("rows of %s = %v, want none", created.Key, outcomes)
	}
	var issues int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from issues where key = $1`, created.Key).Scan(&issues); err != nil {
		t.Fatalf("count the issue %s: %v", created.Key, err)
	}
	if issues != 1 {
		t.Fatalf("issue %s stands %d times, want once", created.Key, issues)
	}
	if lines := logs.Lines("the near-copy check's record was not written"); !strings.Contains(lines, "issue="+created.Key) ||
		!strings.Contains(lines, "near_copy_checks takes no row") {
		t.Fatalf("log lines of the lost row = %q, want one naming %s and the table's refusal", lines, created.Key)
	}
}
