package pause

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// These tests run the service's pause and resume over a real PostgreSQL through a proxy that cuts
// one transaction's commit (testdb.CommitFaults). An act whose commit landed keeps its success
// work once — its line, its flag and the daemon's Effects — whether its reply came back, the store
// said later that it committed, or the store could not say until it answered again; an act whose
// commit never landed does none of it, and its retry does.

// faultedRig is a pause service over a store opened through the fault proxy, beside direct, the
// same database opened without it, which reads what committed.
type faultedRig struct {
	*rig
	direct *store.Store
	fault  *testdb.CommitFault
}

func newFaultedRig(t *testing.T) *faultedRig {
	t.Helper()
	direct := testpg.CloneStore(t, "legion_pause")
	proxied, fault := testdb.CommitFaults(t, direct.Pool().Config().ConnString())
	faulted, err := store.Open(context.Background(), proxied)
	if err != nil {
		t.Fatalf("open the store through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	seedClaims(t, direct)
	return &faultedRig{rig: rigOn(t, faulted, &testClock{at: pausedAt}, capable()), direct: direct, fault: fault}
}

// openPause is the open pause the store holds, read directly, 0 for none.
func (r *faultedRig) openPause(t *testing.T) int64 {
	t.Helper()
	open, found, err := r.direct.OpenPause(context.Background(), "LEGION")
	if err != nil {
		t.Fatalf("read the open pause: %v", err)
	}
	if !found {
		return 0
	}
	return open.ID
}

// wantCounts fails the test for each kind of success work not counted as want says.
func wantCounts(t *testing.T, got, want map[string]int, when string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got[name] != want[name] {
			t.Errorf("%s %s = %d, want %d", when, name, got[name], want[name])
		}
	}
}

func oneIf(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pauseWork counts a pause's success work: its line, the daemon's Effects.Paused, and a flag
// holding the pause the store holds open.
func (r *faultedRig) pauseWork(t *testing.T) map[string]int {
	t.Helper()
	open := r.openPause(t)
	flag, _ := r.svc.Paused()
	return map[string]int{
		"pause lines":             len(lines(t, r.log, "pause: the project is paused")),
		"Effects.Paused calls":    len(r.effects.pausedCalls()),
		"flags holding the pause": oneIf(open != 0 && flag == open),
	}
}

func each(n int) map[string]int {
	return map[string]int{"pause lines": n, "Effects.Paused calls": n, "flags holding the pause": n}
}

const pauseMatch = "insert into project_pauses"

// A pause whose commit's reply is lost keeps its success work: the store says the transaction
// committed, so its line, its flag and its Effects come once, and its retry, which the store
// refuses as already paused, does none of them again. A pause whose commit never landed does
// none of them, and its retry does. A pause whose outcome the store cannot say sets no flag and
// runs no Effects; its line follows the store's answer, and the next Refresh sets the flag and
// runs the Effects, once.
func TestAPauseWhoseCommitReplyIsLostKeepsItsSuccessWork(t *testing.T) {
	ctx := context.Background()
	replyLost := func(t *testing.T, r *faultedRig) store.Pause {
		t.Helper()
		fired := r.fault.Cut(pauseMatch, testdb.CutAfterCommit)
		pause, err := r.svc.Pause(ctx, operator, "the model provider is down")
		testdb.Met(t, fired)
		if err != nil {
			t.Errorf("the pause whose commit landed and whose reply was lost = %v, want nil", err)
		}
		if r.openPause(t) == 0 {
			t.Fatal("the pause whose reply was lost made no pause: the cut came before the commit")
		}
		wantCounts(t, r.pauseWork(t), each(1), "committed with its reply lost, but")
		return pause
	}
	t.Run("reply_lost", func(t *testing.T) {
		replyLost(t, newFaultedRig(t))
	})
	t.Run("rollback", func(t *testing.T) {
		r := newFaultedRig(t)
		fired := r.fault.Cut(pauseMatch, testdb.CutBeforeCommit)
		_, err := r.svc.Pause(ctx, operator, "the model provider is down")
		testdb.Met(t, fired)
		if err == nil {
			t.Error("the pause whose commit never landed = nil, want its failure")
		}
		if r.openPause(t) != 0 {
			t.Fatal("the pause of a commit that never landed is there")
		}
		wantCounts(t, r.pauseWork(t), each(0), "after a commit that never landed")
		if _, err := r.svc.Pause(ctx, operator, "the model provider is down"); err != nil {
			t.Fatalf("the retry with no fault = %v, want nil", err)
		}
		wantCounts(t, r.pauseWork(t), each(1), "after the retry that made the pause")
	})
	t.Run("duplicate_retry", func(t *testing.T) {
		r := newFaultedRig(t)
		first := replyLost(t, r)
		var standing *store.AlreadyPausedError
		if _, err := r.svc.Pause(ctx, operator, "again"); !errors.As(err, &standing) || standing.Standing.ID != first.ID {
			t.Fatalf("the retry after a committed pause = %v, want an AlreadyPausedError naming pause %d", err, first.ID)
		}
		wantCounts(t, r.pauseWork(t), each(1), "committed and its retry refused, but")
	})
	t.Run("unknown", func(t *testing.T) {
		r := newFaultedRig(t)
		fired := r.fault.Cut(pauseMatch, testdb.CutAfterCommitRefusing)
		_, err := r.svc.Pause(ctx, operator, "the model provider is down")
		testdb.Met(t, fired)
		if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
			t.Fatalf("the pause whose outcome the store cannot say = %v, want ErrCommitOutcomeUnknown", err)
		}
		if r.openPause(t) == 0 {
			t.Fatal("the direct store holds no open pause: the cut came before the commit")
		}
		wantCounts(t, r.pauseWork(t), each(0), "before the store answers")
		r.fault.Refuse(false)
		testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
			"the pause's line once the store says it committed", func() bool {
				return len(lines(t, r.log, "pause: the project is paused")) == 1
			})
		wantCounts(t, r.pauseWork(t), map[string]int{"Effects.Paused calls": 0, "flags holding the pause": 0},
			"once the store answered and before a Refresh, the flag is the Refresh's to set:")
		for range 2 {
			if err := r.svc.Refresh(ctx); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		}
		wantCounts(t, r.pauseWork(t), each(1), "after two Refreshes")
		if calls := r.effects.pausedCalls(); len(calls) == 1 && (calls[0].asked != nil || calls[0].flag != r.openPause(t)) {
			t.Errorf("the Refresh's Effects.Paused asked %v with the flag at %d, want no claims asked and the flag set", calls[0].asked, calls[0].flag)
		}
		var standing *store.AlreadyPausedError
		if _, err := r.svc.Pause(ctx, operator, "again"); !errors.As(err, &standing) {
			t.Fatalf("the retry after the pause the store answered = %v, want an AlreadyPausedError", err)
		}
		wantCounts(t, r.pauseWork(t), each(1), "after the retry the store refused")
	})
}

// resumeWork counts a resume's success work: its line, a line for each of the two rows the pause
// held (counted as one set), the daemon's Effects.Resumed, and a flag that holds no pause.
func (r *faultedRig) resumeWork(t *testing.T) map[string]int {
	t.Helper()
	_, flagged := r.svc.Paused()
	return map[string]int{
		"resume lines":          len(lines(t, r.log, "pause: the project is resumed")),
		"held-row line sets":    len(lines(t, r.log, HeldRowReleasedMessage)) / 2,
		"Effects.Resumed calls": len(r.effects.resumedCalls()),
		"flags cleared":         oneIf(!flagged),
	}
}

func eachResume(n int) map[string]int {
	return map[string]int{"resume lines": n, "held-row line sets": n, "Effects.Resumed calls": n, "flags cleared": n}
}

const resumeMatch = "update project_pauses"

// pausedFaultedRig is a faulted rig whose project the service paused with no fault armed, holding
// the two rows seedHeld queues.
func pausedFaultedRig(t *testing.T) (*faultedRig, store.Pause) {
	t.Helper()
	r := newFaultedRig(t)
	pause, err := r.svc.Pause(context.Background(), operator, "the model provider is down")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	seedHeld(t, r.direct)
	r.clock.Advance(30 * time.Minute)
	return r, pause
}

// A resume whose commit's reply is lost keeps its success work: its lines, its cleared flag and
// its Effects come once, and its retry, which the store refuses as not paused, does none of them
// again. A resume whose commit never landed does none of them, and its retry does. A resume whose
// outcome the store cannot say clears no flag and runs no Effects; its lines follow the store's
// answer, and the next Refresh clears the flag and runs the Effects for the pause it held, once.
func TestAResumeWhoseCommitReplyIsLostKeepsItsSuccessWork(t *testing.T) {
	ctx := context.Background()
	replyLost := func(t *testing.T, r *faultedRig) {
		t.Helper()
		fired := r.fault.Cut(resumeMatch, testdb.CutAfterCommit)
		_, _, err := r.svc.Resume(ctx, controller)
		testdb.Met(t, fired)
		if err != nil {
			t.Errorf("the resume whose commit landed and whose reply was lost = %v, want nil", err)
		}
		if r.openPause(t) != 0 {
			t.Fatal("the resume whose reply was lost ended no pause: the cut came before the commit")
		}
		wantCounts(t, r.resumeWork(t), eachResume(1), "committed with its reply lost, but")
	}
	t.Run("reply_lost", func(t *testing.T) {
		r, _ := pausedFaultedRig(t)
		replyLost(t, r)
	})
	t.Run("rollback", func(t *testing.T) {
		r, pause := pausedFaultedRig(t)
		fired := r.fault.Cut(resumeMatch, testdb.CutBeforeCommit)
		_, _, err := r.svc.Resume(ctx, controller)
		testdb.Met(t, fired)
		if err == nil {
			t.Error("the resume whose commit never landed = nil, want its failure")
		}
		if r.openPause(t) != pause.ID {
			t.Fatalf("the open pause after a resume that never landed = %d, want pause %d still", r.openPause(t), pause.ID)
		}
		wantCounts(t, r.resumeWork(t), eachResume(0), "after a commit that never landed")
		if _, _, err := r.svc.Resume(ctx, controller); err != nil {
			t.Fatalf("the retry with no fault = %v, want nil", err)
		}
		wantCounts(t, r.resumeWork(t), eachResume(1), "after the retry that ended the pause")
	})
	t.Run("duplicate_retry", func(t *testing.T) {
		r, _ := pausedFaultedRig(t)
		replyLost(t, r)
		var notPaused *store.NotPausedError
		if _, _, err := r.svc.Resume(ctx, controller); !errors.As(err, &notPaused) {
			t.Fatalf("the retry after a committed resume = %v, want a NotPausedError", err)
		}
		wantCounts(t, r.resumeWork(t), eachResume(1), "committed and its retry refused, but")
	})
	t.Run("unknown", func(t *testing.T) {
		r, pause := pausedFaultedRig(t)
		fired := r.fault.Cut(resumeMatch, testdb.CutAfterCommitRefusing)
		_, _, err := r.svc.Resume(ctx, controller)
		testdb.Met(t, fired)
		if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
			t.Fatalf("the resume whose outcome the store cannot say = %v, want ErrCommitOutcomeUnknown", err)
		}
		if r.openPause(t) != 0 {
			t.Fatal("the direct store still holds the pause open: the cut came before the commit")
		}
		wantCounts(t, r.resumeWork(t), eachResume(0), "before the store answers")
		r.fault.Refuse(false)
		testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
			"the resume's lines once the store says it committed", func() bool {
				return len(lines(t, r.log, "pause: the project is resumed")) == 1 && len(lines(t, r.log, HeldRowReleasedMessage)) == 2
			})
		wantCounts(t, r.resumeWork(t), map[string]int{"Effects.Resumed calls": 0, "flags cleared": 0},
			"once the store answered and before a Refresh, the flag is the Refresh's to clear:")
		for range 2 {
			if err := r.svc.Refresh(ctx); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		}
		wantCounts(t, r.resumeWork(t), eachResume(1), "after two Refreshes")
		if calls := r.effects.resumedCalls(); len(calls) == 1 && (calls[0].ended.ID != pause.ID || !calls[0].ended.Ended() || calls[0].flagged) {
			t.Errorf("the Refresh's Effects.Resumed got %+v, want pause %d as it ended, with the flag cleared", calls[0], pause.ID)
		}
		var notPaused *store.NotPausedError
		if _, _, err := r.svc.Resume(ctx, controller); !errors.As(err, &notPaused) {
			t.Fatalf("the retry after the resume the store answered = %v, want a NotPausedError", err)
		}
		wantCounts(t, r.resumeWork(t), eachResume(1), "after the retry the store refused")
	})
}
