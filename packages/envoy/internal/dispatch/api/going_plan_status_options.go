package api

// going_plan_status_options.go writes out, as the rows of a plan, what the options of
// dispatch_issue_update ask for (R4, INFRA-1588). The status change route behind the options
// (POST /api/v1/issues/{key}/status-change) writes several issues, each in a transaction of its
// own, so one landing of one request cannot record it and Undo's write back of one issue cannot
// cover it: the bar never calls it. A plan call that sets include_children, unlink_children or
// include_parent to true is not refused; planCall turns that one change into one update row for
// each issue the option changes, before it reads any row, so every row is an ordinary update with
// its own approval, its own landing and its own Undo, which walks the rows newest first.
//
// Which issues an option reaches, and in which order the writes keep the parent status rule (R24
// of dispatch://INFRA-3320/spec), are the route's own: expandStatusOptions reads the plan with
// readStatusChange and statusChangeRead.steps, the two functions the route plans its call with, so
// the bar and the route cannot disagree. It reads them from the store, not through the route with
// dry_run: a dry run answers each issue's new status and parent but not the option that moved it,
// which a row's needs are made from, and it answers the rule's conflict alone, with no steps, when
// the plan breaks the rule, even where the deployment's rule is off, so it could give the bar no
// rows there. The person's own read of what a row shows is made later, row by row, as every row of
// a plan is read (goingReadRow); the named issue is read as the person here, so a refusal of it is
// the route's words. The bar's person is a sign-in or a personal token, which names no project
// (callerProject), so the route's project scope (planStatusChange leaves out each step whose issue
// a credential naming one project does not cover) has nothing to hold the steps to.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agentwire"
)

// goingIssueUpdateTool is the one tool whose change an option of the status change expands.
const goingIssueUpdateTool = "dispatch_issue_update"

// goingNeedsRefusal is the refusal of a change that needs a change that is not before it, in the
// model's numbering of its plan.
func goingNeedsRefusal(index, need int) *agentwire.Error {
	return goingAgentRefusal(agentwire.CodeCallRefused,
		"change %d needs change %d, which is not a change before it: list each change after the changes it needs", index, need)
}

// goingExpansion is a plan call's changes as expandStatusOptions writes them out: the rows so far,
// and for each of the model's changes before the one in hand the index of its first row and how
// many rows it became, which a later change's needs and "$N" are renumbered by.
type goingExpansion struct {
	rows         []agentwire.PlanChange
	first, count []int
	// total is how many changes the model proposed.
	total int
}

// shifted is the rows of the expanded plan that a change at index in hand names as the model's
// change n: every row of an earlier change, so a need of one that became several rows waits for
// all of them, and for a number no earlier change has, a number that no row before this change's
// first row has either, so the plan call refuses it as it would have refused the model's own.
func (e *goingExpansion) shifted(index, n int) []int {
	switch {
	case n < 0, n > math.MaxInt32:
		return []int{n}
	case n < index:
		rows := make([]int, e.count[n])
		for offset := range rows {
			rows[offset] = e.first[n] + offset
		}
		return rows
	}
	return []int{n + len(e.rows) - index}
}

// needsOf is needs, the model's numbers of the changes the change at index in hand needs, as the
// expanded plan numbers them.
func (e *goingExpansion) needsOf(index int, needs []int) []int {
	if len(needs) == 0 {
		return needs
	}
	var shifted []int
	for _, need := range needs {
		shifted = append(shifted, e.shifted(index, need)...)
	}
	slices.Sort(shifted)
	return slices.Compact(shifted)
}

// renumbered is arguments of tool with each "$N" it names (goingNewIssueRef, in the arguments that
// take one) put as the index of that change's first row in the expanded plan, and whether any
// moved. A create is never expanded, so its one row is the row the "$N" names; a value no create
// of the plan names keeps meaning nothing, and the plan call refuses it as before.
func (e *goingExpansion) renumbered(index int, tool string, arguments map[string]any) (map[string]any, bool) {
	moved := false
	put := func(value any) any {
		created, named := goingNewIssueRef(value)
		if !named || created < 0 {
			return value
		}
		if target := e.shifted(index, created)[0]; target != created {
			moved = true
			return "$" + strconv.Itoa(target)
		}
		return value
	}
	resolved := maps.Clone(arguments)
	for _, argument := range goingNewIssueArguments[tool] {
		value, given := arguments[argument]
		if !given {
			continue
		}
		list, isList := value.([]any)
		if !isList {
			resolved[argument] = put(value)
			continue
		}
		items := make([]any, len(list))
		for position, item := range list {
			items[position] = put(item)
		}
		resolved[argument] = items
	}
	return resolved, moved
}

// expandStatusOptions is changes, the changes a plan call proposes, with each dispatch_issue_update
// change that sets include_children, unlink_children or include_parent to true written out as the
// update rows of the issues the option changes, in the status change route's order (statusChange
// Read.steps): the ancestors include_parent moves, highest first; the unlinks; for a status that is
// done or not started, the descendants include_children moves, deepest first; the named issue,
// whose row carries the call's status and, for done, its reason; and for a status in work, the
// descendants include_children moves, highest first. A row writes one field: a status, or for an
// unlink the parent, "" for none. An issue the option would leave as it stands gets no row.
//
// Each row needs the earlier rows of its expansion whose issue is its issue's parent or its
// child, so a row whose neighbour's write failed is skipped (R4) and none is written against the
// parent status rule; and it inherits the needs of the change it came from. Every later change is
// renumbered: a need of the expanded change becomes a need of each of its rows, every other
// index shifts, and each "$N" it names is put as the new index of that create.
//
// Before a change is expanded it is held to what the loop holds every change to, in the model's
// own numbering and words: its needs, a "$N" it may not name, its tool's schema and its tool's rule,
// the options' part of it included (goingIssueUpdateProblem), so a call the tool's words refuse is
// refused in those words. A change that sets no option, and a plan that holds none, are returned
// as they were. A plan that the expansion makes hold more than goingPlanMostRows rows is refused,
// naming the count and the bound.
func (a *AgentListener) expandStatusOptions(ctx context.Context, doer GoingDoer, changes []agentwire.PlanChange) ([]agentwire.PlanChange, *agentwire.Error) {
	e := &goingExpansion{
		rows: make([]agentwire.PlanChange, 0, len(changes)), first: make([]int, len(changes)), count: make([]int, len(changes)),
		total: len(changes),
	}
	// earlier is the model's changes before the one in hand as the check of a "$N" reads them:
	// by their own numbers, with their tools.
	earlier := make([]goingPlanRowStored, 0, len(changes))
	for index, change := range changes {
		arguments, decodeErr := goingRowArguments(change.Arguments)
		var options []string
		if decodeErr == nil && change.Tool == goingIssueUpdateTool {
			options = goingStatusOptionsOf(arguments)
		}
		var rows []agentwire.PlanChange
		if len(options) == 0 {
			kept := change
			kept.Needs = e.needsOf(index, change.Needs)
			if decodeErr == nil {
				if renumbered, moved := e.renumbered(index, change.Tool, arguments); moved {
					encoded, err := json.Marshal(renumbered)
					if err != nil {
						return nil, goingAgentFailed(agentwire.CallCall, err)
					}
					kept.Arguments = encoded
				}
			}
			rows = []agentwire.PlanChange{kept}
		} else {
			var refusal *agentwire.Error
			if rows, refusal = a.statusOptionRows(ctx, doer, e, index, change, arguments, options, earlier); refusal != nil {
				return nil, refusal
			}
		}
		e.first[index], e.count[index] = len(e.rows), len(rows)
		e.rows = append(e.rows, rows...)
		earlier = append(earlier, goingPlanRowStored{row: index, tool: change.Tool})
	}
	return e.rows, nil
}

// statusOptionRows is the update rows of change, the change at index in hand, which sets options
// (expandStatusOptions): the refusal of the change in the model's words, or its rows.
func (a *AgentListener) statusOptionRows(ctx context.Context, doer GoingDoer, e *goingExpansion, index int, change agentwire.PlanChange,
	arguments map[string]any, options []string, earlier []goingPlanRowStored,
) ([]agentwire.PlanChange, *agentwire.Error) {
	refused := func(err error) *agentwire.Error {
		refusal := goingCallRefusal(change.Tool, err)
		refusal.Message = fmt.Sprintf("change %d: %s", index, refusal.Message)
		return refusal
	}
	entry, held := a.s.going.Lookup(change.Tool)
	if !held {
		return nil, goingAgentRefusal(agentwire.CodeCallRefused, "change %d names %q, which this server does not run", index, change.Tool)
	}
	needs := slices.Clone(change.Needs)
	slices.Sort(needs)
	needs = slices.Compact(needs)
	for _, need := range needs {
		if need < 0 || need >= index {
			return nil, goingNeedsRefusal(index, need)
		}
	}
	if why := goingNewIssueRefusal(entry, index, arguments, needs, earlier); why != "" {
		return nil, goingAgentRefusal(agentwire.CodeCallRefused, "change %d (%s): %s", index, change.Tool, why)
	}
	if reason := checkGoingArguments(entry.Request, arguments); reason != "" {
		return nil, refused(GoingArgumentError{Action: change.Tool, Reason: reason})
	}
	if reason := goingToolRules[change.Tool](arguments, entry.rule); reason != "" {
		return nil, refused(GoingArgumentError{Action: change.Tool, Reason: reason})
	}

	// The named issue is read as the person, so an issue they may not read, or that is not there,
	// is refused in the route's words and the key the rows name is the issue's own.
	var named struct {
		Key string `json:"key"`
	}
	key := goingText(arguments, "issue")
	if err := (goingReader{doer: doer}).get(ctx, goingIssuePath(key), &named); err != nil {
		return nil, refused(err)
	}
	key = cmp.Or(named.Key, key)
	pool, err := a.s.readyPool()
	if err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	status := statusChange{
		status:          goingText(arguments, "status"),
		includeChildren: slices.Contains(options, optionIncludeChildren),
		unlinkChildren:  slices.Contains(options, optionUnlinkChildren),
		includeParent:   slices.Contains(options, optionIncludeParent),
	}
	read, err := readStatusChange(ctx, pool, key, status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, goingAgentRefusal(agentwire.CodeCallRefused, "change %d: %s was removed while this plan was made: propose it again", index, key)
	}
	if err != nil {
		return nil, goingAgentFailed(agentwire.CallCall, err)
	}
	steps := read.steps(status)
	if rowsHeld := len(e.rows) + len(steps) + e.total - index - 1; rowsHeld > goingPlanMostRows {
		return nil, goingAgentRefusal(agentwire.CodeCallRefused,
			"change %d (%s with %s) is written out as %d changes, one for each issue it changes, and the plan would then hold %d changes, "+
				"where a plan holds at most %d: propose part of it now, and the rest once it ran",
			index, change.Tool, strings.Join(options, ", "), len(steps), rowsHeld, goingPlanMostRows)
	}

	inherited := e.needsOf(index, change.Needs)
	parents := make([]string, len(steps))
	rows := make([]agentwire.PlanChange, 0, len(steps))
	for position, step := range steps {
		if step.parentBefore != nil {
			parents[position] = *step.parentBefore
		}
		written := map[string]any{"issue": step.key}
		if step.reparent {
			written["parent"] = ""
		} else {
			written["status"] = *step.statusAfter
		}
		if reason, given := arguments["reason"]; given && step.option == "" {
			written["reason"] = reason
		}
		encoded, err := json.Marshal(written)
		if err != nil {
			return nil, goingAgentFailed(agentwire.CallCall, err)
		}
		needed := slices.Clone(inherited)
		for before := range position {
			if steps[before].key == parents[position] || parents[before] == step.key {
				needed = append(needed, len(e.rows)+before)
			}
		}
		slices.Sort(needed)
		rows = append(rows, agentwire.PlanChange{Tool: change.Tool, Arguments: encoded, Needs: slices.Compact(needed)})
	}
	return rows, nil
}
