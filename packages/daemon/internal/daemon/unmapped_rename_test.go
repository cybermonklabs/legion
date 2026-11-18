package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// An issue whose repository no mapping names, spelled as the repository was called before GitHub
// renamed it, is recorded on the name GitHub gives it now and admitted. GitHub answers the old
// name with a redirect to the new one, so both role Apps answer installed and the repository's
// read names it under its new name: the record carries GitHub's spelling, as for any repository
// no mapping names (R4). The preparation keeps GitHub's answer under the name GitHub gave, and the
// resolution of the issue's own name must find it there; otherwise the issue is left unread at
// every preparation, with nothing said on it and nothing recorded, for ever, while GitHub has
// answered every read it was asked.
func TestAnIssueOnARenamedRepositoryNoMappingNamesIsRecordedOnGitHubsNameForIt(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{
		"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main",
		"stranger/old-tools": "301 stranger/tools", unmappedRepo: unmappedNamed + "@trunk",
	})
	w, client, _, _ := mappingRigWith(t, &installations{}, func(w *workflowRuntime) {
		w.defaultBranches = newDefaultBranches(roleTokens{}, w.repositories, github.url, defaultBranchTTL, time.Now)
	})
	engine, admission := rigHandlers(w)
	issue, fact := handedOver("LEGION-921", "stranger/old-tools", 1)
	client.issue = issue

	prepared, err := prepareAsDelivered(t, w, fact)
	if err != nil {
		t.Fatalf("prepare LEGION-921 on stranger/old-tools = %v, want it prepared on what GitHub answers", err)
	}
	if prepared.RepositoryRefusal != "" {
		t.Fatalf("the prepared event refuses %q, want it recorded: both Apps are installed", prepared.RepositoryRefusal)
	}
	if _, err := intake.ApplyFact(ctx, w.pool, "test", "todo-921", prepared, engine, admission); err != nil {
		t.Fatal(err)
	}
	got := recordedIssue(t, w, "LEGION-921")
	if got == nil || got.Repo != unmappedNamed || got.GitHubRepo != unmappedNamed || got.DefaultBranch != "trunk" {
		t.Fatalf("LEGION-921 = %+v (unread: %q, notes: %q), want it recorded on %s@trunk, the name GitHub gives the repository now",
			got, w.unreadReasons(), repositoryNotes(t, w, "LEGION-921"), unmappedNamed)
	}
	if got.Phase != phase.Admitted {
		t.Fatalf("LEGION-921 is in phase %s, want it admitted", got.Phase)
	}
}
