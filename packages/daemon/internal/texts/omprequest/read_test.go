package omprequest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The request fixtures under testdata/requests are the first request a real Oh My Pi sent a model
// stand-in in each of three cases, captured by testdata/capture.sh (testdata/README.md says
// how, with which build, and what the measurement showed). Read is how every test that holds the
// real Oh My Pi to what a launch of a folder of texts offers its model reads such a request, so it
// is held to the real bytes here first: each fixture gives the system prompt, the skill list, the
// `task` tool's agent list and the `<dispatch-first-skill>` message, and each case's own facts
// hold of what Read gives.
//
//  1. A root with a changed legion-worker/SKILL.md and a changed goal-red-team agent, and no
//     hiding layer: the root's copy keeps the plain name legion-worker, and the plugin's differing
//     copy is listed as an alias, `pi-legion/legion-worker` (the namespace Oh My Pi gives is the
//     directory the plugin's skills came from, one `/` and no more).
//  2. The same root with the hiding layer (`*/legion-worker`, `*/legion-worker~*`): the root's
//     copy alone, no alias, and every other skill as in case 1.
//  3. A root whose only change is legion-worker/references/plan-tests.md: its SKILL.md is the
//     plugin's own bytes, so the two collapse into one skill, with no alias.
//
// A root of only agents/ and skills/ loaded as an --extension root in cases 1 and 2: the root's
// agent heads the task list and its skill is listed, with no package.json beside them.
func TestTheReaderReadsEachCapturedRequest(t *testing.T) {
	const marker = "Changed by the arm."
	plain := []string{
		"agent-secrets", "approval-review", "asd-ste100", "dispatch", "dispatch-first", "dispatch-writing-specs", "envoy",
		"legion-architect", "legion-controller", "legion-oracle", "legion-retro", "legion-worker", "outside-message",
		"plain-words", "simplify-settled-diff",
	}
	// What each plugin ships and Oh My Pi bundles, as the task tool lists it.
	agents := []string{
		"deep-worker", "design-outsider", "goal-red-team", "oracle", "plan-gap-analyst", "plan-reviewer", "reviewer", "scout",
		"seance", "security-reviewer", "sonic", "task", "thermonuclear-code-quality", "thermonuclear-deep-review",
	}

	type facts struct {
		// alias is the name the plugin's copy of legion-worker is listed under, "" when it is listed
		// under none.
		alias string
		// rootSkill is whether the plain legion-worker is the root's changed copy, rootAgent whether
		// goal-red-team is the root's.
		rootSkill, rootAgent bool
	}
	cases := map[string]facts{
		"1-changed-skill.json":        {alias: "pi-legion/legion-worker", rootSkill: true, rootAgent: true},
		"2-changed-skill-hidden.json": {rootSkill: true, rootAgent: true},
		"3-changed-reference.json":    {},
	}
	files, err := filepath.Glob(filepath.Join("testdata", "requests", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured []string
	for _, file := range files {
		captured = append(captured, filepath.Base(file))
	}
	slices.Sort(captured)
	if want := []string{"1-changed-skill.json", "2-changed-skill-hidden.json", "3-changed-reference.json"}; !slices.Equal(captured, want) {
		t.Fatalf("testdata/requests holds %q, want the three cases %q (capture.sh writes them)", captured, want)
	}

	read := map[string]Request{}
	for _, file := range captured {
		want := cases[file]
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "requests", file))
			if err != nil {
				t.Fatal(err)
			}
			request, err := Read(body)
			if err != nil {
				t.Fatalf("Read(%s) = %v, want the request read", file, err)
			}
			read[file] = request

			if !strings.Contains(request.System, "<skills>") || !strings.Contains(request.System, "The case's appended system prompt.") {
				t.Errorf("the system prompt of %s lacks the skill list or the appended prompt: %.300q", file, request.System)
			}
			var listed []string
			for _, skill := range request.Skills {
				listed = append(listed, skill.Name)
			}
			wantListed := slices.Clone(plain)
			if want.alias != "" {
				wantListed = append(wantListed, want.alias)
			}
			slices.Sort(listed)
			slices.Sort(wantListed)
			if !slices.Equal(listed, wantListed) {
				t.Errorf("%s lists the skills %q, want %q", file, listed, wantListed)
			}
			for _, skill := range request.Skills {
				if skill.Description == "" {
					t.Errorf("%s lists the skill %q with no description", file, skill.Name)
				}
				if strings.Count(skill.Name, "/") > 1 {
					t.Errorf("%s lists the skill %q, a name with more than one `/`, which a hiding pattern `*/<skill>` would not match", file, skill.Name)
				}
			}
			byName := func(name string) (Skill, bool) {
				for _, skill := range request.Skills {
					if skill.Name == name {
						return skill, true
					}
				}
				return Skill{}, false
			}
			worker, ok := byName("legion-worker")
			if !ok {
				t.Fatalf("%s lists no plain legion-worker", file)
			}
			if got := strings.HasPrefix(worker.Description, marker); got != want.rootSkill {
				t.Errorf("%s lists legion-worker as %q: the root's changed copy = %t, want %t", file, worker.Description, got, want.rootSkill)
			}
			if want.alias != "" {
				alias, ok := byName(want.alias)
				if !ok || strings.HasPrefix(alias.Description, marker) || !strings.HasPrefix(alias.Description, "Use when dispatched as a per-process Legion phase worker") {
					t.Errorf("%s lists the alias %q as %+v, want the plugin's own copy, which the root's replaced", file, want.alias, alias)
				}
			}

			var offered []string
			for _, agent := range request.Agents {
				offered = append(offered, agent.Name)
			}
			sorted := slices.Clone(offered)
			slices.Sort(sorted)
			if !slices.Equal(sorted, agents) {
				t.Errorf("%s offers the task agents %q, want %q (each once, Oh My Pi's bundled scout and reviewer among them)", file, sorted, agents)
			}
			for i, agent := range request.Agents {
				if agent.Name == "goal-red-team" {
					if got := strings.HasPrefix(agent.Description, marker); got != want.rootAgent {
						t.Errorf("%s offers goal-red-team as %q: the root's changed copy = %t, want %t", file, agent.Description, got, want.rootAgent)
					}
					if (i == 0) != want.rootAgent {
						t.Errorf("%s offers goal-red-team at place %d of %q: the root's agents come before the plugins', so it heads the list exactly when it is the root's", file, i, offered)
					}
				}
			}

			if len(request.DispatchFirst) != 1 || !strings.HasPrefix(request.DispatchFirst[0], "<dispatch-first-skill>") ||
				!strings.HasSuffix(request.DispatchFirst[0], "</dispatch-first-skill>") || !strings.Contains(request.DispatchFirst[0], "# Dispatch first") {
				t.Errorf("%s carries the dispatch-first messages %.200q, want the one skill block", file, request.DispatchFirst)
			}
		})
	}

	// The hiding layer removes the alias and nothing else, and a changed reference alone changes
	// no listing: the facts the three cases were captured to show.
	one, two, three := read["1-changed-skill.json"], read["2-changed-skill-hidden.json"], read["3-changed-reference.json"]
	skillNames := func(r Request) []string {
		var names []string
		for _, skill := range r.Skills {
			if skill.Name != "pi-legion/legion-worker" {
				names = append(names, skill.Name)
			}
		}
		return names
	}
	if !slices.Equal(skillNames(one), skillNames(two)) || !slices.Equal(skillNames(two), skillNames(three)) {
		t.Errorf("the three cases list other skills than each other: %q, %q, %q", skillNames(one), skillNames(two), skillNames(three))
	}

	// Read refuses what is no conversation request, so a test never reads a request that holds
	// nothing of what it asks as one that holds none of it.
	for name, body := range map[string]string{
		"not JSON":         `{"system":`,
		"an empty object":  `{}`,
		"no system prompt": `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"Compress into one routing hint of at most 12 words"}]}]}`,
		"no messages":      `{"model":"m","system":[{"type":"text","text":"A system prompt."}]}`,
	} {
		if request, err := Read([]byte(body)); err == nil {
			t.Errorf("Read(%s) = %+v, want an error: it is no conversation request", name, request)
		}
	}
}

// A pane's Oh My Pi offers its built-in tools under a name with a leading `_` (`_task`, `_read`),
// which the texts-folder live proof's panes measured, where the captured fixtures' Oh My Pi offered
// them bare. Read takes the task tool's agent list under either spelling: fixture 3 with every tool
// renamed so gives the same agents as fixture 3 itself.
func TestTheReaderTakesTheTaskToolUnderAPanesSpelling(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "requests", "3-changed-reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	tools, _ := raw["tools"].([]any)
	for _, tool := range tools {
		if named, ok := tool.(map[string]any); ok {
			named["name"] = "_" + named["name"].(string)
		}
	}
	spelled, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := Read(body)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := Read(spelled)
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Agents) == 0 || !slices.Equal(pane.Agents, bare.Agents) {
		t.Errorf("Read of the request whose tools are spelled `_task` gives the agents %+v, want fixture 3's own %+v", pane.Agents, bare.Agents)
	}
}
