package legion

// crowded.go holds the crowded key of a project's messages (dispatch://INFRA-3931): how many of a
// project's messages wait on its runner and the count's two ends (waitingCounts, Intake.finished),
// the message past stateMaxWaiting that runs on a key of its own beside the runner (crowdedKey,
// judgeCrowded), and what that message asks of the whole read in flight on the runner (readsNow,
// inFlight).

import (
	"context"
	"fmt"

	natsgo "github.com/nats-io/nats.go"
)

// stateMaxWaiting is how many messages one project keeps waiting on its runner, the one running
// included (Intake.waiting). A message that arrives while that many wait does not wait: it runs on
// its project's crowded key (crowdedKey), where judgeCrowded finishes it or has it owe a read. A
// read the daemon does not answer holds the runner for RequestTimeout, and every message that
// waits holds one message of the durable's window (stateMaxAckPending) until it is acknowledged, a
// message handed back with a delay included. So a project in such a read holds stateMaxWaiting
// messages that wait and one that holds the owed read, 17 of the 256, and 15 projects can be in
// such a read at once before the window is full.
const stateMaxWaiting = 16

// crowdedKeyPrefix begins each project's crowded key (crowdedKey). Its first byte is not UTF-8, so
// no project and no subject is a key that begins with it: messageKey decodes a project from JSON,
// which gives UTF-8 alone, and every subject of the durable begins with "notifications.".
const crowdedKeyPrefix = "\xffcrowded:"

// crowdedKey is the runner key of each message of project that arrives while stateMaxWaiting of its
// messages wait on its runner: a key of its own beside the runner, so the message waits for no read
// of the project. Its handler makes no read (judgeCrowded).
func crowdedKey(project string) string {
	return crowdedKeyPrefix + project
}

// waitingCounts is, for one subscription of the durable, how many messages of each project are
// keyed to the project's runner and not yet finished (projects), and the project of each such
// message (keyed). messageKey counts a message up as it keys it to its project's runner, and
// deliver counts it down as it returns for it (finished). A message the stop of a subscription
// hands back never reaches deliver, so a take-over starts new counts at 0 (takenOver), and a
// message keyed under the counts before is counted down in none.
type waitingCounts struct {
	projects map[string]int
	keyed    map[*natsgo.Msg]string
}

// newWaitingCounts is a subscription's counts before any message is keyed.
func newWaitingCounts() waitingCounts {
	return waitingCounts{projects: map[string]int{}, keyed: map[*natsgo.Msg]string{}}
}

// finished counts msg down among the messages that wait on its project's runner, where messageKey
// counted it under the counts of the subscription the durable delivers on now (waiting). deliver
// calls it as it returns for msg.
func (in *Intake) finished(msg *natsgo.Msg) {
	in.mu.Lock()
	defer in.mu.Unlock()
	project, keyed := in.waiting.keyed[msg]
	if !keyed {
		return
	}
	delete(in.waiting.keyed, msg)
	if in.waiting.projects[project]--; in.waiting.projects[project] <= 0 {
		delete(in.waiting.projects, project)
	}
}

// judgeCrowded is one attempt at a message on its project's crowded key (crowdedKey), which runs
// beside the project's runner while stateMaxWaiting of the project's messages wait there, behind a
// read in flight for example. It makes no read and waits for none. In one transaction that holds
// the project's position (lockCursor), it answers outcomeDropped for a project with no setting,
// finishes a message whose hold a write of the setting ended (cursor.releases, endHold), and
// answers outcomeForgotten, writing nothing, for a message the cursor finishes unapplied
// (cursor.finishesUnapplied, a message of a store the cursor drops) while no whole read of the
// project is in flight (readsNow). A read in flight can find that store behind the setting's URL
// (the same store behind a new URL, a marked store a redeploy started there), and the cursor it
// leaves then applies that store's later events, so a message of it past the cap is judged as any
// other: its events may be past that read's snapshot. Every other message owes a read:
// ReadReasonStore for a message of another store than the one a read verified behind the
// setting's URL, as judge answers it, unless the cursor drops it (cursor.drops); otherwise the one
// its position calls for against the cursor as it stands (stateMessage.unfollowed: a boot that
// does not follow calls for ReadReasonBoot), and ReadReasonGap where it calls for none, since its
// events may be past the snapshot of the read in flight. The read rule decides the rest as for a
// delivery that has made its read already, with the read in flight that inFlight answers
// (cursor.readFor), so it never makes the read now: which message holds the read and goes back to
// the stream, and which is acknowledged unapplied (outcomeCrowded). A message that goes back is
// keyed to the runner at its redelivery once fewer messages wait there, and makes the read there
// once it is due. A message the rule does not send back leaves its sequence as the highest the
// cursor skipped in its store (cursor.skips, writeSkipped), for the read in flight to judge
// (cursor.afterRead). A project with no cursor yet, whose starting read is queued and has not
// begun, is given one that follows no daemon (lockCursor), so the read it owes is recorded; a read
// gives its project a cursor before it calls the daemon (Intake.readWhole).
func (in *Intake) judgeCrowded(ctx context.Context, attempt *messageAttempt) (outcome, error) {
	message := attempt.message
	project := message.Project
	tx, err := in.pool.Begin(ctx)
	if err != nil {
		return outcomeDone, fmt.Errorf("open the transaction of the Legion state events of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateURL, current, found, err := lockCursor(ctx, tx, project)
	attempt.cursor = current
	finishes, dropped := current.finishesUnapplied(message, stateURL, attempt.seq)
	switch {
	case err != nil:
		return outcomeDone, err
	case !found:
		return outcomeDropped, nil
	case current.releases(attempt.seq):
		return endHold(ctx, tx, project, current, attempt.seq, message.StateID)
	case finishes && !in.readsNow(project):
		return outcomeForgotten, nil
	}
	reason := ReadReasonGap
	switch unfollowed := message.unfollowed(current, nil); {
	case current.follows(stateURL) && current.stateID != nil && !current.holdsStore(message.StateID) &&
		!dropped:
		reason = ReadReasonStore
	case unfollowed != "":
		reason = unfollowed
	}
	attempt.reason = reason
	// made: a message on the crowded key never makes a read, as a delivery that made one already.
	flight := in.inFlight(project, attempt.seq)
	answer := current.readFor(reason, in.now(), attempt.seq, true, flight)
	if err := owe(ctx, tx, project, answer, attempt.seq); err != nil {
		return outcomeDone, err
	}
	if answer.back == 0 {
		stateIDs, seqs := current.skips(message.StateID, message.Seq)
		if err := writeSkipped(ctx, tx, project, stateIDs, seqs); err != nil {
			return outcomeDone, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return outcomeDone, fmt.Errorf("commit the Legion state events of %s: %w", project, err)
	}
	attempt.answered(answer)
	return outcomeCrowded, nil
}

// readsNow reports whether a whole read of project is in flight on its runner (reads).
func (in *Intake) readsNow(project string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	_, reading := in.reading[project]
	return reading
}

// inFlight is the whole read in flight on project's runner as judgeCrowded judges the message at
// stream sequence seq beside it, and nil where no whole read of project is in flight (reading).
// Where the read has no first message past stateMaxWaiting yet (readInFlight.pastCap), inFlight
// records seq as that message. The read rule decides the rest from what inFlight answers
// (cursor.readFor): the read after the read in flight waits ReadRetryAfter after that read's
// start, and the read's first message past the cap takes the hold of the read after it. Every
// later message past the cap joins that hold, so the project holds one message of the durable's
// window beside those that wait on its runner (R3-h).
func (in *Intake) inFlight(project string, seq int64) *readInFlight {
	in.mu.Lock()
	defer in.mu.Unlock()
	flight, reading := in.reading[project]
	if !reading {
		return nil
	}
	if flight.pastCap == 0 {
		flight.pastCap = seq
		in.reading[project] = flight
	}
	return &flight
}
