package tmux

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// TextsLaunch is what a launch of an experiment arm's folder of texts runs Oh My Pi with beyond
// the launch of no arm: the extension root PrepareTexts built and the last settings layer written
// in it. The zero value is a launch that runs no folder, the spec's own or the control's after a
// fallback. A pane is told it through PaneInputs.Texts (LEGION_TEXTS_ROOT, and the layer last in
// PI_CONFIG_FILES), and Oh My Pi's argv takes its root first (Args, innerCommand).
type TextsLaunch struct {
	// Root is the extension root, `<state>/texts/<claim>/<n>/root`, holding agents/, skills/ and
	// the settings layer.
	Root string
	// SettingsLayer is `<Root>/texts-settings.yml`.
	SettingsLayer string
}

// Args are the Oh My Pi arguments that load the root: `--extension <Root>`, which comes before
// the installed plugins' own roots, since Oh My Pi gives an explicit root the bare name of a skill
// both it and a plugin ship. nil for a launch that runs no folder.
func (t TextsLaunch) Args() []string {
	if t.Root == "" {
		return nil
	}
	return []string{"--extension", t.Root}
}

// ConfigLayers are the settings overlays of a pane's PI_CONFIG_FILES below the texts layer, in
// their order: the operator's, the role's own, then each arm's, which is what a launch's base
// skills.ignoredSkills is read over (PrepareTexts).
func (in PaneInputs) ConfigLayers() []string {
	return slices.Concat(in.PiConfigFiles, in.RoleConfigFiles, runtime.VariantOverlays(in.Variants))
}

// PrepareTexts is the one preparation of a launch's folder of texts, which the tmux runtime and the
// live proofs' local runtime share, so a proof runs the production check, root and layer. It does
// nothing and answers spec as it is where spec carries no folder (spec.Texts nil). Otherwise it
// reads the daemon's copy of the folder (spec.Texts.Copy), holds its agents and skills to the
// plugins the pane has installed, builds the extension root beside the copy and writes the
// settings layer into it.
//
// The plugins are those of host (EnvoyPlugin and LegionPlugin) or, where it names none, the
// profile's own, `plugins/node_modules/@sjawhar/pi-envoy` and `pi-legion` under the root Oh My Pi
// resolves over env (ompdirs.ProfileRoot), as the boot gate's pane lane finds pi-legion
// (pluginManifestPath). env is the environment the pane's Oh My Pi starts under and workDir its
// working directory; layers are the pane's PI_CONFIG_FILES before the texts layer
// (PaneInputs.ConfigLayers), which with the workspace's and the profile's own settings give the
// skills the layer keeps ignoring (texts.BaseIgnoredSkills).
//
// A copy that cannot be read, a plugin root that cannot be read, and an agent or skill that is at
// a stale digest, whose frontmatter name differs, or that no plugin ships make the launch its
// fallback (runtime.SpawnSpec.FallBack): the spec answered is the control's, with Legion's prompt
// parts and none of the arm's overlays and instructions, and the arm is out of its spend
// attributes; no root is built, and fellBack, when not nil, is told which launch fell back and
// why. A fault that is the machine's rather than the folder's, such as a settings overlay Oh My Pi
// would refuse to start on, or a root it cannot write, is the launch's error.
//
// Call it before the prompt is composed: a fallback changes the prompt.
func PrepareTexts(spec runtime.SpawnSpec, host Host, env map[string]string, workDir string, layers []string, fellBack runtime.TextsFellBack) (runtime.SpawnSpec, TextsLaunch, error) {
	prepared, fallback, err := checkTexts(spec, host, env, workDir, layers)
	if err != nil {
		return runtime.SpawnSpec{}, TextsLaunch{}, err
	}
	if fallback != nil {
		if fellBack != nil {
			fellBack(fallback.report)
		}
		return fallback.spec, TextsLaunch{}, nil
	}
	if prepared == nil {
		return spec, TextsLaunch{}, nil
	}
	built, err := prepared.build()
	if err != nil {
		return runtime.SpawnSpec{}, TextsLaunch{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	return spec, built, nil
}

// checkArmTexts is checkTexts for a pane of this runtime: its plugins are the ones installed in the
// profile the pane runs under, found over the pane's environment as the boot gate's pane lane
// finds them, the workspace is the one the pane starts in, and the layers are the operator's, the
// role's and the arms' that PI_CONFIG_FILES names. It answers the spec the launch goes on with: the
// control's where the folder failed a check, with the report the daemon is told of it, and the
// spec as it is otherwise.
func (r *Runtime) checkArmTexts(spec runtime.SpawnSpec) (*checkedTexts, *runtime.TextsReport, runtime.SpawnSpec, error) {
	if spec.Texts == nil {
		return nil, nil, spec, nil
	}
	workDir, err := WorkspaceDir(r.stateDir, spec.Repository, spec.Issue)
	if err != nil {
		return nil, nil, spec, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	env := maps.Clone(r.paneEnv)
	maps.Copy(env, r.operatorEnv)
	layers := PaneInputs{PiConfigFiles: r.piConfigFiles, RoleConfigFiles: r.roleConfigFiles[spec.Role], Variants: spec.Variants}.ConfigLayers()
	checked, fallback, err := checkTexts(spec, Host{StateDir: r.stateDir, Legion: r.legion}, env, workDir, layers)
	switch {
	case err != nil:
		return nil, nil, spec, err
	case fallback != nil:
		return nil, &fallback.report, fallback.spec, nil
	}
	return checked, nil, spec, nil
}

// checkedTexts is a folder of texts that passed every check, ready to build its root.
type checkedTexts struct {
	folder                texts.Folder
	envoyRoot, legionRoot string
	root                  string
	base                  []string
}

// textsFallback is a launch that fell back: the control's spec, and the report of why.
type textsFallback struct {
	spec   runtime.SpawnSpec
	report runtime.TextsReport
}

// checkTexts is PrepareTexts less its writes: nothing is made on disk until build. The tmux
// runtime calls it before it composes the prompt and builds under its launch lock, once the
// refusal of a claim whose process still runs has passed, so a refused launch rewrites no root a
// live process reads. Exactly one of the results is not nil for a spec that carries a folder, and
// none for one that does not.
func checkTexts(spec runtime.SpawnSpec, host Host, env map[string]string, workDir string, layers []string) (*checkedTexts, *textsFallback, error) {
	if spec.Texts == nil {
		return nil, nil, nil
	}
	fellBack := func(fault texts.Fault) (*checkedTexts, *textsFallback, error) {
		fault.Dir = spec.Texts.Folder
		next, report := spec.FallBack(fault)
		return nil, &textsFallback{spec: next, report: report}, nil
	}

	folder, err := texts.Read(spec.Texts.Copy)
	if err != nil {
		var fault *texts.Fault
		if errors.As(err, &fault) {
			return fellBack(*fault)
		}
		return fellBack(texts.Fault{Cause: texts.Unreadable, Detail: err.Error()})
	}
	envoyRoot, legionRoot, agentDir, err := textsHost(host, env, workDir)
	if err != nil {
		return nil, nil, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	if err := readablePlugins(envoyRoot, legionRoot); err != nil {
		return fellBack(texts.Fault{Cause: texts.Unreadable, Detail: err.Error()})
	}
	if faults := texts.CheckPlugins(folder, envoyRoot, legionRoot); len(faults) > 0 {
		return fellBack(faults[0])
	}
	// A caller that names neither the pane's directories nor its layers (a Launch of a spec whose
	// root nobody prepared) has no settings to read a base from.
	var base []string
	if workDir != "" && agentDir != "" {
		if base, err = texts.BaseIgnoredSkills(layers, workDir, agentDir); err != nil {
			return nil, nil, fmt.Errorf("spawn %s: %w", spec.Claim, err)
		}
	}
	return &checkedTexts{
		folder: folder, envoyRoot: envoyRoot, legionRoot: legionRoot, base: base,
		root: filepath.Join(filepath.Dir(spec.Texts.Copy), "root"),
	}, nil, nil
}

// build writes the extension root beside the daemon's copy of the folder, removing what an earlier
// build of the launch's generation left, and the settings layer in it.
func (c *checkedTexts) build() (TextsLaunch, error) {
	if err := os.RemoveAll(c.root); err != nil {
		return TextsLaunch{}, fmt.Errorf("remove the texts root of an earlier launch: %w", err)
	}
	root, err := texts.BuildRoot(c.folder, c.envoyRoot, c.legionRoot, c.root)
	if err != nil {
		return TextsLaunch{}, err
	}
	layer, err := texts.WriteSettingsLayer(root.Dir, c.base, root.Touched)
	if err != nil {
		return TextsLaunch{}, err
	}
	return TextsLaunch{Root: root.Dir, SettingsLayer: layer}, nil
}

// textsHost is the two plugin roots a launch loads and the agent directory of the profile it runs
// under, where its own config.yml sits ("" where env names nothing to resolve it over). A root
// host names is that root; the rest are the profile's installed plugins, found as the boot gate's
// pane lane finds pi-legion (pluginManifestPath).
//
// The agent directory is the config root's `agent`: an XDG data root that holds the profile
// moves its plugins, sessions and other data-class files, never config.yml, so it is resolved with
// no XDG data home.
func textsHost(host Host, env map[string]string, workDir string) (envoyRoot, legionRoot, agentDir string, err error) {
	envoyRoot, legionRoot = host.EnvoyPlugin, host.LegionPlugin
	if envoyRoot == "" || legionRoot == "" {
		root, _, err := ompdirs.ProfileRoot(env, workDir)
		if err != nil {
			return "", "", "", err
		}
		plugins := filepath.Join(root, "plugins", "node_modules", "@sjawhar")
		if envoyRoot == "" {
			envoyRoot = filepath.Join(plugins, "pi-envoy")
		}
		if legionRoot == "" {
			legionRoot = filepath.Join(plugins, "pi-legion")
		}
	}
	if len(env) > 0 {
		config := maps.Clone(env)
		delete(config, "XDG_DATA_HOME")
		root, _, err := ompdirs.ProfileRoot(config, workDir)
		if err != nil {
			return "", "", "", err
		}
		agentDir = filepath.Join(root, "agent")
	}
	return envoyRoot, legionRoot, agentDir, nil
}

// readablePlugins is nil when the directories the root is built from and the check reads can be
// read: pi-envoy's skills, and pi-legion's agents and skills. A plugin that is not installed, or
// whose pack is cut, would otherwise read as a plugin that ships none of the folder's texts.
func readablePlugins(envoyRoot, legionRoot string) error {
	for _, dir := range []string{
		filepath.Join(envoyRoot, "dist", "skills"),
		filepath.Join(legionRoot, "agents"),
		filepath.Join(legionRoot, "dist", "skills"),
	} {
		if _, err := os.ReadDir(dir); err != nil {
			return fmt.Errorf("the installed plugin's texts cannot be read: %w", err)
		}
	}
	return nil
}
