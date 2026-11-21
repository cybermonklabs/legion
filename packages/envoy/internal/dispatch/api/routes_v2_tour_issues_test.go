package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func init() { addTourSteps("issues", issuesTourSteps) }

// issuesTourSteps are the route tour's steps of the issue routes: each
// route, the list with its cross-project filters and its route_status cut, the read with every
// include=, the patch of the design gates alone, and the checks read with the commit at= names.
func issuesTourSteps() []tourStep {
	alice := tourPerson("alice")
	issueMissing := []tourMissing{{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}}
	onIssue := func(query url.Values, body func(*testing.T, *routeTour, tourIssue) any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			issue := tour.issue(t)
			request := tourRequest{Path: map[string]string{"key": issue.Key}, Query: query, As: alice}
			if body != nil {
				request.Body = body(t, tour, issue)
			}
			return request
		}
	}
	withChecks := func(query url.Values) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			issue := tour.issue(t)
			tour.result(t, issue)
			return tourRequest{Path: map[string]string{"key": issue.Key}, Query: query, As: alice}
		}
	}
	// recordsResult is the request of the result record and its dry run: k1 passing at the head
	// commit, on an issue whose Verification document holds tourChecks.
	recordsResult := func(t *testing.T, tour *routeTour) tourRequest {
		issue := tour.issue(t)
		tour.issueDocument(t, issue.Key, "verification.md", tourChecks)
		return tourRequest{Path: map[string]string{"key": issue.Key}, Body: resultBody("k1", "pass", headCommit, nil), As: alice}
	}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/issues", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"project": {tour.projectOf(t, tour.issue(t))}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/issues", Name: "across projects, every filter", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{
					"project": {tour.projectOf(t, tour.issue(t)), tour.project(t)}, "status": {"backlog", "doing"},
					"label": {"tour"}, "priority": {"1", "none"}, "advances": {"none"}, "open": {"true"}, "moved": {"true"},
					"updated_from": {"2026-01-01T00:00:00Z"}, "updated_to": {"2099-01-01T00:00:00Z"},
					"closed_from": {"2026-01-01T00:00:00Z"}, "closed_to": {"2099-01-01T00:00:00Z"},
					"limit": {"10"}, "offset": {"0"},
				}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/issues", Name: "the caller's pins", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"pinned": {"true"}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/issues", Name: "by an issue reference", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"ref": {tour.issue(t).Key}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/issues", Name: "cut after the route_status filter", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"route_status": {"unknown"}, "limit": {"5"}}, As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/issues", Status: http.StatusCreated,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"project": tour.project(t), "title": "An issue the tour creates"}, As: alice}
			},
		},
		{
			// An import of a reference that already has its issue answers that issue, 200, with the
			// advice and suggestions a create writes (issue_create.go, the existing-issue arm).
			Method: http.MethodPost, Pattern: "/issues", Name: "an import of a reference already imported", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				project, repo := tour.repository(t)
				body := map[string]any{"project": project, "external": repo + "#7", "title": "An issue the tour imports"}
				if imported := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues", body, "alice"); imported.Code != http.StatusCreated {
					t.Fatalf("import %s#7: %d %s", repo, imported.Code, imported.Body.String())
				}
				return tourRequest{Body: body, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/issues", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"key": tour.projectOf(t, tour.issue(t))}, As: alice}
			},
			Missing: []tourMissing{{Param: "key", Value: "NOPE", Code: apicode.ProjectNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}", Status: http.StatusOK,
			Setup: onIssue(nil, nil), Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}", Name: "with every include", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.ask(t, issue)
				return tourRequest{
					Path:  map[string]string{"key": issue.Key},
					Query: url.Values{"include": {"open_asks,children,progress,design_gates,reach,referenced_by_count"}}, As: alice,
				}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/issues/{key}", Status: http.StatusOK,
			Setup: onIssue(nil, func(*testing.T, *routeTour, tourIssue) any {
				return map[string]any{"title": "The tour renamed this issue", "labels_add": []string{"tour"}}
			}),
			Missing: issueMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/issues/{key}", Name: "its design gates alone", Status: http.StatusOK,
			Setup: onIssue(nil, func(_ *testing.T, _ *routeTour, issue tourIssue) any {
				return map[string]any{"design_gates": []map[string]string{{"step": "spec", "artifact_id": issue.Document}}}
			}),
			Missing: issueMissing,
		},
		{
			Method: http.MethodDelete, Pattern: "/issues/{key}/claim", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.claim(t, issue)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/claim", Status: http.StatusOK,
			Setup:   onIssue(nil, func(*testing.T, *routeTour, tourIssue) any { return map[string]any{} }),
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/collaborators", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.comment(t, issue)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/coverage", Status: http.StatusOK,
			Setup: onIssue(nil, nil), Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/decisions", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.decision(t, issue)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/decisions", Name: "with the spec's open questions", Status: http.StatusOK,
			Setup: onIssue(url.Values{"include": {"open_questions"}}, nil), Missing: issueMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/decisions/{block_id}/state", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				block, sum := tour.decision(t, issue)
				return tourRequest{
					Path: map[string]string{"key": issue.Key, "block_id": block},
					Body: map[string]any{"state": "kept", "text_sha256": sum}, As: alice,
				}
			},
			Missing: append([]tourMissing{{Param: "block_id", Value: "b-nope-404", Code: apicode.BlockNotFound}}, issueMissing...),
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/references", Status: http.StatusOK,
			// The issue cites a project document, so the contract is held on a member's row.
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue, project := tour.issue(t), tour.project(t)
				cited := createProjectDocument(t, tour.handler, project, "Tour cited notes.md", "# Cited\n\nWhat the issue cites.\n")
				createReferenceAPIComment(t, tour.handler, issue.Key, "dispatch://"+project+"/artifact/"+cited.Slug)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/checks", Status: http.StatusOK,
			Setup: withChecks(nil), Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/checks", Name: "compared with the commit at names", Status: http.StatusOK,
			Setup: withChecks(url.Values{"at": {headCommit}}), Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/results", Status: http.StatusCreated,
			Setup: recordsResult, Missing: issueMissing,
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/results/dry-run", Status: http.StatusOK,
			Setup: recordsResult, Missing: issueMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/subscribers", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.subscribeToIssue(t, issue.Key)
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodDelete, Pattern: "/issues/{key}/subscribers/{session_id}", Status: http.StatusNoContent,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				return tourRequest{Path: map[string]string{"key": issue.Key, "session_id": tour.subscribeToIssue(t, issue.Key)}, As: alice}
			},
			Missing: issueMissing,
		},
		{
			Method: http.MethodPut, Pattern: "/results/{id}/state", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{
					Path: map[string]string{"id": tour.result(t, tour.issue(t))},
					Body: map[string]any{"state": "retracted", "reason": "The tour ran it at the wrong commit."}, As: alice,
				}
			},
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.ResultNotFound}},
		},
	}
}

// tourChecks is the Verification document the tour records results against: one check, whose
// words hold a link.
const tourChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\n" +
	"The tour records a result against [the link text](https://example.com) of a check.\n" +
	":::\n"

// projectOf is the project issue belongs to.
func (tour *routeTour) projectOf(t *testing.T, issue tourIssue) string {
	t.Helper()
	var project string
	if err := tour.database.Pool.QueryRow(t.Context(), `select project_key from issues where key = $1`, issue.Key).Scan(&project); err != nil {
		t.Fatalf("read the project of %s: %v", issue.Key, err)
	}
	return project
}

// claim claims issue for alice through /api/v1.
func (tour *routeTour) claim(t *testing.T, issue tourIssue) {
	t.Helper()
	if claimed := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{}, "alice"); claimed.Code != http.StatusOK {
		t.Fatalf("claim %s: %d %s", issue.Key, claimed.Code, claimed.Body.String())
	}
}

// decision writes issue a decisions.md holding one decision, answering its heading block and its
// text_sha256 as GET /api/v1/issues/{key}/decisions reads them.
func (tour *routeTour) decision(t *testing.T, issue tourIssue) (block, sum string) {
	t.Helper()
	tour.issueDocument(t, issue.Key, "decisions.md", "## Keep the tour short\n\nThe tour reads one decision and keeps it.\n")
	read := dispatchRequest(t, tour.handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/decisions", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the decisions of %s: %d %s", issue.Key, read.Code, read.Body.String())
	}
	decisions := decodeBody[issueDecisions](t, read).Decisions
	if len(decisions) != 1 {
		t.Fatalf("the decisions of %s are %+v, want the one the tour wrote", issue.Key, decisions)
	}
	return decisions[0].BlockID, decisions[0].TextSHA256
}

// result records a pass of tourChecks' one check on issue, as alice, answering the result's id.
func (tour *routeTour) result(t *testing.T, issue tourIssue) string {
	t.Helper()
	tour.issueDocument(t, issue.Key, "verification.md", tourChecks)
	recorded := recordResult(t, tour.handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result on %s: %d %s", issue.Key, recorded.Code, recorded.Body.String())
	}
	return decodeBody[recordedResult](t, recorded).Result.ID
}

// subscribeToIssue tells the listener a session of its own follows the issue's topics, answering
// the session.
func (tour *routeTour) subscribeToIssue(t *testing.T, key string) string {
	t.Helper()
	session := "session-tour-subscriber-" + uuid.NewString()[:8]
	tour.listener.mu.Lock()
	tour.listener.interests[session] = []string{issueSubscriberBase(key) + ".>"}
	tour.listener.mu.Unlock()
	return session
}

// TestAProjectsIssuesAreTheIssuesItsProjectFilterLists holds R1.3: GET
// /api/v2/projects/{key}/issues and GET /api/v2/issues?project={key} answer the same rows for the
// same filters, the project's own and none of another's.
func TestAProjectsIssuesAreTheIssuesItsProjectFilterLists(t *testing.T) {
	tour := newRouteTour(t)
	first := tour.issue(t)
	project := tour.projectOf(t, first)
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": "A second issue of the project", "labels": []string{"second"},
		"spec": "# Spec\n\nThe second issue's passage.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a second issue: %d %s", created.Code, created.Body.String())
	}
	secondIssue := decodeBody[tourIssue](t, created)
	awaitSettled(t, tour.database, secondIssue.Document)
	second := secondIssue.Key
	elsewhere := tour.issue(t)

	for _, filters := range []url.Values{
		{},
		{"label": {"second"}},
		{"limit": {"1"}, "offset": {"1"}},
		{"route_status": {"unknown"}},
	} {
		t.Run("filtered by "+filters.Encode(), func(t *testing.T) {
			read := func(target string) []byte {
				t.Helper()
				response := dispatchRequest(t, tour.handler, http.MethodGet, target, nil, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s: %d %s", target, response.Code, response.Body.String())
				}
				return response.Body.Bytes()
			}
			ofProject := read("/api/v2/projects/" + project + "/issues?" + filters.Encode())
			byFilter := read("/api/v2/issues?" + withQueryValue(filters, "project", project).Encode())
			if string(ofProject) != string(byFilter) {
				t.Fatalf("the project's list answered %s, and the list filtered by project %s", ofProject, byFilter)
			}
			listed := map[string]bool{}
			for _, issue := range decodeAnswer[page[model.IssueSummary]](t, ofProject).Items {
				listed[issue.Key] = true
			}
			if listed[elsewhere.Key] {
				t.Errorf("the project's list holds %s, another project's issue", elsewhere.Key)
			}
			if len(filters) == 0 && (!listed[first.Key] || !listed[second]) {
				t.Errorf("the project's list holds %v, want both of its issues %s and %s", listed, first.Key, second)
			}
		})
	}
}

// TestIssueChangedNamesTheSequenceTheCallerReadUnderBothVersions is the parent's R10.2, which R3
// lets /api/v1 change too: a fenced patch whose expected_sequence the issue has moved past is 409
// ISSUE_CHANGED saying the issue changed since the sequence the caller read, and naming no
// hand-over, which the write never was, under /api/v1 and /api/v2 alike.
func TestIssueChangedNamesTheSequenceTheCallerReadUnderBothVersions(t *testing.T) {
	tour := newRouteTour(t)
	issue := tour.issue(t)
	for _, target := range []string{"/api/v1/issues/" + issue.Key, "/api/v2/issues/" + issue.Key} {
		refused := dispatchRequest(t, tour.handler, http.MethodPatch, target, map[string]any{
			"title": "A title the stale read would write", "expected_sequence": 0,
		}, "alice")
		code, text := refusalOf(t, refused)
		if refused.Code != http.StatusConflict || code != "ISSUE_CHANGED" || text != "the issue changed since the sequence you read" {
			t.Errorf("PATCH %s with a stale expected_sequence: %d %s, want 409 ISSUE_CHANGED %q",
				target, refused.Code, refused.Body.String(), "the issue changed since the sequence you read")
		}
	}
	if read := decodeBody[model.Issue](t, dispatchRequest(t, tour.handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")); read.Title != "A toured issue" {
		t.Fatalf("the refused patches left the title %q, want it as it was", read.Title)
	}
}

// TestAChecksTextAndARequirementsTextKeepALinksText is R10's first case (the parent's R9.2), which
// R3.3 lets /api/v1 change too: a check whose words hold [the link text](https://example.com)
// answers "the link text" and no URL in the checks read, and a requirement holding the same link
// answers it so in the coverage report, under both versions; a bare URL is still left out.
func TestAChecksTextAndARequirementsTextKeepALinksText(t *testing.T) {
	tour := newRouteTour(t)
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": tour.project(t), "title": "Links in the words",
		"spec": ":::requirement{#r1}\nThe server keeps [the link text](https://example.com) in the report, https://example.org/bare left out.\n:::\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: %d %s", created.Code, created.Body.String())
	}
	issue := decodeBody[tourIssue](t, created)
	awaitSettled(t, tour.database, issue.Document)
	tour.issueDocument(t, issue.Key, "verification.md",
		":::check{#k1 surface=\"cli\" run=\"go test ./...\"}\nThe check reads [the link text](https://example.com) back.\n:::\n")
	const check, requirement = "The check reads the link text back.", "The server keeps the link text in the report, left out."
	get := func(target string) []byte {
		t.Helper()
		response := dispatchRequest(t, tour.handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "example.") {
			t.Errorf("GET %s answers a link's target: %s", target, response.Body.String())
		}
		return response.Body.Bytes()
	}
	type text struct {
		Text string `json:"text"`
	}
	if checks := decodeAnswer[page[text]](t, get("/api/v2/issues/"+issue.Key+"/checks")).Items; len(checks) != 1 || checks[0].Text != check {
		t.Errorf("the /api/v2 checks read answers %+v, want the one check reading %q", checks, check)
	}
	if checks := decodeAnswer[issueResults](t, get("/api/v1/issues/"+issue.Key+"/results")).Checks; len(checks) != 1 || checks[0].Text != check {
		t.Errorf("the /api/v1 results read answers %+v, want the one check reading %q", checks, check)
	}
	for _, version := range []string{"v1", "v2"} {
		coverage := decodeAnswer[model.IssueCoverage](t, get("/api/"+version+"/issues/"+issue.Key+"/coverage"))
		if len(coverage.Requirements) != 1 || coverage.Requirements[0].Text != requirement {
			t.Errorf("the /api/%s coverage report answers %+v, want the one requirement reading %q", version, coverage.Requirements, requirement)
		}
	}
}

// TestAResultsDryRunAnswersOneLineUnderBothVersions: POST /api/v2/issues/{key}/results/dry-run
// answers the line POST /api/v1/issues/{key}/results/dry-run answers for the same body, the line
// the /api/v2 record then answers, and records nothing, no result and no event. The line leaves
// out the issue's own key, a key of a project this Dispatch holds, under both versions.
func TestAResultsDryRunAnswersOneLineUnderBothVersions(t *testing.T) {
	tour := newRouteTour(t)
	issue := tour.issue(t)
	tour.issueDocument(t, issue.Key, "verification.md", tourChecks)
	body := resultBody("k1", "pass", headCommit, map[string]any{"observed": issue.Key + "'s guard held on every row."})
	events := issueEventTypes(t, tour.handler, issue.Key)
	want := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. guard held on every row."
	for _, version := range []string{"v1", "v2"} {
		dry := dispatchRequest(t, tour.handler, http.MethodPost, "/api/"+version+"/issues/"+issue.Key+"/results/dry-run", body, "alice")
		if dry.Code != http.StatusOK {
			t.Fatalf("the /api/%s dry run: %d %s, want 200", version, dry.Code, dry.Body.String())
		}
		if line := decodeBody[resultDryRun](t, dry).Line; line != want {
			t.Errorf("the /api/%s dry run's line = %q, want %q", version, line, want)
		}
	}
	if results := checkOf(t, readResults(t, tour.handler, issue.Key), "k1").Results; len(results) != 0 {
		t.Fatalf("the dry runs recorded %+v, want nothing", results)
	}
	if after := issueEventTypes(t, tour.handler, issue.Key); !slices.Equal(after, events) {
		t.Fatalf("the dry runs moved the issue's events from %v to %v, want them unmoved", events, after)
	}
	recorded := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v2/issues/"+issue.Key+"/results", body, "alice")
	if recorded.Code != http.StatusCreated {
		t.Fatalf("the /api/v2 record: %d %s, want 201", recorded.Code, recorded.Body.String())
	}
	if line := decodeBody[recordedResult](t, recorded).Line; line != want {
		t.Fatalf("the /api/v2 record's line = %q, want the dry runs' %q", line, want)
	}
}

// TestAnEqualKeepOrRetractionFromAnotherCallerIsAChange is the other half of decision 8's rule for
// the keep and the retraction, whose no-op TestV2Retries holds: the same body from a caller who set
// nothing is a change. Bob's keep of the text alice kept moves the keep to him and appends its
// decision.kept; bob's retraction of the result alice retracted, and alice's own with another
// reason, are each 409 RESULT_RETRACTED with nothing written.
func TestAnEqualKeepOrRetractionFromAnotherCallerIsAChange(t *testing.T) {
	tour := newRouteTour(t)
	alice, bob := tourPerson("alice"), tourPerson("bob")

	t.Run("a keep", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/issues/{key}/decisions/{block_id}/state")
		issue := tour.issue(t)
		block, sum := tour.decision(t, issue)
		keep := tourRequest{Path: map[string]string{"key": issue.Key, "block_id": block}, Body: map[string]any{"state": "kept", "text_sha256": sum}}
		keep.As = alice
		if kept := tour.send(t, route, keep, ""); kept.Code != http.StatusOK {
			t.Fatalf("alice's keep: %d %s", kept.Code, kept.Body.String())
		}
		keep.As = bob
		moved := tour.send(t, route, keep, "")
		if moved.Code != http.StatusOK || decodeBody[model.DecisionKeep](t, moved).KeptBy != "bob" {
			t.Fatalf("bob's keep of the same text: %d %s, want 200 the keep moved to bob", moved.Code, moved.Body.String())
		}
		if kept := tour.eventCount(t, issue.Key, "decision.kept"); kept != 2 {
			t.Fatalf("the two keeps appended %d decision.kept events, want one each", kept)
		}
	})

	t.Run("a retraction", func(t *testing.T) {
		route := tour.requireMounted(t, http.MethodPut, "/results/{id}/state")
		issue := tour.issue(t)
		retract := tourRequest{
			Path: map[string]string{"id": tour.result(t, issue)}, Body: map[string]any{"state": "retracted", "reason": "Recorded at the wrong commit."},
		}
		retract.As = alice
		if retracted := tour.send(t, route, retract, ""); retracted.Code != http.StatusOK {
			t.Fatalf("alice's retraction: %d %s", retracted.Code, retracted.Body.String())
		}
		events := tour.eventCount(t, issue.Key, "")
		retract.As = bob
		again := tour.send(t, route, retract, "")
		if code, _ := refusalOf(t, again); again.Code != http.StatusConflict || code != "RESULT_RETRACTED" {
			t.Errorf("bob's retraction of the retracted result: %d %s, want 409 RESULT_RETRACTED", again.Code, again.Body.String())
		}
		retract.As, retract.Body = alice, map[string]any{"state": "retracted", "reason": "Another reason altogether."}
		again = tour.send(t, route, retract, "")
		if code, _ := refusalOf(t, again); again.Code != http.StatusConflict || code != "RESULT_RETRACTED" {
			t.Errorf("alice's retraction with another reason: %d %s, want 409 RESULT_RETRACTED", again.Code, again.Body.String())
		}
		if after := tour.eventCount(t, issue.Key, ""); after != events {
			t.Fatalf("the refused retractions appended %d events, want none", after-events)
		}
	})
}

// TestTheChecksReadNamesWhatLeftACheckNothingToCompareWith is R10's second case (the parent's
// R9.3): GET /api/v2/issues/{key}/checks names compared_nothing no_pull_request for a check whose
// issue resolves to a repository Dispatch can read and holds no pull request, and no_repository
// for one whose issue resolves to no repository, where GET /api/v1/issues/{key}/results keeps the
// one reason no_pull_request_or_branch for both, which the deployed dispatch command keys a
// record by.
func TestTheChecksReadNamesWhatLeftACheckNothingToCompareWith(t *testing.T) {
	handler, _, _, _ := readThroughGitHub(t, "acme/widgets")
	inRepository := issueInRepository(t, handler, "CMPR", "acme/widgets", "In a repository, no pull request")
	uploadDocument(t, handler, "/api/v1/issues/"+inRepository.Key, "verification.md", twoChecks)
	inNone := createInteractionIssue(t, handler, "CMPN", "In no repository", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+inNone.Key, "verification.md", twoChecks)

	type check struct {
		BlockID         string `json:"block_id"`
		ComparedNothing string `json:"compared_nothing"`
	}
	v2Reason := func(t *testing.T, key, blockID string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+key+"/checks", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v2/issues/%s/checks: %d %s", key, response.Code, response.Body.String())
		}
		for _, read := range decodeBody[page[check]](t, response).Items {
			if read.BlockID == blockID {
				return read.ComparedNothing
			}
		}
		t.Fatalf("the /api/v2 checks of %s hold no %s: %s", key, blockID, response.Body.String())
		return ""
	}
	for _, row := range []struct {
		name, key, blockID, v2 string
	}{
		{"a before_merge check of an issue holding no pull request", inRepository.Key, "k1", reasonNoPullRequest},
		{"a before_merge check of an issue in no repository", inNone.Key, "k1", reasonNoRepository},
		{"an after_merge check of an issue in no repository", inNone.Key, "k2", reasonNoRepository},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := v2Reason(t, row.key, row.blockID); got != row.v2 {
				t.Errorf("/api/v2 names compared_nothing %q, want %q", got, row.v2)
			}
			if got := checkOf(t, readResults(t, handler, row.key), row.blockID).ComparedNothing; got != model.ReasonNoPullRequestOrBranch {
				t.Errorf("/api/v1 names compared_nothing %q, want %q", got, model.ReasonNoPullRequestOrBranch)
			}
		})
	}
}

// decodeAnswer is body decoded as a T, failing the test where it is not one.
func decodeAnswer[T any](t *testing.T, body []byte) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return decoded
}
