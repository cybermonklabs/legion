package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A holder's baseline is written once, at the versions its tree root's gate stands approved at in
// the writing transaction, and a later baseline leaves it as it stands: the work started from those
// versions, and a second baseline would hide the news of every version approved since. A step
// whose row holds no approval is left out, a version the record does not know. What a holder took
// then only rises: a raise takes the greater of each step's two versions, fills a step the row did
// not know and keeps a step the raise does not name. The owed opening is set and cleared apart from
// the versions, and setting it on an issue with no row writes one that knows no version.
func TestToldRowsTakeOneBaselineAndOnlyRise(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := issueFixture("LEGION-300")
	child := issueFixture("LEGION-301")
	child.Tree = root.Key
	sibling := issueFixture("LEGION-302")
	sibling.Tree, sibling.Phase = root.Key, phase.Admitted
	at := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{root, child, sibling} {
			must(t, records.PutIssue(ctx, tx, issue))
		}
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root.Key, Step: DesignStepSpec, ArtifactID: "doc-spec", LatestVersion: 3, ApprovedVersion: new(3)}))
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root.Key, Step: DesignStepCriteria, ArtifactID: "doc-criteria", LatestVersion: 2}))
		must(t, records.BaselineTold(ctx, tx, child.Key, AudienceHolder, root.Key, at))
		// The gate moves on, and a second baseline of the same holder is no baseline at all: the
		// row read back below keeps the first one's version and moment.
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root.Key, Step: DesignStepSpec, ArtifactID: "doc-spec", LatestVersion: 4, ApprovedVersion: new(4)}))
		must(t, records.BaselineTold(ctx, tx, child.Key, AudienceHolder, root.Key, at.Add(time.Hour)))
	})
	toldOf := func(issue string, audience DesignAudience) DesignTold {
		t.Helper()
		var rows []DesignTold
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			rows, err = records.TreeTold(ctx, tx, root.Key)
			must(t, err)
		})
		for _, row := range rows {
			if row.Issue == issue && row.Audience == audience {
				return row
			}
		}
		t.Fatalf("the told rows of %s = %+v, want one of %s's %s", root.Key, rows, issue, audience)
		return DesignTold{}
	}
	if got, want := toldOf(child.Key, AudienceHolder), (DesignTold{Issue: child.Key, Audience: AudienceHolder,
		Versions: map[DesignStep]int{DesignStepSpec: 3}, ToldAt: at}); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s's baseline = %+v, want %+v: the spec at the version approved when it was taken, the criteria unknown", child.Key, got, want)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RaiseTold(ctx, tx, child.Key, AudienceHolder, map[DesignStep]int{DesignStepCriteria: 2}, at.Add(2*time.Hour)))
		must(t, records.RaiseTold(ctx, tx, child.Key, AudienceHolder, map[DesignStep]int{DesignStepSpec: 2, DesignStepCriteria: 1}, at.Add(3*time.Hour)))
		must(t, records.RaiseTold(ctx, tx, root.Key, AudienceArchitect, map[DesignStep]int{DesignStepSpec: 4}, at.Add(3*time.Hour)))
	})
	if got := toldOf(child.Key, AudienceHolder).Versions; !reflect.DeepEqual(got, map[DesignStep]int{DesignStepSpec: 3, DesignStepCriteria: 2}) {
		t.Fatalf("%s's versions after the raises = %v, want spec 3 kept against a lower raise and criteria filled at 2", child.Key, got)
	}
	if got := toldOf(root.Key, AudienceArchitect).Versions; !reflect.DeepEqual(got, map[DesignStep]int{DesignStepSpec: 4}) {
		t.Fatalf("the architect's versions after a raise of no row = %v, want the row written at spec 4", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.SetOpeningOwed(ctx, tx, child.Key, AudienceHolder, true))
		must(t, records.SetOpeningOwed(ctx, tx, sibling.Key, AudienceHolder, true))
	})
	if got := toldOf(child.Key, AudienceHolder); !got.OpeningOwed || got.Versions[DesignStepSpec] != 3 {
		t.Fatalf("%s's row once the opening is owed = %+v, want the opening owed and its versions kept", child.Key, got)
	}
	if got := toldOf(sibling.Key, AudienceHolder); !got.OpeningOwed || len(got.Versions) != 0 || !got.ToldAt.IsZero() {
		t.Fatalf("%s's row once the opening is owed = %+v, want a row that owes the opening and knows no version", sibling.Key, got)
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.SetOpeningOwed(ctx, tx, child.Key, AudienceHolder, false))
	})
	if got := toldOf(child.Key, AudienceHolder); got.OpeningOwed {
		t.Fatalf("%s's row once the opening is told = %+v, want it owed no more", child.Key, got)
	}
}

// A version the record does not know is resolved into the told row once, and a fill never moves a
// version the row knows: it writes each step the row does not know, leaves each step it knows as
// it stands whatever the fill names, and keeps the owed opening. A fill of an audience with no row
// writes one.
func TestAFillWritesOnlyTheVersionsTheToldRowDoesNotKnow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := issueFixture("LEGION-310")
	at := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	var rows []DesignTold
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, root))
		must(t, records.RaiseTold(ctx, tx, root.Key, AudienceHolder, map[DesignStep]int{DesignStepSpec: 4}, at))
		must(t, records.SetOpeningOwed(ctx, tx, root.Key, AudienceHolder, true))
		must(t, records.FillTold(ctx, tx, root.Key, AudienceHolder, map[DesignStep]int{DesignStepSpec: 2, DesignStepCriteria: 1}, at.Add(time.Hour)))
		must(t, records.FillTold(ctx, tx, root.Key, AudienceArchitect, map[DesignStep]int{DesignStepSpec: 3}, at.Add(time.Hour)))
		var err error
		rows, err = records.TreeTold(ctx, tx, root.Key)
		must(t, err)
	})
	want := []DesignTold{
		{Issue: root.Key, Audience: AudienceArchitect, Versions: map[DesignStep]int{DesignStepSpec: 3}, ToldAt: at.Add(time.Hour)},
		{Issue: root.Key, Audience: AudienceHolder, Versions: map[DesignStep]int{DesignStepSpec: 4, DesignStepCriteria: 1}, OpeningOwed: true, ToldAt: at},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("the told rows after the fills = %+v, want %+v: the known spec kept at 4, the unknown criteria filled at 1, the opening still owed, and the architect's row written", rows, want)
	}
}

// One reconcile of a tree's news stands queued at a time: a queue beside one that has not begun to
// run queues nothing, since that one reads the record when it runs, and a queue beside one a lane
// runs now queues another, since the running one may have read the record before the write.
func TestOneReconcileOfATreesNewsStandsQueued(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := issueFixture("LEGION-320")
	now := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	count := func() int {
		t.Helper()
		var rows int
		inTx(t, st, func(tx pgx.Tx) {
			must(t, tx.QueryRow(ctx, `select count(*) from outbox where kind = $1 and issue = $2`, string(OutboxKindDesignChanges), root.Key).Scan(&rows))
		})
		return rows
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, root))
		must(t, QueueDesignNews(ctx, records, tx, root.Key, now))
		must(t, QueueDesignNews(ctx, records, tx, root.Key, now))
	})
	if got := count(); got != 1 {
		t.Fatalf("design_changes rows after two queues = %d, want one: the second finds the first still to run", got)
	}
	inTx(t, st, func(tx pgx.Tx) {
		_, err := tx.Exec(ctx, `update outbox set lease_token = 'a-lane', lease_until = $2 where kind = $1`,
			string(OutboxKindDesignChanges), now.Add(time.Minute))
		must(t, err)
		must(t, QueueDesignNews(ctx, records, tx, root.Key, now))
	})
	if got := count(); got != 2 {
		t.Fatalf("design_changes rows after a queue beside a running one = %d, want two", got)
	}
}

// A change set is recorded per pair of versions. A pair Dispatch could not answer is recorded
// unavailable with its reason, and a later read replaces it; a pair recorded read is final, since
// two stored versions never change, and a later failure leaves it as it stands. A set whose shape
// the table refuses is refused before the write, naming the pair.
func TestChangeSetsAreRecordedPerPairAndAReadOneIsFinal(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	pair := VersionPair{ArtifactID: "doc-spec", From: 3, To: 5}
	other := VersionPair{ArtifactID: "doc-spec", From: 0, To: 2}
	at := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	read := DesignChanges{VersionPair: pair, State: DesignChangesRead, ReadAt: at.Add(time.Hour),
		Sections: []SectionChange{{Heading: "Requirements", Change: "changed"}},
		Blocks:   []BlockChange{{ID: "r2", Type: "requirement", Change: "added"}}}
	recorded := func() map[VersionPair]DesignChanges {
		t.Helper()
		var sets map[VersionPair]DesignChanges
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			sets, err = records.DesignChangesOf(ctx, tx, []VersionPair{pair, other})
			must(t, err)
		})
		return sets
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDesignChanges(ctx, tx, DesignChanges{VersionPair: pair, State: DesignChangesUnavailable,
			Reason: "dispatch has no changes route: 404", ReadAt: at}))
	})
	if got := recorded(); len(got) != 1 || got[pair].State != DesignChangesUnavailable || got[pair].Reason == "" {
		t.Fatalf("the change sets after an unreadable pair = %+v, want that pair alone, unavailable with its reason", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDesignChanges(ctx, tx, read))
		must(t, records.PutDesignChanges(ctx, tx, DesignChanges{VersionPair: pair, State: DesignChangesUnavailable,
			Reason: "three reads failed", ReadAt: at.Add(2 * time.Hour)}))
	})
	if got := recorded(); !reflect.DeepEqual(got, map[VersionPair]DesignChanges{pair: read}) {
		t.Fatalf("the change sets after a read and a later failure = %+v, want the read kept", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		for _, refused := range []DesignChanges{
			{VersionPair: other, State: DesignChangesRead, Reason: "a read has no reason", ReadAt: at},
			{VersionPair: other, State: DesignChangesUnavailable, ReadAt: at},
			{VersionPair: VersionPair{ArtifactID: "doc-spec", From: 5, To: 5}, State: DesignChangesRead, ReadAt: at},
			{VersionPair: other, State: "skimmed", ReadAt: at},
		} {
			if err := records.PutDesignChanges(ctx, tx, refused); err == nil {
				t.Fatalf("recording %+v was taken, want it refused", refused)
			}
		}
		if sets, err := records.DesignChangesOf(ctx, tx, nil); err != nil || len(sets) != 0 {
			t.Fatalf("the change sets of no pairs = %+v, %v, want none", sets, err)
		}
	})
}

// A generation's clear takes what its holders took of the gate with the gate itself: an issue's
// told rows go with the generation, so the next generation's work takes its own baseline. A tree's
// clear takes every member's rows, and an issue's own clear takes its rows alone.
func TestClearGenerationTakesTheToldRowsWithTheGate(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := issueFixture("LEGION-310")
	child := issueFixture("LEGION-311")
	child.Tree = root.Key
	other := issueFixture("LEGION-320")
	at := time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{root, child, other} {
			must(t, records.PutIssue(ctx, tx, issue))
		}
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root.Key, Step: DesignStepSpec, ArtifactID: "doc-spec", LatestVersion: 3, ApprovedVersion: new(3)}))
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: other.Key, Step: DesignStepSpec, ArtifactID: "doc-other", LatestVersion: 1, ApprovedVersion: new(1)}))
		for _, issue := range []string{root.Key, child.Key} {
			must(t, records.RaiseTold(ctx, tx, issue, AudienceHolder, map[DesignStep]int{DesignStepSpec: 3}, at))
		}
		must(t, records.RaiseTold(ctx, tx, root.Key, AudienceArchitect, map[DesignStep]int{DesignStepSpec: 3}, at))
		must(t, records.RaiseTold(ctx, tx, other.Key, AudienceHolder, map[DesignStep]int{DesignStepSpec: 1}, at))
	})
	count := func(tree string) int {
		t.Helper()
		var rows []DesignTold
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			rows, err = records.TreeTold(ctx, tx, tree)
			must(t, err)
		})
		return len(rows)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ClearGeneration(ctx, tx, child.Key, at))
	})
	if got := count(root.Key); got != 2 {
		t.Fatalf("the told rows of %s after its child's clear = %d, want the root's two left", root.Key, got)
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ClearTreeGeneration(ctx, tx, root.Key, at))
	})
	if got := count(root.Key); got != 0 {
		t.Fatalf("the told rows of %s after its tree's clear = %d, want none: they went with the gate", root.Key, got)
	}
	if got := count(other.Key); got != 1 {
		t.Fatalf("the told rows of %s after another tree's clear = %d, want its own one kept", other.Key, got)
	}
}
