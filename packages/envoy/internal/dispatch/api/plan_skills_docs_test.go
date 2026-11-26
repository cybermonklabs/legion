package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// writtenList is items as the renderer writes a `string[]` attribute's value between its quotes:
// the JSON array of strings json.Marshal writes, each quote a character reference (pmdoc's
// escapeDirectiveAttribute), which no other character of the items these tests write needs.
func writtenList(t *testing.T, items []string) string {
	t.Helper()
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("encode the list %q: %v", items, err)
	}
	return strings.ReplaceAll(string(encoded), `"`, "&#x22;")
}

// uploadPlan uploads markdown as the plan of the issue key, plan.md, as alice, and fails, naming
// the answer, unless the upload is taken.
func uploadPlan(t *testing.T, handler http.Handler, key, markdown string) {
	t.Helper()
	path := "/api/v1/issues/" + key + "/artifacts"
	response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
		"name": "plan.md", "content": markdown, "precondition": uploadBaseNamed(t, handler, path, "plan.md"),
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("the upload of the plan of %s answered %d: %s", key, response.Code, response.Body.String())
	}
}

// orderedNode is the plan node of order whose store address is address, in its order or in one
// of its cycles, and whether order holds it.
func orderedNode(order refs.TreeOrder, address string) (refs.OrderedNode, bool) {
	named := func(node refs.OrderedNode) bool { return node.Kind == refs.BlockNodeKind && node.ID == address }
	if index := slices.IndexFunc(order.Order, named); index >= 0 {
		return order.Order[index], true
	}
	for _, cycle := range order.Cycles {
		if index := slices.IndexFunc(cycle, named); index >= 0 {
			return cycle[index], true
		}
	}
	return refs.OrderedNode{}, false
}

// requireEmptyLists fails t unless skills and docs, the lists what answers for a node that names
// none, are each an empty list: `[]` in the JSON of an answer, never null and never left out.
func requireEmptyLists(t *testing.T, what string, skills, docs []string) {
	t.Helper()
	for _, list := range []struct {
		attribute string
		got       []string
	}{{refs.AttributePlanSkills, skills}, {refs.AttributePlanDocs, docs}} {
		switch {
		case list.got == nil:
			t.Errorf("%s answers no empty list of %s, which it holds none of", what, list.attribute)
		case len(list.got) != 0:
			t.Errorf("%s answers %s %q, want an empty list", what, list.attribute, list.got)
		}
	}
}

// requirePlanJSONLists fails t unless raw, a JSON object representing an IssueTreePlan in the plan
// view's body, writes both skills and docs as `[]`, neither null nor left out.
func requirePlanJSONLists(t *testing.T, what string, raw json.RawMessage) {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode %s %s: %v", what, raw, err)
	}
	for _, name := range []string{refs.AttributePlanSkills, refs.AttributePlanDocs} {
		got, present := members[name]
		if !present || string(got) != "[]" {
			t.Errorf("%s writes %s as %q (present=%t), want []", what, name, string(got), present)
		}
	}
}

// TestAPlanTasksSkillsAndDocsReadBackFromTheOrderedRead is c-ordered-read's proof that a plan node
// carries the skills and the docs its implementer should read (R18 of the issue tree's spec). An
// issue is created whose spec holds one requirement, and its plan is uploaded with two plan tasks
// that serve it: the first writes skills naming two skills and docs naming two repository paths,
// each as the renderer writes a `string[]` attribute, and the second writes neither. The ordered
// read (refs.PlanOrder) answers the first task's lists item for item and in order, and the
// second's two empty lists, never absent ones, each beside the task's needs - its issue's start -
// and its size in whole minutes; each half of the issue answers two empty lists too. The plan view
// (`GET /api/v1/issues/{key}/tree?view=plan`) answers the same lists on each task's node, and `[]`
// for each on the issue's own entry and on each of its two halves. The check's control - skills
// and docs dropped from plan_task in pmdoc/schema/blocks.json - has the parse refuse the upload,
// and the test fails naming the upload's answer before it reads anything.
func TestAPlanTasksSkillsAndDocsReadBackFromTheOrderedRead(t *testing.T) {
	ctx := context.Background()
	handler, database := blockIndexHandler(t)
	createRoomProject(t, handler, "READS")
	issue, _ := createRoomIssue(t, handler, database, "READS", "A plan that says what to read",
		":::requirement{#r1}\nEach task says what its implementer reads.\n:::\n", "")
	key := issue.Key
	serves := "dispatch://" + key + "/spec#b-r1"
	skills, docs := []string{"writing-plans", "dispatch"}, []string{"docs/testing.md", "packages/envoy/AGENTS.md"}
	uploadPlan(t, handler, key,
		":::plan_task{#t-reads serves=\""+serves+"\" size=\"1h30m\" skills=\""+writtenList(t, skills)+"\" docs=\""+writtenList(t, docs)+"\"}\n"+
			"The task that names what to read.\n:::\n\n"+
			":::plan_task{#t-bare serves=\""+serves+"\" size=\"45m\"}\nThe task that names nothing to read.\n:::\n")
	document := readDecisionDocument(t, database, key, "plan-md")
	tasks := []struct {
		id, size     string
		skills, docs []string
	}{
		{id: "t-reads", size: "90", skills: skills, docs: docs},
		{id: "t-bare", size: "45", skills: []string{}, docs: []string{}},
	}

	order, err := refs.PlanOrder(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, key)
	if err != nil {
		t.Fatalf("read the order of %s: %v", key, err)
	}
	start := refs.OrderNode{Kind: graphstore.KindIssue, ID: key, Half: refs.HalfStart}
	for _, task := range tasks {
		node, held := orderedNode(order, document.address(task.id))
		if !held {
			t.Fatalf("the ordered read of %s answers no node for %s (%s)", key, task.id, document.address(task.id))
		}
		for _, list := range []struct {
			attribute string
			got       []string
		}{{refs.AttributePlanSkills, node.Skills}, {refs.AttributePlanDocs, node.Docs}} {
			if list.got == nil {
				t.Errorf("the ordered read of %s answers no empty list of %s for %s, which holds none", key, list.attribute, task.id)
			}
		}
		if !slices.Equal(node.Skills, task.skills) || !slices.Equal(node.Docs, task.docs) {
			t.Errorf("the ordered read of %s answers %s with skills %q and docs %q, want %q and %q", key, task.id, node.Skills, node.Docs, task.skills, task.docs)
		}
		if node.Size != task.size || !slices.Contains(node.Needs, start) {
			t.Errorf("the ordered read of %s answers %s with the size %q and the needs %+v, want the size %s in minutes and the start of %s among its needs",
				key, task.id, node.Size, node.Needs, task.size, key)
		}
	}
	for _, half := range []string{refs.HalfStart, refs.HalfDone} {
		want := refs.OrderNode{Kind: graphstore.KindIssue, ID: key, Half: half}
		index := slices.IndexFunc(order.Order, func(node refs.OrderedNode) bool { return node.OrderNode == want })
		if index < 0 {
			t.Fatalf("the ordered read of %s answers no %s half for the issue", key, half)
		}
		requireEmptyLists(t, fmt.Sprintf("the ordered read of %s, for the issue's %s half,", key, half),
			order.Order[index].Skills, order.Order[index].Docs)
	}
	view := treeRead(t, handler, key, "?view=plan", "alice", false)
	for _, task := range tasks {
		nodes := view.nodesNamed(refs.BlockNodeKind, document.address(task.id))
		if len(nodes) != 1 || nodes[0].Plan == nil {
			t.Errorf("the plan view of %s answers %s as %+v, want one node carrying its place in the order", key, task.id, nodes)
			continue
		}
		for _, list := range []struct {
			attribute string
			got, want []string
		}{{refs.AttributePlanSkills, nodes[0].Plan.Skills, task.skills}, {refs.AttributePlanDocs, nodes[0].Plan.Docs, task.docs}} {
			if list.got == nil || !slices.Equal(list.got, list.want) {
				t.Errorf("the plan view of %s answers %s with %s %q, want %q", key, task.id, list.attribute, list.got, list.want)
			}
		}
		if size := nodes[0].Attributes[refs.AttributePlanSize]; size != task.size {
			t.Errorf("the plan view of %s answers %s with %s %q, want %q", key, task.id, refs.AttributePlanSize, size, task.size)
		}
	}
	issueNode, _ := view.node(t, graphstore.KindIssue, key)
	if issueNode.Plan == nil || issueNode.Plan.Start == nil || issueNode.Plan.Done == nil {
		t.Fatalf("the plan view of %s answers the issue node with plan %+v, want its entry and both halves", key, issueNode.Plan)
	}
	requireEmptyLists(t, fmt.Sprintf("the plan view of %s, for the issue's own entry,", key),
		issueNode.Plan.Skills, issueNode.Plan.Docs)
	requireEmptyLists(t, fmt.Sprintf("the plan view of %s, for the issue's start half,", key),
		issueNode.Plan.Start.Skills, issueNode.Plan.Start.Docs)
	requireEmptyLists(t, fmt.Sprintf("the plan view of %s, for the issue's done half,", key),
		issueNode.Plan.Done.Skills, issueNode.Plan.Done.Docs)

	// Check the raw JSON body so neither a missing member nor null can pass for `[]`.
	var rawBody struct {
		Nodes []struct {
			Kind string          `json:"kind"`
			ID   string          `json:"id"`
			Plan json.RawMessage `json:"plan"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(view.body), &rawBody); err != nil {
		t.Fatalf("decode the raw plan view body of %s: %v", key, err)
	}
	for _, rawNode := range rawBody.Nodes {
		switch {
		case rawNode.Kind == refs.BlockNodeKind && rawNode.ID == document.address("t-bare"):
			requirePlanJSONLists(t, fmt.Sprintf("the plan view JSON of %s for t-bare", key), rawNode.Plan)
		case rawNode.Kind == graphstore.KindIssue && rawNode.ID == key:
			requirePlanJSONLists(t, fmt.Sprintf("the plan view JSON of %s for the issue's own entry", key), rawNode.Plan)
			var halves struct {
				Start json.RawMessage `json:"start"`
				Done  json.RawMessage `json:"done"`
			}
			if err := json.Unmarshal(rawNode.Plan, &halves); err != nil {
				t.Fatalf("decode the raw halves of %s: %v", key, err)
			}
			requirePlanJSONLists(t, fmt.Sprintf("the plan view JSON of %s for the issue's start half", key), halves.Start)
			requirePlanJSONLists(t, fmt.Sprintf("the plan view JSON of %s for the issue's done half", key), halves.Done)
		}
	}
}
