package config

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// keyDescriptor is one row of the loader's key table: a top-level key of `legion.yaml`, what the
// daemon does without it, whether it is required, one line saying what it does, and the reader
// that reads its value. The table is the only way the loader dispatches a top-level key — readKeys
// looks each key up in it and calls its Read, and a key the table does not list falls through to
// the warned, migration and tossed classification — so a new key is a new row here, never a new
// case. `legion config-reference --json` prints the table, the documentation site's configuration
// reference is generated from that output, and TestNoKeyBypassesTheKeyTable holds internal/config
// to it: a key the loader dispatches with no row, a row with no description, and a nested key
// named by neither a descriptor nor the undescribed list each fail the build.
type keyDescriptor struct {
	// Name is the top-level key as it is written in legion.yaml.
	Name string
	// Default is what the daemon does when the key is absent, in an operator's words, with
	// backticks around a literal value; empty where there is nothing in its place.
	Default string
	// RequiredBy is "yes", "no", or "when " followed by the condition that makes it required. The
	// vocabulary is the Dispatch settings table's (packages/envoy/cmd/dispatch/settings.go), whose
	// generator already holds it to ^(yes|no|when .+)$, and this table's generator does the same.
	RequiredBy string
	// Description is one line saying what the key does.
	Description string
	// Read reads the key's value into the file being loaded, answering the refusal the key's own
	// reader makes of a value it cannot take.
	Read func(value *yaml.Node, key string, file *fileConfig) error
}

// reads is the Read of a key whose reader answers one value that the file keeps in one field: the
// reader runs, and the field takes its answer, nothing being kept from a refusal.
func reads[T any](read func(*yaml.Node, string) (T, error), keep func(*fileConfig, T)) func(*yaml.Node, string, *fileConfig) error {
	return func(value *yaml.Node, key string, file *fileConfig) error {
		got, err := read(value, key)
		if err != nil {
			return err
		}
		keep(file, got)
		return nil
	}
}

// roundCap reads one of the two pod-update round caps and the review round cap: a positive integer
// with no upper bound, whose absent key is no limit rather than a number nobody chose.
func roundCap(value *yaml.Node, key string) (*int, error) { return readPositiveInteger(value, key, 0) }

// topLevelKeys is every top-level key of legion.yaml, in the order an operator meets them in
// `deploy/kubernetes/daemon/legion.yaml.example`: the project and where the daemon keeps its state
// and listens, what it runs at once, the controller, the agents' Oh My Pi and its credentials, the
// endpoints and token files, the projects and the GitHub Apps, the runtime block, the per-role
// overrides, the wake pacing, the workflow limits, and the supervision budgets and timers.
var topLevelKeys = []keyDescriptor{
	{
		Name: "project", Default: "none: the daemon refuses a file that names no project", RequiredBy: "yes",
		Description: "The Dispatch project key this daemon runs, which must also be a key of `projects`.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.Project = v }),
	},
	{
		Name: "state_dir", Default: "none: the daemon refuses a file that names no state directory", RequiredBy: "yes",
		Description: "The directory the daemon keeps its own state in; a relative path resolves against the configuration file's directory, and a switch of runtime needs a fresh one, since the locators a state directory holds are the runtime's own.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.StateDir = v }),
	},
	{
		Name: "postgres_dsn", Default: "`LEGION_POSTGRES_DSN` in the daemon's environment", RequiredBy: "when `LEGION_POSTGRES_DSN` is unset in the daemon's environment",
		Description: "The daemon's own record database, as a `postgres://…` connection string; the agents' conversations live in another database, which `runtime.kubernetes.session_store` names.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.PostgresDSN = v }),
	},
	{
		Name: "port", Default: "`13370`", RequiredBy: "no",
		Description: "The TCP port the daemon's API listens on, which every pod and pane reaches it at.",
		Read:        reads(readInt, func(f *fileConfig, v *int) { f.Port = v }),
	},
	{
		Name: "bind", Default: "`127.0.0.1`", RequiredBy: "no",
		Description: "The address the daemon listens on, and the host every pod dials the worker stream at while `advertise_host` is unset; under `runtime: kubernetes` it may be `0.0.0.0`, where a daemon whose own pod restarts onto a new IP binds every interface, but never a loopback address, which no pod reaches.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.Bind = v }),
	},
	{
		Name: "advertise_host", Default: "every pod dials `bind`", RequiredBy: "no",
		Description: "The stable address every pod dials the worker stream at in `bind`'s place — an IP address or an ASCII DNS name, such as the Service that fronts a daemon whose own pod restarts onto a new IP; only `runtime: kubernetes` accepts it, since every pane dials the daemon's own unix socket.",
		Read:        reads(readAdvertiseHost, func(f *fileConfig, v *string) { f.AdvertiseHost = v }),
	},
	{
		Name: "daemon_url", Default: "`http://127.0.0.1:<port>`", RequiredBy: "when runtime is kubernetes: a pod cannot reach the daemon's loopback address",
		Description: "The daemon's API as the pods and panes it starts are told to reach it, with no trailing slash and, under Kubernetes, neither a loopback nor the unspecified address.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.DaemonURL = v }),
	},
	{
		Name: "worker_stream_port", Default: "`port` + 1", RequiredBy: "no",
		Description: "The TCP port the worker stream listens on, which every pod's shim dials; it must differ from `port`.",
		Read:        reads(readInt, func(f *fileConfig, v *int) { f.WorkerStreamPort = v }),
	},
	{
		Name: "admission_cap", Default: "`4`, or `LEGION_ADMISSION_CAP` in the daemon's environment", RequiredBy: "no",
		Description: "How many issues, roots and children alike, hold a slot at once; an issue takes a slot when the daemon starts it and keeps it until it is done, and the rest wait in line.",
		Read:        reads(readInt, func(f *fileConfig, v *int) { f.AdmissionCap = v }),
	},
	{
		Name: "controller", Default: "`operator`", RequiredBy: "no",
		Description: "Who runs the project's controller: `operator`, a person with `legion controller start`, or `daemon`, a pod the daemon launches and keeps running (a pane of its own window under `runtime: tmux`), which needs `dispatch_url`.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.Controller = v }),
	},
	{
		Name: "instructions", Default: "nothing is appended to an agent's system prompt", RequiredBy: "no",
		Description: "A Markdown file of the deployment's standing rules — required checks, deploy and smoke commands, scope — appended to every agent's system prompt; it must hold no secret.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.Instructions = v }),
	},
	{
		Name: "provider_keys", Default: "the agents' Oh My Pi receives no provider credential", RequiredBy: "no",
		Description: "The provider credentials every agent's Oh My Pi receives in its environment: each variable Oh My Pi reads mapped to the key of the providers Secret that holds it, or, under `runtime: tmux`, to a secretsd key or a `file:/abs/path` the daemon reads once at boot.",
		Read:        reads(readProviderKeys, func(f *fileConfig, v []ProviderKey) { f.ProviderKeys = v }),
	},
	{
		Name: "omp_invocation", Default: "`LEGION_OMP_PATH` in the daemon's environment, which names the `omp` binary directly", RequiredBy: "when runtime is tmux and `LEGION_OMP_PATH` is unset",
		Description: "How a pane starts Oh My Pi on the daemon's own host, as `mise x <tool> -- omp`; `runtime: kubernetes` refuses it, since every pod runs the worker image's Oh My Pi.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.OmpInvocation = v }),
	},
	{
		Name: "omp_launch_prefix", Default: "a pane runs Oh My Pi with no prefix", RequiredBy: "no",
		Description: "An argv every pane's Oh My Pi launch is wrapped in; `runtime: kubernetes` refuses it, since every pod runs the worker image's Oh My Pi, and it may not run `secrets`, which `provider_keys` is for.",
		Read:        reads(readLaunchPrefix, func(f *fileConfig, v []string) { f.OmpLaunchPrefix = v }),
	},
	{
		Name: "operator_token_file", Default: "none: the boot refuses a daemon that names no operator token", RequiredBy: "yes",
		Description: "The file holding the operator's bearer token, which `legion controller start`, `legion claims` and `legion status` present and the daemon's operator routes authenticate against; make it one long random string readable by its owner alone.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.OperatorTokenFile = v }),
	},
	{
		Name: "dispatch_url", Default: "the daemon runs no workflow", RequiredBy: "when runtime is kubernetes: the workflow is what launches every pod",
		Description: "Dispatch's service base URL, never its `/mcp` endpoint, which its clients append themselves; setting it configures the workflow, which then requires `dispatch_token_file`, `nats_urls` and a `projects` entry for the daemon's own project.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.DispatchURL = v }),
	},
	{
		Name: "dispatch_token_file", Default: "none: a configured `dispatch_url` is refused without it", RequiredBy: "when `dispatch_url` is configured",
		Description: "The file holding the bearer token Legion's agents present to Dispatch; the daemon hands each pod its own copy, and no pod reads the operator's file.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.DispatchTokenFile = v }),
	},
	{
		Name: "envoy_url", Default: "`http://127.0.0.1:9020`", RequiredBy: "when runtime is kubernetes: a pod cannot reach the loopback listener it defaults to",
		Description: "The Envoy listener the daemon and every agent reach each other through.",
		Read:        reads(readString, func(f *fileConfig, v *string) { f.EnvoyURL = v }),
	},
	{
		Name: "envoy_token_file", Default: "none: the pods receive no Envoy bearer", RequiredBy: "when runtime is kubernetes: every pod receives the Envoy bearer",
		Description: "The file holding the Envoy listener's API token; the daemon hands each pod its own copy.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.EnvoyTokenFile = v }),
	},
	{
		Name: "nats_urls", Default: "none: the daemon reads no notification stream", RequiredBy: "when runtime is kubernetes: every pod's Envoy client connects to NATS, and when `dispatch_url` is configured: the workflow's intake reads Envoy's notification stream",
		Description: "The NATS URLs the daemon and every pod's Envoy client connect to, in order and each once; the daemon reads Dispatch's issue events and each repository's GitHub events from the notification stream they carry.",
		Read:        reads(readNatsURLs, func(f *fileConfig, v []string) { f.NatsURLs = v }),
	},
	{
		Name: "nats_nkey_seed_file", Default: "the agents connect to NATS with no nkey user", RequiredBy: "no",
		Description: "The file holding the agents' NATS nkey user seed, readable by its owner alone; the daemon publishes it to the pods as the providers Secret's `NATS_NKEY_SEED`.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.NatsNkeySeedFile = v }),
	},
	{
		Name: "nats_daemon_nkey_seed_file", Default: "the daemon connects to NATS with no nkey user of its own", RequiredBy: "no",
		Description: "The file holding the NATS nkey user seed for the daemon's own connection, readable by its owner alone, which no agent receives.",
		Read:        reads(readNonEmptyString, func(f *fileConfig, v *string) { f.NatsDaemonNkeySeedFile = v }),
	},
	{
		Name: "projects", Default: "none: a file that configures the workflow is refused without it", RequiredBy: "when any of `dispatch_url`, `dispatch_token_file` and `github_apps` is set, and when runtime is kubernetes: every pod's workspace is a repository Dispatch maps to the daemon's project",
		Description: "Each Dispatch project the daemon runs, by project key, one of them the daemon's own `project`; a project's repositories are Dispatch's own mapping, and its block carries what only that project does — its merge-queue role, its by-hand merge paths, its Slack channels, its production check, its memory group, its experiments and the repositories whose commits carry their issue key.",
		Read: func(value *yaml.Node, key string, file *fileConfig) error {
			// The projects' own warnings join whatever a warned key above them raised, in file
			// order: one list carries both, so neither producer may write over the other.
			projects, raised, err := readProjects(value, key)
			if err != nil {
				return err
			}
			file.Projects, file.Warnings = projects, append(file.Warnings, raised...)
			return nil
		},
	},
	{
		Name: repositoryRenamesKey, Default: "no two names are one repository", RequiredBy: "no",
		Description: "The repositories that moved, each pair naming the repository as every record holds it and the repository GitHub holds it under now, which the daemon then treats as one repository wherever it compares a name; the equivalence is read-side only, so a pair stays here for as long as any record names the old repository.",
		Read:        reads(readRepositoryRenames, func(f *fileConfig, v []ghrepo.Rename) { f.RepositoryRenames = v }),
	},
	{
		Name: "gates", Default: "`design: [spec]`, the root issue's primary document", RequiredBy: "no",
		Description: "The design gate: the documents of a tree's root a person approves before any phase of the tree starts, any released child starts or the merger's `READY` is accepted.",
		Read:        reads(readGates, func(f *fileConfig, v *Gates) { f.Gates = v }),
	},
	{
		Name: "github_apps", Default: "none: a file that configures the workflow is refused without it", RequiredBy: "when any of `dispatch_url`, `dispatch_token_file` and `projects` is set, and when runtime is kubernetes: every pod's workspace is cloned with the implement App's token",
		Description: "The implement App (the implementer's and merger's identity) and the review App (every other role's), each with its id and its private key, from which the daemon mints every agent's GitHub token, narrowed to the repositories that agent works.",
		Read:        reads(readGitHubApps, func(f *fileConfig, v *GitHubApps) { f.GitHubApps = v }),
	},
	{
		Name: "runtime", Default: "`tmux`, with no block", RequiredBy: "no",
		Description: "Where the agents run: the scalar `tmux`, the daemon and every pane on one host, or a one-key mapping, `{kubernetes: {…}}` or `{tmux: {…}}`, whose block the loader reads; a bare `kubernetes` scalar is refused, since the block is the only source of what a pod needs.",
		Read: func(value *yaml.Node, key string, file *fileConfig) error {
			kubernetes, tmux, err := readRuntime(value)
			if err != nil {
				return err
			}
			file.Kubernetes, file.Tmux = kubernetes, tmux
			return nil
		},
	},
	{
		Name: "worker_backends", Default: "every role runs on Oh My Pi", RequiredBy: "no",
		Description: "The issue roles that run on Claude Code instead of Oh My Pi: each key a phase worker's role (`planner`, `implementer`, `tester`, `reviewer`, `merger`), each value `omp` or `claude`; the architect and the controller always run on Oh My Pi.",
		Read:        reads(readWorkerBackends, func(f *fileConfig, v map[claim.Role]runtime.Backend) { f.WorkerBackends = v }),
	},
	{
		Name: "role_config_files", Default: "every role names only the overlays every role names", RequiredBy: "no",
		Description: "Oh My Pi settings overlays one role names and no other — a model fallback chain for the roles whose output a machine checks, say: each key a role, each value absolute paths that role's `PI_CONFIG_FILES` names after the overlays every role names, so each outranks those.",
		Read:        reads(readRoleConfigFiles, func(f *fileConfig, v map[claim.Role][]string) { f.RoleConfigFiles = v }),
	},
	{
		Name: "capabilities", Default: "every capability the deployment leaves open is reported open", RequiredBy: "no",
		Description: "The worker capabilities this deployment decides rather than closes: under `decided`, each capability the report would leave open mapped to the reason the boot, `legion state` and the controller's tick then show in the gap's place.",
		Read:        reads(readCapabilities, func(f *fileConfig, v *Capabilities) { f.Capabilities = v }),
	},
	{
		Name: "worker_wakes_per_minute", Default: "`8`", RequiredBy: "no",
		Description: "How many wakes a minute the daemon lets out — the role processes it starts, and the tasks and notices it queues to wake a session that already runs — so that a restart or a plugin wave does not push a provider organization over its per-minute uncached-input cap; `0` turns the pacing off, and a value below 0 is refused.",
		Read:        reads(readInt, func(f *fileConfig, v *int) { f.WorkerWakesPerMinute = v }),
	},
	{
		Name: "worker_wake_burst", Default: "`1`", RequiredBy: "no",
		Description: "How many wakes may leave at once before `worker_wakes_per_minute` holds the next one back; a burst below 1 is refused.",
		Read:        readPositive,
	},
	{
		Name: "linger_hours", Default: "`72`", RequiredBy: "no",
		Description: "How many hours a closed root's tree lingers before the daemon retires it, which is also where a claim carried out of that tree has its stop bounded; decimals allowed.",
		Read:        reads(readHours, func(f *fileConfig, v *time.Duration) { f.Linger = v }),
	},
	{
		Name: "production_check_hours", Default: "`336`, fourteen days", RequiredBy: "no",
		Description: "How many hours a closed issue's production check runs before its last look; decimals allowed.",
		Read:        reads(readHours, func(f *fileConfig, v *time.Duration) { f.ProductionCheckBound = v }),
	},
	{
		Name: "pod_update_retask_hours", Default: "`1`", RequiredBy: "no",
		Description: "How many hours a pod-update round waits before it asks a worker that has not answered again; decimals allowed.",
		Read:        reads(readHours, func(f *fileConfig, v *time.Duration) { f.PodUpdateRetask = v }),
	},
	{
		Name: "pod_update_notice_hours", Default: "`4`", RequiredBy: "no",
		Description: "How many hours a pod-update round runs before the controller is told it is not finishing; decimals allowed.",
		Read:        reads(readHours, func(f *fileConfig, v *time.Duration) { f.PodUpdateNotice = v }),
	},
	{
		Name: "pod_update_max_open_rounds", Default: "no limit at all: the daemon opens a round for any number of issues at once", RequiredBy: "no",
		Description: "How many issues may hold an open pod-update round at once, a round being the ask to every worker of its issue to push and wait.",
		Read:        reads(roundCap, func(f *fileConfig, v *int) { f.PodUpdateMaxOpenRounds = v }),
	},
	{
		Name: "pod_update_rounds_per_minute", Default: "no limit at all: a sweep opens as many rounds as it has issues for", RequiredBy: "no",
		Description: "How many pod-update rounds one sweep of the daemon opens; a sweep within 50 seconds of the last opening opens none.",
		Read:        reads(roundCap, func(f *fileConfig, v *int) { f.PodUpdateRoundsPerMinute = v }),
	},
	{
		Name: "review_round_cap", Default: "`3`", RequiredBy: "no",
		Description: "How many review rounds one pull request may take; it is the one cap left that blocks a pull request.",
		Read:        reads(roundCap, func(f *fileConfig, v *int) { f.ReviewRoundCap = v }),
	},
	{
		Name: "controller_wake_interval_seconds", Default: "`3600`", RequiredBy: "no",
		Description: "How often the daemon wakes the project's controller, which is also the period of its status repair, so raising it to quiet the controller slows that repair with it.",
		Read:        readPositive,
	},
	{
		Name: "launch_failure_limit", Default: "`3`", RequiredBy: "no",
		Description: "How many failed launches, and deaths of a ready agent while it had work outstanding, a claim takes before the daemon fails it.",
		Read:        readPositive,
	},
	{
		Name: "prompt_failure_limit", Default: "`3`", RequiredBy: "no",
		Description: "How many prompts a worker may acknowledge without taking a turn before the daemon fails its claim.",
		Read:        readPositive,
	},
	{
		Name: "prompt_retire_limit", Default: "`2`", RequiredBy: "no",
		Description: "How many times a worker is retired and relaunched over prompts it acknowledged without taking a turn before the daemon fails its claim.",
		Read:        readPositive,
	},
	{
		Name: stallBackstopKey, Default: "twice the bound that role's own worker holds a tool call no deadline judges to: eight hours for an implementer and a reviewer, two for every other role and the controller", RequiredBy: "no",
		Description: "The daemon's own backstop under a worker whose turn has stopped: per role, how many hours after that role's last recorded step the daemon ends the turn that step belongs to, so the task goes back to waiting and the session is sent it again, charging the claim nothing; `0` switches the backstop off for that role, and a bound below the role's whole stall ladder is refused.",
		Read:        reads(readStallBackstops, func(f *fileConfig, v map[claim.Role]time.Duration) { f.WorkerStallBackstops = v }),
	},
	{
		Name: "worker_boot_timeout_seconds", Default: "`120`", RequiredBy: "no",
		Description: "How long a launch waits for a role's launcher to connect — the image pull's bound, and how long a pod may stay `Pending` and unschedulable before the boot watchdog reads it dead; a GKE deployment, where a pod waits for a node of its own and pulls cold, needs a larger one.",
		Read:        readPositive,
	},
	{
		Name: "worker_boot_registration_deadline_intervals", Default: "`3`", RequiredBy: "no",
		Description: "How many `worker_boot_timeout_seconds` the registration deadline is: an agent whose process is alive but which has not registered, or not said it is ready, within the product is retired and counted as a launch failure.",
		Read:        readPositive,
	},
	{
		Name: "worker_rpc_timeout_seconds", Default: "`5`", RequiredBy: "no",
		Description: "How long each request on the worker stream waits for its answer, the one reconnect that judges a dead worker among them; it also sizes how long the intake gives one fact's handling.",
		Read:        readPositive,
	},
	{
		Name: "worker_stop_timeout_seconds", Default: "`10`", RequiredBy: "no",
		Description: "How long a role's stop gives its agent to exit before the daemon kills it, which under Kubernetes is the pod's termination grace and so has a floor the loader refuses a shorter value against.",
		Read:        readPositive,
	},
	{
		Name: "tree_stop_timeout_seconds", Default: "`60`", RequiredBy: "no",
		Description: "How long the stop of a tree's root process waits for that process to be gone before the stop reports what it found.",
		Read:        readPositive,
	},
	{
		Name: "slow_command_timeout_seconds", Default: "`300`", RequiredBy: "no",
		Description: "How long the daemon's slow commands run: the cluster check, a runtime's adoption of a recorded process, and each attempt of the boot's image probe, which takes `worker_boot_timeout_seconds` plus 60 seconds instead wherever that is larger.",
		Read:        readPositive,
	},
	{
		Name: "probe_interval_seconds", Default: "`30`", RequiredBy: "no",
		Description: "How often the runtime sweeps every recorded process, which is also how often a launch or relaunch the store refused is tried again.",
		Read:        readPositive,
	},
}

// topLevelByName is topLevelKeys by key name, which readKeys dispatches through.
var topLevelByName = func() map[string]*keyDescriptor {
	byName := make(map[string]*keyDescriptor, len(topLevelKeys))
	for i := range topLevelKeys {
		byName[topLevelKeys[i].Name] = &topLevelKeys[i]
	}
	return byName
}()

// member is one nested key a mapping may carry, with the same four columns a top-level key has.
type member struct {
	Name        string
	Default     string
	RequiredBy  string
	Description string
}

// memberGroup is one mapping of nested keys: the dotted path an operator writes them under, with
// `<KEY>` for a project key and `[]` for a sequence entry, and the keys the mapping may carry. A
// group's path begins with the top-level key it belongs to, which is how `legion config-reference
// --json` prints each nested key under its own top-level key.
type memberGroup struct {
	Path    string
	Members []member
}

// topLevel is the top-level key a group's path begins with.
func (g memberGroup) topLevel() string {
	if cut := strings.IndexAny(g.Path, ".["); cut >= 0 {
		return g.Path[:cut]
	}
	return g.Path
}

// nestedKeys is every mapping whose nested keys carry a description, by the path an operator
// writes them under. describedMembers looks a group up by that path, so a reader that passes a
// path this list does not hold is a bug TestNoKeyBypassesTheKeyTable catches, never a value.
var nestedKeys = []memberGroup{
	{Path: "gates", Members: []member{
		{Name: "design", Default: "`[spec]`, the root issue's primary document, which the word `root-issues` also means", RequiredBy: "no",
			Description: "The design gate's steps, in the order a person approves them: `decisions` (the root issue's decisions document), `criteria` (its Verification document) and `spec` (its primary document); the empty list skips the approval, which the word `off` also means, and any other word, or a step named twice, is refused."},
	}},
	{Path: repositoryRenamesKey + "[]", Members: []member{
		{Name: "from", Default: "none: a pair that names no `from` is refused", RequiredBy: "yes",
			Description: "The repository as it was, which every record Legion already holds names; compared without case."},
		{Name: "to", Default: "none: a pair that names no `to` is refused", RequiredBy: "yes",
			Description: "The repository it moved to, which GitHub's events name and which the daemon calls GitHub under; compared without case."},
	}},
	{Path: "capabilities", Members: []member{
		{Name: "decided", Default: "every capability the deployment leaves open is reported open", RequiredBy: "no",
			Description: "Each worker capability this deployment decides, mapped to the reason the capability report shows in the gap's place."},
	}},
	{Path: "runtime.tmux", Members: []member{
		{Name: "pane_env", Default: "a pane is told only what the runtime tells it", RequiredBy: "no",
			Description: "Plain variables added to every pane's environment, by name; refused are `PATH`, a name the runtime sets itself, the git identity, a launch secret's `_FILE` pointer, a variable that places Oh My Pi's sessions, a provider key, a credential-shaped name and a URL value carrying a password, since a secret reaches a pane as a file and never as a value on tmux's command line."},
		{Name: "pi_config_files", Default: "a pane names no overlay but its own role's", RequiredBy: "no",
			Description: "Oh My Pi settings overlays every pane names in `PI_CONFIG_FILES`, in file order and before the pane's own `role_config_files`; a path holding `:` is refused, since that is what separates the overlays the variable names."},
		{Name: "take_over", Default: "the tmux runtime refuses a claim another runtime recorded", RequiredBy: "no",
			Description: "The runtime whose claims this daemon takes over from the project's store while a project moves onto this host, `kubernetes` being the one value accepted; stop every process of that runtime first."},
	}},
	{Path: "runtime.kubernetes.pod", Members: []member{
		{Name: "env", Default: "a pod's Oh My Pi is told only what the runtime tells it", RequiredBy: "no",
			Description: "Plain variables every pod carries — the model route's `PI_CONFIG_FILES` and endpoint settings among them — which outrank a repository's own `.env`; a credential-shaped name is refused, and `provider_keys` is where a credential goes."},
		{Name: "volumes", Default: "a pod mounts only the volumes Legion gives it", RequiredBy: "no",
			Description: "The volumes every pod carries, each named once and with exactly one source: a Secret, a ConfigMap, a projected volume, a PersistentVolumeClaim of the namespace, or an NFS export the nodes reach."},
		{Name: "volume_mounts", Default: "a volume this block declares is mounted nowhere", RequiredBy: "no",
			Description: "Where each of those volumes is mounted in every pod, read-only unless the mount says otherwise."},
		{Name: "service_account", Default: "the namespace's `default` ServiceAccount", RequiredBy: "no",
			Description: "The ServiceAccount the agents' pods run as; it needs no permission of its own, and its projected token can be the model route's credential."},
	}},
	{Path: "runtime.kubernetes.agent_secrets", Members: []member{
		{Name: "url", Default: "none: a block that names no broker is refused", RequiredBy: "yes",
			Description: "The secrets broker every pod is enrolled with, as an absolute URL with no path, query, fragment or user, and `https` unless its host is a loopback address."},
		{Name: "token_file", Default: "none: a block that names no token file is refused", RequiredBy: "yes",
			Description: "The absolute path the daemon's own projected ServiceAccount token for the broker's audience is mounted at; the daemon logs in with it at every start and keeps logging in, and the broker's `BROKER_SERVICE_LOGINS` names that account."},
		{Name: "operator", Default: "", RequiredBy: "no",
			Description: "Removed: the daemon logs in with its own ServiceAccount token, which no person approves, so the block names no approver; the loader refuses a file that still sets it."},
		{Name: "audience", Default: "`agent-secrets`", RequiredBy: "no",
			Description: "The audience of the projected token the daemon presents to the broker."},
		{Name: "token_expiry_seconds", Default: "`3600`", RequiredBy: "no",
			Description: "How long a worker's broker token lives, between 600 and 3600 seconds, the API server issuing none under 600."},
	}},
	{Path: "runtime.kubernetes.plugins", Members: []member{
		{Name: "pi_legion", Default: "the pair the release workflow linked into this binary, which `legion version`'s second line names", RequiredBy: "when `pi_envoy` is set: a relaunch installs the pair together or not at all",
			Description: "The exact `@sjawhar/pi-legion` a stale-plugin relaunch installs into a kept pod, as an exact version, an absolute tarball path or an http(s) URL the pod can read, never a dist-tag or a range."},
		{Name: "pi_envoy", Default: "the pair the release workflow linked into this binary, which `legion version`'s second line names", RequiredBy: "when `pi_legion` is set: a relaunch installs the pair together or not at all",
			Description: "The exact `@sjawhar/pi-envoy` that relaunch installs beside it, in the same three forms."},
	}},
	{Path: "runtime.kubernetes.idle_release", Members: []member{
		{Name: "idle_minutes", Default: "`10`", RequiredBy: "no",
			Description: "How long an issue's pod may be quiet before the daemon releases it, suspending every claim of the released pod with the source `released`; `0` is the one zero the block accepts and releases no pod at all."},
		{Name: "backstop_minutes", Default: "`60`", RequiredBy: "no",
			Description: "How long after the release a released worker is brought back whatever else wakes it; a backstop below `idle_minutes` is refused, since it would wake a pod the daemon has only just released."},
	}},
}

// nestedByPath is nestedKeys by path, which describedMembers looks a group up in.
var nestedByPath = func() map[string]*memberGroup {
	byPath := make(map[string]*memberGroup, len(nestedKeys))
	for i := range nestedKeys {
		byPath[nestedKeys[i].Path] = &nestedKeys[i]
	}
	return byPath
}()

// undescribed is every nested key name whose line is not written yet, one entry per name however
// many mappings carry it. It is what lets a round end with the nested half of the configuration
// reference unfinished rather than with a red build or a weakened guard:
// TestNoKeyBypassesTheKeyTable refuses a name passed to members(...) that is in neither
// nestedKeys nor this list, so no nested key is ever forgotten; it refuses a top-level key the
// loader dispatches with no descriptor whatever this list holds, because those 51 are what makes
// the configuration reference true; and `legion config-reference --json` prints no entry for a
// name on this list, so the reference never carries a key with no description. A name is on this
// list for every mapping that still reaches it through plain members(...), whether or not another
// mapping describes the same name through describedMembers: a `path` or an `operator` means a
// different thing in each mapping that carries it, so one mapping's line never stands in for
// another's. Writing a mapping's lines is carrying its site over to describedMembers and taking
// its names off this list, and an empty list is the finished state, where every site carries its
// own lines.
var undescribed = []string{
	"agent_secrets",
	"app_id",
	"audience",
	"by_hand",
	"commit_issue_trailer_repos",
	"config_files",
	"config_map",
	"context",
	"cpu",
	"cpu_request",
	"default_mode",
	"deploys",
	"effect",
	"ephemeral_storage",
	"ephemeral_storage_request",
	"expiration_seconds",
	"experiments",
	"gateway",
	"idle_release",
	"image",
	"implement",
	"installations",
	"instance",
	"instructions",
	"issue_volume",
	"items",
	"key",
	"kubeconfig",
	"limits",
	"memory",
	"memory_request",
	"merge",
	"merge_queue_role",
	"mergeQueueRole",
	"mode",
	"mount_path",
	"name",
	"namespace",
	"nfs",
	"node_selector",
	"on_variant",
	"operator",
	"path",
	"persistent_volume_claim",
	"plugins",
	"pod",
	"priority_class",
	"private_key",
	"private_key_command",
	"private_key_secret",
	"production_check",
	"projected",
	"read_only",
	"refresh_minutes",
	"repository_cache",
	"requests",
	"resources",
	"retain_hours",
	"review",
	"role_profiles",
	"scheduling",
	"secret",
	"server",
	"service_account_token",
	"session_database",
	"session_dsn_secret",
	"session_store",
	"setup",
	"skills_repository",
	"slack_channels",
	"sources",
	"storage_class",
	"sub_path",
	"test_postgres",
	"token_command",
	"tolerations",
	"tree_volume",
	"unit",
	"user",
	"value",
	"variants",
	"volume",
	"weight",
}

// ConfigKey is one top-level key as `legion config-reference --json` prints it: the key table's
// own four columns, and beneath them the nested keys that belong to this key, so a generator of
// the documentation site renders one section per top-level key and one table of the nested keys
// under it. Nested is never null, and a nested key still on the undescribed list is in no entry.
type ConfigKey struct {
	Name        string            `json:"name"`
	Default     string            `json:"default"`
	Required    string            `json:"required"`
	Description string            `json:"description"`
	Nested      []ConfigNestedKey `json:"nested"`
}

// ConfigNestedKey is one nested key of that entry: Path is the mapping an operator writes it
// under, with `<KEY>` for a project key and `[]` for a sequence entry, so Path and Name joined
// with a dot are the key as it is written.
type ConfigNestedKey struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Default     string `json:"default"`
	Required    string `json:"required"`
	Description string `json:"description"`
}

// ConfigReference is the key table as `legion config-reference --json` prints it: every top-level
// key in table order, each with its described nested keys in nestedKeys order. It reads no file
// and no daemon.
func ConfigReference() []ConfigKey {
	entries := make([]ConfigKey, len(topLevelKeys))
	for index, row := range topLevelKeys {
		entry := ConfigKey{
			Name:        row.Name,
			Default:     row.Default,
			Required:    row.RequiredBy,
			Description: row.Description,
			Nested:      []ConfigNestedKey{},
		}
		for _, group := range nestedKeys {
			if group.topLevel() != row.Name {
				continue
			}
			for _, key := range group.Members {
				entry.Nested = append(entry.Nested, ConfigNestedKey{
					Path:        group.Path,
					Name:        key.Name,
					Default:     key.Default,
					Required:    key.RequiredBy,
					Description: key.Description,
				})
			}
		}
		entries[index] = entry
	}
	return entries
}
