package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of a check CI proves whose detail, case and observed text carry words GitHub wrote
// about the head and its run. Each test calls only what the tree before this change has too, so
// the check's control (19) runs it there, where it fails on its captured output.

// TestAHeadThatIsNotASHAReachesTheCheckRunsDetailAsAPhrase: the newest pull request head Dispatch
// holds names a head SHA that holds a user and a password before an @ (the head a webhook or a
// GraphQL answer gave), and the read of its check runs fails. The detail the check shows
// (ciUnreadDetail) names the head by redact.ShapedSHA's phrase, and the read's error, which the
// detail carries, holds no target that names it. Captures api.ci-detail-head.
func TestAHeadThatIsNotASHAReachesTheCheckRunsDetailAsAPhrase(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	appKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"Server Error"}`)
	}))
	t.Cleanup(github.Close)
	client, err := githubapp.New(&auth.AppConfig{ID: 1, PEM: string(appKey)}, github.URL)
	if err != nil || client == nil {
		t.Fatalf("githubapp.New = %v, %v", client, err)
	}
	head := delivery.NewestHead{Repo: "acme/widgets", HeadSHA: redacttest.User + ":" + redacttest.Password + "@issuer.example"}
	_, err = client.CommitCheckRuns(context.Background(), "installation-token", "acme", "widgets", head.HeadSHA, 1, "")
	if err == nil {
		t.Fatal("CommitCheckRuns of a head that is not a SHA answered no error")
	}
	detail := ciUnreadDetail(head, fmt.Errorf("read check runs: %w", err))
	redacttest.RequireNone(t, "api.ci-detail-head", detail)
	if want := "GitHub could not be read for the check runs of acme/widgets at (the head is not a SHA): "; !strings.HasPrefix(detail, want) {
		t.Errorf("the detail is %q, want it to begin %q", detail, want)
	}
}

// TestAStoredHeadsRepositoryReachesTheCheckRunsDetailAsTheRuleReadsIt: the newest pull request head
// Dispatch holds was stored, before the routes held names to GitHub's rule, under a repository
// name a webhook's envelope gave that quotes a URL carrying a credential. The read of its check
// runs refuses the name before any GitHub call (readCIRuns, refs.SplitGitHubRepo), and the detail
// the check shows (ciUnreadDetail) names the repository as the rule reads it, while the refusal
// names no part of it. Captures api.ci-detail-repo.
func TestAStoredHeadsRepositoryReachesTheCheckRunsDetailAsTheRuleReadsIt(t *testing.T) {
	head := delivery.NewestHead{Repo: "acme/" + redacttest.Code, HeadSHA: strings.Repeat("ab", 20)}
	_, err := (&server{}).readCIRuns(context.Background(), head, map[string]struct{}{"contracts": {}})
	if err == nil {
		t.Fatal("the read of a stored head under a name GitHub does not allow answered no error")
	}
	detail := ciUnreadDetail(head, err)
	redacttest.RequireNone(t, "api.ci-detail-repo", detail)
	if want := "GitHub could not be read for the check runs of acme/" + redacttest.CodeShown + " at "; !strings.HasPrefix(detail, want) {
		t.Errorf("the detail is %q, want it to begin %q", detail, want)
	}
}

// TestACheckRunsStatusAndConclusionPrintOnlyAsGitHubDocumentsThem: GitHub lists the check run a
// check CI proves names with a status, or a conclusion, that quotes a URL carrying a credential.
// The check's `ci` member - its detail, its case and its pull-request body line - and the derived
// result's observed text print each word only where it is one of GitHub's documented check-run
// statuses and conclusions, and otherwise a fixed phrase. Captures api.ci-run-words/<row>.
func TestACheckRunsStatusAndConclusionPrintOnlyAsGitHubDocumentsThem(t *testing.T) {
	check := model.CheckBlock{BlockID: "c1", When: "before_merge", Text: "The contracts job passes at the head."}
	head := delivery.NewestHead{Repo: "acme/widgets", Number: 7, HeadSHA: strings.Repeat("ab", 20)}
	for _, row := range []struct {
		name string
		run  ciRun
	}{
		{"status", ciRun{id: 1, name: "contracts", status: redacttest.Code}},
		{"conclusion", ciRun{id: 2, name: "contracts", status: "completed", conclusion: redacttest.Code}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ci, result := ciAnswerOf(check, "contracts", head, &row.run, lineWordsFor(nil, ""))
			text := ci.Detail + " | " + ci.Case + " | " + ci.Line
			if result != nil {
				text += " | " + result.Observed
			}
			redacttest.RequireNone(t, "api.ci-run-words/"+row.name, text)
		})
	}
}
