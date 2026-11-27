package leakguard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/programbound"
)

// heldReadBound is how long each read of what a push's destination holds runs at most
// (Guard.advertised, Guard.negotiated), and a read past it counts as failed (destinationHolds).
// The hook's own context has no deadline (cmd/legion's main), so without it a destination that
// stops answering would hold the push for as long as it did not answer. Two minutes is each read
// many times over: measured from an issue pod on 2026-11-18, seven reads of cybermonklabs/legion's
// 5,749 branches and tags took 0.58 s to 0.67 s, 0.63 s the median, and on 2026-11-27 its 7,180
// lines took 0.7 s and the negotiation 0.4 s. It is a variable so that a test can shorten it, and
// so that cmd/legion's tests, whose pushes run these reads on a stand-in git, can give it their
// own deadline (programbound).
var heldReadBound = 2 * time.Minute

func init() {
	programbound.Register(&heldReadBound)
}

// fetchesNothing is the environment of a read of what a repository has that must fetch nothing
// to answer: GIT_NO_LAZY_FETCH=1 (git 2.44 and later), under which an object a partial clone
// lacks is answered missing rather than fetched from its promisor remote. The reads of what the
// destination holds run so (Guard.advertised, Guard.negotiated, Guard.holds); the reads of a
// commit's files do not, since a commit read in a partial clone needs the blobs it lacks fetched.
var fetchesNothing = []string{"GIT_NO_LAZY_FETCH=1"}

// readsNoConfigFile is the environment of a read that must read neither the machine's git
// configuration nor the user's (Guard.advertised, Guard.negotiated, Guard.lsRemote):
// GIT_CONFIG_NOSYSTEM=1 and GIT_CONFIG_GLOBAL=/dev/null take away the system file and the user's,
// the second also the one under XDG_CONFIG_HOME, and with them every file either includes.
var readsNoConfigFile = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}

// destinationHolds is what a push's destination says it holds, read once a push
// (Guard.pushRecords) by its two reads of the URL git handed the hook: not, a line ^<id> for each
// commit of each read that answered, which Guard.commits feeds rev-list, and negotiated, whether
// the fetch negotiation answered. tips are the local commits of the push's updates, which the
// negotiation walks (Guard.negotiated); the advertised refs are read whole (Guard.advertised).
// The two reads run at the same time, the negotiation in a goroutine of its own, so a destination
// that answers neither holds the push for one heldReadBound, not two.
//
// Where the negotiation answers, the remote-tracking refs bound nothing (Guard.commits): they can
// be stale, since the clone may not have fetched the destination since it moved; no remote may
// name the push's URL; and they can name commits the destination does not hold, which
// heldRemotes keeps, among them refs filed before a `git remote set-url` to another repository
// and refs a push to a local path filed. A union with them would let such a ref leave a commit
// the destination lacks unread, so the negotiation's commits and the advertised ones replace
// them, and the push reads every commit the destination does not hold and none that it holds,
// also where the destination moved to a commit the clone does not have. Where the negotiation
// fails and the advertised read answers (a destination served on protocol v0 or v1, which the
// negotiation needs v2 of, or a negotiation past its bound), the advertised commits are added to
// the remote-tracking refs: each is a commit the destination holds, so the push reads no commit
// it would read without them. Where both fail, not is empty and the remote-tracking refs bound
// the push alone, as they did before these reads.
//
// A read that fails refuses nothing. A stop by ctx itself, the caller's deadline or its cancel,
// is the answer, as it is of every git a push's read runs (Guard.git): the push is then refused
// as a setup failure naming the stop, never bounded as though the read had failed. Where the
// advertised read failed with ctx already ended, the stop it names is that read's, and otherwise
// the negotiation's.
func (g *Guard) destinationHolds(ctx context.Context, dir, url string, tips []string) (not string, negotiated bool, err error) {
	var common []string
	var negotiatedErr error
	done := make(chan struct{})
	go func() {
		common, negotiatedErr = g.negotiated(ctx, dir, url, tips)
		close(done)
	}()
	advertised, advertisedErr := g.advertised(ctx, dir, url)
	stopped := advertisedErr != nil && ctx.Err() != nil
	<-done
	if stopped {
		return "", false, advertisedErr
	}
	if negotiatedErr != nil && ctx.Err() != nil {
		return "", false, negotiatedErr
	}
	var lines strings.Builder
	for _, ids := range [][]string{common, advertised} {
		for _, id := range ids {
			lines.WriteString("^")
			lines.WriteString(id)
			lines.WriteString("\n")
		}
	}
	return lines.String(), negotiatedErr == nil, nil
}

// advertised are the commits the repository at url holds at its branches and tags, of those the
// repository at dir has: commits a push to url need not publish again (destinationHolds). The
// read is of url itself, never of a ref of dir's own, which says nothing reliable of what url
// holds (destinationHolds says why):
//
//	git -c credential.helper= ls-remote --heads --tags -- <url>
//
// run outside dir, in /, a directory only root can write, so that git finds no repository there
// and no configuration of dir's applies to the read. git hands the hook the URL dir's
// configuration has already led the push to, and a rewrite read a second time can lead the read
// to another repository: under url.<base>.pushInsteadOf, which git applies to the push URL alone,
// the URL the hook is handed can be one a url.<base>.insteadOf of dir's rewrites, and a read made
// in dir would list the refs of the repository that rewrite names, not of the one the push writes
// to. The review of cybermonklabs/legion#3946 reproduced that on git 2.47.3: with origin at
// https://example.invalid/fork, url.<real.git>.pushInsteadOf that URL and
// url.<fake.git>.insteadOf <real.git>, the push landed in real.git, the hook was handed real.git,
// and a git ls-remote of it in the clone listed fake.git's refs. So a URL that is a relative local
// path is read against dir (againstDir), as git read it for the push, and the read reads no git
// configuration but its own command line's (heldRead): of the caller's environment it keeps what
// the guard's read with no credential keeps (lsRemoteInherits), of git's own names only
// GIT_ALLOW_PROTOCOL, GIT_SSL_CAINFO and GIT_SSL_CAPATH, so no GIT_DIR, which git hands a pre-push
// hook for a push made with `git --git-dir <dir>`, as `jj git push` makes it, and under which git
// reads dir's configuration from any directory, and no GIT_CONFIG_COUNT block and no
// GIT_CONFIG_PARAMETERS, whose url.<base>.insteadOf would lead the read elsewhere as dir's would;
// and it reads neither the system file nor the user's (readsNoConfigFile). A git wrapper of a
// host's own can still add to its command line, the case `legion git --after` runs.
//
// The read carries no credential (heldRead). The `--` keeps a URL that starts with a dash from
// being read as an option. Each sha it prints is kept where dir has it as a commit, an annotated
// tag's peeled commit (<tag>^{}) too; one cat-file --batch-check of them all, run in dir, answers
// which. A line whose first field is no object id as git prints one, 40 or 64 lower-case hex
// digits (isObjectID), holds nothing and is not asked of: cat-file reads a name or an expression,
// HEAD or main~1, as a revision of dir's, and would take the commit it names in dir as one url
// holds. A sha dir does not have holds nothing, since rev-list cannot walk from it: where it is a
// commit ahead of dir's, the commits of dir's beneath it are left to the negotiation
// (Guard.negotiated), where that fails to the remote-tracking refs, and past those they are read,
// which is the fail-closed direction. The cat-file fetches nothing (fetchesNothing), so that in a
// partial clone, as an issue pod's fork clones are (--filter=blob:none), a sha dir lacks is
// answered missing rather than fetched from the promisor remote. An empty answer, a repository
// with no branch and no tag, holds nothing. A git that cannot be run, that exits other than 0, or
// that its bound stops is an error, as Guard.git's is, and destinationHolds reads it as a read
// that failed.
func (g *Guard) advertised(ctx context.Context, dir, url string) ([]string, error) {
	bounded, cancel := context.WithTimeout(ctx, programbound.Of(&heldReadBound))
	defer cancel()
	home, err := os.MkdirTemp("", "legion-held-read-")
	if err != nil {
		return nil, fmt.Errorf("the empty home for git ls-remote could not be made: %w", err)
	}
	defer os.RemoveAll(home)
	out, err := g.heldRead(bounded, "/", home, nil, "ls-remote", "--heads", "--tags", "--", againstDir(dir, url))
	if err != nil {
		return nil, err
	}
	// "<sha>\t<ref>" a line; a tag and its peeled commit are two lines, and two refs at one commit
	// ask for it once.
	var shas strings.Builder
	asked := map[string]bool{}
	for line := range strings.Lines(out) {
		sha, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		if !isObjectID(sha) || asked[sha] {
			continue
		}
		asked[sha] = true
		shas.WriteString(sha)
		shas.WriteString("\n")
	}
	if len(asked) == 0 {
		return nil, nil
	}
	types, err := g.gitFed(bounded, dir, fetchesNothing, strings.NewReader(shas.String()), "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	var commits []string
	for line := range strings.Lines(types) {
		// "<sha> <type> <size>", and "<sha> missing" for an object dir does not have.
		if fields := strings.Fields(line); len(fields) == 3 && fields[1] == "commit" {
			commits = append(commits, fields[0])
		}
	}
	return commits, nil
}

// negotiated are the commits in the history of tips, the local commits of a push's updates, that
// the repository at url says it holds: what a fetch negotiation with it finds in common, which is
// git's own way to ask a repository which of a client's commits it holds and how git fetch picks
// what to send (destinationHolds). Where url's branch moved to a commit dir does not have, its
// advertised refs name nothing dir has (Guard.advertised), and the negotiation still finds the
// commits beneath them that dir has:
//
//	git -c credential.helper= --shallow-file <dir's shallow file> fetch --negotiate-only \
//	    --negotiation-tip=<tip>... -- <url>
//
// run in an empty bare repository that the guard makes for the read in a private temporary
// directory (os.MkdirTemp, 0700) and removes after, whose objects are dir's through
// GIT_ALTERNATE_OBJECT_DIRECTORIES (`git rev-parse --path-format=absolute --git-path objects` in
// dir), so that git walks dir's commits and no configuration of dir's applies to the read: run in
// dir, a url.<base>.insteadOf of dir's led it to another repository, and run so it answered for
// the URL it was given (measured on 2026-11-27 with git 2.47.3). The URL is joined to dir as
// Guard.advertised joins it (againstDir), and the read keeps of the caller's environment, reads
// of git configuration and carries of a credential what that read does (heldRead), and fetches
// nothing (fetchesNothing). git fetches nothing and writes no ref for it, in dir or in the bare:
// it prints each commit the destination acknowledged as one it holds, one a line, and stops. A
// line that is no object id (isObjectID) holds nothing.
//
// git is named dir's shallow file (`--git-path shallow`) whether or not dir is shallow. Where dir
// is shallow, its walk stops at dir's shallow commits as a walk in dir does: without it, git
// 2.47.3 printed `error: Could not read <parent>` for a shallow commit's parent the alternate
// does not hold, and still answered. Where dir is not, the file does not exist, and git reads a
// missing shallow file as a repository that is not shallow and creates none (measured with git
// 2.47.3 on 2026-11-27). The bare repository's object format is dir's (`--show-object-format`),
// so that the two read one another's ids.
//
// The negotiation needs protocol v2: a destination served on v0 or v1 fails it (git 2.47.3 printed
// `warning: --negotiate-only requires protocol v2` and `fatal: the remote end hung up
// unexpectedly`, exit 255), and so do a destination with no commit at all (`fatal: expected
// 'acknowledgments', received 'packfile'`, exit 128) and one that asks for a credential. A clone
// whose object directory's path holds a colon fails it too, since
// GIT_ALTERNATE_OBJECT_DIRECTORIES reads a colon as a separator, and git then holds none of the
// tips. Each tip is a `--negotiation-tip` argument of its own, which git reads from its command
// line alone, so a push of so many refs that their arguments pass the system's limit on a
// command's arguments fails to start the read, and the push keeps the base's bound
// (destinationHolds). The read runs for at most heldReadBound in all, and an error of any of its
// gits, as Guard.git's is, is a read that failed (destinationHolds).
func (g *Guard) negotiated(ctx context.Context, dir, url string, tips []string) ([]string, error) {
	bounded, cancel := context.WithTimeout(ctx, programbound.Of(&heldReadBound))
	defer cancel()
	paths, err := g.git(bounded, dir, "rev-parse", "--path-format=absolute", "--git-path", "objects", "--git-path", "shallow", "--show-object-format")
	if err != nil {
		return nil, err
	}
	answer := strings.Split(strings.TrimSuffix(paths, "\n"), "\n")
	if len(answer) != 3 {
		return nil, fmt.Errorf("git rev-parse printed %q, want the object directory, the shallow file and the object format", paths)
	}
	objects, shallow, format := answer[0], answer[1], answer[2]
	args := []string{"--shallow-file", shallow}
	scratch, err := os.MkdirTemp("", "legion-negotiation-")
	if err != nil {
		return nil, fmt.Errorf("the repository for git fetch --negotiate-only could not be made: %w", err)
	}
	defer os.RemoveAll(scratch)
	home, bare := filepath.Join(scratch, "home"), filepath.Join(scratch, "repo.git")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, err
	}
	// --template= copies no template, so the repository holds nothing but what git needs.
	if _, err := g.heldRead(bounded, scratch, home, nil, "init", "--quiet", "--bare", "--template=", "--object-format="+format, bare); err != nil {
		return nil, err
	}
	args = append(args, "fetch", "--negotiate-only")
	for _, tip := range tips {
		args = append(args, "--negotiation-tip="+tip)
	}
	args = append(args, "--", againstDir(dir, url))
	env := append([]string{"GIT_DIR=" + bare, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}, fetchesNothing...)
	out, err := g.heldRead(bounded, bare, home, env, args...)
	if err != nil {
		return nil, err
	}
	var common []string
	for line := range strings.Lines(out) {
		if id := strings.TrimRight(line, "\r\n"); isObjectID(id) {
			common = append(common, id)
		}
	}
	return common, nil
}

// heldRead runs one git of a read of what a push's destination holds (Guard.advertised,
// Guard.negotiated) in dir, with args after `-c credential.helper=`, and answers its standard
// output alone, its standard error taken apart, failing as Guard.git fails. It reads no git
// configuration but its command line's and carries no credential:
//
//   - of the caller's environment it keeps what the guard's read with no credential keeps
//     (lsRemoteInherits): every name that is not git's own, and of git's own GIT_ALLOW_PROTOCOL,
//     GIT_SSL_CAINFO and GIT_SSL_CAPATH alone, so no GIT_DIR, no GIT_CONFIG_COUNT block, no
//     GIT_CONFIG_PARAMETERS and no GIT_ASKPASS of the caller's; and none of the names libcurl
//     finds a netrc file by, HOME, NETRC and USERPROFILE;
//   - HOME is home, an empty directory of the read's own, so no .netrc of the caller's reaches it,
//     and it reads neither the system's configuration file nor the user's (readsNoConfigFile);
//   - `-c credential.helper=` empties the helper list again on its command line, after anything a
//     git wrapper of a host's own put in front of it, and the guard's prompt settings
//     (promptsNothing) leave git's askpass and its terminal prompt nothing to answer with.
//
// So over HTTP(S) no credential helper runs for it and no netrc file reaches it, and it reads no
// REST API: the `legion git-credential` that an issue pod's configuration names for github.com,
// which makes a REST read for an owner the bundle does not serve, never runs for it. A destination
// that asks for a credential fails it, as does a push whose destination is reached only through a
// transport setting made in git configuration (a core.sshCommand, an http.proxy, an
// http.sslCAInfo), where the environment variables that make the same settings reach it. Over
// ssh, SSH_AUTH_SOCK stays and ssh finds the user's keys by the home of the password database,
// not by HOME, so an ssh destination can still be read with the user's key: that is git's own
// transport, and no REST read. env goes on top.
func (g *Guard) heldRead(ctx context.Context, dir, home string, env []string, args ...string) (string, error) {
	var stderr strings.Builder
	status, out, err := g.run(ctx, child{
		argv:    gitArgv(append([]string{"-c", "credential.helper="}, args...)...),
		dir:     dir,
		inherit: func([]string) func(name, value string) bool { return lsRemoteInherits },
		env:     slices.Concat([]string{"HOME=" + home}, readsNoConfigFile, env),
		stderr:  &stderr,
	})
	if err != nil {
		return "", procgroup.Said(err, gitSaid(stderr.String()))
	}
	if status != 0 {
		return "", gitExited("git "+subcommand(args), status, stderr.String())
	}
	return out, nil
}

// subcommand is the git subcommand args run, after the global options heldRead's callers put
// before it: `--shallow-file <file>` (Guard.negotiated).
func subcommand(args []string) string {
	if len(args) > 2 && args[0] == "--shallow-file" {
		return args[2]
	}
	return args[0]
}

// againstDir is url as a read made outside dir reaches what git reached from dir: a URL that is a
// relative local path joined to dir, the directory git runs the hook in, which is the one git read
// that path from for the push, and every other URL as it is. A URL is a local path by git's own
// rule (connect.c's url_is_local_not_ssh): it names no scheme (`<scheme>://`), and it has no
// colon, or a slash before its first colon, since `host:path` with the colon first is git's scp
// form of ssh. The path is joined, not cleaned, so that a `..` in it is read from the directory
// itself, as git's own read of it is, rather than from the parent of a symbolic link dir was
// named through.
func againstDir(dir, url string) string {
	if strings.Contains(url, "://") || filepath.IsAbs(url) {
		return url
	}
	if colon := strings.IndexByte(url, ':'); colon >= 0 {
		if slash := strings.IndexByte(url, '/'); slash < 0 || slash > colon {
			return url
		}
	}
	return dir + string(filepath.Separator) + url
}

// isObjectID is whether id is an object id as git prints one: 40 or 64 lower-case hex digits, a
// SHA-1 or a SHA-256 one. A line git ls-remote prints, and one git fetch --negotiate-only prints,
// is read by it (Guard.advertised, Guard.negotiated): the first field of the one is the object id
// of the ref it lists, and the other is a commit's id alone, and a field that is no such id names
// no object of the repository read.
func isObjectID(id string) bool {
	return (len(id) == 40 || len(id) == 64) && strings.Trim(id, "0123456789abcdef") == ""
}
