package sandbox

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// otherAccount is the ServiceAccount an operator moves runtime.kubernetes.pod.service_account to
// over pods the configuration before it created: distinct from testOptions()'s own account, which
// names none, so every pod a rig launches runs as the namespace's default.
const otherAccount = "legion-worker"

// controllerAccount is the runtime.kubernetes.pod.controller_service_account an operator gives the
// controller's pod in place of otherAccount, the account every other pod runs as.
const controllerAccount = "legion-controller"

// withControllerAccount configures a restarted daemon as an operator who gives the controller an
// account of its own does: every other pod on otherAccount, the controller's on controllerAccount.
func withControllerAccount(o *Options) {
	o.Pod.ServiceAccount, o.Pod.ControllerServiceAccount = otherAccount, controllerAccount
}

// onAccount edits a stored pod to run as account, as the API server records the account a pod was
// created under: the one field accountDrift reads.
func onAccount(account string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Spec.ServiceAccountName = account }
}

// A pod created under a ServiceAccount other than the configured one runs its agents with that
// account's identity for the pod's whole life: nothing the daemon writes moves it, so the account
// is reached only by replacing the pod, and which replacement that is follows the pod's kind
// (accountDrift, roundCovers). A launch into a pod an update round covers — every issue pod —
// keeps it for the account as for every other kind of drift, and leaves the replacement to that
// round, which first has every worker in the pod do the steps of its update message
// (supervise.UpdateTask). A launch into a pod no round covers — the daemon-launched controller's
// — replaces it for the account whatever it is installing, since no round will ever come for such
// a pod: the account is one of the two drifts, beside the image (podImageDrift), that still beat
// the stale-plugin exemption, which otherwise keeps a drifted controller pod and installs into it.
// A replaced Sandbox is kept under its new pod (an issue pod's disk is the pod's own ephemeral
// volume and goes with it, as for image drift). An issue pod is kept here whatever account it runs
// as, so the rows that hold the launch's own reading of the account are the controller's: its pod
// is kept where it already runs as the configured account — including a pod on `default` under a
// configuration naming no account, which is that same account spelled the other way — and
// replaced where it does not. What an issue pod's account reaches instead is its round's reasons
// (TestPodDriftNamesTheServiceAccountThePodRunsAs).
func TestALaunchKeepsAnAccountDriftedPodARoundCoversAndReplacesOneNoRoundCovers(t *testing.T) {
	t.Parallel()
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	for name, tc := range map[string]struct {
		spec     func(*testing.T) runtime.SpawnSpec
		session  string
		account  string            // the account the restarted daemon is configured with
		stored   func(*corev1.Pod) // what the stored pod is edited to run as before the restart
		plugins  *runtime.PluginInstall
		replaced bool
	}{
		"an issue pod under a configuration naming another ServiceAccount":             {workerSpec, resumeSession, otherAccount, nil, nil, false},
		"the controller's pod under a configuration naming another ServiceAccount":     {controllerSpec, controllerSession, otherAccount, nil, nil, true},
		"an installing Resume keeps an issue pod on another ServiceAccount":            {workerSpec, resumeSession, otherAccount, nil, plugins, false},
		"an installing Resume replaces the controller's pod on another ServiceAccount": {controllerSpec, controllerSession, otherAccount, nil, plugins, true},
		"the controller's pod already on the configured ServiceAccount":                {controllerSpec, controllerSession, otherAccount, onAccount(otherAccount), nil, false},
		"no configured ServiceAccount and the controller's pod on default":             {controllerSpec, controllerSession, "", onAccount(defaultServiceAccount), nil, false},
		"no configured ServiceAccount and the controller's pod on another one":         {controllerSpec, controllerSession, "", onAccount(otherAccount), nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := tc.spec(t)
			loc := g.spawn(spec)
			sandboxUID := g.sandbox(loc.Sandbox.Name).UID
			if tc.stored != nil {
				g.update(g.pod(loc.Sandbox.Name), tc.stored)
			}
			r2 := secondRuntime(t, g, func(o *Options) { o.Pod.ServiceAccount = tc.account })()
			g.connectRunning(spec.Claim, loc.Sandbox.Generation)
			g.eventually("the role's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(spec.Claim, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			spec.Generation, spec.BootToken, spec.ResumeSessionFile, spec.Plugins = 2, "boot-g2", tc.session, tc.plugins
			newLoc, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if replaced := newLoc.Sandbox.PodUID != loc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("Resume replaced the pod: %t, want %t (pod %s, then %s)", replaced, tc.replaced, loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
			}
			if newLoc.Sandbox.Generation != 2 {
				t.Fatalf("resumed at %+v, want generation 2", newLoc.Sandbox)
			}
			// The replacement is the Sandbox's next pod, never another Sandbox: the Sandbox and
			// its address record are kept across it.
			if after := g.sandbox(loc.Sandbox.Name); after == nil || after.UID != sandboxUID {
				t.Fatalf("the Sandbox is %+v, want the one the launch spawned (%s) kept", after, sandboxUID)
			}
			if tc.replaced {
				if account, drifted := accountDrift(g.pod(loc.Sandbox.Name), tc.account); drifted {
					t.Fatalf("the pod the role runs in runs as %s, want the configured %s", account, serviceAccountOr(tc.account))
				}
			}
			if tc.plugins != nil {
				// A kept pod's installing Resume starts through the install wrapper, as a kept
				// drifted image's does (TestRelaunchKeepsADriftedPodAndReplacesASuspendedSandboxs):
				// the launch installs the current pair into the pod it is leaving to its round.
				// A replaced pod's does not: its fresh copies are already current, so the argv
				// starts the shim directly.
				start := lastStart(t, g, spec.Claim)
				i := slices.Index(start.Argv, "--")
				if i < 0 || i+1 >= len(start.Argv) {
					t.Fatalf("the installing Resume's argv %q names no command after --", start.Argv)
				}
				switch wrapped := start.Argv[i+1] == "/bin/sh"; {
				case tc.replaced && wrapped:
					t.Fatalf("a pod replaced for its ServiceAccount was started through the install wrapper, want a plain agent command: argv %q", start.Argv)
				case !tc.replaced && !wrapped:
					t.Fatalf("a pod kept for its own round to replace was started plain, want the install wrapper: argv %q", start.Argv)
				}
			}
		})
	}
}

// The boot reconciliation names, in the log, each Sandbox whose pod runs as a ServiceAccount other
// than the configured one — a pod created under the account the configuration named before, whose
// agents hold that account's identity until the pod is replaced, by its own update round where a
// round covers the pod and by the next launch of one of its roles where none does (roundCovers) —
// with the account it runs as and the configured one, one warning per Sandbox, the daemon-launched
// controller's included, and nothing of a pod already on the configured account. A cache Sandbox
// is passed over before the warning, as it is for the image: its own sweep makes its pod again
// under the new account, with no round and no launch to wait for.
func TestTheBootReconciliationWarnsOfEachPodRunningAnotherServiceAccount(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	spec := workerSpec(t)
	issue, controller := g.spawn(spec), g.spawn(controllerSpec(t))
	child := g.spawn(childSpec(t))
	// The child's pod already runs as the account the restarted daemon is configured with.
	g.update(g.pod(child.Sandbox.Name), onAccount(otherAccount))
	logs := &lockedLog{}
	r2 := secondRuntime(t, g, func(o *Options) {
		o.Pod.ServiceAccount = otherAccount
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})()
	warnings := func(sandbox string) int {
		return strings.Count(logs.String(), "sandbox runtime: "+sandbox+" runs as ServiceAccount ")
	}
	known := []runtime.Known{{Claim: workerToken, Locator: &issue}, {Claim: controllerToken, Locator: &controller}, {Claim: childToken, Locator: &child}}

	if err := r2.ReconcileOrphans(g.ctx, known, 0); err != nil {
		t.Fatal(err)
	}
	for _, loc := range []runtime.Locator{issue, controller} {
		want := "sandbox runtime: " + loc.Sandbox.Name + " runs as ServiceAccount " + defaultServiceAccount +
			", the configured ServiceAccount is " + otherAccount + "; its agents hold that identity until the pod is replaced" +
			" — by its update round where a round covers the pod, by the next launch of one of its roles where none does"
		if !strings.Contains(logs.String(), want) || warnings(loc.Sandbox.Name) != 1 {
			t.Errorf("the boot reconciliation logged %q, want one warning %q", logs.String(), want)
		}
	}
	if warnings(child.Sandbox.Name) != 0 {
		t.Errorf("the boot reconciliation warned of %s, whose pod runs as the configured account: %q", child.Sandbox.Name, logs.String())
	}
	if cache := CacheSandboxName(testProject, spec.Repository); warnings(cache) != 0 {
		t.Errorf("the boot reconciliation warned of the cache %s, which its own sweep makes again: %q", cache, logs.String())
	}
}

// Where runtime.kubernetes.pod.controller_service_account names an account, the controller's pod
// is held to that account and not to pod.service_account (accountDrift, podKind.serviceAccount):
// a launch of the controller into a pod already on the controller's account keeps it, as a pod on
// the configured account is kept, and one into a pod on pod.service_account — the account every
// controller pod ran as before the key was set — or on the namespace's default replaces it, since
// no round covers the controller's pod. The pod the replacement makes runs as the controller's
// account. Holding the controller to pod.service_account here would read every controller pod the
// key built as drifted and replace it at every launch.
func TestALaunchHoldsTheControllersPodToTheControllersOwnServiceAccount(t *testing.T) {
	for name, tc := range map[string]struct {
		stored   func(*corev1.Pod) // what the stored pod is edited to run as before the restart
		replaced bool
	}{
		"a pod on the controller's account":                        {onAccount(controllerAccount), false},
		"a pod on pod.service_account while the key names another": {onAccount(otherAccount), true},
		"a pod on the namespace's default while the key names one": {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := controllerSpec(t)
			loc := g.spawn(spec)
			if tc.stored != nil {
				g.update(g.pod(loc.Sandbox.Name), tc.stored)
			}
			r2 := secondRuntime(t, g, withControllerAccount)()
			g.connectRunning(spec.Claim, loc.Sandbox.Generation)
			g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(spec.Claim, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", controllerSession
			newLoc, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if replaced := newLoc.Sandbox.PodUID != loc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("Resume replaced the pod: %t, want %t (pod %s, then %s)", replaced, tc.replaced, loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
			}
			if got := serviceAccountOr(g.pod(loc.Sandbox.Name).Spec.ServiceAccountName); got != controllerAccount {
				t.Fatalf("the controller runs in a pod on %s, want the controller's own %s", got, controllerAccount)
			}
		})
	}
}

// Where runtime.kubernetes.pod.controller_service_account names an account, the boot
// reconciliation warns of the controller's pod when it runs as any other — pod.service_account
// included, which every other pod is right to run as — naming the controller's account as the
// configured one, and of nothing when it runs as the controller's account. An issue pod on
// pod.service_account is on its own configured account and is warned of in neither case.
func TestTheBootReconciliationHoldsTheControllersPodToTheControllersOwnServiceAccount(t *testing.T) {
	for name, tc := range map[string]struct {
		runs   string // the account the controller's stored pod runs as
		warned bool
	}{
		"the controller's pod on pod.service_account": {otherAccount, true},
		"the controller's pod on its own account":     {controllerAccount, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			issue, controller := g.spawn(workerSpec(t)), g.spawn(controllerSpec(t))
			g.update(g.pod(issue.Sandbox.Name), onAccount(otherAccount))
			g.update(g.pod(controller.Sandbox.Name), onAccount(tc.runs))
			logs := &lockedLog{}
			r2 := secondRuntime(t, g, func(o *Options) {
				withControllerAccount(o)
				o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			})()
			known := []runtime.Known{{Claim: workerToken, Locator: &issue}, {Claim: controllerToken, Locator: &controller}}
			if err := r2.ReconcileOrphans(g.ctx, known, 0); err != nil {
				t.Fatal(err)
			}
			warnings := func(sandbox string) int {
				return strings.Count(logs.String(), "sandbox runtime: "+sandbox+" runs as ServiceAccount ")
			}
			want := "sandbox runtime: " + controller.Sandbox.Name + " runs as ServiceAccount " + otherAccount +
				", the configured ServiceAccount is " + controllerAccount + "; its agents hold that identity until the pod is replaced"
			switch {
			case tc.warned && (!strings.Contains(logs.String(), want) || warnings(controller.Sandbox.Name) != 1):
				t.Errorf("the boot reconciliation logged %q, want one warning %q", logs.String(), want)
			case !tc.warned && warnings(controller.Sandbox.Name) != 0:
				t.Errorf("the boot reconciliation warned of the controller's pod on its own account: %q", logs.String())
			}
			if warnings(issue.Sandbox.Name) != 0 {
				t.Errorf("the boot reconciliation warned of %s, whose pod runs as pod.service_account: %q", issue.Sandbox.Name, logs.String())
			}
		})
	}
}

// The boot reconciliation logs the held and configured leak-check manifests of a pod that was
// built under another setting. Without this warning an operator sees the round's drift reason
// only after the round has chosen the pod, rather than in the boot sweep that names every pod
// still running the old check.
func TestTheBootReconciliationWarnsOfEachPodRunningAnotherLeakCheckManifest(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	moved := leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
	logs := &lockedLog{}
	r2 := secondRuntime(t, g, func(o *Options) {
		o.LeakCheckSetting = moved
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})()

	if err := r2.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, 0); err != nil {
		t.Fatal(err)
	}
	want := "sandbox runtime: " + loc.Sandbox.Name + " runs leak check manifest " + testLeakCheck.ManifestSHA256 +
		", the configured manifest is " + moved.ManifestSHA256 + "; its agents keep the held check until its update round replaces the pod"
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Fatalf("the boot reconciliation logged %q matching leak-check warning(s), want one in %q", want, logs.String())
	}
}

// A Sandbox whose labels name no kind (podKindOf) is held to no account, since no kind's account
// is its own, and the sweep names it rather than adopting it. It is still warned of for every
// other drift its owned pod holds: the leak-check manifest its pod runs is read from the Sandbox's
// own record, whatever its labels say. So under a restart that moves both the account and the
// manifest, the boot reconciliation gives such a pod the leak-check warning and no account
// warning.
func TestTheBootReconciliationWarnsOfTheLeakCheckOfAPodWhoseLabelsNameNoKind(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	// The Sandbox keeps its pod and its record, and loses the label that names its kind.
	s := g.sandbox(loc.Sandbox.Name)
	if s == nil {
		t.Fatalf("sandbox %s is absent", loc.Sandbox.Name)
	}
	delete(s.Labels, labelIssue)
	if _, err := podKindOf(s.Labels); err == nil {
		t.Fatalf("labels %v still name a pod kind, want none", s.Labels)
	}
	u, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
		t.Fatal(err)
	}
	moved := leakguard.Setting{Repository: testLeakCheck.Repository, ManifestSHA256: strings.Repeat("7c", 32)}
	logs := &lockedLog{}
	r2 := secondRuntime(t, g, func(o *Options) {
		o.Pod.ServiceAccount = otherAccount
		o.LeakCheckSetting = moved
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})()

	// The sweep keeps a Sandbox whose labels name no kind, and says so.
	if err := r2.ReconcileOrphans(g.ctx, nil, 0); err == nil || !strings.Contains(err.Error(), loc.Sandbox.Name) {
		t.Fatalf("ReconcileOrphans = %v, want it to name %s, whose labels name no kind", err, loc.Sandbox.Name)
	}
	leak := "sandbox runtime: " + loc.Sandbox.Name + " runs leak check manifest " + testLeakCheck.ManifestSHA256 +
		", the configured manifest is " + moved.ManifestSHA256 + "; its agents keep the held check until its update round replaces the pod"
	if got := strings.Count(logs.String(), leak); got != 1 {
		t.Errorf("the boot reconciliation logged %d leak-check warnings %q, want one, in %q", got, leak, logs.String())
	}
	if account := "sandbox runtime: " + loc.Sandbox.Name + " runs as ServiceAccount "; strings.Contains(logs.String(), account) {
		t.Errorf("the boot reconciliation held %s, whose labels name no kind, to an account: %q", loc.Sandbox.Name, logs.String())
	}
}
