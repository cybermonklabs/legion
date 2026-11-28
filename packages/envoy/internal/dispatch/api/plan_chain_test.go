package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// planChainTask is one plan task of plan.md in TestEveryPlanTaskUnderAReplyOrAnAskAnswersItsPlanAfterItsNeeds:
// its block id, the requirement of the spec its serves names, which places it under that
// requirement, the tasks its needs names, by block id, and whether it stands in the test's cycle.
type planChainTask struct {
	id, requirement string
	needs           []string
	cycle           bool
}

// planChainTasks are the tasks of plan.md, in their document's order, one chain under each
// requirement: r-top under the issue, r-reply under a reply to a comment, r-ask under an ask. Each
// task's needs names tasks written after it alone, so a read that keeps the document's order puts
// each of t-top and t-reply before a task it needs. The c tasks need each other around the three
// chains, c-top c-ask, c-ask c-reply and c-reply c-top: one cycle.
var planChainTasks = []planChainTask{
	{id: "t-top", requirement: "r-top", needs: []string{"t-reply", "t-ask"}},
	{id: "t-reply", requirement: "r-reply", needs: []string{"t-ask"}},
	{id: "t-ask", requirement: "r-ask"},
	{id: "c-top", requirement: "r-top", needs: []string{"c-ask"}, cycle: true},
	{id: "c-reply", requirement: "r-reply", needs: []string{"c-top"}, cycle: true},
	{id: "c-ask", requirement: "r-ask", needs: []string{"c-reply"}, cycle: true},
}

// planChainMarkdown is plan.md of the issue key: each of planChainTasks, its serves naming its
// requirement in the spec, and its needs each task it needs at plan.md's own address.
func planChainMarkdown(key string) string {
	spec, plan := "dispatch://"+key+"/spec#b-", "dispatch://"+key+"/artifact/plan-md#b-"
	var written strings.Builder
	for _, task := range planChainTasks {
		needs := ""
		if len(task.needs) > 0 {
			pointers := make([]string, len(task.needs))
			for index, need := range task.needs {
				pointers[index] = plan + need
			}
			needs = ` needs="` + strings.Join(pointers, " ") + `"`
		}
		fmt.Fprintf(&written, ":::plan_task{#%s serves=\"%s%s\"%s}\nThe step %s.\n:::\n\n", task.id, spec, task.requirement, needs, task.id)
	}
	return written.String()
}

// TestEveryPlanTaskUnderAReplyOrAnAskAnswersItsPlanAfterItsNeeds holds the plan view to R11 of
// INFRA-1253's spec, a task after every task in its needs, for a task that reaches its issue
// through the conversation (INFRA-1098's architect, dispatch://INFRA-1253/comment/84b22a85-9114-4d9f-a9f3-d51b79abc476,
// item 1, on the finding dispatch://INFRA-1253/comment/2255a52f-f824-418c-a635-d6d3e6cd37d3). On
// converted copies the plan view answered no `plan` on a task under a requirement under a reply to
// a comment, or under an ask: the ordered read walked down from the issue's node and from the
// blocks of its conversation alone, and no part_of of an item it hangs under (a reply under the
// comment its reply_to names, a comment or an ask under its issue) placed that task in its tree,
// so the page drew it before a task it needs.
//
// The issue holds r-top under its node, a person's comment with a person's reply to it, and an
// agent's ask; r-reply's parent attribute names the reply and r-ask's the ask. plan.md writes the
// tasks of planChainTasks, each under the requirement its serves names: the chains task ->
// requirement -> reply -> comment -> issue, task -> requirement -> ask -> issue and task ->
// requirement -> issue, with needs between them, and a cycle around the three. The tree read with
// no view places each node so, or the case proves nothing.
//
// The issue is born converted in one case, and in the other created unconverted, written, and
// converted by docs.ConvertIssue, as the copies were. In each, three reads of
// `GET /api/v1/issues/{key}/tree?view=plan` answer the plan of each node from a statement of their
// own: the head read, from the view's head rows (refs.PlanOrderOfRows); the same read with the
// issue's head mark taken away, which walks the tuples; and the read at the plan query's version,
// which climbs the head projection from every plan task of the tree's documents
// (requirePlanChainView). Each answers every live plan_task with a `plan`; each task of the cycle
// marked a cycle with no position; each other task at a position after the position of each task
// its needs names; and each task's needs as its opening line writes them, beside its issue's start.
//
// The control: the climb that hangs no conversation item under the node its row names (planHangs
// of none) reaches the issue from no task under r-reply or r-ask, so the view answers each with no
// `plan`, and this test fails naming each.
func TestEveryPlanTaskUnderAReplyOrAnAskAnswersItsPlanAfterItsNeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	service := deps.Docs.(*docs.Service)
	settle := func(t *testing.T, key string) {
		t.Helper()
		settleIssue(t, service, key)
		reconcileBlockIndexPass(t, deps)
	}
	for _, seeded := range []struct {
		name, project string
		converts      bool
	}{{"an issue born converted", "PCHB", false}, {"an issue the conversion converts", "PCHC", true}} {
		t.Run(seeded.name, func(t *testing.T) {
			createRoomProject(t, handler, seeded.project)
			key := seeded.project + "-1"
			release := func() {}
			if seeded.converts {
				release = holdUnconverted(t, database)
			}
			issue, tree := createRoomIssue(t, handler, database, seeded.project, "Plan tasks under the conversation",
				":::requirement{#r-top}\nThe board pages its asks.\n:::\n", "")
			if issue.Key != key {
				t.Fatalf("the project's first issue is %s, want %s, which its pointers name", issue.Key, key)
			}
			if born := tree != ""; born == seeded.converts {
				t.Fatalf("%s holds the tree %q, want it born converted %t: this case proves nothing", key, tree, !seeded.converts)
			}
			head, _ := personComment(t, handler, database, key, "The board must page.")
			reply := createThreadComment(t, handler, key, map[string]any{"body": "Page it by twenty.", "reply_to": head}, "alice").ID
			ask := createAnswerListAsk(t, handler, key, "Which column sorts the page?").ID
			editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#r-reply parent=\"dispatch://" + key + "/comment/" + reply + "\"}\n" +
				"The board pages by twenty.\n:::\n\n:::requirement{#r-ask parent=\"dispatch://" + key + "/ask/" + ask + "\"}\nThe page sorts by the column asked.\n:::\n"}})
			uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", planChainMarkdown(key))
			if seeded.converts {
				if document := readDecisionDocument(t, database, key, "plan-md"); document.room != document.id {
					t.Fatalf("the plan of %s is a fragment of the room %s before the conversion: this case proves nothing", key, document.room)
				}
				release()
				settle(t, key)
				converted, err := service.ConvertIssue(ctx, key, docs.IssueRoomGate, false)
				if err != nil || converted.Outcome != docs.IssueConverted {
					t.Fatalf("the conversion of %s answered %+v, %v; want it converted", key, converted, err)
				}
			}
			settle(t, key)
			spec, plan := readDecisionDocument(t, database, key, "spec"), readDecisionDocument(t, database, key, "plan-md")
			if plan.room == plan.id {
				t.Fatalf("the plan of %s is a room of its own, want a fragment of the issue's room: this case proves nothing", key)
			}

			page := treeRead(t, handler, key, "", "alice", false)
			requireUnder := func(what string, node, parent refs.Target) {
				t.Helper()
				held, _ := page.node(t, node.Kind, node.ID)
				if held.Parent == nil || held.Parent.Kind != parent.Kind || held.Parent.ID != parent.ID {
					t.Fatalf("the tree read of %s places %s (%s %s) under %+v, want %s %s: this case proves nothing", key, what, node.Kind, node.ID, held.Parent, parent.Kind, parent.ID)
				}
			}
			issueNode := refs.Target{Kind: graphstore.KindIssue, ID: key}
			comment := refs.Target{Kind: graphstore.KindComment, ID: head}
			replied := refs.Target{Kind: graphstore.KindComment, ID: reply}
			asked := refs.Target{Kind: graphstore.KindAsk, ID: ask}
			requireUnder("the comment", comment, issueNode)
			requireUnder("the reply", replied, comment)
			requireUnder("the ask", asked, issueNode)
			requirements := map[string]refs.Target{}
			for name, parent := range map[string]refs.Target{"r-top": issueNode, "r-reply": replied, "r-ask": asked} {
				requirements[name] = refs.Target{Kind: refs.BlockNodeKind, ID: spec.address(name)}
				requireUnder(name, requirements[name], parent)
			}
			for _, task := range planChainTasks {
				requireUnder(task.id, refs.Target{Kind: refs.BlockNodeKind, ID: plan.address(task.id)}, requirements[task.requirement])
			}

			rig := headRig{t: t, handler: handler, database: database}
			rig.refill(key)
			requirePlanChainView(t, treeRead(t, handler, key, "?view=plan", "alice", false), key, "the head read", plan)
			status, body := rig.withoutMark(t, key, true, func() (int, []byte) {
				response := treeResponse(t, handler, key, "?view=plan", "alice", false)
				return response.Code, response.Body.Bytes()
			})
			if status != http.StatusOK {
				t.Fatalf("the plan view of %s with its mark taken away: status=%d body=%s", key, status, cut(body))
			}
			walked := treeAnswer{body: string(body)}
			if err := json.Unmarshal(body, &walked.IssueTreeRead); err != nil {
				t.Fatalf("decode the plan view of %s with its mark taken away: %v", key, err)
			}
			requirePlanChainView(t, walked, key, "the walk of the tuples", plan)
			version := planQueryVersion(t, database, key)
			requirePlanChainView(t, treeRead(t, handler, key, fmt.Sprintf("?view=plan&v=%d", version), "alice", false), key,
				fmt.Sprintf("the read at version %d", version), plan)
		})
	}
}

// requirePlanChainView fails unless read, the plan view of key that how names, answers each task of
// planChainTasks, whose blocks plan holds, once, live and with a `plan`: a task of the cycle marked
// a cycle with no position, every other task at a position after the position of each task its
// needs names; and the needs of each task, beside its issue's start, the tasks its opening line
// names. Every live plan_task node of read answers a `plan`, and read holds the tasks alone.
func requirePlanChainView(t *testing.T, read treeAnswer, key, how string, plan decisionDocument) {
	t.Helper()
	positions := map[string]int{}
	for _, task := range planChainTasks {
		address := plan.address(task.id)
		nodes := read.nodesNamed(refs.BlockNodeKind, address)
		if len(nodes) != 1 {
			t.Errorf("%s of the plan view of %s answers %d nodes of %s (%s), want one", how, key, len(nodes), task.id, address)
			continue
		}
		node := nodes[0]
		if node.State != graphstore.StateLive {
			t.Errorf("%s of the plan view of %s answers %s at state %q, want live", how, key, task.id, node.State)
		}
		if node.Plan == nil {
			t.Errorf("%s of the plan view of %s answers %s, a live plan_task under %s, with no plan: the ordered read placed it in none of its order, its cycles or its overtaken",
				how, key, task.id, task.requirement)
			continue
		}
		var needs []string
		start := false
		for _, need := range node.Plan.Needs {
			switch {
			case need.Kind == refs.BlockNodeKind:
				needs = append(needs, need.ID)
			case need.Kind == graphstore.KindIssue && need.ID == key && need.Half == refs.HalfStart:
				start = true
			}
		}
		want := make([]string, len(task.needs))
		for index, need := range task.needs {
			want[index] = plan.address(need)
		}
		slices.Sort(needs)
		slices.Sort(want)
		if !slices.Equal(needs, want) || !start {
			t.Errorf("%s of the plan view of %s answers %s needing %+v, want the tasks %v and %s's start", how, key, task.id, node.Plan.Needs, want, key)
		}
		switch position := node.Plan.Position; {
		case task.cycle && (!node.Plan.Cycle || position != nil):
			t.Errorf("%s of the plan view of %s answers %s, of the cycle c-top c-ask c-reply, with cycle %t and position %v, want a cycle with no position",
				how, key, task.id, node.Plan.Cycle, describePosition(position))
		case !task.cycle && (node.Plan.Cycle || position == nil):
			t.Errorf("%s of the plan view of %s answers %s with cycle %t and position %v, want a position and no cycle", how, key, task.id, node.Plan.Cycle, describePosition(position))
		case position != nil:
			positions[task.id] = *position
		}
	}
	for _, task := range planChainTasks {
		at, placed := positions[task.id]
		if !placed {
			continue
		}
		for _, need := range task.needs {
			if before, held := positions[need]; !held || before >= at {
				t.Errorf("%s of the plan view of %s puts %s at %d and %s, which it needs, at %s: R11 wants each task after every task in its needs",
					how, key, task.id, at, need, describeHeld(before, held))
			}
		}
	}
	tasks := 0
	for _, node := range read.Nodes {
		if node.Kind != refs.BlockNodeKind || node.Supertag != planTaskBlockType {
			continue
		}
		tasks++
		if node.State == graphstore.StateLive && node.Plan == nil {
			t.Errorf("%s of the plan view of %s answers the live plan_task %s with no plan", how, key, node.ID)
		}
	}
	if tasks != len(planChainTasks) {
		t.Errorf("%s of the plan view of %s answers %d plan_task nodes, want the %d of plan.md", how, key, tasks, len(planChainTasks))
	}
}

// describePosition is a plan's position as a failure names it, "none" for null.
func describePosition(position *int) string {
	if position == nil {
		return "none"
	}
	return fmt.Sprint(*position)
}

// describeHeld is a position a failure names, "no position" where held is false.
func describeHeld(position int, held bool) string {
	if !held {
		return "no position"
	}
	return fmt.Sprint(position)
}
