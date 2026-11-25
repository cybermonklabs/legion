package sandbox

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// An experiment arm's folder of texts reaches the agent in a pod as one extension
// root, built by the launcher on the issue's volume at `<role state home>/legion-texts/g<N>`, and
// Oh My Pi must load it before both plugins: the root's copy of a skill the plugin ships too keeps
// the plain name, and the plugin's differing copy is only an alias (Oh My Pi's skills
// documentation, "Collision and duplicate handling"), which the arm's settings layer then hides. A root named after either plugin
// would make the plugin's copy the one the model reads, so the order of agentArgv's flags is the
// observable of the requirement. The order check below reads the argument list a pod's agent is
// started with and passes only a list whose extension roots are the arm's root, pi-envoy's and
// pi-legion's, in that order, all before `--model`. The last subtest is the negative control of
// the order test: the same check on the arguments with the root after pi-legion fails, so a check
// that passed whatever the order was would show here.
func TestATextsLaunchPutsItsRootAheadOfBothPlugins(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.Variants = []runtime.Variant{{Experiment: "shorter-prompts", Name: "texts", Texts: "/etc/legion/texts/shorter"}}
	spec.Texts = &runtime.Texts{
		Folder:      "/etc/legion/texts/shorter",
		Fingerprint: strings.Repeat("ab", 32),
		Experiment:  "shorter-prompts",
		Variant:     "texts",
	}
	root := roleStateHome(spec.Role) + "/legion-texts/g" + strconv.FormatUint(spec.Generation, 10)

	t.Run("a launch carrying a folder", func(t *testing.T) {
		argv := launchOf(t, r, spec).agentArgv(r.agent)
		if err := rootAheadOfBothPlugins(argv, root); err != nil {
			t.Errorf("agentArgv = %q: %v", argv, err)
		}
		want := slices.Concat(r.agent, []string{"--extension", root, "--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default",
			"--mode", "rpc", "--append-system-prompt", systemPromptPath(spec.Generation)})
		if !slices.Equal(argv, want) {
			t.Errorf("agentArgv = %q, want %q", argv, want)
		}
	})

	t.Run("a launch that resumes names its generation's own root", func(t *testing.T) {
		later := spec
		later.Generation, later.BootToken, later.ResumeSessionFile = 2, "boot-g2", resumeSession
		argv := launchOf(t, r, later).agentArgv(r.agent)
		if err := rootAheadOfBothPlugins(argv, roleStateHome(later.Role)+"/legion-texts/g2"); err != nil {
			t.Errorf("agentArgv of generation 2 = %q: %v", argv, err)
		}
	})

	t.Run("a launch carrying no folder keeps the two plugins alone", func(t *testing.T) {
		plain := spec
		plain.Variants, plain.Texts = nil, nil
		argv := launchOf(t, r, plain).agentArgv(r.agent)
		want := slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default",
			"--mode", "rpc", "--append-system-prompt", systemPromptPath(plain.Generation)})
		if !slices.Equal(argv, want) {
			t.Errorf("agentArgv with no folder = %q, want %q", argv, want)
		}
	})

	t.Run("negative control: the order test fails arguments with the root after pi-legion", func(t *testing.T) {
		model := []string{"--model", "@default", "--mode", "rpc"}
		for name, argv := range map[string][]string{
			"the root after pi-legion":    slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--extension", root}, model),
			"the root between the two":    slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", root, "--extension", legionPlugin}, model),
			"the root after the model":    slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin, "--model", "@default", "--extension", root, "--mode", "rpc"}),
			"no root":                     slices.Concat(r.agent, []string{"--extension", envoyPlugin, "--extension", legionPlugin}, model),
			"a root of another role":      slices.Concat(r.agent, []string{"--extension", "/legion/state/other/legion-texts/g1", "--extension", envoyPlugin, "--extension", legionPlugin}, model),
			"the root and a plugin twice": slices.Concat(r.agent, []string{"--extension", root, "--extension", envoyPlugin, "--extension", legionPlugin, "--extension", legionPlugin}, model),
		} {
			if err := rootAheadOfBothPlugins(argv, root); err == nil {
				t.Errorf("the order test passed %s: %q", name, argv)
			}
		}
		good := slices.Concat(r.agent, []string{"--extension", root, "--extension", envoyPlugin, "--extension", legionPlugin}, model)
		if err := rootAheadOfBothPlugins(good, root); err != nil {
			t.Errorf("the order test failed arguments with the root first: %v", err)
		}
	})
}

// rootAheadOfBothPlugins is nil when argv names exactly three extension roots, root, then the
// image's pi-envoy, then its pi-legion, and names all three before `--model`.
func rootAheadOfBothPlugins(argv []string, root string) error {
	var roots []string
	last, model := -1, -1
	for i, word := range argv {
		switch word {
		case "--extension":
			if i+1 == len(argv) {
				return fmt.Errorf("--extension names no root")
			}
			roots = append(roots, argv[i+1])
			last = i
		case "--model":
			model = i
		}
	}
	switch {
	case !slices.Equal(roots, []string{root, envoyPlugin, legionPlugin}):
		return fmt.Errorf("the extension roots are %q, want the arm's root %q, then %q and %q", roots, root, envoyPlugin, legionPlugin)
	case model < 0:
		return fmt.Errorf("no --model flag, so the roots cannot come before it")
	case last > model:
		return fmt.Errorf("an --extension at word %d comes after --model at word %d", last, model)
	}
	return nil
}
