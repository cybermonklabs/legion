package outbox

import (
	"context"
	crand "crypto/rand"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestADeliveredRoleIsLeftOutPastTheAddressBound is this change's R2 at the bound for the
// outbox: a role name carries no length bound, and a role of 1,400 bytes once failed
// graph_tuples_once with "index row size ... exceeds ... 2704" inside recordBatch's one statement,
// so the issue's outbox never drained again. deliveredGraph now leaves out every node, page and
// tuple whose address is past graphstore.MaxAddressBytes, so the record commits and the stream
// drains.
//
// Three targeted messages share one issue's batch: a role of exactly the bound, whose node is
// written (its page, role/<name>, is five bytes longer and past the bound, so it is left out), a
// role a byte over, whose node and page are both left out, and the 1,400-byte role, left out
// whole. All three publish, the store then holds no node and no tuple end past the bound, and a
// record after them publishes too.
func TestADeliveredRoleIsLeftOutPastTheAddressBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	issueKey := "RB-1"
	seedIssue(t, database, issueKey, nil)

	atBound := strings.Repeat("r", graphstore.MaxAddressBytes)
	pastBound := strings.Repeat("r", graphstore.MaxAddressBytes+1)
	// The role the bug met: at 1,400 bytes its page is 1,405, and a part_of tuple keying both ends
	// at once is about 2,805 bytes, past the roughly 2,704 a btree entry holds, so the old code
	// failed recordBatch's one statement with "index row size ... exceeds ... 2704". It is
	// incompressible, since a btree compresses a run of one byte but not a real role name.
	blowsBound := randRole(1400)
	atTarget := "role:" + atBound
	overTarget := "role:" + pastBound
	blowsTarget := "role:" + blowsBound

	atMsg := appendEvent(t, database, broker, model.Event{
		IssueKey: &issueKey, Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{
			ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f30", IssueKey: &issueKey, Body: "At the bound.", Target: &atTarget,
		}},
	})
	overMsg := appendEvent(t, database, broker, model.Event{
		IssueKey: &issueKey, Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{
			ID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", IssueKey: &issueKey, Body: "One byte over.", Target: &overTarget,
		}},
	})
	blowsMsg := appendEvent(t, database, broker, model.Event{
		IssueKey: &issueKey, Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{
			ID: "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f", IssueKey: &issueKey, Body: "The role the bug met.", Target: &blowsTarget,
		}},
	})

	holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker}).scan(ctx, unfenced{})
	requirePublished(t, database, atMsg, true)
	requirePublished(t, database, overMsg, true)
	requirePublished(t, database, blowsMsg, true)

	// The record writes claims and tuples as well as nodes, and a tuple whose end is a longer role
	// or a page is no node the reads below see, since they ask after six nodes by name. So both
	// tables are read whole for any address past the bound. A value is no address, so a value's
	// to_id is not counted.
	var longNodes, longTuples int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where octet_length(id) > $1`,
		graphstore.MaxAddressBytes).Scan(&longNodes); err != nil {
		t.Fatalf("count the nodes past the bound: %v", err)
	}
	if longNodes != 0 {
		t.Errorf("the store holds %d node(s) whose id is past the %d-byte bound, want none", longNodes, graphstore.MaxAddressBytes)
	}
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from graph_tuples
		where octet_length(from_id) > $1 or (to_kind <> 'value' and octet_length(to_id) > $1)
	`, graphstore.MaxAddressBytes).Scan(&longTuples); err != nil {
		t.Fatalf("count the tuples with an end past the bound: %v", err)
	}
	if longTuples != 0 {
		t.Errorf("the store holds %d tuple(s) with an end past the %d-byte bound, want none", longTuples, graphstore.MaxAddressBytes)
	}

	// The role of exactly the bound is a node; its page is five bytes longer and left out.
	requireLiveNode(t, database, graphstore.KindRole, atBound)
	requireNoNode(t, database, graphstore.KindPage, graphstore.RolePageID(atBound))
	// The role a byte over the bound is no node, and no page either.
	requireNoNode(t, database, graphstore.KindRole, pastBound)
	requireNoNode(t, database, graphstore.KindPage, graphstore.RolePageID(pastBound))
	// The role the bug met is left out whole, and the batch recorded all the same.
	requireNoNode(t, database, graphstore.KindRole, blowsBound)
	requireNoNode(t, database, graphstore.KindPage, graphstore.RolePageID(blowsBound))

	// A record after the over-bound one drains too: the stream is not stuck on it.
	nextMsg := appendEvent(t, database, broker, model.Event{
		IssueKey: &issueKey, Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{
			ID: "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e", IssueKey: &issueKey, Body: "The next record.",
		}},
	})
	holding(t, Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker}).scan(ctx, unfenced{})
	requirePublished(t, database, nextMsg, true)
}

// randRole is an incompressible role name of n bytes, drawn from the grammar roleRoutePattern
// admits ([a-z0-9-]): a btree compresses a run of one byte, so a control needs a key the index
// cannot shrink under its bound.
func randRole(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
