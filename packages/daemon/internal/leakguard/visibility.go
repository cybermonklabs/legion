package leakguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/programbound"
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

// VisibilitySource says where a guard's answer came from: GitHub's REST API through gh, the role's
// kept answer, GitHub's GraphQL API, git's ls-remote, or GitHub's REST API through curl. GraphQL
// and ls-remote are the reads the guard makes when the REST read is refused for a spent rate limit
// it does not wait for (Guard.Visibility). curl and ls-remote are the reads it makes when gh's REST
// read failed on its connection to GitHub (Guard.readWithoutGh); where curl meets a spent core
// limit there, the GraphQL read through gh follows, as it follows gh's (readWithoutREST).
// ls-remote is also the one read it makes when `legion gh` refuses the REST read because the
// project's gh bundle holds no directory for the repository's owner (Guard.readUnservedOwner).
type VisibilitySource string

const (
	VisibilityFromGitHub   VisibilitySource = "github"
	VisibilityFromCache    VisibilitySource = "kept"
	VisibilityFromGraphQL  VisibilitySource = "graphql"
	VisibilityFromLsRemote VisibilitySource = "ls-remote"
	VisibilityFromCurl     VisibilitySource = "rest-curl"
)

// Visibility is what GitHub reports the repository slug ("owner/name") as. Public answers stay
// for the role's pod life. Private and internal answers are read each time, so a repository made
// public cannot skip a rule pass.
//
// The REST read comes first (readVisibility). A read GitHub refuses for a rate limit, read with
// internal/githubrest's model (rateLimitFrom), is read once more after the wait GitHub asks for,
// where waitFits allows that wait. A limit still spent after it, and one not waited for, is read
// another way (readWithoutREST). A gh read that failed on its connection to GitHub, with no answer
// from GitHub (ghTransportFailure), is read without gh (readWithoutGh). A read `legion gh` refuses
// because the project's gh bundle holds no directory for the repository's owner
// (Guard.unservedOwner) is read with no credential (readUnservedOwner). A read GitHub answered 404
// (notFoundError) is read no other way. The answer is kept as gh's is.
//
// The header-bearing read is:
//
//	gh api --hostname github.com repos/<slug> --include --jq .visibility
//
// run with PUBLIC_LEAK_CHECK_VISIBILITY_READ=<slug> in its environment. A pod's gh is `legion
// gh`, which calls this guard, so this read must not be mistaken for the shared check's callback
// or it would recurse. The callback's exact argv is separate (VisibilityReadArgs): it has no
// `--include`, while this reader needs the headers to detect a near rate limit. `legion gh`
// answers the callback by the same rule (FreshVisibility). The caller reaches this through
// Guard.scanThrough, which lets no slug the exemption would refuse get this far.
func (g *Guard) Visibility(ctx context.Context, slug string) (Visibility, error) {
	return g.visibility(ctx, slug, true)
}

// FreshVisibility is Visibility without the kept answers: it neither trusts a kept public nor
// keeps one. `legion gh` answers the shared public leak check's own read with it. The check keeps
// answers of its own, for a time it sets, and asks some of them fresh: it reads every other remote
// of a push fresh, and counts the commits of one it reads public as held, which it does not scan.
// A kept public, which never expires, would answer that read for a repository made private since,
// and its commits would reach a public target unscanned. For that read a false public is a leak.
func (g *Guard) FreshVisibility(ctx context.Context, slug string) (Visibility, error) {
	return g.visibility(ctx, slug, false)
}

// visibility is Visibility, trusting and keeping public answers where kept says so.
func (g *Guard) visibility(ctx context.Context, slug string, kept bool) (Visibility, error) {
	if kept {
		if visibility, ok := g.keptVisibility(slug); ok {
			g.reportVisibility(VisibilityFromCache)
			return visibility, nil
		}
	}
	visibility, err := g.readVisibility(ctx, slug)
	var limited *rateLimitError
	if errors.As(err, &limited) {
		if wait, ok := g.waitFits(ctx, limited); ok {
			if err := g.clock().Sleep(ctx, wait); err != nil {
				return "", err
			}
			visibility, err = g.readVisibility(ctx, slug)
		}
	}
	source := VisibilityFromGitHub
	var unreached *transportError
	var unserved *unservedOwnerError
	switch {
	case errors.As(err, &limited):
		visibility, source, err = g.readWithoutREST(ctx, slug, limited)
	case errors.As(err, &unreached):
		visibility, source, err = g.readWithoutGh(ctx, slug, unreached)
	case errors.As(err, &unserved):
		visibility, source, err = g.readUnservedOwner(ctx, slug, unserved)
	}
	if err != nil {
		return "", err
	}
	if kept {
		g.keepVisibility(slug, visibility)
	}
	g.reportVisibility(source)
	return visibility, nil
}

// waitFits is how long the guard waits before it makes the REST read again after GitHub refused
// it for limited, and whether it waits at all. For a spent core limit the wait is until its reset.
// For a secondary limit the guard waits only for a Retry-After: without one it reads the visibility
// with ls-remote at once, which makes no API request, so it does not wait for the reset GitHub's
// documentation asks a caller with no requests left to wait for before its next API request. A
// wait is made only where it is no more than rateLimitWaitBound, and where it and then
// lsRemoteBound end before ctx's deadline, where ctx has one: a wait that leaves the reads after
// it no room only makes the call refuse later. lsRemoteBound is the cap of the longest one read
// that can follow the wait, and it is more than the caps of the retried REST read (ghReadBound)
// and the curl read (curlBound) together, so each read after the wait has room to start. The
// reads after the wait can still take longer than lsRemoteBound in turn: the retried REST read,
// then GraphQL and ls-remote after a rate limit, or curl and ls-remote after a failure on gh's
// connection. Each stops at ctx's deadline, and the call is then refused. A core limit with no
// reset, or one already past, and a secondary limit with no Retry-After are not waited for.
func (g *Guard) waitFits(ctx context.Context, limited *rateLimitError) (time.Duration, bool) {
	var wait time.Duration
	switch {
	case limited.secondary && limited.retry > 0:
		wait = limited.retry
	case !limited.secondary && !limited.reset.IsZero():
		wait = limited.reset.Sub(g.clock().Now())
	default:
		return 0, false
	}
	if wait < 0 || wait > rateLimitWaitBound {
		return 0, false
	}
	if deadline, ok := ctx.Deadline(); ok && wait+lsRemoteBound >= time.Until(deadline) {
		return 0, false
	}
	return wait, true
}

// readWithoutREST reads slug's visibility after GitHub refused the REST read for limited, where
// the guard did not wait or was refused again after its wait. A core limit is read from GitHub's
// GraphQL API (Guard.graphQLVisibility) with the REST read's token, whose GraphQL budget is
// separate from the core one, and its public, private or internal is taken as gh's is. A
// secondary limit is not, because GraphQL requests count against the same secondary limit. For a
// secondary limit, and where GraphQL refuses the read for a rate limit of its own, the visibility
// is read with git's ls-remote and no credential (Guard.lsRemote), which answers public and
// nothing else. Every other failure, an unauthenticated refusal of ls-remote included, refuses
// the call, naming the spent limit and what the read said.
func (g *Guard) readWithoutREST(ctx context.Context, slug string, limited *rateLimitError) (Visibility, VisibilitySource, error) {
	also := ""
	if !limited.secondary {
		visibility, err := g.graphQLVisibility(ctx, slug)
		var graphQLLimited *rateLimitError
		switch {
		case err == nil:
			return visibility, VisibilityFromGraphQL, nil
		case !errors.As(err, &graphQLLimited):
			return "", "", fmt.Errorf("%w, and GraphQL could not answer: %w", limited, err)
		}
		also = "; GraphQL's rate limit is spent too"
	}
	visibility, err := g.lsRemote(ctx, githubOrigin, slug)
	if err != nil {
		return "", "", fmt.Errorf("%w%s, and git ls-remote could not answer: %w", limited, also, err)
	}
	return visibility, VisibilityFromLsRemote, nil
}

// readUnservedOwner reads slug's visibility after `legion gh` refused the REST read because the
// project's gh bundle holds no directory for slug's owner (unserved): `legion gh` refuses every
// call naming that owner, so no read through gh can answer, GraphQL's no more than REST's. The
// pod's git credential helper can hold a token for such a repository, the token of the served
// repository a fork was made from (cmd/legion's forkUpstreamDir), but it answers git alone and
// never this read. An unauthenticated REST read is no way round that either: GitHub allows 60 of
// those an hour to an address, shared by everything that reads GitHub from it. The read is git's
// ls-remote with no credential (Guard.lsRemote), which makes no API request and answers public
// and nothing else: its exit 0 is the evidence that anyone can read the repository (an empty
// repository lists no ref and exits 0 all the same). Every other outcome, the 401 GitHub answers
// an anonymous read of a private repository and of a name no repository has with included,
// refuses the call, naming the refusal of `legion gh` and what git said.
func (g *Guard) readUnservedOwner(ctx context.Context, slug string, unserved *unservedOwnerError) (Visibility, VisibilitySource, error) {
	visibility, err := g.lsRemote(ctx, githubOrigin, slug)
	if err != nil {
		return "", "", fmt.Errorf("%w, and git ls-remote with no credential could not answer: %w", unserved, err)
	}
	return visibility, VisibilityFromLsRemote, nil
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
// name and the visibility of the repository its two variables name. The name is what the answer is
// checked against (Guard.graphQLVisibility).
const visibilityQuery = "query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { nameWithOwner visibility } }"

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

// ghReadEnv is what each of the guard's gh reads of slug carries beyond the guard's own names:
// VisibilityReadVar, which `legion gh` lets exactly these reads through by; GH_HOST, which names
// the host for a gh that has no configuration of its own to read it from, beside the --hostname
// the argv already carries; and NO_COLOR=1 with CLICOLOR_FORCE=0, which keep gh's output plain. A
// caller's CLICOLOR_FORCE=1 makes gh 2.98.0 colour the --include headers and the JSON body, and
// neither read could then be read.
func ghReadEnv(slug string) []string {
	return []string{VisibilityReadVar + "=" + slug, "GH_HOST=github.com", "NO_COLOR=1", "CLICOLOR_FORCE=0"}
}

// ghVisibility is the gh read itself, the guard's answer where a caller named no reader of its
// own. gh runs under a bound of the guard's own (ghReadContext), which leaves part of the caller's
// bound to the read through curl. A rate limit is a *rateLimitError. A failure on gh's connection
// to GitHub with no answer from GitHub is a *transportError: one gh reports (ghTransportFailure),
// or a stop by the guard's own bound before GitHub sent a status line (ghStoppedByItsBound). A gh
// that exited non-zero after GitHub's status line 404, and was no rate limit, is a *notFoundError.
// A gh that the caller's bound stopped is the *stoppedError run answers, since no bound is left for
// another read.
func (g *Guard) ghVisibility(ctx context.Context, slug string) (Visibility, error) {
	bounded, cancel := ghReadContext(ctx)
	defer cancel()
	status, printed, err := g.run(bounded, child{
		argv: append([]string{"gh"}, VisibilityReadWithHeadersArgs(slug)...),
		env:  ghReadEnv(slug),
	})
	if err != nil {
		if unreached := ghStoppedByItsBound(ctx, err, printed); unreached != nil {
			return "", unreached
		}
		return "", procgroup.Said(err, printed)
	}
	if limited := rateLimitFrom(printed); limited != nil {
		return "", limited
	}
	if unreached := ghTransportFailure(status, printed, slug); unreached != nil {
		return "", unreached
	}
	if status != 0 {
		if unserved := g.unservedOwner(status, printed, slug); unserved != nil {
			return "", unserved
		}
		if httpStatus, _ := responseHeaders(printed); httpStatus == http.StatusNotFound {
			return "", &notFoundError{slug: slug}
		}
		return "", fmt.Errorf("gh api repos/%s exited %d: %s", shownText(slug), status, strings.TrimSpace(printed))
	}
	switch visibility := Visibility(strings.ToLower(strings.TrimSpace(visibilityBody(printed)))); visibility {
	case Public, Private, Internal:
		return visibility, nil
	default:
		return "", fmt.Errorf("gh api repos/%s answered no visibility GitHub reports", shownText(slug))
	}
}

// OwnerNotServedError is the refusal `legion gh` makes of a call naming a repository of Owner, an
// owner the project's gh bundle under OwnersDir holds no directory for (ghroute's ownerDir): the
// role's GitHub App is not installed on Owner, so no directory of the bundle is Owner's own.
// `legion gh` prints it after `legion gh: `, alone, and exits 2. It is exported because the guard
// reads exactly that answer to its REST read as such an owner (Guard.unservedOwner), and reads
// the repository's visibility with no credential instead: words written from a second copy could
// part from the ones `legion gh` prints, and a write to another owner's public repository would
// again be refused before it was scanned.
type OwnerNotServedError struct {
	Owner     string
	OwnersDir string
}

func (e *OwnerNotServedError) Error() string {
	return fmt.Sprintf("the role's GitHub App is not installed on %q: %s holds no gh directory for it", e.Owner, e.OwnersDir)
}

// ownerNotServedLine is the one line `legion gh` prints for its refusal of a repository of owner
// (OwnerNotServedError), without its newline: the line the guard matches (Guard.unservedOwner)
// and its refusal quotes (unservedOwnerError).
func ownerNotServedLine(owner, ownersDir string) string {
	return "legion gh: " + (&OwnerNotServedError{Owner: owner, OwnersDir: ownersDir}).Error()
}

// unservedOwner is gh's answer to the guard's REST read of slug, its exit status and everything it
// printed, read as the refusal `legion gh` makes of a repository whose owner the project's gh
// bundle holds no directory for (OwnerNotServedError), and nil for any other answer. That refusal
// is exit 2, runGh's status for a call it refuses before gh runs, and one line with nothing
// before or after it (ownerNotServedLine): `legion gh: ` and OwnerNotServedError's words for
// slug's owner, lowercased as `legion gh` names it, and for the bundle LEGION_GH_OWNERS_DIR names
// in the guard's environment, which is the environment gh ran with. Anything else says nothing of
// the owner and stays the failure it was: another refusal of `legion gh`, these words for another
// owner or another bundle, these words beside other words in one line (the refusal of a
// configured rename whose new owner the bundle holds no directory for, ghroute.DirFor), these
// words beside other output, and these words under another exit status.
func (g *Guard) unservedOwner(status int, printed, slug string) *unservedOwnerError {
	if status != 2 {
		return nil
	}
	ownersDir := value(g.base(), "LEGION_GH_OWNERS_DIR")
	if ownersDir == "" {
		return nil
	}
	owner, _, _ := strings.Cut(slug, "/")
	owner = strings.ToLower(owner)
	if printed != ownerNotServedLine(owner, ownersDir)+"\n" {
		return nil
	}
	return &unservedOwnerError{slug: slug, owner: owner, ownersDir: ownersDir}
}

// unservedOwnerError is the guard's REST read of slug that `legion gh` refused because the gh
// bundle under ownersDir holds no directory for slug's owner, owner as `legion gh` names it
// (Guard.unservedOwner). The refusal says nothing of the repository's visibility, which the guard
// reads with no credential instead (Guard.readUnservedOwner). It says what `legion gh` printed,
// the repository and its owner as written.
type unservedOwnerError struct {
	slug, owner, ownersDir string
}

func (e *unservedOwnerError) Error() string {
	return fmt.Sprintf("gh api repos/%s exited 2: %s", shownText(e.slug), ownerNotServedLine(e.owner, e.ownersDir))
}

// notFoundError is the guard's REST read of slug that GitHub answered 404, through gh
// (Guard.ghVisibility) or through curl (curlAnswer): no repository of that name exists, or the
// read's credential cannot see it. GitHub answered, so no other read follows it, and a write it
// refuses is a *NotFound (Guard.scanThrough). It says what GitHub answered, the repository as
// shownText shows it.
type notFoundError struct {
	slug string
}

func (e *notFoundError) Error() string {
	return notFoundText(shownText(e.slug))
}

// graphQLVisibility reads slug's visibility from GitHub's GraphQL API through gh, as the same
// installation as the REST read and with the same variables (ghReadEnv), under the caller's
// bound, and answers what graphQLAnswer reads of it. `legion gh` runs it with the token of the
// repository VisibilityReadVar names, or with the caller's GH_TOKEN or GITHUB_TOKEN where one is
// set, as it runs the REST read (its ghVisibilityRead). GraphQL's budget is separate from the
// core REST one: at 2026-11-15T09:34:29Z `gh api rate_limit` showed installation 171972716 with
// 12,221 of its 12,500 GraphQL points left, and GraphQL reads have answered while core reads were
// refused. A gh that the caller's bound stops refuses the call: no bound is left for another
// read.
func (g *Guard) graphQLVisibility(ctx context.Context, slug string) (Visibility, error) {
	var stderr bytes.Buffer
	status, printed, err := g.run(ctx, child{
		argv:   append([]string{"gh"}, VisibilityGraphQLArgs(slug)...),
		env:    ghReadEnv(slug),
		stderr: &stderr,
	})
	if err != nil {
		return "", procgroup.Said(err, visibilityBody(printed)+"\n"+stderr.String())
	}
	return graphQLAnswer(status, printed, stderr.String(), slug)
}

// graphQLAnswer is what the guard's GraphQL read of slug answered: status is gh's exit status,
// printed its standard output (the status line, the headers and the body, as --include prints
// them) and stderr its standard error, which only a refusal quotes.
//
// A wrong answer can leak either way: a wrong private or internal skips the rule pass for a public
// repository, and a wrong public widens what the shared check's fresh read of another remote
// counts as held (FreshVisibility). So every way this read can be wrong refuses the call instead:
//
//   - the body must be one whole JSON document, with nothing after it but space;
//   - errors refuse, unless every one of them is a rate limit (type RATE_LIMITED, which GitHub can
//     send with the status 200, or a message naming the secondary limit,
//     githubrest.NamesSecondaryLimit), which is a *rateLimitError that sends the read on to
//     ls-remote; any other error refuses, beside rate-limit errors or beside data, NOT_FOUND among
//     them (which is also what an installation that cannot see a private repository is told);
//   - an error response that githubrest.Limit reads as a rate limit is a *rateLimitError too;
//   - gh must have printed a status line, as --include always does, and it must say 200; and gh
//     must have exited 0;
//   - the repository must be there, and its nameWithOwner must be slug, case aside: GitHub can
//     resolve a renamed or moved repository's old name to the repository under its new one,
//     which is not the repository the push names;
//   - and its visibility must be exactly PUBLIC, PRIVATE or INTERNAL.
func graphQLAnswer(status int, printed, stderr, slug string) (Visibility, error) {
	body := visibilityBody(printed)
	var answer struct {
		Data *struct {
			Repository *struct {
				NameWithOwner string `json:"nameWithOwner"`
				Visibility    string `json:"visibility"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
		Message string `json:"message"`
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	whole := decoder.Decode(&answer) == nil
	if whole {
		_, err := decoder.Token()
		whole = errors.Is(err, io.EOF)
	}
	if !whole {
		return "", fmt.Errorf("gh api graphql for %s exited %d without one whole GraphQL answer: %s", shownText(slug), status, said(body, stderr))
	}
	httpStatus, headers := responseHeaders(printed)
	if len(answer.Errors) > 0 {
		limitedOnly := true
		errs := make([]string, len(answer.Errors))
		for i, e := range answer.Errors {
			limitedOnly = limitedOnly && (e.Type == "RATE_LIMITED" || githubrest.NamesSecondaryLimit(e.Message))
			errs[i] = e.Message
			if e.Type != "" {
				errs[i] = e.Type + ": " + errs[i]
			}
		}
		if limitedOnly {
			return "", &rateLimitError{}
		}
		return "", fmt.Errorf("GraphQL answered %s with %s", shownText(slug), strings.Join(errs, "; "))
	}
	if limited, _ := githubrest.Limit(gitHubResponse(httpStatus, headers), []byte(answer.Message)); limited {
		return "", &rateLimitError{}
	}
	switch {
	case httpStatus == 0:
		return "", fmt.Errorf("gh api graphql for %s printed no status line this reads: %s", shownText(slug), said(body, stderr))
	case httpStatus != 200:
		return "", fmt.Errorf("gh api graphql for %s answered the status %d: %s", shownText(slug), httpStatus, said(body, stderr))
	}
	if status != 0 {
		return "", fmt.Errorf("gh api graphql for %s exited %d: %s", shownText(slug), status, said(body, stderr))
	}
	if answer.Data == nil || answer.Data.Repository == nil {
		return "", fmt.Errorf("GraphQL answered no repository for %s: %s", shownText(slug), said(body, stderr))
	}
	if answered := answer.Data.Repository.NameWithOwner; !strings.EqualFold(answered, slug) {
		return "", fmt.Errorf("GraphQL answered for the repository %q, not %s", answered, shownText(slug))
	}
	switch answer.Data.Repository.Visibility {
	case "PUBLIC":
		return Public, nil
	case "PRIVATE":
		return Private, nil
	case "INTERNAL":
		return Internal, nil
	default:
		return "", fmt.Errorf("GraphQL answered %q as the visibility of %s, which is none GitHub defines", answer.Data.Repository.Visibility, shownText(slug))
	}
}

// said is what a read printed, as a refusal quotes it: the body of its answer and its standard
// error, each trimmed and joined by a space. The GraphQL read (graphQLAnswer), the curl read
// (curlAnswer), the token read for curl (Guard.ghToken) and a fork read curl got no answer to
// (CurlResponse.Unreached) quote it.
func said(body, stderr string) string {
	return strings.TrimSpace(strings.TrimSpace(body) + " " + strings.TrimSpace(stderr))
}

// gitHubResponse is the status line and the headers gh's --include output printed, as an answer
// internal/githubrest reads (githubrest.Limit): the model the daemon's own REST client reads
// GitHub's rate limits with, which the guard's two reads through gh share.
func gitHubResponse(status int, headers map[string]string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header, len(headers))}
	for name, value := range headers {
		response.Header.Set(name, value)
	}
	return response
}

// githubOrigin is the scheme and host of the read without a credential: the repository's URL
// begins with it.
const githubOrigin = "https://github.com"

// lsRemote is what `git ls-remote <origin>/<slug>.git HEAD`, read with no credential, says of the
// repository's visibility: public where git exits 0, listing the repository's HEAD (an empty
// repository lists nothing and exits 0 all the same), and an error for anything else. origin is a
// parameter so that a test can point the read at a server of its own; the guard's three callers
// (readWithoutREST, readWithoutGh, readUnservedOwner) pass githubOrigin. That read goes through
// git's smart HTTP protocol, which GitHub does not count against the REST rate limit: pushes
// refused for that limit have met `git ls-remote origin` answering at the same moments. It uses no
// Go TLS either: git's HTTPS is libcurl's.
//
// Public is the one answer because a refusal says nothing of the repository. GitHub answers an
// anonymous read of a private repository, and of a name no repository has, with a 401; it also
// answers one of a public repository with a 401 while it limits unusual traffic (GitHub's
// changelog of 2026-10-19, "Some Git over HTTPS may temporarily require authentication"); and git
// prints the same `could not read Username` line for each, as it does for a server's ERR packet
// that carries that line. So every exit but 0 refuses the call, and this read never answers
// private or internal, which would skip the rule pass for a public repository.
//
// Its public must be true too. For the guard's own read a wrong public only scans a private
// repository's push, but for the shared check's fresh read of another remote (FreshVisibility) a
// wrong public is the leak: the check counts that remote's commits as held and does not scan them.
// An anonymous read exits 0 only for a repository anyone can read, so no credential may reach this
// read: a credential that reads a private repository would exit 0 for it. This git reads no
// configuration but the command line's, and no credential file:
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
// The resets name the settings measured to put a credential or a header on this read, one key at
// a time. A setting they do not name can still make it refuse the call. A wrapper that rewrites
// the URL with its own url.<base>.insteadOf points the read at another repository, whose public
// would answer for this one; the image's git adds no such setting, and the rewrite needs a git
// wrapper of a host's own, the case `legion git --after` runs.
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
	ctx, cancel := context.WithTimeout(ctx, programbound.Of(&lsRemoteBound))
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
		return "", procgroup.Said(err, printed)
	}
	if status != 0 {
		return "", fmt.Errorf("git ls-remote of %s exited %d: %s", shownText(slug), status, strings.TrimSpace(printed))
	}
	return Public, nil
}

// lsRemoteBound is the longest the read with no credential runs (Guard.lsRemote): as long as the
// guard waits for a near reset, so a server that never answers costs no more than that wait would
// have. It is a variable so that a test can shorten it, and so that cmd/legion's tests, whose
// `legion gh` runs this read on a stand-in git, can give it their own deadline (programbound): a
// stand-in slower than 2 min is stopped here and the read refused.
var lsRemoteBound = rateLimitWaitBound

func init() {
	programbound.Register(&lsRemoteBound)
}

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

// rateLimitError is a read GitHub refused for a rate limit: whether it is GitHub's secondary
// limit (githubrest.BudgetSecondary); for the core limit, when it resets (X-RateLimit-Reset),
// zero where GitHub gave no reset; and for the secondary limit, the wait its Retry-After asks
// for, zero where it gave none. It keeps no reset for a secondary limit, since the guard waits
// only for a Retry-After there and otherwise reads with ls-remote, which makes no API request
// (Guard.waitFits).
type rateLimitError struct {
	secondary bool
	reset     time.Time
	retry     time.Duration
}

func (e *rateLimitError) Error() string {
	switch {
	case e.secondary && e.retry > 0:
		return fmt.Sprintf("GitHub's secondary rate limit refused the read and asks for a wait of %d s", wholeSeconds(e.retry))
	case e.secondary:
		return "GitHub's secondary rate limit refused the read"
	case e.reset.IsZero():
		return "the installation's rate limit is spent and GitHub gave no reset time"
	default:
		return fmt.Sprintf("the installation's rate limit is spent until %s UTC", e.reset.UTC().Format(time.RFC3339))
	}
}

// rateLimitFrom is the rate limit the REST read's --include output, printed, reports, and nil
// where it reports none, read from the error response alone with internal/githubrest's model
// (githubrest.Limit): its status line, its headers and its body's message, never gh's own line.
// A 429 is a limit, and so is a 403 with X-RateLimit-Remaining: 0, a Retry-After, or a message
// naming the secondary limit or GitHub's abuse detection mechanism. The limit is the secondary
// one where the message names it, whatever X-RateLimit-Remaining says, or where the response has a
// Retry-After or leaves requests, and the core limit otherwise.
func rateLimitFrom(printed string) *rateLimitError {
	status, headers := responseHeaders(printed)
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(strings.NewReader(visibilityBody(printed))).Decode(&body)
	limited, budget := githubrest.Limit(gitHubResponse(status, headers), []byte(body.Message))
	switch {
	case !limited:
		return nil
	case budget == githubrest.BudgetSecondary:
		seconds, _ := strconv.Atoi(headers["retry-after"])
		return &rateLimitError{secondary: true, retry: time.Duration(max(seconds, 0)) * time.Second}
	}
	if reset, _ := strconv.ParseInt(headers["x-ratelimit-reset"], 10, 64); reset != 0 {
		return &rateLimitError{reset: time.Unix(reset, 0).UTC()}
	}
	return &rateLimitError{}
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
// and the same two with a user in front. The host is read from the URL's authority alone, and
// only for one of git's network schemes (gitSchemes). The authority ends at the first "/", "?" or
// "#" after the scheme, as git's credential code (`strcspn(cp, "/?#")`) and curl end it, and a
// URL whose authority ends at a "?" or a "#" names no repository path here. scp syntax is read
// only where its ":" comes before any "/", which is how git tells it from a local path. So a
// path, a query or a fragment that holds "@github.com", on another host or on this machine, names
// no host here.
func afterHost(url string) (string, bool) {
	if scheme, rest, ok := strings.Cut(url, "://"); ok {
		end := strings.IndexAny(rest, "/?#")
		if end < 0 || rest[end] != '/' || !gitSchemes[strings.ToLower(scheme)] {
			return "", false
		}
		authority, path := rest[:end], rest[end+1:]
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			authority = authority[at+1:]
		}
		return path, isGitHub(authority)
	}
	host, path, ok := strings.Cut(url, ":")
	if !ok || strings.Contains(host, "/") {
		return "", false
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	return path, isGitHub(host)
}

// gitSchemes are the schemes of the URLs git reaches a host over the network with (git's
// urls.adoc, "GIT URLS"): a file URL, whatever host it names, is a path on this machine.
var gitSchemes = map[string]bool{"https": true, "http": true, "ssh": true, "git": true, "git+ssh": true, "ssh+git": true}

// isGitHub is whether host is github.com, port and case aside.
func isGitHub(host string) bool {
	if portless, _, ok := strings.Cut(host, ":"); ok {
		host = portless
	}
	return strings.EqualFold(host, "github.com")
}
