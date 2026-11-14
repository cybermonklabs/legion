package sandbox

import (
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The launch settings a pod template records and the drift read from them (drift.go's
// LaunchSettings, launchSettingsFor, launchSettingsDrift): what each kind of pod records of the
// keys its own launch reads, and which moves of them are drift of which pod. The pod shape's own
// drift, which compares what a pod is built from rather than what its launch read, is
// drift_test.go.

// launchFixture is a configuration whose every launch key is set, as a deployment's file settles
// them: what a pod's own launch reads, with a planted value for each key a record may never carry
// in the clear.
func launchFixture() *config.Config {
	return &config.Config{
		Project:          "LEGION",
		DaemonURL:        "http://legion-daemon.legion.svc:13370",
		AdvertiseHost:    "legion-daemon.legion.svc",
		OmpInvocation:    "omp",
		OmpLaunchPrefix:  []string{"/usr/bin/env"},
		InstructionsPath: "/etc/legion/instructions.md",
		ProviderKeys:     []config.ProviderKey{{Env: "ANTHROPIC_API_KEY", Secret: "fixture-anthropic-secret"}},
		RoleConfigFiles:  map[claim.Role][]string{claim.RoleTester: {"/etc/legion-operator/tester.yml"}},
		WorkerBackends:   map[claim.Role]runtime.Backend{claim.RoleTester: runtime.BackendOmp},
		Runtime: config.Runtime{
			Name: "kubernetes",
			Kubernetes: &config.Kubernetes{
				Namespace: "legion", Image: testImage, StorageClass: "standard", IssueVolume: "50Gi",
				SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "SESSION_DSN"},
				Setup:        []string{"/bin/sh", "-c", "echo setup"},
				TestPostgres: true,
				// The skills marketplace an issue pod's init container clones: a launch key of
				// an issue pod's own, set here as every other launch key of the fixture is, so
				// the move that proves its drift is one repository for another.
				SkillsRepository: ghrepo.MustParse("sjawhar/legion-skills"),
				Scheduling: config.Scheduling{
					NodeSelector:  map[string]string{"cloud.google.com/compute-class": "Performance"},
					Tolerations:   []config.Toleration{{Key: "legion", Operator: "Equal", Value: "true", Effect: "NoSchedule"}},
					PriorityClass: "legion-worker",
				},
				Resources: config.PodResources{
					Issue:      config.Reservation{CPU: "4", CPURequest: "4", Memory: "24Gi", MemoryRequest: "24Gi", EphemeralStorage: "80Gi", EphemeralStorageRequest: "6Gi"},
					Controller: config.Reservation{CPU: "2", CPURequest: "1", Memory: "8Gi", MemoryRequest: "4Gi", EphemeralStorage: "10Gi", EphemeralStorageRequest: "6Gi"},
				},
				Pod: config.PodConfig{
					Env:            map[string]string{"OPERATOR_SETTING": "fixture-operator-value"},
					Volumes:        []corev1.Volume{operatorVolume},
					VolumeMounts:   []corev1.VolumeMount{operatorMount},
					ServiceAccount: "legion-worker",
				},
				AgentSecrets: &config.AgentSecretsConfig{
					URL: "https://secrets.internal.example", Audience: "agent-secrets",
					TokenExpirySeconds: 3600, TokenFile: "/var/run/secrets/agent-secrets/fixture-token",
				},
				Plugins: &config.PluginsConfig{PiLegion: "10.4.0", PiEnvoy: "https://legion:fixture-plugin-password@plugins.internal.example/pi-envoy-10.4.0.tgz"},
			},
		},
	}
}

// The record a pod template carries holds exactly the launch keys a pod's own launch reads, which
// are the class table's: every row of class `launch` that is a value rather than a block and
// whose path is not a template or the tmux runtime's, which is a pane's launch and reaches no
// pod. The set is pinned here as well as derived, so a row added to the table is read by someone
// before every pod of the fleet is compared against it.
//
// A secret row is recorded as a digest of its value and never as itself: an annotation is
// readable by everything that may read the pod, and a rotation must be drift without the
// credential, its source or the path of its file reaching the cluster.
func TestLaunchSettingsHoldEveryLaunchKeyOfTheClassTable(t *testing.T) {
	recorded := LaunchSettings(launchFixture())
	settings, err := launchSettingsOf(recorded)
	if err != nil {
		t.Fatalf("the record cannot be read: %v", err)
	}
	want := []string{
		"advertise_host", "daemon_url", "instructions", "omp_invocation", "omp_launch_prefix",
		"provider_keys", "role_config_files",
		"runtime.kubernetes.agent_secrets.audience", "runtime.kubernetes.agent_secrets.token_expiry_seconds",
		"runtime.kubernetes.agent_secrets.token_file", "runtime.kubernetes.agent_secrets.url",
		"runtime.kubernetes.image", "runtime.kubernetes.issue_volume",
		"runtime.kubernetes.plugins.pi_envoy", "runtime.kubernetes.plugins.pi_legion",
		"runtime.kubernetes.pod.env", "runtime.kubernetes.pod.service_account",
		"runtime.kubernetes.pod.volume_mounts", "runtime.kubernetes.pod.volumes",
		"runtime.kubernetes.resources.controller", "runtime.kubernetes.resources.issue",
		"runtime.kubernetes.scheduling.node_selector", "runtime.kubernetes.scheduling.priority_class",
		"runtime.kubernetes.scheduling.tolerations",
		"runtime.kubernetes.session_store", "runtime.kubernetes.setup",
		"runtime.kubernetes.skills_repository", "runtime.kubernetes.storage_class",
		"runtime.kubernetes.test_postgres", "worker_backends",
	}
	if got := slices.Sorted(maps.Keys(settings)); !slices.Equal(got, want) {
		t.Errorf("the recorded launch keys are\n%q\nwant\n%q", got, want)
	}
	// Every launch row of the table the walk must reach: one entry per path it calls a value of a
	// pod's launch. A row it misses would be a key an operator changes with no pod replaced.
	for _, row := range config.Classes {
		switch {
		case row.Class != config.ClassLaunch || row.Block,
			strings.Contains(row.Path, "<") || strings.Contains(row.Path, "[]"),
			!podLaunchPath(row.Path):
			continue
		}
		if _, held := settings[row.Path]; !held {
			t.Errorf("the class table calls %s a launch key of a pod and the record holds none", row.Path)
		}
	}
	for _, planted := range []string{
		"fixture-anthropic-secret", "fixture-operator-value",
		"/var/run/secrets/agent-secrets/fixture-token", "fixture-plugin-password",
	} {
		if strings.Contains(recorded, planted) {
			t.Errorf("the record carries %q: %s", planted, recorded)
		}
	}
	if digest := settings["runtime.kubernetes.agent_secrets.token_file"]; !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("agent_secrets.token_file is recorded as %q, want a digest", digest)
	}
	if again := LaunchSettings(launchFixture()); again != recorded {
		t.Errorf("one configuration encodes differently twice:\n%s\n%s", recorded, again)
	}
}

// The guard this annotation exists for: a pod carrying today's annotations and no launch-settings
// one — every pod of the fleet, each built before this release — has drifted from nothing,
// whatever launch keys the daemon now reads. Its absence is read as "built before this release",
// exactly as a missing pod shape is, because the other answer is an update round over every open
// issue at once on the first boot of the new build.
func TestPodDriftReadsAPodThatRecordsNoLaunchSettingsAsUndrifted(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	annotations := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations
	if _, recorded := annotations[launchSettingsAnnotation]; recorded {
		t.Fatalf("the pod template records %s: this case is the pod of a release that recorded none", launchSettingsAnnotation)
	}
	if annotations[podShapeAnnotation] == "" {
		t.Fatalf("the pod template records no %s, so this proves nothing of a pod carrying today's annotations", podShapeAnnotation)
	}

	moved := launchFixture()
	moved.InstructionsPath = "/etc/legion/instructions-2.md"
	r2 := secondRuntime(t, g, func(o *Options) { o.LaunchSettings = LaunchSettings(moved) })()
	if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) > 0 {
		t.Fatalf("PodDrift = %q, want nothing of a pod that records no launch settings", drifted)
	}
}

// A pod whose launch keys have moved since its template recorded them has drifted, and the reason
// names the key, what the pod's launch read and what a launch now reads. A secret key names
// itself and no value either way.
func TestPodDriftNamesTheLaunchKeyThatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		move func(*config.Config)
		says string
	}{
		"the same configuration": {func(*config.Config) {}, ""},
		"the instructions": {
			func(cfg *config.Config) { cfg.InstructionsPath = "/etc/legion/instructions-2.md" },
			"launch setting instructions /etc/legion/instructions.md, now /etc/legion/instructions-2.md",
		},
		"a role's overlays": {
			func(cfg *config.Config) {
				cfg.RoleConfigFiles[claim.RoleTester] = []string{"/etc/legion-operator/tester-2.yml"}
			},
			`launch setting role_config_files {"tester":["/etc/legion-operator/tester.yml"]}, now {"tester":["/etc/legion-operator/tester-2.yml"]}`,
		},
		"a rotated provider key": {
			func(cfg *config.Config) { cfg.ProviderKeys[0].Secret = "fixture-anthropic-secret-2" },
			"launch setting provider_keys " + withheldLaunchValue,
		},
		"the pod's service account": {
			func(cfg *config.Config) { cfg.Runtime.Kubernetes.Pod.ServiceAccount = "legion-worker-2" },
			"launch setting runtime.kubernetes.pod.service_account legion-worker, now legion-worker-2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			built := LaunchSettings(launchFixture())
			g := newRig(t, nil, withOptions(func(o *Options) { o.LaunchSettings = built }))
			loc := g.spawn(workerSpec(t))
			// An issue pod records its own kind's launch keys (launchSettingsFor).
			if recorded := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Metadata.Annotations[launchSettingsAnnotation]; recorded != launchSettingsFor(built, issuePod{}) {
				t.Fatalf("the pod template records %q, want the launch settings its launch read", recorded)
			}

			moved := launchFixture()
			tc.move(moved)
			r2 := secondRuntime(t, g, func(o *Options) { o.LaunchSettings = LaunchSettings(moved) })()
			drifted := driftOf(t, g.ctx, r2, loc)
			if tc.says == "" {
				if len(drifted) > 0 {
					t.Fatalf("PodDrift = %q, want nothing: no launch key moved", drifted)
				}
				return
			}
			if !slices.Equal(drifted, []string{tc.says}) {
				t.Fatalf("PodDrift = %q, want exactly one reason, %q", drifted, tc.says)
			}
		})
	}
}

// A record neither side can read is one reason naming it, and never a silent no-drift: an
// operator who patched an annotation by hand reads why the daemon will not compare it. A runtime
// that records no launch settings of its own compares nothing, as a pod that records none is
// compared against nothing.
func TestLaunchSettingsDriftReadsAnUnreadableAndAnAbsentRecord(t *testing.T) {
	built := LaunchSettings(launchFixture())
	if drifted := launchSettingsDrift("{not json", built); len(drifted) != 1 || !strings.Contains(drifted[0], "cannot be read") {
		t.Errorf("launchSettingsDrift of an unreadable record = %q, want one reason naming it", drifted)
	}
	if drifted := launchSettingsDrift("", built); drifted != nil {
		t.Errorf("launchSettingsDrift of a pod that records none = %q, want nothing", drifted)
	}
	if drifted := launchSettingsDrift(built, ""); drifted != nil {
		t.Errorf("launchSettingsDrift against a runtime that records none = %q, want nothing", drifted)
	}
	if drifted := launchSettingsDrift(built, "{not json either"); drifted != nil {
		t.Errorf("launchSettingsDrift against a record a launch cannot read = %q, want nothing", drifted)
	}
	if drifted := launchSettingsDrift(built, built); drifted != nil {
		t.Errorf("launchSettingsDrift of one record against itself = %q, want nothing", drifted)
	}
	if drifted := launchSettingsDrift(`{"instructions":"/etc/legion/instructions.md"}`, `{}`); !slices.Equal(drifted, []string{"launch setting instructions /etc/legion/instructions.md, now " + config.UnsetValue}) {
		t.Errorf("launchSettingsDrift of a key the configuration dropped = %q, want it named as unset", drifted)
	}
}

// Each kind of pod records the launch keys its own launch reads and no other kind's: the record
// an issue pod carries holds every key only an issue pod's launch reads (issueLaunchPaths) and
// none of the controller pod's own (controllerLaunchPaths), and the controller pod's the other
// way round. So a change of the controller's reservation is no drift of an issue pod, and no
// update round is opened over every issue pod of the fleet for a key none of their launches read
// (spec A5: a round opens for each issue pod whose launch reads the key and for no other).
func TestEachKindRecordsOnlyTheLaunchKeysItsOwnLaunchReads(t *testing.T) {
	built := LaunchSettings(launchFixture())
	moved := launchFixture()
	moved.Runtime.Kubernetes.Resources.Controller.CPURequest = "2"
	shifted := LaunchSettings(moved)
	if built == shifted {
		t.Fatal("the whole record did not move with the controller's reservation, so this proves nothing")
	}
	for kind, paths := range map[podKind]struct{ own, other []string }{
		issuePod{}:      {own: issueLaunchPaths, other: controllerLaunchPaths},
		controllerPod{}: {own: controllerLaunchPaths, other: issueLaunchPaths},
	} {
		record, err := launchSettingsOf(launchSettingsFor(built, kind))
		if err != nil {
			t.Fatalf("the record of %T cannot be read: %v", kind, err)
		}
		for _, path := range paths.own {
			if _, held := record[path]; !held {
				t.Errorf("the record of %T holds no %s, which its own launch reads", kind, path)
			}
		}
		for _, path := range paths.other {
			if _, held := record[path]; held {
				t.Errorf("the record of %T holds %s, which no launch of its reads", kind, path)
			}
		}
	}
	// The issue pod's own record does not move with the controller's reservation, so no issue
	// pod has drifted; the controller pod's does, and its own next launch replaces it.
	if before, after := launchSettingsFor(built, issuePod{}), launchSettingsFor(shifted, issuePod{}); before != after {
		t.Errorf("a controller-only change moved the issue pod's record:\n%s\n%s", before, after)
	}
	if drifted := launchSettingsDrift(launchSettingsFor(built, controllerPod{}), launchSettingsFor(shifted, controllerPod{})); len(drifted) != 1 ||
		!strings.Contains(drifted[0], "runtime.kubernetes.resources.controller") {
		t.Errorf("the controller pod's drift = %q, want its own reservation named", drifted)
	}
}

// movedIssueLaunchKeys is the whole launch record as it reads after one launch key only an issue
// pod's launch reads has moved (podkind.go's issueLaunchPaths), by path: what an operator who
// changes that one key leaves for the next pod's template to record.
var movedIssueLaunchKeys = map[string]func() string{
	"runtime.kubernetes.resources.issue": movedBy(func(cfg *config.Config) {
		cfg.Runtime.Kubernetes.Resources.Issue.CPURequest = "2"
	}),
	"runtime.kubernetes.skills_repository": movedBy(func(cfg *config.Config) {
		cfg.Runtime.Kubernetes.SkillsRepository = ghrepo.MustParse("sjawhar/other-skills")
	}),
	"runtime.kubernetes.test_postgres": movedBy(func(cfg *config.Config) {
		cfg.Runtime.Kubernetes.TestPostgres = false
	}),
	"worker_backends": movedBy(func(cfg *config.Config) {
		cfg.WorkerBackends[claim.RoleTester] = runtime.BackendClaude
	}),
}

// movedBy is the record of the launch fixture with move applied to it.
func movedBy(move func(*config.Config)) func() string {
	return func() string {
		cfg := launchFixture()
		move(cfg)
		return LaunchSettings(cfg)
	}
}

// A launch key only an issue pod's launch reads is drift of every issue pod and of no controller
// pod: the skills repository, which the controller's pod clones in no init container and mounts
// in no container (controllerPod.skillsRepository, controllerPod.initContainers); the pod's own
// test database, which no container of the controller's serves (issuePod.mainVolumes); the issue
// reservation, which no container of the controller's carries; and the workers' backends, which
// name issue roles alone. Were one of them compared against the controller's record, its next
// launch would replace a pod nothing about it had changed — the spurious round launchSettingsFor
// exists to refuse, in the one direction its own comment did not cover.
func TestAnIssueOnlyLaunchKeyDriftsTheIssuePodAndNotTheController(t *testing.T) {
	built := LaunchSettings(launchFixture())
	recorded, err := launchSettingsOf(built)
	if err != nil {
		t.Fatalf("the record cannot be read: %v", err)
	}
	for _, path := range slices.Concat(issueLaunchPaths, controllerLaunchPaths) {
		if _, held := recorded[path]; !held {
			t.Fatalf("%s is named as a key one kind's launch alone reads and no pod records it: the name has moved or is misspelt", path)
		}
	}
	for _, path := range issueLaunchPaths {
		move, named := movedIssueLaunchKeys[path]
		if !named {
			t.Fatalf("%s is a launch key an issue pod's launch alone reads and this test moves no such key", path)
		}
		t.Run(path, func(t *testing.T) {
			shifted := move()
			if shifted == built {
				t.Fatalf("the whole record did not move with %s, so this proves nothing", path)
			}
			drifted := launchSettingsDrift(launchSettingsFor(built, issuePod{}), launchSettingsFor(shifted, issuePod{}))
			if len(drifted) != 1 || !strings.Contains(drifted[0], path) {
				t.Errorf("the issue pod's drift = %q, want the one reason naming %s", drifted, path)
			}
			before, after := launchSettingsFor(built, controllerPod{}), launchSettingsFor(shifted, controllerPod{})
			if before != after {
				t.Errorf("%s moved the controller pod's record, which no launch of its reads:\n%s\n%s", path, before, after)
			}
			if drifted := launchSettingsDrift(before, after); len(drifted) > 0 {
				t.Errorf("the controller pod's drift = %q, want nothing: no launch of its reads %s", drifted, path)
			}
		})
	}
}

// A launch key both kinds' launches read is drift of both pods, which is the other half of the
// same rule: the filter takes a key from a pod's record only where that pod's own launch does not
// read it, so a key both read reaches both, and the issue pod's round and the controller pod's
// own next launch each carry the change. The image every container runs, the setup command that
// wraps every role's start (launcherCommand), the issue volume's size, which sizes the issue
// pod's own ephemeral claim and the controller Sandbox's template alike (diskSpec), and the
// deployment instructions in every agent's system prompt are four such keys.
func TestALaunchKeyBothKindsReadDriftsBothPods(t *testing.T) {
	built := LaunchSettings(launchFixture())
	for path, move := range map[string]func(*config.Config){
		"runtime.kubernetes.image":        func(cfg *config.Config) { cfg.Runtime.Kubernetes.Image = nextImage },
		"runtime.kubernetes.setup":        func(cfg *config.Config) { cfg.Runtime.Kubernetes.Setup = []string{"/bin/sh", "-c", "echo setup 2"} },
		"runtime.kubernetes.issue_volume": func(cfg *config.Config) { cfg.Runtime.Kubernetes.IssueVolume = "80Gi" },
		"instructions":                    func(cfg *config.Config) { cfg.InstructionsPath = "/etc/legion/instructions-2.md" },
	} {
		t.Run(path, func(t *testing.T) {
			moved := launchFixture()
			move(moved)
			shifted := LaunchSettings(moved)
			if shifted == built {
				t.Fatalf("the whole record did not move with %s, so this proves nothing", path)
			}
			for _, kind := range []podKind{issuePod{}, controllerPod{}} {
				drifted := launchSettingsDrift(launchSettingsFor(built, kind), launchSettingsFor(shifted, kind))
				if len(drifted) != 1 || !strings.Contains(drifted[0], path) {
					t.Errorf("the drift of %T = %q, want the one reason naming %s, which its launch reads", kind, drifted, path)
				}
			}
		})
	}
}
