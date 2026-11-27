package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The client reads an issue's links from Dispatch's detail of it (GET /api/v1/issues/<KEY>,
// HTTPClient.GetIssue): each link's URL, in Dispatch's order, and that the detail named its links
// (Issue.ExternalLinks, Issue.LinksKnown), which the boot's read of a running issue's links takes
// (the daemon's readLinksAtBoot, intake.DetailLinks). testdata/links/detail-acme-5768.json is the
// captured detail of ACME-5768, which links two pull requests, acme/widgets#5481 in its own
// repository and acme/deploy#6990 in another (testdata/links/README.md).
//
// The faults it catches: a client that reads no external_links, under which the detail names no
// links and says nothing of them, and one that drops or reorders a link.
func TestTheClientReadsTheLinksOfAnIssueFromItsDetail(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "links", "detail-acme-5768.json"))
	if err != nil {
		t.Fatalf("read detail-acme-5768.json: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues/ACME-5768" {
			t.Errorf("request = %s %s, want GET /api/v1/issues/ACME-5768", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	got, err := New(server.URL, BearerTransport(dispatchToken, nil)).GetIssue(context.Background(), "ACME-5768")
	if err != nil {
		t.Fatalf("read the detail of ACME-5768: %v", err)
	}
	want := []string{"https://github.com/acme/widgets/pull/5481", "https://github.com/acme/deploy/pull/6990"}
	if !got.LinksKnown || !slices.Equal(got.ExternalLinks, want) {
		t.Fatalf("the detail of ACME-5768 names the links %q, known %t; want %q, known", got.ExternalLinks, got.LinksKnown, want)
	}
}
