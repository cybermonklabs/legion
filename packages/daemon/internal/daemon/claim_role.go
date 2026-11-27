package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// planRoleReader is the daemon's one read of a role an owner's plan defines: the role named role
// of the plan in force of issue (record.Store.PlanRoleOf, at the issue's current generation), and
// whether that plan defines one. The store kept the role when the version that defines it was
// taken, so a launch never waits on Dispatch.
type planRoleReader func(ctx context.Context, issue string, role claim.Role) (record.PlanRole, bool, error)

// planRoleIn is the reader for a caller that already holds tx, a transaction of the daemon's
// store: the role is read through tx, since the pool refuses a caller that holds a connection a
// second one of its own (store.ErrNestedAcquire). The state's snapshot is such a caller
// (stateNoPluginInstall).
func planRoleIn(tx pgx.Tx) planRoleReader {
	records := record.NewStore()
	return func(ctx context.Context, issue string, role claim.Role) (record.PlanRole, bool, error) {
		defined, found, err := records.PlanRoleOf(ctx, tx, issue, string(role))
		if err != nil {
			return record.PlanRole{}, false, fmt.Errorf("the role %s of the plan of %s: %w", role, issue, err)
		}
		return defined, found, nil
	}
}

// planRoleOf is planRoleIn in a transaction of its own on st, as the supervisor's predicates read
// a record (launchVariants).
func planRoleOf(st *store.Store) planRoleReader {
	return func(ctx context.Context, issue string, role claim.Role) (defined record.PlanRole, found bool, err error) {
		err = store.TransactWith(ctx, st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			defined, found, err = planRoleIn(tx)(ctx, issue, role)
			return err
		})
		return defined, found, err
	}
}

// claimRole is what a claim's role is on its issue, resolved once (resolveClaimRole) for
// everything a launch derives from it: its type, its model and its harness (def), whose
// definition the name takes (source: Legion's, the issue's plan's or the deployment's, by
// stepplan.SourceOf), its prompt (the plan's text, or the deployment's prompt file), and the App it
// acts as (appauth.AppRoleFor). A role nobody defines resolves with source SourceNone and no App,
// and its launch is refused (specs.rolePromptPaths).
type claimRole struct {
	def        stepplan.RoleDef
	source     stepplan.Source
	prompt     string
	promptFile string
	app        appauth.AppRole
}

// resolveClaimRole is the one resolution of a claim's role on its issue, which the launch, its git
// identity, its addressing, the stall backstop and the outbox's check of a running process all
// read. The plan is read once, through read, and only for a role that is none of Legion's on a
// claim of an issue; a nil read reads none. Whose definition the name takes is stepplan.RoleOf's,
// with the deployment's role of the name from roles, on the harness backendFor names; a role of a
// plan runs on Oh My Pi whatever backendFor says of its name.
func resolveClaimRole(ctx context.Context, read planRoleReader, roles []config.Role, backendFor func(claim.Role) runtime.Backend,
	issue string, role claim.Role) (claimRole, error) {
	var planned *stepplan.PlanRole
	if read != nil && issue != "" && !stepplan.IsLegionName(string(role)) {
		row, found, err := read(ctx, issue, role)
		if err != nil {
			return claimRole{}, err
		}
		if found {
			planned = &stepplan.PlanRole{Name: row.Role, Type: stepplan.Type(row.Type), Model: row.Model, Prompt: row.Prompt}
		}
	}
	var d stepplan.Deployment
	var deployed config.Role
	apps := appauth.DeploymentApps{}
	for _, defined := range roles {
		if defined.Name == role {
			deployed = defined
			d.Roles = map[string]stepplan.RoleDef{string(role): {Name: string(role), Type: defined.Type, Model: defined.Model}}
			apps = deploymentApps([]config.Role{defined})
		}
	}
	resolved := claimRole{}
	resolved.def, resolved.source = stepplan.RoleOf(string(role), planned, d)
	switch resolved.source {
	case stepplan.SourcePlan:
		resolved.prompt = planned.Prompt
	case stepplan.SourceDeployment:
		resolved.promptFile = deployed.PromptFile
	}
	if resolved.source != stepplan.SourcePlan {
		resolved.def.Backend = string(runtime.BackendOmp)
		if backendFor != nil {
			resolved.def.Backend = string(backendFor(role))
		}
	}
	if resolved.source != stepplan.SourceNone {
		app, err := appauth.AppRoleFor(role, apps, appauth.Planned{Defines: resolved.source == stepplan.SourcePlan, Type: resolved.def.Type})
		if err != nil {
			return claimRole{}, err
		}
		resolved.app = app
	}
	return resolved, nil
}

// fingerprint is the definition a launch of a role of a plan runs, as the supervisor keeps it
// (runtime.SpawnSpec.RoleDefinition): a hash of the role's type, its model and its prompt, which
// any change of the role block changes; "" for a role no plan defines.
func (r claimRole) fingerprint() string {
	if r.source != stepplan.SourcePlan {
		return ""
	}
	sum := sha256.Sum256([]byte(string(r.def.Type) + "\x00" + r.def.Model + "\x00" + r.prompt))
	return hex.EncodeToString(sum[:])
}

// roleApps is the one value every reader of a claim's App holds (appauth.RoleApps): deployed, the
// App of each role the deployment defines (deploymentApps), and read, the daemon's one reader of a
// role of a plan, read for the role's type, so a role of an issue's plan acts as the App of its
// type. The boot builds it once, over the store it opened (run), and hands it to the workflow
// (openWorkflow), whose identities read it, and to the runtime it builds (runtimeFactory), whose
// gh credential and each walk's memo of bundles read it.
func roleApps(deployed appauth.DeploymentApps, read planRoleReader) appauth.RoleApps {
	return appauth.RoleApps{Deployed: deployed, Plan: func(ctx context.Context, issue string, role claim.Role) (appauth.Planned, error) {
		defined, found, err := read(ctx, issue, role)
		if err != nil {
			return appauth.Planned{}, err
		}
		return appauth.Planned{Defines: found, Type: stepplan.Type(defined.Type)}, nil
	}}
}

// roleBackend is the harness role runs on for a claim of issue (roleBackends).
type roleBackend func(ctx context.Context, issue string, role claim.Role) (runtime.Backend, error)

// roleBackends is the harness a role runs on for a claim of issue, for the readers that hold no
// resolution of the claim's role: the rule of a claim no install can move (noPluginInstall) and
// the compaction route (api.Options.Backend). A role the plan in force of issue defines runs on Oh
// My Pi, as every role of a plan does (stepplan.RoleOf), even where the deployment took its name
// and worker_backends puts the deployment's role on another harness. Every other role runs on the
// harness worker_backends names (backendFor, config.Config.BackendFor). So the plan is read only
// where it can change the answer: for a role backendFor puts on another harness than Oh My Pi,
// that is none of Legion's, on a claim of an issue; a nil read reads none. A read of the plan that
// fails is the error.
func roleBackends(backendFor func(claim.Role) runtime.Backend, read planRoleReader) roleBackend {
	return func(ctx context.Context, issue string, role claim.Role) (runtime.Backend, error) {
		backend := backendFor(role)
		if runtime.Of(backend) == runtime.BackendOmp || read == nil || issue == "" || stepplan.IsLegionName(string(role)) {
			return backend, nil
		}
		_, found, err := read(ctx, issue, role)
		if err != nil {
			return "", fmt.Errorf("the harness of %s on %s: %w", role, issue, err)
		}
		if stepplan.Overrides(string(role), found) {
			return runtime.BackendOmp, nil
		}
		return backend, nil
	}
}
