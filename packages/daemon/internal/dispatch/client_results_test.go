package dispatch

//ci:reads packages/contracts/fixtures/dispatch-results/* TestIssueResultsReadsEachCheckState and TestGetIssueKeepsEachChild read Dispatch's captured answers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// resultsFixture is one of Dispatch's answers of its results read, as Dispatch's own
// TestTheResultsReadOfEachState wrote it (packages/contracts/fixtures/dispatch-results).
func resultsFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "dispatch-results", name+".json"))
	if err != nil {
		t.Fatalf("read the fixture %s: %v", name, err)
	}
	return body
}

// The read of an issue's checks decodes every field the close reads of Dispatch's own answers,
// one fixture for each state: what each check is compared with and why nothing is, its answer and
// reason, its deciding results and their commit, the current deploy of a live check, and each
// subject of a before_merge check. The read asks GET /api/v1/issues/{key}/results with no query,
// and the read at one commit asks ?at=<commit>.
func TestIssueResultsReadsEachCheckState(t *testing.T) {
	const (
		merge = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
		c1    = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
		c2    = "2222222222222222222222222222222222222222"
		c0    = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"
	)
	onMain := []CheckBasis{{Kind: BasisBranch, Branch: "main", MergeCommit: merge}}
	// The fixture's placeholder of the deploy's time: the second time of its answer, after the result.
	deployed := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	for _, tc := range []struct {
		fixture, check string
		want           CheckBlock
	}{
		{"code-pass", "k5", CheckBlock{BlockID: "k5", When: CheckAfterMerge, ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsYes, Results: []string{"00000000-0000-4000-8000-000000000002"}, Commit: c1}}},
		{"code-fail", "k5", CheckBlock{BlockID: "k5", When: CheckAfterMerge, ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsNo, Reason: PassStandsReasonFail, Results: []string{"00000000-0000-4000-8000-000000000002"}, Commit: c1}}},
		{"code-pass-then-newer-fail", "k5", CheckBlock{BlockID: "k5", When: CheckAfterMerge, ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsNo, Reason: PassStandsReasonFail, Commit: c2}}},
		{"code-pass-and-fail", "k5", CheckBlock{BlockID: "k5", When: CheckAfterMerge, ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsUnknown, Reason: PassStandsReasonPassAndFail, Commit: c1}}},
		{"code-no-result", "k5", CheckBlock{BlockID: "k5", When: CheckAfterMerge, ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsNo, Reason: PassStandsReasonNoResult}}},
		{"code-no-merge", "k5", CheckBlock{BlockID: "k5", When: CheckAfterMerge, ComparedWith: []CheckBasis{}, ComparedNothing: "not_merged",
			PassStands: PassStands{Answer: PassStandsNo, Reason: PassStandsReasonNoResult}}},
		{"live-no-deploy", "live", CheckBlock{BlockID: "live", When: CheckAfterMerge, Subject: CheckSubjectProduction, Program: "renderer", ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsUnknown, Reason: PassStandsReasonNoDeploy}}},
		{"live-no-result-from-deploy", "live", CheckBlock{BlockID: "live", When: CheckAfterMerge, Subject: CheckSubjectProduction, Program: "renderer", ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsUnknown, Reason: PassStandsReasonNoResultFromDeploy, Deploy: &CheckDeploy{Program: "renderer", Commit: c1, DeployedAt: deployed}}}},
		{"live-program-not-named", "live", CheckBlock{BlockID: "live", When: CheckAfterMerge, Subject: CheckSubjectProduction, ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsUnknown, Reason: PassStandsReasonProgramNotNamed}}},
		{"live-deploy-not-merged", "live", CheckBlock{BlockID: "live", When: CheckAfterMerge, Subject: CheckSubjectProduction, Program: "renderer", ComparedWith: onMain,
			PassStands: PassStands{Answer: PassStandsUnknown, Reason: PassStandsReasonDeployNotMerged, Deploy: &CheckDeploy{Program: "renderer", Commit: c0, DeployedAt: deployed}}}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			body := resultsFixture(t, tc.fixture)
			var asked []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBearer(t, r)
				asked = append(asked, r.Method+" "+r.URL.RequestURI())
				_, _ = w.Write(body)
			}))
			defer server.Close()
			client := New(server.URL, BearerTransport(dispatchToken, nil))
			read, err := client.IssueResults(context.Background(), "ACME-1")
			if err != nil {
				t.Fatalf("read the results: %v", err)
			}
			if _, err := client.IssueResultsAt(context.Background(), "ACME-1", c1); err != nil {
				t.Fatalf("read the results at %s: %v", c1, err)
			}
			if want := []string{"GET /api/v1/issues/ACME-1/results", "GET /api/v1/issues/ACME-1/results?at=" + c1}; !reflect.DeepEqual(asked, want) {
				t.Fatalf("asked %q, want %q", asked, want)
			}
			if read.Issue != "ACME-1" || read.VerificationArtifactID == "" || len(read.Checks) != 1 {
				t.Fatalf("the read = %+v, want ACME-1's one check and its document", read)
			}
			got := read.Checks[0]
			got.ArtifactID = ""
			if tc.want.PassStands.Results == nil {
				got.PassStands.Results = nil
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the check = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The issue read keeps each child Dispatch lists, by key and status, as Dispatch's own answer of
// an issue with one child in backlog holds it.
func TestGetIssueKeepsEachChild(t *testing.T) {
	body := resultsFixture(t, "issue-with-a-backlog-child")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues/ACME-1" {
			t.Fatalf("request = %s %s, want GET /api/v1/issues/ACME-1", r.Method, r.URL.Path)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	issue, err := New(server.URL, BearerTransport(dispatchToken, nil)).GetIssue(context.Background(), "ACME-1")
	if err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	if want := []IssueChild{{Key: "ACME-2", Status: "backlog"}}; !reflect.DeepEqual(issue.Children, want) {
		t.Fatalf("the children = %+v, want %+v", issue.Children, want)
	}
}

// The issue read keeps each external link Dispatch lists, by URL and kind, in Dispatch's order: the
// merge read takes a link to a pull request as a candidate of the issue's merge.
func TestGetIssueKeepsItsLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues/ACME-1" {
			t.Fatalf("request = %s %s, want GET /api/v1/issues/ACME-1", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"key":"ACME-1","project":"ACME","title":"ACME-1","status":"done","parent":null,"rank":"A","labels":[],"last_seq":9,` +
			`"resolved_repo":"acme/widgets","blocked_by":[],"claim":null,"children":[],"external_links":[` +
			`{"url":"https://github.com/acme/widgets/pull/70","kind":"github_pr"},{"url":"https://tracker.example/ACME-1"}]}`))
	}))
	defer server.Close()
	issue, err := New(server.URL, BearerTransport(dispatchToken, nil)).GetIssue(context.Background(), "ACME-1")
	if err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	want := []IssueLink{{URL: "https://github.com/acme/widgets/pull/70", Kind: "github_pr"}, {URL: "https://tracker.example/ACME-1"}}
	if !reflect.DeepEqual(issue.Links, want) {
		t.Fatalf("the links = %+v, want %+v", issue.Links, want)
	}
}
