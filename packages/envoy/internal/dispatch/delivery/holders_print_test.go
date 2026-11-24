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
	type responseTwin = struct {
		Data struct {
			Search struct {
				IssueCount int                     `json:"issueCount"`
				PageInfo   searchPullRequestsPage  `json:"pageInfo"`
				Nodes      []searchPullRequestNode `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
		Errors []errorTwin `json:"errors"`
	}
	for _, h := range []redacttest.Holder{
		{
			Name:     "delivery.searchPullRequestsResponse",
			Kind:     "fields",
			Boxed:    searchPullRequestsResponse{Errors: []searchError{{Message: words}}},
			Same:     responseTwin{Errors: []errorTwin{{redacttest.Words}}},
			Exported: responseTwin{Errors: []errorTwin{{redacttest.Words}}},
		},
		{
			Name:     "delivery.searchError",
			Kind:     "fields",
			Boxed:    searchError{Message: words},
			Same:     errorTwin{redacttest.Words},
			Exported: errorTwin{redacttest.Words},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
