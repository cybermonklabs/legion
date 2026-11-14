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

// VisibilitySource says whether a guard's answer read GitHub or used the role's kept answer.
type VisibilitySource string

const (
	VisibilityFromGitHub VisibilitySource = "github"
	VisibilityFromCache  VisibilitySource = "kept"
)

// Visibility is what GitHub reports the repository slug ("owner/name") as. Public answers stay
// for the role's pod life. Private and internal answers are read each time, so a repository made
// public cannot skip a rule pass.
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
	if err != nil {
		var limited *rateLimitError
		if !errors.As(err, &limited) {
			return "", err
		}
		wait := limited.reset.Sub(g.clock().Now())
		if limited.reset.IsZero() || wait < 0 || wait > rateLimitWaitBound {
			return "", limited.refusal()
		}
		if err := g.clock().Sleep(ctx, wait); err != nil {
			return "", err
		}
		visibility, err = g.readVisibility(ctx, slug)
		if err != nil {
			if errors.As(err, &limited) {
				return "", limited.refusal()
			}
			return "", err
		}
	}
	g.keepVisibility(slug, visibility)
	g.reportVisibility(VisibilityFromGitHub)
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
	dir, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	fd, err := syscall.Openat(int(dir.Fd()), filepath.Base(path), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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
