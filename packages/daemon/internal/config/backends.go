package config

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// BackendFor is the backend role runs on: the one `worker_backends` names, else Oh My Pi.
func (c Config) BackendFor(role claim.Role) runtime.Backend {
	if backend, ok := c.WorkerBackends[role]; ok {
		return backend
	}
	return runtime.BackendOmp
}

// Backends are the backends the daemon launches agents on, each once, in the order of the first
// role on it in claim.Roles (the order of the default steps): every issue role's (BackendFor),
// then the controller's when the daemon launches it. Boot resolves each one's launch (readTmux).
func (c Config) Backends() []runtime.Backend {
	roles := slices.Clone(claim.Roles)
	if c.ControllerLaunch == ControllerLaunchDaemon {
		roles = append(roles, claim.RoleController)
	}
	var backends []runtime.Backend
	for _, role := range roles {
		if backend := c.BackendFor(role); !slices.Contains(backends, backend) {
			backends = append(backends, backend)
		}
	}
	return backends
}

// NoPluginInstall is the launch of a claim of role installing no plugin at all, whatever contract
// the claim records: the runtime is Kubernetes and role runs on Claude Code, whose plugin there is
// the worker image's own claude-envoy. No install reaches it (internal/daemon's
// specs.stalePluginInstall installs none for that backend, and no workflow releases claude-envoy)
// and no relaunch moves it, because the launch keeps the pod the claim already runs in: only a new
// image carries another claude-envoy, which is what the pod round replaces the pod for.
//
// Under tmux a Claude Code role is exempt from nothing. Its pane loads the claude-envoy held at
// LEGION_CLAUDE_PLUGIN_DIR (internal/daemon's resolveClaude; tmux's ClaudeLaunch, whose MCP
// configuration and plugin argument every launch writes afresh), so a relaunch once that pane's
// turn ends brings the worker up on whatever that directory holds now, exactly as an Oh My Pi pane
// comes back on the plugin installed on the daemon's host. No tmux launch installs a plugin for
// any backend (specs.stalePluginInstall), so a relaunch is the whole of the lane there — and for
// this role it is enough.
//
// It is one rule read in three places, each of which would otherwise act on a claim no install
// of this deployment can move: internal/daemon's stale-plugin pass (refreshStalePlugins, through
// supervise.StaleForTheLane); the machine's stale registration, which supervise cannot decide for
// itself because a role's backend is the daemon's configuration (supervise.Deps.NoPluginInstall,
// handed this same rule); and the API's stale-caller refusal, which would otherwise hold such a
// worker's grants, its handoff_complete and its ready on a relaunch that never comes
// (api.Options.NoPluginInstall, api's staleClaim).
func (c Config) NoPluginInstall(role claim.Role) bool {
	return c.Runtime.Name == "kubernetes" && runtime.Of(c.BackendFor(role)) == runtime.BackendClaude
}

// readWorkerBackends is `worker_backends`: a mapping of an issue role to the backend it runs on
// (runtime.Backends). A role named twice, a name that is not an issue role, a backend there is no
// entry for, and a backend that does not run the role are each refused naming the entry.
func readWorkerBackends(value *yaml.Node, key string) (map[claim.Role]runtime.Backend, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	names := strings.Join(runtime.BackendNames(), ", ")
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of an issue role to its backend (%s), e.g. {implementer: claude}", key, names)
	}
	backends := map[claim.Role]runtime.Backend{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, backendNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(claim.Roles, role) {
			return nil, fmt.Errorf("%s names %q, which is not an issue role (%s)", key, roleNode.Value, runtime.JoinRoles(claim.Roles))
		}
		if _, seen := backends[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		var name string
		if backendNode.Kind != yaml.ScalarNode || backendNode.Decode(&name) != nil {
			return nil, fmt.Errorf("%s.%s must be one of %s", key, role, names)
		}
		backend := runtime.Backend(name)
		info, ok := runtime.Backends[backend]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s.%s must be one of %s (got %q)", key, role, names, name)
		case !info.Runs(role):
			return nil, fmt.Errorf("%s.%s is %s (%s), which runs only %s: %s", key, role, backend, info.Name, runtime.JoinRoles(info.Roles), info.Why)
		}
		backends[role] = backend
	}
	return backends, nil
}
