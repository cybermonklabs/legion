package record

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The architect-stalled wake is a controller notice and no architect's. The architect is the
// role a notice would otherwise go to, and every arm of the wake is a reading that it did not
// take its work, is not there, or has not moved, so there is nobody inside the tree to tell. The
// validation takes it in a ControllerNotice and refuses it in a Notice, which is what that split
// of the two notice-kind lists means.
func TestTheArchitectStalledWakeIsOneOfTheControllerOnlyNoticeKinds(t *testing.T) {
	if !controllerOnlyNoticeKind(ArchitectStalledNotice) {
		t.Fatalf("the %s kind is not controller-only, so a notice of it would be queued to an architect's topic", ArchitectStalledNotice)
	}
	if validNoticeKind(ArchitectStalledNotice) {
		t.Fatalf("the %s kind is on the architect's own list, so an architect would be told a wake meant for the controller", ArchitectStalledNotice)
	}
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	reason := ArchitectTookNothing.Words() + ": the task of phase entry 3 was queued to the architect's claim of LEGION-208"
	if _, err := NewOutboxRow("LEGION-208", ControllerNotice{Kind: ArchitectStalledNotice, Role: claim.RoleArchitect, Reason: reason}, at); err != nil {
		t.Fatalf("the wake the daemon writes is refused: %v", err)
	}
	if _, err := NewOutboxRow("LEGION-208", ControllerNotice{Kind: ArchitectStalledNotice, Role: claim.RoleArchitect}, at); err == nil {
		t.Fatalf("a wake carrying no reason is taken, and a controller reading it would learn nothing of which arm raised it")
	}
	if _, err := NewOutboxRow("LEGION-208", Notice{Kind: ArchitectStalledNotice, Role: claim.RoleArchitect, Reason: reason}, at); err == nil {
		t.Fatalf("the wake is taken as an architect's notice, which is the one reader that cannot act on it")
	}
}

// Each arm says its own words, and no two arms say the same ones: the reason of every wake
// begins with them (admit's wakeStalledArchitect), so a controller holding two wakes of one root
// reads two different facts rather than one sentence twice.
func TestEachArchitectWakeArmSaysWordsOfItsOwn(t *testing.T) {
	said := map[string]ArchitectStallArm{}
	for _, arm := range []ArchitectStallArm{ArchitectTookNothing, ArchitectClaimUnreachable, ArchitectSlowBeforeGate} {
		words := arm.Words()
		if words == "" || words == string(arm) {
			t.Fatalf("the %s arm says %q, want words a person reads rather than the value the mark is keyed by", arm, words)
		}
		if strings.ContainsAny(words, ":;") {
			t.Fatalf("the %s arm says %q, which carries the punctuation the reason puts after it", arm, words)
		}
		if other, twice := said[words]; twice {
			t.Fatalf("the %s arm says what the %s arm says, %q: two arms a controller cannot tell apart", arm, other, words)
		}
		said[words] = arm
	}
}

// The mark tells once for one issue, one tree, one run and one arm, and tells again for a second
// arm and for the run after it. It is what keeps the wake the admission pass raises to one
// however many facts, ticks and sweeps read the same root (admit's wakeStalledArchitect).
func TestTheArchitectWakeMarkTellsOncePerArmAndRun(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	root := issueFixture("LEGION-829")
	root.Parent = nil
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, root))
		told, err := records.TellArchitectStalled(ctx, tx, root.Key, root.Tree, root.Generation, ArchitectTookNothing, at)
		must(t, err)
		if !told {
			t.Fatalf("the first call for an arm answers false, so nothing would ever tell the controller")
		}
		told, err = records.TellArchitectStalled(ctx, tx, root.Key, root.Tree, root.Generation, ArchitectTookNothing, at.Add(time.Minute))
		must(t, err)
		if told {
			t.Fatalf("the second call for the same root, run and arm answers true, so every pass would wake the controller again")
		}
		told, err = records.TellArchitectStalled(ctx, tx, root.Key, root.Tree, root.Generation, ArchitectSlowBeforeGate, at)
		must(t, err)
		if !told {
			t.Fatalf("a second arm of the same root answers false, so one arm would hide the other")
		}
		told, err = records.TellArchitectStalled(ctx, tx, root.Key, root.Tree, root.Generation+1, ArchitectTookNothing, at)
		must(t, err)
		if !told {
			t.Fatalf("the run after a re-admission answers false, so the new run's controller would never hear it")
		}
	})
}

// The batched read of the phases rows carries the moment each entry was raised beside its
// number: the admission pass weighs the one and reads how long the role has been owed its work
// from the other, in the one statement it makes for the whole pass. A row no entry has been
// raised on reads as the zero entry at the zero moment.
func TestTheArchitectWakeReadsTheEntryAndTheMomentItWasRaised(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	raised := issueFixture("LEGION-830")
	raised.Parent = nil
	unraised := issueFixture("LEGION-831")
	unraised.Parent = nil
	at := time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, raised))
		must(t, records.PutIssue(ctx, tx, unraised))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: raised.Key, Role: claim.RoleArchitect, Claim: "architect",
			Entry: 4, EntryTask: "Carry the tree on.", EntryRaisedAt: at}))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: unraised.Key, Role: claim.RoleArchitect, Claim: "architect"}))
	})
	inTx(t, st, func(tx pgx.Tx) {
		entries, err := records.PhaseEntries(ctx, tx, []string{raised.Key, unraised.Key})
		must(t, err)
		got := entries[PhaseRole{Issue: raised.Key, Role: claim.RoleArchitect}]
		if got.Entry != 4 || !got.RaisedAt.Equal(at) {
			t.Fatalf("the architect's entry of %s = %+v, want entry 4 raised at %s", raised.Key, got, at)
		}
		if empty := entries[PhaseRole{Issue: unraised.Key, Role: claim.RoleArchitect}]; empty.Entry != 0 || !empty.RaisedAt.IsZero() {
			t.Fatalf("the architect's entry of the row nothing raised = %+v, want the zero entry at the zero moment", empty)
		}
	})
}
