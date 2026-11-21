package legionfacts

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
)

// TestAProjectNamingADaemonTheSamplerNeverReadGetsItsFacts: a project with a Legion setting whose
// daemon the sampler has never read holds no observation and, at first, no stored row, and its
// issues still carry the phase work the event log holds. Every Legion number is read from the
// stored facts, so the pass computes such a project as it computes one the sampler has read: one
// row for each issue with Legion phase work. A project that names no daemon and holds neither an
// observation nor a row is still no work of the loop.
func TestAProjectNamingADaemonTheSamplerNeverReadGetsItsFacts(t *testing.T) {
	run := newConsumer(t, 2)
	ctx := t.Context()
	const named, unnamed = "NAMED", "UNNAMED"
	histories := map[string]legiontest.Fixture{}
	for _, project := range []string{named, unnamed} {
		histories[project] = legiontest.Seed(t, run.database.Pool, legiontest.Spec{Project: project, Trees: 3, Unobserved: 3, Now: run.at})
	}
	run.exec(t, "name the daemon of "+named, `
		insert into legion_settings (project, state_url, created_by)
		values ($1, 'http://legion-named.internal:13370', '{"kind":"user","id":"alice"}'::jsonb)
	`, named)
	for _, project := range []string{named, unnamed} {
		var observed int
		if err := run.database.Pool.QueryRow(ctx, `select count(*) from legion_observations where project = $1`, project).Scan(&observed); err != nil {
			t.Fatalf("count the observations of %s: %v", project, err)
		}
		if observed > 0 || len(histories[project].PhaseWork) == 0 {
			t.Fatalf("%s holds %d observations and %d issues with phase work, want none and some, or this test examines nothing",
				project, observed, len(histories[project].PhaseWork))
		}
	}

	run.pass(t)
	for project, want := range map[string]int{named: len(histories[named].PhaseWork), unnamed: 0} {
		stored, err := readStored(ctx, run.database.Pool, project, run.at)
		if err != nil {
			t.Fatalf("read the stored facts of %s: %v", project, err)
		}
		if len(stored) != want {
			t.Errorf("%s holds %d stored rows after a pass, want %d", project, len(stored), want)
		}
		t.Logf("%s: %d stored rows, of %d issues with phase work", project, len(stored), len(histories[project].PhaseWork))
	}
}
