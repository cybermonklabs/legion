package config

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// readModelContextBytes is `model_context_bytes`: a mapping of a Legion role (RoleConfigRoles, the
// controller among them) to the context window of the model that role runs, in bytes of system
// prompt, the shape `worker_backends` and `role_config_files` give a per-role key. The daemon
// cannot read a model's context for itself — the model is the operator's to name, in an Oh My Pi
// settings overlay or in Claude Code's own environment, and neither tells the daemon how much the
// model holds — so the operator states it here, and a launch of that role whose composed system
// prompt is larger is refused before anything starts and before its claim is charged a launch
// failure (runtime.SystemPrompt, runtime.PromptTooLarge). A role the key leaves out has no bound.
// A role named twice, a name that is not a Legion role, and a value that is not a whole number
// above zero are each refused naming the entry: a bound of zero would refuse every launch of the
// role, which is a role taken out of the deployment rather than a model's context.
func readModelContextBytes(value *yaml.Node, key string) (map[claim.Role]int, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a role to the bytes of system prompt its model's context holds, e.g. {implementer: 800000}", key)
	}
	bounds := map[claim.Role]int{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, bytesNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(RoleConfigRoles, role) {
			return nil, fmt.Errorf("%s names %q, which is not a Legion role (%s)", key, roleNode.Value, runtime.JoinRoles(RoleConfigRoles))
		}
		if _, seen := bounds[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		field := key + "." + string(role)
		var bound int
		if bytesNode.Kind != yaml.ScalarNode || bytesNode.Tag != "!!int" || bytesNode.Decode(&bound) != nil {
			return nil, fmt.Errorf("%s must be a whole number of bytes", field)
		}
		if bound < 1 {
			return nil, fmt.Errorf("%s must be a whole number of bytes above zero (got %d): leave the role out to give it no bound", field, bound)
		}
		bounds[role] = bound
	}
	return bounds, nil
}
