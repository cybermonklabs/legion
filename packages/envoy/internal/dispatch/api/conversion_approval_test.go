package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The spec a decisions document's conversion starts from: an answered ask, the person's
// answer given before the approval so that the answer's own carry cannot absorb the change; two
// requirement nodes the ask shaped; and a `## Decisions` part holding its pointer line to the
// issue's decisions document.
const (
	conversionAskWords = "Which store holds the corpus?"
	conversionAsk      = ":::ask{#a1 urgency=\"med\" multiple=\"false\"}\n" + conversionAskWords + "\n\n- Postgres: The one we run\n- Files: A directory\n:::\n"
	conversionSpec     = "# Plan\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n" +
		":::requirement{#r2}\nA reader names the database a reading came from.\n:::\n\n" + conversionAsk
)

// conversionFixture is one issue of project TEST whose spec, conversionSpec with its Decisions
// part, a person approved once its ask held her answer.
type conversionFixture struct {
	issueKey, specID, askID string
	approval                model.ArtifactReview
}

// approvedConversionSpec has the agent's session create an issue holding conversionSpec and give
// it a `## Decisions` part whose pointer line names the issue's decisions document, has alice
// answer the ask and approve the spec's latest version.
func approvedConversionSpec(t *testing.T, handler http.Handler, title string) conversionFixture {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": title, "spec": conversionSpec, "force": true, "actor": sessionActor(),
	})
	requireAgentWrite(t, "the agent's issue "+title, created, http.StatusCreated)
	issue := decodeBody[model.Issue](t, created)
	asks := readingAsksOpened(t, handler, issue.Key, map[string]string{"a1": conversionAskWords})
	agentEdit(t, handler, issue.PrimaryArtifactID, 2, map[string]string{
		"op": "insert", "after": "end",
		"markdown": "## Decisions\n\nThe decisions this spec rests on are in dispatch://" + issue.Key + "/artifact/decisions-md.",
	})
	answerDecisionBlock(t, handler, asks["a1"], "alice", []string{"Postgres"}, "Postgres, the database we back up.", nil)
	// The answer writes one version, version 3, the block's kept state alone (R3), and the person
	// approves the spec as that version leaves it.
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	approval := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	return conversionFixture{issueKey: issue.Key, specID: issue.PrimaryArtifactID, askID: asks["a1"], approval: approval}
}

// seedDecisionsDocument writes the issue's decisions.md below the routes, through the document
// service, as the release before stored one (the routes of this release refuse a new one): the
// document, role decisions, holding markdown as its version 1 written by the agent's session and
// indexed as an upload of a new document is.
func seedDecisionsDocument(t *testing.T, database *store.Store, service *docs.Service, issueKey, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
		select $1, project_key, 'decisions-md', 'decisions.md', 'doc', false, 'decisions', '{"kind":"session","id":"`+decidingSession+`"}'
		from issues where key = $1
		returning id::text
	`, issueKey).Scan(&id); err != nil {
		t.Fatalf("store the decisions document of %s: %v", issueKey, err)
	}
	session := model.Actor{Kind: "session", ID: decidingSession}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	written, err := service.SeedText(joined, id, markdown, session)
	if err != nil {
		t.Fatalf("seed the decisions document: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"session","id":"`+decidingSession+`"}]')
	`, id, written); err != nil {
		t.Fatalf("store the decisions document's version: %v", err)
	}
	_, indexed, err := service.IndexDocumentReferences(joined, id, 1, written)
	if err != nil {
		t.Fatalf("index the decisions document: %v", err)
	}
	if err := graphstore.Caused(ctx, tx, indexed, graphstore.NoEvent); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the decisions document: %v", err)
	}
	return id
}

// The conversion's one new version of an approved spec holds only what R9 gives it: the two
// `shaped` pointers on the answered ask's block, from the links of the section that restated it,
// the section's body as a reading inside the ask, and the decision another section became, in
// place of the pointer line. GET /api/v1/artifacts/{id} answers that version approved, carried from
// the person's approval by the reading case and the conversion's (conversionCase). The same
// pointers written by an agent's upload, which is no conversion, carry nothing by the conversion's
// case, and that upload supersedes the approval.
func TestTheConversionCarriesTheApprovalAcrossTheShapedPointersItGivesAnAsk(t *testing.T) {
	handler, database, service := settlingCarryDocuments(t, 20*time.Millisecond)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "TEST project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	converted := approvedConversionSpec(t, handler, "The conversion's spec")
	seedDecisionsDocument(t, database, service, converted.issueKey, "# Decisions\n\n"+
		"## Which store holds the corpus\n\nThe person chose Postgres, and we read it as the database we already back up. "+
		"This shapes dispatch://"+converted.issueKey+"/spec#b-r1 and dispatch://"+converted.issueKey+"/spec#b-r2. "+
		"Decided in dispatch://"+converted.issueKey+"/ask/"+converted.askID+".\n\n"+
		"## The census runs first\n\nThe census reads the corpus before any view of it.\n")
	step := docs.NewDecisionStep(database, "", nil)
	step.UseDocuments(service)
	done, failed, err := step.RestoreAndConvert(context.Background(), "TEST", true, false)
	if err != nil || len(failed) > 0 || len(done) != 1 {
		t.Fatalf("the conversion answered %+v, %+v, %v, want the one issue converted", done, failed, err)
	}
	if done[0].SpecVersion != converted.approval.Version+1 || done[0].Readings != 1 || done[0].Decisions != 1 || !done[0].Retired {
		t.Fatalf("the conversion did %+v, want one version of the spec, one reading, one decision and the document retired", done[0])
	}
	stored := versionMarkdown(t, handler, converted.specID, done[0].SpecVersion)
	pointers := `shaped="dispatch://` + converted.issueKey + `/spec#b-r1 dispatch://` + converted.issueKey + `/spec#b-r2"`
	for _, want := range []string{pointers, ":::reading{", "we read it as the database we already back up", ":::decision{", "The census reads the corpus"} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the converted version reads\n%s\nwant it holding %q", stored, want)
		}
	}
	if strings.Contains(stored, "artifact/decisions-md") {
		t.Fatalf("the converted version still holds the pointer line:\n%s", stored)
	}
	got := readApproval(t, handler, converted.specID)
	if got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil ||
		*got.Approval.Version != got.Approval.LatestVersion || got.Approval.LatestVersion != done[0].SpecVersion {
		t.Fatalf("GET /api/v1/artifacts/%s answers %+v, want the conversion's version %d approved, carried from the person's approval of version %d",
			converted.specID, got.Approval, done[0].SpecVersion, converted.approval.Version)
	}
	carried := documentReviews(t, handler, converted.specID)[0]
	want := fmt.Sprintf("Carried forward from the approval of version %d: ", converted.approval.Version)
	if carried.Reason == nil || !strings.HasPrefix(*carried.Reason, want) || !strings.Contains(*carried.Reason, "the spec nodes its answered asks shaped") {
		t.Fatalf("the carried review is %+v, want its reason opening %q and naming the shaped pointers", carried, want)
	}
	t.Logf("the conversion's version %d reads approved, carried from version %d: %s", done[0].SpecVersion, converted.approval.Version, *carried.Reason)

	uploaded := approvedConversionSpec(t, handler, "The agent's upload of the pointers")
	markdown := documentMarkdown(t, handler, uploaded.specID)
	opener := `:::ask{#a1 urgency="med" multiple="false"`
	if !strings.Contains(markdown, opener) {
		t.Fatalf("the approved spec reads\n%s\nwant its ask opening %q", markdown, opener)
	}
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+uploaded.issueKey+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec.md",
		"content":      strings.Replace(markdown, opener, opener+` shaped="dispatch://`+uploaded.issueKey+`/spec#b-r1 dispatch://`+uploaded.issueKey+`/spec#b-r2"`, 1),
		"precondition": uploadBase(t, handler, uploaded.specID),
	})
	advice := adviceFromResponse(t, written.Code, written.Body.String())
	waitForArtifactVersion(t, handler, uploaded.specID, uploaded.approval.Version+1)
	requireSupersededByTheWrite(t, handler, readingFixture{issueKey: uploaded.issueKey, artifactID: uploaded.specID, approval: uploaded.approval}, advice,
		"an agent's upload of the shaped pointers alone")
	t.Logf("an agent's upload of the same pointers supersedes the person's approval of version %d", uploaded.approval.Version)
}
