package leakguard

import (
	"context"
	"fmt"
	"strings"
)

// Visibility is what GitHub reports the repository slug ("owner/name") as, cached for the life of
// this guard: a write makes several calls, and a repository does not change what it is between
// them.
//
// The read is the one the shared check itself makes:
//
//	gh api --hostname github.com repos/<slug> --jq .visibility
//
// run with PUBLIC_LEAK_CHECK_VISIBILITY_READ=<slug> in its environment. Both of those are copied
// from the check on purpose. A pod's gh is `legion gh`, which calls this guard, so a read that
// looked like any other gh call would call the guard again, and the guard again: the exemption
// `legion gh` makes for the check's own visibility read — that exact argv for that exact slug —
// covers this read too, because it is built from the same VisibilityReadVar and
// VisibilityReadArgs this read is.
func (g *Guard) Visibility(ctx context.Context, slug string) (Visibility, error) {
	g.mu.Lock()
	cached, ok := g.seen[slug]
	g.mu.Unlock()
	if ok {
		return cached, nil
	}
	read := g.opts.Visibility
	if read == nil {
		read = g.ghVisibility
	}
	visibility, err := read(ctx, slug)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.seen[slug] = visibility
	g.mu.Unlock()
	return visibility, nil
}

// VisibilityReadVar is the environment variable the shared public leak check's own visibility
// read carries, naming the repository it reads. It is exported with VisibilityReadArgs because
// `legion gh` has to let exactly this read through unchecked, and an exemption written from a
// second copy of these strings is one an edit here would silently part from: the two sides
// parted means a first write to another owner's repository recurses without end.
const VisibilityReadVar = "PUBLIC_LEAK_CHECK_VISIBILITY_READ"

// VisibilityReadArgs is the visibility read's argv for slug, gh's own name aside, which is the
// shape `legion gh`'s exemption matches a call against.
func VisibilityReadArgs(slug string) []string {
	return []string{"api", "--hostname", "github.com", "repos/" + slug, "--jq", ".visibility"}
}

// ghVisibility is the gh read itself, the guard's answer where a caller named no reader of its
// own. GH_HOST names the host for a gh that has no configuration of its own to read it from,
// beside the --hostname the argv already carries.
func (g *Guard) ghVisibility(ctx context.Context, slug string) (Visibility, error) {
	status, printed, err := g.run(ctx, child{
		argv: append([]string{"gh"}, VisibilityReadArgs(slug)...),
		env:  []string{VisibilityReadVar + "=" + slug, "GH_HOST=github.com"},
	})
	if err != nil {
		return "", err
	}
	if status != 0 {
		return "", fmt.Errorf("gh api repos/%s exited %d: %s", maskPath(slug), status, strings.TrimSpace(printed))
	}
	switch visibility := Visibility(strings.ToLower(strings.TrimSpace(printed))); visibility {
	case Public, Private, Internal:
		return visibility, nil
	default:
		return "", fmt.Errorf("gh api repos/%s answered no visibility GitHub reports", maskPath(slug))
	}
}

// RepoFromURL is the "owner/name" a git remote URL names on github.com, and "" for a URL that
// names no repository there: another host, a local path, a remote an alias stands for. A caller
// that cannot name the destination gets a scan it may not have needed, never a scan it needed and
// did not get (Guard.scanThrough).
func RepoFromURL(url string) string {
	rest, ok := afterHost(url)
	if !ok {
		return ""
	}
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return owner + "/" + name
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
