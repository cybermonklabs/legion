package sandbox

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// bundleTake is the take's statement as the fault proxy matches it.
const bundleTake = "update workspace_bundles"

// faultedBundleStore is st's database through a commit-fault proxy, opened as a store of its
// own: the take runs on it, and st reads what committed.
func faultedBundleStore(t *testing.T, st *store.Store) (*store.Store, *testdb.CommitFault) {
	t.Helper()
	dsn, fault := testdb.CommitFaults(t, st.Pool().Config().ConnString())
	faulted, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	return faulted, fault
}

// faultedBundleRig is a store holding savedWorkspaceBundle, the same database through the fault
// proxy, and a rig whose hop takes the bundle through the proxy and deletes it straight.
func faultedBundleRig(t *testing.T) (*store.Store, *testdb.CommitFault, runtime.WorkspaceBundle, *rig) {
	t.Helper()
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	faulted, fault := faultedBundleStore(t, st)
	// The hop runs inside the start, whose bound is BootTimeout: the rig's two seconds would end
	// a hop that waits for the store before the store answers.
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.SavedWorkspaceBundle = func(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
			return faulted.TakeWorkspaceBundleForRestore(ctx, testProject, issue, podUID, podCreated)
		}
		o.WorkspaceBundleRestored = st.DeleteRestoredWorkspaceBundle
		o.BootTimeout = 15 * time.Second
	}))
	return st, fault, saved, g
}

// releaseAfter lets the refused store answer again two seconds after fired closes, while the hop
// it refused still waits inside its bound. The answer says whether fired closed at all.
func releaseAfter(fault *testdb.CommitFault, fired <-chan struct{}) *atomic.Bool {
	met := &atomic.Bool{}
	go func() {
		select {
		case <-fired:
			met.Store(true)
			time.Sleep(2 * time.Second)
		case <-time.After(30 * time.Second):
		}
		fault.Refuse(false)
	}()
	return met
}

// requireSentOnce wants exactly one whole slice sequence of saved before the role's start, the
// restore reported (the store's row deleted on the report), and no slice at a second hop.
func requireSentOnce(t *testing.T, st *store.Store, g *rig, spec runtime.SpawnSpec, saved runtime.WorkspaceBundle) {
	t.Helper()
	frames := g.sent(spec.Claim)
	start := slices.IndexFunc(frames, func(f shimwire.Frame) bool { _, ok := f.(shimwire.LauncherStart); return ok })
	if start < 1 {
		t.Fatalf("the launcher was sent %v, want the bundle's slices and then the role's start", frameTypes(frames))
	}
	var body []byte
	var sequence string
	for i, frame := range frames[:start] {
		slice, ok := frame.(shimwire.LauncherBundle)
		if !ok {
			t.Fatalf("the launcher was sent %v, want only the bundle's slices before the role's start", frameTypes(frames))
		}
		if i == 0 {
			sequence = slice.ID
		}
		if slice.ID != sequence || slice.Index != i || slice.Count != start {
			t.Fatalf("slice %d of %d under %q arrived at position %d of the %d sent before the start: want one whole sequence",
				slice.Index, slice.Count, slice.ID, i, start)
		}
		body = append(body, slice.Payload...)
	}
	read, err := shimwire.ParseWorkspaceBundle(body)
	if err != nil {
		t.Fatalf("the %d slices rebuilt no workspace bundle: %v", start, err)
	}
	if !bytes.Equal(read.Bundle, saved.Bundle) {
		t.Fatalf("the bundle crossed as %d bytes, want the %d saved", len(read.Bundle), len(saved.Bundle))
	}
	if _, held, err := st.WorkspaceBundleSize(g.ctx, testTree); err != nil || held {
		t.Fatalf("after the launcher reported its restore the store holds the bundle = %t (%v), want it deleted", held, err)
	}
	before := len(g.sent(spec.Claim))
	g.connect(spec.Claim)
	g.r.launchers.restoreWorkspace(g.ctx, spec.Claim, string(g.pod(SandboxName(spec.Claim)).UID))
	for _, frame := range g.sent(spec.Claim)[before:] {
		if slice, ok := frame.(shimwire.LauncherBundle); ok {
			t.Fatalf("a second hop was sent slice %d of %d, want nothing: the bundle had its one restore", slice.Index, slice.Count)
		}
	}
}

// A take whose commit landed and whose reply was lost is read from the store, and the hop sends
// the body that run read: one attempt, one send.
func TestABundleTakeWhoseReplyIsLostIsSentOnce(t *testing.T) {
	st, fault, saved, g := faultedBundleRig(t)
	fired := fault.Cut(bundleTake, testdb.CutAfterCommit)
	spec := workerSpec(t)
	g.spawn(spec)
	testdb.Met(t, fired)
	requireSentOnce(t, st, g, spec, saved)
}

// A take that did not commit leaves the attempt unspent, so the hop takes again for the same pod
// and sends what that run read.
func TestABundleTakeThatRolledBackIsTakenAgainInItsHop(t *testing.T) {
	st, fault, saved, g := faultedBundleRig(t)
	fired := fault.Cut(bundleTake, testdb.CutBeforeCommit)
	spec := workerSpec(t)
	g.spawn(spec)
	testdb.Met(t, fired)
	requireSentOnce(t, st, g, spec, saved)
}

// A take whose outcome the store cannot say waits inside the hop's bound for the store to answer,
// and then sends the body of the run that committed.
func TestABundleTakeWhoseOutcomeIsUnknownIsSentOnceTheStoreAnswers(t *testing.T) {
	st, fault, saved, g := faultedBundleRig(t)
	fired := fault.Cut(bundleTake, testdb.CutAfterCommitRefusing)
	met := releaseAfter(fault, fired)
	spec := workerSpec(t)
	g.spawn(spec)
	if !met.Load() {
		t.Fatal("the armed fault met no COMMIT of the take")
	}
	requireSentOnce(t, st, g, spec, saved)
}

// A take whose outcome stays unknown past its caller's bound says that the bundle's one restore
// attempt may be spent. The run committed, so the row records the attempt, and a later take by the
// same pod answers nothing, as after a send that failed halfway; the work stays in the database.
func TestABundleTakeWhoseOutcomeStaysUnknownSaysTheAttemptMayBeSpent(t *testing.T) {
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	faulted, fault := faultedBundleStore(t, st)
	const podUID = "pod-whose-take-stays-unknown"
	created := saved.At.Add(time.Minute)
	fired := fault.Cut(bundleTake, testdb.CutAfterCommitRefusing)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, take, err := faulted.TakeWorkspaceBundleForRestore(ctx, testProject, testTree, podUID, created)
	testdb.Met(t, fired)
	if !errors.Is(err, store.ErrCommitOutcomeUnknown) || !strings.Contains(err.Error(), "may be spent") {
		t.Fatalf("the take whose outcome stayed unknown = %v (%v), want an error that is store.ErrCommitOutcomeUnknown and says the attempt may be spent", take, err)
	}
	fault.Refuse(false)
	var attempt *string
	if err := st.Pool().QueryRow(context.Background(), `select attempt_pod_uid from workspace_bundles where issue = $1`, testTree).Scan(&attempt); err != nil {
		t.Fatalf("read the bundle's attempt: %v", err)
	}
	if attempt == nil || *attempt != podUID {
		t.Fatalf("the bundle's attempt names %v, want %s: the run committed", attempt, podUID)
	}
	if _, again, err := st.TakeWorkspaceBundleForRestore(context.Background(), testProject, testTree, podUID, created); err != nil || again != runtime.WorkspaceBundleNone {
		t.Fatalf("a later take by the same pod = %v, %v; want nothing: the attempt is spent", again, err)
	}
	if _, held, err := st.WorkspaceBundleSize(context.Background(), testTree); err != nil || !held {
		t.Fatalf("the store holds the bundle = %t (%v), want the work kept", held, err)
	}
}

// The restored-bundle delete runs on the launcher's one report of a restore, so a delete whose
// outcome was unknown waits for the store's answer rather than leaving the row to hold the pod.
func TestARestoredBundleDeleteWhoseReplyIsLostDeletesIt(t *testing.T) {
	st := migratedStore(t)
	saved := savedWorkspaceBundle()
	if err := st.PutWorkspaceBundle(context.Background(), testTree, saved.Release, saved); err != nil {
		t.Fatalf("save the released workspace's bundle: %v", err)
	}
	faulted, fault := faultedBundleStore(t, st)
	fired := fault.Cut("delete from workspace_bundles", testdb.CutAfterCommitRefusing)
	met := releaseAfter(fault, fired)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := faulted.DeleteRestoredWorkspaceBundle(ctx, testTree, saved.Release); err != nil {
		t.Fatalf("the restored bundle's delete = %v, want nil", err)
	}
	if !met.Load() {
		t.Fatal("the armed fault met no COMMIT of the delete")
	}
	if _, held, err := st.WorkspaceBundleSize(context.Background(), testTree); err != nil || held {
		t.Fatalf("the store holds the bundle = %t (%v), want it deleted", held, err)
	}
}
