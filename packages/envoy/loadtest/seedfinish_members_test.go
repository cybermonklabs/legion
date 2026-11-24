package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheEmbeddingGatesRecordNamesTheEmbedderAsTheRuleReadsIt: Dispatch's coverage report, or a
// stand-in's in its place, names an embedder identity and model that echo the read's bearer and
// quote a credential's URL (answer_members_test.go), and the gate's record (-embed-progress) wrote
// them as they came. The record names them as the rule reads them with the read's record. The
// control: the record still names the embedder, its URL as the rule prints it and its place as it
// came. Captures loadtest.answer-members/embed-record.
func TestTheEmbeddingGatesRecordNamesTheEmbedderAsTheRuleReadsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		none := map[string]any{"items": 0, "current": 0, "lacking": 0, "failed": 0, "oldest_lacking_seconds": nil}
		issue := map[string]any{"items": 4, "current": 4, "lacking": 0, "failed": 0, "oldest_lacking_seconds": nil}
		writeJSONAnswer(w, http.StatusOK, map[string]any{"identity": echoed(r), "place": "azure", "model": echoed(r), "total": issue,
			"kinds": map[string]any{"issue": issue, "document_block": none, "comment_block": none, "message_block": none, "ask_part": none}})
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "seedfinish.embed.json")
	var logged bytes.Buffer
	f, _ := fakeEmbedFinisher(time.Hour, dispatchCoverage{a: newAPI(srv.URL, memberBearer, 2)}, path, &logged)
	if err := f.waitEmbedded(context.Background()); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	redacttest.RequireNoPiece(t, "loadtest.answer-members/embed-record", string(written)+"\n"+logged.String(), memberBearer)
	record := readCoverageRecord(t, path)
	if record.Identity == nil || !strings.Contains(*record.Identity, redacttest.CodeShown) || record.Place == nil || *record.Place != "azure" {
		t.Errorf("the record is %s, want the embedder's identity as the rule reads it and its place", written)
	}
}
