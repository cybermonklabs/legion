package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// The answer loop attachAnswers builds (answersLoop) reads the project's pause flag, the one the
// daemon's pause service holds: not paused while the project runs, paused while a pause stands,
// made through the store's pause transaction and read into the service's flag as the outbox's
// sweep reads it (pause.Service.Refresh), and not paused once the service's resume has ended it.
// No proof reaches this wiring, since the rig's tmux daemon cannot pause, and it is what pauses
// the loop in production.
func TestTheAnswerLoopReadsTheProjectsPauseFlag(t *testing.T) {
	r := newPauseRig(t)
	w := &workflowRuntime{pauses: r.pauses, log: quietLogger()}
	loop := w.answersLoop(config.Config{Answers: &config.Answers{}}, plan{}, &supervision{}, r.st, nil, ghTools{})
	if loop.Paused == nil {
		t.Fatal("the answer loop is handed no pause flag; want the project's")
	}
	if loop.Paused() {
		t.Fatal("the answer loop reads paused while the project runs")
	}
	held := r.pauseLegion()
	if !loop.Paused() {
		t.Fatalf("the answer loop reads the project running while pause %d stands", held.ID)
	}
	if ended := r.resume(); loop.Paused() {
		t.Fatalf("the answer loop reads paused once pause %d has ended", ended.ID)
	}
}
