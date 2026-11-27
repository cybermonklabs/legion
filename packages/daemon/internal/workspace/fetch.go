package workspace

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/httpoutage"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// isolatedGitConfig is the configuration Fetch's git reads besides its own and its command line's
// (isolatedGit): none. Not the system's, not the image user's; and git's `-c`,
// GIT_CONFIG_PARAMETERS, no process provisioning starts inherits (config.go).
var isolatedGitConfig = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// isolatedGit is the reusable git command prefix that adds packWindowConfig as `-c` pairs. Cache
// fetches, issue-clone fills, and the helpers that read or tend either repository use it. That git
// reads no system file (isolatedGitConfig), so the worker image's /etc/gitconfig does not reach it,
// its runner removes the role environment's pairs (execRunner.Run), and the repository cache's own
// configuration may hold none of the four (cacheConfigKeys). The runner's own pins
// (pinnedGitConfig) reach it too, but they reach every command any NewRunner starts, the daemon's
// host runners among them, so the four go on this command line instead. git exports the pairs to
// every git it starts in turn, the index-pack of a clone or a fetch among them. They come before
// everything a caller adds, and git takes the last value of a key, so a caller's own `-c` of one of
// them wins: the repository cache's repack keeps repackMemory's. Its capacity is its length, so an
// append to it copies rather than writing past the shared prefix.
var isolatedGit = func() []string {
	argv := []string{"git"}
	for _, pair := range packWindowConfig {
		argv = append(argv, "-c", pair[0]+"="+pair[1])
	}
	return slices.Clip(argv)
}()

// FetchRequest is a fetch from GitHub: the repository cache's (Fetch), or an issue pod's fill's
// (fetchIssuePod). It names the repository and, for Fetch alone, how to read the provisioning
// token, the directory its one-shot credential goes under, and Root, the cache's volume. Bound is
// the one deadline all of one fetch's transfers from GitHub share, counted from its first; zero is
// FetchTimeout, and the repository cache gives its first fill CacheFetchTimeout.
type FetchRequest struct {
	Repo ghrepo.Repository
	// Token answers the provisioning token as it stands when called, never a copy kept from an
	// earlier call: the file a pod mounts it at, which the daemon keeps fresh for the pod's whole
	// life, whatever its fetch has done.
	Token         func() (string, error)
	CredentialDir string
	Root          string
	Log           func(line string)
	Clock         Clock
	Bound         time.Duration
}

// CacheStream is the repository-cache input to one issue clone. The cache's bearer token is used
// only for /packs and /tar; /healthz remains public. Bound limits the verification of what /tar
// streamed, counted from the stream's end; zero takes FetchTimeout. The attempt on the cache's
// bucket holds BucketFetchTimeout of its own.
type CacheStream struct {
	Repo  ghrepo.Repository
	URL   string
	Token string
	Log   func(string)
	Clock Clock
	Bound time.Duration
}

func (s CacheStream) bound() time.Duration {
	if s.Bound > 0 {
		return s.Bound
	}
	return FetchTimeout
}

// StreamReport is where StreamCache filled a clone's history from and what that source brought:
// Source, the cache's bucket (SourceBucket) or its /tar (SourceTar), with why the bucket was not
// used (TarReason); from the bucket, each pack and index it downloaded and verified (Objects),
// their bytes and how long the manifest and the downloads took; from /tar, its length and how long
// it streamed (Bytes, Duration); and how long the history from either took to verify.
//
// On a failure StreamCache answers the zero report until it has chosen where the history comes
// from, and the report as it stood once it has (Source), so its caller can say which and why,
// whatever failed after.
type StreamReport struct {
	Source         string
	TarReason      string
	Objects        []BucketObject
	BucketBytes    int64
	BucketDuration time.Duration
	Bytes          int64
	Duration       time.Duration
	VerifyDuration time.Duration
}

// bound is the time each clocked stage of the fetch gets once it may start: Bound, or FetchTimeout.
func (r FetchRequest) bound() time.Duration {
	if r.Bound > 0 {
		return r.Bound
	}
	return FetchTimeout
}

// StreamCache waits for the narrowed repository cache (waitForCache), then fills gitDir with the
// cache's history and refs and verifies them, before the caller fetches its issue refs from
// GitHub: from the cache's bucket where the cache serves GET /packs and everything the bucket gave
// verifies (fromBucket), and otherwise from the cache's /tar, once whatever the bucket's attempt
// wrote is gone (fromCache). gitDir may already be the .git directory of the normal clone being
// made; when it is absent this function initializes the bare repository a cache test or caller
// needs. Either source leaves gitDir as /tar does: the cache's refs and its packs, each with its
// index, and nothing else of the cache's.
//
// A cache that is not ready within CacheWaitTimeout, that refuses the pod's token or names another
// repository (its /packs or its /tar), fails with ErrOutsideVolume, and so does a /tar the cache
// does not answer. No failure of the bucket's is StreamCache's: each is the reason /tar fills the
// clone instead.
func StreamCache(ctx context.Context, run Runner, stream CacheStream, gitDir string) (StreamReport, error) {
	if stream.Repo.IsZero() {
		return StreamReport{}, errors.New("workspace repository is required")
	}
	if stream.URL == "" {
		return StreamReport{}, OutsideVolume(errors.New("workspace repository cache URL is required"))
	}
	if stream.Token == "" {
		return StreamReport{}, OutsideVolume(errors.New("workspace repository cache token is required"))
	}
	if stream.Log == nil {
		return StreamReport{}, errors.New("workspace repository cache log is required")
	}
	if stream.Clock == nil {
		return StreamReport{}, errors.New("workspace repository cache clock is required")
	}
	if err := waitForCache(ctx, stream); err != nil {
		return StreamReport{}, OutsideVolume(err)
	}
	exists, err := pathExists(gitDir)
	if err != nil {
		return StreamReport{}, err
	}
	if !exists {
		if _, err := RunChecked(ctx, run, slices.Concat(isolatedGit, []string{"init", "--bare", "--quiet", gitDir}), isolatedGitConfig, filepath.Dir(gitDir)); err != nil {
			return StreamReport{}, err
		}
	}
	var report StreamReport
	if err := report.fromCache(ctx, run, stream, gitDir); err != nil {
		if report.Source == "" {
			return StreamReport{}, err
		}
		return report, err
	}
	return report, nil
}

// fromCache fills gitDir from the repository cache once it is ready: from its bucket where that
// attempt succeeds (fromBucket), and otherwise from its tar. Before /tar it removes from gitDir
// everything the bucket's attempt wrote there (heldGitDir.restore), so /tar starts from the git
// directory as StreamCache found it, and records why the bucket was not used (TarReason); a
// caller's context that ended is the fill's end, and no /tar starts. /tar is then asked afresh with
// the cache's token, unpacked (unpackCacheInto) and verified (verifyCache) within the stream's own
// bound, counted from the stream's end: the stream holds the low-speed bound rather than a clock,
// and the bucket's attempt held a bound of its own, so neither leaves the verification less than its
// whole bound. A pack the tar streamed without its index is indexed here. A refusal of the bucket's
// answer (a 401, another repository) is the fill's failure, as /tar's is.
func (r *StreamReport) fromCache(ctx context.Context, run Runner, stream CacheStream, gitDir string) error {
	held, err := holdGitDir(gitDir)
	if err != nil {
		return err
	}
	err = r.fromBucket(ctx, run, stream, gitDir)
	var failure *bucketFailure
	if !errors.As(err, &failure) {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("the fill ended as it filled the clone from the repository cache %s's bucket (%s): %w", stream.URL, failure.Error(), ctx.Err())
	}
	r.Source, r.TarReason = SourceTar, failure.Error()
	if err := held.restore(); err != nil {
		return fmt.Errorf("remove what the repository cache's bucket filled %s with: %w", gitDir, err)
	}
	started := stream.Clock.Now()
	streamed, err := unpackCacheInto(ctx, run, stream, gitDir)
	verifying := stream.Clock.Now()
	if err == nil {
		err = verifyCache(ctx, run, stream.Clock, gitDir, verifying.Add(stream.bound()), "cache stream's", stream.bound(), true)
	}
	if err != nil {
		return fmt.Errorf("fill the clone from the repository cache %s: %w", stream.URL, err)
	}
	r.Bytes, r.Duration, r.VerifyDuration = streamed, verifying.Sub(started), stream.Clock.Now().Sub(verifying)
	return nil
}

// heldGitDir is what a git directory held, before the bucket's attempt, of what that attempt may
// write there: HEAD's bytes, and every path under objects/pack and refs, and packed-refs.
type heldGitDir struct {
	gitDir string
	head   []byte
	paths  map[string]bool
}

// holdGitDir reads what gitDir holds of what the bucket's attempt may write (heldGitDir).
func holdGitDir(gitDir string) (heldGitDir, error) {
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return heldGitDir{}, err
	}
	held := heldGitDir{gitDir: gitDir, head: head, paths: map[string]bool{}}
	err = held.walk(func(path string, _ fs.DirEntry) error {
		held.paths[path] = true
		return nil
	})
	return held, err
}

// restore removes from the git directory each path under objects/pack and refs, and packed-refs,
// that it did not hold when holdGitDir read it, a directory with all it holds, and writes HEAD's
// bytes back: the git directory as it was before the bucket's attempt, for /tar to fill.
func (h heldGitDir) restore() error {
	var added []string
	if err := h.walk(func(path string, entry fs.DirEntry) error {
		if h.paths[path] {
			return nil
		}
		added = append(added, path)
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		return err
	}
	for _, path := range added {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(h.gitDir, "HEAD"), h.head, 0o644)
}

// walk calls visit for objects/pack, refs and packed-refs in the git directory and for everything
// under the first two, passing over those that are not there.
func (h heldGitDir) walk(visit func(path string, entry fs.DirEntry) error) error {
	for _, part := range []string{filepath.Join("objects", "pack"), "refs", "packed-refs"} {
		err := filepath.WalkDir(filepath.Join(h.gitDir, part), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return visit(path, entry)
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// CacheFetchTimeout is the repository cache's own Bound: its first fill takes the default branch's
// whole history from GitHub and indexes every object of it, which on 2026-10-26 took one
// deployment's cache pod (one CPU, gVisor) past FetchTimeout 289 times over 30 hours, each attempt
// killed at 30 minutes and begun again from nothing, so the cache was never ready
// (sjawhar/legion#2240).
const CacheFetchTimeout = 2 * time.Hour

// CacheRepositoryHeader names, on every answer the repository cache gives a request its token
// authorizes, the repository it caches (owner/name), which Fetch checks is its own before it takes
// anything the answer carries.
const CacheRepositoryHeader = "Legion-Repository"

// InitExitUsage is the exit status of `legion workspace-init provision` for a usage error, a flag
// or an argument it does not know. It is the usage exit of every legion command.
const InitExitUsage = 2

// InitExitOutsideVolume is the exit status of `legion workspace-init provision` for a failure whose
// cause is outside the issue's volume (ErrOutsideVolume). A new volume does not cure such a failure,
// so the runtime keeps a kept volume for it. Every other failure exits 1.
const InitExitOutsideVolume = 3

// ErrOutsideVolume is the cause of a failure outside the issue's volume. These failures wrap it
// (OutsideVolume):
//   - a refusal of `provision` before it touches the volume,
//   - a failed read of the provisioning token,
//   - the wait for the repository cache, and a request that the cache refuses or does not answer,
//   - a transfer or a ref listing from GitHub that failed as outsideVolumeFailure reads it.
var ErrOutsideVolume = errors.New("the cause is outside the issue's volume")

// OutsideVolume is err with ErrOutsideVolume as a cause too. Its text is err's own text, so a log
// or a test that reads the text reads no change. OutsideVolume(nil) is nil.
func OutsideVolume(err error) error {
	if err == nil {
		return nil
	}
	return outsideVolume{err}
}

// outsideVolume is an error that OutsideVolume returned.
type outsideVolume struct{ err error }

func (e outsideVolume) Error() string   { return e.err.Error() }
func (e outsideVolume) Unwrap() []error { return []error{e.err, ErrOutsideVolume} }

// Fetch fills the repository cache's bare clone with the provisioning token and no configuration
// but the one-shot credential's and the pins. The cache process (repocache) owns its volume and
// reaches GitHub as the tmux runtime's Source does, so the token's holders share one credential
// rule.
//
// A cache clone that already holds the repository has the delta fetched into it from GitHub, once
// the locks and partial packs a killed fetch left are gone (RemoveStaleCacheLocks). A cache clone
// in a shape Fetch does not write is removed and filled afresh. An absent cache clone is filled in a
// temporary sibling, its own configuration written first (cacheConfiguration: no garbage collection,
// and the fetch's objects kept as the pack they arrive in, which a fetch of fewer objects than git's
// default unpack limit would otherwise explode into loose objects the cache's pack-only stream
// leaves out), and renamed into place once everything that fills it succeeded, so the repository is
// whole and current, or absent. Both fetch GitHub's default branch alone, as GitHub's HEAD names
// it, with --no-tags: the first fill is that fetch into an empty bare repository, never a
// `git clone`, which asks GitHub for every annotated tag that points into the branch it brings
// (include-tag) even with --no-tags, so the cache would hold, and stream to every pod, tag objects
// no ref names.
//
// Each GitHub transfer gets FetchTimeout unless the request sets Bound (the repository cache's own
// first fill), and none starts past the one it is in. The repository cache's first fill may take
// the whole CacheFetchTimeout. The few local git commands around each transfer (reading refs and
// writing the cache's configuration) keep CommandTimeout and take moments. Every attempt reads the
// provisioning token anew as it starts (FetchRequest.Token, fetchCredential.renew), so it presents
// the token as its file stands then, not as it stood when the fetch began. A transfer from GitHub
// that fails transiently (transientCloneFailure) is run again after the next of fetchBackoff's
// waits, once per wait, its failure logged and, in the first fill, whatever a failed attempt left in
// the temporary sibling removed first, and no retry starts past the bound. A transfer still failing
// after the last wait, and every other failure, is the answer at once. Each attempt's stderr has
// every token the fetch has read replaced by *** as soon as the attempt returns, so nothing Fetch
// logs or returns carries one, wherever git echoed it.
//
// A failure whose cause is outside the issue's volume wraps ErrOutsideVolume, and keeps its text: a
// read of the provisioning token that failed, or a transfer or ref listing from GitHub that
// outsideVolumeFailure reads so, retried or not. Every other failure is the answer unchanged.
func Fetch(ctx context.Context, run Runner, request FetchRequest) error {
	cache, err := CacheRepository(request.Root, request.Repo)
	if err != nil {
		return err
	}
	if request.Token == nil {
		return errors.New("workspace fetch token is required")
	}
	token, err := request.Token()
	if err != nil {
		return OutsideVolume(err)
	}
	github := gitHubSource{token: func() (string, error) { return token, nil }, credentialDir: request.CredentialDir}
	if err := github.check(request.Repo); err != nil {
		return err
	}
	if request.Log == nil {
		return errors.New("workspace fetch log is required")
	}
	if request.Clock == nil {
		return errors.New("workspace fetch clock is required")
	}
	credential, err := github.open(request.Repo, "", "")
	if err != nil {
		return OutsideVolume(err)
	}
	defer func() {
		_ = credential.remove()
	}()
	present, err := OwnCacheRepository(cache, request.Repo, request.Clock.Now(), request.Log)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return fmt.Errorf("create cache parent: %w", err)
	}
	held := newFetchCredential(credential, request.Token)
	if present {
		deadline := request.Clock.Now().Add(request.bound())
		if _, err := fetchDefaultBranch(ctx, run, request, cache, held, deadline, false); err != nil {
			return err
		}
		fetched, err := repositoryRefs(ctx, run, cache)
		if err != nil {
			return err
		}
		if err := followHead(ctx, run, request, cache, fetched, held, deadline); err != nil {
			return err
		}
		return credential.remove()
	}

	staging, err := stageBeside(cache, request.Log)
	if err != nil {
		return err
	}
	defer func() {
		_ = rolefile.RemoveTree(staging)
	}()
	if _, err := RunChecked(ctx, run, slices.Concat(isolatedGit, []string{"init", "--bare", "--quiet", staging}), isolatedGitConfig, filepath.Dir(staging)); err != nil {
		return err
	}
	if err := cacheConfiguration(ctx, run, staging); err != nil {
		return err
	}
	target, err := fetchDefaultBranch(ctx, run, request, staging, held, request.Clock.Now().Add(request.bound()), true)
	if err != nil {
		return err
	}
	if _, err := CacheGit(ctx, run, staging, "symbolic-ref", "HEAD", target); err != nil {
		return err
	}
	if err := os.Rename(staging, cache); err != nil {
		return fmt.Errorf("move the fetched repository into the cache at %s: %w", cache, err)
	}
	return credential.remove()
}

// fetchDefaultBranch reads the branch GitHub's HEAD names and fetches it alone into the bare
// repository gitDir (cacheFetchInto), both under the one-shot credential held, with Fetch's
// retries, before deadline, and answers the branch's ref. With sweep set, each retry first removes
// the locks and partial packs a failed attempt left in gitDir, so it starts from the repository as
// the first attempt found it: the first fill's temporary sibling.
func fetchDefaultBranch(ctx context.Context, run Runner, request FetchRequest, gitDir string, held *fetchCredential, deadline time.Time, sweep bool) (string, error) {
	target, err := remoteHead(ctx, run, request, gitDir, held, deadline)
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", fmt.Errorf("GitHub names no default branch for %s", request.Repo)
	}
	transfer := cacheFetchInto(gitDir, request.Repo, target)
	if sweep {
		transfer.sweep = gitDir
	}
	if _, err := transferWithRetries(ctx, run, request, transfer, held, deadline); err != nil {
		return "", err
	}
	return target, nil
}

// cacheFetchInto is the fetch of target, the one branch the cache retains, into the bare
// repository gitDir, with no tag: the cache's first fill into its temporary sibling and each
// refresh. repocache narrows any other ref after this fetch and before it publishes the cache.
func cacheFetchInto(gitDir string, repo ghrepo.Repository, target string) gitHubTransfer {
	return gitHubTransfer{
		verb: "fetch",
		argv: slices.Concat(isolatedGit, []string{
			"-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"--git-dir=" + gitDir, "fetch", "--no-tags", "--prune", "--quiet", GitHubURL(repo),
			"+" + target + ":" + target,
		}),
		dir: gitDir,
	}
}

// remoteHead is the ref GitHub's HEAD names, read as remoteRefs reads it; "" when GitHub names
// none.
func remoteHead(ctx context.Context, run Runner, request FetchRequest, gitDir string, held *fetchCredential, deadline time.Time) (string, error) {
	target, _, err := remoteRefs(ctx, run, request, gitDir, held, deadline)
	return target, err
}

// remoteRefs is the ref GitHub's HEAD names, and every ref GitHub lists matching patterns with the
// object it names (`git ls-remote --symref <url> HEAD <patterns…>`), run as a transfer under the
// fetch's credential, retries and deadline; "" when GitHub names no HEAD. A HEAD that names
// anything but a branch is the fetch's failure.
func remoteRefs(ctx context.Context, run Runner, request FetchRequest, gitDir string, held *fetchCredential, deadline time.Time, patterns ...string) (string, map[string]string, error) {
	argv := slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir, "ls-remote", "--symref", GitHubURL(request.Repo), "HEAD"}, patterns)
	result, err := transferWithRetries(ctx, run, request, gitHubTransfer{verb: "ls-remote", argv: argv, dir: gitDir}, held, deadline)
	if err != nil {
		return "", nil, fmt.Errorf("read GitHub's default branch of %s: %w", request.Repo, err)
	}
	target := ""
	listed := map[string]string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if ref, rest, ok := strings.Cut(strings.TrimPrefix(line, "ref: "), "\t"); ok && strings.HasPrefix(line, "ref: ") && rest == "HEAD" {
			target = ref
			continue
		}
		if id, ref, ok := strings.Cut(line, "\t"); ok && ref != "HEAD" {
			listed[ref] = id
		}
	}
	if target != "" && !strings.HasPrefix(target, "refs/heads/") {
		return "", nil, fmt.Errorf("GitHub's HEAD of %s names %q, not a branch", request.Repo, target)
	}
	return target, listed, nil
}

// followHead points the bare repository gitDir's HEAD at the branch GitHub's HEAD names now
// (`git ls-remote --symref`), under the fetch's credential, retries and deadline, and logs the
// move when there is one. A fetch never moves HEAD, and the cache's own fetch prunes the branches
// GitHub deleted, so a repository whose default branch GitHub renamed would leave the cache a HEAD
// naming a branch that is gone: it could publish no head, and an issue's clone would check nothing
// out. HEAD moves only to
// a branch among fetched, the refs the fetch left: GitHub renaming its branch between the fetch and
// this read names one the fetch did not bring, so HEAD stays, logged, until a fetch brings it. A
// clone takes GitHub's HEAD itself. A repository GitHub answers with no default branch (nothing
// pushed yet) keeps its HEAD.
func followHead(ctx context.Context, run Runner, request FetchRequest, gitDir string, fetched map[string]string, held *fetchCredential, deadline time.Time) error {
	target, err := remoteHead(ctx, run, request, gitDir, held, deadline)
	if err != nil {
		return err
	}
	if target == "" {
		return nil
	}
	symbolicRef := slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir, "symbolic-ref", "--quiet", "HEAD"})
	current, err := runCommandTimeout(ctx, run, symbolicRef, isolatedGitConfig, gitDir, run.Timeout())
	if err := answered(symbolicRef, current, err); err != nil {
		return err
	}
	was := strings.TrimSpace(current.Stdout)
	if was == target {
		return nil
	}
	if _, ok := fetched[target]; !ok {
		request.Log(fmt.Sprintf("GitHub's default branch of %s is now %s, which this fetch did not bring: HEAD stays at %s until a fetch brings it", request.Repo, strings.TrimPrefix(target, "refs/heads/"), cmp.Or(was, "nothing")))
		return nil
	}
	if _, err := CacheGit(ctx, run, gitDir, "symbolic-ref", "HEAD", target); err != nil {
		return err
	}
	request.Log(fmt.Sprintf("the default branch of %s is now %s (HEAD named %s)", request.Repo, strings.TrimPrefix(target, "refs/heads/"), cmp.Or(was, "nothing")))
	return nil
}

// repositoryRefs is every ref of gitDir with the object it names.
func repositoryRefs(ctx context.Context, run Runner, gitDir string) (map[string]string, error) {
	listed, err := CacheGit(ctx, run, gitDir, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
		if ref, id, ok := strings.Cut(line, " "); ok {
			refs[ref] = id
		}
	}
	return refs, nil
}

// objectsSize is how many bytes the regular files under gitDir's objects directory hold; none when
// there is no such directory yet.
func objectsSize(gitDir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(filepath.Join(gitDir, "objects"), func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

// cacheClient reaches the repository cache directly, at the address it is given, the cache's
// Service: no proxy the container's environment names stands between an issue pod and a pod of its
// own namespace.
var cacheClient = &http.Client{Transport: &http.Transport{Proxy: nil, ForceAttemptHTTP2: false}}

// CachePoll is how long Fetch waits between two asks of a repository cache that is not ready.
const CachePoll = 10 * time.Second

// CacheWaitTimeout bounds Fetch's wait on a repository cache that is not ready: the cache's own
// first fill, which may take up to CacheFetchTimeout, and 10 minutes for its pod to be scheduled
// and its volume bound. A pod launched while a large repository's first fill runs waits it out
// rather than failing its fetch, which the daemon would count as one of its claim's launch
// failures (launch_failure_limit, 3 by default), so a first fill that finishes within
// CacheFetchTimeout never ends a claim failed. The cost is that a waiting pod holds its node
// reservation, and its launch the issue's lifecycle lane, for up to the whole wait. That happens
// while a first fill runs, and also whenever the cache never becomes ready: a cache in a crash
// loop (a refused token, GitHub down), one whose pod cannot be scheduled or whose volume does not
// bind, or a first fill that runs past CacheFetchTimeout and starts again from nothing. Nothing
// cuts the wait short then (the daemon describes the cache only once the fetch has failed), so
// each launch spends 130 minutes rather than 40 before its fetch fails, and a claim takes about
// three times as long to reach its launch_failure_limit.
const CacheWaitTimeout = CacheFetchTimeout + 10*time.Minute

// FetchBudget is the issue-pod fill's clocked budget: CacheWaitTimeout for a narrowed cache to
// become ready, then BucketFetchTimeout for the attempt on the cache's bucket (its /packs, its
// downloads and their verification), then FetchTimeout to verify the tar StreamCache unpacked when
// that attempt failed, and FetchTimeout for the GitHub delta, which has a bound of its own so that
// a slow verification cannot leave the delta minutes to run in (sjawhar/legion#2235): 205 minutes.
// A bucket attempt that runs its whole bound and fails leaves /tar's verification and the delta
// theirs. The tar stream itself holds its low-speed bound instead of a clock, so a stream that keeps
// progressing may run longer. CloneTimeout separately bounds the Jujutsu setup after these stages,
// and CheckoutTimeout bounds the workspace checkout (sandbox.Runtime.ProvisionBound sums the three).
const FetchBudget = CacheWaitTimeout + BucketFetchTimeout + FetchTimeout + FetchTimeout

// waitForCache asks the repository cache's GET <cache>/healthz every CachePoll until it answers
// 200 ready, logging what it answered whenever that changes and at least once a minute, and fails
// with the last answer once a further ask would start past CacheWaitTimeout. Each ask is bounded
// by cacheAsk, and by what remains of the wait, so the wait ends within it. An answer that names
// another repository (askCache) fails it at once: no wait makes that right. The asks carry no
// credential (cacheNotReady).
func waitForCache(ctx context.Context, request CacheStream) error {
	deadline := request.Clock.Now().Add(CacheWaitTimeout)
	var logged string
	var loggedAt time.Time
	for {
		why, err := cacheNotReady(ctx, request, min(cacheAsk, deadline.Sub(request.Clock.Now())))
		if err != nil {
			return err
		}
		if why == "" {
			return nil
		}
		now := request.Clock.Now()
		if !now.Add(CachePoll).Before(deadline) {
			return fmt.Errorf("the repository cache %s was not ready within %s: %s", request.URL, CacheWaitTimeout, why)
		}
		if why != logged || now.Sub(loggedAt) >= time.Minute {
			request.Log(fmt.Sprintf("waiting for the repository cache %s to be ready: %s", request.URL, why))
			logged, loggedAt = why, now
		}
		if err := request.Clock.Sleep(ctx, CachePoll); err != nil {
			return fmt.Errorf("the wait for the repository cache %s ended: %w", request.URL, err)
		}
	}
}

// cacheAsk bounds one ask of a repository cache's /healthz.
const cacheAsk = 30 * time.Second

// cacheNotReady is why the repository cache is not ready to stream, or "" when its /healthz
// answers 200 with ready set, within timeout; an answer askCache refuses is the error. The ask
// carries no credential: the cache answers its health to anyone, and its token goes with the tar
// alone (unpackCacheInto).
func cacheNotReady(ctx context.Context, request CacheStream, timeout time.Duration) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := askCache(bounded, request, "/healthz", false)
	if response == nil {
		return err.Error(), nil
	}
	defer response.Body.Close()
	if err != nil {
		return "", err
	}
	cache := strings.TrimSuffix(request.URL, "/")
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("GET %s/healthz answered %s", cache, response.Status), nil
	}
	var health struct {
		Ready             bool `json:"ready"`
		DefaultBranchOnly bool `json:"defaultBranchOnly"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&health); err != nil {
		return fmt.Sprintf("GET %s/healthz answered a body that is not the cache's health: %v", cache, err), nil
	}
	if !health.Ready {
		return fmt.Sprintf("GET %s/healthz answered not ready", cache), nil
	}
	if !health.DefaultBranchOnly {
		return fmt.Sprintf("GET %s/healthz answered ready without defaultBranchOnly", cache), nil
	}
	return "", nil
}

// askCache is GET <cache><path>, with the cache's bearer token when withToken is set. It answers
// (nil, the transport's failure) when the cache could not be reached, which is worth another ask.
// An answer that refuses the token (401) to a request that carried it, or whose
// CacheRepositoryHeader is not the request's repository, comes back with the refusal as its error:
// the caller closes the body and takes nothing it carries.
func askCache(ctx context.Context, request CacheStream, path string, withToken bool) (*http.Response, error) {
	cache := strings.TrimSuffix(request.URL, "/")
	ask, err := http.NewRequestWithContext(ctx, http.MethodGet, cache+path, nil)
	if err != nil {
		return nil, err
	}
	if withToken {
		ask.Header.Set("Authorization", "Bearer "+request.Token)
	}
	response, err := cacheClient.Do(ask)
	if err != nil {
		return nil, err
	}
	if withToken && response.StatusCode == http.StatusUnauthorized {
		return response, fmt.Errorf("ask the repository cache %s: GET %s%s answered %s: it is not the cache whose token this pod holds", request.URL, cache, path, response.Status)
	}
	if named := strings.TrimSpace(response.Header.Get(CacheRepositoryHeader)); !strings.EqualFold(named, request.Repo.String()) {
		shown := named
		if shown == "" {
			shown = `""`
		}
		return response, fmt.Errorf("the repository cache %s is the repository cache of %s, not of %s", request.URL, shown, request.Repo)
	}
	return response, nil
}

// unpackCacheInto streams the repository cache's tar into the clone's existing .git directory.
func unpackCacheInto(ctx context.Context, run Runner, request CacheStream, gitDir string) (int64, error) {
	bounded, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := &countingReader{window: time.After}
	stream.watchStall(bounded, cancel)
	response, err := askCache(bounded, request, "/tar", true)
	if response != nil {
		defer response.Body.Close()
	}
	// A request the cache refuses or does not answer fails outside the issue's volume. What the
	// answer streams is written to the volume, so a failure from there on is not.
	if err != nil {
		return 0, OutsideVolume(stream.stalledBy(err))
	}
	if response.StatusCode != http.StatusOK {
		return 0, OutsideVolume(fmt.Errorf("GET %s answered %s", response.Request.URL, response.Status))
	}
	stream.reader = response.Body
	archive := tar.NewReader(stream)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read the repository cache's tar: %w", err)
		}
		if err := unpackEntry(gitDir, header, archive); err != nil {
			return 0, err
		}
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return 0, fmt.Errorf("read the repository cache's tar: %w", err)
	}
	return stream.read.Load(), nil
}

// verifyCache holds the history the cache's tar, or its bucket, filled a clone's .git directory
// with: every pack's bytes must be the ones its checksum names, its index must carry its own
// checksum and name that pack, and every object the refs reach must be there (`git fsck
// --connectivity-only`, which walks the reachable commits and trees through the packs' indexes and
// reads no blob).
//
// A pack that came with its index (CacheEntry) is checked by the checksums git writes into
// both (packChecksums): the pack's own trailing hash over its bytes, the index's over its, and the
// pack hash the index records, which must be the pack's. That catches a pack or an index a
// transfer or a disk damaged, at a few seconds per gigabyte. It does not repeat `git index-pack`'s
// inflation and rehashing of every object, so it takes the index's names on the cache's word: an
// index whose checksums hold but which maps an object's name to another object's offset passes
// here and passes `fsck --connectivity-only`, and a checkout of a blob it misnames writes the other
// blob's content without an error, and the issue's jj workspace records no change for it. What
// this trusts is every writer of the cache's indexes: `git index-pack`, which named every object by
// its content when `git fetch` received the pack from GitHub; the cache's daily
// `git repack -a -d`, whose `pack-objects` writes new indexes from names it already holds and does
// not hash the objects again; the cache pod's volume; and whoever answers at the cache's address,
// since the bearer token proves the pod to the cache and nothing proves the cache to the pod. The
// daemon checks the cache's Service is the cache's own before it tells a pod the address, once
// (docs/kubernetes.md "The repository cache"). A pack and an index from the bucket are the cache's
// files too, each held to the SHA-256 the cache sent over that same channel before it reaches here
// (downloadObject), so the bucket adds no writer to that list. Rehashing in every pod caught a fault
// in any of those; it costs minutes of CPU per gigabyte in every pod instead — on one deployment's
// 3.2 GB cache, 8 to 12 minutes in every issue pod, the verification nine of them ran out of their
// bound in (sjawhar/legion#2235).
//
// A pack the tar streamed without its index, as a cache of a release that streams no index sends
// every pack, is indexed here when indexHere is set (/tar): then the clone, and not the cache,
// writes the index git reads. From the bucket (indexHere unset) such a pack is refused, and nothing
// is indexed: each index comes from the bucket. A pack that does not verify and a history that is
// not whole (errHistoryNotWhole) are refused, naming what failed. Each git command runs within
// what remains before deadline, the verification's own bound, which is bound long and the stage's
// own (whose): for /tar, FetchTimeout counted from the stream's end (StreamCache); for the
// bucket, what is left of its attempt's BucketFetchTimeout (fromBucket). One with nothing left
// fails, naming the bound. A history check that what remained ended before it answered has shown
// no object missing, so it is no history that is not whole: it fails naming the bound
// (errHistoryCheckRanOut), with none of git's words.
func verifyCache(ctx context.Context, run Runner, clock Clock, gitDir string, deadline time.Time, whose string, bound time.Duration, indexHere bool) error {
	packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return err
	}
	hash, err := objectHash(ctx, run, gitDir)
	if err != nil {
		return err
	}
	for _, pack := range packs {
		index := strings.TrimSuffix(pack, ".pack") + ".idx"
		indexed, err := pathExists(index)
		if err != nil {
			return err
		}
		if indexed {
			if err := packChecksums(pack, index, hash); err != nil {
				return fmt.Errorf("the repository cache's pack %s, which does not verify: %w", filepath.Base(pack), err)
			}
			continue
		}
		if !indexHere {
			return fmt.Errorf("the repository cache's pack %s came without its index, which this fill does not write", filepath.Base(pack))
		}
		left, err := remaining(clock, deadline, whose, bound, "indexing the repository cache's pack "+filepath.Base(pack))
		if err != nil {
			return err
		}
		if _, err := CacheGitWithin(ctx, run, gitDir, left, "index-pack", pack); err != nil {
			return fmt.Errorf("the repository cache's pack %s, which does not verify: %w", filepath.Base(pack), err)
		}
	}
	left, err := remaining(clock, deadline, whose, bound, "checking the repository cache's history is whole")
	if err != nil {
		return err
	}
	// The check runs as CacheGitWithin would, but keeps the run's Result: a checked run's error does
	// not say whether its bound ended it.
	fsck := slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"})
	result, err := runCommandTimeout(ctx, run, fsck, isolatedGitConfig, gitDir, left)
	if err == nil && result.TimedOut {
		return fmt.Errorf("the %s %s bound %w", whose, bound, errHistoryCheckRanOut)
	}
	if _, err := checkedResult(fsck, result, err); err != nil {
		return fmt.Errorf("%w: %w", errHistoryNotWhole, err)
	}
	return nil
}

// errHistoryNotWhole is verifyCache's refusal of a history whose refs reach an object the clone
// does not hold, with git's own words after it.
var errHistoryNotWhole = errors.New("the repository cache's history is not whole")

// errHistoryCheckRanOut is verifyCache's failure where what remained of the verification's bound
// ended `git fsck --connectivity-only` before it answered, wrapped after that bound and the stage
// it is (verifyCache's whose): "the cache stream's <bound> bound ran out while checking the
// repository cache's history is whole", or the bucket attempt's. It says nothing of
// the history, which a long one checked on a pod short of CPU outlasts while whole: 20 fetches at
// once in one pod of 8 CPU each ran out of both the bucket attempt's bound and /tar's in this check
// on firefox's history.
var errHistoryCheckRanOut = errors.New("ran out while checking the repository cache's history is whole")

// objectHash is the hash function the repository at gitDir names its objects with, as git reports
// it (`git rev-parse --show-object-format`). Every checksum a pack and its index carry is that
// hash's, so its size is how those files end. Today it is always sha1: the fill streams into the
// .git of a clone it makes with `git init`, StreamCache makes a bare repository with `git init
// --bare` under no configuration only where its caller names none, and the repository cache
// refuses any object format but sha1 (cacheConfigKeys), so a SHA-256 cache is refused at its pack's
// checksum. The sha256 case is for a git whose default format has changed, and reads the same file
// layout with 32-byte hashes.
func objectHash(ctx context.Context, run Runner, gitDir string) (func() hash.Hash, error) {
	named, err := CacheGit(ctx, run, gitDir, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	switch format := strings.TrimSpace(named.Stdout); format {
	case "sha1":
		return sha1.New, nil
	case "sha256":
		return sha256.New, nil
	default:
		return nil, fmt.Errorf("the repository at %s names its objects with %q, which this fetch does not know", gitDir, format)
	}
}

// packChecksums holds a pack and the index git reads it through to the checksums git wrote into
// them: a pack ends with the hash of everything before it, an index with the hash of everything
// before it, and an index records before its own hash the hash of the pack it indexes. All three
// must hold, so neither file lost or gained a byte on the way and the index is this pack's.
func packChecksums(pack, index string, hash func() hash.Hash) error {
	packHash, err := trailingHash(pack, hash)
	if err != nil {
		return err
	}
	if _, err := trailingHash(index, hash); err != nil {
		return err
	}
	size := hash().Size()
	indexed := make([]byte, size)
	file, err := os.Open(index)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < int64(2*size) {
		return fmt.Errorf("its index %s is %d bytes, too short to hold the checksums git writes", filepath.Base(index), info.Size())
	}
	if _, err := file.ReadAt(indexed, info.Size()-int64(2*size)); err != nil {
		return fmt.Errorf("read the pack checksum its index %s records: %w", filepath.Base(index), err)
	}
	if !bytes.Equal(indexed, packHash) {
		return fmt.Errorf("its index %s indexes the pack %x, not this one", filepath.Base(index), indexed)
	}
	return nil
}

// trailingHash checks that the file at path ends with the hash of everything before it, git's own
// checksum of a pack and of a pack index, and answers that hash.
func trailingHash(path string, hash func() hash.Hash) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	sum := hash()
	size := int64(sum.Size())
	if info.Size() < size {
		return nil, fmt.Errorf("%s is %d bytes, too short to hold the checksum git writes", filepath.Base(path), info.Size())
	}
	if _, err := io.Copy(sum, io.LimitReader(file, info.Size()-size)); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	written := make([]byte, size)
	if _, err := file.ReadAt(written, info.Size()-size); err != nil {
		return nil, fmt.Errorf("read the checksum of %s: %w", filepath.Base(path), err)
	}
	if computed := sum.Sum(nil); !bytes.Equal(computed, written) {
		return nil, fmt.Errorf("%s carries the checksum %x over bytes that hash to %x", filepath.Base(path), written, computed)
	}
	return written, nil
}

// remaining is what is left before deadline, by clock, for what runs next, or the failure naming
// it and the bound of the stage (whose) it runs in once nothing is. It takes the clock and bound
// rather than a request, so a cache stream's checks, a GitHub transfer and the shared clone's
// steps (cloneSteps) share it.
func remaining(clock Clock, deadline time.Time, whose string, bound time.Duration, what string) (time.Duration, error) {
	left := deadline.Sub(clock.Now())
	if left <= 0 {
		return 0, fmt.Errorf("the %s %s bound ran out before %s", whose, bound, what)
	}
	return left, nil
}

// unpackEntry writes one entry of the repository cache's tar into a clone's .git directory,
// refusing one the clone does not take.
func unpackEntry(gitDir string, header *tar.Header, body io.Reader) error {
	name := strings.TrimSuffix(header.Name, "/")
	if !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
		return fmt.Errorf("the repository cache's tar holds %q, which is not a path inside the clone's git directory", name)
	}
	dir := header.Typeflag == tar.TypeDir
	if !dir && header.Typeflag != tar.TypeReg {
		return fmt.Errorf("the repository cache's tar holds %q, which is neither a regular file nor a directory", name)
	}
	if !CacheEntry(name, dir) {
		return fmt.Errorf("the repository cache's tar holds %q, which is not one of a repository's refs or packs", name)
	}
	target := filepath.Join(gitDir, filepath.FromSlash(name))
	if dir {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, body); err != nil {
		_ = file.Close()
		return fmt.Errorf("read %s from the repository cache's tar: %w", name, err)
	}
	return file.Close()
}

// countingReader counts what it reads and holds the repository cache's answer to the low-speed
// bound: its watch ends an ask the cache never answers and a stream that falls below the bound
// (watchStall), and a stream above it runs for as long as the repository needs. reader is the
// answer's body, set once the ask has answered, since the watch starts before the ask: a cache
// that takes a connection and sends nothing has delivered nothing, which is the bound's own case.
// Each download from the cache's bucket holds the same bound through one of its own
// (downloadObject), whose failure names the bucket (errDownloadStalled) rather than the cache.
type countingReader struct {
	reader io.Reader
	// window answers a channel that delivers once d has passed: time.After in a pod, a test's own
	// clock. The watch waits on it beside its context rather than on the request's Clock, whose
	// Sleep cannot be waited on beside a Read that has blocked, which is the stall that matters.
	window  func(d time.Duration) <-chan time.Time
	read    atomic.Int64
	stalled atomic.Bool
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read.Add(int64(n))
	return n, r.stalledBy(err)
}

// watchStall ends the cache's answer, by stop, the cancel of the context the ask and every Read of
// its body run under, once a whole CacheStreamLowSpeedTime has passed in which fewer than
// CacheStreamLowSpeedBytes arrived. It reads the count the Reads keep rather than checking inside
// one, so a stream whose Read has blocked on a cache that sends nothing is ended at the same
// bound as one that dribbles. It opens the first window and reads the count that window starts
// from before it returns, and only then starts the goroutine that judges the windows, so the first
// window and its baseline are set before the ask and every Read: taken when the scheduler first
// ran that goroutine, after Reads had begun, they would leave those bytes out of the window and
// could end a stream that delivered them. The goroutine returns when ctx ends, which
// unpackCacheInto's own cancel does as soon as the answer has been read to its end.
func (r *countingReader) watchStall(ctx context.Context, stop context.CancelFunc) {
	window, at := r.window(CacheStreamLowSpeedTime), r.read.Load()
	go r.judgeWindows(ctx, stop, window, at)
}

// judgeWindows is watchStall's goroutine: it waits out window, whose count started at at, and each
// window after it, and ends the stream at the first window that fell below the bound.
func (r *countingReader) judgeWindows(ctx context.Context, stop context.CancelFunc, window <-chan time.Time, at int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-window:
		}
		read := r.read.Load()
		if read-at < CacheStreamLowSpeedBytes {
			r.stalled.Store(true)
			stop()
			return
		}
		at = read
		window = r.window(CacheStreamLowSpeedTime)
	}
}

// stalledBy names the low-speed bound in err once the watch has ended the answer: every failure
// the ask and the body's Reads give after that is the cancelled context's, which says nothing of
// why. An err of nil, io.EOF included, passes through as it is.
func (r *countingReader) stalledBy(err error) error {
	if err == nil || !r.stalled.Load() {
		return err
	}
	return fmt.Errorf("%w: %w", errStreamStalled, err)
}

// errStreamStalled is what a stream the low-speed bound ended says of itself, naming the bound.
var errStreamStalled = fmt.Errorf("the repository cache delivered less than %d bytes in %s, the low-speed bound below which its stream is ended", CacheStreamLowSpeedBytes, CacheStreamLowSpeedTime)

// gitHubTransfer is how a transfer from GitHub runs: verb names each attempt in the log and in a
// failure, argv is the attempt, dir is the directory each attempt runs in, and sweep, when set, is
// the bare repository whose stale locks and partial packs a failed attempt left are removed before
// the next one (RemoveStaleCacheLocks): the repository cache's first fill's temporary sibling,
// which nothing else writes.
type gitHubTransfer struct {
	verb  string
	argv  []string
	dir   string
	sweep string
}

// stageBeside is the temporary sibling of destination a repository that lacks it is filled in.
// Fetch uses it for the repository cache, and ensureRepoClone uses it for an issue pod's clone.
// A sibling an earlier attempt left — killed before it renamed or removed it — is removed first and
// logged: a killed fill of a large repository left on a volume of fixed size would fill it.
func stageBeside(destination string, log func(string)) (string, error) {
	stale, err := filepath.Glob(destination + cloneSuffix + "*")
	if err != nil {
		return "", err
	}
	for _, left := range stale {
		if err := rolefile.RemoveTree(left); err != nil {
			return "", fmt.Errorf("remove the clone an earlier fetch left at %s: %w", left, err)
		}
		log("removed the clone an earlier fetch left at " + left)
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), filepath.Base(destination)+cloneSuffix)
	if err != nil {
		return "", fmt.Errorf("create the clone's temporary directory beside %s: %w", destination, err)
	}
	return staging, nil
}

// cloneSuffix follows the destination repository's name in the temporary directory it is filled in.
const cloneSuffix = ".clone-"

// cacheConfiguration is what the cache repository's own configuration sets: no automatic garbage
// collection or maintenance, ever, and every fetch's objects kept as the pack they arrived in
// (`fetch.unpackLimit=1`). It keeps cache refreshes predictable and preserves the repository
// cache's pack-only stream.
func cacheConfiguration(ctx context.Context, run Runner, gitDir string) error {
	for _, pair := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}, {"fetch.unpackLimit", "1"}} {
		if _, err := CacheGit(ctx, run, gitDir, "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// CacheGit runs git on gitDir — the repository cache's bare repository or an issue pod clone's
// .git — under the runner's slow-command budget, holding no credential and reading no configuration
// but that repository's own and the pack settings isolatedGit names. It runs in gitDir itself,
// which is there whenever something reads or tends it, so no caller has to name a directory and no
// child of this package's takes the one the process happens to run in.
func CacheGit(ctx context.Context, run Runner, gitDir string, args ...string) (Result, error) {
	return RunChecked(ctx, run, slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir}, args), isolatedGitConfig, gitDir)
}

// CacheGitWithin is CacheGit bounded by timeout instead of the runner's slow-command budget: for a
// command on such a repository whose duration follows the repository's size, as the cache's repack
// does. A `-c` among args comes after isolatedGit's, so it wins over them for its key.
func CacheGitWithin(ctx context.Context, run Runner, gitDir string, timeout time.Duration, args ...string) (Result, error) {
	argv := slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir}, args)
	result, err := runCommandTimeout(ctx, run, argv, isolatedGitConfig, gitDir, timeout)
	return checkedResult(argv, result, err)
}

// transferWithRetries runs transfer under the one-shot credential held until it succeeds, retrying
// a transient failure as Fetch says, each attempt bounded by what remains before deadline and run
// on the provisioning token as it stands when the attempt starts (renew). It answers the attempt
// that succeeded.
func transferWithRetries(ctx context.Context, run Runner, request FetchRequest, transfer gitHubTransfer, held *fetchCredential, deadline time.Time) (Result, error) {
	now := request.Clock.Now()
	if _, err := remaining(request.Clock, deadline, "fetch's", request.bound(), "the "+transfer.verb+" from GitHub"); err != nil {
		return Result{}, err
	}
	for attempt := 1; ; attempt++ {
		if err := held.renew(); err != nil {
			return Result{}, fmt.Errorf("%s attempt %d: %w", transfer.verb, attempt, err)
		}
		result, runErr := runCommandTimeout(ctx, run, transfer.argv, held.env, transfer.dir, deadline.Sub(now))
		result.Stderr = held.scrub(result.Stderr)
		_, failed := checkedResult(transfer.argv, result, runErr)
		if failed == nil {
			return result, nil
		}
		if runErr == nil && outsideVolumeFailure(result) {
			failed = OutsideVolume(failed)
		}
		if runErr != nil || !transientCloneFailure(result) {
			return Result{}, failed
		}
		if attempt > len(fetchBackoff) {
			return Result{}, fmt.Errorf("%w\n%s attempt %d failed, and the fetch retries a %s at most %d times", failed, transfer.verb, attempt, transfer.verb, len(fetchBackoff))
		}
		wait := fetchBackoff[attempt-1]
		if !request.Clock.Now().Add(wait).Before(deadline) {
			return Result{}, fmt.Errorf("%w\n%s attempt %d failed, and a retry in %s would start past the fetch's %s bound", failed, transfer.verb, attempt, wait, request.bound())
		}
		request.Log(fmt.Sprintf("%s attempt %d failed; retrying in %s: %s", transfer.verb, attempt, wait,
			strings.ReplaceAll(strings.TrimSpace(result.Stderr), "\n", " | ")))
		if transfer.sweep != "" {
			if err := RemoveStaleCacheLocks(transfer.sweep, request.Clock.Now(), request.Log); err != nil {
				return Result{}, fmt.Errorf("remove what %s attempt %d left in %s: %w", transfer.verb, attempt, transfer.sweep, err)
			}
		}
		if err := request.Clock.Sleep(ctx, wait); err != nil {
			return Result{}, fmt.Errorf("%w\nthe wait before %s attempt %d ended: %w", failed, transfer.verb, attempt+1, err)
		}
		if now = request.Clock.Now(); !now.Before(deadline) {
			return Result{}, fmt.Errorf("%w\n%s attempt %d failed, and its retry after the %s wait would start past the fetch's %s bound", failed, transfer.verb, attempt, wait, request.bound())
		}
	}
}

// fetchCredential is the one-shot credential a fetch's transfers from GitHub run under (remote,
// with env the environment that names it), the way to read the provisioning token
// (FetchRequest.Token), and every token it has held, none of which anything Fetch logs or returns
// carries.
type fetchCredential struct {
	remote remote
	env    []string
	read   func() (string, error)
	held   []string
}

// newFetchCredential is the fetchCredential of the one-shot credential r, renewed from read, which
// has held r's own token.
func newFetchCredential(r remote, read func() (string, error)) *fetchCredential {
	return &fetchCredential{remote: r, env: merge(r.env, isolatedGitConfig), read: read, held: []string{r.token}}
}

// renew reads the provisioning token as it stands now and writes it over the credential's, so the
// transfer about to start presents it: a token file the daemon refreshed during a long wait on the
// repository cache, or a retry's backoff, is the one git is given, never the token the fetch began
// with.
func (c *fetchCredential) renew() error {
	token, err := c.read()
	if err != nil {
		return OutsideVolume(fmt.Errorf("read the provisioning token: %w", err))
	}
	if err := c.remote.setToken(token); err != nil {
		return err
	}
	if !slices.Contains(c.held, token) {
		c.held = append(c.held, token)
	}
	return nil
}

// scrub is text with every token the credential has held replaced by ***.
func (c *fetchCredential) scrub(text string) string {
	for _, token := range c.held {
		text = strings.ReplaceAll(text, token, "***")
	}
	return text
}

// Clock is the time Fetch's retries read and wait on: clock.Real(), or a test's own. It is a
// narrowing of the module's clock.Clock to the two calls Fetch makes, so a test's fake needs only
// those.
type Clock interface {
	Now() time.Time
	// Sleep waits d, or returns ctx's error once ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
}

// fetchBackoff is the wait before each retry of a transfer from GitHub, one retry per entry: a
// first retry soon after a stream GitHub cut, the last a minute later. A transfer still failing
// after the last fails the fetch, so a GitHub outage holds a launch for a few attempts, not the whole
// FetchTimeout, which bounds a slow transfer instead.
var fetchBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

// gitFatalExit is the status git's die() exits with, which every failure below ends in. Any other
// is not git reporting a failed transfer: a signal, a usage error, or the attempt's own timeout.
const gitFatalExit = 128

var (
	// causeLine is git's line saying why a request of the clone failed, in either of its forms:
	// remote-curl's `RPC failed; …` for a request of the transfer, once the repository was found,
	// and `unable to access '<url>': …` for the first request. What follows is the HTTP status the
	// server answered with, or curl's own error.
	causeLine = regexp.MustCompile(`(?:RPC failed; |unable to access '[^']*': )(.*)`)
	// statusFailure is the HTTP status a cause names, in either form: `HTTP <status> curl …` after
	// `RPC failed;`, or curl's `The requested URL returned error: <status>`.
	statusFailure = regexp.MustCompile(`(?:^HTTP |The requested URL returned error: )(\d+)`)
)

// curlNetworkFailures are curl's errors for a connection that could not be made or broke: never
// an answer from GitHub, so the next attempt may not meet them. Any other curl error a cause names
// is not here — a certificate that fails to verify (curl 60), a write to the clone that failed
// (curl 23) — since it fails the same way every time.
var curlNetworkFailures = []string{
	"Failed to connect to",     // CURLE_COULDNT_CONNECT (7): refused, or the connect timed out
	"Could not resolve host",   // CURLE_COULDNT_RESOLVE_HOST (6)
	"timed out",                // CURLE_OPERATION_TIMEDOUT (28): "Connection timed out after …", "Operation timed out after …"
	"Connection reset by peer", // a reset under any of curl's errors
	"Recv failure",             // CURLE_RECV_ERROR (56)
	"Send failure",             // CURLE_SEND_ERROR (55)
	"Empty reply from server",  // CURLE_GOT_NOTHING (52)
	"transfer closed with outstanding read data remaining", // CURLE_PARTIAL_FILE (18)
	"was not closed cleanly",                               // CURLE_HTTP2_STREAM (92)
	"HTTP2 framing layer",                                  // CURLE_HTTP2 (16)
	"SSL_ERROR_SYSCALL",                                    // OpenSSL: the connection closed under TLS
	"unexpected eof while reading",                         // OpenSSL 3: the same
	"TLS connection was non-properly terminated",           // GnuTLS, Debian's git: the same
	"GnuTLS recv error",                                    // GnuTLS: a read that failed
	"Error in the pull function",                           // GnuTLS: the same, under the handshake
	"Error in the push function",                           // GnuTLS: a write that failed
}

// streamCuts are git's own lines for a pack stream that ended before the pack did, which a
// connection closed mid-transfer leaves when curl itself reported nothing.
var streamCuts = []string{
	"early EOF",
	"unexpected disconnect while reading sideband packet",
	"the remote end hung up unexpectedly",
}

// transientCloneFailure is whether a transfer from GitHub that failed may succeed if run again:
// git's fatal exit, with a cause that names a status GitHub may not answer again — 408 (it timed the
// request out), 429 (it is limiting the rate) or a 5xx (it failed) — or a connection that failed
// or broke (curlNetworkFailures), or, with no cause line at all, a pack stream that ended early
// (streamCuts). The cause line decides, never the follow-on line git prints after it: a 403 on a
// request of the transfer ends in the same `expected flush after ref listing` a 502 does. A
// credential refused (`Authentication failed`), a repository not found or not granted (404, 403),
// a certificate that does not verify, a full disk, and anything this does not recognise fail at
// once, as every clone failure did before the fetch retried.
func transientCloneFailure(result Result) bool {
	if result.TimedOut || result.ExitCode != gitFatalExit {
		return false
	}
	cause := causeLine.FindStringSubmatch(result.Stderr)
	if cause == nil {
		return containsAny(result.Stderr, streamCuts)
	}
	if status := statusFailure.FindStringSubmatch(cause[1]); status != nil {
		return transientStatus(status[1])
	}
	return containsAny(cause[1], curlNetworkFailures)
}

// transientStatus is whether an HTTP status in git's output is one a server may not answer again:
// an outage (httpoutage.Is: a 408, a 429, a 5xx) no greater than 599, since git prints any number.
func transientStatus(status string) bool {
	code, err := strconv.Atoi(status)
	return err == nil && code <= 599 && httpoutage.Is(code)
}

// gitHubRefusals are GitHub's own words, in git's output, for a credential it refused and for a
// repository it did not find or did not grant.
var gitHubRefusals = []string{"Authentication failed", "Repository not found"}

// outsideVolumeFailure is whether a transfer or a ref listing from GitHub failed for a cause
// outside the issue's volume (ErrOutsideVolume). A new volume does not cure such a cause, because
// the new volume needs GitHub too. The causes are an attempt that ran out of its time, and git's
// fatal exit with one of these in its output:
//   - a cause line (causeLine) that names an HTTP status (statusFailure), any status,
//   - a cause line that names a connection that failed or broke (curlNetworkFailures),
//   - a pack stream that ended early (streamCuts), with no cause line,
//   - GitHub's refusal of the credential or of the repository (gitHubRefusals).
//
// Every other cause line is not outside the volume: curl 23 is a write to the volume that failed,
// and curl 60 is a certificate that does not verify. Nor is a credential git could not read
// (`could not read Username`), an object the repository cache lacks (`fatal: bad object`), or any
// other failure. transientCloneFailure keeps its own rule, which tells which failures the fetch
// runs again.
func outsideVolumeFailure(result Result) bool {
	if result.TimedOut {
		return true
	}
	if result.ExitCode != gitFatalExit {
		return false
	}
	if containsAny(result.Stderr, gitHubRefusals) {
		return true
	}
	cause := causeLine.FindStringSubmatch(result.Stderr)
	if cause == nil {
		return containsAny(result.Stderr, streamCuts)
	}
	return statusFailure.MatchString(cause[1]) || containsAny(cause[1], curlNetworkFailures)
}

// containsAny is whether text holds any of needles.
func containsAny(text string, needles []string) bool {
	return slices.ContainsFunc(needles, func(needle string) bool { return strings.Contains(text, needle) })
}
