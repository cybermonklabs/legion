package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// keptPodStore is a migrated store whose issues table records testTree under the rig's project:
// the store records a pod of a kept volume only for an issue the asking project records
// (store.Store.StartKeptPod).
func keptPodStore(t *testing.T) *store.Store {
	t.Helper()
	st := migratedStore(t)
	if _, err := st.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, testTree); err != nil {
		t.Fatalf("record %s: %v", testTree, err)
	}
	return st
}

// keptPodStart is st's record of a role's start in a pod of the rig's project whose Sandbox owns
// the issue's kept volume, as the daemon wires it into the runtime (internal/daemon's keptPodStart).
func keptPodStart(st *store.Store) runtime.KeptPodStart {
	return func(ctx context.Context, issue, podUID string, keptSince time.Time) error {
		return st.StartKeptPod(ctx, testProject, issue, podUID, keptSince)
	}
}

// faultedStore is st's database through a commit-fault proxy, opened as a store of its own: the
// record runs on it, and st reads what committed.
func faultedStore(t *testing.T, st *store.Store) (*store.Store, *testdb.CommitFault) {
	t.Helper()
	dsn, fault := testdb.CommitFaults(t, st.Pool().Config().ConnString())
	faulted, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	return faulted, fault
}

// keptPodRows is the store's record of each pod of the rig's issue, as "<pod>/<kept_since>/<mark>"
// in the order of the pods' uids, kept_since in RFC 3339.
func keptPodRows(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool().Query(t.Context(), `select pod_uid || '/'
		|| to_char(kept_since at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') || '/'
		|| case when holds_earlier_work then 'marked' else 'unmarked' end
		from issue_pod_disks where issue = $1 order by pod_uid`, testTree)
	if err != nil {
		t.Fatalf("read the record of the disks of the pods of %s: %v", testTree, err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the record of the disks of the pods of %s: %v", testTree, err)
	}
	return got
}

// A role's start in a pod whose Sandbox owns the issue's kept volume records that pod with its
// volume (launchers.recordKeptPod, store.StartKeptPod). What the roles of that pod leave unpushed
// stays on the volume, so the next pod of the same Sandbox holds it, and the store marks that next
// pod for it, while the first pod of the volume stays unmarked (store.PodHoldsEarlierWork). A
// launcher that recorded no pod of a kept volume would leave the next pod reading as holding
// nothing from before it.
func TestAKeptPodsStartRecordsThePodAndMarksTheNextPodOfItsVolume(t *testing.T) {
	t.Parallel()
	st := keptPodStore(t)
	g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), withOptions(func(o *Options) {
		o.KeptPodStart = keptPodStart(st)
	}))
	spec := workerSpec(t)
	first := g.spawn(spec)
	s := g.sandbox(first.Sandbox.Name)
	if s == nil || !ownsKeptVolume(s) {
		t.Fatalf("the issue's Sandbox = %+v, want one that owns the kept volume, or this case proves nothing", s)
	}

	// The release keeps the Sandbox and its volume, and the issue's next launch brings the next pod
	// of that Sandbox, in which the role starts again.
	if released, err := g.r.ReleaseIssue(g.ctx, testTree, "", authorized, nil); err != nil || !released {
		t.Fatalf("ReleaseIssue = %v, %v; want the issue's pod released", released, err)
	}
	next := g.relaunchOnNewPod(first, &spec)
	if kept := g.sandbox(next.Sandbox.Name); kept == nil || kept.UID != s.UID {
		t.Fatalf("after the release the issue's Sandbox is %+v, want %s kept", kept, s.UID)
	}

	for _, pod := range []struct {
		what string
		uid  string
		want bool
	}{
		{"the first pod of the volume", first.Sandbox.PodUID, false},
		{"the next pod of the volume", next.Sandbox.PodUID, true},
	} {
		holds, err := st.PodHoldsEarlierWork(g.ctx, testTree, pod.uid)
		if err != nil || holds != pod.want {
			t.Errorf("the store marks %s, %s, as holding work from before it = %t (%v), want %t; the store records %q",
				pod.what, pod.uid, holds, err, pod.want, keptPodRows(t, st))
		}
	}
}

// A role's start in a pod of a kept volume does not go on while the store cannot record that pod:
// a role that ran there unrecorded could leave work on the volume that the round over the next pod
// of it would name no role for. The store refuses every connection here for the whole of the first
// launch, so the record fails, whether the store's own retries of it end at once or at the bound of
// the role's start. The launch then answers the refusal (runtime.KeptPodUnrecorded), whose sentence
// names the pod and its volume's creation; the launcher was sent no Start, and no Stop either,
// since nothing was issued; and the store holds no row of the pod. Once the store answers again,
// the next launch starts the role and the row is written.
func TestAKeptPodTheStoreCannotRecordStartsNoRoleUntilItCan(t *testing.T) {
	t.Parallel()
	st := keptPodStore(t)
	faulted, fault := faultedStore(t, st)
	g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), withOptions(func(o *Options) {
		o.KeptPodStart = keptPodStart(faulted)
	}))
	fault.Refuse(true)

	spec := workerSpec(t)
	g.launcher(spec.Claim)
	_, err := g.r.Spawn(g.ctx, spec)
	name := SandboxName(spec.Claim)
	s, pod := g.sandbox(name), g.pod(name)
	if s == nil || !ownsKeptVolume(s) || pod == nil {
		t.Fatalf("the issue's Sandbox = %+v with pod %+v, want one that owns the kept volume and its pod, or this case proves nothing", s, pod)
	}
	var refused runtime.KeptPodUnrecorded
	if !errors.As(err, &refused) || refused.PodUID != string(pod.UID) || !refused.KeptSince.Equal(s.CreationTimestamp.Time) {
		t.Fatalf("the launch whose pod the store could not record answered %v, want the refusal of pod %s of the kept volume made %s",
			err, pod.UID, s.CreationTimestamp)
	}
	for _, says := range []string{string(pod.UID), s.CreationTimestamp.UTC().Format(time.RFC3339), "could not be recorded for the issue's update round"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal reads %q, which does not say %q", err, says)
		}
	}
	frames := g.sent(spec.Claim)
	if slices.ContainsFunc(frames, func(f shimwire.Frame) bool {
		switch f.(type) {
		case shimwire.LauncherStart, shimwire.LauncherStop:
			return true
		}
		return false
	}) {
		t.Fatalf("the launcher was sent %v, want no Start, and so no Stop", frameTypes(frames))
	}
	if rows := keptPodRows(t, st); len(rows) != 0 {
		t.Fatalf("after the refused launch the store records %q, want no row", rows)
	}

	fault.Refuse(false)
	spec = nextGeneration(spec, spec.Generation+1)
	loc := g.spawn(spec)
	if frames := g.sent(spec.Claim); !slices.ContainsFunc(frames, func(f shimwire.Frame) bool {
		start, ok := f.(shimwire.LauncherStart)
		return ok && start.Generation == spec.Generation
	}) {
		t.Fatalf("the launcher was sent %v, want the start of generation %d", frameTypes(frames), spec.Generation)
	}
	want := []string{loc.Sandbox.PodUID + "/" + s.CreationTimestamp.UTC().Format(time.RFC3339) + "/unmarked"}
	if rows := keptPodRows(t, st); !slices.Equal(rows, want) {
		t.Fatalf("after the launch the store records %q, want %q", rows, want)
	}
}
