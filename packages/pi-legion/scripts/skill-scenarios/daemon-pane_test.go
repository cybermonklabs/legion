// The tests of daemon-pane.go hold its restatements of the daemon to the daemon's own functions:
// stepPromptPaths to prompts.Composer.ComposeStep, and rigProductionCheck to the key table's
// default. What an architect is told after its addressing is no restatement: the program composes
// it with daemon.ArchitectAddressing itself (daemon-pane-architect.go), which
// internal/daemon/rig_prompt_test.go holds byte for byte to the daemon's prompt. The file belongs to
// no module either: daemon-pane.ts (goInDaemonModule) compiles it with the program as
// cmd/daemon-pane/main_test.go of a checkout's daemon module, and daemon-pane.test.ts runs it in
// this checkout's module alone, whose daemon has each function a restatement copies. No row runs
// it against another checkout: an older one lacks those functions, so the file does not compile
// there, while the program itself does.
package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
)

// TestTheRigsRestatementsAreTheDaemons holds the production check of the rig's deployment to the
// key table's default, and the parts of a step of a role of the deployment to the daemon's own.
func TestTheRigsRestatementsAreTheDaemons(t *testing.T) {
	defaults := []string{}
	for _, key := range config.ConfigReference() {
		for _, nested := range key.Nested {
			if nested.Path == "projects.<KEY>" && nested.Name == "production_check" {
				defaults = append(defaults, nested.Default)
			}
		}
	}
	if want := "`" + string(rigProductionCheck) + "`"; !slices.Equal(defaults, []string{want}) {
		t.Errorf("the key table's defaults of production_check = %q, want the rig's %q alone", defaults, want)
	}

	stateDir := t.TempDir()
	composer, err := prompts.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(t.TempDir(), "checker.md")
	if got, want := stepPromptPaths(stateDir, composer, promptFile), composer.ComposeStep(promptFile).RolePromptPaths; !slices.Equal(got, want) {
		t.Errorf("stepPromptPaths = %q, want the daemon's %q", got, want)
	}
}
