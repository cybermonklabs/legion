package sandbox

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A pod whose launcher is ended after the copy was sent and before it reported, with no role of
// the pod registered, leaves the work for the next start of the issue: its restarted roles
// container is sent the copy again, and so is the pod after it, each over a workspace no role has
// touched. Once a role of a pod the copy was sent to registers, that role may change the
// workspace, and neither that pod's restarted container nor the pod after it is sent the copy.
// The runtime's restore hop runs over the real store, and the registration is the store's own
// record of it, which the daemon makes before it answers a registration.
func TestAnUnspentCopyComesBackAfterAnUnregisteredPodEndsAndNotOnceARoleRegistered(t *testing.T) {
	st := bundleStore(t)
	g := newRig(t, nil, withSavedWorkspace(st))
	spec := workerSpec(t)
	// The issue holds nothing at its pod's first role start, and the copy of the pod before that
	// one is written after it, so the first pod the copy is sent to is the one after.
	g.spawn(spec)
	if frames := g.sent(spec.Claim); countOf(frames, shimwire.TypeLauncherBundle) != 0 {
		t.Fatalf("the first pod was sent %v, want no slice: the issue held nothing", frameTypes(frames))
	}
	taken := runningWorkspaceCopy("pod-one")
	saveRunningCopy(t, st, taken)
	requireTheWork := func(who string, read shimwire.WorkspaceBundle) {
		t.Helper()
		if want := sentBookmarks(taken); !bytes.Equal(read.Bundle, taken.Bundle) || read.Head != taken.Head ||
			!slices.Equal(read.Tips, taken.Tips) || !maps.Equal(read.Bookmarks, want) {
			t.Fatalf("%s was sent %d bytes at head %s with tips %v and bookmarks %v, want pod-one's %d at %s with %v and %v",
				who, len(read.Bundle), read.Head, read.Tips, read.Bookmarks, len(taken.Bundle), taken.Head, taken.Tips, want)
		}
	}

	// The restore hop alone: the copy reaches the launcher whole, and no start follows to apply
	// it and report, which is a launcher ended between the two. Each g.connect is a new launcher
	// process, as the restarted container's is, with new launcher ids.
	three := secondEnd(t, g, spec, time.Now().UTC())
	requireTheWork("the pod after the first", sentTo(t, g, spec, three))
	g.connect(spec.Claim)
	requireTheWork("that pod's restarted container", sentTo(t, g, spec, three))
	four := secondEnd(t, g, spec, time.Now().UTC())
	requireTheWork("the pod after the one that ended unregistered", sentTo(t, g, spec, four))

	// A role of that pod registers before its launcher ends.
	if err := st.RecordPodRegistration(g.ctx, testTree, four); err != nil {
		t.Fatal(err)
	}
	sentNothing := func(who, pod string) {
		t.Helper()
		before := len(g.sent(spec.Claim))
		g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, pod)
		for _, frame := range g.sent(spec.Claim)[before:] {
			if slice, ok := frame.(shimwire.LauncherBundle); ok {
				t.Fatalf("%s was sent slice %d of %d of copy %d, want nothing: a role of the pod the copy was sent to registered",
					who, slice.Index, slice.Count, slice.Release)
			}
		}
	}
	g.connect(spec.Claim)
	sentNothing("that pod's restarted container", four)
	five := secondEnd(t, g, spec, time.Now().UTC())
	sentNothing("the pod after it", five)
	if restore, err := st.TakeWorkspaceCopyForRestore(g.ctx, testProject, testTree, "pod-six"); err != nil || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the store answered a later pod %v (%v), want nothing", restore.Running.Take, err)
	}
}

// withCountedTakes is withSavedWorkspace with every take of the issue's copies counted into
// takes, which is how a test tells a role start that took the copies, under the issue's copy lock,
// from one that took nothing. Each take first pauses for pause, as a take behind the issue's copy
// lock can, which is what lets starts that run at once meet inside their hops.
func withCountedTakes(st *store.Store, takes *atomic.Int32, pause time.Duration) rigOption {
	return withOptions(func(o *Options) { countedTakes(o, st, takes, pause) })
}

// countedTakes sets o's two halves of a restore over st, as withSavedWorkspace does, with every
// take counted into takes and paused for pause.
func countedTakes(o *Options, st *store.Store, takes *atomic.Int32, pause time.Duration) {
	o.SavedWorkspaceBundle = func(ctx context.Context, issue, podUID string) (runtime.WorkspaceRestore, error) {
		takes.Add(1)
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return runtime.WorkspaceRestore{}, ctx.Err()
		}
		return st.TakeWorkspaceCopyForRestore(ctx, testProject, issue, podUID)
	}
	o.WorkspaceBundleRestored = st.KeepRestoredWorkspaceCopy
}

// unspentCopyPod is a rig whose issue's second pod is to be sent the issue's running copy at its
// first ask: the copy, off a pod the rig never makes, was written after the first pod's only take
// and before the second pod's first ask. It answers the rig, the count of takes from that second
// pod on, and that pod's uid. The pod holds the tester's launcher alone, which a test replaces
// with the launcher process it models.
func unspentCopyPod(t *testing.T) (*rig, *atomic.Int32, *store.Store, string) {
	t.Helper()
	st := bundleStore(t)
	takes := new(atomic.Int32)
	g := newRig(t, nil, withCountedTakes(st, takes, 0))
	spec := workerSpec(t)
	g.spawn(spec)
	saveRunningCopy(t, st, runningWorkspaceCopy("pod-one"))
	pod := secondEnd(t, g, spec, time.Now().UTC())
	takes.Store(0)
	return g, takes, st, pod
}

// issueRoleTokens is the claim token of every role of workerToken's issue, all of which run in
// the issue's one pod.
func issueRoleTokens(t *testing.T) []claim.Token {
	t.Helper()
	issue, _, ok := workerToken.Cut()
	if !ok {
		t.Fatalf("%s names no role", workerToken)
	}
	tokens := make([]claim.Token, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		tokens = append(tokens, claim.Token(issue+"-"+string(role)))
	}
	return tokens
}

// copiesSent is how many sends of a copy reached tokens' launchers: the first slices among what
// each was sent, every send being one sequence of slices.
func copiesSent(g *rig, tokens ...claim.Token) int {
	n := 0
	for _, token := range tokens {
		for _, frame := range g.sent(token) {
			if slice, ok := frame.(shimwire.LauncherBundle); ok && slice.Index == 0 {
				n++
			}
		}
	}
	return n
}

// startRole is token's role start in pod, as the runtime makes it: the restore hop and then the
// start frame on the pod's current launcher session, which the launcher answers once it has
// applied what it holds whole.
func startRole(t *testing.T, g *rig, token claim.Token, pod string) {
	t.Helper()
	session, err := g.r.launchers.await(g.ctx, token, pod)
	if err != nil {
		t.Fatalf("the launcher of %s in %s: %v", token, pod, err)
	}
	if err := g.r.launchers.start(g.ctx, token, pod, session, shimwire.LauncherStart{ID: "start-" + string(token), Generation: 1}); err != nil {
		t.Fatalf("start %s in %s: %v", token, pod, err)
	}
}

// A pod runs up to six roles in one launcher process, which holds the copy its pod is sent for
// every role it serves and applies it at its first start of any role. So six role starts of one
// launcher process take the issue's copies and send the copy once: the first start's restore hop
// takes and sends it and the process answers Held, and the five after it, each on a session of
// that process, take nothing and send nothing, even where all six hops come before the first
// start applies the copy, as they do while a long restore runs. The process applies the copy
// once. A session of that process that connects only after the Held answer is not one the daemon
// saw hold it, so its start takes the copies again and is sent the copy again, and the process
// answers that send from its own record, Held again with no second restore.
//
// The negative control is the skip removed from restoreWorkspace, where each of the six starts
// takes the copies again and streams every slice to the same process.
func TestAnUnspentCopyIsTakenAndSentOncePerLauncherProcess(t *testing.T) {
	t.Run("six role starts of one process", func(t *testing.T) {
		g, takes, _, pod := unspentCopyPod(t)
		roles := issueRoleTokens(t)
		process := g.newLauncherProcess()
		for _, token := range roles {
			g.connectToProcess(token, process)
		}

		for _, token := range roles {
			g.r.launchers.restoreWorkspace(g.ctx, token, pod)
		}
		if n, sent := takes.Load(), copiesSent(g, roles...); n != 1 || sent != 1 {
			t.Fatalf("the restore hops of six role starts of one launcher process took the copies %d times and sent the copy %d times, want once each", n, sent)
		}
		for _, token := range roles {
			startRole(t, g, token, pod)
		}
		if n, sent, applied := takes.Load(), copiesSent(g, roles...), process.restores(); n != 1 || sent != 1 || applied != 1 {
			t.Fatalf("six role starts of one launcher process took the copies %d times, sent the copy %d times and applied it %d times, want once each",
				n, sent, applied)
		}
	})

	t.Run("a session that connects after the Held answer", func(t *testing.T) {
		g, takes, _, pod := unspentCopyPod(t)
		process := g.newLauncherProcess()
		g.connectToProcess(workerToken, process)
		g.r.launchers.restoreWorkspace(g.ctx, workerToken, pod)
		if n, sent := takes.Load(), copiesSent(g, workerToken); n != 1 || sent != 1 {
			t.Fatalf("the tester's start took the copies %d times and was sent the copy %d times, want once each", n, sent)
		}

		g.connectToProcess(otherToken, process)
		g.r.launchers.restoreWorkspace(g.ctx, otherToken, pod)
		if n, sent := takes.Load(), copiesSent(g, otherToken); n != 2 || sent != 1 {
			t.Fatalf("after the tester's copy was held, the reviewer's later session took the copies %d times in all and was sent the copy %d times, want 2 and 1: the daemon never saw that session hold it",
				n, sent)
		}
		startRole(t, g, workerToken, pod)
		startRole(t, g, otherToken, pod)
		if applied := process.restores(); applied != 1 {
			t.Fatalf("the launcher process applied the copy %d times, want once: it answers a copy it holds from its own record", applied)
		}
		if n := takes.Load(); n != 2 {
			t.Fatalf("the two starts took the copies %d times in all, want still 2: both sessions were seen holding the copy", n)
		}
	})
}

// The six roles of an issue start in its one pod one at a time, each start's restore hop included:
// relaunch holds the pod's launch turn (lockPod) from before it reads the pod until its start is
// answered, and the six share one Sandbox name. So six Spawns released together into a new pod,
// whose one launcher process serves every role of it, take the issue's copies once, send the copy
// once and apply it once, although each take pauses long enough for starts outside the turn to
// meet inside their hops: each start after the first begins only once the one before it was
// answered, so it finds the process holding the copy the first start sent (heldCopy), and takes
// and sends nothing. The pause is two seconds, so that with no turn the other five starts reach
// their takes inside the first's however widely the pod's launches are spread: at 100 ms the
// control below failed one run in three at e1fd7dc6, and at two seconds it failed three in three.
//
// The negative control is the turn given back right before relaunch calls launchers.start, where
// the six starts meet inside their hops and each takes the copies and sends the copy.
func TestAnUnspentCopyIsTakenAndSentOnceWhenSixRolesOfOnePodSpawnTogether(t *testing.T) {
	st := bundleStore(t)
	saveRunningCopy(t, st, runningWorkspaceCopy("pod-one"))
	takes := new(atomic.Int32)
	g := newRig(t, nil, withCountedTakes(st, takes, 2*time.Second))
	roles := issueRoleTokens(t)
	type spawned struct {
		loc runtime.Locator
		err error
	}
	results := make(chan spawned, len(roles))
	together := make(chan struct{})
	for i, role := range claim.Roles {
		spec := testSpec(t, roles[i], role, testTree)
		go func() {
			<-together
			loc, err := g.r.Spawn(g.ctx, spec)
			results <- spawned{loc, err}
		}()
	}
	close(together)

	// The pod's one launcher process dials the daemon for every role of the pod once the pod is
	// there, as the rig's launcher of one role does for a pod the controller makes (createPod).
	name := SandboxName(workerToken)
	g.eventually("the issue's pod", func() bool { return g.pod(name) != nil })
	pod := string(g.pod(name).UID)
	process := g.newLauncherProcess()
	for _, token := range roles {
		g.connectToProcess(token, process)
	}
	for range roles {
		result := <-results
		if result.err != nil {
			t.Fatalf("a spawn of one of the six roles failed: %v", result.err)
		}
		if result.loc.Sandbox == nil || result.loc.Sandbox.PodUID != pod {
			t.Fatalf("%s started in %+v, want the issue's one pod %s", result.loc.Claim, result.loc.Sandbox, pod)
		}
	}
	if n, sent, applied := takes.Load(), copiesSent(g, roles...), process.restores(); n != 1 || sent != 1 || applied != 1 {
		t.Fatalf("six Spawns of one pod's roles released together took the copies %d times, sent the copy %d times and applied it %d times, want once each",
			n, sent, applied)
	}
}

// A launcher process the daemon did not see hold the copy is sent it as before: a restarted roles
// container, whose new process brings new launcher ids for every role, and the same process after
// a daemon restart, whose record of who holds the copy was in memory alone. Neither pod has a
// registered role, so the copy is still unspent for that pod, and each sends it to one session of
// that process and no more. The process that survived the daemon restart answers the send from
// its own record, so its starts apply the copy once.
func TestAnUnspentCopyIsSentAgainToALauncherProcessTheDaemonDidNotSeeHoldIt(t *testing.T) {
	t.Run("a restarted roles container", func(t *testing.T) {
		g, takes, _, pod := unspentCopyPod(t)
		roles := issueRoleTokens(t)
		first := g.newLauncherProcess()
		for _, token := range roles {
			g.connectToProcess(token, first)
		}
		g.r.launchers.restoreWorkspace(g.ctx, workerToken, pod)
		if sent := copiesSent(g, roles...); sent != 1 {
			t.Fatalf("the first process was sent the copy %d times, want once", sent)
		}

		restarted := g.newLauncherProcess()
		for _, token := range roles {
			g.connectToProcess(token, restarted)
		}
		for _, token := range roles {
			g.r.launchers.restoreWorkspace(g.ctx, token, pod)
		}
		if n, sent := takes.Load(), copiesSent(g, roles...); n != 2 || sent != 2 {
			t.Fatalf("the restarted container's role starts brought the takes to %d and the sends to %d, want 2 and 2: its new process holds nothing, and is sent the copy once",
				n, sent)
		}
	})

	t.Run("a daemon restart", func(t *testing.T) {
		g, takes, st, pod := unspentCopyPod(t)
		roles := issueRoleTokens(t)
		process := g.newLauncherProcess()
		for _, token := range roles {
			g.connectToProcess(token, process)
		}
		g.r.launchers.restoreWorkspace(g.ctx, workerToken, pod)

		restartedTakes := new(atomic.Int32)
		secondRuntime(t, g, func(o *Options) { countedTakes(o, st, restartedTakes, 0) })()
		for _, token := range roles {
			g.connectToProcess(token, process)
		}
		before := copiesSent(g, roles...)
		for _, token := range roles {
			startRole(t, g, token, pod)
		}
		if n, sent := restartedTakes.Load(), copiesSent(g, roles...)-before; n != 1 || sent != 1 {
			t.Fatalf("after the daemon restart the six role starts took the copies %d times and sent the copy %d times, want once each", n, sent)
		}
		if n, applied := takes.Load(), process.restores(); n != 1 || applied != 1 {
			t.Fatalf("the daemon before the restart took the copies %d times and the process applied the copy %d times, want once each", n, applied)
		}
	})
}
