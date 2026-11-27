package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// Test helpers the S3c-documents tests read; the rest of this file is S3c-issues's.

// fragmentCensusFixture is verification v16's seed on a server whose pool census traces, holding
// the objects of each project the walk reads at.
type fragmentCensusFixture struct {
	privateSeed
	public, private censusObjects
}

// censusOneLine is a statement's SQL with its whitespace run together, as a failure prints it.
func censusOneLine(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

// fragmentCensus records the SQL of every statement a request made on a recording context
// (recordingKey), each statement of a batch on its own, so the background work of the server is
// no part of the record.
type fragmentCensus struct {
	mu         sync.Mutex
	statements []string
}

// censusObjects are the objects of one project a walked read is filled with: its documents, an
// ask, a comment and a message of its issue, a handoff, a session that acted in it with a file of
// that session's conversation, a person, a broadcast to the session; and the priority and alice's
// going run, which belong to no project and are the same on both walks.
type censusObjects struct {
	documentObjects
	block                          string
	ask, comment, message, handoff string
	session, conversationSlug      string
	person, broadcast              string
	priority, goingRun             string
	// nearCopyCheck is the near-copy gate's row of the project's own create (advancing the
	// priority), which GET /api/v1/near-copy-checks/{id} reads under the project's rule.
	nearCopyCheck string
	// deliveryRun is a deploy run of the deploy repository, one for each walk, which GET
	// /api/v1/delivery/runs/{id} reads under the timeline's rule.
	deliveryRun string
}

// fill is pattern with each wildcard filled by the object probe names for it. A document's slug
// is that of the document the path's owner holds: the conversation's, the issue's or the
// project's.
func (o censusObjects) fill(t *testing.T, pattern string, probe readProbe) string {
	t.Helper()
	slug := o.documentSlug
	switch {
	case strings.HasPrefix(pattern, "/api/v1/agents/"):
		slug = o.conversationSlug
	case strings.HasPrefix(pattern, "/api/v1/issues/"):
		slug = o.issueDocumentSlug
	}
	return wildcard.ReplaceAllStringFunc(pattern, func(match string) string {
		object := probe[wildcard.FindStringSubmatch(match)[1]]
		value := map[probeObject]string{
			probeProject: o.project, probeIssue: o.issue, probeDocument: o.document, probeDocumentSlug: slug,
			probeDocumentVersion: "1", probeBlock: o.block, probeAsk: o.ask, probeComment: o.comment,
			probeMessage: o.message, probeHandoff: o.handoff, probeHandoffPhase: "plan", probeSession: o.session,
			probePerson: o.person, probePriority: o.priority, probeBroadcast: o.broadcast, probeGoingRun: o.goingRun,
			probeNearCopyCheck: o.nearCopyCheck, probeDeliveryRun: o.deliveryRun,
			// The repository seedDocumentObjects maps to the project.
			probeRepositoryOwner: "acme", probeRepositoryName: strings.ToLower(o.project) + "-app",
		}[object]
		if value == "" {
			t.Fatalf("%s: the census seeds no %q for %s", pattern, object, match)
		}
		return value
	})
}

// censusQueries are the queries the walked reads that read nothing without one are sent, each
// naming objects of the project the walk reads at.
var censusQueries = map[string]func(censusObjects) url.Values{
	"GET /api/v1/issues/resolve": func(o censusObjects) url.Values { return url.Values{"ref": {o.issue}} },
	"GET /api/v1/references":     func(o censusObjects) url.Values { return url.Values{"to": {"dispatch://" + o.issue}} },
	"GET /api/v1/references/questions": func(o censusObjects) url.Values {
		return url.Values{"kind": {"comment"}, "id": {o.comment}}
	},
	"GET /api/v1/block-links": func(o censusObjects) url.Values {
		spec := "dispatch://" + o.issue + "/spec"
		return url.Values{"from": {spec}, "to": {spec}}
	},
	"GET /api/v1/asks/open":              func(o censusObjects) url.Values { return url.Values{"project": {o.project}} },
	"GET /api/v1/search":                 func(o censusObjects) url.Values { return url.Values{"q": {"runbook"}} },
	"GET /api/v1/search/results":         func(o censusObjects) url.Values { return url.Values{"q": {"runbook"}} },
	"GET /api/v1/projects/{key}/related": func(o censusObjects) url.Values { return url.Values{"title": {"Runbook steps"}} },
	"GET /api/v1/related-checks":         func(o censusObjects) url.Values { return url.Values{"project": {o.project}} },
	"GET /api/v1/near-copy-checks":       func(o censusObjects) url.Values { return url.Values{"project": {o.project}} },
	"GET /api/v1/reference-targets": func(o censusObjects) url.Values {
		return url.Values{"ref": {"dispatch://" + o.issue}}
	},
	// Both versions are required (400 INVALID_VERSION without them); the seed names the project
	// document's second version.
	"GET /api/v1/artifacts/{id}/changes": func(o censusObjects) url.Values { return url.Values{"from": {"1"}, "to": {"2"}} },
}

// seedFragmentCensus writes, through the routes and as the person who would write each, what every
// walked read answers: each project's documents, threads, handoff, session work, conversation file
// and broadcast; alice's comment, answer, direct message and going run on PUB; a priority each
// project's work advances; and the delivery settings, pull requests and CI pass the deployment
// reads count. The delivery rows are written as the code that stores them writes them, because no
// route writes those.
func seedFragmentCensus(t *testing.T, census *fragmentCensus) fragmentCensusFixture {
	t.Helper()
	interests := func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1/interests/":
			_, _ = w.Write([]byte(`[{"session_id":"` + sessionOfPublic + `","topics":["notifications.dispatch.>"],"updated_at":1}]`))
		case "/v1/legion/agents":
			_, _ = w.Write([]byte("[]"))
		default:
			return false
		}
		return true
	}
	// The listener lists session-pub's and session-priv's credentials' projects, so alice reads
	// PUB's session's conversation file and her direct message to it, which are its content
	// (access.SessionContentClause), and no conversation of PRIV's session.
	listener, _ := newBroadcastListener(t, privateRosterListed, interests)
	github := &fakeGitHubApp{installations: map[string]string{}, repos: map[string]*fakeArchitectureRepo{}}
	for _, project := range []string{seedPublic, seedPrivate} {
		github.installations[architectureRepo(project)] = "read"
		github.installations["acme/"+strings.ToLower(project)+"-app"] = "read"
		github.repos[architectureRepo(project)] = &fakeArchitectureRepo{commit: shaOne, files: archFiles()}
	}
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	seed := seedPrivateProjectWith(t, testServerOptions{
		store: storetest.TracedStore(t, storetest.Open(t), census), envoyURL: listener.URL, embedder: embedder,
		app: fakeGitHubAppConfig(t), githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: github.handler(t).ServeHTTP}),
	})
	seedSessionWork(t, seed)
	// A list of the agents stores the listener's list, whose projects the content rule reads.
	seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)
	fixture := fragmentCensusFixture{privateSeed: seed}
	// Each project names a Legion daemon, so its board and its daemon setting read their rows.
	for _, project := range []string{seedPublic, seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+project+"/legion/daemon",
			map[string]any{"state_url": "http://legion-" + strings.ToLower(project) + ".internal:8080"}, http.StatusOK)
	}

	priority := decodeBody[model.Priority](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/priorities",
		map[string]any{"title": "Ship the census"}, http.StatusCreated)).ID
	goingRun := createdID(t, seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/going/requests",
		map[string]any{"words": "What do the runbook steps say?", "project": seedPublic}, http.StatusCreated))
	// Each project's objects name a session that acted in that project alone (seedSessionWork), so
	// alice, who sees PUB, sees PUB's session and not PRIV's (access.SessionClause).
	for _, owned := range []struct{ project, session string }{{seedPublic, sessionOfPublic}, {seedPrivate, sessionOfPrivate}} {
		project, session := owned.project, owned.session
		objects := censusObjects{
			documentObjects: seedDocumentObjects(t, seed, project), session: session, priority: priority, goingRun: goingRun,
		}
		path := "/api/v1/issues/" + objects.issue
		objects.block = decodeBody[[]struct {
			ID string `json:"id"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/artifacts/"+objects.document+"/blocks", nil, http.StatusOK))[0].ID
		// The project document's second version, an edit named as a version, which writes it at
		// once: the change set of versions 1 and 2 (censusQueries) then reads both.
		if revised := decodeBody[struct {
			Version *model.Version `json:"version"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/artifacts/"+objects.document+"/edits", map[string]any{
			"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "The revised runbook steps of " + project + "."}},
			"summary": "Revise the runbook",
		}, http.StatusOK)).Version; revised == nil || revised.Number != 2 {
			t.Fatalf("carol's named edit of %s's runbook wrote %+v, want its version 2", project, revised)
		}
		objects.comment = createdID(t, seed.request(t, callerCarol, http.MethodPost, path+"/comments",
			map[string]any{"body": "The runbook comment of " + project + "."}))
		objects.ask = createdID(t, seed.request(t, callerCarol, http.MethodPost, path+"/asks",
			map[string]any{"question": "Which runbook does " + project + " keep?"}))
		objects.message = createdID(t, seed.request(t, callerCarol, http.MethodPost, path+"/messages",
			map[string]any{"body": "The runbook message of " + project + "."}))
		writer := map[string]privateCaller{seedPublic: callerCarolToken, seedPrivate: callerCarolPrivToken}[project]
		objects.handoff = createdID(t, seed.request(t, writer, http.MethodPost, path+"/handoffs/plan", map[string]any{
			"body": `{"plan":"the runbook of ` + project + `"}`, "actor": map[string]string{"kind": "session", "id": session},
		}))
		uploaded := multipartRequest(t, seed.handler, "/api/v1/agents/"+session+"/artifacts", map[string]string{"name": "shot.png"},
			"shot.png", "image/png", pngBytes("the runbook shot of "+project), seedCarol)
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("carol uploads to %s's conversation: %d %s", session, uploaded.Code, uploaded.Body.String())
		}
		objects.conversationSlug = decodeBody[artifactUploadResponse](t, uploaded).Artifact.Slug
		broadcast := keyedRequest(t, seed.handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "Status of " + project + "?", "delivery": "btw", "session_ids": []string{session},
		}, seedCarol, uuid.NewString())
		objects.broadcast = createdID(t, broadcast)
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": "Advance " + project + "'s census", "advances": []string{priority}, "force": true,
		}, http.StatusCreated)
		if err := seed.database.Pool.QueryRow(context.Background(), `
			select id::text from near_copy_checks where project_key = $1 order by created_at desc, id desc limit 1
		`, project).Scan(&objects.nearCopyCheck); err != nil {
			t.Fatalf("read the near-copy check of %s's create: %v", project, err)
		}
		objects.person = seedCarol
		if project == seedPublic {
			objects.person = seedAlice
			fixture.public = objects
		} else {
			fixture.private = objects
		}
	}

	// alice's own rows on PUB: a comment, an answer to its ask, and a direct message to its session.
	public := fixture.public
	createdID(t, seed.request(t, callerAlice, http.MethodPost, "/api/v1/issues/"+public.issue+"/comments",
		map[string]any{"body": "alice read the runbook."}))
	seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/asks/"+public.ask+"/answer",
		map[string]any{"text": "The first runbook.", "expected_edited_at": nil}, http.StatusOK)
	createdID(t, postAgentMessage(t, seed.handler, public.session,
		map[string]any{"body": "alice asks about the runbook.", "delivery": "btw"}, seedAlice))

	ctx := context.Background()
	if _, err := delivery.PutSettings(ctx, seed.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/deploy", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	merged := time.Now().UTC().Add(-time.Hour)
	opened := merged.Add(-time.Hour)
	for number, project := range []string{seedPublic, seedPrivate} {
		repo := "acme/" + strings.ToLower(project) + "-app"
		if err := delivery.UpsertPullRequest(ctx, seed.database.Pool, delivery.DeliveryPullRequest{
			Repo: repo, Number: number + 1, Title: "runbook change of " + project, Author: "octocat",
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number+1), CreatedAt: &opened, MergedAt: &merged,
		}); err != nil {
			t.Fatalf("store %s#%d: %v", repo, number+1, err)
		}
	}
	// A deploy run of the deploy repository for each walk, which ships none of the pull requests
	// above: they are of other repositories.
	for index, objects := range []*censusObjects{&fixture.public, &fixture.private} {
		started := merged.Add(time.Duration(index+1) * 10 * time.Minute)
		run := delivery.DeliveryRun{
			Repo: "acme/deploy", RunID: int64(900 + index), Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("census%d", index),
			HeadCommitAt: started, StartedAt: started, CompletedAt: new(started.Add(5 * time.Minute)),
			Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: fmt.Sprintf("https://github.com/acme/deploy/actions/runs/%d", 900+index),
			HeadBranch: new("main"), Event: new("push"),
		}
		if err := delivery.UpsertRun(ctx, seed.database.Pool, run); err != nil {
			t.Fatalf("store deploy run %d: %v", run.RunID, err)
		}
		if err := delivery.UpsertRunJobs(ctx, seed.database.Pool, run.Repo, run.RunID, []delivery.DeliveryRunJob{{
			Repo: run.Repo, RunID: run.RunID, Name: "release / release", StartedAt: new(started), CompletedAt: new(started.Add(4 * time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess),
		}}, true); err != nil {
			t.Fatalf("store the jobs of deploy run %d: %v", run.RunID, err)
		}
		objects.deliveryRun = strconv.FormatInt(run.RunID, 10)
	}
	endedCIPass(t, seed.database, seedServerProcess(t, seed.database, 0), time.Hour, "complete")

	awaitBackgroundWrites(t, seed.server)
	windowOwedOwners(t, seed.deps)
	processAllPending(t, seed.database, embedder)
	return fixture
}

// readsProjectRows reports whether a GET row of class answers rows a project, the graph, a person
// or a session owns: the reads the census walks.
func readsProjectRows(class readclass.Class) bool {
	switch class {
	case readclass.Project, readclass.Graph, readclass.People, readclass.Agents:
		return true
	}
	return false
}

// wildcard matches one wildcard of a route pattern.
var wildcard = regexp.MustCompile(`\{([a-z_]+)\}`)

func (c *fragmentCensus) add(ctx context.Context, statement string) {
	if ctx.Value(recordingKey{}) == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, statement)
}

func (c *fragmentCensus) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.add(ctx, data.SQL)
	return ctx
}

func (c *fragmentCensus) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *fragmentCensus) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *fragmentCensus) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	c.add(ctx, data.SQL)
}

func (c *fragmentCensus) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// read makes one GET of target as alice on a recording context and answers the statements it made.
func (c *fragmentCensus) read(t *testing.T, handler http.Handler, target string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	c.mu.Lock()
	c.statements = nil
	c.mu.Unlock()
	request := httptest.NewRequestWithContext(context.WithValue(t.Context(), recordingKey{}, true), http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", seedAlice)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	c.mu.Lock()
	defer c.mu.Unlock()
	return response, slices.Clone(c.statements)
}
