package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A run of comment marks written into an issue room no room holds, one after another inside the
// settle delay, forks the room's stored history for its first two writes alone. The first loads
// no room at its publish (audit finding 11), the second loads the room at its publish
// (publishLoadsRoom), and every later one forks that room (warmFork). A write that forks the store
// decodes the room's whole stored history, which each comment's mark makes longer, so without the
// room every comment of the run costs more than the one before: on c-tree-read-bound's spec of
// 1,393 requirements the marks of comments 2,201 to 2,400 took 512 ms each that way, against 45 ms
// on the room.
func TestARunOfCommentMarksForksTheIssueRoomFromItsThirdWrite(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	var spec strings.Builder
	const requirements = 3
	for index := 1; index <= requirements; index++ {
		fmt.Fprintf(&spec, ":::requirement{#r%d}\nThe requirement r%d holds one paragraph.\n:::\n\n", index, index)
	}
	seedServiceText(t, service, room.spec, spec.String())
	if service.roomHeld(room.tree) {
		t.Fatal("the issue room is held before the run of comments, so nothing here forks the store")
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	const comments = 6
	for index := range comments {
		block := fmt.Sprintf("r%d", index%requirements+1)
		var cold, opened bool
		err := joinedWrite(service, func(ctx context.Context) error {
			mark := MarkSpec{Kind: MarkComment, ID: fmt.Sprintf("comment-%d", index+1), By: alice}
			if _, err := service.MarkQuote(ctx, room.spec, mark, "The requirement "+block+" holds one paragraph.", &QuoteScope{BlockID: block}); err != nil {
				return err
			}
			if write := joinedLiveWrite(ctx, room.spec); write != nil {
				cold, opened = write.cold, true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("mark comment %d in %s: %v", index+1, block, err)
		}
		if !opened {
			t.Fatalf("comment %d's mark opened no write on the issue room", index+1)
		}
		held := service.roomHeld(room.tree)
		switch index {
		case 0:
			if !cold || held {
				t.Errorf("the run's first mark forked the store %t and left the room held %t, want a fork of the store and no room held (audit finding 11)", cold, held)
			}
		case 1:
			if !cold || !held {
				t.Errorf("the run's second mark forked the store %t and left the room held %t, want a fork of the store and the room loaded at its publish", cold, held)
			}
		default:
			if cold {
				t.Errorf("comment %d's mark forked the room's stored history, want a fork of the room the run's second mark loaded: each such fork decodes every mark the comments before it wrote", index+1)
			}
		}
	}
}
