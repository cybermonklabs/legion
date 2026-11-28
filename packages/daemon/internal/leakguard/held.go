package leakguard

import (
	"context"
	"errors"
	"io/fs"
	"os"
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

// destinationHolds is what a push's destination says it holds, read once a push
// (Guard.pushRecords) by its two reads of the URL git handed the hook: not, a line ^<id> for each
// commit of each read that answered, which Guard.commits feeds rev-list, and negotiated, whether
// the fetch negotiation answered. tips are the local commits of the push's updates, which the
// negotiation walks (Guard.negotiated); the advertised refs are read whole (Guard.advertised).
// The two reads run at the same time, the negotiation in a goroutine of its own, so a destination
// that answers neither holds the push for one heldReadBound, not two. Each connects as the push
// connects (Guard.heldRead), and pushRecords makes them only where they reach the repository the
// push writes to (readsReachItsRepository).
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
// the push alone, as they did before these reads and as they do for a push pushRecords makes
// neither read for.
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

// readsReachItsRepository is whether pushRecords makes the two reads of what the push's
// destination holds (destinationHolds) for the push git hands the pre-push hook, to url from the
// repository dir and the guard's environment name: whether those reads, which connect as the push
// connects (Guard.heldRead), reach the repository the push writes to. Where it answers false, the
// remote-tracking refs bound the push alone, as they did before these reads, so that a read never
// stands for a repository the push does not write to (R10 of dispatch://INFRA-3524's spec). It
// answers false where a read it makes fails, and in each of these cases, a condition of its own:
//
//   - url is not one whose transport git serves itself (gitServesItsTransport): git reaches the
//     server of any other URL through a program, an ssh program, the first `ssh` on PATH where the
//     push names none, or a git-remote-<scheme> found on PATH, which sees whether it serves a read
//     or the push and can send the two to different repositories.
//   - The push can write somewhere other than url (writesOnlyToItsURL, by pushConfig): a
//     receive-pack program of its own, a remote helper or another exec path's git-receive-pack
//     writes where that program writes, and what url holds says nothing of that repository.
//   - A key of the configuration is one routedKeys names: a program or a proxy between git and
//     the server, or a transport key of a remote that a read of a URL does not apply.
//   - The guard's environment sets one of routedNames: a program or a proxy between git and the
//     server, named by the variable git or libcurl reads it from.
//   - A url.<base>.insteadOf of the configuration rewrites url (rewritesURL).
//   - url is also the name of a remote of the repository (urlNamesARemote).
//
// The configuration is read once for the push, `git config -z --list` in dir through Guard.git
// (pushConfig), which reads it as the push's own gits read it: every scope down to the command
// line's (GIT_CONFIG_PARAMETERS, which `git -c` hands the hook, and a GIT_CONFIG_COUNT block),
// under the guard's whole environment, a GIT_DIR there included.
func (g *Guard) readsReachItsRepository(ctx context.Context, dir, url string) bool {
	if !gitServesItsTransport(url) {
		return false
	}
	base := g.base()
	config, only := g.pushConfig(ctx, dir, url, base)
	if !only {
		return false
	}
	if setsKeyOf(config, routedKeys) {
		return false
	}
	if setsNameOf(base, routedNames) {
		return false
	}
	if rewritesURL(config, url) {
		return false
	}
	if g.urlNamesARemote(ctx, dir, config, url) {
		return false
	}
	return true
}

// gitServesItsTransport is whether git serves the transport of url, the URL git hands the hook,
// itself, with no program of another's between git and the server: https://, http://, git://,
// file:// and a local path (R10 of dispatch://INFRA-3524's spec). For every other URL git connects
// through a program it finds, and hands that program the service it asks for, git-upload-pack for
// a read and git-receive-pack for the push, so the program can send the two to different
// repositories, and a read of url through it need not reach the repository the push writes to:
//
//   - Over ssh, ssh://, git+ssh://, ssh+git:// and the scp-like host:path, git always runs an ssh
//     program, the one the push names (core.sshCommand, GIT_SSH_COMMAND or GIT_SSH, which
//     routedKeys and routedNames name too) or else the first `ssh` on PATH, and hands it the
//     service as its remote command. Measured by dispatch://INFRA-3524's tester on 2026-11-28 at
//     33a24deb, through the head's git wrapper and hook: with no setting at all and an `ssh` first
//     on PATH that routed by service, a push to ssh://example.invalid<path> landed a commit the
//     rules refuse in a second repository while the reads answered for the hook's URL, and main's
//     hook refused it.
//   - Any other <scheme>:// git serves through the remote helper git-remote-<scheme>, which it
//     looks up on PATH after its own exec path, and which, where it answers `connect`, it hands
//     the service in `connect <service>` (gitremote-helpers(7)). git's own exec path holds
//     git-remote-http and git-remote-https, which serve http:// and https://, and a push whose
//     exec path is not git's own gets no read either (pushConfig, ownExecPath).
//
// url is read as git 2.47.3 reads it to pick a transport (transport.c's transport_get, connect.c's
// parse_connect_url, get_protocol and url_is_local_not_ssh). A URL that holds `://` is served by
// git itself only where what comes before its first `://` is exactly http, https, git or file,
// which git compares case-sensitively, so that a git-remote-HTTPS serves HTTPS://. A URL that
// holds no `://` is the scp-like ssh where it holds a colon and no slash comes before its first
// colon, and a local path otherwise. A <transport>::<address>, which git serves through
// git-remote-<transport>, answers false too, since no slash comes before its first colon and what
// comes before a `://` in it is none of the four, and R1 keeps it guarded on its own (pushConfig).
func gitServesItsTransport(url string) bool {
	if scheme, _, ok := strings.Cut(url, "://"); ok {
		return scheme == "https" || scheme == "http" || scheme == "git" || scheme == "file"
	}
	colon := strings.IndexByte(url, ':')
	if colon < 0 {
		return true
	}
	slash := strings.IndexByte(url, '/')
	return slash >= 0 && slash < colon
}

// configKey is a key of git's configuration in its three parts, as `git config -z --list` writes
// one (keyParts): its section, its subsection and its variable. In routedKeys a subsection of "*"
// stands for any subsection, and "" for none.
type configKey struct{ section, subsection, variable string }

// routedKeys are the configuration keys under which the two reads of what a push's destination
// holds need not reach the repository the push writes to (readsReachItsRepository), set in any
// scope with any value, empty included.
var routedKeys = []configKey{
	// A program between git and the server, which git runs for the connection and hands the
	// service it asks for: an ssh command (core.sshCommand) and the proxy command of a git:// URL
	// (core.gitProxy). Such a program sees whether it serves a read (git-upload-pack) or the push
	// (git-receive-pack), and can send the two to different repositories: measured by
	// dispatch://INFRA-3524's tester at 917b2071, a push through each landed a commit the rules
	// refuse in a second repository while the reads answered for the hook's URL.
	{"core", "", "sshcommand"},
	{"core", "", "gitproxy"},
	// A proxy between git and an http(s) server, which sees each request's service in its path and
	// can do the same: http.proxy, an http.<url>.proxy of any URL, and a remote.<name>.proxy of
	// any remote, which a push by that remote's name applies and a read of a URL does not.
	{"http", "", "proxy"},
	{"http", "*", "proxy"},
	{"remote", "*", "proxy"},
	// A transport key of a remote that a read of a URL does not apply, so that a push by that
	// remote's name connects in a way neither read does: remote.<name>.proxyAuthMethod, beside
	// remote.<name>.proxy above.
	{"remote", "*", "proxyauthmethod"},
}

// routedNames are the environment variables under which the two reads of what a push's
// destination holds need not reach the repository the push writes to (readsReachItsRepository),
// each set in the guard's environment with any value, empty included.
var routedNames = []string{
	// A program between git and the server, for core.sshCommand's and core.gitProxy's reason
	// (routedKeys), as git reads one from its environment: an ssh command (GIT_SSH_COMMAND, and
	// GIT_SSH, a program run in ssh's place) and the proxy command of a git:// URL.
	"GIT_SSH_COMMAND", "GIT_SSH", "GIT_PROXY_COMMAND",
	// A proxy between git and an http(s) server, for http.proxy's reason, as git and libcurl
	// read one from the environment.
	"http_proxy", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY",
}

// keyParts is key, a key as `git config -z --list` writes it, in its three parts: its first
// dot-part the section, its last the variable, and what lies between them the subsection, which
// can hold dots, "" where there is none. git writes the section and the variable in lower case
// and the subsection as it was written. A key with no dot is none git writes, and answers false.
func keyParts(key string) (configKey, bool) {
	section, rest, ok := strings.Cut(key, ".")
	if !ok {
		return configKey{}, false
	}
	last := strings.LastIndexByte(rest, '.')
	if last < 0 {
		return configKey{section: section, variable: rest}, true
	}
	return configKey{section: section, subsection: rest[:last], variable: rest[last+1:]}, true
}

// setsKeyOf is whether config, the output of `git config -z --list`, holds a key one of keys
// names: the same section and variable, each read without case as git reads them, and a
// subsection where keys has "*", none where it has "".
func setsKeyOf(config string, keys []configKey) bool {
	for entry := range strings.SplitSeq(config, "\x00") {
		name, _, _ := strings.Cut(entry, "\n")
		key, ok := keyParts(name)
		if !ok {
			continue
		}
		for _, named := range keys {
			if strings.EqualFold(key.section, named.section) && strings.EqualFold(key.variable, named.variable) &&
				(named.subsection == "*") == (key.subsection != "") {
				return true
			}
		}
	}
	return false
}

// setsNameOf is whether env sets one of names, with any value, empty included.
func setsNameOf(env, names []string) bool {
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && slices.Contains(names, name) {
			return true
		}
	}
	return false
}

// rewritesURL is whether config, the output of `git config -z --list`, holds a
// url.<base>.insteadOf whose value url begins with, an empty value included: git reads a URL that
// begins with such a value as <base> and the rest, so a read of url in the repository reaches
// what <base> names. git hands the hook the URL its rewrites have already led the push to, and a
// rewrite applied to it again can lead elsewhere: under url.<base>.pushInsteadOf, which git
// applies to the push URL alone, the URL the hook is handed can be one a url.<base>.insteadOf
// rewrites. The review of cybermonklabs/legion#3946 reproduced that on git 2.47.3: with origin at
// https://example.invalid/fork, url.<real.git>.pushInsteadOf that URL and url.<fake.git>.insteadOf
// <real.git>, the push landed in real.git, the hook was handed real.git, and a git ls-remote of it
// in the clone listed fake.git's refs. The section and the variable are read without case, and
// the base is any.
func rewritesURL(config, url string) bool {
	for entry := range strings.SplitSeq(config, "\x00") {
		name, value, _ := strings.Cut(entry, "\n")
		key, ok := keyParts(name)
		if ok && strings.EqualFold(key.section, "url") && strings.EqualFold(key.variable, "insteadof") && strings.HasPrefix(url, value) {
			return true
		}
	}
	return false
}

// urlNamesARemote is whether url, the URL git hands the hook, is also the name of a remote of the
// repository in dir. git ls-remote and git fetch read their repository argument as a remote's
// name first (remote.c's remote_get), and then read that remote's URL and its other keys,
// remote.<name>.uploadpack among them, rather than url itself, while a push by another remote
// whose URL is url writes to url. Measured with git 2.47.3 on 2026-11-28: with origin at
// file://…/dest.git and remote."file://…/dest.git".url=…/other.git, git ls-remote
// file://…/dest.git in the clone listed other.git's main, while a push by origin writes to
// dest.git; a remote."file://…/dest.git".uploadpack of `git-upload-pack '…/other.git' #` led git
// ls-remote and git fetch --negotiate-only there too, as a remotes/<name> or branches/<name> file
// naming other.git led git ls-remote of the bare name there.
//
// url names a remote where a key of config, the output of `git config -z --list`, is
// remote.<url>.<variable> for any variable, the subsection compared exactly, as git compares a
// remote's name. Where url is a name git reads the legacy files for (valid_remote_nick: not
// empty, not `.` or `..`, and no `/`), it also names one where remotes/<url> or branches/<url>
// exists under the repository's git directory, the two paths read with one
// `git rev-parse --path-format=absolute --git-path remotes/<url> --git-path branches/<url>` in
// dir, and only then. A read that fails answers true, which keeps the push to the base's bound.
func (g *Guard) urlNamesARemote(ctx context.Context, dir, config, url string) bool {
	for entry := range strings.SplitSeq(config, "\x00") {
		name, _, _ := strings.Cut(entry, "\n")
		if key, ok := keyParts(name); ok && strings.EqualFold(key.section, "remote") && key.subsection == url {
			return true
		}
	}
	if url == "" || url == "." || url == ".." || strings.Contains(url, "/") {
		return false
	}
	paths, err := g.git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "remotes/"+url, "--git-path", "branches/"+url)
	if err != nil {
		return true
	}
	files := strings.Split(strings.TrimSuffix(paths, "\n"), "\n")
	if len(files) != 2 {
		return true
	}
	for _, file := range files {
		if _, err := os.Lstat(file); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// advertised are the commits the repository at url holds at its branches and tags, of those the
// repository at dir has: commits a push to url need not publish again (destinationHolds). The
// read is of url itself, never of a ref of dir's own, which says nothing reliable of what url
// holds (destinationHolds says why):
//
//	git -c credential.helper= ls-remote --heads --tags -- <url>
//
// run in dir, as the push's own gits run, so that it connects to url as the push did (heldRead):
// a URL that is a relative local path is read from dir, where git read it for the push, and each
// transport setting the push uses carries the read too. It carries no credential (heldRead). The
// `--` keeps a URL that starts with a dash from being read as an option. Each sha it prints is
// kept where dir has it as a commit, an annotated tag's peeled commit (<tag>^{}) too; one
// cat-file --batch-check of them all, run in dir, answers which. A line whose first field is no
// object id as git prints one, 40 or 64 lower-case hex digits (isObjectID), holds nothing and is
// not asked of: cat-file reads a name or an expression, HEAD or main~1, as a revision of dir's,
// and would take the commit it names in dir as one url holds. A sha dir does not have holds
// nothing, since rev-list cannot walk from it: where it is a commit ahead of dir's, the commits of
// dir's beneath it are left to the negotiation (Guard.negotiated), where that fails to the
// remote-tracking refs, and past those they are read, which is the fail-closed direction. The
// cat-file fetches nothing (fetchesNothing), so that in a partial clone, as an issue pod's fork
// clones are (--filter=blob:none), a sha dir lacks is answered missing rather than fetched from
// the promisor remote. An empty answer, a repository with no branch and no tag, holds nothing. A
// git that cannot be run, that exits other than 0, or that its bound stops is an error, as
// Guard.git's is, and destinationHolds reads it as a read that failed.
func (g *Guard) advertised(ctx context.Context, dir, url string) ([]string, error) {
	bounded, cancel := context.WithTimeout(ctx, programbound.Of(&heldReadBound))
	defer cancel()
	out, err := g.heldRead(bounded, dir, nil, "ls-remote", "--heads", "--tags", "--", url)
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
//	git -c credential.helper= fetch --negotiate-only --no-write-fetch-head \
//	    --no-auto-maintenance --negotiation-tip=<tip>... -- <url>
//
// run in dir, as Guard.advertised's read runs, so that it connects to url as the push did
// (heldRead), and fetching nothing (fetchesNothing). git walks dir's own commits, and in a shallow
// clone stops at its shallow commits, since it reads dir's shallow file as every git run in dir
// does. It fetches nothing and writes no ref: it prints each commit the destination acknowledged
// as one it holds, one a line, and stops; measured with git 2.47.3 on 2026-11-28, in a clone it
// wrote no FETCH_HEAD, no ref and no object. --no-write-fetch-head and --no-auto-maintenance say
// so on its command line all the same, so that no FETCH_HEAD is written and no maintenance runs
// after it, whatever dir's configuration says of either. A line that is no object id
// (isObjectID) holds nothing.
//
// The negotiation needs protocol v2: a destination served on v0 or v1 fails it (git 2.47.3 printed
// `warning: --negotiate-only requires protocol v2` and `fatal: the remote end hung up
// unexpectedly`, exit 255), and so do a destination with no commit at all (`fatal: expected
// 'acknowledgments', received 'packfile'`, exit 128) and one that asks for a credential. Each tip
// is a `--negotiation-tip` argument of its own, which git reads from its command line alone, so a
// push of so many refs that their arguments pass the system's limit on a command's arguments
// fails to start the read, and the push keeps the base's bound (destinationHolds). The read runs
// for at most heldReadBound, and an error of its git, as Guard.git's is, is a read that failed
// (destinationHolds).
func (g *Guard) negotiated(ctx context.Context, dir, url string, tips []string) ([]string, error) {
	bounded, cancel := context.WithTimeout(ctx, programbound.Of(&heldReadBound))
	defer cancel()
	args := append(make([]string, 0, len(tips)+6), "fetch", "--negotiate-only", "--no-write-fetch-head", "--no-auto-maintenance")
	for _, tip := range tips {
		args = append(args, "--negotiation-tip="+tip)
	}
	args = append(args, "--", url)
	out, err := g.heldRead(bounded, dir, fetchesNothing, args...)
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
// Guard.negotiated) in dir, with args after `-c credential.helper=` and env on top of the guard's
// environment, and answers its standard output alone, its standard error taken apart, failing as
// Guard.git fails. It connects as the push connects: it runs in the push's own repository, with
// the push's own configuration, every scope of it, and the guard's whole environment, which is
// the hook's, GIT_DIR, GIT_CONFIG_PARAMETERS, a GIT_CONFIG_COUNT block and HOME included. So each
// transport setting the push uses, an http or curl setting or a protocol setting, carries the
// read too, and the read answers for the destination the push reaches through it; where the push
// reaches its server through a program or a proxy, one it names or one git finds on PATH for a
// URL whose transport git does not serve itself, which can send a read and the push to two
// repositories, no read is made (Guard.readsReachItsRepository). It uses no credential:
//
//   - `-c credential.helper=` empties the list of credential helpers on its command line, which
//     git reads after every other scope: measured with git 2.47.3, it empties a
//     credential.<url>.helper of the repository's own file, and one a GIT_CONFIG_COUNT block or
//     GIT_CONFIG_PARAMETERS gives, as it empties credential.helper;
//   - the guard's prompt settings (promptsNothing) leave git's askpass, ssh's askpass and git's
//     terminal prompt nothing to answer with.
//
// So no credential helper runs for it and nothing is prompted, and it reads no REST API: the
// `legion git-credential` that an issue pod's configuration names for github.com, which makes a
// REST read for an owner the bundle does not serve, never runs for it. The rules run only for a
// destination that reads as public, which answers a read with no credential, and a destination
// that asks for one fails both reads. No read is made over ssh (gitServesItsTransport), so no
// key of the user's reaches a server for one.
func (g *Guard) heldRead(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	var stderr strings.Builder
	status, out, err := g.run(ctx, child{
		argv:   gitArgv(append([]string{"-c", "credential.helper="}, args...)...),
		dir:    dir,
		env:    env,
		stderr: &stderr,
	})
	if err != nil {
		return "", procgroup.Said(err, gitSaid(stderr.String()))
	}
	if status != 0 {
		return "", gitExited("git "+args[0], status, stderr.String())
	}
	return out, nil
}

// isObjectID is whether id is an object id as git prints one: 40 or 64 lower-case hex digits, a
// SHA-1 or a SHA-256 one. A line git ls-remote prints, and one git fetch --negotiate-only prints,
// is read by it (Guard.advertised, Guard.negotiated): the first field of the one is the object id
// of the ref it lists, and the other is a commit's id alone, and a field that is no such id names
// no object of the repository read.
func isObjectID(id string) bool {
	return (len(id) == 40 || len(id) == 64) && strings.Trim(id, "0123456789abcdef") == ""
}
