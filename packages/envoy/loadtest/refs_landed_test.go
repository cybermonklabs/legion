package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The seed's landed write after main's #3000 (dispatch://INFRA-1523, test round at 869d340e). A
// version of a reference target that landed with no answer to give (409 KEY_COMMITTED) was made,
// and the seed goes on to the next version (writeRefTargets). Since #3000 each later version reads
// the document's token first (refDocumentToken, GET /api/v1/artifacts/{id}/text) by the id an
// earlier version's answer gave, and a landed version's answer gives none, so the seed must keep
// the document it already knows rather than stop. This fake server answers the read as Dispatch
// does: 200 with the document's token, and no Idempotency-Key on it.
func TestRefSeedGoesOnPastALandedVersionToTheDocumentItAlreadyKnows(t *testing.T) {
	p := refPlan{
		targets: []refTarget{{n: 0, issue: "LGB-1", versions: 3, writers: []int{0}, order: []int{0, 0, 0}}},
	}
	landed := map[string]bool{refTargetKey(0, 2): true}
	var mu sync.Mutex
	var writes, reads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			reads = append(reads, r.URL.Path)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		key := r.Header.Get(contracts.IdempotencyKeyHeader)
		mu.Lock()
		writes = append(writes, key)
		mu.Unlock()
		if landed[key] {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "landed", "code": contracts.IdempotencyKeyCommitted})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"artifact":{"id":"a"}}`)
	}))
	defer srv.Close()

	versions, err := writeRefTargets(context.Background(), newAPI(srv.URL, "token", 2), p)
	if err != nil || versions != 3 {
		t.Fatalf("the targets wrote %d versions (%v), want all 3: the landed version 2 is made, and version 3 reads the document version 1 created", versions, err)
	}
	if want := []string{refTargetKey(0, 1), refTargetKey(0, 2), refTargetKey(0, 3)}; !slices.Equal(writes, want) {
		t.Errorf("the seed sent %v, want each version once: %v", writes, want)
	}
	if want := []string{"/api/v1/artifacts/a/text", "/api/v1/artifacts/a/text"}; !slices.Equal(reads, want) {
		t.Errorf("the seed read %v, want the document's token read before versions 2 and 3: %v", reads, want)
	}
}
