package api

// going_plan_status_options_test.go holds what a plan does with the options of
// dispatch_issue_update (include_children, unlink_children, include_parent): the one change is
// written out as one update row for each issue the option changes, in the status change route's
// order, and each row is approved, run and undone as any update row is (going_plan_status_options.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// newStatusOptionsRig is the plan rig over a server whose parent status rule
// (DISPATCH_PARENT_STATUS_RULE) is on or off. The setting is made before the agent listens, so no
// request reads it while it is set.
func newStatusOptionsRig(t *testing.T, ruleOn bool) goingPlanRig {
	t.Helper()
	writes := newGoingWritesRig(t)
	writes.served.deps.ParentStatusRule = ParentStatusRule{on: ruleOn}
	return newGoingPlanRigOver(t, writes)
}

// childOf makes one more issue of DEMO under parent.
func (rig goingPlanRig) childOf(t *testing.T, title, parent string) string {
	t.Helper()
	return decodeBody[model.Issue](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": title, "parent": parent, "force": true,
	})).Key
}

// setStatus moves issue to status as alice does by hand, which must succeed: the fixtures put the
// statuses in an order the parent status rule keeps.
func (rig goingPlanRig) setStatus(t *testing.T, issue, status string) {
	t.Helper()
	rig.fixture(t, "alice", http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"status": status})
}

// optionState is the status and the parent of an issue, "" for no parent.
type optionState struct{ status, parent string }

// states is the status and the parent of each of issues now.
func (rig goingPlanRig) states(t *testing.T, issues ...string) map[string]optionState {
	t.Helper()
	held := make(map[string]optionState, len(issues))
	for _, key := range issues {
		read := rig.issueRead(t, key)
		state := optionState{status: read.Status}
		if read.Parent != nil {
			state.parent = *read.Parent
		}
		held[key] = state
	}
	return held
}

// optionRow is a row an option's change must become: the issue it changes, the one field it writes,
// the value that field has now, and the rows it needs.
type optionRow struct {
	issue string
	write map[string]any
	now   map[string]any
	needs []int
}

// statusRow is the row of a status change of issue from now to status.
func statusRow(issue, from, to string, needs ...int) optionRow {
	return optionRow{issue: issue, write: map[string]any{"status": to}, now: map[string]any{"status": from}, needs: needs}
}

// unlinkRow is the row that unlinks issue from parent.
func unlinkRow(issue, parent string, needs ...int) optionRow {
	return optionRow{issue: issue, write: map[string]any{"parent": ""}, now: map[string]any{"parent": parent}, needs: needs}
}

// requireOptionRows fails t unless rows are the update rows want names, in order.
func requireOptionRows(t *testing.T, rows []goingPlanRowStored, want []optionRow) {
	t.Helper()
	if len(rows) != len(want) {
		var held []string
		for _, row := range rows {
			held = append(held, fmt.Sprintf("%d %s %s needing %v", row.row, row.tool, row.arguments, row.needs))
		}
		t.Fatalf("the plan holds %d rows, want %d: %s", len(rows), len(want), strings.Join(held, "; "))
	}
	for index, row := range rows {
		wantArguments := maps.Clone(want[index].write)
		wantArguments["issue"] = want[index].issue
		var arguments, now map[string]any
		if err := json.Unmarshal(row.arguments, &arguments); err != nil {
			t.Fatalf("row %d's arguments %s: %v", index, row.arguments, err)
		}
		if err := json.Unmarshal(row.shown.Now, &now); err != nil {
			t.Fatalf("row %d's value now %s: %v", index, row.shown.Now, err)
		}
		switch {
		case row.row != index || row.tool != "dispatch_issue_update":
			t.Errorf("row %d is number %d of %s, want an update of its own number", index, row.row, row.tool)
		case !reflect.DeepEqual(arguments, wantArguments):
			t.Errorf("row %d writes %v, want %v", index, arguments, wantArguments)
		case !reflect.DeepEqual(now, want[index].now):
			t.Errorf("row %d shows %v now, want %v", index, now, want[index].now)
		case !slices.Equal(row.needs, want[index].needs):
			t.Errorf("row %d needs %v, want %v", index, row.needs, want[index].needs)
		}
	}
}

// proposedPlan makes a plan call of a working run of alice's that proposes changes, and answers the
// run, which holds the plan where the server kept it, and the server's refusal, nil where it kept
// the plan. The run is stopped once its call is answered, so alice's runs that work stay under the 3
// one person may have.
func (rig goingPlanRig) proposedPlan(t *testing.T, changes ...map[string]any) (string, *agentwire.Error) {
	t.Helper()
	run, token := rig.working(t, "alice", "change the issues under it")
	defer rig.exec(t, `update going_runs set state = 'stopped', lease_token = null, lease_owner = null, lease_ends_at = null where id = $1::uuid`, run)
	encoded, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	var proposed []agentwire.PlanChange
	if err := json.Unmarshal(encoded, &proposed); err != nil {
		t.Fatal(err)
	}
	_, err = rig.call(agentwire.CallRequest{Run: run, LeaseToken: token, Step: 2, Call: 1, Kind: agentwire.KindPlan,
		Proposal: &agentwire.PlanProposal{Summary: "Change them.", Changes: proposed}})
	var refused *agentwire.Error
	if errors.As(err, &refused) {
		return run, refused
	}
	if err != nil {
		t.Fatalf("the plan call: %v", err)
	}
	return run, nil
}

// keptRows is the rows of the plan the server kept for run.
func (rig goingPlanRig) keptRows(t *testing.T, run string) []goingPlanRowStored {
	t.Helper()
	plan, err := readGoingPlan(context.Background(), rig.database.Pool, run, 0)
	if err != nil || plan == nil {
		t.Fatalf("read the plan of %s: %v (%v)", run, plan, err)
	}
	return plan.rows
}

// optionCase is one tree of issues, the change that sets an option over it, the rows the change
// must become, and what the issues the rows change must hold once they ran.
type optionCase struct {
	change map[string]any
	rows   []optionRow
	after  map[string]optionState
}

// optionScenarios build one tree each, in an order of statuses that keeps the parent status rule, so
// each tree is the same with the rule on and off.
var optionScenarios = []struct {
	name  string
	build func(t *testing.T, rig goingPlanRig) optionCase
}{
	{"include_children into work moves the issue, then the issues under it, highest first", func(t *testing.T, rig goingPlanRig) optionCase {
		root := rig.issue(t, "Epic to start")
		a := rig.childOf(t, "Child A", root)
		b := rig.childOf(t, "Grandchild B", a)
		c := rig.childOf(t, "Child C", root)
		done := rig.childOf(t, "Child already done", root)
		iced := rig.childOf(t, "Child in the icebox", root)
		rig.setStatus(t, done, "done")
		rig.setStatus(t, iced, "icebox")
		return optionCase{
			change: map[string]any{"issue": root, "status": "in_progress", "include_children": true},
			// The done child is kept, and the icebox child is not in work, so it moves.
			rows: []optionRow{
				statusRow(root, "triage", "in_progress"),
				statusRow(a, "triage", "in_progress", 0),
				statusRow(c, "triage", "in_progress", 0),
				statusRow(iced, "icebox", "in_progress", 0),
				statusRow(b, "triage", "in_progress", 1),
			},
			after: map[string]optionState{
				root: {"in_progress", ""}, a: {"in_progress", root}, b: {"in_progress", a}, c: {"in_progress", root},
				iced: {"in_progress", root}, done: {"done", root},
			},
		}
	}},
	{"include_children out of work moves the issues under the issue, deepest first, then the issue", func(t *testing.T, rig goingPlanRig) optionCase {
		root := rig.issue(t, "Epic to shelve")
		rig.setStatus(t, root, "in_progress")
		a := rig.childOf(t, "Child A", root)
		b := rig.childOf(t, "Grandchild B", a)
		c := rig.childOf(t, "Child C", root)
		done := rig.childOf(t, "Child already done", root)
		iced := rig.childOf(t, "Child in the icebox", root)
		rig.setStatus(t, a, "in_progress")
		rig.setStatus(t, b, "todo")
		rig.setStatus(t, c, "testing")
		rig.setStatus(t, done, "done")
		rig.setStatus(t, iced, "icebox")
		return optionCase{
			change: map[string]any{"issue": root, "status": "backlog", "include_children": true},
			// The done child is kept, and the icebox child is not started already.
			rows: []optionRow{
				statusRow(b, "todo", "backlog"),
				statusRow(a, "in_progress", "backlog", 0),
				statusRow(c, "testing", "backlog"),
				statusRow(root, "in_progress", "backlog", 1, 2),
			},
			after: map[string]optionState{
				root: {"backlog", ""}, a: {"backlog", root}, b: {"backlog", a}, c: {"backlog", root},
				iced: {"icebox", root}, done: {"done", root},
			},
		}
	}},
	{"include_parent moves the ancestors out of work to in_progress, highest first, a done one included", func(t *testing.T, rig goingPlanRig) optionCase {
		top := rig.issue(t, "Top in work")
		rig.setStatus(t, top, "in_progress")
		grand := rig.childOf(t, "Grandparent", top)
		parent := rig.childOf(t, "Parent", grand)
		named := rig.childOf(t, "Issue to reopen", parent)
		rig.setStatus(t, named, "done")
		rig.setStatus(t, parent, "done")
		rig.setStatus(t, grand, "done")
		return optionCase{
			change: map[string]any{"issue": named, "status": "todo", "include_parent": true},
			// The top issue is in work already, so it gets no row.
			rows: []optionRow{
				statusRow(grand, "done", "in_progress"),
				statusRow(parent, "done", "in_progress", 0),
				statusRow(named, "done", "todo", 1),
			},
			after: map[string]optionState{
				top: {"in_progress", ""}, grand: {"in_progress", top}, parent: {"in_progress", grand}, named: {"todo", parent},
			},
		}
	}},
	{"unlink_children unlinks each open child, then the issue moves, and a done child stays", func(t *testing.T, rig goingPlanRig) optionCase {
		root := rig.issue(t, "Epic to shelve and empty")
		rig.setStatus(t, root, "in_progress")
		x := rig.childOf(t, "Open child X", root)
		y := rig.childOf(t, "Open child Y", root)
		done := rig.childOf(t, "Child already done", root)
		rig.setStatus(t, x, "in_progress")
		rig.setStatus(t, done, "done")
		return optionCase{
			change: map[string]any{"issue": root, "status": "backlog", "unlink_children": true},
			rows: []optionRow{
				unlinkRow(x, root),
				unlinkRow(y, root),
				statusRow(root, "in_progress", "backlog", 0, 1),
			},
			after: map[string]optionState{
				root: {"backlog", ""}, x: {"in_progress", ""}, y: {"triage", ""}, done: {"done", root},
			},
		}
	}},
	{"include_parent with unlink_children moves the ancestors, then unlinks, then the issue", func(t *testing.T, rig goingPlanRig) optionCase {
		top := rig.issue(t, "Top not started")
		middle := rig.childOf(t, "Middle not started", top)
		named := rig.childOf(t, "Issue to start", middle)
		child := rig.childOf(t, "Child set free", named)
		return optionCase{
			change: map[string]any{"issue": named, "status": "in_progress", "include_parent": true, "unlink_children": true},
			rows: []optionRow{
				statusRow(top, "triage", "in_progress"),
				statusRow(middle, "triage", "in_progress", 0),
				unlinkRow(child, named),
				statusRow(named, "triage", "in_progress", 1, 2),
			},
			after: map[string]optionState{
				top: {"in_progress", ""}, middle: {"in_progress", top}, named: {"in_progress", middle}, child: {"triage", ""},
			},
		}
	}},
}

// TestAStatusOptionIsWrittenOutAsOneRowForEachIssueItChanges: a dispatch_issue_update change that
// sets include_children, unlink_children or include_parent is not refused: the plan holds one
// update row for each issue the option changes, each with its key and the one field it writes - the
// new status, or for an unlink the parent, "" for none - showing that field's value now, in the
// status change route's order, each row needing the earlier rows of its issue's parent or child. An
// issue the option would leave as it stands, or whose status is not the one to move, gets no row.
// The named issue's row carries the call's status, and its reason where the status is done.
func TestAStatusOptionIsWrittenOutAsOneRowForEachIssueItChanges(t *testing.T) {
	t.Parallel()
	rig := newStatusOptionsRig(t, true)
	for _, scenario := range optionScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			c := scenario.build(t, rig)
			run, refused := rig.proposedPlan(t, goingChange("dispatch_issue_update", c.change))
			if refused != nil {
				t.Fatalf("the plan call was refused %+v, want the plan kept", refused)
			}
			requireOptionRows(t, rig.keptRows(t, run), c.rows)
		})
	}

	t.Run("an option that reaches no issue leaves the one row of the named issue", func(t *testing.T) {
		leaf := rig.issue(t, "Leaf")
		run, refused := rig.proposedPlan(t, goingChange("dispatch_issue_update", map[string]any{"issue": leaf, "status": "todo", "include_children": true}))
		if refused != nil {
			t.Fatalf("the plan call was refused %+v, want the plan kept", refused)
		}
		requireOptionRows(t, rig.keptRows(t, run), []optionRow{statusRow(leaf, "triage", "todo")})
	})

	t.Run("an option set to false asks for nothing, and its change is one row as written", func(t *testing.T) {
		parent := rig.issue(t, "Parent kept as it is")
		rig.childOf(t, "Child kept as it is", parent)
		run, refused := rig.proposedPlan(t, goingChange("dispatch_issue_update", map[string]any{"issue": parent, "status": "todo", "include_children": false}))
		if refused != nil {
			t.Fatalf("the plan call was refused %+v, want the plan kept", refused)
		}
		if rows := rig.keptRows(t, run); len(rows) != 1 {
			t.Fatalf("the plan holds %d rows, want the one change as written", len(rows))
		}
	})

	t.Run("the named issue's row carries a done status and its reason", func(t *testing.T) {
		root := rig.issue(t, "Epic to close")
		x := rig.childOf(t, "Child set free for the close", root)
		run, refused := rig.proposedPlan(t, goingChange("dispatch_issue_update",
			map[string]any{"issue": root, "status": "done", "reason": "Split into its own issues.", "unlink_children": true}))
		if refused != nil {
			t.Fatalf("the plan call was refused %+v, want the plan kept", refused)
		}
		rows := rig.keptRows(t, run)
		if len(rows) != 2 {
			t.Fatalf("the plan holds %d rows, want the unlink and the close", len(rows))
		}
		var closing map[string]any
		if err := json.Unmarshal(rows[1].arguments, &closing); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"issue": root, "status": "done", "reason": "Split into its own issues."}
		if !reflect.DeepEqual(closing, want) || !slices.Equal(rows[1].needs, []int{0}) || !strings.Contains(string(rows[0].arguments), x) {
			t.Errorf("the close is %v needing %v after the unlink of %s (%s), want %v needing [0]", closing, rows[1].needs, x, rows[0].arguments, want)
		}
	})
}

// TestTheRowsOfAStatusOptionLandInTheOrderTheParentRuleNeedsAndAreUndone: the rows an option is
// written out as are approved and run as ordinary update rows, and with the parent status rule on
// or off every row lands - the status change route's order keeps the rule at each write, so no row
// is refused 409 PARENT_STATUS_CONFLICT - and the issues hold what the option asks. Undo walks the
// rows newest first, each by its own write back of the status or the parent the row found, and
// with the rule on no undo row is refused by the rule.
func TestTheRowsOfAStatusOptionLandInTheOrderTheParentRuleNeedsAndAreUndone(t *testing.T) {
	t.Parallel()
	for _, ruleOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("with the rule %s", map[bool]string{true: "on", false: "off"}[ruleOn]), func(t *testing.T) {
			t.Parallel()
			rig := newStatusOptionsRig(t, ruleOn)
			rig.requireRuleSetting(t, ruleOn)
			for index, scenario := range optionScenarios {
				t.Run(scenario.name, func(t *testing.T) {
					c := scenario.build(t, rig)
					keys := slices.Sorted(maps.Keys(c.after))
					before := rig.states(t, keys...)
					run := rig.ran(t, fmt.Sprintf("write out option scenario %d with the rule %v", index, ruleOn), goingChange("dispatch_issue_update", c.change))
					plan := rig.plan(t, run)
					if len(plan.Rows) != len(c.rows) {
						t.Fatalf("the plan holds %d rows, want %d", len(plan.Rows), len(c.rows))
					}
					for _, row := range plan.Rows {
						if row.State != "done" {
							t.Errorf("row %d is %s with %+v, want it done: every row lands, the rule never refusing one", row.Row, row.State, row.Result)
						}
					}
					if got := rig.states(t, keys...); !reflect.DeepEqual(got, c.after) {
						t.Fatalf("after the run the issues hold %v, want %v", got, c.after)
					}

					tools := slices.Repeat([]string{"dispatch_issue_update"}, len(c.rows))
					lines := rig.undone(t, run, tools...)
					for position, line := range lines {
						if want := len(c.rows) - 1 - position; line.Row != want {
							t.Errorf("undo line %d is of row %d, want row %d: Undo walks the rows newest first", position, line.Row, want)
						}
						if !line.Outcome.OK {
							t.Errorf("the undo of row %d answered %+v, want it written back", line.Row, line.Outcome)
						}
					}
					if got := rig.states(t, keys...); !reflect.DeepEqual(got, before) {
						t.Fatalf("after Undo the issues hold %v, want %v back", got, before)
					}
				})
			}
		})
	}
}

// requireRuleSetting fails t unless the rig's server has the parent status rule on or off as ruleOn
// says: a done parent over an open child, written by hand, is refused 409 PARENT_STATUS_CONFLICT
// with the rule on and written with it off.
func (rig goingPlanRig) requireRuleSetting(t *testing.T, ruleOn bool) {
	t.Helper()
	parent := rig.issue(t, "Parent the rule's control closes")
	rig.childOf(t, "Open child of the control", parent)
	response := dispatchRequest(t, rig.mux, http.MethodPatch, "/api/v1/issues/"+parent, map[string]any{"status": "done"}, "alice")
	refused := response.Code == http.StatusConflict && strings.Contains(response.Body.String(), "PARENT_STATUS_CONFLICT")
	if ruleOn != refused || (!ruleOn && response.Code != http.StatusOK) {
		t.Fatalf("closing a parent over an open child answered %d %s with the rule %v, want 409 PARENT_STATUS_CONFLICT only with it on",
			response.Code, response.Body.String(), ruleOn)
	}
}

// TestAnOptionThatMakesAPlanTooLongIsRefusedWithNothingKept: an option that reaches more issues
// than a plan holds rows, beside the plan's other changes, is refused naming the number of changes
// the plan would hold and the most it holds, and no plan is kept; the same option beside one change
// fewer is kept at the bound.
func TestAnOptionThatMakesAPlanTooLongIsRefusedWithNothingKept(t *testing.T) {
	t.Parallel()
	rig := newStatusOptionsRig(t, true)
	root := rig.issue(t, "Epic with many children")
	for index := range goingPlanMostRows - 2 {
		rig.childOf(t, fmt.Sprintf("Child %d", index), root)
	}
	other := rig.issue(t, "Issue beside the epic")
	option := goingChange("dispatch_issue_update", map[string]any{"issue": root, "status": "in_progress", "include_children": true})
	// The epic's row and one row for each of its children: goingPlanMostRows - 1 rows.
	t.Run("one change more than the bound is refused", func(t *testing.T) {
		run, refused := rig.proposedPlan(t, goingChange("dispatch_issue_update", retitle(other, "Retitled first")), option,
			goingChange("dispatch_issue_update", retitle(other, "Retitled last")))
		if refused == nil || refused.Code != agentwire.CodeCallRefused {
			t.Fatalf("the plan call answered %+v, want %s", refused, agentwire.CodeCallRefused)
		}
		numbers := strings.FieldsFunc(refused.Message, func(r rune) bool { return !unicode.IsDigit(r) })
		if !slices.Contains(numbers, fmt.Sprint(goingPlanMostRows+1)) || !slices.Contains(numbers, fmt.Sprint(goingPlanMostRows)) {
			t.Fatalf("the refusal says %q, want it to name the %d changes the plan would hold and the %d a plan holds at most",
				refused.Message, goingPlanMostRows+1, goingPlanMostRows)
		}
		if kept := rig.count(t, `select count(*) from going_plans where run_id = $1::uuid`, run); kept != 0 {
			t.Fatalf("the refused call kept %d plans, want none", kept)
		}
	})
	t.Run("a plan at the bound is kept", func(t *testing.T) {
		run, refused := rig.proposedPlan(t, goingChange("dispatch_issue_update", retitle(other, "Retitled at the bound")), option)
		if refused != nil {
			t.Fatalf("the plan call was refused %+v, want the plan kept", refused)
		}
		if rows := rig.keptRows(t, run); len(rows) != goingPlanMostRows {
			t.Fatalf("the plan holds %d rows, want %d", len(rows), goingPlanMostRows)
		}
	})
}

// TestAnExpandedChangeRenumbersTheChangesAfterIt: a later change that needs the expanded change
// needs each of its rows, a later change's need of another change and each "$N" it names are the
// new indexes of those changes, so the plan, approved whole, runs in order and the move under the
// issue a create makes after the expansion lands under that issue's key.
func TestAnExpandedChangeRenumbersTheChangesAfterIt(t *testing.T) {
	t.Parallel()
	rig := newStatusOptionsRig(t, true)
	root := rig.issue(t, "Epic to start with its parts")
	rig.childOf(t, "Part one", root)
	rig.childOf(t, "Part two", root)
	other := rig.issue(t, "Issue to move under the new epic")
	changes := []map[string]any{
		goingChange("dispatch_issue_update", map[string]any{"issue": root, "status": "in_progress", "include_children": true}),
		goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "New epic after the start", "force": true}, 0),
		goingChange("dispatch_issue_update", map[string]any{"issue": other, "parent": "$1"}, 1),
		goingChange("dispatch_issue_update", retitle(root, "Epic started with its parts"), 0),
	}

	t.Run("each later change names the new indexes", func(t *testing.T) {
		run, refused := rig.proposedPlan(t, changes...)
		if refused != nil {
			t.Fatalf("the plan call was refused %+v, want the plan kept", refused)
		}
		rows := rig.keptRows(t, run)
		wantNeeds := [][]int{nil, {0}, {0}, {0, 1, 2}, {3}, {0, 1, 2}}
		if len(rows) != len(wantNeeds) {
			t.Fatalf("the plan holds %d rows, want %d", len(rows), len(wantNeeds))
		}
		for index, row := range rows {
			if !slices.Equal(row.needs, wantNeeds[index]) {
				t.Errorf("row %d needs %v, want %v", index, row.needs, wantNeeds[index])
			}
		}
		var move map[string]any
		if err := json.Unmarshal(rows[4].arguments, &move); err != nil || move["parent"] != "$3" {
			t.Errorf("the move names %s (%v), want its parent \"$3\", the create's new index", rows[4].arguments, err)
		}
	})

	t.Run("the plan runs in order and the move lands under the new issue", func(t *testing.T) {
		run := rig.ran(t, "start the epic with its parts and file a new epic", changes...)
		plan := rig.plan(t, run)
		if states := rowStates(plan); !slices.Equal(states, slices.Repeat([]string{"done"}, 6)) {
			t.Fatalf("the rows are %v, want every one done; the calls were told %q", states, rig.toolResults(t, run))
		}
		var created string
		if err := rig.database.Pool.QueryRow(context.Background(), `select key from issues where title = 'New epic after the start'`).Scan(&created); err != nil {
			t.Fatalf("read the issue the run made: %v", err)
		}
		if moved := rig.states(t, other)[other].parent; moved != created {
			t.Fatalf("the moved issue's parent is %q, want %s, the issue the plan made", moved, created)
		}
	})
}

// TestAChangeWithAnOptionIsHeldToItsToolsRuleBeforeItIsWrittenOut: a change whose call the tool's
// words refuse - an option with a field of its own, with no status, or an issue that is not there -
// is refused as the call is refused anywhere, in those words, naming the change by its number in the
// plan as the model wrote it, and no plan is kept.
func TestAChangeWithAnOptionIsHeldToItsToolsRuleBeforeItIsWrittenOut(t *testing.T) {
	t.Parallel()
	rig := newStatusOptionsRig(t, true)
	issue := rig.issue(t, "Issue under the rule")
	for _, tc := range []struct {
		name    string
		changes []map[string]any
		refusal string
	}{
		{"an option beside a field of its own", []map[string]any{goingChange("dispatch_issue_update",
			map[string]any{"issue": issue, "status": "todo", "include_parent": true, "title": "Retitled"})},
			"change 0: With include_parent, give status alone, and reason when the status is done. This call also names title. Send those fields in a call of their own."},
		{"an option with no status", []map[string]any{goingChange("dispatch_issue_update", map[string]any{"issue": issue, "unlink_children": true})},
			"change 0: status is required with unlink_children. Add status, or drop unlink_children."},
		{"include_children with done", []map[string]any{goingChange("dispatch_issue_update",
			map[string]any{"issue": issue, "status": "done", "reason": "Finished.", "include_children": true})},
			"change 0: " + goingOptionsDoneRule},
		{"an issue that is not there, after a change that is kept", []map[string]any{
			goingChange("dispatch_issue_update", retitle(issue, "Retitled first")),
			goingChange("dispatch_issue_update", map[string]any{"issue": "DEMO-99999", "status": "todo", "include_children": true})},
			"change 1: not found"},
		{"an option on the issue a create of the plan makes", []map[string]any{
			goingChange("dispatch_issue", map[string]any{"project": "DEMO", "title": "Made by the plan", "force": true}),
			goingChange("dispatch_issue_update", map[string]any{"issue": "$0", "status": "todo", "include_children": true}, 0)},
			`change 1 (dispatch_issue_update): argument "issue" is "$0", but "issue" names the item this change changes, which the plan reads before anything runs: ` +
				`a new issue may only be a value a change writes, such as parent or blocked_by; propose a change of the new issue itself once it exists`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, refused := rig.proposedPlan(t, tc.changes...)
			switch {
			case refused == nil || refused.Code != agentwire.CodeCallRefused:
				t.Fatalf("the plan call answered %+v, want %s", refused, agentwire.CodeCallRefused)
			case refused.Message != tc.refusal:
				t.Fatalf("the plan call was refused %q, want %q", refused.Message, tc.refusal)
			}
			if kept := rig.count(t, `select count(*) from going_plans where run_id = $1::uuid`, run); kept != 0 {
				t.Fatalf("the refused call kept %d plans, want none", kept)
			}
		})
	}
}

// TestACallThatReachesARowWithAnOptionIsStillRefusedBeforeAnyRequest: no row of a plan carries an
// option, since a plan call writes the change out first, so a call that reaches a row with one
// anyway is refused before a request is made (goingUnsendable), where a call of the same tool
// without it is sent.
func TestACallThatReachesARowWithAnOptionIsStillRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range goingStatusOptions {
		t.Run(option, func(t *testing.T) {
			doer := &countingDoer{}
			_, err := GoingCaller{Catalogue: catalogue, Session: goingPlanner, Doer: doer}.Do(context.Background(), "dispatch_issue_update",
				map[string]any{"issue": "DSP-1", "status": "todo", option: true})
			var refused GoingArgumentError
			if !errors.As(err, &refused) || refused.Reason != goingUnsendable("dispatch_issue_update", map[string]any{option: true}) || len(doer.requests) != 0 {
				t.Fatalf("the call answered %v after %d request(s), want the bar's refusal and none sent", err, len(doer.requests))
			}
		})
	}
	t.Run("the call without the option is sent", func(t *testing.T) {
		doer := &countingDoer{}
		if _, err := (GoingCaller{Catalogue: catalogue, Session: goingPlanner, Doer: doer}).Do(context.Background(), "dispatch_issue_update",
			map[string]any{"issue": "DSP-1", "status": "todo", "include_children": false}); err != nil || len(doer.requests) != 1 {
			t.Fatalf("the call answered %v after %d request(s), want it sent once", err, len(doer.requests))
		}
	})
}
