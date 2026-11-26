package api

// 20261126031014 texts-folder-root: an experiment arm can name a folder of texts that replaces
// Legion's own role prompt parts, agents, skills and dispatch-first. Two surfaces on this
// contract gain members:
//
// The pane and worker-container environment gains `LEGION_TEXTS_ROOT`, which both runtimes set
// when a launch runs a folder of texts (`internal/runtime/tmux/spawn.go`'s `panePairs` and
// `internal/runtime/sandbox/manifest.go`'s `mainEnvironment`, each also keeping the name in its
// runtime-owned set so the loader refuses an operator variable of that name). It names the
// extension root built for the launch (`<state>/texts/<claim>/<n>/root` under tmux,
// `<roleStateHome>/legion-texts/g<generation>` in a pod). At module load `@sjawhar/pi-envoy` reads
// `LEGION_TEXTS_ROOT` and sets it to the empty string so an Oh My Pi a worker starts reads
// Legion's own texts; when the root holds `skills/dispatch-first/SKILL.md`, that file is the
// `dispatch-first` it adds to each request in place of its own, and when the root holds `skills/`,
// `resources_discover` gives that folder in place of its own. A plugin built before this entry
// ignores `LEGION_TEXTS_ROOT` and would offer its own `dispatch-first` and skills over the arm's,
// so the boot gate and `legion probe-image` pair a daemon of this entry with plugins and an image
// that read it.
//
// The launcher wire gains three optional members by which a pod's launcher says it checks a folder
// of texts, is handed one, and refuses one: `shimwire.LauncherHello.ChecksTexts` (`checksTexts`,
// omitempty), `shimwire.LauncherStart.Texts` (`texts`: `folder`, `fingerprint`, `root`,
// `envoyRoot`, `legionRoot`, omitempty), and `shimwire.LauncherStartResult.Texts` (`texts`:
// `path`, `cause`, `detail`, omitempty). The skew is safe in both directions: an older launcher's
// hello omits `checksTexts`, which decodes as false, and the sandbox runtime then sends the start
// of the arm's control rather than an unchecked folder; an older daemon's start omits `texts` and
// a launcher of this entry runs it as a start of no folder. No shape of `GET /legion/v1/state` or
// any other HTTP answer a plugin parses moves (`api.VariantSettings` gains `texts`, omitempty,
// under the loosely parsed `settings` block, which moves no contract shape by the settings rule of
// version.go).
const textsFolderRootContract = 20261126031014

func init() {
	registerEntry(contractEntry{id: textsFolderRootContract, slug: "texts-folder-root"})
}
