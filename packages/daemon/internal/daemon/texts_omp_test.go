package daemon

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/testbin"
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
