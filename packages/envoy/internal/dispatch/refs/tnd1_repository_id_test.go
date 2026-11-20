package refs

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// The GitHub ids of tnd1's repositories: PRIV's acme/privrenamed, which GitHub renamed to
// acme/renamednow, and PRIV's acme/privmoved, which GitHub moved to other/privmoved.
const (
	renamedID int64 = 7007
	movedID   int64 = 7008
)

// seedRenamedRepositories adds to seedRepositoryIssues' rows PRIV-7, naming acme/privrenamed, and
// PRIV-8, naming acme/privmoved, each recorded with the GitHub id GitHub gives it.
func seedRenamedRepositories(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PRIV-7', 'PRIV', 7, 'Names acme/privrenamed', '{"kind":"user","id":"alice"}', 'g', null, 'acme/privrenamed', 'triage'),
			('PRIV-8', 'PRIV', 8, 'Names acme/privmoved', '{"kind":"user","id":"alice"}', 'h', null, 'acme/privmoved', 'done')
	`); err != nil {
		t.Fatalf("seed PRIV-7 and PRIV-8: %v", err)
	}
	for repository, id := range map[string]int64{"acme/privrenamed": renamedID, "acme/privmoved": movedID} {
		if err := RecordRepositoryID(context.Background(), database.Pool, repository, id); err != nil {
			t.Fatal(err)
		}
	}
}

// passesWithID answers RepositoryClause for who over repository whose GitHub id is id, nil being
// an id that could not be read.
func passesWithID(t *testing.T, database *store.Store, who model.Actor, repository string, id *int64) bool {
	t.Helper()
	var passes bool
	if err := database.Pool.QueryRow(context.Background(), `select `+RepositoryClause("$2::text", "$3::bigint", "$1"),
		VisibilityArg(who), repository, id).Scan(&passes); err != nil {
		t.Fatalf("RepositoryClause(%s, %v): %v", repository, id, err)
	}
	return passes
}

// TestRepositoryClauseHoldsARenamedOrMovedRepositoryToItsGitHubID: a private project's repository
// is hidden from an open viewer who does not see the project under any name GitHub gives it, since
// GitHub keeps its id. PRIV-7 names acme/privrenamed and PRIV-8, done, names acme/privmoved; GitHub
// renamed the first to acme/renamednow and moved the second to other/privmoved, each keeping its
// id. alice, open and seeing PUB, is refused both new names, and reads a repository of either name
// with an id no project's repository has; carol, who sees PRIV, reads both new names. A repository
// whose id could not be read is refused to alice unless an issue she sees resolves to it, as
// acme/pub does, and a name a private project owns whose id is not known refuses alice every
// repository no issue she sees resolves to, until it is learned.
//
// Control (the name rule): RepositoryClause without its id arm (`learned.github_id = <id>`) shows
// alice both new names, and the test fails naming each.
func TestRepositoryClauseHoldsARenamedOrMovedRepositoryToItsGitHubID(t *testing.T) {
	database := seedRepositoryIssues(t)
	seedRenamedRepositories(t, database)
	alice := model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}}
	carol := model.Actor{Viewer: &model.Viewer{Person: "carol", Open: true, Sees: []string{"PRIV", "PRIV2", "PUB"}, AnyPrivate: true}}
	unrelated := int64(9999)
	for _, row := range []struct {
		name, repository string
		id               int64
	}{
		{"PRIV's acme/privrenamed under the name GitHub renamed it to", "acme/renamednow", renamedID},
		{"PRIV's acme/privmoved under the owner GitHub moved it to", "other/privmoved", movedID},
	} {
		if passesWithID(t, database, alice, row.repository, &row.id) {
			t.Errorf("%s: RepositoryClause(%s, id %d) for alice = true, want false: PRIV, which she does not see, owns id %d", row.name, row.repository, row.id, row.id)
		}
		if !passesWithID(t, database, carol, row.repository, &row.id) {
			t.Errorf("%s: RepositoryClause(%s, id %d) for carol = false, want true: she sees PRIV", row.name, row.repository, row.id)
		}
		if !passesWithID(t, database, alice, row.repository, &unrelated) {
			t.Errorf("%s: RepositoryClause(%s, id %d) for alice = false, want true: no project's repository has that id, so the name alone hides nothing",
				row.name, row.repository, unrelated)
		}
	}
	if passesWithID(t, database, alice, "acme/none", nil) {
		t.Errorf("RepositoryClause(acme/none, no id) for alice = true, want false: while a project is private a repository whose id is not known is hidden")
	}
	if !passesWithID(t, database, alice, "acme/pub", nil) {
		t.Errorf("RepositoryClause(acme/pub, no id) for alice = false, want true: PUB-1, which she sees, resolves to it")
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PRIV-9', 'PRIV', 9, 'Names acme/unlearned', '{"kind":"user","id":"alice"}', 'i', null, 'acme/unlearned', 'triage')
	`); err != nil {
		t.Fatalf("seed PRIV-9: %v", err)
	}
	if passesWithID(t, database, alice, "acme/none", &unrelated) {
		t.Errorf("RepositoryClause(acme/none) for alice = true while PRIV's acme/unlearned has no known id, want false: it could be acme/unlearned")
	}
	if !passesWithID(t, database, alice, "acme/pub", &unrelated) {
		t.Errorf("RepositoryClause(acme/pub) for alice = false while PRIV's acme/unlearned has no known id, want true: PUB-1 resolves to it")
	}
	if err := recordRepositoryID(context.Background(), database.Pool, "acme/unlearned", nil); err != nil {
		t.Fatal(err)
	}
	if passesWithID(t, database, alice, "acme/none", &unrelated) {
		t.Errorf("RepositoryClause(acme/none) for alice = true where GitHub answered that the App reads nothing under acme/unlearned, want false")
	}
	if err := RecordRepositoryID(context.Background(), database.Pool, "ACME/Unlearned", 7009); err != nil {
		t.Fatal(err)
	}
	if !passesWithID(t, database, alice, "acme/none", &unrelated) {
		t.Errorf("RepositoryClause(acme/none) for alice = false once every private repository's id is known, want true")
	}
}

// TestRepositoryIDNeededClauseSaysWhenTheAnswerTurnsOnTheID: the id is needed only for an open
// viewer, while a project is private, of a repository no issue on either side resolves to.
func TestRepositoryIDNeededClauseSaysWhenTheAnswerTurnsOnTheID(t *testing.T) {
	database := seedRepositoryIssues(t)
	alice := model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}}
	bob := model.Actor{Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}}
	public := model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PRIV", "PRIV2", "PUB"}}}
	for _, row := range []struct {
		name       string
		who        model.Actor
		repository string
		needed     bool
	}{
		{"alice, of a repository no issue names", alice, "acme/newname", true},
		{"alice, of a repository a PUB issue names", alice, "acme/both", false},
		{"alice, of a repository only PRIV names", alice, "acme/priv", false},
		{"bob, a restricted member", bob, "acme/newname", false},
		{"an open viewer of a deployment with no private project", public, "acme/newname", false},
		{"the server's own viewer", model.Actor{Viewer: model.ServerViewer}, "acme/newname", false},
	} {
		var needed bool
		if err := database.Pool.QueryRow(context.Background(), `select `+RepositoryIDNeededClause("$2::text", "$1"),
			VisibilityArg(row.who), row.repository).Scan(&needed); err != nil {
			t.Fatalf("RepositoryIDNeededClause(%s): %v", row.repository, err)
		}
		if needed != row.needed {
			t.Errorf("%s: RepositoryIDNeededClause(%s) = %t, want %t", row.name, row.repository, needed, row.needed)
		}
	}
}

// identityGitHub is GitHub for LearnRepositoryIDs: each repository of answers installed and read
// as the repository it names (the name GitHub gives it now and its id), each of failing answering
// 500, and every other repository not installed. Every request is recorded.
type identityGitHub struct {
	answers map[string]githubapp.RepositoryAnswer
	failing []string
	mu      sync.Mutex
	asked   []string
}

func (g *identityGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.asked = append(g.asked, r.Method+" "+r.URL.Path)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/11/access_tokens" {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_ids","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		return
	}
	repository, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/installation")
	answer, known := g.answers[repository]
	switch {
	case slices.Contains(g.failing, repository):
		http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
	case !known:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	case path == "" && strings.HasSuffix(r.URL.Path, "/installation"):
		fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"metadata":"read"}}`)
	default:
		fmt.Fprintf(w, `{"full_name":%q,"id":%d}`, answer.FullName, answer.ID)
	}
}

func (g *identityGitHub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.asked...)
}

// identityClient is an App client whose GitHub is github.
func identityClient(t *testing.T, github *identityGitHub) *githubapp.Client {
	t.Helper()
	_, pem := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.ids", PEM: pem}, githubtest.Server(t, githubtest.Spec{Handle: github.serve}))
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client
}

// recordedIDs is every row of repository_ids, as the name and its id, "none" for no id.
func recordedIDs(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select repo, coalesce(github_id::text, 'none') from repository_ids`)
	if err != nil {
		t.Fatalf("read repository_ids: %v", err)
	}
	defer rows.Close()
	recorded := map[string]string{}
	for rows.Next() {
		var repository, id string
		if err := rows.Scan(&repository, &id); err != nil {
			t.Fatalf("read repository_ids: %v", err)
		}
		recorded[repository] = id
	}
	return recorded
}

// TestLearnRepositoryIDsRecordsWhatGitHubAnswersForEachPrivateRepository: LearnRepositoryIDs asks
// GitHub for the id of each name a private project's issue resolves to that repository_ids holds
// none for, records it with the name GitHub gives the repository now where that is another, records
// no id where GitHub answers that the App reads nothing there, and stops at any other failure,
// leaving that name owed. A name it holds an id for is not asked again, and a deployment with no
// private project asks GitHub nothing.
func TestLearnRepositoryIDsRecordsWhatGitHubAnswersForEachPrivateRepository(t *testing.T) {
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee, visibility) values ('PUB', 'Public', 'alice', 'public'), ('PRIV', 'Private', 'alice', 'private');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PUB-1', 'PUB', 1, 'Names acme/pub', '{"kind":"user","id":"alice"}', 'a', null, 'acme/pub', 'triage'),
			('PRIV-1', 'PRIV', 1, 'Names acme/privrenamed', '{"kind":"user","id":"alice"}', 'a', null, 'Acme/PrivRenamed', 'triage'),
			('PRIV-2', 'PRIV', 2, 'Names acme/gone', '{"kind":"user","id":"alice"}', 'b', null, 'acme/gone', 'done'),
			('PRIV-3', 'PRIV', 3, 'Names acme/zfailing', '{"kind":"user","id":"alice"}', 'c', null, 'acme/zfailing', 'triage');
	`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	github := &identityGitHub{
		answers: map[string]githubapp.RepositoryAnswer{"acme/privrenamed": {FullName: "acme/RenamedNow", ID: renamedID}},
		failing: []string{"acme/zfailing"},
	}
	client := identityClient(t, github)
	if err := LearnRepositoryIDs(context.Background(), database.Pool, client); err == nil || !strings.Contains(err.Error(), "acme/zfailing") {
		t.Errorf("LearnRepositoryIDs = %v, want the failure naming acme/zfailing", err)
	}
	want := map[string]string{"acme/privrenamed": "7007", "acme/renamednow": "7007", "acme/gone": "none"}
	if got := recordedIDs(t, database); !maps.Equal(got, want) {
		t.Errorf("repository_ids = %v, want %v: the id of each private repository, the name GitHub gives it now, none where the App reads nothing, and nothing for a failure", got, want)
	}
	asked := len(github.requests())
	github.failing = nil
	github.answers["acme/zfailing"] = githubapp.RepositoryAnswer{FullName: "acme/zfailing", ID: 7010}
	if err := LearnRepositoryIDs(context.Background(), database.Pool, client); err != nil {
		t.Fatalf("LearnRepositoryIDs again: %v", err)
	}
	for _, request := range github.requests()[asked:] {
		if strings.Contains(request, "privrenamed") || strings.Contains(request, "gone") {
			t.Errorf("the second LearnRepositoryIDs asked GitHub %q, want only the name still owed", request)
		}
	}
	if got := recordedIDs(t, database)["acme/zfailing"]; got != "7010" {
		t.Errorf("acme/zfailing's id after the second call = %q, want 7010", got)
	}

	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'public'; delete from repository_ids`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	asked = len(github.requests())
	if err := LearnRepositoryIDs(context.Background(), database.Pool, client); err != nil {
		t.Fatalf("LearnRepositoryIDs with no private project: %v", err)
	}
	if newly := github.requests()[asked:]; len(newly) != 0 {
		t.Errorf("with no private project LearnRepositoryIDs asked GitHub %q, want nothing (R9)", newly)
	}
}
