// The tests of daemon-pane.go hold its restatements of the daemon to the daemon's own functions:
// deploymentRolesLine to daemon.DeploymentRolesFragment, productionCheckLine to
// daemon.ProductionCheckFragment and stepPromptPaths to prompts.Composer.ComposeStep. The file
// belongs to no module either: daemon-pane.ts (goInDaemonModule) compiles it with the program as
// cmd/daemon-pane/main_test.go of a checkout's daemon module, and daemon-pane.test.ts runs it in
// this checkout's module alone, whose daemon has each function a restatement copies. No row runs
// it against another checkout: an older one lacks those functions, so the file does not compile
// there, while the program itself does.
package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/prompts"
)

// TestTheRigsRestatementsAreTheDaemons holds the sentence that tells an architect the roles of
// its deployment, for a role that names a model and one that names none, the sentence of its
// production check, for the rig's check and for none, and the parts of a step of a role of the
// deployment, to the daemon's own. The rig's check is the key table's default, and this checkout's
// daemon has the key, so the rig tells every architect the sentence.
func TestTheRigsRestatementsAreTheDaemons(t *testing.T) {
	roles := []rigRole{{name: "checker", typ: "testing", model: "checker-model"}, {name: "critic", typ: "review"}}
	defined := []config.Role{
		{Name: "checker", Type: plan.TypeTesting, Model: "checker-model"},
		{Name: "critic", Type: plan.TypeReview},
	}
	if got, want := deploymentRolesLine(roles), daemon.DeploymentRolesFragment(defined); got != want {
		t.Errorf("deploymentRolesLine = %q, want the daemon's %q", got, want)
	}

	for _, check := range []claim.Role{rigProductionCheck, ""} {
		if got, want := productionCheckLine(check), daemon.ProductionCheckFragment(check); got != want {
			t.Errorf("productionCheckLine(%q) = %q, want the daemon's %q", check, got, want)
		}
	}
	if !checksProduction() {
		t.Error("checksProduction() = false, want true: this checkout's daemon has the project key production_check")
	}
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
