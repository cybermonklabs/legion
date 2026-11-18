package store

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// requestWaitHold is how long each case below holds what the server sends once its trigger has
// passed: 13 s, longer than every wait WithRequestWait replaces (a JetStream request's 10 s, the
// role scan's 10 s for its next entry and its consumer create's 5 s), so a registry still on any
// of them gives up inside the hold. It is a stand-in's own bound, a link that goes quiet and comes
// back, on a real timer (stallingProxy): the waits it outlasts are nats.go's, on the wall clock,
// which no clock of the test reaches.
const requestWaitHold = 13 * time.Second

// waitLeft is the wait a test hands WithRequestWait: all that the shared wait has left
// (testwait.Context), and under -timeout 0, where the shared wait has no end, the longest a
// duration holds.
func waitLeft(t *testing.T) time.Duration {
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		return time.Until(deadline)
	}
	return math.MaxInt64
}

// A registry opened with WithRequestWait waits for the bus as long as its caller says, and for
// nothing less: each case holds the server's answers past one of the store's own waits, and the
// call still succeeds once the link comes back. Without the option the last two cases fail inside
// the hold (TestOpenFailsWhenTheRoleRevisionScanStalls is the role scan's), so a load that slows
// the bus past those waits ends no call whose caller waits longer. That is the restart test's
// writer in cmd/listener, which waits on its write for as long as the test binary's deadline
// leaves. The first case succeeds without the option too: the open sends a consumer create the
// server left unanswered again until the server answers it (roleRevisions).
func TestARegistryOpenedWithARequestWaitOutlastsAQuietLink(t *testing.T) {
	const claims = 400
	for _, held := range []struct {
		name    string
		trigger string
		budget  int
	}{
		// The role scan's consumer create, whose answer is the first thing held.
		{name: "the open's role scan's consumer create", trigger: "$JS.API.CONSUMER.CREATE.KV_" + bus.RoleClaimBucket, budget: 0},
		// The role scan itself, once 16 KiB of its entries have come through.
		{name: "the open's role scan partway through the bucket", trigger: "$JS.API.CONSUMER.CREATE.KV_" + bus.RoleClaimBucket, budget: 16 * 1024},
		// A write's read of the row it merges into, a JetStream request of the opened registry.
		{name: "a write's read of its interest record", trigger: "$JS.API.DIRECT.GET.KV_" + bus.InterestBucket, budget: 0},
	} {
		t.Run(held.name, func(t *testing.T) {
			direct, cleanup := connectNATS(t)
			defer cleanup()
			js, err := direct.JetStream(testnats.MaxWait(t))
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
			if err != nil {
				t.Fatalf("create the role bucket: %v", err)
			}
			for i := range claims {
				putRoleClaim(t, rawRoles, fmt.Sprintf("role-%03d", i), fmt.Sprintf("ses_%03d", i))
			}

			account, err := url.Parse(testnats.URL(t))
			if err != nil {
				t.Fatalf("read the test's NATS URL: %v", err)
			}
			quietURL, holdEnded := stallingProxy(t, account.Host, held.trigger, held.budget, requestWaitHold)
			proxy, err := url.Parse(quietURL)
			if err != nil {
				t.Fatalf("read the proxy's URL: %v", err)
			}
			proxy.User = account.User
			quiet := testnats.Connect(t, proxy.String())
			defer quiet.Close()

			registry, err := Open(quiet, WithReplicas(1), WithRequestWait(waitLeft(t)))
			if err != nil {
				t.Fatalf("Open with a request wait of its caller's, over a link held %s: %v", requestWaitHold, err)
			}
			t.Cleanup(registry.StopWatch)
			if got := len(registry.restoredRoleRevisions); got != claims {
				t.Fatalf("Open restored %d of the bucket's %d role claims", got, claims)
			}
			const sessionID = "ses_quiet_link_writer"
			topic := "notifications.agent." + sessionID
			written, err := registry.Upsert(Interest{SessionID: sessionID, MachineID: "other-listener"}, []string{topic})
			if err != nil {
				t.Fatalf("Upsert through a registry with a request wait of its caller's, over a link held %s: %v", requestWaitHold, err)
			}
			if !slices.Contains(written.Topics, topic) {
				t.Fatalf("Upsert wrote %+v, want its topics to hold %s", written, topic)
			}
			// Each call above read what the server sent after its trigger, so a hold that happened
			// has ended by now; one that never began means the trigger never passed, and the case
			// proved nothing.
			select {
			case <-holdEnded:
			default:
				t.Fatalf("the link was never held: no request sent %s", held.trigger)
			}
		})
	}
}

// Open refuses a request wait that is not positive rather than open a registry whose every
// request would end before it is answered.
func TestOpenRefusesARequestWaitThatIsNotPositive(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	for _, wait := range []time.Duration{0, -time.Second} {
		if registry, err := Open(conn, WithReplicas(1), WithRequestWait(wait)); err == nil {
			registry.StopWatch()
			t.Fatalf("Open with WithRequestWait(%s) opened a registry, want a refusal", wait)
		}
	}
}
