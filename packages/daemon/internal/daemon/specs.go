package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/notify"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var _ supervise.Specs = specs{}

// specs builds the part of every launch the claim does not carry. An operator may retain a
// one-off prompt under the state directory for the Stage 2 proof; otherwise the Go daemon
// composes the shipped role parts plus its own role-specific instructions. The repository is the
// one the claim's issue's record names, at the default branch the record names; a runtime that
// provisions workspaces itself (a Sandbox pod's init containers) provisions the issue's workspace
// from them, and under any other the daemon makes sure the workspace is on its own host first.
type specs struct {
	stateDir string
	project  string
	// instructions is the path of the daemon's copy of the deployment's instructions, "" when the
	// configuration names none, and copies every copy of an instructions file, which SpawnSpec
	// reads again before it builds a launch (instructionsCopies.refresh), so an edited file
	// reaches the next launch. Boot always sets copies; it is nil only in a test whose specs
	// literal leaves it out, and such a specs launches on whatever the copies hold.
	instructions string
	copies       *instructionsCopies
	secrets      map[string]string
	prompts      *prompts.Composer
	// roles are the roles the deployment defines (config.Config.Roles), in the file's order:
	// each launches with its own prompt file (prompts.ComposeStep) and its model, and the owner's
	// addressing lists them in this order. Nil is a deployment that defines none.
	roles []config.Role
	// variants is every arm of a declared experiment the claim's unit was drawn into and that
	// reaches the claim's role, in the order the project's file declares the experiments, and
	// the daemon's own copies of the instructions those arms name (launchVariants, over the
	// daemon's own store). Boot always sets it; it is nil only in a test whose specs literal
	// leaves it out, and such a specs launches every role in no experiment.
	variants func(ctx context.Context, tree, issue string, role claim.Role) (experiment.Arms, error)
	// designSteps is the design-gate steps the project gates (gates.design), in the order it
	// gates them, which a tree's root architect is told after its addressing
	// (DesignGateFragment); empty for a deployment that gates no step.
	designSteps []record.DesignStep
	// productionCheck is the project's `production_check` (config.Project.ProductionCheck): every
	// architect is told it after its addressing (ProductionCheckFragment). The empty role, a
	// test's zero value, reads `off`.
	productionCheck claim.Role
	// slackChannels is the project's slack_channels, which the controller is told after the
	// design gate steps (SlackChannelsFragment); its LEGION_SLACK_CHANNELS is the runtime's to set.
	slackChannels []string
	// commitIssueTrailerRepos is the project's commit_issue_trailer_repos, the repositories whose
	// commits a pane stamps with its issue key (runtime.SpawnSpec.IssueTrailer); nil for a
	// project that names none, whose panes stamp nothing.
	commitIssueTrailerRepos []ghrepo.Repository
	// same is whether two repository names name one repository: the daemon's one equivalence
	// (repositories.same) — the same name compared without case, or the
	// two names of a configured rename (repository_renames) — which issueTrailer compares
	// commit_issue_trailer_repos against, so a launch on the name a transfer moved a repository
	// away from stamps exactly as one on the name the project wrote. Boot always sets it; it is
	// nil only in a test whose specs literal leaves it out, and such a specs stamps no issue key
	// at all, as variants' nil leaves every launch in no experiment.
	same func(a, b string) bool
	// identity is the role's App bot identity every pane commits as, minted for the owner whose
	// installation holds the repository the launch works — the record's name over a transfer, the
	// mint's owner its serving one (workflowRuntime.identity). A worker's launch asks it for its
	// own role, and for one role of the other App when knownIdentity knows none of that App's, so
	// one call per App, its own first (roleIdentities). nil for a daemon with no GitHub Apps.
	identity func(ctx context.Context, role claim.Role, repository ghrepo.Repository) (runtime.GitIdentity, error)
	// knownIdentity is the bot identity of role's App that the daemon already holds, read with no
	// mint (workflowRuntime.knownIdentity), which a worker's launch reads for the other App's roles
	// before it asks identity; nil for a daemon with no workflow and in a test whose specs literal
	// leaves it out, whose launches read every identity through identity.
	knownIdentity func(role claim.Role) (runtime.GitIdentity, bool)
	// log is where a launch says what it carries less of than it would (roleIdentities); nil logs
	// to slog.Default, as a test's specs literal does.
	log *slog.Logger
	// repository is the repository an issue works and its default branch, as every launch reads it
	// (workflowRuntime.launchRepository: its record's, or, for an issue the workflow does not
	// record, the one Dispatch resolves it to, read and prepared at its first launch and kept while
	// the issue holds its workspace); nil for a daemon with no workflow, whose launches work no
	// repository.
	repository func(ctx context.Context, issue string) (ghrepo.Repository, string, error)
	// appOwners is the owner accounts the role's own GitHub App is installed on AND the role's gh
	// bundle holds a directory for, lowercased and sorted: the reach a worker's addressing names
	// beside its own issue's repository, and the controller's after its design gate steps
	// (RepositoriesClause). The two conditions are one rule, GitHubDirs': an installed owner the
	// last bundle left out — not installed, suspended, granted none, permissions refused, or a
	// login that is no directory name — holds no token this role could use, so naming it would
	// promise a reach the role does not have. Under runtime: kubernetes the Apps read are the
	// POD's (sandbox.PodAppRoles): an issue pod projects only the directories both Apps' bundles
	// hold, so an owner one App alone reaches is in no container of that pod and `legion gh`
	// refuses it there, while a tmux pane holds the role's own bundle and reads that App alone.
	// The daemon answers it from what GitHub last answered of that App's installations
	// (appauth.Installed, plan.appOwners): the boot's proof of the App's key first, then the last
	// gh bundle of it, which costs no read of its own on a launch. The daemon sets it at boot
	// whether or not a workflow is configured; a nil one, and a daemon with no GitHub Apps, names
	// no owner, as an App installed nowhere does.
	appOwners func(role claim.Role) []string
	// owner is the architect that owns a claim on an issue that is not its tree's root, as the
	// store records the architect claims of the issue's tree at the launch
	// (workflowRuntime.owningArchitect): what the claim's addressing names (ownerOf). It is nil
	// for a daemon with no workflow, which records no issue to walk, and in a test whose specs
	// literal leaves it out; every claim of such a specs is addressed to its tree's architect.
	owner func(ctx context.Context, project string, c supervise.Claim) (claim.Token, error)
	// runtimeName is cfg.Runtime.Name: a stale-plugin install (stalePluginInstall) is Kubernetes'
	// alone, since a tmux pane always resumes on the plugin installed on the daemon's host.
	runtimeName string
	// plugins is this daemon's resolved plugin pair under Kubernetes — the file's
	// `runtime.kubernetes.plugins`, or the binary's linked pair when the file names none — with
	// Contract already set to api.DaemonAPIVersion; nil when the daemon knows none
	// (LEGION-164's degraded mode). Never read under any other runtime.
	plugins *runtime.PluginInstall
	// backend is the backend a role runs on (config.Config.BackendFor); nil runs every role on Oh
	// My Pi.
	backend func(claim.Role) runtime.Backend
	// hostWorkspaces is the runtime leaving each claim's workspace to the daemon's own host
	// (runtime.Runtime.ProvisionsWorkspaces false: tmux). ensureWorkspace then provisions an
	// issue's workspace a launch finds missing or not checked out there (outbox.ensureWorkspace,
	// through the workflow); nil for a daemon with no workflow, whose launches work no repository.
	hostWorkspaces  bool
	ensureWorkspace func(ctx context.Context, issue string, repo ghrepo.Repository, defaultBranch string) error
}

// rolePromptPath is where a claim's role prompt is kept for every launch of it.
func rolePromptPath(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "prompts", string(token)+".md")
}

// ownersOf is the owner accounts role's own GitHub App is installed on (appOwners), and none
// for a specs built without that function, which is a test's literal: a launch is told its App
// reaches nothing rather than told nothing at all.
func (s specs) ownersOf(role claim.Role) []string {
	if s.appOwners == nil {
		return nil
	}
	return s.appOwners(role)
}

// SpawnSpec is the launch's secrets (launchSecrets: the Envoy bearer and the NATS nkey seed, each
// when the daemon has one), its prompt — the role prompt parts, the addressing sentence, the
// deployment instructions and the instructions of each arm the claim's unit was drawn into, each
// file read again first (copies), so a launch carries what the operator's files hold now
// — and its repository with that repository's default branch, whose workspace is on the daemon's
// host by then when the runtime leaves workspaces to the daemon
// (ensureWorkspace). The controller's launch (controllerSpawnSpec) is its own. Both name the
// backend the role runs on and are told its prompt part after the role's parts (launchBackend).
// Plugins carries the plugin pair a stale-plugin relaunch installs (stalePluginInstall), set the
// same way for both — the controller the daemon launches (`controller: daemon`) is covered like
// any claim.
// Env carries the stall limits the daemon's own environment holds, merged after the fork
// (stallLimitVariables), so the controller's launch is told them exactly as a worker's is.
func (s specs) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	s.copies.refresh(ctx)
	var spec runtime.SpawnSpec
	var err error
	if c.Role == claim.RoleController {
		spec, err = s.controllerSpawnSpec()
	} else {
		spec, err = s.workerSpawnSpec(ctx, c)
	}
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	// After the fork, so every launch is served by one seam, and after the git identity, which
	// replaces a worker's spec.Env rather than merging into it: the stall limits are merged into
	// whatever map the launch holds by then (stallLimitVariables).
	passthrough(spec.Env, stallLimitVariables...)
	var part string
	if spec.Backend, part, err = s.launchBackend(c.Role); err != nil {
		return runtime.SpawnSpec{}, err
	}
	if part != "" {
		spec.Prompt.RolePromptPaths = append(spec.Prompt.RolePromptPaths, part)
	}
	spec.Plugins = s.stalePluginInstall(c, spec.Backend)
	return spec, nil
}

// launchBackend is the backend role runs on (backend; Oh My Pi when nil) and the path of the
// daemon prompt part its entry names (runtime.Backends), "" when it names none.
func (s specs) launchBackend(role claim.Role) (runtime.Backend, string, error) {
	backend := runtime.BackendOmp
	if s.backend != nil {
		backend = s.backend(role)
	}
	info, err := runtime.LookupBackend(backend)
	if err != nil {
		return "", "", fmt.Errorf("the %s's backend: %w", role, err)
	}
	if info.PromptPart == "" {
		return backend, "", nil
	}
	if s.prompts == nil {
		return "", "", errors.New("the daemon prompt bundle was not constructed at boot")
	}
	return backend, s.prompts.Part(info.PromptPart), nil
}

// workerSpawnSpec is SpawnSpec's own build for every claim but the controller's. It is the one
// launch that may run in an experiment: the arms the claim's unit was drawn into (variants),
// which decide both the overlays each runtime names last in PI_CONFIG_FILES
// (runtime.SpawnSpec.Variants) and the instructions its prompt ends with. It is the one launch
// that may stamp an issue key into its commits too (issueTrailer), since it is the one that
// works a repository.
func (s specs) workerSpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	promptPaths, err := s.rolePromptPaths(c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	spec := runtime.SpawnSpec{
		Env:     map[string]string{},
		Secrets: maps.Clone(s.secrets),
	}
	if s.repository != nil {
		if spec.Repository, spec.DefaultBranch, err = s.repository(ctx, c.Issue); err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the repository of %s: %w", c.Token, err)
		}
	}
	spec.IssueTrailer = s.issueTrailer(spec.Repository, c.Issue)
	// Every launch, not only a claim's first: a claim resumed, relaunched or recovered on this host
	// may have no workspace here — its issue ran in another runtime's pods before, or its tree's
	// linger removed it — and a pane cannot start without one. Provisioning checks out the issue's
	// branch legion/<KEY> when GitHub has it, so whatever the claim pushed is there.
	if s.hostWorkspaces && s.ensureWorkspace != nil && !spec.Repository.IsZero() {
		if err := s.ensureWorkspace(ctx, c.Issue, spec.Repository, spec.DefaultBranch); err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the workspace of %s: %w", c.Token, err)
		}
	}
	architect, err := s.ownerOf(ctx, c)
	if err != nil {
		return runtime.SpawnSpec{}, fmt.Errorf("the addressing of %s: %w", c.Token, err)
	}
	addressing := addressingFragment(s.project, c, architect, spec.Repository, spec.DefaultBranch, s.ownersOf(c.Role))
	// Every architect owns a plan, whose steps name the roles this sentence lists; it stands
	// before the tree architect's design-gate line, which ends that addressing.
	if c.Role == claim.RoleArchitect {
		addressing += " " + DeploymentRolesFragment(s.roles)
	}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		addressing += " " + DesignGateFragment(s.designSteps)
	}
	if c.Role == claim.RoleArchitect {
		addressing += " " + ProductionCheckFragment(s.productionCheck)
	}
	spec.Prompt = runtime.PromptParts{
		RolePromptPaths:            promptPaths,
		Addressing:                 addressing,
		DeploymentInstructionsPath: s.instructions,
	}
	// The arms are read from the record alone: a unit the daemon drew into an arm keeps that
	// arm's overlays and its instructions while the project's file names them, whatever the
	// experiment's mode is now — the mode decides which new units are drawn, and only taking the
	// arm out of the file takes the files away. An experiment whose `roles` leaves this claim's
	// role out gives this launch none of its arms, while the unit keeps the arm it drew
	// (config.Experiment.Reaches). A specs with no reader (variants nil, a test's) asks
	// nothing, so each of its launches runs in no experiment, and an arm that names no
	// instructions file appends nothing, its roles getting the overlays alone.
	if s.variants != nil {
		arms, err := s.variants(ctx, c.Tree, c.Issue, c.Role)
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the experiment variants of %s: %w", c.Token, err)
		}
		spec.Variants, spec.Prompt.VariantInstructionsPaths = arms.Variants, arms.Instructions
	}
	// The claim's own identity is the six variables its commits carry (GitIdentity.Env), and a
	// failed mint of it fails the launch. Every role's of claim.Roles is the table the push step of
	// `legion jj` reads to set the author of a commit a push sends from the role its Legion-Role
	// trailer names, where the step's rules let it (runtime.RoleIdentitiesVariable): an issue's
	// roles share one working copy, so a commit's author may be another role's App. The table is
	// built with the claim's own identity for its own App's roles and with one resolution of the
	// other App's (roleIdentities); when the other App's identity cannot be resolved, the launch
	// goes on with a table of its own App's roles alone, and a commit whose trailer names a role
	// left out keeps its author. A launch whose context ended during that resolution fails, as one
	// whose context ended during the claim's own mint does: that is no failure of the other App.
	// The controller's launch (controllerSpawnSpec) asks for no identity and carries no table.
	if s.identity != nil {
		id, err := s.identity(ctx, c.Role, spec.Repository)
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the git identity of %s: %w", c.Token, err)
		}
		identities, err := s.roleIdentities(ctx, c, spec.Repository, id)
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the role identities of %s: %w", c.Token, err)
		}
		spec.Env = id.Env()
		spec.Env[runtime.RoleIdentitiesVariable] = runtime.RoleIdentities(identities)
	}
	// After the git identity, which replaces spec.Env rather than merging into it: the refusal
	// cooldowns are merged into whatever map the launch holds by then. The stall limits are
	// merged after the fork, onto every launch (SpawnSpec).
	passthrough(spec.Env, refusalCooldownVariables...)
	// A role the deployment defines runs the model its settings give it; Legion's roles name none
	// and run the profile's own (runtime.SpawnSpec.Model).
	if role, defined := s.roleOf(c.Role); defined && !claim.IsBuiltin(c.Role) {
		spec.Model = role.Model
	}
	return spec, nil
}

// roleIdentities is the table of every role's identity a launch of c carries
// (runtime.RoleIdentitiesVariable), given own, c's own identity. A role of c's own App takes own,
// since one App commits as one identity, and asks nothing more. The other App's roles take that
// App's identity, resolved once for all of them (otherAppIdentity). When it cannot be resolved,
// the table leaves them out and the daemon logs one warning naming c, that App and the error, so
// a failed mint of one App's lease fails no launch of the other App's role, and the push step
// leaves the author of a commit whose trailer names a role left out as it is. A resolution that
// failed once ctx had ended is the launch's own end rather than the other App's failure: it is
// the error, and nothing is logged.
func (s specs) roleIdentities(ctx context.Context, c supervise.Claim, repository ghrepo.Repository, own runtime.GitIdentity) (map[claim.Role]runtime.GitIdentity, error) {
	app := appauth.AppRoleFor(c.Role)
	identities := make(map[claim.Role]runtime.GitIdentity, len(claim.Roles))
	var others []claim.Role
	for _, role := range claim.Roles {
		if appauth.AppRoleFor(role) == app {
			identities[role] = own
		} else {
			others = append(others, role)
		}
	}
	if len(others) == 0 {
		return identities, nil
	}
	other, err := s.otherAppIdentity(ctx, others[0], repository)
	switch {
	case err == nil:
		for _, role := range others {
			identities[role] = other
		}
	case ctx.Err() != nil:
		return nil, fmt.Errorf("the git identity of the %s App: %w", appauth.AppRoleFor(others[0]), err)
	default:
		log := s.log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("legion launch carries no identity of the other App's roles: its role identities hold its own App's roles alone",
			"claim", c.Token, "app", appauth.AppRoleFor(others[0]), "error", err)
	}
	return identities, nil
}

// otherAppIdentity is the identity of role's App, the App other than the claim's own: first the
// one the daemon already holds (knownIdentity), which costs no mint, and only when it holds none
// a lease's (identity, which reuses that App's cached lease: workflowRuntime.identity).
func (s specs) otherAppIdentity(ctx context.Context, role claim.Role, repository ghrepo.Repository) (runtime.GitIdentity, error) {
	if s.knownIdentity != nil {
		if id, known := s.knownIdentity(role); known {
			return id, nil
		}
	}
	return s.identity(ctx, role, repository)
}

// ownerOf is the architect c's addressing names (addressingFragment), read again at each launch:
// for a claim on its tree's root, whatever its role, the tree's architect, which is the root's
// own; for a claim on any other issue, the end of the owner walk over the claims the store
// records now (owner), so a planner launched while its child's own architect holds its role is
// told that architect, and one launched while that architect does not hold its role (it failed,
// for example) is told the architect above it. A specs with no owner reader addresses every claim
// to its tree's architect. A read that fails fails the launch, as a repository read does.
func (s specs) ownerOf(ctx context.Context, c supervise.Claim) (claim.Token, error) {
	if s.owner == nil || claim.IsTreeRoot(c.Issue, c.Tree) {
		return claim.NewToken(s.project, c.Tree, claim.RoleArchitect)
	}
	return s.owner(ctx, s.project, c)
}

// owningArchitect is specs.owner: the architect that owns c, a claim on an issue that is not its
// tree's root, as the store records the architect claims of the issue's tree now
// (record.ReadOwnerWalk). It is where a worker-died about c's role goes, so the architect the
// daemon tells when this claim fails is the one it answers to. For a phase worker the walk starts
// at its issue, so it is the issue's own architect while that claim holds its role, else the
// nearest one above it that does, else the tree root's; for the issue's own architect it starts
// at the issue's parent (record.NoticeStartsAtParent), which is the architect above it. An issue
// the workflow does not record, such as an operator's claim, has no tree to walk, so its claim is
// its tree's architect's. project is the claim project token every token of the launch is spelled
// in. It reads in a read-only transaction of its own, as the supervisor's predicates read a
// record.
func (w *workflowRuntime) owningArchitect(ctx context.Context, project string, c supervise.Claim) (claim.Token, error) {
	owner, err := claim.NewToken(project, c.Tree, claim.RoleArchitect)
	if err != nil {
		return "", err
	}
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := w.records.Issue(ctx, tx, c.Issue)
		if err != nil || issue == nil {
			return err
		}
		walk, err := record.ReadOwnerWalk(ctx, w.records, tx, project, issue.Tree)
		if err != nil {
			return err
		}
		owner, err = walk.Architect(*issue, c.Role == claim.RoleArchitect)
		return err
	}); err != nil {
		return "", fmt.Errorf("read the architect that owns %s: %w", c.Token, err)
	}
	return owner, nil
}

// passthrough merges into env each of names the daemon's own environment holds, and no other: a
// role told none of a group runs on the defaults its plugin holds. Both runtimes forward spec.Env
// verbatim — a pane's -e pairs (runtime/tmux's panePairs) and a pod launcher's start command
// (runtime/sandbox's mainEnvironment) — so these two calls are the whole seam.
func passthrough(env map[string]string, names ...string) {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}
}

// issueTrailer is the issue key a launch on repository stamps into its commits
// (runtime.SpawnSpec.IssueTrailer, which each runtime tells its agent as LEGION_ISSUE_TRAILER):
// the claim's own issue when the project names that repository in commit_issue_trailer_repos, and
// "" for every other repository and for a launch that works none. The two names are compared as
// every repository comparison of the daemon is, by the daemon's one equivalence (s.same): without
// case and through the deployment's configured renames, so a claim on the name a transfer moved
// a repository away from stamps exactly as one on the name the project wrote. A specs with no
// equivalence stamps nothing.
func (s specs) issueTrailer(repository ghrepo.Repository, issue string) string {
	if repository.IsZero() || s.same == nil {
		return ""
	}
	name := repository.String()
	if slices.ContainsFunc(s.commitIssueTrailerRepos, func(named ghrepo.Repository) bool { return s.same(named.String(), name) }) {
		return issue
	}
	return ""
}

// stallLimitVariables are the seven numbers a Legion session's turn runs on, in name order: how
// many delivered reminders in a row change nothing before the architect is told, the bound for a
// tool call no deadline judges, the grace over the deadline a call itself declared, how long the
// worker waits for a reminder after a settle, how long after a resume on an open phase, the gap
// between the recovery ladder's steps, and how long a reply that said what it waits on buys. A
// session told none of them runs on the defaults its plugin holds
// (packages/pi-shared/src/stall-limits.ts), so the daemon passes exactly the ones its own
// environment holds and no more. Both runtimes forward spec.Env verbatim — a pane's -e pairs
// (runtime/tmux's panePairs) and a pod launcher's start command (runtime/sandbox's
// mainEnvironment) — so SpawnSpec, after its fork, is the one seam that serves them all, the
// controller's launch among them.
var stallLimitVariables = []string{
	"LEGION_STALL_ARCHITECT_AFTER",
	"LEGION_STALL_CALL_BOUND_MS",
	"LEGION_STALL_CALL_GRACE_MS",
	"LEGION_STALL_REMINDER_MS",
	"LEGION_STALL_RESUME_MS",
	"LEGION_STALL_STEP_MS",
	"LEGION_STALL_WAITING_MS",
}

// refusalCooldownVariables are the two numbers a Legion role's model fallback runs on
// (INFRA-956 R1), in name order: the longest the session stays on the weaker model after a
// provider's content filter refused a turn, and how long the first such wait is. They ride the
// same seam as the stall limits, which SpawnSpec merges after the fork, and reach a worker's
// launch from workerSpawnSpec, which is why they are a list of their own: these are no stall
// limit, and a list whose name said so would lie. A worker told neither runs on the defaults its
// plugin holds (packages/pi-shared/src/fallback-cooldown.ts); a live proof sets both to a few
// seconds.
//
// They reach the daemon's own controller as well (controllerSpawnSpec). The controller is a
// Legion role whose session the same model fallback serves — one chain, one cooldown machine, in
// the plugin every Oh My Pi role loads (packages/pi-legion/extensions/legion.ts) — so a
// deployment that shortens the wait for its workers and not for its controller would leave the
// controller on the weaker model for the default hour.
var refusalCooldownVariables = []string{
	"LEGION_REFUSAL_COOLDOWN_MAX_MS",
	"LEGION_REFUSAL_COOLDOWN_MS",
}

// stalePluginInstall is spec.Plugins for c's launch (LEGION-164): the daemon's resolved pair,
// with Contract already api.DaemonAPIVersion, iff the launch runs on Oh My Pi, the runtime is
// Kubernetes, c has registered a session, and its recorded contract differs from this daemon's —
// never for a launch that is not replacing a process on another plugin contract, and never when
// the daemon knows no pair (s.plugins nil, the degraded mode). A fresh copy, so a caller mutating
// one launch's spec never reaches another's.
//
// backend is the launch's, as SpawnSpec resolved it (launchBackend). A Claude Code launch gets
// nothing: that install is `@sjawhar/pi-legion` and `@sjawhar/pi-envoy` from npm
// (runtime/sandbox's plugins.go), no workflow releases claude-envoy, and a Claude worker's plugin
// is the image's — so a plugin-contract change for such a role is answered by a new image, not by
// an install. Such a claim whose recorded contract differs still relaunches: it is the image's
// plugin that decides, and the image probe is what refuses a wrong one. The daemon-launched
// controller runs on Oh My Pi (runtime.Backends runs Claude Code for phase workers alone), so its
// launch is covered here exactly as before.
func (s specs) stalePluginInstall(c supervise.Claim, backend runtime.Backend) *runtime.PluginInstall {
	if runtime.Of(backend) == runtime.BackendClaude {
		return nil
	}
	if s.runtimeName != "kubernetes" || c.Session == "" || c.PluginContract == api.DaemonAPIVersion() || s.plugins == nil {
		return nil
	}
	install := *s.plugins
	return &install
}

// controllerSpawnSpec is the daemon's controller's launch (`controller: daemon`): its role part and
// the headless part; as its addressing the design-gate steps the project gates, then the reach
// its GitHub App gives it as a sentence of its own (RepositoriesSentence, which also says when
// its App is installed on no owner, so that it has no GitHub owner) and, when it names any, its
// Slack channels; then the deployment instructions and the launch secrets. The operator's
// controller (`legion controller start`) is told the steps and the reach the same way, never
// channels, since only the daemon's may have channels (config.checkSlackChannels). The channels
// reach its environment as LEGION_SLACK_CHANNELS from the runtime
// (sandbox.Options.ControllerSlackChannels), which compares them with the list a running
// controller was launched under and relaunches it when they change; a spec cannot set that
// variable.
// It has no repository and no git identity, nor the table of every role's identity
// (runtime.RoleIdentitiesVariable): the controller works Dispatch, never a checkout, and commits
// and pushes nothing, so it stamps no issue key into a commit either (runtime.SpawnSpec.IssueTrailer,
// which validateControllerShape refuses on a controller's spec). It runs in no experiment and is
// told no variant instructions whatever the modes are: an experiment's unit is a tree or an
// issue of one, and the controller runs in neither.
//
// Of the passthrough variables it carries the two refusal cooldowns itself: the model fallback
// those two bound serves every Legion role, the controller's session among them
// (refusalCooldownVariables). The stall limits reach it from SpawnSpec, after the fork, as they
// reach a worker's launch (stallLimitVariables).
func (s specs) controllerSpawnSpec() (runtime.SpawnSpec, error) {
	if s.prompts == nil {
		return runtime.SpawnSpec{}, errors.New("the daemon prompt bundle was not constructed at boot")
	}
	paths, err := s.prompts.ControllerPromptPaths(true)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	addressing := DesignGateFragment(s.designSteps) + " " + RepositoriesSentence(s.ownersOf(claim.RoleController))
	if len(s.slackChannels) > 0 {
		addressing += " " + SlackChannelsFragment(s.slackChannels)
	}
	env := map[string]string{}
	passthrough(env, refusalCooldownVariables...)
	return runtime.SpawnSpec{
		Env:     env,
		Secrets: maps.Clone(s.secrets),
		Prompt: runtime.PromptParts{
			RolePromptPaths:            paths,
			Addressing:                 addressing,
			DeploymentInstructionsPath: s.instructions,
		},
	}, nil
}

// rolePromptPaths keeps an explicit operator prompt as a narrow test override. Every ordinary
// claim uses the role prompts daemon boot snapshotted below its state directory (prompts.New):
// Legion's roles their composed parts (prompts.Composer.Compose), a role the deployment defines
// its own prompt file between the headless mechanics and Legion's worker part
// (prompts.Composer.ComposeStep). A role neither names is refused, with no prompt.
func (s specs) rolePromptPaths(c supervise.Claim) ([]string, error) {
	override := rolePromptPath(s.stateDir, c.Token)
	if _, err := os.Stat(override); err == nil {
		return []string{override}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("the role prompt of %s: %w", c.Token, err)
	}
	if s.prompts == nil {
		return nil, errors.New("the daemon prompt bundle was not constructed at boot")
	}
	if claim.IsBuiltin(c.Role) {
		parts, err := s.prompts.Compose(c.Role, claim.IsTreeRoot(c.Issue, c.Tree))
		if err != nil {
			return nil, err
		}
		return parts.RolePromptPaths, nil
	}
	role, defined := s.roleOf(c.Role)
	if !defined {
		return nil, fmt.Errorf("the role prompt of %s: the deployment defines no role %s", c.Token, c.Role)
	}
	return s.prompts.ComposeStep(role.PromptFile).RolePromptPaths, nil
}

// roleOf is the role of the deployment named name, and whether the deployment defines one.
func (s specs) roleOf(name claim.Role) (config.Role, bool) {
	for _, role := range s.roles {
		if role.Name == name {
			return role, true
		}
	}
	return config.Role{}, false
}

// AddressingFragment is addressingFragment for a claim whose owner is its tree's architect, a pane
// on its tree's root: it names the tree's architect, spelled from project and c.Tree. It is
// exported for the rigs under packages/pi-legion/scripts, which tell a worker what a pane is told.
// Its last parameter became the owners the role's App reaches with INFRA-2543, where it was a view
// of the project's repositories before, so a label checked out before that change does not build
// against a rig that passes owners.
func AddressingFragment(project string, c supervise.Claim, repo ghrepo.Repository, defaultBranch string, owners []string) (string, error) {
	architect, err := claim.NewToken(project, c.Tree, claim.RoleArchitect)
	if err != nil {
		return "", fmt.Errorf("the addressing of %s: %w", c.Token, err)
	}
	return addressingFragment(project, c, architect, repo, defaultBranch, owners), nil
}

// addressingFragment is the sentence that tells an agent where it and its peers are reached: its
// own role topic, the topic of architect, and the project controller's, spelled from the tokens so
// the model never hand-encodes one; then, when repo is not the zero repository, the issue's own
// repository and default branch and the reach its role's own App gives it (RepositoriesClause) —
// left off entirely for a daemon that runs no workflow, whose claims name no repository.
//
// architect is the architect that owns c (specs.ownerOf): its tree's architect for a claim on the
// tree root, and for a claim on any other issue the end of the owner walk read at the launch. The
// architect of such an issue, a sub-architect, owns that issue itself, so its architect is the one
// above it, and the sentence names it that way.
//
// The copy of the claim a launch is built from (supervise's spawnSpec) can carry two marks, and
// each adds a sentence. A launch that follows a forget (c.SessionLost) starts a fresh session on a
// claim that had one, and is told so in words the forget's cause chooses, which name the session
// forgotten (c.Abandoned.Session) where the cause is not that no store holds it. A launch that has
// Oh My Pi resume the session by its id itself, through a pod launcher older than the lookup
// (c.AgentResumeGeneration equal to c.Generation, which only that launch's copy has), is told that
// its workspace may have been recreated: that launcher reads no row of the session, so its word
// that the workspace was not recreated was not checked (dispatch://INFRA-4108 R3). Such a workspace
// holds what was pushed to the claim's own branch, which the sentence names: the branch of the task
// its pending delivery is of (supervise.Delivery.Branch), and the issue's own where that names none
// or the claim holds no delivery, as the replacement of an abandoned session is told (supervise's
// abandonedTask). Where nothing was pushed it holds what that branch started from (the outbox's
// taskBranch): for a task that needs none, its base (record.StoredPlan.BaseOf), the default branch
// for a task of the top level and its task's branch for a part; for a task with needs, the joined
// work of its needs. The claim holds neither, so the sentence names none, and names no default
// branch. The guidance is that of the plugin's own notice of a recreated workspace
// (packages/pi-shared/src/workspace-recreated.ts). Each sentence that names a session quotes its id
// as a Go string (%q), as the workflow's message of a forgotten session does: the id is what an
// agent's registration chose, and the quotes bound it in a text the agent reads.
func addressingFragment(project string, c supervise.Claim, architect claim.Token, repo ghrepo.Repository, defaultBranch string, owners []string) string {
	owner := "the architect that owns your issue is"
	if c.Role == claim.RoleArchitect && !claim.IsTreeRoot(c.Issue, c.Tree) {
		owner = "the architect above you is"
	}
	fragment := fmt.Sprintf("Legion addressing: your role topic is `%s%s`; %s `%s%s`; "+
		"the project's controller is `%s%s`; a sibling role on your issue is your topic with the trailing `-<role>` replaced.",
		notify.RoleTopicPrefix, c.Token, owner, notify.RoleTopicPrefix, architect, notify.RoleTopicPrefix, claim.ControllerToken(project))
	// The agent a launch after a forget starts is a fresh session on a claim that had one: nothing in
	// it says an earlier session existed, so it would read an empty transcript as a fresh start and
	// redo work that is already pushed (dispatch://INFRA-714 R8). A session the launcher could not
	// look up was not deleted, and a store may hold it still, so its words say neither that it was
	// lost nor that no store holds it.
	const readFirst = "Read your issue and the committed handoffs in your workspace before you continue. " +
		"Then tell the architect named above what you found, unless you are your tree's root architect."
	switch c.SessionLost {
	case "":
	case supervise.SessionLossNotResumedByID:
		fragment += fmt.Sprintf(" The pod's launcher, which is older than this release, could not look the session %q this claim recorded up by its id, "+
			"and Oh My Pi did not resume it by that id, so this is a new session: the turns, tool calls and subagents of your earlier work are not in it, "+
			"and only what you pushed and committed reaches you. ", c.Abandoned.Session) + readFirst
	case supervise.SessionLossClaudeNoFile:
		fragment += fmt.Sprintf(" This Claude Code claim recorded the session %q with no transcript file, so the launch could not resume it and this is a new session: "+
			"the turns, tool calls and subagents of your earlier work are not in it, and only what you pushed and committed survives. ", c.Abandoned.Session) + readFirst
	default:
		fragment += " The session this claim recorded was found in no session store, so this is a new session: " +
			"the turns, tool calls and subagents of your earlier work are gone, and only what you pushed and committed survives. " + readFirst
	}
	if c.AgentResumeGeneration != 0 && c.AgentResumeGeneration == c.Generation {
		branch := claim.IssueBranch(c.Issue)
		if c.Pending != nil && c.Pending.Branch != "" {
			branch = c.Pending.Branch
		}
		fragment += fmt.Sprintf(" This launch has Oh My Pi resume the session %q by its id itself. The pod's launcher is older than this release and did not read "+
			"when that session was last written, so its word that your workspace was not recreated was not checked: your workspace may have been recreated "+
			"since the session last ran. If it was, it holds what was pushed to %s (or, if nothing was, the base that branch started from). Anything you had not "+
			"pushed is gone. Before you continue, check the workspace against what you remember doing, and read the last handoff recorded on %s again with "+
			"`dispatch handoff-read --issue %s --phase <word>`; Dispatch keeps it, whatever the workspace holds.",
			c.Session, branch, c.Issue, c.Issue)
	}
	if repo.IsZero() {
		return fragment
	}
	return fragment + " " + fmt.Sprintf("Your issue's repository is %s (default branch %s); %s",
		repo.String(), defaultBranch, RepositoriesClause(owners))
}

// RepositoriesClause tells an agent how far its GitHub token reaches, as a clause that ends in a
// full stop and starts in lower case, to follow a worker's `; `: "your role's GitHub App is
// installed on the owners a, b, and its token reaches every repository of those owners." owners
// are the owner accounts of the role's own App, lowercased and sorted (specs.appOwners), and not
// the owners of any list of the project's repositories: since INFRA-2543 a role's reach is its
// App's installations and no repository list bounds any of it, so naming such a list's owners
// told an agent its reach was smaller than it is wherever the App reaches an owner the list held
// nothing of — the case cmd/legion's own gh tests pin, where a repository of such an owner is
// answered from that owner's directory.
//
// It names no repository list: a role's token is its App's own, with no repository narrowing,
// so the project's repositories are no bound on the work and the list would be a whole
// installation's worth of names — 216 of them for this deployment's largest owner when that
// spec was written. What the agent needs is the account its token acts on and that it reaches
// every repository there; its own issue's repository is named before this clause, and its gh
// bundle holds a directory per owner.
//
// An App installed on no owner, which a deployment with no installation has, is told so
// plainly, with the GitHub owner the role has, none: a clause that named nothing would read as one
// the daemon failed to fill, and a controller whose review App is installed on no account is
// launched on an empty gh bundle (runtime.ErrNoGitHubOwner), so this clause is what tells it that
// its gh acts as nobody (dispatch://INFRA-4401 A3).
func RepositoriesClause(owners []string) string {
	if len(owners) == 0 {
		return "your role's GitHub App is installed on no owner, so you have no GitHub owner and its token reaches no repository."
	}
	if len(owners) == 1 {
		return "your role's GitHub App is installed on the owner " + owners[0] + ", and its token reaches every repository of that owner."
	}
	return "your role's GitHub App is installed on the owners " + strings.Join(owners, ", ") + ", and its token reaches every repository of those owners."
}

// RepositoriesSentence is RepositoriesClause as a sentence of its own, for the controller's
// addressing, which follows the design gate steps' sentence. The controller works Dispatch and
// no checkout, so it is told the reach without an issue's repository before it.
func RepositoriesSentence(owners []string) string {
	clause := RepositoriesClause(owners)
	return strings.ToUpper(clause[:1]) + clause[1:]
}

// DesignGateFragment is the sentence a tree's root architect is told after its addressing, and
// the controller as its launch addressing: the design-gate steps this deployment gates, in the
// order it gates them, each in backticks, which is the "Design gate steps" line the shared role
// prompts read. A deployment that gates no step is told so by name, since a line that named
// nothing would read as a line the daemon failed to fill. What each step asks of the architect,
// and which document each records, is in its role parts (prompts/roles/architect-root.md and
// prompts/go/architect-root.md).
//
// The steps the line names are the ones every root must register. A deployment that gates spec
// and not criteria holds a root on criteria too once its architect registers the Verification
// document as that step (record.TreeSteps), so its line says that as a sentence of its own: a
// step no root must register, and that holds the root that registers it.
func DesignGateFragment(steps []record.DesignStep) string {
	if len(steps) == 0 {
		return "Design gate steps: none."
	}
	quoted := make([]string, len(steps))
	for i, step := range steps {
		quoted[i] = "`" + string(step) + "`"
	}
	line := fmt.Sprintf("Design gate steps: %s.", strings.Join(quoted, ", "))
	if record.JoinsByRegistration(steps, record.DesignStepCriteria) {
		line += " A root that registers its Verification document as `criteria` is held on that step too."
	}
	return line
}

// ProductionCheckFragment is the sentence every architect, the root's and a sub-architect's alike,
// is told last in its addressing: the project's `production_check` (check) as the file writes it
// (config.Project.ProductionCheckWord), the role that works the production check a merge closing
// one of the project's issues opens, or `off` for the empty role. It is the "Production check" line
// the architect prompts read (prompts/roles/architect-root.md, prompts/roles/architect.md and
// prompts/go/architect-common.md): where it names a role, the architect writes one `after_merge`
// check for that check in each Verification document it writes, while the issue is open, since a
// closed issue takes no new document; where it reads `off`, it writes none (dispatch://INFRA-175
// R7). A root architect is told it after the design gate steps, and a sub-architect, which is
// told no steps, after its addressing. The controller writes no Verification document and is
// told nothing of it.
func ProductionCheckFragment(check claim.Role) string {
	return "Production check: " + config.Project{ProductionCheck: check}.ProductionCheckWord() + "."
}

// DeploymentRolesFragment is the sentence every architect is told after its addressing: the roles
// a step of its plan may name, each with the type of its step — Legion's five (BuiltinRoles,
// whose types it reads), then each role the deployment defines (roles, config.Config.Roles) in the
// file's order, `<name> (<type>)`, with ` with the model <model>` where the role's settings give
// one, the model its agent starts with. It is exported for the rigs under packages/pi-legion,
// which restate it, as AddressingFragment is.
func DeploymentRolesFragment(roles []config.Role) string {
	builtin := stepplan.BuiltinRoles()
	entries := make([]string, 0, len(stepRoleOrder)+len(roles))
	for _, name := range stepRoleOrder {
		entries = append(entries, fmt.Sprintf("%s (%s)", name, builtin[name].Type))
	}
	for _, role := range roles {
		entry := fmt.Sprintf("%s (%s)", role.Name, role.Type)
		if role.Model != "" {
			entry += " with the model " + role.Model
		}
		entries = append(entries, entry)
	}
	return "The roles of this deployment, each with the type of its step: " + strings.Join(entries, ", ") + "."
}

// stepRoleOrder is the order the owner is told Legion's five roles of a step in: the line's own.
var stepRoleOrder = []string{"planner", "implementer", "tester", "reviewer", "acceptance"}

// SlackChannelsFragment names the project's Slack channels, the list the controller answers
// mentions from: its plugin subscribes to each one's mention topic itself, while the session holds
// the controller role, so the model subscribes to nothing. What the controller does with a mention,
// and with a delivery from a channel the list does not name, is the skill's.
func SlackChannelsFragment(channels []string) string {
	named := make([]string, 0, len(channels))
	for _, channel := range channels {
		named = append(named, "`"+channel+"`")
	}
	return "Slack channels: " + strings.Join(named, ", ") +
		": your session hears each one's app mentions while it holds the controller role; answer each in its thread, " +
		"and nothing from any other channel (skill://legion-controller/references/slack-mentions.md)."
}
