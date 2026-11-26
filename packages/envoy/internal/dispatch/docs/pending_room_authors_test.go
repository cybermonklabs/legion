package docs

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The pending authors a write of one document of an issue room reads (pendingRoomAuthors) are the
// authors of that document's edits: of the room's in-flight credits, those whose update changed its
// fragment and those that name no document, as a version of it lists them
// (captureDocumentAuthors). An upload holds the room's removals as the person those authors name
// (holdRoomRemovals), so a person whose only pending edit was in the spec, read among them, would
// be recorded as the remover of an answered ask whose block the plan's upload took out.
func TestPendingRoomAuthorsOfADocumentTakeOnlyTheCreditsOfItsFragment(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	spec, plan := uuid.NewString(), uuid.NewString()
	bob := model.Actor{Kind: "user", ID: "bob"}
	carol := model.Actor{Kind: "user", ID: "carol"}
	dana := model.Actor{Kind: "user", ID: "dana"}
	state := newRoomState()
	for index, credit := range []*inflightCredit{
		{authors: map[string]model.Actor{actorKey(bob): bob}, documents: []string{spec}},
		{authors: map[string]model.Actor{actorKey(carol): carol}, documents: []string{spec, plan}},
		{authors: map[string]model.Actor{actorKey(dana): dana}},
	} {
		credit.seq = uint64(index + 1)
		state.inflight[credit.seq] = credit
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read: %v", err)
	}
	defer tx.Rollback(ctx)

	authors, err := pendingRoomAuthors(ctx, tx, plan, &liveWrite{state: state, forkSeq: 3})
	if err != nil {
		t.Fatalf("read the plan's pending room authors: %v", err)
	}
	want := actorKeysOf([]model.Actor{carol, dana})
	if got := actorKeysOf(authors); !slices.Equal(got, want) {
		t.Fatalf("the plan's pending room authors are %q, want %q: bob's edit changed only the spec", got, want)
	}
}
