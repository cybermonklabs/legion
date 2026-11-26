package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/texts/omprequest"
)

// The folder of texts an experiment arm names is checked against the agents and
// skills the worker image ships, and the check reads the names off the folder's texts with
// promptrefs. A form of naming an agent or a skill that the reader misses is a name that check
// never sees, so this is the list the reader answers to: every form in which a text Legion ships
// names one. The texts are the two plugins' shipped skills (`dist/skills`) and agents, as packed
// by TestTheRealPluginsOnTheRealOhMyPi, and both embedded role-prompt trees (prompts.Texts). The
// scan is wider than the reader on purpose: an `agent` token followed by `=` or `:` and a quoted
// name (double, single, backtick or escaped double quotes), `subagent_type`, `skill://` and
// `/skill:`, so a form someone writes later fails here instead of passing the reader unseen. It
// runs as a subtest of that test, which packs the plugins once, and in CI's `plugin-pack` job,
// which already reads those texts.
func everyReferenceFormInLegionsTextsIsCollected(t *testing.T, envoy, legion string) {
	t.Helper()

	// The scan finds each form it is written to find, before it is trusted to find none the
	// reader misses.
	t.Run("the scan finds each form", func(t *testing.T) {
		for _, tc := range []struct {
			text string
			want namedReference
		}{
			{`task(agent="x")`, namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{`task(agent=\"x\")`, namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{`task(agent='x')`, namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{"task(agent=`x`)", namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{`{ agent: "x" }`, namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{`{"agent": "x"}`, namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{`subagent_type: "legion:x"`, namedReference{kind: promptrefs.TaskAgents, name: "x"}},
			{`skill://x/references/y.md`, namedReference{kind: promptrefs.Skills, name: "x"}},
			{`Load skill://x.`, namedReference{kind: promptrefs.Skills, name: "x"}},
			{`run /skill:x now`, namedReference{kind: promptrefs.Skills, name: "x"}},
		} {
			found := referencesIn(tc.text)
			if len(found) != 1 || found[0].kind != tc.want.kind || found[0].name != tc.want.name {
				t.Errorf("referencesIn(%q) = %+v, want the one reference %+v", tc.text, found, tc.want)
			}
		}
		for _, text := range []string{`agent://1-Reviewer`, `write agent://all`, `skill://<name>/<path>`, `agent="<name>"`, `the agent: a reviewer`} {
			if found := referencesIn(text); len(found) != 0 {
				t.Errorf("referencesIn(%q) = %+v, want none (no name is written)", text, found)
			}
		}
	})

	var sources []shippedText
	for _, plugin := range []struct{ name, root string }{{"pi-envoy", envoy}, {"pi-legion", legion}} {
		for _, dir := range []string{"agents", filepath.Join("dist", "skills")} {
			sources = append(sources, shippedPluginTexts(t, plugin.name, plugin.root, dir)...)
		}
	}
	for _, prefix := range []string{"pi-legion/agents/", "pi-envoy/dist/skills/", "pi-legion/dist/skills/"} {
		if !slices.ContainsFunc(sources, func(s shippedText) bool { return strings.HasPrefix(s.label, prefix) }) {
			t.Fatalf("no text under %s was read from the packed plugins %s and %s: the scan covers none of what they ship", prefix, envoy, legion)
		}
	}
	embedded := 0
	if err := fs.WalkDir(prompts.Texts(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := fs.ReadFile(prompts.Texts(), path)
		if err != nil {
			return err
		}
		sources = append(sources, shippedText{label: path, body: string(body)})
		embedded++
		return nil
	}); err != nil {
		t.Fatalf("read prompts.Texts(): %v", err)
	}
	if embedded == 0 {
		t.Fatal("prompts.Texts() holds no file: the scan covers none of the embedded role parts")
	}

	// The reader reads each text under the label the scan names it by, as the folder check will
	// read the folder's, so a name is collected only where its own file writes it.
	collected := promptrefs.New()
	for _, source := range sources {
		collected.Text(source.label, []byte(source.body))
	}
	scanned := 0
	for _, source := range sources {
		for _, ref := range referencesIn(source.body) {
			scanned++
			if !slices.Contains(collected[ref.kind][ref.name], source.label) {
				kind := "an agent"
				if ref.kind == promptrefs.Skills {
					kind = "a skill"
				}
				t.Errorf("%s line %d names %s %q as %s, a form promptrefs does not collect: teach promptrefs.Text that form and add it to TestTheReaderCollectsEachForm",
					source.label, ref.line, kind, ref.name, ref.form)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("the scan found no reference in %d texts, though Legion's skills and role parts name agents and skills", len(sources))
	}
}

// shippedText is one text Legion ships: a Markdown file of a plugin's agents or skills, or a part
// of the embedded role-prompt trees, by the label a reference check names it by.
type shippedText struct{ label, body string }

// shippedPluginTexts reads every Markdown file under dir of the packed plugin at root. A plugin
// that ships no such directory (pi-envoy ships no agents) contributes none; one whose directory
// cannot be read fails the test.
func shippedPluginTexts(t *testing.T, name, root, dir string) []shippedText {
	t.Helper()
	base := filepath.Join(root, dir)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil
	}
	var texts []shippedText
	if err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		texts = append(texts, shippedText{label: name + "/" + filepath.ToSlash(rel), body: string(body)})
		return nil
	}); err != nil {
		t.Fatalf("read the shipped texts of %s under %s: %v", name, base, err)
	}
	return texts
}

// namedReference is one agent or skill a text names, with the form it is written in and its line.
type namedReference struct {
	kind       promptrefs.Kind
	name, form string
	line       int
}

const (
	// quoteAfterToken follows an `agent` or `subagent_type` token: an `=` or `:`, then an opening
	// quote of any of the four kinds, the double quote possibly escaped.
	quoteAfterToken = "\\s*[=:]\\s*\\\\?[\"'\x60]"
	// nameGroup is a name that starts on a letter or digit, so a placeholder (`<name>`) or an
	// address (`agent://`) names nothing and does not match.
	nameGroup = "([A-Za-z0-9][A-Za-z0-9._-]*)"
)

var referenceForms = []struct {
	form string
	kind promptrefs.Kind
	re   *regexp.Regexp
	// trim drops the punctuation a sentence puts after a skill's name, as promptrefs's own skill
	// name ends on a letter or digit.
	trim bool
}{
	{"an `agent` token followed by = or : and a quoted name", promptrefs.TaskAgents,
		regexp.MustCompile("\\bagent[\"']?" + quoteAfterToken + nameGroup), false},
	{"subagent_type", promptrefs.TaskAgents,
		regexp.MustCompile("\\bsubagent_type[\"']?" + quoteAfterToken + "(?:legion:)?" + nameGroup), false},
	{"skill://", promptrefs.Skills, regexp.MustCompile("skill://" + nameGroup), true},
	{"/skill:", promptrefs.Skills, regexp.MustCompile("/skill:" + nameGroup), true},
}

// referencesIn is every reference body writes in any of the forms, in the order of the forms and
// then of the text.
func referencesIn(body string) []namedReference {
	var found []namedReference
	for _, form := range referenceForms {
		for _, at := range form.re.FindAllStringSubmatchIndex(body, -1) {
			name := body[at[2]:at[3]]
			if form.trim {
				name = strings.TrimRight(name, "._-")
			}
			found = append(found, namedReference{kind: form.kind, name: name, form: form.form, line: strings.Count(body[:at[0]], "\n") + 1})
		}
	}
	return found
}

// The load probe's own list is what the image ships. On the real Oh My Pi over both packed plugins,
// loaded as a pod loads them — the two roots explicit, discovery on, LEGION_PROMPT_ROOTS naming
// them — and run as capture-names.sh runs its stand-alone module, probe.mjs under
// LEGION_PROMPT_LIST=1 prints exactly the agents and skills testdata/texts/shipped-names.txt holds,
// Oh My Pi's bundled agents among them, though no name was asked of it. The gate's pod lane, which
// sets the variable itself, answers the same names (pluginGate.verify), as a live proof's gate
// answers its boot. A difference after a change to a skill, an agent or the Oh My Pi pin is the
// fixture's to capture again, with capture-names.sh.
func theProbeListOnTheRealOhMyPi(t *testing.T, omp, envoy, legion string) {
	wantAgents, wantSkills := shippedNamesFixture(t)
	dir := t.TempDir()
	home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
	testbin.OMPHome(t, omp, home)
	// The profile capture-names.sh writes: one operator provider nothing listens on, since `omp
	// models` loads its extensions only for a profile with a model.
	writeTree(t, filepath.Join(home, ".omp", "agent"), map[string]string{
		"models.yml": "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n" +
			"    apiKey: unused\n    models:\n      - id: probe-test\n        name: Probe test\n",
		"config.yml": "modelRoles:\n  default: operator/probe-test\n",
	})
	mkdir(t, work)
	probe := filepath.Join(dir, "probe.mjs")
	if err := os.WriteFile(probe, pluginLoadProbe, 0o600); err != nil {
		t.Fatal(err)
	}
	environ := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
	models := exec.Command(omp, "models", "--extension", envoy, "--extension", legion, "--extension", probe, "--json")
	models.Env = append(slices.Clone(environ), "LEGION_PROMPT_ROOTS="+envoy+":"+legion, "LEGION_PROMPT_LIST=1")
	models.Dir = work
	var stderr bytes.Buffer
	models.Stderr = &stderr
	if err := models.Run(); err != nil {
		t.Fatalf("omp models with the probe: %v\n%s", err, stderr.String())
	}
	var listed []string
	for line := range strings.Lines(stderr.String()) {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, shippedAgentMarker); ok {
			listed = append(listed, "agent "+name)
		} else if name, ok := strings.CutPrefix(line, shippedSkillMarker); ok {
			listed = append(listed, "skill "+name)
		}
	}
	slices.Sort(listed)
	var want []string
	for _, name := range wantAgents {
		want = append(want, "agent "+name)
	}
	for _, name := range wantSkills {
		want = append(want, "skill "+name)
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("probe.mjs under LEGION_PROMPT_LIST=1 listed\n%s\nwant testdata/texts/shipped-names.txt\n%s\n"+
			"(a change to a skill, an agent or the Oh My Pi pin: capture the file again with testdata/texts/capture-names.sh)\nstderr:\n%s",
			strings.Join(listed, "\n"), strings.Join(want, "\n"), stderr.String())
	}

	env := map[string]string{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	gate := pluginGate{env: env, workDir: work, invocation: omp, timeout: defaultProbeTimeout, retry: bootprobe.Image,
		contract: api.DaemonAPIVersion(), pluginRoot: legion, envoyPluginRoot: envoy, skipAgentModels: true,
		log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	shipped, err := gate.verify(context.Background())
	if err != nil {
		t.Fatalf("the gate's pod lane over both packed plugins = %v, want a pass", err)
	}
	if shipped == nil || !slices.Equal(shipped.Agents, wantAgents) || !slices.Equal(shipped.Skills, wantSkills) {
		t.Fatalf("the gate's pod lane answered %+v, want the names of testdata/texts/shipped-names.txt: agents %v, skills %v", shipped, wantAgents, wantSkills)
	}
}

// shippedNamesFixture is testdata/texts/shipped-names.txt: the agents and the skills a real Oh My
// Pi at the pin discovers over both packed plugins, each list in the file's sorted order.
func shippedNamesFixture(t *testing.T) (agents, skills []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "texts", "shipped-names.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(raw)) {
		kind, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case ok && kind == "agent":
			agents = append(agents, name)
		case ok && kind == "skill":
			skills = append(skills, name)
		default:
			t.Fatalf("shipped-names.txt holds %q, which is no `agent <name>` or `skill <name>` line", line)
		}
	}
	return agents, skills
}

// The extension root a launch of a folder builds, and the settings layer beside it, on the real Oh
// My Pi, which is what the launch's two halves rest on: the root's copy of a replaced text is the
// one the model is offered, under its plain name and with no copy of the plugin's beside it, and
// the layer that hides the plugin's copies keeps the skills the launch already ignored.
//
// The folder replaces a pi-legion skill (legion-worker), a pi-envoy skill (dispatch-first) and a
// pi-legion agent (goal-red-team), each marked in what a request shows of it. The root is built by
// texts.BuildRoot and named first, ahead of both packed plugins, with LEGION_TEXTS_ROOT set for
// pi-envoy; Oh My Pi runs in RPC mode against a model stand-in, and the first request it sends,
// read by omprequest.Read, is held to what the launch must offer: the replaced skills once each
// under their plain names, every other skill of the two plugins once, the `task` tool's agents
// each once with the root's goal-red-team among them, and the root's dispatch-first once.
//
// The layer is written over the skills.ignoredSkills Oh My Pi reads without it, which depends on
// where that list comes from, so each source is measured on its own, and Oh My Pi's own reading
// (`omp config get skills.ignoredSkills`) is held to what texts.BaseIgnoredSkills read:
//
//   - an operator's overlay in PI_CONFIG_FILES ignores a skill;
//   - the project's .omp/config.yml ignores one, with no overlay setting the key;
//   - the profile's agent/config.yml ignores one, with neither;
//   - an overlay and the project both set it, and Oh My Pi keeps the overlay's list alone, since
//     an array in a higher layer replaces a lower one whole (the project's skill is offered);
//   - the negative control: the first case's layer written over no base offers the skill the
//     overlay ignored, so the base is what keeps it out and not something else.
//
// The last case builds the root through the tmux runtime's own helper (tmux.PrepareTexts) over
// the plugins installed into the profile (`omp plugin install`), as a pane loads them, with the
// root as the one `--extension`.
func aTextsRootOnTheRealOhMyPi(t *testing.T, omp, envoy, legion string) {
	t.Helper()
	const marker = "Changed by the arm."
	const armsDispatchFirst = "The arm's own dispatch-first, read from the texts root."

	// The folder: the plugins' own texts with a marker, and a manifest recording the digests.
	shipped := map[string]string{
		"skills/legion-worker/SKILL.md":  filepath.Join(legion, "dist", "skills", "legion-worker", "SKILL.md"),
		"skills/dispatch-first/SKILL.md": filepath.Join(envoy, "dist", "skills", "dispatch-first", "SKILL.md"),
		"agents/goal-red-team.md":        filepath.Join(legion, "agents", "goal-red-team.md"),
	}
	original := map[string]string{}
	for p, file := range shipped {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		original[p] = string(raw)
	}
	arms := map[string]string{
		"skills/legion-worker/SKILL.md": replacedOnce(t, "legion-worker", original["skills/legion-worker/SKILL.md"],
			"\ndescription: Use when", "\ndescription: "+marker+" Use when"),
		"skills/dispatch-first/SKILL.md": replacedOnce(t, "dispatch-first", replacedOnce(t, "dispatch-first", original["skills/dispatch-first/SKILL.md"],
			`description: "Use when`, `description: "`+marker+` Use when`),
			"\n# Dispatch first\n", "\n# Dispatch first\n\n"+armsDispatchFirst+"\n"),
		"agents/goal-red-team.md": replacedOnce(t, "goal-red-team", original["agents/goal-red-team.md"],
			"\n  Goal red team.", "\n  "+marker+" Goal red team."),
	}
	folderDir := filepath.Join(t.TempDir(), "folder")
	digests := map[string]string{}
	for p, text := range arms {
		writeTree(t, folderDir, map[string]string{filepath.FromSlash(p): text})
		digests[p] = texts.Digest([]byte(original[p]))
	}
	if err := texts.WriteManifest(folderDir, digests); err != nil {
		t.Fatal(err)
	}
	folder, err := texts.Read(folderDir)
	if err != nil {
		t.Fatalf("texts.Read of the folder = %v", err)
	}
	if faults := texts.CheckPlugins(folder, envoy, legion); len(faults) != 0 {
		t.Fatalf("texts.CheckPlugins found %v in the folder cut from these plugins", faults)
	}

	visible := visibleSkills(t, envoy, legion)
	wantAgents, _ := shippedNamesFixture(t)

	// check holds the first request to the launch's offer, given the skills the launch ignores.
	check := func(t *testing.T, request omprequest.Request, ignored []string) {
		t.Helper()
		var offered []string
		for _, skill := range request.Skills {
			offered = append(offered, skill.Name)
		}
		slices.Sort(offered)
		var want []string
		for _, name := range visible {
			if !slices.Contains(ignored, name) {
				want = append(want, name)
			}
		}
		if !slices.Equal(offered, want) {
			t.Errorf("the request offers the skills %q, want each of the plugins' skills once under its plain name, less %q: %q (no alias such as pi-legion/legion-worker or pi-envoy/dispatch-first)", offered, ignored, want)
		}
		for _, skill := range request.Skills {
			if skill.Name == "legion-worker" && !strings.HasPrefix(skill.Description, marker) {
				t.Errorf("legion-worker is offered as %q, want the root's copy, which the plugin's differing copy does not displace", skill.Description)
			}
			if skill.Name == "dispatch-first" && !strings.HasPrefix(skill.Description, marker) {
				t.Errorf("dispatch-first is offered as %q, want the root's copy", skill.Description)
			}
		}

		var agents []string
		redTeam := 0
		for _, agent := range request.Agents {
			agents = append(agents, agent.Name)
			if agent.Name == "goal-red-team" {
				redTeam++
				if !strings.HasPrefix(agent.Description, marker) {
					t.Errorf("goal-red-team is offered as %q, want the root's copy", agent.Description)
				}
			}
		}
		slices.Sort(agents)
		if !slices.Equal(agents, wantAgents) || redTeam != 1 {
			t.Errorf("the task tool offers the agents %q (goal-red-team %d times), want each of %q once", agents, redTeam, wantAgents)
		}

		if len(request.DispatchFirst) != 1 || !strings.Contains(request.DispatchFirst[0], armsDispatchFirst) {
			t.Errorf("the request carries the dispatch-first blocks %.300q, want the root's, once", request.DispatchFirst)
		}
	}

	for _, tc := range []struct {
		name                       string
		operator, project, profile string
		// lower is skills.ignoredSkills as Oh My Pi reads it below the layer.
		lower []string
		// bare writes the layer over no base: the negative control.
		bare bool
	}{
		{name: "an operator's overlay ignores a skill", operator: "asd-ste100", lower: []string{"asd-ste100"}},
		{name: "the project's .omp/config.yml ignores a skill", project: "plain-words", lower: []string{"plain-words"}},
		{name: "the profile's config.yml ignores a skill", profile: "legion-retro", lower: []string{"legion-retro"}},
		{name: "an overlay and the project both ignore one: the overlay's list is the base", operator: "asd-ste100", project: "plain-words", lower: []string{"asd-ste100"}},
		{name: "negative control: a layer over no base offers the skill the overlay ignored", operator: "asd-ste100", lower: []string{"asd-ste100"}, bare: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
			standin := omprequest.NewStandin(t)
			agentDir := writeStandinProfile(t, omp, home, standin.URL, ignoringSkill(tc.profile))
			mkdir(t, work)
			var layers []string
			if tc.operator != "" {
				writeTree(t, dir, map[string]string{"operator.yml": ignoringSkill(tc.operator)})
				layers = append(layers, filepath.Join(dir, "operator.yml"))
			}
			if tc.project != "" {
				writeTree(t, work, map[string]string{filepath.Join(".omp", "config.yml"): ignoringSkill(tc.project)})
			}
			environ := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}

			// What Oh My Pi reads below the layer, and what texts.BaseIgnoredSkills read of it.
			lower := ohMyPiIgnoredSkills(t, omp, work, environ, layers)
			base, err := texts.BaseIgnoredSkills(layers, work, agentDir)
			if err != nil {
				t.Fatalf("texts.BaseIgnoredSkills = %v", err)
			}
			if !slices.Equal(lower, tc.lower) || !slices.Equal(base, lower) {
				t.Fatalf("Oh My Pi reads skills.ignoredSkills = %q below the layer, texts.BaseIgnoredSkills reads %q, want %q from the source this case sets", lower, base, tc.lower)
			}
			t.Logf("Oh My Pi reads skills.ignoredSkills = %q below the layer", lower)

			root, err := texts.BuildRoot(folder, envoy, legion, dir, "root")
			if err != nil {
				t.Fatalf("texts.BuildRoot = %v", err)
			}
			defer root.Close()
			over := base
			if tc.bare {
				over = nil
			}
			layer, err := texts.WriteSettingsLayer(root, over, root.Touched)
			if err != nil {
				t.Fatalf("texts.WriteSettingsLayer = %v", err)
			}

			request := firstRequestOfOhMyPi(t, omp, work, standin, root.Dir,
				append(slices.Clone(environ), "PI_CONFIG_FILES="+strings.Join(append(slices.Clone(layers), layer), ":")),
				root.Dir, envoy, legion)
			check(t, request, over)
			if tc.bare {
				for _, skill := range request.Skills {
					if skill.Name == tc.operator {
						return
					}
				}
				t.Errorf("a layer written over no base did not offer %s, which the operator's overlay ignores: the base is not what keeps it out", tc.operator)
			}
		})
	}

	t.Run("the tmux runtime's helper over the plugins installed in the profile", func(t *testing.T) {
		dir := t.TempDir()
		home, work := filepath.Join(dir, "home"), filepath.Join(dir, "work")
		standin := omprequest.NewStandin(t)
		writeStandinProfile(t, omp, home, standin.URL, "")
		mkdir(t, work)
		environ := []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
		installPlugins(t, omp, environ, envoy, legion)

		// The copy is the daemon's, `<state>/texts/<claim>/<n>/folder`, and the root is built beside it.
		copyDir := filepath.Join(dir, "state", "texts", "legion-omp-legion-43-tester", "1", "folder")
		if err := os.CopyFS(copyDir, os.DirFS(folderDir)); err != nil {
			t.Fatal(err)
		}
		spec := runtime.SpawnSpec{Claim: "legion-omp-legion-43-tester", Role: claim.RoleTester,
			Texts: &runtime.Texts{Folder: "/etc/legion/texts/shorter", Copy: copyDir, Experiment: "shorter", Variant: "folder"}}
		fellBack := func(report runtime.TextsReport) { t.Errorf("the helper fell back: %+v", report.Fault) }
		_, built, err := tmux.PrepareTexts(spec, map[string]string{"HOME": home}, work, nil, fellBack)
		if err != nil || built.Root == "" {
			t.Fatalf("tmux.PrepareTexts over the installed plugins = %+v, %v; want the root built", built, err)
		}
		if want := filepath.Join(filepath.Dir(copyDir), "root"); built.Root != want || built.SettingsLayer != filepath.Join(want, texts.SettingsLayerName) {
			t.Fatalf("tmux.PrepareTexts built %+v, want the root %s and its layer", built, want)
		}

		request := firstRequestOfOhMyPi(t, omp, work, standin, built.Root,
			append(slices.Clone(environ), "PI_CONFIG_FILES="+built.SettingsLayer), built.Root)
		check(t, request, nil)
	})
}

// replacedOnce is text with the one place old stands replaced: a text of Legion's that changed so
// that old is gone or stands twice fails the test, which says what to update.
func replacedOnce(t *testing.T, label, text, old, replacement string) string {
	t.Helper()
	if strings.Count(text, old) != 1 {
		t.Fatalf("the shipped %s holds %q %d times, want once: the test's edit of it needs updating", label, old, strings.Count(text, old))
	}
	return strings.Replace(text, old, replacement, 1)
}

// ignoringSkill is a settings file that ignores skill, and nothing when skill is "".
func ignoringSkill(skill string) string {
	if skill == "" {
		return ""
	}
	return "skills:\n  ignoredSkills:\n    - " + skill + "\n"
}

// visibleSkills are the skills the two packed plugins ship that a request lists, sorted: every
// skill directory but the ones whose frontmatter asks to be kept from the model's list.
func visibleSkills(t *testing.T, plugins ...string) []string {
	t.Helper()
	var names []string
	for _, plugin := range plugins {
		skills := filepath.Join(plugin, "dist", "skills")
		entries, err := os.ReadDir(skills)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(skills, entry.Name(), "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			frontmatter, _, _ := strings.Cut(strings.TrimPrefix(string(raw), "---\n"), "\n---")
			if !strings.Contains("\n"+frontmatter+"\n", "\ndisable-model-invocation: true\n") {
				names = append(names, entry.Name())
			}
		}
	}
	slices.Sort(names)
	return names
}

// writeStandinProfile writes the profile a real Oh My Pi runs under in home with one provider, the
// model stand-in at url, serving every model role (testdata/capture.sh's profile), and extra at
// the end of the profile's config.yml. It returns the profile's agent directory.
func writeStandinProfile(t *testing.T, omp, home, url, extra string) string {
	t.Helper()
	testbin.OMPHome(t, omp, home)
	agentDir := filepath.Join(home, ".omp", "agent")
	var config strings.Builder
	config.WriteString("enabledModels:\n  - standin/*\ndisabledProviders: [amazon-bedrock, bedrock-mantle, google, google-vertex, ollama, llama.cpp, lm-studio]\nmodelRoles:\n")
	for _, role := range []string{"default", "smol", "slow", "plan", "task", "commit", "tiny", "vision", "advisor"} {
		config.WriteString("  " + role + ": standin/standin-model\n")
	}
	config.WriteString(extra)
	writeTree(t, agentDir, map[string]string{
		"models.yml": "providers:\n  standin:\n    baseUrl: " + url + "\n    auth: apiKey\n    api: anthropic-messages\n    apiKey: standin-key\n" +
			"    models:\n      - id: standin-model\n        name: Stand-in model\n        reasoning: false\n        input: [text]\n" +
			"        contextWindow: 200000\n        maxTokens: 8000\n        cost: {input: 0, output: 0, cacheRead: 0, cacheWrite: 0}\n",
		"config.yml": config.String(),
	})
	return agentDir
}

// ohMyPiIgnoredSkills is skills.ignoredSkills as the real Oh My Pi reads it in work under environ
// with layers as its PI_CONFIG_FILES: `omp config get`, which layers the profile, the project and
// the overlays as a session does.
func ohMyPiIgnoredSkills(t *testing.T, omp, work string, environ, layers []string) []string {
	t.Helper()
	get := exec.Command(omp, "config", "get", "skills.ignoredSkills", "--json")
	get.Dir, get.Env = work, slices.Clone(environ)
	if len(layers) > 0 {
		get.Env = append(get.Env, "PI_CONFIG_FILES="+strings.Join(layers, ":"))
	}
	var stderr bytes.Buffer
	get.Stderr = &stderr
	out, err := get.Output()
	if err != nil {
		t.Fatalf("omp config get skills.ignoredSkills: %v\n%s", err, stderr.String())
	}
	var answer struct {
		Value []string `json:"value"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("omp config get answered %q: %v", out, err)
	}
	return answer.Value
}

// firstRequestOfOhMyPi runs the real Oh My Pi in RPC mode in work under environ, with
// `--extension` for each of extensions in order, puts one prompt to it, and answers the first
// request it sends the model stand-in with that request read (omprequest.Read), once the
// stand-in has it. The process, and what it started, are ended with the test. LEGION_TEXTS_ROOT,
// which pi-envoy reads, is root, and Dispatch is on, which is what adds dispatch-first to a
// request.
func firstRequestOfOhMyPi(t *testing.T, omp, work string, standin *omprequest.Standin, root string, environ []string, extensions ...string) omprequest.Request {
	t.Helper()
	appended := filepath.Join(t.TempDir(), "append.md")
	writeTree(t, filepath.Dir(appended), map[string]string{"append.md": "The case's appended system prompt.\n"})
	args := []string{"--mode", "rpc"}
	for _, extension := range extensions {
		args = append(args, "--extension", extension)
	}
	args = append(args, "--append-system-prompt", appended, "--no-lsp", "--no-title")
	cmd := exec.Command(omp, args...)
	cmd.Dir = work
	bearer := filepath.Join(t.TempDir(), "dispatch-bearer")
	writeTree(t, filepath.Dir(bearer), map[string]string{"dispatch-bearer": "standin\n"})
	cmd.Env = append(slices.Clone(environ), "LEGION_TEXTS_ROOT="+root, "DISPATCH_URL="+standin.URL, "DISPATCH_TOKEN_FILE="+bearer)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", omp, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	if _, err := io.WriteString(stdin, `{"type":"prompt","message":"First question.","id":"1"}`+"\n"); err != nil {
		t.Fatalf("put the prompt to Oh My Pi: %v", err)
	}
	if !testwait.Holds(testwait.Bounded(t, 3*time.Minute, 30*time.Second), 50*time.Millisecond, func() bool {
		select {
		case <-done:
			return true
		default:
			return len(standin.Turns()) > 0
		}
	}) || len(standin.Turns()) == 0 {
		t.Fatalf("Oh My Pi sent the model stand-in no request with a system prompt and tools; its stderr:\n%s", stderr.String())
	}
	request, err := omprequest.Read(standin.Turns()[0])
	if err != nil {
		t.Fatalf("omprequest.Read of Oh My Pi's first request = %v", err)
	}
	_ = stdin.Close()
	return request
}
