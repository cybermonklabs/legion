package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A role message or hook prompt whose publish the listener answered 504 receipt_timeout was stored
// in the stream for the holder the answer names: that session reads the copy from its own consumer
// whenever it runs again, as w5's resumed implementer read both of its rows at 16:24:26Z on
// 2026-11-16. A later publish of the row under the same key is dropped by the stream as the copy
// it already holds, and the listener answers receipt_timeout again, for ever. So once a newer
// process of the claim says ready on that same session — the relaunch that brings it back, the role
// held again — the row finishes with no new publish (R5). Every other earlier answer — a publish
// that was not stored, a holder that is another session — is published again under the same key, as
// before.

// storingPublisher answers each publish to a topic it stores for with the listener's 504
// receipt_timeout naming that session, and delivers every other publish.
type storingPublisher struct {
	mu        sync.Mutex
	storesFor map[string]string
	attempts  map[string]int
	delivered []outboxPublish
}

func newStoringPublisher() *storingPublisher {
	return &storingPublisher{storesFor: map[string]string{}, attempts: map[string]int{}}
}

func (p *storingPublisher) Publish(_ context.Context, topic, message string, payload any, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts[key]++
	if holder, ok := p.storesFor[topic]; ok {
		return fmt.Errorf("publish notice to %s: listener returned 504 Gateway Timeout: %w", topic,
			notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: holder, EventID: "evt-" + key})
	}
	p.delivered = append(p.delivered, outboxPublish{topic: topic, message: message, key: key, payload: payload})
	return nil
}

// PublishKept answers as Publish does: a stored publish is the same 504, and a delivered one was
// taken by its live holder, so nothing is kept.
func (p *storingPublisher) PublishKept(ctx context.Context, topic, message string, payload any, key string) (bool, error) {
	return false, p.Publish(ctx, topic, message, payload, key)
}

func (p *storingPublisher) storeFor(topic, holder string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if holder == "" {
		delete(p.storesFor, topic)
		return
	}
	p.storesFor[topic] = holder
}

func (p *storingPublisher) attemptsOf(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts[key]
}

func (p *storingPublisher) deliveredKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var keys []string
	for _, published := range p.delivered {
		keys = append(keys, published.key)
	}
	return keys
}

// relaunchedOnto points runner at a supervisor whose implementer claim on LEGION-208 is a newer
// process of generation 2, in state, on session: the daemon's view once the relaunch is back.
func relaunchedOnto(t *testing.T, runner *outbox, session string, state supervise.ClaimState) {
	t.Helper()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	if _, err := sup.restore(context.Background(), []supervise.Claim{{Token: token,
		Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 2, ServingGeneration: 2,
		State: state, Session: session, SessionFile: t.TempDir() + "/session.jsonl", LastReady: time.Now(),
		Locator: pluginTestLocator(token)}}); err != nil {
		t.Fatalf("restore the relaunched implementer: %v", err)
	}
	runner.supervisor = sup
}

// storedRows are the two kinds the rule covers, each as its row's payload for the implementer.
func storedRows(t *testing.T) map[string]record.OutboxPayload {
	t.Helper()
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	return map[string]record.OutboxPayload{
		"a role message": record.RoleMessage{Role: string(token), Body: "LEGION-208: the head's checks settled green."},
		"a hook prompt":  firedHook,
	}
}

func TestAMessageTheListenerStoredForTheSessionThatHoldsTheRoleAgainFinishesWithNoPublish(t *testing.T) {
	for name, payload := range storedRows(t) {
		t.Run(name, func(t *testing.T) {
			publisher := newStoringPublisher()
			runner, sup, pool := hookRunner(t, publisher)
			implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
			topic := notify.RoleTopicPrefix + string(implementer.Claim().Token)
			publisher.storeFor(topic, implementer.Claim().Session)
			row := mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute))
			enqueueOutbox(t, pool, runner.records, row)
			key := record.OutboxKey(outboxRowIDs(t, pool)[0])

			drain(t, runner)
			if got := publisher.attemptsOf(key); got != 1 {
				t.Fatalf("publishes of %s = %d, want the first", key, got)
			}
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows left = %d, want the row kept while its session has not come back", left)
			}

			// The controller relaunches the claim onto the same session, which says ready: the
			// session holds its role again and reads the stored copy from its own consumer.
			relaunchedOnto(t, runner, implementer.Claim().Session, supervise.StateIdle)
			dueNow(t, pool)
			drain(t, runner)
			if got := publisher.attemptsOf(key); got != 1 {
				t.Fatalf("publishes of %s = %d after the session holds its role again, want no new one", key, got)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the row finished: the listener stored it for the session that holds the role again", left)
			}
		})
	}
}

// Control: a publish that was not stored — the listener did not answer — is published again
// under the same key once the session holds its role again.
func TestAMessageWhoseEarlierPublishWasNotStoredIsPublishedAgain(t *testing.T) {
	for name, payload := range storedRows(t) {
		t.Run(name, func(t *testing.T) {
			publisher := &flakyPublisher{holderPublisher: &holderPublisher{}, failures: 1}
			runner, sup, pool := hookRunner(t, publisher)
			implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute)))
			drain(t, runner)

			relaunchedOnto(t, runner, implementer.Claim().Session, supervise.StateIdle)
			dueNow(t, pool)
			drain(t, runner)
			if _, delivered := publisher.snapshot(); len(delivered) != 1 {
				t.Fatalf("delivered = %#v, want the row published again", delivered)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the delivered row finished", left)
			}
		})
	}
}

// Control: a copy the listener stored for one session is published again when another session
// holds the role now, which has no such copy in its consumer.
func TestAMessageStoredForAnotherSessionIsPublishedAgain(t *testing.T) {
	for name, payload := range storedRows(t) {
		t.Run(name, func(t *testing.T) {
			publisher := newStoringPublisher()
			runner, sup, pool := hookRunner(t, publisher)
			implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
			topic := notify.RoleTopicPrefix + string(implementer.Claim().Token)
			publisher.storeFor(topic, implementer.Claim().Session)
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute)))
			key := record.OutboxKey(outboxRowIDs(t, pool)[0])
			drain(t, runner)

			publisher.storeFor(topic, "")
			relaunchedOnto(t, runner, "ses-fresh", supervise.StateIdle)
			dueNow(t, pool)
			drain(t, runner)
			if got := publisher.attemptsOf(key); got != 2 {
				t.Fatalf("publishes of %s = %d, want it published again to the role's other session", key, got)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the delivered row finished", left)
			}
		})
	}
}

// Control: once the stored row finishes, a later role message to the same implementer is
// published once and finishes, and the stored row is not published again behind it.
func TestALaterRoleMessageIsNotQueuedBehindAStoredOne(t *testing.T) {
	publisher := newStoringPublisher()
	runner, sup, pool := hookRunner(t, publisher)
	implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	token := implementer.Claim().Token
	topic := notify.RoleTopicPrefix + string(token)
	publisher.storeFor(topic, implementer.Claim().Session)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208",
		record.RoleMessage{Role: string(token), Body: "LEGION-208: the first message."}, time.Now().Add(-time.Minute)))
	stored := record.OutboxKey(outboxRowIDs(t, pool)[0])
	drain(t, runner)

	// The session comes back: the listener now confirms what it forwards.
	publisher.storeFor(topic, "")
	relaunchedOnto(t, runner, implementer.Claim().Session, supervise.StateIdle)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208",
		record.RoleMessage{Role: string(token), Body: "LEGION-208: the later message."}, time.Now().Add(-time.Second)))
	dueNow(t, pool)
	for range 3 {
		drain(t, runner)
		dueNow(t, pool)
	}
	if got := publisher.attemptsOf(stored); got != 1 {
		t.Errorf("publishes of the stored row %s = %d, want only its first", stored, got)
	}
	if delivered := publisher.deliveredKeys(); len(delivered) != 1 || delivered[0] == stored {
		t.Errorf("delivered keys = %v, want the later message alone, once", delivered)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Errorf("outbox rows left = %d, want both rows finished", left)
	}
}

// outboxRowIDs is the ids of the outbox's rows, oldest first.
func outboxRowIDs(t *testing.T, pool *store.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select id from outbox order by id")
	if err != nil {
		t.Fatalf("read the outbox's rows: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read an outbox row's id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}
