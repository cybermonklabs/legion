package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// stateTestPod is one listener pod for a test of the state its pods share: its own NATS connection,
// its own handle on the listener state bucket and its delivery lease over it, and its interest and
// session registries wired to them as main wires them. startedAgo is how long before now the pod
// started, as its interest registry's clock reads while it opens.
type stateTestPod struct {
	deps  *listenerDeps
	lease *lease
}

func openStateTestPod(t *testing.T, instance string, startedAgo time.Duration, sessionOptions ...session.SessionRegistryOption) stateTestPod {
	t.Helper()
	client := setupPublishTestClient(t)
	stateKV := leaseBucket(t)
	podLease := testLease(stateKV, instance, productionLeaseConfig(), time.Now)
	var elapsed atomic.Int64
	elapsed.Store(int64(-startedAgo))
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1),
		store.WithClock(func() time.Time { return time.Now().Add(time.Duration(elapsed.Load())) }),
		store.WithServiceSince(func() time.Time { return podLease.Status().ServiceSince }))
	if err != nil {
		t.Fatalf("open pod %s's interest registry: %v", instance, err)
	}
	t.Cleanup(registry.StopWatch)
	elapsed.Store(0)
	sessionOptions = append([]session.SessionRegistryOption{session.WithSessionReplicas(1), session.WithLastSeenBucket(stateKV)}, sessionOptions...)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, sessionOptions...)
	if err != nil {
		t.Fatalf("open pod %s's session registry: %v", instance, err)
	}
	t.Cleanup(sessions.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm pod %s's session cache: %v", instance, err)
	}
	return stateTestPod{deps: &listenerDeps{client: client, registry: registry, sessions: sessions}, lease: podLease}
}

// lapsedRoleAnswer is GET /v1/roles/<role>'s answer for a holder that is gone.
type lapsedRoleAnswer struct {
	Reason        roles.State `json:"reason"`
	Holder        string      `json:"holder"`
	LastSeen      *int64      `json:"last_seen"`
	ClaimReleased *bool       `json:"claim_released"`
}

// getLapsedRole asks pod for role, whose holder is gone, and returns the raw answer and its fields.
func (pod stateTestPod) getLapsedRole(t *testing.T, role string) (string, lapsedRoleAnswer) {
	t.Helper()
	recorder := httptest.NewRecorder()
	roleGetHandler(pod.deps).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/roles/%s: status %d %s, want 404 for a lapsed holder", role, recorder.Code, recorder.Body.String())
	}
	body := strings.TrimSpace(recorder.Body.String())
	var answer lapsedRoleAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode GET /v1/roles/%s: %v (%s)", role, err, body)
	}
	if answer.Reason != roles.Lapsed || answer.ClaimReleased == nil {
		t.Fatalf("GET /v1/roles/%s = %s, want a lapsed holder whose claim this lookup released or kept", role, body)
	}
	return body, answer
}

func (pod stateTestPod) claim(t *testing.T, role, holder string) {
	t.Helper()
	if _, err := pod.deps.registry.SetRole(holder, "box-a", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
}

// A role whose holder is gone is answered alike by every pod of the machine, whenever each pod
// started, and no lookup releases its claim whatever its holder's state. The release is the role
// claim reaper's alone, and the grace it holds a claim for counts from the service start in the
// delivery lease record, which each pod's lease reads: asked to release the same claim, every pod
// answers the same. Before, each pod counted that grace from its own start, so a pod that had run
// for long released a claim from before the machine's pods began serving again, and one that
// started a moment ago kept a claim made long after the machine's service began.
func TestEveryPodAnswersARoleWhoseHolderLapsedAlike(t *testing.T) {
	t.Run("the machine has served since long before the claim", func(t *testing.T) {
		kv := leaseBucket(t)
		serviceSince := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		putLease(t, kv, `{"holder":"pod-z","boot":"0123456789abcdef","service_since":"`+serviceSince+`"}`)
		a := openStateTestPod(t, "pod-a", 10*time.Minute)
		a.claim(t, "role-a", "ses_gone_a")
		a.claim(t, "role-b", "ses_gone_b")
		b := openStateTestPod(t, "pod-b", 0)
		a.lease.step()
		b.lease.step()

		_, onA := a.getLapsedRole(t, "role-a")
		_, onB := b.getLapsedRole(t, "role-b")
		if *onA.ClaimReleased || *onB.ClaimReleased {
			t.Fatalf("claim released on a lookup: pod a = %v, pod b = %v; want neither, whatever the holder's state",
				*onA.ClaimReleased, *onB.ClaimReleased)
		}
		requireRelease(t, a, "role-a", "ses_gone_a", store.ExpiredRoleClaimReleased)
		requireRelease(t, b, "role-b", "ses_gone_b", store.ExpiredRoleClaimReleased)
	})

	t.Run("the machine began serving again after the claim", func(t *testing.T) {
		a := openStateTestPod(t, "pod-a", 10*time.Minute)
		a.claim(t, "role", "ses_gone")
		// Pod a takes the absent lease record, which starts the machine's service at the server's
		// time of that write, after the claim; pod b, starting now, reads that record.
		a.lease.step()
		b := openStateTestPod(t, "pod-b", 0)
		b.lease.step()
		if since := b.lease.Status().ServiceSince; since.IsZero() || !since.Equal(a.lease.Status().ServiceSince) {
			t.Fatalf("pod b read service_since %v, pod a wrote %v", since, a.lease.Status().ServiceSince)
		}

		bodyA, onA := a.getLapsedRole(t, "role")
		bodyB, _ := b.getLapsedRole(t, "role")
		if *onA.ClaimReleased {
			t.Fatalf("pod a released a claim on a lookup: %s; want no lookup to release one", bodyA)
		}
		if bodyA != bodyB {
			t.Fatalf("pod a answers %s, pod b answers %s", bodyA, bodyB)
		}
		// The claim the machine's service start is younger than keeps its grace on every pod.
		requireRelease(t, a, "role", "ses_gone", store.ExpiredRoleClaimRetained)
		requireRelease(t, b, "role", "ses_gone", store.ExpiredRoleClaimRetained)
	})
}

// requireRelease fails the test unless pod's registry answers want when asked to release role's
// claim, held by holder, with the live window the listener gives its role claim reaper.
func requireRelease(t *testing.T, pod stateTestPod, role, holder string, want store.ExpiredRoleClaimRelease) {
	t.Helper()
	live, _ := pod.deps.sessions.Windows()
	release, err := pod.deps.registry.ReleaseExpiredRoleClaim(context.Background(), role, holder, live)
	if err != nil || release != want {
		t.Fatalf("release %s's claim held by %s = %v, %v; want %v", role, holder, release, err, want)
	}
}

// heldRoleDeleteKV is the role bucket with its first Delete of role held before anything is sent:
// held closes as that Delete is called, and the Delete then waits for letGo and deletes as asked.
// The role claim reaper's release of an expired claim is that Delete, called on the goroutine
// bus.BoundedOn starts for it, so a row lands what it needs between the pass's decision and the
// write that carries it out.
type heldRoleDeleteKV struct {
	natsgo.KeyValue
	role  string
	held  chan struct{}
	letGo chan struct{}
	holds sync.Once
}

func (kv *heldRoleDeleteKV) Delete(key string, opts ...natsgo.DeleteOpt) error {
	if key == kv.role {
		kv.holds.Do(func() {
			close(kv.held)
			<-kv.letGo
		})
	}
	return kv.KeyValue.Delete(key, opts...)
}

// A release the role claim reaper's pass decided before its pod's lease stepped down, and sends
// once the term has ended and the session that holds the role next has claimed it, leaves that
// session's claim standing. The reaper runs on the lease holder alone (holderLoops, main.go),
// under the context the receive loop makes for the term's loops from context.Background() and
// cancels only once it hears the term has ended (durableReceiver.serve), and the pass sends each
// release on that context (store.Registry.ReapRoleClaims): nothing holds the release to the term
// or to the lease's loss deadline, as a session consumer's delete is held
// (sessionRelays.termContext). So the release reaches the bucket. Here the pass has judged the
// claim's holder gone and called the delete, which is held before it is sent while the lease
// steps down, its loss deadline passes and the next holder claims the role, the loop's context
// still live. What refuses the delete is the revision the pass's scan read, which the next
// holder's claim moved (store.Registry.releaseReadExpiredRoleClaim). The lease is stepped by
// hand, as the relay's and the consumer reaper's rows step theirs (relay_ownership_test.go).
func TestARoleClaimReleaseReachedAfterTheLeaseSteppedDownKeepsTheNextHoldersClaim(t *testing.T) {
	const (
		role       = "release-after-step-down"
		goneHolder = "ses_release_after_step_down_gone"
		nextHolder = "ses_release_after_step_down_next"
	)
	a := openStateTestPod(t, "pod-a", 0)
	a.lease.step()
	status := a.lease.Status()
	if !status.Holding {
		t.Fatalf("pod-a did not take the lease: %+v", status)
	}
	// The claim is written after pod-a's take of the absent lease record started the machine's
	// service, so no restart grace holds it, and is aged past the live window, since its holder
	// stopped renewing it; that holder never registered, so the pass finds its session gone.
	live := roleClaimLiveWindow(a.deps)
	a.claim(t, role, goneHolder)
	ageRoleClaim(t, a.deps, role, 2*live)
	judged := storedRoleClaim(t, a.deps.registry, role)

	release := &heldRoleDeleteKV{KeyValue: registryRoleKV(t, a.deps.registry), role: role, held: make(chan struct{}), letGo: make(chan struct{})}
	setRegistryRoleKV(t, a.deps.registry, release)
	letGo := sync.OnceFunc(func() { close(release.letGo) })
	defer letGo()

	// The pass runs on a goroutine of its own, as the loop's does
	// (store.Registry.StartRoleClaimReaper), with the liveness test and the windows holderLoops
	// gives it, under a context made as the receive loop makes the loops' one, which nothing here
	// cancels: the receive loop has not heard the term end.
	loopCtx, stopLoop := context.WithCancel(context.Background())
	defer stopLoop()
	type passAnswer struct {
		sweep store.RoleClaimSweep
		err   error
	}
	answered := make(chan passAnswer, 1)
	go func() {
		sweep, err := a.deps.registry.ReapRoleClaims(loopCtx, func(sessionID string) bool {
			return roleHolderMayBeLive(logging.New("test"), a.deps.sessions, sessionID)
		}, live, live)
		answered <- passAnswer{sweep: sweep, err: err}
	}()
	select {
	case <-release.held:
	case got := <-answered:
		t.Fatalf("the pass answered %+v, %v without releasing %s's claim, want it to judge %s gone and send the release", got.sweep, got.err, role, goneHolder)
	case <-time.After(30 * time.Second):
		t.Fatalf("the pass did not reach the release of %s's claim within 30 s", role)
	}

	if !stepLeaseDown(t, a.lease.kv, a.lease) {
		t.Fatal("pod-a's lease did not step down while the pass's release was held")
	}
	if left := a.lease.UntilLoss(status.Term); left != 0 {
		t.Fatalf("pod-a's term has %s left before its loss deadline after the step-down, want none", left)
	}
	a.claim(t, role, nextHolder)
	claimed := storedRoleClaim(t, a.deps.registry, role)
	if claimed.HolderSessionID != nextHolder || claimed.Revision <= judged.Revision {
		t.Fatalf("%s's claim after the next holder claimed it is %q's at revision %d, want %s's past the revision %d the pass judged",
			role, claimed.HolderSessionID, claimed.Revision, nextHolder, judged.Revision)
	}
	letGo()
	var got passAnswer
	select {
	case got = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatalf("the pass did not answer within 30 s of its release of %s's claim being let go", role)
	}
	if stood := storedRoleClaim(t, a.deps.registry, role); stood.HolderSessionID != nextHolder || stood.Revision != claimed.Revision {
		t.Fatalf("%s's claim after the pass is %q's at revision %d, want %s's at revision %d: a release pod-a's pass sent after its lease stepped down took the next holder's claim",
			role, stood.HolderSessionID, stood.Revision, nextHolder, claimed.Revision)
	}
	if got.err != nil || got.sweep != (store.RoleClaimSweep{}) {
		t.Fatalf("the pass answered %+v, %v; want nothing reaped and nothing skipped: its release lost to the next holder's claim, which is the release's answer and not a failure", got.sweep, got.err)
	}
}

// A role holder whose session heartbeated only through pod A and then expired is reported with one
// last_seen by every pod, pod B started after it lapsed included. Before, the last-seen time lived
// in the memory of a pod whose cache had held the session, so pod B reported none.
func TestARoleHoldersLastSeenIsTheSameOnAPodStartedAfterItLapsed(t *testing.T) {
	const (
		holder = "ses_lapsing_holder"
		ttl    = 3 * time.Second
	)
	a := openStateTestPod(t, "pod-a", 0, session.WithSessionTTL(ttl))
	if err := a.deps.sessions.Put(holder, session.SessionEntry{MachineID: "box-a", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s through pod a: %v", holder, err)
	}
	a.claim(t, "role-a", holder)
	a.claim(t, "role-b", holder)
	sessionKV, err := a.deps.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	deadline := time.Now().Add(ttl + 10*time.Second)
	for {
		if _, err := sessionKV.Get(holder); errors.Is(err, natsgo.ErrKeyNotFound) {
			break
		} else if err != nil {
			t.Fatalf("read %s while it expires: %v", holder, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's registration was not removed within %s of its write", holder, ttl+10*time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b := openStateTestPod(t, "pod-b", 0)

	bodyA, onA := a.getLapsedRole(t, "role-a")
	bodyB, onB := b.getLapsedRole(t, "role-b")
	if onA.LastSeen == nil || onB.LastSeen == nil || *onA.LastSeen != *onB.LastSeen {
		t.Fatalf("pod a answers %s, pod b answers %s; want one last_seen on both", bodyA, bodyB)
	}
}
