package supervise

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A reviewer works in a clone of its own, which the launch that starts its process makes or
// brings to the head the launch's spec gives it (runtime.SpawnSpec.ReviewHead): the daemon's host
// does it on tmux (internal/daemon's specs.reviewerGit), and the reviewer's role-init does it in a
// pod (internal/launcher's roleWorkspace). Nothing moves that clone while the process runs, and a
// reviewer outlives the round its process was launched for: the next round, and an ask of its
// verdict past review, reach the same process. So the daemon hands a reviewer nothing at a head
// its process was not launched at. Where the head the record holds for the issue's pull request
// now (Deps.ReviewHead) is not the head the reviewer's launch gave it, the process is replaced, on
// the same session, by a launch that reads the record's head (ReviewHeadMoved,
// relaunchAtReviewHead), and what was held for it goes to the process that launch starts, once it
// is ready. That launch refuses to start the reviewer where its clone cannot stand at the head
// (workspace.ProvisionReviewClone).
//
// The relaunch goes through the launch path every replacement takes (replaceProcess): it is paced
// as every launch is (reserveWake), a held suspension ends it in a suspension, and the process it
// lets go records what that ended (endReviewHead), which the next process is told.
//
// Where the clone stands is the claim's own record of its launch (Claim.ReviewLaunchHead and
// ReviewLaunchGeneration), which the store keeps, so a daemon that restarts and re-adopts a live
// reviewer whose head has not moved hands it its next task in the process it found running. Only a
// row that records no head for the process the claim records — one a release before this one
// launched — sends that reviewer's next task to a launch at the record's head.

// reviewAt is a head the record held, at a generation of the claim's, as reviewRaisedFor keeps the
// one a relaunch was raised for; known is false while none was raised.
type reviewAt struct {
	generation uint64
	head       string
	known      bool
}

// launchedAt is whether the launch that started the process the claim records gave the reviewer
// head (Claim.ReviewLaunchHead): its record names the claim's own generation, which no launch
// before this release's wrote.
func (m *Machine) launchedAt(head string) bool {
	return m.claim.ReviewLaunchGeneration != 0 && m.claim.ReviewLaunchGeneration == m.claim.Generation &&
		m.claim.ReviewLaunchHead == head
}

// reviewHeadHolds is whether the reviewer's process works at the head the record holds now, which
// every send to a reviewer asks (sendPending) and so does every message the daemon holds for one
// (ReviewHeadHolds). It holds for every role but the reviewer, for a daemon that reads no head
// (Deps.ReviewHead nil) and for an issue whose record holds none. Where it does not hold, a ready
// or idle reviewer's relaunch at the record's head is raised (raiseReviewRelaunch); a reviewer in
// a turn is left to it, and the next send after the turn asks again. A relaunch is raised once
// for each head the record holds: where the launch it made still gave the reviewer another head
// (the spec builder gave none, which it does for an issue whose repository it cannot read), a
// second would do the same, so the task is refused instead, unsent, with one error line, until
// the record holds another head. An error is the record's read, and holds nothing. The caller
// holds mu.
func (m *Machine) reviewHeadHolds(ctx context.Context) (bool, error) {
	if m.claim.Role != claim.RoleReviewer || m.deps.ReviewHead == nil {
		return true, nil
	}
	head, err := m.deps.ReviewHead(ctx, m.claim.Issue)
	if err != nil {
		return false, err
	}
	if head == "" || m.launchedAt(head) {
		return true, nil
	}
	if raised := m.reviewRaisedFor; raised.known && raised.head == head && m.claim.Generation > raised.generation {
		if m.reviewRefused != head {
			m.reviewRefused = head
			m.log.Error("supervise: the reviewer was started again for the record's head and its launch gave it another; it is handed nothing until the record holds another head",
				"head", head, "launchedAt", m.claim.ReviewLaunchHead, "generation", m.claim.Generation)
		}
		return false, nil
	}
	if m.claim.State == StateReady || m.claim.State == StateIdle {
		m.raiseReviewRelaunch(head)
	}
	return false, nil
}

// raiseReviewRelaunch posts ReviewHeadMoved to this machine, on a goroutine of its own, so the
// relaunch is decided by the table under the lock the caller is about to give up, and is counted
// with the sends Wait waits for. A relaunch already raised and not yet decided raises none.
func (m *Machine) raiseReviewRelaunch(head string) {
	if m.reviewRaising {
		return
	}
	m.reviewRaising = true
	m.reviewRaisedFor = reviewAt{generation: m.claim.Generation, head: head, known: true}
	ev := ReviewHeadMoved{Claim: m.claim.Token, Generation: m.claim.Generation, Head: head}
	m.log.Info("supervise: the reviewer's process works at another head than its record's; it is started again at the record's head before it is handed anything",
		"launchedAt", m.claim.ReviewLaunchHead, "launchGeneration", m.claim.ReviewLaunchGeneration, "head", head, "generation", ev.Generation)
	m.goroutines++
	go func() {
		if err := m.Handle(m.worker(), ev); err != nil {
			m.log.Error("supervise: the relaunch at the reviewer's head", "head", head, "error", err)
		}
		m.mu.Lock()
		m.goroutines--
		m.idle.Broadcast()
		m.mu.Unlock()
	}()
}

// relaunchAtReviewHead is ReviewHeadMoved's row: the process launched at generation is replaced
// on its session (replaceProcess), and the launch that replaces it builds its spec again, reading
// the record's head. It decides nothing where the claim has moved past that launch, where a launch
// is already waiting to replace the process (launchWaits), which reads the record's head itself,
// and where the launch since gave the reviewer that head.
func relaunchAtReviewHead(m *Machine, ctx context.Context, ev Event) error {
	moved := ev.(ReviewHeadMoved)
	if moved.Generation != m.claim.Generation || m.claim.Role != claim.RoleReviewer || m.launchWaits() ||
		m.launchedAt(moved.Head) {
		return nil
	}
	_, err := m.replaceProcess(ctx, "the reviewer's process is started again at the head under review", ordinaryEnd, endReviewHead)
	return err
}

// ReviewHeadHolds is reviewHeadHolds for a message the daemon holds for a reviewer outside its
// tasks (internal/daemon's deliverHeld: a merge forward's review, an ask of its verdict past
// review): such a message reaches the reviewer's session through Envoy, not through a send of
// this machine, and is published only while this holds. A reviewer that is not ready, idle or in
// a turn holds nothing: a process still launching is waited for, and the launch reads the
// record's head itself. Nor does a reviewer whose task waits for a reason of the daemon's
// (Deps.DeliveryWaits), which sendPending asks before the head: its issue pod runs another image,
// so its relaunch would be refused there (internal/daemon's reviewerPods), and the message waits
// for the process the pod's replacement starts, with no relaunch raised. It answers false then,
// with that wait as its error, so the message's row waits on what the task waits on. It takes the
// lock only when the lock is free, and answers ErrMachineBusy otherwise, as PublishUpdateAsk does,
// so the outbox never waits on a launch in flight.
func (m *Machine) ReviewHeadHolds(ctx context.Context) (bool, error) {
	if !m.mu.TryLock() {
		return false, ErrMachineBusy
	}
	defer m.unlock()
	switch m.claim.State {
	case StateReady, StateIdle, StateWorking:
		if m.claim.Role == claim.RoleReviewer && m.deps.DeliveryWaits != nil {
			if waits := m.deps.DeliveryWaits(m.claim); waits != nil {
				return false, waits
			}
		}
		return m.reviewHeadHolds(ctx)
	}
	return m.claim.Role != claim.RoleReviewer, nil
}
