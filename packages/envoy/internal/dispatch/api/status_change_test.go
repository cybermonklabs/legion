// status_change_test.go holds the tests of POST /api/v1/issues/{key}/status-change: the three
// options and the dry run, who may close the issues under an issue with it, the order of its
// writes, its answer, its refusals, and its sequence fence (R20).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// statusChangeRule is the rule as each comment of an option states it.
const statusChangeRule = "Dispatch keeps a parent's status in line with its children's: a parent is done only when each " +
	"child is done, and while a child is in work its parent is in work too."

// browserSignInRefusal is the message of the 403 BROWSER_SIGN_IN_REQUIRED.
const browserSignInRefusal = "Only a person who is signed in at a browser can close the open issues under an issue together " +
	"with it (include_children with done). This request presents a token."

// statusChangeRig is ruleRig with the server Register mounted, whose step hook a test sets
// (statusChangeAfterStep).
type statusChangeRig struct {
	ruleRig
	served *server
}

func newStatusChangeRig(t *testing.T, on bool) statusChangeRig {
	t.Helper()
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{parentStatusRule: on})
	createTestProject(t, handler, ruleProject)
	return statusChangeRig{ruleRig: ruleRig{handler: handler, database: database, deps: deps}, served: served}
}

// afterStep sets the server's step hook to hook for the rest of the test.
func (rig statusChangeRig) afterStep(t *testing.T, hook func(ctx context.Context, step int)) {
	rig.served.statusChangeAfterStep = hook
	t.Cleanup(func() { rig.served.statusChangeAfterStep = nil })
}

// quiet waits until no write the server left running and no document settlement is owed, so the
// next fingerprint of the database reads only what the next request writes.
func (rig statusChangeRig) quiet(t *testing.T) map[string]string {
	t.Helper()
	awaitNoSettlementOwed(t, rig.database)
	awaitBackgroundWrites(t, rig.served)
	return databaseFingerprint(t, rig.database)
}

// statusChangeOf is the status change of key with body.
func statusChangeOf(key string, body map[string]any) ruleWrite {
	return ruleWrite{http.MethodPost, "/api/v1/issues/" + key + "/status-change", body}
}

// wantChange is an entry of changed, or the answer's issue, as a test expects it: the key, the
// status and the parent after the call ("" for no parent), the issue's last_seq before its step,
// and the last_seq of the entry's issue after it. last is -1 for a dry run's entry, which holds
// no issue.
type wantChange struct {
	key, status, parent string
	previous, last      int
}

// issue is the issue of the entry as projectedStatusChange keeps it.
func (change wantChange) issue() map[string]any {
	return map[string]any{"key": change.key, "status": change.status, "parent": optionalKey(change.parent), "last_seq": float64(change.last)}
}

// entry is the entry as projectedStatusChange keeps it.
func (change wantChange) entry() map[string]any {
	entry := map[string]any{
		"key": change.key, "status": change.status, "parent": optionalKey(change.parent), "previous_last_seq": float64(change.previous),
	}
	if change.last >= 0 {
		entry["issue"] = change.issue()
	}
	return entry
}

// optionalKey is key as JSON decodes it: nil for "".
func optionalKey(key string) any {
	if key == "" {
		return nil
	}
	return key
}

// entries is each change as an entry of changed.
func entries(changed []wantChange) []any {
	listed := make([]any, 0, len(changed))
	for _, change := range changed {
		listed = append(listed, change.entry())
	}
	return listed
}

// wantAnswer is the 200 answer of a call as projectedStatusChange keeps it.
func wantAnswer(named wantChange, changed ...wantChange) map[string]any {
	return map[string]any{"issue": named.issue(), "changed": entries(changed)}
}

// wantRefusal is a refusal's whole body: its code, its message and the committed steps' entries.
func wantRefusal(code, message string, changed ...wantChange) map[string]any {
	return map[string]any{"code": code, "error": message, "changed": entries(changed)}
}

// wantConflict is a 409 PARENT_STATUS_CONFLICT's whole body: the rule's refusal that names holders
// (ruleRefusalBody), and the committed steps' entries.
func wantConflict(holders []ruleHolder, changed ...wantChange) map[string]any {
	body := ruleRefusalBody(holders)
	body["changed"] = entries(changed)
	return body
}

// projectedStatusChange is response's whole body as JSON decodes it, with each issue in it, the
// answer's and each entry's, held to its key, status, parent and last_seq: every other member of
// the body is compared as it stands.
func projectedStatusChange(t *testing.T, response *httptest.ResponseRecorder) any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the answer %s: %v", response.Body.String(), err)
	}
	project := func(value any) any {
		issue, ok := value.(map[string]any)
		if !ok {
			return value
		}
		return map[string]any{"key": issue["key"], "status": issue["status"], "parent": issue["parent"], "last_seq": issue["last_seq"]}
	}
	if issue, ok := body["issue"]; ok {
		body["issue"] = project(issue)
	}
	if changed, ok := body["changed"].([]any); ok {
		for _, value := range changed {
			if entry, ok := value.(map[string]any); ok {
				if issue, ok := entry["issue"]; ok {
					entry["issue"] = project(issue)
				}
			}
		}
	}
	return body
}

// requireStatusChange fails t unless response has status and its whole body, each issue in it held
// to its key, status, parent and last_seq, is want.
func requireStatusChange(t *testing.T, request string, response *httptest.ResponseRecorder, status int, want map[string]any) {
	t.Helper()
	if got := projectedStatusChange(t, response); response.Code != status || !reflect.DeepEqual(got, any(want)) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("%s: status=%d body=%s\nwant %d %s", request, response.Code, gotJSON, status, wantJSON)
	}
}

// storedComments is every comment on keys, oldest first, each as "<issue> <author kind>:<author
// id>: <body>".
func storedComments(t *testing.T, database *store.Store, keys ...string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select issue_key || ' ' || (author ->> 'kind') || ':' || (author ->> 'id') || ': ' || body
		from comments where issue_key = any($1) order by created_at, id
	`, keys)
	if err != nil {
		t.Fatalf("read the comments: %v", err)
	}
	comments, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the comments: %v", err)
	}
	return comments
}

// optionComment is a comment of Dispatch's system actor on key, as storedComments lists it.
func optionComment(key, body string) string {
	return key + " " + parentStatusRuleActor.Kind + ":" + parentStatusRuleActor.ID + ": " + body + " " + statusChangeRule
}

// namedComment is the comment of Dispatch's system actor on named, the issue a call of caller
// names, that lists what the call's options changed, as storedComments lists it: options is each
// option that changed an issue as the comment names it, and each line is one change (movedLine,
// removedLine).
func namedComment(named, caller, options string, lines ...string) string {
	return optionComment(named, fmt.Sprintf("Dispatch changed the issues below, because %s's status change of %s passed %s.", caller, named, options)) +
		"\n\n" + strings.Join(lines, "\n")
}

// movedLine is the line of namedComment for key, which option moved from status from to to.
func movedLine(key, from, to, option string) string {
	return fmt.Sprintf("- %s moved from `%s` to `%s` (`%s`)", key, from, to, option)
}

// removedLine is the line of namedComment for key, which unlink_children removed from named.
func removedLine(key, named string) string {
	return fmt.Sprintf("- %s removed from %s (`unlink_children`)", key, named)
}

// requireComments fails t unless the comments on keys are want, in their order.
func requireComments(t *testing.T, database *store.Store, keys []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := storedComments(t, database, keys...); !slices.Equal(got, want) {
		t.Errorf("the comments on %v are\n%s\nwant\n%s", keys, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// requireIssue fails t unless key is stored with status under parent ("" for no parent).
func (rig ruleRig) requireIssue(t *testing.T, key, status, parent string) {
	t.Helper()
	gotStatus, gotParent := rig.issue(t, key)
	if got := deref(gotParent); gotStatus != status || got != parent {
		t.Errorf("%s is %s under %q, want %s under %q", key, gotStatus, got, status, parent)
	}
}

// TestIncludeChildrenMovesTheOpenIssuesUnderIt: include_children writes the status of the call on
// each open issue under the issue, at any depth, that is not in that status's group. For done the
// issues close deepest first, then the issue itself; for backlog an icebox child is kept, as it is
// not started already; a done child is kept in both. The answer lists each change in the order of
// the writes, and each issue an option changes has one comment of Dispatch's system actor that
// names the caller, the issue and the option. The issue the call names then has one comment of
// that actor too, after them, which names the call and lists each change; it has none when the
// option moved no issue, and none after a call with no option. With the rule on, the same close
// with no option is refused before anything is written.
func TestIncludeChildrenMovesTheOpenIssuesUnderIt(t *testing.T) {
	rig := newStatusChangeRig(t, true)

	t.Run("done closes every open issue under the issue, deepest first", func(t *testing.T) {
		root := rig.file(t, "Root closed with its tree", "in_progress", "")
		middle := rig.file(t, "Middle in work", "in_progress", root)
		leaf := rig.file(t, "Leaf in todo", "todo", middle)
		done := rig.file(t, "Child already done", "done", root)
		iced := rig.file(t, "Child on ice", "icebox", root)
		seq := map[string]int{}
		for _, key := range []string{root, middle, leaf, done, iced} {
			seq[key] = issueLastSeq(t, rig.database, key)
		}
		write := statusChangeOf(root, map[string]any{"status": "done", "include_children": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{root, "done", "", seq[root] + 2, seq[root] + 3},
			wantChange{leaf, "done", middle, seq[leaf], seq[leaf] + 2},
			wantChange{middle, "done", root, seq[middle] + 1, seq[middle] + 3},
			wantChange{iced, "done", root, seq[iced], seq[iced] + 2},
			wantChange{root, "done", "", seq[root] + 2, seq[root] + 3},
		))
		for _, key := range []string{root, middle, leaf, done, iced} {
			if status, _ := rig.issue(t, key); status != "done" {
				t.Errorf("%s is %s after the call, want done", key, status)
			}
		}
		moved := func(key, from string) string {
			return optionComment(key, fmt.Sprintf("Dispatch moved this issue from `%s` to `done` with %s, because alice's status change of %s passed `include_children`.", from, root, root))
		}
		requireComments(t, rig.database, []string{root, middle, leaf, done, iced},
			moved(leaf, "todo"), moved(middle, "in_progress"), moved(iced, "icebox"),
			namedComment(root, "alice", "`include_children`", movedLine(leaf, "todo", "done", optionIncludeChildren),
				movedLine(middle, "in_progress", "done", optionIncludeChildren), movedLine(iced, "icebox", "done", optionIncludeChildren)))
	})

	t.Run("backlog moves the issues in work under the issue and keeps the rest", func(t *testing.T) {
		root := rig.file(t, "Root put back on the backlog", "in_progress", "")
		child := rig.file(t, "Child in work", "in_progress", root)
		grandchild := rig.file(t, "Grandchild in todo", "todo", child)
		iced := rig.file(t, "Child kept on ice", "icebox", root)
		done := rig.file(t, "Child kept done", "done", root)
		seq := map[string]int{}
		for _, key := range []string{root, child, grandchild} {
			seq[key] = issueLastSeq(t, rig.database, key)
		}
		write := statusChangeOf(root, map[string]any{"status": "backlog", "include_children": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{root, "backlog", "", seq[root] + 1, seq[root] + 2},
			wantChange{grandchild, "backlog", child, seq[grandchild], seq[grandchild] + 2},
			wantChange{child, "backlog", root, seq[child] + 1, seq[child] + 3},
			wantChange{root, "backlog", "", seq[root] + 1, seq[root] + 2},
		))
		rig.requireIssue(t, root, "backlog", "")
		rig.requireIssue(t, child, "backlog", root)
		rig.requireIssue(t, grandchild, "backlog", child)
		rig.requireIssue(t, iced, "icebox", root)
		rig.requireIssue(t, done, "done", root)
		moved := func(key, from string) string {
			return optionComment(key, fmt.Sprintf("Dispatch moved this issue from `%s` to `backlog` with %s, because alice's status change of %s passed `include_children`.", from, root, root))
		}
		requireComments(t, rig.database, []string{root, child, grandchild, iced, done}, moved(grandchild, "todo"), moved(child, "in_progress"),
			namedComment(root, "alice", "`include_children`", movedLine(grandchild, "todo", "backlog", optionIncludeChildren),
				movedLine(child, "in_progress", "backlog", optionIncludeChildren)))
	})

	t.Run("an option that moves no issue, and a call with no option, leave no comment", func(t *testing.T) {
		root := rig.file(t, "Root moved alone", "in_progress", "")
		done := rig.file(t, "Child that stays done", "done", root)
		seq := issueLastSeq(t, rig.database, root)
		write := statusChangeOf(root, map[string]any{"status": "todo", "include_children": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{root, "todo", "", seq, seq + 1},
			wantChange{root, "todo", "", seq, seq + 1},
		))
		plain := statusChangeOf(root, map[string]any{"status": "backlog"})
		requireStatusChange(t, plain.String(), rig.send(t, plain), http.StatusOK, wantAnswer(
			wantChange{root, "backlog", "", seq + 1, seq + 2},
			wantChange{root, "backlog", "", seq + 1, seq + 2},
		))
		requireComments(t, rig.database, []string{root, done})
	})

	t.Run("with no option the close is refused and nothing is written", func(t *testing.T) {
		root := rig.file(t, "Root closed with no option", "in_progress", "")
		child := rig.file(t, "Child that holds it", "todo", root)
		before := rig.quiet(t)
		write := statusChangeOf(root, map[string]any{"status": "done"})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusConflict, wantConflict([]ruleHolder{
			{child, "Child that holds it", "todo", "child", child + " (todo) is a child of " + root + " and is not done"},
		}))
		assertUnchanged(t, write.String(), before, databaseFingerprint(t, rig.database))
	})
}

// TestAStatusChangesCommentIsANodeOfTheStore: the comment Dispatch's system actor leaves on an
// issue an option moves is written as the comment route writes one (queueIssueComment): its row
// names its node, and the node is the comment's, project-visible and live, made at the row's
// created_at by the system actor and caused by the comment's own comment.created, under which its
// two base attributes are stated. It states no authored_by: the system actor is nobody the store
// names (graphstore.AuthorOf), so the store census reads no author off its row either.
func TestAStatusChangesCommentIsANodeOfTheStore(t *testing.T) {
	rig := newStatusChangeRig(t, true)
	ctx := context.Background()
	root := rig.file(t, "Root closed with its child", "in_progress", "")
	child := rig.file(t, "Child closed with it", "todo", root)
	write := statusChangeOf(root, map[string]any{"status": "done", "include_children": true})
	if response := rig.send(t, write); response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", write.String(), response.Code, response.Body.String())
	}
	var id string
	var createdAt time.Time
	var nodeKind, nodeAddress *string
	if err := rig.database.Pool.QueryRow(ctx, `
		select id::text, created_at, node_kind, node_address from comments where issue_key = $1
	`, child).Scan(&id, &createdAt, &nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read the comment the status change left on %s: %v", child, err)
	}
	if deref(nodeKind) != graphstore.KindComment || deref(nodeAddress) != id {
		t.Fatalf("the comment %s on %s names the node (%q, %q), want (%q, %q)",
			id, child, deref(nodeKind), deref(nodeAddress), graphstore.KindComment, id)
	}
	node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindComment, id)
	if err != nil || !held {
		t.Fatalf("read the node of the comment %s: held=%t err=%v", id, held, err)
	}
	event := commentCreatedEvent(t, rig.database, id)
	switch {
	case node.Supertag != graphstore.KindComment:
		t.Errorf("the node's supertag is %q, want %s", node.Supertag, graphstore.KindComment)
	case node.Visibility != graphstore.VisibilityProject || node.State != graphstore.StateLive:
		t.Errorf("the node is %s/%s, want %s/%s", node.Visibility, node.State, graphstore.VisibilityProject, graphstore.StateLive)
	case !node.MadeAt.Equal(createdAt):
		t.Errorf("the node was made at %s, and its row at %s", node.MadeAt, createdAt)
	case !node.MadeBy.SameAs(parentStatusRuleActor):
		t.Errorf("the node was made by %+v, want Dispatch's system actor %+v", node.MadeBy, parentStatusRuleActor)
	case node.SourceEvent != event:
		t.Errorf("the node's cause is event %d, want the comment's comment.created, %d", node.SourceEvent, event)
	}
	attributes := map[string]bool{}
	for _, tuple := range tuplesTheWriteAppended(t, rig.database, graphstore.KindComment, id) {
		if tuple.FromKind != graphstore.KindComment || tuple.FromID != id {
			continue
		}
		if tuple.Kind == graphstore.TupleAuthoredBy {
			t.Errorf("the comment states it was authored by %s %q, and Dispatch's system actor is nobody the store names",
				tuple.ToKind, tuple.ToID)
		}
		if tuple.ToKind == graphstore.KindValue && (tuple.Kind == graphstore.AttributeVisibility || tuple.Kind == graphstore.AttributeState) {
			attributes[tuple.Kind] = true
			if tuple.SourceEvent != event {
				t.Errorf("the comment's %s tuple carries cause %d, want its comment.created, %d", tuple.Kind, tuple.SourceEvent, event)
			}
		}
	}
	if !attributes[graphstore.AttributeVisibility] || !attributes[graphstore.AttributeState] {
		t.Errorf("the comment's write stated its visibility %t and its state %t, want both",
			attributes[graphstore.AttributeVisibility], attributes[graphstore.AttributeState])
	}
}

// TestAScopedStatusChangeIsRefusedAnAncestorOfAnotherProject: a parent may be an issue of another
// project, so include_parent can move an ancestor the caller's credential may not write. A
// credential scoped to BETA is refused the status change of its own BETA issue whose
// include_parent would move the ALPHA ancestor: 403 PROJECT_SCOPE naming the ancestor and both
// projects, with changed [] and nothing written, no status moved and no event appended on either
// issue, and its dry run is refused the same way. The same call by a person, whose credential
// names no project, moves the ancestor, which is the control.
func TestAScopedStatusChangeIsRefusedAnAncestorOfAnotherProject(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{})
	caller := newBrokerCaller(t)
	ctx := context.Background()
	for _, project := range []string{"ALPHA", "BETA"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	ancestor := createdIssue(t, "create ALPHA's ancestor", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ALPHA", "title": "Another project's ancestor", "force": true,
	})).Key
	named := createdIssue(t, "create BETA's issue under it", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BETA", "title": "Its own project's issue under it", "parent": ancestor, "force": true,
	})).Key
	const token = "a-token-scoped-to-beta"
	answer := liveNamedToken(caller.jkt)
	answer.Projects = []string{"BETA"}
	rig.broker.Says(token, answer)
	// stored is each issue's status and last_seq, and the events on the two issues.
	stored := func() string {
		t.Helper()
		var held string
		if err := rig.database.Pool.QueryRow(ctx, `
			select string_agg(key || ' ' || status || ' ' || last_seq, ', ' order by key)
			       || '; ' || (select count(*) from events where issue_key = any($1))
			from issues where key = any($1)
		`, []string{ancestor, named}).Scan(&held); err != nil {
			t.Fatalf("read %s and %s: %v", ancestor, named, err)
		}
		return held
	}
	before := stored()
	for _, dryRun := range []bool{false, true} {
		label := fmt.Sprintf("a scoped status change moving an ancestor of another project (dry_run %t)", dryRun)
		refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+named+"/status-change", token, map[string]any{
			"status": "in_progress", "include_parent": true, "dry_run": dryRun,
		}))
		requireStatusChange(t, label, refused, http.StatusForbidden, wantRefusal(apicode.ProjectScope.V1Name(), fmt.Sprintf(
			"this request's credential may write project BETA; an issue this status change's include_parent writes, %s, belongs to project ALPHA",
			ancestor)))
	}
	if after := stored(); after != before {
		t.Fatalf("the refused status changes moved %s to %s, want nothing written", before, after)
	}
	moved := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+named+"/status-change", map[string]any{
		"status": "in_progress", "include_parent": true,
	})
	if moved.Code != http.StatusOK {
		t.Fatalf("a person's status change moving an ancestor of another project: status=%d body=%s, want 200", moved.Code, moved.Body.String())
	}
	for _, key := range []string{ancestor, named} {
		if status := decodeBody[model.Issue](t, rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+key, nil)).Status; status != "in_progress" {
			t.Errorf("the person's status change left %s %s, want in_progress", key, status)
		}
	}
}

// TestAScopedStatusChangeIsRefusedADescendantOfAnotherProject: a child may be an issue of another
// project, so include_children can move, and unlink_children can unlink, an issue below the
// caller's own that its credential may not write. A credential scoped to ALPHA is refused the
// status change of its own ALPHA parent whose include_children would move the BETA child, and
// the one whose unlink_children would unlink it: 403 PROJECT_SCOPE naming the child, the option
// and both projects, with changed [] and nothing written - no status or parent moved, no event
// appended and no comment left on either issue - and each dry run is refused the same way. The
// same unlink by a person, whose credential names no project, unlinks the child, which is the
// control.
func TestAScopedStatusChangeIsRefusedADescendantOfAnotherProject(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{})
	caller := newBrokerCaller(t)
	ctx := context.Background()
	for _, project := range []string{"ALPHA", "BETA"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	named := createdIssue(t, "create ALPHA's parent", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ALPHA", "title": "Its own project's parent", "force": true,
	})).Key
	child := createdIssue(t, "create BETA's child under it", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BETA", "title": "Another project's child", "parent": named, "force": true,
	})).Key
	if moved := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+child+"/status-change", map[string]any{
		"status": "in_progress", "include_parent": true,
	}); moved.Code != http.StatusOK {
		t.Fatalf("move %s and %s into work: status=%d body=%s", child, named, moved.Code, moved.Body.String())
	}
	const token = "a-token-scoped-to-alpha"
	answer := liveNamedToken(caller.jkt)
	answer.Projects = []string{"ALPHA"}
	rig.broker.Says(token, answer)
	// stored is each issue's status, parent and last_seq, and the events and comments on the two.
	stored := func() string {
		t.Helper()
		var held string
		if err := rig.database.Pool.QueryRow(ctx, `
			select string_agg(key || ' ' || status || ' ' || coalesce(parent_key, '-') || ' ' || last_seq, ', ' order by key)
			       || '; ' || (select count(*) from events where issue_key = any($1))
			       || '; ' || (select count(*) from comments where issue_key = any($1))
			from issues where key = any($1)
		`, []string{named, child}).Scan(&held); err != nil {
			t.Fatalf("read %s and %s: %v", named, child, err)
		}
		return held
	}
	before := stored()
	for _, call := range []struct {
		option, status string
	}{
		{optionIncludeChildren, "backlog"},
		{optionUnlinkChildren, "done"},
	} {
		for _, dryRun := range []bool{false, true} {
			label := fmt.Sprintf("a scoped status change whose %s writes a child of another project (dry_run %t)", call.option, dryRun)
			refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+named+"/status-change", token, map[string]any{
				"status": call.status, call.option: true, "dry_run": dryRun,
			}))
			requireStatusChange(t, label, refused, http.StatusForbidden, wantRefusal(apicode.ProjectScope.V1Name(), fmt.Sprintf(
				"this request's credential may write project ALPHA; an issue this status change's %s writes, %s, belongs to project BETA",
				call.option, child)))
		}
	}
	if after := stored(); after != before {
		t.Fatalf("the refused status changes moved %s to %s, want nothing written", before, after)
	}
	unlinked := rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+named+"/status-change", map[string]any{
		"status": "done", "unlink_children": true,
	})
	if unlinked.Code != http.StatusOK {
		t.Fatalf("a person's status change unlinking a child of another project: status=%d body=%s, want 200", unlinked.Code, unlinked.Body.String())
	}
	if parent := decodeBody[model.Issue](t, rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+child, nil)).Parent; parent != nil {
		t.Errorf("the person's status change left %s under %s, want no parent", child, *parent)
	}
}

// statusChangeCaller is one way a request reaches the status-change route: what it presents, and
// whether optionalActor takes it for a person at a browser.
type statusChangeCaller struct {
	name   string
	person bool
	send   func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder
}

// statusChangeServer is a server whose callers reach the route in one of the ways optionalActor
// knows, with the project its issues are filed in and its person.
type statusChangeServer struct {
	name     string
	project  string
	database *store.Store
	// beside sends a request as the server's person, with authorization as its Authorization
	// header when it is not empty.
	beside  func(t *testing.T, method, path string, body map[string]any, authorization string) *httptest.ResponseRecorder
	callers []statusChangeCaller
}

// asPerson sends a request as the server's person.
func (server statusChangeServer) asPerson(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
	return server.beside(t, method, path, body, "")
}

// seed files an issue titled title as the server's person, under parent when parent is not
// empty, and moves it to status.
func (server statusChangeServer) seed(t *testing.T, title, status, parent string) string {
	t.Helper()
	body := map[string]any{"project": server.project, "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	created := server.asPerson(t, http.MethodPost, "/api/v1/issues", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("file %q: status=%d body=%s", title, created.Code, created.Body.String())
	}
	key := decodeBody[model.Issue](t, created).Key
	if status != model.IssueStatusTriage {
		if moved := server.asPerson(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": status}); moved.Code != http.StatusOK {
			t.Fatalf("move %s to %s: status=%d body=%s", key, status, moved.Code, moved.Body.String())
		}
	}
	return key
}

// createProject creates the server's project as its person.
func (server statusChangeServer) createProject(t *testing.T) {
	t.Helper()
	if created := server.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": server.project, "name": server.project}); created.Code != http.StatusCreated {
		t.Fatalf("create the project %s on %s: status=%d body=%s", server.project, server.name, created.Code, created.Body.String())
	}
}

// personCaller is the caller that the server's person is.
func (server statusChangeServer) personCaller(name string) statusChangeCaller {
	return statusChangeCaller{name: name, person: true, send: server.asPerson}
}

// statusChangeSession is the session a bearer that proves none names in its body.
var statusChangeSession = map[string]any{"kind": "session", "id": "ses_status_change"}

// tokenCallersServer is the server of every bearer branch of optionalActor and of the trusted
// identity header: the deployment's shared token, a personal token, one holding the person scope,
// a service-account token, a broker token acting as its session for one project, and one scoped
// to its person.
func tokenCallersServer(t *testing.T, on bool) statusChangeServer {
	t.Helper()
	issuer := newServiceTokenIssuer(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{parentStatusRule: on, oidc: issuer.verifier(t)})
	agent := newBrokerCaller(t)
	const sessionToken, personToken = "a-token-acting-as-its-session", "a-token-scoped-to-its-person"
	rig.broker.Says(sessionToken, liveNamedToken(agent.jkt))
	scoped := liveNamedToken(agent.jkt)
	scoped.ProjectScope, scoped.Projects = ciphertoken.ProjectScopePerson, nil
	rig.broker.Says(personToken, scoped)
	server := statusChangeServer{
		name: "the token server", project: brokerTestProject, database: rig.database,
		beside: func(t *testing.T, method, path string, body map[string]any, authorization string) *httptest.ResponseRecorder {
			return rig.serve(withPerson(brokerRequest(t, method, path, body, authorization, ""), brokerTestPerson))
		},
	}
	server.createProject(t)
	bearer := func(token string) func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			body["actor"] = statusChangeSession
			return rig.serve(brokerRequest(t, method, path, body, "Bearer "+token, ""))
		}
	}
	broker := func(token string) func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.serve(agent.presenting(t, method, path, token, body))
		}
	}
	server.callers = []statusChangeCaller{
		{name: "the shared token", send: bearer("agent-token")},
		{name: "a personal token", send: bearer(personalTokenOwnedBy(t, rig.database, brokerTestPerson))},
		{name: "a person-scoped personal token", send: bearer(personalTokenOwnedBy(t, rig.database, brokerTestPerson, model.ScopePerson))},
		{name: "a service-account token", send: bearer(issuer.mint(t, serviceTokenAudience))},
		{name: "a session broker token", send: broker(sessionToken)},
		{name: "a person-scoped broker token", send: broker(personToken)},
		server.personCaller("a trusted identity header"),
	}
	return server
}

// cookieServer is a server whose person signs in with a session cookie.
func cookieServer(t *testing.T, on bool) statusChangeServer {
	t.Helper()
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	handler, _, _ := newTestServer(t, testServerOptions{
		store: database, parentStatusRule: on, identity: identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
	})
	const login = "alice@d.example"
	generation, err := sessions.EnsureSession(context.Background(), login)
	if err != nil {
		t.Fatalf("establish the session of %s: %v", login, err)
	}
	seedPeople(t, database, login)
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(login, generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	server := statusChangeServer{
		name: "the cookie server", project: ruleProject, database: database,
		beside: func(t *testing.T, method, path string, body map[string]any, authorization string) *httptest.ResponseRecorder {
			request := brokerRequest(t, method, path, body, authorization, "")
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		},
	}
	server.createProject(t)
	server.callers = []statusChangeCaller{server.personCaller("a sign-in cookie")}
	return server
}

// iapServer is a server behind an identity-aware proxy, whose person no Dispatch cookie names:
// the proxy's signed assertion alone. Its verifier judges an assertion's times on the wall clock:
// each request carries an assertion minted for it that lives nine minutes, and no row here asks
// when one expires.
func iapServer(t *testing.T, on bool) statusChangeServer {
	t.Helper()
	database := storetest.Open(t)
	iap := newIAPSocketKeys(t, database, clock.Real())
	handler, _, _ := newTestServer(t, testServerOptions{store: database, parentStatusRule: on, identity: iap.identity})
	server := statusChangeServer{
		name: "the IAP server", project: ruleProject, database: database,
		beside: func(t *testing.T, method, path string, body map[string]any, authorization string) *httptest.ResponseRecorder {
			request := brokerRequest(t, method, path, body, authorization, "")
			for name, values := range iap.assertion(t, time.Now().Add(9*time.Minute)) {
				request.Header[name] = values
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		},
	}
	server.createProject(t)
	server.callers = []statusChangeCaller{server.personCaller("an identity-aware proxy's signed assertion with no cookie")}
	return server
}

// TestOnlyABrowserSignInClosesChildrenWithTheirParent: include_children with done is taken only
// from a request that presents no token and that the browser identity names: a sign-in cookie, a
// trusted identity header, or an identity-aware proxy's signed assertion with no Dispatch cookie.
// Every bearer branch of optionalActor is refused 403 BROWSER_SIGN_IN_REQUIRED before anything is
// written: the shared token, a personal token, one holding the person scope, a service-account
// token, a broker token acting as its session, and one scoped to its person. Both hold with the
// setting off too.
func TestOnlyABrowserSignInClosesChildrenWithTheirParent(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("with the rule on %t", on), func(t *testing.T) {
			for _, server := range []statusChangeServer{tokenCallersServer(t, on), cookieServer(t, on), iapServer(t, on)} {
				for c, caller := range server.callers {
					t.Run(caller.name, func(t *testing.T) {
						parent := server.seed(t, fmt.Sprintf("Parent %d of %s", c+1, server.name), "in_progress", "")
						child := server.seed(t, fmt.Sprintf("Child %d of %s", c+1, server.name), "todo", parent)
						path := "/api/v1/issues/" + parent + "/status-change"
						response := caller.send(t, http.MethodPost, path, map[string]any{"status": "done", "include_children": true})
						rig := ruleRig{database: server.database}
						if caller.person {
							if response.Code != http.StatusOK {
								t.Fatalf("POST %s from %s: status=%d body=%s, want 200", path, caller.name, response.Code, response.Body.String())
							}
							rig.requireIssue(t, parent, "done", "")
							rig.requireIssue(t, child, "done", parent)
							return
						}
						requireStatusChange(t, "POST "+path+" from "+caller.name, response, http.StatusForbidden,
							wantRefusal("BROWSER_SIGN_IN_REQUIRED", browserSignInRefusal))
						rig.requireIssue(t, parent, "in_progress", "")
						rig.requireIssue(t, child, "todo", parent)
						requireComments(t, server.database, []string{parent, child})
					})
				}
			}
		})
	}
}

// TestATokenBesideABrowserIdentityIsTheTokens: a request that presents a token beside a browser
// identity, a personal token beside the trusted identity header or the shared token beside a
// sign-in cookie, is the token's. Its include_children with done is refused as a token's, and the
// write of a call it is allowed records the session the token acts as, not the person.
func TestATokenBesideABrowserIdentityIsTheTokens(t *testing.T) {
	tokens := tokenCallersServer(t, true)
	cookies := cookieServer(t, true)
	for _, beside := range []struct {
		name          string
		server        statusChangeServer
		authorization string
	}{
		{"a personal token beside the trusted identity header", tokens, "Bearer " + personalTokenOwnedBy(t, tokens.database, brokerTestPerson)},
		{"the shared token beside a sign-in cookie", cookies, "Bearer agent-token"},
	} {
		t.Run(beside.name, func(t *testing.T) {
			server := beside.server
			parent := server.seed(t, "Parent changed from beside a person", "in_progress", "")
			child := server.seed(t, "Child of that parent", "todo", parent)
			path := "/api/v1/issues/" + parent + "/status-change"
			send := func(body map[string]any) *httptest.ResponseRecorder {
				body["actor"] = statusChangeSession
				return server.beside(t, http.MethodPost, path, body, beside.authorization)
			}
			requireStatusChange(t, "POST "+path+" with "+beside.name, send(map[string]any{"status": "done", "include_children": true}),
				http.StatusForbidden, wantRefusal("BROWSER_SIGN_IN_REQUIRED", browserSignInRefusal))
			rig := ruleRig{database: server.database}
			rig.requireIssue(t, parent, "in_progress", "")
			rig.requireIssue(t, child, "todo", parent)

			if response := send(map[string]any{"status": "backlog", "include_children": true}); response.Code != http.StatusOK {
				t.Fatalf("POST %s to backlog with %s: status=%d body=%s, want 200", path, beside.name, response.Code, response.Body.String())
			}
			for _, key := range []string{parent, child} {
				rig.requireIssue(t, key, "backlog", map[string]string{parent: "", child: parent}[key])
				if actor := latestEventActor(t, server.database, key, "issue.updated"); actor.Kind != "session" || actor.ID != statusChangeSession["id"] {
					t.Errorf("the issue.updated of %s is %s %s, want the token's session %s", key, actor.Kind, actor.ID, statusChangeSession["id"])
				}
			}
		})
	}
}

// TestAStatusChangesCommentNamesItsCallerAsItsCredentialProvedIt: the comment of an option names
// its caller from what the request's credential proved. A session that the body names under the
// deployment's shared token, which proves none, is quoted as code: the id with every backtick,
// line ending, other control character, format character, variation selector and other
// default-ignorable character removed, cut to claimedSessionNameMax runes, in one code span, so a
// Markdown link, a new line and a block line in the id stay inside that span, the comment reads as
// one paragraph that links nowhere, and no bidirectional control or zero-width character of the id
// gets into it. An id the cleaning empties is "a session". A personal token names the person it
// belongs to, whatever session its body names.
func TestAStatusChangesCommentNamesItsCallerAsItsCredentialProvedIt(t *testing.T) {
	rig := newStatusChangeRig(t, true)
	// started starts a new issue on the backlog, under a new parent on the backlog, with
	// include_parent and actor as the body's actor, sent by send, and answers the parent, which
	// the call moves into work and comments on, and the issue.
	started := func(
		t *testing.T, label string, actor map[string]any, send func(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder,
	) (string, string) {
		t.Helper()
		parent := rig.file(t, label+" parent", "backlog", "")
		named := rig.file(t, label+" issue", "backlog", parent)
		path := "/api/v1/issues/" + named + "/status-change"
		if response := send(t, path, map[string]any{"status": "todo", "include_parent": true, "actor": actor}); response.Code != http.StatusOK {
			t.Fatalf("POST %s, %s: status=%d body=%s, want 200", path, label, response.Code, response.Body.String())
		}
		return parent, named
	}
	shared := func(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
		return sessionRequest(t, rig.handler, http.MethodPost, path, body)
	}
	// moved is the comment that the call on named by caller leaves on parent, and listed the one it
	// leaves on named, which lists that move.
	moved := func(parent, named, caller string) string {
		return optionComment(parent, fmt.Sprintf(
			"Dispatch moved this issue from `backlog` to `in_progress`, because %s's status change of %s passed `include_parent`.", caller, named))
	}
	listed := func(parent, named, caller string) string {
		return namedComment(named, caller, "`include_parent`", movedLine(parent, "backlog", "in_progress", optionIncludeParent))
	}

	t.Run("a session the shared token's body names is code that holds no link, line or block", func(t *testing.T) {
		const id = "ses_`evil`\r\n[a link](https://evil.example/x)\n\n:::decision\nWhich one?\n:::\npast the cut"
		// The id with no backtick and no line ending, cut at 64 runes: where its block's closing
		// ::: ends.
		const quoted = "ses_evil[a link](https://evil.example/x):::decisionWhich one?:::"
		parent, named := started(t, "Named by the body", map[string]any{"kind": "session", "id": id}, shared)
		requireComments(t, rig.database, []string{parent, named}, moved(parent, named, "`"+quoted+"`"), listed(parent, named, "`"+quoted+"`"))
		var body string
		if err := rig.database.Pool.QueryRow(context.Background(), `select body from comments where issue_key = $1`, parent).Scan(&body); err != nil {
			t.Fatalf("read the comment on %s: %v", parent, err)
		}
		tree := pmdoc.ReadForDisplay(body, pmdoc.DisplayLatest)
		if len(tree.Children) != 1 || tree.Children[0].Type != "paragraph" {
			t.Fatalf("the comment on %s reads as %d blocks, want one paragraph: %q", parent, len(tree.Children), body)
		}
		var code []string
		for _, text := range tree.Children[0].Children {
			for _, mark := range text.Marks {
				switch mark.Type {
				case "link":
					t.Errorf("the comment on %s links %q to %v, want no link", parent, text.Text, mark.Attrs)
				case "inlineCode":
					code = append(code, text.Text)
				}
			}
		}
		if !slices.Contains(code, quoted) {
			t.Errorf("the code spans of the comment on %s are %q, want the session's id %q among them", parent, code, quoted)
		}
	})

	t.Run("a session id the cleaning empties is a session", func(t *testing.T) {
		parent, named := started(t, "Named by backticks alone", map[string]any{"kind": "session", "id": "``\n`\t`"}, shared)
		requireComments(t, rig.database, []string{parent, named}, moved(parent, named, "a session"), listed(parent, named, "a session"))
	})

	t.Run("a session id is quoted with no bidirectional control and no zero-width character", func(t *testing.T) {
		// U+202E would turn the text after the span right to left. U+200B, the combining grapheme
		// joiner U+034F, the variation selector U+FE0F and the Hangul filler U+3164 each stay in the
		// span with no width: only U+200B of them is a format character.
		parent, named := started(t, "Named with format characters", map[string]any{"kind": "session", "id": "ses_\u202Eevil\u200B\u034F\uFE0F\u3164_id"}, shared)
		requireComments(t, rig.database, []string{parent, named}, moved(parent, named, "`ses_evil_id`"), listed(parent, named, "`ses_evil_id`"))
	})

	t.Run("a session id of format characters and other zero-width characters alone is a session", func(t *testing.T) {
		parent, named := started(t, "Named by zero-width characters alone", map[string]any{"kind": "session", "id": "\u202E\u200B\u2066\u200E\u061C\uFEFF\u034F\uFE0F\u3164"}, shared)
		requireComments(t, rig.database, []string{parent, named}, moved(parent, named, "a session"), listed(parent, named, "a session"))
	})

	t.Run("a personal token names its owner, not the session its body names", func(t *testing.T) {
		token := personalTokenOwnedBy(t, rig.database, "bob")
		parent, named := started(t, "Named by a personal token", map[string]any{"kind": "session", "id": "ses_personal_token"},
			func(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
				return agentRequest(t, rig.handler, http.MethodPost, path, body, token)
			})
		requireComments(t, rig.database, []string{parent, named}, moved(parent, named, "bob"), listed(parent, named, "bob"))
	})
}

// TestUnlinkChildrenMakesEachARoot: unlink_children removes each open child of the issue from it,
// a child of every group, so each one becomes a root with its status and the issues under it; a
// done child stays. Each unlinked child has one comment that names the call, and the issue the
// call names has one after them, which names the call and lists each unlink.
func TestUnlinkChildrenMakesEachARoot(t *testing.T) {
	rig := newStatusChangeRig(t, true)
	named := rig.file(t, "Parent closed without its children", "in_progress", "")
	triaged := rig.file(t, "Child in triage", model.IssueStatusTriage, named)
	backlog := rig.file(t, "Child on the backlog", "backlog", named)
	grandchild := rig.file(t, "Grandchild on the backlog", "backlog", backlog)
	planned := rig.file(t, "Child in todo", "todo", named)
	running := rig.file(t, "Child in work", "in_progress", named)
	done := rig.file(t, "Child already done", "done", named)
	seq := map[string]int{}
	for _, key := range []string{named, triaged, backlog, planned, running} {
		seq[key] = issueLastSeq(t, rig.database, key)
	}
	write := statusChangeOf(named, map[string]any{"status": "done", "unlink_children": true})
	unlinked := func(key, status string) wantChange { return wantChange{key, status, "", seq[key], seq[key] + 2} }
	requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
		wantChange{named, "done", "", seq[named] + 4, seq[named] + 5},
		unlinked(triaged, model.IssueStatusTriage), unlinked(backlog, "backlog"), unlinked(planned, "todo"), unlinked(running, "in_progress"),
		wantChange{named, "done", "", seq[named] + 4, seq[named] + 5},
	))
	rig.requireIssue(t, named, "done", "")
	rig.requireIssue(t, triaged, model.IssueStatusTriage, "")
	rig.requireIssue(t, backlog, "backlog", "")
	rig.requireIssue(t, grandchild, "backlog", backlog)
	rig.requireIssue(t, planned, "todo", "")
	rig.requireIssue(t, running, "in_progress", "")
	rig.requireIssue(t, done, "done", named)
	removed := func(key string) string {
		return optionComment(key, fmt.Sprintf("Dispatch removed this issue from %s, because alice's status change of %s passed `unlink_children`.", named, named))
	}
	requireComments(t, rig.database, []string{named, triaged, backlog, grandchild, planned, running, done},
		removed(triaged), removed(backlog), removed(planned), removed(running),
		namedComment(named, "alice", "`unlink_children`", removedLine(triaged, named), removedLine(backlog, named),
			removedLine(planned, named), removedLine(running, named)))
}

// TestIncludeParentMovesTheAncestorsIntoWork: include_parent moves each ancestor of the issue that
// is not in work to in_progress, highest first, and keeps an ancestor in work as it is. A done
// ancestor is reopened into in_progress, which the reopen of its done child under it needs:
// with no option the rule refuses that reopen. Each moved ancestor has one comment that names the
// call, and the issue the call names has one after them, which names the call and lists each
// move; it has none when every ancestor is in work already.
func TestIncludeParentMovesTheAncestorsIntoWork(t *testing.T) {
	rig := newStatusChangeRig(t, true)

	t.Run("two ancestors that have not started, highest first", func(t *testing.T) {
		top := rig.file(t, "Ancestor in work", "in_progress", "")
		upper := rig.file(t, "Upper ancestor on the backlog", "backlog", top)
		lower := rig.file(t, "Lower ancestor on the backlog", "backlog", upper)
		named := rig.file(t, "Issue started", "backlog", lower)
		seq := map[string]int{}
		for _, key := range []string{upper, lower, named} {
			seq[key] = issueLastSeq(t, rig.database, key)
		}
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_parent": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{named, "todo", lower, seq[named], seq[named] + 1},
			wantChange{upper, "in_progress", top, seq[upper], seq[upper] + 2},
			wantChange{lower, "in_progress", upper, seq[lower], seq[lower] + 2},
			wantChange{named, "todo", lower, seq[named], seq[named] + 1},
		))
		rig.requireIssue(t, top, "in_progress", "")
		rig.requireIssue(t, upper, "in_progress", top)
		rig.requireIssue(t, lower, "in_progress", upper)
		rig.requireIssue(t, named, "todo", lower)
		moved := func(key string) string {
			return optionComment(key, fmt.Sprintf("Dispatch moved this issue from `backlog` to `in_progress`, because alice's status change of %s passed `include_parent`.", named))
		}
		requireComments(t, rig.database, []string{top, upper, lower, named}, moved(upper), moved(lower),
			namedComment(named, "alice", "`include_parent`", movedLine(upper, "backlog", "in_progress", optionIncludeParent),
				movedLine(lower, "backlog", "in_progress", optionIncludeParent)))
	})

	t.Run("a done ancestor is reopened", func(t *testing.T) {
		parent := rig.file(t, "Ancestor closed", "in_progress", "")
		named := rig.file(t, "Issue reopened under it", "done", parent)
		setIssueStatus(t, rig.handler, parent, "done")
		reopen := statusChangeOf(named, map[string]any{"status": "backlog"})
		requireStatusChange(t, reopen.String(), rig.send(t, reopen), http.StatusConflict, wantConflict([]ruleHolder{
			{parent, "Ancestor closed", "done", "parent", parent + " (done) is the parent of " + named + " and is done"},
		}))
		seq := map[string]int{parent: issueLastSeq(t, rig.database, parent), named: issueLastSeq(t, rig.database, named)}
		write := statusChangeOf(named, map[string]any{"status": "backlog", "include_parent": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{named, "backlog", parent, seq[named], seq[named] + 1},
			wantChange{parent, "in_progress", "", seq[parent], seq[parent] + 2},
			wantChange{named, "backlog", parent, seq[named], seq[named] + 1},
		))
		rig.requireIssue(t, parent, "in_progress", "")
		rig.requireIssue(t, named, "backlog", parent)
		requireComments(t, rig.database, []string{parent, named}, optionComment(parent,
			fmt.Sprintf("Dispatch moved this issue from `done` to `in_progress`, because alice's status change of %s passed `include_parent`.", named)),
			namedComment(named, "alice", "`include_parent`", movedLine(parent, "done", "in_progress", optionIncludeParent)))
	})

	t.Run("an ancestor in work is kept, and the issue gets no comment", func(t *testing.T) {
		parent := rig.file(t, "Ancestor kept in work", "in_progress", "")
		named := rig.file(t, "Issue started under it", "backlog", parent)
		seq := issueLastSeq(t, rig.database, named)
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_parent": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{named, "todo", parent, seq, seq + 1},
			wantChange{named, "todo", parent, seq, seq + 1},
		))
		requireComments(t, rig.database, []string{parent, named})
	})
}

// statusChangeCase is a call with one option over issues it seeds first, in a state that keeps
// the rule.
type statusChangeCase struct {
	option string
	// seed files the case's issues, titled after label, and answers the issue the call names, the
	// call's body, and each issue it filed.
	seed func(t *testing.T, rig ruleRig, label string) (named string, body map[string]any, filed []string)
}

// statusChangeCases are a call of each option.
var statusChangeCases = []statusChangeCase{
	{optionIncludeChildren, func(t *testing.T, rig ruleRig, label string) (string, map[string]any, []string) {
		named := rig.file(t, label+" parent", "in_progress", "")
		child := rig.file(t, label+" child", "backlog", named)
		return named, map[string]any{"status": "todo", "include_children": true}, []string{named, child}
	}},
	{optionUnlinkChildren, func(t *testing.T, rig ruleRig, label string) (string, map[string]any, []string) {
		named := rig.file(t, label+" parent", "in_progress", "")
		child := rig.file(t, label+" child", "todo", named)
		return named, map[string]any{"status": "done", "unlink_children": true}, []string{named, child}
	}},
	{optionIncludeParent, func(t *testing.T, rig ruleRig, label string) (string, map[string]any, []string) {
		parent := rig.file(t, label+" parent", "backlog", "")
		named := rig.file(t, label+" child", "backlog", parent)
		return named, map[string]any{"status": "todo", "include_parent": true}, []string{parent, named}
	}},
}

// withMembers is body with the members given added.
func withMembers(body map[string]any, members map[string]any) map[string]any {
	out := maps.Clone(body)
	maps.Copy(out, members)
	return out
}

// changedOf is each entry of a call's answer as key, status and parent.
func changedOf(t *testing.T, response *httptest.ResponseRecorder) []string {
	t.Helper()
	answer := decodeBody[statusChangeAnswer](t, response)
	listed := make([]string, 0, len(answer.Changed))
	for _, entry := range answer.Changed {
		listed = append(listed, entry.Key+" "+entry.Status+" under "+deref(entry.Parent))
	}
	return listed
}

// keyedStatusChange sends write as alice under the Idempotency-Key key.
func (rig ruleRig) keyedStatusChange(t *testing.T, write ruleWrite, key string) *httptest.ResponseRecorder {
	t.Helper()
	return keyedRequest(t, rig.handler, write.method, write.path, write.body, "alice", key)
}

// TestADryRunWritesNothing: a dry run of each option answers each issue the call would change,
// with the status and the parent the call would leave, the issue's last_seq now and no issue, and
// writes nothing: no row, no event, no comment, and under a request key no recovery point. Its
// entries name the issues, statuses and parents of the real call's. The issue the call names is in
// both when the call writes it the status it has, since its own step writes it then too. Its 200
// goes out through the route's layers as the handler wrote it (addChangedToRefusals holds it and
// writes it unchanged): Content-Type application/json, and the bytes WriteJSON writes for the
// answer, with no changed added or moved. With the rule off, a dry run of a call the rule would
// refuse answers 409 PARENT_STATUS_CONFLICT as with it on, and the real call is taken.
func TestADryRunWritesNothing(t *testing.T) {
	rig := newStatusChangeRig(t, true)
	for index, test := range statusChangeCases {
		t.Run(test.option, func(t *testing.T) {
			named, body, _ := test.seed(t, rig.ruleRig, "Dry run of "+test.option)
			dry := statusChangeOf(named, withMembers(body, map[string]any{"dry_run": true}))
			before := rig.quiet(t)
			response := rig.send(t, dry)
			assertUnchanged(t, dry.String(), before, databaseFingerprint(t, rig.database))
			if response.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s, want 200", dry, response.Code, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
				t.Errorf("%s answered Content-Type %q, want application/json", dry, contentType)
			}
			answer := decodeBody[statusChangeAnswer](t, response)
			direct := httptest.NewRecorder()
			WriteJSON(direct, http.StatusOK, answer)
			if !bytes.Equal(response.Body.Bytes(), direct.Body.Bytes()) {
				t.Errorf("%s answered %s, want the handler's answer as WriteJSON writes it, %s", dry, response.Body.String(), direct.Body.String())
			}
			for _, entry := range answer.Changed {
				if entry.Issue != nil {
					t.Errorf("the dry run's entry of %s holds an issue, want none", entry.Key)
				}
				if now := issueLastSeq(t, rig.database, entry.Key); entry.PreviousLastSeq != now {
					t.Errorf("the dry run's entry of %s has previous_last_seq %d, want the last_seq now, %d", entry.Key, entry.PreviousLastSeq, now)
				}
			}
			if answer.Issue.Key != named || answer.Issue.LastSeq != issueLastSeq(t, rig.database, named) {
				t.Errorf("the dry run answered the issue %s at last_seq %d, want %s as it stands", answer.Issue.Key, answer.Issue.LastSeq, named)
			}
			key := fmt.Sprintf("status-change-dry-run-%d", index)
			if keyed := rig.keyedStatusChange(t, dry, key); keyed.Code != http.StatusOK {
				t.Fatalf("%s under a request key: status=%d body=%s, want 200", dry, keyed.Code, keyed.Body.String())
			}
			var pointless bool
			if err := rig.database.Pool.QueryRow(context.Background(), `
				select recovery is null from request_keys where idempotency_key = $1
			`, key).Scan(&pointless); err != nil || !pointless {
				t.Errorf("the keyed dry run left a recovery point (%v), want none", err)
			}
			real := statusChangeOf(named, body)
			taken := rig.send(t, real)
			if taken.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s, want 200", real, taken.Code, taken.Body.String())
			}
			if planned, made := changedOf(t, response), changedOf(t, taken); !slices.Equal(planned, made) {
				t.Errorf("the dry run answered %q, and the call changed %q", planned, made)
			}
		})
	}

	t.Run("the issue the call writes at the status it has is in the dry run as in the call", func(t *testing.T) {
		named := rig.file(t, "Parent kept in work", "in_progress", "")
		child := rig.file(t, "Child moved into work", "backlog", named)
		body := map[string]any{"status": "in_progress", "include_children": true}
		dry := statusChangeOf(named, withMembers(body, map[string]any{"dry_run": true}))
		planned := rig.send(t, dry)
		if planned.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", dry, planned.Code, planned.Body.String())
		}
		real := statusChangeOf(named, body)
		taken := rig.send(t, real)
		if taken.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", real, taken.Code, taken.Body.String())
		}
		want := []string{named + " in_progress under ", child + " in_progress under " + named}
		if got := changedOf(t, planned); !slices.Equal(got, want) {
			t.Errorf("the dry run answered %q, want %q: the step of %s writes in_progress again", got, want, named)
		}
		if got := changedOf(t, taken); !slices.Equal(got, want) {
			t.Errorf("the call changed %q, want %q: the step of %s writes in_progress again", got, want, named)
		}
	})

	t.Run("a call the rule refuses, with the rule off", func(t *testing.T) {
		off := newStatusChangeRig(t, false)
		named := off.file(t, "Parent closed over an open child", "in_progress", "")
		child := off.file(t, "Open child", "todo", named)
		dry := statusChangeOf(named, map[string]any{"status": "done", "dry_run": true})
		before := off.quiet(t)
		requireStatusChange(t, dry.String(), off.send(t, dry), http.StatusConflict, wantConflict([]ruleHolder{
			{child, "Open child", "todo", "child", child + " (todo) is a child of " + named + " and is not done"},
		}))
		assertUnchanged(t, dry.String(), before, databaseFingerprint(t, off.database))
		real := statusChangeOf(named, map[string]any{"status": "done"})
		if taken := off.send(t, real); taken.Code != http.StatusOK {
			t.Fatalf("%s with the rule off: status=%d body=%s, want 200", real, taken.Code, taken.Body.String())
		}
		off.requireIssue(t, named, "done", "")
		off.requireIssue(t, child, "todo", named)
	})
}

// TestTheOptionsRideARouteOfTheirOwn: the PATCH takes none of the options. Its include_children is
// a member it does not declare, so the close moves no child: with the rule off it closes the
// parent alone, and with the rule on it is refused for the open child.
func TestTheOptionsRideARouteOfTheirOwn(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("with the rule on %t", on), func(t *testing.T) {
			rig := newRuleRig(t, on)
			parent := rig.file(t, "Parent patched with an option", "in_progress", "")
			child := rig.file(t, "Child the option would move", "todo", parent)
			write := patchOf(parent, map[string]any{"status": "done", "include_children": true})
			response := rig.send(t, write)
			if on {
				requireRuleRefusal(t, write.String(), response, []ruleHolder{
					{child, "Child the option would move", "todo", "child", child + " (todo) is a child of " + parent + " and is not done"},
				})
				rig.requireIssue(t, parent, "in_progress", "")
			} else {
				if response.Code != http.StatusOK {
					t.Fatalf("%s with the rule off: status=%d body=%s, want 200", write, response.Code, response.Body.String())
				}
				rig.requireIssue(t, parent, "done", "")
			}
			rig.requireIssue(t, child, "todo", parent)
		})
	}
}

// TestAStatusChangeRefusedMidwayNamesWhatItChanged: a step that another writer's write between two
// steps made the rule refuse stops the call. Here a writer files an issue under a child after the
// first child closed, so the second child's close is refused: the call answers that refusal's
// status, code and body, with changed holding the first child's entry as its write answered it,
// and no step after it runs. A call refused before its first step answers changed [].
func TestAStatusChangeRefusedMidwayNamesWhatItChanged(t *testing.T) {
	rig := newStatusChangeRig(t, true)
	named := rig.file(t, "Parent closed with its children", "in_progress", "")
	first := rig.file(t, "First child", "todo", named)
	second := rig.file(t, "Second child", "todo", named)
	seq := issueLastSeq(t, rig.database, first)
	var late string
	hooked := 0
	rig.afterStep(t, func(ctx context.Context, step int) {
		hooked++
		if step != 0 {
			return
		}
		created := rig.send(t, createUnder(second, "Filed between two steps"))
		if created.Code != http.StatusCreated {
			t.Fatalf("file an issue under %s between two steps: status=%d body=%s", second, created.Code, created.Body.String())
		}
		late = decodeBody[model.Issue](t, created).Key
	})
	write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true})
	response := rig.send(t, write)
	requireStatusChange(t, write.String(), response, http.StatusConflict, wantConflict([]ruleHolder{
		{late, "Filed between two steps", model.IssueStatusTriage, "child", late + " (triage) is a child of " + second + " and is not done"},
	}, wantChange{first, "done", named, seq, seq + 2}))
	if hooked != 1 {
		t.Errorf("the hook ran after %d steps, want after the one step that committed", hooked)
	}
	refusal := decodeBody[struct {
		Changed []statusChangeEntry `json:"changed"`
	}](t, response)
	if issue := refusal.Changed[0].Issue; issue == nil || issue.ClosedAt == nil || issue.Title != "First child" {
		t.Errorf("the entry of %s holds %+v, want the closed issue as its write answered it", first, issue)
	}
	rig.requireIssue(t, first, "done", named)
	rig.requireIssue(t, second, "todo", named)
	rig.requireIssue(t, named, "in_progress", "")
	requireComments(t, rig.database, []string{named, first, second, late}, optionComment(first,
		fmt.Sprintf("Dispatch moved this issue from `todo` to `done` with %s, because alice's status change of %s passed `include_children`.", named, named)))

	other := rig.file(t, "Parent refused before its first step", "in_progress", "")
	child := rig.file(t, "Child that refuses it", "todo", other)
	refused := statusChangeOf(other, map[string]any{"status": "done"})
	requireStatusChange(t, refused.String(), rig.send(t, refused), http.StatusConflict, wantConflict([]ruleHolder{
		{child, "Child that refuses it", "todo", "child", child + " (todo) is a child of " + other + " and is not done"},
	}))
}

// TestAStatusChangeRefusedByItsRequestKeyAnswersChanged: every refusal that the route's handler
// answers carries changed beside its code, [] when no step committed before it (plan v19, "The
// contract"; the check c-route-fence: "a call refused before its first step answers an empty
// changed"). The refusals of the call's request key carry it too. A repeat while the first holder
// runs (409 KEY_IN_FLIGHT, a 409 whose declared body, StatusChangeRefusal, requires changed) names
// the entries the running call has committed: here its step 0, the named issue's own move into
// work. The key sent again with another body (422 KEY_REUSED) and a malformed key (400
// KEY_INVALID), under which nothing ran, answer changed [].
func TestAStatusChangeRefusedByItsRequestKeyAnswersChanged(t *testing.T) {
	rig := newRepeatRig(t)
	named := rig.file(t, "Issue whose key is refused", "backlog", "")
	rig.file(t, "Child of the keyed call", "backlog", named)
	const key = "refused-by-its-key"
	write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true})
	requireEmptyChanged := func(t *testing.T, request string, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode the answer %s: %v", request, response.Body.String(), err)
		}
		changed, ok := body["changed"].([]any)
		if response.Code != status || body["code"] != code || !ok || len(changed) != 0 {
			t.Errorf("%s answered %d %s, want %d %s with changed []", request, response.Code, response.Body.String(), status, code)
		}
	}

	resume := rig.stopAfterStep(t, write, key, 0)
	held := write.String() + " again while the first holder runs"
	inFlight := rig.keyedStatusChange(t, write, key)
	var body map[string]any
	if err := json.Unmarshal(inFlight.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode the answer %s: %v", held, inFlight.Body.String(), err)
	}
	changed, _ := body["changed"].([]any)
	var entry map[string]any
	if len(changed) == 1 {
		entry, _ = changed[0].(map[string]any)
	}
	parent, hasParent := entry["parent"]
	if inFlight.Code != http.StatusConflict || body["code"] != contracts.IdempotencyKeyInFlight || len(changed) != 1 ||
		entry["key"] != named || entry["status"] != "todo" || !hasParent || parent != nil {
		t.Errorf("%s answered %d %s, want %d %s with changed the one entry of %s's own step, at todo with parent null",
			held, inFlight.Code, inFlight.Body.String(), http.StatusConflict, contracts.IdempotencyKeyInFlight, named)
	}
	if first := resume(); first.Code != http.StatusOK {
		t.Fatalf("the first run answered %d %s once it went on, want 200", first.Code, first.Body.String())
	}
	other := statusChangeOf(named, map[string]any{"status": "backlog", "include_children": true})
	requireEmptyChanged(t, other.String()+" under the first call's key", rig.keyedStatusChange(t, other, key),
		http.StatusUnprocessableEntity, contracts.IdempotencyKeyReused)
	requireEmptyChanged(t, write.String()+" under a malformed key", rig.keyedStatusChange(t, write, "not a key!"),
		http.StatusBadRequest, contracts.IdempotencyKeyInvalid)
}

// TestARefusalBeforeTheKeyedLayerAnswersChanged: a refusal by a layer register puts outside the
// route's keyed layer carries changed [] too, since the route's declared 403 and 409 bodies
// (StatusChangeRefusal) require changed, and nothing ran under such a refusal. The layers here are
// the read-only gate (a read-only token), the sign-in check (a broker token that names a service
// and no person) and the Dispatch-Login check (a write pressed by another person). Each answers a
// status change with the status, the code and every other member it answers PATCH
// /api/v1/issues/{key} with for the same credential, and with changed [] beside them, which the
// PATCH's answer does not carry, under a Content-Length that counts it. Nothing is written.
func TestARefusalBeforeTheKeyedLayerAnswersChanged(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{parentStatusRule: true})
	agent := newBrokerCaller(t)
	const readOnly, serviceOnly = "a-read-only-token-that-changes-a-status", "a-token-naming-no-person-that-changes-a-status"
	rig.broker.Says(readOnly, readOnlyToken(agent.jkt))
	rig.broker.Says(serviceOnly, serviceOnlyToken(agent.jkt))
	server := statusChangeServer{
		name: "the token server", project: brokerTestProject, database: rig.database,
		beside: func(t *testing.T, method, path string, body map[string]any, authorization string) *httptest.ResponseRecorder {
			return rig.serve(withPerson(brokerRequest(t, method, path, body, authorization, ""), brokerTestPerson))
		},
	}
	server.createProject(t)
	named := server.seed(t, "Issue refused before the keyed layer", "todo", "")
	broker := func(token string) func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.serve(agent.presenting(t, method, path, token, body))
		}
	}
	pressedByAnother := func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
		request := withPerson(brokerRequest(t, method, path, body, "", ""), brokerTestPerson)
		request.Header.Set(contracts.DispatchLoginHeader, "someone-else@d.example")
		return rig.serve(request)
	}
	changePath, patchPath := "/api/v1/issues/"+named+"/status-change", "/api/v1/issues/"+named
	for _, refused := range []struct {
		name   string
		send   func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"a read-only token", broker(readOnly), http.StatusForbidden, contracts.DispatchReadOnlyToken},
		{"a broker token that names no person", broker(serviceOnly), http.StatusForbidden, contracts.DispatchSignInRequired},
		{"a write pressed by another person", pressedByAnother, http.StatusConflict, contracts.DispatchLoginChanged},
	} {
		t.Run(refused.name, func(t *testing.T) {
			change := refused.send(t, http.MethodPost, changePath, map[string]any{"status": "done"})
			patch := refused.send(t, http.MethodPatch, patchPath, map[string]any{"status": "done"})
			changeBody, patchBody := decodeBody[map[string]any](t, change), decodeBody[map[string]any](t, patch)
			if _, carries := patchBody["changed"]; patch.Code != refused.status || patchBody["code"] != refused.code || carries {
				t.Fatalf("PATCH %s from %s answered %d %s, want %d %s with no changed",
					patchPath, refused.name, patch.Code, patch.Body.String(), refused.status, refused.code)
			}
			changed, listed := changeBody["changed"].([]any)
			others := maps.Clone(changeBody)
			delete(others, "changed")
			if change.Code != patch.Code || !listed || len(changed) != 0 || !reflect.DeepEqual(others, patchBody) {
				t.Errorf("POST %s from %s answered %d %s, want the PATCH's %d %s with changed [] beside it",
					changePath, refused.name, change.Code, change.Body.String(), patch.Code, patch.Body.String())
			}
			if length := change.Header().Get("Content-Length"); length != fmt.Sprint(change.Body.Len()) {
				t.Errorf("POST %s from %s answered Content-Length %q for its body of %d bytes",
					changePath, refused.name, length, change.Body.Len())
			}
		})
	}
	ruleRig{database: rig.database}.requireIssue(t, named, "todo", "")
}

// TestARepeatOfAStatusChangeAnswersTheIssuesItsStepsRead: every answer that carries a committed
// step's entry carries that entry's issue as the step read it, not as the issue stands when the
// answer is written, and the answer's own issue as the uninterrupted call answers it (spec v10: a
// repeat "answers with the entries that the first run's committed steps read, which its recovery
// point stores"; c-route-fence: "the entries of the first run's steps being the ones those steps
// read"; the architect's ruling of 2026-11-21 in round 2 of testing). A client such as the Legion
// daemon takes every event of an entry's issue after previous_last_seq up to issue.last_seq as the
// call's own, so an issue read after another writer touched it hands that writer's events to the
// call. Here another writer labels a step's issue after the step committed and before each answer:
// a KEY_IN_FLIGHT while the first run holds the key, a replay of the finished call, and the answer
// of an attempt that resumed a call whose holder died. Each answers the steps' reads: the named
// issue's own step moves it into work (one event), and each child's step moves it with a comment
// (two events), and no label the other writer added is in any issue of the answer.
func TestARepeatOfAStatusChangeAnswersTheIssuesItsStepsRead(t *testing.T) {
	rig := newRepeatRig(t)
	label := func(t *testing.T, issue, label string) {
		t.Helper()
		if labelled := rig.send(t, patchOf(issue, map[string]any{"labels": []string{label}})); labelled.Code != http.StatusOK {
			t.Fatalf("label %s: status=%d body=%s", issue, labelled.Code, labelled.Body.String())
		}
	}
	requireNoLabel := func(t *testing.T, what string, response *httptest.ResponseRecorder) {
		t.Helper()
		answer := decodeBody[struct {
			Issue   *model.Issue        `json:"issue"`
			Changed []statusChangeEntry `json:"changed"`
		}](t, response)
		if answer.Issue != nil && len(answer.Issue.Labels) != 0 {
			t.Errorf("%s answered its issue %s with the labels %v another writer added after the call's step, want the step's read",
				what, answer.Issue.Key, answer.Issue.Labels)
		}
		for _, entry := range answer.Changed {
			if entry.Issue != nil && len(entry.Issue.Labels) != 0 {
				t.Errorf("%s answered the entry of %s with the labels %v another writer added after its step, want the step's read",
					what, entry.Key, entry.Issue.Labels)
			}
		}
	}
	// expect is requireStatusChange that goes on after a mismatch, so a held run is always let go
	// and each answer of the test is judged.
	expect := func(t *testing.T, request string, response *httptest.ResponseRecorder, status int, want map[string]any) {
		t.Helper()
		if got := projectedStatusChange(t, response); response.Code != status || !reflect.DeepEqual(got, any(want)) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Errorf("%s: status=%d body=%s\nwant %d %s", request, response.Code, gotJSON, status, wantJSON)
		}
	}

	t.Run("while the first run holds the key, and after it finished", func(t *testing.T) {
		named := rig.file(t, "Issue whose step another writer touches", "backlog", "")
		child := rig.file(t, "Child whose step another writer touches", "backlog", named)
		seq := rig.seqs(t, named, child)
		const key = "read-by-its-steps"
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true})
		namedStep := wantChange{named, "todo", "", seq[named], seq[named] + 1}
		want := wantAnswer(namedStep, namedStep, wantChange{child, "todo", named, seq[child], seq[child] + 2})

		resume := rig.stopAfterStep(t, write, key, 0)
		label(t, named, "touched-while-the-call-is-held")
		inFlight := rig.keyedStatusChange(t, write, key)
		expect(t, write.String()+" again while the first run holds the key", inFlight, http.StatusConflict,
			wantRefusal(contracts.IdempotencyKeyInFlight, fmt.Sprintf(
				"the first request under %s %q is still running; this one was not run: ask again under the same key",
				contracts.IdempotencyKeyHeader, key), namedStep))
		requireNoLabel(t, "the KEY_IN_FLIGHT", inFlight)
		first := resume()
		expect(t, write.String(), first, http.StatusOK, want)
		requireNoLabel(t, "the first run", first)

		label(t, named, "touched-after-the-call")
		label(t, child, "touched-after-the-call")
		again := rig.keyedStatusChange(t, write, key)
		if !replayed(again) {
			t.Errorf("the repeat of the finished call answered %d %s, not marked replayed", again.Code, again.Body.String())
		}
		expect(t, write.String()+" repeated after it finished", again, http.StatusOK, want)
		requireNoLabel(t, "the replay", again)
	})

	t.Run("an attempt that resumed a call whose holder died", func(t *testing.T) {
		named := rig.file(t, "Issue whose resumed call another writer touches", "backlog", "")
		first := rig.file(t, "First child of the resumed call", "backlog", named)
		second := rig.file(t, "Second child of the resumed call", "backlog", named)
		seq := rig.seqs(t, named, first, second)
		const key = "read-by-its-steps-resumed"
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true})
		namedStep := wantChange{named, "todo", "", seq[named], seq[named] + 1}

		resume := rig.stopAfterStep(t, write, key, 1)
		rig.lapse(t, key)
		label(t, named, "touched-before-the-repeat")
		label(t, first, "touched-before-the-repeat")
		repeat := rig.keyedStatusChange(t, write, key)
		expect(t, write.String()+" repeated past the lease", repeat, http.StatusOK, wantAnswer(namedStep,
			namedStep,
			wantChange{first, "todo", named, seq[first], seq[first] + 2},
			wantChange{second, "todo", named, seq[second], seq[second] + 2},
		))
		requireNoLabel(t, "the resumed attempt", repeat)
		resume()
	})
}

// ruleState is an issue's status and parent, as a replay of the call's writes holds them.
type ruleState struct {
	status string
	parent *string
}

// statesOf reads the status and the parent of each issue of keys.
func (rig ruleRig) statesOf(t *testing.T, keys ...string) map[string]ruleState {
	t.Helper()
	states := make(map[string]ruleState, len(keys))
	for _, key := range keys {
		status, parent := rig.issue(t, key)
		states[key] = ruleState{status, parent}
	}
	return states
}

// breaches is each pair of states that breaks R24, as "<child> under <parent>".
func breaches(states map[string]ruleState) []string {
	var broken []string
	for key, state := range states {
		if state.parent == nil {
			continue
		}
		if parent, ok := states[*state.parent]; ok && pairBreaksTheRule(state.status, parent.status) {
			broken = append(broken, fmt.Sprintf("%s (%s) under %s (%s)", key, state.status, *state.parent, parent.status))
		}
	}
	slices.Sort(broken)
	return broken
}

// requireEachWriteKeepsTheRule replays the status and parent writes of the issues of states that
// the events after the event id after record, in the order of their ids, over states, which keep
// the rule, and fails t for each write that leaves a state that breaks it.
func requireEachWriteKeepsTheRule(t *testing.T, database *store.Store, after int64, states map[string]ruleState) {
	t.Helper()
	if broken := breaches(states); len(broken) > 0 {
		t.Fatalf("the fixture breaks the rule before the call: %v", broken)
	}
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	rows, err := database.Pool.Query(context.Background(), `
		select id, issue_key, payload ->> 'status', payload ->> 'parent' from events
		where id > $1 and issue_key = any($2) and type in ('issue.updated', 'issue.closed') order by id
	`, after, keys)
	if err != nil {
		t.Fatalf("read the call's writes: %v", err)
	}
	type write struct {
		id     int64
		key    string
		status string
		parent *string
	}
	writes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (write, error) {
		var w write
		err := row.Scan(&w.id, &w.key, &w.status, &w.parent)
		return w, err
	})
	if err != nil {
		t.Fatalf("read the call's writes: %v", err)
	}
	if len(writes) == 0 {
		t.Fatal("the call wrote no status and no parent")
	}
	for _, w := range writes {
		states[w.key] = ruleState{w.status, w.parent}
		if broken := breaches(states); len(broken) > 0 {
			t.Errorf("after event %d (%s to %s), the state breaks the rule: %v", w.id, w.key, w.status, broken)
		}
	}
}

// requireTheCallWroteItsEntries holds each entry of answer to what the call wrote. Each event of
// the entry's issue after its previous_last_seq, up to its issue's last_seq, was appended after the
// event id after, when the call began. Each event of the issue after that, up to its last_seq now,
// is a child.status that a later step of the call appended, but for one comment.created on named,
// the issue the call names, after them: the comment that lists what the options changed, which
// the call writes after its last step. The answer's issue is the named issue's entry's.
func requireTheCallWroteItsEntries(t *testing.T, database *store.Store, after int64, named string, answer statusChangeAnswer) {
	t.Helper()
	for index, entry := range answer.Changed {
		if entry.Issue == nil {
			t.Fatalf("the entry of %s holds no issue", entry.Key)
		}
		later := map[string]bool{}
		for _, next := range answer.Changed[index+1:] {
			later[next.Key] = true
		}
		rows, err := database.Pool.Query(context.Background(), `
			select id, seq, type, coalesce(payload ->> 'child_key', '') from events where issue_key = $1 and seq > $2 order by seq
		`, entry.Key, entry.PreviousLastSeq)
		if err != nil {
			t.Fatalf("read the events of %s: %v", entry.Key, err)
		}
		commented := false
		for rows.Next() {
			var id int64
			var seq int
			var kind, child string
			if err := rows.Scan(&id, &seq, &kind, &child); err != nil {
				t.Fatalf("read an event of %s: %v", entry.Key, err)
			}
			switch {
			case id <= after:
				t.Errorf("the event %d of %s at seq %d, after the entry's previous_last_seq %d, was appended before the call", id, entry.Key, seq, entry.PreviousLastSeq)
			case seq > entry.Issue.LastSeq && entry.Key == named && kind == "comment.created" && !commented:
				commented = true
			case seq > entry.Issue.LastSeq && (commented || kind != "child.status" || !later[child]):
				t.Errorf("the event %d of %s at seq %d, after the entry's last_seq %d, is %s of %q, want a child.status of a later step, then the comment on %s",
					id, entry.Key, seq, entry.Issue.LastSeq, kind, child, named)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read the events of %s: %v", entry.Key, err)
		}
		var now int
		if err := database.Pool.QueryRow(context.Background(), `select last_seq from issues where key = $1`, entry.Key).Scan(&now); err != nil || now < entry.Issue.LastSeq {
			t.Errorf("%s stands at last_seq %d (%v), below its entry's %d", entry.Key, now, err, entry.Issue.LastSeq)
		}
		if entry.Key == named {
			got, _ := json.Marshal(answer.Issue)
			want, _ := json.Marshal(entry.Issue)
			if !bytes.Equal(got, want) {
				t.Errorf("the answer's issue is %s, want its entry's %s", got, want)
			}
		}
	}
}

// reopenNotes is a second document of the issue the reopen case closes, with a decision block whose
// ask the close withdraws and the reopen restores.
const reopenNotes = "Notes context\n\n:::ask{#reopen-decision urgency=\"med\" multiple=\"false\"}\nShip the notes?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// TestTheStatusChangeRouteTakesThePatchFence: R20 on the route. A call that sends
// expected_sequence is fenced as the PATCH is: with the rule on and off, for each option and for
// its dry run, a stale value is 409 ISSUE_CHANGED with nothing written, decided before
// ISSUE_CLOSED, and the issue's own last_seq is taken. A call with no expected_sequence is taken,
// and actor names the write's actor. A fenced call is fenced again at each step, with every event
// that its earlier steps appended on the issue it names counted as its own: its own step's into
// work, and the document work of a reopen through the ledger's sink. Each entry holds what its
// step wrote, and each status and parent write of the call leaves a state that keeps the rule.
// Through the step hook, another writer's event on the named issue stops the call at the next
// step, and an issue that another writer moved first is left as that writer left it.
func TestTheStatusChangeRouteTakesThePatchFence(t *testing.T) {
	const changedSince = "issue changed since the hand-over read"

	t.Run("a stale expected_sequence refuses each option and its dry run", func(t *testing.T) {
		for _, on := range []bool{true, false} {
			rig := newStatusChangeRig(t, on)
			for _, test := range statusChangeCases {
				t.Run(fmt.Sprintf("%s with the rule on %t", test.option, on), func(t *testing.T) {
					named, body, _ := test.seed(t, rig.ruleRig, "Fenced "+test.option)
					stale := issueLastSeq(t, rig.database, named)
					if retitled := rig.send(t, patchOf(named, map[string]any{"title": "Retitled by another writer"})); retitled.Code != http.StatusOK {
						t.Fatalf("retitle %s: status=%d body=%s", named, retitled.Code, retitled.Body.String())
					}
					for _, dry := range []bool{false, true} {
						write := statusChangeOf(named, withMembers(body, map[string]any{"expected_sequence": stale, "dry_run": dry}))
						before := rig.quiet(t)
						requireStatusChange(t, write.String(), rig.send(t, write), http.StatusConflict, wantRefusal("ISSUE_CHANGED", changedSince))
						assertUnchanged(t, write.String(), before, databaseFingerprint(t, rig.database))
					}
					current := statusChangeOf(named, withMembers(body, map[string]any{"expected_sequence": issueLastSeq(t, rig.database, named)}))
					if taken := rig.send(t, current); taken.Code != http.StatusOK {
						t.Fatalf("%s with the issue's own last_seq: status=%d body=%s, want 200", current, taken.Code, taken.Body.String())
					}
				})
			}
		}
	})

	// The subtests below file issues of their own on one server with the rule on.
	rig := newStatusChangeRig(t, true)

	t.Run("a stale done on an issue closed since the read is ISSUE_CHANGED, not ISSUE_CLOSED", func(t *testing.T) {
		named := rig.file(t, "Issue closed since the read", "in_progress", "")
		stale := issueLastSeq(t, rig.database, named)
		setIssueStatus(t, rig.handler, named, "done")
		write := statusChangeOf(named, map[string]any{"status": "done", "unlink_children": true, "expected_sequence": stale})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusConflict, wantRefusal("ISSUE_CHANGED", changedSince))
		unfenced := statusChangeOf(named, map[string]any{"status": "done", "unlink_children": true})
		requireStatusChange(t, unfenced.String(), rig.send(t, unfenced), http.StatusConflict, wantRefusal("ISSUE_CLOSED", "issue is closed"))
	})

	t.Run("no expected_sequence is taken, and actor names the write's actor", func(t *testing.T) {
		named := rig.file(t, "Issue moved by a session", "in_progress", "")
		child := rig.file(t, "Child moved by the session", "backlog", named)
		path := "/api/v1/issues/" + named + "/status-change"
		if taken := sessionRequest(t, rig.handler, http.MethodPost, path, map[string]any{
			"status": "todo", "include_children": true, "actor": sessionActor(),
		}); taken.Code != http.StatusOK {
			t.Fatalf("POST %s with no expected_sequence: status=%d body=%s, want 200", path, taken.Code, taken.Body.String())
		}
		for _, key := range []string{named, child} {
			rig.requireIssue(t, key, "todo", map[string]string{named: "", child: named}[key])
			if actor := latestEventActor(t, rig.database, key, "issue.updated"); actor.Kind != "session" || actor.ID != sessionActor()["id"] {
				t.Errorf("the issue.updated of %s is %s %s, want the session the body names", key, actor.Kind, actor.ID)
			}
		}
	})

	t.Run("a fenced call into work is taken after its own step", func(t *testing.T) {
		named := rig.file(t, "Issue started with its tree", "backlog", "")
		child := rig.file(t, "Child on the backlog", "backlog", named)
		grandchild := rig.file(t, "Grandchild on the backlog", "backlog", child)
		iced := rig.file(t, "Child on ice", "icebox", named)
		seq := map[string]int{}
		for _, key := range []string{named, child, grandchild, iced} {
			seq[key] = issueLastSeq(t, rig.database, key)
		}
		dry := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true, "expected_sequence": seq[named], "dry_run": true})
		before := rig.quiet(t)
		requireStatusChange(t, dry.String(), rig.send(t, dry), http.StatusOK, map[string]any{
			"issue": wantChange{named, "backlog", "", 0, seq[named]}.issue(),
			"changed": entries([]wantChange{
				{named, "todo", "", seq[named], -1}, {child, "todo", named, seq[child], -1},
				{iced, "todo", named, seq[iced], -1}, {grandchild, "todo", child, seq[grandchild], -1},
			}),
		})
		assertUnchanged(t, dry.String(), before, databaseFingerprint(t, rig.database))

		states := rig.statesOf(t, named, child, grandchild, iced)
		started := lastEventID(t, rig.database)
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true, "expected_sequence": seq[named]})
		response := rig.send(t, write)
		requireStatusChange(t, write.String(), response, http.StatusOK, wantAnswer(
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{child, "todo", named, seq[child], seq[child] + 2},
			wantChange{iced, "todo", named, seq[iced], seq[iced] + 2},
			wantChange{grandchild, "todo", child, seq[grandchild], seq[grandchild] + 2},
		))
		requireTheCallWroteItsEntries(t, rig.database, started, named, decodeBody[statusChangeAnswer](t, response))
		requireEachWriteKeepsTheRule(t, rig.database, started, states)
	})

	t.Run("a reopen's own events through the ledger's sink refuse none of the later steps", func(t *testing.T) {
		ctx := context.Background()
		handler, database, documents := closedSettlementHandlerSettlingAfter(t, time.Hour)
		issue := createInteractionIssue(t, handler, "TEST", "Issue reopened with the issues under it", "Context\n")
		named, spec := issue.Key, issue.PrimaryArtifactID
		settle := func(what string) {
			t.Helper()
			if err := documents.SettleIssueDocuments(ctx, named); err != nil {
				t.Fatalf("settle %s: %v", what, err)
			}
		}
		settle("the spec")
		awaitSettled(t, database, spec)
		notes := uploadDocument(t, handler, "/api/v1/issues/"+named, "notes.md", reopenNotes).Artifact
		settle("the notes")
		awaitIndexedAskBlock(t, handler, notes.ID, "reopen-decision", "Ship the notes?")
		anchored := createThreadComment(t, handler, named, map[string]any{
			"body": "Does this still hold?", "anchor": map[string]any{"artifact": notes.ID, "quote": "Notes context"},
		}, "alice")
		requestApproval(t, handler, spec)
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": "Child left on the backlog", "parent": named, "force": true,
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("file the child: status=%d body=%s", created.Code, created.Body.String())
		}
		child := decodeBody[model.Issue](t, created).Key
		if moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"status": "backlog"}, "alice"); moved.Code != http.StatusOK {
			t.Fatalf("move the child to backlog: status=%d body=%s", moved.Code, moved.Body.String())
		}
		settle("the issue's documents")
		awaitSettled(t, database, spec, notes.ID)
		// The settle delay is an hour, so no settlement runs between two steps of the call: the test
		// runs each one itself. The browser's edit inside the close margin is settled into a version
		// while the issue is closed, which the reopen's follow then moves the approval request to.
		peer := connectBrowserPeer(t, documents, spec)
		closeIssueThroughPatch(t, handler, database, named)
		stored := storedUpdateCount(t, database, spec)
		peer.edit(t, replaceFirstParagraph("Context", "Context, edited in the margin"))
		peer.barrier(t)
		awaitStoredUpdates(t, database, spec, stored+1)
		settle("the closed issue's documents")
		awaitSettled(t, database, spec, notes.ID)
		peer.closeAndWait(t)
		staleAnchorQuote(t, database, anchored.ID)

		check := ruleRig{database: database}
		seq := issueLastSeq(t, database, named)
		started := lastEventID(t, database)
		path := "/api/v1/issues/" + named + "/status-change"
		response := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
			"status": "in_progress", "include_children": true, "expected_sequence": seq,
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("POST %s: status=%d body=%s, want 200", path, response.Code, response.Body.String())
		}
		answer := decodeBody[statusChangeAnswer](t, response)
		if len(answer.Changed) != 2 || answer.Changed[0].Key != named || answer.Changed[1].Key != child {
			t.Fatalf("the call changed %+v, want %s and then %s", answer.Changed, named, child)
		}
		own := issueEventsAfter(t, database, named, seq)
		kinds := eventKinds(own[:answer.Changed[0].Issue.LastSeq-seq])
		for _, kind := range []string{"ask.opened", "comment.anchor_refreshed", "ask.edited", "issue.updated"} {
			if !slices.Contains(kinds, kind) {
				t.Errorf("the reopen's own step appended %v on %s, want a %s among them", kinds, named, kind)
			}
		}
		check.requireIssue(t, named, "in_progress", "")
		check.requireIssue(t, child, "in_progress", named)
		requireTheCallWroteItsEntries(t, database, started, named, answer)
	})

	t.Run("another writer's event on the named issue stops the call at the next step", func(t *testing.T) {
		named := rig.file(t, "Issue retitled during its call", "backlog", "")
		first := rig.file(t, "First child", "backlog", named)
		second := rig.file(t, "Second child", "backlog", named)
		seq := map[string]int{named: issueLastSeq(t, rig.database, named), first: issueLastSeq(t, rig.database, first)}
		rig.afterStep(t, func(ctx context.Context, step int) {
			if step != 1 {
				return
			}
			if retitled := rig.send(t, patchOf(named, map[string]any{"title": "Retitled between two steps"})); retitled.Code != http.StatusOK {
				t.Fatalf("retitle %s between two steps: status=%d body=%s", named, retitled.Code, retitled.Body.String())
			}
		})
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true, "expected_sequence": seq[named]})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusConflict, wantRefusal("ISSUE_CHANGED", changedSince,
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{first, "todo", named, seq[first], seq[first] + 2},
		))
		rig.requireIssue(t, named, "todo", "")
		rig.requireIssue(t, first, "todo", named)
		rig.requireIssue(t, second, "backlog", named)
		var title string
		if err := rig.database.Pool.QueryRow(context.Background(), `select title from issues where key = $1`, named).Scan(&title); err != nil || title != "Retitled between two steps" {
			t.Errorf("%s is titled %q (%v), want the other writer's title", named, title, err)
		}
		requireComments(t, rig.database, []string{named, first, second}, optionComment(first,
			fmt.Sprintf("Dispatch moved this issue from `backlog` to `todo` with %s, because alice's status change of %s passed `include_children`.", named, named)))
	})

	t.Run("a grandchild another writer closed between two steps stays done", func(t *testing.T) {
		named := rig.file(t, "Issue put back on the backlog", "in_progress", "")
		child := rig.file(t, "Child in work", "in_progress", named)
		first := rig.file(t, "First grandchild", "todo", child)
		second := rig.file(t, "Grandchild closed by another writer", "todo", child)
		seq := map[string]int{
			named: issueLastSeq(t, rig.database, named), child: issueLastSeq(t, rig.database, child), first: issueLastSeq(t, rig.database, first),
		}
		rig.afterStep(t, func(ctx context.Context, step int) {
			if step == 0 {
				setIssueStatus(t, rig.handler, second, "done")
			}
		})
		write := statusChangeOf(named, map[string]any{"status": "backlog", "include_children": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{named, "backlog", "", seq[named] + 1, seq[named] + 2},
			wantChange{first, "backlog", child, seq[first], seq[first] + 2},
			wantChange{child, "backlog", named, seq[child] + 2, seq[child] + 4},
			wantChange{named, "backlog", "", seq[named] + 1, seq[named] + 2},
		))
		rig.requireIssue(t, second, "done", child)
		moved := func(key, from string) string {
			return optionComment(key, fmt.Sprintf("Dispatch moved this issue from `%s` to `backlog` with %s, because alice's status change of %s passed `include_children`.", from, named, named))
		}
		requireComments(t, rig.database, []string{named, child, first, second}, moved(first, "todo"), moved(child, "in_progress"),
			namedComment(named, "alice", "`include_children`", movedLine(first, "todo", "backlog", optionIncludeChildren),
				movedLine(child, "in_progress", "backlog", optionIncludeChildren)))
	})

	t.Run("an ancestor another writer moved into work between two steps keeps that status", func(t *testing.T) {
		upper := rig.file(t, "Upper ancestor", "backlog", "")
		lower := rig.file(t, "Lower ancestor moved by another writer", "backlog", upper)
		named := rig.file(t, "Issue started", "backlog", lower)
		seq := map[string]int{upper: issueLastSeq(t, rig.database, upper), named: issueLastSeq(t, rig.database, named)}
		rig.afterStep(t, func(ctx context.Context, step int) {
			if step == 0 {
				setIssueStatus(t, rig.handler, lower, "todo")
			}
		})
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_parent": true})
		requireStatusChange(t, write.String(), rig.send(t, write), http.StatusOK, wantAnswer(
			wantChange{named, "todo", lower, seq[named], seq[named] + 1},
			wantChange{upper, "in_progress", "", seq[upper], seq[upper] + 2},
			wantChange{named, "todo", lower, seq[named], seq[named] + 1},
		))
		rig.requireIssue(t, lower, "todo", upper)
		// The comment on the named issue lists the one ancestor the call moved, not the plan's two.
		requireComments(t, rig.database, []string{upper, lower, named}, optionComment(upper,
			fmt.Sprintf("Dispatch moved this issue from `backlog` to `in_progress`, because alice's status change of %s passed `include_parent`.", named)),
			namedComment(named, "alice", "`include_parent`", movedLine(upper, "backlog", "in_progress", optionIncludeParent)))
	})
}

// TestAStatusChangeChecksItsPlanUnderItsLocks: the route reads its plan again under the owner
// locks of every issue the plan writes. A writer that holds the owner lock of one step's
// issue makes a dry run wait for it, which pg_locks shows, and the open child that writer files
// under that issue before it commits is in the dry run's answer.
func TestAStatusChangeChecksItsPlanUnderItsLocks(t *testing.T) {
	ctx := context.Background()
	rig := newStatusChangeRig(t, true)
	named := rig.file(t, "Parent whose plan is read under its locks", "in_progress", "")
	child := rig.file(t, "Child whose lock a writer holds", "todo", named)
	awaitNoSettlementOwed(t, rig.database)
	var number int
	if err := rig.database.Pool.QueryRow(ctx, `
		update projects set next_number = next_number + 1 where key = $1 returning next_number - 1
	`, ruleProject).Scan(&number); err != nil {
		t.Fatalf("take the late child's number: %v", err)
	}
	late := fmt.Sprintf("%s-%d", ruleProject, number)
	hold, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the writer: %v", err)
	}
	defer hold.Rollback(ctx)
	if err := store.LockOwners(ctx, hold, store.IssueOwner(child)); err != nil {
		t.Fatalf("take the owner lock of %s: %v", child, err)
	}
	if _, err := hold.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		values ($1, $2, $3, 'Child filed under the lock', $4, '{"kind":"user","id":"alice"}', 'zz')
	`, late, ruleProject, number, child); err != nil {
		t.Fatalf("file the late child: %v", err)
	}
	handler, pid := oneConnectionHandler(t, rig.deps)
	write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true, "dry_run": true})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- dispatchRequest(t, handler, write.method, write.path, write.body, "alice") }()
	testwait.Eventually(t, write.String()+" to wait on the writer's lock", func() bool {
		select {
		case response := <-answered:
			t.Fatalf("%s answered %d before it waited on the writer: %s", write, response.Code, response.Body.String())
		default:
		}
		queued, err := storetest.QueuedBehind(ctx, hold, pid)
		if err != nil {
			t.Fatal(err)
		}
		return queued
	})
	if err := hold.Commit(ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
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
	seq := map[string]int{
		named: issueLastSeq(t, rig.database, named), child: issueLastSeq(t, rig.database, child), late: issueLastSeq(t, rig.database, late),
	}
	requireStatusChange(t, write.String(), response, http.StatusOK, map[string]any{
		"issue": wantChange{named, "in_progress", "", 0, seq[named]}.issue(),
		"changed": entries([]wantChange{
			{late, "done", child, seq[late], -1}, {child, "done", named, seq[child], -1}, {named, "done", "", seq[named], -1},
		}),
	})
}

// repeatRig is a status change rig, the rule on, whose keyed calls a test stops as a process that
// died leaves them: the lease of a stopped holder is never renewed, and the test lapses it itself
// (lapse). sweeps records each ask sweep of a closed issue.
type repeatRig struct {
	statusChangeRig
	sweeps *sweptAsks
}

func newRepeatRig(t *testing.T) repeatRig {
	t.Helper()
	rig := newStatusChangeRig(t, true)
	rig.served.requestKeyRenewal = time.Hour
	sweeps := &sweptAsks{API: rig.served.deps.Docs, swept: map[string][][]string{}}
	rig.served.deps.Docs = sweeps
	return repeatRig{statusChangeRig: rig, sweeps: sweeps}
}

// sweptAsks is the document service with each ask sweep of a closed issue recorded: every write of
// an issue's ask states that marks its unindexed blocks resolved, which only that sweep makes
// (sweepIssueAskBlocksTx), as the ids of the asks it wrote, by issue.
type sweptAsks struct {
	docs.API
	mu    sync.Mutex
	swept map[string][][]string
}

func (s *sweptAsks) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]docs.BlockRef, error) {
	if markUnindexed {
		ids := make([]string, 0, len(asks))
		for _, ask := range asks {
			ids = append(ids, ask.ID)
		}
		s.mu.Lock()
		s.swept[issueKey] = append(s.swept[issueKey], ids)
		s.mu.Unlock()
	}
	return s.API.WriteIssueAskStates(ctx, issueKey, asks, actor, markUnindexed)
}

// sweptOf is each ask sweep of issue, as the ids of the asks it wrote.
func (rig repeatRig) sweptOf(issue string) [][]string {
	rig.sweeps.mu.Lock()
	defer rig.sweeps.mu.Unlock()
	return slices.Clone(rig.sweeps.swept[issue])
}

// requireSweptOnce fails t unless the asks of issue were swept once, and that sweep wrote ask.
func (rig repeatRig) requireSweptOnce(t *testing.T, issue, ask string) {
	t.Helper()
	if swept := rig.sweptOf(issue); len(swept) != 1 || !slices.Contains(swept[0], ask) {
		t.Errorf("the ask sweeps of %s wrote %v, want one sweep that wrote the withdrawn ask %s", issue, swept, ask)
	}
}

// seqs reads the last_seq of each of keys.
func (rig repeatRig) seqs(t *testing.T, keys ...string) map[string]int {
	t.Helper()
	seq := make(map[string]int, len(keys))
	for _, key := range keys {
		seq[key] = issueLastSeq(t, rig.database, key)
	}
	return seq
}

// stopAfterStep sends write under key in the background and stops its run once the step at index
// step of its plan has committed, as a process that died there leaves its key: no answer, and a
// lease nobody renews, which lapse ages. Only the first run to reach that step stops. resume lets
// the stopped run go on and answers what it answered.
func (rig repeatRig) stopAfterStep(t *testing.T, write ruleWrite, key string, step int) (resume func() *httptest.ResponseRecorder) {
	t.Helper()
	stopped := make(chan struct{})
	release, releaseOnce := heldUntilReleased(t)
	var once sync.Once
	rig.afterStep(t, func(_ context.Context, index int) {
		if index != step {
			return
		}
		stops := false
		once.Do(func() { stops = true })
		if stops {
			close(stopped)
			<-release
		}
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.keyedStatusChange(t, write, key) }()
	select {
	case <-stopped:
	case response := <-answered:
		t.Fatalf("%s answered %d before its step %d committed: %s", write, response.Code, step, response.Body.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never reached its step %d", write, step)
	}
	return func() *httptest.ResponseRecorder {
		t.Helper()
		releaseOnce()
		select {
		case response := <-answered:
			return response
		case <-time.After(30 * time.Second):
			t.Fatalf("the stopped run of %s never answered once it went on", write)
			return nil
		}
	}
}

// lapse ages the lease of key, which no answer holds, past IdempotencyLease, as a holder whose
// process died leaves it.
func (rig repeatRig) lapse(t *testing.T, key string) {
	t.Helper()
	tag, err := rig.database.Pool.Exec(context.Background(), `
		update request_keys set leased_at = now() - interval '2 minutes' where idempotency_key = $1 and status is null
	`, key)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("lapse the lease of %s: %v (%d rows)", key, err, tag.RowsAffected())
	}
}

// statusChangeKeyRow is the request_keys row of a status change's key: the attempt that holds it,
// the stored answer's status (nil for none), whether its lease is released, and its recovery point
// as every reader reads it, with the steps' entries (recoveryPointRead; nil for none).
type statusChangeKeyRow struct {
	attempt  int
	status   *int
	released bool
	point    *statusChangeRecovery
}

func (rig repeatRig) keyRow(t *testing.T, key string) statusChangeKeyRow {
	t.Helper()
	var row statusChangeKeyRow
	var recovery []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select attempt, status, leased_at is null, `+recoveryPointRead+` from request_keys where idempotency_key = $1
	`, key).Scan(&row.attempt, &row.status, &row.released, &recovery); err != nil {
		t.Fatalf("read the key %s: %v", key, err)
	}
	if recovery != nil {
		row.point = &statusChangeRecovery{}
		if err := json.Unmarshal(recovery, row.point); err != nil {
			t.Fatalf("decode the recovery point of %s: %v", key, err)
		}
	}
	return row
}

// pointKeys is the key of each entry the row's recovery point holds, in order.
func (row statusChangeKeyRow) pointKeys() []string {
	if row.point == nil {
		return nil
	}
	keys := make([]string, 0, len(row.point.Changed))
	for _, entry := range row.point.Changed {
		keys = append(keys, entry.Key)
	}
	return keys
}

// issueEventCount is how many events of kind issue holds.
func issueEventCount(t *testing.T, database *store.Store, issue, kind string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and type = $2
	`, issue, kind).Scan(&count); err != nil {
		t.Fatalf("count the %s events of %s: %v", kind, issue, err)
	}
	return count
}

// requireEachClosedOnce fails t unless each of keys holds one issue.closed event.
func requireEachClosedOnce(t *testing.T, database *store.Store, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if closed := issueEventCount(t, database, key, "issue.closed"); closed != 1 {
			t.Errorf("%s holds %d issue.closed events, want one", key, closed)
		}
	}
}

// unlinkComment is the comment of an unlink of key from named, as storedComments lists it.
func unlinkComment(key, named string) string {
	return optionComment(key, fmt.Sprintf("Dispatch removed this issue from %s, because alice's status change of %s passed `unlink_children`.", named, named))
}

// movedComment is the comment of include_children's move of key from status from to to with
// named, as storedComments lists it.
func movedComment(key, from, to, named string) string {
	return optionComment(key, fmt.Sprintf("Dispatch moved this issue from `%s` to `%s` with %s, because alice's status change of %s passed `include_children`.", from, to, named, named))
}

// TestEachFieldOfAStatusChangesRecoveryPointIsTaggedWithItsMember: each field of
// statusChangeRecovery has as its json tag the constant that names its member in every statement
// that writes or reads the point (pointChanged and the others), and each constant tags one field,
// so every json.Unmarshal of the point (resumedStatusChange, refuseStatusChangeKey,
// replayRecovered) names each member as those statements do.
func TestEachFieldOfAStatusChangesRecoveryPointIsTaggedWithItsMember(t *testing.T) {
	members := map[string]string{
		"Changed": pointChanged, "Events": pointEvents, "Withdrawn": pointWithdrawn, "Moved": pointMoved, "Finished": pointFinished,
		"Issue": pointIssue,
	}
	fields := reflect.VisibleFields(reflect.TypeFor[statusChangeRecovery]())
	if len(fields) != len(members) {
		t.Errorf("statusChangeRecovery has %d fields, want one for each of the %d members", len(fields), len(members))
	}
	for _, field := range fields {
		member, named := members[field.Name]
		if !named {
			t.Errorf("statusChangeRecovery.%s is the field of no member: name its member's constant here", field.Name)
			continue
		}
		if tag, _, _ := strings.Cut(field.Tag.Get("json"), ","); tag != member {
			t.Errorf("statusChangeRecovery.%s has the json tag %q, want %q, its member's constant", field.Name, tag, member)
		}
	}
}

// TestTheCommentOnTheNamedIssueListsWhatOneBlockHoldsAndCountsTheRest: the comment on the issue a
// call names lists the moves, in their order, while one block holds them, and its last line then
// counts the moves it leaves out. So the comment of a call whose options changed thousands of
// issues passes each check that queueIssueComment makes of its body: its line endings are line
// feeds, it holds text, and itemblocks.Check refuses none of its blocks. The list stops close to
// the bound, not far before it. The paragraph names the option of each move, of a move the list
// leaves out too. A call whose moves fit lists each of them and counts none.
func TestTheCommentOnTheNamedIssueListsWhatOneBlockHoldsAndCountsTheRest(t *testing.T) {
	const named = "ACME-1"
	// moved is count moves by include_children of the issues after first, then, past the cut, one
	// move by include_parent: no call writes in that order, so that only a move the list leaves out
	// brings its option.
	moved := func(first, count int) []statusChangeMove {
		moves := make([]statusChangeMove, 0, count+1)
		for index := range count {
			moves = append(moves, statusChangeMove{
				Key: fmt.Sprintf("ACME-%d", first+index), Option: optionIncludeChildren, From: "in_progress", To: "done",
			})
		}
		return append(moves, statusChangeMove{Key: "ACME-99999", Option: optionIncludeParent, From: "backlog", To: "in_progress"})
	}
	// The moves of an earlier attempt come first, all by include_children, then the attempt's own.
	earlier := moved(2, 3000)[:3000]
	run := &statusChangeRun{key: named, callerName: "alice", resumed: &statusChangeRecovery{Moved: earlier}, moved: moved(3002, 2000)}
	all := append(slices.Clone(run.resumed.Moved), run.moved...)
	body := run.namedComment()
	if err := itemblocks.Check("body", body); err != nil {
		t.Fatalf("the comment for %d moves is refused: %v", len(all), err)
	}
	blocks := itemblocks.Split(body)
	if len(blocks) != 2 {
		t.Fatalf("the comment is %d blocks, want a paragraph and a list", len(blocks))
	}
	want := "Dispatch changed the issues below, because alice's status change of " + named + " passed `include_children` and `include_parent`. " + statusChangeRule
	if blocks[0].Text != want {
		t.Errorf("the paragraph is %q, want %q", blocks[0].Text, want)
	}
	lines := strings.Split(blocks[1].Text, "\n")
	listed := len(lines) - 1
	if listed < 1 || listed >= len(all) {
		t.Fatalf("the list holds %d lines for %d moves, want the moves one block holds and one line that counts the rest", len(lines), len(all))
	}
	for index, line := range lines[:listed] {
		if move := all[index]; line != movedLine(move.Key, move.From, move.To, move.Option) {
			t.Fatalf("line %d of the list is %q, want the move of %s", index+1, line, move.Key)
		}
	}
	if count := fmt.Sprintf("- and %d more, each with a comment of its own", len(all)-listed); lines[listed] != count {
		t.Errorf("the last line of the list is %q, want %q", lines[listed], count)
	}
	if units := itemblocks.Units16(blocks[1].Text); units < contracts.DispatchBlockUnits-200 {
		t.Errorf("the list is %d units, want it to stop close to the bound of %d", units, contracts.DispatchBlockUnits)
	}

	// A list that holds every move counts none. namedComment is the comment as storedComments
	// lists it, after its issue and its author.
	few := &statusChangeRun{key: named, callerName: "alice", moved: moved(2, 2)}
	got := named + " " + parentStatusRuleActor.Kind + ":" + parentStatusRuleActor.ID + ": " + few.namedComment()
	if want := namedComment(named, "alice", "`include_children` and `include_parent`",
		movedLine("ACME-2", "in_progress", "done", optionIncludeChildren), movedLine("ACME-3", "in_progress", "done", optionIncludeChildren),
		movedLine("ACME-99999", "backlog", "in_progress", optionIncludeParent)); got != want {
		t.Errorf("the comment for three moves is\n%s\nwant\n%s", got, want)
	}
}

// TestARepeatOfAStatusChangeFinishesWhatItsFirstRunLeft: a keyed call of the route keeps its
// progress in its key's recovery point. A repeat whose holder died before the point's mark takes
// the key over as a new attempt, counts the events the point holds as the call's own in its fence,
// makes only the steps the point does not hold, and answers as the uninterrupted call would, with
// the first run's entries before its own; when the point holds the named issue's close, that
// close's ask sweep runs once, over the asks it withdrew, whatever the answer. A repeat of a call
// whose point carries the mark, or that answered a success beside its point, is answered from the
// point with no step and no fence. A stored refusal and a keyed dry run are replayed as they were
// stored, and a holder inside its lease is KEY_IN_FLIGHT. A repeat judged once its body arrived
// (answerDrainedRepeat) is judged the same way.
func TestARepeatOfAStatusChangeFinishesWhatItsFirstRunLeft(t *testing.T) {
	const changedSince = "issue changed since the hand-over read"
	rig := newRepeatRig(t)

	t.Run("a fenced unlink whose process died before the named issue's step is finished by its repeat", func(t *testing.T) {
		named := rig.file(t, "Issue closed after its unlinks", "in_progress", "")
		first := rig.file(t, "First child unlinked", "todo", named)
		second := rig.file(t, "Second child unlinked", "todo", named)
		seq := rig.seqs(t, named, first, second)
		const key = "repeat-unlinks"
		write := statusChangeOf(named, map[string]any{"status": "done", "unlink_children": true, "expected_sequence": seq[named]})
		resume := rig.stopAfterStep(t, write, key, 1)
		if held := rig.keyRow(t, key); !slices.Equal(held.pointKeys(), []string{first, second}) || len(held.point.Events) != 2 || held.point.Finished {
			t.Fatalf("the stopped run left the point %+v, want the two unlinks' entries and their child.removed events, unmarked", held.point)
		}
		rig.lapse(t, key)
		want := wantAnswer(
			wantChange{named, "done", "", seq[named] + 2, seq[named] + 3},
			wantChange{first, "todo", "", seq[first], seq[first] + 2},
			wantChange{second, "todo", "", seq[second], seq[second] + 2},
			wantChange{named, "done", "", seq[named] + 2, seq[named] + 3},
		)
		repeat := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated past the lease", repeat, http.StatusOK, want)
		if replayed(repeat) {
			t.Error("the repeat that took the key over is marked replayed")
		}
		if held := rig.keyRow(t, key); held.attempt != 2 || !held.point.Finished || !slices.Equal(held.pointKeys(), []string{first, second, named}) {
			t.Errorf("the key holds attempt %d and the point %+v, want attempt 2 with every entry, marked", held.attempt, held.point)
		}
		if stopped := resume(); stopped.Code != http.StatusConflict {
			t.Errorf("the stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
		}
		rig.requireIssue(t, named, "done", "")
		rig.requireIssue(t, first, "todo", "")
		rig.requireIssue(t, second, "todo", "")
		requireEachClosedOnce(t, rig.database, named)
		// The repeat that took the key over wrote the one comment on the named issue, which lists the
		// unlinks the first run made; the replay below writes no second one.
		comments := []string{
			unlinkComment(first, named), unlinkComment(second, named),
			namedComment(named, "alice", "`unlink_children`", removedLine(first, named), removedLine(second, named)),
		}
		requireComments(t, rig.database, []string{named, first, second}, comments...)
		again := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated after it finished", again, http.StatusOK, want)
		if !replayed(again) {
			t.Error("the repeat of the finished call is not marked replayed")
		}
		requireComments(t, rig.database, []string{named, first, second}, comments...)
	})

	t.Run("a resumed attempt cut off in its turn and repeated again makes every step once", func(t *testing.T) {
		named := rig.file(t, "Issue closed over three cut-off runs", "in_progress", "")
		children := []string{
			rig.file(t, "Child closed by the first run", "todo", named),
			rig.file(t, "Child closed by the second run", "todo", named),
			rig.file(t, "Child closed by the third run", "todo", named),
		}
		seq := rig.seqs(t, append([]string{named}, children...)...)
		const key = "repeat-twice"
		write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true})
		resumeFirst := rig.stopAfterStep(t, write, key, 0)
		rig.lapse(t, key)
		resumeSecond := rig.stopAfterStep(t, write, key, 0)
		if held := rig.keyRow(t, key); held.attempt != 2 || !slices.Equal(held.pointKeys(), children[:2]) {
			t.Fatalf("the second run left attempt %d with the point %+v, want attempt 2 with the first two children's entries", held.attempt, held.point)
		}
		rig.lapse(t, key)
		requireStatusChange(t, write.String()+" repeated a second time", rig.keyedStatusChange(t, write, key), http.StatusOK, wantAnswer(
			wantChange{named, "done", "", seq[named] + 3, seq[named] + 4},
			wantChange{children[0], "done", named, seq[children[0]], seq[children[0]] + 2},
			wantChange{children[1], "done", named, seq[children[1]], seq[children[1]] + 2},
			wantChange{children[2], "done", named, seq[children[2]], seq[children[2]] + 2},
			wantChange{named, "done", "", seq[named] + 3, seq[named] + 4},
		))
		for _, resume := range []func() *httptest.ResponseRecorder{resumeFirst, resumeSecond} {
			if stopped := resume(); stopped.Code != http.StatusConflict {
				t.Errorf("a stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
			}
		}
		requireEachClosedOnce(t, rig.database, append([]string{named}, children...)...)
		requireComments(t, rig.database, append([]string{named}, children...),
			movedComment(children[0], "todo", "done", named), movedComment(children[1], "todo", "done", named),
			movedComment(children[2], "todo", "done", named),
			namedComment(named, "alice", "`include_children`", movedLine(children[0], "todo", "done", optionIncludeChildren),
				movedLine(children[1], "todo", "done", optionIncludeChildren), movedLine(children[2], "todo", "done", optionIncludeChildren)))
	})

	t.Run("a run cut off at its time bound keeps its row with a released lease, and its repeat finishes", func(t *testing.T) {
		named := rig.file(t, "Issue whose run hit its bound", "in_progress", "")
		first := rig.file(t, "Child closed before the bound", "todo", named)
		second := rig.file(t, "Child closed by the repeat", "todo", named)
		seq := rig.seqs(t, named, first, second)
		const key = "repeat-cut-off"
		write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true})
		rig.served.requestKeyRunLimit = 5 * time.Second
		t.Cleanup(func() { rig.served.requestKeyRunLimit = 0 })
		var once sync.Once
		rig.afterStep(t, func(ctx context.Context, _ int) { once.Do(func() { <-ctx.Done() }) })
		if cut := rig.keyedStatusChange(t, write, key); cut.Code < http.StatusInternalServerError {
			t.Fatalf("%s cut off at its bound answered %d %s, want a 5xx", write, cut.Code, cut.Body.String())
		}
		rig.served.requestKeyRunLimit = 0
		if held := rig.keyRow(t, key); held.status != nil || !held.released || !slices.Equal(held.pointKeys(), []string{first}) {
			t.Fatalf("the cut-off run left status %v, a released lease %t and the point %+v, want no answer, the lease released and the first child's entry",
				held.status, held.released, held.point)
		}
		requireStatusChange(t, write.String()+" repeated after the bound", rig.keyedStatusChange(t, write, key), http.StatusOK, wantAnswer(
			wantChange{named, "done", "", seq[named] + 2, seq[named] + 3},
			wantChange{first, "done", named, seq[first], seq[first] + 2},
			wantChange{second, "done", named, seq[second], seq[second] + 2},
			wantChange{named, "done", "", seq[named] + 2, seq[named] + 3},
		))
		requireEachClosedOnce(t, rig.database, named, first, second)
	})

	t.Run("a repeat of a call that finished is answered from its point with no fence, after the close's sweep", func(t *testing.T) {
		named := rig.file(t, "Issue closed with an open ask", "in_progress", "")
		ask := createIssueAsk(t, rig.handler, named, "Ship the close?")
		seq := issueLastSeq(t, rig.database, named)
		const key = "repeat-finished"
		write := statusChangeOf(named, map[string]any{"status": "done", "expected_sequence": seq})
		first := rig.keyedStatusChange(t, write, key)
		if first.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", write, first.Code, first.Body.String())
		}
		rig.requireSweptOnce(t, named, ask)
		again := rig.keyedStatusChange(t, write, key)
		// The close's sweep ran after the first answer; the replay answers what the first did.
		requireStatusChange(t, write.String()+" repeated", again, http.StatusOK, projectedStatusChange(t, first).(map[string]any))
		if !replayed(again) {
			t.Error("the repeat of the finished call is not marked replayed")
		}
		rig.requireSweptOnce(t, named, ask)
		requireEachClosedOnce(t, rig.database, named)
	})

	t.Run("a marked point with no answer is answered from the point after another writer's write", func(t *testing.T) {
		named := rig.file(t, "Issue started whose answer was lost", "backlog", "")
		child := rig.file(t, "Child started whose answer was lost", "backlog", named)
		seq := rig.seqs(t, named, child)
		const key = "repeat-marked"
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true, "expected_sequence": seq[named]})
		if first := rig.keyedStatusChange(t, write, key); first.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", write, first.Code, first.Body.String())
		}
		loseAnswer(t, rig.database, key)
		if retitled := rig.send(t, patchOf(named, map[string]any{"title": "Retitled after the lost answer"})); retitled.Code != http.StatusOK {
			t.Fatalf("retitle %s: status=%d body=%s", named, retitled.Code, retitled.Body.String())
		}
		again := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated", again, http.StatusOK, wantAnswer(
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{child, "todo", named, seq[child], seq[child] + 2},
		))
		if !replayed(again) || decodeBody[statusChangeAnswer](t, again).Issue.Title != "Issue started whose answer was lost" {
			t.Errorf("the repeat answered %s, want the point replayed with the issue as its step answered it, before the retitle", again.Body.String())
		}
		if held := rig.keyRow(t, key); held.attempt != 1 {
			t.Errorf("the key holds attempt %d, want the first: a marked point is never taken over", held.attempt)
		}
	})

	t.Run("a point with every entry and no mark is taken over, and no step is left", func(t *testing.T) {
		named := rig.file(t, "Issue started whose holder died after its last step", "backlog", "")
		child := rig.file(t, "Last child started", "backlog", named)
		seq := rig.seqs(t, named, child)
		const key = "repeat-unmarked"
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true, "expected_sequence": seq[named]})
		resume := rig.stopAfterStep(t, write, key, 1)
		rig.lapse(t, key)
		before := lastEventID(t, rig.database)
		want := wantAnswer(
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{child, "todo", named, seq[child], seq[child] + 2},
		)
		requireStatusChange(t, write.String()+" repeated", rig.keyedStatusChange(t, write, key), http.StatusOK, want)
		// No step was left, so the repeat appends one event alone: the comment.created of the
		// comment on the named issue, which the stopped run had not written.
		var appended []string
		rows, err := rig.database.Pool.Query(context.Background(), `select issue_key || ' ' || type from events where id > $1 order by id`, before)
		if err == nil {
			appended, err = pgx.CollectRows(rows, pgx.RowTo[string])
		}
		if err != nil || !slices.Equal(appended, []string{named + " comment.created"}) {
			t.Errorf("the repeat appended %q (%v), want the comment.created of %s alone: no step was left", appended, err, named)
		}
		if held := rig.keyRow(t, key); held.attempt != 2 || !held.point.Finished {
			t.Errorf("the key holds attempt %d and the point %+v, want attempt 2, marked", held.attempt, held.point)
		}
		if stopped := resume(); stopped.Code != http.StatusConflict || refusalCode(stopped.Body.Bytes()) != contracts.IdempotencyKeyInFlight {
			t.Errorf("the stopped run went on and answered %d %s, want 409 %s: its mark found the key taken over",
				stopped.Code, stopped.Body.String(), contracts.IdempotencyKeyInFlight)
		}
		again := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated again", again, http.StatusOK, want)
		if !replayed(again) {
			t.Error("the repeat of the finished call is not marked replayed")
		}
		// The stopped run's comment rolled back with its refused mark, and the replay wrote none.
		requireComments(t, rig.database, []string{named, child}, movedComment(child, "backlog", "todo", named),
			namedComment(named, "alice", "`include_children`", movedLine(child, "backlog", "todo", optionIncludeChildren)))
	})

	t.Run("a point with no mark and another writer's event on the named issue answers ISSUE_CHANGED", func(t *testing.T) {
		named := rig.file(t, "Issue retitled after its holder died", "backlog", "")
		child := rig.file(t, "Child started before the retitle", "backlog", named)
		seq := rig.seqs(t, named, child)
		const key = "repeat-changed"
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true, "expected_sequence": seq[named]})
		resume := rig.stopAfterStep(t, write, key, 1)
		rig.lapse(t, key)
		if retitled := rig.send(t, patchOf(named, map[string]any{"title": "Retitled after the holder died"})); retitled.Code != http.StatusOK {
			t.Fatalf("retitle %s: status=%d body=%s", named, retitled.Code, retitled.Body.String())
		}
		before := lastEventID(t, rig.database)
		want := wantRefusal("ISSUE_CHANGED", changedSince,
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{child, "todo", named, seq[child], seq[child] + 2},
		)
		requireStatusChange(t, write.String()+" repeated", rig.keyedStatusChange(t, write, key), http.StatusConflict, want)
		if after := lastEventID(t, rig.database); after != before {
			t.Errorf("the refused repeat appended events up to %d after %d, want none", after, before)
		}
		again := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated again", again, http.StatusConflict, want)
		if !replayed(again) {
			t.Error("the repeat of the refusal is not marked replayed")
		}
		if stopped := resume(); stopped.Code != http.StatusConflict {
			t.Errorf("the stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
		}
	})

	t.Run("a point that holds the named issue's close sweeps its withdrawn ask once, refused or not", func(t *testing.T) {
		for _, refused := range []bool{false, true} {
			t.Run(fmt.Sprintf("another writer's event on the issue %t", refused), func(t *testing.T) {
				named := rig.file(t, fmt.Sprintf("Issue closed before its holder died, refused %t", refused), "in_progress", "")
				child := rig.file(t, fmt.Sprintf("Child closed first, refused %t", refused), "todo", named)
				ask := createIssueAsk(t, rig.handler, named, "Close it with its child?")
				seq := rig.seqs(t, named, child)
				key := fmt.Sprintf("repeat-closed-%t", refused)
				write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true, "expected_sequence": seq[named]})
				resume := rig.stopAfterStep(t, write, key, 1)
				// The close's step left the issue at this last_seq, which its stored entry holds.
				closed := issueLastSeq(t, rig.database, named)
				rig.lapse(t, key)
				if swept := rig.sweptOf(named); len(swept) != 0 {
					t.Fatalf("the close of %s was swept %v before its mark", named, swept)
				}
				if refused {
					createThreadComment(t, rig.handler, named, map[string]any{"body": "Closed while its call was cut off."}, "alice")
				}
				repeat := rig.keyedStatusChange(t, write, key)
				changed := []wantChange{
					{child, "done", named, seq[child], seq[child] + 2},
					{named, "done", "", seq[named] + 1, closed},
				}
				if refused {
					requireStatusChange(t, write.String()+" repeated", repeat, http.StatusConflict, wantRefusal("ISSUE_CHANGED", changedSince, changed...))
				} else {
					requireStatusChange(t, write.String()+" repeated", repeat, http.StatusOK, wantAnswer(changed[1], changed...))
				}
				rig.requireSweptOnce(t, named, ask)
				if stopped := resume(); stopped.Code != http.StatusConflict {
					t.Errorf("the stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
				}
				rig.requireSweptOnce(t, named, ask)
				requireEachClosedOnce(t, rig.database, named, child)
			})
		}
	})

	t.Run("a resumed attempt whose named issue another writer closed answers as the uninterrupted call would", func(t *testing.T) {
		for _, fenced := range []bool{true, false} {
			t.Run(fmt.Sprintf("fenced %t", fenced), func(t *testing.T) {
				named := rig.file(t, fmt.Sprintf("Issue closed by another writer, fenced %t", fenced), "in_progress", "")
				first := rig.file(t, fmt.Sprintf("First child unlinked, fenced %t", fenced), "todo", named)
				second := rig.file(t, fmt.Sprintf("Second child unlinked, fenced %t", fenced), "todo", named)
				seq := rig.seqs(t, named, first, second)
				key := fmt.Sprintf("repeat-closed-by-another-%t", fenced)
				body := map[string]any{"status": "done", "unlink_children": true}
				code, message := "ISSUE_CLOSED", "issue is closed"
				if fenced {
					body["expected_sequence"] = seq[named]
					code, message = "ISSUE_CHANGED", changedSince
				}
				write := statusChangeOf(named, body)
				resume := rig.stopAfterStep(t, write, key, 1)
				rig.lapse(t, key)
				setIssueStatus(t, rig.handler, named, "done")
				before := lastEventID(t, rig.database)
				want := wantRefusal(code, message,
					wantChange{first, "todo", "", seq[first], seq[first] + 2},
					wantChange{second, "todo", "", seq[second], seq[second] + 2},
				)
				requireStatusChange(t, write.String()+" repeated", rig.keyedStatusChange(t, write, key), http.StatusConflict, want)
				if after := lastEventID(t, rig.database); after != before {
					t.Errorf("the refused repeat appended events up to %d after %d, want none", after, before)
				}
				again := rig.keyedStatusChange(t, write, key)
				requireStatusChange(t, write.String()+" repeated again", again, http.StatusConflict, want)
				if !replayed(again) {
					t.Error("the repeat of the refusal is not marked replayed")
				}
				if stopped := resume(); stopped.Code != http.StatusConflict {
					t.Errorf("the stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
				}
				requireEachClosedOnce(t, rig.database, named)
			})
		}
	})

	t.Run("a resumed attempt refused at one of its own steps carries the stored entries and its own", func(t *testing.T) {
		named := rig.file(t, "Issue whose resumed close is refused", "in_progress", "")
		first := rig.file(t, "Child closed by the first run", "todo", named)
		second := rig.file(t, "Child closed by the resumed run", "todo", named)
		third := rig.file(t, "Child that gains an open child", "todo", named)
		seq := rig.seqs(t, named, first, second, third)
		const key = "repeat-refused-step"
		write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true})
		resume := rig.stopAfterStep(t, write, key, 0)
		rig.lapse(t, key)
		var late string
		var once sync.Once
		rig.afterStep(t, func(_ context.Context, step int) {
			if step != 0 {
				return
			}
			once.Do(func() {
				created := rig.send(t, createUnder(third, "Filed during the resumed run"))
				if created.Code != http.StatusCreated {
					t.Fatalf("file an issue under %s: status=%d body=%s", third, created.Code, created.Body.String())
				}
				late = decodeBody[model.Issue](t, created).Key
			})
		})
		repeat := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated", repeat, http.StatusConflict, wantConflict([]ruleHolder{
			{late, "Filed during the resumed run", model.IssueStatusTriage, "child", late + " (triage) is a child of " + third + " and is not done"},
		},
			wantChange{first, "done", named, seq[first], seq[first] + 2},
			wantChange{second, "done", named, seq[second], seq[second] + 2},
		))
		if stopped := resume(); stopped.Code != http.StatusConflict {
			t.Errorf("the stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
		}
		rig.requireIssue(t, third, "todo", named)
		rig.requireIssue(t, named, "in_progress", "")
	})

	t.Run("a call whose last step writes nothing still leaves the mark", func(t *testing.T) {
		named := rig.file(t, "Issue started whose last child moved first", "backlog", "")
		child := rig.file(t, "Child started by the call", "backlog", named)
		last := rig.file(t, "Child another writer started", "backlog", named)
		seq := rig.seqs(t, named, child)
		const key = "repeat-last-step-left"
		var once sync.Once
		rig.afterStep(t, func(_ context.Context, step int) {
			if step == 1 {
				once.Do(func() { setIssueStatus(t, rig.handler, last, "todo") })
			}
		})
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true})
		requireStatusChange(t, write.String(), rig.keyedStatusChange(t, write, key), http.StatusOK, wantAnswer(
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{child, "todo", named, seq[child], seq[child] + 2},
		))
		if held := rig.keyRow(t, key); !held.point.Finished {
			t.Fatalf("the call left the point %+v, want it marked", held.point)
		}
		loseAnswer(t, rig.database, key)
		again := rig.keyedStatusChange(t, write, key)
		requireStatusChange(t, write.String()+" repeated", again, http.StatusOK, wantAnswer(
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{named, "todo", "", seq[named], seq[named] + 1},
			wantChange{child, "todo", named, seq[child], seq[child] + 2},
		))
		if held := rig.keyRow(t, key); !replayed(again) || held.attempt != 1 {
			t.Errorf("the repeat is replayed %t under attempt %d, want replayed from the point under the first", replayed(again), held.attempt)
		}
	})

	t.Run("a repeat of a stored answer replays it and writes nothing", func(t *testing.T) {
		named := rig.file(t, "Issue refused midway", "in_progress", "")
		first := rig.file(t, "Child closed before the refusal", "todo", named)
		second := rig.file(t, "Child that gains an open child midway", "todo", named)
		var once sync.Once
		rig.afterStep(t, func(_ context.Context, step int) {
			if step == 0 {
				once.Do(func() {
					if created := rig.send(t, createUnder(second, "Filed midway")); created.Code != http.StatusCreated {
						t.Fatalf("file an issue under %s: status=%d body=%s", second, created.Code, created.Body.String())
					}
				})
			}
		})
		dry := statusChangeOf(rig.file(t, "Issue whose close is a dry run", "in_progress", ""), map[string]any{"status": "done", "dry_run": true})
		// A call that writes always writes its named issue's step, a status the issue already has
		// included, so a keyed dry run is the one success that leaves no recovery point.
		for _, stored := range []struct {
			name   string
			write  ruleWrite
			status int
		}{
			{"a call refused midway", statusChangeOf(named, map[string]any{"status": "done", "include_children": true}), http.StatusConflict},
			{"a keyed dry run", dry, http.StatusOK},
		} {
			t.Run(stored.name, func(t *testing.T) {
				key := "repeat-stored-" + strings.ReplaceAll(stored.name, " ", "-")
				answered := rig.keyedStatusChange(t, stored.write, key)
				if answered.Code != stored.status {
					t.Fatalf("%s: status=%d body=%s, want %d", stored.write, answered.Code, answered.Body.String(), stored.status)
				}
				if held := rig.keyRow(t, key); held.status == nil || *held.status != stored.status || (stored.status == http.StatusOK && held.point != nil) {
					t.Fatalf("the key holds status %v and the point %+v, want the answer stored with no point", held.status, held.point)
				}
				before := rig.quiet(t)
				again := rig.keyedStatusChange(t, stored.write, key)
				assertUnchanged(t, stored.write.String()+" repeated", before, databaseFingerprint(t, rig.database))
				if again.Code != answered.Code || again.Body.String() != answered.Body.String() || !replayed(again) {
					t.Errorf("the repeat answered %d %s replayed %t, want the stored %d %s replayed",
						again.Code, again.Body.String(), replayed(again), answered.Code, answered.Body.String())
				}
			})
		}
		requireComments(t, rig.database, []string{named, first, second}, movedComment(first, "todo", "done", named))
	})

	t.Run("a close whose settlement marked the key committed before any point is taken over", func(t *testing.T) {
		pattern := "/api/v1/issues/{key}/status-change"
		holdSettled := func(t *testing.T, write ruleWrite, key, named string) {
			t.Helper()
			holdRequestKey(t, rig.database, heldKey{
				login: "alice", key: key, pattern: pattern, leasedAgo: 2 * time.Minute,
				request: jsonRequest(t, write.method, write.path, write.body, map[string]string{"key": named}),
			})
			if _, err := rig.database.Pool.Exec(context.Background(), `
				update request_keys set committed_at = now() where idempotency_key = $1
			`, key); err != nil {
				t.Fatalf("mark the key %s committed: %v", key, err)
			}
		}

		named := rig.file(t, "Issue whose settlement committed", "in_progress", "")
		child := rig.file(t, "Child of the settled issue", "todo", named)
		seq := rig.seqs(t, named, child)
		write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true})
		holdSettled(t, write, "repeat-settled", named)
		requireStatusChange(t, write.String()+" repeated", rig.keyedStatusChange(t, write, "repeat-settled"), http.StatusOK, wantAnswer(
			wantChange{named, "done", "", seq[named] + 1, seq[named] + 2},
			wantChange{child, "done", named, seq[child], seq[child] + 2},
			wantChange{named, "done", "", seq[named] + 1, seq[named] + 2},
		))
		if held := rig.keyRow(t, "repeat-settled"); held.attempt != 2 {
			t.Errorf("the key holds attempt %d, want the takeover's 2", held.attempt)
		}
		requireEachClosedOnce(t, rig.database, named, child)

		stale := rig.file(t, "Issue whose settled close is stale", "in_progress", "")
		staleSeq := issueLastSeq(t, rig.database, stale)
		if retitled := rig.send(t, patchOf(stale, map[string]any{"title": "Retitled before the repeat"})); retitled.Code != http.StatusOK {
			t.Fatalf("retitle %s: status=%d body=%s", stale, retitled.Code, retitled.Body.String())
		}
		staleWrite := statusChangeOf(stale, map[string]any{"status": "done", "expected_sequence": staleSeq})
		holdSettled(t, staleWrite, "repeat-settled-stale", stale)
		before := rig.quiet(t)
		requireStatusChange(t, staleWrite.String()+" repeated", rig.keyedStatusChange(t, staleWrite, "repeat-settled-stale"), http.StatusConflict,
			wantRefusal("ISSUE_CHANGED", changedSince))
		after := databaseFingerprint(t, rig.database)
		delete(before, "request_keys")
		delete(after, "request_keys")
		assertUnchanged(t, staleWrite.String()+" repeated", before, after)
	})

	t.Run("a repeat while the first holder holds its lease is KEY_IN_FLIGHT", func(t *testing.T) {
		named := rig.file(t, "Issue whose call still runs", "backlog", "")
		rig.file(t, "Child of the running call", "backlog", named)
		const key = "repeat-in-flight"
		write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true})
		resume := rig.stopAfterStep(t, write, key, 0)
		repeat := rig.keyedStatusChange(t, write, key)
		if repeat.Code != http.StatusConflict || refusalCode(repeat.Body.Bytes()) != contracts.IdempotencyKeyInFlight {
			t.Errorf("a repeat inside the lease answered %d %s, want 409 %s", repeat.Code, repeat.Body.String(), contracts.IdempotencyKeyInFlight)
		}
		if first := resume(); first.Code != http.StatusOK {
			t.Errorf("the first run answered %d %s once it went on, want 200", first.Code, first.Body.String())
		}
	})

	t.Run("a repeat judged once its body arrived is judged the same way", func(t *testing.T) {
		server := httptest.NewServer(rig.handler)
		t.Cleanup(server.Close)
		found := make(chan struct{}, 1)
		rig.served.repeatFoundInFlight = func() { found <- struct{}{} }
		t.Cleanup(func() { rig.served.repeatFoundInFlight = nil })
		awaitFound := func(t *testing.T) {
			t.Helper()
			select {
			case <-found:
			case <-time.After(30 * time.Second):
				t.Fatal("the repeat never found its key in flight")
			}
		}
		sendRepeat := func(t *testing.T, write ruleWrite, key string) (*heldBackBody, []byte) {
			t.Helper()
			body, err := json.Marshal(write.body)
			if err != nil {
				t.Fatal(err)
			}
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set("X-Dispatch-User", "alice")
			header.Set(contracts.IdempotencyKeyHeader, key)
			return sendPartOfBody(t, server.Listener.Addr().String(), write.path, header, len(body), body[:5]), body
		}

		t.Run("a call refused midway while the body arrived answers the stored refusal", func(t *testing.T) {
			named := rig.file(t, "Issue refused while a repeat arrives", "in_progress", "")
			rig.file(t, "Child closed while a repeat arrives", "todo", named)
			second := rig.file(t, "Child that gains an open child while a repeat arrives", "todo", named)
			const key = "repeat-drained-refusal"
			write := statusChangeOf(named, map[string]any{"status": "done", "include_children": true})
			reached := make(chan struct{})
			release, releaseOnce := heldUntilReleased(t)
			var once sync.Once
			rig.afterStep(t, func(_ context.Context, step int) {
				once.Do(func() {
					if created := rig.send(t, createUnder(second, "Filed while a repeat arrives")); created.Code != http.StatusCreated {
						t.Errorf("file an issue under %s: status=%d body=%s", second, created.Code, created.Body.String())
					}
					close(reached)
					<-release
				})
			})
			encoded, err := json.Marshal(write.body)
			if err != nil {
				t.Fatal(err)
			}
			firstAnswer := make(chan keyedAnswered, 1)
			// The first request carries the shared wait's bound itself, as body_drain_test.go's
			// first requests do (testwait.Context).
			ctx := testwait.Context(t)
			go func() {
				status, body, _, err := keyedPostContext(ctx, server, write.path, string(encoded), key)
				firstAnswer <- keyedAnswered{status: status, body: body, err: err}
			}()
			select {
			case <-reached:
			case <-time.After(30 * time.Second):
				t.Fatal("the first run never reached its first step")
			}
			repeat, body := sendRepeat(t, write, key)
			awaitFound(t)
			releaseOnce()
			var first keyedAnswered
			select {
			case first = <-firstAnswer:
			case <-time.After(30 * time.Second):
				t.Fatal("the first run never answered once it went on")
			}
			if first.err != nil || first.status != http.StatusConflict {
				t.Fatalf("the first run answered %d %s (%v), want its 409 refusal", first.status, first.body, first.err)
			}
			response, got := repeat.finish(body[5:])
			if response.StatusCode != first.status || string(got) != first.body || response.Header.Get(contracts.IdempotencyReplayedHeader) != "?1" {
				t.Errorf("the drained repeat answered %d %s replayed %q, want the stored %d %s replayed",
					response.StatusCode, got, response.Header.Get(contracts.IdempotencyReplayedHeader), first.status, first.body)
			}
		})

		t.Run("a key whose holder lapsed with no mark is freed, and the retry finishes the call", func(t *testing.T) {
			named := rig.file(t, "Issue whose holder lapsed while a repeat arrived", "backlog", "")
			child := rig.file(t, "Child started by the retry", "backlog", named)
			seq := rig.seqs(t, named, child)
			const key = "repeat-drained-freed"
			write := statusChangeOf(named, map[string]any{"status": "todo", "include_children": true})
			resume := rig.stopAfterStep(t, write, key, 0)
			repeat, body := sendRepeat(t, write, key)
			awaitFound(t)
			rig.lapse(t, key)
			response, got := repeat.finish(body[5:])
			if response.StatusCode != http.StatusConflict || refusalCode(got) != contracts.IdempotencyKeyInFlight ||
				!strings.Contains(string(got), "ended while this one's body arrived") {
				t.Errorf("the drained repeat answered %d %s, want 409 %s telling it to ask again", response.StatusCode, got, contracts.IdempotencyKeyInFlight)
			}
			requireStatusChange(t, write.String()+" retried", rig.keyedStatusChange(t, write, key), http.StatusOK, wantAnswer(
				wantChange{named, "todo", "", seq[named], seq[named] + 1},
				wantChange{named, "todo", "", seq[named], seq[named] + 1},
				wantChange{child, "todo", named, seq[child], seq[child] + 2},
			))
			if stopped := resume(); stopped.Code != http.StatusConflict {
				t.Errorf("the stopped run went on and answered %d %s, want 409", stopped.Code, stopped.Body.String())
			}
		})
	})
}

// statusChangeFixtures is where TestStatusChangeAnswersMatchTheirFixtures commits the route's
// answers, from this package's directory.
var statusChangeFixtures = filepath.Join("testdata", "status-change")

// TestStatusChangeAnswersMatchTheirFixtures holds testdata/status-change, the answers of POST
// /api/v1/issues/{key}/status-change that the CLI's and the web app's tests answer their stubs
// with, to what the real server answers: a call that moves a parent and unlinks a child
// (changed.json), a dry run (dry-run.json), a refusal by the rule before any step (conflict.json)
// and after one (conflict-midway.json), the refusal of a token's close of the issues under an
// issue (browser-only.json), and a stale expected_sequence (stale.json). Each file is the answer's
// status and body, every value that differs between runs written as a placeholder
// (normalizeAskReplyFixture).
// `go -C packages/envoy test -count=1 -run '^TestStatusChangeAnswersMatchTheirFixtures$' ./internal/dispatch/api -update`
// rewrites them.
func TestStatusChangeAnswersMatchTheirFixtures(t *testing.T) {
	const rewrite = "go -C packages/envoy test -count=1 -run '^TestStatusChangeAnswersMatchTheirFixtures$' ./internal/dispatch/api -update"
	rig := newStatusChangeRig(t, true)
	type answer struct {
		name     string
		response *httptest.ResponseRecorder
		status   int
	}
	var answers []answer

	epic := rig.file(t, "Epic on the backlog", "backlog", "")
	started := rig.file(t, "Issue started with its epic", "backlog", epic)
	rig.file(t, "Child unlinked from the issue", "backlog", started)
	answers = append(answers, answer{"changed.json", rig.send(t, statusChangeOf(started, map[string]any{
		"status": "in_progress", "include_parent": true, "unlink_children": true,
	})), http.StatusOK})

	closing := rig.file(t, "Parent closed with its tree", "in_progress", "")
	middle := rig.file(t, "Child in work", "todo", closing)
	rig.file(t, "Grandchild in work", "todo", middle)
	answers = append(answers, answer{"dry-run.json", rig.send(t, statusChangeOf(closing, map[string]any{
		"status": "done", "include_children": true, "dry_run": true,
	})), http.StatusOK})
	answers = append(answers, answer{"conflict.json", rig.send(t, statusChangeOf(closing, map[string]any{"status": "done"})), http.StatusConflict})
	answers = append(answers, answer{"browser-only.json", sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+closing+"/status-change", map[string]any{
		"status": "done", "include_children": true, "actor": sessionActor(),
	}), http.StatusForbidden})

	refused := rig.file(t, "Parent refused midway", "in_progress", "")
	rig.file(t, "First child closed", "todo", refused)
	second := rig.file(t, "Second child refused", "todo", refused)
	var once sync.Once
	rig.afterStep(t, func(_ context.Context, step int) {
		once.Do(func() {
			if created := rig.send(t, createUnder(second, "Filed between two steps")); created.Code != http.StatusCreated {
				t.Fatalf("file an issue under %s: status=%d body=%s", second, created.Code, created.Body.String())
			}
		})
	})
	answers = append(answers, answer{"conflict-midway.json", rig.send(t, statusChangeOf(refused, map[string]any{
		"status": "done", "include_children": true,
	})), http.StatusConflict})

	stale := rig.file(t, "Issue changed since its read", "in_progress", "")
	staleSeq := issueLastSeq(t, rig.database, stale)
	if retitled := rig.send(t, patchOf(stale, map[string]any{"title": "Issue retitled since its read"})); retitled.Code != http.StatusOK {
		t.Fatalf("retitle %s: status=%d body=%s", stale, retitled.Code, retitled.Body.String())
	}
	answers = append(answers, answer{"stale.json", rig.send(t, statusChangeOf(stale, map[string]any{
		"status": "done", "expected_sequence": staleSeq,
	})), http.StatusConflict})

	for _, answered := range answers {
		if answered.response.Code != answered.status {
			t.Fatalf("%s: status=%d body=%s, want %d", answered.name, answered.response.Code, answered.response.Body.String(), answered.status)
		}
		encoded, err := json.MarshalIndent(struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		}{answered.response.Code, answered.response.Body.Bytes()}, "", "  ")
		if err != nil {
			t.Fatalf("encode %s: %v", answered.name, err)
		}
		got := normalizeAskReplyFixture(append(encoded, '\n'))
		path := filepath.Join(statusChangeFixtures, answered.name)
		if *updateFixtures {
			if err := os.MkdirAll(statusChangeFixtures, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", path)
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (%s writes it): %v", path, rewrite, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the answer this run got; when the change is meant, rewrite it with %s:\n%s", path, rewrite, got)
		}
	}
}
