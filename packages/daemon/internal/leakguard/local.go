package leakguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/lookpath"
)

// PushNamesReceivePackVar is the environment variable the pod's git wrapper
// (packages/daemon/docker/git) exports to 1 before it runs the real git for a push whose command
// line can name its own receive-pack program: any argument after the subcommand matching `--<P>` or
// `--<P>=<value>` where `<P>` is a non-empty prefix of `receive-pack` or of `exec`. git hands its
// environment to the pre-push hook, where Guard.writesOnlyToItsURL reads the variable being set —
// with any value, empty included — as a push that can write somewhere other than its URL: one
// Guard.StaysLocal does not clear as local, and one whose commits the guard bounds by the
// remote-tracking refs alone, with no read of what the URL holds (Guard.readsReachItsRepository).
// The wrapper never unsets it, so a caller that sets it by hand changes only that: the push is not
// cleared, and its URL is not read.
const PushNamesReceivePackVar = "LEGION_PUSH_NAMES_RECEIVE_PACK"

// execPathVar is the variable git reads its exec path from, the directory whose git-receive-pack a
// push to a local path runs, and which git sets in the environment of every hook it runs.
const execPathVar = "GIT_EXEC_PATH"

// StaysLocal is whether the push git hands the pre-push hook — to url, from the repository git runs
// the hook in, which dir and the guard's environment name together (a GIT_DIR there names it from
// any directory, as `jj git push` does) — can only write to a path on this machine, so that the
// hook clears it with no check: it copies bytes that are already on this machine's disk, and a
// later push of them to a host goes through the hook again. It answers true only when every one of
// these holds, and false when any read it makes fails, so a push it cannot read is guarded:
//
//   - url begins with `/`, `./`, `../` or `file:///` and holds no `::` (localURL), an allowlist
//     rather than a parse that looks for a host: the scp-like `git@github.com:o/n` holds no `://`,
//     `<transport>::<address>` runs a remote helper, and git 2.47.3 pushed to
//     `file://localhost/<path>` and `file://otherhost/<path>` as local paths, which stay guarded
//     because no push needs them (git-push(1), "GIT URLS"; measured by dispatch://INFRA-3309's
//     architect on 2026-11-13).
//   - The push can write only to url (writesOnlyToItsURL): the guard's environment carries no
//     mark of the git wrapper's, git's exec path is its own, and no configuration names a
//     remote's own receive-pack program or remote helper.
//   - Every remote of the repository has a URL, and each URL `git remote get-url --all` and `git
//     remote get-url --push --all` print for it is one localURL takes. Those print the URL after
//     url.<base>.insteadOf, pushInsteadOf and remote.<name>.pushurl, which is the URL git hands
//     the hook (measured by dispatch://INFRA-3309's architect), so a local URL a rewrite sends to a
//     host reads as that host. This is the shared check's own rule for a checkout, made narrower:
//     its hook sweep leaves with no hook a checkout whose every remote is a local repository, and
//     this keeps guarded a checkout that also has a remote at any host, GitHub's or another.
//     The remotes are the names `git remote` prints and the files under the repository's legacy
//     remotes and branches directories (remoteNames).
//
// Its git reads after ownExecPath's go through Guard.git in dir, with the package's pins and the
// guard's environment. It resolves no shared check and reads no commit.
func (g *Guard) StaysLocal(ctx context.Context, dir, url string) bool {
	if !localURL(url) || !g.writesOnlyToItsURL(ctx, dir, url) {
		return false
	}
	names, err := g.remoteNames(ctx, dir)
	if err != nil {
		return false
	}
	for _, name := range names {
		if !g.remoteStaysLocal(ctx, dir, name) {
			return false
		}
	}
	return true
}

// writesOnlyToItsURL is whether the push git hands the pre-push hook — to url, from the
// repository dir and the guard's environment name — can write only to url by the ways a push to
// a local path can write elsewhere: whether no program of the caller's choosing stands in for the
// git-receive-pack of url's repository, a receive-pack program, a remote helper or another exec
// path's git-receive-pack, each of which can write somewhere else, a host included, while the hook
// gets url. A push to a host can also connect through a program or a proxy between git and the
// server, which can send its git-receive-pack to another repository; none serves a push to a
// local path, the one push Guard.StaysLocal clears, and Guard.readsReachItsRepository adds them
// for the reads of what url holds. Guard.StaysLocal clears no push it answers false for, and
// Guard.pushRecords makes neither read of what url holds (destinationHolds) for one: url then says
// nothing of what the repository the push writes to holds, and a url that holds the push's
// commits (`.`, the clone itself, a local mirror) would have every commit read as held while the
// push wrote them where none is held. The review of cybermonklabs/legion#5914 reproduced that:
// with the reads made for every push,
// `git push --receive-pack="git-receive-pack <another path> #" . main:refs/heads/leak` of a
// commit the rules refuse passed, and the other path took the ref. It answers true only when
// every one of these holds, and false when any read it makes fails:
//
//   - The guard's environment does not set PushNamesReceivePackVar. A push whose command line
//     names its own receive-pack program writes where that program writes: measured by both
//     reviews of dispatch://INFRA-3309's spec, a push to a local path with
//     `--receive-pack="git-receive-pack <another path> #"` wrote to the other path while the hook
//     got the first one, and such a program can be `ssh git@github.com git-receive-pack <o>/<n>`.
//     Only the git wrapper sees the command line, so its mark is what says so here.
//   - url holds no `::`. git pushes to `<transport>::<address>` through the remote helper
//     git-remote-<transport>, which writes where it writes: `ext::<command>` runs any command
//     (gitremote-helpers(7), git-remote-ext(1)).
//   - GIT_EXEC_PATH is unset, or names git's own exec path, the two compared after filepath.Clean
//     (ownExecPath, the first git it runs). A push runs the git-receive-pack of that
//     directory: measured with /usr/bin/git 2.47.3 on 2026-11-27, with GIT_EXEC_PATH=<dir> and
//     with `git --exec-path=<dir> push`, a `<dir>/git-receive-pack` that ran the real one on a
//     second bare repository made `git push <bare A> main:e1` write e1 there, while the hook got
//     bare A's path and saw GIT_EXEC_PATH=<dir>.
//   - No key of `git config -z --list`, which holds every scope down to the command line's
//     (GIT_CONFIG_PARAMETERS, which `git -c` hands the hook, and a GIT_CONFIG_COUNT block), is
//     remote.<name>.receivepack or remote.<name>.vcs (namesItsOwnTransport). The first writes where
//     its program writes, as `--receive-pack` does (measured by both reviews of
//     dispatch://INFRA-3309's spec), and the second makes git push through a remote helper
//     whatever the URL holds.
//
// Its reads go through Guard.git in dir, with the package's pins and the guard's environment:
// ownExecPath's first (receivesAtItsURL), so that no git of a foreign exec path runs for it, and
// then `git config -z --list`, a read of its own. Guard.StaysLocal, which asks it, is a step of
// the hook before Guard.Push and shares no read with it, so a push whose configuration this reads
// and which the hook then does not clear has its configuration read twice: once here, and once by
// Guard.pushRecords, which hands that read to Guard.readsReachItsRepository, where these
// conditions are asked again of it.
func (g *Guard) writesOnlyToItsURL(ctx context.Context, dir, url string) bool {
	if !g.receivesAtItsURL(ctx, dir, url, g.base()) {
		return false
	}
	config, err := g.git(ctx, dir, "config", "-z", "--list")
	return err == nil && !namesItsOwnTransport(config)
}

// receivesAtItsURL is whether, by all writesOnlyToItsURL asks but the configuration
// (namesItsOwnTransport), git's own git-receive-pack of url's repository receives the push under
// base, the guard's environment: base does not set PushNamesReceivePackVar, url holds no `::`,
// and the exec path is git's own (ownExecPath, which runs a git). Guard.writesOnlyToItsURL and
// Guard.readsReachItsRepository each ask it, and each asks namesItsOwnTransport of the
// configuration it holds.
func (g *Guard) receivesAtItsURL(ctx context.Context, dir, url string, base []string) bool {
	if _, marked := lookup(base, PushNamesReceivePackVar); marked || strings.Contains(url, "::") {
		return false
	}
	return g.ownExecPath(ctx, dir, base)
}

// localURL is whether url names a path on this machine by a spelling that can name nothing else: a
// path from the root or from the working directory, or a file URL with no host. Every other URL
// stays guarded, a bare relative path such as `remote.git` among them (R2 of
// dispatch://INFRA-3524's spec).
func localURL(url string) bool {
	if strings.Contains(url, "::") {
		return false
	}
	for _, prefix := range []string{"/", "./", "../", "file:///"} {
		if strings.HasPrefix(url, prefix) {
			return true
		}
	}
	return false
}

// ownExecPath is whether base sets no GIT_EXEC_PATH, or sets git's own: the path `git --exec-path`
// prints in a child that does not inherit GIT_EXEC_PATH, each read after filepath.Clean, so a
// symbolic link to git's own directory is another path. git puts its exec path first on a hook's
// PATH (measured: /usr/lib/git-core for /usr/bin/git 2.47.3), and Guard.command runs a bare `git`
// from the base environment's PATH (program), so a foreign exec path's own git would be the one
// that answered. The git that answers is therefore looked up here, on base's PATH with every entry
// that is GIT_EXEC_PATH taken out, and run by its absolute path under that PATH, so no git of a
// foreign exec path runs for this read. Its standard error is read apart and never as the answer.
func (g *Guard) ownExecPath(ctx context.Context, dir string, base []string) bool {
	named, set := lookup(base, execPathVar)
	if !set {
		return true
	}
	named = filepath.Clean(named)
	searchPath := strings.Join(slices.DeleteFunc(filepath.SplitList(value(base, "PATH")), func(entry string) bool {
		return filepath.Clean(entry) == named
	}), string(os.PathListSeparator))
	git, ok := lookpath.On(searchPath, "git")
	if !ok {
		return false
	}
	var stderr bytes.Buffer
	status, out, err := g.run(ctx, child{
		argv:    []string{git, "--exec-path"},
		dir:     dir,
		env:     []string{"PATH=" + searchPath},
		inherit: withoutExecPath,
		stderr:  &stderr,
	})
	if err != nil || status != 0 {
		return false
	}
	own, ok := strings.CutSuffix(out, "\n")
	return ok && own != "" && !strings.Contains(own, "\n") && filepath.Clean(own) == named
}

// withoutExecPath is the inherit of ownExecPath's child: every entry of the base but
// GIT_EXEC_PATH, so the git it runs prints the exec path it holds of its own.
func withoutExecPath([]string) func(name, value string) bool {
	return func(name, _ string) bool { return name != execPathVar }
}

// remoteNames are the remotes of the repository in dir: the names `git remote` prints, and the
// name of every file in the legacy directories that one `git rev-parse --git-path remotes
// --git-path branches` names, a line each, under the common directory a linked worktree shares,
// where a missing directory holds none. An answer of any other count of lines is an error, which
// keeps the push guarded. `git remote` lists no remote such a file defines, while `git remote
// get-url` prints its URL and a push by its name goes there (measured with git 2.47.3 on
// 2026-11-27: a .git/remotes/gh holding `URL: https://example.invalid/o/n.git` and a
// .git/branches/br each printed their URL, and `git remote` printed neither). Every entry but a
// directory is taken, as legacyFetchDestinations takes them: a name that is no remote fails its
// URL read, which keeps the push guarded.
func (g *Guard) remoteNames(ctx context.Context, dir string) ([]string, error) {
	out, err := g.git(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	// One name a line; a remote's name holds no space (heldRemotes).
	names := strings.Fields(out)
	paths, err := g.git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "remotes", "--git-path", "branches")
	if err != nil {
		return nil, err
	}
	legacy := strings.Split(strings.TrimSuffix(paths, "\n"), "\n")
	if len(legacy) != 2 {
		return nil, fmt.Errorf("git rev-parse printed %q, want the remotes and the branches directories", paths)
	}
	for _, path := range legacy {
		entries, err := os.ReadDir(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// remoteStaysLocal is whether the remote name has a URL and every fetch and push URL git prints
// for it is one localURL takes. A URL can hold a space (a path on this machine), so the URLs are
// read one a line, as heldRemotes reads them, and an empty line is an empty URL, which is none.
func (g *Guard) remoteStaysLocal(ctx context.Context, dir, name string) bool {
	urls := 0
	for _, args := range [][]string{
		{"remote", "get-url", "--all", "--", name},
		{"remote", "get-url", "--push", "--all", "--", name},
	} {
		out, err := g.git(ctx, dir, args...)
		if err != nil {
			return false
		}
		for line := range strings.Lines(out) {
			if !localURL(strings.TrimSuffix(line, "\n")) {
				return false
			}
			urls++
		}
	}
	return urls > 0
}

// namesItsOwnTransport is whether config, the output of `git config -z --list`, holds a key
// remote.<name>.receivepack or remote.<name>.vcs for any name. A name can hold dots, so the key is
// read by its first and its last dot-part, each without case, as git reads a section and a
// variable name.
func namesItsOwnTransport(config string) bool {
	for entry := range strings.SplitSeq(config, "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		section, rest, ok := strings.Cut(key, ".")
		if !ok || !strings.EqualFold(section, "remote") {
			continue
		}
		last := strings.LastIndex(rest, ".")
		if last < 0 {
			continue
		}
		if variable := rest[last+1:]; strings.EqualFold(variable, "receivepack") || strings.EqualFold(variable, "vcs") {
			return true
		}
	}
	return false
}
