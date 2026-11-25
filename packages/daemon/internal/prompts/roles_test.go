package prompts

//ci:reads packages/pi-legion/agents/*.md TestEveryTaskAgentARolePromptDispatchesIsShipped stats agents/<name>.md for each task agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// The role prompt parts Compose joins. A core and the interactive fragment are also composed for an
// interactive session (modeNeutral); the headless fragment is composed for a phase worker alone.
var (
	phaseRoles    = []string{"planner", "implementer", "tester", "reviewer", "merger"}
	cores         = []string{"planner", "implementer", "tester", "reviewer", "oracle"}
	modeNeutral   = append(append([]string{"core/common.md"}, coreFiles()...), "mechanics/interactive.md")
	reusableParts = append(slices.Clone(modeNeutral), "mechanics/headless.md")
)

func coreFiles() []string {
	files := make([]string, 0, len(cores))
	for _, role := range cores {
		files = append(files, "core/"+role+".md")
	}
	return files
}

// rolePart is one embedded role prompt part, by its path under roles/.
func rolePart(t *testing.T, name string) string {
	t.Helper()
	body, err := roleParts.ReadFile("roles/" + name)
	if err != nil {
		t.Fatalf("read role prompt part %s: %v", name, err)
	}
	return string(body)
}

// Every role a claim can be on has its own part, and none is blank: a blank part boots a worker with
// no instructions at all. The merger composes no core.
func TestEveryRoleHasItsPartAndTheMergerHasNoCore(t *testing.T) {
	for _, role := range claim.Roles {
		if strings.TrimSpace(rolePart(t, string(role)+".md")) == "" {
			t.Errorf("roles/%s.md is blank", role)
		}
	}
	if _, err := fs.Stat(roleParts, "roles/core/merger.md"); err == nil {
		t.Error("roles/core/merger.md exists; the merger composes no core")
	}
}

// The runtimes join the parts byte for byte, so each starts with a heading and ends with one
// newline.
func TestEveryPartStartsWithAHeadingAndEndsWithOneNewline(t *testing.T) {
	parts := slices.Clone(reusableParts)
	for _, role := range phaseRoles {
		parts = append(parts, role+".md")
	}
	for _, file := range parts {
		text := rolePart(t, file)
		if !strings.HasPrefix(text, "#") {
			t.Errorf("%s does not start with a heading", file)
		}
		if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
			t.Errorf("%s does not end with exactly one newline", file)
		}
	}
}

// Every task agent a role prompt dispatches, in the form the boot gate resolves, is shipped in the
// plugin's agents/, where Oh My Pi finds it in a pane and in a pod. The test walks the role parts
// as RoleReferences does, gives each to the boot gate's parser (promptrefs), and holds what it found
// to what RoleReferences answers. It counts the parts it walked, so a walk that read nothing fails
// whatever the parts say.
func TestEveryTaskAgentARolePromptDispatchesIsShipped(t *testing.T) {
	found := promptrefs.New()
	walked := 0
	if err := eachPart(roleParts, "roles", func(name string, body []byte) error {
		walked++
		found.Text("roles/"+name, body)
		return nil
	}); err != nil {
		t.Fatalf("read the roles parts: %v", err)
	}
	// A walk that read no part would pass below while checking nothing.
	if walked == 0 {
		t.Fatal("the walk of the embedded roles/ tree read no part")
	}
	agents := found[promptrefs.TaskAgents]
	if want := RoleReferences()[promptrefs.TaskAgents]; !reflect.DeepEqual(agents, want) {
		t.Errorf("the walk of roles/ finds the task agents %q, and RoleReferences answers %q", agents, want)
	}
	for agent, files := range agents {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "pi-legion", "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
}
