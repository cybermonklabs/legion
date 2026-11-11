package config

import (
	"fmt"
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
	// Positive is the row of a positive-integer key: the number the key defaults to and the
	// field it resolves into, nil for every other key. Such a row writes no Default and no Read
	// of its own — the table below renders the number into Default and takes the reader from
	// Positive — so the number, the field and the line an operator reads are one row.
	Positive *positiveKey
}

// positiveKey is what a positive-integer key resolves to: Value is the number the daemon takes
// where the file names none, and exactly one of Duration and Count is the Config field the key
// fills — Duration for a key written in seconds, which is held to the timer bound as well, and
// Count for one with no unit.
type positiveKey struct {
	Value    int
	Duration func(*Config) *time.Duration
	Count    func(*Config) *int
}

// read is such a key's reader: a positive integer and, for a duration, at most the timer bound
// (config.ts readPositiveInteger, and loadConfigFromFile's lifecycle keys). It keeps what it read
// in the file's own map, which resolve then reads back.
func (p *positiveKey) read(value *yaml.Node, key string, file *fileConfig) error {
	read, err := readInt(value, key)
	if err != nil || read == nil {
		return err
	}
	if *read <= 0 {
		return fmt.Errorf("%s must be a positive integer", key)
	}
	if p.Duration == nil {
		file.Counts[key] = *read
		return nil
	}
	if *read > maxTimerSeconds {
		return fmt.Errorf("%s must be at most %d", key, maxTimerSeconds)
	}
	file.Durations[key] = *read
	return nil
}

// resolve writes key's value onto cfg: the number the file named, or this row's own default where
// it named none. A duration's number is seconds.
func (p *positiveKey) resolve(key string, file fileConfig, cfg *Config) {
	if p.Duration == nil {
		count, named := file.Counts[key]
		if !named {
			count = p.Value
		}
		*p.Count(cfg) = count
		return
	}
	seconds, named := file.Durations[key]
	if !named {
		seconds = p.Value
	}
	*p.Duration(cfg) = time.Duration(seconds) * time.Second
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
//
// A row that names a Positive is finished here rather than written out: its Default is that row's
// number as an operator reads it, and its Read is that row's own reader, so a positive-integer
// key's number is written once and the line the configuration reference prints can never name
// another.
var topLevelKeys = func() []keyDescriptor {
	table := []keyDescriptor{
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
			Name: hooksKey, Default: "one hook: every fifth move back of any kind puts the daemon's own words in front of the implementer, the tester and the reviewer", RequiredBy: "no",
			Description: "The deployment's own prompts, each naming the event it stands on, the moves back it counts, how many of them fire it, the roles its words are put in front of and those words; `hooks: []` runs none, and a file that names the key runs what it names and no default.",
			Read:        reads(readHooks, func(f *fileConfig, v *[]Hook) { f.Hooks = v }),
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
			Name: "worker_wake_burst", RequiredBy: "no",
			Description: "How many wakes may leave at once before `worker_wakes_per_minute` holds the next one back; a burst below 1 is refused.",
			Positive:    &positiveKey{Value: 1, Count: func(c *Config) *int { return &c.WorkerWakeBurst }},
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
			Name: "controller_wake_interval_seconds", RequiredBy: "no",
			Description: "How often the daemon wakes the project's controller, which is also the period of its status repair, so raising it to quiet the controller slows that repair with it.",
			Positive:    &positiveKey{Value: 3600, Duration: func(c *Config) *time.Duration { return &c.ControllerWakeInterval }},
		},
		{
			Name: "launch_failure_limit", RequiredBy: "no",
			Description: "How many failed launches, and deaths of a ready agent while it had work outstanding, a claim takes before the daemon fails it.",
			Positive:    &positiveKey{Value: 3, Count: func(c *Config) *int { return &c.LaunchFailureLimit }},
		},
		{
			Name: "prompt_failure_limit", RequiredBy: "no",
			Description: "How many prompts a worker may acknowledge without taking a turn before the daemon fails its claim.",
			Positive:    &positiveKey{Value: 3, Count: func(c *Config) *int { return &c.PromptFailureLimit }},
		},
		{
			Name: "prompt_retire_limit", RequiredBy: "no",
			Description: "How many times a worker is retired and relaunched over prompts it acknowledged without taking a turn before the daemon fails its claim.",
			Positive:    &positiveKey{Value: 2, Count: func(c *Config) *int { return &c.PromptRetireLimit }},
		},
		{
			Name: stallBackstopKey, Default: "twice the bound that role's own worker holds a tool call no deadline judges to: eight hours for an implementer and a reviewer, two for every other role and the controller", RequiredBy: "no",
			Description: "The daemon's own backstop under a worker whose turn has stopped: per role, how many hours after that role's last recorded step the daemon ends the turn that step belongs to, so the task goes back to waiting and the session is sent it again, charging the claim nothing; `0` switches the backstop off for that role, and a bound below the role's whole stall ladder is refused.",
			Read:        reads(readStallBackstops, func(f *fileConfig, v map[claim.Role]time.Duration) { f.WorkerStallBackstops = v }),
		},
		{
			Name: "worker_boot_timeout_seconds", RequiredBy: "no",
			Description: "How long a launch waits for a role's launcher to connect — the image pull's bound, and how long a pod may stay `Pending` and unschedulable before the boot watchdog reads it dead; a GKE deployment, where a pod waits for a node of its own and pulls cold, needs a larger one.",
			Positive:    &positiveKey{Value: 120, Duration: func(c *Config) *time.Duration { return &c.WorkerBootTimeout }},
		},
		{
			Name: "worker_boot_registration_deadline_intervals", RequiredBy: "no",
			Description: "How many `worker_boot_timeout_seconds` the registration deadline is: an agent whose process is alive but which has not registered, or not said it is ready, within the product is retired and counted as a launch failure.",
			Positive:    &positiveKey{Value: 3, Count: func(c *Config) *int { return &c.WorkerBootRegistrationDeadlineIntervals }},
		},
		{
			Name: "worker_rpc_timeout_seconds", RequiredBy: "no",
			Description: "How long each request on the worker stream waits for its answer, the one reconnect that judges a dead worker among them; it also sizes how long the intake gives one fact's handling.",
			Positive:    &positiveKey{Value: 5, Duration: func(c *Config) *time.Duration { return &c.WorkerRPCTimeout }},
		},
		{
			Name: "worker_stop_timeout_seconds", RequiredBy: "no",
			Description: "How long a role's stop gives its agent to exit before the daemon kills it, which under Kubernetes is the pod's termination grace and so has a floor the loader refuses a shorter value against.",
			Positive:    &positiveKey{Value: 10, Duration: func(c *Config) *time.Duration { return &c.WorkerStopTimeout }},
		},
		{
			Name: "tree_stop_timeout_seconds", RequiredBy: "no",
			Description: "How long the stop of a tree's root process waits for that process to be gone before the stop reports what it found.",
			Positive:    &positiveKey{Value: 60, Duration: func(c *Config) *time.Duration { return &c.TreeStopTimeout }},
		},
		{
			Name: "slow_command_timeout_seconds", RequiredBy: "no",
			Description: "How long the daemon's slow commands run: the cluster check, a runtime's adoption of a recorded process, and each attempt of the boot's image probe, which takes `worker_boot_timeout_seconds` plus 60 seconds instead wherever that is larger.",
			Positive:    &positiveKey{Value: 300, Duration: func(c *Config) *time.Duration { return &c.SlowCommandTimeout }},
		},
		{
			Name: "probe_interval_seconds", RequiredBy: "no",
			Description: "How often the runtime sweeps every recorded process, which is also how often a launch or relaunch the store refused is tried again.",
			Positive:    &positiveKey{Value: 30, Duration: func(c *Config) *time.Duration { return &c.ProbeInterval }},
		},
	}
	for i := range table {
		if row := &table[i]; row.Positive != nil {
			row.Default, row.Read = fmt.Sprintf("`%d`", row.Positive.Value), row.Positive.read
		}
	}
	return table
}()

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
	{Path: hooksKey + "[]", Members: []member{
		{Name: "name", Default: "none: a hook that names no `name` is refused", RequiredBy: "yes",
			Description: "The hook's own name, which every prompt it publishes carries and which is named once in the file."},
		{Name: "on", Default: "none: a hook that names no `on` is refused", RequiredBy: "yes",
			Description: "The event the hook stands on: `work_back`, a move to an earlier phase, is the one event the daemon has."},
		{Name: "moves", Default: "`any`, every move back", RequiredBy: "no",
			Description: "The moves back the hook counts, each written `<from>-<to>` of two workflow phases, or the word `any`; the empty list is refused, since it selects nothing."},
		{Name: "every", Default: "none: a hook that names no `every` is refused", RequiredBy: "yes",
			Description: "How many of the moves it counts fire the hook: it fires when those counts, added up, are a whole multiple of this number."},
		{Name: "roles", Default: "none: a hook that names no `roles` is refused", RequiredBy: "yes",
			Description: "The roles its words are put in front of, at least one of implementer, tester, reviewer, planner and merger; the architect is refused, since the daemon's own wake to an architect is a notice."},
		{Name: "prompt", Default: "the words come from `prompt_file`", RequiredBy: "when the hook names no `prompt_file`: a hook carries the words it puts in front of its roles",
			Description: "The words themselves, written in the file."},
		{Name: "prompt_file", Default: "the words come from `prompt`", RequiredBy: "when the hook names no `prompt`: a hook carries the words it puts in front of its roles",
			Description: "A file the words are read from at boot, resolved against the directory of the file that set it, as `instructions` is; a missing, unreadable or empty one refuses the boot."},
	}},
	{Path: "capabilities", Members: []member{
		{Name: "decided", Default: "every capability the deployment leaves open is reported open", RequiredBy: "no",
			Description: "Each worker capability this deployment decides, mapped to the reason the capability report shows in the gap's place."},
	}},
	{Path: "github_apps", Members: []member{
		{Name: "implement", Default: "none: a file that names no implement App is refused", RequiredBy: "yes",
			Description: "The App the implementer and the merger work as, whose token clones every pod's workspace and pushes and lands the change; no review ever comes from it."},
		{Name: "review", Default: "none: a file that names no review App is refused", RequiredBy: "yes",
			Description: "The App every other role works as — architect, planner, tester and reviewer — whose token writes the reviews and the check runs the implement App may not."},
	}},
	{Path: "github_apps.<APP>", Members: []member{
		{Name: "app_id", Default: "none: an App that names no id is refused", RequiredBy: "yes",
			Description: "The App's numeric id, which every token the daemon mints from it is asked for under."},
		{Name: "private_key", Default: "the App's credential comes from one of the other three keys", RequiredBy: "when the App names none of `private_key_command`, `private_key_secret` and `token_command`: an App carries exactly one credential",
			Description: "The App's private key as the PEM itself, which the daemon mints each agent's token with."},
		{Name: "private_key_command", Default: "the App's credential comes from one of the other three keys", RequiredBy: "when the App names none of `private_key`, `private_key_secret` and `token_command`: an App carries exactly one credential",
			Description: "A command the daemon runs whose output is that PEM, for a key kept outside this file."},
		{Name: "private_key_secret", Default: "the App's credential comes from one of the other three keys", RequiredBy: "when the App names none of `private_key`, `private_key_command` and `token_command`: an App carries exactly one credential",
			Description: "The secretsd key holding that PEM, as one key name with no whitespace in it."},
		{Name: "token_command", Default: "the App's credential comes from one of the other three keys", RequiredBy: "when the App names none of `private_key`, `private_key_command` and `private_key_secret`: an App carries exactly one credential",
			Description: "A command that prints an installation token the daemon uses in the App's place; its own answer names each installation, which is why `installations` may not be set beside it."},
		{Name: "installations", Default: "the daemon finds each owner's installation itself", RequiredBy: "no",
			Description: "The App's installation id for each owner, where the deployment pins them rather than letting the daemon look each one up."},
	}},
	{Path: "projects.<KEY>", Members: []member{
		{Name: "commit_issue_trailer_repos", Default: "a pane stamps no commit with its issue key", RequiredBy: "no",
			Description: "The repositories whose commits carry their issue key, as the `Dispatch: <KEY>` trailer the pane's jj appends, so a squash landing the branch carries the key; each must be one Dispatch maps to this project."},
		{Name: "deploys", Default: "", RequiredBy: "no",
			Description: "Removed: the daemon no longer wakes a production check from a deploy run, so the workflows this key names configure nothing; a project that still sets it is warned about once at boot and the key is read for nothing."},
		{Name: "experiments", Default: "every tree and issue of the project runs on the deployment's own settings", RequiredBy: "no",
			Description: "The experiments this project draws its trees and issues into, by name, each naming the arms it compares and what an arm gives the units drawn into it."},
		{Name: "memory", Default: "no tree of the project is drawn into a memory group", RequiredBy: "no",
			Description: "What each tree this project opens runs with: which new trees are drawn into the memory group, and the overlays and instructions a drawn tree's roles then carry."},
		{Name: "merge", Default: "the merger arms every pull request it opens", RequiredBy: "no",
			Description: "The merge rules that hold for this project: which changed paths a person merges by hand rather than the merger."},
		{Name: "merge_queue_role", Default: "no role receives the copies", RequiredBy: "no",
			Description: "The Envoy role that receives a copy of every `READY` of this project, and the notice withdrawing it when the head's own CI turns red, the head starts conflicting with its base, or GitHub reports its merge-queue entry unmergeable; a bare role name, with no `notifications.role.` in front."},
		{Name: "mergeQueueRole", Default: "the project's role comes from `merge_queue_role`", RequiredBy: "no",
			Description: "The camelCase spelling of `merge_queue_role` a file written for an earlier release still carries; it means the same role, and `merge_queue_role` is the one read where a project sets both."},
		{Name: "production_check", Default: "`tester`", RequiredBy: "no",
			Description: "Who works the production check a merge closing one of this project's issues opens: `tester`, who takes its first look in the live session while the issue's other claims retire, or `off`, which opens none."},
		{Name: "slack_channels", Default: "no channel's mentions reach the project", RequiredBy: "no",
			Description: "The Slack channels whose mentions of the Legion app reach this project's controller, which answers in the thread; it needs `controller: daemon` and the secrets broker, and a channel belongs to one project."},
	}},
	{Path: "projects.<KEY>.merge", Members: []member{
		{Name: "by_hand", Default: "the merger arms every pull request it opens", RequiredBy: "no",
			Description: "The paths a person merges by hand, as regular expressions matched anywhere in a changed path, so anchor one (`^migrations/versions/`) to mean a directory; a pull request touching one is `READY` with a `MANUAL_MERGE_REQUIRED:` note and the merger arms nothing."},
	}},
	{Path: "projects.<KEY>.memory", Members: []member{
		{Name: "mode", Default: "`off`", RequiredBy: "no",
			Description: "Which of the project's new trees are drawn into the memory group: `off` none, `on` every one, `random` a fair coin for each; the draw is made once, when the tree's lifecycle row is created, and the tree keeps it for its whole life."},
		{Name: "config_files", Default: "none: a mode other than `off` is refused without them", RequiredBy: "when `mode` is `on` or `random`: a memory-group role reads its memory from these files",
			Description: "The Oh My Pi settings overlays every role of a memory-group tree names last in `PI_CONFIG_FILES`, after the deployment's own and the role's own, as absolute paths; under Kubernetes each must be a file one of the pod's mounts carries."},
		{Name: "instructions", Default: "a memory-group role's system prompt gains nothing", RequiredBy: "no",
			Description: "One Markdown file on the daemon's own disk, never a pod mount, whose text the daemon reads at boot and appends to every memory-group role's system prompt, after the deployment's own instructions."},
	}},
	{Path: "projects.<KEY>.experiments.<NAME>", Members: []member{
		{Name: "mode", Default: "`off`", RequiredBy: "no",
			Description: "Which of the project's new units the experiment draws: `off` none, `on` every one into `on_variant`, `random` a draw among the variants by weight; the draw is made once and the unit keeps it."},
		{Name: "unit", Default: "`tree`", RequiredBy: "no",
			Description: "What the experiment draws one at a time: a whole `tree`, or each `issue` on its own."},
		{Name: "on_variant", Default: "none: a block whose mode is `on` is refused without it", RequiredBy: "when `mode` is `on`: that mode gives every unit one named variant",
			Description: "The variant `mode: on` gives every unit, which must be one this experiment lists."},
		{Name: "variants", Default: "none: a block that lists no variant is refused", RequiredBy: "yes",
			Description: "The arms the experiment draws among, and at least two of them while the mode is not `off`, since a draw among one arm measures nothing."},
	}},
	{Path: "projects.<KEY>.experiments.<NAME>.variants[]", Members: []member{
		{Name: "name", Default: "none: an arm that names no name is refused", RequiredBy: "yes",
			Description: "The arm's name, 1 to 32 characters of a-z, 0-9 and `-`, which every draw into it records and which the experiment names once."},
		{Name: "weight", Default: "`1`", RequiredBy: "no",
			Description: "How much of a random draw this arm takes against the other arms' weights, as a whole number above zero."},
		{Name: "config_files", Default: "a unit drawn into this arm names no overlay of the arm's own", RequiredBy: "no",
			Description: "The Oh My Pi settings overlays every role of a unit drawn into this arm names in `PI_CONFIG_FILES`, as absolute paths; the arm that names no file is the control the others are measured against."},
		{Name: "instructions", Default: "a role of a unit drawn into this arm gains nothing", RequiredBy: "no",
			Description: "One Markdown file on the daemon's own disk, which the daemon reads at boot and appends to the system prompt of every role of a unit drawn into this arm."},
	}},
	{Path: "runtime.tmux", Members: []member{
		{Name: "pane_env", Default: "a pane is told only what the runtime tells it", RequiredBy: "no",
			Description: "Plain variables added to every pane's environment, by name; refused are `PATH`, a name the runtime sets itself, the git identity, a launch secret's `_FILE` pointer, a variable that places Oh My Pi's sessions, a provider key, a credential-shaped name and a URL value carrying a password, since a secret reaches a pane as a file and never as a value on tmux's command line."},
		{Name: "pi_config_files", Default: "a pane names no overlay but its own role's", RequiredBy: "no",
			Description: "Oh My Pi settings overlays every pane names in `PI_CONFIG_FILES`, in file order and before the pane's own `role_config_files`; a path holding `:` is refused, since that is what separates the overlays the variable names."},
		{Name: "take_over", Default: "the tmux runtime refuses a claim another runtime recorded", RequiredBy: "no",
			Description: "The runtime whose claims this daemon takes over from the project's store while a project moves onto this host, `kubernetes` being the one value accepted; stop every process of that runtime first."},
	}},
	{Path: kubernetesKey, Members: []member{
		{Name: "namespace", Default: "none: a block that names no namespace is refused", RequiredBy: "yes",
			Description: "The namespace every pod, volume, Secret and Sandbox the daemon makes lives in."},
		{Name: "image", Default: "none: a block that names no image is refused", RequiredBy: "yes",
			Description: "The worker image every agent runs from, pinned by digest (`@sha256:…`) and never by a tag, which moves under a daemon that must know exactly what it probed."},
		{Name: "storage_class", Default: "none: a block that names no class is refused, the cluster having no default for the issue volumes", RequiredBy: "yes",
			Description: "The StorageClass each issue pod's own volume is provisioned from."},
		{Name: "issue_volume", Default: "`50Gi`", RequiredBy: "no",
			Description: "How large each issue pod's own volume is, the daemon-launched controller's pod included, as a Kubernetes quantity."},
		{Name: "tree_volume", Default: "", RequiredBy: "no",
			Description: "Removed: a volume is one issue's now rather than one tree's, so its size is `issue_volume`; the loader refuses a file that still sets it."},
		{Name: "kubeconfig", Default: "the daemon's own in-cluster credentials", RequiredBy: "no",
			Description: "The kubeconfig the daemon reaches the cluster with, as a path resolved against this file's directory."},
		{Name: "context", Default: "the kubeconfig's own current context", RequiredBy: "when the kubeconfig sets no current context",
			Description: "Which context of that kubeconfig the daemon uses; naming one requires `kubeconfig`."},
		{Name: "scheduling", Default: "a pod selects the Legion pool and tolerates nothing else", RequiredBy: "no",
			Description: "Where the pods may run beyond the pool the runtime selects itself: the node labels they also select, the taints they tolerate and their priority class."},
		{Name: "resources", Default: "both pods take the daemon's own reservations", RequiredBy: "no",
			Description: "What each of the two pods the daemon builds reserves and is bounded by: `issue`, every issue pod's, and `controller`, the daemon-launched controller's."},
		{Name: "gateway", Default: "", RequiredBy: "no",
			Description: "Removed (LEGION-270): a pod's model route is the operator's own, which `pod` carries; the loader refuses a file that still sets it."},
		{Name: "pod", Default: "a pod carries only what Legion gives it", RequiredBy: "no",
			Description: "What the operator adds to every pod Legion runs, the image probe's included: its variables, volumes, mounts and ServiceAccount, the model route among them, since Legion holds none."},
		{Name: "session_store", Default: "none: a file that names no store is refused", RequiredBy: "yes",
			Description: "Where every pod's Oh My Pi keeps its conversation: `postgres`, whose URL the providers Secret holds, or `cloudsql-iam`, which each pod signs in to as itself; `pvc` and an omitted key are refused, since a conversation kept as a file on the pod's own disk dies with the pod and the next launch's resume fails on it."},
		{Name: "session_dsn_secret", Default: "none: `postgres` is refused without it", RequiredBy: "when `session_store` is `postgres`",
			Description: "The key of the providers Secret holding that database's `postgres://…` URL, which every pod mounts read-only; `cloudsql-iam` refuses the key rather than leaving it inert."},
		{Name: "session_database", Default: "none: `cloudsql-iam` is refused without it", RequiredBy: "when `session_store` is `cloudsql-iam`",
			Description: "The Cloud SQL database each pod signs in to as itself, no URL being held anywhere; `postgres` refuses the key rather than leaving it inert."},
		{Name: "role_profiles", Default: "", RequiredBy: "no",
			Description: "Removed: an issue pod has one reservation, `resources.issue`, whose absent field takes the daemon's default; the loader refuses a file that still sets it."},
		{Name: "agent_secrets", Default: "no pod is enrolled with a secrets broker", RequiredBy: "no",
			Description: "The secrets broker every pod is enrolled with, and the daemon's own login to it."},
		{Name: "plugins", Default: "a relaunch installs the pair the release workflow linked into this binary", RequiredBy: "no",
			Description: "The exact pi-legion and pi-envoy pair a stale-plugin relaunch installs into a kept pod, in place of the image's own copies."},
		{Name: "repository_cache", Default: "every cache runs on the daemon's own sizes and intervals", RequiredBy: "no",
			Description: "Each mapped repository's cache pod, which an issue pod's feed is filled from before GitHub is asked for the rest."},
		{Name: "setup", Default: "a role starts its Oh My Pi with nothing run first", RequiredBy: "no",
			Description: "A command, as an argv, every role runs in its working directory with its own environment before each start of its Oh My Pi, which starts only once the command exits 0; it must fetch nothing, since a clone in a role's launch context ends the pod's sandbox."},
		{Name: "skills_repository", Default: "a pod's roles install no skill", RequiredBy: "no",
			Description: "The Oh My Pi plugin marketplace every issue pod's roles install their skills from, as `owner/name`, cloned shallow in an init container of the pod's own, so a pod's skills are that branch as of the pod's start."},
		{Name: "test_postgres", Default: "`false`", RequiredBy: "no",
			Description: "Whether every issue pod's roles container serves its own PostgreSQL for the tests that need one."},
		{Name: "idle_release", Default: "a pod quiet for ten minutes is released, and an open phase's worker is woken an hour after that", RequiredBy: "no",
			Description: "How long an issue's pod may be quiet before the daemon releases it, and how long after that release a worker whose phase is open is brought back whatever else wakes it."},
	}},
	{Path: kubernetesKey + ".scheduling", Members: []member{
		{Name: "node_selector", Default: "a pod selects the Legion pool alone", RequiredBy: "no",
			Description: "The node labels every pod selects as well, merged over the pool label the runtime sets itself and which this key may not name, since the cluster's admission policy requires the pool's own value."},
		{Name: "tolerations", Default: "a pod tolerates no taint, so a tainted pool needs its toleration named here", RequiredBy: "no",
			Description: "The node taints every pod tolerates, each a key, an operator and an effect."},
		{Name: "priority_class", Default: "a pod runs at the namespace's own default priority", RequiredBy: "no",
			Description: "The PriorityClass every pod carries, which decides what the scheduler preempts to place it."},
	}},
	{Path: kubernetesKey + ".scheduling.tolerations[]", Members: []member{
		{Name: "key", Default: "none: a toleration that names no key is refused", RequiredBy: "yes",
			Description: "The taint key this toleration is for."},
		{Name: "operator", Default: "none: a toleration that names no operator is refused", RequiredBy: "yes",
			Description: "How the taint is matched: `Equal`, which takes the `value` beside it, or `Exists`, which takes none."},
		{Name: "value", Default: "`Equal` matches the taint whose value is empty", RequiredBy: "no",
			Description: "The taint value matched under `Equal`; `Exists` refuses it, matching the key whatever its value."},
		{Name: "effect", Default: "none: a toleration that names no effect is refused", RequiredBy: "yes",
			Description: "The taint effect tolerated: `NoSchedule`, `NoExecute` or `PreferNoSchedule`."},
	}},
	{Path: kubernetesKey + ".resources.<POD>", Members: []member{
		{Name: "cpu", Default: "the daemon's own limit for that pod: `3` for an issue pod, `1` for the controller's", RequiredBy: "no",
			Description: "The CPU limit every container of the pod carries, as a Kubernetes quantity."},
		{Name: "cpu_request", Default: "the pod's own `cpu`, which is what makes a pod that raises a limit alone Guaranteed", RequiredBy: "no",
			Description: "What the scheduler reserves of that CPU, at most the limit; below the limit the pod is Burstable rather than Guaranteed."},
		{Name: "memory", Default: "the daemon's own limit for that pod: `19Gi` for an issue pod, `4Gi` for the controller's", RequiredBy: "no",
			Description: "The memory limit every container of the pod carries; under gVisor every process of the pod draws on this one number, so the pod's whole peak must fit it."},
		{Name: "memory_request", Default: "the pod's own `memory`, which is what makes a pod that raises a limit alone Guaranteed", RequiredBy: "no",
			Description: "What the scheduler reserves of that memory, at most the limit; below the limit the pod is Burstable rather than Guaranteed."},
		{Name: "ephemeral_storage", Default: "the daemon's own limit for that pod: `80Gi` for an issue pod, `10Gi` for the controller's", RequiredBy: "no",
			Description: "The pod's bound on the node's own disk — the image's content and each role's home and `/tmp`, the issue volume being its own — past which that pod alone is evicted."},
		{Name: "ephemeral_storage_request", Default: "the daemon's own request for that pod: `6Gi` for an issue pod, `1Gi` for the controller's", RequiredBy: "no",
			Description: "What the scheduler reserves of that disk; an operator who knows the root volume raises it so no pod is placed where a full node could not hold it."},
		{Name: "requests", Default: "", RequiredBy: "no",
			Description: "Removed: a reservation is its `cpu` and `memory` limits, with `cpu_request` and `memory_request` below them where set; the loader refuses a file that still sets it."},
		{Name: "limits", Default: "", RequiredBy: "no",
			Description: "Removed: a reservation's limits are its own `cpu` and `memory`; the loader refuses a file that still sets it."},
	}},
	{Path: kubernetesKey + ".repository_cache", Members: []member{
		{Name: "volume", Default: "`20Gi`", RequiredBy: "no",
			Description: "How large the cache pod's own volume is, which holds the repository's bare clone, as a Kubernetes quantity."},
		{Name: "refresh_minutes", Default: "`10`", RequiredBy: "no",
			Description: "How long the cache waits between its fetches from GitHub, in whole minutes."},
		{Name: "retain_hours", Default: "`168`", RequiredBy: "no",
			Description: "How long a cache outlives its repository leaving Dispatch's mapping before the orphan sweep deletes it with its volume and its Secret, in whole hours; the repository mapped again before that keeps its cache."},
		{Name: "resources", Default: "the cache's container takes the daemon's own reservation", RequiredBy: "no",
			Description: "What the cache pod's one container reserves and is bounded by, in a pod reservation's own six fields."},
	}},
	{Path: kubernetesKey + ".repository_cache.resources", Members: []member{
		{Name: "cpu", Default: "`1`", RequiredBy: "no",
			Description: "The CPU limit the cache's container carries, as a Kubernetes quantity."},
		{Name: "cpu_request", Default: "the cache's own `cpu`, so a raised limit is requested in full", RequiredBy: "no",
			Description: "What the scheduler reserves of that CPU, at most the limit."},
		{Name: "memory", Default: "`4Gi`", RequiredBy: "no",
			Description: "The memory limit the cache's container carries, which its daily `git repack -a -d` of a large repository wants."},
		{Name: "memory_request", Default: "the cache's own `memory`, so a raised limit is requested in full", RequiredBy: "no",
			Description: "What the scheduler reserves of that memory, at most the limit."},
		{Name: "ephemeral_storage", Default: "`2Gi`", RequiredBy: "no",
			Description: "The cache pod's bound on the node's own disk, the bare clone living on the cache's volume rather than there."},
		{Name: "ephemeral_storage_request", Default: "`1Gi`", RequiredBy: "no",
			Description: "What the scheduler reserves of that disk, at most the limit."},
	}},
	{Path: sessionDatabaseKey, Members: []member{
		{Name: "instance", Default: "none: the block is refused without it", RequiredBy: "yes",
			Description: "The Cloud SQL instance's connection name, `project:region:instance`, or `domain:project:region:instance` for a domain-scoped project, which each pod's session proxy dials."},
		{Name: "name", Default: "none: the block is refused without it", RequiredBy: "yes",
			Description: "The database in that instance holding Oh My Pi's session tables."},
		{Name: "user", Default: "none: the block is refused without it", RequiredBy: "yes",
			Description: "The IAM login each pod signs in to that database as."},
	}},
	{Path: podKey, Members: []member{
		{Name: "env", Default: "a pod's Oh My Pi is told only what the runtime tells it", RequiredBy: "no",
			Description: "Plain variables every pod carries — the model route's `PI_CONFIG_FILES` and endpoint settings among them — which outrank a repository's own `.env`; a credential-shaped name is refused, and `provider_keys` is where a credential goes."},
		{Name: "volumes", Default: "a pod mounts only the volumes Legion gives it", RequiredBy: "no",
			Description: "The volumes every pod carries, each named once and with exactly one source: a Secret, a ConfigMap, a projected volume, a PersistentVolumeClaim of the namespace, or an NFS export the nodes reach."},
		{Name: "volume_mounts", Default: "a volume this block declares is mounted nowhere", RequiredBy: "no",
			Description: "Where each of those volumes is mounted in every pod, read-only unless the mount says otherwise."},
		{Name: "service_account", Default: "the namespace's `default` ServiceAccount", RequiredBy: "no",
			Description: "The ServiceAccount the agents' pods run as; it needs no permission of its own, and its projected token can be the model route's credential."},
	}},
	{Path: podKey + ".volumes[]", Members: []member{
		{Name: "name", Default: "none: a volume that names no name is refused", RequiredBy: "yes",
			Description: "The volume's name, which a `volume_mounts` entry mounts it by and which no other volume of the block carries."},
		{Name: "secret", Default: "the volume's content comes from one of the other four sources", RequiredBy: "when the volume names none of `config_map`, `projected`, `persistent_volume_claim` and `nfs`: a volume has exactly one source",
			Description: "A Secret of the daemon's namespace as the volume's content, with the keys it projects and the mode of every file."},
		{Name: "config_map", Default: "the volume's content comes from one of the other four sources", RequiredBy: "when the volume names none of `secret`, `projected`, `persistent_volume_claim` and `nfs`: a volume has exactly one source",
			Description: "A ConfigMap of the daemon's namespace as the volume's content, with the keys it projects and the mode of every file."},
		{Name: "projected", Default: "the volume's content comes from one of the other four sources", RequiredBy: "when the volume names none of `secret`, `config_map`, `persistent_volume_claim` and `nfs`: a volume has exactly one source",
			Description: "Several sources projected into the one volume: ServiceAccount tokens, Secrets and ConfigMaps together."},
		{Name: "persistent_volume_claim", Default: "the volume's content comes from one of the other four sources", RequiredBy: "when the volume names none of `secret`, `config_map`, `projected` and `nfs`: a volume has exactly one source",
			Description: "An existing share as the volume's content, through a PersistentVolumeClaim the operator made in the daemon's namespace; one volume names a claim, never two."},
		{Name: "nfs", Default: "the volume's content comes from one of the other four sources", RequiredBy: "when the volume names none of `secret`, `config_map`, `projected` and `persistent_volume_claim`: a volume has exactly one source",
			Description: "An NFS export the nodes reach as the volume's content, mounted by the node's own NFS client."},
	}},
	{Path: podKey + ".volumes[].secret", Members: []member{
		{Name: "name", Default: "none: a source that names no object is refused", RequiredBy: "yes",
			Description: "The Secret of the daemon's namespace this volume holds, which must exist with every key the volume asks of it, or the image probe refuses the boot naming it."},
		{Name: "items", Default: "every key of that Secret becomes a file of its own name", RequiredBy: "no",
			Description: "The keys of that Secret the volume projects, each with the file it becomes."},
		{Name: "default_mode", Default: "Kubernetes' own `0644`, which every user in the pod can read", RequiredBy: "no",
			Description: "The mode of every file this source projects, in octal digits after a leading `0` or `0o` and from 0 to 0777; a mode written in decimal is refused, since `440` would be 0670."},
	}},
	{Path: podKey + ".volumes[].secret.items[]", Members: []member{
		{Name: "key", Default: "none: an item that names no key is refused", RequiredBy: "yes",
			Description: "The key of that Secret or ConfigMap this item projects."},
		{Name: "path", Default: "none: an item that names no path is refused", RequiredBy: "yes",
			Description: "The file that key becomes, under the volume's own root."},
	}},
	{Path: podKey + ".volumes[].config_map", Members: []member{
		{Name: "name", Default: "none: a source that names no object is refused", RequiredBy: "yes",
			Description: "The ConfigMap of the daemon's namespace this volume holds, which must exist with every key the volume asks of it, or the image probe refuses the boot naming it."},
		{Name: "items", Default: "every key of that ConfigMap becomes a file of its own name", RequiredBy: "no",
			Description: "The keys of that ConfigMap the volume projects, each with the file it becomes; the item's own lines are the `secret.items[]` ones, which both sources' items are read against."},
		{Name: "default_mode", Default: "Kubernetes' own `0644`, which every user in the pod can read", RequiredBy: "no",
			Description: "The mode of every file this source projects, in octal digits after a leading `0` or `0o` and from 0 to 0777; a mode written in decimal is refused, since `440` would be 0670."},
	}},
	{Path: podKey + ".volumes[].persistent_volume_claim", Members: []member{
		{Name: "name", Default: "none: a claim source that names no claim is refused", RequiredBy: "yes",
			Description: "The PersistentVolumeClaim of the daemon's namespace the pods mount, by the name Kubernetes holds it under; whether a pod writes the share is its `volume_mounts` entry's `read_only`."},
	}},
	{Path: podKey + ".volumes[].nfs", Members: []member{
		{Name: "server", Default: "none: an NFS source that names no server is refused", RequiredBy: "yes",
			Description: "The NFS server the nodes mount from, an IP address or an ASCII DNS name with no scheme, port, path or brackets, and neither the node itself nor the unspecified address."},
		{Name: "path", Default: "none: an NFS source that names no export is refused", RequiredBy: "yes",
			Description: "The export on that server, as a clean absolute path."},
	}},
	{Path: podKey + ".volumes[].projected", Members: []member{
		{Name: "sources", Default: "none: a projected volume with no source is refused", RequiredBy: "yes",
			Description: "What is projected into the one volume, at least one source and each exactly one of a ServiceAccount token, a Secret and a ConfigMap."},
	}},
	{Path: podKey + ".volumes[].projected.sources[]", Members: []member{
		{Name: "service_account_token", Default: "the source's content comes from a `secret` or a `config_map` instead", RequiredBy: "when the source names neither `secret` nor `config_map`: a source projects exactly one thing",
			Description: "A token of the pods' own ServiceAccount, written into the volume for the audience and lifetime this source names."},
		{Name: "secret", Default: "the source's content comes from a `service_account_token` or a `config_map` instead", RequiredBy: "when the source names neither `service_account_token` nor `config_map`: a source projects exactly one thing",
			Description: "A Secret of the daemon's namespace projected into the volume, with the keys it projects; the mode is the whole volume's, so no source of one sets `default_mode`."},
		{Name: "config_map", Default: "the source's content comes from a `service_account_token` or a `secret` instead", RequiredBy: "when the source names neither `service_account_token` nor `secret`: a source projects exactly one thing",
			Description: "A ConfigMap of the daemon's namespace projected into the volume, with the keys it projects; the mode is the whole volume's, so no source of one sets `default_mode`."},
	}},
	{Path: podKey + ".volumes[].projected.sources[].secret", Members: []member{
		{Name: "name", Default: "none: a source that names no object is refused", RequiredBy: "yes",
			Description: "The Secret of the daemon's namespace this source projects, which must exist with every key the volume asks of it."},
		{Name: "items", Default: "every key of that Secret becomes a file of its own name", RequiredBy: "no",
			Description: "The keys of that Secret the volume projects, each with the file it becomes."},
	}},
	{Path: podKey + ".volumes[].projected.sources[].config_map", Members: []member{
		{Name: "name", Default: "none: a source that names no object is refused", RequiredBy: "yes",
			Description: "The ConfigMap of the daemon's namespace this source projects, which must exist with every key the volume asks of it."},
		{Name: "items", Default: "every key of that ConfigMap becomes a file of its own name", RequiredBy: "no",
			Description: "The keys of that ConfigMap the volume projects, each with the file it becomes; the item's own lines are the `secret.items[]` ones, which every source's items are read against."},
	}},
	{Path: podKey + ".volumes[].projected.sources[].service_account_token", Members: []member{
		{Name: "audience", Default: "the API server's own audience, which the daemon's own API accepts", RequiredBy: "no",
			Description: "The audience this token is issued for, which the service reading it checks; it may not still hold a `${…}` placeholder, since nothing in Legion expands one and every call made with such a token would be refused."},
		{Name: "expiration_seconds", Default: "the API server's own lifetime for a projected token, an hour", RequiredBy: "no",
			Description: "How long each token lives before the kubelet replaces it in place, and never under 600 seconds, which is the shortest the API server issues."},
		{Name: "path", Default: "none: a token projection that names no path is refused", RequiredBy: "yes",
			Description: "The file the token is written to, under the volume's own root."},
	}},
	{Path: podKey + ".volume_mounts[]", Members: []member{
		{Name: "volume", Default: "none: a mount that names no volume is refused", RequiredBy: "yes",
			Description: "Which `volumes` entry this mount carries into every pod, by its name."},
		{Name: "mount_path", Default: "none: a mount that names no path is refused", RequiredBy: "yes",
			Description: "Where that volume is mounted in every pod, as a clean absolute path no other mount of the operator's takes."},
		{Name: "sub_path", Default: "the whole volume is mounted there", RequiredBy: "no",
			Description: "The one file or directory of the volume mounted at that path, as a path inside the volume; a file mounted this way is never refreshed in a running pod, so a changed file reaches a role once its pod is replaced."},
		{Name: "read_only", Default: "`true`", RequiredBy: "no",
			Description: "Whether the pod may only read what is mounted; a share no mount of it writes is read-only at its source as well, so the node mounts an export `-o ro` and the kubelet changes no ownership on it."},
	}},
	{Path: agentSecretsKey, Members: []member{
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
	{Path: pluginsKey, Members: []member{
		{Name: "pi_legion", Default: "the pair the release workflow linked into this binary, which `legion version`'s second line names", RequiredBy: "when `pi_envoy` is set: a relaunch installs the pair together or not at all",
			Description: "The exact `@sjawhar/pi-legion` a stale-plugin relaunch installs into a kept pod, as an exact version, an absolute tarball path or an http(s) URL the pod can read, never a dist-tag or a range."},
		{Name: "pi_envoy", Default: "the pair the release workflow linked into this binary, which `legion version`'s second line names", RequiredBy: "when `pi_legion` is set: a relaunch installs the pair together or not at all",
			Description: "The exact `@sjawhar/pi-envoy` that relaunch installs beside it, in the same three forms."},
	}},
	{Path: kubernetesKey + ".idle_release", Members: []member{
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
// loader dispatches with no descriptor whatever this list holds, because those 52 are what makes
// the configuration reference true; and `legion config-reference --json` prints no entry for a
// name on this list, so the reference never carries a key with no description. A name is on this
// list for every mapping that still reaches it through plain members(...), whether or not another
// mapping describes the same name through describedMembers: a `path` or an `operator` means a
// different thing in each mapping that carries it, so one mapping's line never stands in for
// another's. Writing a mapping's lines is carrying its site over to describedMembers and taking
// its names off this list, and an empty list is the finished state, where every site carries its
// own lines.
var undescribed = []string{}

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
