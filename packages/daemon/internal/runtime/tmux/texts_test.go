package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// An experiment arm's folder of texts reaches the agent as one extension root that
// Oh My Pi loads before the plugins a pane has installed: `--extension <root>` ahead of
// `--model @default`. Oh My Pi gives an explicit root the bare name of a skill both it and a
// plugin ship, and the plugin's differing copy an alias, so a root named after the plugins (or not
// at all) would make the plugin's copy the one the model reads. The order is the observable of the
// requirement, so it is held at each place the command line is built: innerCommand, and the
// launcher the daemon's live proofs call in place of a tmux pane (NewOmpLauncher's Launch, whose
// inner command and pairs a local runtime runs Oh My Pi under). The installed plugins appear in no
// flag, so the root is the one `--extension` of the command. The last subtest is the negative
// control: the same check, run on a command with the root after the model flag, on one with no
// root and on one with another root, fails, so a check that passed whatever the command said would
// show here.
func TestATextsLaunchPutsItsRootBeforeTheInstalledPlugins(t *testing.T) {
	const omp = "/opt/omp/bin/omp"
	const promptFile = "/state/secrets/legion-omp-legion-43-tester-system-prompt.md"

	t.Run("innerCommand", func(t *testing.T) {
		root := "/state/texts/legion-omp-legion-43-tester/1/root"
		for resume, want := range map[string]string{
			"":                              omp + " --extension " + root + " --model @default --mode rpc --append-system-prompt " + promptFile,
			"/state/trees/x/.omp/s 1.jsonl": omp + " --extension " + root + " --model @default --resume='/state/trees/x/.omp/s 1.jsonl' --mode rpc --append-system-prompt " + promptFile,
		} {
			got := innerCommand(nil, omp, resume, promptFile, root)
			if got != want {
				t.Errorf("innerCommand with a root (resume %q) = %q, want %q", resume, got, want)
			}
			if err := rootBeforeTheModel(got, root); err != nil {
				t.Errorf("innerCommand with a root (resume %q): %v", resume, err)
			}
		}
		// A launch with no root is the command it always was.
		if got, want := innerCommand(nil, omp, "", promptFile), omp+" --model @default --mode rpc --append-system-prompt "+promptFile; got != want {
			t.Errorf("innerCommand with no root = %q, want %q", got, want)
		}
		// Behind an operator's launch prefix the root still follows the invocation and leads the
		// flags.
		got := innerCommand([]string{"env", "K=v", "--"}, omp, "", promptFile, root)
		if want := "env 'K=v' -- " + omp + " --extension " + root + " --model @default --mode rpc --append-system-prompt " + promptFile; got != want {
			t.Errorf("innerCommand behind a prefix = %q, want %q", got, want)
		}
	})

	t.Run("the launcher a local runtime runs Oh My Pi by", func(t *testing.T) {
		spec, host, root := textsLaunch(t)
		inner, _, err := NewOmpLauncher(nil, omp).Launch(spec, host, promptFile)
		if err != nil {
			t.Fatalf("Launch of a spec carrying a folder of texts = %v, want the command", err)
		}
		if err := rootBeforeTheModel(inner, root); err != nil {
			t.Errorf("Launch's command %q: %v", inner, err)
		}
		if _, err := os.Stat(filepath.Join(root, "agents", "goal-red-team.md")); err != nil {
			t.Errorf("the command names the root %s, but the launch built none holding the folder's agent: %v", root, err)
		}
		// The launch of no folder names no root: the command is innerCommand's own.
		plain := spec
		plain.Texts, plain.Variants = nil, nil
		control, _, err := NewOmpLauncher(nil, omp).Launch(plain, host, promptFile)
		if err != nil {
			t.Fatal(err)
		}
		if want := innerCommand(nil, omp, "", promptFile); control != want {
			t.Errorf("Launch of a spec with no folder = %q, want %q", control, want)
		}
	})

	t.Run("negative control: the order check fails a command that breaks the order", func(t *testing.T) {
		const root = "/state/texts/legion-omp-legion-43-tester/1/root"
		for name, command := range map[string]string{
			"the root after the model flag":  omp + " --model @default --extension " + root + " --mode rpc --append-system-prompt " + promptFile,
			"the root after every flag":      omp + " --model @default --mode rpc --append-system-prompt " + promptFile + " --extension " + root,
			"no root":                        omp + " --model @default --mode rpc --append-system-prompt " + promptFile,
			"another root":                   omp + " --extension /opt/legion/pi-legion --model @default --mode rpc --append-system-prompt " + promptFile,
			"the root and a plugin's root":   omp + " --extension " + root + " --extension /opt/legion/pi-legion --model @default --mode rpc",
			"the root with no model flag":    omp + " --extension " + root + " --mode rpc --append-system-prompt " + promptFile,
			"an --extension with no value":   omp + " --model @default --extension",
			"a model flag before a plugin's": omp + " --model @default --extension " + root + " --extension /opt/legion/pi-envoy",
		} {
			if err := rootBeforeTheModel(command, root); err == nil {
				t.Errorf("the order check passed %s: %q", name, command)
			}
		}
		if err := rootBeforeTheModel(omp+" --extension "+root+" --model @default --mode rpc", root); err != nil {
			t.Errorf("the order check failed the command with the root first: %v", err)
		}
	})
}

// rootBeforeTheModel is nil when the Oh My Pi command line names exactly one extension root, root,
// and names it before the model flag: the installed plugins load after the explicit roots, so
// nothing else is named, and the first root Oh My Pi reads is the arm's.
func rootBeforeTheModel(command, root string) error {
	words := strings.Fields(command)
	var roots []string
	rootAt, modelAt := -1, -1
	for i, word := range words {
		switch word {
		case "--extension":
			if i+1 == len(words) {
				return fmt.Errorf("--extension names no root")
			}
			roots = append(roots, words[i+1])
			if words[i+1] == root {
				rootAt = i
			}
		case "--model":
			modelAt = i
		}
	}
	switch {
	case !slices.Equal(roots, []string{root}):
		return fmt.Errorf("the extension roots are %q, want %q alone", roots, root)
	case modelAt < 0:
		return fmt.Errorf("no --model flag, so the root cannot come before it")
	case rootAt > modelAt:
		return fmt.Errorf("--extension %s is at word %d, after --model at word %d", root, rootAt, modelAt)
	}
	return nil
}

// textsLaunch is a spec for the tester's claim that carries a folder of texts, with the state
// directory its launch writes under, the two plugins the pane has installed (each holding the one
// text the folder replaces or none), and the extension root the launch builds beside the daemon's
// copy of the folder: `<state>/texts/<claim>/<n>/root`, the copy being `<n>/folder`. The folder's
// manifest records the digest of the plugin's agent, the Legion text its own agent replaces.
func textsLaunch(t *testing.T) (runtime.SpawnSpec, Host, string) {
	t.Helper()
	dir := t.TempDir()
	state, envoy, legion := filepath.Join(dir, "state"), filepath.Join(dir, "pi-envoy"), filepath.Join(dir, "pi-legion")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agent := "---\nname: goal-red-team\ndescription: Goal red team.\n---\nReads the change against the goal.\n"
	write(filepath.Join(envoy, "dist", "skills", "dispatch-first", "SKILL.md"), "---\nname: dispatch-first\ndescription: Use when a session has the dispatch command.\n---\nSearch first.\n")
	write(filepath.Join(legion, "agents", "goal-red-team.md"), agent)
	write(filepath.Join(legion, "dist", "skills", "legion-worker", "SKILL.md"), "---\nname: legion-worker\ndescription: Use when dispatched as a worker.\n---\nWork.\n")

	spec := testSpec()
	spec.Variants = []runtime.Variant{{Experiment: "shorter-prompts", Name: "texts", Texts: "/etc/legion/texts/shorter"}}
	folder := filepath.Join(state, "texts", string(spec.Claim), "1", "folder")
	digest := sha256.Sum256([]byte(agent))
	write(filepath.Join(folder, "agents", "goal-red-team.md"), "---\nname: goal-red-team\ndescription: Changed by the arm.\n---\nReads the change.\n")
	write(filepath.Join(folder, "manifest.json"), `{"files":{"agents/goal-red-team.md":"sha256:`+hex.EncodeToString(digest[:])+`"}}`)
	spec.Texts = &runtime.Texts{
		Folder:      "/etc/legion/texts/shorter",
		Fingerprint: strings.Repeat("ab", 32),
		Copy:        folder,
		Experiment:  "shorter-prompts",
		Variant:     "texts",
	}
	return spec, Host{StateDir: state, Legion: "/opt/legion/legion", EnvoyPlugin: envoy, LegionPlugin: legion}, filepath.Join(filepath.Dir(folder), "root")
}
