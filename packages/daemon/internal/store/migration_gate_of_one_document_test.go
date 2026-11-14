package store

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A deployment that gates spec alone runs trees gated on one document, and every gate registered
// before this release is such a tree: nothing wrote a criteria row before the architect could
// register one (dispatch://INFRA-175, option b'). The upgrade must leave such a tree exactly as
// gated as it stood, and owe its work nothing.
//
// A spec gate open at version 3 is registered on the schema before the release, with a member at
// work in implementing. After the upgrade the tree's gate is read as the daemon reads it: the
// steps it is gated on are spec alone (record.TreeSteps), no criteria row exists, and the gate
// stands open (classify.DesignGatesOpen). The member's told row was seeded at version 3, and the
// one rule for news (record.OwedNews) owes it nothing, nor the root's architect.
//
// The negative control is in the same test: a new version 4 of the spec, applied as the daemon
// applies one (classify.ApplyDesignGateEvent) and written back through the store, leaves the gate
// closed. So the open gate read above is the gate's own answer and not a read that cannot see a
// closed one.
func TestAGateOfOneDocumentSurvivesTheUpgrade(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, designNewsVersion-1)
	const root, member = "LEGION-208", "LEGION-209"
	for _, seed := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'admitted', 1, 'in_progress', 'A', 1)`,
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-209', 'LEGION-208', 'LEGION', 'member', 'LEGION-208', 'implementing', 1, 'in_progress', 'B', 1)`,
		`insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
			values ('LEGION-208', 'spec', 'doc-spec', 3, 3, true)`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("register the gate before the upgrade: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	steps := []record.DesignStep{record.DesignStepSpec}
	records := record.NewStore()
	var facts record.NewsFacts
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		facts, err = record.TreeNewsFacts(ctx, records, tx, steps, root)
		return err
	}); err != nil {
		t.Fatalf("read the tree as the daemon reads it: %v", err)
	}
	gates, told, news := facts.Gates, facts.Told, record.OwedNews(facts)
	if got := record.TreeSteps(steps, gates); !slices.Equal(got, steps) {
		t.Fatalf("the steps the upgraded tree is gated on = %v, want spec alone", got)
	}
	if _, registered := gates[record.DesignStepCriteria]; registered {
		t.Fatalf("the upgraded tree holds a criteria row %+v, want none: only an architect's registration writes one", gates[record.DesignStepCriteria])
	}
	if !classify.DesignGatesOpen(steps, gates) {
		t.Fatalf("the upgraded gate %+v reads closed, want it open at version 3 as it was registered", gates)
	}
	var holder *record.DesignTold
	for i := range told {
		if told[i].Issue == member && told[i].Audience == record.AudienceHolder {
			holder = &told[i]
		}
	}
	if holder == nil || !reflect.DeepEqual(holder.Versions, map[record.DesignStep]int{record.DesignStepSpec: 3}) {
		t.Fatalf("the told rows of the upgraded tree = %+v, want %s's holder seeded at spec 3", told, member)
	}
	if len(news.Members) != 0 || len(news.Architect) != 0 {
		t.Fatalf("the upgraded tree owes %+v, want nothing: its work stands on the version the record shows it took", news)
	}

	// The negative control: a new version of the spec closes the gate.
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		closed := classify.ApplyDesignGateEvent(gates[record.DesignStepSpec], classify.DesignGateVersion, 4, record.EventMark{})
		if err := records.PutGate(ctx, tx, closed); err != nil {
			return err
		}
		var err error
		gates, err = records.GatesOf(ctx, tx, root)
		return err
	}); err != nil {
		t.Fatalf("apply a new version of the spec: %v", err)
	}
	if classify.DesignGatesOpen(steps, gates) {
		t.Fatalf("the gate after a new version 4 = %+v reads open, want it closed: the read above could not see a closed gate", gates)
	}
}
