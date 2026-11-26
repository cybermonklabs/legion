package api

// node_writer_test.go holds the rule that on an issue Legion runs, only a person, the issue's own
// architect or an architect above it writes a requirement, criterion or check node, or a value or
// a relation out of one, each such write judged by the check of a version of the issue's spec or
// Verification document (refuseSpecWriterOfNode, spec_writer.go); and that a reviewer reads the
// agreed spec and Verification queries at the version a person approved, whatever was written
// after it.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// nodeWriterSpec is the rig's spec, which a person writes: a criterion and two requirements under
// it.
const nodeWriterSpec = ":::criterion{#k1}\nI want every write held to its writer.\n:::\n\n" +
	":::requirement{#r1 serves=\"dispatch://CORE-1/spec#b-k1\"}\nA write names its writer.\n:::\n\n" +
	":::requirement{#r2 serves=\"dispatch://CORE-1/spec#b-k1\"}\nA refused write stores nothing.\n:::\n"

// nodeWriterCheckAttributes are the attributes of one of the rig's checks, serving the requirement
// requirement with negativeControl as its negative control, as a retype of the check sets them.
func nodeWriterCheckAttributes(requirement, negativeControl string) map[string]any {
	return map[string]any{
		"serves": []string{"dispatch://CORE-1/spec#b-" + requirement}, "surface": "api", "run": "go test ./...",
		"negative_control": negativeControl,
	}
}

// nodeWriterCheckBlock is one check of the rig's Verification document: id, serving requirement,
// with negativeControl as its negative control.
func nodeWriterCheckBlock(id, requirement, negativeControl, words string) string {
	return ":::check{#" + id + " serves=\"dispatch://CORE-1/spec#b-" + requirement + "\" surface=\"api\" run=\"go test ./...\" " +
		"negative_control=\"" + negativeControl + "\"}\n" + words + "\n:::\n"
}

// nodeWriterControls are the negative controls the rig's two checks are written with.
const (
	nodeWriterStrongControl = "Remove the check from the route: the test reads the write stored."
	nodeWriterWeakControl   = "Read the code."
)

// nodeWriterRig is one issue of an issue tree Legion runs, as TestSpecWriteRefusesAWorkerOnEveryRoute
// builds one (specWriterServer.rig): CORE-1, a root a person filed with the label legion, born
// converted, its spec holding a criterion and two requirements under it (nodeWriterSpec), its
// Verification document four checks, c1 and c3 serving r1 and c2 and c4 serving r2, none holding a
// result, and a constraint a person wrote; then the daemon of CORE published its role holders on
// it, an architect session and a worker session that holds the implementer role, which marks the
// project.
type nodeWriterRig struct {
	handler  http.Handler
	database *store.Store
	served   *server
	issue    string
	// spec, verification and tree are the issue's spec, its Verification document and its tree
	// artifact, as the store addresses their blocks.
	spec, verification, tree decisionDocument
	// constraint is the dispatch:// address of the constraint a person wrote.
	constraint string
	// architect and worker are the two sessions holding roles on the issue, and person the person
	// alice on a token of hers that holds the person scope.
	architect, worker, person nodeWriterAs
}

// nodeWriterAs is one writer of the test: a session on the deployment's shared token, which its
// body names as its actor, or the person alice on a token of hers holding the person scope.
type nodeWriterAs struct {
	name    string
	session string
	token   string
}

func newNodeWriterRig(t *testing.T) *nodeWriterRig {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createAdviceProject(t, handler, "CORE")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Writes held to their writers", "spec": nodeWriterSpec, "labels": []string{"legion"}, "force": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("rig: a person files the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)
	if issue.Key != "CORE-1" {
		t.Fatalf("rig: the issue is %s, want CORE-1, which its spec's references name", issue.Key)
	}
	if tree, err := issueTreeOf(context.Background(), database.Pool, issue.Key); err != nil || tree == "" {
		t.Fatalf("rig: %s holds the tree %q (%v), want it born converted", issue.Key, tree, err)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		nodeWriterCheckBlock("c1", "r1", nodeWriterStrongControl, "The route names its writer.")+"\n"+
			nodeWriterCheckBlock("c2", "r2", nodeWriterStrongControl, "A refused route stores nothing.")+"\n"+
			nodeWriterCheckBlock("c3", "r1", nodeWriterStrongControl, "A route answers a refusal alone.")+"\n"+
			nodeWriterCheckBlock("c4", "r2", nodeWriterStrongControl, "A refusal names the writer's role."))
	constraint := created201[constraintRead](t, "rig: a person writes a constraint", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice"))
	rig := &nodeWriterRig{
		handler: handler, database: database, served: served, issue: issue.Key,
		spec:         readDecisionDocument(t, database, issue.Key, "spec"),
		verification: readDecisionDocument(t, database, issue.Key, model.ArtifactSlugVerification),
		tree:         readDecisionDocument(t, database, issue.Key, issueTreeName),
		constraint:   constraint.Address,
		architect:    nodeWriterAs{name: "the architect", session: "ses-architect-core"},
		worker:       nodeWriterAs{name: "the worker", session: "ses-implementer-core"},
		person: nodeWriterAs{name: "the person on a token holding the person scope",
			token: mintPersonalToken(t, handler, "alice", "a person-scoped token", []string{model.ScopePerson}).Token},
	}
	if published := putRoleHoldersAs(t, handler, issue.Key, map[string]any{
		"holders": []issueRoleHolder{{Role: "architect", Session: rig.architect.session}, {Role: "implementer", Session: rig.worker.session}},
		"actor":   daemonSessionOf("CORE"),
	}); published.Code != http.StatusOK {
		t.Fatalf("rig: the daemon of CORE publishes its role holders on %s: status=%d body=%s", issue.Key, published.Code, published.Body.String())
	}
	rig.quiet(t)
	return rig
}

// created201 fails unless response is 201 Created, and answers its body.
func created201[T any](t *testing.T, what string, response *httptest.ResponseRecorder) T {
	t.Helper()
	return decodeBody[T](t, created(t, what, response))
}

// quiet waits until no document owes a settlement and no write the server left running past its
// answer is still running.
func (rig *nodeWriterRig) quiet(t *testing.T) {
	t.Helper()
	awaitNoSettlementOwed(t, rig.database)
	awaitBackgroundWrites(t, rig.served)
}

// send sends body as writer as: on the shared token, naming its session as the actor, or on the
// person's token, naming the session acting for her, as a personal token names it.
func (rig *nodeWriterRig) send(t *testing.T, as nodeWriterAs, method, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	named := maps.Clone(body)
	if named == nil {
		named = map[string]any{}
	}
	if as.token != "" {
		named["actor"] = map[string]any{"kind": "session", "id": "ses-acting-for-alice"}
		return agentRequest(t, rig.handler, method, path, named, as.token)
	}
	named["actor"] = map[string]any{"kind": "session", "id": as.session}
	return sessionRequest(t, rig.handler, method, path, named)
}

// caller is as's calls of the node routes that take an If-Match, as a nodeCaller sends them: on the
// shared token, naming its session as the actor, or on the person's token, naming the session
// acting for her.
func (rig *nodeWriterRig) caller(t *testing.T, as nodeWriterAs) nodeCaller {
	t.Helper()
	return func(method, path string, body any, header http.Header) *httptest.ResponseRecorder {
		named := map[string]any{}
		if members, ok := body.(map[string]any); ok {
			maps.Copy(named, members)
		}
		header = header.Clone()
		if header == nil {
			header = http.Header{}
		}
		if as.token != "" {
			named["actor"] = map[string]any{"kind": "session", "id": "ses-acting-for-alice"}
			header.Set("Authorization", "Bearer "+as.token)
		} else {
			named["actor"] = map[string]any{"kind": "session", "id": as.session}
			header.Set("Authorization", "Bearer agent-token")
		}
		return serveNodeRequest(t, rig.handler, method, path, named, header)
	}
}

// nodeWriterCounts is what a refused write leaves as it found it: the store's graph_tuples rows,
// of which the issue's are a part, the issue's artifact_versions rows and the issue's events.
type nodeWriterCounts struct {
	tuples, versions, events int
}

func (rig *nodeWriterRig) counts(t *testing.T) nodeWriterCounts {
	t.Helper()
	var counts nodeWriterCounts
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_tuples),
		       (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = $1),
		       (select count(*) from events where issue_key = $1)`, rig.issue).Scan(&counts.tuples, &counts.versions, &counts.events); err != nil {
		t.Fatalf("count the store's tuples and %s's versions and events: %v", rig.issue, err)
	}
	return counts
}

// nodeWrite is one write of a requirement, criterion or check node, or of a value or a relation
// out of one: the route it is sent on, the node it writes, as a failure names it, and its send.
type nodeWrite struct {
	route, node string
	send        func(t *testing.T) *httptest.ResponseRecorder
}

// nodeWriterTargets are the nodes of the rig one writer writes, each its own: the requirement it
// overtakes through the state route and constrains through the pointer route, the id of the
// criterion it adds, the check it moves and the requirement it moves that check under, and the
// check it deletes.
type nodeWriterTargets struct {
	requirement, criterion string
	moved, under           string
	deleted                string
}

// writes are as's five writes of a requirement, criterion or check node, each on a node of its own:
// a check moved under the other requirement through PATCH /api/v1/issues/{key}/nodes/{id}, with the
// If-Match of the check's placement_seq; a check a person wrote that holds no result deleted
// through DELETE of the same route, with that If-Match and the expected_count a person's dry run
// answers; overtaken on a requirement through the state route; a constrained_by out of it to the
// rig's constraint through the pointer route; and a new criterion through an edit of the issue's
// tree artifact. The moves and the deletes come first, so a requirement the writer overtakes
// afterwards is no parent a move was refused under.
func (rig *nodeWriterRig) writes(as nodeWriterAs, targets nodeWriterTargets) []nodeWrite {
	address := rig.spec.address(targets.requirement)
	moved, under, deleted := rig.verification.address(targets.moved), rig.spec.address(targets.under), rig.verification.address(targets.deleted)
	return []nodeWrite{{
		route: "PATCH /api/v1/issues/{key}/nodes/{id}", node: "check " + moved + " under requirement " + under,
		send: func(t *testing.T) *httptest.ResponseRecorder {
			_, tag := placementOf(t, rig.database, refs.BlockNodeKind, moved)
			return patchNodeAs(rig.caller(t, as), rig.issue, moved, tag, map[string]any{"parent": under})
		},
	}, {
		route: "DELETE /api/v1/issues/{key}/nodes/{id}", node: "check " + deleted,
		send: func(t *testing.T) *httptest.ResponseRecorder {
			count := deletedCount(t, "the person's dry run of the delete of "+deleted,
				deleteNodeAs(personCaller(t, rig.handler, "alice"), rig.issue, deleted, true, -1, ""))
			_, tag := placementOf(t, rig.database, refs.BlockNodeKind, deleted)
			return deleteNodeAs(rig.caller(t, as), rig.issue, deleted, false, count, tag)
		},
	}, {
		route: "POST /api/v1/issues/{key}/nodes/{id}/state", node: "requirement " + address,
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, as, http.MethodPost, "/api/v1/issues/"+rig.issue+nodePath(address, "state"),
				map[string]any{"state": graphstore.StateOvertaken})
		},
	}, {
		route: "POST /api/v1/issues/{key}/nodes/{id}/pointers", node: "the constrained_by out of requirement " + address,
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, as, http.MethodPost, "/api/v1/issues/"+rig.issue+nodePath(address, "pointers"),
				map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": rig.constraint})
		},
	}, {
		route: "POST /api/v1/artifacts/{id}/edits", node: "criterion " + rig.tree.address(targets.criterion),
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, as, http.MethodPost, "/api/v1/artifacts/"+rig.tree.id+"/edits", map[string]any{"ops": []map[string]any{{
				"op": "insert", "after": "end", "markdown": ":::criterion{#" + targets.criterion + "}\nI want " + as.name + "'s criterion read.\n:::\n",
			}}})
		},
	}}
}

// requireApproved waits until version 1 of the document id holds the version_renders row of the
// pass that stated it, then approves it as a person, and answers the digest the approval pins.
func (rig *nodeWriterRig) requireApproved(t *testing.T, id string) string {
	t.Helper()
	testwait.Eventually(t, "version 1 of "+id+" to hold its render", func() bool {
		var rendered bool
		err := rig.database.Pool.QueryRow(context.Background(),
			`select exists (select 1 from version_renders where artifact_id = $1 and number = 1)`, id).Scan(&rendered)
		return err == nil && rendered
	})
	review := reviewDocument(t, rig.handler, id, "alice", "approved", "")
	if review.Version != 1 || review.ContentSHA256 == nil {
		t.Fatalf("a person's approval of %s approves version %d pinning %v, want version 1 and a digest", id, review.Version, review.ContentSHA256)
	}
	return *review.ContentSHA256
}

// recordAtVersion1 is the record of query of the rig's issue at the store revision version 1 of
// the document id read (version_renders: its time and its seq), the cut the tree read answers that
// version at (issueTreeCut).
func (rig *nodeWriterRig) recordAtVersion1(t *testing.T, query, id string) docs.QueryRecord {
	t.Helper()
	ctx := context.Background()
	var cut refs.TreeCut
	if err := rig.database.Pool.QueryRow(ctx, `select at, read_seq from version_renders where artifact_id = $1 and number = 1`, id).
		Scan(&cut.At, &cut.ReadSeq); err != nil {
		t.Fatalf("read the cut version 1 of the %s query read: %v", query, err)
	}
	record, err := docs.ReadQueryRecord(ctx, rig.database.Pool, rig.issue, query, cut)
	if err != nil {
		t.Fatalf("read the %s record of %s at version 1: %v", query, rig.issue, err)
	}
	return record
}

// treeNodeLine is one node of a tree read's query, or of a record, as the comparison of the two
// reads it: its store address, its type, its parent's store address and its rank.
func treeNodeLine(address, typ, parent string, rank *string) string {
	ranked := "none"
	if rank != nil {
		ranked = fmt.Sprintf("%q", *rank)
	}
	return fmt.Sprintf("%s %s under %s rank %s", address, typ, parent, ranked)
}

// requireAgreed fails unless the tree read of the rig's issue at ?view=query&v=1 answers, as the
// query's nodes, the nodes of record in the record's order, each with its type, its parent and its
// rank, else naming the first node where they differ; and unless that record digests to pin, the
// digest the person's approval of version 1 pins, and the record of the head does not. The record
// must hold each node of holds, by store address, so the comparison is never one of two empty
// lists.
func (rig *nodeWriterRig) requireAgreed(t *testing.T, query string, record docs.QueryRecord, pin string, holds ...string) {
	t.Helper()
	for _, address := range holds {
		if !slices.ContainsFunc(record.Nodes, func(node docs.TreeNode) bool { return node.Address() == address }) {
			t.Fatalf("the %s record of %s at version 1 holds %v, want it to hold %s", query, rig.issue, walkAddresses(record.Nodes), address)
		}
	}
	answer := treeRead(t, rig.handler, rig.issue, "?view="+query+"&v=1", "alice", false)
	var answered []string
	for _, node := range answer.Nodes {
		if node.Role != treeRoleQuery {
			continue
		}
		parent := "none"
		if node.Parent != nil {
			parent = node.Parent.Kind + ":" + node.Parent.ID
		}
		answered = append(answered, treeNodeLine(node.Kind+":"+node.ID, node.Supertag, parent, node.Rank))
	}
	recorded := make([]string, len(record.Nodes))
	for index, node := range record.Nodes {
		var rank *string
		if node.Ranked {
			rank = &node.Rank
		}
		parent := "none"
		if node.Parent.Kind != "" {
			parent = node.Parent.Kind + ":" + node.Parent.ID
		}
		recorded[index] = treeNodeLine(node.Address(), node.Type, parent, rank)
	}
	t.Logf("the %s answer at version 1 answers the query's nodes %v", query, answered)
	for index := range max(len(answered), len(recorded)) {
		switch {
		case index >= len(recorded):
			t.Errorf("the %s answer at version 1 differs from its record at node %s: the record holds none there\n\tanswer %v\n\trecord %v",
				query, answered[index], answered, recorded)
		case index >= len(answered):
			t.Errorf("the %s answer at version 1 differs from its record at node %s: the answer holds none there\n\tanswer %v\n\trecord %v",
				query, record.Nodes[index].Address(), answered, recorded)
		case answered[index] != recorded[index]:
			t.Errorf("the %s answer at version 1 differs from its record at node %s: the answer holds %s\n\tanswer %v\n\trecord %v",
				query, record.Nodes[index].Address(), answered[index], answered, recorded)
		default:
			continue
		}
		break
	}
	if record.Digest != pin {
		t.Errorf("the agreed %s state at version 1 digests to %s, the approval pins %s", query, record.Digest, pin)
	}
	head, err := docs.ReadQueryRecord(context.Background(), rig.database.Pool, rig.issue, query, refs.TreeCut{})
	if err != nil {
		t.Fatalf("read the %s record of %s at the head: %v", query, rig.issue, err)
	}
	if head.Digest == pin {
		t.Errorf("the %s record of %s at the head digests to %s, the digest the approval of version 1 pins: the writes after it moved nothing it covers",
			query, rig.issue, head.Digest)
	}
}

// TestOnlyAPersonOrAnArchitectWritesASpecNode: on an issue Legion runs, born converted, whose spec
// holds a criterion and two requirements under it, whose Verification document holds four checks
// serving them, and which holds a constraint (newNodeWriterRig), the worker's five writes of a
// requirement, criterion or check node - a check moved under the other requirement through PATCH
// /api/v1/issues/{key}/nodes/{id} with the If-Match of its placement_seq, a check a person wrote that
// holds no result deleted through DELETE of that route with that If-Match and the expected_count its
// dry run answers, overtaken on a requirement through the state route, a constrained_by out of a
// requirement through the pointer route, and a new criterion through an edit of the issue's tree
// artifact - are each refused 403 DOCUMENT_WRITER_NOT_ARCHITECT, R28's writer check coming before
// R32's on both node routes, the store's graph_tuples and the issue's artifact_versions and events
// counted the same before and after each, else printing `<route> let worker <session> write <node>`.
// The architect's and the person's same five writes, each on a node of its own, are each taken, else
// printing `<route> refused <writer>`. The worker still writes what is no such node: a constraint of
// its own, overtaken on it through the state route, and a paragraph in the tree artifact.
//
// A person approves version 1 of the spec and of the Verification document, each the version of
// its query the issue's create and the upload stated, once the pass that stated it has recorded its
// render, before any of those writes. After them a person moves r2 under her own criterion through
// PATCH, and weakens c1's negative control through the edit route. The tree read at ?view=spec&v=1
// and ?view=verification&v=1 then answers the nodes of each query's record at the store revision
// version 1 read (docs.ReadQueryRecord), in its order, each record digesting to its approval's
// content_sha256 and the head's to another digest (requireAgreed).
func TestOnlyAPersonOrAnArchitectWritesASpecNode(t *testing.T) {
	rig := newNodeWriterRig(t)
	specPin := rig.requireApproved(t, rig.spec.id)
	verificationPin := rig.requireApproved(t, rig.verification.id)
	rig.quiet(t)

	for _, write := range rig.writes(rig.worker, nodeWriterTargets{requirement: "r1", criterion: "kw", moved: "c2", under: "r1", deleted: "c3"}) {
		before := rig.counts(t)
		response := write.send(t)
		rig.quiet(t)
		after := rig.counts(t)
		var refusal refusalAnswer
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != http.StatusForbidden || refusal.Code != specWriterRefusalCode || after != before {
			t.Errorf("%s let worker %s write %s: status=%d body=%s; tuples %d -> %d, versions %d -> %d, events %d -> %d",
				write.route, rig.worker.session, write.node, response.Code, clippedBody(response),
				before.tuples, after.tuples, before.versions, after.versions, before.events, after.events)
			continue
		}
		t.Logf("%s refuses worker %s its write of %s: %s", write.route, rig.worker.session, write.node, refusal.Error)
	}
	for _, writer := range []struct {
		as      nodeWriterAs
		targets nodeWriterTargets
	}{
		{rig.architect, nodeWriterTargets{requirement: "r2", criterion: "ka", moved: "c1", under: "r2", deleted: "c3"}},
		{rig.person, nodeWriterTargets{requirement: "r1", criterion: "kp", moved: "c2", under: "r1", deleted: "c4"}},
	} {
		for _, write := range rig.writes(writer.as, writer.targets) {
			if response := write.send(t); response.Code != http.StatusOK {
				t.Errorf("%s refused %s: status=%d body=%s", write.route, writer.as.name, response.Code, clippedBody(response))
			}
			rig.quiet(t)
		}
	}
	for _, taken := range []struct{ check, parent string }{{"c1", "r2"}, {"c2", "r1"}} {
		check, parent := rig.verification.address(taken.check), rig.spec.address(taken.parent)
		if held, _ := placementOf(t, rig.database, refs.BlockNodeKind, check); held.ToID != parent {
			t.Errorf("the move of %s under %s left it under %s %s", check, parent, held.ToKind, held.ToID)
		}
	}
	for _, deleted := range []string{"c3", "c4"} {
		check := rig.verification.address(deleted)
		if state := nodeStateOf(t, rig.database, refs.BlockNodeKind, check); state != graphstore.StateGone {
			t.Errorf("the delete of %s left its node %s, want gone", check, state)
		}
	}
	rig.quiet(t)

	// What is no requirement, criterion or check node the worker still writes.
	own := created201[constraintRead](t, "the worker's constraint", rig.send(t, rig.worker, http.MethodPost,
		"/api/v1/issues/"+rig.issue+"/constraints", map[string]any{"measurement": "p99 write latency", "value": "41 ms", "taken_where": "staging"}))
	for _, write := range []nodeWrite{{
		route: "POST /api/v1/issues/{key}/nodes/{id}/state", node: "its own constraint " + own.Address,
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.worker, http.MethodPost, "/api/v1/issues/"+rig.issue+nodePath(own.Address, "state"),
				map[string]any{"state": graphstore.StateOvertaken})
		},
	}, {
		route: "POST /api/v1/artifacts/{id}/edits", node: "a paragraph of the tree artifact",
		send: func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, rig.worker, http.MethodPost, "/api/v1/artifacts/"+rig.tree.id+"/edits", map[string]any{"ops": []map[string]any{{
				"op": "insert", "after": "end", "markdown": "The worker's note on the tree.\n",
			}}})
		},
	}} {
		if response := write.send(t); response.Code != http.StatusOK {
			t.Errorf("%s refused %s its write of %s: status=%d body=%s", write.route, rig.worker.name, write.node, response.Code, clippedBody(response))
		}
	}
	rig.quiet(t)

	// The person moves r2 under her criterion through PATCH, and weakens c1.
	r2, k1, kp := rig.spec.address("r2"), rig.spec.address("k1"), rig.tree.address("kp")
	testwait.Eventually(t, "the person's criterion "+kp+" to be a node of the store", func() bool {
		_, held, err := graphstore.ReadNode(context.Background(), rig.database.Pool, refs.BlockNodeKind, kp)
		return err == nil && held
	})
	_, tag := placementOf(t, rig.database, refs.BlockNodeKind, r2)
	succeeded(t, "the person moves "+r2+" under her criterion "+kp, patchNode(t, rig.handler, rig.issue, r2, tag, map[string]any{"parent": kp}, "alice"))
	succeeded(t, "the person weakens c1's negative control", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/artifacts/"+rig.verification.id+"/edits", map[string]any{"ops": []map[string]any{{
			"op": "retype", "block": "c1", "type": graphstore.BlockTypeCheck, "attributes": nodeWriterCheckAttributes("r1", nodeWriterWeakControl),
		}}}, "alice"))
	rig.quiet(t)

	rig.requireAgreed(t, graphstore.QuerySpec, rig.recordAtVersion1(t, graphstore.QuerySpec, rig.spec.id), specPin,
		blockAddress(k1), blockAddress(rig.spec.address("r1")), blockAddress(r2))
	rig.requireAgreed(t, graphstore.QueryVerification, rig.recordAtVersion1(t, graphstore.QueryVerification, rig.verification.id), verificationPin,
		blockAddress(rig.verification.address("c1")), blockAddress(rig.verification.address("c2")))
}
