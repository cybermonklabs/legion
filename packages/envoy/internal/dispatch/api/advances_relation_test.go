// advances_relation_test.go drives the `advances` relation an issue states on a priority
// (issue_advances.go), on the database store/storetest's Open clones from this package's
// migrated template, through the handler the neighbouring tests drive
// (newTestHandlerWithStore, dispatchRequest).
//
// The relation crosses projects, so the issues here are of two of them. It is stated in a log
// nothing updates and nothing deletes, so a retraction is read as a row appended beside the one
// that stated it, and the history of one issue's relation holds both acts with their moments and
// their writers. The refusals are read by their code, and the last reading is that a write of
// this field alone moves no other field of the issue and says so in the event it writes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestAdvancesRelation(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createTestProject(t, handler, "CORE")
	createTestProject(t, handler, "OPS")
	priority := createTestPriority(t, handler, map[string]any{
		"id": "cut-the-queue", "title": "Cut the queue", "owner": "alice",
	}, "alice").ID

	// Work in two projects advances the one priority, which belongs to neither.
	core := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Daemon work", "advances": []string{priority},
	}, "alice")
	ops := createAdvancingIssue(t, handler, map[string]any{
		"project": "OPS", "title": "Runner work", "advances": []string{priority},
	}, "bob")
	for _, issue := range []model.Issue{core, ops} {
		if got := advanceEntries(issue.Advances); !slices.Equal(got, []string{priority + " own"}) {
			t.Fatalf("%s advances %v on the write's answer, want its own relation to %s", issue.Key, got, priority)
		}
		read := readTestIssue(t, handler, issue.Key)
		if got := advanceEntries(read.Advances); !slices.Equal(got, []string{priority + " own"}) {
			t.Fatalf("%s advances %v when read back, want its own relation to %s", issue.Key, got, priority)
		}
		if read.AdvancesRevision == 0 {
			t.Fatalf("%s answers no advances revision, want the log position of its newest relation", issue.Key)
		}
		if row := listedIssue(t, handler, "/api/v1/issues?project="+read.Project, read.Key); !slices.Equal(
			advanceEntries(row.Advances), []string{priority + " own"}) {
			t.Fatalf("%s's listing row advances %v, want its own relation to %s", issue.Key, advanceEntries(row.Advances), priority)
		}
	}
	if keys := listedIssueKeys(t, handler, "/api/v1/issues?advances="+priority); !slices.Equal(
		keys, []string{core.Key, ops.Key}) && !slices.Equal(keys, []string{ops.Key, core.Key}) {
		t.Fatalf("the issues advancing %s = %v, want both projects' work", priority, keys)
	}

	// A retraction is a row appended: the log grew by one, no row of it was updated or deleted,
	// and the issue no longer serves the priority.
	before := priorityTupleRows(t, database)
	retracted := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+ops.Key, map[string]any{
		"advances": []string{}, "advances_revision": readTestIssue(t, handler, ops.Key).AdvancesRevision,
	}, "bob")
	if retracted.Code != http.StatusOK {
		t.Fatalf("retract %s's relation: status=%d body=%s", ops.Key, retracted.Code, retracted.Body.String())
	}
	after := priorityTupleRows(t, database)
	if len(after) != len(before)+1 {
		t.Fatalf("the log holds %d rows after the retraction, want %d: a retraction appends one row", len(after), len(before)+1)
	}
	for seq, row := range before {
		switch held, there := after[seq]; {
		case !there:
			t.Fatalf("the retraction deleted the tuple at seq %d: %s", seq, row)
		case held != row:
			t.Fatalf("the retraction updated the tuple at seq %d: %s, was %s", seq, held, row)
		}
	}
	if got := advanceEntries(readTestIssue(t, handler, ops.Key).Advances); len(got) != 0 {
		t.Fatalf("%s advances %v after the retraction, want nothing", ops.Key, got)
	}
	if keys := listedIssueKeys(t, handler, "/api/v1/issues?advances="+priority); !slices.Equal(keys, []string{core.Key}) {
		t.Fatalf("the issues advancing %s = %v, want %s alone", priority, keys, core.Key)
	}
	if keys := listedIssueKeys(t, handler, "/api/v1/issues?project=OPS&advances=none"); !slices.Contains(keys, ops.Key) {
		t.Fatalf("the work advancing no priority = %v, want %s among it", keys, ops.Key)
	}

	// The history of that issue's relation: both acts, each with its moment and its writer.
	history, err := prioritystore.History(ctx, database.Pool, prioritystore.KindIssue, ops.Key, prioritystore.RelationAdvances)
	if err != nil {
		t.Fatalf("read %s's advances history: %v", ops.Key, err)
	}
	if len(history) != 2 {
		t.Fatalf("%s's advances history holds %d acts, want the statement and the retraction", ops.Key, len(history))
	}
	stated, ended := history[0], history[1]
	if !stated.Asserted || ended.Asserted {
		t.Fatalf("the history = asserted %v then %v, want the statement then the retraction", stated.Asserted, ended.Asserted)
	}
	for _, act := range history {
		if act.ToID != priority || act.ToKind != prioritystore.KindPriority || act.FromID != ops.Key {
			t.Fatalf("an act of the history names %s/%s → %s/%s, want %s advancing %s",
				act.FromKind, act.FromID, act.ToKind, act.ToID, ops.Key, priority)
		}
		if act.At.IsZero() {
			t.Fatalf("an act of the history carries no moment: %+v", act)
		}
	}
	if !ended.At.After(stated.At) && !ended.At.Equal(stated.At) {
		t.Fatalf("the retraction's moment %v is before the statement's %v", ended.At, stated.At)
	}
	if stated.Who != (model.Actor{Kind: "user", ID: "bob"}) || ended.Who != (model.Actor{Kind: "user", ID: "bob"}) {
		t.Fatalf("the acts were written by %+v and %+v, want the person who wrote them", stated.Who, ended.Who)
	}

	// Each refusal, by its code.
	goneID := createTestPriority(t, handler, map[string]any{"id": "ship-it-later", "title": "Ship it later"}, "alice").ID
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/"+goneID, nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete the priority: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	twentyOne := make([]string, 0, contracts.MaxIssueAdvances+1)
	for index := range contracts.MaxIssueAdvances + 1 {
		id := fmt.Sprintf("goal-%02d", index)
		twentyOne = append(twentyOne, createTestPriority(t, handler, map[string]any{
			"id": id, "title": fmt.Sprintf("Goal %02d", index),
		}, "alice").ID)
	}
	for name, body := range map[string]map[string]any{
		"a priority whose state is gone": {"project": "CORE", "title": "Under a dead goal", "advances": []string{goneID}},
		"an id no priority carries":      {"project": "CORE", "title": "Under no goal", "advances": []string{"no-such-priority"}},
		"more priorities than one issue holds": {
			"project": "CORE", "title": "Under every goal", "advances": twentyOne,
		},
	} {
		body["force"] = true
		refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if code, text := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != codeAdvancesInput {
			t.Fatalf("a write naming %s: status=%d code=%s error=%q, want 400 %s",
				name, refused.Code, code, text, codeAdvancesInput)
		}
	}
	// The cap is the issue's own relations: the twenty under it are written.
	capped := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Under twenty goals", "force": true,
		"advances": twentyOne[:contracts.MaxIssueAdvances],
	}, "alice")
	if len(capped.Advances) != contracts.MaxIssueAdvances {
		t.Fatalf("the issue holds %d relations, want %d", len(capped.Advances), contracts.MaxIssueAdvances)
	}
	refuseAdvancesOnAnIssueNobodyHas(t, database, priority)

	// A whole set written twice at one revision: the second write is answering a read the first
	// write made old, and is refused.
	second := createTestPriority(t, handler, map[string]any{"id": "hold-the-line", "title": "Hold the line"}, "alice").ID
	revision := readTestIssue(t, handler, core.Key).AdvancesRevision
	whole := map[string]any{"advances": []string{priority, second}, "advances_revision": revision}
	if written := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core.Key, whole, "alice"); written.Code != http.StatusOK {
		t.Fatalf("write the whole set: status=%d body=%s", written.Code, written.Body.String())
	}
	again := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core.Key, whole, "alice")
	if code, text := refusalOf(t, again); again.Code != http.StatusConflict || code != codeAdvancesRevision {
		t.Fatalf("the same write at the same revision: status=%d code=%s error=%q, want 409 %s",
			again.Code, code, text, codeAdvancesRevision)
	}
	if got := advanceEntries(readTestIssue(t, handler, core.Key).Advances); !slices.Equal(got,
		[]string{priority + " own", second + " own"}) && !slices.Equal(got, []string{second + " own", priority + " own"}) {
		t.Fatalf("%s advances %v after the refused second write, want the set the first wrote", core.Key, got)
	}

	// An issue that already stands under a parent, behind a blocker, in a repository and out of
	// the architecture keeps every one of them when it states what it advances, and the event
	// says that is all the write moved.
	if mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": "CORE"}, "alice"); mapped.Code != http.StatusOK && mapped.Code != http.StatusCreated {
		t.Fatalf("map the repository to CORE: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	parent := createAdvancingIssue(t, handler, map[string]any{"project": "CORE", "title": "The parent", "force": true}, "alice")
	blocker := createAdvancingIssue(t, handler, map[string]any{"project": "CORE", "title": "The blocker", "force": true}, "alice")
	child := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "The child", "force": true,
		"parent": parent.Key, "blocked_by": []string{blocker.Key}, "repo": "acme/widgets",
		"components": map[string]any{"mode": "none", "reason": "process"},
	}, "alice")
	if child.Parent == nil || *child.Parent != parent.Key || !slices.Equal(child.BlockedBy, []string{blocker.Key}) {
		t.Fatalf("the child stands under %v behind %v, want %s and %s", child.Parent, child.BlockedBy, parent.Key, blocker.Key)
	}
	if child.Repo == nil || *child.Repo != "acme/widgets" || child.Components.Mode != "none" {
		t.Fatalf("the child's repository is %v and its components %+v, want acme/widgets and mode none", child.Repo, child.Components)
	}
	advanced := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]any{
		"advances": []string{priority}, "advances_revision": child.AdvancesRevision,
	}, "alice")
	if advanced.Code != http.StatusOK {
		t.Fatalf("state what the child advances: status=%d body=%s", advanced.Code, advanced.Body.String())
	}
	written := decodeBody[model.Issue](t, advanced)
	if got := advanceEntries(written.Advances); !slices.Equal(got, []string{priority + " own"}) {
		t.Fatalf("the child advances %v, want its own relation to %s", got, priority)
	}
	if !stringPointersEqual(written.Parent, child.Parent) || !slices.Equal(written.BlockedBy, child.BlockedBy) {
		t.Fatalf("the write moved the parent to %v and the blockers to %v, want %v and %v",
			written.Parent, written.BlockedBy, child.Parent, child.BlockedBy)
	}
	if !stringPointersEqual(written.Repo, child.Repo) || !stringPointersEqual(written.ResolvedRepo, child.ResolvedRepo) {
		t.Fatalf("the write moved the repository to %v (resolved %v), want %v (%v)",
			written.Repo, written.ResolvedRepo, child.Repo, child.ResolvedRepo)
	}
	if !issueComponentsEqual(written.Components, child.Components) {
		t.Fatalf("the write moved the components to %+v, want %+v", written.Components, child.Components)
	}
	kind, payload := newestIssueEvent(t, database, child.Key)
	if kind != "issue.updated" {
		t.Fatalf("the newest event of %s is %q, want the issue.updated the write emitted", child.Key, kind)
	}
	named, isList := payload["changed"].([]any)
	if !isList {
		t.Fatalf("the issue.updated names no changed list: %v", payload["changed"])
	}
	changed := make([]string, 0, len(named))
	for _, field := range named {
		changed = append(changed, fmt.Sprint(field))
	}
	if !slices.Contains(changed, "advances") {
		t.Fatalf("the issue.updated's changed list = %v, want it to name advances", changed)
	}
	for _, field := range []string{"parent", "blocked_by", "repo", "components"} {
		if slices.Contains(changed, field) {
			t.Fatalf("the issue.updated's changed list names %s: %v, want advances alone", field, changed)
		}
	}
}

// refuseAdvancesOnAnIssueNobodyHas reads the fourth refusal of the field: a relation stated on an
// issue that does not exist. It is read where the route reads it — writeIssueAdvances, in a
// transaction of its own — because every route that carries the field addresses an issue it has
// already loaded, and so answers its own not-found first.
func refuseAdvancesOnAnIssueNobodyHas(t *testing.T, database *store.Store, priority string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open a transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	err = writeIssueAdvances(ctx, tx, "CORE-9999", nil, []string{priority},
		model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())
	var refusal *apiError
	if !errors.As(err, &refusal) {
		t.Fatalf("a relation on an issue nobody has answered %v, want a refusal", err)
	}
	if refusal.code != codeAdvancesInput || refusal.status != http.StatusBadRequest {
		t.Fatalf("the refusal = %d %s %q, want 400 %s", refusal.status, refusal.code, refusal.message, codeAdvancesInput)
	}
	if !strings.Contains(refusal.message, "CORE-9999") {
		t.Fatalf("the refusal %q does not name the issue", refusal.message)
	}
}

// createAdvancingIssue writes one issue through POST /api/v1/issues.
func createAdvancingIssue(t *testing.T, handler http.Handler, body map[string]any, login string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %v: status=%d body=%s", body["title"], response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// readTestIssue reads one issue through GET /api/v1/issues/{key}.
func readTestIssue(t *testing.T, handler http.Handler, key string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read issue %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// listedIssueKeys is the keys one listing answers, in the order it draws them.
func listedIssueKeys(t *testing.T, handler http.Handler, target string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	rows := decodeBody[[]model.IssueSummary](t, response)
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	return keys
}

// listedIssue is one row of a listing, by its key.
func listedIssue(t *testing.T, handler http.Handler, target, key string) model.IssueSummary {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	for _, row := range decodeBody[[]model.IssueSummary](t, response) {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("%s answers no row for %s", target, key)
	return model.IssueSummary{}
}

// advanceEntries writes each relation a read answered as "<priority> own" or "<priority>
// inherited", so a comparison says which of the two a test meant.
func advanceEntries(entries []model.IssueAdvance) []string {
	written := make([]string, 0, len(entries))
	for _, entry := range entries {
		how := "own"
		if entry.Inherited {
			how = "inherited"
		}
		written = append(written, entry.ID+" "+how)
	}
	return written
}

// priorityTupleRows is the whole log as one row per seq, each written out whole, so a later
// reading says both that no row was deleted and that no row was changed.
func priorityTupleRows(t *testing.T, database *store.Store) map[int64]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select seq, from_kind || '/' || from_id || ' ' || kind || ' ' || to_kind || '/' || to_id ||
			' at ' || at || ' by ' || who::text || ' event ' || source_event || ' asserted ' || asserted
		from priority_tuples order by seq
	`)
	if err != nil {
		t.Fatalf("read the priority tuples: %v", err)
	}
	log := map[int64]string{}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var written string
		if err := rows.Scan(&seq, &written); err != nil {
			t.Fatalf("read a priority tuple: %v", err)
		}
		log[seq] = written
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the priority tuples: %v", err)
	}
	if len(log) == 0 {
		t.Fatal("the log holds no tuple; a reading of it proves nothing")
	}
	return log
}

// newestIssueEvent is the kind and the payload of the newest event one issue carries.
func newestIssueEvent(t *testing.T, database *store.Store, key string) (string, map[string]any) {
	t.Helper()
	var kind string
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select type, payload from events where issue_key = $1 order by id desc limit 1
	`, key).Scan(&kind, &raw); err != nil {
		t.Fatalf("read %s's newest event: %v", key, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %s's newest event: %v", key, err)
	}
	return kind, payload
}
