package workflow

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main's hook prompt (record/outbox.go:HookPrompt.Message at d46ec4e0) said what its count is:
// "Moves back this hook counts: %d. Counts by move: %s.", and the firing hands it the hook's own
// number of moves back, the counts of every move the hook selects added up (workflow/hooks.go:
// fireWorkBackHooks, `count := hookCount(hook, counts)`; hookCount: "A hook over every move adds
// up the whole map"). dispatch://INFRA-2551's R6: "A hook over `any` adds up the counts of R1",
// and its R5's default words say the work "has gone back the number of times recorded below".
// INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the prompt
// does not give it"; the change's own wrong fact is its defect. The branch's message gives the
// number as `count: N`, and roles/mechanics/headless.md, which every worker a hook can name loads,
// says "`count` is the count of the move that fired the hook". The default hook, over every move,
// fires at the fifth move back of an issue whose work went back twice from testing and three times
// from reviewing: its count is 5, and the move that fired it, reviewing-implementing, counts 3.
func TestTheWorkersPromptSaysWhatAHookPromptsCountCounts(t *testing.T) {
	shipped := config.DefaultHooks()[0]
	hook := Hook{Name: shipped.Name, On: HookEventWorkBack, Every: shipped.Every, Roles: shipped.Roles, Prompt: shipped.Prompt}
	from, to := phase.Reviewing, phase.Implementing
	fired := phase.Pair(from, to)
	counts := map[string]int{phase.Pair(phase.Testing, phase.Implementing): 2, fired: 3}
	count := hookCount(hook, counts)
	if !hookSelects(hook, fired) || count%hook.Every != 0 {
		t.Fatalf("the default hook does not fire on %s at counts %v (count %d, every %d)", fired, counts, count, hook.Every)
	}
	message, err := record.HookPrompt{Hook: hook.Name, Role: claim.RoleImplementer, From: from, To: to, Count: count,
		Counts: counts, PullRequest: "acme/widgets#42", Prompt: hook.Prompt}.Message("LEGION-7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "\ncount: "+strconv.Itoa(count)+"\n") {
		t.Fatalf("the hook prompt does not give count: %d:\n%s", count, message)
	}
	body, err := os.ReadFile(filepath.Join("..", "prompts", "roles", "mechanics", "headless.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		_, gloss, found := strings.Cut(line, "`count` is ")
		if !found {
			continue
		}
		gloss, _, _ = strings.Cut(gloss, ",")
		if strings.Contains(gloss, "the count of the move that fired the hook") && count != counts[fired] {
			t.Errorf("roles/mechanics/headless.md says \"`count` is %s\", but the default hook fired on %s, whose count is %d, "+
				"with count: %d, the moves back it counts; main said \"Moves back this hook counts: %d.\":\n%s",
				gloss, fired, counts[fired], count, count, message)
		}
	}
}
