package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// GET /api/v2/issues/{key}/references walks the closure GET /api/v1/issues/{key}/references
// walks, under the same bound (issueClosure): a walk past referencesClosureBound answers 503
// REFERENCES_TIMEOUT at about that bound, as /api/v1 does. refs is locked in another connection,
// so the walk waits however fast it would run; the read before the lock answers 200.
func TestV2IssueReferencesRefuseAtTheClosureBound(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	target := "/api/v2/issues/" + issue.Key + "/references"
	if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("v2 references before the lock: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	holdLock(t, database, "lock table refs in access exclusive mode")
	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	took := time.Since(started)
	if code, text := refusalOf(t, response); response.Code != http.StatusServiceUnavailable || code != "REFERENCES_TIMEOUT" ||
		!strings.Contains(text, referencesClosureBound.String()) {
		t.Fatalf("v2 references past the bound = %d %s %q, want 503 REFERENCES_TIMEOUT naming the %s bound", response.Code, code, text, referencesClosureBound)
	}
	if took < referencesClosureBound || took > referencesClosureBound+3*time.Second {
		t.Errorf("v2 references past the bound answered after %s, want about its %s bound", took, referencesClosureBound)
	}
}
