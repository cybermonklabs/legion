package api

// read_class_drive_seed_test.go builds the one seeded store the drives of read_class_drive_test.go
// read: every kind of state a GET row of the table can read, written through the API's own write
// routes where it has one and through the package's own fixtures where it has none, so the data is
// the data the server writes.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// drivePerson is P: the person the golden file's machine-login answer names, whom every
	// token the drives send acts for, and the person the seeded work waits on.
	drivePerson = "ada@example.com"
	// driveSession is S: the session those broker tokens prove, and the one session the
	// listener stand-in lists.
	driveSession = "ses_d71e0a5b3c9f"
	// driveProject holds the seeded work. driveRelatedProject and precheckRelatedProject hold
	// none and have no term statistics, so a related read of either starts their refresh: the
	// related row's read of the first, and the pre-check's of the second.
	driveProject           = "DRV"
	driveRelatedProject    = "RELB"
	precheckRelatedProject = "RELA"
	// driveRepo is the repository the drive project's issues name, on driveGitHub.
	driveRepo = "acme/widgets"
	// driveSessionsJSON is every session the listener stand-in lists: S alone, live, seen at a
	// fixed moment, so two reads of the roster answer alike.
	driveSessionsJSON = `[{"session_id":"` + driveSession + `","title":"The drive's session","roles":["implementer"],` +
		`"capabilities":["aside","btw","steer"],"state":"live","last_seen":1763600000000}]`
)

// driveWorld is the seeded store and the server that serves it: the rig with its stub broker,
// the untraced store a fixture writes through, the tracer on the server's whole pool, the GitHub
// stand-in, the caller whose key every broker token is bound to, the four tokens the drives send,
// and the value of every placeholder a drive's target names ($name, filled by fill).
type driveWorld struct {
	rig    *brokerTokenRig
	raw    *store.Store
	tracer *poolStatements
	github *driveGitHub
	caller *brokerCaller
	// readOnly is the golden machine-login narrowed answer's token (read-person, P, S) and
	// writer the same answer with the project scope person and no read; personalReadOnly is P's
	// personal token holding read and person, and personalWriter P's holding person.
	readOnly, writer, personalReadOnly, personalWriter string
	values                                             map[string]string
}

// fill is target with every $name it names replaced by the world's value, failing for a name
// the world holds no value of.
func (w *driveWorld) fill(t *testing.T, target string) string {
	t.Helper()
	return os.Expand(target, func(name string) string {
		value, ok := w.values[name]
		if !ok {
			t.Fatalf("the target %s names $%s, which the seeded store holds no value of", target, name)
		}
		return value
	})
}

// newDriveWorld is the drive rig (newDriveRig) seeded with every kind of state a GET row reads,
// its documents settled and its rooms closed.
func newDriveWorld(t *testing.T) *driveWorld {
	t.Helper()
	world := newDriveRig(t)
	world.seed(t)
	awaitSettledDocuments(t, world.raw)
	// Every room the seed's writes opened, closed before the drive, once their settlements are
	// done: the document service evicts an idle room a minute after its last write, on its own
	// clock, which would change what the resident rooms row answers between a read-only request
	// and its writer's.
	world.as(t, "alice", http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, http.StatusOK)
	awaitSettledDocuments(t, world.raw)
	return world
}

// newDriveRig builds the server on a store of its own, traced whole, with a GitHub, a listener
// and the test hooks, and mints the four tokens; it seeds nothing else.
func newDriveRig(t *testing.T) *driveWorld {
	t.Helper()
	raw := storetest.Open(t)
	world := &driveWorld{raw: raw, tracer: &poolStatements{}, github: &driveGitHub{}, values: map[string]string{}}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the GitHub App's key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	world.rig = newBrokerTokenRigOn(t, testServerOptions{
		store:         raw,
		tracer:        world.tracer,
		testHooks:     true,
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: world.github.serve(t),
		envoyURL: driveListener(t, driveSessionsJSON, map[string][]string{
			driveSession: {"notifications.dispatch.issue." + driveProject + "-1.>", "notifications.dispatch.document." + driveProject + ".>"},
		}),
	})
	world.caller = newBrokerCaller(t)
	seedPeople(t, raw, drivePerson)
	world.mintTokens(t)
	return world
}

// mintTokens says the two broker tokens to the stub broker, for the caller's key, and mints P's
// two personal tokens through the route a person mints one on; each is held to be the credential
// it must be.
func (w *driveWorld) mintTokens(t *testing.T) {
	t.Helper()
	answer := ciphertokentest.NarrowedAnswers(t, w.caller.jkt, driveSession, time.Now().Add(time.Hour).Unix())["machine-login"]
	if answer.Person != drivePerson || answer.AsOf != "" {
		t.Fatalf("the golden machine-login answer names %q with the cut %q, want %s and no cut", answer.Person, answer.AsOf, drivePerson)
	}
	writer := answer
	writer.ProjectScope, writer.Scope = ciphertoken.ProjectScopePerson, ""
	w.readOnly, w.writer = "the-drive-read-only-token", "the-drive-writer-token"
	w.rig.broker.Says(w.readOnly, answer)
	w.rig.broker.Says(w.writer, writer)
	for _, token := range []struct {
		name     string
		readOnly bool
	}{{w.readOnly, true}, {w.writer, false}} {
		actor, _, err := w.rig.served.brokerTokenActor(w.caller.presenting(t, http.MethodGet, "/api/v1/whoami", token.name, nil))
		if err != nil || actor.Credential == nil || actor.Credential.ReadOnly != token.readOnly ||
			actor.Owner == nil || *actor.Owner != drivePerson || actor.ID != driveSession {
			t.Fatalf("%s: Dispatch's actor is %+v (%v), want session %s of %s, read-only %v", token.name, actor, err, driveSession, drivePerson, token.readOnly)
		}
	}
	for _, token := range []struct {
		target   *string
		scopes   []string
		readOnly bool
	}{
		{&w.personalReadOnly, []string{model.ScopeRead, model.ScopePerson}, true},
		{&w.personalWriter, []string{model.ScopePerson}, false},
	} {
		minted := mintPersonalToken(t, w.rig.handler, drivePerson, fmt.Sprintf("the drive's %v token", token.scopes), token.scopes)
		actor, _, err := w.rig.served.optionalActor(brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+minted.Token, ""))
		if credential := actor.Credential; err != nil || credential == nil || credential.ReadOnly != token.readOnly ||
			!credential.Holds(model.ScopePerson) || actor.Owner == nil || *actor.Owner != drivePerson {
			t.Fatalf("P's personal token holding %v: Dispatch's credential is %+v (%v), want one of %s holding person, read-only %v",
				token.scopes, credential, err, drivePerson, token.readOnly)
		}
		*token.target = minted.Token
	}
}

// startPersonalPair leaves P's personal tokens with the last uses a personal pair needs: the
// read-only token's none, so the read-only request makes its last-use write (agentTokenUse, at
// most once a minute), and the writer's now, so the writer's request, sent within that minute,
// makes none. Both answers then read one state: the read-only request's own write is under each,
// and the writer's request writes nothing a token list (GET /api/v1/me/agent-tokens) shows. It
// writes through the untraced store, before the window opens.
func (w *driveWorld) startPersonalPair(t *testing.T) {
	t.Helper()
	for _, statement := range []string{
		`update agent_read_tokens set last_used_at = null where owner = $1`,
		`update agent_tokens set last_used_at = now() where owner = $1`,
	} {
		if _, err := w.raw.Pool.Exec(context.Background(), statement, drivePerson); err != nil {
			t.Fatalf("set the last use of %s's personal tokens (%s): %v", drivePerson, statement, err)
		}
	}
}

// as is one write of the seed, as login through the header identity, which must answer want.
func (w *driveWorld) as(t *testing.T, login, method, target string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	response := dispatchRequest(t, w.rig.handler, method, target, body, login)
	if response.Code != want {
		t.Fatalf("seed %s %s as %s: status=%d body=%s, want %d", method, target, login, response.Code, response.Body.String(), want)
	}
	return response
}

// asSession is one write of the seed as session, over the deployment's shared token, which names
// its writer in the body.
func (w *driveWorld) asSession(t *testing.T, session, method, target string, body map[string]any, want int) *httptest.ResponseRecorder {
	t.Helper()
	body["actor"] = map[string]any{"kind": "session", "id": session}
	response := sessionRequest(t, w.rig.handler, method, target, body)
	if response.Code != want {
		t.Fatalf("seed %s %s as session %s: status=%d body=%s, want %d", method, target, session, response.Code, response.Body.String(), want)
	}
	return response
}

// idOf is the id member of response's body, failing for none.
func idOf(t *testing.T, what string, response *httptest.ResponseRecorder) string {
	t.Helper()
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
	if id == "" {
		t.Fatalf("the seeded %s has no id: %s", what, response.Body.String())
	}
	return id
}

// driveSpec is the drive issue's spec: a requirement, a paragraph the anchors quote, and a task.
const driveSpec = "# The drive\n\n" +
	":::requirement{#r1}\nThe issue page lists every open ask.\n:::\n\n" +
	"A paragraph the anchors quote: before the cut, and after it.\n\n" +
	"- [ ] A task of the spec\n"

// seed writes every kind of state the GET rows read. Each step says which rows read it.
func (w *driveWorld) seed(t *testing.T) {
	t.Helper()
	alice, ada := "alice", drivePerson
	w.values["session"] = driveSession
	w.values["person"] = drivePerson
	w.values["project"] = driveProject
	w.values["relatedProject"] = driveRelatedProject
	w.values["precheckProject"] = precheckRelatedProject

	// The projects: the drive project, whose default repository is driveRepo, defaulting to P and
	// reading its architecture from legion/arch; and the two projects with no work and no term
	// statistics.
	for _, project := range []string{driveProject, driveRelatedProject, precheckRelatedProject} {
		w.as(t, alice, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "The " + project + " project"}, http.StatusCreated)
	}
	w.as(t, alice, http.MethodPut, "/api/v1/projects/"+driveProject+"/default-repository", map[string]any{"repo": driveRepo}, http.StatusOK)
	w.as(t, alice, http.MethodPut, "/api/v1/projects/"+driveProject+"/default-assignee", map[string]any{"assignee": ada}, http.StatusOK)
	w.as(t, alice, http.MethodPut, "/api/v1/projects/"+driveProject+"/architecture-sources/legion/arch", map[string]any{"branch": "main"}, http.StatusOK)
	w.as(t, alice, http.MethodPost, "/api/v1/projects/"+driveProject+"/architecture-sources/legion/arch/sync", nil, http.StatusOK)

	// The Legion daemon setting, naming a daemon nothing samples here.
	daemon := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(daemon.Close)
	w.as(t, alice, http.MethodPut, "/api/v1/projects/"+driveProject+"/legion/daemon", map[string]any{"state_url": daemon.URL}, http.StatusOK)

	// The drive issue, routed to S, with a child, and a finished issue with its plan and steps.
	issue := decodeBody[model.Issue](t, w.as(t, alice, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": driveProject, "title": "The drive reads every row", "spec": driveSpec,
	}, http.StatusCreated))
	w.values["issue"] = issue.Key
	w.values["issueRef"] = url.QueryEscape("dispatch://" + issue.Key)
	w.values["spec"] = issue.PrimaryArtifactID
	w.values["specRef"] = url.QueryEscape("dispatch://" + issue.Key + "/spec")
	w.as(t, alice, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "session:" + driveSession, "assignee": ada}, http.StatusOK)
	w.as(t, alice, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": driveProject, "title": "A child the drive issue holds", "parent": issue.Key,
	}, http.StatusCreated)
	finished := decodeBody[model.Issue](t, w.as(t, alice, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": driveProject, "title": "Finished work with a plan",
	}, http.StatusCreated)).Key
	daemonActor := daemonSessionOf(driveProject)
	putPlanRecordOK(t, w.rig.handler, finished, uuid.NewString(), defaultPlanBody(daemonActor, 1))
	recordRuns(t, w.rig.handler, driveProject, finished,
		run{"step", "planner", 1, new(at(0)), new(at(5)), "passed"},
		run{"step", "implementer", 2, new(at(5)), new(at(20)), "passed"},
	)
	closeIssue(t, w.rig.handler, finished)

	// The drive issue's steps, handoff and role holders.
	putStepOK(t, w.rig.handler, issue.Key, uuid.NewString(), implementerStart(daemonActor))
	w.values["phase"] = "planning"
	handoff := w.asSession(t, driveSession, http.MethodPost, "/api/v1/issues/"+issue.Key+"/handoffs/planning",
		map[string]any{"body": `{"plan":"handed on"}`}, http.StatusCreated)
	w.values["handoff"] = idOf(t, "handoff", handoff)
	if published := putRoleHoldersAs(t, w.rig.handler, issue.Key, map[string]any{
		"holders": []issueRoleHolder{{Role: "implementer", Session: driveSession}}, "actor": daemonActor,
	}); published.Code != http.StatusOK {
		t.Fatalf("publish the drive issue's role holders: status=%d body=%s", published.Code, published.Body.String())
	}

	// Documents: notes in two versions, reviewed; the decisions; a project document; and a file in
	// S's conversation.
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, w.rig.handler, owner, "notes.md", "# Notes\n\nThe first version.\n")
	notes := uploadDocument(t, w.rig.handler, owner, "notes.md", "# Notes\n\nThe second version.\n")
	w.values["notes"] = notes.Artifact.ID
	w.values["notesSlug"] = notes.Artifact.Slug
	openApprovalAsk(t, w.rig.handler, notes.Artifact.ID)
	w.as(t, ada, http.MethodPost, "/api/v1/artifacts/"+notes.Artifact.ID+"/reviews", map[string]any{"state": "approved"}, http.StatusCreated)
	// A document whose block links into the spec, for the block-links read. This change refuses a
	// new decisions.md (409 DECISIONS_ARE_NODES), so it is links.md.
	links := uploadDocument(t, w.rig.handler, owner, "links.md",
		"## Read every row\n\nShapes dispatch://"+issue.Key+"/spec#r1.\n")
	w.values["linksRef"] = url.QueryEscape("dispatch://" + issue.Key + "/artifact/" + links.Artifact.Slug)
	guide := uploadDocument(t, w.rig.handler, "/api/v1/projects/"+driveProject, "guide.md", "# Guide\n\nHow the drive project works.\n")
	w.values["guide"] = guide.Artifact.ID
	w.values["guideSlug"] = guide.Artifact.Slug
	file := multipartRequest(t, w.rig.handler, "/api/v1/agents/"+driveSession+"/artifacts", map[string]string{"name": "notes.txt"},
		"notes.txt", "text/plain", []byte("Plain notes in S's conversation."), ada)
	if file.Code != http.StatusCreated {
		t.Fatalf("upload a file to S's conversation: status=%d body=%s", file.Code, file.Body.String())
	}
	w.values["fileSlug"] = decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, file).Artifact.Slug

	// The spec's requirement block, whose path the block path row reads.
	blocks := decodeBody[[]struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}](t, w.as(t, alice, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, http.StatusOK))
	for _, block := range blocks {
		if block.Type == "requirement" {
			w.values["block"] = block.ID
		}
	}
	if w.values["block"] == "" {
		t.Fatalf("the drive issue's spec holds no requirement block: %+v", blocks)
	}

	// Comments: a remark, P's reply to it and a comment anchored on the spec. P's reply is what
	// ties P to a node of the graph, so P's page (GET /api/v1/people/{login}/nodes) lists it.
	remark := createThreadComment(t, w.rig.handler, issue.Key, map[string]any{"body": "A remark on the drive."}, alice)
	w.values["comment"] = remark.ID
	createThreadComment(t, w.rig.handler, issue.Key, map[string]any{"body": "P's reply to the remark.", "in_reply_to": remark.ID}, ada)
	createThreadComment(t, w.rig.handler, issue.Key, map[string]any{
		"body": "A comment on the cut.", "anchor": map[string]any{"artifact": "spec", "quote": "before the cut"},
	}, alice)

	// Messages: P's message to S on the issue and S's reply; P's direct message to S.
	message := createIssueMessage(t, w.rig.handler, issue.Key, map[string]any{
		"body": "P's message to the session.", "target": "session:" + driveSession, "delivery": "btw",
	}, ada)
	w.values["message"] = message.ID
	w.asSession(t, driveSession, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": "The session's reply.", "in_reply_to": message.ID}, http.StatusCreated)
	if direct := postAgentMessage(t, w.rig.handler, driveSession, map[string]any{"body": "P's direct note to the session.", "delivery": "btw"}, ada); direct.Code != http.StatusCreated {
		t.Fatalf("P's direct message to S: status=%d body=%s", direct.Code, direct.Body.String())
	}

	// A comment and an issue message, each corrected once by its author, so the history row of
	// each reads the one body the correction kept.
	correctedComment := createThreadComment(t, w.rig.handler, issue.Key, map[string]any{"body": "A remark its author corrects."}, alice)
	w.as(t, alice, http.MethodPatch, "/api/v1/comments/"+correctedComment.ID, map[string]any{"body": "A remark its author corrected."}, http.StatusOK)
	w.values["correctedComment"] = correctedComment.ID
	correctedMessage := createIssueMessage(t, w.rig.handler, issue.Key, map[string]any{"body": "A message its author corrects."}, alice)
	w.as(t, alice, http.MethodPatch, "/api/v1/messages/"+correctedMessage.ID, map[string]any{"body": "A message its author corrected."}, http.StatusOK)
	w.values["correctedMessage"] = correctedMessage.ID
	for _, history := range []string{"/api/v1/comments/" + correctedComment.ID + "/history", "/api/v1/messages/" + correctedMessage.ID + "/history"} {
		if kept := readPostHistory(t, w.rig.handler, history); len(kept.Entries) != 1 {
			t.Fatalf("GET %s holds %d kept bodies, want the one the correction kept", history, len(kept.Entries))
		}
	}

	// Asks: one open on the issue, which waits on P; one S asked, which P answered; and one
	// anchored on the spec.
	w.as(t, alice, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which way does the drive go?", "options": []map[string]string{{"label": "North"}, {"label": "South"}},
	}, http.StatusCreated)
	answered := openAskAs(t, w.rig.handler, issue.Key, driveSession, "Shall the session go on?")
	w.values["followedAsk"] = answered
	w.as(t, ada, http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{"selected": []string{"Yes"}, "expected_edited_at": nil}, http.StatusOK)
	w.values["ask"] = openAnchoredAsk(t, w.rig.handler, issue.Key, "spec", "after it", "Is the cut in the right place?")

	// A broadcast to S.
	broadcast := postBroadcast(t, w.rig.handler, map[string]any{"body": "Report your state.", "delivery": "btw", "session_ids": []string{driveSession}}, ada)
	if broadcast.Code != http.StatusCreated {
		t.Fatalf("broadcast to S: status=%d body=%s", broadcast.Code, broadcast.Body.String())
	}
	w.values["broadcast"] = idOf(t, "broadcast", broadcast)

	// A priority the drive issue advances.
	w.values["priority"] = createTestPriority(t, w.rig.handler, map[string]any{
		"id": "drive-every-row", "title": "Drive every row", "description": "Every read row is driven.", "owner": alice,
	}, alice).ID
	w.as(t, alice, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"advances": []string{w.values["priority"]}}, http.StatusOK)

	// P's own state: the issue pinned, the notes read, and S's conversation pinned.
	w.as(t, ada, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{"pinned": true}, http.StatusOK)
	w.as(t, ada, http.MethodPut, "/api/v1/me/documents/"+notes.Artifact.ID+"/read", map[string]any{"version": 1}, http.StatusOK)
	putAgentState(t, w.rig.handler, ada, driveSession, map[string]any{"pinned": true})

	// The results: a verification document with a before_merge, an after_merge and a
	// default_branch check, each with a result. GitHub names, once they are recorded, the pull
	// request whose head is the before_merge result's commit, merged into integration/x, which
	// no row holds; and it answers which branches carry the other results' commits.
	verification := ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe server records a result against a check.\n:::\n\n" +
		afterMergeAndDefaultChecks
	uploadDocument(t, w.rig.handler, owner, "verification.md", verification)
	for _, result := range []struct{ check, commit string }{
		{"k1", headCommit}, {"k4", baseMergeCommit}, {"k4", integrationCommit}, {"k5", mergeCommit},
	} {
		if recorded := recordResult(t, w.rig.handler, issue.Key, resultBody(result.check, "pass", result.commit, nil)); recorded.Code != http.StatusCreated {
			t.Fatalf("record %s at %s: status=%d body=%s", result.check, result.commit, recorded.Code, recorded.Body.String())
		}
	}
	w.github.answersPulls(func(repo, sha string) string {
		if sha != headCommit {
			return `[]`
		}
		return fmt.Sprintf(`[{"number":50,"title":%q,"body":"","html_url":"https://github.com/%s/pull/50",
			"state":"closed","merged_at":"2026-11-05T07:15:08Z","merge_commit_sha":%q,
			"updated_at":"2026-11-05T07:15:08Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":%q}},"base":{"ref":"integration/x"}}]`,
			issue.Key+" the work", repo, baseMergeCommit, headCommit, repo)
	})
	w.github.answersCompare(func(base, head string) string {
		if (base == "integration/x" && head == integrationCommit) || (base == "main" && head == mergeCommit) {
			return "behind"
		}
		return "diverged"
	})

	// The delivery and going state the GETs that write read (seedWrites), and the session answers
	// row's window, which starts at a moment fixed here, before every write the seed made, so two
	// reads of it answer one since rather than each its own three days back.
	w.seedWrites(t)
	w.values["answersSince"] = url.QueryEscape(time.Now().UTC().Truncate(time.Second).Add(-time.Hour).Format(time.RFC3339))

	// A CI reconcile pass, and the near-copy gate's answer to a drive project create.
	openCIPass(t, w.raw, uuid.NewString(), time.Minute)
	var nearCopy string
	if err := w.raw.Pool.QueryRow(context.Background(), `
		select id::text from near_copy_checks where project_key = $1 order by created_at limit 1
	`, driveProject).Scan(&nearCopy); err != nil {
		t.Fatalf("read the near-copy gate's answer to a drive project create: %v", err)
	}
	w.values["nearCopy"] = nearCopy

	// The pre-check's prose: a document no other test of this process has shown. A write stores
	// the tree of every text it writes in the latest mode, and only a read of a version stores a
	// tree in the version mode, so the pre-check's read of this version is where its tree first
	// reaches the table.
	prose := uploadDocument(t, w.rig.handler, owner, "prose.md", "# Prose\n\nProse no other test of this process has shown: "+uuid.NewString()+".\n")
	w.values["prose"] = prose.Artifact.ID

	// The route roster S is on, stored by one read that needs it.
	w.as(t, alice, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
}

// seedWrites writes the state the GETs that write read (getWriteDrives): the delivery settings and
// three deploy runs on main with no job the settings name as production's, each the unmatched run
// of one delivery read, the newest the timeline's, the middle the measures', the oldest the
// pipeline's; and a going run past its deadline with no ending stored, asked by the writer's
// credential.
func (w *driveWorld) seedWrites(t *testing.T) {
	t.Helper()
	settings := pipelineSettings()
	if _, err := delivery.PutSettings(t.Context(), w.raw.Pool, settings, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	runs := []struct {
		name string
		id   int64
		at   time.Time
	}{{"pipelineRun", 9001, now.Add(-30 * time.Hour)}, {"measuresRun", 9002, now.Add(-20 * time.Hour)}, {"timelineRun", 9003, now.Add(-10 * time.Hour)}}
	for _, unmatched := range runs {
		seedPipelineRun(t, w.raw.Pool, delivery.DeliveryRun{
			RunID: unmatched.id, Kind: delivery.DeliveryRunKindDeploy, StartedAt: unmatched.at,
			CompletedAt: new(unmatched.at.Add(10 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			HeadBranch: new("main"), Event: new("push"),
		}, []pipelineFixtureJob{{name: "build / unit", conclusion: "success", start: unmatched.at, end: unmatched.at.Add(5 * time.Minute)}})
		w.values[unmatched.name] = strconv.FormatInt(unmatched.id, 10)
	}
	window := func(at time.Time) (string, string) {
		return url.QueryEscape(at.Add(-time.Minute).Format(time.RFC3339)), url.QueryEscape(at.Add(time.Minute).Format(time.RFC3339))
	}
	w.values["timelineFrom"], w.values["timelineTo"] = window(runs[2].at)
	w.values["measuresFrom"], w.values["measuresTo"] = window(runs[1].at)
	w.values["pipelineAt"] = url.QueryEscape(now.Format(time.RFC3339))

	writerActor, _, err := w.rig.served.brokerTokenActor(w.caller.presenting(t, http.MethodGet, "/api/v1/whoami", w.writer, nil))
	if err != nil {
		t.Fatalf("resolve the writer's actor: %v", err)
	}
	author, err := json.Marshal(writerActor)
	if err != nil {
		t.Fatalf("encode the writer's actor: %v", err)
	}
	var going string
	if err := w.raw.Pool.QueryRow(context.Background(), `
		insert into going_requests (author, project, words, created_at, ends_at)
		values ($1::jsonb, '', 'what happened to the pod?', now() - interval '2 minutes', now() - interval '90 seconds')
		returning id::text
	`, author).Scan(&going); err != nil {
		t.Fatalf("write a going run past its deadline: %v", err)
	}
	w.values["going"] = going
}

// staleRoster makes the stored route roster an hour past its expiry, so the next read that needs
// it fetches it again.
func (w *driveWorld) staleRoster(t *testing.T) {
	t.Helper()
	tag, err := w.raw.Pool.Exec(context.Background(), `update route_roster set expires_at = now() - interval '1 hour'`)
	if err != nil {
		t.Fatalf("make the route roster stale: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("made %d route roster rows stale, want the one row", tag.RowsAffected())
	}
}
