package events

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestARegistrationRecordsEachPairOnce (Registrations.Record): the listener's answer that S
// registered under PRIV and PUB records (S, PRIV) and (S, PUB) proven_by `registration`, once
// however often it is answered or named; a pair an event already recorded is not written again
// and keeps its proof; and an answer with no project, or no session, records nothing.
//
// Control, in the test: the same pairs inserted with no guard against a row that holds them,
// twice, leave two rows of one pair, and the check fails naming it.
func TestARegistrationRecordsEachPairOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key) values ('S', 'PUB')`); err != nil {
		t.Fatalf("record (S, PUB) as an event does: %v", err)
	}
	registrations := SessionRegistrations(database.Pool)
	answer := []model.SessionProjects{
		{SessionID: "S", Projects: []string{"PRIV", "PUB", "PRIV"}},
		{SessionID: "T"},
		{Projects: []string{"PRIV"}},
	}
	for range 2 {
		if err := registrations.Record(ctx, answer); err != nil {
			t.Fatalf("record S's registration: %v", err)
		}
	}
	rows := func() []string {
		read, err := database.Pool.Query(ctx, `select session_id || ' ' || project_key || ' ' || proven_by from session_projects order by 1`)
		if err != nil {
			t.Fatalf("read session_projects: %v", err)
		}
		defer read.Close()
		var pairs []string
		for read.Next() {
			var pair string
			if err := read.Scan(&pair); err != nil {
				t.Fatalf("read session_projects: %v", err)
			}
			pairs = append(pairs, pair)
		}
		return pairs
	}
	want := []string{"S PRIV registration", "S PUB event"}
	if got := rows(); !slices.Equal(got, want) {
		t.Fatalf("session_projects holds %q, want %q: each pair once, an event's pair kept as it was", got, want)
	}

	// Control: with no guard the same pair is written twice.
	for range 2 {
		if _, err := database.Pool.Exec(ctx, `insert into session_projects (session_id, project_key, proven_by) values ('U', 'PRIV', 'registration')`); err != nil {
			t.Fatalf("control: insert (U, PRIV): %v", err)
		}
	}
	if got := rows(); slices.Equal(got, append(slices.Clone(want), "U PRIV registration")) {
		t.Fatalf("control: two unguarded inserts left %q, want two rows of (U, PRIV), which the check names", got)
	}
}
