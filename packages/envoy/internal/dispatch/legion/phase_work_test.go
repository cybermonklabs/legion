package legion

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// phaseWorkDay is the fixture's first moment.
var phaseWorkDay = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

// seedTitledIssueEvent seeds a root issue and one comment event a session wrote on it, stamping the
// given session title, which the phase-work read resolves.
func seedTitledIssueEvent(t *testing.T, database *store.Store, issue string, number int, session, title string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ($1, 'INFRA', $2, $1, 'in_progress', $1, '{"kind":"user","id":"alice"}'::jsonb, $3)`,
		issue, number, phaseWorkDay); err != nil {
		t.Fatalf("seed issue %s: %v", issue, err)
	}
	actor := `{"kind":"session","id":"` + session + `","origin":{"session_title":"` + title + `"}}`
	if _, err := database.Pool.Exec(ctx, `insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, 1, 'comment.created', $2::jsonb, '{}'::jsonb, false, $3)`, issue, actor, phaseWorkDay); err != nil {
		t.Fatalf("seed %s's event on %s: %v", session, issue, err)
	}
}

// TestPhaseWorkKeysIgnoresTheLegionDaemonsOwnEvents: PhaseWorkKeys counts an issue as Legion phase
// work when a session titled `Legion <role> · <that issue>` wrote on it. The Legion daemon's own
// session (`legion-daemon:<PROJECT>`) stamps no title, so resolving it would read every event it
// wrote for nothing; readSessionIssues leaves it out before the title read (DaemonSession). This
// seeds the daemon an event carrying a role title for its issue, which the old code would have
// counted, and holds that PhaseWorkKeys does not. Its control is an ordinary session carrying the
// same shape of title on another issue, which PhaseWorkKeys does count.
func TestPhaseWorkKeysIgnoresTheLegionDaemonsOwnEvents(t *testing.T) {
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(),
		`insert into projects (key, name, default_assignee) values ('INFRA', 'INFRA', 'alice')`); err != nil {
		t.Fatalf("seed project INFRA: %v", err)
	}
	seedTitledIssueEvent(t, database, "INFRA-9", 9, "legion-daemon:INFRA", "Legion architect · INFRA-9")
	seedTitledIssueEvent(t, database, "INFRA-8", 8, "s-worker", "Legion architect · INFRA-8")

	keys, err := PhaseWorkKeys(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the phase work: %v", err)
	}
	if _, ok := keys["INFRA-8"]; !ok {
		t.Errorf("PhaseWorkKeys left out INFRA-8, whose ordinary session carries an architect title: the control that the title path counts an issue at all (%v)", keys)
	}
	if _, ok := keys["INFRA-9"]; ok {
		t.Errorf("PhaseWorkKeys counted INFRA-9, whose only titled session is the daemon the skip must drop (%v)", keys)
	}
}
