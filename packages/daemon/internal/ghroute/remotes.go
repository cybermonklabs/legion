package ghroute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// Remote is one line of the working directory's git remotes that gh reads to find the
// repository of a command naming none: a URL line, a remote's fetch or push (push) URL as `git
// remote -v` prints it, or a marker line (marker), a gh-resolved setting, gh's default marker
// (`gh repo set-default`'s), as `git config --get-regexp` prints it: its key, its value
// (resolved), "base" for the remote's own repository or [HOST/]OWNER/REPO for another, and whether
// git printed a value at all (valued), which it does not for a key set with none. name is the
// remote's, for a marker line its key without `remote.` and `.gh-resolved`.
type Remote struct {
	name          string
	url           string
	push          bool
	marker        bool
	key, resolved string
	valued        bool
}

// markerLine is the marker line of one line `git config --get-regexp` prints, `<key> <value>`,
// or `<key>` alone for a key set with no value.
func markerLine(line string) Remote {
	key, value, valued := strings.Cut(line, " ")
	name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".gh-resolved")
	return Remote{name: name, marker: true, key: key, resolved: value, valued: valued}
}

// what is the remote line as a refusal names it. A URL line is named by its remote's name and, when
// it names a github.com OWNER/REPO this reads (repository), by its URL as remoteShown prints it;
// any other URL, which no rule has read as a repository's and whose path can hold a token
// (`https://github.com/acme/x-access-token:<token>`, where the URL rule finds no user info), as
// redact.URLNotPrinted. A marker line is named by its key and its value as markerShown prints it.
func (r Remote) what() string {
	if r.marker {
		return fmt.Sprintf("remote.%s.gh-resolved %s", r.name, markerShown(r.resolved))
	}
	if owner, _, _ := r.repository(); owner == "" {
		return fmt.Sprintf("remote %q %s", r.name, redact.URLNotPrinted)
	}
	return fmt.Sprintf("remote %q (%s)", r.name, remoteShown(r.url))
}

// markerShown is a gh-resolved marker's value as a refusal prints it: as written when it is empty,
// `base` or the OWNER/REPO gh writes there, an owner that is a GitHub login and a name a
// repository's (ghrepo.IsName, its `.git` trimmed), with nothing before or after it; anything
// else, which only a person writes, as redact.URLNotPrinted. A host before it, a URL's user info
// or text after the name (`github.com/acme/widgets/<token>`, which repositoryTarget reads as
// acme/widgets) can hold a token the URL rule does not see.
func markerShown(value string) string {
	if value == "" || value == "base" {
		return value
	}
	if owner, name, ok := strings.Cut(value, "/"); ok && ghconfig.IsOwner(strings.ToLower(owner)) && ghrepo.IsName(strings.TrimSuffix(name, ".git")) {
		return value
	}
	return redact.URLNotPrinted
}

// remoteShown is a remote's URL as legion gh prints it: read as gh reads it (ghParseGitURL, which
// makes an scp-like `git@host:owner/repo` an ssh URL), its query and fragment not at all, its path,
// the repository it names, as written, all through redact.URL, so its user info prints as the
// mark: a token written into a remote URL (`https://x-access-token:<token>@github.com/owner/repo`,
// which a CI checkout writes) printed whole. An ssh URL's user name alone (`ssh://git@host/…`, the
// scp-like `git@host:…`) prints as the mark too, its host with it: the rule cannot tell an SSH
// login from a token written as the user (`<token>@gh-work:owner/repo`), and every command's
// stderr passes the URL rule (newRoot) whatever this writes. A local path, with no ":", "@", "?"
// or "#", prints as written: it holds no scheme, user info, query or fragment. Anything else gh's
// parser reads as no URL with a host prints as redact.URLNotPrinted.
func remoteShown(raw string) string {
	if !strings.ContainsAny(raw, ":@?#") {
		return raw
	}
	u, err := ghParseGitURL(raw)
	if err != nil {
		return redact.URLNotPrinted
	}
	return redact.URL(u.String())
}

// ghRemoteSchemes are the URL schemes gh's git.ParseURL reads as they are; a remote with no such
// prefix and a colon it reads as scp-like ssh.
var ghRemoteSchemes = []string{"ssh:", "git+ssh:", "git:", "http:", "git+https:", "https:", "ftp:", "ftps:", "file:"}

// ghParseGitURL reads a git remote's URL as gh does (cli/cli's git.ParseURL, gh 2.65.0 through
// 2.98.0): a value with none of ghRemoteSchemes' prefixes, a colon and no backslash becomes an ssh
// URL, its first colon the path's first slash (`[user@]host:path`, `github.com://owner/repo`
// among them); git+https and git+ssh are https and ssh; and an ssh URL's path loses a doubled
// leading slash and its host its port. gh then takes the repository from the host and path
// whatever the scheme.
func ghParseGitURL(raw string) (*url.URL, error) {
	known := slices.ContainsFunc(ghRemoteSchemes, func(scheme string) bool { return strings.HasPrefix(raw, scheme) })
	if !known && strings.Contains(raw, ":") && !strings.Contains(raw, `\`) {
		raw = "ssh://" + strings.Replace(raw, ":", "/", 1)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "git+https":
		u.Scheme = "https"
	case "git+ssh":
		u.Scheme = "ssh"
	}
	if u.Scheme == "ssh" {
		if strings.HasPrefix(u.Path, "//") {
			u.Path = strings.TrimPrefix(u.Path, "/")
		}
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	return u, nil
}

// repository is the lowercased owner and the name of the github.com repository the remote line
// names, read as gh reads it (ghParseGitURL), "" for one naming none gh reads: a remote with no
// host (a local path) or on another host, and a marker line with no value, an empty one or
// "base", whose remote's own URL is read beside it. A URL line's name must have a repository
// name's characters once its `.git` is trimmed (ghrepo.IsName), as a marker's and a -R value's
// must, since the refusal of two owners prints it (ghNamed.shown) and a token can be written into
// the URL's path (`https://github.com/acme/x-access-token:<token>@github.com`, `…/acme/<token>%40x`).
// It is then carried on as the line spells it, `.git` and all, and fromWorkingDirectory keys it
// with ghconfig.RepositoryName. why says why a line is one this cannot read: a URL gh's parser
// refuses, a remote whose host holds a letter beyond ASCII (isASCII), which gh's lowering may make
// github.com, an ssh remote on another host, which gh may read as an alias of github.com from
// ssh's configuration, and a github.com one naming no OWNER/REPO whose owner is a GitHub login
// and whose name is a repository name.
func (r Remote) repository() (owner, repo, why string) {
	if r.marker {
		if r.resolved == "" || r.resolved == "base" {
			return "", "", ""
		}
		owner, repo, err := repositoryTarget(r.resolved, "", false)
		if err != nil {
			return "", "", "names no github.com repository this can read"
		}
		return owner, repo, ""
	}
	u, err := ghParseGitURL(r.url)
	switch {
	case err != nil:
		return "", "", "is a URL this cannot read as gh does"
	case u.Hostname() == "":
		return "", "", ""
	case !isASCII(u.Hostname()):
		return "", "", "is a remote whose host holds a letter beyond ASCII, which gh may lower to github.com"
	case u.Scheme == "ssh" && !isGitHubHost(u.Hostname()):
		return "", "", "is an ssh remote whose host gh may read as github.com through ssh's configuration"
	case !isGitHubHost(u.Hostname()):
		return "", "", ""
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 3)
	if owner = strings.ToLower(parts[0]); len(parts) != 2 || parts[1] == "" || !ghconfig.IsOwner(owner) || !ghrepo.IsName(strings.TrimSuffix(parts[1], ".git")) {
		return "", "", "names no OWNER/REPO whose owner is a GitHub login and whose name is a repository name"
	}
	return owner, parts[1], ""
}

// isPlainGitHubURL says whether the URL line names a github.com repository gh reads with nothing
// the wrapper cannot read: gh's parser reads it (ghParseGitURL) as no ssh URL, whose host gh
// rewrites through ssh's own configuration (go-gh pkg/ssh/ssh.go), on an ASCII host (isASCII) gh's
// normalizeHostname (cli/cli internal/ghrepo/repo.go: a leading lower-case `www.` stripped, then
// lower-cased) makes github.com, the one host context/remote.go's FilterByHosts keeps, and it
// names an OWNER/REPO whose owner is a GitHub login and whose name is a repository name
// (repository).
func (r Remote) isPlainGitHubURL() bool {
	u, err := ghParseGitURL(r.url)
	if err != nil || u.Scheme == "ssh" || !isASCII(u.Hostname()) || strings.ToLower(strings.TrimPrefix(u.Hostname(), "www.")) != "github.com" {
		return false
	}
	owner, _, _ := r.repository()
	return owner != ""
}

// readsAsMarker says whether the marker line counts among the markers gh reads (cli/cli
// git/client.go, populateResolvedRemotes): git printed a value, and the key has a second
// `.`-separated part, gh's name of its remote (`remote.origin.fork.gh-resolved` is origin's). An
// empty value only clears its remote's marker, which counts on origin, whose `base` it may clear,
// and on no other remote, where a marker it could clear is a line of its own.
func (r Remote) readsAsMarker() bool {
	_, rest, named := strings.Cut(r.key, ".")
	remote, _, _ := strings.Cut(rest, ".")
	return r.valued && named && (r.resolved != "" || remote == "origin")
}

// markedOrigin is the index in remotes of origin's fetch line, and whether gh's default marker
// makes that line's repository the one gh resolves: of the marker lines gh reads as one
// (readsAsMarker), one is left, `remote.origin.gh-resolved base`, and origin has one fetch line,
// whose URL gh keeps as a github.com remote with nothing the wrapper cannot read
// (isPlainGitHubURL). gh then takes origin, the first remote marked "base" (cli/cli
// context/context.go, ResolvedRemotes.BaseRepo), and its repository from that fetch URL
// (context/remote.go, TranslateRemotes), whatever origin's push URLs and the other remotes name.
func markedOrigin(remotes []Remote) (fetch int, ok bool) {
	var marker Remote
	markers, fetches := 0, 0
	for i, remote := range remotes {
		switch {
		case remote.marker && remote.readsAsMarker():
			markers++
			marker = remote
		case !remote.marker && !remote.push && remote.name == "origin":
			fetches++
			fetch = i
		}
	}
	return fetch, markers == 1 && marker.key == "remote.origin.gh-resolved" && marker.resolved == "base" &&
		fetches == 1 && remotes[fetch].isPlainGitHubURL()
}

// gitRemoteLine is a line of `git remote -v` as git prints it: `<name>\t<url> (fetch)` or
// ` (push)`, a fetch line of a remote with a partial clone filter followed by ` [<filter>]`
// (builtin/remote.c). git prints the URL and the filter as they are configured, so either can hold
// whitespace, and gh's reading (ghRemoteLine) then takes another URL from the line; the URL and the
// filter accepted here hold none of the whitespace gh's `\s` matches.
var gitRemoteLine = regexp.MustCompile(`^([^\t]+)\t([^\t\n\f\r ]+) \((fetch|push)\)( \[[^\t\n\f\r ]+\])?$`)

// ghRemoteLine is gh's own reading of a `git remote -v` line (remoteRE, cli/cli v2.98.0
// git/client.go:26): unanchored and greedy, so gh takes the URL before the line's last
// whitespace-preceded `(fetch)` or `(push)`, and trims the name and URL it captures.
var ghRemoteLine = regexp.MustCompile(`(.+)\s+(.+)\s+\((push|fetch)\)`)

// ReadGitRemotes is the working directory's git remotes as gh reads them, with git itself run
// under env, the environment gh runs with (`legion gh`'s cmd/legion ghChildEnviron), so it reads
// the repository, the remotes and the configuration gh's own git reads, and no GIT_DIR, global
// file or url.<base>.insteadOf of the caller's that gh's git never sees: every fetch and push URL
// `git remote -v` prints, each with its kind, a partial clone's fetch line included (git prints
// the remote's filter after it, `(fetch) [blob:none]`), and every gh-resolved line `git config
// --get-regexp` prints (markerLine); none for a directory that is no git repository, or where no
// git runs, since gh then reads none either. A URL line is read only when it has git's shape
// (gitRemoteLine, a filter on a fetch line alone) and gh's own reading of it (ghRemoteLine) names
// the same remote, URL and kind; any other line is refused, since gh could act on a repository
// this reading never counted. Both reads run under ctx (gitLines).
func ReadGitRemotes(ctx context.Context, env []string) ([]Remote, error) {
	lines, err := gitLines(ctx, env, "remote", "-v")
	if err != nil {
		return nil, err
	}
	var remotes []Remote
	for _, line := range lines {
		git := gitRemoteLine.FindStringSubmatch(line)
		gh := ghRemoteLine.FindStringSubmatch(line)
		if git == nil || (git[3] == "push" && git[4] != "") || gh == nil ||
			strings.TrimSpace(gh[1]) != git[1] || strings.TrimSpace(gh[2]) != git[2] || gh[3] != git[3] {
			// The line is named by its remote alone: its URL or filter holds whitespace, where the
			// URL rule ends a URL, so a credential holding a space would print past the rule.
			name, _, tabbed := strings.Cut(line, "\t")
			if !tabbed {
				return nil, errors.New("git remote -v printed a line with no tab, which legion gh cannot read as gh does; the line is not printed")
			}
			return nil, fmt.Errorf("git remote -v printed a line for the remote %q that legion gh cannot read as gh does (its URL or filter holds whitespace, or it has no git's shape); the line is not printed", name)
		}
		remotes = append(remotes, Remote{name: git[1], url: git[2], push: git[3] == "push"})
	}
	if lines == nil {
		return nil, nil
	}
	resolved, err := gitLines(ctx, env, "config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	if err != nil {
		return nil, err
	}
	for _, line := range resolved {
		remotes = append(remotes, markerLine(line))
	}
	return remotes, nil
}

// GitReadBound is the longest `legion gh` waits for one of the git reads it makes of the
// repository before gh runs: the configuration read (cmd/legion's ghRepoGitConfigRefusal) and the
// remotes and their markers (gitLines). Each reads a few files of the repository and answers in
// milliseconds. A git that has not answered in 30 s waits on something it will not get, a
// configuration on a mount that hangs or a git wrapper that waits, and the person running `legion
// gh` gets the read's refusal, which sends nothing, rather than a call that never returns; 30 s is
// room for a loaded machine. It is a variable so that a test binary can give it its own deadline
// (programbound), and exported so that both reads take this one bound.
var GitReadBound = 30 * time.Second

func init() {
	programbound.Register(&GitReadBound)
}

// gitLines runs git with args in the working directory, under env, and answers the lines it
// prints: none for a directory that is no git repository, where no git runs, or, for `git config
// --get-regexp`, where nothing matches.
//
// It takes no lock: `git remote -v` and `git config --get-regexp` read configuration and write
// nothing, so neither can leave a lock file behind for the next reader of the repository. It runs
// under ctx, `legion gh`'s signal context (cmd/legion's runGh, beforeGh), in a process group of
// its own that its bound or the end of ctx stops whole (procgroup.Configure): a git, or a wrapper
// of it, whose child holds the output pipe would otherwise keep the call waiting past the bound
// for as long as that child lives, since a stop of git alone leaves the pipe open, and the
// terminal's signals reach that group only through ctx (runGh's rule). A git that exits while a
// child still holds the pipe is waited for no longer than procgroup's WaitDelay, and its read is
// then refused, since a remote list cut short could route the call to another owner's directory.
func gitLines(ctx context.Context, env []string, args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, programbound.Of(&GitReadBound))
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	procgroup.Configure(cmd)
	cmd.Env = append(slices.Clone(env), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil, fmt.Errorf("git %s exited, but something it started still held its output %s later, so what was read may be cut short", strings.Join(args, " "), procgroup.WaitDelayNow())
	}
	end := procgroup.Ended(err)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, nil
	case end.Ran() && strings.Contains(stderr.String(), "not a git repository"):
		return nil, nil
	case args[0] == "config" && end.ExitedWith(1):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	var lines []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, nil
}
