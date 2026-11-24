package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABlobInAnotherEncodingNamesNoEncodingItWrote: GitHub, or whatever answers in its place,
// answers a note's blob under an encoding that is not base64 and that quotes a credential's URL.
// DirFiles' error says the blob is in an encoding that is not base64, and prints none of the
// encoding the answer wrote. Captures githubapp.answer-words/blob-encoding.
func TestABlobInAnotherEncodingNamesNoEncodingItWrote(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":"","encoding":%q}`, redacttest.Code)
	})
	_, err := client.DirFiles(context.Background(), "ghs_test", "acme", "web", Dir{SHA: "t1", Files: []DirFile{{Name: "a.md", Path: "docs/a.md", SHA: "b1", Size: 1}}})
	if err == nil {
		t.Fatal("DirFiles answered no error")
	}
	redacttest.RequireNone(t, "githubapp.answer-words/blob-encoding", err.Error())
	if want := "blob b1 (docs/a.md) is in an encoding that is not base64"; !strings.Contains(err.Error(), want) {
		t.Errorf("DirFiles' error is %q, want %q in it", err, want)
	}
}
