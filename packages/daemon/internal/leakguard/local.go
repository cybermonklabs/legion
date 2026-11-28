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
//     mark of the git wrapper's, git's exec path is its own, and every GIT_ name of that
//     environment and every key of the configuration's section remote is one the guard knows
//     (knowsEveryInput), which leaves out a remote's own receive-pack program and remote helper,
//     and every key of that section with no remote's name or an empty one.
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
// guard's environment, but for the read of the configuration (Guard.pushConfiguration), which takes
// GIT_CONFIG out of that environment. It resolves no shared check and reads no commit.
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
//   - Every name of the guard's environment that begins GIT_ is one knownGitNames holds, and
//     every key of the section remote of the push's configuration, as Guard.pushConfiguration
//     reads it, is remote.<name>.<variable> with a name that is not empty and a variable
//     knownRemoteKeys holds (knowsEveryInput, R1 (e) of dispatch://INFRA-3524's spec). That
//     configuration holds every scope down to the command line's (GIT_CONFIG_PARAMETERS, which
//     `git -c` hands the hook, and a GIT_CONFIG_COUNT block). knownRemoteKeys holds neither
//     receivepack nor vcs: remote.<name>.receivepack writes where its program writes, as
//     `--receive-pack` does (measured by both reviews of dispatch://INFRA-3309's spec), and
//     remote.<name>.vcs makes git push through a remote helper whatever the URL holds. Each review
//     round of cybermonklabs/legion#5914 found one more input of that kind, so the guard takes
//     only the inputs it knows, and one it does not know, or one a later git adds, can only keep
//     the push guarded.
//
// Its first git is ownExecPath's (receivesAtItsURL), which does not go through Guard.git: it runs
// the git it looks up itself, by its absolute path, under base's PATH with GIT_EXEC_PATH's entries
// taken out and with no GIT_EXEC_PATH (withoutExecPath), so that no git of a foreign exec path runs
// for it. Its one read after that is the push's configuration (Guard.pushConfiguration), a read of
// its own. Guard.StaysLocal, which asks it, is a step of the hook before Guard.Push and shares no
// read with it, so a push whose configuration this reads and which the hook then does not clear
// has its configuration read twice: once here, and once by Guard.pushRecords, which hands that
// read to Guard.readsReachItsRepository, where these conditions are asked again of it.
func (g *Guard) writesOnlyToItsURL(ctx context.Context, dir, url string) bool {
	base := g.base()
	if !g.receivesAtItsURL(ctx, dir, url, base) {
		return false
	}
	config, err := g.pushConfiguration(ctx, dir)
	if err != nil {
		return false
	}
	// R1 (e): every input that can change where git writes is one the guard knows.
	if !knowsEveryInput(base, config) {
		return false
	}
	return true
}

// receivesAtItsURL is whether, by all writesOnlyToItsURL asks but the inputs it reads in the
// environment and the configuration (knowsEveryInput), git's own git-receive-pack of url's
// repository receives the push under base, the guard's environment: base does not set
// PushNamesReceivePackVar, url holds no `::`, and the exec path is git's own (ownExecPath, which
// runs a git). Guard.writesOnlyToItsURL and Guard.readsReachItsRepository each ask it, and each
// asks knowsEveryInput of base and of the configuration it holds.
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

// knowsEveryInput is R1 (e) of dispatch://INFRA-3524's spec: whether every name of env, the
// guard's environment, that begins GIT_ is one knownGitNames holds (knownGitName), and every key
// of config, the push's configuration as `git config -z --list` writes it
// (Guard.pushConfiguration), whose section is remote is one it can place as
// remote.<name>.<variable>: a key with a subsection, the remote's name, that is not empty, and a
// variable knownRemoteKeys holds. A remote's name can hold dots, so the key is read by its first
// and its last dot-part (keyParts), the section without case and the variable as git writes it,
// in lower case. Every other key of the section remote is an input the guard does not know, and
// answers false: a key with no subsection, such as remote.pushDefault or an invented
// remote.inventedtop, and a key whose subsection is empty, which git does not skip but applies to
// the remote named by what follows `remote.`, a dot and the variable (config.c's
// parse_config_key gives the name a length of 0, which remote.c's make_remote reads to the end of
// the key). The review of cybermonklabs/legion#5914 measured that with git 2.47.3: with
// remote..receivepack.url at a local path and remote..receivepack a receive-pack program that
// wrote to another repository, `git push .receivepack` wrote there, and a guard that skipped the
// key cleared the push. Guard.StaysLocal clears no push it answers false for
// (writesOnlyToItsURL), and Guard.readsReachItsRepository makes neither read of what the
// destination holds for one.
func knowsEveryInput(env []string, config string) bool {
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") && !knownGitName(name) {
			return false
		}
	}
	for entry := range strings.SplitSeq(config, "\x00") {
		name, _, _ := strings.Cut(entry, "\n")
		key, subsectioned, ok := keyParts(name)
		if !ok || !strings.EqualFold(key.section, "remote") {
			continue
		}
		if !subsectioned || key.subsection == "" || !slices.Contains(knownRemoteKeys, key.variable) {
			return false
		}
	}
	return true
}

// knownGitName is whether name is one an entry of knownGitNames stands for: the entry itself; for
// an entry ending in `*`, any name that begins with what comes before it; and for one ending in
// `<n>`, any name that is what comes before it and one or more decimal digits.
func knownGitName(name string) bool {
	for _, known := range knownGitNames {
		if prefix, ok := strings.CutSuffix(known, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if prefix, ok := strings.CutSuffix(known, "<n>"); ok {
			if digits, ok := strings.CutPrefix(name, prefix); ok && digits != "" && strings.Trim(digits, "0123456789") == "" {
				return true
			}
		} else if name == known {
			return true
		}
	}
	return false
}

// knownGitNames are the names beginning GIT_ that the guard's environment may hold for a push
// Guard.StaysLocal clears or whose destination Guard.readsReachItsRepository reads (R1 (e) of
// dispatch://INFRA-3524's spec, knowsEveryInput). An entry ending in `*` stands for every name
// that begins with what comes before it, and one ending in `<n>` for every name that is what
// comes before it and one or more decimal digits (knownGitName). Each is a name that an issue
// pod's push, jj's push, a script of this repository or a fixture of its tests hands the hook, and
// none changes where a push the guard clears or reads writes in a way the guard does not read as
// the push does: the guard's gits inherit each one, and its read of the configuration reads every
// scope the push reads (Guard.pushConfiguration). Measured by dispatch://INFRA-3524's implementer
// and tester on 2026-11-28 in an issue pod, with git 2.47.3 and jj 0.46.0, through the pod's git
// and jj wrappers and a pre-push hook that recorded the GIT_ names of its environment: the pushes
// of check c2, a `git push` and a `jj git push` among them, and the rows of R4, each in the role's
// environment and with no shared check; and the rows of checks c1 and c10, through the guard they
// call.
//
// Absent on purpose, among the names git reads: GIT_CONFIG, which of the gits a push and the guard
// run only `git config` reads, as the one file it reads in place of every scope, so that a read of
// the configuration under it would not see what the push runs by (Guard.pushConfiguration);
// GIT_NAMESPACE, which moves every ref a push writes under refs/namespaces/<name>/;
// GIT_OBJECT_DIRECTORY and GIT_ALTERNATE_OBJECT_DIRECTORIES, which name where git reads and writes
// objects; and GIT_PROXY_COMMAND, the proxy command of a git:// URL, a program between git and
// the server that sees whether it serves a read (git-upload-pack) or the push (git-receive-pack)
// and can send the two to different repositories, as core.gitProxy can (routedKeys).
var knownGitNames = []string{
	// The repository the hook runs in, which git sets for `git --git-dir` and `git --work-tree`:
	// jj's push through the pod's wrappers handed the hook GIT_DIR (c2, measured), as c1's and
	// c10's rows of a push from another directory do. The guard's gits inherit both, so they read
	// the repository the push is made from.
	"GIT_DIR", "GIT_WORK_TREE",
	// git's exec path, which git sets in every hook's environment (each push of c2, measured) and
	// R1 (d) reads (ownExecPath).
	"GIT_EXEC_PATH",
	// The configuration of the command line, which `git -c` hands the hook (the pod's git
	// wrapper's `-c core.hooksPath`, each push of c2 through it, measured), and a GIT_CONFIG_COUNT
	// block (an issue pod's role environment holds GIT_CONFIG_KEY_0 to GIT_CONFIG_KEY_8 and their
	// values, measured). The read of the configuration holds both.
	"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_<n>", "GIT_CONFIG_VALUE_<n>",
	// The configuration files git reads, which the read of the configuration reads as the push
	// does: fixtureenv.Isolated sets GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM for every fixture's
	// git, and c10's rows hand both to the guard; c10's eleventh case sets GIT_CONFIG_SYSTEM to put
	// a url.<base>.insteadOf in the system file.
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_SYSTEM",
	// The directory below the repository's top that the command was run from, which git set in
	// the hook's environment of each push of c2 (measured).
	"GIT_PREFIX",
	// A commit's identity: an issue pod's role environment sets GIT_AUTHOR_NAME,
	// GIT_AUTHOR_EMAIL, GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL (measured), and so does
	// fixtureenv.Isolated.
	"GIT_AUTHOR_*", "GIT_COMMITTER_*",
	// The programs git runs for a person at a terminal, and whether it may prompt one: an issue
	// pod's role environment sets all four (measured), and fixtureenv.Isolated sets
	// GIT_TERMINAL_PROMPT.
	"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER", "GIT_TERMINAL_PROMPT",
	// git's traces, which write what git does and change none of it: c9's push under GIT_TRACE=1
	// is judged as without it (TestAPushUnderGitTraceIsJudgedAsWithoutIt).
	"GIT_TRACE*",
	// The transports git may use, which can only refuse one: fixtureenv.Guard sets it to file in
	// every test process of packages/daemon, and c1's and c10's rows hand it to the guard.
	"GIT_ALLOW_PROTOCOL",
	// The ssh program git runs for a URL over ssh, which .github/scripts/release-push.sh exports
	// for every deploy push (GIT_SSH_COMMAND): the hook of each push of release-push.test.sh, whose
	// GIT_CONFIG_COUNT block rewrites the deploy remote git@github.com:example/repo.git to a local
	// bare repository, held it in both environments of R4 (measured by dispatch://INFRA-3524's
	// tester, dispatch://INFRA-3524/comment/1ed5511a-e269-4411-9612-4a5c7ba1874d). Neither names
	// a program for a push the guard clears or reads: R1 clears only a push to a local path, for
	// which git runs no ssh, and the reads are made only over a transport git serves itself
	// (gitServesItsTransport), where neither acts, while a URL over ssh keeps the base's bound.
	"GIT_SSH_COMMAND", "GIT_SSH",
	// Whether git checks an https server's certificate: c10's tenth case sets it for its
	// https_proxy row, whose proxy answers with a certificate of the test's own
	// (TestAPushThatNamesAProgramOrProxyBetweenGitAndTheServerKeepsTheBasesBound, measured through
	// the guard). It names no destination, and the reads, which inherit it, check the server's
	// certificate, or leave it unchecked, as the push does.
	"GIT_SSL_NO_VERIFY",
}

// knownRemoteKeys are the variables that a key remote.<name>.<variable> of the push's
// configuration may have for a push Guard.StaysLocal clears or whose destination
// Guard.readsReachItsRepository reads (R1 (e) of dispatch://INFRA-3524's spec, knowsEveryInput),
// each as `git config -z --list` writes a variable, in lower case. Each is one that git, jj, gh or
// a fixture of this repository's tests writes, and none names a program, or a place a push writes
// to but the URLs that R1 (a) reads (remoteStaysLocal) and the refs the hook's update lines name,
// or makes a read of a URL go elsewhere.
//
// Absent on purpose: receivepack, a program of the remote's own that a push runs in place of the
// URL's git-receive-pack and that writes where it writes, as `--receive-pack` does, and vcs,
// which makes git push through a remote helper whatever the URL holds (R1 (b) of the spec); and
// proxy and proxyauthmethod, which a push by the remote's name applies and a read of a URL does
// not (R10 of the spec): such a proxy sees whether it serves a read (git-upload-pack) or the push
// (git-receive-pack) and can send the two to different repositories, as the remote.origin.proxy
// row of c10's tenth case shows
// (TestAPushThatNamesAProgramOrProxyBetweenGitAndTheServerKeepsTheBasesBound). uploadpack, a
// program that a read by the remote's name runs in place of the URL's git-upload-pack, stays out
// too: c10's row of a remote named the push's URL with an uploadpack sets it, and gets no read
// (urlNamesARemote).
var knownRemoteKeys = []string{
	// What `git remote add`, `git clone` and `jj git remote add` write: each repository of c2's
	// pushes held both (measured), and an issue pod's clone of this repository holds both.
	"url", "fetch",
	// Where and what a push by the remote's name pushes: pushurl, whose URLs R1 (a) reads with the
	// fetch URLs (remoteStaysLocal) and which c1's row of a pushurl at a host sets, and push,
	// refspecs whose refs the hook's update lines name. Both are R1 (e)'s own minimum.
	"pushurl", "push",
	// Which tags a fetch takes, which `git remote add --tags` and `--no-tags` write: R1 (e)'s own
	// minimum.
	"tagopt",
	// The repository gh resolved the remote to, which gh writes when it resolves one: an issue
	// pod's clone of this repository held remote.origin.gh-resolved on 2026-11-28, measured by
	// dispatch://INFRA-3118's architect and by dispatch://INFRA-3524's.
	"gh-resolved",
	// A partial clone's remote: the one git fetches an object the clone lacks from (promisor), and
	// the filter its fetches ask for (partialclonefilter). `git clone --filter=blob:none` wrote
	// both (measured with git 2.47.3 on 2026-11-28), and so do the fixtures of c10's
	// TestTheReadOfWhatTheDestinationHoldsFetchesNothing, whose reads of what the destination holds
	// in a blobless clone are what that row holds, and of
	// TestAPushToAURLReadsOnlyWhatTheRemoteWithThatURLDoesNotHold (measured through the guard).
	// Neither names where a push writes, and the reads of what the destination holds fetch nothing
	// from a promisor, since they run under GIT_NO_LAZY_FETCH=1 (fetchesNothing).
	"promisor", "partialclonefilter",
}
