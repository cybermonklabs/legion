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

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// isolatedGitConfig is the configuration Fetch's git reads besides its own and its command line's
// (feedGit): none. Not the system's, not the image user's; and git's `-c`, GIT_CONFIG_PARAMETERS,
// no process provisioning starts inherits (config.go).
var isolatedGitConfig = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// feedGit is how every git command of Fetch's, FeedGit's and FeedGitWithin's begins: git, then
// packWindowConfig as `-c` pairs. That git reads no system file (isolatedGitConfig), so the worker
// image's /etc/gitconfig does not reach it, its runner removes the role environment's pairs
// (execRunner.Run), and the feed's own configuration may hold none of the four (feedConfigKeys).
// The runner's own pins (pinnedGitConfig) reach it too, but they reach every command any NewRunner
// starts, the daemon's host runners among them, so the four go on this command line instead. git
// exports the pairs to every git it starts in turn, the index-pack of a clone or a fetch among
// them. They come before everything a caller adds, and git takes the last value of a key, so a
// caller's own `-c` of one of them wins: the repository cache's repack keeps repackMemory's. Its
// capacity is its length, so an append to it copies rather than writing past the shared prefix.
var feedGit = func() []string {
	argv := []string{"git"}
	for _, pair := range packWindowConfig {
		argv = append(argv, "-c", pair[0]+"="+pair[1])
	}
	return slices.Clip(argv)
}()

// FetchRequest is the repository cache's GitHub fetch: the repository, how to read the
// provisioning token, the directory its one-shot credential goes under, and Root, the cache's
// volume. Bound limits each GitHub transfer; the cache's initial clone uses CacheFetchTimeout.
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
// only for /tar; /healthz remains public. Bound limits verification after the tar ends; zero takes
// FetchTimeout.
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

// StreamReport records the bytes and two stages of a repository-cache stream.
type StreamReport struct {
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

// StreamCache waits for the narrowed repository cache, unpacks its tar into gitDir, and verifies
// the history before the caller fetches its issue refs from GitHub. gitDir may already be the .git
// directory of the normal clone being made; when it is absent this function initializes the bare
// repository a cache test or caller needs.
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
		if _, err := RunChecked(ctx, run, slices.Concat(feedGit, []string{"init", "--bare", "--quiet", gitDir}), isolatedGitConfig, filepath.Dir(gitDir)); err != nil {
			return StreamReport{}, err
		}
	}
	started := stream.Clock.Now()
	bytes, err := unpackCacheInto(ctx, run, stream, gitDir)
	if err != nil {
		return StreamReport{}, err
	}
	verifying := stream.Clock.Now()
	if err := verifyCache(ctx, run, stream, gitDir, verifying.Add(stream.bound())); err != nil {
		return StreamReport{}, fmt.Errorf("fill the clone from the repository cache %s: %w", stream.URL, err)
	}
	return StreamReport{
		Bytes: bytes, Duration: verifying.Sub(started), VerifyDuration: stream.Clock.Now().Sub(verifying),
	}, nil
}

// CacheFetchTimeout is the repository cache's own Bound: its first clone takes the whole repository
// from GitHub and indexes every object of it, which on 2026-10-26 took one deployment's cache pod (one CPU,
// gVisor) past FetchTimeout 289 times over 30 hours, each attempt killed at 30 minutes and begun
// again from nothing, so the cache was never ready (sjawhar/legion#2240).
const CacheFetchTimeout = 2 * time.Hour

// CacheRepositoryHeader names, on every answer the repository cache gives a request its token
// authorizes, the repository it caches (owner/name), which Fetch checks is its own before it takes
// anything the answer carries.
const CacheRepositoryHeader = "Legion-Repository"

// InitExitUsage is the exit status of `legion workspace-init fetch` and `provision` for a usage
// error, a flag or an argument they do not know. It is the usage exit of every legion command.
const InitExitUsage = 2

// InitExitOutsideVolume is the exit status of `legion workspace-init fetch` and `provision` for a
// failure whose cause is outside the issue's volume (ErrOutsideVolume). A new volume does not cure
// such a failure, so the runtime keeps a kept volume for it. Every other failure exits 1.
const InitExitOutsideVolume = 3

// ErrOutsideVolume is the cause of a failure outside the issue's volume. These failures wrap it
// (OutsideVolume):
//   - a refusal of `fetch` or `provision` before it touches the volume,
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

// FetchReport is where Fetch filled the feed from and what each source brought: the feed
// repository; whether it was cloned whole from GitHub; the repository cache's tar, its length, how
// long it streamed and how long its history took to verify, when the feed was unpacked from it;
// and how much the feed's objects grew across the transfer from GitHub, with how many refs that
// transfer made, moved or deleted.
type FetchReport struct {
	Cloned      bool
	GitHubBytes int64
	Refs        int
}

// Fetch fills the feed with the provisioning token and no configuration but the one-shot
// credential's and the pins: the process of a pod's first init container, the only one that holds
// the token, whose feed is its own and which mounts nothing a tree agent can write, and the
// repository cache's process (repocache), whose volume is its own. It reaches GitHub as the tmux
// runtime's Source does, so the token's holders share one credential rule.
//
// A feed that holds the repository already — the cache's clone at each refresh, or an issue pod's
// feed when the kubelet retries its fetch container in the same pod — has the delta fetched into
// it from GitHub, once the locks and partial packs a killed fetch left are gone
// (RemoveStaleFeedLocks). An issue pod's feed found present is never removed or replaced: one in a
// shape Fetch does not write fails the fetch and is kept (ownFeedRepository), since the pod's clone
// reads the feed's objects through its alternates; the repository cache's own clone, which no clone
// borrows from, is removed in that case and filled afresh (FetchRequest.ReplaceForeign). A feed
// that does not exist is filled in a temporary sibling, its garbage collection turned off
// (feedConfiguration), and renamed into place once everything that fills it succeeded, so the
// feed's repository is whole and current, or absent.
// It is the repository cache's own process (repocache): a feed that does not exist is cloned bare
// from GitHub, its default branch alone; one that exists has that branch fetched into it. An issue
// pod streams the cache's tar into its own clone instead (StreamCache).
//
// Each size-following stage of a fetch that holds a clock gets its own bound, FetchTimeout unless
// the request sets Bound (the repository cache's own first clone), and none starts past the one it
// is in: from the cache, the verification of what was streamed has one, counted from the stream's
// end, and the delta from GitHub has another; without a cache, the clone or fetch from GitHub has
// one. The stream from the cache holds no clock: it ends when it falls below the low-speed bound
// (CacheStreamLowSpeedBytes, CacheStreamLowSpeedTime) and otherwise runs as long as the repository
// needs, so a download that is still receiving is never thrown away and begun again from nothing
// (INFRA-495). So an issue pod's fetch ends its waits and its clocked stages within FetchBudget,
// however long it waited on the cache, and a slow stream never leaves the verification or the
// delta minutes to run in: nine of one deployment's issue pods died in the delta on 2026-10-26,
// the stream and its verification having used most of the one bound they then shared
// (sjawhar/legion#2235). The repository cache's first clone, which waits on nothing, may take its
// whole CacheFetchTimeout, within FetchBudget too, and is no issue pod's. The few local git
// commands around them (reading refs, writing the feed's configuration) keep CommandTimeout
// each and take moments. Every attempt of a transfer from GitHub reads
// the provisioning token anew as it starts (FetchRequest.Token, fetchCredential.renew), so it
// presents the token as its file stands then, not as it stood when the fetch began. A transfer
// from GitHub that fails transiently (transientCloneFailure) is run again after the next of
// fetchBackoff's waits, once per wait, its failure logged and whatever a failed clone left removed
// first, and no retry starts past the bound. A transfer still failing after the last wait, and
// every other failure, is the answer at once. Each attempt's stderr has every token the fetch has
// read replaced by *** as soon as the attempt returns, so nothing Fetch logs or returns carries
// one, wherever git echoed it.
//
// A failure whose cause is outside the issue's volume wraps ErrOutsideVolume, and keeps its text:
// a read of the provisioning token that failed, a cache named without its token, the wait for the
// cache, a request the cache refuses or does not answer, and a transfer or a ref listing from
// GitHub that outsideVolumeFailure reads so, retried or not. The failures of what the cache
// streamed do not: unpackEntry gives one error for a write to the volume that failed and for a
// stream that broke, and a write that failed is the volume's, whatever the stream did. Nor does
// verifyCache's failure, or any other failure.
func Fetch(ctx context.Context, run Runner, request FetchRequest) (FetchReport, error) {
	cache, err := CacheRepository(request.Root, request.Repo)
	if err != nil {
		return FetchReport{}, err
	}
	if request.Token == nil {
		return FetchReport{}, errors.New("workspace fetch token is required")
	}
	token, err := request.Token()
	if err != nil {
		return FetchReport{}, OutsideVolume(err)
	}
	github := gitHubSource{token: func() (string, error) { return token, nil }, credentialDir: request.CredentialDir}
	if err := github.check(request.Repo); err != nil {
		return FetchReport{}, err
	}
	if request.Log == nil {
		return FetchReport{}, errors.New("workspace fetch log is required")
	}
	if request.Clock == nil {
		return FetchReport{}, errors.New("workspace fetch clock is required")
	}
	credential, err := github.open(request.Repo, "", "")
	if err != nil {
		return FetchReport{}, OutsideVolume(err)
	}
	defer func() {
		_ = credential.remove()
	}()
	present, err := OwnCacheRepository(cache, request.Repo, request.Clock.Now(), request.Log)
	if err != nil {
		return FetchReport{}, err
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return FetchReport{}, fmt.Errorf("create cache parent: %w", err)
	}
	held := &fetchCredential{remote: credential, env: merge(credential.env, isolatedGitConfig), read: request.Token, held: []string{token}}
	report := FetchReport{}
	if present {
		deadline := request.Clock.Now().Add(request.bound())
		target, err := remoteHead(ctx, run, request, cache, held, deadline)
		if err != nil {
			return FetchReport{}, err
		}
		if target == "" {
			return FetchReport{}, fmt.Errorf("GitHub names no default branch for %s", request.Repo)
		}
		if err := report.fromGitHub(ctx, run, request, cacheFetchInto(cache, request.Repo, target), cache, held, deadline); err != nil {
			return FetchReport{}, err
		}
		return report, credential.remove()
	}

	staging, err := stageBeside(cache, request.Log)
	if err != nil {
		return FetchReport{}, err
	}
	defer func() {
		_ = rolefile.RemoveTree(staging)
	}()
	clone := feedTransfer{
		verb: "clone",
		argv: slices.Concat(feedGit, []string{
			"-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"clone", "--bare", "--single-branch", "--no-tags", "--quiet", GitHubURL(request.Repo), staging,
		}),
		destination: staging,
		dir:         filepath.Dir(staging),
	}
	report.Cloned = true
	deadline := request.Clock.Now().Add(request.bound())
	if err := report.fromGitHub(ctx, run, request, clone, staging, held, deadline); err != nil {
		return FetchReport{}, err
	}
	if err := settleFeed(ctx, run, staging, cache); err != nil {
		return FetchReport{}, err
	}
	return report, credential.remove()
}

// cacheFetchInto refreshes the one branch the cache retains. repocache narrows any pre-existing
// foreign refs after this fetch and before it publishes the cache.
func cacheFetchInto(cache string, repo ghrepo.Repository, target string) feedTransfer {
	return feedTransfer{
		verb: "fetch",
		argv: slices.Concat(feedGit, []string{
			"-c", "gc.auto=0", "-c", "maintenance.auto=false",
			"--git-dir=" + cache, "fetch", "--no-tags", "--prune", "--quiet", GitHubURL(repo),
			"+" + target + ":" + target,
		}),
		dir: cache,
	}
}

// settleFeed writes the feed repository's own configuration into staging and moves it to feed,
// once what fills it has succeeded, so the feed's repository is whole and current, or absent.
func settleFeed(ctx context.Context, run Runner, staging, feed string) error {
	if err := feedConfiguration(ctx, run, staging); err != nil {
		return err
	}
	if err := os.Rename(staging, feed); err != nil {
		return fmt.Errorf("move the fetched repository into the feed at %s: %w", feed, err)
	}
	return nil
}

// fromGitHub runs transfer into the bare repository gitDir under the one-shot credential held,
// with Fetch's retries, all before deadline, and records how much the repository's objects grew
// across it and how many refs it made, moved or deleted. A clone's gitDir holds no repository
// before it runs: nothing, and no ref.
func (r *FetchReport) fromGitHub(ctx context.Context, run Runner, request FetchRequest, transfer feedTransfer, gitDir string, held *fetchCredential, deadline time.Time) error {
	refsBefore, err := feedRefs(ctx, run, gitDir, transfer.verb == "fetch")
	if err != nil {
		return err
	}
	sizeBefore, err := objectsSize(gitDir)
	if err != nil {
		return err
	}
	if _, err := transferWithRetries(ctx, run, request, transfer, held, deadline); err != nil {
		return err
	}
	refsAfter, err := feedRefs(ctx, run, gitDir, true)
	if err != nil {
		return err
	}
	if transfer.verb == "fetch" {
		if err := followHead(ctx, run, request, gitDir, refsAfter, held, deadline); err != nil {
			return err
		}
	}
	sizeAfter, err := objectsSize(gitDir)
	if err != nil {
		return err
	}
	r.GitHubBytes = sizeAfter - sizeBefore
	for ref, id := range refsAfter {
		if refsBefore[ref] != id {
			r.Refs++
		}
	}
	for ref := range refsBefore {
		if _, kept := refsAfter[ref]; !kept {
			r.Refs++
		}
	}
	return nil
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
	argv := slices.Concat(feedGit, []string{"--git-dir=" + gitDir, "ls-remote", "--symref", GitHubURL(request.Repo), "HEAD"}, patterns)
	result, err := transferWithRetries(ctx, run, request, feedTransfer{verb: "ls-remote", argv: argv, dir: gitDir}, held, deadline)
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
	symbolicRef := slices.Concat(feedGit, []string{"--git-dir=" + gitDir, "symbolic-ref", "--quiet", "HEAD"})
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

// feedRefs is every ref of the bare repository gitDir with the object it names, or none when held
// is unset: a repository a clone is yet to make.
func feedRefs(ctx context.Context, run Runner, gitDir string, held bool) (map[string]string, error) {
	refs := map[string]string{}
	if !held {
		return refs, nil
	}
	listed, err := CacheGit(ctx, run, gitDir, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
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
// first clone, which may take up to CacheFetchTimeout, and 10 minutes for its pod to be scheduled
// and its volume bound. A pod launched while a large repository's first clone runs waits it out
// rather than failing its fetch, which the daemon would count as one of its claim's launch
// failures (launch_failure_limit, 3 by default), so a first clone that finishes within
// CacheFetchTimeout never ends a claim failed. The cost is that a waiting pod holds its node
// reservation, and its launch the issue's lifecycle lane, for up to the whole wait. That happens
// while a first clone runs, and also whenever the cache never becomes ready: a cache in a crash
// loop (a refused token, GitHub down), one whose pod cannot be scheduled or whose volume does not
// bind, or a first clone that runs past CacheFetchTimeout and starts again from nothing. Nothing
// cuts the wait short then (the daemon describes the cache only once the fetch has failed), so
// each launch spends 130 minutes rather than 40 before its fetch fails, and a claim takes about
// three times as long to reach its launch_failure_limit.
const CacheWaitTimeout = CacheFetchTimeout + 10*time.Minute

// FetchBudget is the issue-pod fill's clocked budget: CacheWaitTimeout for a narrowed cache to
// become ready, FetchTimeout to verify the tar StreamCache unpacked, and FetchTimeout for the
// GitHub delta. The tar stream itself holds its low-speed bound instead of a clock, so a stream
// that keeps progressing may run longer. CloneTimeout separately bounds the Jujutsu setup after
// these stages, and CheckoutTimeout bounds the workspace checkout.
const FetchBudget = CacheWaitTimeout + FetchTimeout + FetchTimeout

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
// alone (unpackCache).
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
func unpackCacheInto(ctx context.Context, run Runner, request CacheStream, staging string) (int64, error) {
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
		if err := unpackEntry(staging, header, archive); err != nil {
			return 0, err
		}
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return 0, fmt.Errorf("read the repository cache's tar: %w", err)
	}
	return stream.read.Load(), nil
}

// verifyCache holds the history the cache streamed into staging to what a feed takes: every pack's
// bytes must be the ones its checksum names, its index must carry its own checksum and name that
// pack, and every object the refs reach must be there (`git fsck --connectivity-only`, which walks
// the reachable commits and trees through the packs' indexes and reads no blob).
//
// A pack the cache streamed its index with (FeedEntry) is checked by the checksums git writes into
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
// (docs/kubernetes.md "The repository cache"). Rehashing in every pod caught a fault in any of
// those; it costs minutes of CPU per gigabyte in every pod instead — on one deployment's
// 3.2 GB cache, 8 to 12 minutes in every issue pod, the verification nine of them ran out of their
// bound in (sjawhar/legion#2235).
//
// A pack that arrived without its index is indexed here, as every pack was before: then the feed,
// and not the cache, writes the index git reads. A pack that does not verify and a history that is
// not whole are refused, naming what failed. Each git command runs within what remains before
// deadline, the verification's own bound, counted from the stream's end (Fetch); one with nothing
// left fails the fetch, naming the bound.
func verifyCache(ctx context.Context, run Runner, request CacheStream, staging string, deadline time.Time) error {
	packs, err := filepath.Glob(filepath.Join(staging, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return err
	}
	hash, err := objectHash(ctx, run, staging)
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
		left, err := remaining(request.Clock, deadline, "cache stream's", request.bound(), "indexing the repository cache's pack "+filepath.Base(pack))
		if err != nil {
			return err
		}
		if _, err := CacheGitWithin(ctx, run, staging, left, "index-pack", pack); err != nil {
			return fmt.Errorf("the repository cache's pack %s, which does not verify: %w", filepath.Base(pack), err)
		}
	}
	left, err := remaining(request.Clock, deadline, "cache stream's", request.bound(), "checking the repository cache's history is whole")
	if err != nil {
		return err
	}
	if _, err := CacheGitWithin(ctx, run, staging, left, "fsck", "--connectivity-only", "--no-dangling", "--no-progress"); err != nil {
		return fmt.Errorf("the repository cache's history is not whole: %w", err)
	}
	return nil
}

// objectHash is the hash function the repository at gitDir names its objects with, as git reports
// it (`git rev-parse --show-object-format`). Every checksum a pack and its index carry is that
// hash's, so its size is how those files end. Today it is always sha1: unpackCache makes the staging
// repository with `git init --bare` under no configuration, and a feed refuses any object format but
// sha1 (feedConfigKeys), so a SHA-256 cache is refused at its pack's checksum. The sha256 case is
// for a git whose default format has changed, and reads the same file layout with 32-byte hashes.
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
// rather than a request, so a cache stream's checks and a GitHub transfer share it.
func remaining(clock Clock, deadline time.Time, whose string, bound time.Duration, what string) (time.Duration, error) {
	left := deadline.Sub(clock.Now())
	if left <= 0 {
		return 0, fmt.Errorf("the %s %s bound ran out before %s", whose, bound, what)
	}
	return left, nil
}

// unpackEntry writes one entry of the repository cache's tar into staging, refusing one a feed does
// not take.
func unpackEntry(staging string, header *tar.Header, body io.Reader) error {
	name := strings.TrimSuffix(header.Name, "/")
	if !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
		return fmt.Errorf("the repository cache's tar holds %q, which is not a path inside the feed", name)
	}
	dir := header.Typeflag == tar.TypeDir
	if !dir && header.Typeflag != tar.TypeReg {
		return fmt.Errorf("the repository cache's tar holds %q, which is neither a regular file nor a directory", name)
	}
	if !CacheEntry(name, dir) {
		return fmt.Errorf("the repository cache's tar holds %q, which is not one of a repository's refs or packs", name)
	}
	target := filepath.Join(staging, filepath.FromSlash(name))
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
// unpackCache's own cancel does as soon as the answer has been read to its end.
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

// feedTransfer is how Fetch brings the repository from GitHub: verb names each attempt in the
// log and in a failure, argv is the attempt, destination, for a clone, is the temporary
// directory the attempt clones into, removed before a retry, and dir is the directory each
// attempt runs in. That is never destination: transferWithRetries removes it between attempts,
// and a child whose working directory has been removed cannot start.
type feedTransfer struct {
	verb        string
	argv        []string
	destination string
	dir         string
}

// stageBeside is the temporary sibling of feed a feed that lacks the repository is filled in. A
// sibling an earlier Fetch left — killed before it renamed or removed it — is removed first and
// logged: only Fetch writes beside the feed's repository, and a killed clone of a large repository
// left on a volume of fixed size would fill it.
func stageBeside(feed string, log func(string)) (string, error) {
	stale, err := filepath.Glob(feed + cloneSuffix + "*")
	if err != nil {
		return "", err
	}
	for _, left := range stale {
		if err := rolefile.RemoveTree(left); err != nil {
			return "", fmt.Errorf("remove the clone an earlier fetch left at %s: %w", left, err)
		}
		log("removed the clone an earlier fetch left at " + left)
	}
	staging, err := os.MkdirTemp(filepath.Dir(feed), filepath.Base(feed)+cloneSuffix)
	if err != nil {
		return "", fmt.Errorf("create the clone's temporary directory beside %s: %w", feed, err)
	}
	return staging, nil
}

// cloneSuffix follows the feed repository's name in the temporary directory it is filled in.
const cloneSuffix = ".clone-"

// feedConfiguration is what the feed repository's own configuration sets: no automatic garbage
// collection or maintenance, ever, and every fetch's objects kept as the pack they arrived in
// (`fetch.unpackLimit=1`). An issue pod's clone reads the feed's objects through its alternates
// (feedClone), so no maintenance may remove an object the clone borrows; it also keeps feed
// refreshes predictable and preserves the repository cache's pack-only stream.
func feedConfiguration(ctx context.Context, run Runner, gitDir string) error {
	for _, pair := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}, {"fetch.unpackLimit", "1"}} {
		if _, err := CacheGit(ctx, run, gitDir, "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// CacheGit runs git on the bare repository gitDir — a feed's, or the repository cache's — under
// the runner's slow-command budget, holding no credential and reading no configuration but that
// repository's own and the pack settings feedGit names: what reads or tends such a repository
// outside Fetch's transfer. It runs in gitDir itself, which is there whenever something reads or
// tends it, so no caller has to name a directory and no child of this package's takes the one the
// process happens to run in.
func CacheGit(ctx context.Context, run Runner, gitDir string, args ...string) (Result, error) {
	return RunChecked(ctx, run, slices.Concat(feedGit, []string{"--git-dir=" + gitDir}, args), isolatedGitConfig, gitDir)
}

// CacheGitWithin is FeedGit bounded by timeout instead of the runner's slow-command budget: for a
// command on such a repository whose duration follows the repository's size, as the cache's repack
// does. A `-c` among args comes after feedGit's, so it wins over them for its key.
func CacheGitWithin(ctx context.Context, run Runner, gitDir string, timeout time.Duration, args ...string) (Result, error) {
	argv := slices.Concat(feedGit, []string{"--git-dir=" + gitDir}, args)
	result, err := runCommandTimeout(ctx, run, argv, isolatedGitConfig, gitDir, timeout)
	return checkedResult(argv, result, err)
}

// transferWithRetries runs transfer under the one-shot credential held until it succeeds, retrying
// a transient failure as Fetch says, each attempt bounded by what remains before deadline and run
// on the provisioning token as it stands when the attempt starts (renew). It answers the attempt
// that succeeded.
func transferWithRetries(ctx context.Context, run Runner, request FetchRequest, transfer feedTransfer, held *fetchCredential, deadline time.Time) (Result, error) {
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
		if transfer.destination != "" {
			if err := rolefile.RemoveTree(transfer.destination); err != nil {
				return Result{}, fmt.Errorf("remove what %s attempt %d left at %s: %w", transfer.verb, attempt, transfer.destination, err)
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

// fetchBackoff is the wait before each retry of the fetch's clone, one retry per entry: a first
// retry soon after a stream GitHub cut, the last a minute later. A clone still failing after the
// last fails the fetch, so a GitHub outage holds a launch for a few attempts, not the whole
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

// transientCloneFailure is whether a clone of GitHub that failed may succeed if run again: git's
// fatal exit, with a cause that names a status GitHub may not answer again — 408 (it timed the
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

// transientStatus is whether an HTTP status is one a server may not answer again: 408, 429, 5xx.
func transientStatus(status string) bool {
	code, err := strconv.Atoi(status)
	return err == nil && (code == 408 || code == 429 || code >= 500 && code <= 599)
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
// (`could not read Username`), an object the feed lacks (`fatal: bad object`), or any other
// failure. transientCloneFailure keeps its own rule, which tells which failures the fetch runs
// again.
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
