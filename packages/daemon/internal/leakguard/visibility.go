package leakguard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"golang.org/x/sys/unix"
)

const rateLimitWaitBound = 2 * time.Minute

// Clock supplies the time and waits visibility reads use. Tests replace it so retrying a near
// rate-limit reset does not sleep for wall-clock time.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Sleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// VisibilitySource says where a guard's answer came from: GitHub's REST API, the role's kept
// answer, or git's ls-remote, the read the guard makes when the REST read is refused for a spent
// rate limit it does not wait for (Guard.lsRemoteVisibility).
type VisibilitySource string

const (
	VisibilityFromGitHub   VisibilitySource = "github"
	VisibilityFromCache    VisibilitySource = "kept"
	VisibilityFromLsRemote VisibilitySource = "ls-remote"
)

// Visibility is what GitHub reports the repository slug ("owner/name") as. Public answers stay
// for the role's pod life. Private and internal answers are read each time, so a repository made
// public cannot skip a rule pass.
//
// A read GitHub refuses for a spent rate limit is read once more after the reset, where that is
// at most rateLimitWaitBound away. A limit still spent after that, and one that resets later, is
// read a second way, which costs no REST request: git's ls-remote (Guard.lsRemoteVisibility),
// whose answer is kept as gh's is. Where that read cannot answer either, the call is refused,
// naming the spent limit and its reset.
//
// The header-bearing read is:
//
//	gh api --hostname github.com repos/<slug> --include --jq .visibility
//
// run with PUBLIC_LEAK_CHECK_VISIBILITY_READ=<slug> in its environment. A pod's gh is `legion
// gh`, which calls this guard, so this read must not be mistaken for the shared check's callback
// or it would recurse. The callback's exact argv is separate (VisibilityReadArgs): it has no
// `--include`, while this reader needs the headers to detect a near rate limit. The caller reaches
// this through Guard.scanThrough, which lets no slug the exemption would refuse get this far.
func (g *Guard) Visibility(ctx context.Context, slug string) (Visibility, error) {
	if visibility, ok := g.keptVisibility(slug); ok {
		g.reportVisibility(VisibilityFromCache)
		return visibility, nil
	}
	visibility, err := g.readVisibility(ctx, slug)
	var limited *rateLimitError
	if errors.As(err, &limited) {
		if wait := limited.reset.Sub(g.clock().Now()); !limited.reset.IsZero() && wait >= 0 && wait <= rateLimitWaitBound {
			if err := g.clock().Sleep(ctx, wait); err != nil {
				return "", err
			}
			visibility, err = g.readVisibility(ctx, slug)
		}
	}
	source := VisibilityFromGitHub
	if errors.As(err, &limited) {
		visibility, err = g.lsRemoteVisibility(ctx, slug, limited)
		source = VisibilityFromLsRemote
	}
	if err != nil {
		return "", err
	}
	g.keepVisibility(slug, visibility)
	g.reportVisibility(source)
	return visibility, nil
}

func (g *Guard) clock() Clock {
	if g.opts.Clock != nil {
		return g.opts.Clock
	}
	return wallClock{}
}

func (g *Guard) readVisibility(ctx context.Context, slug string) (Visibility, error) {
	if read := g.opts.Visibility; read != nil {
		return read(ctx, slug)
	}
	return g.ghVisibility(ctx, slug)
}

func (g *Guard) reportVisibility(source VisibilitySource) {
	if g.opts.OnVisibility != nil {
		g.opts.OnVisibility(source)
	}
}

type visibilityCacheEntry struct {
	Visibility Visibility `json:"visibility"`
}

func (g *Guard) keptVisibility(slug string) (Visibility, bool) {
	path := g.visibilityCachePath(slug)
	if path == "" {
		return "", false
	}
	dir, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	cache := os.NewFile(uintptr(fd), path)
	defer cache.Close()
	info, err = cache.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	content, err := io.ReadAll(cache)
	if err != nil {
		return "", false
	}
	var entry visibilityCacheEntry
	if json.Unmarshal(content, &entry) != nil || entry.Visibility != Public {
		return "", false
	}
	return Public, true
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func (g *Guard) keepVisibility(slug string, visibility Visibility) {
	if visibility != Public {
		return
	}
	path := g.visibilityCachePath(slug)
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return
	}
	encoded, err := json.Marshal(visibilityCacheEntry{Visibility: visibility})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".visibility-")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}

func (g *Guard) visibilityCachePath(slug string) string {
	state := value(g.base(), "XDG_STATE_HOME")
	if state == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(slug))
	return filepath.Join(state, "legion-gh", "visibility", fmt.Sprintf("%x.json", sum))
}

// VisibilityReadVar is the environment variable the shared public leak check's own visibility
// callback carries, naming the repository it reads. It is exported with VisibilityReadArgs because
// `legion gh` has to let exactly this callback through unchecked. A callback written from a second
// copy of these strings can silently part from this exemption, making a first write to another
// owner's repository recurse without end.
const VisibilityReadVar = "PUBLIC_LEAK_CHECK_VISIBILITY_READ"

// VisibilityReadArgs is the shared public leak check callback's exact argv for slug, gh's own
// name aside. The callback does not request response headers.
func VisibilityReadArgs(slug string) []string {
	return []string{"api", "--hostname", "github.com", "repos/" + slug, "--jq", ".visibility"}
}

// VisibilityReadWithHeadersArgs is the guard's own visibility read. It is separate from the
// shared check callback because it needs headers for rate-limit handling.
func VisibilityReadWithHeadersArgs(slug string) []string {
	return []string{"api", "--hostname", "github.com", "repos/" + slug, "--include", "--jq", ".visibility"}
}

// ghVisibility is the gh read itself, the guard's answer where a caller named no reader of its
// own. GH_HOST names the host for a gh that has no configuration of its own to read it from,
// beside the --hostname the argv already carries.
func (g *Guard) ghVisibility(ctx context.Context, slug string) (Visibility, error) {
	status, printed, err := g.run(ctx, child{
		argv: append([]string{"gh"}, VisibilityReadWithHeadersArgs(slug)...),
		env:  []string{VisibilityReadVar + "=" + slug, "GH_HOST=github.com"},
	})
	if err != nil {
		return "", err
	}
	if limited := rateLimitFrom(printed); limited != nil {
		return "", limited
	}
	if status != 0 {
		return "", fmt.Errorf("gh api repos/%s exited %d: %s", maskPath(slug), status, strings.TrimSpace(printed))
	}
	switch visibility := Visibility(strings.ToLower(strings.TrimSpace(visibilityBody(printed)))); visibility {
	case Public, Private, Internal:
		return visibility, nil
	default:
		return "", fmt.Errorf("gh api repos/%s answered no visibility GitHub reports", maskPath(slug))
	}
}

// githubOrigin is the scheme and host of the read without REST: the repository's URL begins with
// it, and git names it in the line that says GitHub asked for a credential (lsRemoteRefused).
const githubOrigin = "https://github.com"

// lsRemoteVisibility is the visibility read Guard.Visibility makes when GitHub refused gh's read
// for a spent rate limit that the guard does not wait for: `git ls-remote` of the repository's
// HTTPS URL, made with no credential (Guard.lsRemote). That read goes through git's smart HTTP
// protocol, which GitHub does not count against the REST rate limit, so it answers while the
// installation's REST budget is spent: pushes refused for that limit have met `git ls-remote
// origin` answering at the same moments. GitHub's GraphQL API would be no second way, since its
// budget is the installation's as well, and the same load spends it.
//
// A read that answers neither public nor private leaves the visibility unknown, and the call is
// refused as it was before this read was made, naming the spent limit and its reset, with what
// git said after them.
func (g *Guard) lsRemoteVisibility(ctx context.Context, slug string, limited *rateLimitError) (Visibility, error) {
	visibility, err := g.lsRemote(ctx, githubOrigin, slug)
	if err != nil {
		return "", fmt.Errorf("%w, and the read without REST could not answer: %w", limited, err)
	}
	return visibility, nil
}

// lsRemote is what `git ls-remote <origin>/<slug>.git HEAD` says of the repository's
// visibility, read with no credential: public where git exits 0, listing the repository's HEAD
// (an empty repository lists nothing and exits 0 all the same), private where the server asks for
// a credential (lsRemoteRefused), and an error for anything else. origin is a parameter so that a
// test can point the read at a server of its own; the guard's one caller passes githubOrigin.
//
// Private is the one answer that cannot be told from a repository that is not there: GitHub asks
// an anonymous reader for a credential in both cases, and git prints the same line for both
// (measured with git 2.47.3). Both are answered private. The write's own request, which carries
// the role's credential, then decides: a write to a repository that is not there fails on its own.
//
// Private skips the rule pass, which is the direction that can leak, so no credential may reach
// this read. GitHub refuses a bad credential for a public repository too, and git reports that
// refusal with the same line as an anonymous read of a private one: measured with git 2.47.3, an
// http.extraHeader carrying an invented token made ls-remote of a public repository print
// lsRemoteRefused's line. A pod hands its git a credential helper in GIT_CONFIG_KEY_<n>
// variables, and a repository's own configuration can hold an extra header or a URL rewrite that
// carries a token. So this git reads no configuration but the command line's:
//
//   - of the caller's environment it keeps no GIT_ variable but three that carry no credential
//     (lsRemoteInherits), so no GIT_CONFIG_COUNT block, GIT_CONFIG_PARAMETERS or GIT_DIR reaches
//     it;
//   - GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL=/dev/null take away the system file and the
//     user's;
//   - it runs in /, a directory only root can write, so it finds no repository and no
//     repository's configuration;
//   - `-c credential.helper=` empties the helper list again on the command line, after anything
//     a wrapper of the pod's git puts in front of it;
//   - the guard's prompt settings (promptsNothing) leave git's askpass and its terminal prompt
//     nothing to answer with, and the URL holds no user and no token.
//
// A ~/.netrc is left alone. git's libcurl sends a netrc credential only to a server that has
// asked for one, and GitHub asks only for a repository that is not public: such a repository
// reads private, or public where the netrc credential can read it, which only scans more
// (measured with git 2.47.3: a ~/.netrc holding an invented token left ls-remote of a public
// repository answering).
//
// LC_ALL=C keeps git's messages in the English lsRemoteRefused reads. The read has at most
// rateLimitWaitBound of the caller's bound, which is as long as the guard would have waited for a
// near reset, so a server that never answers costs the call no more than that wait.
func (g *Guard) lsRemote(ctx context.Context, origin, slug string) (Visibility, error) {
	ctx, cancel := context.WithTimeout(ctx, rateLimitWaitBound)
	defer cancel()
	status, printed, err := g.run(ctx, child{
		argv:    gitArgv("-c", "credential.helper=", "ls-remote", origin+"/"+slug+".git", "HEAD"),
		dir:     "/",
		inherit: lsRemoteInherits,
		env:     []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"},
	})
	if err != nil {
		return "", err
	}
	switch {
	case status == 0:
		return Public, nil
	case lsRemoteRefused(origin, status, printed):
		return Private, nil
	default:
		masked := maskPath(slug)
		return "", fmt.Errorf("git ls-remote of %s exited %d: %s", masked, status, strings.ReplaceAll(strings.TrimSpace(printed), slug, masked))
	}
}

// lsRemoteInherits is which names of the caller's environment the read with no credential keeps
// (Guard.lsRemote): every name that is not git's own, and of git's own the three that say which
// transports git may use and which certificates it trusts, none of which can carry a credential.
// The proxy variables libcurl reads (HTTPS_PROXY and the rest) are not git's and stay, so a pod
// that reaches GitHub through a proxy reaches it the same way here.
func lsRemoteInherits(name string) bool {
	switch name {
	case "GIT_ALLOW_PROTOCOL", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH":
		return true
	}
	return !strings.HasPrefix(name, "GIT_")
}

// lsRemoteRefused is whether git's exit status and output are its account of a server that asked
// for a credential the read had none to give. Asked on a 401, git runs its askpass, which answers
// nothing (promptsNothing), finds its terminal prompt off, and exits 128 with
//
//	fatal: could not read Username for 'https://github.com': terminal prompts disabled
//
// That line, whole and naming origin, is the one read as private: it is what git prints when
// GitHub answers an anonymous read of a private repository (measured with git 2.47.3, and with
// the git CI runs in TestLsRemoteReadsWhatARealGitPrints). Three near misses are read as no
// answer, because each can come from a public repository:
//
//   - "Authentication failed for …": git sent a credential and the server refused it, which
//     GitHub does for a public repository too. lsRemote sends none, so this means a credential
//     reached the request some way it did not close, and the answer says nothing of the
//     repository.
//   - "The requested URL returned error: 403": a 403 asks for no credential. It is a refusal a
//     credential would not change, and a proxy that refuses the request answers one whatever the
//     repository's visibility.
//   - The line naming another origin, or the line inside a "remote: " line: the first is git
//     asking for some other server's credential, such as a proxy's, and the second is text the
//     server wrote.
//
// Anything else leaves the visibility unknown, and the call is refused as it was before.
func lsRemoteRefused(origin string, status int, printed string) bool {
	if status != 128 {
		return false
	}
	want := "fatal: could not read Username for '" + origin + "': terminal prompts disabled"
	for line := range strings.Lines(printed) {
		if strings.TrimRight(line, "\r\n") == want {
			return true
		}
	}
	return false
}

type rateLimitError struct{ reset time.Time }

func (e *rateLimitError) Error() string { return e.refusal().Error() }

func (e *rateLimitError) refusal() error {
	if e.reset.IsZero() {
		return errors.New("the installation's rate limit is spent and GitHub gave no reset time")
	}
	return fmt.Errorf("the installation's rate limit is spent until %s UTC", e.reset.UTC().Format(time.RFC3339))
}

func rateLimitFrom(printed string) *rateLimitError {
	status, headers := responseHeaders(printed)
	lower := strings.ToLower(printed)
	if !((status == 403 || status == 429) && headers["x-ratelimit-remaining"] == "0") &&
		!strings.Contains(lower, "secondary rate limit") && !strings.Contains(lower, "secondary rate-limit") {
		return nil
	}
	reset, _ := strconv.ParseInt(headers["x-ratelimit-reset"], 10, 64)
	if reset == 0 {
		return &rateLimitError{}
	}
	return &rateLimitError{reset: time.Unix(reset, 0).UTC()}
}

func responseHeaders(printed string) (int, map[string]string) {
	lines := strings.Split(strings.ReplaceAll(printed, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "HTTP/") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, nil
		}
		status, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, nil
		}
		headers := map[string]string{}
		for _, line := range lines[i+1:] {
			if line == "" {
				break
			}
			name, value, found := strings.Cut(line, ":")
			if found {
				headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
			}
		}
		return status, headers
	}
	return 0, map[string]string{}
}

func visibilityBody(printed string) string {
	lines := strings.Split(strings.ReplaceAll(printed, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "HTTP/") {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "" {
				return strings.Join(lines[j+1:], "\n")
			}
		}
		break
	}
	return printed
}

// RepoFromURL is the "owner/name" a git remote URL names on github.com, and "" for a URL that
// names no repository there: another host, a local path, a remote an alias stands for. A caller
// that cannot name the destination gets a scan it may not have needed, never a scan it needed and
// did not get (Guard.scanThrough).
//
// A URL whose two names are no repository GitHub serves names none here either: the guard reads
// no visibility for a slug ghrepo.Parse refuses (Guard.scanThrough), so a URL that answered one
// would refuse the push over its remote's spelling rather than scan it. Answering "" scans the
// push instead, which is this function's own fail-closed direction.
func RepoFromURL(url string) string {
	rest, ok := afterHost(url)
	if !ok {
		return ""
	}
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	repo, err := ghrepo.Parse("", rest)
	if err != nil {
		return ""
	}
	return repo.String()
}

// afterHost is what a remote URL holds after github.com, in the three shapes a remote is written
// in: a URL with a scheme (https://github.com/owner/name), scp syntax (git@github.com:owner/name)
// and the same two with a user in front.
func afterHost(url string) (string, bool) {
	if scheme := strings.Index(url, "://"); scheme >= 0 {
		rest := url[scheme+len("://"):]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, path, ok := strings.Cut(rest, "/")
		if !ok {
			return "", false
		}
		return path, isGitHub(host)
	}
	host, path, ok := strings.Cut(url, ":")
	if !ok {
		return "", false
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	return path, isGitHub(host)
}

// isGitHub is whether host is github.com, port and case aside.
func isGitHub(host string) bool {
	if portless, _, ok := strings.Cut(host, ":"); ok {
		host = portless
	}
	return strings.EqualFold(host, "github.com")
}
