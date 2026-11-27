// Package podsafety is what a Legion pod's Oh My Pi starts with beyond the operator's pod, and
// nothing in it names a model, a provider, or a route, nor holds a repository's settings off. It is
// four things. The baseline variables: two of the variables that place a pod's sessions
// (baseline), set on the agent where the pod leaves them unset — when Oh My Pi would otherwise
// fill them from the working directory's .env — and never on the shim; the Sandbox runtime
// refuses them, and OMP_SESSION_SQL_DSN_FILE, in the operator's pod (sandbox.CheckPod).
// Another is OMP_CONFIG_ROOT, which from the pinned Oh My Pi on outranks PI_CONFIG_DIR as the
// config root the agent directory, models.yml and file sessions live under. The runtime refuses it
// in the operator's pod and does not set it, and Apply does not either, so a repository's .env that
// sets it moves the config root: Oh My Pi applies its working directory's .env and then rebuilds its
// directories (env.ts, refreshDirsFromEnv).
// Every session of a workflow role is a row in the database the deployment must name
// (runtime.kubernetes.session_store postgres or cloudsql-iam): the runtime starts every such
// generation with OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE itself, and Apply keeps
// both. The baseline's `file` is left for the one
// container the runtime tells neither, the image probe's, whose session nothing resumes — neither
// the probe's pod nor an issue's mounts a disk that outlives it, an issue pod's being the pod's
// own ephemeral volume (the project controller's pod does mount one its Sandbox owns, and it is
// told the database like every other role). The state-home
// directory: the one Oh My Pi's state root resolves to under the role's XDG_STATE_HOME
// (EnsureStateHome), made before Oh My Pi starts, since Oh My Pi reads the variable only where
// that directory exists. The scratch directories: each directory the runtime tells a role to
// write what it writes without being told where — the temporary directory, the cache and data
// homes, bun's install cache and the two Go caches (EnsureScratch) — made before Oh My Pi
// starts, since a role told a temporary directory nothing created cannot run. And the image's gh
// extensions, linked into gh's extension directory under the data home, the one place gh reads
// an extension from (EnsureGhExtensions). Apply, the baseline
// alone, runs only in a pod, for a worker's shim (`legion worker-shim
// --pod-safety`) and for the image probe (`legion probe-image --pod-safety`), which certifies the
// baseline a worker's Oh My Pi starts on. The directories are the shim's alone: every role the
// shim starts is told a state home of its own (sandbox.roleStateHome), which in an issue pod keeps
// a role's browser broker lock apart from its siblings', and the issue's scratch on the issue's
// volume, which holds all of those but the two Go caches beside it and which every role of the
// pod is told the same of, while the probe pod runs one container, on the image's own
// directories, and is told neither.
//
// Legion writes no settings overlay of its own. A role's PI_CONFIG_FILES is the operator's own
// overlays with the role's own after them, composed by the runtime that starts the role
// (sandbox.roleOperatorEnv in a pod, tmux.panePairs in a pane), and Apply passes the value the pod
// carries through untouched: a repository's settings reach a pod's agent as they reach any agent
// session, under the operator's, and nothing of Legion's outranks either.
package podsafety

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
)

// placesSessions is why an operator's pod may not set a variable that decides where Oh My Pi keeps
// a session: Legion places a pod's sessions where a resume reads them, which under the Kubernetes
// runtime is the session database and nothing else, a pod's disk dying with the pod (INFRA-80).
const placesSessions = "it decides where Oh My Pi keeps the session a resume reads"

// baseline are the variables Apply sets where the pod's environment leaves them unset or empty,
// which is when Oh My Pi would fill them from the working directory's .env: two of the variables
// that place a pod's sessions, each with why an operator's pod may not set it instead
// (Variable.Reserved). Another, OMP_CONFIG_ROOT, outranks PI_CONFIG_DIR from the pinned Oh My Pi
// on; it is not here, so a repository's .env that sets it moves the config root (see the package
// doc). Nothing else is set on a pod's agent; a repository's own settings and .env are its own
// otherwise.
var baseline = []struct{ name, value, reserved string }{
	// Names the config root Oh My Pi's agent directory is joined under (.omp in the image), which
	// chooses the models.yml and profile it reads, and the sessions under it: no settings overlay
	// outranks it.
	{"PI_CONFIG_DIR", ".omp", placesSessions},
	// Outranks session.storage, and with OMP_SESSION_SQL_DSN_FILE would write the conversation to
	// a database the repository names; `file` keeps each session a file. A runtime that keeps
	// sessions in its own database sets sql and the URL file's pointer before Apply runs.
	{"OMP_SESSION_STORAGE", "file", placesSessions},
}

// Variable is one variable Apply sets, with Reserved: why an operator's pod may not set it itself,
// since Apply yields to the pod's own value and such a value would give away something the runtime
// relies on. Every variable Variables publishes carries one.
type Variable struct {
	Name, Reserved string
}

// Variables are the variables Apply sets: each baseline variable, with its reason.
func Variables() []Variable {
	variables := make([]Variable, 0, len(baseline))
	for _, v := range baseline {
		variables = append(variables, Variable{Name: v.name, Reserved: v.reserved})
	}
	return variables
}

// Apply is environ as a pod's Oh My Pi starts with it: each baseline variable set where environ
// leaves it unset or empty, and nothing else. PI_CONFIG_FILES rides through as the pod carries it,
// the operator's overlays and the role's own, already composed by the runtime that starts the
// role, and no file is written anywhere, since Legion names no settings overlay of its own.
func Apply(environ []string) []string {
	out := make([]string, 0, len(environ)+len(baseline))
	set := map[string]bool{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		if value != "" {
			set[name] = true
		}
		out = append(out, pair)
	}
	for _, v := range baseline {
		if !set[v.name] {
			out = append(removed(out, v.name), v.name+"="+v.value)
		}
	}
	return out
}

// removed is environ without its entries for name.
func removed(environ []string, name string) []string {
	return slices.DeleteFunc(environ, func(pair string) bool { return strings.HasPrefix(pair, name+"=") })
}

// CheckOverlay refuses file, a settings overlay a role's PI_CONFIG_FILES names, unless it is a
// regular file this process can open, as the role's Oh My Pi must: Oh My Pi refuses to start on an
// overlay it cannot read ("Config overlay not found"). A symbolic link is followed, since a
// ConfigMap's file is one.
func CheckOverlay(file string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", file)
	}
	opened, err := os.Open(file)
	if err != nil {
		return err
	}
	return opened.Close()
}

// EnsureStateHome makes the directory Oh My Pi's state root resolves to under environ's
// XDG_STATE_HOME, so the variable takes effect. Oh My Pi's state-class directories (`run/daemons`,
// `logs`, `browser-profiles`, `reports`, `terminal-sessions`; sessions are data-class) hang under
// `$XDG_STATE_HOME/omp/profiles/<profile>` for a named profile and `$XDG_STATE_HOME/omp` for the
// default one, but only when that directory already exists when Oh My Pi starts; otherwise they
// fall back to the config root's `profiles/<profile>` (`$OMP_CONFIG_ROOT` when it names an
// absolute directory, else `$HOME/$PI_CONFIG_DIR`, as ompdirs.StateRoot ports it), and a state
// home the pod set to keep one role's browser broker lock apart from its siblings'
// (sandbox.roleStateHome) changes nothing. The directory is the one ompdirs answers
// (ompdirs.StateRootCandidate), so the shim and the port cannot drift: the profile is OMP_PROFILE
// where environ defines it, else PI_PROFILE, as Oh My Pi reads them, and a name Oh My Pi would
// refuse is refused here. With XDG_STATE_HOME unset or empty nothing is made: Oh My Pi reads no
// state home then. Once made, ompdirs.StateRoot over environ, in the working directory the shim's
// Oh My Pi inherits, must answer that directory; where it answers another — an honoured
// PI_CODING_AGENT_DIR elsewhere turns Oh My Pi's XDG lookup off — this refuses, naming both,
// rather than let the shim make a directory Oh My Pi ignores while every role's broker takes one
// lock name.
func EnsureStateHome(environ []string) error {
	env := lookup(environ)
	dir, ok, err := ompdirs.StateRootCandidate(env)
	if err != nil {
		return fmt.Errorf("pod safety: no state directory to make under XDG_STATE_HOME=%s: %w", env["XDG_STATE_HOME"], err)
	}
	if !ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pod safety: make Oh My Pi's state directory %s: %w", dir, err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("pod safety: the working directory Oh My Pi starts in: %w", err)
	}
	root, _, err := ompdirs.StateRoot(env, workDir)
	if err != nil {
		return fmt.Errorf("pod safety: %w", err)
	}
	if root != dir {
		return fmt.Errorf("pod safety: Oh My Pi would keep its state under %s, not under %s, the directory made under XDG_STATE_HOME: the state home changes nothing (an honoured PI_CODING_AGENT_DIR elsewhere turns Oh My Pi's XDG lookup off)", root, dir)
	}
	return nil
}

// ScratchVariables are the variables EnsureScratch makes the directory of, in the order it makes
// them: what sandbox.roleEnvironment tells a role to write in without being told where,
// wherever on the issue's volume the runtime puts them — the issue's scratch for the temporary
// directory, the cache and data homes and bun's install cache, the pair beside it for the two Go
// caches, every one of them the same for every role of the pod — less XDG_STATE_HOME, which is
// the role's own and EnsureStateHome's. The runtime is held to this list rather than to a copy
// of it (sandbox.TestTheRuntimeTellsTheShimEveryScratchDirectoryItMakes), so a directory the
// runtime starts naming that the shim would never make is a test failure, not a role whose
// temporary directory does not exist.
var ScratchVariables = []string{"TMPDIR", "XDG_CACHE_HOME", "XDG_DATA_HOME", "GOCACHE", "GOMODCACHE", "BUN_INSTALL_CACHE_DIR"}

// EnsureScratch makes every directory environ names in ScratchVariables — TMPDIR, XDG_CACHE_HOME,
// XDG_DATA_HOME, GOCACHE, GOMODCACHE and BUN_INSTALL_CACHE_DIR — with every parent, before Oh My
// Pi starts. A name environ leaves unset or empty, or sets to a relative path, makes nothing: the
// runtime always sets absolute ones (sandbox.roleEnvironment, on the issue's volume), a relative
// one is not the shim's to resolve, and making it under whatever working directory the pod
// happens to have would be worse than leaving the tool its own default. A directory that cannot
// be made is a refusal naming the variable and the path, so a role whose volume cannot be written
// never starts half-configured rather than part way through its first turn. XDG_STATE_HOME is not
// here: EnsureStateHome keeps that one, since it makes Oh My Pi's profile directory under the
// state home rather than the state home itself, and its refusal carries its own reason.
//
// `go` and `bun` make their own cache directories, so GOCACHE, GOMODCACHE and BUN_INSTALL_CACHE_DIR
// would come up by themselves; TMPDIR has no creator at all — mktemp and every library equivalent
// fail where the directory the variable names does not exist, and the pod's roles are
// told no other temporary directory. The list is the whole told set rather than that one name, so
// the shim makes what the runtime said it would use whatever the tool reading it does.
//
// The shim is what makes them, not the kubelet: a subPath mount per directory would have the
// kubelet create each one, but it would also contradict the rule that these paths sit on the issue
// volume's own mount (sandbox.TestTheIssuesRolesShareOneScratchAndOneGoCachePair), ask the
// kubelet for twelve directories — the four the issue's roles share, the two Go caches beside
// them and a state home for each of the issue pod's six roles — and rest on an unproven
// ownership behaviour: what mode a kubelet gives a nested sub-path directory it creates on a
// fresh volume. The shim runs as the pod's own user, which can write the volume, and makes
// exactly the directories the runtime told it to use, read from the same environment the agent
// starts on, so the names cannot drift.
//
// TMPDIR is left without the set-group-ID bit. The issue's volume carries the pod's fsGroup, and
// the kubelet's ownership change for it sets that bit on each of the volume's directories, so
// Linux gives it to every directory made under one, whatever mode its maker asked for: a tool a
// role runs that makes a directory 0700 in its TMPDIR, as test suites do, would find 02700
// (OSSV-775, four mode assertions of OSS Verify's that fail there and pass under a 0700
// temporary directory off the volume). Clearing the bit on TMPDIR, the one directory every
// role's tools make their own directories directly under, gives those directories the mode
// asked for. It is cleared on a TMPDIR this call makes and on one it finds, a retained volume's
// from an earlier release; TMPDIR's access bits, its owner and its group stay as they are, and
// so do its parent and the other directories of the set, which keep the volume's group as the
// kubelet set it. A bit that cannot be cleared is a refusal naming the path, as a directory that
// cannot be made is.
func EnsureScratch(environ []string) error {
	env := lookup(environ)
	for _, name := range ScratchVariables {
		dir := env[name]
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("pod safety: make the directory %s names, %s: %w", name, dir, err)
		}
		if name != "TMPDIR" {
			continue
		}
		if err := clearSetgid(dir); err != nil {
			return fmt.Errorf("pod safety: clear the set-group-ID bit of the directory TMPDIR names, %s: %w", dir, err)
		}
	}
	return nil
}

// clearSetgid removes the set-group-ID bit from dir's mode, where it is set, and changes nothing
// else of it: its access bits, its set-user-ID and sticky bits, its owner and its group.
func clearSetgid(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSetgid == 0 {
		return nil
	}
	return os.Chmod(dir, info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSticky))
}

// GhExtensionsDir is where the worker image installs the gh extensions it ships
// (packages/daemon/docker/worker.Dockerfile's ARG GH_STACK_TOOL): one directory per extension,
// named gh-<name> and holding the extension's executable under that same name, as gh lays out an
// extension it installs itself. It is root's, so no role changes the copy its siblings run.
const GhExtensionsDir = "/opt/legion/gh-extensions"

// EnsureGhExtensions links each extension of shipped (GhExtensionsDir in a pod) into gh's own
// extension directory under environ's XDG_DATA_HOME, `$XDG_DATA_HOME/gh/extensions/gh-<name>`.
// gh looks for extensions there and nowhere else: under its data directory, which is
// $XDG_DATA_HOME/gh where that is set (go-gh v2.13.0 pkg/config/config.go, DataDir; cli/cli
// v2.98.0 pkg/cmd/extension/manager.go, list), and a role's data home is the issue's scratch
// (sandbox.roleEnvironment), which starts empty in every pod, so without the link gh never sees
// the image's copy and answers `gh stack` with its offer to install the extension. gh reads a
// symbolic link there as a local extension, and runs `<link>/gh-<name>`.
//
// It runs after EnsureScratch, which makes the data home. An entry already at a link's path —
// a sibling role's link, or an extension an agent installed itself — is left as it is, and a
// sibling that links the same entry first is no refusal. A shipped directory that does not exist
// (an image built before it shipped any), and an XDG_DATA_HOME unset, empty or relative, link
// nothing, as EnsureScratch makes nothing for them. A link that cannot be made is a refusal naming
// the extension and the path.
func EnsureGhExtensions(environ []string, shipped string) error {
	data := lookup(environ)["XDG_DATA_HOME"]
	if data == "" || !filepath.IsAbs(data) {
		return nil
	}
	entries, err := os.ReadDir(shipped)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pod safety: read the image's gh extensions, %s: %w", shipped, err)
	}
	extensions := filepath.Join(data, "gh", "extensions")
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "gh-") {
			continue
		}
		if err := os.MkdirAll(extensions, 0o700); err != nil {
			return fmt.Errorf("pod safety: make gh's extension directory %s: %w", extensions, err)
		}
		link := filepath.Join(extensions, entry.Name())
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(shipped, entry.Name()), link); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("pod safety: link the gh extension %s at %s: %w", entry.Name(), link, err)
		}
	}
	return nil
}

// lookup is environ by name: a name environ defines is present, with its value, "" included.
func lookup(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env
}
