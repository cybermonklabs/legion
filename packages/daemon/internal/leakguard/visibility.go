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
// answer, GitHub's GraphQL API, or git's ls-remote. The last two are the reads the guard makes
// when the REST read is refused for a spent rate limit it does not wait for (Guard.Visibility).
type VisibilitySource string

const (
	VisibilityFromGitHub   VisibilitySource = "github"
	VisibilityFromCache    VisibilitySource = "kept"
	VisibilityFromGraphQL  VisibilitySource = "graphql"
	VisibilityFromLsRemote VisibilitySource = "ls-remote"
)

// Visibility is what GitHub reports the repository slug ("owner/name") as. Public answers stay
// for the role's pod life. Private and internal answers are read each time, so a repository made
// public cannot skip a rule pass.
//
// A read GitHub refuses for a spent rate limit is read once more after the reset, where that is
// at most rateLimitWaitBound away. The limit is the installation's core REST limit, or GitHub's
// secondary rate limit, which rateLimitFrom does not tell apart from it and which usually gives no
// reset. A limit still spent after that wait, and one whose reset is further away, already past
// or not given, is read a second way that costs no REST request: GitHub's GraphQL API
// (Guard.graphQLVisibility), as the same installation, whose budget is separate from the core
// one. Its answer of public, private or internal is taken as gh's is. Only where GraphQL refuses
// the read for a rate limit of its own is the visibility read a third way: git's ls-remote with no
// credential (Guard.lsRemote), which answers public and nothing else. Every other failure of
// either read, an unauthenticated refusal of ls-remote included, refuses the call, naming the
// spent limit and its reset and what the read said. The answer is kept as gh's is.
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
		source = VisibilityFromGraphQL
		visibility, err = g.graphQLVisibility(ctx, slug)
		var graphQLLimited *rateLimitError
		switch {
		case errors.As(err, &graphQLLimited):
			source = VisibilityFromLsRemote
			if visibility, err = g.lsRemote(ctx, githubOrigin, slug); err != nil {
				err = fmt.Errorf("%w; GraphQL's rate limit is spent too, and git ls-remote could not answer: %w", limited, err)
			}
		case err != nil:
			err = fmt.Errorf("%w, and GraphQL could not answer: %w", limited, err)
		}
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
// callback carries, naming the repository it reads; the guard's own two reads carry it too
// (VisibilityReadWithHeadersArgs, VisibilityGraphQLArgs). It is exported with those argv because
// `legion gh` has to let exactly these reads through unchecked. A read written from a second copy
// of these strings can silently part from this exemption, making a first write to another owner's
// repository recurse without end.
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

// visibilityQuery is the GraphQL query of the guard's GraphQL read (VisibilityGraphQLArgs): the
// visibility of the repository its two variables name.
const visibilityQuery = "query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { visibility } }"

// VisibilityGraphQLArgs is the guard's GraphQL read of slug's visibility, gh's own name aside: the
// second read Guard.Visibility makes when GitHub refuses the REST read for a spent rate limit
// (Guard.graphQLVisibility). It is exported beside VisibilityReadArgs because `legion gh` lets
// exactly this argv through unchecked, with VisibilityReadVar naming the same slug: checking it
// would call the guard again. The owner and the name go as raw string variables (-f), so a name
// of digits stays a string, and --include prints the status line and the headers, which say
// whether GitHub refused the read for a rate limit.
func VisibilityGraphQLArgs(slug string) []string {
	owner, name, _ := strings.Cut(slug, "/")
	return []string{"api", "--hostname", "github.com", "graphql", "--include",
		"-f", "query=" + visibilityQuery, "-f", "owner=" + owner, "-f", "name=" + name}
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

// graphQLVisibility reads slug's visibility from GitHub's GraphQL API through gh, as the same
// installation as the REST read and with the same VisibilityReadVar and GH_HOST
// (Guard.ghVisibility), under the caller's bound. GraphQL's budget is separate from the core REST
// one: at 2026-11-15T09:34:29Z `gh api rate_limit` showed installation 171972716 with 12,221 of
// its 12,500 GraphQL points left, and GraphQL reads have answered while core reads were refused.
//
// PUBLIC, PRIVATE and INTERNAL are the answer. A refusal for GraphQL's own rate limit is a
// *rateLimitError: the status 403 or 429 with no requests left, GitHub's secondary rate limit,
// or an error of type RATE_LIMITED, which GitHub can send with the status 200. Anything else is an
// error that Guard.Visibility refuses the call on, and never an answer: an error beside the data,
// such as NOT_FOUND for a repository GitHub could not resolve (which is also what an
// installation that cannot see a private repository is told), no repository, a visibility
// GraphQL does not define, a body that is no JSON, gh exiting non-zero, and a gh that the
// caller's bound stopped, after which no bound is left for another read.
func (g *Guard) graphQLVisibility(ctx context.Context, slug string) (Visibility, error) {
	status, printed, err := g.run(ctx, child{
		argv: append([]string{"gh"}, VisibilityGraphQLArgs(slug)...),
		env:  []string{VisibilityReadVar + "=" + slug, "GH_HOST=github.com"},
	})
	if err != nil {
		return "", err
	}
	if limited := rateLimitFrom(printed); limited != nil {
		return "", limited
	}
	masked := maskPath(slug)
	body := visibilityBody(printed)
	said := strings.ReplaceAll(strings.TrimSpace(body), slug, masked)
	var answer struct {
		Data *struct {
			Repository *struct {
				Visibility string `json:"visibility"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	// The first JSON value alone: gh prints its own error line after the body when GraphQL
	// answers errors.
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&answer); err != nil {
		return "", fmt.Errorf("gh api graphql for %s exited %d with no GraphQL answer: %s", masked, status, said)
	}
	if len(answer.Errors) > 0 {
		errs := make([]string, len(answer.Errors))
		for i, e := range answer.Errors {
			if e.Type == "RATE_LIMITED" {
				_, headers := responseHeaders(printed)
				return "", limitResettingAt(headers)
			}
			errs[i] = e.Type + ": " + strings.ReplaceAll(e.Message, slug, masked)
		}
		return "", fmt.Errorf("GraphQL answered %s with %s", masked, strings.Join(errs, "; "))
	}
	if status != 0 {
		return "", fmt.Errorf("gh api graphql for %s exited %d: %s", masked, status, said)
	}
	if answer.Data == nil || answer.Data.Repository == nil {
		return "", fmt.Errorf("GraphQL answered no repository for %s: %s", masked, said)
	}
	switch answer.Data.Repository.Visibility {
	case "PUBLIC":
		return Public, nil
	case "PRIVATE":
		return Private, nil
	case "INTERNAL":
		return Internal, nil
	default:
		return "", fmt.Errorf("GraphQL answered %q as the visibility of %s, which is none GitHub defines", answer.Data.Repository.Visibility, masked)
	}
}

// githubOrigin is the scheme and host of the read without a credential: the repository's URL
// begins with it.
const githubOrigin = "https://github.com"

// lsRemote is what `git ls-remote <origin>/<slug>.git HEAD`, read with no credential, says of the
// repository's visibility: public where git exits 0, listing the repository's HEAD (an empty
// repository lists nothing and exits 0 all the same), and an error for anything else. origin is a
// parameter so that a test can point the read at a server of its own; the guard's one caller
// passes githubOrigin. That read goes through git's smart HTTP protocol, which GitHub does not
// count against the REST rate limit: pushes refused for that limit have met `git ls-remote origin`
// answering at the same moments.
//
// Public is the one answer because a refusal says nothing of the repository. GitHub answers an
// anonymous read of a private repository, and of a name no repository has, with a 401; it also
// answers one of a public repository with a 401 while it limits unusual traffic (GitHub's
// changelog of 2026-10-19, "Some Git over HTTPS may temporarily require authentication"); and git
// prints the same `could not read Username` line for each, as it does for a server's ERR packet
// that carries that line. Public is also the safe answer: it runs the rule pass. So this read can
// never skip one, and every exit but 0 refuses the call.
//
// For the same reason no credential reaching this read can skip a rule pass: a credential that
// reads a private repository answers 0, and the push is scanned; a bad one is refused with a 401,
// even for a public repository. What follows spares a refusal the read did not need, or a scan of
// a private repository's push. This git reads no configuration but the command line's, and no
// credential file:
//
//   - of the caller's environment it keeps no GIT_ variable but three that carry no credential,
//     and none of the names libcurl finds a netrc file by (lsRemoteInherits), so no
//     GIT_CONFIG_COUNT block, GIT_CONFIG_PARAMETERS, GIT_DIR or NETRC reaches it;
//   - HOME is an empty directory made for this read and removed after it. git turns libcurl's
//     netrc lookup on, and libcurl reads the .netrc under HOME and sends its credential to a
//     server that asks for one: measured with git 2.47.3, a server a ~/.netrc named answered
//     git's request 401, and git sent the request again carrying that netrc credential;
//   - GIT_CONFIG_NOSYSTEM and GIT_CONFIG_GLOBAL=/dev/null take away the system file and the
//     user's, the second also the one under XDG_CONFIG_HOME, which the read keeps;
//   - it runs in /, a directory only root can write, so it finds no repository and no
//     repository's configuration;
//   - `-c credential.helper=` empties the helper list again on the command line, after anything
//     a wrapper of the pod's git puts in front of it;
//   - `-c http.extraHeader=`, `-c http.<origin>/<slug>.git.extraHeader=` and
//     `-c http.<origin>/<slug>.git//.extraHeader=` empty the extra headers again after one that a
//     wrapper's own `-c` added; outside the image, `legion git --after` runs the host's next git
//     on PATH, a wrapper of its own included. git keeps the setting whose URL matches the request
//     most closely and passes over a later one that matches less closely, so a generic reset
//     alone leaves a header set for the host. git matches http settings against the repository's
//     URL with a slash appended, and strips one trailing slash from a setting's URL, so a setting
//     for `<origin>/<slug>.git//` matches more closely than one for the URL itself. The `//`
//     reset matches as closely as any setting can, and it comes after the wrapper's, which wins a
//     tie. Measured with git 2.47.3 against a public repository on github.com, with a wrapper's
//     header carrying an invented token, after `-c credential.helper=`: a generic header made
//     ls-remote exit 128, and it exited 0 after the generic reset; a header for
//     https://github.com/ exited 128 after the generic reset too, and 0 after the reset for the
//     repository's URL; a header for that URL with `//` appended, also with the URL in capitals,
//     with :443, or with a letter %-escaped, exited 128 after both of those resets, and 0 after
//     the `//` reset;
//   - `-c http.proactiveAuth=none` and, last, `-c http.<origin>/<slug>.git//.proactiveAuth=none`
//     undo a wrapper's http.proactiveAuth, by the same rule of the closest URL. `basic` or `auto`
//     makes git ask for a credential before its first request, and with none to give it exits
//     128. Measured with git 2.47.3 against a local server serving a public repository: a
//     wrapper's setting for no URL, for the host, for the repository's URL or for that URL with
//     `//` appended exited 128; the generic reset cleared the first alone, and the `//` reset
//     after it cleared all four;
//   - the guard's prompt settings (promptsNothing) leave git's askpass and its terminal prompt
//     nothing to answer with, and the URL holds no user and no token.
//
// The resets name the settings measured to make an anonymous read of a public repository fail,
// one key at a time. A setting they do not name, or a wrapper that rewrites the URL with its own
// url.<base>.insteadOf and sets a header for the rewritten URL, can still make this read refuse
// the call; none can make it answer anything but public.
//
// LC_ALL=C keeps git's messages in the refusal in English.
//
// The read runs at most lsRemoteBound, and never past the caller's own bound. That cap is this
// read's own, not the call's: a call that waited out a near reset and was refused again has spent
// up to rateLimitWaitBound waiting before this read starts, so the two together can take about
// twice rateLimitWaitBound where the caller's bound allows that long.
func (g *Guard) lsRemote(ctx context.Context, origin, slug string) (Visibility, error) {
	home, err := os.MkdirTemp("", "legion-ls-remote-")
	if err != nil {
		return "", fmt.Errorf("the empty home for git ls-remote could not be made: %w", err)
	}
	defer os.RemoveAll(home)
	ctx, cancel := context.WithTimeout(ctx, lsRemoteBound)
	defer cancel()
	url := origin + "/" + slug + ".git"
	status, printed, err := g.run(ctx, child{
		argv: gitArgv("-c", "credential.helper=",
			"-c", "http.extraHeader=", "-c", "http."+url+".extraHeader=", "-c", "http."+url+"//.extraHeader=",
			"-c", "http.proactiveAuth=none", "-c", "http."+url+"//.proactiveAuth=none",
			"ls-remote", url, "HEAD"),
		dir:     "/",
		inherit: lsRemoteInherits,
		env:     []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"},
	})
	if err != nil {
		return "", err
	}
	if status != 0 {
		masked := maskPath(slug)
		return "", fmt.Errorf("git ls-remote of %s exited %d: %s", masked, status, strings.ReplaceAll(strings.TrimSpace(printed), slug, masked))
	}
	return Public, nil
}

// lsRemoteBound is the longest the read with no credential runs (Guard.lsRemote): as long as the
// guard waits for a near reset, so a server that never answers costs no more than that wait would
// have. It is a variable so that a test can shorten it.
var lsRemoteBound = rateLimitWaitBound

// lsRemoteInherits is which names of the caller's environment the read with no credential keeps
// (Guard.lsRemote). It keeps every name that is not git's own, and of git's own the three that say
// which transports git may use and which certificates it trusts, none of which can carry a
// credential. It drops the names libcurl finds a netrc file by: HOME, which the read sets to an
// empty directory of its own, NETRC, which names the file outright from curl 8.16, and
// USERPROFILE, libcurl's second home on Windows. The proxy variables libcurl reads (HTTPS_PROXY
// and the rest) stay, so a pod that reaches GitHub through a proxy reaches it the same way here.
func lsRemoteInherits(name string) bool {
	switch name {
	case "GIT_ALLOW_PROTOCOL", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH":
		return true
	case "HOME", "NETRC", "USERPROFILE":
		return false
	}
	return !strings.HasPrefix(name, "GIT_")
}

// rateLimitError is a read GitHub refused for a spent rate limit, and when the limit resets, zero
// where GitHub gave no reset.
type rateLimitError struct{ reset time.Time }

func (e *rateLimitError) Error() string {
	if e.reset.IsZero() {
		return "the installation's rate limit is spent and GitHub gave no reset time"
	}
	return fmt.Sprintf("the installation's rate limit is spent until %s UTC", e.reset.UTC().Format(time.RFC3339))
}

// rateLimitFrom is the spent limit gh's --include output reports, and nil where it reports none:
// the status 403 or 429 with no requests left, or GitHub's secondary rate limit by its text.
func rateLimitFrom(printed string) *rateLimitError {
	status, headers := responseHeaders(printed)
	lower := strings.ToLower(printed)
	if !((status == 403 || status == 429) && headers["x-ratelimit-remaining"] == "0") &&
		!strings.Contains(lower, "secondary rate limit") && !strings.Contains(lower, "secondary rate-limit") {
		return nil
	}
	return limitResettingAt(headers)
}

// limitResettingAt is a spent limit that resets when headers' X-RateLimit-Reset says, and one
// with no reset where they give none.
func limitResettingAt(headers map[string]string) *rateLimitError {
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
