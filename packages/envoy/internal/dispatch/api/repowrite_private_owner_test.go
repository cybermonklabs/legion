package api

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// repositoryNotAvailableRefusal is the whole answer to a write that makes a repository the GitHub
// proxy hides from the writer an issue's own or a project's default (refuseRepoHidden).
const repositoryNotAvailableRefusal = `{"code":"REPOSITORY_NOT_AVAILABLE","error":"this repository is not available to the caller: an issue's repo or a project's default repository must be one the caller can read through Dispatch"}`

// repoWriteSeed is the private seed seedRepoWrite writes, with the keys of the two issues that
// share acme/both, one of PUB and one of PRIV.
type repoWriteSeed struct {
	privateSeed
	sharedPublic, sharedPrivate string
}

// seedRepoWrite is the private seed with a repository PRIV owns for each way an issue makes one its
// project's, and one PRIV shares with PUB, each written by carol, who sees both, through the
// routes: PRIV-1 names acme/priv; PRIV-2 names acme/privdone and is done; PRIV-3 names none and
// resolves to PRIV's default, acme/privdefault; PUB-2 and PRIV-4 both name acme/both. The server
// settles no document while a test runs, so no write but the test's own adds a row the test counts.
func seedRepoWrite(t *testing.T, options testServerOptions) repoWriteSeed {
	t.Helper()
	options.settle = time.Hour
	seed := repoWriteSeed{privateSeed: seedPrivateProjectWith(t, options)}
	private := "/api/v1/issues/" + seed.issue[seedPrivate]
	seed.mustRequest(t, callerCarol, http.MethodPatch, private, map[string]any{"repo": "acme/priv"}, http.StatusOK)
	done := decodeBody[model.Issue](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPrivate, "title": "Finished in its own repository", "repo": "acme/privdone", "force": true}, http.StatusCreated))
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+done.Key, map[string]any{"status": "done"}, http.StatusOK)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/default-repository",
		map[string]any{"repo": "acme/privdefault"}, http.StatusOK)
	inherits := decodeBody[model.Issue](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPrivate, "title": "Works in its project's default", "force": true}, http.StatusCreated))
	if repoOf(inherits.ResolvedRepo) != "acme/privdefault" {
		t.Fatalf("%s resolves to %s, want PRIV's default acme/privdefault", inherits.Key, repoOf(inherits.ResolvedRepo))
	}
	for _, shared := range []struct {
		project string
		key     *string
	}{{seedPublic, &seed.sharedPublic}, {seedPrivate, &seed.sharedPrivate}} {
		*shared.key = decodeBody[model.Issue](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": shared.project, "title": "Works in the shared repository of " + shared.project, "repo": "acme/both", "force": true},
			http.StatusCreated)).Key
	}
	return seed
}

// sendAs makes one write as caller, the body naming the session a token acts for, as a token's
// write must.
func (s privateSeed) sendAs(t *testing.T, caller privateCaller, method, target string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if caller.login() == "" {
		withActor := map[string]any{"actor": sessionActor()}
		maps.Copy(withActor, body)
		body = withActor
	}
	return s.request(t, caller, method, target, body)
}

// repoWriteSnapshot is issueWritesSnapshot with every project's default repository: everything a
// refused issue or default-repository write could have written.
func repoWriteSnapshot(t *testing.T, database *store.Store) string {
	t.Helper()
	var defaults string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(string_agg(concat_ws(',', key, coalesce(default_repo, '-')), '; ' order by key), '') from projects
	`).Scan(&defaults); err != nil {
		t.Fatalf("read the projects' default repositories: %v", err)
	}
	return issueWritesSnapshot(t, database) + " | " + defaults
}

// namedIn answers the first of names an answer's body holds as a whole word, "" for none.
func namedIn(body string, names ...string) string {
	for _, name := range names {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(body) {
			return name
		}
	}
	return ""
}

// repoWrite is one write that names a repository as an issue's own or a project's default.
type repoWrite struct {
	name, method, target string
	body                 map[string]any
}

// repoWritesOf are the three writes that give PUB repository: a create of a PUB issue naming it, a
// patch of PUB's first issue naming it, and a PUT that makes it PUB's default repository.
func (s repoWriteSeed) repoWritesOf(repository string) []repoWrite {
	return []repoWrite{
		{"create a PUB issue", http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPublic, "title": "Works in " + repository, "repo": repository, "force": true}},
		{"patch " + s.issue[seedPublic], http.MethodPatch, "/api/v1/issues/" + s.issue[seedPublic], map[string]any{"repo": repository}},
		{"make it PUB's default", http.MethodPut, "/api/v1/projects/" + seedPublic + "/default-repository",
			map[string]any{"repo": repository}},
	}
}

// TestAnIssueWriteCannotTakeAHiddenPrivateRepository: alice, a full member who sees PUB and not
// private PRIV, and carol's token that lists no project, which sees PUB alone, cannot make a
// repository the GitHub proxy hides from them PUB's: a create of a PUB issue naming it, a patch of
// PUB-1 naming it and a PUT that makes it PUB's default repository are each 403
// REPOSITORY_NOT_AVAILABLE, the same bytes for every spelling and every way PRIV alone owns it (an
// issue's own repository, a done issue's, PRIV's default), naming no project and no issue, and
// nothing is written: no issue, no repository, no event, no project's default. The proxy still
// hides each one from alice.
//
// Control (default-repository write): refuseRepoHidden taken out of putProjectDefaultRepository,
// under which alice's PUT making acme/priv PUB's default answers 200 and writes it.
//
// The proxy's gate with the writer as its viewer is the one test, so a repository a PUB issue
// already resolves to is open to the write though PRIV owns it too: alice's three writes naming
// acme/both, which PUB-2 and PRIV-4 both name, are each taken. So are hers naming acme/pub, which
// no project owns, and clearing PUB-1's repository; and carol, who sees PRIV, makes the three
// writes naming PRIV's repositories.
func TestAnIssueWriteCannotTakeAHiddenPrivateRepository(t *testing.T) {
	seed := seedRepoWrite(t, testServerOptions{})
	public := "/api/v1/issues/" + seed.issue[seedPublic]
	keys := []string{seedPublic, seedPrivate, seed.issue[seedPublic], seed.issue[seedPrivate], seed.sharedPublic, seed.sharedPrivate}
	held := []string{"acme/priv", "ACME/Priv", "acme/privdone", "acme/privdefault"}
	for _, caller := range []privateCaller{callerAlice, callerCarolToken} {
		for _, repository := range held {
			for _, write := range seed.repoWritesOf(repository) {
				before := repoWriteSnapshot(t, seed.database)
				response := seed.sendAs(t, caller, write.method, write.target, write.body)
				got := strings.TrimSpace(response.Body.String())
				if response.Code != http.StatusForbidden || got != repositoryNotAvailableRefusal {
					t.Errorf("%s: %s naming %s = %d %s, want 403 %s", caller, write.name, repository, response.Code, got, repositoryNotAvailableRefusal)
				}
				if named := namedIn(got, keys...); named != "" {
					t.Errorf("%s: the answer to %s naming %s names %s: %s", caller, write.name, repository, named, got)
				}
				if after := repoWriteSnapshot(t, seed.database); after != before {
					t.Errorf("%s: the refused %s naming %s wrote:\nbefore %s\nafter  %s", caller, write.name, repository, before, after)
				}
			}
		}
	}
	for _, repository := range held {
		if proxyShows(t, seed.database, seedAlice, repository) {
			t.Errorf("the GitHub proxy shows alice %s, which only PRIV owns", repository)
		}
	}
	// The instrument's own control: the same read shows carol, who sees PRIV, what it hides from
	// alice, so the false above is the clause's answer, not a read that shows nothing.
	if !proxyShows(t, seed.database, seedCarol, "acme/priv") {
		t.Fatalf("the GitHub proxy hides acme/priv from carol, who sees PRIV")
	}

	// A repository an issue alice sees already resolves to is hers to name, though PRIV owns it
	// too; so is one no project owns, and clearing one is never refused.
	for _, repository := range []string{"acme/both", "acme/pub"} {
		for _, write := range seed.repoWritesOf(repository) {
			response := seed.request(t, callerAlice, write.method, write.target, write.body)
			wrote := ""
			switch write.method {
			case http.MethodPut:
				if response.Code == http.StatusOK {
					stored := decodeBody[projectDefaultRepository](t, response)
					if stored.Project == seedPublic && stored.Repo != nil {
						wrote = *stored.Repo
					}
				}
			default:
				if response.Code == http.StatusOK || response.Code == http.StatusCreated {
					wrote = repoOf(decodeBody[model.Issue](t, response).Repo)
				}
			}
			if wrote != repository {
				t.Errorf("alice: %s naming %s = %d %s, want it written", write.name, repository, response.Code, response.Body.String())
			}
		}
	}
	if cleared := seed.request(t, callerAlice, http.MethodPatch, public, map[string]any{"repo": nil}); cleared.Code != http.StatusOK ||
		repoOf(decodeBody[model.Issue](t, cleared).Repo) != "<none>" {
		t.Errorf("alice clears %s's repo = %d %s, want 200 with none", seed.issue[seedPublic], cleared.Code, cleared.Body.String())
	}

	// carol sees PRIV, so a repository it owns is hers to give PUB too.
	created := seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPublic, "title": "Shares the private repository", "repo": "ACME/Priv", "force": true})
	if created.Code != http.StatusCreated || repoOf(decodeBody[model.Issue](t, created).Repo) != "acme/priv" {
		t.Errorf("carol creates a PUB issue naming ACME/Priv = %d %s, want 201 with acme/priv", created.Code, created.Body.String())
	}
	patched := seed.request(t, callerCarol, http.MethodPatch, public, map[string]any{"repo": "acme/privdone"})
	if patched.Code != http.StatusOK || repoOf(decodeBody[model.Issue](t, patched).Repo) != "acme/privdone" {
		t.Errorf("carol patches %s to acme/privdone = %d %s, want 200 with acme/privdone", seed.issue[seedPublic], patched.Code, patched.Body.String())
	}
	if set := seed.request(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/default-repository",
		map[string]any{"repo": "acme/privdefault"}); set.Code != http.StatusOK {
		t.Errorf("carol makes acme/privdefault PUB's default = %d %s, want 200", set.Code, set.Body.String())
	}
}

// TestAListingCredentialNamesOnlyARepositoryItReads: the gate is the proxy's, and a full member's
// token is an open viewer whatever it lists, since its list decides only which private projects it
// reads (access.CredentialCovers). So while PRIV is private, carol's token that lists PRIV names in
// PRIV every repository the proxy shows it: acme/privdone and acme/both, which PRIV issues resolve
// to, acme/fresh, which no issue names, and acme/pubonly, which only a PUB issue names; and names
// acme/pub in PUB, a public project its list does not name. alice's token that lists PRIV, which
// alice does not see, reads PUB alone, and is refused 403 REPOSITORY_NOT_AVAILABLE in PUB for
// acme/priv and acme/privdone, which only PRIV owns, with nothing written. bob's token that lists
// PRIV, a restricted member's, names no issue's repository at all (403 RESTRICTED_MEMBER), as bob
// does not.
func TestAListingCredentialNamesOnlyARepositoryItReads(t *testing.T) {
	seed := seedRepoWrite(t, testServerOptions{})
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": seedPublic, "title": "Works in a repository of PUB alone", "repo": "acme/pubonly", "force": true}, http.StatusCreated)
	const restrictedRefusal = `{"code":"RESTRICTED_MEMBER","error":"a restricted member cannot set or clear an issue's repo; only a full member names an issue's own repository"}`
	for _, write := range []struct {
		caller              privateCaller
		project, repository string
		status              int
		refusal             string
	}{
		{callerCarolPrivToken, seedPrivate, "acme/privdone", http.StatusCreated, ""},
		{callerCarolPrivToken, seedPrivate, "acme/both", http.StatusCreated, ""},
		{callerCarolPrivToken, seedPrivate, "acme/fresh", http.StatusCreated, ""},
		{callerCarolPrivToken, seedPrivate, "acme/pubonly", http.StatusCreated, ""},
		{callerCarolPrivToken, seedPublic, "acme/pub", http.StatusCreated, ""},
		{callerAlicePrivToken, seedPublic, "acme/priv", http.StatusForbidden, repositoryNotAvailableRefusal},
		{callerAlicePrivToken, seedPublic, "acme/privdone", http.StatusForbidden, repositoryNotAvailableRefusal},
		{callerBobPrivToken, seedPrivate, "acme/privdone", http.StatusForbidden, restrictedRefusal},
		{callerCarol, seedPrivate, "acme/fresh", http.StatusCreated, ""},
	} {
		before := repoWriteSnapshot(t, seed.database)
		response := seed.sendAs(t, write.caller, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": write.project, "title": "Works in " + write.repository, "repo": write.repository, "force": true})
		got := strings.TrimSpace(response.Body.String())
		switch {
		case response.Code != write.status:
			t.Errorf("%s creates a %s issue naming %s = %d %s, want %d", write.caller, write.project, write.repository, response.Code, got, write.status)
		case write.status == http.StatusForbidden && got != write.refusal:
			t.Errorf("%s creates a %s issue naming %s = %s, want %s", write.caller, write.project, write.repository, got, write.refusal)
		case write.status == http.StatusForbidden && repoWriteSnapshot(t, seed.database) != before:
			t.Errorf("%s's refused create naming %s wrote", write.caller, write.repository)
		}
	}
}

// TestARepositoryWriteAtAHiddenObjectAnswersAsAtAMissingOne: the repository check runs after the
// write reads its issue and its project as the writer reads them, so alice's patch of PRIV-1, her
// create in PRIV and her PUT of PRIV's default repository, each naming acme/priv, which PRIV owns,
// answer exactly as the same write at an issue and a project nobody holds, the key substituted,
// and write nothing: PRIV's default stays acme/privdefault and no event is appended.
//
// Control (default-repository write): putProjectDefaultRepository with no read of the project
// before its update, as main wrote it (the update first, then the project read), under which
// alice's PUT of PRIV's default is the repository's refusal rather than the missing project's.
func TestARepositoryWriteAtAHiddenObjectAnswersAsAtAMissingOne(t *testing.T) {
	seed := seedRepoWrite(t, testServerOptions{})
	hiddenIssue, missingIssue := seed.issue[seedPrivate], seedPrivate+"-999"
	for _, write := range []struct {
		name, method         string
		hidden, missing      string
		hiddenBody, nameless map[string]any
		hiddenKey, missKey   string
	}{
		{"patch", http.MethodPatch, "/api/v1/issues/" + hiddenIssue, "/api/v1/issues/" + missingIssue,
			map[string]any{"repo": "acme/priv"}, map[string]any{"repo": "acme/priv"}, hiddenIssue, missingIssue},
		{"create", http.MethodPost, "/api/v1/issues", "/api/v1/issues",
			map[string]any{"project": seedPrivate, "title": "Inside the private project", "repo": "acme/priv", "force": true},
			map[string]any{"project": "NOPE", "title": "Inside the private project", "repo": "acme/priv", "force": true}, seedPrivate, "NOPE"},
		{"default-repository", http.MethodPut, "/api/v1/projects/" + seedPrivate + "/default-repository", "/api/v1/projects/NOPE/default-repository",
			map[string]any{"repo": "acme/priv"}, map[string]any{"repo": "acme/priv"}, seedPrivate, "NOPE"},
	} {
		before := repoWriteSnapshot(t, seed.database)
		hidden := seed.request(t, callerAlice, write.method, write.hidden, write.hiddenBody)
		missing := seed.request(t, callerAlice, write.method, write.missing, write.nameless)
		hiddenBody := strings.ReplaceAll(strings.TrimSpace(hidden.Body.String()), write.hiddenKey, write.missKey)
		if hidden.Code != missing.Code || hiddenBody != strings.TrimSpace(missing.Body.String()) || hidden.Code == http.StatusForbidden {
			t.Errorf("alice's %s naming acme/priv at a hidden object = %d %s, at a missing one %d %s: want one answer, not the repository's refusal",
				write.name, hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
		}
		if after := repoWriteSnapshot(t, seed.database); after != before {
			t.Errorf("alice's refused %s wrote:\nbefore %s\nafter  %s", write.name, before, after)
		}
	}
}

// TestRepositoryWritesAnswerAsBeforeWithNoPrivateProject (R9): once PRIV is public again, the
// deployment holds no private project, and alice's create and patch naming acme/priv, her PUT that
// makes it PUB's default repository, and her token that lists PRIV creating and patching PRIV's
// issues with a repository, acme/fresh among them, which no issue names, are each taken and make
// no statement of the repository check (no statement names access.HiddenPrivateProjectClause):
// with no private project the check reads nothing. Each answer is logged masked (maskedAnswer), so
// two builds' runs compare byte for byte.
func TestRepositoryWritesAnswerAsBeforeWithNoPrivateProject(t *testing.T) {
	recorder := &statementRecorder{}
	seed := seedRepoWrite(t, testServerOptions{tracer: recorder})
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, write := range []struct {
		name         string
		caller       privateCaller
		method, path string
		body         map[string]any
		status       int
	}{
		{"alice creates a PUB issue naming acme/priv", callerAlice, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPublic, "title": "Works in the formerly private repository", "repo": "acme/priv", "force": true}, http.StatusCreated},
		{"alice patches PUB-1 to acme/priv", callerAlice, http.MethodPatch, "/api/v1/issues/" + seed.issue[seedPublic],
			map[string]any{"repo": "acme/priv"}, http.StatusOK},
		{"alice makes acme/priv PUB's default", callerAlice, http.MethodPut, "/api/v1/projects/" + seedPublic + "/default-repository",
			map[string]any{"repo": "acme/priv"}, http.StatusOK},
		{"alice's token that lists PRIV creates a PRIV issue naming acme/privdone", callerAlicePrivToken, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPrivate, "title": "Filed through a listing token", "repo": "acme/privdone", "force": true}, http.StatusCreated},
		{"alice's token that lists PRIV patches PRIV-1 to acme/privdefault", callerAlicePrivToken, http.MethodPatch, "/api/v1/issues/" + seed.issue[seedPrivate],
			map[string]any{"repo": "acme/privdefault"}, http.StatusOK},
		{"alice's token that lists PRIV creates a PRIV issue naming acme/fresh", callerAlicePrivToken, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": seedPrivate, "title": "Filed in a repository no issue names", "repo": "acme/fresh", "force": true}, http.StatusCreated},
	} {
		var response *httptest.ResponseRecorder
		statements := recorder.record(func() {
			body := write.body
			if write.caller.login() == "" {
				body = map[string]any{"actor": sessionActor()}
				maps.Copy(body, write.body)
			}
			response = seed.requestIn(t, context.WithValue(context.Background(), recordingKey{}, true), write.caller, write.method, write.path, body)
		})
		t.Logf("R9 %s: %s", write.name, maskedAnswer(response))
		if response.Code != write.status {
			t.Errorf("%s = %d %s, want %d", write.name, response.Code, response.Body.String(), write.status)
		}
		if len(statements) == 0 {
			t.Fatalf("%s recorded no statement, so the record cannot show the check's", write.name)
		}
		for _, statement := range statements {
			if strings.Contains(statement.SQL, access.Marker+"HiddenPrivateProjectClause") {
				t.Errorf("%s, with no private project, made a statement of the repository check:\n%s", write.name, oneLine(statement.SQL))
			}
		}
	}
}
