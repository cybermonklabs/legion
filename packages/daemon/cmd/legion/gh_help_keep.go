package main

import (
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
	"syscall"

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
// The copies are under the role's own state directory, $XDG_STATE_HOME/legion-gh/help, as the
// guard keeps its visibility answers beside them (leakguard's keepVisibility). Each is a file of
// mode 0600, in a directory of mode 0700, both owned by this process's user. A copy that is not,
// or whose directory is not, is not read: another user that could write it could change what
// this wrapper parses, and so which owner's token a command is run with. With no XDG_STATE_HOME,
// no GH_CONFIG_DIR, or a gh path that is not absolute, nothing is kept and every help is read
// from gh.
type ghHelpKeep struct {
	gh string
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
// removing or renaming one changes that directory's own modification time. Env is a digest of the
// variables that may change how gh prints (ghHelpEnvDigest): a digest, so no value of them is
// written into a copy.
type ghHelpInputs struct {
	Gh         ghFileState `json:"gh"`
	Config     ghFileState `json:"config"`
	Hosts      ghFileState `json:"hosts"`
	Extensions ghFileState `json:"extensions"`
	Env        string      `json:"env"`
}

// ghFileState is what tells one version of a file apart from another without reading it: its
// absolute path, and its device, inode, size, modification time and mode, or that it is missing.
// A gh replaced by another build, in place or by a rename over it, differs in at least one of
// them, and so does a file gh can no longer read.
type ghFileState struct {
	Path    string `json:"path"`
	Missing bool   `json:"missing,omitempty"`
	Dev     uint64 `json:"dev,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`
	Size    int64  `json:"size,omitempty"`
	ModTime int64  `json:"mtime,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
}

// ghHelpEntry is one kept copy: the help gh printed for Path under Inputs.
type ghHelpEntry struct {
	Inputs ghHelpInputs `json:"inputs"`
	Path   []string     `json:"path"`
	Help   string       `json:"help"`
}

// ghPrintingVariables are the variables outside gh's own GH_* that change what gh prints as help:
// with CLICOLOR_FORCE, or GH_FORCE_TTY, gh 2.98.0 prints its help in colour, which CLICOLOR and
// NO_COLOR turn off again; TERM and COLORTERM name the colours a terminal takes.
var ghPrintingVariables = map[string]bool{"CLICOLOR": true, "CLICOLOR_FORCE": true, "NO_COLOR": true, "TERM": true, "COLORTERM": true}

// newGhHelpKeep is the keep of the gh at the path gh for a call run under env.
func newGhHelpKeep(gh string, env map[string]string) *ghHelpKeep {
	keep := &ghHelpKeep{gh: gh, uid: os.Getuid()}
	state, config := env["XDG_STATE_HOME"], env["GH_CONFIG_DIR"]
	if state == "" || config == "" || !filepath.IsAbs(gh) {
		return keep
	}
	data := filepath.Join(env["HOME"], ".local", "share", "gh")
	if xdg := env["XDG_DATA_HOME"]; xdg != "" {
		data = filepath.Join(xdg, "gh")
	}
	var inputs ghHelpInputs
	var err error
	if inputs.Gh, err = ghFileStateOf(gh); err != nil || inputs.Gh.Missing {
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
	inputs.Env = ghHelpEnvDigest(env)
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
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return ghFileState{Path: abs, Missing: true}, nil
	}
	if err != nil {
		return ghFileState{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ghFileState{}, errors.New("no device and inode for " + abs)
	}
	return ghFileState{Path: abs, Dev: uint64(stat.Dev), Ino: uint64(stat.Ino), Size: info.Size(), ModTime: info.ModTime().UnixNano(), Mode: uint32(info.Mode())}, nil
}

// ghHelpEnvDigest is the sha256 of every GH_* variable of env and every one ghPrintingVariables
// names, as NAME=VALUE in sorted order: gh reads its settings from GH_* variables, and a later gh
// may print its help differently under one of them.
func ghHelpEnvDigest(env map[string]string) string {
	var pairs []string
	for name, value := range env {
		if strings.HasPrefix(name, "GH_") || ghPrintingVariables[name] {
			pairs = append(pairs, name+"="+value)
		}
	}
	slices.Sort(pairs)
	sum := sha256.Sum256([]byte(strings.Join(pairs, "\x00")))
	return hex.EncodeToString(sum[:])
}

// read is gh's help of path: the kept copy where one answers, and otherwise gh's own answer
// (readGhHelp), which is kept when gh printed it.
func (k *ghHelpKeep) read(path []string) (string, error) {
	if k.dir == "" {
		return readGhHelp(k.gh, path)
	}
	name := k.fileName(path)
	if help, ok := k.kept(name, path); ok {
		return help, nil
	}
	help, err := readGhHelp(k.gh, path)
	if err == nil {
		k.keep(name, path, help)
	}
	return help, err
}

// fileName is the name of path's copy: the sha256 of the places its inputs are read from, the
// variables' digest and the path. A new gh at the same place writes over the copy the old one
// left, so the directory holds one copy per path a role reads.
func (k *ghHelpKeep) fileName(path []string) string {
	named, err := json.Marshal([]any{k.inputs.Gh.Path, k.inputs.Config.Path, k.inputs.Extensions.Path, k.inputs.Env, append([]string{}, path...)})
	if err != nil {
		// Encoding strings cannot fail; a failure here is a bug.
		panic(err)
	}
	sum := sha256.Sum256(named)
	return hex.EncodeToString(sum[:]) + ".json"
}

// kept is the help the copy name holds for path, and whether it answers: a regular file, not a
// link, in a directory that is not a link, both private to k.uid (private), holding path's help
// under exactly k's inputs.
func (k *ghHelpKeep) kept(name string, path []string) (string, bool) {
	dir, err := os.OpenFile(k.dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer dir.Close()
	if info, err := dir.Stat(); err != nil || !info.IsDir() || !k.private(info) {
		return "", false
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", false
	}
	file := os.NewFile(uintptr(fd), filepath.Join(k.dir, name))
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || !k.private(info) {
		return "", false
	}
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

// keep writes help as path's copy name: into a file of mode 0600 made beside it and renamed over
// it, so a reader finds the old copy or the new one whole. Nothing is written where the directory
// cannot be made private to k.uid. A copy that cannot be written is not an error: the next call
// reads the help from gh, as this one did.
func (k *ghHelpKeep) keep(name string, path []string, help string) {
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return
	}
	if err := os.Chmod(k.dir, 0o700); err != nil {
		return
	}
	if info, err := os.Lstat(k.dir); err != nil || !info.IsDir() || !k.private(info) {
		return
	}
	encoded, err := json.Marshal(ghHelpEntry{Inputs: k.inputs, Path: path, Help: help})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(k.dir, ".help-")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), filepath.Join(k.dir, name))
}

// private says whether info is k.uid's and no one else's: owned by that user, with no permission
// for the file's group or for others.
func (k *ghHelpKeep) private(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == k.uid && info.Mode().Perm()&0o077 == 0
}
