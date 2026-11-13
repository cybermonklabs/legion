// priority_node_test.go drives the four routes of a priority (priorities.go) against the
// database store/storetest's Open clones from this package's migrated template, through the
// handler the neighbouring tests drive (newTestHandlerWithStore, dispatchRequest).
//
// What it reads, beside the writes and the refusals, is the two tables' own shape: a priority
// belongs to no project, so neither priority_nodes nor priority_tuples may carry a column that
// names one, and the node's nine columns and the tuple's ends, kind, time, writer and asserted
// flag are the node and tuple store's own (store/migrations/20261112024320_priority_nodes.up.sql).
// It reads the two credential refusals the deployment class brings with it too, and that the
// shared token every pod carries is unaffected by either (project_scope.go:383-386).
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// priorityNodeColumns is what priority_nodes carries, in the order the migration writes them:
// the node and tuple store's own columns, and no column that names a project.
var priorityNodeColumns = []string{
	"kind", "id", "supertag", "visibility", "state", "attributes", "made_at", "made_by", "source_event",
}

// priorityTupleEnds are the columns one stated fact needs: both ends, the kind, the time, the
// writer and whether it is asserted. The tuple table carries its log position beside them.
var priorityTupleEnds = []string{
	"from_kind", "from_id", "kind", "to_kind", "to_id", "at", "who", "asserted",
}

func TestPriorityNode(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()

	// A priority is written, and read back with everything the write said and the three the
	// store decides: its place in the one order, its state and who made it when.
	made := createTestPriority(t, handler, map[string]any{
		"id":          "ship-the-daemon",
		"title":       "Ship the daemon",
		"description": "# Why\n\nThe daemon runs every workspace.",
		"owner":       "alice",
	}, "alice")
	if made.ID != "ship-the-daemon" || made.Title != "Ship the daemon" {
		t.Fatalf("created priority = %+v, want the id and title the write named", made)
	}
	if made.Description != "# Why\n\nThe daemon runs every workspace." || made.Owner != "alice" {
		t.Fatalf("created priority description=%q owner=%q, want what the write named", made.Description, made.Owner)
	}
	if made.Rank == "" {
		t.Fatal("the created priority holds no place in the order")
	}
	if made.State != prioritystore.StateLive {
		t.Fatalf("created priority state = %q, want %q", made.State, prioritystore.StateLive)
	}
	if made.MadeAt.IsZero() || made.MadeBy != (model.Actor{Kind: "user", ID: "alice"}) {
		t.Fatalf("created priority made_at=%v made_by=%+v, want the moment and the person who wrote it", made.MadeAt, made.MadeBy)
	}
	read := readTestPriority(t, handler, "ship-the-daemon")
	if read.ID != made.ID || read.Title != made.Title || read.Description != made.Description ||
		read.Owner != made.Owner || read.Rank != made.Rank || read.State != made.State ||
		read.MadeBy != made.MadeBy || !sameStoredMoment(read.MadeAt, made.MadeAt) {
		t.Fatalf("the priority read back = %+v, want the one written %+v", read, made)
	}

	// A second priority takes the last place of the one order, so the two read back in the
	// order they were written.
	second := createTestPriority(t, handler, map[string]any{
		"id": "cut-the-queue", "title": "Cut the queue", "owner": "bob",
	}, "alice")
	if second.Rank <= made.Rank {
		t.Fatalf("the second priority's order %q is not after the first's %q", second.Rank, made.Rank)
	}
	if order := listedPriorityIDs(t, handler); !slices.Equal(order, []string{"ship-the-daemon", "cut-the-queue"}) {
		t.Fatalf("the order = %v, want the two in the order they were written", order)
	}

	// A rename writes the title and leaves every other attribute, and the row's own first
	// write, as they were.
	renamed := patchTestPriority(t, handler, "ship-the-daemon", map[string]any{"title": "Ship the daemon everywhere"}, "bob")
	if renamed.Title != "Ship the daemon everywhere" {
		t.Fatalf("renamed title = %q, want the new one", renamed.Title)
	}
	if renamed.Description != made.Description || renamed.Owner != made.Owner || renamed.Rank != made.Rank {
		t.Fatalf("the rename moved more than the title: %+v, want description, owner and order of %+v", renamed, made)
	}
	if !sameStoredMoment(renamed.MadeAt, made.MadeAt) || renamed.MadeBy != made.MadeBy {
		t.Fatalf("the rename moved made_at/made_by to %v/%+v, want the first write's %v/%+v",
			renamed.MadeAt, renamed.MadeBy, made.MadeAt, made.MadeBy)
	}
	if got := readTestPriority(t, handler, "ship-the-daemon"); got.Title != renamed.Title {
		t.Fatalf("the renamed priority reads back as %q", got.Title)
	}

	// A move writes a new place in the order, and the list is drawn in it.
	moved := patchTestPriority(t, handler, "cut-the-queue", map[string]any{
		"rank": map[string]any{"before": "ship-the-daemon"},
	}, "alice")
	if moved.Rank >= renamed.Rank {
		t.Fatalf("the moved priority's order %q is not above %q", moved.Rank, renamed.Rank)
	}
	if moved.Rank == second.Rank {
		t.Fatalf("the move left the order where it was (%q)", moved.Rank)
	}
	if order := listedPriorityIDs(t, handler); !slices.Equal(order, []string{"cut-the-queue", "ship-the-daemon"}) {
		t.Fatalf("the order after the move = %v, want the moved priority first", order)
	}
	if got := readTestPriority(t, handler, "cut-the-queue"); got.Rank != moved.Rank {
		t.Fatalf("the moved priority reads its order back as %q, want %q", got.Rank, moved.Rank)
	}

	// A delete writes the state gone and removes no row: the listing answers without it, and
	// the row still says who made it and when.
	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/cut-the-queue", nil, "bob")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete a priority: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if order := listedPriorityIDs(t, handler); !slices.Equal(order, []string{"ship-the-daemon"}) {
		t.Fatalf("the listing after the delete = %v, want the deleted priority gone from it", order)
	}
	var state, title string
	var madeBy []byte
	if err := database.Pool.QueryRow(ctx, `
		select state, attributes->>'title', made_by::text from priority_nodes where kind = $1 and id = $2
	`, prioritystore.KindPriority, "cut-the-queue").Scan(&state, &title, &madeBy); err != nil {
		t.Fatalf("read the deleted priority's row: %v", err)
	}
	if state != prioritystore.StateGone {
		t.Fatalf("the deleted priority's state = %q, want %q", state, prioritystore.StateGone)
	}
	if title != "Cut the queue" || !strings.Contains(string(madeBy), "alice") {
		t.Fatalf("the deleted priority's row = title %q made_by %s, want the row kept whole", title, madeBy)
	}
	states, err := prioritystore.History(ctx, database.Pool, prioritystore.KindPriority, "cut-the-queue", prioritystore.AttributeState)
	if err != nil {
		t.Fatalf("read the deleted priority's state history: %v", err)
	}
	if len(states) != 2 || states[0].ToID != prioritystore.StateLive || states[1].ToID != prioritystore.StateGone {
		t.Fatalf("the state history = %+v, want live then gone", states)
	}

	// The refusals a write is answered with.
	duplicate := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"id": "ship-the-daemon", "title": "Ship it again",
	}, "alice")
	if code, text := refusalOf(t, duplicate); duplicate.Code != http.StatusConflict || code != codePriorityExists {
		t.Fatalf("a create of an id the store holds: status=%d code=%s error=%q, want 409 %s",
			duplicate.Code, code, text, codePriorityExists)
	}
	blank := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"id": "nameless", "title": "   ",
	}, "alice")
	if code, text := refusalOf(t, blank); blank.Code != http.StatusBadRequest || code != codePriorityInput {
		t.Fatalf("a create with an empty title: status=%d code=%s error=%q, want 400 %s",
			blank.Code, code, text, codePriorityInput)
	}
	renamedBlank := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/ship-the-daemon", map[string]any{
		"title": "  ",
	}, "alice")
	if code, text := refusalOf(t, renamedBlank); renamedBlank.Code != http.StatusBadRequest || code != codePriorityInput {
		t.Fatalf("a rename to an empty title: status=%d code=%s error=%q, want 400 %s",
			renamedBlank.Code, code, text, codePriorityInput)
	}
	// An order write that names no priority: one that names neither neighbour, and one whose
	// neighbour is an id no priority carries. Each is a move to a place the order does not have.
	for name, body := range map[string]map[string]any{
		"naming neither neighbour":  {"rank": map[string]any{}},
		"naming no priority it has": {"rank": map[string]any{"before": "no-such-priority"}},
	} {
		refused := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/ship-the-daemon", body, "alice")
		if code, text := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != codeRankInput {
			t.Fatalf("an order write %s: status=%d code=%s error=%q, want 400 %s",
				name, refused.Code, code, text, codeRankInput)
		}
	}
	if got := readTestPriority(t, handler, "ship-the-daemon"); got.Rank != renamed.Rank {
		t.Fatalf("a refused order write moved the priority to %q, want it left at %q", got.Rank, renamed.Rank)
	}

	// The two tables' own columns, read out of the catalogue rather than off the migration: a
	// priority belongs to no project, so no column of either names one.
	nodeColumns := tableColumnNames(t, database, "priority_nodes")
	tupleColumns := tableColumnNames(t, database, "priority_tuples")
	for _, table := range []struct {
		name    string
		columns []string
	}{{"priority_nodes", nodeColumns}, {"priority_tuples", tupleColumns}} {
		for _, column := range table.columns {
			if strings.Contains(strings.ToLower(column), "project") {
				t.Errorf("%s carries the column %q, which names a project: a priority belongs to none",
					table.name, column)
			}
		}
	}
	if !slices.Equal(nodeColumns, priorityNodeColumns) {
		t.Fatalf("priority_nodes columns = %v, want %v", nodeColumns, priorityNodeColumns)
	}
	for _, column := range priorityTupleEnds {
		if !slices.Contains(tupleColumns, column) {
			t.Errorf("priority_tuples carries no %q column: a tuple needs both ends, its kind, its time, its writer and whether it is asserted", column)
		}
	}

	// The state a priority stands in is live or gone and nothing else, in the rows and in the
	// log alike: a delete writes gone and nothing writes a third word.
	nodeStates := distinctStrings(t, database, `select distinct state from priority_nodes order by 1`)
	if !slices.Equal(nodeStates, []string{prioritystore.StateGone, prioritystore.StateLive}) {
		t.Fatalf("priority_nodes holds the states %v, want exactly %q and %q",
			nodeStates, prioritystore.StateGone, prioritystore.StateLive)
	}
	statedStates := distinctStrings(t, database, `
		select distinct to_id from priority_tuples where kind = 'state' and to_kind = 'value' order by 1
	`)
	if !slices.Equal(statedStates, []string{prioritystore.StateGone, prioritystore.StateLive}) {
		t.Fatalf("the state tuples hold %v, want exactly %q and %q",
			statedStates, prioritystore.StateGone, prioritystore.StateLive)
	}

	// The shared token every pod carries reads the route and writes a priority: it names no
	// project, and it names a session rather than a service with no person behind it.
	shared := agentRequest(t, handler, http.MethodGet, "/api/v1/priorities", nil, "agent-token")
	if shared.Code != http.StatusOK {
		t.Fatalf("the shared token reading the priorities: status=%d body=%s", shared.Code, shared.Body.String())
	}
	written := agentRequest(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{
		"id": "hold-the-line", "title": "Hold the line",
		"actor": map[string]any{"kind": "session", "id": "ses_pod_one"},
	}, "agent-token")
	if written.Code != http.StatusCreated {
		t.Fatalf("the shared token writing a priority: status=%d body=%s", written.Code, written.Body.String())
	}
	if pod := decodeBody[model.Priority](t, written); pod.MadeBy.Kind != "session" || pod.MadeBy.ID != "ses_pod_one" {
		t.Fatalf("the pod's priority was recorded under %+v, want the session its body named", pod.MadeBy)
	}
	if order := listedPriorityIDs(t, handler); !slices.Contains(order, "hold-the-line") {
		t.Fatalf("the priority the shared token wrote is not listed: %v", order)
	}

	// The two credential refusals the write class brings: a credential held to one project may
	// not write a priority, which belongs to none, and a token naming a service and no person
	// may write nothing at all.
	refuseScopedCredentials(t)
}

// TestPriorityGoneTakesNoWrite reads that a deleted priority is no priority to a write (R4: "A
// priority whose state is gone is no priority"). Its page answers that no priority of the id is
// written, the listing leaves it out and a relation to it is refused, so a rename or a second
// delete answered as a success writes history nobody can see and tells the caller a deleted
// priority was changed.
func TestPriorityGoneTakesNoWrite(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createTestPriority(t, handler, map[string]any{"id": "retired-goal", "title": "Retired goal"}, "alice")
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/retired-goal", nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete a priority: status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	renamed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/retired-goal", map[string]any{"title": "Revived"}, "alice")
	if renamed.Code < 400 || renamed.Code >= 500 {
		t.Fatalf("rename a deleted priority: status=%d body=%s, want a refusal", renamed.Code, renamed.Body.String())
	}
	titles, err := prioritystore.History(ctx, database.Pool, prioritystore.KindPriority, "retired-goal", prioritystore.AttributeTitle)
	if err != nil {
		t.Fatalf("read the deleted priority's title history: %v", err)
	}
	if len(titles) != 1 {
		t.Fatalf("the deleted priority's title history = %+v, want the one title it was made with", titles)
	}

	again := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/retired-goal", nil, "alice")
	states, err := prioritystore.History(ctx, database.Pool, prioritystore.KindPriority, "retired-goal", prioritystore.AttributeState)
	if err != nil {
		t.Fatalf("read the deleted priority's state history: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("a second delete (status=%d) left the state history %+v, want live then one gone", again.Code, states)
	}
}

// TestPriorityIDRefusesTheNoneKeyword reads that no priority takes the id `none`, given or
// derived from its title: the issue listing reads `advances=none` as the work under no priority
// (parseAdvancesFilter), so a priority of that id would have its own page and row list the work
// under no priority, and no filter could ask for the work under it. An id that merely begins
// with the word is a slug like any other and is written.
func TestPriorityIDRefusesTheNoneKeyword(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	for name, body := range map[string]map[string]any{
		"named in id":            {"id": noAdvancesFilter, "title": "Whatever is left"},
		"derived from the title": {"title": "None"},
		"derived from a title in capitals and spaces": {"title": "  NONE  "},
	} {
		refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", body, "alice")
		code, text := refusalOf(t, refused)
		if refused.Code != http.StatusBadRequest || code != codePriorityInput {
			t.Fatalf("a create whose id is %q, %s: status=%d code=%s error=%q, want 400 %s",
				noAdvancesFilter, name, refused.Code, code, text, codePriorityInput)
		}
		if !strings.Contains(text, "advances="+noAdvancesFilter) {
			t.Fatalf("the refusal of the id %q, %s, says %q, want it to name the filter keyword", noAdvancesFilter, name, text)
		}
	}
	var written int
	if err := database.Pool.QueryRow(ctx, `select count(*) from priority_nodes where kind = $1 and id = $2`,
		prioritystore.KindPriority, noAdvancesFilter).Scan(&written); err != nil {
		t.Fatalf("count the priorities of the id %q: %v", noAdvancesFilter, err)
	}
	if written != 0 {
		t.Fatalf("the refused creates wrote %d priorities of the id %q, want none", written, noAdvancesFilter)
	}
	if made := createTestPriority(t, handler, map[string]any{"title": "None left behind"}, "alice"); made.ID != "none-left-behind" {
		t.Fatalf("a title beginning with the word derives the id %q, want %q", made.ID, "none-left-behind")
	}
}

// TestPriorityIDRefusesTheAnyKeyword reads that no priority takes the id `any`, given or derived
// from its title, for the reason it takes no `none`: the issue listing reads `advances=any` as
// the work under any live priority (parseAdvancesFilter), so a priority of that id would have its
// own page and row list the work under every priority, and no filter could ask for the work
// under it. An id that merely begins with the word is a slug like any other and is written.
func TestPriorityIDRefusesTheAnyKeyword(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	for name, body := range map[string]map[string]any{
		"named in id":            {"id": anyAdvancesFilter, "title": "Whatever matters"},
		"derived from the title": {"title": "Any"},
		"derived from a title in capitals and spaces": {"title": "  ANY  "},
	} {
		refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", body, "alice")
		code, text := refusalOf(t, refused)
		if refused.Code != http.StatusBadRequest || code != codePriorityInput {
			t.Fatalf("a create whose id is %q, %s: status=%d code=%s error=%q, want 400 %s",
				anyAdvancesFilter, name, refused.Code, code, text, codePriorityInput)
		}
		if !strings.Contains(text, "advances="+anyAdvancesFilter) {
			t.Fatalf("the refusal of the id %q, %s, says %q, want it to name the filter keyword", anyAdvancesFilter, name, text)
		}
	}
	var written int
	if err := database.Pool.QueryRow(ctx, `select count(*) from priority_nodes where kind = $1 and id = $2`,
		prioritystore.KindPriority, anyAdvancesFilter).Scan(&written); err != nil {
		t.Fatalf("count the priorities of the id %q: %v", anyAdvancesFilter, err)
	}
	if written != 0 {
		t.Fatalf("the refused creates wrote %d priorities of the id %q, want none", written, anyAdvancesFilter)
	}
	if made := createTestPriority(t, handler, map[string]any{"title": "Any other business"}, "alice"); made.ID != "any-other-business" {
		t.Fatalf("a title beginning with the word derives the id %q, want %q", made.ID, "any-other-business")
	}
}

// TestPriorityTitleAndIDAreBounded reads the two bounds a priority write holds its text to. The
// title is at most contracts.IssueTitleMax, as an issue title is, on a create and a rename alike.
// The id is at most priorityIDMax, whether given or derived from the title: it is a btree key, so
// an id with no bound reached the insert and failed it as a 500. Each is refused 400
// PRIORITY_INPUT naming its bound, and a refused write leaves the store as it was.
func TestPriorityTitleAndIDAreBounded(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	refuse := func(what string, response *httptest.ResponseRecorder, bound int) {
		t.Helper()
		code, text := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != codePriorityInput {
			t.Fatalf("%s: status=%d code=%s error=%.300q, want 400 %s", what, response.Code, code, text, codePriorityInput)
		}
		if !strings.Contains(text, fmt.Sprint(bound)) {
			t.Fatalf("%s is refused with %.300q, want it to name the bound %d", what, text, bound)
		}
	}

	// A title at the bound is written, and one past it is refused on a create and a rename.
	atLimit := strings.Repeat("a", contracts.IssueTitleMax)
	made := createTestPriority(t, handler, map[string]any{"id": "long-title", "title": "  " + atLimit + "  "}, "alice")
	if made.Title != atLimit {
		t.Fatalf("the created title is %d characters, want the %d it was sent without its spaces", len(made.Title), contracts.IssueTitleMax)
	}
	overTitle := atLimit + "a"
	refuse("a create whose title is past the bound", dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": "longer-title", "title": overTitle}, "alice"), contracts.IssueTitleMax)
	refuse("a rename past the bound", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/long-title",
		map[string]any{"title": overTitle}, "alice"), contracts.IssueTitleMax)
	if got := readTestPriority(t, handler, "long-title"); got.Title != atLimit {
		t.Fatalf("the refused rename left a title of %d characters, want the %d it had", len(got.Title), contracts.IssueTitleMax)
	}

	// An id at the bound is written. One past it, one far past the btree's own bound, and one
	// derived from a title past it are each refused, and none is a 500.
	if atBound := strings.Repeat("b", priorityIDMax); createTestPriority(t, handler,
		map[string]any{"id": atBound, "title": "At the bound"}, "alice").ID != atBound {
		t.Fatalf("the id of %d characters was not written as it was sent", priorityIDMax)
	}
	refuse("a create whose id is one past the bound", dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": strings.Repeat("c", priorityIDMax+1), "title": "One past"}, "alice"), priorityIDMax)
	refuse("a create whose id is past the btree's bound", dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"id": strings.Repeat("d", 3000), "title": "Far past"}, "alice"), priorityIDMax)
	refuse("a create whose title derives an id past the bound", dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities",
		map[string]any{"title": strings.Repeat("e", priorityIDMax+1)}, "alice"), priorityIDMax)

	var written int
	if err := database.Pool.QueryRow(ctx, `select count(*) from priority_nodes where kind = $1`,
		prioritystore.KindPriority).Scan(&written); err != nil {
		t.Fatalf("count the priorities: %v", err)
	}
	if written != 2 {
		t.Fatalf("the store holds %d priorities, want the 2 within the bounds", written)
	}
}

// refuseScopedCredentials reads the two refusals a priority write answers a broker-issued
// credential with, on a rig of its own, since each needs a broker the package's plain test
// server has none of (broker_token_test.go).
func refuseScopedCredentials(t *testing.T) {
	t.Helper()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)

	const scoped = "a-token-held-to-one-project"
	rig.broker.Says(scoped, liveNamedToken(caller.jkt))
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project the credential covers: status=%d body=%s", created.Code, created.Body.String())
	}
	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/priorities", scoped, map[string]any{
		"id": "one-project-only", "title": "One project only",
	}))
	code, text := refusalOf(t, refused)
	if refused.Code != http.StatusForbidden || code != codeProjectScopeDeployment {
		t.Fatalf("a priority write under a project-scoped token: status=%d code=%s error=%q, want 403 %s",
			refused.Code, code, text, codeProjectScopeDeployment)
	}
	if !strings.Contains(text, brokerTestProject) || !strings.Contains(text, "POST /api/v1/priorities") {
		t.Fatalf("the refusal %q names neither the project the credential covers nor the route", text)
	}

	const serviceOnly = "a-token-naming-no-person"
	rig.broker.Says(serviceOnly, serviceOnlyToken(caller.jkt))
	nobody := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/priorities", serviceOnly, map[string]any{
		"id": "written-by-nobody", "title": "Written by nobody",
	}))
	nobodyCode, nobodyText := refusalOf(t, nobody)
	if nobody.Code != http.StatusForbidden || nobodyCode != contracts.DispatchSignInRequired {
		t.Fatalf("a priority write under a token naming a service and no person: status=%d code=%s error=%q, want 403 %s",
			nobody.Code, nobodyCode, nobodyText, contracts.DispatchSignInRequired)
	}
	if !strings.Contains(nobodyText, brokerTestService) || !strings.Contains(nobodyText, "agent-secrets login") {
		t.Fatalf("the refusal %q names neither the service nor the command that signs a person in", nobodyText)
	}

	listed := rig.asPerson(t, http.MethodGet, "/api/v1/priorities", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list the priorities: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if body := listed.Body.String(); strings.Contains(body, "one-project-only") || strings.Contains(body, "written-by-nobody") {
		t.Fatalf("a refused write wrote a priority: %s", body)
	}
}

// createTestPriority writes one priority through POST /api/v1/priorities and answers the row the
// route hands back.
func createTestPriority(t *testing.T, handler http.Handler, body map[string]any, login string) model.Priority {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/priorities", body, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("create priority %v: status=%d body=%s", body["id"], response.Code, response.Body.String())
	}
	return decodeBody[model.Priority](t, response)
}

// patchTestPriority writes one priority through PATCH /api/v1/priorities/{id}.
func patchTestPriority(t *testing.T, handler http.Handler, id string, body map[string]any, login string) model.Priority {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/"+id, body, login)
	if response.Code != http.StatusOK {
		t.Fatalf("write priority %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[model.Priority](t, response)
}

// listTestPriorities reads GET /api/v1/priorities: every live priority, in the one order.
func listTestPriorities(t *testing.T, handler http.Handler) []model.PriorityRow {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/priorities", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list the priorities: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Priorities](t, response).Priorities
}

// listedPriorityIDs is the ids of the live priorities, in the order they are drawn.
func listedPriorityIDs(t *testing.T, handler http.Handler) []string {
	t.Helper()
	rows := listTestPriorities(t, handler)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// readTestPriority answers one live priority as the listing draws it.
func readTestPriority(t *testing.T, handler http.Handler, id string) model.Priority {
	t.Helper()
	for _, row := range listTestPriorities(t, handler) {
		if row.ID == id {
			return row.Priority
		}
	}
	t.Fatalf("no live priority %q is listed", id)
	return model.Priority{}
}

// tableColumnNames reads one table's own columns out of the catalogue, in the order the table
// carries them.
func tableColumnNames(t *testing.T, database *store.Store, table string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select column_name from information_schema.columns
		where table_schema = 'public' and table_name = $1
		order by ordinal_position
	`, table)
	if err != nil {
		t.Fatalf("read the columns of %s: %v", table, err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the columns of %s: %v", table, err)
	}
	if len(columns) == 0 {
		t.Fatalf("the schema holds no table %s", table)
	}
	return columns
}

// distinctStrings answers one single-column read as the strings it held.
func distinctStrings(t *testing.T, database *store.Store, query string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return values
}

// sameStoredMoment answers whether two readings of one write's moment are the same moment. The
// create answers the time it wrote, to the nanosecond, and every later read answers the column
// Postgres stored, whose precision is the microsecond, so the two are compared within one.
func sameStoredMoment(left, right time.Time) bool {
	return left.Sub(right).Abs() < time.Microsecond
}
