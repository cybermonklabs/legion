package api

// v2_priority_successors_test.go holds what #4316 gave the /api/v1 priority and issue routes to
// their /api/v2 successors: the issue listing's own=true, the description_tree_exact beside each
// priority answer's description_tree, and the priority write's expected. The refusals of expected
// are drawn on the route tour too (tourRefusals).

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// GET /api/v2/issues takes own=true as GET /api/v1/issues does: of the issues an advances value
// answers, it keeps those whose own row names the priority with no issue above them carrying it.
// A parent that states a priority, a child that inherits it and a child that restates it are all
// three answered without own and the parent alone with it, and none keeps nothing under own, in
// both versions alike.
func TestTheV2IssueListTakesOwnAsTheV1ListDoes(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "OWN")
	priority := createTestPriority(t, handler, map[string]any{"id": "keep-the-lights", "title": "Keep the lights"}, "alice").ID
	parent := createAdvancingIssue(t, handler, map[string]any{
		"project": "OWN", "title": "States the priority", "force": true, "advances": []string{priority},
	}, "alice")
	inherits := createAdvancingIssue(t, handler, map[string]any{
		"project": "OWN", "title": "Inherits the priority", "force": true, "parent": parent.Key,
	}, "alice")
	restates := createAdvancingIssue(t, handler, map[string]any{
		"project": "OWN", "title": "Restates the priority", "force": true, "parent": parent.Key, "advances": []string{priority},
	}, "alice")
	v2Keys := func(t *testing.T, target string) []string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		keys := []string{}
		for _, row := range decodeBody[page[model.IssueSummary]](t, response).Items {
			keys = append(keys, row.Key)
		}
		slices.Sort(keys)
		return keys
	}
	for _, each := range []struct {
		query string
		want  []string
	}{
		{"advances=" + priority, []string{parent.Key, inherits.Key, restates.Key}},
		{"advances=" + priority + "&own=true", []string{parent.Key}},
		{"advances=any&own=true", []string{parent.Key}},
		{"advances=none&own=true", []string{}},
	} {
		want := slices.Sorted(slices.Values(each.want))
		v1 := slices.Sorted(slices.Values(listedIssueKeys(t, handler, "/api/v1/issues?project=OWN&"+each.query)))
		if !slices.Equal(v1, want) {
			t.Errorf("GET /api/v1/issues?project=OWN&%s answers %v, want %v", each.query, v1, want)
		}
		if v2 := v2Keys(t, "/api/v2/issues?project=OWN&"+each.query); !slices.Equal(v2, want) {
			t.Errorf("GET /api/v2/issues?project=OWN&%s answers %v, want %v as /api/v1 answers", each.query, v2, want)
		}
		if v2 := v2Keys(t, "/api/v2/projects/OWN/issues?"+each.query); !slices.Equal(v2, want) {
			t.Errorf("GET /api/v2/projects/OWN/issues?%s answers %v, want %v as /api/v1 answers", each.query, v2, want)
		}
	}
}

// The /api/v2 priority routes answer description_tree_exact beside description_tree as the
// /api/v1 ones do (TestAPriorityDescriptionTreeSaysWhetherItIsExact): the create's answer, each
// listed row and the write's answer. The write takes expected as PATCH /api/v1/priorities/{id}
// does: one naming the description stored lands, and one naming a description no longer stored
// is 409 PRIORITY_CHANGED naming the one stored now and writes nothing.
func TestTheV2PriorityRoutesAnswerExactnessAndTakeExpected(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	const (
		id      = "exact-heading"
		exact   = "# Why\n\nEvery machine runs the daemon."
		refused = "# Why\n\n<div>now HTML</div>"
	)
	created := decodeBody[model.Priority](t, priorityTreesRequest(t, handler, http.MethodPost, "/api/v2/priorities",
		map[string]any{"id": id, "title": "Exact heading", "description": exact}, http.StatusCreated))
	requireExactness(t, "the /api/v2 create's answer", created, true)

	listed := decodeBody[page[model.PriorityRow]](t, treesRequest(t, handler, "/api/v2/priorities", true))
	index := slices.IndexFunc(listed.Items, func(row model.PriorityRow) bool { return row.ID == id })
	if index < 0 {
		t.Fatalf("GET /api/v2/priorities lists no %s: %+v", id, listed.Items)
	}
	requireExactness(t, "the /api/v2 listing's row", listed.Items[index].Priority, true)

	patched := decodeBody[model.Priority](t, priorityTreesRequest(t, handler, http.MethodPatch, "/api/v2/priorities/"+id,
		map[string]any{"description": refused, "expected": map[string]any{"description": exact}}, http.StatusOK))
	requireExactness(t, "the /api/v2 write's answer", patched, false)

	stale := priorityTreesRequest(t, handler, http.MethodPatch, "/api/v2/priorities/"+id,
		map[string]any{"description": "Written over a change unseen", "expected": map[string]any{"description": exact}},
		http.StatusConflict)
	code, text := refusalOf(t, stale)
	if want := `its description is now "# Why\n\n<div>now HTML</div>"`; code != apicode.PriorityChanged.Name() || !strings.Contains(text, want) {
		t.Fatalf("a write from a description no longer stored: code=%s error=%q, want %s naming %s",
			code, text, apicode.PriorityChanged.Name(), want)
	}
	if got := readTestPriority(t, handler, id).Description; got != refused {
		t.Fatalf("the refused write left the description %q, want %q", got, refused)
	}
}
