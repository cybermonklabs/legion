package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// liveRepo is the repository the project's Dispatch mapping serves (workflowConfig's
// servedBootCall), which every issue a live proof and the idle rig seed is on.
const liveRepo = "acme/widgets"

// liveDispatch is workflowConfig's Dispatch stand-in — the repository mapping and an empty issue
// list — grown into the board the daemon reads and writes: it holds each issue's status, parent and
// labels, as Dispatch's own board does, answers the issue reads the daemon makes (the outbox's
// before a status write, a release's walk up an issue's parent chain, an event's walk up its
// Dispatch chain to the top) and 404s an issue it does not hold, records every status and message
// the daemon posted, so a step can read back what the daemon told Dispatch and tell a write that
// never came from one it cannot see, serves the handoff records a step seeds (handoff), which a
// phase completion names and the daemon reads back, and takes the record of each step the daemon
// writes (putStep), the rating a retro gave the plan (putRating) and the plan of each run (putPlan)
// from the daemon's own session alone, as Dispatch's routes take them, and refuses a write whose id
// names another issue's record or plan 409 STEP_OTHER_ISSUE or PLAN_OTHER_ISSUE, as Dispatch's store
// does, keeping nothing of it. Anything else is a call a live proof does not expect, and fails the
// test as workflowConfig's stand-in does.
type liveDispatch struct {
	t       *testing.T
	project string
	// url is the board's own server, which a caller's DispatchURL names.
	url string
	// lists is whether GET /api/v1/issues answers the issues the board holds or an empty list.
	// The idle rig sets it, because its daemon's boot lingers closed every recorded issue the
	// listing does not hold; a live proof leaves it false, seeding its own admission instead, so
	// the boot's reconcile admits nothing of its own behind the driver's back.
	lists bool
	mu    sync.Mutex
	// issues is each issue as this board holds it, the daemon's writes applied.
	issues map[string]liveDispatchIssue
	posted []liveDispatchWrite
	// handoffs is each seeded handoff record by its id, as GET /api/v1/handoffs/{id} answers it.
	handoffs map[string]map[string]any
	// steps is each step record the daemon wrote, by its id, its fields as the writes left them;
	// stepOrder is the ids in the order of their first write.
	steps     map[string]map[string]any
	stepOrder []string
	// plans is each plan the daemon wrote, by its id, its fields as the first write set them.
	plans map[string]map[string]any
}

// liveDispatchIssue is one issue as the board holds it: its status, its parent ("" for none), and
// whether it is handed to Legion, which its labels say.
type liveDispatchIssue struct {
	status, parent string
	handed         bool
}

// liveDispatchWrite is one write the daemon made to Dispatch: a status it set on an issue, or a
// message it posted on one.
type liveDispatchWrite struct {
	Issue  string
	Status string
	Body   string
}

// newLiveDispatch is the board on a server of its own, closed with the test: a caller points its
// configuration at url.
func newLiveDispatch(t *testing.T, project string) *liveDispatch {
	t.Helper()
	b := &liveDispatch{t: t, project: project, issues: map[string]liveDispatchIssue{}, handoffs: map[string]map[string]any{},
		steps: map[string]map[string]any{}, plans: map[string]map[string]any{}}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	b.url = server.URL
	return b
}

// show puts key on the board at status, a root handed to Legion, as the seeded fixture stands on
// Dispatch before the daemon writes anything.
func (b *liveDispatch) show(key, status string) {
	b.place(key, status, "", true)
}

// place puts key on the board at status under parent ("" for none), labelled for Legion when
// handed.
func (b *liveDispatch) place(key, status, parent string, handed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.issues[key] = liveDispatchIssue{status: status, parent: parent, handed: handed}
}

// liveDispatchLabels is an issue's labels: Legion's when it is handed to Legion, none otherwise.
func liveDispatchLabels(handed bool) []string {
	if handed {
		return []string{dispatch.LegionLabel}
	}
	return []string{}
}

// handoff seeds version 1 of key's handoff record of phase word, written by session, as a worker's
// `dispatch handoff-write` leaves it, and answers its id for the completion to name.
func (b *liveDispatch) handoff(key, word, session string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(b.handoffs)+1)
	b.handoffs[id] = map[string]any{
		"id": id, "issue_key": key, "phase": word, "version": 1, "created_at": "2026-10-27T00:00:00Z",
		"author": map[string]any{"kind": "session", "id": session}, "body": "{}",
	}
	return id
}

func (b *liveDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if servedBootCall(b.t, w, req, b.project) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	key := issueOfDispatchPath(path)
	switch {
	case req.Method == http.MethodGet && path == "/api/v1/issues":
		b.listIssues(w)
	case req.Method == http.MethodGet && path == "/healthz":
		_, _ = io.WriteString(w, `{"status":"ok","commit":null}`)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		// The outbox reads an issue's events to see whether its message is already posted; this
		// board keeps none, so every message the daemon decides to post is posted.
		_, _ = io.WriteString(w, `[]`)
	// The issue's own path, exactly: a path under an issue that this board does not model — a
	// message read, say — falls through to the default arm and is reported, rather than being
	// answered with an issue body the caller would read as an empty field and carry on with. A
	// broad arm answering a narrow route is worse than no arm at all, which at least fails here.
	case req.Method == http.MethodGet && key != "" && path == "/api/v1/issues/"+key:
		b.showIssue(key, w)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
		b.record(liveDispatchWrite{Issue: key, Body: b.decode(req, w)}, w)
	case req.Method == http.MethodPatch && key != "" && path == "/api/v1/issues/"+key:
		write := liveDispatchWrite{Issue: key, Status: b.decode(req, w)}
		b.mu.Lock()
		issue := b.issues[key]
		issue.status = write.Status
		b.issues[key] = issue
		b.mu.Unlock()
		b.record(write, w)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/handoffs/"):
		b.showHandoff(strings.TrimPrefix(path, "/api/v1/handoffs/"), w)
	// The reconcile of a tree's news of its approved design reads what changed between two
	// approved versions of a gated document (dispatch.Client's ArtifactChanges). This board keeps
	// no document, so it answers as Dispatch answers for a document it does not hold, which the
	// reconcile records unreadable at once (dispatch.ErrDocumentNotFound): the news then names the
	// two versions and says why it lists no change.
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/artifacts/") && strings.HasSuffix(path, "/changes"):
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"`+dispatch.ArtifactNotFound+`","error":"artifact not found"}`)
	case req.Method == http.MethodPut && key != "" && strings.HasPrefix(path, "/api/v1/issues/"+key+"/steps/") && strings.HasSuffix(path, "/rating"):
		b.putRating(key, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/issues/"+key+"/steps/"), "/rating"), req, w)
	case req.Method == http.MethodPut && key != "" && strings.HasPrefix(path, "/api/v1/issues/"+key+"/steps/"):
		b.putStep(key, strings.TrimPrefix(path, "/api/v1/issues/"+key+"/steps/"), req, w)
	case req.Method == http.MethodPut && key != "" && strings.HasPrefix(path, "/api/v1/issues/"+key+"/plans/"):
		b.putPlan(key, strings.TrimPrefix(path, "/api/v1/issues/"+key+"/plans/"), req, w)
	default:
		b.t.Errorf("Dispatch request = %s %s", req.Method, path)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
}

// listIssues answers Dispatch's project listing: every issue the board holds when it lists, and
// an empty list otherwise.
func (b *liveDispatch) listIssues(w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	listed := make([]map[string]any, 0, len(b.issues))
	if b.lists {
		for key := range b.issues {
			listed = append(listed, b.issueBody(key))
		}
	}
	if err := json.NewEncoder(w).Encode(listed); err != nil {
		b.t.Errorf("write the Dispatch issue list: %v", err)
	}
}

// showIssue answers Dispatch's issue read of key from the board, and 404 for an issue it does not
// hold.
func (b *liveDispatch) showIssue(key string, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.issues[key]; !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"NOT_FOUND","error":"no such issue"}`)
		return
	}
	if err := json.NewEncoder(w).Encode(b.issueBody(key)); err != nil {
		b.t.Errorf("write the Dispatch issue %s: %v", key, err)
	}
}

// issueBody is one issue as Dispatch answers it. The caller holds the lock.
func (b *liveDispatch) issueBody(key string) map[string]any {
	issue := b.issues[key]
	var parent any
	if issue.parent != "" {
		parent = issue.parent
	}
	return map[string]any{
		"key": key, "project": b.project, "title": key, "status": issue.status, "parent": parent, "rank": "U",
		"labels": liveDispatchLabels(issue.handed), "last_seq": 1, "resolved_repo": liveRepo,
	}
}

// showHandoff answers the handoff record read a completion makes: the seeded record, or Dispatch's
// 404 HANDOFF_NOT_FOUND.
func (b *liveDispatch) showHandoff(id string, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	handoff, ok := b.handoffs[id]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"HANDOFF_NOT_FOUND","error":"handoff `+id+` not found"}`)
		return
	}
	if err := json.NewEncoder(w).Encode(handoff); err != nil {
		b.t.Errorf("write the Dispatch handoff %s: %v", id, err)
	}
}

// putStep takes one write of the step record id of key, as Dispatch's PUT
// /api/v1/issues/{key}/steps/{id} does: only the session legion-daemon:<project> writes one (403
// STEP_WRITER for any other writer), each field is written once and kept, a field a write leaves
// out keeps what an earlier write gave it, and the answer is 200 with the record as it stands after
// the write and its attempt: the place of the record among the issue's records of the same task,
// kind and step, in the order of their first write. An id whose record is another issue's is
// refused 409 STEP_OTHER_ISSUE (writeStep). The daemon sends a rating to the rating route alone
// (dispatch.HTTPClient.PutStep), so no write here carries one.
func (b *liveDispatch) putStep(key, id string, req *http.Request, w http.ResponseWriter) {
	write, ok := b.daemonWrite(key, "a step record", req, w)
	if !ok {
		return
	}
	b.writeStep(key, id, write, false, w)
}

// putRating takes the rating a retro gave the plan onto the step record id of key, as Dispatch's
// PUT /api/v1/issues/{key}/steps/{id}/rating does: the step write's body and writer, which makes
// the record where the retro's start and end have not landed, on a record of key's (409
// STEP_OTHER_ISSUE otherwise) of kind retro alone (400 STEP_INPUT for any other kind), each field
// written once. It does not refuse a rating or its reason for their values, as Dispatch does (a
// whole number from 1 to 5, with a reason neither blank after trimming nor longer than 2,000 code
// points): the daemon refuses such a rating at the completion that ends the retro
// (PLAN_RATING_INPUT, api's planRatingOf), and a rating without its reason where its row is
// queued (record's NewOutboxRow), before anything reaches Dispatch, so no write of the daemon's
// carries one, and a second copy of Dispatch's rule here would hold nothing the daemon can send.
func (b *liveDispatch) putRating(key, id string, req *http.Request, w http.ResponseWriter) {
	write, ok := b.daemonWrite(key, "a step record", req, w)
	if !ok {
		return
	}
	b.writeStep(key, id, write, true, w)
}

// daemonWrite decodes a write to a record of key, which only the session legion-daemon:<project>
// makes (403 STEP_WRITER for any other writer, naming what), and answers its fields without the
// actor.
func (b *liveDispatch) daemonWrite(key, what string, req *http.Request, w http.ResponseWriter) (map[string]any, bool) {
	var write map[string]any
	if err := json.NewDecoder(req.Body).Decode(&write); err != nil {
		b.t.Errorf("decode the write to %s: %v", req.URL.Path, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	project, _, _ := strings.Cut(key, "-")
	actor, _ := write["actor"].(map[string]any)
	if actor["kind"] != "session" || actor["id"] != dispatch.DaemonSession(key) {
		answerRefusal(w, http.StatusForbidden, steprecord.CodeWriter, fmt.Sprintf("only the session legion-daemon:%s writes %s of %s", project, what, key))
		return nil, false
	}
	delete(write, "actor")
	return write, true
}

// answerRefusal answers status with the body of Dispatch's refusals, its code and its message, as
// Dispatch's routes answer one.
func answerRefusal(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": message})
}

// writeStep writes the fields of write onto the step record id of key, each field once, and
// answers the record with its attempt (putStep). rates is the rating route (putRating), which
// takes a record of kind retro alone, judged by the kind the record holds where it holds one. An
// id whose record is another issue's is refused 409 STEP_OTHER_ISSUE before that kind is read, as
// Dispatch's store refuses it (workflowstore's putNode), and nothing of the write is kept.
func (b *liveDispatch) writeStep(key, id string, write map[string]any, rates bool, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	stored, refused := nodeOf(b.steps, "step", steprecord.CodeOtherIssue, key, id, w)
	if refused {
		return
	}
	if rates {
		kind := write["kind"]
		if stored["kind"] != nil {
			kind = stored["kind"]
		}
		if kind != "retro" {
			answerRefusal(w, http.StatusBadRequest, steprecord.CodeInput, fmt.Sprintf("a rating belongs to a retro's record; this record is of kind %v", kind))
			return
		}
	}
	if stored == nil {
		b.stepOrder = append(b.stepOrder, id)
	}
	stored = setOnce(b.steps, key, id, write)
	answer := map[string]any{}
	for field, value := range stored {
		answer[field] = value
	}
	attempt := 0
	for _, other := range b.stepOrder {
		record := b.steps[other]
		if record["task"] == stored["task"] && record["kind"] == stored["kind"] && record["step"] == stored["step"] {
			attempt++
		}
		if other == id {
			break
		}
	}
	answer["attempt"] = attempt
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		b.t.Errorf("write the step record %s: %v", id, err)
	}
}

// nodeOf is the node id of nodes as Dispatch's store finds it before a write under key
// (workflowstore's putNode): nil where none is made yet, and refused where the id is a node of
// another issue, which is answered 409 under code, the refusal of the kind of node what names
// (steprecord.CodeOtherIssue for a step, steprecord.CodePlanOtherIssue for a plan), and keeps
// nothing of the write. The caller holds b.mu.
func nodeOf(nodes map[string]map[string]any, what, code, key, id string, w http.ResponseWriter) (stored map[string]any, refused bool) {
	stored = nodes[id]
	if stored != nil && stored["issue"] != key {
		answerRefusal(w, http.StatusConflict, code, fmt.Sprintf("%s %s is part of %v, and this write names %s", what, id, stored["issue"], key))
		return nil, true
	}
	return stored, false
}

// setOnce writes write onto the node id of key in nodes as Dispatch's store does: it makes the node
// where nodes holds none, and sets each field the node does not hold yet, leaving every field it
// holds. It answers the node. The caller holds b.mu.
func setOnce(nodes map[string]map[string]any, key, id string, write map[string]any) map[string]any {
	stored := nodes[id]
	if stored == nil {
		stored = map[string]any{"id": id, "issue": key}
		nodes[id] = stored
	}
	for field, value := range write {
		if _, written := stored[field]; !written {
			stored[field] = value
		}
	}
	return stored
}

// putPlan takes the plan id of key, as Dispatch's PUT /api/v1/issues/{key}/plans/{id} does: only
// the session legion-daemon:<project> writes one (403 STEP_WRITER for any other writer), a plan
// names at least one task, each with at least one step (400 PLAN_INPUT otherwise, before the node
// of its id is found, as the store checks a write's fields first), an id whose plan is another
// issue's is refused 409 PLAN_OTHER_ISSUE and nothing of the write is kept, each field is set
// once, and the answer is 200 with the plan as it stands after the write. It does not refuse a
// plan for its generation, its source or its document, as Dispatch does: dispatch.PlanWrite names
// the generation and the source on every write, the default plan names no document (workflow's
// defaultPlan), and no test sends the board a plan Dispatch refuses for those values, so a second
// copy of Dispatch's rules here would refuse nothing that reaches it.
func (b *liveDispatch) putPlan(key, id string, req *http.Request, w http.ResponseWriter) {
	write, ok := b.daemonWrite(key, "a plan", req, w)
	if !ok {
		return
	}
	tasks, _ := write["tasks"].([]any)
	if len(tasks) == 0 {
		answerRefusal(w, http.StatusBadRequest, steprecord.CodePlanInput, "a plan has at least one task")
		return
	}
	for _, task := range tasks {
		named, _ := task.(map[string]any)
		if steps, _ := named["steps"].([]any); len(steps) == 0 {
			answerRefusal(w, http.StatusBadRequest, steprecord.CodePlanInput, "a task has at least one step")
			return
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, refused := nodeOf(b.plans, "plan", steprecord.CodePlanOtherIssue, key, id, w); refused {
		return
	}
	stored := setOnce(b.plans, key, id, write)
	if err := json.NewEncoder(w).Encode(stored); err != nil {
		b.t.Errorf("write the plan %s: %v", id, err)
	}
}

// decode is the write's status or body, whichever the daemon sent.
func (b *liveDispatch) decode(req *http.Request, w http.ResponseWriter) string {
	var write struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(req.Body).Decode(&write); err != nil {
		b.t.Errorf("decode the Dispatch write to %s: %v", req.URL.Path, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return ""
	}
	if write.Status != "" {
		return write.Status
	}
	return write.Body
}

func (b *liveDispatch) record(write liveDispatchWrite, w http.ResponseWriter) {
	b.mu.Lock()
	b.posted = append(b.posted, write)
	b.mu.Unlock()
	_, _ = io.WriteString(w, `{}`)
}

// writes is every status the daemon has set and every message it has posted, oldest first.
func (b *liveDispatch) writes() []liveDispatchWrite {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]liveDispatchWrite(nil), b.posted...)
}

// wrote says whether the daemon set issue to status.
func (b *liveDispatch) wrote(issue, status string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, write := range b.posted {
		if write.Issue == issue && write.Status == status {
			return true
		}
	}
	return false
}

// issueOfDispatchPath is the issue key in /api/v1/issues/<key>[/...].
func issueOfDispatchPath(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/issues/")
	key, _, _ := strings.Cut(rest, "/")
	return key
}

// The board refuses a write whose id names a record or a plan of another issue, as Dispatch does:
// 409 STEP_OTHER_ISSUE from both step routes and 409 PLAN_OTHER_ISSUE from the plan route, keeping
// nothing of the write, so the record and the plan stay their first issue's as they were. Each
// refused write but the rating of the step record is one Dispatch takes under its own issue, so
// the 409 is the other-issue rule's. That rating is refused under its own issue too, for the
// record's kind (400 STEP_INPUT), which is the control, and under another issue for its issue, not
// its kind, since Dispatch's store judges the issue before the kind the record holds
// (workflowstore's putNode). A plan Dispatch refuses for what it states is refused 400 PLAN_INPUT
// under any issue's key, since the store checks a plan write's fields before it finds the node of
// its id (workflowstore's PutPlan): a plan with no task, and a task with no step, under the other
// issue's key.
func TestTheBoardRefusesAnIDOfAnotherIssue(t *testing.T) {
	b := newLiveDispatch(t, "SR1")
	first, other := "SR1-1", "SR1-2"
	put := func(key, path string, write map[string]any) (int, string) {
		t.Helper()
		write["actor"] = map[string]any{"kind": "session", "id": dispatch.DaemonSession(key)}
		body, err := json.Marshal(write)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPut, b.url+"/api/v1/issues/"+key+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %s of %s: %v", path, key, err)
		}
		defer resp.Body.Close()
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read the answer to PUT %s of %s: %v", path, key, err)
		}
		return resp.StatusCode, string(answer)
	}
	const step, retro, plan = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"
	for path, write := range map[string]map[string]any{
		"/steps/" + step:  {"task": "default", "kind": "step", "step": 1, "role": "planner", "generation": 1},
		"/steps/" + retro: {"task": "default", "kind": "retro", "step": 0, "role": "implementer", "generation": 1},
		"/plans/" + plan:  {"generation": 1, "source": "default", "tasks": []any{map[string]any{"id": "default", "steps": []any{"planner"}}}},
	} {
		if status, answer := put(first, path, write); status != http.StatusOK {
			t.Fatalf("PUT %s of %s = %d %s, want 200", path, first, status, answer)
		}
	}
	held := func() string {
		b.mu.Lock()
		defer b.mu.Unlock()
		text, err := json.Marshal(map[string]any{"steps": b.steps, "plans": b.plans})
		if err != nil {
			t.Fatal(err)
		}
		return string(text)
	}
	before := held()
	// rating is the one rating write the control and the two refused ratings send: a fresh map each
	// time, since put names the writer in the map it is given.
	rating := func() map[string]any { return map[string]any{"rating": 4, "rating_reason": "The plan fit the work."} }
	if status, answer := put(first, "/steps/"+step+"/rating", rating()); status != http.StatusBadRequest || !strings.Contains(answer, `"code":"`+steprecord.CodeInput+`"`) {
		t.Errorf("control: a rating of %s's step record under %s = %d %s, want 400 %s, the rating refused for the record's kind", first, first, status, strings.TrimSpace(answer), steprecord.CodeInput)
	}
	for _, refused := range []struct {
		what, path string
		write      map[string]any
		code       string
	}{
		{"a write of " + first + "'s step record", "/steps/" + step, map[string]any{"model": "claude-opus"}, steprecord.CodeOtherIssue},
		{"a rating of " + first + "'s retro record", "/steps/" + retro + "/rating", rating(), steprecord.CodeOtherIssue},
		{"a rating of " + first + "'s step record", "/steps/" + step + "/rating", rating(), steprecord.CodeOtherIssue},
		{"a write of " + first + "'s plan", "/plans/" + plan, map[string]any{"generation": 2, "source": "default", "tasks": []any{map[string]any{"id": "default", "steps": []any{"tester"}}}}, steprecord.CodePlanOtherIssue},
	} {
		if status, answer := put(other, refused.path, refused.write); status != http.StatusConflict || !strings.Contains(answer, `"code":"`+refused.code+`"`) {
			t.Errorf("%s under %s = %d %s, want 409 %s", refused.what, other, status, strings.TrimSpace(answer), refused.code)
		}
	}
	for what, tasks := range map[string][]any{
		"a plan with no task": {},
		"a task with no step": {map[string]any{"id": "default", "steps": []any{}}},
	} {
		write := map[string]any{"generation": 2, "source": "default", "tasks": tasks}
		if status, answer := put(other, "/plans/"+plan, write); status != http.StatusBadRequest || !strings.Contains(answer, `"code":"`+steprecord.CodePlanInput+`"`) {
			t.Errorf("%s under %s's id = %d %s, want 400 %s, the input refused before the node of the id is found", what, first, status, strings.TrimSpace(answer), steprecord.CodePlanInput)
		}
	}
	if after := held(); after != before {
		t.Errorf("the refused writes changed what the board holds from\n%s\nto\n%s", before, after)
	}
}
