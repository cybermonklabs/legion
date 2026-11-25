package docs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A document CreateDocument creates whose tree a settlement has nothing to write into - every
// block given its id by the parse, no ask block, no check block carrying `ci_check`, as a
// project's `evals-md` is on every project create - owes no settlement (seedOwesSettlement). Its
// commit leaves no pending-settlement row and no record of its room, and the version
// CreateDocument wrote stands holding the seeded markdown. Nothing settles it afterwards, so no
// settlement takes its room: not settleOwedNow, not a load of the room, not Shutdown. The same
// create of a document holding an ask block still owes its settlement, which runs as before: the
// row and the room's record stand after the commit, and settleOwedNow takes the room, opens the
// ask and deletes the row. Red where the seed owes a settlement whatever its tree holds: the plain
// document's create leaves the row, which settleOwedNow then settles, warming its room.
func TestACreatedDocumentOwesASettlementOnlyWhereItsTreeNeedsOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice')`); err != nil {
		t.Fatalf("create the test project: %v", err)
	}
	// Settlements wait an hour, so only the calls below run one.
	service := newShutdownTestService(t, database, nil)
	var mu sync.Mutex
	var taken []string
	service.afterSettleWarm = func(room string) {
		mu.Lock()
		defer mu.Unlock()
		taken = append(taken, room)
	}
	takes := func(room string) int {
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, took := range taken {
			if took == room {
				count++
			}
		}
		return count
	}
	project, alice := "DOC", model.Actor{Kind: "user", ID: "alice"}
	create := func(slug, markdown string) (model.Artifact, WrittenText) {
		t.Helper()
		var artifact model.Artifact
		var written WrittenText
		if err := joinedWrite(service, func(ctx context.Context) error {
			var err error
			artifact, written, err = CreateDocument(ctx, service, NewArtifact{
				Project: &project, RefPrefix: project, Slug: slug, Name: slug + ".md", CreatedBy: alice,
			}, markdown)
			return err
		}); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		return artifact, written
	}
	owed := func(room string) bool {
		t.Helper()
		owes, err := settlementPending(ctx, database.Pool, room)
		if err != nil {
			t.Fatalf("read whether %s owes a settlement: %v", room, err)
		}
		return owes
	}
	// record is whether the service holds a record of room's room, and whether that record holds
	// an edit source no settlement has read.
	record := func(room string) (held, unsettled bool) {
		state := service.lockExistingState(room)
		if state == nil {
			return false, false
		}
		unsettled = state.unsettled
		service.unlockState(room, state)
		return true, unsettled
	}

	plain, plainWritten := create("plain-md", "# Evals of Documents\n\nOne section for each eval.\n\n"+
		"## review-latency\n\n:::callout{#method-1}\nCounts the hours to a first review.\n\n- Only merged pull requests.\n:::\n")
	asking, askingWritten := create("asking-md", "# Asks\n\n:::ask{#ship urgency=\"med\"}\nShip it?\n:::\n")

	if plainWritten.OwesSettlement {
		t.Errorf("CreateDocument answered that the plain document owes a settlement")
	}
	if owed(plain.ID) {
		t.Errorf("the plain document's create left a pending-settlement row")
	}
	if held, unsettled := record(plain.ID); held {
		t.Errorf("the plain document's create left a record of its room (unsettled %t)", unsettled)
	}
	if service.roomHeld(plain.ID) {
		t.Errorf("the plain document's create loaded its room")
	}
	if number, markdown, authors := latestDocumentVersion(t, database, plain.ID); number != 1 || markdown != plainWritten.Markdown ||
		len(authors) != 1 || authors[0] != alice {
		t.Errorf("the plain document's latest version is %d holding %q by %v, want version 1 holding the seeded %q by alice",
			number, markdown, authors, plainWritten.Markdown)
	}
	// What the store holds is what the decision read: a settlement would stamp no id into it and
	// index no ask.
	if stored, err := service.storedTree(ctx, plain.ID); err != nil {
		t.Errorf("read the plain document's stored tree: %v", err)
	} else if repairs := pmdoc.BlockIDRepairCount(stored); repairs != 0 || pmdoc.HoldsAskBlock(stored) {
		t.Errorf("the plain document's stored tree lacks %d block ids or holds an ask block (%t), so a settlement owed it something",
			repairs, pmdoc.HoldsAskBlock(stored))
	}
	if !askingWritten.OwesSettlement {
		t.Errorf("CreateDocument answered that the document holding an ask block owes no settlement")
	}
	if !owed(asking.ID) {
		t.Errorf("the create of the document holding an ask block left no pending-settlement row")
	}
	if held, unsettled := record(asking.ID); !held || !unsettled {
		t.Errorf("the create of the document holding an ask block left its room's record held %t, unsettled %t; want an unsettled record", held, unsettled)
	}

	deadline := time.Now().Add(roomWait(t))
	if err := service.settleOwedNow(ctx, plain.ID, deadline); err != nil {
		t.Errorf("settle the plain document now: %v", err)
	}
	if takes(plain.ID) != 0 || service.roomHeld(plain.ID) {
		t.Errorf("settleOwedNow took the plain document's room %d times (held %t), want none", takes(plain.ID), service.roomHeld(plain.ID))
	}
	if err := service.settleOwedNow(ctx, asking.ID, deadline); err != nil {
		t.Errorf("settle the document holding an ask block now: %v", err)
	}
	var opened int
	if err := database.Pool.QueryRow(ctx, `select count(*) from asks where block_artifact_id = $1 and state = 'open'`, asking.ID).Scan(&opened); err != nil {
		t.Fatalf("count the opened asks: %v", err)
	}
	if takes(asking.ID) != 1 || opened != 1 || owed(asking.ID) {
		t.Errorf("settleOwedNow took the room of the document holding an ask block %d times, opened %d asks and left it owing %t; want once, one and nothing owed",
			takes(asking.ID), opened, owed(asking.ID))
	}

	// A load of the plain document's room arms no settlement: no row says one is owed.
	liveTree(t, service, plain.ID)
	state := service.lockExistingState(plain.ID)
	if state == nil {
		t.Fatal("the load of the plain document's room left no record of it")
	}
	armed, unsettled := service.isSettleTimerArmed(state.settle), state.unsettled
	service.unlockState(plain.ID, state)
	if armed || unsettled {
		t.Errorf("the load of the plain document's room armed its settlement (%t) or marked its record unsettled (%t)", armed, unsettled)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shut the service down: %v", err)
	}
	if takes(plain.ID) != 0 || owed(plain.ID) {
		t.Errorf("Shutdown took the plain document's room %d times and left it owing %t, want neither", takes(plain.ID), owed(plain.ID))
	}
	if number, markdown, _ := latestDocumentVersion(t, database, plain.ID); number != 1 || markdown != plainWritten.Markdown {
		t.Errorf("after Shutdown the plain document's latest version is %d holding %q, want version 1 holding %q", number, markdown, plainWritten.Markdown)
	}
}
