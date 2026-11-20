package graphcensus_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheCensusHoldsTheDeliveryASessionsReplyStates holds the store census to a delivery no
// receipt records. A session's reply to the comment whose mention the listener refused flips that
// attempt to sent, and since no comment.delivery receipt follows a reply, the reply's own write
// states the comment's delivered_to the session (api's queueAnsweredDelivery), under the reply's
// comment.answered, beside the attempt's own. No topic of any event's published_destinations
// names that delivery, so the census reads it as the answered attempt's, which that attempt's
// comment_deliveries row projects, and every kind of the census reads no difference.
func TestTheCensusHoldsTheDeliveryASessionsReplyStates(t *testing.T) {
	r := newWritersRig(t)
	ctx := context.Background()
	issue := decoded[struct {
		Key string `json:"key"`
	}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SIZE", "title": "What a reply delivers",
	}, "alice", http.StatusCreated)).Key
	refused := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "Can you take this, @" + refusedSession + "?", "mentions": []map[string]any{{"target": "session:" + refusedSession}},
	}, "alice", http.StatusCreated)).ID
	r.settledAttempts(t, refused, "failed")
	r.request(t, http.MethodPost, "/api/v1/comments/"+refused+"/reply", map[string]any{
		"actor": sessionActor(refusedSession), "attempt": 1, "body": "Yes, I take it.",
	}, "", http.StatusCreated)
	r.settledAttempts(t, refused, model.DeliveryStateSent)

	report, err := graphcensus.StoreCensus(ctx, r.database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	answered := int64(-1)
	for _, kind := range report.Kinds {
		if kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
			t.Errorf("the census of %s reads %d row and %d node difference(s): rows %+v, nodes %+v",
				kind.Kind, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
		}
		for _, narrowing := range kind.Narrowings {
			if kind.Kind == "published_destinations" && narrowing.Name == "answered_deliveries" {
				answered = narrowing.Count
			}
		}
	}
	if answered != 1 {
		t.Errorf("the census of published_destinations counts %d delivery stated under a reply, want the one the reply stated", answered)
	}
}
