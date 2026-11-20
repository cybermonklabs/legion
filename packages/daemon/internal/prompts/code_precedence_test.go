package prompts

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Each kind of Legion session's composed prompt gives the code, a command's output and its
// `--help` precedence over a skill, and gives it once: INFRA-4673 moved the sentence out of the
// skills and into the prompt every session of a kind is given. The rows are built from
// claim.Roles, so a role added later is read too, and a role Compose refuses fails here rather
// than going unread: the architect in both its shapes, each phase worker, and the controller's
// headless and interactive launches (ControllerPromptPaths), each alone and again with each part a
// backend's launch is told after its role's parts. The backend rows come from runtime.Backends'
// PromptPart (today Claude Code's claude-mechanics.md), so a backend added later is read too. Both
// counts read the folded text, so a rewrap leaves the sentence one sentence. The second count, of
// "disagree(s) with the code" in any case, catches a second sentence that says the same thing in
// other words. The deployment's instructions, the addressing and a per-token override file are
// appended by the runtime after Compose, so they are outside this test. This shows the text is
// loaded, never that a role follows it.
func TestEveryComposedPromptGivesTheCodePrecedenceOnce(t *testing.T) {
	const precedence = "If a skill disagrees with the code, a command's output or its `--help`, follow those and say which line of the skill is wrong."
	restated := regexp.MustCompile(`(?i)disagrees? with the code`)
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type prompt struct{ name, text string }
	var prompts []prompt
	for _, role := range claim.Roles {
		if role == claim.RoleArchitect {
			prompts = append(prompts,
				prompt{"architect root", composedText(t, composer, role, true)},
				prompt{"architect sub-architect", composedText(t, composer, role, false)})
			continue
		}
		prompts = append(prompts, prompt{string(role), composedText(t, composer, role, false)})
	}
	for _, launch := range []struct {
		name     string
		headless bool
	}{{"controller headless", true}, {"controller interactive", false}} {
		paths, err := composer.ControllerPromptPaths(launch.headless)
		if err != nil {
			t.Fatalf("ControllerPromptPaths(headless %t): %v", launch.headless, err)
		}
		prompts = append(prompts, prompt{launch.name, readJoined(t, paths...)})
	}
	var withBackend []prompt
	for _, backend := range slices.Sorted(maps.Keys(runtime.Backends)) {
		part := runtime.Backends[backend].PromptPart
		if part == "" {
			continue
		}
		partText := readJoined(t, composer.Part(part))
		for _, p := range prompts {
			withBackend = append(withBackend, prompt{p.name + " + " + part, p.text + partText})
		}
	}

	for _, p := range append(prompts, withBackend...) {
		t.Run(p.name, func(t *testing.T) {
			folded := foldWhitespace(p.text)
			if found := strings.Count(folded, precedence); found != 1 {
				t.Errorf("the composed %s prompt gives the code precedence %d times, want once: %q", p.name, found, precedence)
			}
			if found := len(restated.FindAllStringIndex(folded, -1)); found != 1 {
				t.Errorf("the composed %s prompt matches %s %d times, want once", p.name, restated, found)
			}
		})
	}
}
