package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The words of the searchable objects writeSearchableObjects writes: every object holds
// searchAccessWord, which the keyword leg finds, and the fake embedder places every object beside
// searchAccessMeaning, a query that shares no word with any of them, which only the meaning leg
// finds.
const (
	searchAccessWord    = "astrolabe"
	searchAccessMeaning = "wibbleflorp"
)

// searchNobodysProject is a project key no row holds: what a hidden project must answer as.
const searchNobodysProject = "NOPE"

// writeSearchableObjects writes, as carol and through the routes, one object of each kind search
// ranks in each project of the seed - an issue with a spec (the issue and its document), a comment,
// an ask and a message - each holding searchAccessWord, and places each one's vector beside
// searchAccessMeaning's. It answers each project's objects as the "kind/id" a hit names them by.
// Every object's number is the project's second: the seed made each project's first issue.
func writeSearchableObjects(t *testing.T, seed privateSeed, embedder *fakeEmbedder) map[string][]string {
	t.Helper()
	near := angledVector(0.9, 2)
	title := func(project string) string { return "Astrolabe survey of " + project }
	spec := func(project string) string { return "Astrolabe calibration notes for " + project + "." }
	// Every vector a write's own related-item check could read is placed before the first write,
	// so no check reads the map while the test writes it.
	for _, project := range []string{seedPublic, seedPrivate} {
		embedder.vectors[issueEmbeddingText(title(project), spec(project))] = near
	}
	type windowed struct{ kind, owner string }
	var windows []windowed
	objects := map[string][]string{}
	for _, project := range []string{seedPublic, seedPrivate} {
		issue := decodeBody[struct {
			Key               string `json:"key"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": title(project), "spec": spec(project),
		}, http.StatusCreated))
		path := "/api/v1/issues/" + issue.Key
		comment := decodeBody[model.Comment](t, seed.mustRequest(t, callerCarol, http.MethodPost, path+"/comments",
			map[string]any{"body": "An astrolabe remark on " + project + "."}, http.StatusCreated))
		ask := decodeBody[model.Ask](t, seed.mustRequest(t, callerCarol, http.MethodPost, path+"/asks", map[string]any{
			"question": "Which astrolabe does " + project + " keep?",
			"options":  []map[string]string{{"label": "Brass"}, {"label": "Steel"}},
		}, http.StatusCreated))
		message := decodeBody[model.Message](t, seed.mustRequest(t, callerCarol, http.MethodPost, path+"/messages",
			map[string]any{"body": "The astrolabe of " + project + " is calibrated."}, http.StatusCreated))
		objects[project] = []string{
			"issue/" + issue.Key, "document/" + issue.PrimaryArtifactID,
			"comment/" + comment.ID, "ask/" + ask.ID, "message/" + message.ID,
		}
		windows = append(windows,
			windowed{"document_block", issue.PrimaryArtifactID}, windowed{"comment_block", "comment/" + comment.ID},
			windowed{"ask_part", "ask/" + ask.ID}, windowed{"message_block", "message/" + message.ID})
	}
	awaitBackgroundWrites(t, seed.server)
	windowOwedOwners(t, seed.deps)
	for _, window := range windows {
		embedWindowsAs(t, seed.database, embedder, window.kind, window.owner, near)
	}
	processAllPending(t, seed.database, embedder)
	return objects
}

// searchAs is caller's search of query on the seeded server, which must answer 200.
func (s privateSeed) searchAs(t *testing.T, caller privateCaller, query string) model.SearchResponse {
	t.Helper()
	return decodeBody[model.SearchResponse](t, s.mustRequest(t, caller, http.MethodGet, "/api/v1/search?"+query, nil, http.StatusOK))
}

// searchAnswer is a search answer with what differs between two equal answers taken apart: how long
// each took, and the project key it echoes.
func searchAnswer(response model.SearchResponse) model.SearchResponse {
	response.TookMS = 0
	response.Filters.Project = ""
	return response
}

// searchFirstHit is the "kind/id" of an answer's first hit, the one it ranks highest, and empty
// for an answer with none.
func searchFirstHit(response model.SearchResponse) string {
	if len(response.Results) == 0 {
		return ""
	}
	return response.Results[0].Kind + "/" + response.Results[0].ID
}

// TestSearchOfAPrivateProjectFindsNothing: every caller of verification v16 finds, by both legs of
// search, the objects of the projects it sees and nothing of any other - on every statement search
// runs (the keyword statement; the meaning statement unscoped, scoped on the stored project and
// joined to each owner while the fill labels the rows, and joined for a filtered search) - and the
// total and the reachable count are of those objects alone. A project the caller does not see, named
// by ?project=, by ?number_project= or by an issue key, answers exactly as a key no row holds.
func TestSearchOfAPrivateProjectFindsNothing(t *testing.T) {
	t.Parallel()
	// searchAccessWord's own vector is near nothing, the seed's unmapped texts included, so a
	// search for it finds what its keyword legs find and nothing by meaning.
	embedder := &fakeEmbedder{vectors: map[string][]float32{searchAccessMeaning: queryVector(), searchAccessWord: angledVector(0, 3)}}
	seed := seedPrivateProjectWith(t, testServerOptions{embedder: embedder})
	objects := writeSearchableObjects(t, seed, embedder)
	issueOf := func(project string) string { return project + "-2" }

	type search struct {
		name, query, project string
		// filled is the embeddings project_key fill's state the search runs on, which decides
		// between the scoped meaning statement and the joined one for a search naming a project.
		filled bool
	}
	searches := []search{
		{name: "the keyword statement", query: "q=" + searchAccessWord + "&keywords_only=true"},
		{name: "the meaning statement's keyword legs", query: "q=" + searchAccessWord},
		{name: "the meaning statement's meaning legs", query: "q=" + searchAccessMeaning},
		{name: "the joined meaning statement a filtered search runs",
			query: "q=" + searchAccessMeaning + "&updated_since=" + url.QueryEscape("2000-01-01T00:00:00Z")},
	}
	for _, project := range []string{seedPublic, seedPrivate} {
		searches = append(searches,
			search{name: "the keyword statement of " + project,
				query: "q=" + searchAccessWord + "&keywords_only=true&project=" + project, project: project},
			search{name: "the scoped meaning statement of " + project,
				query: "q=" + searchAccessMeaning + "&project=" + project, project: project, filled: true},
			search{name: "the joined meaning statement of " + project,
				query: "q=" + searchAccessMeaning + "&project=" + project, project: project},
		)
	}

	for _, caller := range privateCallers {
		t.Run(string(caller), func(t *testing.T) {
			sees := caller.sees()
			for _, search := range searches {
				recordEmbeddingProjectKeyFill(t, seed.database, search.filled)
				want := []string{}
				for _, project := range sees {
					if search.project == "" || search.project == project {
						want = append(want, objects[project]...)
					}
				}
				slices.Sort(want)
				answer := seed.searchAs(t, caller, search.query)
				if got := searchResultKeys(answer.Results); !slices.Equal(got, want) {
					t.Errorf("%s: %s finds %v, want %v (the objects of %v)", caller, search.name, got, want, sees)
				}
				if answer.Total != len(want) || answer.Reachable != len(want) {
					t.Errorf("%s: %s counts total %d and reachable %d, want %d each", caller, search.name,
						answer.Total, answer.Reachable, len(want))
				}
				if search.project != "" && !slices.Contains(sees, search.project) {
					nobodys := seed.searchAs(t, caller, strings.Replace(search.query, "project="+search.project, "project="+searchNobodysProject, 1))
					if !reflect.DeepEqual(searchAnswer(answer), searchAnswer(nobodys)) {
						t.Errorf("%s: %s answers %+v, want what %s, a key no row holds, answers: %+v",
							caller, search.name, answer, searchNobodysProject, nobodys)
					}
				}
			}

			// The number is written 02: search refuses a query of fewer than two characters, and 02
			// names the issue of number 2 as 2 does (searchKeyForms).
			for _, project := range []string{seedPublic, seedPrivate} {
				own := "issue/" + issueOf(project)
				numbered := seed.searchAs(t, caller, "q=02&number_project="+project)
				named := seed.searchAs(t, caller, "q="+issueOf(project))
				if slices.Contains(sees, project) {
					if first := searchFirstHit(numbered); first != own {
						t.Errorf("%s: number 2 searched from %s finds %q first, want %s", caller, project, first, own)
					}
					if first := searchFirstHit(named); first != own {
						t.Errorf("%s: the key %s finds %q first, want %s", caller, issueOf(project), first, own)
					}
					continue
				}
				nobodysNumbered := seed.searchAs(t, caller, "q=02&number_project="+searchNobodysProject)
				if !reflect.DeepEqual(searchAnswer(numbered), searchAnswer(nobodysNumbered)) {
					t.Errorf("%s: number 2 searched from %s answers %+v, want what it answers searched from %s: %+v",
						caller, project, numbered, searchNobodysProject, nobodysNumbered)
				}
				nobodysNamed := seed.searchAs(t, caller, "q="+issueOf(searchNobodysProject))
				if !reflect.DeepEqual(searchAnswer(named), searchAnswer(nobodysNamed)) {
					t.Errorf("%s: the key %s answers %+v, want what the key %s answers: %+v",
						caller, issueOf(project), named, issueOf(searchNobodysProject), nobodysNamed)
				}
			}
		})
	}
}

// TestTheSearchFallbackWeighsWordsByTheProjectsTheCallerSees: a query its words as typed match
// nothing of is searched again over its rarest words, weighed by the term statistics of the projects
// the caller sees and of no other. PUB's one comment holds the sixth word alone; PUB counts that
// word once and none of the other five, so the fallback of a caller who sees PUB keeps it among the
// five it searches and finds the comment, as it would on a deployment where PRIV was never written.
// PRIV counts the five rare and the sixth common, so a fallback weighing PRIV's counts too drops the
// sixth word and finds nothing.
func TestTheSearchFallbackWeighsWordsByTheProjectsTheCallerSees(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	words := []string{"zorbalite", "quintexa", "plumbagor", "vexillum", "moraine", "corundum"}
	comment := decodeBody[model.Comment](t, seed.mustRequest(t, callerCarol, http.MethodPost,
		"/api/v1/issues/"+seed.issue[seedPublic]+"/comments", map[string]any{"body": "The " + words[5] + " is polished."},
		http.StatusCreated))
	awaitBackgroundWrites(t, seed.server)
	statistics := map[string]map[string]int{
		seedPublic:  {words[5]: 1},
		seedPrivate: {words[0]: 1, words[1]: 2, words[2]: 3, words[3]: 4, words[4]: 5, words[5]: 1000},
	}
	for project, counts := range statistics {
		for word, ndoc := range counts {
			if _, err := seed.database.Pool.Exec(context.Background(), `
				insert into search_term_stats (project_key, lexeme, ndoc)
				select $1, (select l.lexeme from unnest(to_tsvector('english', $2)) l limit 1), $3
				on conflict (project_key, lexeme) do update set ndoc = excluded.ndoc`, project, word, ndoc); err != nil {
				t.Fatalf("count %q %d times in %s: %v", word, ndoc, project, err)
			}
		}
	}
	query := "q=" + url.QueryEscape(strings.Join(words, " "))
	for _, caller := range []privateCaller{callerAlice, callerCarolToken, callerSharedToken} {
		answer := seed.searchAs(t, caller, query)
		if keys := searchResultKeys(answer.Results); !answer.Query.Broadened || !slices.Equal(keys, []string{"comment/" + comment.ID}) {
			t.Errorf("%s: the fallback of %q finds %v (broadened %t), want PUB's comment %s, weighed by PUB's counts alone",
				caller, strings.Join(words, " "), keys, answer.Query.Broadened, comment.ID)
		}
	}
}

// TestSearchCoverageCountsOnlyTheProjectsTheCallerSees: GET /api/v1/search/coverage counts, for
// each caller of verification v16, the items of the projects it sees and of no other, and the
// sessions it may see. Both projects hold an item of every kind the report counts, so PUB's report
// and PRIV's report are each non-zero in every count and add up to the report of carol, who sees
// both; a report that counted a project its caller does not see would add up to more.
func TestSearchCoverageCountsOnlyTheProjectsTheCallerSees(t *testing.T) {
	t.Parallel()
	embedder := &fakeEmbedder{vectors: map[string][]float32{searchAccessMeaning: queryVector()}}
	seed := seedPrivateProjectWith(t, testServerOptions{embedder: embedder})
	objects := writeSearchableObjects(t, seed, embedder)
	ctx := context.Background()
	for index, project := range []string{seedPublic, seedPrivate} {
		issue, document := strings.TrimPrefix(objects[project][0], "issue/"), strings.TrimPrefix(objects[project][1], "document/")
		// No route of this seed writes these three for PRIV: a handoff is a session's, a result needs
		// a Verification document's check, and a component an architecture import. They are written
		// as TestSearchCoverageNamesEveryKindItDoesNotIndex writes them (seedNotIndexedKinds).
		if _, err := seed.database.Pool.Exec(ctx, `
			with source as (
				insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
				values ($1, $4, 'acme/' || lower($1), 'main', 1, '{"kind":"system","id":"test"}') returning id
			), snapshot as (
				insert into architecture_snapshots (id, project_key, commit, files) values ($4, $1, 'c1', '{}') returning id
			), component as (
				insert into components (project_key, id, title, prose, snapshot_id, source_id)
				select $1, 'astrolabe', 'Astrolabe', 'The instrument.', snapshot.id, source.id from snapshot, source
			), handoff as (
				insert into handoffs (issue_key, phase, version, body, author)
				values ($2, 'plan', 1, '{}', '{"kind":"user","id":"carol@example.com"}')
			)
			insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
				check_sha256, role, commit_sha, verdict, actor)
			values ($2, $2, $3::uuid, 'k1', 1, 'a-check-sha', 'tester', repeat('a', 40), 'pass', '{"kind":"user","id":"carol@example.com"}')`,
			project, issue, document, 900+index); err != nil {
			t.Fatalf("write %s's component, handoff and result: %v", project, err)
		}
	}
	// One session that acted in each project, in the listener's stored session list.
	if _, err := seed.database.Pool.Exec(ctx, `
		insert into route_roster (answered, sessions, sent_at, expires_at)
		values (true, '[{"session_id":"session-pub"},{"session_id":"session-priv"}]', now(), now() + interval '1 hour')
		on conflict (id) do update
		set answered = excluded.answered, sessions = excluded.sessions, sent_at = excluded.sent_at, expires_at = excluded.expires_at`); err != nil {
		t.Fatalf("write the listener's sessions: %v", err)
	}
	if _, err := seed.database.Pool.Exec(ctx,
		`insert into session_projects (session_id, project_key) values ('session-pub', $1), ('session-priv', $2)`,
		seedPublic, seedPrivate); err != nil {
		t.Fatalf("record each session's project: %v", err)
	}

	coverage := func(caller privateCaller) model.SearchCoverage {
		return decodeBody[model.SearchCoverage](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/search/coverage", nil, http.StatusOK))
	}
	public, private, both := coverage(callerAlice), coverage(callerBob), coverage(callerCarol)
	none := model.SearchCoverage{}
	byProjects := map[string]model.SearchCoverage{seedPublic: public, seedPrivate: private, seedPrivate + "," + seedPublic: both, "": none}
	for _, kind := range searchCoverageCounts(both) {
		if got, want := searchCoverageCount(public, kind)+searchCoverageCount(private, kind), searchCoverageCount(both, kind); got != want {
			t.Errorf("%s: PUB's %d and PRIV's %d add up to %d, want carol's %d",
				kind, searchCoverageCount(public, kind), searchCoverageCount(private, kind), got, want)
		}
		if searchCoverageCount(public, kind) == 0 || searchCoverageCount(private, kind) == 0 {
			t.Errorf("%s: PUB counts %d and PRIV %d, want each project's own item counted",
				kind, searchCoverageCount(public, kind), searchCoverageCount(private, kind))
		}
	}
	// alice, bob and carol were read above, and their counts are the ones every other caller is
	// compared with, so each is checked by the sum above, and read again for nothing here.
	measured := map[privateCaller]model.SearchCoverage{callerAlice: public, callerBob: private, callerCarol: both}
	for _, caller := range privateCallers {
		t.Run(string(caller), func(t *testing.T) {
			got, read := measured[caller]
			if !read {
				got = coverage(caller)
				want := byProjects[strings.Join(caller.sees(), ",")]
				for _, kind := range searchCoverageCounts(both) {
					if searchCoverageCount(got, kind) != searchCoverageCount(want, kind) {
						t.Errorf("%s: %s counts %d, want %d, the count of %v", caller, kind,
							searchCoverageCount(got, kind), searchCoverageCount(want, kind), caller.sees())
					}
				}
			}
			// One session acted in each project, and a session is seen by a caller that sees a
			// project it acted in; an open viewer sees no session of a project it does not see.
			sessions := len(caller.sees())
			if got := searchCoverageNotIndexed(got, "session"); got != int64(sessions) {
				t.Errorf("%s: counts %d sessions, want %d", caller, got, sessions)
			}
		})
	}
}

// searchCoverageCounts names every count of a coverage report that counts a project's items, as
// searchCoverageCount reads it: each covered kind's items and current, and each uncovered kind's
// items but the sessions, which belong to no project.
func searchCoverageCounts(report model.SearchCoverage) []string {
	counts := []string{}
	for _, kind := range []string{"issue", "document_block", "comment_block", "message_block", "ask_part"} {
		counts = append(counts, kind+" items", kind+" current")
	}
	for _, uncovered := range report.NotIndexed {
		if uncovered.Kind != "session" {
			counts = append(counts, uncovered.Kind+" items")
		}
	}
	return counts
}

// searchCoverageCount is one count of report, named as searchCoverageCounts names it.
func searchCoverageCount(report model.SearchCoverage, count string) int64 {
	kind, field, _ := strings.Cut(count, " ")
	kinds := map[string]model.SearchCoverageCounts{
		"issue": report.Kinds.Issue, "document_block": report.Kinds.DocumentBlock, "comment_block": report.Kinds.CommentBlock,
		"message_block": report.Kinds.MessageBlock, "ask_part": report.Kinds.AskPart,
	}
	counts, covered := kinds[kind]
	if !covered {
		return searchCoverageNotIndexed(report, kind)
	}
	if field == "current" {
		return counts.Current
	}
	return counts.Items
}

// searchCoverageNotIndexed is how many items of an uncovered kind report counts.
func searchCoverageNotIndexed(report model.SearchCoverage, kind string) int64 {
	for _, uncovered := range report.NotIndexed {
		if uncovered.Kind == kind {
			return uncovered.Items
		}
	}
	return 0
}
