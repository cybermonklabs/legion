package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// migratedStore is a migrated store on a database of its own, cloned from the package's template:
// the daemon's authorization of an issue's close, which SuspendIssue runs under its pod's launch
// turn.
func migratedStore(t *testing.T) *store.Store {
	t.Helper()
	return testpg.CloneStore(t, "legion_sandbox_test")
}

// authorizeClose is the daemon's authorization of close, the store's check (store.IssueSuspension).
func authorizeClose(st *store.Store, close store.IssueClose) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		return st.IssueSuspension(ctx, testProject, close, record.OutOfWorkflow)
	}
}

func TestIssueSuspensionReadmissionResumesTheRetainedSession(t *testing.T) {
	t.Parallel()
	st := migratedStore(t)
	g := newRig(t, nil)
	if _, err := st.OpenTreeLifecycle(g.ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(g.ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
		values ($1, $1, 'LEGION', 'root', 'done', 1, 'done', 'A', now() + interval '1 hour', 1)`, testTree); err != nil {
		t.Fatal(err)
	}
	g.issueLaunchers(rootToken)
	spec := rootSpec(t)
	old := g.spawn(spec)
	name := SandboxName(rootToken)
	uid := g.sandbox(name).UID
	authorize := authorizeClose(st, store.IssueClose{Issue: testTree, Tree: testTree, IssueGeneration: 1, TreeGeneration: 1, Row: 1})
	stored := supervise.Claim{Token: rootToken, Project: testProject, Tree: testTree, TreeEpoch: 1, Issue: testTree, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateWorking, Locator: &old, Session: "retained-session", SessionFile: resumeSession}
	if err := st.PutClaim(g.ctx, stored); err != nil {
		t.Fatal(err)
	}
	// A stored role still working keeps the close a wait, independently of runtime membership.
	if err := g.r.SuspendIssue(g.ctx, testTree, testTree, authorize); !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), "stored claim") {
		t.Fatalf("issue suspension with a remaining role process = %v, want the stored claim's wait", err)
	}
	if g.sandbox(name).mode() != modeRunning {
		t.Fatal("suspension stopped a role that still runs")
	}
	if err := g.r.Suspend(g.ctx, old); err != nil {
		t.Fatal(err)
	}
	stored.State, stored.Locator = supervise.StateSuspended, nil
	if err := st.PutClaim(g.ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := g.r.SuspendIssue(g.ctx, testTree, testTree, authorize); err != nil {
		t.Fatal(err)
	}
	if got := g.sandbox(name); got != nil {
		t.Fatalf("closed issue Sandbox = %+v, want it deleted: a close keeps nothing", got)
	}
	if g.pod(name) != nil {
		t.Fatal("issue suspension returned while its pod remained")
	}
	if _, err := st.Pool().Exec(g.ctx, `update issues set generation = 2, phase = 'admitted', status = 'todo', linger_until = null where key = $1`, testTree); err != nil {
		t.Fatal(err)
	}
	spec.Generation = 2
	spec.ResumeSessionFile = stored.SessionFile
	loc, err := g.r.Resume(g.ctx, &old, spec)
	if err != nil {
		t.Fatal(err)
	}
	if s := g.sandbox(name); loc.Sandbox.PodUID == old.Sandbox.PodUID || s == nil || s.UID == uid || s.mode() != modeRunning {
		t.Fatalf("re-admission did not resume the session in a Sandbox and pod of its own: old=%+v new=%+v sandbox=%+v", old, loc, g.sandbox(name))
	}
	var resumed string
	for _, frame := range g.sent(rootToken) {
		if start, ok := frame.(shimwire.LauncherStart); ok && start.Generation == 2 {
			resumed = start.ResumeFile
		}
	}
	if resumed != stored.SessionFile {
		t.Fatalf("resumed session = %q, want %q", resumed, stored.SessionFile)
	}
	if err := g.r.SuspendIssue(context.Background(), testTree, testTree, authorize); err != nil {
		t.Fatalf("stale close: %v", err)
	}
	if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("stale close stopped the re-admitted role: %s, %v", obs.Kind, err)
	}
}

// A child's close never touches its parent's pod: the parent runs in a pod of its own. Whichever
// state the close's stops left the child's claim in — suspended with its session for a child
// moved to backlog, icebox or triage, retired for one closed as done — the close deletes the
// child's Sandbox, since keeping it keeps nothing: the disk the roles worked on was the pod's own
// and went with the pod. The delete is a foreground one, fenced to the object as read, and
// awaited until the API no longer has it, so garbage collection takes the role Secrets the
// Sandbox owns; the restricted runtime sends no PVC request itself. A retried close finds the
// Sandbox gone and finishes.
func TestChildIssueSuspensionKeepsItsParentRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		session bool
		state   supervise.ClaimState
	}{
		{name: "a child in backlog or icebox, its claim suspended with its session", session: true, state: supervise.StateSuspended},
		{name: "a done child, its claim retired", state: supervise.StateRetired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			g := newRig(t, nil)
			deletes := map[string]metav1.DeleteOptions{}
			g.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
				delete := action.(k8stesting.DeleteAction)
				deletes[delete.GetName()] = delete.GetDeleteOptions()
				return false, nil, nil
			})
			if _, err := st.OpenTreeLifecycle(g.ctx, testProject, testTree, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Pool().Exec(g.ctx, `insert into issues
				(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ($1, $1, 'LEGION', 'root', 'implementing', 7, 'in_progress', 'A', 1),
					($2, $1, 'LEGION', 'child', 'done', 2, 'done', 'B', 1)`, testTree, childIssue); err != nil {
				t.Fatal(err)
			}
			parent := g.spawn(rootSpec(t))
			g.issueLaunchers(childToken)
			child := g.spawn(childSpec(t))
			if err := g.r.Suspend(g.ctx, child); err != nil {
				t.Fatal(err)
			}
			// The child's one stored claim is where the close's stops left it: suspended with its
			// session for a child in backlog or icebox, retired with none for a done one.
			stored := supervise.Claim{Token: childToken, Project: testProject, Tree: testTree, TreeEpoch: 1, Issue: childIssue, Role: claim.RoleImplementer,
				Generation: 1, State: tc.state}
			if tc.session {
				stored.Session, stored.SessionFile = "child-session", resumeSession
			}
			if err := st.PutClaim(g.ctx, stored); err != nil {
				t.Fatal(err)
			}
			name := SandboxName(childToken)
			uid := g.sandbox(name).UID
			g.clearActions()
			authorize := authorizeClose(st, store.IssueClose{Issue: childIssue, Tree: testTree, IssueGeneration: 2, TreeGeneration: 7, Row: 1})

			if err := g.r.SuspendIssue(g.ctx, childIssue, testTree, authorize); err != nil {
				t.Fatal(err)
			}

			if obs, err := g.r.Probe(g.ctx, parent); err != nil || obs.Kind != runtime.Alive {
				t.Fatalf("child close affected its parent: %s, %v", obs.Kind, err)
			}
			if got := g.sandbox(SandboxName(rootToken)); got == nil || got.mode() != modeRunning {
				t.Fatalf("parent Sandbox after the child's close = %+v, want Running", got)
			}
			if got := g.sandbox(name); got != nil {
				t.Fatalf("closed child Sandbox = %+v, want it gone", got)
			}
			got, deleted := deletes[name]
			if !deleted || got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Fatalf("child Sandbox delete options = %+v (deleted %t), want foreground propagation", got, deleted)
			}
			if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != uid || got.Preconditions.ResourceVersion == nil {
				t.Fatalf("child Sandbox delete preconditions = %+v, want the Sandbox's uid %s and its resourceVersion", got.Preconditions, uid)
			}
			if _, other := deletes[SandboxName(rootToken)]; other || len(deletes) != 1 {
				t.Fatalf("deletes = %v, want the child's Sandbox alone", deletes)
			}
			for _, a := range g.writes() {
				if a.resource == "persistentvolumeclaims" {
					t.Fatalf("the restricted runtime sent a PVC request: %s", a)
				}
			}
			// A retry after the delete — the outbox row run again — finds no Sandbox and finishes.
			if err := g.r.SuspendIssue(g.ctx, childIssue, testTree, authorize); err != nil {
				t.Fatalf("retried close = %v, want it finished on the Sandbox already gone", err)
			}
			if len(deletes) != 1 {
				t.Fatalf("the retried close deleted again: %v", deletes)
			}
		})
	}
}

// authorized is the store's answer that lets an issue's close or release act (store.IssueSuspension,
// store.IssueRelease), for a test whose subject is what the runtime does once the act is
// authorized, not the store's check.
func authorized(context.Context) (bool, error) { return true, nil }

// issueSecrets are the seven Secrets an issue's Sandbox owns: its provisioning Secret and the role
// Secret of each of the issue's six roles.
func issueSecrets(sandbox string) []string {
	secrets := []string{secretName(sandbox)}
	for _, role := range claim.Roles {
		secrets = append(secrets, roleSecretName(sandbox, role))
	}
	return secrets
}

// A Sandbox under the issue's name whose labels name another project, issue or tree is not the
// issue's to end. The close (SuspendIssue) and the release (ReleaseIssue) refuse it, and they do so
// before they ask authorize. The daemon's authorize of a release commits the release where it acts
// (store.Store.CommitIssueRelease), and a committed release is never cancelled. A refusal after
// that commit would come on every attempt, and the release would never end. The refusal leaves the
// Sandbox, its pod and its Secrets as the launch made them. The control is each end of the issue's
// own Sandbox, whose labels name the issue and its tree: it asks authorize once, and where
// authorize answers false it acts on nothing.
func TestASandboxOfAnotherProjectIssueOrTreeIsRefusedBeforeAuthorize(t *testing.T) {
	for _, end := range []struct {
		name string
		call func(*Runtime, context.Context, string, string, func(context.Context) (bool, error)) error
	}{
		{name: "the close", call: (*Runtime).SuspendIssue},
		{name: "the release", call: func(r *Runtime, ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
			_, err := r.ReleaseIssue(ctx, issue, tree, authorize, nil)
			return err
		}},
	} {
		for _, other := range []struct {
			name string
			// tree is the tree the end names. labels are the labels the test sets on the
			// launch's Sandbox.
			tree   string
			labels map[string]string
		}{
			{name: "another project", tree: testTree, labels: map[string]string{labelProject: "other"}},
			{name: "another issue", tree: testTree, labels: map[string]string{labelIssue: labelValue("LEGION-210")}},
			{name: "another tree", tree: "LEGION-210"},
		} {
			t.Run(end.name+" of a Sandbox of "+other.name, func(t *testing.T) {
				g := newRig(t, nil)
				loc := g.spawn(childSpec(t))
				name := SandboxName(childToken)
				if len(other.labels) > 0 {
					object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
					if err != nil {
						t.Fatal(err)
					}
					relabelled := object.(*unstructured.Unstructured).DeepCopy()
					labels := relabelled.GetLabels()
					maps.Copy(labels, other.labels)
					relabelled.SetLabels(labels)
					if err := g.dyn.Tracker().Update(sandboxGVR, relabelled, testNamespace); err != nil {
						t.Fatal(err)
					}
				}
				g.clearActions()
				var asked atomic.Int32
				authorize := func(context.Context) (bool, error) {
					asked.Add(1)
					return true, nil
				}

				err := end.call(g.r, g.ctx, childIssue, other.tree, authorize)
				if err == nil || !strings.Contains(err.Error(), "does not belong to this issue and tree") {
					t.Fatalf("%s = %v, want the refusal of a Sandbox of %s", end.name, err, other.name)
				}
				if n := asked.Load(); n != 0 {
					t.Fatalf("%s asked authorize %d times before its refusal, want none: a release commits where authorize answers that it acts", end.name, n)
				}
				if s := g.sandbox(name); s == nil || s.mode() != modeRunning {
					t.Fatalf("after the refused %s the Sandbox is %+v, want it Running as the launch left it", end.name, s)
				}
				if pod := g.pod(name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
					t.Fatalf("after the refused %s the pod is %+v, want the launch's %s", end.name, pod, loc.Sandbox.PodUID)
				}
				secrets := issueSecrets(name)
				for _, w := range g.writes() {
					if w.name == name || slices.Contains(secrets, w.name) {
						t.Fatalf("the refused %s sent %s, want nothing written to the Sandbox or its Secrets", end.name, w)
					}
				}
			})
		}
		t.Run("control: "+end.name+" of the issue's own Sandbox asks authorize once", func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(childSpec(t))
			name := SandboxName(childToken)
			g.clearActions()
			var asked atomic.Int32
			refuse := func(context.Context) (bool, error) {
				asked.Add(1)
				return false, nil
			}

			if err := end.call(g.r, g.ctx, childIssue, testTree, refuse); err != nil {
				t.Fatalf("%s of the issue's own Sandbox = %v, want no error: authorize answered that it does not act", end.name, err)
			}
			if n := asked.Load(); n != 1 {
				t.Fatalf("%s asked authorize %d times, want once: the Sandbox is the issue's own", end.name, n)
			}
			if s := g.sandbox(name); s == nil || s.mode() != modeRunning {
				t.Fatalf("after %s that authorize refused the Sandbox is %+v, want it Running as the launch left it", end.name, s)
			}
			if pod := g.pod(name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
				t.Fatalf("after %s that authorize refused the pod is %+v, want the launch's %s", end.name, pod, loc.Sandbox.PodUID)
			}
		})
	}
}

// An issue whose microVM pod keeps its volume (keptVolume) keeps one claim through every end of a
// pod that leaves the issue in the workflow. The claim is the Sandbox's own, made by Agent Sandbox
// from the one `issue` template, and each of those ends keeps the Sandbox and so the claim: an
// eviction, which deletes the pod while the Sandbox stays Running; the update round's ReplacePod; a
// launch that fails once its Running patch is sent; the release of an idle pod, which keeps the
// Sandbox Suspended and deletes the Secrets that its next launch writes again; and a launch under a
// new tree, which relabels the Sandbox. The next pod after each of them mounts the same claim at
// TreeRoot. The release records on the Sandbox that it deleted the Secrets
// (secretsReleasedAnnotation), and the released Sandbox runs no pod, so IssueSandbox answers it
// absent: the release of a pod no claim owns has nothing of it to release. The next launch removes
// that record before it writes the first Secret. The daemon makes no request on a
// PersistentVolumeClaim at any step: it has no PVC verb, and the claim's life is the Sandbox's.
func TestAKeptVolumeOutlivesItsPodsAndItsRelease(t *testing.T) {
	t.Parallel()
	name, claimName := SandboxName(childToken), IssueClaimName(childToken)
	labels := map[string]string{labelProject: testProject, labelIssue: childIssue, labelVolume: volumeKept}
	// keeps fails the test unless, after step, the issue's Sandbox is still uid and owns the one
	// kept template it was made with, and the pod that loc records mounts the kept volume's claim at
	// TreeRoot.
	keeps := func(t *testing.T, g *rig, step string, loc runtime.Locator, uid types.UID, template volumeClaimTemplate) {
		t.Helper()
		s := g.sandbox(name)
		if s == nil || s.UID != uid {
			t.Fatalf("after %s the issue's Sandbox is %+v, want %s kept", step, s, uid)
		}
		if held := s.Spec.VolumeClaimTemplates; len(held) != 1 || !sameClaimTemplate(held[0], template) || !maps.Equal(held[0].Metadata.Labels, labels) {
			t.Fatalf("after %s the issue's Sandbox owns %+v, want the kept template %+v it was made with", step, held, template)
		}
		pod := g.pod(name)
		if pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
			t.Fatalf("after %s the issue's pod is %+v, want the launch's %s", step, pod, loc.Sandbox.PodUID)
		}
		if disk := podDiskSource(t, pod.Spec); disk.PersistentVolumeClaim == nil || disk.PersistentVolumeClaim.ClaimName != claimName || disk.Ephemeral != nil {
			t.Fatalf("after %s the issue's pod takes its disk from %+v, want the claim %s alone", step, disk, claimName)
		}
		if mount := mountHolding(containerNamed(t, pod.Spec, runtime.IssuePodContainer), TreeRoot); mount == nil || mount.Name != issueVolume || mount.MountPath != TreeRoot {
			t.Fatalf("after %s the issue's pod mounts %+v at %s, want its %s volume", step, mount, TreeRoot, issueVolume)
		}
	}
	for project, config := range map[string]func(*Options){
		"a microVM issue of a microvm project": keptVolumes(SandboxMicroVM),
		"a canary of a gvisor project":         keptVolumes(SandboxGVisor, childIssue),
	} {
		t.Run(project, func(t *testing.T) {
			g := newRig(t, nil, withOptions(config))
			spec := childSpec(t)
			loc := g.spawn(spec)

			s := g.sandbox(name)
			if s == nil {
				t.Fatal("the first launch made no Sandbox")
			}
			uid := s.UID
			templates := s.Spec.VolumeClaimTemplates
			if len(templates) != 1 || templates[0].Metadata.Name != issueVolume {
				t.Fatalf("the issue's Sandbox owns %+v, want the one %s template of its kept volume", templates, issueVolume)
			}
			template := templates[0]
			if !maps.Equal(template.Metadata.Labels, labels) {
				t.Fatalf("the kept template carries %v, want exactly %v and no tree", template.Metadata.Labels, labels)
			}
			if class := template.Spec.StorageClassName; class == nil || *class != testMicroVMStorageClass {
				t.Fatalf("the kept template's class is %v, want the microvm block's %s", class, testMicroVMStorageClass)
			}
			if size, want := template.Spec.Resources.Requests.Storage(), testOptions().IssueVolume; size.Cmp(want) != 0 {
				t.Fatalf("the kept template requests %s, want issue_volume's %s", size, &want)
			}
			if modes := template.Spec.AccessModes; len(modes) != 1 || modes[0] != corev1.ReadWriteOnce {
				t.Fatalf("the kept template's access modes are %v, want ReadWriteOnce", modes)
			}
			keeps(t, g, "the first launch", loc, uid, template)
			created := s.CreationTimestamp.Time
			if !g.r.IssueKeepsVolume(childIssue, created.Add(time.Second)) {
				t.Fatal("IssueKeepsVolume = false for a round opened after the kept Sandbox was made, want true")
			}
			if g.r.IssueKeepsVolume(childIssue, created) {
				t.Fatal("IssueKeepsVolume = true for a round opened when the Sandbox was made, want false: such a Sandbox holds a new volume")
			}
			// The API server records the Sandbox's creation to the second alone. A round that
			// opened in that same second may have opened before the Sandbox was made.
			if g.r.IssueKeepsVolume(childIssue, created.Add(time.Second/2)) {
				t.Fatal("IssueKeepsVolume = true for a round opened in the second the Sandbox was made, want false: such a Sandbox may hold a new volume")
			}

			// An eviction deletes the pod and leaves the Sandbox Running. The next launch reads the
			// pod store, so it starts once that store has lost the evicted pod.
			if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
				t.Fatal(err)
			}
			g.eventually("the pod store to lose the evicted pod", func() bool {
				pod := g.r.storedPod(name)
				return pod == nil || string(pod.UID) != loc.Sandbox.PodUID
			})
			loc = g.relaunchOnNewPod(loc, &spec)
			keeps(t, g, "an eviction", loc, uid, template)

			// The update round ends the pod once every role in it has answered.
			if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: childToken, Locator: &loc}, stillDue); err != nil {
				t.Fatalf("ReplacePod: %v", err)
			}
			loc = g.relaunchOnNewPod(loc, &spec)
			keeps(t, g, "the round's ReplacePod", loc, uid, template)

			// A launch that fails once its Running patch is sent: the server applies the patch and
			// its answer never arrives, so the launch sets the Sandbox Suspended again. The round
			// ends the live pod first, so that the launch replaces it and reaches the patch.
			if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: childToken, Locator: &loc}, stillDue); err != nil {
				t.Fatalf("ReplacePod: %v", err)
			}
			g.setHold(true)
			var timedOut atomic.Bool
			apply := k8stesting.ObjectReaction(g.dyn.Tracker())
			g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				patch := a.(k8stesting.PatchAction)
				if patch.GetName() != name || !modePatched(t, string(patch.GetPatch()), modeRunning) || !timedOut.CompareAndSwap(false, true) {
					return false, nil, nil
				}
				if _, _, err := apply(a); err != nil {
					t.Errorf("apply the Running patch: %v", err)
				}
				return true, nil, fmt.Errorf("the client gave up waiting for the answer: %w", context.DeadlineExceeded)
			})
			spec = nextGeneration(spec, spec.Generation+1)
			launching, cancel := context.WithTimeout(g.ctx, 10*time.Second)
			err := failedLaunchOn(t, g, launching, spec)
			cancel()
			if err == nil || !strings.Contains(err.Error(), "set its sandbox running") {
				t.Fatalf("the launch whose Running patch timed out = %v, want the patch's failure", err)
			}
			if s := g.sandbox(name); s == nil || s.UID != uid || s.mode() != modeSuspended {
				t.Fatalf("after the failed launch the issue's Sandbox is %+v, want %s kept, Suspended", s, uid)
			}
			g.setHold(false)
			loc = g.relaunchOnNewPod(loc, &spec)
			keeps(t, g, "a launch that failed after its Running patch", loc, uid, template)

			// The release of the idle pod keeps the Sandbox, Suspended, and deletes its Secrets.
			secrets := issueSecrets(name)
			before := map[string]types.UID{}
			for _, secret := range secrets {
				held := g.secret(secret)
				if held == nil {
					t.Fatalf("the running pod's Sandbox has no Secret %s", secret)
				}
				before[secret] = held.UID
			}
			if sandbox, present := g.r.IssueSandbox(childIssue); !present || sandbox != name {
				t.Fatalf("IssueSandbox of the running kept Sandbox = %q, %t; want %s present", sandbox, present, name)
			}
			if released, err := g.r.ReleaseIssue(g.ctx, childIssue, testTree, authorized, nil); err != nil || !released {
				t.Fatalf("ReleaseIssue = %t, %v; want the running kept Sandbox released", released, err)
			}
			if s := g.sandbox(name); s == nil || s.UID != uid || s.mode() != modeSuspended || !ownsKeptVolume(s) {
				t.Fatalf("after the release the issue's Sandbox is %+v, want %s kept, Suspended, with its kept volume", s, uid)
			}
			if g.pod(name) != nil {
				t.Fatal("ReleaseIssue returned while the issue's pod remained")
			}
			for _, secret := range secrets {
				if held := g.secret(secret); held != nil {
					t.Fatalf("the release left the Secret %s, want each Secret of the Sandbox deleted", secret)
				}
			}
			if record := g.sandbox(name).Annotations[secretsReleasedAnnotation]; record != string(uid) {
				t.Fatalf("after the release the Sandbox's record of its deleted Secrets is %q, want %s", record, uid)
			}
			g.eventually("IssueSandbox to answer the released kept Sandbox absent", func() bool {
				_, present := g.r.IssueSandbox(childIssue)
				return !present
			})
			if !g.r.IssueKeepsVolume(childIssue, created.Add(time.Second)) {
				t.Fatal("IssueKeepsVolume of the released kept Sandbox = false, want true: its volume is still there")
			}
			launched := len(g.writes())
			loc = g.relaunchOnNewPod(loc, &spec)
			keeps(t, g, "the release of the idle pod", loc, uid, template)
			if s := g.sandbox(name); s.mode() != modeRunning {
				t.Fatalf("the launch after the release left the Sandbox %s, want Running", s.mode())
			}
			if record, held := g.sandbox(name).Annotations[secretsReleasedAnnotation]; held {
				t.Fatalf("the launch after the release left the record %q of the deleted Secrets on the Sandbox, want none", record)
			}
			writes := g.writes()[launched:]
			cleared := slices.IndexFunc(writes, func(a action) bool {
				return a.resource == "sandboxes" && a.verb == "patch" && a.name == name && strings.Contains(a.patch, secretsReleasedAnnotation)
			})
			written := slices.IndexFunc(writes, func(a action) bool { return a.resource == "secrets" })
			if cleared < 0 || written < 0 || cleared > written {
				t.Fatalf("the launch after the release removed the record at write %d and wrote the first Secret at write %d of %v; want the record removed first",
					cleared, written, writes)
			}
			for _, secret := range secrets {
				if held := g.secret(secret); held == nil || held.UID == before[secret] {
					t.Fatalf("the launch after the release left the Secret %s as %+v, want a new one", secret, held)
				}
			}
			if sandbox, present := g.r.IssueSandbox(childIssue); !present || sandbox != name {
				t.Fatalf("IssueSandbox after the launch = %q, %t; want %s present", sandbox, present, name)
			}

			// The issue is re-admitted as a root of its own: its launch relabels the Sandbox for the
			// new tree and keeps it. The relabel is fenced to the Sandbox's resourceVersion, which an
			// API server gives every object and the rig's tracker does not, so the test gives the
			// Sandbox one first.
			object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
			if err != nil {
				t.Fatal(err)
			}
			versioned := object.(*unstructured.Unstructured).DeepCopy()
			versioned.SetResourceVersion("sandbox-rv-before-the-new-tree")
			if err := g.dyn.Tracker().Update(sandboxGVR, versioned, testNamespace); err != nil {
				t.Fatal(err)
			}
			orphan := claim.Token("legion-legion-legion-209-architect")
			orphanSpec := testSpec(t, orphan, claim.RoleArchitect, childIssue)
			orphanSpec.Tree = childIssue
			loc = g.spawn(orphanSpec)
			if s := g.sandbox(name); s.Labels[labelTree] != labelValue(childIssue) {
				t.Fatalf("after the launch under tree %s the Sandbox carries tree %q", childIssue, s.Labels[labelTree])
			}
			keeps(t, g, "a launch under a new tree", loc, uid, template)

			for _, request := range g.requests() {
				if request.resource == "persistentvolumeclaims" {
					t.Fatalf("the runtime sent a PVC request: %s", request)
				}
			}
		})
	}
}

// A kept volume does not outlive its issue's time in the workflow. Each end that takes the issue
// out of the workflow deletes the Sandbox that owns the kept volume, in the foreground, so garbage
// collection takes the claim with it: the issue's suspension (SuspendIssue: a close, a tree's
// linger, a production check's wait) waits the pod out first; the tree's cleanup (CleanupTree)
// deletes each kept Sandbox of the tree; and the orphan sweep (ReconcileOrphans) deletes a kept
// Sandbox of a tree that is not live. The API then holds no Sandbox of the name.
func TestAKeptVolumeGoesWithItsIssuesSuspension(t *testing.T) {
	t.Parallel()
	t.Run("the issue's suspension", func(t *testing.T) {
		name := SandboxName(childToken)
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
		g.suspendDelay.Store(int64(50 * time.Millisecond))
		var mu sync.Mutex
		deletes := map[string]metav1.DeleteOptions{}
		podAtDelete := false
		g.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			mu.Lock()
			defer mu.Unlock()
			delete := action.(k8stesting.DeleteAction)
			deletes[delete.GetName()] = delete.GetDeleteOptions()
			podAtDelete = podAtDelete || g.pod(delete.GetName()) != nil
			return false, nil, nil
		})
		g.spawn(childSpec(t))
		s := g.sandbox(name)
		if s == nil || !ownsKeptVolume(s) {
			t.Fatalf("the issue's Sandbox = %+v, want one that owns the kept volume, or this case proves nothing", s)
		}

		if err := g.r.SuspendIssue(g.ctx, childIssue, testTree, authorized); err != nil {
			t.Fatalf("SuspendIssue: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		got, deleted := deletes[name]
		if !deleted || got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationForeground {
			t.Fatalf("the kept Sandbox's delete options = %+v (deleted %t), want foreground propagation", got, deleted)
		}
		if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != s.UID {
			t.Fatalf("the kept Sandbox's delete preconditions = %+v, want its uid %s", got.Preconditions, s.UID)
		}
		if podAtDelete {
			t.Fatal("SuspendIssue deleted the kept Sandbox while its pod remained, want the pod waited out first")
		}
		if g.sandbox(name) != nil || g.pod(name) != nil {
			t.Fatalf("after SuspendIssue the Sandbox is %+v and the pod %+v, want both gone", g.sandbox(name), g.pod(name))
		}
		for _, request := range g.requests() {
			if request.resource == "persistentvolumeclaims" {
				t.Fatalf("the runtime sent a PVC request: %s", request)
			}
		}
	})

	// deletedKept fails the test unless c's runtime deleted each issue Sandbox of the tree, both of
	// them kept, in the foreground, fenced to the uid and resourceVersion it read, and the API holds
	// neither any more, while the other tree's Sandbox stays.
	deletedKept := func(t *testing.T, c *cleanupRig) {
		t.Helper()
		for name, uid := range map[string]types.UID{c.root: "uid-sandbox-root", c.child: "uid-sandbox-child"} {
			got, deleted := c.sandboxDeletes[name]
			if !deleted || got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Fatalf("the kept Sandbox %s's delete options = %+v (deleted %t), want foreground propagation", name, got, deleted)
			}
			if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != uid || got.Preconditions.ResourceVersion == nil {
				t.Fatalf("the kept Sandbox %s's delete preconditions = %+v, want its uid %s and its resourceVersion", name, got.Preconditions, uid)
			}
			if s := c.sandbox(name); s != nil {
				t.Fatalf("the kept Sandbox %s = %+v, want it gone", name, s)
			}
		}
		if c.sandbox(c.other) == nil {
			t.Fatalf("the other tree's Sandbox %s is gone, want it left alone", c.other)
		}
		if len(c.pvcDeletes) != 0 {
			t.Fatalf("the runtime sent PVC delete requests: %+v", c.pvcDeletes)
		}
	}

	t.Run("the tree's cleanup", func(t *testing.T) {
		c := keptCleanupRig(t)
		if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
			t.Fatalf("CleanupTree: %v", err)
		}
		deletedKept(t, c)
	})

	t.Run("the orphan sweep of a tree that is not live", func(t *testing.T) {
		c := keptCleanupRig(t)
		c.store.close(testTree)
		if err := c.r.ReconcileOrphans(c.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans: %v", err)
		}
		deletedKept(t, c)
	})
}

// keptCleanupRig is cleanupRig with each of the tree's two issue Sandboxes owning the claim
// template of a kept volume (withKeptTemplate), as a microVM issue under keep_volume leaves them,
// each at the resourceVersion cleanupRig gave it, and the runtime's Sandbox store holding both so.
func keptCleanupRig(t *testing.T) *cleanupRig {
	t.Helper()
	c := newCleanupRig(t)
	for _, name := range []string{c.root, c.child} {
		object, err := c.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
		if err != nil {
			t.Fatal(err)
		}
		kept := withKeptTemplate(t, object.(*unstructured.Unstructured).DeepCopy(), true)
		if err := c.dyn.Tracker().Update(sandboxGVR, kept, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	c.eventually("the Sandbox store to hold both issue Sandboxes kept", func() bool {
		for _, name := range []string{c.root, c.child} {
			if u, err := c.r.storedObject(name); err != nil || u == nil || !storedOwnsKeptVolume(u) {
				return false
			}
		}
		return true
	})
	c.clearActions()
	return c
}

// IssueSandbox answers a Sandbox that is going absent, though the informer's store still holds
// it. The API answers a delete (awaitSandboxDeleted) before the watch carries it to the store, so
// a Sandbox this runtime's own delete took answers absent from the moment the API accepted the
// delete, by what the runtime keeps of that delete (its uid), until the store lets it go, and a
// Sandbox made again under the name answers present. A stored Sandbox that carries a
// deletionTimestamp, whoever deleted it, answers absent too. A release that finds no Sandbox says
// it released none (ReleaseIssue's false). A kept Sandbox is the same case with the release's
// record in place of the delete: it answers absent from the moment the API answered the patch
// that wrote the record, and a release of it after that takes nothing and answers false.
func TestIssueSandboxAnswersASandboxThatIsGoingAbsent(t *testing.T) {
	t.Parallel()
	name := SandboxName(workerToken)
	t.Run("one this runtime's release deleted, before the store lets it go", func(t *testing.T) {
		release := make(chan struct{})
		let := sync.OnceFunc(func() { close(release) })
		t.Cleanup(let)
		g := newRig(t, nil, withHeldSandboxEvents(release, heldDeletion))
		g.issueLaunchers(workerToken)
		g.spawn(workerSpec(t))
		if got, present := g.r.IssueSandbox(testTree); !present || got != name {
			t.Fatalf("IssueSandbox of the running pod's Sandbox = %q, %t; want %s present", got, present, name)
		}
		stored, err := g.r.storedObject(name)
		if err != nil || stored == nil {
			t.Fatalf("the store holds no Sandbox %s before the release (%v), or this case proves nothing", name, err)
		}
		took := stored.GetUID()

		// The release calls its writing callback once, before its first write: it took the pod. The
		// callback reads the runtime's writes since the clear below, which must be none yet.
		var wrote int
		var before []action
		writing := func(context.Context) error {
			wrote++
			before = g.writes()
			return nil
		}
		g.clearActions()
		if released, err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized, writing); err != nil || !released || wrote != 1 {
			t.Fatalf("ReleaseIssue = %t, %v with its writing callback called %d times; want the running pod's Sandbox released and the callback called once",
				released, err, wrote)
		}
		if len(before) != 0 {
			t.Fatalf("the release had written %v when it called its writing callback, want nothing: the callback comes before the first write", before)
		}
		if len(g.writes()) == 0 {
			t.Fatal("the release wrote nothing after its writing callback, or this case proves nothing")
		}
		if s := g.sandbox(name); s != nil {
			t.Fatalf("after the release the API holds the Sandbox %+v, want it deleted", s)
		}
		if u, err := g.r.storedObject(name); err != nil || u == nil || u.GetUID() != took {
			t.Fatalf("the store holds %v (%v) after the delete, want the deleted Sandbox %s still there, or this case proves nothing", u, err, took)
		}
		if got, present := g.r.IssueSandbox(testTree); present {
			t.Fatalf("IssueSandbox of the Sandbox the release deleted = %q, present; want it absent while the store still holds it", got)
		}
		// The teardown of a second release, as the daemon's would run, finds no Sandbox and
		// answers that it released none.
		if released, err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized, writing); err != nil || released || wrote != 1 {
			t.Fatalf("a second ReleaseIssue = %t, %v with the callback called %d times in all; want false and no call: there is no Sandbox left to release",
				released, err, wrote)
		}

		// A Sandbox made again under the name is another object, and answers present once the
		// store holds it, behind the deletion it was made after.
		again := stored.DeepCopy()
		again.SetUID("uid-sandbox-made-again")
		again.SetResourceVersion("")
		if err := g.dyn.Tracker().Create(sandboxGVR, again, testNamespace); err != nil {
			t.Fatal(err)
		}
		if got, present := g.r.IssueSandbox(testTree); present {
			t.Fatalf("IssueSandbox = %q, present, while the store still holds the deleted Sandbox; want it absent", got)
		}
		let()
		// The informer writes its store before it hands the delete on to the runtime, which drops
		// what it keeps of the release only then (notify), so the store may answer first: both are
		// waited for.
		g.eventually("the store to hold the Sandbox made again, IssueSandbox to answer it present, and the runtime to drop the release's delete", func() bool {
			u, err := g.r.storedObject(name)
			got, present := g.r.IssueSandbox(testTree)
			g.r.mu.Lock()
			_, held := g.r.taken[name]
			g.r.mu.Unlock()
			return err == nil && u != nil && u.GetUID() == "uid-sandbox-made-again" && present && got == name && !held
		})
	})

	t.Run("one whose stored object carries a deletionTimestamp", func(t *testing.T) {
		g := newRig(t, nil)
		g.issueLaunchers(workerToken)
		g.spawn(workerSpec(t))
		if _, present := g.r.IssueSandbox(testTree); !present {
			t.Fatal("IssueSandbox of the running pod's Sandbox = absent, want present, or this case proves nothing")
		}
		// A delete by another hand, held by a finalizer: the API server keeps the object, with
		// its deletionTimestamp, until the finalizer is removed.
		object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
		if err != nil {
			t.Fatal(err)
		}
		deleting := object.(*unstructured.Unstructured).DeepCopy()
		now := metav1.Now()
		deleting.SetDeletionTimestamp(&now)
		deleting.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
		if err := g.dyn.Tracker().Update(sandboxGVR, deleting, testNamespace); err != nil {
			t.Fatal(err)
		}
		g.eventually("IssueSandbox to answer the Sandbox being deleted absent", func() bool {
			_, present := g.r.IssueSandbox(testTree)
			return !present
		})
	})

	t.Run("one this runtime's release kept, before the store shows the release's record", func(t *testing.T) {
		kept := SandboxName(childToken)
		release := make(chan struct{})
		let := sync.OnceFunc(func() { close(release) })
		t.Cleanup(let)
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)), withHeldSandboxEvents(release, heldReleaseRecord))
		g.spawn(childSpec(t))
		if got, present := g.r.IssueSandbox(childIssue); !present || got != kept {
			t.Fatalf("IssueSandbox of the running kept Sandbox = %q, %t; want %s present", got, present, kept)
		}

		if released, err := g.r.ReleaseIssue(g.ctx, childIssue, testTree, authorized, nil); err != nil || !released {
			t.Fatalf("ReleaseIssue = %t, %v; want the running kept Sandbox released", released, err)
		}
		s := g.sandbox(kept)
		if s == nil || s.mode() != modeSuspended || s.Annotations[secretsReleasedAnnotation] != string(s.UID) {
			t.Fatalf("after the release the API holds the Sandbox %+v, want it kept, Suspended, with the release's record", s)
		}
		if u, err := g.r.storedObject(kept); err != nil || u == nil || recordsRelease(u, s.UID) {
			t.Fatalf("the store holds %v (%v) after the release, want the kept Sandbox without the release's record yet, or this case proves nothing", u, err)
		}
		if got, present := g.r.IssueSandbox(childIssue); present {
			t.Fatalf("IssueSandbox of the Sandbox the release kept = %q, present; want it absent while the store has not shown the release's record", got)
		}
		// The teardown of a second release, as the daemon's would run, finds the Sandbox the first
		// one kept, takes nothing and answers that it released nothing.
		g.clearActions()
		var wrote int
		writing := func(context.Context) error { wrote++; return nil }
		if released, err := g.r.ReleaseIssue(g.ctx, childIssue, testTree, authorized, writing); err != nil || released || wrote != 0 {
			t.Fatalf("a second ReleaseIssue = %t, %v with its writing callback called %d times; want false and no call: the kept Sandbox holds nothing left to release",
				released, err, wrote)
		}
		if writes := g.writes(); len(writes) != 0 {
			t.Fatalf("the second release wrote %v, want nothing: an earlier release already kept the Sandbox", writes)
		}

		let()
		g.eventually("the store to show the release's record, and the runtime to drop what it kept of it", func() bool {
			u, err := g.r.storedObject(kept)
			g.r.mu.Lock()
			_, held := g.r.taken[kept]
			g.r.mu.Unlock()
			return err == nil && recordsRelease(u, s.UID) && !held
		})
		if got, present := g.r.IssueSandbox(childIssue); present {
			t.Fatalf("IssueSandbox of the released kept Sandbox = %q, present; want it absent on its record", got)
		}
	})
}

// A release whose writing callback fails ends there, before its first write
// (runtime.IssueReleaser): it answers the callback's error and false, writes nothing, and leaves
// the Sandbox and its pod as they were. So the daemon's teardown, which notes its mark from the
// callback, takes no pod it could not note, and its retry finds the Sandbox to release.
func TestAReleaseWhoseWritingCallbackFailsWritesNothing(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.issueLaunchers(workerToken)
	g.spawn(workerSpec(t))
	name := SandboxName(workerToken)
	before := g.sandbox(name)
	if before == nil {
		t.Fatalf("the API holds no Sandbox %s before the release, or this case proves nothing", name)
	}
	refused := errors.New("the release's mark cannot be noted")
	g.clearActions()
	released, err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized, func(context.Context) error { return refused })
	if !errors.Is(err, refused) || released {
		t.Fatalf("ReleaseIssue = %t, %v; want false and the callback's error", released, err)
	}
	if writes := g.writes(); len(writes) != 0 {
		t.Fatalf("the release wrote %v after its callback failed, want nothing", writes)
	}
	if after := g.sandbox(name); !reflect.DeepEqual(after, before) {
		t.Fatalf("after the failed release the API holds the Sandbox %+v, want it unchanged, %+v", after, before)
	}
	if got, present := g.r.IssueSandbox(testTree); !present || got != name {
		t.Fatalf("IssueSandbox after the failed release = %q, %t; want %s present: nothing took it", got, present, name)
	}
}
