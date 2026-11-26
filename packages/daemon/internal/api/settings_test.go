package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// settingsConfigTypes is config.Config and every struct type of its package reachable from it
// through exported fields, pointers, slices, arrays and map keys and elements, each once: the
// types whose exported fields TestSettingsOfClassifiesEveryConfigField holds to settingsEmitted and
// settingsExcluded. Because the set is derived, a nested struct a new field introduces is read
// field by field before the build is green, with no list to extend. A type of another package (a
// Kubernetes API type, time.Duration, a role, a backend, a compiled regexp) is a leaf: its fields
// are no legion.yaml keys.
func settingsConfigTypes() []reflect.Type {
	root := reflect.TypeFor[config.Config]()
	var types []reflect.Type
	var walk func(typ reflect.Type)
	walk = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		case reflect.Map:
			walk(typ.Key())
			walk(typ.Elem())
		case reflect.Struct:
			if typ.PkgPath() != root.PkgPath() || slices.Contains(types, typ) {
				return
			}
			types = append(types, typ)
			for i := range typ.NumField() {
				if field := typ.Field(i); field.IsExported() {
					walk(field.Type)
				}
			}
		}
	}
	walk(root)
	return types
}

// settingsEmitted is every config field SettingsOf emits, by `<Type>.<Field>`, and the settings
// key that carries it.
var settingsEmitted = map[string]string{
	"Config.Project":                                 "project",
	"Config.Port":                                    "port",
	"Config.Bind":                                    "bind",
	"Config.AdvertiseHost":                           "advertise_host",
	"Config.StateDir":                                "state_dir",
	"Config.Runtime":                                 "runtime",
	"Config.AdmissionCap":                            "admission_cap",
	"Config.AdmissionP0Bound":                        "admission_p0_bound",
	"Config.DaemonURL":                               "daemon_url",
	"Config.WorkerStreamPort":                        "worker_stream_port",
	"Config.OmpInvocation":                           "omp_invocation",
	"Config.OmpLaunchPrefix":                         "omp_launch_prefix",
	"Config.ProviderKeys":                            "provider_keys (the variable names alone)",
	"Config.InstructionsPath":                        "instructions",
	"Config.PrivateNameRulesFile":                    "private_name_rules_file (the path alone, never the rules)",
	"Config.WorkerBootTimeout":                       "worker_boot_timeout_seconds",
	"Config.WorkerRPCTimeout":                        "worker_rpc_timeout_seconds",
	"Config.WorkerStopTimeout":                       "worker_stop_timeout_seconds",
	"Config.TreeStopTimeout":                         "tree_stop_timeout_seconds",
	"Config.SlowCommandTimeout":                      "slow_command_timeout_seconds",
	"Config.ProbeInterval":                           "probe_interval_seconds",
	"Config.ControllerWakeInterval":                  "controller_wake_interval_seconds",
	"Config.WorkerBootRegistrationDeadlineIntervals": "worker_boot_registration_deadline_intervals",
	"Config.LaunchFailureLimit":                      "launch_failure_limit",
	"Config.PromptFailureLimit":                      "prompt_failure_limit",
	"Config.PromptRetireLimit":                       "prompt_retire_limit",
	"Config.WorkerWakesPerMinute":                    "worker_wakes_per_minute",
	"Config.WorkerWakeBurst":                         "worker_wake_burst",
	"Config.EnvoyURL":                                "envoy_url",
	"Config.NatsURLs":                                "nats_urls",
	"Config.DispatchURL":                             "dispatch_url",
	"Config.DispatchTokenBroker":                     "dispatch_token_broker (absent while false)",
	"Config.Projects":                                "projects",
	"Config.Gates":                                   "gates",
	"Config.GitHubApps":                              "github_apps",
	"Config.Linger":                                  "linger_hours",
	"Config.ProductionCheckBound":                    "production_check_hours",
	"Config.PodUpdateGrace":                          "pod_update_grace_minutes",
	"Config.StalledTreeNotice":                       "stalled_tree_notice_hours",
	"Config.WorkerSilence":                           "worker_silence_minutes",
	"Config.WorkerBackground":                        "worker_background_hours",
	"Config.PodUpdateMaxOpenRounds":                  "pod_update_max_open_rounds",
	"Config.PodUpdateRoundsPerMinute":                "pod_update_rounds_per_minute",
	"Config.ReviewRoundCap":                          "review_round_cap",
	"Config.ControllerLaunch":                        "controller",
	"Config.ControllerTurnBound":                     "controller_turn_bound_minutes",
	"Config.ControllerSessionMaxAge":                 "controller_session_max_age_hours",
	"Config.Capabilities":                            "capabilities",
	"Config.WorkerBackends":                          "worker_backends",
	"Config.RoleConfigFiles":                         "role_config_files",
	"Config.WorkerStallBackstops":                    "worker_stall_backstop_hours",
	"Config.RepositoryRenames":                       "repository_renames",
	"Runtime.Name":                                   "runtime.name",
	"Runtime.Kubernetes":                             "runtime.kubernetes",
	"Runtime.Tmux":                                   "runtime.tmux",
	"Tmux.PaneEnv":                                   "runtime.tmux.pane_env (the variable names alone)",
	"Tmux.PiConfigFiles":                             "runtime.tmux.pi_config_files",
	"Tmux.TakeOver":                                  "runtime.tmux.take_over",
	"Kubernetes.Namespace":                           "runtime.kubernetes.namespace",
	"Kubernetes.Image":                               "runtime.kubernetes.image",
	"Kubernetes.StorageClass":                        "runtime.kubernetes.storage_class",
	"Kubernetes.IssueVolume":                         "runtime.kubernetes.issue_volume",
	"Kubernetes.Scheduling":                          "runtime.kubernetes.scheduling",
	"Kubernetes.Resources":                           "runtime.kubernetes.resources",
	"Kubernetes.AgentSecrets":                        "runtime.kubernetes.agent_secrets",
	"Kubernetes.SessionStore":                        "runtime.kubernetes.session_store (the kind alone: neither the Secret key nor the instance and login behind it is emitted)",
	"Kubernetes.Plugins":                             "runtime.kubernetes.plugins",
	"Kubernetes.Setup":                               "runtime.kubernetes.setup",
	"Kubernetes.TestPostgres":                        "runtime.kubernetes.test_postgres",
	"Kubernetes.RepositoryCache":                     "runtime.kubernetes.repository_cache",
	"Kubernetes.IdleRelease":                         "runtime.kubernetes.idle_release",
	"IdleReleaseConfig.IdleMinutes":                  "runtime.kubernetes.idle_release.idle_minutes",
	"IdleReleaseConfig.BackstopMinutes":              "runtime.kubernetes.idle_release.backstop_minutes",
	"Kubernetes.Sandbox":                             "runtime.kubernetes.sandbox",
	"SandboxConfig.Type":                             "runtime.kubernetes.sandbox.type",
	"SandboxConfig.CanaryIssues":                     "runtime.kubernetes.sandbox.canary_issues",
	"SandboxConfig.MicroVM":                          "runtime.kubernetes.sandbox.microvm",
	"MicroVMConfig.ComputeClass":                     "runtime.kubernetes.sandbox.microvm.compute_class",
	"MicroVMConfig.StorageClass":                     "runtime.kubernetes.sandbox.microvm.storage_class (runtime.kubernetes.storage_class when the block names none)",
	"MicroVMConfig.KeepVolume":                       "runtime.kubernetes.sandbox.microvm.keep_volume",
	"MicroVMConfig.Metadata":                         "runtime.kubernetes.sandbox.microvm.metadata",
	"MetadataConfig.WorkloadPool":                    "runtime.kubernetes.sandbox.microvm.metadata.workload_pool",
	"MetadataConfig.IdentityProvider":                "runtime.kubernetes.sandbox.microvm.metadata.identity_provider",
	"Kubernetes.MemoryGuard":                         "runtime.kubernetes.memory_guard",
	"MemoryGuardConfig.IssuePods":                    "runtime.kubernetes.memory_guard.issue_pods",
	"MemoryGuardConfig.ControllerPod":                "runtime.kubernetes.memory_guard.controller_pod",
	"MemoryGuardPodConfig.Enabled":                   "runtime.kubernetes.memory_guard.<pod>.enabled",
	"MemoryGuardPodConfig.ActPercent":                "runtime.kubernetes.memory_guard.<pod>.act_percent",
	"MemoryGuardPodConfig.FloorPercent":              "runtime.kubernetes.memory_guard.<pod>.floor_percent",
	"MemoryGuardPodConfig.WaitSeconds":               "runtime.kubernetes.memory_guard.<pod>.wait_seconds",
	"MemoryGuardPodConfig.IntervalMilliseconds":      "runtime.kubernetes.memory_guard.<pod>.interval_milliseconds",
	"Kubernetes.VolumeTrim":                          "runtime.kubernetes.volume_trim",
	"VolumeTrimConfig.Enabled":                       "runtime.kubernetes.volume_trim.enabled",
	"VolumeTrimConfig.HighPercent":                   "runtime.kubernetes.volume_trim.high_percent",
	"VolumeTrimConfig.LowPercent":                    "runtime.kubernetes.volume_trim.low_percent",
	"VolumeTrimConfig.MinAgeMinutes":                 "runtime.kubernetes.volume_trim.min_age_minutes",
	"VolumeTrimConfig.IntervalSeconds":               "runtime.kubernetes.volume_trim.interval_seconds",
	"RepositoryCacheConfig.Volume":                   "runtime.kubernetes.repository_cache.volume",
	"RepositoryCacheConfig.RefreshMinutes":           "runtime.kubernetes.repository_cache.refresh_minutes",
	"RepositoryCacheConfig.RetainHours":              "runtime.kubernetes.repository_cache.retain_hours",
	"RepositoryCacheConfig.Resources":                "runtime.kubernetes.repository_cache.resources",
	"Scheduling.NodeSelector":                        "runtime.kubernetes.scheduling.node_selector",
	"Scheduling.Tolerations":                         "runtime.kubernetes.scheduling.tolerations",
	"Scheduling.PriorityClass":                       "runtime.kubernetes.scheduling.priority_class",
	"Toleration.Key":                                 "runtime.kubernetes.scheduling.tolerations[].key",
	"Toleration.Operator":                            "runtime.kubernetes.scheduling.tolerations[].operator",
	"Toleration.Value":                               "runtime.kubernetes.scheduling.tolerations[].value",
	"Toleration.Effect":                              "runtime.kubernetes.scheduling.tolerations[].effect",
	"PodResources.Issue":                             "runtime.kubernetes.resources.issue",
	"PodResources.Controller":                        "runtime.kubernetes.resources.controller",
	"Reservation.CPU":                                "runtime.kubernetes.resources.<pod>.cpu",
	"Reservation.CPURequest":                         "runtime.kubernetes.resources.<pod>.cpu_request",
	"Reservation.Memory":                             "runtime.kubernetes.resources.<pod>.memory",
	"Reservation.MemoryRequest":                      "runtime.kubernetes.resources.<pod>.memory_request",
	"Reservation.EphemeralStorage":                   "runtime.kubernetes.resources.<pod>.ephemeral_storage",
	"Reservation.EphemeralStorageRequest":            "runtime.kubernetes.resources.<pod>.ephemeral_storage_request",
	"AgentSecretsConfig.URL":                         "runtime.kubernetes.agent_secrets.url",
	"AgentSecretsConfig.Audience":                    "runtime.kubernetes.agent_secrets.audience",
	"AgentSecretsConfig.TokenExpirySeconds":          "runtime.kubernetes.agent_secrets.token_expiry_seconds",
	"AgentSecretsConfig.ExchangeAudience":            "runtime.kubernetes.agent_secrets.exchange_audience",
	"PluginsConfig.PiLegion":                         "runtime.kubernetes.plugins.pi_legion",
	"PluginsConfig.PiEnvoy":                          "runtime.kubernetes.plugins.pi_envoy",
	"ProviderKey.Env":                                "provider_keys[]",
	"Project.SlackChannels":                          "projects.<KEY>.slack_channels",
	"Project.Memory":                                 "projects.<KEY>.memory",
	"Project.ProductionCheck":                        "projects.<KEY>.production_check",
	"Project.DefaultSteps":                           "projects.<KEY>.default_steps",
	"Memory.Mode":                                    "projects.<KEY>.memory.mode",
	"Memory.ConfigFiles":                             "projects.<KEY>.memory.config_files",
	"Memory.InstructionsPath":                        "projects.<KEY>.memory.instructions",
	"Project.Experiments":                            "projects.<KEY>.experiments",
	"Experiment.Name":                                "projects.<KEY>.experiments.<name> (the mapping key the block is declared under)",
	"Experiment.Mode":                                "projects.<KEY>.experiments.<name>.mode",
	"Experiment.Unit":                                "projects.<KEY>.experiments.<name>.unit",
	"Experiment.Roles":                               "projects.<KEY>.experiments.<name>.roles",
	"Experiment.OnVariant":                           "projects.<KEY>.experiments.<name>.on_variant",
	"Experiment.Variants":                            "projects.<KEY>.experiments.<name>.variants",
	"Variant.Name":                                   "projects.<KEY>.experiments.<name>.variants[].name",
	"Variant.Weight":                                 "projects.<KEY>.experiments.<name>.variants[].weight",
	"Variant.ConfigFiles":                            "projects.<KEY>.experiments.<name>.variants[].config_files",
	"Variant.InstructionsPath":                       "projects.<KEY>.experiments.<name>.variants[].instructions",
	"Variant.Plan":                                   "projects.<KEY>.experiments.<name>.variants[].plan",
	"Gates.Design":                                   "gates.design",
	"GitHubApps.Implement":                           "github_apps.implement",
	"GitHubApps.Review":                              "github_apps.review",
	"GitHubApp.AppID":                                "github_apps.<app>.app_id",
	"GitHubApp.Installations":                        "github_apps.<app>.installations",
	"Capabilities.Decided":                           "capabilities.decided",
	"Config.Hooks":                                   "hooks",
	"Hook.Name":                                      "hooks[].name",
	"Hook.On":                                        "hooks[].on",
	"Hook.Moves":                                     "hooks[].moves (`any` for a hook that selects every move back)",
	"Hook.Every":                                     "hooks[].every",
	"Hook.Roles":                                     "hooks[].roles",
	"Hook.PromptFile":                                "hooks[].prompt_file (the path alone, never the words the file holds)",
}

// settingsExcluded is every config field SettingsOf leaves out, by `<Type>.<Field>`, and why: a
// credential, the source of one, the path of a file holding one, the operator's pod pieces, a
// deployment's own repositories, or a derived value that is no setting.
var settingsExcluded = map[string]string{
	"Config.PostgresDSN":                 "a credential: a DSN carries its password",
	"GitHubApp.PrivateKey":               "a credential: Config holds the PEM resolved whatever the file's source",
	"GitHubApp.PrivateKeyCommand":        "the source of a credential",
	"GitHubApp.PrivateKeySecret":         "the source of a credential",
	"GitHubApp.TokenCommand":             "the source of a credential: the command that prints the App's installation tokens",
	"ProviderKey.Secret":                 "the source of a credential: a secretsd key or a providers Secret key",
	"ProviderKey.File":                   "the path of a file holding a credential",
	"Config.OperatorTokenFile":           "the path of a file holding a credential",
	"Config.EnvoyTokenFile":              "the path of a file holding a credential",
	"Config.NatsNkeySeedFile":            "the path of a file holding a credential",
	"Config.NatsDaemonNkeySeedFile":      "the path of a file holding a credential",
	"Config.DispatchTokenFile":           "the path of a file holding a credential",
	"Config.DispatchTokenTransitionFile": "the path of a file holding a credential: the shared Dispatch token a launch into a pod built before dispatch_token_broker gets",
	"AgentSecretsConfig.TokenFile":       "the path of a file holding a credential: the daemon's own broker login token",
	"Kubernetes.Kubeconfig":              "the daemon's cluster credential file",
	"Kubernetes.Context":                 "the context inside the daemon's cluster credential file",
	"Kubernetes.Pod":                     "not a daemon setting: the operator's pod pieces, which every pod reads from its own spec and whose lists are Kubernetes API types with no legion.yaml-shaped rendering",
	"PodConfig.Env":                      "runtime.kubernetes.pod: the operator's free text, delivered to every pod's spec",
	"PodConfig.Volumes":                  "runtime.kubernetes.pod: Kubernetes API types naming the operator's Secrets, ConfigMaps, claims and NFS exports",
	"PodConfig.VolumeMounts":             "runtime.kubernetes.pod: Kubernetes API types",
	"PodConfig.ServiceAccount":           "runtime.kubernetes.pod: the operator's ServiceAccount, which every pod runs as and reads from its own spec",
	"PodConfig.ControllerServiceAccount": "runtime.kubernetes.pod: the operator's ServiceAccount the controller's pod runs as in place of service_account, which that pod reads from its own spec",
	"Kubernetes.PluginsFromBinary":       "derived: where the settled plugins pair came from, not a setting",
	"Config.Warnings":                    "not a setting: the loader's own lines for the operator about a key the file still sets and this daemon no longer reads, the warned keys among them (config.warnedKeys), which `legion start` logs and `legion config check` prints",
	"Project.CommitIssueTrailerRepos":    "a deployment's own repositories, named by a route that authenticates nobody (GET /legion/v1/state)",
	"Config.LeakCheck":                   "a deployment's own repository, the one its shared leak check is published from, named by a route that authenticates nobody (GET /legion/v1/state); every role is told the checker beside it by the runtime, and needs no route for it",
	"Config.StallLimits":                 "derived: the three LEGION_STALL_* numbers this daemon's own environment sets, which it passes to every session; no legion.yaml key names them",
	"Config.PrivateNameRulesNamedBy":     "derived: which of private_name_rules_file and LEGION_PRIVATE_NAME_RULES_FILE named the rule file, which a refusal of the file at boot names; the settings carry the path under private_name_rules_file either way",
	"StallLimits.CallBound":              "derived: LEGION_STALL_CALL_BOUND_MS in the daemon's environment, not a legion.yaml key",
	"StallLimits.CallGrace":              "derived: LEGION_STALL_CALL_GRACE_MS in the daemon's environment, not a legion.yaml key",
	"StallLimits.Step":                   "derived: LEGION_STALL_STEP_MS in the daemon's environment, not a legion.yaml key",
	"Hook.Prompt":                        "the words a hook puts in front of its roles, which the deployment writes for its own agents and no pod needs to read from a route that authenticates nobody; `hooks[].prompt_file` carries the path of the file the other form holds them in, as `instructions` carries a path and never its text",
	"Variant.ConfigFilesField":           "derived: the key an operator wrote the arm's `config_files` under, built by config.Project.ExperimentsDeclared from the project key and the names beside it, which the settings already carry",
	"Variant.MemoryBlock":                "derived: whether the arm is the `memory` block's rather than the `experiments` key's, set by config.Project.ExperimentsDeclared, which the settings already tell by carrying the block under `memory` and the key under `experiments`",
	"Config.Revision":                    "not a setting: the commit this binary was built from, which `legion start` sets from cmd/legion's link-time revision and no legion.yaml key names",
	"Config.GitHubGraphQLShare":          "not shown yet: the pace of the daemon's own GraphQL reads of GitHub, which no pod or plugin reads or acts on; carrying it on GET /legion/v1/state is a change of the daemon API contract (answers.go, packages/contracts) that the pull request adding the key does not make",
}

// Every exported field of every struct the walk reaches (settingsConfigTypes) is named in exactly
// one of the two tables, and every table row names a field the walk reaches: a new field, on a
// type the walk already reads or on a nested type the field itself introduces, fails here until
// someone decides whether an unauthenticated route may carry it.
func TestSettingsOfClassifiesEveryConfigField(t *testing.T) {
	seen := map[string]bool{}
	for _, typ := range settingsConfigTypes() {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			path := typ.Name() + "." + field.Name
			seen[path] = true
			_, emitted := settingsEmitted[path]
			_, excluded := settingsExcluded[path]
			switch {
			case emitted && excluded:
				t.Errorf("%s is in both settingsEmitted and settingsExcluded", path)
			case !emitted && !excluded:
				t.Errorf("%s is in neither settingsEmitted nor settingsExcluded: decide whether every pod may read it, put it in one table, and have SettingsOf agree", path)
			}
		}
	}
	for name, table := range map[string]map[string]string{"settingsEmitted": settingsEmitted, "settingsExcluded": settingsExcluded} {
		for path := range table {
			if !seen[path] {
				t.Errorf("%s names %s, which is no exported field of a struct reachable from config.Config", name, path)
			}
		}
	}
}

// The tmux block shows the runtime whose claims the daemon takes over, and leaves the key out when
// the file names none.
func TestTheTmuxSettingsShowTheTakeOver(t *testing.T) {
	for _, tc := range []struct {
		block *config.Tmux
		want  string
	}{
		{&config.Tmux{TakeOver: config.TakeOverKubernetes}, `{"pane_env":[],"pi_config_files":[],"take_over":"kubernetes"}`},
		{&config.Tmux{}, `{"pane_env":[],"pi_config_files":[]}`},
	} {
		encoded, err := json.Marshal(tmuxSettingsOf(tc.block))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != tc.want {
			t.Errorf("tmuxSettingsOf(%+v) = %s, want %s", tc.block, encoded, tc.want)
		}
	}
}

// The memory_guard block shows each pod kind's own settled block under its own key: different
// numbers in the two come out each under its kind, and one kind off leaves the other on.
func TestTheMemoryGuardSettingsShowEachPodKindsOwnBlock(t *testing.T) {
	encoded, err := json.Marshal(kubernetesSettingsOf(&config.Kubernetes{MemoryGuard: config.MemoryGuardConfig{
		IssuePods:     config.MemoryGuardPodConfig{Enabled: true, ActPercent: 80, FloorPercent: 15, WaitSeconds: 8, IntervalMilliseconds: 250},
		ControllerPod: config.MemoryGuardPodConfig{Enabled: false, ActPercent: 70, FloorPercent: 20, WaitSeconds: 9, IntervalMilliseconds: 400},
	}}).MemoryGuard)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"issue_pods":{"enabled":true,"act_percent":80,"floor_percent":15,"wait_seconds":8,"interval_milliseconds":250},` +
		`"controller_pod":{"enabled":false,"act_percent":70,"floor_percent":20,"wait_seconds":9,"interval_milliseconds":400}}`
	if string(encoded) != want {
		t.Errorf("settings.runtime.kubernetes.memory_guard = %s, want %s", encoded, want)
	}
}

// settingsFixturePlanted is every value of settingsFixtureFile that is a credential, the source of
// one, the path of a file holding one, the userinfo of an address, or an operator pod piece: each
// is in the file (so the list cannot go stale) and none is in the settings' bytes.
var settingsFixturePlanted = []string{
	"BEGIN", "PRIVATE KEY",
	"postgres://", "fixture-dsn-password",
	"legion:fixture-", "fixture-envoy-password", "fixture-nats-password",
	"legion:fixture-plugin-", "fixture-plugin-password", "fixture-plugin-token",
	"/var/run/legion/OPERATOR_TOKEN", "/var/run/legion/ENVOY_TOKEN", "/var/run/legion/NATS_NKEY_SEED",
	"/var/run/legion/NATS_DAEMON_NKEY_SEED", "/var/run/legion/DISPATCH_TOKEN", "/var/run/secrets/agent-secrets/token",
	"/etc/legion/kubeconfig", "legion-daemon@fixture-cluster",
	"SESSION_DSN_FIXTURE",
	"ANTHROPIC_API_KEY_FIXTURE", "GEMINI_API_KEY_FIXTURE",
	"/etc/legion-operator/overlay.yml", "operator-fixture-value",
	"operator-creds", "fixture-operator-creds", "/etc/legion-operator/creds", "fixture-operator-account",
}

// settingsForbiddenKeys are the legion.yaml keys that never appear in `settings`, at any depth.
var settingsForbiddenKeys = []string{
	"postgres_dsn", "private_key", "private_key_command", "private_key_secret", "token_command",
	"kubeconfig", "context", "session_dsn_secret", "session_database", "pod", "env", "volumes", "volume_mounts", "service_account", "controller_service_account",
	// A hook's own words, which the settings leave out whichever key the file writes them in.
	"prompt",
}

// settingsPathKeys are the `*_file` keys the settings do carry, each by the whole path it stands
// under: a `_file` key is a credential's file by the loader's own naming everywhere else, and the
// rule below reads the suffix, so each key that is a path of something else is named here with
// the reason it is no credential. The path and not the key alone, so the exemption covers the one
// block that earned it and a `prompt_file` grown anywhere else is still refused. The fixture
// carries each of these paths, which the rule below holds it to: an exemption no document
// exercises is an exemption nobody has read.
var settingsPathKeys = map[string]string{
	"settings.hooks[].prompt_file":     "the file a hook's words are read from, the deployment's own prose for its agents, carried as a path exactly as `instructions` is",
	"settings.private_name_rules_file": "the deployment's private-name rule file, carried as a path alone and never its rules, exactly as `instructions` is",
}

// The maximal kubernetes file resolves to a settings block whose top-level, runtime.kubernetes and
// github_apps.implement key sets are exactly the modelled ones, whose bytes carry none of the
// planted secrets, sources, paths or pod pieces, no credential-shaped key and no null.
func TestSettingsOfAKubernetesConfigurationNamesEveryKeyAndNoSecret(t *testing.T) {
	settings := SettingsOf(settingsFixtureConfig(t))
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// (a) The top level: every key of the table, omp_invocation apart, which the loader refuses
	// under runtime kubernetes.
	wantTop := []string{
		"project", "port", "bind", "advertise_host", "state_dir", "runtime", "admission_cap", "admission_p0_bound", "daemon_url",
		"worker_stream_port", "omp_launch_prefix", "provider_keys", "instructions", "private_name_rules_file",
		"worker_boot_timeout_seconds", "worker_rpc_timeout_seconds", "worker_stop_timeout_seconds",
		"tree_stop_timeout_seconds", "slow_command_timeout_seconds", "probe_interval_seconds",
		"controller_wake_interval_seconds", "worker_boot_registration_deadline_intervals",
		"launch_failure_limit", "prompt_failure_limit", "prompt_retire_limit",
		"worker_wakes_per_minute", "worker_wake_burst", "envoy_url", "nats_urls",
		"dispatch_url", "projects", "gates", "github_apps", "linger_hours", "production_check_hours",
		"pod_update_grace_minutes", "stalled_tree_notice_hours", "worker_silence_minutes",
		"worker_background_hours",
		"pod_update_max_open_rounds", "pod_update_rounds_per_minute", "review_round_cap",
		"controller", "controller_turn_bound_minutes", "controller_session_max_age_hours",
		"capabilities", "worker_backends", "role_config_files", "repository_renames",
		"hooks",
	}
	top := keysOf(t, encoded)
	assertKeys(t, "settings", top, wantTop)

	// (b) runtime.kubernetes, one project, and one App.
	runtimeKeys := keysOf(t, top["runtime"])
	assertKeys(t, "settings.runtime", runtimeKeys, []string{"name", "kubernetes"})
	kubernetesKeys := keysOf(t, runtimeKeys["kubernetes"])
	assertKeys(t, "settings.runtime.kubernetes", kubernetesKeys, []string{
		"namespace", "image", "storage_class", "issue_volume", "scheduling", "resources", "session_store",
		"agent_secrets", "plugins", "setup", "test_postgres", "repository_cache", "idle_release",
		"memory_guard", "volume_trim", "sandbox",
	})
	assertKeys(t, "settings.runtime.kubernetes.volume_trim", keysOf(t, kubernetesKeys["volume_trim"]), []string{
		"enabled", "high_percent", "low_percent", "min_age_minutes", "interval_seconds",
	})
	assertKeys(t, "settings.runtime.kubernetes.idle_release", keysOf(t, kubernetesKeys["idle_release"]), []string{
		"idle_minutes", "backstop_minutes",
	})
	memoryGuardKeys := keysOf(t, kubernetesKeys["memory_guard"])
	assertKeys(t, "settings.runtime.kubernetes.memory_guard", memoryGuardKeys, []string{"issue_pods", "controller_pod"})
	for _, kind := range []string{"issue_pods", "controller_pod"} {
		assertKeys(t, "settings.runtime.kubernetes.memory_guard."+kind, keysOf(t, memoryGuardKeys[kind]), []string{
			"enabled", "act_percent", "floor_percent", "wait_seconds", "interval_milliseconds",
		})
	}
	assertKeys(t, "settings.runtime.kubernetes.repository_cache", keysOf(t, kubernetesKeys["repository_cache"]), []string{
		"volume", "refresh_minutes", "retain_hours", "resources",
	})
	apps := keysOf(t, top["github_apps"])
	assertKeys(t, "settings.github_apps", apps, []string{"implement", "review"})
	assertKeys(t, "settings.github_apps.implement", keysOf(t, apps["implement"]), []string{"app_id", "installations"})
	projects := keysOf(t, top["projects"])
	assertKeys(t, "settings.projects", projects, []string{"LEGION", "WIDGETS"})
	// `experiments` always rides the wire, `[]` for the fixture's projects, which declare none
	// under that key (contract 141); what a project that declares one carries is
	// TestSettingsOfNamesAProjectsExperiments'.
	assertKeys(t, "settings.projects.WIDGETS", keysOf(t, projects["WIDGETS"]), []string{
		"slack_channels", "memory", "experiments", "production_check", "default_steps", "deploys",
	})

	// (c) Nothing planted reaches the wire, and no key of a credential's shape does either.
	for _, planted := range settingsFixturePlanted {
		if !strings.Contains(settingsFixtureFile, planted) {
			t.Errorf("settingsFixturePlanted names %q, which settingsFixtureFile does not plant", planted)
		}
		if bytes.Contains(encoded, []byte(planted)) {
			t.Errorf("settings carry %q: %s", planted, encoded)
		}
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	exercised := map[string]bool{}
	for _, key := range everyKey(document, "settings") {
		if _, path := settingsPathKeys[key]; path {
			exercised[key] = true
			continue
		}
		name := key[strings.LastIndex(key, ".")+1:]
		if slices.Contains(settingsForbiddenKeys, name) || strings.HasSuffix(name, "_file") {
			t.Errorf("settings carry the key %s", key)
		}
	}
	for path := range settingsPathKeys {
		if !exercised[path] {
			t.Errorf("the fixture carries no %s, so the exemption for it is never read: set the key in settingsFixtureFile or drop the exemption", path)
		}
	}

	// (d) No null anywhere: the plugin's reader refuses one.
	if bytes.Contains(encoded, []byte("null")) {
		t.Errorf("settings carry null: %s", encoded)
	}

	// The shapes the table changes from the file's: the variable names alone, the derived store,
	// the expressions as written, the hours as a number, the resolved reservations.
	if want := []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY"}; !slices.Equal(settings.ProviderKeys, want) {
		t.Errorf("provider_keys = %q, want %q", settings.ProviderKeys, want)
	}
	if settings.Runtime.Kubernetes.SessionStore != "postgres" {
		t.Errorf("session_store = %q, want postgres", settings.Runtime.Kubernetes.SessionStore)
	}
	// The project that names no `production_check` shows the default role, and the one that turns
	// the check off shows that word, never the empty string the role takes for off.
	if got := settings.Projects["LEGION"].ProductionCheck; got != "tester" {
		t.Errorf("projects.LEGION.production_check = %q, want tester, which a project that names no key gets", got)
	}
	if got := settings.Projects["WIDGETS"].ProductionCheck; got != "off" {
		t.Errorf("projects.WIDGETS.production_check = %q, want the file's off as the word", got)
	}
	// The project that names no `default_steps` shows the steps it runs by default, today's line
	// up to the owner's acceptance, and the one that names two shows those two, each as the word
	// the file takes.
	if got, want := settings.Projects["LEGION"].DefaultSteps, []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}; !slices.Equal(got, want) {
		t.Errorf("projects.LEGION.default_steps = %q, want %q, which a project that names no key runs", got, want)
	}
	if got, want := settings.Projects["WIDGETS"].DefaultSteps, []string{"implementer", "reviewer"}; !slices.Equal(got, want) {
		t.Errorf("projects.WIDGETS.default_steps = %q, want the file's %q", got, want)
	}
	if settings.ProductionCheckHours != 240 {
		t.Errorf("production_check_hours = %v, want the file's 240", settings.ProductionCheckHours)
	}
	if want := map[string]string{"tester": "omp"}; !maps.Equal(settings.WorkerBackends, want) {
		t.Errorf("worker_backends = %q, want %q", settings.WorkerBackends, want)
	}
	if want := []RepositoryRenameSettings{{From: "acme/widgets-legacy", To: "acme/widgets"}}; !slices.Equal(settings.RepositoryRenames, want) {
		t.Errorf("repository_renames = %+v, want the file's pair %+v", settings.RepositoryRenames, want)
	}
	if want := []string{"/bin/sh", "-c", "echo fixture-setup"}; !slices.Equal(settings.Runtime.Kubernetes.Setup, want) {
		t.Errorf("runtime.kubernetes.setup = %q, want %q", settings.Runtime.Kubernetes.Setup, want)
	}
	cache := settings.Runtime.Kubernetes.RepositoryCache
	if cache.Volume != "40Gi" || cache.RefreshMinutes != 15 || cache.RetainHours != 48 || cache.Resources.CPU != "500m" || cache.Resources.CPURequest != "500m" || cache.Resources.Memory != "1Gi" || cache.Resources.EphemeralStorage != "2Gi" {
		t.Errorf("runtime.kubernetes.repository_cache = %+v, want the file's volume, refresh, retention, cpu and memory with the default ephemeral storage", cache)
	}
	if settings.LingerHours != 1.5 || settings.WorkerStopTimeoutSeconds != 30 || settings.Controller != config.ControllerLaunchDaemon {
		t.Errorf("linger_hours %v, worker_stop_timeout_seconds %d, controller %q; want 1.5, 30, daemon", settings.LingerHours, settings.WorkerStopTimeoutSeconds, settings.Controller)
	}
	controller := settings.Runtime.Kubernetes.Resources.Controller
	if controller.CPU != "2" || controller.CPURequest != "1" || controller.MemoryRequest != "4Gi" || controller.EphemeralStorage != "10Gi" {
		t.Errorf("resources.controller = %+v, want the file's cpu, cpu_request and memory_request with the default ephemeral storage", controller)
	}
	if issue := settings.Runtime.Kubernetes.Resources.Issue; issue != (ReservationSettings{CPU: "4", CPURequest: "4", Memory: "24Gi", MemoryRequest: "24Gi", EphemeralStorage: "80Gi", EphemeralStorageRequest: "6Gi"}) {
		t.Errorf("resources.issue = %+v, want the file's cpu and memory, each requested in full, with the default disk bound", issue)
	}
	if want := []string{"C0123ABCD", "C0123ABCE"}; !slices.Equal(settings.Projects["LEGION"].SlackChannels, want) {
		t.Errorf("projects.LEGION.slack_channels = %q, want %q", settings.Projects["LEGION"].SlackChannels, want)
	}
	if want := map[string]string{"pool-capacity": "the pool is sized by hand for the admission cap"}; !maps.Equal(settings.Capabilities.Decided, want) {
		t.Errorf("capabilities.decided = %q, want %q", settings.Capabilities.Decided, want)
	}
	// The two addresses the file gives userinfo ride masked; the one it gives none rides as written.
	if want := "http://xxxxx@envoy-listener.internal.example:9020"; settings.EnvoyURL != want {
		t.Errorf("envoy_url = %q, want %q", settings.EnvoyURL, want)
	}
	if want := []string{"nats://xxxxx@nats.internal.example:4222", "nats://nats-2.internal.example:4222"}; !slices.Equal(settings.NatsURLs, want) {
		t.Errorf("nats_urls = %q, want %q: the first entry's userinfo masked, the second unchanged", settings.NatsURLs, want)
	}
	// The plugin source in URL form rides with its userinfo masked and its query dropped; the one in
	// version form rides as written.
	want := PluginsSettings{PiLegion: "10.4.0", PiEnvoy: "https://xxxxx@plugins.internal.example/pi-envoy-10.4.0.tgz"}
	if plugins := settings.Runtime.Kubernetes.Plugins; plugins == nil || *plugins != want {
		t.Errorf("runtime.kubernetes.plugins = %+v, want %+v", plugins, want)
	}
}

// The sandbox rides the settings as the loader settles it. The maximal file names no block, so it
// shows gVisor, `canary_issues` as `[]` and no `microvm`. A block shows each member the file
// names, and `keep_volume` as false where it names none. An inert microvm block, which no issue pod
// uses, shows no compute class, and its storage class is runtime.kubernetes.storage_class. None of
// it is a credential (settingsEmitted).
func TestSettingsOfTheSandboxShowsEachMember(t *testing.T) {
	const pool = "example-project.svc.id.goog"
	const provider = "https://container.googleapis.com/v1/projects/example-project/locations/us-central1/clusters/example-cluster"
	const metadata = `
        metadata:
          workload_pool: ` + pool + `
          identity_provider: ` + provider + `
`
	for _, tc := range []struct {
		name, block string
		want        SandboxSettings
		wire        string
	}{
		{
			name: "no block",
			want: SandboxSettings{Type: "gvisor", CanaryIssues: []string{}},
			wire: `{"type":"gvisor","canary_issues":[]}`,
		},
		{
			name:  "a canary list on a gVisor project",
			block: "    sandbox:\n      canary_issues: [LEGION-208, LEGION-209]\n      microvm:\n        compute_class: example-microvm\n        storage_class: example-microvm-disk" + metadata,
			want: SandboxSettings{Type: "gvisor", CanaryIssues: []string{"LEGION-208", "LEGION-209"}, MicroVM: &MicroVMSettings{
				ComputeClass: "example-microvm", StorageClass: "example-microvm-disk",
				Metadata: &MetadataSettings{WorkloadPool: pool, IdentityProvider: provider},
			}},
			wire: `{"type":"gvisor","canary_issues":["LEGION-208","LEGION-209"],"microvm":{"compute_class":"example-microvm","storage_class":"example-microvm-disk","keep_volume":false,` +
				`"metadata":{"workload_pool":"` + pool + `","identity_provider":"` + provider + `"}}}`,
		},
		{
			name:  "a microVM project",
			block: "    sandbox:\n      type: microvm\n      microvm: {compute_class: example-microvm, keep_volume: true}\n",
			want:  SandboxSettings{Type: "microvm", CanaryIssues: []string{}, MicroVM: &MicroVMSettings{ComputeClass: "example-microvm", StorageClass: "gp3", KeepVolume: true}},
			wire:  `{"type":"microvm","canary_issues":[],"microvm":{"compute_class":"example-microvm","storage_class":"gp3","keep_volume":true}}`,
		},
		{
			name:  "an inert microvm block",
			block: "    sandbox:\n      microvm:" + metadata,
			want: SandboxSettings{Type: "gvisor", CanaryIssues: []string{}, MicroVM: &MicroVMSettings{
				StorageClass: "gp3", Metadata: &MetadataSettings{WorkloadPool: pool, IdentityProvider: provider},
			}},
			wire: `{"type":"gvisor","canary_issues":[],"microvm":{"storage_class":"gp3","keep_volume":false,"metadata":{"workload_pool":"` + pool + `","identity_provider":"` + provider + `"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := SettingsOf(loadSettingsFile(t, settingsFixtureFile+tc.block))
			if got := settings.Runtime.Kubernetes.Sandbox; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("runtime.kubernetes.sandbox = %+v, want %+v", got, tc.want)
			}
			encoded, err := json.Marshal(settings)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			kubernetes := keysOf(t, keysOf(t, keysOf(t, encoded)["runtime"])["kubernetes"])
			if got := string(kubernetes["sandbox"]); got != tc.wire {
				t.Errorf("settings.runtime.kubernetes.sandbox =\n%s\nwant\n%s", got, tc.wire)
			}
		})
	}
}

// endpointOf keeps an address's scheme, host and path and masks its userinfo, a user alone or a
// user and password, as xxxxx@; "" stays "", and an address that yields no scheme and host is xxxxx
// whole.
func TestEndpointOfMasksUserinfoAndKeepsThePath(t *testing.T) {
	for _, tc := range []struct{ address, want string }{
		{"http://envoy-listener.internal.example:9020", "http://envoy-listener.internal.example:9020"},
		{"http://legion@envoy-listener.internal.example:9020", "http://xxxxx@envoy-listener.internal.example:9020"},
		{"http://legion:fixture-envoy-password@envoy-listener.internal.example:9020", "http://xxxxx@envoy-listener.internal.example:9020"},
		{"https://dispatch.internal.example/legion", "https://dispatch.internal.example/legion"},
		{"https://legion:fixture-dispatch-password@dispatch.internal.example/legion", "https://xxxxx@dispatch.internal.example/legion"},
		{"nats://legion:fixture-nats-password@nats.internal.example:4222", "nats://xxxxx@nats.internal.example:4222"},
		{"", ""},
		{"not a url", "xxxxx"},
		{"//host-only", "xxxxx"},
	} {
		if got := endpointOf(tc.address); got != tc.want {
			t.Errorf("endpointOf(%q) = %q, want %q", tc.address, got, tc.want)
		}
	}
}

// pluginSourceOf leaves a version and an absolute tarball path as written and rides the URL form
// through endpointOf, masking its userinfo as xxxxx@ and dropping its query and fragment.
func TestPluginSourceOfMasksOnlyTheURLForm(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"10.4.0", "10.4.0"},
		{"/opt/plugins/pi-legion-10.4.0.tgz", "/opt/plugins/pi-legion-10.4.0.tgz"},
		{"https://plugins.internal.example/pi-legion-10.4.0.tgz", "https://plugins.internal.example/pi-legion-10.4.0.tgz"},
		{"https://legion:secret@plugins.internal.example/pi-legion-10.4.0.tgz?X-Amz-Signature=abc#frag", "https://xxxxx@plugins.internal.example/pi-legion-10.4.0.tgz"},
		{"http://plugins.internal.example/x.tgz?token=t", "http://plugins.internal.example/x.tgz"},
	} {
		if got := pluginSourceOf(tc.source); got != tc.want {
			t.Errorf("pluginSourceOf(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

// A tmux file of the three required keys resolves to the defaults, with every optional block
// absent and every collection empty rather than null; and a key the file sets is the file's.
// The two pod-update round caps are the one exception: a file that names no limit carries them
// as null, since an absent key is the one thing no number could say.
func TestSettingsOfAMinimalConfigurationResolvesTheDefaults(t *testing.T) {
	settings := SettingsOf(minimalSettingsConfig(t))
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if settings.ReviewRoundCap != 3 || settings.WorkerBootTimeoutSeconds != 120 {
		t.Errorf("review_round_cap %d, worker_boot_timeout_seconds %d; want the defaults 3 and 120", settings.ReviewRoundCap, settings.WorkerBootTimeoutSeconds)
	}
	if settings.Runtime.Name != "tmux" || settings.Runtime.Kubernetes != nil {
		t.Errorf("runtime = %+v, want tmux with no kubernetes block", settings.Runtime)
	}
	if settings.GitHubApps != nil {
		t.Errorf("github_apps = %+v, want none", settings.GitHubApps)
	}
	top := keysOf(t, encoded)
	for key, want := range map[string]string{"provider_keys": "[]", "omp_launch_prefix": "[]", "nats_urls": "[]", "repository_renames": "[]", "projects": "{}", "capabilities": `{"decided":{}}`} {
		if got := string(top[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	for _, absent := range []string{"advertise_host", "omp_invocation", "instructions", "dispatch_url", "github_apps"} {
		if raw, present := top[absent]; present {
			t.Errorf("%s = %s, want absent", absent, raw)
		}
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantNull := []string{"settings.pod_update_max_open_rounds", "settings.pod_update_rounds_per_minute"}
	if got := slices.Sorted(slices.Values(nullKeys(document, "settings"))); !slices.Equal(got, wantNull) {
		t.Errorf("settings carry the null keys %q, want %q and no other: %s", got, wantNull, encoded)
	}
	if bytes.Contains(encoded, []byte("postgres://")) || bytes.Contains(encoded, []byte("fixture-dsn-password")) {
		t.Errorf("settings carry the DSN: %s", encoded)
	}

	set := SettingsOf(loadSettingsFile(t, minimalSettingsFile+"review_round_cap: 5\n"))
	if set.ReviewRoundCap != 5 {
		t.Errorf("review_round_cap = %d, want the file's 5", set.ReviewRoundCap)
	}
}

// The deployment's private-name rule file reaches the settings by its path alone, under
// private_name_rules_file, as `instructions` carries a path and never its text: the rules name the
// deployment's private names, and the route that carries the settings authenticates nobody. A
// file that names none carries no key at all, so the shapes the state golden pins do not move.
func TestSettingsOfNamesThePrivateNameRuleFileByItsPathAlone(t *testing.T) {
	const path = "/etc/legion/private-names.toml"
	encoded, err := json.Marshal(SettingsOf(loadSettingsFile(t, minimalSettingsFile+"private_name_rules_file: "+path+"\n")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(keysOf(t, encoded)["private_name_rules_file"]), `"`+path+`"`; got != want {
		t.Errorf("private_name_rules_file = %s, want %s", got, want)
	}

	none, err := json.Marshal(SettingsOf(minimalSettingsConfig(t)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if raw, present := keysOf(t, none)["private_name_rules_file"]; present {
		t.Errorf("private_name_rules_file = %s for a file that names none, want absent", raw)
	}
}

// A project's commit_issue_trailer_repos reaches no `settings`, at any depth: the value names a
// deployment's own repositories, and the route that carries the block authenticates nobody
// (settingsExcluded). The census above only forces the choice; this pins it, so a later
// ProjectSettings field would have to be decided here too.
func TestSettingsOfLeavesOutTheCommitIssueTrailerRepositories(t *testing.T) {
	const stamped = "acme/stamped"
	body := strings.Replace(settingsFixtureFile, "  WIDGETS:\n", "  WIDGETS:\n    commit_issue_trailer_repos: ["+stamped+"]\n", 1)
	if body == settingsFixtureFile {
		t.Fatal("the fixture's WIDGETS project was not given the key")
	}
	cfg := loadSettingsFile(t, body)
	if got := cfg.Projects["WIDGETS"].CommitIssueTrailerRepos; len(got) != 1 || got[0].String() != stamped {
		t.Fatalf("the fixture's project carries %v, want %s: the assertions below would prove nothing", got, stamped)
	}
	encoded, err := json.Marshal(SettingsOf(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range everyKey(document, "settings") {
		if strings.HasSuffix(key, ".commit_issue_trailer_repos") {
			t.Errorf("settings carry the key %s: %s", key, encoded)
		}
	}
	if bytes.Contains(encoded, []byte(stamped)) {
		t.Errorf("settings carry the configured repository: %s", encoded)
	}
}

// A project's `experiments` ride the settings as the file writes them: every block in the file's
// order, under the name it is declared with, with its mode, its unit, the roles its arms reach
// (absent where it names none), the arm `on` would give and its arms in the file's order, each
// with its weight and its overlays. The paths are the
// operator's own file names, as the `memory` block's are (settingsEmitted). A project that
// declares none under the key carries `[]`, never no key at all, which the census above reads off
// the fixture.
func TestSettingsOfNamesAProjectsExperiments(t *testing.T) {
	const declared = `    experiments:
      plan-length:
        mode: random
        unit: issue
        variants:
          - {name: short, weight: 3}
          - name: long
      review-depth:
        mode: on
        roles: [planner, implementer]
        on_variant: deep
        variants:
          - {name: deep}
          - {name: shallow}
`
	body := strings.Replace(settingsFixtureFile, "  WIDGETS:\n", "  WIDGETS:\n"+declared, 1)
	if body == settingsFixtureFile {
		t.Fatal("the fixture's WIDGETS project was not given the key")
	}
	settings := SettingsOf(loadSettingsFile(t, body))
	want := []ExperimentSettings{
		{Name: "plan-length", Mode: config.ExperimentRandom, Unit: config.ExperimentIssue, Variants: []VariantSettings{
			{Name: "short", Weight: 3, ConfigFiles: []string{}},
			{Name: "long", Weight: 1, ConfigFiles: []string{}},
		}},
		{Name: "review-depth", Mode: config.ExperimentOn, Unit: config.ExperimentTree, Roles: []claim.Role{claim.RolePlanner, claim.RoleImplementer}, OnVariant: "deep", Variants: []VariantSettings{
			{Name: "deep", Weight: 1, ConfigFiles: []string{}},
			{Name: "shallow", Weight: 1, ConfigFiles: []string{}},
		}},
	}
	if got := settings.Projects["WIDGETS"].Experiments; !reflect.DeepEqual(got, want) {
		t.Fatalf("the project's experiments = %+v, want %+v", got, want)
	}
	if got := settings.Projects["LEGION"].Experiments; len(got) != 0 {
		t.Fatalf("a project that declares no experiment under the key carries %+v, want none", got)
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	projects := keysOf(t, keysOf(t, encoded)["projects"])
	assertKeys(t, "settings.projects.WIDGETS", keysOf(t, projects["WIDGETS"]), []string{
		"slack_channels", "memory", "experiments", "production_check", "default_steps", "deploys",
	})
	if got := string(keysOf(t, projects["LEGION"])["experiments"]); got != "[]" {
		t.Errorf("a project that declares no experiment under the key carries experiments = %s, want []: the key always rides the wire, so a reader tells such a project from a daemon too old to know it (contract 141)", got)
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := nullKeys(document, "settings"); len(got) != 0 {
		t.Errorf("settings carry the null keys %q, want none: an arm that names no overlay carries `[]`", got)
	}
}

// An arm's `plan` rides the settings as the file writes it, absent where the arm names none, and
// the declared list the settings give back carries it on the same arm, so `legion state`, which
// reads that list, sees how the issues of each arm are done. The fault caught is experimentsOf
// leaving the word out.
func TestTheSettingsCarryThePlanOfAVariant(t *testing.T) {
	const declared = `    experiments:
      planning:
        mode: random
        unit: issue
        variants:
          - {name: a, plan: owner}
          - {name: b, plan: default}
`
	body := strings.Replace(settingsFixtureFile, "  WIDGETS:\n", "  WIDGETS:\n"+declared, 1)
	if body == settingsFixtureFile {
		t.Fatal("the fixture's WIDGETS project was not given the key")
	}
	settings := SettingsOf(loadSettingsFile(t, body))
	variants := settings.Projects["WIDGETS"].Experiments[0].Variants
	if variants[0].Plan != "owner" || variants[1].Plan != "default" {
		t.Fatalf("the arms' plans = %q and %q, want owner and default", variants[0].Plan, variants[1].Plan)
	}
	encoded, err := json.Marshal(settings.Projects["WIDGETS"].Experiments[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"plan":"default"`)) {
		t.Errorf("the experiment's settings = %s, want the key plan on its arm", encoded)
	}
	back := settings.Projects["WIDGETS"].DeclaredExperiments("WIDGETS")
	if len(back) != 2 || back[1].Name != "planning" {
		t.Fatalf("declared experiments = %+v, want memory then planning", back)
	}
	if got := back[1].Variants[1].Plan; got != config.PlanGroupDefault {
		t.Errorf("the declared arm b's plan = %q, want %q", got, config.PlanGroupDefault)
	}
	if !back[1].SetsPlan() {
		t.Error("the declared planning sets no plan, want it to")
	}
}

// The two pod-update round caps ride the settings as the numbers the maximal file names, and as
// null under the minimal one, which names neither: the key is always on the wire, and its null is
// how a reader learns the deployment caps nothing. An omitted key would say only that the daemon
// is too old to know the setting.
func TestSettingsPodUpdateRounds(t *testing.T) {
	maximal := SettingsOf(settingsFixtureConfig(t))
	if maximal.PodUpdateMaxOpenRounds == nil || maximal.PodUpdateRoundsPerMinute == nil {
		t.Fatalf("pod_update_max_open_rounds = %v, pod_update_rounds_per_minute = %v; want the file's two numbers",
			maximal.PodUpdateMaxOpenRounds, maximal.PodUpdateRoundsPerMinute)
	}
	if *maximal.PodUpdateMaxOpenRounds != 6 || *maximal.PodUpdateRoundsPerMinute != 2 {
		t.Errorf("pod_update_max_open_rounds = %d, pod_update_rounds_per_minute = %d; want the file's 6 and 2",
			*maximal.PodUpdateMaxOpenRounds, *maximal.PodUpdateRoundsPerMinute)
	}
	encoded, err := json.Marshal(maximal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for key, want := range map[string]string{"pod_update_max_open_rounds": "6", "pod_update_rounds_per_minute": "2"} {
		if got := string(keysOf(t, encoded)[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}

	minimal := SettingsOf(minimalSettingsConfig(t))
	if minimal.PodUpdateMaxOpenRounds != nil || minimal.PodUpdateRoundsPerMinute != nil {
		t.Errorf("pod_update_max_open_rounds = %v, pod_update_rounds_per_minute = %v; want nil for both, since the file names no limit",
			minimal.PodUpdateMaxOpenRounds, minimal.PodUpdateRoundsPerMinute)
	}
	encoded, err = json.Marshal(minimal)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	top := keysOf(t, encoded)
	for _, key := range []string{"pod_update_max_open_rounds", "pod_update_rounds_per_minute"} {
		raw, present := top[key]
		if !present || string(raw) != "null" {
			t.Errorf("%s = %s (present: %v), want null, the limit no file named", key, raw, present)
		}
	}
}

// The settings name every hook the daemon runs and no hook's words. A file that writes no
// `hooks:` key — the minimal one, since the maximal file names a hook of its own — runs the one
// default hook, which selects every move back: its `moves` is the one-element list `["any"]`,
// the word the operator writes for that, never the empty list and never a null, and it carries
// no `prompt_file`, since its words are the daemon's own. A file that names hooks carries each
// as it wrote it, with `prompt_file` as the path the loader resolved and never the words of
// either form.
func TestSettingsOfTheHooksNamesEachHookAndNoWords(t *testing.T) {
	byDefault := SettingsOf(minimalSettingsConfig(t))
	want := []HookSettings{{
		Name:  "work-back",
		On:    config.HookEventWorkBack,
		Moves: []string{"any"},
		Every: 5,
		Roles: []string{"implementer", "tester", "reviewer"},
	}}
	if !reflect.DeepEqual(byDefault.Hooks, want) {
		t.Errorf("the hooks of a file naming none = %+v, want the one default hook %+v", byDefault.Hooks, want)
	}

	const promptFile = "/etc/legion/hooks/back-to-implementing.md"
	const inline = "Read the reviewer's thread before you write anything."
	named := SettingsOf(loadSettingsFile(t, minimalSettingsFile+`hooks:
  - name: back-to-implementing
    on: work_back
    moves: [reviewing-implementing, testing-implementing]
    every: 2
    roles: [implementer]
    prompt_file: `+promptFile+`
  - name: stuck
    on: work_back
    every: 3
    roles: [implementer, tester]
    prompt: `+inline+`
`))
	wantNamed := []HookSettings{
		{
			Name: "back-to-implementing", On: config.HookEventWorkBack,
			Moves: []string{"reviewing-implementing", "testing-implementing"}, Every: 2,
			Roles: []string{"implementer"}, PromptFile: promptFile,
		},
		{
			Name: "stuck", On: config.HookEventWorkBack, Moves: []string{"any"}, Every: 3,
			Roles: []string{"implementer", "tester"},
		},
	}
	if !reflect.DeepEqual(named.Hooks, wantNamed) {
		t.Errorf("the hooks of the file = %+v, want %+v", named.Hooks, wantNamed)
	}

	encoded, err := json.Marshal(named)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	hooks := []json.RawMessage{}
	if err := json.Unmarshal(keysOf(t, encoded)["hooks"], &hooks); err != nil {
		t.Fatalf("read the hooks of %s: %v", encoded, err)
	}
	if len(hooks) != 2 {
		t.Fatalf("the wire carries %d hooks, want the file's 2: %s", len(hooks), encoded)
	}
	assertKeys(t, "settings.hooks[0]", keysOf(t, hooks[0]), []string{"name", "on", "moves", "every", "roles", "prompt_file"})
	assertKeys(t, "settings.hooks[1]", keysOf(t, hooks[1]), []string{"name", "on", "moves", "every", "roles"})
	for _, words := range []string{inline, config.DefaultWorkBackPrompt} {
		if bytes.Contains(encoded, []byte(words)) {
			t.Errorf("settings carry a hook's words %q: %s", words, encoded)
		}
	}
}

// keysOf is the members of one JSON object.
func keysOf(t *testing.T, encoded []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	return object
}

// assertKeys fails unless object's key set is exactly want.
func assertKeys(t *testing.T, name string, object map[string]json.RawMessage, want []string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(object))
	if expected := slices.Sorted(slices.Values(want)); !slices.Equal(got, expected) {
		t.Errorf("%s keys = %q, want %q", name, got, expected)
	}
}

// everyKey is every object key under document, as a dotted path from prefix.
func everyKey(document any, prefix string) []string {
	var keys []string
	switch value := document.(type) {
	case map[string]any:
		for key, member := range value {
			path := prefix + "." + key
			keys = append(keys, path)
			keys = append(keys, everyKey(member, path)...)
		}
	case []any:
		for _, member := range value {
			keys = append(keys, everyKey(member, prefix+"[]")...)
		}
	}
	return keys
}

// nullKeys is every object key under document whose value is null, as a dotted path from prefix:
// the keys a test holds to the short list the plugin's reader tolerates.
func nullKeys(document any, prefix string) []string {
	var keys []string
	switch value := document.(type) {
	case map[string]any:
		for key, member := range value {
			path := prefix + "." + key
			if member == nil {
				keys = append(keys, path)
			}
			keys = append(keys, nullKeys(member, path)...)
		}
	case []any:
		for _, member := range value {
			keys = append(keys, nullKeys(member, prefix+"[]")...)
		}
	}
	return keys
}

// runtime.kubernetes.agent_secrets.exchange_audience reaches the settings as the block's
// exchange_audience where the file sets it, an audience and no credential, and is absent where it
// does not, so a deployment that never set it reads byte for byte as before the key existed.
func TestSettingsOfNamesTheExchangeAudienceOnlyWhereItIsSet(t *testing.T) {
	for name, tc := range map[string]struct{ audience, want string }{
		"set":   {"legion-broker-exchange", `"exchange_audience":"legion-broker-exchange"`},
		"unset": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := settingsFixtureConfig(t)
			cfg.Runtime.Kubernetes.AgentSecrets.ExchangeAudience = tc.audience
			encoded, err := json.Marshal(SettingsOf(cfg).Runtime.Kubernetes.AgentSecrets)
			if err != nil {
				t.Fatal(err)
			}
			const before = `{"url":"https://agent-secrets.internal.example","audience":"fixture-broker-audience","token_expiry_seconds":1800`
			want := before + "}"
			if tc.want != "" {
				want = before + "," + tc.want + "}"
			}
			if string(encoded) != want {
				t.Fatalf("settings.runtime.kubernetes.agent_secrets = %s, want %s", encoded, want)
			}
		})
	}
}

// dispatch_token_broker reaches the settings as `true` where the file turns it on, a word about how
// the daemon and its roles reach Dispatch and no credential, and is absent where it is off, so a
// deployment that never set it reads byte for byte as before the key existed.
func TestSettingsOfNamesTheDispatchTokenBrokerOnlyWhereItIsOn(t *testing.T) {
	for name, tc := range map[string]struct {
		on   bool
		want string
	}{
		"on":  {true, "true"},
		"off": {false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := settingsFixtureConfig(t)
			cfg.DispatchTokenBroker = tc.on
			encoded, err := json.Marshal(SettingsOf(cfg))
			if err != nil {
				t.Fatal(err)
			}
			raw, present := keysOf(t, encoded)["dispatch_token_broker"]
			if got := string(raw); present != (tc.want != "") || got != tc.want {
				t.Fatalf("settings.dispatch_token_broker = %q (present %t), want %q", got, present, tc.want)
			}
		})
	}
}
