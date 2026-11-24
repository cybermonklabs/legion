package delivery

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this package's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	words := redact.NewRemoteText(redacttest.Words)
	type errorTwin = struct {
		Message string `json:"message"`
	}
	type pageTwin = struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	}
	type responseTwin = struct {
		Data struct {
			Search struct {
				IssueCount int                     `json:"issueCount"`
				PageInfo   pageTwin                `json:"pageInfo"`
				Nodes      []searchPullRequestNode `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
		Errors []errorTwin `json:"errors"`
	}
	response := searchPullRequestsResponse{Errors: []searchError{{Message: words}}}
	response.Data.Search.PageInfo.EndCursor = words
	responseSame := responseTwin{Errors: []errorTwin{{redacttest.Words}}}
	responseSame.Data.Search.PageInfo.EndCursor = redacttest.Words
	type searchPageTwin = struct {
		items      []FetchedPullRequest
		endCursor  string
		issueCount int
	}
	type searchPageExported = struct {
		Items      []FetchedPullRequest
		EndCursor  string
		IssueCount int
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "delivery.searchPullRequestsResponse",
			Kind:     "fields",
			Boxed:    response,
			Same:     responseSame,
			Exported: responseSame,
		},
		{
			Name:     "delivery.searchError",
			Kind:     "fields",
			Boxed:    searchError{Message: words},
			Same:     errorTwin{redacttest.Words},
			Exported: errorTwin{redacttest.Words},
		},
		{
			Name:     "delivery.searchPullRequestsPage",
			Kind:     "fields",
			Boxed:    searchPullRequestsPage{HasNextPage: true, EndCursor: words},
			Same:     pageTwin{HasNextPage: true, EndCursor: redacttest.Words},
			Exported: pageTwin{HasNextPage: true, EndCursor: redacttest.Words},
		},
		{
			Name:     "delivery.searchPage",
			Kind:     "struct",
			Boxed:    searchPage{endCursor: words, issueCount: 1},
			Same:     searchPageTwin{endCursor: redacttest.Words, issueCount: 1},
			Exported: searchPageExported{EndCursor: redacttest.Words, IssueCount: 1},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
