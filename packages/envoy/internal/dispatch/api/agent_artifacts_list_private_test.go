package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// hiddenFilesSession is a session of carol's that wrote in PRIV (a session_projects pair) and that
// her credential proved (session_owners): alice, who does not see PRIV, may not read its
// conversation, and carol, who sees PRIV, may (access.SessionContentClause).
const hiddenFilesSession = "session-hidden-file-list"

// TestAConversationsFileListAnswersOnlyWhatItsReadBySlugAnswers (F14): GET
// /api/v1/agents/{session_id}/artifacts lists a file of the conversation only where the read by
// slug, GET /api/v1/agents/{session_id}/artifacts/{slug}, answers it to the same caller, so a
// caller who may not read the conversation lists none of its files, as a conversation that holds
// none. carol uploads a file to hiddenFilesSession's conversation; for each seeded caller the list
// admits (the conversation read's callers, TestAgentFileListAdmitsExactlyTheCallersTheConversationReadAdmits),
// the list names the file exactly when that caller's read by slug answers 200: alice's read
// answers 404, as a file no row holds, and her list is empty; carol reads the file and lists it.
// With no private project alice lists it again, as before (R9).
//
// Control: agentArtifactsSQL selecting by session_id alone, as HEAD c99245cbc6 has it
// (controls/F14.patch reversed): alice's list names the file her read by slug answers 404, and the
// test fails at the comparison row for alice and at the row "alice lists ... of a conversation she
// may not read".
func TestAConversationsFileListAnswersOnlyWhatItsReadBySlugAnswers(t *testing.T) {
	seed := seedPrivateProject(t)
	ctx := context.Background()
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ($1, $2)`,
		hiddenFilesSession, seedPrivate); err != nil {
		t.Fatalf("place %s in PRIV: %v", hiddenFilesSession, err)
	}
	if _, err := seed.database.Pool.Exec(ctx, `insert into session_owners (session_id, login) values ($1, $2)`,
		hiddenFilesSession, seedCarol); err != nil {
		t.Fatalf("tie %s to carol: %v", hiddenFilesSession, err)
	}
	uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+hiddenFilesSession+"/artifacts",
		map[string]string{"name": "private-build.png"}, "private-build.png", "image/png", pngBytes("a shot of PRIV's build"), seedCarol)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("carol uploads to %s's conversation: %d %s", hiddenFilesSession, uploaded.Code, uploaded.Body.String())
	}
	slug := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.Slug
	if slug == "" {
		t.Fatalf("the upload answered no slug: %s", uploaded.Body.String())
	}

	// listed is caller's list of the conversation's slugs, and the list's status; read is the
	// status of caller's read of the file by its slug.
	listed := func(caller privateCaller) (int, []string) {
		t.Helper()
		response := seed.request(t, caller, http.MethodGet, "/api/v1/agents/"+hiddenFilesSession+"/artifacts", nil)
		if response.Code != http.StatusOK {
			return response.Code, nil
		}
		slugs := []string{}
		for _, artifact := range decodeBody[[]model.Artifact](t, response) {
			slugs = append(slugs, artifact.Slug)
		}
		return response.Code, slugs
	}
	read := func(caller privateCaller) int {
		t.Helper()
		return seed.request(t, caller, http.MethodGet, "/api/v1/agents/"+hiddenFilesSession+"/artifacts/"+slug, nil).Code
	}

	compared := 0
	for _, caller := range privateCallers {
		status, slugs := listed(caller)
		if status != http.StatusOK {
			continue
		}
		compared++
		want := []string{}
		if got := read(caller); got == http.StatusOK {
			want = []string{slug}
		}
		if !slices.Equal(slugs, want) {
			t.Errorf("%s lists %v of %s's conversation, want %v, what its read by slug answers", caller, slugs, hiddenFilesSession, want)
		}
	}
	if compared == 0 {
		t.Fatal("no seeded caller's list answered 200, so the comparison compared nothing")
	}
	// The two rows the comparison rests on, so it cannot pass with every read refused or every
	// list empty.
	if got := read(callerAlice); got != http.StatusNotFound {
		t.Errorf("alice's read of the file by slug = %d, want 404: she may not read %s's conversation", got, hiddenFilesSession)
	}
	if _, slugs := listed(callerAlice); len(slugs) != 0 {
		t.Errorf("alice lists %v of a conversation she may not read, want []", slugs)
	}
	if _, slugs := listed(callerCarol); !slices.Equal(slugs, []string{slug}) {
		t.Errorf("carol lists %v, want [%s]: she sees PRIV and proved the session", slugs, slug)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if _, slugs := listed(callerAlice); !slices.Equal(slugs, []string{slug}) {
		t.Errorf("with no project private alice lists %v, want [%s], as before (R9)", slugs, slug)
	}
}
