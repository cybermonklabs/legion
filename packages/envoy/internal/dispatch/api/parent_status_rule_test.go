// parent_status_rule_test.go holds the tests of R24, the rule between the status of a parent and
// the statuses of its children: the group of each status, each writer of a status or a parent,
// and the PATCH's and the create's refusals of a write that breaks the rule.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestEveryStatusFallsInOneGroup: each status of model.IssueStatuses has its group in the table
// below, each row of the table is a status, and statusGroup gives each status the group of its
// row. A status added to the lifecycle fails this test until it has a row.
func TestEveryStatusFallsInOneGroup(t *testing.T) {
	groups := map[string]group{
		"triage":       groupNotStarted,
		"icebox":       groupNotStarted,
		"backlog":      groupNotStarted,
		"todo":         groupInWork,
		"in_progress":  groupInWork,
		"testing":      groupInWork,
		"needs_review": groupInWork,
		"retro":        groupInWork,
		"done":         groupDone,
	}
	for _, status := range slices.Sorted(maps.Keys(groups)) {
		if !model.IsIssueStatus(status) {
			t.Errorf("the table has a row for %q, which is not a status of model.IssueStatuses", status)
			continue
		}
		if got, want := statusGroup(status), groups[status]; got != want {
			t.Errorf("statusGroup(%q) = %q, want %q", status, got, want)
		}
	}
	for _, status := range model.IssueStatuses {
		if _, listed := groups[status]; !listed {
			t.Errorf("the status %q of model.IssueStatuses has no row in the table: give it its group", status)
		}
	}
	if got := statusGroup("closed"); got != "" {
		t.Errorf(`statusGroup("closed") = %q, want no group for a string that is not a status`, got)
	}
}

// TestEveryWriteOfAStatusOrAParentKeepsTheRule: with DISPATCH_PARENT_STATUS_RULE on, each writer
// of an issue's status or parent refuses a write that breaks R24 with 409
// PARENT_STATUS_CONFLICT, and the refusal names the issue at the other end of the pair. This is
// true whoever sends the write: a person signed in, a personal token or a broker token. The
// writers are the PATCH with a status, the PATCH with a parent, the create with a parent and
// the status-change route. A grep of each non-test `update issues`, `insert into issues`,
// `set("parent_key"` and `"status = "` in packages/envoy found no other writer.
//
// Each case seeds issues of its own, a parent before its child, so each seed keeps the rule
// and only the case's own write breaks it.
func TestEveryWriteOfAStatusOrAParentKeepsTheRule(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{parentStatusRule: true})
	agent := newBrokerCaller(t)
	const brokerToken = "a-token-that-writes-statuses"
	rig.broker.Says(brokerToken, liveNamedToken(agent.jkt))
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
	}
	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "laptop"})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a personal token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := decodeBody[agentTokenResponse](t, minted).Token

	// seed files an issue as the person, under parent when parent is not empty, and moves it
	// to status.
	seed := func(t *testing.T, title, status, parent string) string {
		t.Helper()
		body := map[string]any{"project": brokerTestProject, "title": title, "force": true}
		if parent != "" {
			body["parent"] = parent
		}
		created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", body)
		if created.Code != http.StatusCreated {
			t.Fatalf("seed %q: status=%d body=%s", title, created.Code, created.Body.String())
		}
		key := decodeBody[struct {
			Key string `json:"key"`
		}](t, created).Key
		if status != model.IssueStatusTriage {
			moved := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": status})
			if moved.Code != http.StatusOK {
				t.Fatalf("move the seed %s to %s: status=%d body=%s", key, status, moved.Code, moved.Body.String())
			}
		}
		return key
	}

	// breakingWrite is one write that breaks R24, and holds is the key of the issue that its
	// refusal must name.
	type breakingWrite struct {
		method, path string
		body         map[string]any
		holds        string
	}
	writers := []struct {
		name string
		// write seeds the case's issues, titled after label, and gives the write that breaks
		// the rule over them.
		write func(t *testing.T, label string) breakingWrite
	}{
		{"PATCH with a status: a parent to done over a child in todo", func(t *testing.T, label string) breakingWrite {
			parent := seed(t, label+" parent", "in_progress", "")
			child := seed(t, label+" child", "todo", parent)
			return breakingWrite{http.MethodPatch, "/api/v1/issues/" + parent, map[string]any{"status": "done"}, child}
		}},
		{"PATCH with a parent: a child in todo under a done parent", func(t *testing.T, label string) breakingWrite {
			parent := seed(t, label+" done parent", "done", "")
			child := seed(t, label+" child", "todo", "")
			return breakingWrite{http.MethodPatch, "/api/v1/issues/" + child, map[string]any{"parent": parent}, parent}
		}},
		{"create with a parent: an issue filed under a done parent", func(t *testing.T, label string) breakingWrite {
			parent := seed(t, label+" done parent", "done", "")
			return breakingWrite{http.MethodPost, "/api/v1/issues", map[string]any{
				"project": brokerTestProject, "title": label + " new child", "parent": parent, "force": true,
			}, parent}
		}},
		{"the status-change route: a parent to done over a child in todo with no option", func(t *testing.T, label string) breakingWrite {
			parent := seed(t, label+" parent", "in_progress", "")
			child := seed(t, label+" child", "todo", parent)
			return breakingWrite{http.MethodPost, "/api/v1/issues/" + parent + "/status-change", map[string]any{"status": "done"}, child}
		}},
	}
	callers := []struct {
		name string
		send func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder
	}{
		{"a person signed in", func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.asPerson(t, method, path, body)
		}},
		{"a personal token", func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			body["actor"] = map[string]any{"kind": "session", "id": "ses_personal_token"}
			return rig.serve(brokerRequest(t, method, path, body, "Bearer "+personal, ""))
		}},
		{"a broker token", func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.serve(agent.presenting(t, method, path, brokerToken, body))
		}},
	}
	for w, writer := range writers {
		t.Run(writer.name, func(t *testing.T) {
			for c, caller := range callers {
				t.Run(caller.name, func(t *testing.T) {
					write := writer.write(t, fmt.Sprintf("Rule case %d.%d", w+1, c+1))
					requireParentStatusConflict(t, write.method+" "+write.path, caller.send(t, write.method, write.path, write.body), write.holds)
				})
			}
		})
	}
}

// requireParentStatusConflict fails t unless response is 409 PARENT_STATUS_CONFLICT and its
// issues name holds and no other issue.
func requireParentStatusConflict(t *testing.T, request string, response *httptest.ResponseRecorder, holds string) {
	t.Helper()
	var refusal struct {
		Code   string `json:"code"`
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	// A body that is not the refusal's JSON leaves the code empty, and the check below names it.
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusConflict || refusal.Code != "PARENT_STATUS_CONFLICT" {
		t.Errorf("%s: status=%d body=%.300s, want 409 PARENT_STATUS_CONFLICT naming %s", request, response.Code, response.Body.String(), holds)
		return
	}
	named := make([]string, 0, len(refusal.Issues))
	for _, issue := range refusal.Issues {
		named = append(named, issue.Key)
	}
	slices.Sort(named)
	if named = slices.Compact(named); !slices.Equal(named, []string{holds}) {
		t.Errorf("%s: the refusal names %q, want %s alone", request, named, holds)
	}
}

// ruleProject is the project that the rule's tests file their issues in.
const ruleProject = "ACME"

// ruleRig is a server for R24's tests, with DISPATCH_PARENT_STATUS_RULE on or off, and its
// project. Every write goes as alice, a person signed in.
type ruleRig struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
}

func newRuleRig(t *testing.T, on bool) ruleRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{parentStatusRule: on})
	createTestProject(t, handler, ruleProject)
	return ruleRig{handler: handler, database: database, deps: deps}
}

// createProjects creates each project of keys through the API, as alice.
func createProjects(t *testing.T, handler http.Handler, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
}

// file files an issue titled title under parent, or as a root when parent is empty, and moves it
// to status. A test seeds its issues in an order that keeps the rule, so each seed is taken with
// the rule on.
func (rig ruleRig) file(t *testing.T, title, status, parent string) string {
	t.Helper()
	created := rig.send(t, createUnder(parent, title))
	if created.Code != http.StatusCreated {
		t.Fatalf("file %q: status=%d body=%s", title, created.Code, created.Body.String())
	}
	key := decodeBody[model.Issue](t, created).Key
	if status != model.IssueStatusTriage {
		setIssueStatus(t, rig.handler, key, status)
	}
	return key
}

// send sends write as alice.
func (rig ruleRig) send(t *testing.T, write ruleWrite) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, rig.handler, write.method, write.path, write.body, "alice")
}

// issue reads the status and the parent of key as they are stored.
func (rig ruleRig) issue(t *testing.T, key string) (string, *string) {
	t.Helper()
	var status string
	var parent *string
	if err := rig.database.Pool.QueryRow(context.Background(), `select status, parent_key from issues where key = $1`, key).Scan(&status, &parent); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return status, parent
}

// ruleWrite is one write that a test sends.
type ruleWrite struct {
	method, path string
	body         map[string]any
}

func (write ruleWrite) String() string { return write.method + " " + write.path }

// patchOf is the PATCH of key with body.
func patchOf(key string, body map[string]any) ruleWrite {
	return ruleWrite{http.MethodPatch, "/api/v1/issues/" + key, body}
}

// createUnder is the create of an issue titled title under parent, or of a root when parent is
// empty.
func createUnder(parent, title string) ruleWrite {
	body := map[string]any{"project": ruleProject, "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	return ruleWrite{http.MethodPost, "/api/v1/issues", body}
}

// ruleHolder is an issue that holds R24 against a write, as the refusal names it: its key, title,
// status and relation in the body's issues, and the sentence about it in the body's error.
type ruleHolder struct {
	key, title, status, relation, sentence string
}

// ruleBreach is a write that breaks R24 over issues that it seeds first, and the issues that the
// rule's refusal of it names, in their order.
type ruleBreach struct {
	name string
	seed func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder)
}

// parentDoneBreaches are writes that leave a parent done while a child of it is not done, over
// three levels: a close over an open child, a close over an open child that has an open child of
// its own, the close of that middle issue, and a reopen under a done parent.
var parentDoneBreaches = []ruleBreach{
	{"a root closed over its child in todo", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		root := rig.file(t, "Root over a todo child", "in_progress", "")
		child := rig.file(t, "Todo child", "todo", root)
		return patchOf(root, map[string]any{"status": "done"}), []ruleHolder{
			{child, "Todo child", "todo", "child", child + " (todo) is a child of " + root + " and is not done"},
		}
	}},
	{"a root closed over its child in work, which has an open child", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		root := rig.file(t, "Root of three levels", "in_progress", "")
		middle := rig.file(t, "Middle in work", "in_progress", root)
		rig.file(t, "Leaf in todo", "todo", middle)
		return patchOf(root, map[string]any{"status": "done"}), []ruleHolder{
			{middle, "Middle in work", "in_progress", "child", middle + " (in_progress) is a child of " + root + " and is not done"},
		}
	}},
	{"a middle issue closed over its open child", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		root := rig.file(t, "Root of an open middle", "in_progress", "")
		middle := rig.file(t, "Middle over a backlog leaf", "in_progress", root)
		leaf := rig.file(t, "Backlog leaf", "backlog", middle)
		return patchOf(middle, map[string]any{"status": "done"}), []ruleHolder{
			{leaf, "Backlog leaf", "backlog", "child", leaf + " (backlog) is a child of " + middle + " and is not done"},
		}
	}},
	{"a leaf reopened under its done parent", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		root := rig.file(t, "Closed root", "in_progress", "")
		middle := rig.file(t, "Closed middle", "in_progress", root)
		leaf := rig.file(t, "Closed leaf", "done", middle)
		setIssueStatus(t, rig.handler, middle, "done")
		setIssueStatus(t, rig.handler, root, "done")
		return patchOf(leaf, map[string]any{"status": "backlog"}), []ruleHolder{
			{middle, "Closed middle", "done", "parent", middle + " (done) is the parent of " + leaf + " and is done"},
		}
	}},
}

// childInWorkBreaches are writes that leave a child in work under a parent that is not in work:
// a parent moved out of work over children in work, and a child moved into work under a parent
// that has not started.
var childInWorkBreaches = []ruleBreach{
	{"a parent moved to backlog over its child in todo", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Parent leaving work", "in_progress", "")
		child := rig.file(t, "Child planned", "todo", parent)
		return patchOf(parent, map[string]any{"status": "backlog"}), []ruleHolder{
			{child, "Child planned", "todo", "child", child + " (todo) is a child of " + parent + " and is in work"},
		}
	}},
	{"a parent moved to icebox over two children in work and one not started", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Parent put on ice", "in_progress", "")
		running := rig.file(t, "Child running", "in_progress", parent)
		tested := rig.file(t, "Child under test", "testing", parent)
		rig.file(t, "Child on the backlog", "backlog", parent)
		return patchOf(parent, map[string]any{"status": "icebox"}), []ruleHolder{
			{running, "Child running", "in_progress", "child", running + " (in_progress) is a child of " + parent + " and is in work"},
			{tested, "Child under test", "testing", "child", tested + " (testing) is a child of " + parent + " and is in work"},
		}
	}},
	{"a child moved into work under its parent on the backlog", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Parent on the backlog", "backlog", "")
		child := rig.file(t, "Child starting", "backlog", parent)
		return patchOf(child, map[string]any{"status": "todo"}), []ruleHolder{
			{parent, "Parent on the backlog", "backlog", "parent", parent + " (backlog) is the parent of " + child + " and is not in work"},
		}
	}},
	{"a child moved into work under its parent in triage", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Parent in triage", model.IssueStatusTriage, "")
		child := rig.file(t, "Child picked up", model.IssueStatusTriage, parent)
		return patchOf(child, map[string]any{"status": "in_progress"}), []ruleHolder{
			{parent, "Parent in triage", "triage", "parent", parent + " (triage) is the parent of " + child + " and is not in work"},
		}
	}},
}

// reparentAndCreateBreaches are a change of parent and a create that break R24: an open issue
// moved under a done parent, an issue in work moved under a parent that has not started, and an
// issue filed under a done parent.
var reparentAndCreateBreaches = []ruleBreach{
	{"an open issue moved under a done parent", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Finished parent", "done", "")
		child := rig.file(t, "Open issue to move", "todo", "")
		return patchOf(child, map[string]any{"parent": parent}), []ruleHolder{
			{parent, "Finished parent", "done", "parent", parent + " (done) is the parent of " + child + " and is done"},
		}
	}},
	{"an issue in work moved under a parent on the backlog", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Unstarted parent", "backlog", "")
		child := rig.file(t, "Running issue to move", "in_progress", "")
		return patchOf(child, map[string]any{"parent": parent}), []ruleHolder{
			{parent, "Unstarted parent", "backlog", "parent", parent + " (backlog) is the parent of " + child + " and is not in work"},
		}
	}},
	{"an issue filed under a done parent", func(t *testing.T, rig ruleRig) (ruleWrite, []ruleHolder) {
		parent := rig.file(t, "Shipped parent", "done", "")
		return createUnder(parent, "Late child"), []ruleHolder{
			{parent, "Shipped parent", "done", "parent", parent + " (done) is the parent of the new issue and is done"},
		}
	}},
}

// requireRuleRefusals seeds and sends each breach on rig, whose rule is on, as a subtest, and
// holds each answer to the refusal that names the breach's holders.
func requireRuleRefusals(t *testing.T, rig ruleRig, breaches []ruleBreach) {
	t.Helper()
	for _, breach := range breaches {
		t.Run(breach.name, func(t *testing.T) {
			write, holders := breach.seed(t, rig)
			requireRuleRefusal(t, write.String(), rig.send(t, write), holders)
		})
	}
}

// requireRuleRefusal fails t unless response is 409 PARENT_STATUS_CONFLICT and its whole body is
// the refusal that names holders, in their order, and no other issue.
func requireRuleRefusal(t *testing.T, request string, response *httptest.ResponseRecorder, holders []ruleHolder) {
	t.Helper()
	want := ruleRefusalBody(holders)
	var got any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || response.Code != http.StatusConflict || !reflect.DeepEqual(got, want) {
		wanted, _ := json.Marshal(want)
		t.Errorf("%s: status=%d body=%s\nwant 409 %s", request, response.Code, strings.TrimSpace(response.Body.String()), wanted)
	}
}

// ruleRefusalBody is the whole 409 PARENT_STATUS_CONFLICT body that names holders, in their order,
// as JSON decodes it.
func ruleRefusalBody(holders []ruleHolder) map[string]any {
	sentences := make([]string, 0, len(holders))
	issues := make([]any, 0, len(holders))
	for _, holder := range holders {
		sentences = append(sentences, holder.sentence+".")
		issues = append(issues, map[string]any{"key": holder.key, "title": holder.title, "status": holder.status, "relation": holder.relation})
	}
	return map[string]any{
		"code": "PARENT_STATUS_CONFLICT",
		"error": "this change would leave a parent's status out of line with its children's. " + strings.Join(sentences, " ") +
			" A parent is done only when each child is done, and while a child is in work, its parent is in work.",
		"issues": issues,
	}
}

// TestAParentIsDoneOnlyWhenEachChildIsDone: with the rule on, a parent's move to done is refused
// while a child of it is not done, over three levels, and so is the reopen of a child under a done
// parent. Each refusal names the issue that holds the rule, with its status. Closed from the
// bottom up, each issue of a chain is closed once each child of it is done.
func TestAParentIsDoneOnlyWhenEachChildIsDone(t *testing.T) {
	rig := newRuleRig(t, true)
	requireRuleRefusals(t, rig, parentDoneBreaches)
	root := rig.file(t, "Root closed last", "in_progress", "")
	middle := rig.file(t, "Middle closed second", "in_progress", root)
	leaf := rig.file(t, "Leaf closed first", "todo", middle)
	closeMiddle := patchOf(middle, map[string]any{"status": "done"})
	requireRuleRefusal(t, closeMiddle.String(), rig.send(t, closeMiddle), []ruleHolder{
		{leaf, "Leaf closed first", "todo", "child", leaf + " (todo) is a child of " + middle + " and is not done"},
	})
	if status, _ := rig.issue(t, middle); status != "in_progress" {
		t.Errorf("the refused close left %s %s, want in_progress", middle, status)
	}
	for _, key := range []string{leaf, middle, root} {
		setIssueStatus(t, rig.handler, key, "done")
	}
}

// TestAChildInWorkKeepsItsParentInWork: with the rule on, a parent's move out of work is refused
// while a child of it is in work, and a child's move into work is refused under a parent that is
// not in work. Each refusal names the issues that hold the rule, with their statuses.
func TestAChildInWorkKeepsItsParentInWork(t *testing.T) {
	requireRuleRefusals(t, newRuleRig(t, true), childInWorkBreaches)
}

// TestAReparentAndACreateAreCheckedToo: with the rule on, a change of parent that puts an open
// issue under a done parent, or an issue in work under a parent that has not started, is refused,
// and so is a create under a done parent. A create under a parent that has not started is taken,
// since a new issue is in triage.
func TestAReparentAndACreateAreCheckedToo(t *testing.T) {
	rig := newRuleRig(t, true)
	requireRuleRefusals(t, rig, reparentAndCreateBreaches)
	parent := rig.file(t, "Parent still on the backlog", "backlog", "")
	created := rig.send(t, createUnder(parent, "Child of an unstarted parent"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create under a parent on the backlog: status=%d body=%s, want 201", created.Code, created.Body.String())
	}
	child := decodeBody[model.Issue](t, created)
	if child.Status != model.IssueStatusTriage || child.Parent == nil || *child.Parent != parent {
		t.Errorf("the create answered status %q under %v, want triage under %s", child.Status, child.Parent, parent)
	}
}

// TestTheRuleJudgesOnlyThePairsAWriteChanges: with the rule on, a write is judged by the pairs it
// changes. Beside a pair that broke the rule before the write (a done parent over a child in
// todo, written straight into the database), a write that leaves that pair as it is, is taken: a
// PATCH that names the status the child already has, a status change of the child's own child,
// which judges only its pair with the child, and the unlink of the child, R25's first step, while
// an issue in work is still under the child.
func TestTheRuleJudgesOnlyThePairsAWriteChanges(t *testing.T) {
	rig := newRuleRig(t, true)
	parent := rig.file(t, "Parent closed before the rule", "in_progress", "")
	child := rig.file(t, "Child left open", "todo", parent)
	grandchild := rig.file(t, "Grandchild in work", "in_progress", child)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update issues set status = 'done', closed_at = now() where key = $1
	`, parent); err != nil {
		t.Fatalf("close %s past the rule: %v", parent, err)
	}
	for _, write := range []ruleWrite{
		patchOf(child, map[string]any{"status": "todo"}),
		patchOf(grandchild, map[string]any{"status": "testing"}),
		patchOf(child, map[string]any{"parent": nil}),
	} {
		if response := rig.send(t, write); response.Code != http.StatusOK {
			t.Errorf("%s %v beside the old breach: status=%d body=%s, want 200", write, write.body, response.Code, response.Body.String())
		}
	}
	if status, parentOf := rig.issue(t, child); status != "todo" || parentOf != nil {
		t.Errorf("%s is %s under %v, want todo with no parent", child, status, parentOf)
	}
	if status, parentOf := rig.issue(t, grandchild); status != "testing" || parentOf == nil || *parentOf != child {
		t.Errorf("%s is %s under %v, want testing under %s", grandchild, status, parentOf, child)
	}
}

// TestTheParentStatusRuleDecidesBeforeTheOpenForeignChildCheck: with DISPATCH_PARENT_STATUS_RULE
// on, the PATCH judges an acceptance write by R24 first, and runs the open-child check that
// refuse_open_foreign_children asks for only on a write that R24 takes. So done over an open child
// of another project is 409 PARENT_STATUS_CONFLICT, since a parent is done only when each child is
// done, and retro over that child, which R24 takes, is 409 FOREIGN_CHILD_OPEN. Done over a child
// in icebox, which the open-child check alone takes, is still R24's refusal.
func TestTheParentStatusRuleDecidesBeforeTheOpenForeignChildCheck(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, parentStatusRule: true})
	rig := ruleRig{handler: handler, database: database, deps: deps}
	parent := createListedIssues(t, handler, "CORE", 1)[0]
	child := createListedIssues(t, handler, "SIDE", 1)[0]
	// The routes of this build refuse a parent of another project, so the test writes the link
	// itself, as TestAnAcceptanceWriteWaitsForAnOpenChildOfAnotherProject does.
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatalf("put %s under %s: %v", child, parent, err)
	}
	for _, row := range []struct{ write, child, code string }{
		{"done", "triage", "PARENT_STATUS_CONFLICT"},
		{"retro", "triage", "FOREIGN_CHILD_OPEN"},
		{"done", "icebox", "PARENT_STATUS_CONFLICT"},
	} {
		t.Run(row.write+" over a child of another project in "+row.child+" is "+row.code, func(t *testing.T) {
			setIssueStatus(t, handler, parent, "in_progress")
			setIssueStatus(t, handler, child, row.child)
			write := patchOf(parent, map[string]any{"status": row.write, "refuse_open_foreign_children": true})
			response := rig.send(t, write)
			if row.code == "PARENT_STATUS_CONFLICT" {
				requireParentStatusConflict(t, write.String(), response, child)
			} else {
				refusal := decodeBody[foreignChildOpenAnswer](t, response)
				named := make([]string, 0, len(refusal.Issues))
				for _, issue := range refusal.Issues {
					named = append(named, issue.Key)
				}
				if response.Code != http.StatusConflict || refusal.Code != row.code || !slices.Equal(named, []string{child}) {
					t.Errorf("%s: status=%d body=%.300s, want 409 %s naming %s", write, response.Code, response.Body.String(), row.code, child)
				}
			}
			if status, _ := rig.issue(t, parent); status != "in_progress" {
				t.Errorf("the refused write left %s %s, want in_progress", parent, status)
			}
		})
	}
}

// TestACreateRacingItsParentsCloseIsRefusedEitherWay: a create under a parent and the close of
// that parent end the same way in either order, with the rule on. A create holds no owner
// lock of its parent, so each order is made by a transaction of the test's own that holds what
// the first of the two writes takes. Each wait is on the backend of the write under test
// (oneConnectionHandler), which pg_locks shows queued behind the test's transaction.
func TestACreateRacingItsParentsCloseIsRefusedEitherWay(t *testing.T) {
	ctx := context.Background()
	// race starts write through a handler of one connection of its own while hold is open, waits
	// until that connection's backend waits on a lock that hold owns, commits hold and answers
	// what the write answered.
	race := func(t *testing.T, rig ruleRig, hold pgx.Tx, write ruleWrite) *httptest.ResponseRecorder {
		t.Helper()
		handler, pid := oneConnectionHandler(t, rig.deps)
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- dispatchRequest(t, handler, write.method, write.path, write.body, "alice") }()
		testwait.Eventually(t, write.String()+" to wait on the test's transaction", func() bool {
			select {
			case response := <-answered:
				t.Fatalf("%s answered %d before it waited on the test's transaction: %s", write, response.Code, response.Body.String())
			default:
			}
			queued, err := storetest.QueuedBehind(ctx, hold, pid)
			if err != nil {
				t.Fatal(err)
			}
			return queued
		})
		if err := hold.Commit(ctx); err != nil {
			t.Fatalf("commit the test's transaction: %v", err)
		}
		var response *httptest.ResponseRecorder
		testwait.Eventually(t, write.String()+" to answer", func() bool {
			select {
			case response = <-answered:
				return true
			default:
				return false
			}
		})
		return response
	}

	t.Run("the close's update first", func(t *testing.T) {
		rig := newRuleRig(t, true)
		parent := rig.file(t, "Parent closed before the create", "in_progress", "")
		awaitNoSettlementOwed(t, rig.database)
		hold, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the close: %v", err)
		}
		defer hold.Rollback(ctx)
		if _, err := hold.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = $1`, parent); err != nil {
			t.Fatalf("run the close's update: %v", err)
		}
		create := createUnder(parent, "Child filed during the close")
		requireRuleRefusal(t, create.String(), race(t, rig, hold, create), []ruleHolder{
			{parent, "Parent closed before the create", "done", "parent", parent + " (done) is the parent of the new issue and is done"},
		})
		var children int
		if err := rig.database.Pool.QueryRow(ctx, `select count(*) from issues where parent_key = $1`, parent).Scan(&children); err != nil || children != 0 {
			t.Errorf("the refused create left %d children under %s (%v), want none", children, parent, err)
		}
	})

	t.Run("the create's share lock first", func(t *testing.T) {
		rig := newRuleRig(t, true)
		parent := rig.file(t, "Parent closed after the create", "in_progress", "")
		awaitNoSettlementOwed(t, rig.database)
		var number int
		if err := rig.database.Pool.QueryRow(ctx, `
			update projects set next_number = next_number + 1 where key = $1 returning next_number - 1
		`, ruleProject).Scan(&number); err != nil {
			t.Fatalf("take the child's number: %v", err)
		}
		child := fmt.Sprintf("%s-%d", ruleProject, number)
		hold, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the create: %v", err)
		}
		defer hold.Rollback(ctx)
		if _, err := hold.Exec(ctx, `select status from issues where key = $1 for share`, parent); err != nil {
			t.Fatalf("run the create's read of its parent: %v", err)
		}
		if _, err := hold.Exec(ctx, `
			insert into issues (key, project_key, number, title, parent_key, created_by, rank)
			values ($1, $2, $3, 'Child filed before the close', $4, '{"kind":"user","id":"alice"}', 'zz')
		`, child, ruleProject, number, parent); err != nil {
			t.Fatalf("insert the create's child: %v", err)
		}
		closeParent := patchOf(parent, map[string]any{"status": "done"})
		requireRuleRefusal(t, closeParent.String(), race(t, rig, hold, closeParent), []ruleHolder{
			{child, "Child filed before the close", "triage", "child", child + " (triage) is a child of " + parent + " and is not done"},
		})
		if status, _ := rig.issue(t, parent); status != "in_progress" {
			t.Errorf("the refused close left %s %s, want in_progress", parent, status)
		}
	})
}

// awaitNoSettlementOwed waits until no document of the database owes a settlement, so a test that
// then holds an issue's row holds it against the write it sends, not against a settlement.
func awaitNoSettlementOwed(t *testing.T, database *store.Store) {
	t.Helper()
	testwait.Eventually(t, "every document to settle", func() bool {
		var owed bool
		if err := database.Pool.QueryRow(context.Background(), `select exists (select 1 from doc_settlements_pending)`).Scan(&owed); err != nil {
			t.Fatalf("read the settlements owed: %v", err)
		}
		return !owed
	})
}

// TestTheParentStatusRuleWaitsForItsSetting: with DISPATCH_PARENT_STATUS_RULE off, each write
// that the rule's tests above see refused is taken, on the PATCH, on the create and on the
// status-change route, whose call with no option writes the status alone and moves no child.
func TestTheParentStatusRuleWaitsForItsSetting(t *testing.T) {
	rig := newRuleRig(t, false)
	for _, breaches := range [][]ruleBreach{parentDoneBreaches, childInWorkBreaches, reparentAndCreateBreaches} {
		for _, breach := range breaches {
			t.Run(breach.name, func(t *testing.T) {
				write, _ := breach.seed(t, rig)
				want := http.StatusOK
				if write.method == http.MethodPost {
					want = http.StatusCreated
				}
				if response := rig.send(t, write); response.Code != want {
					t.Errorf("%s with the rule off: status=%d body=%s, want %d", write, response.Code, response.Body.String(), want)
				}
			})
		}
	}
	t.Run("the status-change route: a parent to done over a child in todo with no option", func(t *testing.T) {
		parent := rig.file(t, "Parent closed through the route", "in_progress", "")
		child := rig.file(t, "Child left in todo", "todo", parent)
		write := statusChangeOf(parent, map[string]any{"status": "done"})
		if response := rig.send(t, write); response.Code != http.StatusOK {
			t.Fatalf("%s with the rule off: status=%d body=%s, want 200", write, response.Code, response.Body.String())
		}
		if status, _ := rig.issue(t, parent); status != "done" {
			t.Errorf("the call left %s %s, want done", parent, status)
		}
		if status, parentOf := rig.issue(t, child); status != "todo" || parentOf == nil || *parentOf != parent {
			t.Errorf("the call left %s %s under %v, want todo under %s", child, status, parentOf, parent)
		}
	})
}

// TestAStepAndItsCommentCommitTogether: a write of an issue that carries a comment (writeIssue's
// issueWriteCall.comment) writes the comment in its own transaction, after its own events. Refused
// under its locks, here by R24 with the rule on, the write leaves no comment and no event. The same
// call, taken once the child that refused it is done, leaves exactly one comment: written by
// Dispatch's system actor with the body given, with the reference that the body makes, and with
// its comment.created event on the issue after the issue's own event. One transaction wrote each
// of those rows (their xmin), and the write answers the comment's event as its last on the issue.
// The event of a system author wakes nobody.
func TestAStepAndItsCommentCommitTogether(t *testing.T) {
	ctx := context.Background()
	rig := newRuleRig(t, true)
	parent := rig.file(t, "Parent closed with a comment", "in_progress", "")
	child := rig.file(t, "Child that holds the close", "todo", parent)
	body := "Dispatch closed this issue once " + child + " was done."
	alice := model.Actor{Kind: "user", ID: "alice"}
	call := issueWriteCall{
		writer:      issueWriter{actor: alice, human: true},
		key:         parent,
		fields:      updateIssueInput{Status: new("done")},
		outliving:   ctx,
		adviceRoute: "PATCH /api/v1/issues/{key}",
		comment:     &issueComment{author: parentStatusRuleActor, words: func(model.Issue) string { return body }},
	}
	served := directServer(rig.deps)

	// storedRow is a row of the comment as stored: a comment, an event or a reference, with the
	// transaction that wrote it.
	type storedRow struct {
		id, kind string
		actor    model.Actor
		text     string
		seq      int
		notify   bool
		xmin     string
	}
	read := func(t *testing.T, what, query string, args ...any) []storedRow {
		t.Helper()
		rows, err := rig.database.Pool.Query(ctx, query, args...)
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedRow, error) {
			var got storedRow
			err := row.Scan(&got.id, &got.kind, &got.actor.Kind, &got.actor.ID, &got.text, &got.seq, &got.notify, &got.xmin)
			return got, err
		})
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		return stored
	}
	comments := func(t *testing.T) []storedRow {
		return read(t, "the comments", `
			select id, '', author ->> 'kind', author ->> 'id', body, 0, false, xmin::text
			from comments where issue_key = $1 order by created_at
		`, parent)
	}
	eventsAfter := func(t *testing.T, seq int) []storedRow {
		return read(t, "the events", `
			select id::text, type, actor ->> 'kind', actor ->> 'id', coalesce(payload ->> 'id', ''), seq, notify, xmin::text
			from events where issue_key = $1 and seq > $2 order by seq
		`, parent, seq)
	}

	refusedAt := issueLastSeq(t, rig.database, parent)
	if _, err := served.writeIssue(ctx, call); !errors.As(err, new(*parentStatusConflict)) {
		t.Fatalf("the close of %s with a comment over its open child answered %v, want the rule's conflict", parent, err)
	}
	if stored := comments(t); len(stored) != 0 {
		t.Errorf("the refused close left the comments %+v on %s, want none", stored, parent)
	}
	if stored := eventsAfter(t, refusedAt); len(stored) != 0 {
		t.Errorf("the refused close left the events %+v on %s, want none", stored, parent)
	}
	if status, _ := rig.issue(t, parent); status != "in_progress" {
		t.Errorf("the refused close left %s %s, want in_progress", parent, status)
	}

	setIssueStatus(t, rig.handler, child, "done")
	written, err := served.writeIssue(ctx, call)
	if err != nil {
		t.Fatalf("the close of %s with a comment once its child is done: %v", parent, err)
	}
	stored := comments(t)
	if len(stored) != 1 {
		t.Fatalf("the close left the comments %+v on %s, want exactly one", stored, parent)
	}
	comment := stored[0]
	if comment.actor != parentStatusRuleActor || comment.text != body {
		t.Errorf("the comment is %q by %+v, want %q by %+v", comment.text, comment.actor, body, parentStatusRuleActor)
	}
	appended := eventsAfter(t, written.previousLastSeq)
	if len(appended) != 2 ||
		appended[0].kind != "issue.closed" || appended[0].actor != alice ||
		appended[1].kind != "comment.created" || appended[1].actor != parentStatusRuleActor || appended[1].text != comment.id {
		t.Fatalf("the close appended %+v on %s, want issue.closed by %+v, then comment.created of %s by %+v",
			appended, parent, alice, comment.id, parentStatusRuleActor)
	}
	created := appended[1]
	if created.notify {
		t.Errorf("the comment.created of Dispatch's system actor notifies, want it to wake nobody")
	}
	references := read(t, "the comment's references", `
		select to_id, to_kind, '', '', '', 0, false, xmin::text from refs where from_kind = 'comment' and from_id = $1
	`, comment.id)
	if len(references) != 1 || references[0].id != child || references[0].kind != "issue" {
		t.Errorf("the comment's references are %+v, want one to the issue %s", references, child)
	}
	for _, row := range append([]storedRow{appended[0], created}, references...) {
		if row.xmin != comment.xmin {
			t.Errorf("the %s %s was written by transaction %s, and the comment by %s: want one transaction", row.kind, row.id, row.xmin, comment.xmin)
		}
	}
	last := written.appended[len(written.appended)-1]
	if last.Type != "comment.created" || strconv.FormatInt(last.ID, 10) != created.id || written.issue.LastSeq != created.seq {
		t.Errorf("the write answered its last event %s %d and last_seq %d, want the comment.created %s at %d",
			last.Type, last.ID, written.issue.LastSeq, created.id, created.seq)
	}
}
