// issue_as_of_test.go holds two parts of the issue read and the issue listing at a cut that the
// shadows do not give (issue_as_of.go): whether a priority was live at the cut, and the members
// of a component attachment, which keep no history.
package api

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// newIssueAtCutServer is a test server with the test hooks in, and settle, which finishes what
// the writes so far left running: the documents' settlements, the server's detached work and the
// event horizon.
func newIssueAtCutServer(t *testing.T) (http.Handler, *store.Store, func()) {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{testHooks: true})
	settle := func() {
		t.Helper()
		asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice")
		awaitBackgroundWrites(t, served)
		safeHorizon(t, served)
	}
	return handler, database, settle
}

// readAsAlice reads target as alice and fails the test unless it answers 200. It answers the
// decoded body and the members that Dispatch-As-Of-Absent names.
func readAsAlice[T any](t *testing.T, handler http.Handler, target string) (T, []string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[T](t, response), absentPointers(response.Header().Get(contracts.DispatchAsOfAbsentHeader))
}

// advanceIDs is the id of each priority that an issue or a listing row advances.
func advanceIDs(advances []model.IssueAdvance) []string {
	ids := make([]string, len(advances))
	for index, advance := range advances {
		ids[index] = advance.ID
	}
	return ids
}

// listedRow is the listing row of key, and false when the listing holds none.
func listedRow(rows []model.IssueSummary, key string) (model.IssueSummary, bool) {
	index := slices.IndexFunc(rows, func(row model.IssueSummary) bool { return row.Key == key })
	if index < 0 {
		return model.IssueSummary{}, false
	}
	return rows[index], true
}

// TestAPriorityDeletedAfterTheCutIsAdvancedAtTheCut: an issue advances a priority, and the
// priority is deleted after the cut. At the present the issue advances nothing, because a gone
// priority is no priority. At the cut the priority was live, so the issue read and the listing
// show the advance as it stood, and the listing's advances filter finds the issue. The state at
// the cut is the priority's newest state tuple at the cut (prioritiesLiveAtCutTerm), not the
// state that priority_nodes holds now.
func TestAPriorityDeletedAfterTheCutIsAdvancedAtTheCut(t *testing.T) {
	handler, database, settle := newIssueAtCutServer(t)
	const project, priority = "ASPD", "asof-deleted-after-the-cut"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "A priority deleted later"}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/priorities", map[string]any{"id": priority, "title": "Deleted after the cut"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{"project": project, "title": "Advance a priority", "advances": []string{priority}})
	settle()
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, handler, http.MethodDelete, "/api/v1/priorities/"+priority, nil, "alice")
	settle()

	issueTarget := "/api/v1/issues/" + issue.Key
	listTarget := "/api/v1/issues?" + url.Values{"project": {project}}.Encode()
	filterTarget := "/api/v1/issues?" + url.Values{"project": {project}, "advances": {priority}}.Encode()

	if present, _ := readAsAlice[model.Issue](t, handler, issueTarget); len(present.Advances) != 0 {
		t.Fatalf("the present issue read advances %v, want none: the priority is gone, so the read at the cut proves nothing", advanceIDs(present.Advances))
	}
	if read, _ := readAsAlice[model.Issue](t, handler, withAsOf(t, issueTarget, cut)); !slices.Equal(advanceIDs(read.Advances), []string{priority}) {
		t.Errorf("the issue read at the cut advances %v, want [%s]: the priority was live at the cut", advanceIDs(read.Advances), priority)
	}

	present, _ := readAsAlice[[]model.IssueSummary](t, handler, listTarget)
	if row, listed := listedRow(present, issue.Key); !listed || len(row.Advances) != 0 {
		t.Fatalf("the present listing row of %s is %+v (listed %t), want it with no advance", issue.Key, row, listed)
	}
	atCut, _ := readAsAlice[[]model.IssueSummary](t, handler, withAsOf(t, listTarget, cut))
	if row, listed := listedRow(atCut, issue.Key); !listed || !slices.Equal(advanceIDs(row.Advances), []string{priority}) {
		t.Errorf("the listing row of %s at the cut advances %v (listed %t), want [%s]", issue.Key, advanceIDs(row.Advances), listed, priority)
	}

	if present, _ := readAsAlice[[]model.IssueSummary](t, handler, filterTarget); len(present) != 0 {
		t.Fatalf("the present listing of the work that advances %s lists %d issues, want none: the priority is gone", priority, len(present))
	}
	filtered, _ := readAsAlice[[]model.IssueSummary](t, handler, withAsOf(t, filterTarget, cut))
	if _, listed := listedRow(filtered, issue.Key); !listed {
		t.Errorf("the listing at the cut of the work that advances %s lists %d issues without %s: the issue advanced the live priority then", priority, len(filtered), issue.Key)
	}
}

// TestAComponentAttachmentChangedAfterTheCutReadsAsItStoodThen: each issue's component attachment
// changes after the cut, and each one holds the member api now. At the cut the issue read and
// the listing give each attachment as it stood, and never today's members:
//   - an attachment of mode none at the cut (the issue's own, and one an issue inherits from its
//     parent) gives mode none and empty ids and unknown ids, its value then: an attachment of
//     mode none holds no member at any time (issue_component_members cascades from
//     issue_components, and writeIssueComponents writes members only for explicit);
//   - an explicit attachment at the cut (the issue's own, and one an issue inherits) leaves out
//     its ids and its unknown ids and names both absent: the members and the project's component
//     model keep no history (reason M).
func TestAComponentAttachmentChangedAfterTheCutReadsAsItStoodThen(t *testing.T) {
	handler, database, settle := newIssueAtCutServer(t)
	const project = "ASCM"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "Components read at a cut"}, "alice")
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('ASCM', 1, 'example/architecture', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (903, 'ASCM', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('ASCM', 'platform', 'The platform', '', null, 903, 1), ('ASCM', 'api', 'Public API', '', 'platform', 903, 1)`,
	} {
		if _, err := database.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("seed the component model: %v", err)
		}
	}
	none := map[string]any{"mode": "none", "reason": "No component at the cut"}
	platform := map[string]any{"mode": "explicit", "ids": []string{"platform"}}
	create := func(title string, body map[string]any) string {
		t.Helper()
		body["project"], body["title"] = project, title
		return asOfIssue(t, handler, body).Key
	}
	ownNone := create("Its own attachment is none at the cut", map[string]any{"components": none})
	noneParent := create("A parent whose attachment is none at the cut", map[string]any{"components": none})
	inheritsNone := create("It inherits none at the cut", map[string]any{"parent": noneParent})
	explicitParent := create("A parent whose attachment is explicit at the cut", map[string]any{"components": platform})
	inheritsExplicit := create("It inherits an explicit attachment at the cut", map[string]any{"parent": explicitParent})
	ownExplicit := create("Its own attachment is explicit at the cut", map[string]any{"components": platform})
	settle()
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	for _, key := range []string{ownNone, noneParent, explicitParent, ownExplicit} {
		asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
			map[string]any{"components": map[string]any{"mode": "explicit", "ids": []string{"api"}}}, "alice")
	}
	settle()

	issues := []struct {
		name     string
		key      string
		mode     string
		leftOut  bool
		inherits string
	}{
		{name: "its own attachment none at the cut", key: ownNone, mode: "none"},
		{name: "an inherited attachment none at the cut", key: inheritsNone, mode: "none", inherits: noneParent},
		{name: "an inherited attachment explicit at the cut", key: inheritsExplicit, mode: "explicit", leftOut: true, inherits: explicitParent},
		{name: "its own attachment explicit at the cut", key: ownExplicit, mode: "explicit", leftOut: true},
	}
	listTarget := "/api/v1/issues?" + url.Values{"project": {project}}.Encode()
	presentRows, _ := readAsAlice[[]map[string]any](t, handler, listTarget)
	rowsAtCut, listingAbsent := readAsAlice[[]map[string]any](t, handler, withAsOf(t, listTarget, cut))
	for _, issue := range issues {
		t.Run(issue.name, func(t *testing.T) {
			present, _ := readAsAlice[map[string]any](t, handler, "/api/v1/issues/"+issue.key)
			if components, _ := present["components"].(map[string]any); abbreviateValue(components["ids"]) != `["api"]` {
				t.Fatalf("the present read's components are %v, want the member api: the read at the cut proves nothing", components)
			}
			read, absent := readAsAlice[map[string]any](t, handler, withAsOf(t, "/api/v1/issues/"+issue.key, cut))
			for _, problem := range componentsAtCut(read, absent, "/components", issue.mode, issue.inherits, issue.leftOut) {
				t.Errorf("the issue read at the cut: %s", problem)
			}

			index := slices.IndexFunc(rowsAtCut, func(row map[string]any) bool { return row["key"] == issue.key })
			if index < 0 || !slices.ContainsFunc(presentRows, func(row map[string]any) bool { return row["key"] == issue.key }) {
				t.Fatalf("the listing lists %s at the cut (%t) and at the present (%t), want both", issue.key, index >= 0,
					slices.ContainsFunc(presentRows, func(row map[string]any) bool { return row["key"] == issue.key }))
			}
			for _, problem := range componentsAtCut(rowsAtCut[index], listingAbsent, "/*/components", issue.mode, issue.inherits, issue.leftOut) {
				t.Errorf("the listing row at the cut: %s", problem)
			}
		})
	}
}

// componentsAtCut says each way the components of answer, an issue read or a listing row at the
// cut whose answer named absent, are not the attachment that stood then: mode and inherited_from
// as given, and either ids and unknown left out, with pointer/ids and pointer/unknown named, or
// both given as empty lists.
func componentsAtCut(answer map[string]any, absent []string, pointer, mode, inherits string, leftOut bool) []string {
	components, _ := answer["components"].(map[string]any)
	var problems []string
	if components["mode"] != mode {
		problems = append(problems, "its mode is "+abbreviateValue(components["mode"])+", want "+mode)
	}
	if inheritedFrom, _ := components["inherited_from"].(string); inheritedFrom != inherits {
		problems = append(problems, "it inherits from "+abbreviateValue(components["inherited_from"])+", want "+abbreviateValue(inherits))
	}
	for _, member := range []string{"ids", "unknown"} {
		value, given := components[member]
		named := slices.Contains(absent, pointer+"/"+member)
		switch {
		case leftOut && given:
			problems = append(problems, "it gives "+member+" "+abbreviateValue(value)+": the members keep no history, so an explicit attachment leaves them out")
		case leftOut && !named:
			problems = append(problems, "it leaves out "+member+" and names absent only "+abbreviateValue(absent))
		case !leftOut && (!given || abbreviateValue(value) != "[]"):
			problems = append(problems, "it gives "+member+" "+abbreviateValue(value)+" (given "+abbreviateValue(given)+"), want []: an attachment of mode none held no member then")
		}
	}
	// In an issue read the pointers name this issue alone; in a listing they name every row.
	if !leftOut && pointer == "/components" && slices.ContainsFunc(absent, func(named string) bool { return named == pointer+"/ids" || named == pointer+"/unknown" }) {
		problems = append(problems, "it names "+abbreviateValue(absent)+" absent for an attachment of mode none")
	}
	return problems
}
