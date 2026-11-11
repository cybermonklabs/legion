package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// experimentAssignmentsVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const experimentAssignmentsVersion = 20261111045636

// 20261111045636 gives the declared experiments their own table and copies the one hard-coded
// memory experiment into it. The copy is what the measurement job reads from this release on: the
// four issues.memory_* columns stay in the schema and only stop being written, so without the
// back-fill every tree drawn before the deploy would read as having drawn nothing and the memory
// experiment would lose its whole exposure to date.
//
// The rule is the pair record.Store.DrawMemory refuses to draw over: a root that holds a draw
// (memory_drawn_at is not null) or whose hand-over settled it (memory_carried) holds an answer and
// takes a row, under the experiment `memory` and the unit `tree`, with its group as the variant,
// its mode as the mode, its instant as the instant and its mark as the mark. A root that holds
// neither has no answer to record and takes no row at all, so this release's own first open draws
// for it like any other new tree. The lifecycle rows here are the negative control of what the
// back-fill does not read: it walks the issue rows alone, so a drawn root whose tree holds no
// lifecycle row takes its row all the same.
func TestTheMemoryDrawIsCopiedIntoTheAssignmentsBy20261111045636(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, experimentAssignmentsVersion-1)

	drawnAt := time.Date(2026, 11, 11, 4, 56, 36, 0, time.UTC)
	trees := []struct {
		key, group, mode string
		at               *time.Time
		carried          bool
		lifecycle        bool
		// want is what the tree's assignment row holds after the migration, and holds is whether
		// it has one at all.
		holds                 bool
		wantUnit              string
		wantVariant, wantMode string
		wantDrawnAt           *time.Time
		wantCarried           bool
	}{
		// Drawn into the memory group under a randomisation.
		{key: "LEGION-401", group: "memory", mode: "random", at: &drawnAt, lifecycle: true,
			holds: true, wantUnit: "tree", wantVariant: "memory", wantMode: "random", wantDrawnAt: &drawnAt},
		// Drawn under mode off, which draws every tree into no group and records that it did.
		{key: "LEGION-402", group: "none", mode: "off", at: &drawnAt, lifecycle: true,
			holds: true, wantUnit: "tree", wantVariant: "none", wantMode: "off", wantDrawnAt: &drawnAt},
		// A hand-over that carried a draw: the copy keeps the draw and the mark both.
		{key: "LEGION-403", group: "memory", mode: "random", at: &drawnAt, carried: true, lifecycle: true,
			holds: true, wantUnit: "tree", wantVariant: "memory", wantMode: "random", wantDrawnAt: &drawnAt, wantCarried: true},
		// A hand-over that carried no draw: the mark alone is the answer, and it is no draw.
		{key: "LEGION-404", carried: true, lifecycle: true,
			holds: true, wantUnit: "tree", wantVariant: "", wantMode: "", wantDrawnAt: nil, wantCarried: true},
		// Neither drawn nor settled: it holds no answer, so it takes no row and this release's own
		// first open draws for it.
		{key: "LEGION-405", lifecycle: true},
		// The sixth tree, the one with no lifecycle row: the daemon never opened it, so it holds
		// no answer and takes no row, and this release draws for it when its row is made.
		{key: "LEGION-406"},
	}
	for _, tree := range trees {
		if _, err := store.Pool().Exec(ctx, `insert into issues
			(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
			 memory_group, memory_draw_mode, memory_drawn_at, memory_carried)
			values ($1, $1, 'LEGION', 'root', 'admitted', 1, 'in_progress', 'A', 1, $2, $3, $4, $5)`,
			tree.key, tree.group, tree.mode, tree.at, tree.carried); err != nil {
			t.Fatalf("record %s as the previous release leaves it: %v", tree.key, err)
		}
		if !tree.lifecycle {
			continue
		}
		if _, err := store.Pool().Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority)
			values ('legion', $1, 1, 'workflow')`, tree.key); err != nil {
			t.Fatalf("open the tree %s: %v", tree.key, err)
		}
	}

	// The negative control: the statement this release writes assignments with fails before the
	// migration, so what the reads below find is this migration's work and no other's.
	if _, err := store.Pool().Exec(ctx, `insert into experiment_assignments (issue_key, experiment, unit)
		values ('LEGION-401', 'memory', 'tree')`); err == nil {
		t.Fatal("an insert into experiment_assignments passed before the migration, want the table to be this migration's")
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the experiment assignments: %v", err)
	}

	count := 0
	for _, tree := range trees {
		var unit, variant, mode string
		var at *time.Time
		var carried bool
		err := store.Pool().QueryRow(ctx, `select unit, variant, mode, drawn_at, carried
			from experiment_assignments where issue_key = $1 and experiment = 'memory'`, tree.key).
			Scan(&unit, &variant, &mode, &at, &carried)
		if errors.Is(err, pgx.ErrNoRows) {
			if tree.holds {
				t.Errorf("%s holds no assignment after the migration, want the memory draw it carries copied", tree.key)
			}
			count++
			continue
		}
		if err != nil {
			t.Fatalf("read the assignment of %s back: %v", tree.key, err)
		}
		count++
		if !tree.holds {
			t.Errorf("%s holds an assignment at variant %q after the migration, want none: it was neither drawn for nor settled", tree.key, variant)
			continue
		}
		if unit != tree.wantUnit || variant != tree.wantVariant || mode != tree.wantMode || carried != tree.wantCarried {
			t.Errorf("%s holds unit %q variant %q mode %q carried %t, want unit %q variant %q mode %q carried %t",
				tree.key, unit, variant, mode, carried, tree.wantUnit, tree.wantVariant, tree.wantMode, tree.wantCarried)
		}
		switch {
		case tree.wantDrawnAt == nil && at != nil:
			t.Errorf("%s was drawn at %v, want no instant: its hand-over settled it with no draw", tree.key, at)
		case tree.wantDrawnAt != nil && (at == nil || !at.UTC().Equal(*tree.wantDrawnAt)):
			t.Errorf("%s was drawn at %v, want the instant %v its draw carries", tree.key, at, tree.wantDrawnAt)
		}
	}
	if count < len(trees) {
		t.Fatalf("the test read %d trees back, want all %d: every shape the back-fill meets is read by name", count, len(trees))
	}

	// The migration alters no memory column and drops none: the four stand as the previous release
	// left them, which is what the roll back onto that release rests on.
	var group, mode string
	var at *time.Time
	var carried bool
	if err := store.Pool().QueryRow(ctx, `select memory_group, memory_draw_mode, memory_drawn_at, memory_carried
		from issues where key = 'LEGION-403'`).Scan(&group, &mode, &at, &carried); err != nil {
		t.Fatalf("read the memory columns of LEGION-403 back: %v", err)
	}
	if group != "memory" || mode != "random" || at == nil || !at.UTC().Equal(drawnAt) || !carried {
		t.Errorf("LEGION-403 carries group %q drawn as %q at %v, carried %t, want its row untouched", group, mode, at, carried)
	}
}
