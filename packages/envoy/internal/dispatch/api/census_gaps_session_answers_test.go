package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// censusGapsAnswersSession is the agent of carol's the decline row's answers are read for, one of
// PUB's agents, so every caller that reads PUB sees it.
const censusGapsAnswersSession = "s-census-gaps-answers"

// TestASessionsAnswersCarryNoHiddenEvent (R2-R4): an ask.resolved event of PRIV's issue, a person's
// decline published to a PUB session's topic whose payload names an ask of PUB - the row a release
// could keep after the issue the event is of went to a project its reader does not see; no route
// writes it, since every event is appended to its own item's owner - is no item of GET
// /api/v1/agents/{session_id}/answers for any caller that does not see PRIV: the list is the one
// it read before the event was written, and names none of the decline's words. carol, who sees
// PRIV, reads the decline, so the row reaches a live event. With PRIV public, no project is
// private, and alice reads the decline as before (R9).
func TestASessionsAnswersCarryNoHiddenEvent(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	ctx := context.Background()
	seed.asSession(t, censusGapsAnswersSession, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments",
		map[string]any{"body": censusGapsAnswersSession + " works on PUB."}, http.StatusCreated)
	ask := createdID(t, seed.request(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/asks", map[string]any{
		"question": "Ship tonight?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}))
	answers := func(t *testing.T, caller privateCaller) model.SessionAnswersResponse {
		t.Helper()
		return decodeBody[model.SessionAnswersResponse](t, seed.mustRequest(t, caller, http.MethodGet,
			"/api/v1/agents/"+censusGapsAnswersSession+"/answers", nil, http.StatusOK))
	}
	itemsOf := func(t *testing.T, response model.SessionAnswersResponse) string {
		t.Helper()
		items, err := json.Marshal(response.Items)
		if err != nil {
			t.Fatalf("encode the items: %v", err)
		}
		return string(items)
	}
	before := map[privateCaller]string{}
	for _, caller := range censusGapsPublicReaders {
		before[caller] = itemsOf(t, answers(t, caller))
	}
	const words = "The private reason no reader of PUB may read."
	resolution, err := json.Marshal(map[string]any{
		"kind": model.AskResolutionDeclined, "at": time.Now().UTC().Format(time.RFC3339Nano),
		"actor": model.Actor{Kind: "user", ID: seedCarol}, "reason": words,
	})
	if err != nil {
		t.Fatalf("encode the resolution: %v", err)
	}
	if tag, err := seed.database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at, published_destinations)
		select $1::text, coalesce(max(seq), 0) + 1000, 'ask.resolved', jsonb_build_object('kind', 'user', 'id', $2::text),
		       jsonb_build_object('id', $3::text, 'resolution', $4::jsonb), false, now(), array[$5::text]
		from events where issue_key = $1
	`, seed.issue[seedPrivate], seedCarol, ask, resolution, contracts.LegacyAgentSubject(censusGapsAnswersSession)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("store PRIV's decline: %v (%d rows)", err, tag.RowsAffected())
	}
	declined := func(response model.SessionAnswersResponse) bool {
		for _, item := range response.Items {
			if item.Kind == model.SessionAnswerKindDeclined && item.ID == ask && item.Text == words {
				return true
			}
		}
		return false
	}
	t.Run("PRIV private", func(t *testing.T) {
		if read := answers(t, callerCarol); !declined(read) {
			t.Fatalf("carol, who sees PRIV, reads no decline of %s: the row reaches no event: %+v", ask, read.Items)
		}
		for _, caller := range censusGapsPublicReaders {
			read := itemsOf(t, answers(t, caller))
			if read != before[caller] || strings.Contains(read, words) {
				t.Errorf("%s reads the items %s, want %s, as before PRIV's event was written", caller, read, before[caller])
			}
		}
	})
	t.Run("PRIV public: the decline is read as before", func(t *testing.T) {
		seed.setVisibility(t, seedPrivate, model.ProjectPublic)
		if read := answers(t, callerAlice); !declined(read) {
			t.Errorf("alice, with no project private, reads no decline of %s: %+v", ask, read.Items)
		}
	})
}
