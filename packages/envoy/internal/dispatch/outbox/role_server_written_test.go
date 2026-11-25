package outbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// c-one-leader-read (dispatch://INFRA-5459) in Dispatch's outbox. The NATS server stores a message
// under a role's key on its own - here a RePublish of another stream - whose body is a claim naming
// a session that is live. No writer wrote it, and a direct get cannot tell it from a claim: it
// reads the message as that session's claim (the control). The outbox reads a role's claim from
// the role bucket stream's leader, decoded as the listener's every delivery read decodes it
// (roles.Buckets.RoleClaim), which answers such a message store.ErrMalformedRoleClaim: the role
// route stays pending with its attempt counted and the reason named, nothing is sent to that
// session, and nothing is kept, since a claim no read can decode is no role nobody holds.
func TestARoleRouteOverAServerWrittenClaimOfALiveSessionSendsThatSessionNothing(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_server_written_claim"
	if err := rig.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", holder, err)
	}
	body, err := json.Marshal(envoystore.RoleClaim{HolderSessionID: holder, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode the claim the RePublish stores: %v", err)
	}
	testnats.RepublishesKey(t, rig.conn, bus.RoleClaimBucket, role, body)
	if direct := rig.roleClaim(t, role); direct.HolderSessionID != holder {
		t.Fatalf("a direct get of %s's claim reads %+v; want %s's claim, or the control shows nothing a reader could take for one", role, direct, holder)
	}
	rig.silent(t, contracts.LegacyAgentSubject(holder))
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if copies := storedCopies(t, rig.client, contracts.LegacyAgentSubject(holder)); copies != 0 {
		t.Fatalf("the stream holds %d messages on %s's subject, want none: the claim the server wrote names it", copies, holder)
	}
	if published := publishedAt(t, rig.database, event.ID); published != nil {
		t.Fatalf("the event was published at %s, want its role route pending", published)
	}
	attempts, failure, destinations := eventState(t, rig.database, event.ID)
	if attempts != 1 || failure == nil || !strings.Contains(*failure, "a message the server wrote") ||
		strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
		t.Fatalf("attempt_count %d, publish_error %v, published_destinations %v: want it backed off naming the message the server wrote, with the role route pending",
			attempts, failure, destinations)
	}
	if kept := keptCopies(t, rig.client, role); len(kept) != 0 {
		t.Fatalf("role %s's kept lane holds %d copies, want none", role, len(kept))
	}
}
