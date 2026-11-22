package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An external reference resolves under either name of a rename pair: `sjawhar/legion#1955` and
// `cybermonklabs/legion#1955` reach the issue that links #1955 under the old name, and both
// reach the issue that links #2001 under the new name, in any case. A repository that only
// begins with the old name is no name of the pair. The pair is seeded by SQL before the server
// is built, since a server reads the pairs once, as it is built.
func TestAnExternalReferenceResolvesUnderEitherNameOfARenamePair(t *testing.T) {
	t.Parallel()
	database, renames := renamedRepositoryStore(t, "LEGN", "sjawhar/legion", "cybermonklabs/legion")
	handler, _, _ := newTestServer(t, testServerOptions{store: database, renames: renames})
	linked := func(url string) string {
		t.Helper()
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "LEGN", "title": "Linked to " + url, "force": true,
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create the issue linked to %s: status=%d body=%s", url, created.Code, created.Body.String())
		}
		key := decodeBody[model.Issue](t, created).Key
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"external_links": []map[string]string{{"url": url}},
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("link %s to %s: status=%d body=%s", key, url, response.Code, response.Body.String())
		}
		return key
	}
	underOld := linked("https://github.com/sjawhar/legion/issues/1955")
	underNew := linked("https://github.com/cybermonklabs/legion/pull/2001")

	for ref, want := range map[string]string{
		"sjawhar/legion%231955":       underOld,
		"cybermonklabs/legion%231955": underOld,
		"SJawhar/Legion%231955":       underOld,
		"cybermonklabs/legion%232001": underNew,
		"sjawhar/legion%232001":       underNew,
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref="+ref, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("resolve %s: status=%d body=%s", ref, response.Code, response.Body.String())
		}
		if got := decodeBody[issueRefResolved](t, response).Key; got != want {
			t.Fatalf("resolve %s = %s, want %s", ref, got, want)
		}
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref=sjawhar/legion-smoke%231955", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("resolve sjawhar/legion-smoke#1955: status=%d body=%s, want 404: it names no repository of the pair", response.Code, response.Body.String())
	}
}
