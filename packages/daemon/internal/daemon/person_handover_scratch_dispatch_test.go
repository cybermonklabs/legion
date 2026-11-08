package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// LEGION-755's proof that the daemon itself queues a person's handover: a person's legion label on
// an unrecorded root in triage, and a person's approval of a parked labelled root's primary
// document, each become one todo write of the daemon's (a dispatch_status outbox row marked
// personHandover), delivered under the daemon's own actor, so the root is recorded and admitted as
// a person's todo would be and no agent's write is needed. The rig is the blockers proof's
// (blockers_scratch_dispatch_test.go): the real Dispatch server with its events on, a real
// Postgres, a real NATS, and the daemon process-in-process reading Dispatch's events off that
// bus. A person's act is a request with the trusted X-Dispatch-User header; a session's is one
// with the scratch server's agent bearer naming its session in the body (dispatchCallAs). The
// daemon's decisions are read from GET /legion/v1/state, from the outbox rows it queued, from its
// JSON log, and from Dispatch's own event log, where its todo write carries its actor.

// scratchIdentity is who a request to the scratch Dispatch is from: the person `smoke`, through the
// trusted identity header the harness starts the server with, or a session, through the server's
// agent bearer with the session named in the body's actor, as Dispatch requires of a bearer caller.
type scratchIdentity int

const (
	asPerson scratchIdentity = iota
	asSession
)

func (identity scratchIdentity) String() string {
	if identity == asSession {
		return "the session " + scratchSession
	}
	return "the person smoke"
}

const (
	// scratchAgentToken is the agent bearer the scratch Dispatch is started with
	// (startScratchDispatchOn's DISPATCH_AGENT_TOKEN); a request a session makes carries it.
	scratchAgentToken = "scratch-dispatch-agent-token"
	// scratchSession is the session a bearer request names in its body's actor.
	scratchSession = "session-smoke"
)

// dispatchAnswerWith sends one request to the scratch Dispatch server, credential putting on it
// the headers that say who it is from, with body as given, and answers its status and body,
// whatever they are: the request a proof expects Dispatch to refuse goes through here.
func dispatchAnswerWith(t *testing.T, baseURL, method, path string, body any, credential func(*http.Request)) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	credential(request)
	response, err := scratchDispatchClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	return response.StatusCode, answer
}

// dispatchAnswerAs is dispatchAnswerWith as identity: the person's trusted header, or the
// session's agent bearer.
func dispatchAnswerAs(t *testing.T, baseURL, method, path string, body any, identity scratchIdentity) (int, []byte) {
	t.Helper()
	credential := signedInAsThePerson
	if identity == asSession {
		credential = bearing(scratchAgentToken)
	}
	return dispatchAnswerWith(t, baseURL, method, path, body, credential)
}

// signedInAsThePerson is the person smoke at a browser: the trusted identity header the harness
// starts the scratch Dispatch with.
func signedInAsThePerson(request *http.Request) { request.Header.Set("X-Dispatch-User", "smoke") }

// bearing presents token as the request's credential: the scratch server's shared agent token,
// which holds no OAuth scope and acts as no person, or a personal token of the person's, which
// acts as the person who minted it.
func bearing(token string) func(*http.Request) {
	return func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+token) }
}

// withSessionActor is body, whatever shape it was given in, as an object naming scratchSession in
// its actor, which Dispatch's create, update, upload and edit routes require of a bearer caller
// (requireActor). A nil body is the actor alone.
func withSessionActor(t *testing.T, body any) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode a session's request: %v", err)
		}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("a session's request body %s is not an object: %v", encoded, err)
		}
	}
	fields["actor"] = map[string]string{"kind": "session", "id": scratchSession}
	return fields
}

// dispatchCallAs is dispatchCall as identity: one request the scratch Dispatch must answer 200 or
// 201, its answer decoded into `into` when that is not nil. A session's write carries its session
// in the body's actor (withSessionActor), as every route a session writes through requires; a
// session's read sends no body at all.
func dispatchCallAs(t *testing.T, baseURL, method, path string, body, into any, identity scratchIdentity) {
	t.Helper()
	if identity == asSession && method != http.MethodGet {
		body = withSessionActor(t, body)
	}
	status, answer := dispatchAnswerAs(t, baseURL, method, path, body, identity)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("%s %s as %s = %d: %s", method, path, identity, status, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode the answer to %s %s from %s: %v", method, path, answer, err)
		}
	}
}

// scratchActor is an actor as Dispatch's API serves it.
type scratchActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// scratchIssue is an issue as the proofs read Dispatch's answer to a create, an update or a read.
type scratchIssue struct {
	Key               string   `json:"key"`
	Status            string   `json:"status"`
	Labels            []string `json:"labels"`
	PrimaryArtifactID string   `json:"primary_artifact_id"`
	LastSeq           int64    `json:"last_seq"`
}

// scratchEvent is one event of an issue's Dispatch event log: its id, which the envelope the
// daemon consumes carries as `dispatch-<id>` and the daemon records as applied under
// (processed_events), its sequence on the issue, its type, its actor and its payload.
type scratchEvent struct {
	ID      int64           `json:"id"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Actor   scratchActor    `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// status is the issue status the event's payload carries, "" for an event of another kind.
func (e scratchEvent) status() string {
	var payload struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(e.Payload, &payload)
	return payload.Status
}

// scratchReview is one review of a document as Dispatch's review routes serve it.
type scratchReview struct {
	Version int          `json:"version"`
	State   string       `json:"state"`
	Actor   scratchActor `json:"actor"`
	Reason  *string      `json:"reason"`
}

// personHandoverRig is one proof's rig: the scratch Dispatch, the daemon's configuration and
// database, the daemon once booted (boot), the fake runtime it launches on, and the JSON log it
// writes. The spec document every issue is created with gives its primary document a settled
// version 1 to approve.
type personHandoverRig struct {
	t       *testing.T
	baseURL string
	cfg     config.Config
	pool    *pgxpool.Pool
	logs    *syncBuffer
	github  string
	d       *daemon
}

// personHandoverSpec is the spec every issue of these proofs is created with.
const personHandoverSpec = "# Plan\n\nThe service ships behind a flag.\n"

// newPersonHandoverRig starts the NATS server, starts the scratch Dispatch with its events on it,
// and readies the daemon's configuration, its database and the project without booting it: the
// boot proof writes to Dispatch before the first boot. admissionCap is the daemon's cap.
func newPersonHandoverRig(t *testing.T, admissionCap int) *personHandoverRig {
	t.Helper()
	// The NATS server is started first: it is this test's own (testnats.URL), and Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it on its first connection, which the daemon then consumes
	// from.
	natsURL := testnats.URL(t)
	baseURL, token := startScratchDispatchWithNATS(t, natsURL)
	cfg := workflowConfig(t, natsURL)
	// Dispatch takes a project key of at most ten characters; workflowConfig's is longer.
	cfg.Project = "P" + randomSuffix(t)[:9]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.PostgresDSN = blockersDatabase(t)
	cfg.AdmissionCap = admissionCap
	// No controller tick runs: the first is an hour after the runtime starts (controllerFirstWake,
	// in boot) and the next an hour after that, so every decision below is an event's or the boot
	// listing's. The design gate stays at its default, root-issues, so a registered gate stays
	// registrable and closed until a person approves; EnvoyURL stays the rig's unreachable
	// default, so every notice row the daemon queues stays in its outbox for the proof to count.
	cfg.ControllerWakeInterval = time.Hour
	cfg.DispatchURL = baseURL
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	dispatchCall(t, baseURL, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": cfg.Project, "name": "A person's handover"}, nil)
	dispatchCall(t, baseURL, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": cfg.Project, "default": true}, nil)
	pool, err := pgxpool.New(t.Context(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	t.Cleanup(pool.Close)
	return &personHandoverRig{t: t, baseURL: baseURL, cfg: cfg, pool: pool, logs: &syncBuffer{}, github: blockersGitHub(t)}
}

// boot starts the daemon on the rig's configuration and database, on a fresh fake runtime, its log
// a JSON buffer at Debug the proofs read admission's lines from; a second boot appends to the same
// buffer, so a line counted across boots is counted once.
func (r *personHandoverRig) boot() {
	t := r.t
	t.Helper()
	rt := fake.NewRuntime()
	// A pods runtime provisions each claim's workspace itself, so the daemon clones nothing: these
	// issues name a repository that exists only in this test's GitHub stand-in.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.environ = workflowEnviron(t)
	o.githubAPI = r.github
	o.controllerFirstWake = time.Hour
	r.d = startDaemonLogging(t, r.cfg, o, slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// create creates an issue of the rig's project through Dispatch's API as identity, with fields
// beside the project, the title and the spec, and answers it as Dispatch made it: in triage, where
// every Dispatch issue starts. Dispatch refuses a new issue whose title near-duplicates one in its
// project, so every title here shares no word with another.
func (r *personHandoverRig) create(identity scratchIdentity, title string, fields map[string]any) scratchIssue {
	r.t.Helper()
	body := map[string]any{"project": r.cfg.Project, "title": title, "spec": personHandoverSpec}
	maps.Copy(body, fields)
	var created scratchIssue
	dispatchCallAs(r.t, r.baseURL, http.MethodPost, "/api/v1/issues", body, &created, identity)
	if created.Key == "" || created.PrimaryArtifactID == "" {
		r.t.Fatalf("Dispatch created %q as %+v; want a key and a primary document", title, created)
	}
	return created
}

// patch edits key through Dispatch's API as identity and answers the issue as Dispatch shows it
// after the edit, its last_seq the sequence of the event the edit made.
func (r *personHandoverRig) patch(key string, identity scratchIdentity, fields map[string]any) scratchIssue {
	r.t.Helper()
	var patched scratchIssue
	dispatchCallAs(r.t, r.baseURL, http.MethodPatch, "/api/v1/issues/"+key, fields, &patched, identity)
	return patched
}

// addLabel puts labels, the whole set the issue is to carry, on key as identity.
func (r *personHandoverRig) addLabel(key string, identity scratchIdentity, labels ...string) scratchIssue {
	r.t.Helper()
	patched := r.patch(key, identity, map[string]any{"labels": labels})
	if got, want := slices.Sorted(slices.Values(patched.Labels)), slices.Sorted(slices.Values(labels)); !slices.Equal(got, want) {
		r.t.Fatalf("Dispatch answered the labels %v on %s with %v", labels, key, patched.Labels)
	}
	return patched
}

// setStatus moves key as the person at the dashboard does, and answers the issue after the move.
func (r *personHandoverRig) setStatus(key, status string) scratchIssue {
	r.t.Helper()
	patched := r.patch(key, asPerson, map[string]any{"status": status})
	if patched.Status != status {
		r.t.Fatalf("Dispatch answered the move of %s to %s with %s", key, status, patched.Status)
	}
	return patched
}

// approve is the person's own approval of artifactID at a browser, which Dispatch must take and
// record in the person's name: a review is a person's act, and the scratch server's shared agent
// token holds no scope to settle one (api.TestRoutesTableAuthCellsMatchHandlers).
func (r *personHandoverRig) approve(artifactID string) scratchReview {
	r.t.Helper()
	var review scratchReview
	dispatchCall(r.t, r.baseURL, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews",
		map[string]any{"state": "approved"}, &review)
	if review.State != "approved" || review.Actor != (scratchActor{Kind: "user", ID: "smoke"}) {
		r.t.Fatalf("the person's approval of %s = %+v; want an approved review in their own name", artifactID, review)
	}
	return review
}

// reviews lists artifactID's reviews, newest first.
func (r *personHandoverRig) reviews(artifactID string) []scratchReview {
	r.t.Helper()
	var listed []scratchReview
	dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, &listed)
	return listed
}

// editDocument applies ops to artifactID as a session through Dispatch's edits route, which
// versions the document in the route's own transaction, and answers the version it wrote.
func (r *personHandoverRig) editDocument(artifactID string, ops ...map[string]string) int {
	r.t.Helper()
	var edited struct {
		Version *struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	dispatchCallAs(r.t, r.baseURL, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{"ops": ops}, &edited, asSession)
	if edited.Version == nil {
		r.t.Fatalf("the edit of %s wrote no version", artifactID)
	}
	return edited.Version.Number
}

// personHandoverBlock is the decision block the carried case writes into a spec, and the option
// the person answers it with.
const (
	personHandoverBlock  = ":::ask{#wave urgency=\"med\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n"
	personHandoverAnswer = "Two"
)

// blockAsk waits for Dispatch's settlement to index the decision block blockID of key's document
// as an open ask and answers the ask's id.
func (r *personHandoverRig) blockAsk(key, blockID string) string {
	r.t.Helper()
	var found string
	testwait.Eventually(r.t, "Dispatch to index the decision block "+blockID+" of "+key, func() bool {
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
			State   string  `json:"state"`
		}
		dispatchCall(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key+"/asks?state=all", nil, &asks)
		for _, ask := range asks {
			if ask.BlockID != nil && *ask.BlockID == blockID && ask.State == "open" {
				found = ask.ID
				return true
			}
		}
		return false
	})
	return found
}

// answerBlock answers the decision block's ask as the person with selected.
func (r *personHandoverRig) answerBlock(askID, selected string) {
	r.t.Helper()
	dispatchCall(r.t, r.baseURL, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		map[string]any{"selected": []string{selected}, "expected_edited_at": nil}, nil)
}

// carriedReview waits for Dispatch to record its carry of the person's approval of version from on
// artifactID — the newest review, approved in the person's name at a later version with the
// carry's reason (intake.CarriedApprovalReasonPrefix) — and answers it.
func (r *personHandoverRig) carriedReview(artifactID string, from int) scratchReview {
	r.t.Helper()
	var newest scratchReview
	testwait.Eventually(r.t, fmt.Sprintf("Dispatch to carry the approval of version %d of %s forward", from, artifactID), func() bool {
		reviews := r.reviews(artifactID)
		if len(reviews) == 0 {
			return false
		}
		newest = reviews[0]
		return newest.Version > from && newest.Reason != nil && strings.HasPrefix(*newest.Reason, intake.CarriedApprovalReasonPrefix)
	})
	wantPrefix := fmt.Sprintf("%s %d:", intake.CarriedApprovalReasonPrefix, from)
	if newest.State != "approved" || newest.Actor != (scratchActor{Kind: "user", ID: "smoke"}) || !strings.HasPrefix(*newest.Reason, wantPrefix) {
		r.t.Fatalf("the carried review of %s = %+v (reason %q); want approved in the person's name with a reason opening %q", artifactID, newest, *newest.Reason, wantPrefix)
	}
	return newest
}

// issueEvents is key's whole Dispatch event log, oldest first, read with the agent token.
func (r *personHandoverRig) issueEvents(key string) []scratchEvent {
	r.t.Helper()
	var events []scratchEvent
	dispatchCallAs(r.t, r.baseURL, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, &events, asSession)
	return events
}

// newestEvent is the newest event of kind on key, which the log must hold; want, when not nil,
// narrows it to the newest such event want accepts.
func (r *personHandoverRig) newestEvent(key, kind string, want func(scratchEvent) bool) scratchEvent {
	r.t.Helper()
	events := r.issueEvents(key)
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == kind && (want == nil || want(events[i])) {
			return events[i]
		}
	}
	r.t.Fatalf("the event log of %s holds no %s event the proof looks for among %+v", key, kind, events)
	return scratchEvent{}
}

// statusRows is every dispatch_status row of key still in the daemon's outbox, oldest first: a
// delivered row is deleted, so these are the writes still to run.
func (r *personHandoverRig) statusRows(key string) []record.StatusWrite {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), `select payload from outbox where kind = 'dispatch_status' and issue = $1 order by id`, key)
	if err != nil {
		r.t.Fatalf("read the status writes queued on %s: %v", key, err)
	}
	defer rows.Close()
	var writes []record.StatusWrite
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			r.t.Fatalf("scan a status write queued on %s: %v", key, err)
		}
		var write record.StatusWrite
		if err := json.Unmarshal(payload, &write); err != nil {
			r.t.Fatalf("decode the status write %s queued on %s: %v", payload, key, err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the status writes queued on %s: %v", key, err)
	}
	return writes
}

// personHandoverRows is the person's handover rows (StatusWrite.PersonHandover) still queued on key.
func (r *personHandoverRig) personHandoverRows(key string) []record.StatusWrite {
	r.t.Helper()
	var marked []record.StatusWrite
	for _, write := range r.statusRows(key) {
		if write.PersonHandover {
			marked = append(marked, write)
		}
	}
	return marked
}

// outboxRows counts the rows of kind on key in the daemon's outbox whose payload's kind is
// payloadKind: the notice rows, which stay queued since nothing answers the Envoy address the
// notice runner publishes to.
func (r *personHandoverRig) outboxRows(key string, kind record.OutboxKind, payloadKind string) int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from outbox where kind = $1 and issue = $2 and payload->>'kind' = $3`,
		string(kind), key, payloadKind).Scan(&count); err != nil {
		r.t.Fatalf("count the %s rows of kind %s queued on %s: %v", kind, payloadKind, key, err)
	}
	return count
}

// triageNotices counts the controller notices of kind triage the daemon has queued on key: the
// wake a session's label on an unrecorded triage root gives the controller.
func (r *personHandoverRig) triageNotices(key string) int {
	r.t.Helper()
	return r.outboxRows(key, record.OutboxKindControllerNotice, string(record.TriageNotice))
}

// personHandoverLogLines counts the lines of the daemon's log that say admission queued a person's
// handover of key from source (`event`, `listing` or `approval`), the one message every path
// logs its queue under once it commits.
func (r *personHandoverRig) personHandoverLogLines(key, source string) int {
	r.t.Helper()
	lines := 0
	for _, line := range loggedLines(r.t, r.logs.String(), "admission: queued a person's handover") {
		if line["msg"] == "admission: queued a person's handover" && line["issue"] == key && line["source"] == source {
			lines++
		}
	}
	return lines
}

// waiting waits for admission.waiting to name key and answers the state it did in.
func (r *personHandoverRig) waiting(key string) (api.State, string) {
	r.t.Helper()
	return settled(r.t, r.d, "name "+key+" in its waiting line", func(s api.State) bool {
		return slices.Contains(s.Admission.Waiting, key)
	})
}

// queuedByThePerson is what every positive asserts of key once the daemon queued the person's
// handover from source and delivered it: Dispatch shows the issue todo, the newest issue.updated
// event of its log is the daemon's own write under its actor (dispatch.DaemonSession) carrying
// that todo, admission.waiting names it (the cap is nought, so a queued root is recorded and
// waits rather than being admitted), the daemon logged one queue of it from source so far —
// lines counts the queues the proof expects on the key by now — and no person's handover row is
// left in the outbox.
func (r *personHandoverRig) queuedByThePerson(key, source string, lines int) {
	t := r.t
	t.Helper()
	awaitDispatchStatus(t, r.baseURL, key, "todo")
	written := r.newestEvent(key, "issue.updated", nil)
	if written.Actor != (scratchActor{Kind: "session", ID: dispatch.DaemonSession(key)}) || written.status() != "todo" {
		t.Fatalf("the newest issue.updated of %s = %+v; want the daemon's own todo under %s", key, written, dispatch.DaemonSession(key))
	}
	_, body := r.waiting(key)
	t.Logf("the daemon queued the person's handover of %s from %s, wrote its todo as %s, and holds it in its waiting line:\n%s",
		key, source, dispatch.DaemonSession(key), body)
	if got := r.personHandoverLogLines(key, source); got != lines {
		t.Fatalf("the daemon logged %d queues of a person's handover of %s from %s, want %d; its log:\n%s", got, key, source, lines, r.logs.String())
	}
	if rows := r.personHandoverRows(key); len(rows) != 0 {
		t.Fatalf("the person's handover rows still queued on %s = %+v after its todo was written; want none", key, rows)
	}
}

// A2. A person's legion label on an unrecorded root in triage is that person's handover, and the
// daemon queues the root's todo itself: the person creates a root, which Dispatch starts in
// triage, and adds the legion label. Within 60 s Dispatch shows it todo, the newest issue.updated
// of its log is the daemon's own write under legion-daemon:<project>, admission.waiting names it,
// no controller notice of kind triage names it, and the daemon logged the one queue, from source
// event. The cap is nought, so the queued root stands in the waiting line and nothing admits it:
// what admission does after it records a root is no part of this proof.
//
// What it holds that no unit test can is a real Dispatch event log of a person's label PATCH,
// whose shape is what intake.LabelSetter reads `user` from, and the whole path closing once: the
// real event, the consumer, the prepare stage's read of that log, admission, the outbox, and the
// real todo on the real issue. The rule that read applies is intake's own
// (TestLabelSetterIsTheActorOfTheEventThatAddedTheLabel, which holds a session's label, an
// unrelated label a person adds beside it and a comment between them), and what admission then
// does with a session's label, with a child's and with a root it already records is admit's
// (internal/admit/label_test.go).
func TestAPersonsLabelOnATriageRootIsQueuedByTheDaemonAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	r := newPersonHandoverRig(t, 0)
	r.boot()

	handed := r.create(asPerson, "Rollout handed over by hand", nil)
	labelled := r.addLabel(handed.Key, asPerson, "legion")
	r.queuedByThePerson(handed.Key, "event", 1)
	if notices := r.triageNotices(handed.Key); notices != 0 {
		t.Fatalf("the daemon queued %d triage notices on %s, a root a person labelled; want none: the daemon queued its todo instead", notices, handed.Key)
	}
	if lines := r.personHandoverLogLines(handed.Key, "listing") + r.personHandoverLogLines(handed.Key, "approval"); lines != 0 {
		t.Fatalf("the daemon logged %d queues of %s from a source other than its label's event; its log:\n%s", lines, handed.Key, r.logs.String())
	}
	t.Logf("the person's label on %s (Dispatch sequence %d) was queued by the daemon as the root's todo", handed.Key, labelled.LastSeq)
}

// A3. A person's approval of the primary document of a parked labelled root with no parent is
// that person's handover, and the daemon queues the root's todo itself: a session's labelled root
// the person parked in backlog, whose spec the person then approves at a browser, is todo within
// 60 s under the daemon's own actor, stands in admission.waiting, and leaves one queue in the log
// from source approval. The cap is nought here too, so nothing admits it.
//
// What it holds that no unit test can is the live carry. The person answers a decision block of
// that spec, and Dispatch writes a carried review of the version the answer made, in the person's
// name, whose reason opens with the prefix the daemon tells a carry by. That prefix is two
// constants in two modules with nothing shared between them — intake.CarriedApprovalReasonPrefix
// in internal/intake/facts.go and carryReason in packages/envoy/internal/dispatch/docs/
// approval_carry.go — and this is the only place the two are compared at run time. What the
// daemon decides about such a carry, and about a session's review, a child's document, an
// unlabelled root, a document that is not the primary one, a root that is not parked, a live
// root's gate and a recorded parked root, is workflow's
// (internal/workflow/person_handover_test.go).
func TestAPersonsApprovalOfAParkedRootsSpecIsQueuedByTheDaemonAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	r := newPersonHandoverRig(t, 0)
	r.boot()

	// The parked labelled root, its spec carrying the decision block the carry below answers.
	parked := r.create(asSession, "Parked proposal awaiting approval", map[string]any{"labels": []string{"legion"}})
	r.setStatus(parked.Key, "backlog")
	blockVersion := r.editDocument(parked.PrimaryArtifactID, map[string]string{"op": "insert", "after": "end", "markdown": personHandoverBlock})
	askID := r.blockAsk(parked.Key, "wave")

	approved := r.approve(parked.PrimaryArtifactID)
	if approved.Version != blockVersion {
		t.Fatalf("the person approved %s's spec at version %d; want %d, the version holding the decision block", parked.Key, approved.Version, blockVersion)
	}
	r.queuedByThePerson(parked.Key, "approval", 1)
	t.Logf("the person's approval of %s's spec at version %d queued the root", parked.Key, approved.Version)

	// The carry, read from Dispatch alone: the daemon is told nothing of it here.
	r.answerBlock(askID, personHandoverAnswer)
	carry := r.carriedReview(parked.PrimaryArtifactID, approved.Version)
	t.Logf("the person's answer to the decision block of %s carried their approval of version %d to version %d, under the reason %q the daemon reads a carry by",
		parked.Key, approved.Version, carry.Version, *carry.Reason)
}

// A4. The boot listing queues a person's label the daemon's Dispatch consumer never sees. The
// premise is intake's consumer policy (D6): createConsumer makes a durable consumer with no stored
// stream position under DeliverNewPolicy (intake/consume.go), so a daemon booted for the first time
// after a person's label never receives that label's event, and only the boot listing, which
// reads every status, can see the root. A consumer created to start at the stream's first message
// instead would deliver the event, and this proof would read the queue from source event.
//
//  1. before any daemon boots, the person creates a root in triage and adds the legion label;
//  2. the daemon boots: its listing finds the labelled triage root nobody recorded and whose label
//     the person set, and queues its todo — one log line from source listing — which its outbox
//     writes under the daemon's actor and which its consumer, live by then, records;
//
// It shows the boot path. It does not show a daemon down for longer than the event stream keeps a
// message, and it does not show a second boot: that the listing queues one row however often it
// runs is admit's (TestReconcileQueuesAPersonsLabelOnAnUnrecordedRootInTriage, whose own second
// boot queues no second row), and that a root the daemon already records is never queued again is
// admit's too (TestNoTriageWakeForAChildOrARecordedRoot), over the one predicate the listing and
// the stream share (intake.DispatchIssue.LabelledTriageRoot).
func TestABootListingQueuesAPersonsLabelAgainstScratchDispatch(t *testing.T) {
	t.Parallel()
	r := newPersonHandoverRig(t, 1)

	// 1. The person's label falls while no daemon runs.
	booted := r.create(asPerson, "Nightly rotation configured offline", nil)
	labelled := r.addLabel(booted.Key, asPerson, "legion")
	t.Logf("the person labelled %s at Dispatch's sequence %d before the daemon's first boot", booted.Key, labelled.LastSeq)

	// 2. The first boot: the listing queues it, the outbox writes it, the consumer records it.
	r.boot()
	if lines := r.personHandoverLogLines(booted.Key, "listing"); lines != 1 {
		t.Fatalf("the first boot logged %d queues of %s from its listing; want one; its log:\n%s", lines, booted.Key, r.logs.String())
	}
	daemonsTodo := func(e scratchEvent) bool {
		return e.Actor == (scratchActor{Kind: "session", ID: dispatch.DaemonSession(booted.Key)}) && e.status() == "todo"
	}
	testwait.Eventually(t, "the daemon to write the todo of "+booted.Key, func() bool {
		return slices.ContainsFunc(r.issueEvents(booted.Key), func(e scratchEvent) bool { return e.Type == "issue.updated" && daemonsTodo(e) })
	})
	written := r.newestEvent(booted.Key, "issue.updated", daemonsTodo)
	_, body := settled(t, r.d, "record "+booted.Key+" from its own todo write", func(s api.State) bool {
		_, recorded := s.Issues[booted.Key]
		return recorded
	})
	t.Logf("the first boot's listing queued the person's handover of %s, written as %s at Dispatch's sequence %d:\n%s",
		booted.Key, dispatch.DaemonSession(booted.Key), written.Seq, body)
	if lines := r.personHandoverLogLines(booted.Key, "event"); lines != 0 {
		t.Fatalf("the first boot's consumer saw the person's label of %s and queued it from the event: the consumer was not positioned after it; its log:\n%s", booted.Key, r.logs.String())
	}
	if notices := r.triageNotices(booted.Key); notices != 0 {
		t.Fatalf("the daemon queued %d triage notices on %s, a root a person labelled; want none", notices, booted.Key)
	}
	testwait.Eventually(t, "the outbox to finish the handover row of "+booted.Key, func() bool {
		return len(r.personHandoverRows(booted.Key)) == 0
	})
}
