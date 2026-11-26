package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// releasedAgentSource is the processed_events source the daemon marks a message for a released
// agent given under, by the message's key (answerloop.Wake.Key), in the transaction that queues
// its row: the mark is what gives a message once, across redeliveries and restarts.
const releasedAgentSource = string(record.OutboxKindAgentMessage)

// releasedWakes gives a message to an agent whose claim the daemon released (answerloop.Wakes):
// in one transaction it marks the message given by its key and queues one agent_message row on
// the agent's issue (record.AgentMessage), which the outbox delivers as it delivers a
// merge-queue removal: it wakes the claim, and publishes the message to the role's topic once the
// woken session holds the role (agentMessage). A project's pause holds the row until the resume.
// A redelivery, and a restart's, finds the mark and queues nothing more. The outbox's records
// wake the outbox once the transaction commits (projectRecords).
//
// The message is the agent's own: a person's decision on its ask; every other Dispatch event that
// reached it, asks it nothing and is no act of its own session, such as a message, comment or
// reply, an edit or resolution of one, or the answer to, a reply on, or the resolution or edit of
// an ask it follows but did not open (answerloop.Verdict.ForAgent); a message of a source no agent
// answers; a question whose reply the loop does not post; or the loop's own answer to it as an
// asker. So the architect and the controller are told nothing of it, and the woken agent acts on
// it.
type releasedWakes struct {
	outbox *outbox
	now    func() time.Time
	log    *slog.Logger
}

func (w releasedWakes) Wake(ctx context.Context, wake answerloop.Wake) error {
	queued := false
	// The write is its own transaction (store.Transact), which runs the commit hook the row's
	// write registers once it commits.
	if err := store.Transact(ctx, w.outbox.pool, func(ctx context.Context, tx pgx.Tx) error {
		fresh, err := w.outbox.records.MarkProcessed(ctx, tx, releasedAgentSource, wake.Key)
		if err != nil || !fresh {
			return err
		}
		row, err := record.NewOutboxRow(wake.Issue, record.AgentMessage{Role: wake.Role, Session: wake.Session, Text: wake.Text}, w.now())
		if err != nil {
			return fmt.Errorf("the message %s for the released %s: %w", wake.Key, wake.Session, err)
		}
		queued = true
		return w.outbox.records.Enqueue(ctx, tx, row)
	}); err != nil {
		return fmt.Errorf("give the message %s to the released %s: %w", wake.Key, wake.Session, err)
	}
	if queued {
		w.log.Info("answers: a message for a released agent is queued to wake it", "key", wake.Key, "issue", wake.Issue,
			"role", wake.Role, "session", wake.Session)
	}
	return nil
}

// agentMessage publishes the message of an agent_message row (record.AgentMessage), one the
// answer loop does not answer for an agent whose claim the daemon released (releasedWakes), to
// the Envoy topic of that agent's role on the row's issue, and to nothing else. The row is
// delivered as deliverHeld delivers a held message: a released claim is woken, and the row waits
// on its backoff, or for the turn of a launch the wake pacing holds (parkedWait), until a session
// holds the role, and then publishes once. The row and not the listener keeps the message: a kept
// copy lives one hour from its publish, which a wake the pacing or a pause holds can outlast. A
// project's pause holds the row (record.pauseHolds), and the row's first attempt after the resume
// wakes the role with that pause's held wake (record.OutboxRow.HeldByPause).
//
// Each attempt first finishes the row undelivered, with one log line naming why, once no wake can
// bring the claim back: the issue is not recorded, or it has left the workflow or its tree
// lingers (wakeable), where a close stops the claim rather than a wake resuming it. What
// follows — a claim that failed or retired, the design gate, the wake and the publish — is
// deliverHeld's.
func (r *outbox) agentMessage(ctx context.Context, row record.OutboxRow, payload record.AgentMessage) error {
	if r.notices == nil {
		return errors.New("agent message executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("agent message executor has no claim supervisor")
	}
	token, err := roleToken(r.project, row, payload.Role)
	if err != nil {
		return err
	}
	attributes := []any{"row", row.ID, "issue", row.Issue, "role", payload.Role, "session", payload.Session}
	recorded, err := r.recordedIssue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if recorded == nil {
		r.log.Info("outbox message for a released agent finished undelivered: the issue is not recorded", attributes...)
		return nil
	}
	issue := *recorded
	wakeable, err := r.wakeable(ctx, issue)
	if err != nil {
		return err
	}
	if !wakeable {
		r.log.Info("outbox message for a released agent finished undelivered: the issue has left the workflow or its tree lingers",
			append(attributes, "status", issue.Status, "phase", issue.Phase)...)
		return nil
	}
	return r.deliverHeld(ctx, row, heldMessage{issue: issue, token: token, role: payload.Role,
		what: fmt.Sprintf("the message for the released %s of %s", payload.Role, row.Issue), message: payload.Text,
		attributes: attributes,
		claimEnded: "outbox message for a released agent finished undelivered: the role's claim has failed or retired",
		delivered:  "outbox message for a released agent delivered"})
}
