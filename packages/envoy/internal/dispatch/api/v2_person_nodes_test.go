package api

import (
	"net/http"
	"strings"
	"testing"
)

// GET /api/v2/nodes?person=<login> is the /api/v2 successor of main's GET
// /api/v1/people/{login}/nodes, which keeps main's bare answer (dispatch://INFRA-4071/spec R3). It
// holds the parent's shape rules (dispatch://INFRA-2946/spec R1): (a) the list envelope
// {items, total, limit, offset} with its bounds, 50 by default and 200 at most, a limit past 200
// refused 400 naming the bound (R1.4); (b) a top-level read across projects, which takes project as
// a repeatable filter with no default project and declares itself cross-project (R1.3), and takes
// the person as a filter, since a lookup by a key that is not the list's own is a filter (R1.2).
// person is required, and a person filter value that names no person the reader may see answers
// the empty page, as a filter value that names nothing answers on every /api/v2 list; /api/v1
// keeps its 404 NOT_FOUND for the same person.
func TestTheV2PersonNodesAreACrossProjectListInTheEnvelope(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "PAGE", "A person's page", "# Spec\n\nA passage.\n")
	second := createInteractionIssue(t, handler, "OTHER", "Another project's issue", "# Spec\n\nA passage.\n")
	for _, issue := range []string{first.Key, second.Key} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": "Bob's remark on " + issue}, "bob"); response.Code != http.StatusCreated {
			t.Fatalf("bob's comment on %s: status=%d body=%s", issue, response.Code, response.Body.String())
		}
	}
	type pageV2 struct {
		Items  []personNode `json:"items"`
		Total  int          `json:"total"`
		Limit  int          `json:"limit"`
		Offset int          `json:"offset"`
	}
	read := func(target string) pageV2 {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s, want 200", target, response.Code, response.Body.String())
		}
		for _, member := range []string{`"rows"`, `"person"`} {
			if strings.Contains(response.Body.String(), member+":") {
				t.Fatalf("GET %s answers %s beside the envelope: %s", target, member, response.Body.String())
			}
		}
		return decodeBody[pageV2](t, response)
	}

	// (a) The envelope, its default bound, and a page.
	every := read("/api/v2/nodes?person=bob")
	if every.Total != 2 || len(every.Items) != 2 || every.Limit != 50 || every.Offset != 0 {
		t.Fatalf("bob's nodes = %+v, want his two comments in the envelope at limit 50, offset 0", every)
	}
	if paged := read("/api/v2/nodes?person=bob&limit=1&offset=1"); paged.Total != 2 || len(paged.Items) != 1 ||
		paged.Items[0].ID != every.Items[1].ID || paged.Limit != 1 || paged.Offset != 1 {
		t.Fatalf("bob's nodes at limit 1, offset 1 = %+v, want the second of %+v", paged, every.Items)
	}
	if read("/api/v2/nodes?person=bob&limit=200").Limit != 200 {
		t.Fatal("a limit of 200, the bound, is not taken")
	}
	past := dispatchRequest(t, handler, http.MethodGet, "/api/v2/nodes?person=bob&limit=201", nil, "alice")
	if past.Code != http.StatusBadRequest || responseCode(t, past) != "INVALID_LIMIT" || !strings.Contains(past.Body.String(), "200") {
		t.Fatalf("a limit of 201: status=%d body=%s, want 400 INVALID_LIMIT naming 200", past.Code, past.Body.String())
	}

	// (b) The project filter, repeatable, none keeping every project's; and the row's declaration.
	if one := read("/api/v2/nodes?person=bob&project=PAGE"); one.Total != 1 || len(one.Items) != 1 ||
		!strings.HasPrefix(one.Items[0].Href, "/issues/"+first.Key+"/") {
		t.Fatalf("bob's nodes of PAGE = %+v, want his one comment on %s", one, first.Key)
	}
	if both := read("/api/v2/nodes?person=bob&project=PAGE&project=OTHER"); both.Total != 2 {
		t.Fatalf("bob's nodes of PAGE and OTHER = %+v, want both comments", both)
	}
	if none := read("/api/v2/nodes?person=bob&project=NONE"); none.Total != 0 || len(none.Items) != 0 {
		t.Fatalf("bob's nodes of a project he wrote nothing in = %+v, want no row", none)
	}
	bad := dispatchRequest(t, handler, http.MethodGet, "/api/v2/nodes?person=bob&project=not+a+key", nil, "alice")
	if bad.Code != http.StatusBadRequest || responseCode(t, bad) != "INVALID_PROJECT" {
		t.Fatalf("a project that is no key: status=%d body=%s, want 400 INVALID_PROJECT", bad.Code, bad.Body.String())
	}
	declared := false
	for _, route := range (&server{}).routes() {
		if route.Method == http.MethodGet && route.Pattern == "/api/v2/nodes" {
			declared = route.CrossProject && route.Resource == "nodes" && route.Kind == kindCollection
		}
	}
	if !declared {
		t.Fatal("GET /api/v2/nodes is not mounted as a cross-project collection of nodes")
	}

	// person is required, and names nothing as a filter value names nothing.
	for _, target := range []string{"/api/v2/nodes", "/api/v2/nodes?person=", "/api/v2/nodes?person=+"} {
		unnamed := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if unnamed.Code != http.StatusBadRequest || responseCode(t, unnamed) != "INVALID_QUERY" ||
			!strings.Contains(unnamed.Body.String(), "person") {
			t.Fatalf("GET %s: status=%d body=%s, want 400 INVALID_QUERY naming person", target, unnamed.Code, unnamed.Body.String())
		}
	}
	if nobody := read("/api/v2/nodes?person=nobody-at-all"); nobody.Total != 0 || len(nobody.Items) != 0 {
		t.Fatalf("a login nobody holds on /api/v2 = %+v, want the empty page", nobody)
	}
	missingV1 := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/nobody-at-all/nodes", nil, "alice")
	if missingV1.Code != http.StatusNotFound || responseCode(t, missingV1) != "NOT_FOUND" {
		t.Fatalf("a login nobody holds on /api/v1: status=%d body=%s, want 404 NOT_FOUND", missingV1.Code, missingV1.Body.String())
	}
	v1 := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/bob/nodes", nil, "alice")
	if v1.Code != http.StatusOK || !strings.Contains(v1.Body.String(), `"rows"`) || strings.Contains(v1.Body.String(), `"items"`) {
		t.Fatalf("bob's page on /api/v1: status=%d body=%s, want main's answer under rows", v1.Code, v1.Body.String())
	}
	if answer := decodeBody[personNodes](t, v1); answer.Person != "bob" || answer.Total != 2 || len(answer.Rows) != 2 {
		t.Fatalf("bob's page on /api/v1 = %+v, want his two comments", answer)
	}
}
