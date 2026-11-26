package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

// TestAPrioritysRelationToAHiddenIssueIsNoAct (R2-R4): a relation stated out of a priority to an
// issue of PRIV - a tuple of a kind this store writes none of today, as a later relation would be,
// written here in SQL - is no act of GET /api/v1/priorities/{id}/history for any caller that does
// not see PRIV: the history is the one it read before the tuple was written. carol, who sees PRIV,
// reads the act naming PRIV's issue, so the row reads a live tuple. With PRIV public, no project is
// private, and alice reads the act as before (R9).
func TestAPrioritysRelationToAHiddenIssueIsNoAct(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/priorities", map[string]string{"id": "probe", "title": "Probe"}, http.StatusCreated)
	history := func(t *testing.T, caller privateCaller) string {
		t.Helper()
		return seed.mustRequest(t, caller, http.MethodGet, "/api/v1/priorities/probe/history", nil, http.StatusOK).Body.String()
	}
	before := map[privateCaller]string{}
	for _, caller := range censusGapsPublicReaders {
		before[caller] = history(t, caller)
	}
	private := seed.issue[seedPrivate]
	if _, err := seed.database.Pool.Exec(context.Background(), `
		insert into priority_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		values ($1, 'probe', 'relates_to', $2, $3, now(), jsonb_build_object('kind', 'user', 'id', $4::text), 0, true)
	`, prioritystore.KindPriority, prioritystore.KindIssue, private, seedCarol); err != nil {
		t.Fatalf("store probe's relation to %s: %v", private, err)
	}
	names := func(t *testing.T, body string) bool {
		t.Helper()
		var acts []model.PriorityAct
		if err := json.Unmarshal([]byte(body), &acts); err != nil {
			t.Fatalf("decode probe's history %s: %v", body, err)
		}
		for _, act := range acts {
			if act.Field == "relates_to" && act.Issue == private {
				return true
			}
		}
		return false
	}
	t.Run("PRIV private", func(t *testing.T) {
		if read := history(t, callerCarol); !names(t, read) {
			t.Fatalf("carol, who sees PRIV, reads no act naming %s in probe's history: the row reads no tuple: %s", private, read)
		}
		for _, caller := range censusGapsPublicReaders {
			if read := history(t, caller); read != before[caller] || strings.Contains(read, private) {
				t.Errorf("%s reads probe's history as %s; before the relation to %s, %s", caller, read, private, before[caller])
			}
		}
	})
	t.Run("PRIV public: the act is read as before", func(t *testing.T) {
		seed.setVisibility(t, seedPrivate, model.ProjectPublic)
		if read := history(t, callerAlice); !names(t, read) {
			t.Errorf("alice, with no project private, reads no act naming %s: %s", private, read)
		}
	})
}
