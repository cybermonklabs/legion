package record

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// raisedTo is the raise of each step of versions to its version from whatever the row took
// (Postgres.RaiseTold with no From): what a case seeds a told row with.
func raisedTo(versions map[DesignStep]int) []NewsItem {
	items := make([]NewsItem, 0, len(versions))
	for step, version := range versions {
		items = append(items, NewsItem{Step: step, To: version})
	}
	return items
}

// A holder's baseline is written once, at the versions its tree root's gate stands approved at in
// the writing transaction, and a later baseline leaves it as it stands: the work started from those
// versions, and a second baseline would hide the news of every version approved since. A step
// whose row holds no approval is left out, a version the record does not know. What a holder took
// then only rises: a raise takes the greater of each step's two versions, fills a step the row did
// not know and keeps a step the raise does not name. The owed opening is set apart from the
// versions, and owing it to an issue with no row writes one that knows no version. What clears it
// is the opened role message, a close, or a delivery's credit of it: counted while the delivery
// holds its confirmation, and folded into the row once its turn's result says it ran (the claim
// store's design_news_told tests).
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
		must(t, records.RaiseTold(ctx, tx, child.Key, AudienceHolder, raisedTo(map[DesignStep]int{DesignStepCriteria: 2}), at.Add(2*time.Hour)))
		must(t, records.RaiseTold(ctx, tx, child.Key, AudienceHolder, raisedTo(map[DesignStep]int{DesignStepSpec: 2, DesignStepCriteria: 1}), at.Add(3*time.Hour)))
		must(t, records.RaiseTold(ctx, tx, root.Key, AudienceArchitect, raisedTo(map[DesignStep]int{DesignStepSpec: 4}), at.Add(3*time.Hour)))
	})
	if got := toldOf(child.Key, AudienceHolder).Versions; !reflect.DeepEqual(got, map[DesignStep]int{DesignStepSpec: 3, DesignStepCriteria: 2}) {
		t.Fatalf("%s's versions after the raises = %v, want spec 3 kept against a lower raise and criteria filled at 2", child.Key, got)
	}
	if got := toldOf(root.Key, AudienceArchitect).Versions; !reflect.DeepEqual(got, map[DesignStep]int{DesignStepSpec: 4}) {
		t.Fatalf("the architect's versions after a raise of no row = %v, want the row written at spec 4", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.SettleOpening(ctx, tx, child.Key, AudienceHolder, true))
		must(t, records.SettleOpening(ctx, tx, sibling.Key, AudienceHolder, true))
	})
	if got := toldOf(child.Key, AudienceHolder); !got.OpeningOwed || got.Versions[DesignStepSpec] != 3 {
		t.Fatalf("%s's row once the opening is owed = %+v, want the opening owed and its versions kept", child.Key, got)
	}
	if got := toldOf(sibling.Key, AudienceHolder); !got.OpeningOwed || len(got.Versions) != 0 || !got.ToldAt.IsZero() {
		t.Fatalf("%s's row once the opening is owed = %+v, want a row that owes the opening and knows no version", sibling.Key, got)
	}
}

// Each settlement of the gate's opening on a told row gives the row its next opening number
// (DesignTold.Opening), through the one SQL function every writer of the owed opening calls: an
// opening owed to a holder owed news, the opened role message, and the close, which settles every
// row of the tree and no other tree's. So an opening is named by the number its row held when it
// became owed, and a close and a reopening owe one under a new number. A role message to an
// audience with no row writes none, since nothing is owed to it.
func TestEachSettlementOfTheOpeningMovesItsNumber(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := issueFixture("LEGION-320")
	child := issueFixture("LEGION-321")
	child.Tree = root.Key
	other := issueFixture("LEGION-330")
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{root, child, other} {
			must(t, records.PutIssue(ctx, tx, issue))
		}
	})
	read := func() map[string]DesignTold {
		t.Helper()
		rows := map[string]DesignTold{}
		inTx(t, st, func(tx pgx.Tx) {
			for _, tree := range []string{root.Key, other.Key} {
				told, err := records.TreeTold(ctx, tx, tree)
				must(t, err)
				for _, row := range told {
					rows[row.Issue+"/"+string(row.Audience)] = row
				}
			}
		})
		return rows
	}
	want := func(when string, key string, owed bool, opening int) {
		t.Helper()
		got, held := read()[key]
		if !held || got.OpeningOwed != owed || got.Opening != opening {
			t.Fatalf("%s: %s's row = %+v (held %t), want the opening owed %t under number %d", when, key, got, held, owed, opening)
		}
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.SettleOpening(ctx, tx, child.Key, AudienceHolder, true))
		must(t, records.SettleOpening(ctx, tx, root.Key, AudienceArchitect, false))
		must(t, records.SettleOpening(ctx, tx, other.Key, AudienceHolder, true))
	})
	want("at the first opening", child.Key+"/holder", true, 1)
	want("at the first opening", other.Key+"/holder", true, 1)
	if row, held := read()[root.Key+"/architect"]; held {
		t.Fatalf("the architect told by the opened message holds a row %+v, want none: nothing is owed to it", row)
	}

	inTx(t, st, func(tx pgx.Tx) { must(t, records.CloseOpenings(ctx, tx, root.Key)) })
	want("at the close", child.Key+"/holder", false, 2)
	want("at the close of another tree", other.Key+"/holder", true, 1)

	inTx(t, st, func(tx pgx.Tx) { must(t, records.SettleOpening(ctx, tx, child.Key, AudienceHolder, true)) })
	want("at the reopening", child.Key+"/holder", true, 3)

	inTx(t, st, func(tx pgx.Tx) { must(t, records.SettleOpening(ctx, tx, child.Key, AudienceHolder, false)) })
	want("at an opened message", child.Key+"/holder", false, 4)
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
		must(t, records.RaiseTold(ctx, tx, root.Key, AudienceHolder, raisedTo(map[DesignStep]int{DesignStepSpec: 4}), at))
		must(t, records.SettleOpening(ctx, tx, root.Key, AudienceHolder, true))
		must(t, records.FillTold(ctx, tx, root.Key, AudienceHolder, map[DesignStep]int{DesignStepSpec: 2, DesignStepCriteria: 1}, at.Add(time.Hour)))
		must(t, records.FillTold(ctx, tx, root.Key, AudienceArchitect, map[DesignStep]int{DesignStepSpec: 3}, at.Add(time.Hour)))
		var err error
		rows, err = records.TreeTold(ctx, tx, root.Key)
		must(t, err)
	})
	want := []DesignTold{
		{Issue: root.Key, Audience: AudienceArchitect, Versions: map[DesignStep]int{DesignStepSpec: 3}, ToldAt: at.Add(time.Hour)},
		{Issue: root.Key, Audience: AudienceHolder, Versions: map[DesignStep]int{DesignStepSpec: 4, DesignStepCriteria: 1}, OpeningOwed: true, Opening: 1, ToldAt: at},
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

// A change set is read back as Dispatch answered it: an unchanged pair as unchanged, and each run
// of the text outside every section with the place Dispatch named it at, before the first section
// or after a named `##` section, kept in the sections column beside the `##` sections, which carry
// none. A run recorded with no place, as a Dispatch from before the place names it, reads back with
// none. The control is a decision-only pair, which is not unchanged. An unchanged set that is
// unavailable, or decision-only too, is refused before the write, naming the pair.
func TestAChangeSetKeepsWhetherItIsUnchangedAndWhereEachRunStands(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)
	unchanged := DesignChanges{VersionPair: VersionPair{ArtifactID: "doc-spec", From: 4, To: 5}, State: DesignChangesRead, Unchanged: true,
		ReadAt: at}
	decisionOnly := DesignChanges{VersionPair: VersionPair{ArtifactID: "doc-spec", From: 5, To: 6}, State: DesignChangesRead, DecisionOnly: true,
		ReadAt: at}
	placed := DesignChanges{VersionPair: VersionPair{ArtifactID: "doc-spec", From: 6, To: 7}, State: DesignChangesRead, ReadAt: at,
		Sections: []SectionChange{
			{Heading: "", Change: "changed", Place: "before_first_section"},
			{Heading: "Design", Change: "changed"},
			{Heading: "", Change: "moved", Place: "after_section", AfterHeading: "Notes", AfterOccurrence: 1},
			{Heading: "", Change: "removed", Place: "after_section", AfterHeading: "", AfterOccurrence: 1},
			{Heading: "", Change: "added"},
		}}
	inTx(t, st, func(tx pgx.Tx) {
		for _, changes := range []DesignChanges{unchanged, decisionOnly, placed} {
			must(t, records.PutDesignChanges(ctx, tx, changes))
		}
	})
	var got map[VersionPair]DesignChanges
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		got, err = records.DesignChangesOf(ctx, tx, []VersionPair{unchanged.VersionPair, decisionOnly.VersionPair, placed.VersionPair})
		must(t, err)
	})
	if !got[unchanged.VersionPair].Unchanged || got[decisionOnly.VersionPair].Unchanged {
		t.Fatalf("unchanged read back = %t for the unchanged pair and %t for the decision-only one, want true and false",
			got[unchanged.VersionPair].Unchanged, got[decisionOnly.VersionPair].Unchanged)
	}
	if !reflect.DeepEqual(got[placed.VersionPair].Sections, placed.Sections) {
		t.Fatalf("the sections read back = %+v, want %+v, each run at its place", got[placed.VersionPair].Sections, placed.Sections)
	}
	if !got[unchanged.VersionPair].WorkUnchanged() || !got[decisionOnly.VersionPair].WorkUnchanged() || got[placed.VersionPair].WorkUnchanged() {
		t.Fatalf("work unchanged = %t, %t, %t for the unchanged, decision-only and placed pairs, want true, true and false",
			got[unchanged.VersionPair].WorkUnchanged(), got[decisionOnly.VersionPair].WorkUnchanged(), got[placed.VersionPair].WorkUnchanged())
	}

	inTx(t, st, func(tx pgx.Tx) {
		other := VersionPair{ArtifactID: "doc-spec", From: 7, To: 8}
		for _, refused := range []DesignChanges{
			{VersionPair: other, State: DesignChangesUnavailable, Unchanged: true, Reason: "dispatch has no changes route: 404", ReadAt: at},
			{VersionPair: other, State: DesignChangesRead, Unchanged: true, DecisionOnly: true, ReadAt: at},
		} {
			if err := records.PutDesignChanges(ctx, tx, refused); err == nil || !strings.Contains(err.Error(), "unchanged") {
				t.Fatalf("recording %+v = %v, want it refused as an unchanged pair the table does not hold", refused, err)
			}
		}
	})
}

// A review history the reconcile could not read is recorded per question, apart from every
// version: a later failure of the same question replaces its reason and time, the read that answers
// clears it, and a clear of a question with nothing recorded deletes nothing. A row whose shape the
// table refuses, with no reason or of no version, is refused before the write, naming the question.
func TestAnUnreadReviewHistoryIsRecordedPerQuestionUntilItIsRead(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	question := DesignHistoryQuestion{ArtifactID: "doc-spec", To: 5}
	other := DesignHistoryQuestion{ArtifactID: "doc-spec", To: 6}
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	recorded := func() map[DesignHistoryQuestion]DesignHistoryUnread {
		t.Helper()
		var unread map[DesignHistoryQuestion]DesignHistoryUnread
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			unread, err = records.DesignHistoryUnreadOf(ctx, tx, []DesignHistoryQuestion{question, other})
			must(t, err)
		})
		return unread
	}

	later := DesignHistoryUnread{DesignHistoryQuestion: question, ReadAt: at.Add(time.Hour),
		Reason: "Dispatch failed 3 reads of the document's review history, the last with: 503 SERVICE_UNAVAILABLE"}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDesignHistoryUnread(ctx, tx, DesignHistoryUnread{DesignHistoryQuestion: question, ReadAt: at,
			Reason: "Dispatch failed 3 reads of the document's review history, the last with: 502 BAD_GATEWAY"}))
		must(t, records.PutDesignHistoryUnread(ctx, tx, later))
	})
	if got := recorded(); !reflect.DeepEqual(got, map[DesignHistoryQuestion]DesignHistoryUnread{question: later}) {
		t.Fatalf("the unread histories after two failures = %+v, want the question alone, with the later failure's reason and time", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ClearDesignHistoryUnread(ctx, tx, other))
	})
	if got := recorded(); !reflect.DeepEqual(got, map[DesignHistoryQuestion]DesignHistoryUnread{question: later}) {
		t.Fatalf("the unread histories after a clear of a question with none recorded = %+v, want the other question's row kept", got)
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.ClearDesignHistoryUnread(ctx, tx, question))
	})
	if got := recorded(); len(got) != 0 {
		t.Fatalf("the unread histories after the read that answered = %+v, want none", got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		for _, refused := range []DesignHistoryUnread{
			{DesignHistoryQuestion: other, ReadAt: at},
			{DesignHistoryQuestion: DesignHistoryQuestion{ArtifactID: "doc-spec"}, Reason: "a failed read", ReadAt: at},
		} {
			err := records.PutDesignHistoryUnread(ctx, tx, refused)
			if err == nil {
				t.Fatalf("recording %+v was taken, want it refused", refused)
			}
			if named := fmt.Sprintf("%+v", refused.DesignHistoryQuestion); !strings.Contains(err.Error(), named) {
				t.Fatalf("the refusal of %+v = %v, want it to name the question %s", refused, err, named)
			}
		}
		if unread, err := records.DesignHistoryUnreadOf(ctx, tx, nil); err != nil || len(unread) != 0 {
			t.Fatalf("the unread histories of no questions = %+v, %v, want none", unread, err)
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
			must(t, records.RaiseTold(ctx, tx, issue, AudienceHolder, raisedTo(map[DesignStep]int{DesignStepSpec: 3}), at))
		}
		must(t, records.RaiseTold(ctx, tx, root.Key, AudienceArchitect, raisedTo(map[DesignStep]int{DesignStepSpec: 3}), at))
		must(t, records.RaiseTold(ctx, tx, other.Key, AudienceHolder, raisedTo(map[DesignStep]int{DesignStepSpec: 1}), at))
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

	// The clear runs forget over what it is owed before it deletes the gates; nothing is kept here,
	// so the forget has nothing to delete.
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.ClearGeneration(ctx, tx, child, forgetNothing)
		must(t, err)
	})
	if got := count(root.Key); got != 2 {
		t.Fatalf("the told rows of %s after its child's clear = %d, want the root's two left", root.Key, got)
	}
	inTx(t, st, func(tx pgx.Tx) {
		_, err := records.ClearTreeGeneration(ctx, tx, root.Key, forgetNothing)
		must(t, err)
	})
	if got := count(root.Key); got != 0 {
		t.Fatalf("the told rows of %s after its tree's clear = %d, want none: they went with the gate", root.Key, got)
	}
	if got := count(other.Key); got != 1 {
		t.Fatalf("the told rows of %s after another tree's clear = %d, want its own one kept", other.Key, got)
	}
}
