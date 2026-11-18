//go:build basebuild

package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// goneDecisions is what the test reads of GET /api/v1/issues/{key}/decisions: the node entries and
// the members the release before reads.
type goneDecisions struct {
	Entries []struct {
		Node struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"node"`
		Kind        string  `json:"kind"`
		Text        string  `json:"text"`
		TextSHA256  string  `json:"text_sha256"`
		KeptBy      *string `json:"kept_by"`
		KeptTextSum *string `json:"kept_text_sum"`
		Document    *struct {
			ArtifactID string `json:"artifact_id"`
			BlockID    string `json:"block_id"`
		} `json:"document"`
	} `json:"entries"`
	Document  *string `json:"document"`
	Decisions []struct {
		Ref           string `json:"ref"`
		Kind          string `json:"kind"`
		Text          string `json:"text"`
		WaitingReview bool   `json:"waiting_review"`
		Kept          *struct {
			KeptBy string `json:"kept_by"`
		} `json:"kept"`
		Links []struct {
			To struct {
				ID string `json:"id"`
			} `json:"to"`
		} `json:"links"`
	} `json:"decisions"`
}

// readGoneDecisions reads the Decisions read of issue on p.
func readGoneDecisions(t *testing.T, p *dispatchProcess, issue string) goneDecisions {
	t.Helper()
	var read goneDecisions
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/issues/"+issue+"/decisions", "", http.StatusOK), &read); err != nil {
		t.Fatalf("decode the Decisions read of %s: %v", issue, err)
	}
	return read
}

// issueDecisionsArtifactID is the issue read's old decisions_artifact_id member.
func issueDecisionsArtifactID(t *testing.T, p *dispatchProcess, issue string) string {
	t.Helper()
	var read struct {
		DecisionsArtifactID string `json:"decisions_artifact_id"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/issues/"+issue, "", http.StatusOK), &read); err != nil {
		t.Fatalf("decode the issue read of %s: %v", issue, err)
	}
	return read.DecisionsArtifactID
}

// listedSteps is each project's gated steps as GET /api/v1/projects answers them on p.
func listedSteps(t *testing.T, p *dispatchProcess) map[string][]string {
	t.Helper()
	var projects []struct {
		Key        string   `json:"key"`
		GatedSteps []string `json:"gated_steps"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/projects", "", http.StatusOK), &projects); err != nil {
		t.Fatalf("decode the project list: %v", err)
	}
	steps := map[string][]string{}
	for _, project := range projects {
		steps[project.Key] = project.GatedSteps
	}
	return steps
}

// storedSteps is a project's stored projects.gated_steps.
func storedSteps(t *testing.T, ctx context.Context, database *pgx.Conn, project string) []string {
	t.Helper()
	var steps []string
	if err := database.QueryRow(ctx, `select gated_steps from projects where key = $1`, project).Scan(&steps); err != nil {
		t.Fatalf("read the stored steps of %s: %v", project, err)
	}
	return steps
}

// healthzMembers is /healthz on p as a map, so a member's absence is read as absence.
func healthzMembers(t *testing.T, p *dispatchProcess) map[string]any {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/healthz", "", nil)
	var members map[string]any
	if err := json.Unmarshal(answer.body, &members); err != nil {
		t.Fatalf("decode /healthz %s: %v", answer, err)
	}
	return members
}

// headingSection is the heading block of the section of document that opens with heading, and the
// section's markdown from that heading to its last block, as the live document renders it.
func headingSection(t *testing.T, p *dispatchProcess, document, heading, last string) (string, string) {
	t.Helper()
	var read struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/artifacts/"+document+"/text", "", http.StatusOK), &read); err != nil {
		t.Fatalf("decode the text of %s: %v", document, err)
	}
	var blocks []struct {
		ID   string `json:"id"`
		From int    `json:"from"`
		To   int    `json:"to"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/artifacts/"+document+"/blocks", "", http.StatusOK), &blocks); err != nil {
		t.Fatalf("decode the blocks of %s: %v", document, err)
	}
	headingID, start, end := "", -1, -1
	for _, block := range blocks {
		switch read.Markdown[block.From:block.To] {
		case heading:
			headingID, start = block.ID, block.From
		case last:
			end = block.To
		}
	}
	if headingID == "" || start < 0 || end < start {
		t.Fatalf("the document %s holds no block %q followed by %q:\n%s", document, heading, last, read.Markdown)
	}
	return headingID, read.Markdown[start:end]
}

// keepAnswer posts a person's Keep of block of document at sum on p.
func keepAnswer(t *testing.T, p *dispatchProcess, document, block, sum string) httpAnswer {
	t.Helper()
	body, err := json.Marshal(map[string]string{"text_sha256": sum})
	if err != nil {
		t.Fatal(err)
	}
	return p.send(t, http.MethodPost, "/api/v1/artifacts/"+document+"/blocks/"+block+"/keep", string(body), asUser(dispatchTestLogin))
}

// c-document-gone (dispatch://INFRA-1150 R11): no read answers from an issue's decisions document.
//
// Through the base release, the test seeds an issue whose decisions.md holds two `##` sections
// that cite no ask, one of which links a block of the spec, and no decision node, and publishes
// the gated steps `decisions, criteria, spec` for one project and `decisions` alone for a second.
// Before this branch migrates, it clears the document's role by SQL, as a document stored before
// the role existed holds it. After this branch's migrations the role still reads null, the stored
// steps are as published, and decisions_documents() answers the document by its slug.
//
// This head, booted with the step held: the Decisions read answers an empty `entries` and an empty
// `decisions` member, its `document` the decisions document's address, and the issue read its
// `decisions_artifact_id`. A person keeps a heading of the document: 200, and a decision_keeps row.
// `dispatch://CORE-1/decisions` and `/issues/CORE-1/decisions` each read as the issue CORE-1, the
// shared href table's row names the Decisions page, and neither reads as the artifact. The
// gated-steps write takes `decisions` and stores the list without it, and `[decisions]` as
// `[spec]`. The two seeded projects read `criteria, spec` and `[spec]` before the step runs.
// /healthz answers no decisions_step_owed while the step is held.
//
// This head booted a second time without the hold, over the same database: the step converts the
// document. Its role reads null, its node gone, the issue's document list leaves it out, and each
// of its versions still answers. /healthz answers decisions_step_owed at zero once a pass has
// found nothing owed. The stored steps read `criteria, spec` and `[spec]`. The decision node the
// kept section became carries the Keep. A heading of the retired document is 404, a Keep on a
// decision node 200. A new document named decisions.md, or slugged decisions-md, is refused 409
// DECISIONS_ARE_NODES, saying to write a `decision` block in the spec, on the issue whose
// decisions document is retired and on an issue that holds none, and one of another name is
// taken. The old members answer from the store: the same two decision nodes in model.Decision's
// shape, each `ref` naming a node of `entries`, the linked section's link and the kept decision's
// Keep, a null `document` and an empty `decisions_artifact_id`. The three checks still admit
// `decisions`, and no route stored it. A go/parser scan of the api package finds no
// issueDecisionsDocument, and finds keepDecision.
func TestNoReadReachesTheDecisionsDocument(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	head := cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	defer database.Close(ctx)

	// ── The base release writes the corpus. ──
	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	previous.waitHealthy(t)
	baseCommit := announcedBaseCommit(t, ctx, database)
	for _, project := range []string{"GONE", "GSA", "GSB"} {
		previous.call(t, http.MethodPost, "/api/v1/projects", `{"key":"`+project+`","name":"`+project+` project"}`, http.StatusCreated)
	}
	issue, spec := createCensusIssue(t, previous, "GONE", "A decisions document", "# Plan\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	const (
		linkedHeading = "## The store holds the corpus"
		otherHeading  = "## The census runs first"
		otherBody     = "The census reads the corpus before any view of it."
	)
	linkedBody := "We chose Postgres. This shapes dispatch://" + issue + "/spec#b-r1."
	decisions := agentUpload(t, previous, issue, "decisions.md", "# Decisions\n\n"+linkedHeading+"\n\n"+linkedBody+"\n\n"+otherHeading+"\n\n"+otherBody+"\n")
	settledDocument(t, ctx, previous, database, decisions)
	previous.call(t, http.MethodPut, "/api/v1/projects/GSA/gated-steps", `{"steps":["decisions","criteria","spec"]}`, http.StatusOK)
	previous.call(t, http.MethodPut, "/api/v1/projects/GSB/gated-steps", `{"steps":["decisions"]}`, http.StatusOK)
	t.Logf("the base release %s wrote %s's decisions.md %s and the two projects' steps", baseCommit, issue, decisions)
	stopProcess(t, previous)
	waitNoLiveProcess(t, ctx, database)

	// A document stored before the role existed holds none.
	if _, err := database.Exec(ctx, `update artifacts set role = null where id = $1::uuid`, decisions); err != nil {
		t.Fatalf("clear the decisions document's role: %v", err)
	}
	migrated, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the run's database: %v", err)
	}
	defer migrated.Pool.Close()
	if err := migrated.Migrate(ctx); err != nil {
		t.Fatalf("apply this build's migrations: %v", err)
	}
	var role *string
	if err := database.QueryRow(ctx, `select role from artifacts where id = $1::uuid`, decisions).Scan(&role); err != nil || role != nil {
		t.Fatalf("after this branch's migrations the decisions document's role reads %v (%v), want null: no migration rewrites a row", role, err)
	}
	var found []string
	rows, err := database.Query(ctx, `select id::text from decisions_documents() where issue_key = $1`, issue)
	if err != nil {
		t.Fatalf("read decisions_documents(): %v", err)
	}
	found, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(found, []string{decisions}) {
		t.Fatalf("decisions_documents() answers %v (%v) for %s, want the roleless document %s by its slug", found, err, issue, decisions)
	}
	if a, b := storedSteps(t, ctx, database, "GSA"), storedSteps(t, ctx, database, "GSB"); !slices.Equal(a, []string{"decisions", "criteria", "spec"}) || !slices.Equal(b, []string{"decisions"}) {
		t.Fatalf("after this branch's migrations the stored steps read %v and %v, want them as the base release published them", a, b)
	}

	// ── This head, the step held. ──
	held := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay,
		Env: map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_HOLD_DECISIONS_STEP": "1"}})
	held.waitHealthy(t)
	if member, present := healthzMembers(t, held)["decisions_step_owed"]; present {
		t.Fatalf("/healthz answers decisions_step_owed %v while the step is held, want no such member", member)
	}
	before := readGoneDecisions(t, held, issue)
	if len(before.Entries) != 0 || len(before.Decisions) != 0 {
		t.Fatalf("before the conversion the Decisions read answers %d entries and %d decisions, want none: no section of the document reaches a caller", len(before.Entries), len(before.Decisions))
	}
	documentRef := "dispatch://" + issue + "/artifact/decisions-md"
	if before.Document == nil || *before.Document != documentRef {
		t.Fatalf("before the conversion the old document member reads %v, want %s while the document stands", before.Document, documentRef)
	}
	if id := issueDecisionsArtifactID(t, held, issue); id != decisions {
		t.Fatalf("before the conversion the issue read names decisions_artifact_id %q, want %s while the document stands", id, decisions)
	}

	// A person keeps a heading of the document, as the release before keeps it.
	linkedHeadingID, linkedSection := headingSection(t, held, decisions, linkedHeading, linkedBody)
	if kept := keepAnswer(t, held, decisions, linkedHeadingID, decisionread.TextSum(linkedSection)); kept.status != http.StatusOK {
		t.Fatalf("a Keep of the heading %q answered %s, want 200", linkedHeading, kept)
	}
	var keeps int
	if err := database.QueryRow(ctx, `select count(*) from decision_keeps where artifact_id = $1::uuid and block_id = $2`, decisions, linkedHeadingID).Scan(&keeps); err != nil || keeps != 1 {
		t.Fatalf("the heading Keep wrote %d decision_keeps rows (%v), want one", keeps, err)
	}

	// The Decisions page's two forms read as the issue, never as the artifact.
	const server = "https://dispatch.test"
	for _, form := range []string{"dispatch://CORE-1/decisions", server + "/issues/CORE-1/decisions"} {
		refs := text.Extract(form, server)
		if len(refs) != 1 || refs[0].Kind != "issue" || refs[0].IssueKey != "CORE-1" || refs[0].ID != "CORE-1" {
			t.Fatalf("%s reads as %#v, want the issue CORE-1 (Ref{Kind:\"issue\"})", form, refs)
		}
	}
	var hrefs []struct {
		Href string `json:"href"`
		Ref  string `json:"ref"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "dispatch", "text", "testdata", "dispatch-href-references.json"))
	if err != nil || json.Unmarshal(raw, &hrefs) != nil {
		t.Fatalf("read the shared href table: %v", err)
	}
	if !slices.ContainsFunc(hrefs, func(row struct {
		Href string `json:"href"`
		Ref  string `json:"ref"`
	}) bool {
		return row.Href == "/issues/CORE-1/decisions" && row.Ref == "dispatch://CORE-1/decisions"
	}) {
		t.Fatalf("the shared href table holds no row naming /issues/CORE-1/decisions as dispatch://CORE-1/decisions: %s", raw)
	}

	// The gated-steps write takes the step a page of the release before sends and stores the
	// list without it; a list of that step alone is stored as [spec].
	for _, write := range []struct {
		published string
		stored    []string
	}{
		{`["decisions","criteria","spec"]`, []string{"criteria", "spec"}},
		{`["decisions"]`, []string{"spec"}},
	} {
		held.call(t, http.MethodPut, "/api/v1/projects/GONE/gated-steps", `{"steps":`+write.published+`}`, http.StatusOK)
		if got := storedSteps(t, ctx, database, "GONE"); !slices.Equal(got, write.stored) {
			t.Fatalf("publishing %s stored %v, want %v", write.published, got, write.stored)
		}
	}
	listed := listedSteps(t, held)
	if !slices.Equal(listed["GSA"], []string{"criteria", "spec"}) || !slices.Equal(listed["GSB"], []string{"spec"}) {
		t.Fatalf("before the step runs this build reads the stored steps as %v and %v, want [criteria spec] and [spec]", listed["GSA"], listed["GSB"])
	}
	stopProcess(t, held)

	// ── This head, unheld: the step converts the document. ──
	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay,
		Env: map[string]string{"DISPATCH_TEST_HOOKS": "1"}})
	// The unheld boot runs the step's first phase before it serves, over every row the base release
	// wrote, so it takes longer than a held boot does.
	if err := current.healthyWithin(3 * time.Minute); err != nil {
		t.Fatal(err)
	}
	owed := -1.0
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
		if member, present := healthzMembers(t, current)["decisions_step_owed"].(float64); present {
			owed = member
			if owed == 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz answers decisions_step_owed %v three minutes after the unheld boot, want 0 once a pass has found nothing owed", owed)
		}
	}
	t.Logf("/healthz answers decisions_step_owed 0 after the step's passes")
	var retired bool
	if err := database.QueryRow(ctx, `select role, retired_at is not null from artifacts where id = $1::uuid`, decisions).Scan(&role, &retired); err != nil || role != nil || !retired {
		t.Fatalf("after the conversion the document reads role %v and retired %t (%v), want no role and retired", role, retired, err)
	}
	var refKey string
	if err := database.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, decisions).Scan(&refKey); err != nil {
		t.Fatalf("read the document's ref_key: %v", err)
	}
	if node, stands, err := graphstore.ReadNode(ctx, database, graphstore.KindArtifact, refKey); err != nil || !stands || node.State != graphstore.StateGone {
		t.Fatalf("after the conversion the document's node is held %t in state %q (%v), want gone", stands, node.State, err)
	}
	var listing []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(current.call(t, http.MethodGet, "/api/v1/issues/"+issue+"/artifacts", "", http.StatusOK), &listing); err != nil {
		t.Fatalf("decode the document list: %v", err)
	}
	for _, document := range listing {
		if document.ID == decisions {
			t.Fatalf("after the conversion the issue's document list still holds the decisions document %s", decisions)
		}
	}
	var versions int
	if err := database.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1::uuid`, decisions).Scan(&versions); err != nil || versions == 0 {
		t.Fatalf("the retired document holds %d versions (%v)", versions, err)
	}
	for number := 1; number <= versions; number++ {
		current.call(t, http.MethodGet, "/api/v1/artifacts/"+decisions+"/versions/"+strconv.Itoa(number), "", http.StatusOK)
	}
	if a, b := storedSteps(t, ctx, database, "GSA"), storedSteps(t, ctx, database, "GSB"); !slices.Equal(a, []string{"criteria", "spec"}) || !slices.Equal(b, []string{"spec"}) {
		t.Fatalf("after the step the stored steps read %v and %v, want [criteria spec] and [spec]", a, b)
	}

	// The decision node the kept section became carries the Keep, and each node answers the
	// compatibility members from the store.
	after := readGoneDecisions(t, current, issue)
	agentEntries := 0
	var keptNodeBlock, keptNodeDocument, keptNodeSum string
	for _, entry := range after.Entries {
		if entry.Kind != "agent" {
			continue
		}
		agentEntries++
		if strings.Contains(entry.Text, "We chose Postgres") {
			if entry.KeptBy == nil || *entry.KeptBy != dispatchTestLogin || entry.KeptTextSum == nil || entry.Document == nil {
				t.Fatalf("the decision node of the kept section reads kept_by %v and kept_text_sum %v, want the person's Keep carried onto it", entry.KeptBy, entry.KeptTextSum)
			}
			keptNodeBlock, keptNodeDocument, keptNodeSum = entry.Document.BlockID, entry.Document.ArtifactID, entry.TextSHA256
		}
	}
	if agentEntries != 2 || keptNodeBlock == "" {
		t.Fatalf("after the conversion the Decisions read answers %d decision nodes and the kept one at %q, want the two sections as nodes", agentEntries, keptNodeBlock)
	}
	if retiredKeep := keepAnswer(t, current, decisions, linkedHeadingID, decisionread.TextSum(linkedSection)); retiredKeep.status != http.StatusNotFound || retiredKeep.code() != "TARGET_NOT_FOUND" {
		t.Fatalf("a Keep on a heading of the retired document answered %s, want 404 TARGET_NOT_FOUND", retiredKeep)
	}
	if nodeKeep := keepAnswer(t, current, keptNodeDocument, keptNodeBlock, keptNodeSum); nodeKeep.status != http.StatusOK {
		t.Fatalf("a Keep on the decision node answered %s, want 200", nodeKeep)
	}

	// A new document of that name is refused by name, whether the issue holds a retired decisions
	// document or none; one of another name is taken.
	bare, _ := createCensusIssue(t, current, "GONE", "No decisions document", "# Plan\n\nNothing is decided here.\n")
	for _, target := range []string{issue, bare} {
		for _, name := range []string{"decisions.md", "decisions-md"} {
			body, _ := json.Marshal(map[string]string{"name": name, "content": "# Decisions\n\nOne.\n"})
			refused := current.send(t, http.MethodPost, "/api/v1/issues/"+target+"/artifacts", string(body), asUser(dispatchTestLogin))
			if refused.status != http.StatusConflict || refused.code() != "DECISIONS_ARE_NODES" ||
				!strings.Contains(string(refused.body), "`decision` block") || !strings.Contains(string(refused.body), "spec") {
				t.Fatalf("a new document named %s on %s answered %s, want 409 DECISIONS_ARE_NODES saying to write a `decision` block in the spec", name, target, refused)
			}
		}
	}
	current.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", `{"name":"notes.md","content":"# Notes\n\nOne.\n"}`, http.StatusCreated)

	// The old members answer from the store.
	compat := readGoneDecisions(t, current, issue)
	var specRefKey string
	if err := database.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, spec).Scan(&specRefKey); err != nil {
		t.Fatalf("read the spec's ref_key: %v", err)
	}
	if len(compat.Decisions) != 2 {
		t.Fatalf("the old decisions member answers %d decisions, want the two decision nodes", len(compat.Decisions))
	}
	entryNodes := map[string]bool{}
	for _, entry := range compat.Entries {
		if entry.Kind == "agent" {
			entryNodes[entry.Node.Kind+" "+entry.Node.ID] = true
		}
	}
	answered := map[string]bool{}
	for _, decision := range compat.Decisions {
		if decision.Kind != "agent" {
			t.Errorf("the old decisions member answers %s of kind %q, want agent for a decision node", decision.Ref, decision.Kind)
		}
		ref, ok := text.ParseRef(decision.Ref, "")
		node := ref.Kind + " " + text.TargetID(ref)
		if !ok || !entryNodes[node] || answered[node] {
			t.Errorf("the old decisions member answers %s, which names %q: want each of the two decision nodes of entries %v once", decision.Ref, node, entryNodes)
		}
		answered[node] = true
		if strings.Contains(decision.Text, "We chose Postgres") {
			if len(decision.Links) != 1 || decision.Links[0].To.ID != specRefKey+"#r1" {
				t.Errorf("the linked section's decision answers the links %+v, want one, to %s#r1, from its shaped tuple", decision.Links, specRefKey)
			}
			if decision.Kept == nil || decision.Kept.KeptBy != dispatchTestLogin || decision.WaitingReview {
				t.Errorf("the kept decision answers kept %+v and waiting_review %t, want the person's Keep and no wait", decision.Kept, decision.WaitingReview)
			}
		}
	}
	if compat.Document != nil {
		t.Errorf("after the retirement the old document member reads %q, want null", *compat.Document)
	}
	if id := issueDecisionsArtifactID(t, current, issue); id != "" {
		t.Errorf("after the retirement the issue read names decisions_artifact_id %q, want \"\"", id)
	}

	// The three checks still admit the value, and no route of this head stored it.
	tx, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, write := range []struct{ what, statement string }{
		{"artifacts_role_check", `update artifacts set role = 'decisions' where id = '` + decisions + `'::uuid`},
		{"the check on projects.gated_steps", `update projects set gated_steps = '{decisions}' where key = 'GSA'`},
		{"the check on design_gates.step", `insert into design_gates (issue, step, artifact_id) values ('` + issue + `', 'decisions', '` + decisions + `')`},
	} {
		if _, err := tx.Exec(ctx, "savepoint probe"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, write.statement); err != nil {
			t.Errorf("%s refuses decisions (%v), want it still admitted for one release", write.what, err)
		}
		if _, err := tx.Exec(ctx, "rollback to savepoint probe"); err != nil {
			t.Fatal(err)
		}
	}
	_ = tx.Rollback(ctx)
	var stored int
	if err := database.QueryRow(ctx, `
		select (select count(*) from artifacts where role = 'decisions')
		     + (select count(*) from projects where 'decisions' = any(gated_steps))
		     + (select count(*) from design_gates where step = 'decisions')
	`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("%d rows hold decisions after this head's routes ran (%v), want none: no route stores it", stored, err)
	}

	// The document reader is gone from the api package, and Keep stands.
	declared := apiFunctions(t)
	if declared["issueDecisionsDocument"] {
		t.Fatal("the api package declares issueDecisionsDocument, the reader of the decisions document")
	}
	if !declared["keepDecision"] {
		t.Fatal("the api package declares no keepDecision")
	}
	t.Logf("the scan of internal/dispatch/api finds no issueDecisionsDocument and finds keepDecision")
}

// waitNoLiveProcess waits until no process of the database reads live.
func waitNoLiveProcess(t *testing.T, ctx context.Context, database *pgx.Conn) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		var live int
		if err := database.QueryRow(ctx, `select count(*) from server_processes where heartbeat_at > now() - interval '15 seconds'`).Scan(&live); err != nil {
			t.Fatalf("read the live processes: %v", err)
		}
		if live == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d processes still read live", live)
		}
	}
}

// apiFunctions is the name of each function and method the api package declares outside its tests.
func apiFunctions(t *testing.T) map[string]bool {
	t.Helper()
	packages, err := parser.ParseDir(token.NewFileSet(), filepath.Join("..", "..", "internal", "dispatch", "api"),
		func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatalf("parse the api package: %v", err)
	}
	declared := map[string]bool{}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				if function, ok := decl.(*ast.FuncDecl); ok {
					declared[function.Name.Name] = true
				}
			}
		}
	}
	return declared
}
