package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The one word a hook's `on` names is spelled twice on purpose: internal/config spells it for the
// loader, which refuses any other event, and internal/workflow spells it again for the engine,
// which fires the hooks that name it, because internal/workflow imports no configuration. Nothing
// else holds the two equal, and this package is where they meet: it reads the loader's hooks and
// hands them to the engine. A drift between them costs a hook the loader accepts and the engine
// never fires — an operator's prompt that is live in `legion.yaml` and silent on every move back.
func TestHookEventWorkBackIsOneWordInConfigAndWorkflow(t *testing.T) {
	if workflow.HookEventWorkBack != config.HookEventWorkBack {
		t.Fatalf("workflow.HookEventWorkBack = %q, config.HookEventWorkBack = %q: a hook the loader accepts the engine would never fire",
			workflow.HookEventWorkBack, config.HookEventWorkBack)
	}
}
