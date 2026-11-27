package api

import (
	"net/http"
	"slices"
	"testing"
)

// TestTheCrossProjectLegOffersNoAnsweredQuestionOfAProjectItsWriterDoesNotSee: the cross-project
// leg of a question's related-item check (relatedOtherProjects) reads the answered questions of the
// other projects as the writer whose check it is reads them (access.ProjectClause over that
// writer's viewer, before the statement's LIMIT), so a project the writer does not see offers it
// nothing in advice.suggestions.other_projects. carol made PUB and PRIV, and PRIV holds an
// answered question nearer the check's text than anything else; PRIV is made private by SQL, which
// leaves alice, who is on no list of PRIV's, seeing PUB alone. alice's POST
// /api/v1/projects/PUB/related offers no answered question of PRIV, and carol's, who sees PRIV,
// offers it: the leg ran, and the rule is what left it out of alice's.
//
// Control: relatedOtherProjects reading every other project's answered questions, as it did, under
// which alice is offered PRIV's question and the test fails.
func TestTheCrossProjectLegOffersNoAnsweredQuestionOfAProjectItsWriterDoesNotSee(t *testing.T) {
	t.Parallel()
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	seed := seedPublicProjectsWith(t, testServerOptions{embedder: embedder, relatedBound: meaningVerdictBound(t)})
	offered := seedAnsweredQuestion(t, seed.handler, seed.database, embedder, seed.issue[seedPrivate],
		"When the backlog grows, which store do we empty first?",
		"Empty the page store first; the index store costs more to rebuild.", 0.9, 2)
	refreshTermStats(t, seed.database, seedPublic)
	processAllPending(t, seed.database, embedder)
	if _, err := seed.database.Pool.Exec(t.Context(), `insert into session_projects_filled select now() where not exists (select 1 from session_projects_filled)`); err != nil {
		t.Fatalf("write the fill marker before making %s private: %v", seedPrivate, err)
	}
	if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'private' where key = $1`, seedPrivate); err != nil {
		t.Fatalf("make %s private: %v", seedPrivate, err)
	}

	related := func(caller privateCaller) []string {
		t.Helper()
		suggestions := crossRouteSuggestions(t, "POST related as "+string(caller), seed.request(t, caller, http.MethodPost,
			"/api/v1/projects/"+seedPublic+"/related", map[string]any{"title": crossProjectQuestion}))
		if suggestions.Search != relatedMeaningAndKeywords {
			t.Fatalf("POST related as %s = %+v, want it searched by meaning and keywords: a check that fell back to keywords runs no cross-project leg", caller, suggestions)
		}
		return crossProjectItemIDs(suggestions.OtherProjects)
	}
	if ids := related(callerCarol); !slices.Equal(ids, []string{offered}) {
		t.Fatalf("carol, who sees %s, was offered %v, want %s's answered question %s: the comparison proves nothing", seedPrivate, ids, seedPrivate, offered)
	}
	if ids := related(callerAlice); len(ids) != 0 {
		t.Errorf("alice, who does not see %s, was offered %v, want no answered question of %s", seedPrivate, ids, seedPrivate)
	}
}
