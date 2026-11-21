package refs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// repositoryHiddenAndShown answers RepositoryHidden and RepositoryClause for who over repository in
// one statement, each as a pointer so a null answer is seen; repository nil binds a null name.
func repositoryHiddenAndShown(t *testing.T, database *store.Store, who model.Actor, repository *string) (hidden, shown *bool) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `select `+RepositoryHidden("$2::text", "$1")+`, `+RepositoryClause("$2::text", "$1"),
		VisibilityArg(who), repository).Scan(&hidden, &shown); err != nil {
		t.Fatalf("RepositoryHidden(%v): %v", repository, err)
	}
	return hidden, shown
}

// TestRepositoryHiddenIsTheProxyGateNegated: RepositoryHidden, the helper a repository write holds
// its writer to, answers exactly the negation of RepositoryClause, the GitHub proxy's gate, never
// null, for every viewer the clause's rows name and every repository of seedRepositoryIssues' and
// seedHiddenRepositories' rows, in any case and for no name at all. Its answers are pinned too, so
// the equality cannot pass on two blind answers: it hides acme/priv from alice, who sees PUB alone,
// and not acme/both, which PUB-2 shares with PRIV-5, nor acme/none, which no project owns; it hides
// acme/none from bob, a restricted member, and from a viewer that lists PRIV and is not open, which
// read only what an issue of their projects resolves to; and it hides nothing from carol, from the server's
// own viewer, or from alice in a deployment with no private project.
func TestRepositoryHiddenIsTheProxyGateNegated(t *testing.T) {
	database := seedRepositoryIssues(t)
	seedHiddenRepositories(t, database)
	viewers := map[string]model.Actor{
		"alice":                           {Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
		"bob":                             {Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}},
		"carol":                           {Viewer: &model.Viewer{Person: "carol", Open: true, Sees: []string{"PRIV", "PRIV2", "PUB"}, AnyPrivate: true}},
		"a viewer listing PRIV, not open": {Viewer: &model.Viewer{Person: "carol", Sees: []string{"PRIV"}, AnyPrivate: true, Lists: []string{"PRIV"}}},
		"alice with no private project":   {Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}}},
		"the server's own viewer":         {Viewer: model.ServerViewer},
		"the closed viewer":               {},
	}
	repositories := append([]string{"ACME/Priv", "acme/privmapped", "acme/privonlymapped", "acme/pubwalk", "acme/movedold", "acme/movednew"}, clauseRepositories...)
	for name, who := range viewers {
		for _, repository := range repositories {
			hidden, shown := repositoryHiddenAndShown(t, database, who, &repository)
			if hidden == nil || shown == nil || *hidden == *shown {
				t.Errorf("for %s over %s: RepositoryHidden = %v, RepositoryClause = %v; want one the other's negation, neither null",
					name, repository, answerOf(hidden), answerOf(shown))
			}
		}
		if hidden, _ := repositoryHiddenAndShown(t, database, who, nil); hidden == nil {
			t.Errorf("for %s over no name: RepositoryHidden is null, want true or false", name)
		}
	}
	for _, pinned := range []struct {
		viewer, repository string
		hidden             bool
	}{
		{"alice", "acme/priv", true},
		{"alice", "ACME/Priv", true},
		{"alice", "acme/privdefault", true},
		{"alice", "acme/both", false},
		{"alice", "acme/none", false},
		{"bob", "acme/none", true},
		{"bob", "acme/priv", false},
		{"a viewer listing PRIV, not open", "acme/none", true},
		{"a viewer listing PRIV, not open", "acme/both", false},
		{"carol", "acme/priv", false},
		{"the server's own viewer", "acme/priv", false},
		{"alice with no private project", "acme/priv", false},
		{"the closed viewer", "acme/pub", true},
	} {
		repository := pinned.repository
		if hidden, _ := repositoryHiddenAndShown(t, database, viewers[pinned.viewer], &repository); hidden == nil || *hidden != pinned.hidden {
			t.Errorf("RepositoryHidden(%s) for %s = %v, want %t", pinned.repository, pinned.viewer, answerOf(hidden), pinned.hidden)
		}
	}
}

// answerOf prints an answer that may be null.
func answerOf(answer *bool) any {
	if answer == nil {
		return "null"
	}
	return *answer
}
