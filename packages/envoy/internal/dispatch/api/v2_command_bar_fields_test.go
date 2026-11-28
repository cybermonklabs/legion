package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The fields #5345 added to /api/v1 for the command bar - the issue read's blocks and
// comment_count, a search hit's author, and each tool argument's bounds in the catalogue - are
// answered by each route's /api/v2 successor too, in the /api/v2 shape: the issue read's two as
// members its include= names, the hit's author and the arguments' bounds as members of the rows
// they belong to.

// GET /api/v2/issues/{key} answers blocks, the keys of the issues whose blocked_by names the
// issue, and comment_count, how many comments GET /api/v2/issues/{key}/comments lists, each
// where include= names it and never where it does not, with the values GET /api/v1/issues/{key}
// answers. An issue that blocks nothing and holds no comment answers blocks [] and comment_count
// 0 when they are named, where /api/v1 leaves the count out.
func TestTheV2IssueReadNamesWhatItBlocksAndCountsItsComments(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createReferenceAPIProject(t, handler, "BARV")
	blocker := createOutcomeIssue(t, handler, "BARV", "API: rate limit headers")
	waiting := createOutcomeIssue(t, handler, "BARV", "API: client retries on 429")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+waiting, map[string]any{
		"blocked_by": []string{blocker},
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("block %s on %s: status=%d body=%s", waiting, blocker, patched.Code, patched.Body.String())
	}
	for _, body := range []string{"Which headers does the limit send?", "Retry-After and X-RateLimit-Remaining."} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+blocker+"/comments",
			map[string]any{"body": body}, "chen"); response.Code != http.StatusCreated {
			t.Fatalf("comment on %s: status=%d body=%s", blocker, response.Code, response.Body.String())
		}
	}
	read := func(path string) map[string]json.RawMessage {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		return decodeBody[map[string]json.RawMessage](t, response)
	}
	members := func(key string) (blocks []string, comments int) {
		t.Helper()
		answer := read("/api/v2/issues/" + key + "?" + url.Values{"include": {"blocks,comment_count"}}.Encode())
		raw, held := answer["blocks"]
		if !held {
			t.Fatalf("the /api/v2 read of %s with include=blocks carries no blocks: %v", key, answer)
		}
		if err := json.Unmarshal(raw, &blocks); err != nil || blocks == nil {
			t.Fatalf("the /api/v2 read of %s carries blocks %s, want a list: %v", key, raw, err)
		}
		raw, held = answer["comment_count"]
		if !held {
			t.Fatalf("the /api/v2 read of %s with include=comment_count carries no comment_count: %v", key, answer)
		}
		if err := json.Unmarshal(raw, &comments); err != nil {
			t.Fatalf("the /api/v2 read of %s carries comment_count %s: %v", key, raw, err)
		}
		return blocks, comments
	}

	blocks, comments := members(blocker)
	if !slices.Equal(blocks, []string{waiting}) || comments != 2 {
		t.Errorf("the /api/v2 read of %s answers blocks %v and comment_count %d, want [%s] and 2", blocker, blocks, comments, waiting)
	}
	v1 := decodeBody[struct {
		Blocks       []string `json:"blocks"`
		CommentCount int      `json:"comment_count"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+blocker, nil, "alice"))
	if !slices.Equal(blocks, v1.Blocks) || comments != v1.CommentCount {
		t.Errorf("the /api/v2 read of %s answers blocks %v and comment_count %d, where /api/v1 answers %v and %d",
			blocker, blocks, comments, v1.Blocks, v1.CommentCount)
	}
	if blocks, comments := members(waiting); len(blocks) != 0 || comments != 0 {
		t.Errorf("the /api/v2 read of %s, which blocks nothing and holds no comment, answers blocks %v and comment_count %d, want [] and 0",
			waiting, blocks, comments)
	}
	for _, member := range []string{"blocks", "comment_count"} {
		if value, held := read("/api/v2/issues/" + blocker)[member]; held {
			t.Errorf("the /api/v2 read of %s with no include= carries %s %s, want none", blocker, member, value)
		}
	}
}

// GET /api/v2/search-results names the author of a comment's, a message's and an ask's hit, the
// actor that wrote it, as GET /api/v1/search does, and an issue's hit and a document's carry none.
func TestTheV2SearchNamesWhoWroteACommentAMessageAndAnAsk(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCV", "Navigation instruments", "# Instruments\n\nThe astrolabe measures altitude.\n")
	written := func(path string, body map[string]any) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+path, body, "bob")
		if response.Code != http.StatusCreated {
			t.Fatalf("POST %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		return decodeBody[struct {
			ID string `json:"id"`
		}](t, response).ID
	}
	comment := written("/comments", map[string]any{"body": "Replace the sextant diagram."})
	message := written("/messages", map[string]any{"body": "Compass calibration done."})
	ask := written("/asks", map[string]any{"question": "Keep the quadrant?", "options": []map[string]string{{"label": "Yes"}}})
	for _, row := range []struct{ query, kind, id, author string }{
		{"sextant", "comment", comment, "bob"},
		{"compass", "message", message, "bob"},
		{"quadrant", "ask", ask, "bob"},
		{"instruments", "issue", issue.Key, ""},
		{"astrolabe", "document", issue.PrimaryArtifactID, ""},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/search-results?"+url.Values{"q": {row.query}}.Encode(), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("search %q: status=%d body=%s", row.query, response.Code, response.Body.String())
		}
		var hit *model.SearchResult
		for _, result := range decodeBody[searchResultsV2](t, response).Items {
			if result.Kind == row.kind && result.ID == row.id {
				hit = &result
			}
		}
		switch {
		case hit == nil:
			t.Errorf("search %q answered no %s %s: %s", row.query, row.kind, row.id, response.Body.String())
		case row.author == "" && hit.Author != nil:
			t.Errorf("the %s hit names the author %+v, want none", row.kind, *hit.Author)
		case row.author != "" && (hit.Author == nil || hit.Author.ID != row.author):
			t.Errorf("the %s hit names the author %+v, want %s, who wrote it", row.kind, hit.Author, row.author)
		}
	}
}

// GET /api/v2/going-actions serves each argument's bounds as its tool's spec states them, min
// and max, the ones GET /api/v1/going/catalogue serves: dispatch_issue_update's labels_add holds
// at most contracts.MaxIssueLabels labels, each 1 to contracts.IssueLabelMax characters, the
// bounds PATCH /api/v2/issues/{key} enforces, and its route argument, which states none, carries
// neither.
func TestTheV2CatalogueServesEachArgumentsBounds(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	argument := func(fields []contracts.ToolField, name string) contracts.ToolField {
		t.Helper()
		at := slices.IndexFunc(fields, func(field contracts.ToolField) bool { return field.Name == name })
		if at < 0 {
			t.Fatalf("dispatch_issue_update takes no %s: %+v", name, fields)
		}
		return fields[at]
	}
	response := dispatchRequest(t, handler, http.MethodGet, goingActionsPath, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", goingActionsPath, response.Code, response.Body.String())
	}
	actions := decodeBody[page[goingActionV2]](t, response).Items
	at := slices.IndexFunc(actions, func(action goingActionV2) bool { return action.Action == "dispatch_issue_update" })
	if at < 0 {
		t.Fatalf("GET %s holds no dispatch_issue_update: %s", goingActionsPath, response.Body.String())
	}
	request := actions[at].Request
	bound := func(value *float64) float64 {
		if value == nil {
			return -1
		}
		return *value
	}
	labels := argument(request, "labels_add")
	if bound(labels.Min) != -1 || bound(labels.Max) != contracts.MaxIssueLabels || labels.Item == nil ||
		bound(labels.Item.Min) != 1 || bound(labels.Item.Max) != contracts.IssueLabelMax {
		t.Fatalf("labels_add carries min %v and max %v, each label %+v; want max %d, each label 1 to %d",
			labels.Min, labels.Max, labels.Item, contracts.MaxIssueLabels, contracts.IssueLabelMax)
	}
	if route := argument(request, "route"); route.Min != nil || route.Max != nil {
		t.Errorf("route, which states no bound, carries min %v and max %v, want neither", route.Min, route.Max)
	}

	v1 := dispatchRequest(t, handler, http.MethodGet, "/api/v1/going/catalogue", nil, "alice")
	if v1.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/going/catalogue: status=%d body=%s", v1.Code, v1.Body.String())
	}
	for _, action := range decodeBody[goingCatalogueBody](t, v1).Actions {
		if action.Action != "dispatch_issue_update" {
			continue
		}
		if served, want := argument(action.Request, "labels_add"), labels; bound(served.Max) != bound(want.Max) ||
			served.Item == nil || bound(served.Item.Max) != bound(want.Item.Max) || bound(served.Item.Min) != bound(want.Item.Min) {
			t.Errorf("/api/v1 serves labels_add %+v, /api/v2 %+v: want the same bounds", served, want)
		}
		return
	}
	t.Fatalf("GET /api/v1/going/catalogue holds no dispatch_issue_update")
}
