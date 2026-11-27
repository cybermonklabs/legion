package daemon

// What the move's first release holds in the outbox (dispatch://INFRA-3583 R8), in
// TestAProjectMovesToAnotherClusterAndBack (project_move_test.go): the start owed to the claim
// behind its entry, and the two deliveries that arrive during that release, a notice for a tree's
// architect and a role message for a stopped worker, each written, held under the release and
// held to its arrival after the boot.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// moveDeliveryText is the reason of the notice that arrives for a tree's architect during the
// first release: the delivery dispatch://INFRA-3583 R9 names, a notice the outbox holds while the
// release holds every start and routes to the claim once the boot has started it
// (notice_routing.go's holdsRole).
const moveDeliveryText = "the move test's delivery, which arrived during the release"

// moveRoleMessageBody is the body of the role message that arrives for a stopped worker during the
// first release, as the workflow tells a worker a head's check results (workflow's tellChecks). A
// role message publishes with no keep (notify.Publisher.Publish), so a publish while no session
// holds its role ends the row undelivered and the message is lost (outbox's roleMessage). For a
// claim the release stopped the outbox has the row wait instead, until the boot has started the
// claim and its session holds its role again (awaitsBootStart). An early publish of the other
// kinds of delivery to such a claim still arrives: a notice waits for its architect while that
// architect can hold its role again (noticeAnswered), and a hook prompt's row keeps the prompt and
// tries again until a session holds the role (publishKeptToRole). An early publish of a role
// message is lost, so the role message is the delivery the move reads to check that wait.
const moveRoleMessageBody = "CI settled at head while the move test's release held this message for its role."

// awaitHeldStart waits until the outbox holds the start the claim behind its entry is owed, which
// the daemon's entry sweep or its admission writes while the release holds every start: the boot
// then writes no second (StartFor), and every start the boot writes is its own.
func (m *projectMove) awaitHeldStart(t *testing.T, rd *moveRelease) {
	t.Helper()
	key := idleRigKey(m.silent.key, string(m.silent.role))
	idleRigEventually(t, "the outbox holding the start the claim behind its entry is owed", func() bool {
		starts, _ := rd.starts.seen()
		return len(starts[key]) > 0
	})
}

// awaitHeldMessage waits until the outbox has read the role message's row under the release and
// put it back unrun: the row stands with the release's hold as its last error and no attempt
// counted (outbox.holdForRelease). The outbox finds a row the test wrote at its next sweep, which
// comes within outboxLease, so the row is waited for rather than read at the release's end.
func (m *projectMove) awaitHeldMessage(t *testing.T) {
	t.Helper()
	r := m.rig
	var rows []idleRigOutboxRow
	idleRigEventually(t, "the outbox holding the role message for the release", func() bool {
		rows = r.outboxRowsOf(m.clean.key, record.OutboxKindRoleMessage)
		return len(rows) != 1 || strings.Contains(rows[0].lastError, api.ProjectReleaseHeldSentence)
	})
	if len(rows) != 1 || rows[0].attempts != 0 {
		t.Fatalf("the outbox holds the role-message rows %+v for %s under the release, want the one row, held with no attempt counted", rows, m.clean.key)
	}
}

// deliverNotice writes the notice for the clean tree's architect into the outbox, the row the
// workflow writes when it tells an architect of a child's status (workflow's child-status notices).
func (m *projectMove) deliverNotice(t *testing.T) {
	t.Helper()
	r := m.rig
	ctx := context.Background()
	row, err := record.NewOutboxRow(m.clean.key, record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: moveDeliveryText}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, row) }); err != nil {
		t.Fatalf("write the notice for %s's architect: %v", m.clean.key, err)
	}
}

// deliverRoleMessage writes into the outbox the role message for the clean pod's implementer, a
// claim the release has stopped by then: the row the workflow writes when it tells a worker a
// head's check results (workflow's tellChecks), addressed as the workflow addresses it, to the
// claim's token.
func (m *projectMove) deliverRoleMessage(t *testing.T) {
	t.Helper()
	r := m.rig
	ctx := context.Background()
	row, err := record.NewOutboxRow(m.clean.key, record.RoleMessage{Role: string(r.token(m.clean)), Body: moveRoleMessageBody}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error { return r.records.Enqueue(ctx, tx, row) }); err != nil {
		t.Fatalf("write the role message for %s's %s: %v", m.clean.key, m.clean.role, err)
	}
}

// checkDelivery holds the delivery that arrived during the release to dispatch://INFRA-3583 R8: it
// reached its claim after the boot's start, on the process that start ran, once.
func (m *projectMove) checkDelivery(t *testing.T, rd *moveRelease) {
	t.Helper()
	r := m.rig
	role := string(claim.RoleArchitect)
	// The release put the notice's row back unrun every releaseHoldSpacing (outbox.holdForRelease),
	// so its first run after the boot comes up to that long after the daemon stopped, and while
	// its architect is still coming back the row waits on its backoff, whose steps climb to 64
	// seconds (retryBackoff).
	idleRigEventually(t, "the delivery reaching "+m.clean.name+"'s architect", func() bool {
		return idleRigCount(r.agents.messages(m.clean.key, role), moveDeliveryText) > 0
	})
	seen := r.agents.role(m.clean.key, role)
	if got := idleRigCount(seen.Messages, moveDeliveryText); got != 1 {
		t.Errorf("the delivery reached %s's architect %d times, want once", m.clean.name, got)
	}
	for _, message := range seen.Messages {
		if strings.Contains(message.Text, moveDeliveryText) && (message.At.Before(rd.restarted) || message.At.Before(seen.RegisteredAt)) {
			t.Errorf("the delivery reached %s's architect at %s, before the restart at %s or before its process registered at %s",
				m.clean.name, message.At, rd.restarted, seen.RegisteredAt)
		}
	}
}

// checkRoleMessage holds the role message that arrived during the release to
// dispatch://INFRA-3583 R8: its row left the outbox with none of the lines roleMessage logs as it
// ends a row undelivered, and the message reached its claim after the boot's start, on the
// process that start ran, once. A role message publishes with no keep, so a publish while no
// session holds the role ends the row undelivered and the message is lost. No session holds the
// role from the boot until the process the boot's start runs takes it, so the row has to wait
// that out.
func (m *projectMove) checkRoleMessage(t *testing.T, rd *moveRelease) {
	t.Helper()
	r := m.rig
	role := string(m.clean.role)
	// The row waits on its backoff while its claim comes back, whose steps climb to 64 seconds
	// (retryBackoff), so its last run comes up to that long after the claims run again.
	idleRigEventually(t, "the role message's row leaving the outbox", func() bool {
		return len(r.outboxRowsOf(m.clean.key, record.OutboxKindRoleMessage)) == 0
	})
	// Each line roleMessage logs as it ends a row undelivered (outbox.go's roleMessage).
	undelivered := []string{
		"outbox role message finished undelivered: its role's pod is released, and the message is not worth a wake",
		"outbox role message finished undelivered: its role has no live holder, or its claim records a session it does not hold",
		"outbox role message finished undelivered: its role's holder does not advertise the delivery mode",
		"outbox role message finished undelivered: the listener's own NATS grant denied the forward",
	}
	ended := false
	for _, line := range undelivered {
		if count := r.logged(line, map[string]string{"issue": m.clean.key}); count != 0 {
			t.Errorf("the daemon ended the role message for %s's %s undelivered %d times, logging %q, want it delivered once its claim held its role again",
				m.clean.name, role, count, line)
			ended = true
		}
	}
	if ended {
		return
	}
	// The row leaves the outbox once the listener has answered the publish, and the role process
	// records the message as its session's consumer relays it, so that record is waited for.
	idleRigEventually(t, "the role message reaching "+m.clean.name+"'s "+role, func() bool {
		return idleRigCount(r.agents.messages(m.clean.key, role), moveRoleMessageBody) > 0
	})
	seen := r.agents.role(m.clean.key, role)
	if got := idleRigCount(seen.Messages, moveRoleMessageBody); got != 1 {
		t.Errorf("the role message reached %s's %s %d times, want once", m.clean.name, role, got)
	}
	for _, message := range seen.Messages {
		if strings.Contains(message.Text, moveRoleMessageBody) && (message.At.Before(rd.restarted) || message.At.Before(seen.RegisteredAt)) {
			t.Errorf("the role message reached %s's %s at %s, before the restart at %s or before its process registered at %s",
				m.clean.name, role, message.At, rd.restarted, seen.RegisteredAt)
		}
	}
}
