package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// ghHelpKeep answers gh's help of a command path (readGhHelp) from a copy kept the last time gh
// printed it, so a `legion gh` call starts gh for its help only where no kept copy answers.
// ghTargetOf reads two or more helps for a command (gh's own, a group's, the leaf's), and each
// read is a gh process of its own, while gh prints the same help for a path until its binary,
// its configuration, its extensions or the variables it prints under change.
//
// A copy answers only a read whose inputs are the ones it was printed under (ghHelpInputs): the
// gh binary, the two files of the gh directory gh reads before it prints anything, the directory
// it lists its extensions from and the variables that change how it prints. A change to any of
// them reads the help again, so ghTargetOf decides from the text gh would print now. A help gh
// refused is never kept: its refusal is read again on the next call, as it was before there were
// copies.
//
// The copies are under the role's own state directory, $XDG_STATE_HOME/legion-gh/help, one for
// each command path and each set of the variables ghHelpEnvDigest reads. They are written as files
// of mode 0600 in a directory of mode 0700 (ghStateDir, ghReplaceFile), and a copy is read only
// where neither it nor its directory is a link and the user this process runs as owns both, with
// no permission for their group or for others (ghOpenPrivate): another user that could write a
// copy could change what this wrapper parses, and so which owner's token a command is run with.
// In a pod every role runs as one user, so this keeps out another user and not another role. With
// no XDG_STATE_HOME, no GH_CONFIG_DIR, or a gh path that is not absolute or whose links end at a
// file not named gh, nothing is kept and every help is read from gh.
type ghHelpKeep struct {
	// fresh is gh's own answer for a path: readGhHelp of the gh the keep is for, under the context
	// the keep was made with.
	fresh func(path []string) (string, error)
	// dir is where the copies are, "" where nothing is kept.
	dir string
	// uid is the user a copy and its directory must belong to: this process's.
	uid    int
	inputs ghHelpInputs
}

// ghHelpInputs is what decides gh's help beside the command path, read once per call
// (newGhHelpKeep). gh 2.98.0 lists the aliases of GH_CONFIG_DIR's config.yml in its own help, and
// refuses to print any help where config.yml or hosts.yml cannot be read or parsed. It lists the
// extensions it finds in $XDG_DATA_HOME/gh/extensions ($HOME/.local/share/gh/extensions where
// XDG_DATA_HOME is unset); an extension's name is the name of its entry there, so adding,
// removing or renaming one changes that directory. Env is a digest of the variables that may
// change how gh prints (ghHelpEnvDigest): a digest, so no value of them is written into a copy.
type ghHelpInputs struct {
	Gh         ghFileState `json:"gh"`
	Config     ghFileState `json:"config"`
	Hosts      ghFileState `json:"hosts"`
	Extensions ghFileState `json:"extensions"`
	Env        string      `json:"env"`
}

// ghFileState is what tells one version of a file apart from another without reading it: its
// absolute path, and its device, inode, size, modification time, change time and mode, or that
// it is missing. A gh replaced by another build, in place or by a rename over it, differs in at
// least one of them, and so does a file gh can no longer read. A rewrite in place that keeps the
// size and sets the modification time back still moves the change time, which no user can set.
type ghFileState struct {
	Path       string `json:"path"`
	Missing    bool   `json:"missing,omitempty"`
	Dev        uint64 `json:"dev,omitempty"`
	Ino        uint64 `json:"ino,omitempty"`
	Size       int64  `json:"size,omitempty"`
	ModTime    int64  `json:"mtime,omitempty"`
	ChangeTime int64  `json:"ctime,omitempty"`
	Mode       uint32 `json:"mode,omitempty"`
}

// ghHelpEntry is one kept copy: the help gh printed for Path under Inputs.
type ghHelpEntry struct {
	Inputs ghHelpInputs `json:"inputs"`
	Path   []string     `json:"path"`
	Help   string       `json:"help"`
}

// ghPrintingVariables are the variables outside gh's own GH_* that change what gh prints as help:
// with CLICOLOR_FORCE gh 2.98.0 prints its help in colour, which CLICOLOR and NO_COLOR turn off
// again; TERM and COLORTERM name the colours a terminal takes.
var ghPrintingVariables = map[string]bool{"CLICOLOR": true, "CLICOLOR_FORCE": true, "NO_COLOR": true, "TERM": true, "COLORTERM": true}

// newGhHelpKeep is the keep of the gh at the path gh for a call run under env, whose reads of gh
// run under ctx: runGh's signal context (beforeGh), so a SIGINT, SIGTERM or SIGHUP stops a read in
// progress and its process group (readGhHelp). A read a signal or its bound stopped is an error,
// even where gh printed part of its help or exited 0 on the stop (exec answers a command whose
// Cancel ran with the context's error), so it is never kept, and no later call is answered with it.
//
// The gh binary is the file gh's links end at, and only a file named gh is taken for one. A
// compiled multiplexer, such as mise's shim, is a link to the multiplexer itself, which picks the
// gh it runs on each run (from the working directory's mise.toml), so its own file says nothing
// of the gh that prints the help, and nothing is kept for it. A multiplexer copied or hard-linked
// under the name gh is not told apart.
func newGhHelpKeep(ctx context.Context, gh string, env map[string]string, helpEnv []string) *ghHelpKeep {
	keep := &ghHelpKeep{fresh: func(path []string) (string, error) { return readGhHelp(ctx, gh, helpEnv, path) }, uid: os.Getuid()}
	state, config := env["XDG_STATE_HOME"], env["GH_CONFIG_DIR"]
	if state == "" || config == "" || !filepath.IsAbs(gh) {
		return keep
	}
	binary, err := filepath.EvalSymlinks(gh)
	if err != nil || filepath.Base(binary) != "gh" {
		return keep
	}
	data := filepath.Join(env["HOME"], ".local", "share", "gh")
	if xdg := env["XDG_DATA_HOME"]; xdg != "" {
		data = filepath.Join(xdg, "gh")
	}
	var inputs ghHelpInputs
	if inputs.Gh, err = ghFileStateOf(binary); err != nil || inputs.Gh.Missing {
		return keep
	}
	if inputs.Config, err = ghFileStateOf(filepath.Join(config, "config.yml")); err != nil {
		return keep
	}
	if inputs.Hosts, err = ghFileStateOf(filepath.Join(config, "hosts.yml")); err != nil {
		return keep
	}
	if inputs.Extensions, err = ghFileStateOf(filepath.Join(data, "extensions")); err != nil {
		return keep
	}
	inputs.Env = ghHelpEnvDigest(helpEnv)
	keep.dir, keep.inputs = filepath.Join(state, "legion-gh", "help"), inputs
	return keep
}

// ghFileStateOf is the state of the file at path, which gh reads following links, so this does
// too. A missing file is a state of its own; any other failure to read one is an error, and
// nothing is kept for that call.
func ghFileStateOf(path string) (ghFileState, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ghFileState{}, err
	}
	var st unix.Stat_t
	err = unix.Stat(abs, &st)
	if errors.Is(err, fs.ErrNotExist) {
		return ghFileState{Path: abs, Missing: true}, nil
	}
	if err != nil {
		return ghFileState{}, err
	}
	return ghFileState{
		Path: abs, Dev: uint64(st.Dev), Ino: st.Ino, Size: st.Size,
		ModTime: st.Mtim.Nano(), ChangeTime: st.Ctim.Nano(), Mode: uint32(st.Mode),
	}, nil
}

// ghHelpEnvDigest is the sha256 of every GH_* variable of helpEnv, the environment gh reads its
// help under (runGh's ghChildEnviron), and every one ghPrintingVariables names, as NAME=VALUE in
// sorted order: gh reads its settings from GH_* variables, and a later gh may print its help
// differently under one of them. A variable of the caller's that the allowlist keeps from gh
// (GH_PAGER, GH_FORCE_TTY, GH_DEBUG) changes no help, so it keys no copy of its own.
func ghHelpEnvDigest(helpEnv []string) string {
	values := map[string]string{}
	for _, pair := range helpEnv {
		if name, value, ok := strings.Cut(pair, "="); ok {
			values[name] = value
		}
	}
	var pairs []string
	for name, value := range values {
		if strings.HasPrefix(name, "GH_") || ghPrintingVariables[name] {
			pairs = append(pairs, name+"="+value)
		}
	}
	slices.Sort(pairs)
	sum := sha256.Sum256([]byte(strings.Join(pairs, "\x00")))
	return hex.EncodeToString(sum[:])
}

// read is gh's help of path: the kept copy where one answers, and otherwise gh's own answer
// (fresh), which is kept when gh printed it. A copy runs no gh, so it needs no
// leakguard.RunByRealGhVar; every read that runs gh is readGhHelp's, which carries it.
func (k *ghHelpKeep) read(path []string) (string, error) {
	if k.dir == "" {
		return k.fresh(path)
	}
	name := k.fileName(path)
	if help, ok := k.kept(name, path); ok {
		return help, nil
	}
	help, err := k.fresh(path)
	if err == nil {
		k.keep(name, path, help)
	}
	return help, err
}

// fileName is the name of path's copy: the sha256 of the places its inputs are read from, the
// variables' digest and the path. A new gh at the same place writes over the copy the old one
// left. Each set of the variables the digest reads (another GH_REPO, another NO_COLOR) has copies
// of its own, which nothing removes.
func (k *ghHelpKeep) fileName(path []string) string {
	named, err := json.Marshal([]any{k.inputs.Gh.Path, k.inputs.Config.Path, k.inputs.Extensions.Path, k.inputs.Env, append([]string{}, path...)})
	if err != nil {
		// Encoding strings cannot fail; a failure here is a bug.
		panic(err)
	}
	sum := sha256.Sum256(named)
	return hex.EncodeToString(sum[:]) + ".json"
}

// kept is the help the copy name holds for path, and whether it answers: a file ghOpenPrivate
// opens for k.uid, holding path's help under exactly k's inputs.
func (k *ghHelpKeep) kept(name string, path []string) (string, bool) {
	file, ok := ghOpenPrivate(k.dir, name, k.uid)
	if !ok {
		return "", false
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return "", false
	}
	var entry ghHelpEntry
	if json.Unmarshal(content, &entry) != nil || entry.Inputs != k.inputs || !slices.Equal(entry.Path, path) || entry.Help == "" {
		return "", false
	}
	return entry.Help, true
}

// keep writes help as path's copy name (ghReplaceFile), in a directory ghStateDir makes for k.uid.
// A copy that cannot be written is not an error: the next call reads the help from gh, as this one
// did.
func (k *ghHelpKeep) keep(name string, path []string, help string) {
	if !ghStateDir(k.dir, k.uid) {
		return
	}
	encoded, err := json.Marshal(ghHelpEntry{Inputs: k.inputs, Path: path, Help: help})
	if err != nil {
		return
	}
	ghReplaceFile(filepath.Join(k.dir, name), ".help-", encoded)
}
