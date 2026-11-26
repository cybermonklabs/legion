package docs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// This file is the package's external test: the step's test moves a node through the route a
// person moves one by (PATCH /api/v1/issues/{key}/nodes/{id}), and api imports docs, so no test of
// the package's own can reach that route.

// daemonThreadServerURL is the public origin the rig's server serves under.
const daemonThreadServerURL = "https://dispatch.daemon-thread.invalid"

// daemonThreadSpec is the spec each issue of the rig is created with: a criterion and a
// requirement, each a node of the spec query, and each standing under its issue until a parent
// pointer places it.
const daemonThreadSpec = ":::criterion{#crit}\nI want the urgent asks first.\n:::\n\n:::requirement{#req}\nAn urgent ask comes first.\n:::\n"

// daemonThreadRig is the Dispatch API as cmd/dispatch builds it (api.NewDeps, api.Register) over a
// database of its own and the document service beside it (docs.New), whose store the step writes
// and whose drain writes the versions. The document service settles nothing on a timer of its own
// (Settle is an hour), so no settlement takes an owner lock beside the step.
type daemonThreadRig struct {
	database  *store.Store
	documents *docs.Service
	handler   http.Handler
}

func newDaemonThreadRig(t *testing.T) *daemonThreadRig {
	t.Helper()
	ctx := context.Background()
	rig := &daemonThreadRig{database: storetest.Open(t)}
	// No write of the test reaches a session, so the stand-in listener knows none.
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session"}`))
	}))
	testcleanup.Add(t, listener.Close)
	people := store.NewPgPeopleStore(rig.database.Pool, auth.SingleSigningKey("signing-key"), nil)
	if err := people.Record(ctx, "alice"); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	caller := identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}
	broker := events.NewBroker()
	rig.documents = docs.New(docs.Deps{Store: rig.database, Events: broker, Identity: caller, ServerURL: daemonThreadServerURL, Settle: time.Hour})
	testcleanup.Add(t, func() {
		if err := rig.documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	tokens, err := auth.ParseSharedAgentTokens("agent-token")
	if err != nil {
		t.Fatalf("parse the agent token: %v", err)
	}
	deps, err := api.NewDeps(api.DepsInput{
		Store:       rig.database,
		Identity:    caller,
		AgentTokens: tokens,
		ServerURL:   daemonThreadServerURL,
		EnvoyURL:    listener.URL,
		Docs:        rig.documents,
		Events:      broker,
	})
	if err != nil {
		t.Fatalf("build the API's dependencies: %v", err)
	}
	mux := http.NewServeMux()
	// t.Context, the server's lifetime, ends before the cleanups run, so Wait returns once the
	// handlers it started have ended, before the document service shuts down and the database
	// closes under them.
	testcleanup.Add(t, api.Register(mux, t.Context(), deps).Wait)
	rig.handler = mux
	rig.request(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "CORE", "name": "Core"}, "alice", http.StatusCreated)
	return rig
}

// send sends one request to the server, with headers beside the rig's own: as the person login
// names, by the header the rig's identity reads a person's browser from, or, with no login, under
// the shared agent bearer, whose body names the session it acts as. It answers the status and the
// body.
func (r *daemonThreadRig) send(t *testing.T, headers map[string]string, method, target string, body any, login string) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode %s %s: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

// request is send with no header of its own, failing the test unless the answer is want.
func (r *daemonThreadRig) request(t *testing.T, method, target string, body any, login string, want int) []byte {
	t.Helper()
	status, answer := r.send(t, nil, method, target, body, login)
	if status != want {
		t.Fatalf("%s %s: status=%d body=%s, want %d", method, target, status, answer, want)
	}
	return answer
}

// daemonThreadDecoded decodes body into a T, failing the test where it cannot.
func daemonThreadDecoded[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return value
}

// daemonThreadIssue is one issue of the rig, born converted: its key, its spec, and the thread of
// messages the Legion daemon's session began on it - the person's reply in that thread, and the
// node of that reply's first block.
type daemonThreadIssue struct {
	key, spec         string
	reply, replyBlock string
}

// issue creates, as alice, an issue of CORE holding daemonThreadSpec, born converted since no
// live process lists no issue room; the session legion-daemon:CORE posts a message on it, and
// alice replies to that message.
func (r *daemonThreadRig) issue(t *testing.T, title string) daemonThreadIssue {
	t.Helper()
	created := daemonThreadDecoded[struct {
		Key     string `json:"key"`
		Primary string `json:"primary_artifact_id"`
	}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": title, "spec": daemonThreadSpec, "force": true,
	}, "alice", http.StatusCreated))
	issue := daemonThreadIssue{key: created.Key, spec: created.Primary}
	var converted bool
	if err := r.database.Pool.QueryRow(context.Background(),
		`select exists (select 1 from issue_trees where issue_key = $1)`, issue.key).Scan(&converted); err != nil || !converted {
		t.Fatalf("the issue %s holds a tree %t (%v), want it born converted", issue.key, converted, err)
	}
	messages := "/api/v1/issues/" + issue.key + "/messages"
	root := daemonThreadDecoded[struct {
		ID string `json:"id"`
	}](t, r.request(t, http.MethodPost, messages, map[string]any{
		"body": "READY: the issue is claimed.", "actor": map[string]any{"kind": "session", "id": "legion-daemon:CORE"},
	}, "", http.StatusCreated)).ID
	issue.reply = daemonThreadDecoded[struct {
		ID string `json:"id"`
	}](t, r.request(t, http.MethodPost, messages, map[string]any{
		"body": "Hold it until Friday.", "in_reply_to": root,
	}, "alice", http.StatusCreated)).ID
	if err := r.database.Pool.QueryRow(context.Background(), `
		select node_id from message_blocks where message_id = $1::uuid and position is not null order by position limit 1
	`, issue.reply).Scan(&issue.replyBlock); err != nil {
		t.Fatalf("read the first block of the reply %s: %v", issue.reply, err)
	}
	return issue
}

// block is the node address of the block id of the spec of the issue key, read off its
// document_blocks row.
func (r *daemonThreadRig) block(t *testing.T, key, id string) string {
	t.Helper()
	var address string
	if err := r.database.Pool.QueryRow(context.Background(), `
		select d.node_address from document_blocks d join artifacts a on a.id = d.artifact_id
		where a.issue_key = $1 and d.block_id = $2 and d.node_address is not null`, key, id).Scan(&address); err != nil {
		t.Fatalf("read the node of block %s of %s: %v", id, key, err)
	}
	return address
}

// placement is the current part_of of the block node, failing the test where it holds no one
// part_of.
func (r *daemonThreadRig) placement(t *testing.T, node string) graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), r.database.Pool, refs.BlockNodeKind, node, []string{graphstore.TuplePartOf})
	if err != nil || len(held) != 1 {
		t.Fatalf("the block %s stands on %v (%v), want one parent", node, held, err)
	}
	return held[0]
}

// placeUnder moves the block node under the node toKind and toID name, through the store's
// writer, as a parent pointer an agent's session wrote before R2's rule of the daemon placed it:
// the retraction of its part_of and one to that node, dated by the database's clock. It answers
// the seq of the part_of it wrote.
func (r *daemonThreadRig) placeUnder(t *testing.T, node, toKind, toID string) int64 {
	t.Helper()
	ctx := context.Background()
	retracted := r.placement(t, node)
	if err := r.database.Pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&retracted.At); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	retracted.Asserted, retracted.Who, retracted.SourceEvent = false, model.Actor{Kind: "session", ID: "ses_architect"}, graphstore.NoEvent
	placed := retracted
	placed.ToKind, placed.ToID, placed.Asserted = toKind, toID, true
	if err := pgx.BeginFunc(ctx, r.database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, nil, []graphstore.Tuple{retracted, placed})
	}); err != nil {
		t.Fatalf("place %s under %s %s: %v", node, toKind, toID, err)
	}
	now := r.placement(t, node)
	if now.ToKind != toKind || now.ToID != toID {
		t.Fatalf("the block %s stands under %s %s once placed under %s %s", node, now.ToKind, now.ToID, toKind, toID)
	}
	return now.Seq
}

// census reads the census kind daemon_thread_parent as `envoy-dispatch census` reads it, in a
// read-only snapshot, and answers it with the line that holds its count.
func (r *daemonThreadRig) census(t *testing.T) (graphcensus.DaemonThreadCensus, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("open the census's snapshot: %v", err)
	}
	defer tx.Rollback(ctx)
	census, err := graphcensus.ReadDaemonThreadCensus(ctx, tx)
	if err != nil {
		t.Fatalf("read the census kind %s: %v", graphcensus.DaemonThreadParentKind, err)
	}
	var written bytes.Buffer
	census.Write(&written)
	line, _, _ := strings.Cut(written.String(), " (")
	return census, strings.TrimPrefix(line, "census: ")
}

// newestVersion is the number of the newest version of the document.
func (r *daemonThreadRig) newestVersion(t *testing.T, artifactID string) int {
	t.Helper()
	var number int
	if err := r.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(number), 0) from artifact_versions where artifact_id = $1::uuid`, artifactID).Scan(&number); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return number
}

// requestedVersion is the version of the document the open approval request names, failing the
// test where none is open.
func (r *daemonThreadRig) requestedVersion(t *testing.T, artifactID string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read of the open approval request: %v", err)
	}
	defer tx.Rollback(ctx)
	ask, err := docs.OpenApprovalAsk(ctx, tx, artifactID)
	if err != nil || ask == nil || ask.Approval == nil {
		t.Fatalf("the open approval request of %s = %+v, %v; want one open", artifactID, ask, err)
	}
	named := ask.Approval.Document(artifactID)
	if named == nil {
		t.Fatalf("the open approval request %s names no version of %s: %+v", ask.ID, artifactID, ask.Approval)
	}
	return named.Version
}

// requireMovedByTheStep fails unless the block node stands under the node of the issue key by a
// part_of the step wrote, and the step retracted, once, the part_of to the node fromKind and
// fromID name whose seq is placed, the placement the fixture wrote.
func (r *daemonThreadRig) requireMovedByTheStep(t *testing.T, node, key, fromKind, fromID string, placed int64) {
	t.Helper()
	now := r.placement(t, node)
	if now.ToKind != graphstore.KindIssue || now.ToID != key {
		t.Errorf("the block %s stands under %s %s after the step ran, want it under the issue %s", node, now.ToKind, now.ToID, key)
		return
	}
	if !now.Who.SameAs(docs.DaemonThreadStepActor) {
		t.Errorf("the part_of of %s to the issue %s is written by %s %s, want the step %s %s",
			node, key, now.Who.Kind, now.Who.ID, docs.DaemonThreadStepActor.Kind, docs.DaemonThreadStepActor.ID)
	}
	var retractions int
	if err := r.database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5 and not asserted
		  and who->>'kind' = $6 and who->>'id' = $7 and seq > $8
	`, refs.BlockNodeKind, node, graphstore.TuplePartOf, fromKind, fromID,
		docs.DaemonThreadStepActor.Kind, docs.DaemonThreadStepActor.ID, placed).Scan(&retractions); err != nil {
		t.Fatalf("read the retractions of %s's part_of to %s %s: %v", node, fromKind, fromID, err)
	}
	if retractions != 1 {
		t.Errorf("the step retracted the part_of %d of %s to %s %s %d times, want once", placed, node, fromKind, fromID, retractions)
	}
}

// TestANodeUnderADaemonThreadMovesUnderItsIssue holds R2's daemon-thread step
// (docs.DaemonThreadStep) to this issue's spec R2: a node that stands under a message of a thread
// the Legion daemon's session began, or under a block of one, because a parent pointer named it
// before R2's rule, is re-placed under its issue, and each query the move changes gets a version
// that carries no approval.
//
// Two issues of CORE are born converted, each with a criterion and a requirement in its spec, a
// message the session legion-daemon:CORE posts and alice's reply to it. On the first, alice
// approves the spec; the criterion's part_of is then written to her reply and the requirement's to
// a block of that reply, as a parent pointer written before the rule placed them; she edits the
// spec, which writes a version while they stand there; and an agent's session requests approval,
// so a request is open at that version beside her approval of the one before.
//
// The census kind reads daemon_thread_parent=2, with both nodes' addresses. The step runs once:
// it moves both, each under the issue's node by a part_of the step wrote, its old part_of retracted
// by the step; the census reads daemon_thread_parent=0; the spec holds one version more, with its
// artifact.version event; the open request names that version; and alice's approval stands at the
// version she approved, so it reads changed since.
//
// Then a person's move against the step's. A third such part_of is written on one issue and a
// fourth on the other, so one batch of the step takes both issues' owner locks in one call of
// store.LockOwners, which takes them in the order of their keys' hashes. The test holds the owner
// lock of the issue that call takes first (lockstep.HoldOwner) and runs the step, which reads both
// nodes and then waits at that call, holding no lock of the other issue and no table's
// (lockstep.RequireStoppedAt). Alice then moves the other issue's node under its issue through
// PATCH /api/v1/issues/{key}/nodes/{id}, stating the placement the step read. Once the hold lets
// go, the step gives that node up - its part_of is the one the PATCH wrote - and moves the node of
// the held issue; its next run moves nothing, and the census reads daemon_thread_parent=0.
//
// The hold stands on a second issue because the PATCH takes the owner lock of its own issue: a
// hold on that lock would hold the PATCH behind the step, which would then move the node first.
func TestANodeUnderADaemonThreadMovesUnderItsIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newDaemonThreadRig(t)
	first := rig.issue(t, "The daemon's thread holds two nodes")
	second := rig.issue(t, "A second issue of the batch")
	criterion, requirement := rig.block(t, first.key, "crit"), rig.block(t, first.key, "req")

	rig.request(t, http.MethodPost, "/api/v1/artifacts/"+first.spec+"/reviews", map[string]any{"state": "approved"}, "alice", http.StatusCreated)
	approved := rig.newestVersion(t, first.spec)
	criterionPlaced := rig.placeUnder(t, criterion, string(refs.ItemMessage), first.reply)
	requirementPlaced := rig.placeUnder(t, requirement, refs.ItemMessage.BlockKind(), first.replyBlock)
	rig.request(t, http.MethodPost, "/api/v1/artifacts/"+first.spec+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "A closing paragraph."}},
	}, "alice", http.StatusOK)
	requested := rig.newestVersion(t, first.spec)
	if requested <= approved {
		t.Fatalf("alice's edit of the spec of %s left its newest version at %d, the version she approved: the fixture needs a version after it", first.key, requested)
	}
	if now := rig.placement(t, criterion); now.ToID != first.reply {
		t.Fatalf("the version alice's edit wrote moved the criterion %s under %s %s: the fixture needs it under her reply %s", criterion, now.ToKind, now.ToID, first.reply)
	}
	rig.request(t, http.MethodPost, "/api/v1/artifacts/"+first.spec+"/approval-requests", map[string]any{
		"summary": "The spec, with its closing paragraph.", "actor": map[string]any{"kind": "session", "id": "ses_architect"},
	}, "", http.StatusCreated)
	if named := rig.requestedVersion(t, first.spec); named != requested {
		t.Fatalf("the approval request opened on the spec of %s names version %d, want %d", first.key, named, requested)
	}

	census, counted := rig.census(t)
	var nodes []string
	for _, parent := range census.Parents {
		nodes = append(nodes, parent.Node)
	}
	owed := []string{criterion, requirement}
	slices.Sort(owed)
	if census.Count() != 2 || counted != "daemon_thread_parent=2" || !slices.Equal(nodes, owed) {
		t.Fatalf("the census reads %s of %v before the step, want daemon_thread_parent=2 of %v", counted, nodes, owed)
	}

	step := docs.NewDaemonThreadStep(rig.documents, nil)
	run, err := step.Run(ctx)
	if err != nil {
		t.Fatalf("the step's run: %v", err)
	}
	t.Logf("the step's first run moved %d nodes and gave %d up", run.Moved, run.GivenUp)
	if census, counted := rig.census(t); census.Count() != 0 {
		now := rig.placement(t, criterion)
		t.Errorf("the census reads %s after the step ran, want daemon_thread_parent=0: the criterion %s stands under %s %s",
			counted, criterion, now.ToKind, now.ToID)
	}
	if run.Moved != 2 || run.GivenUp != 0 {
		t.Errorf("the step's run moved %d nodes and gave %d up, want it to move the criterion and the requirement", run.Moved, run.GivenUp)
	}
	rig.requireMovedByTheStep(t, criterion, first.key, string(refs.ItemMessage), first.reply, criterionPlaced)
	rig.requireMovedByTheStep(t, requirement, first.key, refs.ItemMessage.BlockKind(), first.replyBlock, requirementPlaced)

	newest := rig.newestVersion(t, first.spec)
	if newest != requested+1 {
		now := rig.placement(t, criterion)
		t.Errorf("the spec query of %s stands at version %d, unchanged, beside the criterion's new parent %s %s: want version %d, written for the move",
			first.key, newest, now.ToKind, now.ToID, requested+1)
	}
	var recorded int
	if err := rig.database.Pool.QueryRow(ctx, `
		select count(*) from events
		where type = 'artifact.version' and payload->>'artifact_id' = $1 and (payload->'version'->>'number')::int = $2
	`, first.spec, requested+1).Scan(&recorded); err != nil {
		t.Fatalf("read the artifact.version of version %d of the spec of %s: %v", requested+1, first.key, err)
	}
	if recorded != 1 {
		t.Errorf("%d artifact.version events record version %d of the spec of %s, want one", recorded, requested+1, first.key)
	}
	if named := rig.requestedVersion(t, first.spec); named != requested+1 {
		t.Errorf("the open approval request of the spec of %s names version %d, want version %d, which the step wrote", first.key, named, requested+1)
	}
	review, err := docs.NewestReview(ctx, rig.database.Pool, first.spec)
	switch {
	case err != nil || review == nil:
		t.Fatalf("the newest review of the spec of %s = %+v, %v; want alice's approval", first.key, review, err)
	case review.State == "approved" && review.Version == newest:
		t.Errorf("the approval of the spec of %s reads approved at version %d, the version the step wrote: the step carried it, and no person approved that version",
			first.key, review.Version)
	case review.State != "approved" || review.Version != approved:
		t.Errorf("the newest review of the spec of %s reads %s at version %d, want alice's approval of version %d, which reads changed since",
			first.key, review.State, review.Version, approved)
	}

	// A person's move against the step's: the held issue is the one LockOwners locks first.
	gate, raced := first, second
	var firstLocksFirst bool
	if err := rig.database.Pool.QueryRow(ctx, `select hashtext($1::text) < hashtext($2::text)`,
		store.IssueOwner(first.key).LockKey(), store.IssueOwner(second.key).LockKey()).Scan(&firstLocksFirst); err != nil {
		t.Fatalf("read the order of the owner locks of %s and %s: %v", first.key, second.key, err)
	}
	if !firstLocksFirst {
		gate, raced = second, first
	}
	held, moving := rig.block(t, gate.key, "crit"), rig.block(t, raced.key, "crit")
	heldPlaced := rig.placeUnder(t, held, string(refs.ItemMessage), gate.reply)
	read := rig.placeUnder(t, moving, string(refs.ItemMessage), raced.reply)
	if census, counted := rig.census(t); census.Count() != 2 {
		t.Fatalf("the census reads %s once a third and a fourth such part_of are written, want daemon_thread_parent=2", counted)
	}
	owner := lockstep.HoldOwner(t, rig.database, &lockstep.Hold{
		Name: "issue " + gate.key + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "issue", ID: gate.key,
		Statement: lockstep.OwnerLockStatement, Unlocked: []string{"graph_tuples", "issue_query_marks", "issue_head_nodes"},
	}, store.IssueOwner(gate.key))
	type outcome struct {
		run docs.DaemonThreadRun
		err error
	}
	answered := make(chan outcome, 1)
	go func() {
		run, err := step.Run(ctx)
		answered <- outcome{run: run, err: err}
	}()
	backend := storetest.WaitForStatementBehind(t, rig.database, owner.Tx, store.OwnerLockStatement)
	lockstep.RequireStoppedAt(t, ctx, rig.database, backend, "the daemon-thread step", owner)
	status, answer := rig.send(t, map[string]string{"If-Match": `"` + strconv.FormatInt(read, 10) + `"`}, http.MethodPatch,
		"/api/v1/issues/"+raced.key+"/nodes/"+url.PathEscape(moving), map[string]any{"parent": graphstore.KindIssue + "/" + raced.key}, "alice")
	if status != http.StatusOK {
		owner.Release()
		t.Fatalf("alice's move of %s under the issue %s, while the step waits for the owner lock of %s: status=%d body=%s, want 200",
			moving, raced.key, gate.key, status, answer)
	}
	patched := daemonThreadDecoded[struct {
		PlacementSeq int64 `json:"placement_seq"`
	}](t, answer).PlacementSeq
	owner.Release()
	raceRun := testwait.Receive(t, "the step's run to answer once the owner lock of "+gate.key+" is let go", answered)
	if raceRun.err != nil {
		t.Fatalf("the step's run beside alice's move: %v", raceRun.err)
	}
	if raceRun.run.Moved != 1 || raceRun.run.GivenUp != 1 {
		t.Errorf("the step's run beside alice's move moved %d nodes and gave %d up, want it to move %s and give %s up",
			raceRun.run.Moved, raceRun.run.GivenUp, held, moving)
	}
	if now := rig.placement(t, moving); now.Seq != patched || now.ToKind != graphstore.KindIssue || now.ToID != raced.key || now.Who.ID != "alice" {
		t.Errorf("the block %s stands under %s %s by the part_of %d that %s %s wrote, want the part_of %d alice's move wrote, under the issue %s: the step did not give it up",
			moving, now.ToKind, now.ToID, now.Seq, now.Who.Kind, now.Who.ID, patched, raced.key)
	}
	rig.requireMovedByTheStep(t, held, gate.key, string(refs.ItemMessage), gate.reply, heldPlaced)
	again, err := step.Run(ctx)
	if err != nil {
		t.Fatalf("the step's next run: %v", err)
	}
	if census, counted := rig.census(t); census.Count() != 0 || again.Moved != 0 || again.GivenUp != 0 {
		t.Errorf("the step's next run moved %d nodes and gave %d up, and the census reads %s: want it to find nothing, and daemon_thread_parent=0",
			again.Moved, again.GivenUp, counted)
	}
}
