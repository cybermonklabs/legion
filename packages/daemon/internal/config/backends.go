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
// role on it in issueRoles (claim.Roles, the order of the default steps, then each role of
// `roles`, in the file's order): every issue role's (BackendFor), then the controller's when the
// daemon launches it. Boot resolves each one's launch (readTmux).
func (c Config) Backends() []runtime.Backend {
	roles := c.issueRoles()
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

// readWorkerBackends is `worker_backends`: a mapping of an issue role to the backend it runs on
// (runtime.Backends), and the line of each role it names (roleNameLines). A role named twice, a
// name that is no role a claim can be on (claim.IsRole), a backend there is no entry for, and a
// backend that does not run the role are each refused naming the entry, a name of no role name's
// shape (roleNameShape) by its line alone; a name that is neither one of Legion's roles nor a role
// of `roles` is checkDeploymentRoleKeys', once both keys are read.
func readWorkerBackends(value *yaml.Node, key string) (map[claim.Role]runtime.Backend, roleNameLines, error) {
	if value.Tag == "!!null" {
		return nil, nil, nil
	}
	names := strings.Join(runtime.BackendNames(), ", ")
	if value.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%s must be a mapping of an issue role to its backend (%s), e.g. {implementer: claude}", key, names)
	}
	backends := map[claim.Role]runtime.Backend{}
	lines := map[claim.Role]int{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, backendNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !claim.IsRole(role) {
			if !roleNameShape(roleNode.Value) {
				return nil, nil, fmt.Errorf("%s: it must be an issue role (%s)", unknownKeyAt(roleNode, key), runtime.JoinRoles(claim.Roles))
			}
			return nil, nil, fmt.Errorf("%s names %q, which is not an issue role (%s)", key, roleNode.Value, runtime.JoinRoles(claim.Roles))
		}
		if _, seen := backends[role]; seen {
			return nil, nil, fmt.Errorf("%s names %s twice", key, shownRole(roleNode.Value, roleNode.Line))
		}
		field := roleField(key, roleNode.Value, roleNode.Line)
		var name string
		if backendNode.Kind != yaml.ScalarNode || backendNode.Decode(&name) != nil {
			return nil, nil, fmt.Errorf("%s must be one of %s", field, names)
		}
		backend := runtime.Backend(name)
		info, ok := runtime.Backends[backend]
		switch {
		case !ok:
			return nil, nil, fmt.Errorf("%s must be one of %s (got %q)", field, names, name)
		case !info.Runs(role):
			return nil, nil, fmt.Errorf("%s is %s (%s), which runs only %s: %s", field, backend, info.Name, info.RunsWhat(), info.Why)
		}
		backends[role] = backend
		lines[role] = roleNode.Line
	}
	return backends, roleNameLines{key: lines}, nil
}
