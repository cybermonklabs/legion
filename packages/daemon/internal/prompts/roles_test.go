package prompts

//ci:reads packages/pi-legion/agents/*.md TestEveryTaskAgentARolePromptDispatchesIsShipped stats agents/<name>.md for each task agent
//ci:reads .github/private-names.toml TestNoReusablePartNamesARepositoryOrLibrary reads it under ../../../.. and runs its rules over every reusable role part

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// The reusable role prompt parts Compose joins for every deployment: the core every phase worker
// but the merger shares, each core, and the two mechanics fragments.
var (
	cores         = []string{"planner", "implementer", "tester", "reviewer", "oracle"}
	reusableParts = append(append([]string{"core/common.md"}, coreFiles()...), "mechanics/interactive.md", "mechanics/headless.md")
	repoSpecific  = []string{
		"Inspect", "inspect_ai", "inspect_", "Hawk", "middleman", "Taiga",
		// The runbook a deployment sends a heavy run to, and the bucket it keeps one's output in.
		// A deployment's own names are not listed here: this repository is public, so a test of it
		// cannot hold them. check-private-names refuses those the deployment's own term list holds,
		// in every file it reads, role parts included.
		"running-heavy-jobs", "gs://",
	}
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

// A reusable part is composed for every deployment, so it names no repository, library, service or
// bucket of one deployment's own. Both trees are read: the roles/ parts Compose joins for any role
// and every go/ part, because the pod memory rule tells a role where its deployment's instructions
// send a heavy run, and the next hand to write that rule down can spell a deployment's own route
// out in the architect parts, which this test read nothing of before.
//
// Each part is held to two checks: the generic names of repoSpecific, and the rules of
// .github/private-names.toml. That file is this repository's own port of
// .github/scripts/check-private-names.sh's built-in rules, and the port harness,
// .github/scripts/check-private-names-port.test.sh, holds the two to one verdict. The test reads
// the file from the repository, since the legion binary embeds no rule. A finding is reported by
// its class alone (refusedClasses): a report line holds the matched text, which is a private name,
// and this repository's CI logs are public. Of a deployment's own names, check-private-names
// refuses those the deployment's own term list holds, in every file it reads.
func TestNoReusablePartNamesARepositoryOrLibrary(t *testing.T) {
	// The rule half reads .github/private-names.toml at the repository root, four levels above
	// this package, because the legion binary embeds no rule.
	rulesPath := filepath.Join("..", "..", "..", "..", ".github", "private-names.toml")
	content, err := leakguard.ReadRulesContent(rulesPath)
	if err != nil {
		t.Fatalf("read %s: %v", rulesPath, err)
	}
	rules, err := leakguard.LoadRules(content)
	if err != nil {
		t.Fatalf("compile %s: %v", rulesPath, err)
	}
	check := func(name, text string) {
		for _, needle := range repoSpecific {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q", name, needle)
			}
		}
		// The record's Path is the part's name, because the rule file's path rules read
		// Record.Path alone. A refusal is reported by class alone, because its report lines hold
		// the matched text and CI logs are public. A SetupFailure holds no matched text.
		err := rules.Check(t.Context(), "the reusable part "+name, leakguard.Record{Label: name, Text: text, Path: name})
		var refusal *leakguard.Refusal
		switch {
		case errors.As(err, &refusal):
			for _, class := range refusedClasses(name, refusal.Lines) {
				t.Errorf("%s: %s refuses it: %s", name, rulesPath, class)
			}
		case err != nil:
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, file := range reusableParts {
		check(file, rolePart(t, file))
	}
	err = eachPart(goParts, "go", func(name string, body []byte) error {
		check("go/"+name, string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("read the go parts: %v", err)
	}
}

// refusedClasses is the class of each line of a rule pass's report on the record labelled label.
// A report line is `<label>: <class>: matched …` (leakguard's reportLine). What follows the class
// holds the matched text, so it is never answered. A line of any other shape is answered as such,
// and none of it is printed.
func refusedClasses(label string, lines []string) []string {
	classes := make([]string, 0, len(lines))
	for _, line := range lines {
		rest, labelled := strings.CutPrefix(line, label+": ")
		class, _, matched := strings.Cut(rest, ": matched ")
		if !labelled || !matched || class == "" {
			class = "a line of an unexpected shape"
		}
		classes = append(classes, class)
	}
	return classes
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

// Every task agent a role prompt dispatches, in the form the boot gate resolves, is shipped in the
// plugin's agents/, where Oh My Pi finds it in a pane and in a pod.
func TestEveryTaskAgentARolePromptDispatchesIsShipped(t *testing.T) {
	agents := RoleReferences()[promptrefs.TaskAgents]
	// A reader that finds nothing would pass below while checking nothing.
	if !slices.Equal(agents["deep-worker"], []string{"roles/implementer.md"}) {
		t.Errorf("task agent deep-worker is dispatched by %q, want roles/implementer.md alone", agents["deep-worker"])
	}
	for agent, files := range agents {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "pi-legion", "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
}
