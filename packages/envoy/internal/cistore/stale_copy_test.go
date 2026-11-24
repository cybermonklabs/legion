package cistore

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// staleOnceKV is the CI bucket as a direct get of a replicated bucket can answer it: once armed,
// the next Get of key answers stale, the record as it stood before the writes since, as a copy of
// the bucket that has not applied them yet answers; every other call, and every read after that
// one, is the bucket's own. A read from the stream's leader (bus.KeyValue.GetFromLeader) does not
// go through this handle, as it does not go through a copy.
type staleOnceKV struct {
	natsgo.KeyValue
	key string

	mu     sync.Mutex
	stale  natsgo.KeyValueEntry
	armed  bool
	served int
}

// arm makes the next Get of kv.key answer stale.
func (kv *staleOnceKV) arm(stale natsgo.KeyValueEntry) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.stale, kv.armed = stale, true
}

// staleReads is how many reads the stale copy answered.
func (kv *staleOnceKV) staleReads() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.served
}

func (kv *staleOnceKV) Get(key string) (natsgo.KeyValueEntry, error) {
	kv.mu.Lock()
	if key == kv.key && kv.armed {
		kv.armed = false
		kv.served++
		stale := kv.stale
		kv.mu.Unlock()
		return stale, nil
	}
	kv.mu.Unlock()
	return kv.KeyValue.Get(key)
}

// A settlement is published once, and marked settled, even when the read that marks it settled
// (MarkSettled) is answered by a copy of the bucket that has not applied the settlement's claim
// yet. On a replicated bucket every Get is a direct get, which any copy near enough to current
// answers. A mark that declines on such a copy's answer leaves the claim standing; a later pass
// reclaims it, which bumps the generation, and publishes the same snapshot again at the next
// generation. In production that was 37 heads published twice on 2026-11-22. Passes run a minute
// apart, past the claim's 2*debounce, so a claim left standing is reclaimed. Control: with the
// mark's read answered by a current copy, the head is published once; and the stale copy must
// answer exactly the mark's read, or the row tested nothing.
func TestASettlementWhoseMarkReadsACopyBehindIsPublishedOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{"the mark's read answered by a copy behind the claim", true},
		{"control: the mark's read answered by a current copy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			store := openStore(t, conn)
			const (
				owner  = "example-org"
				repo   = "example-repo"
				number = "42"
				sha    = "abcdef1234567890abcdef1234567890abcdef12"
			)
			if err := recordCheck(store, owner, repo, number, sha, "build", "906", "https://example.test/906", "completed", "success", "2026-11-22T19:50:00Z"); err != nil {
				t.Fatalf("record a finished check: %v", err)
			}
			waitCacheChecks(t, store, owner, repo, number, sha, 1)
			setLastEventAt(t, store, owner, repo, number, sha, 0)
			key := Key(owner, repo, number, sha)
			original := store.watcher.KV()
			unclaimed, err := original.Legacy().Get(key)
			if err != nil {
				t.Fatalf("read the unclaimed record: %v", err)
			}
			kv := &staleOnceKV{KeyValue: original.Legacy(), key: key}
			useKV(t, store, kv)
			t.Cleanup(func() { useKV(t, store, original.Legacy()) })
			var armed sync.Once
			pub := &recPub{onPublish: func(contracts.Envelope) {
				if tc.stale {
					armed.Do(func() { kv.arm(unclaimed) })
				}
			}}
			z := testSummarizer(store, time.Second)
			logger := logging.New("test")
			now := time.Now().UnixMilli()
			for i := range int64(4) {
				z.pass(pub, logger, now+i*time.Minute.Milliseconds())
			}
			if got := pub.count(); got != 1 {
				t.Fatalf("the head was published %d times, want once", got)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.stale]; kv.staleReads() != want {
				t.Fatalf("the stale copy answered %d reads, want %d", kv.staleReads(), want)
			}
			st := getState(t, store, owner, repo, number, sha)
			if !st.SettledEmitted || st.EmittedCount != 1 || st.Claim != nil {
				t.Fatalf("the record after its settlement is %+v, want it settled once with no claim", st)
			}
		})
	}
}

// A mark that does not apply is logged. When the claim a settlement was published under is gone
// from the stream leader too - another listener reclaimed it between this listener's check
// (ClaimStillHeld) and its mark - MarkSettled declines on the leader's read, and the pass logs
// `checks settlement not marked` at WARN, naming the head and the generation it published: another
// pass may publish that head again. Control: with the claim standing when the mark reads it, the
// mark applies and logs no such line.
func TestAMarkThatFindsNoClaimOnTheLeaderIsLogged(t *testing.T) {
	const notMarked = "checks settlement not marked"
	for _, tc := range []struct {
		name      string
		reclaimed bool
	}{
		{"another listener reclaimed the claim before the mark", true},
		{"control: the claim stands when the mark reads it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			store := openStore(t, conn)
			h := head{owner: "example-org", repo: "example-repo", number: "42", sha: "3333333333333333333333333333333333333333"}
			if err := recordCheck(store, h.owner, h.repo, h.number, h.sha, "build", "909", "https://example.test/909", "completed", "success", "2026-11-22T19:52:00Z"); err != nil {
				t.Fatalf("record a finished check: %v", err)
			}
			waitCacheChecks(t, store, h.owner, h.repo, h.number, h.sha, 1)
			setLastEventAt(t, store, h.owner, h.repo, h.number, h.sha, 0)
			generation := getState(t, store, h.owner, h.repo, h.number, h.sha).Generation
			pub := &recPub{onPublish: func(contracts.Envelope) {
				if !tc.reclaimed {
					return
				}
				// A stale-before past every claim: the reclaim another listener's pass makes of a
				// claim it judged stale, which clears the claim and bumps the generation.
				if reclaimed, err := store.ReclaimSettlement(h.key(), generation, math.MaxInt64); err != nil || !reclaimed {
					t.Errorf("reclaim the published claim = %t, %v; want it reclaimed", reclaimed, err)
				}
			}}
			var logs bytes.Buffer
			runSummaryTick(store, pub, time.Second, logging.NewWithWriter("test", &logs))
			if got := pub.count(); got != 1 {
				t.Fatalf("the head was published %d times, want once", got)
			}
			if tc.reclaimed {
				assertWarning(t, &logs, notMarked, h, map[string]any{"generation": float64(generation)})
				return
			}
			if lines := warnings(&logs, notMarked); len(lines) != 0 {
				t.Fatalf("%d lines %q where the mark applied, want none:\n%s", len(lines), notMarked, logs.String())
			}
			if st := getState(t, store, h.owner, h.repo, h.number, h.sha); !st.SettledEmitted {
				t.Fatalf("the record after its settlement is %+v, want it settled", st)
			}
		})
	}
}

// A settlement whose snapshot a re-arm replaced after its claim is not published, even where the
// check made just before the publication (ClaimStillHeld) would be answered by a copy of the bucket
// that has not applied the re-arm: the check reads the stream's leader. A re-arm of a claimed
// record bumps its generation (rearm), so the claim no longer holds; a copy behind answers the
// record as it was claimed, and a check that read that copy published the replaced snapshot, then
// the re-armed one. The re-arm is another listener's write, landing between the claim and the
// check. Control: with the check's read answered by a current copy, the same re-arm leaves the
// head published once, not at the replaced snapshot's generation.
func TestASettlementAReArmReplacedAfterItsClaimIsNotPublishedWhileTheCopyIsBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
	}{
		{"the check's read answered by a copy behind the re-arm", true},
		{"control: the check's read answered by a current copy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, cleanup := connectNATS(t)
			defer cleanup()
			store := openStore(t, conn)
			rearming := openStore(t, conn)
			h := head{owner: "example-org", repo: "example-repo", number: "42", sha: "4444444444444444444444444444444444444444"}
			if err := recordCheck(store, h.owner, h.repo, h.number, h.sha, "build", "910", "https://example.test/910", "completed", "success", "2026-11-22T19:53:00Z"); err != nil {
				t.Fatalf("record a finished check: %v", err)
			}
			waitCacheChecks(t, store, h.owner, h.repo, h.number, h.sha, 1)
			setLastEventAt(t, store, h.owner, h.repo, h.number, h.sha, 0)
			claimedGeneration := getState(t, store, h.owner, h.repo, h.number, h.sha).Generation
			key := h.key()
			original := store.watcher.KV()
			kv := &staleOnceKV{key: key}
			rearmed := false
			kv.KeyValue = &interleavingKV{KeyValue: original.Legacy(), afterUpdate: func(got string, _ []byte, _ uint64) {
				// The pass's first write of the record is its claim; ClaimStillHeld comes next.
				if got != key || rearmed {
					return
				}
				rearmed = true
				claimed, err := original.Legacy().Get(key)
				if err != nil {
					t.Errorf("read the claimed record: %v", err)
					return
				}
				if err := recordCheck(rearming, h.owner, h.repo, h.number, h.sha, "lint", "911", "https://example.test/911", "completed", "success", "2026-11-22T19:54:00Z"); err != nil {
					t.Errorf("re-arm the claimed record: %v", err)
					return
				}
				if tc.behind {
					kv.arm(claimed)
				}
			}}
			useKV(t, store, kv)
			t.Cleanup(func() { useKV(t, store, original.Legacy()) })
			pub := &recPub{}
			z := testSummarizer(store, time.Second)
			logger := logging.New("test")
			now := time.Now().UnixMilli()
			for i := range int64(4) {
				z.pass(pub, logger, now+i*time.Minute.Milliseconds())
			}
			if !rearmed {
				t.Fatal("no claim of the head landed, so no re-arm replaced its snapshot: the row tested nothing")
			}
			if want := map[bool]int{true: 1, false: 0}[tc.behind]; kv.staleReads() != want {
				t.Fatalf("the stale copy answered %d reads, want %d", kv.staleReads(), want)
			}
			published := pub.all()
			keys := make([]string, len(published))
			for i, env := range published {
				keys[i] = env.DedupeKey
			}
			if len(published) != 1 {
				t.Fatalf("the head was published %d times %v, want once, after its re-arm", len(published), keys)
			}
			if replaced := fmt.Sprintf(".g%d", claimedGeneration); strings.HasSuffix(keys[0], replaced) {
				t.Fatalf("the head was published as %s, the snapshot its re-arm replaced (generation %d)", keys[0], claimedGeneration)
			}
		})
	}
}

// A restart publishes no settlement for a head its listener already settled: the first pass of a
// new store looks at every record the bucket holds (attachSummary), and a settled record is idle.
// Control: a head whose checks finished and that no listener settled before the restart is
// published exactly once by that pass, so the pass did look at the bucket's records.
func TestARestartPublishesNoSettlementForAHeadAlreadySettled(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	const (
		owner       = "example-org"
		repo        = "example-repo"
		number      = "42"
		settledSHA  = "1111111111111111111111111111111111111111"
		finishedSHA = "2222222222222222222222222222222222222222"
	)
	logger := logging.New("test")
	before := openStore(t, conn)
	if err := recordCheck(before, owner, repo, number, settledSHA, "build", "907", "https://example.test/907", "completed", "success", "2026-11-22T19:50:00Z"); err != nil {
		t.Fatalf("record the settled head's check: %v", err)
	}
	waitCacheChecks(t, before, owner, repo, number, settledSHA, 1)
	setLastEventAt(t, before, owner, repo, number, settledSHA, 0)
	settledBy := &recPub{}
	runSummaryTick(before, settledBy, time.Second, logger)
	if got := settledBy.count(); got != 1 {
		t.Fatalf("the listener before the restart published %d settlements, want the settled head's one", got)
	}
	if st := getState(t, before, owner, repo, number, settledSHA); !st.SettledEmitted {
		t.Fatalf("the settled head's record is %+v, want it settled", st)
	}
	if err := recordCheck(before, owner, repo, number, finishedSHA, "build", "908", "https://example.test/908", "completed", "success", "2026-11-22T19:51:00Z"); err != nil {
		t.Fatalf("record the finished head's check: %v", err)
	}
	waitCacheChecks(t, before, owner, repo, number, finishedSHA, 1)
	setLastEventAt(t, before, owner, repo, number, finishedSHA, 0)
	forgetTestSummarizer(before)
	before.StopWatch()

	after := openStore(t, conn)
	pub := &recPub{}
	z := testSummarizer(after, time.Second)
	now := time.Now().UnixMilli()
	for i := range int64(3) {
		z.pass(pub, logger, now+i*time.Minute.Milliseconds())
	}
	published := pub.all()
	if len(published) != 1 {
		t.Fatalf("the restart published %d settlements, want only the finished head's one", len(published))
	}
	if want := "github.checks." + owner + "/" + repo + ".pr." + number + "." + finishedSHA + ".g"; len(published[0].DedupeKey) < len(want) || published[0].DedupeKey[:len(want)] != want {
		t.Fatalf("the restart published %s, want the finished head %s", published[0].DedupeKey, finishedSHA)
	}
}
