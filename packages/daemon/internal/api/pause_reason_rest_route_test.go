package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Main's project-paused notice carried the operator's reason whole (pause/pause.go:pausedNotice at
// d46ec4e0: `Pause: &record.ProjectPause{... Reason: pause.Reason}`). INFRA-3686's R1 bounds a
// person's words at 4,000 units: "Above that, it carries the first part, the count of the rest
// ... and the item's reference", and the change cuts the reason there (pause.PausedNotice,
// `words."pause.reason".rest`). roles/controller-root.md tells the controller where the rest is:
// "Where that block names a `rest`, the reason was cut: `read_state` gives it whole, in `paused`
// while that pause is open and in `lastPause` once a resume has ended it and until a later pause
// ends." The state carries one of `paused` and `lastPause` at most (PauseStateTx: "while the
// project is paused, its open pause ... and no last pause"), so from the moment a later pause
// begins no part of the state holds the cut pause. R2 of dispatch://INFRA-3686: the fields are
// "the state that the reader acts on"; a prompt that sends the reader to a field the state does
// not carry is the change stating a fact wrongly.
//
// Here the operator pauses with a reason over the bound, so the notice cuts it; resumes, after
// which `lastPause` holds the whole reason; and pauses again. While the second pause stands, which
// has not ended, the state names that pause in `paused` and carries no `lastPause` at all.
func TestTheRestOfACutPauseReasonIsWhereTheControllersPromptSaysItIs(t *testing.T) {
	h := newPauseHarness(t, pauseCapable())
	h.controllerSession()
	ctx := context.Background()
	stateOf := func() (*PausedView, *PauseView) {
		t.Helper()
		var paused *PausedView
		var last *PauseView
		if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			paused, last, err = PauseStateTx(ctx, tx, testDispatchProject, h.clock.Now())
			return err
		}); err != nil {
			t.Fatalf("read the pause state: %v", err)
		}
		return paused, last
	}

	reason := strings.Repeat("The model provider answers 529 to every call of every role. ", 80)
	first := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: reason}, bearer))
	rest := 0
	for _, notice := range h.pauseNotices() {
		if notice.Kind == record.ProjectPausedNotice && notice.Pause != nil && notice.Pause.ID == first.ID {
			rest = notice.Words["pause.reason"].Rest
		}
	}
	if rest == 0 {
		t.Fatalf("the project-paused notice of pause %d names no rest of its %d-unit reason; this test proves nothing", first.ID, len(reason))
	}

	h.clock.advance(time.Hour)
	h.pauseOK(operatorResumeRoute, nil, bearer)
	if _, last := stateOf(); last == nil || last.ID != first.ID || last.Reason != reason {
		t.Fatalf("after the resume the state's last pause is %+v, want pause %d with its whole reason", last, first.ID)
	}

	h.clock.advance(time.Hour)
	second := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "a second outage"}, bearer))
	open, last := stateOf()
	if open == nil || open.ID != second.ID {
		t.Fatalf("while the second pause stands the state's pause is %+v, want pause %d", open, second.ID)
	}

	body, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "controller-root.md"))
	if err != nil {
		t.Fatal(err)
	}
	const route = "in `lastPause` once a resume has ended it and until a later pause ends"
	if strings.Contains(string(body), route) && (last == nil || last.ID != first.ID || last.Reason != reason) {
		t.Errorf("roles/controller-root.md says the cut reason of a project-paused notice is %q; pause %d's notice cut %d units of its reason, "+
			"and while pause %d, which has not ended, stands the state carries `paused` %d and `lastPause` %+v: no field of it holds pause %d's reason",
			route, first.ID, rest, second.ID, open.ID, last, first.ID)
	}
}
