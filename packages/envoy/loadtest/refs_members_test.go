package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAReferencesETagIsRecordedOnlyInDispatchsShape: Dispatch's references answer, or a
// stand-in's in its place, sends an ETag that echoes the read's bearer and quotes a credential's
// URL (answer_members_test.go), and refs-fetch's TSV recorded it as it came. The TSV holds none of
// it. The control: an ETag in Dispatch's shape is still recorded as it came. Captures
// loadtest.answer-members/refs-etag.
func TestAReferencesETagIsRecordedOnlyInDispatchsShape(t *testing.T) {
	dispatchETag := `"` + strings.Repeat("fedcba9876543210", 4) + `"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := dispatchETag
		if r.URL.Path == "/api/v1/issues/LGA-1/references" {
			etag = echoed(r)
		}
		w.Header().Set("ETag", etag)
		writeJSONAnswer(w, http.StatusOK, map[string]any{"members": []any{}})
	}))
	t.Cleanup(srv.Close)
	out := filepath.Join(t.TempDir(), "refs.tsv")
	sf := seedFile{Issues: []seedIssue{{Key: "LGA-1"}, {Key: "LGA-2"}}}
	if err := refsFetch(context.Background(), newAPI(srv.URL, memberBearer, 2), sf, out); err != nil {
		t.Fatal(err)
	}
	tsv, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	redacttest.RequireNoPiece(t, "loadtest.answer-members/refs-etag", string(tsv), memberBearer)
	if !strings.Contains(string(tsv), "LGA-2\t200\t"+dispatchETag+"\t") {
		t.Errorf("the TSV is %q, want LGA-2's ETag in Dispatch's shape recorded as it came", tsv)
	}
}
