package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// CloneBound is CloneTimeout as ensureRepoClone reads it, through programbound: a variable so that
// cmd/legion's tests, whose workspace-init runs the shared clone on a stand-in jj, can give it their
// own deadline, since a stand-in slower than CloneTimeout, as a loaded machine can make one, is
// killed there, and can read the bound the clone took (programbound.Of(&CloneBound)). CloneTimeout
// stays the constant ProvisionBound sums.
var CloneBound = CloneTimeout

func init() {
	programbound.Register(&CloneBound)
}

const provisioningTokenFileEnv = "LEGION_PROVISIONING_TOKEN_FILE"

// provisioningHelper is the one-shot credential: a git credential helper that answers `get` with
// the token. git asks it for https://github.com alone (the helper is
// credential.https://github.com.helper, set after a reset of every helper configured before it),
// so a remote rewritten to another scheme, host, or port gets nothing, and no askpass is asked in
// its place. A credentialed fetch of an issue's clone reads that clone's own configuration, which
// the issue's agents write: the tmux runtime's shared clone, and a Kubernetes issue pod's clone,
// which workspace-init fills on the issue's volume. So there the scope is defence, not a boundary
// (config.go). One example of what it leaves open: an http.proxy the tree wrote with
// http.sslVerify off still sees the token on its way to github.com. It is an example, not the whole
// list. The repository cache's Fetch reads a configuration that holds only keys it writes itself
// (cacheConfigKeys).
const provisioningHelper = `#!/bin/sh
[ "$1" = get ] || exit 0
printf 'username=x-access-token\npassword=%s\n' "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")"
`

// remote is how a clone or fetch reaches the repository: its environment, one-shot credential
// directory and token, ordinary tmux branches, and optional Kubernetes issue-pod fill state.
type remote struct {
	env      []string
	dir      string
	token    string
	branches []string
	issue    *issuePod
}

// scrub is text with the credential's token replaced by ***, so that nothing a run under the
// credential returns carries it, wherever git echoed it. A transfer from GitHub that rewrites the
// token before each attempt (setToken, transferWithRetries) scrubs every token it has held itself
// (fetchCredential.scrub).
func (r remote) scrub(text string) string {
	if r.token == "" {
		return text
	}
	return strings.ReplaceAll(text, r.token, "***")
}

// newProvisioningCredential ports workspace.ts's createProvisioningCredential, the one-shot
// credential. The Go daemon keeps the token in a 0600 file under parent, so the processes it is
// handed to receive only a file pointer and never a secret environment value. GIT_ASKPASS is set
// empty, which git reads as no askpass at all — neither core.askPass nor SSH_ASKPASS — and terminal
// prompts are off. parent is the operator's root, not a role's: the tmux daemon's state directory
// (FromGitHub), or a pod's TMPDIR (ForIssuePod in workspace-init's provision, Fetch in the
// repository cache), either of which may be reached through a symlink. It is resolved once here, so
// the credential directory is made, and removed (rolefile.RemoveTree, which refuses a parent that is
// a link), under the directory the operator's link names, and no link below it is followed.
func newProvisioningCredential(parent, token string) (remote, error) {
	if token == "" {
		return remote{}, errors.New("workspace provisioning token is required")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return remote{}, fmt.Errorf("create provisioning credential parent %s: %w", parent, err)
	}
	root, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return remote{}, fmt.Errorf("resolve provisioning credential parent %s: %w", parent, err)
	}
	directory, err := os.MkdirTemp(root, "provisioning-credential-")
	if err != nil {
		return remote{}, fmt.Errorf("create provisioning credential directory: %w", err)
	}
	cleanup := func(cause error) (remote, error) {
		if removeErr := rolefile.RemoveTree(directory); removeErr != nil {
			return remote{}, fmt.Errorf("%w; remove provisioning credential directory: %v", cause, removeErr)
		}
		return remote{}, cause
	}
	tokenFile := filepath.Join(directory, provisioningTokenName)
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return cleanup(fmt.Errorf("write provisioning token: %w", err))
	}
	helper := filepath.Join(directory, "helper")
	if err := os.WriteFile(helper, []byte(provisioningHelper), 0o700); err != nil {
		return cleanup(fmt.Errorf("write provisioning credential helper: %w", err))
	}
	if err := os.Chmod(helper, 0o700); err != nil {
		return cleanup(fmt.Errorf("chmod provisioning credential helper: %w", err))
	}
	return remote{
		dir:   directory,
		token: token,
		env: []string{
			"GIT_ASKPASS=",
			"GIT_TERMINAL_PROMPT=0",
			provisioningTokenFileEnv + "=" + tokenFile,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.https://github.com.helper",
			"GIT_CONFIG_VALUE_1=!" + shellprefix.Literal(helper),
		},
	}, nil
}

// provisioningTokenName is the one-shot credential's token file, in its directory.
const provisioningTokenName = "token"

// setToken writes token over the one-shot credential's, which git's helper reads at each request
// for it: before each transfer from GitHub (transferWithRetries), with the token as it stands then.
func (r remote) setToken(token string) error {
	if token == "" {
		return errors.New("workspace provisioning token is required")
	}
	if err := os.WriteFile(filepath.Join(r.dir, provisioningTokenName), []byte(token), 0o600); err != nil {
		return fmt.Errorf("write provisioning token: %w", err)
	}
	return nil
}

func (r remote) remove() error {
	if r.dir == "" {
		return nil
	}
	if err := rolefile.RemoveTree(r.dir); err != nil {
		return fmt.Errorf("remove provisioning credential directory: %w", err)
	}
	return nil
}

// Source is how provisioning reaches the repository. The tmux runtime uses FromGitHub; a
// Kubernetes issue pod uses ForIssuePod, which fills its one clone from its repository cache and
// GitHub under the provisioning token.
type Source interface {
	// check refuses a Source that cannot reach repository, before provisioning runs anything.
	check(repository ghrepo.Repository) error
	// open is the remote the clone and the fetch of repository use, with bookmark the issue's own
	// and defaultBranch the repository's; the caller removes it.
	open(repository ghrepo.Repository, bookmark, defaultBranch string) (remote, error)
}

// issuePod is the state ForIssuePod carries for one Kubernetes issue pod: its source's settings,
// the repository, the issue's bookmark and the default branch it was admitted against. fetched
// records that the clone path already fetched GitHub, so the configuration step does not repeat
// that transfer.
type issuePod struct {
	issuePodSource
	repository ghrepo.Repository
	bookmark   string
	defaultRef string
	fetched    bool
}

// ForIssuePod reaches GitHub with the mounted provisioning token and, when cacheURL is non-empty,
// starts the clone from the repository cache. The cache's bearer token is separate from the GitHub
// token, and it carries the cache's /packs answer and its tar stream alike. clock is what the fill's waits read and sleep on:
// its polls of a cache that is not ready and its backoff between transfers from GitHub. bound is
// the time each clocked stage of the fill gets — the check of what the cache streamed, and the
// transfers from GitHub — zero being FetchTimeout.
func ForIssuePod(token func() (string, error), credentialDir, cacheURL, cacheToken string, clock Clock, bound time.Duration, log func(string)) Source {
	return issuePodSource{
		gitHubSource: gitHubSource{token: token, credentialDir: credentialDir},
		cacheURL:     cacheURL,
		cacheToken:   cacheToken,
		clock:        clock,
		bound:        bound,
		log:          log,
	}
}

type issuePodSource struct {
	gitHubSource
	cacheURL   string
	cacheToken string
	clock      Clock
	bound      time.Duration
	log        func(string)
}

func (s issuePodSource) check(repository ghrepo.Repository) error {
	if err := s.gitHubSource.check(repository); err != nil {
		return err
	}
	if s.cacheURL != "" && s.cacheToken == "" {
		return errors.New("workspace provision: a repository cache needs its token")
	}
	if s.clock == nil {
		return errors.New("workspace provision clock is required")
	}
	if s.log == nil {
		return errors.New("workspace provision log is required")
	}
	return nil
}

func (s issuePodSource) open(repository ghrepo.Repository, bookmark, defaultBranch string) (remote, error) {
	token, err := s.token()
	if err != nil {
		return remote{}, OutsideVolume(err)
	}
	r, err := newProvisioningCredential(s.credentialDir, token)
	if err != nil {
		return remote{}, err
	}
	r.issue = &issuePod{issuePodSource: s, repository: repository, bookmark: bookmark, defaultRef: defaultBranch}
	return r, nil
}

// FromGitHub is GitHub itself, reached with the one-shot credential holding the token that token
// answers, created under credentialDir and removed again: the tmux runtime, which names its state
// directory. token is asked when the provision opens the source, under the shared clone's store
// lock (Request.StoreLock), never before: a provision may wait on that lock behind a repository's
// whole queue of provisions, and a token minted before the wait could expire in it. Its fetch
// brings only the repository's default branch and the issue's own branch, by their exact names:
// it runs under the store lock, and every branch of a busy repository would hold every other
// issue's provision behind it. A pane's own `jj git fetch` brings the rest when it needs them.
func FromGitHub(token func() (string, error), credentialDir string) Source {
	return gitHubSource{token: token, credentialDir: credentialDir}
}

type gitHubSource struct {
	token         func() (string, error)
	credentialDir string
}

func (s gitHubSource) check(ghrepo.Repository) error {
	if s.credentialDir == "" {
		return errors.New("workspace credential directory is required")
	}
	if s.token == nil {
		return errors.New("workspace provisioning token is required")
	}
	return nil
}

func (s gitHubSource) open(_ ghrepo.Repository, bookmark, defaultBranch string) (remote, error) {
	token, err := s.token()
	if err != nil {
		return remote{}, err
	}
	r, err := newProvisioningCredential(s.credentialDir, token)
	if err != nil {
		return remote{}, err
	}
	r.branches = []string{defaultBranch, bookmark}
	return r, nil
}

// CacheRepository is where Fetch clones a repository under the repository cache's root:
// <root>/<owner>/<name>.git, the layout the repository cache's volume shares. The zero repository
// is refused, as Location refuses it: it could name the root's own `.git`.
func CacheRepository(root string, repository ghrepo.Repository) (string, error) {
	if repository.IsZero() {
		return "", errors.New("workspace repository is required")
	}
	if root == "" {
		return "", errors.New("workspace cache root is required")
	}
	return filepath.Join(root, repository.Owner(), repository.Name()+".git"), nil
}

// CacheEntry is whether name — a path inside a bare repository, slash-separated and relative to it,
// a directory when dir is set — is one of the refs and packs the repository cache streams into an
// issue pod's clone: HEAD, packed-refs, what lies under refs/, and each pack of objects/pack/, both
// its pack-<id>.pack and the pack-<id>.idx git reads it through. The index comes with the pack
// because building it again costs every issue pod minutes of CPU on a large repository (`git
// index-pack` inflates and hashes every object); the clone holds both to the checksums git wrote
// into them and takes the index's names on the cache's word, which verifyCache states exactly. Never
// a loose object, a pack's reverse index or bitmap, a multi-pack index or a commit-graph, which git
// builds for itself; never the repository's own configuration, hooks, info or description, which
// the clone makes for itself, nor objects/info/alternates or http-alternates, which would point the
// clone at objects elsewhere; and never a file or directory git is still writing (tmp_*,
// incoming-*, *.lock).
func CacheEntry(name string, dir bool) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, "tmp_") || strings.HasPrefix(base, "incoming-") || strings.HasSuffix(base, ".lock") {
		return false
	}
	switch {
	case dir:
		return name == "refs" || strings.HasPrefix(name, "refs/") || name == "objects" || name == "objects/pack"
	case name == "HEAD" || name == "packed-refs" || strings.HasPrefix(name, "refs/"):
		return true
	case path.Dir(name) == "objects/pack":
		return packFile.MatchString(base)
	}
	return false
}

// packFile is the name git gives a pack and the index it reads it through: the pack's SHA-1 or
// SHA-256 id.
var packFile = regexp.MustCompile(`^pack-([0-9a-f]{40}|[0-9a-f]{64})\.(pack|idx)$`)

// GitHubURL is the repository's GitHub remote.
func GitHubURL(repository ghrepo.Repository) string {
	return "https://github.com/" + repository.String()
}

// ensureRepoClone makes a clone in a temporary sibling, verifies its Jujutsu repository, then
// renames it into place. A Kubernetes issue pod initializes a Git repository, optionally unpacks
// the repository cache into its .git directory, and fetches the default and issue refs from GitHub.
// The tmux runtime runs `jj git clone` from GitHub. The size-following steps share one
// CloneTimeout, counted on clock from the first's start, each given what remains of it
// (cloneSteps), not the runner's slow-command budget; an issue pod's fill starts it afresh once its
// transfers are done (issuePodClone).
func ensureRepoClone(ctx context.Context, run Runner, clock Clock, cloneDir, remote string, source remote) error {
	jjDir := filepath.Join(cloneDir, ".jj")
	if exists, err := pathExists(cloneDir); err != nil {
		return err
	} else if exists {
		if complete, err := pathExists(jjDir); err != nil {
			return err
		} else if complete {
			return nil
		}
		if err := rolefile.RemoveTree(cloneDir); err != nil {
			return fmt.Errorf("remove incomplete clone %s: %w", cloneDir, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cloneDir), 0o700); err != nil {
		return fmt.Errorf("create clone parent: %w", err)
	}
	var temporary string
	var err error
	if source.issue != nil {
		temporary, err = stageBeside(cloneDir, source.issue.log)
	} else {
		temporary, err = os.MkdirTemp(filepath.Dir(cloneDir), filepath.Base(cloneDir)+cloneSuffix)
	}
	if err != nil {
		return fmt.Errorf("create temporary clone directory: %w", err)
	}
	defer func() {
		_ = rolefile.RemoveTree(temporary)
	}()

	// Every step runs in the clone's parent, the directory os.MkdirAll made above: it is there
	// before the first step starts, and it outlives them all, where temporary is filled by the
	// steps themselves and then renamed or removed.
	bound := programbound.Of(&CloneBound)
	steps := cloneSteps{clock: clock, deadline: clock.Now().Add(bound), bound: bound, dir: filepath.Dir(cloneDir), credential: source}
	if source.issue != nil {
		if err := issuePodClone(ctx, run, steps, temporary, source); err != nil {
			return err
		}
	} else if err := steps.run(ctx, run, []string{"jj", "git", "clone", remote, temporary}, source.env); err != nil {
		return err
	}
	complete, err := pathExists(filepath.Join(temporary, ".jj"))
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("incomplete Jujutsu clone at %s: missing .jj", temporary)
	}
	if err := os.Rename(temporary, cloneDir); err != nil {
		if cloneCollision(err) {
			complete, statErr := pathExists(jjDir)
			if statErr == nil && complete {
				return nil
			}
		}
		return fmt.Errorf("rename cloned repository into place: %w", err)
	}
	return nil
}

// issuePodClone fills the pod's only repository. The cache's refs, which come with its packs from
// its bucket or its /tar (StreamCache), serve solely for Git's pack negotiation; the GitHub fetch
// and the cleanup below establish every ref the clone keeps. The log says which of the cache's
// sources filled the clone (logCacheSource), then its summary line.
func issuePodClone(ctx context.Context, run Runner, steps cloneSteps, temporary string, source remote) error {
	issue := source.issue
	gitDir := filepath.Join(temporary, ".git")
	if err := steps.run(ctx, run, []string{"git", "init", "--quiet", temporary}, source.env); err != nil {
		return err
	}

	var stream StreamReport
	if issue.cacheURL != "" {
		var err error
		stream, err = StreamCache(ctx, run, CacheStream{
			Repo: issue.repository, URL: issue.cacheURL, Token: issue.cacheToken,
			Log: issue.log, Clock: issue.clock, Bound: issue.bound,
		}, gitDir)
		logCacheSource(issue.log, stream)
		if err != nil {
			return err
		}
	}

	bytes, refs, defaults, err := fetchIssuePod(ctx, run, temporary, source, true)
	if err != nil {
		return err
	}
	// Cache readiness, streaming, verification and GitHub each have their own bounds.
	// Only the Jujutsu setup below spends this clone budget.
	steps.deadline = steps.clock.Now().Add(steps.bound)
	if err := steps.run(ctx, run, []string{"jj", "git", "init", "--colocate", temporary}, nil); err != nil {
		return err
	}
	if err := steps.runIn(ctx, run, temporary, []string{"jj", "sparse", "set", "--clear", "--color=never", "-R", temporary}, nil); err != nil {
		return err
	}
	if err := steps.runIn(ctx, run, temporary, []string{"jj", "abandon", "@", "--color=never", "-R", temporary}, nil); err != nil {
		return err
	}
	for _, branch := range defaults {
		track, err := onClone(temporary, "bookmark", "track", originRow(branch))
		if err != nil {
			return err
		}
		if err := steps.runIn(ctx, run, temporary, track, nil); err != nil {
			return err
		}
	}
	issue.fetched = true
	if issue.cacheURL == "" {
		issue.log(fmt.Sprintf("no repository cache named: fetched the default branch of %s from GitHub (%s bytes)", issue.repository, groupedBytes(bytes)))
		return nil
	}
	cache, size, took := issue.cacheURL, stream.Bytes, stream.Duration
	if stream.Source == SourceBucket {
		cache, size, took = issue.cacheURL+" (bucket)", stream.BucketBytes, stream.BucketDuration
	}
	issue.log(fmt.Sprintf("%s bytes from the repository cache %s in %s, its history verified in %s; %s bytes from GitHub for the delta (%s)",
		groupedBytes(size), cache, took.Round(time.Millisecond), stream.VerifyDuration.Round(time.Millisecond), groupedBytes(bytes), refCount(refs)))
	return nil
}

// logCacheSource says which of the repository cache's sources filled a fresh clone, once
// StreamCache chose one, before the fill's summary line or its failure: `source=bucket`, then one
// line for each pack and index downloaded and verified, `bucket object=<name> size=<bytes>
// sha256=<hex>`, the object's name as the cache's manifest named it, its exact size and the
// SHA-256 the fill computed of it, which matched the manifest's, for a reader to compare with the
// bucket's own listing; or `source=/tar reason=<why the bucket was not used>`, `/packs is not
// served` for a cache with no bucket or of a release before it. A reason is one line, and names no
// signed address, query, token or ref's contents (StreamReport.TarReason). A report that chose no
// source says nothing.
func logCacheSource(log func(string), stream StreamReport) {
	switch stream.Source {
	case SourceBucket:
		log("source=bucket")
		for _, object := range stream.Objects {
			log(fmt.Sprintf("bucket object=%s size=%d sha256=%s", object.Name, object.Size, object.SHA256))
		}
	case SourceTar:
		log("source=/tar reason=" + strings.Join(strings.Fields(stream.TarReason), " "))
	}
}

// fetchIssuePod configures origin's persistent narrow refspecs and fetches them without tags.
// Only a fresh clone drops cache refs outside this issue; kept clones retain their legacy refs. It
// answers how many bytes the fetch added to the clone's objects, how many of origin's refs it
// moved (movedRemoteRefs), and the default branches it brought (issuePodDefaults), the one the
// clone's HEAD names first.
func fetchIssuePod(ctx context.Context, run Runner, cloneDir string, source remote, fresh bool) (int64, int, []string, error) {
	issue := source.issue
	gitDir := filepath.Join(cloneDir, ".git")
	request := FetchRequest{Repo: issue.repository, Log: issue.log, Clock: issue.clock, Bound: issue.bound}
	held := newFetchCredential(source, issue.token)
	deadline := issue.clock.Now().Add(request.bound())
	defaults, err := issuePodDefaults(ctx, run, request, gitDir, issue.defaultRef, held, deadline)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(defaults) == 0 {
		return 0, 0, nil, fmt.Errorf("GitHub names no default branch for %s", issue.repository)
	}
	defaultBranch := defaults[0]
	configured := issuePodRefspecs(defaultBranch, issue.bookmark)
	fetchRefs := append([]string{}, configured...)
	for _, branch := range defaults[1:] {
		fetchRefs = append(fetchRefs, "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	}
	if err := configureIssueOrigin(ctx, run, cloneDir, issue.repository, configured); err != nil {
		return 0, 0, nil, err
	}
	before, err := objectsSize(gitDir)
	if err != nil {
		return 0, 0, nil, err
	}
	refsBefore, err := repositoryRefs(ctx, run, gitDir)
	if err != nil {
		return 0, 0, nil, err
	}
	argv := slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir, "fetch", "--no-tags", "--prune", "--quiet", GitHubURL(issue.repository)}, fetchRefs)
	if _, err := transferWithRetries(ctx, run, request, gitHubTransfer{verb: "fetch", argv: argv, dir: cloneDir}, held, deadline); err != nil {
		return 0, 0, nil, err
	}
	after, err := objectsSize(gitDir)
	if err != nil {
		return 0, 0, nil, err
	}
	refsAfter, err := repositoryRefs(ctx, run, gitDir)
	if err != nil {
		return 0, 0, nil, err
	}
	if err := setIssueHeads(ctx, run, gitDir, defaults); err != nil {
		return 0, 0, nil, err
	}
	if fresh {
		if err := keepIssueRefs(ctx, run, gitDir, defaults, issue.bookmark); err != nil {
			return 0, 0, nil, err
		}
	}
	// origin/HEAD names GitHub's default branch, as a clone leaves it: `jj git init --colocate`
	// reads it to set the repository's trunk() alias, which without it resolves to the root commit
	// for a default branch not named main, master or trunk.
	if _, err := CacheGit(ctx, run, gitDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+defaultBranch); err != nil {
		return 0, 0, nil, err
	}
	return after - before, movedRemoteRefs(refsBefore, refsAfter), defaults, nil
}

// movedRemoteRefs is how many of origin's remote-tracking refs a fetch made, moved or deleted:
// the count the fill's log line reports.
func movedRemoteRefs(before, after map[string]string) int {
	moved := 0
	for ref, object := range after {
		if strings.HasPrefix(ref, "refs/remotes/origin/") && before[ref] != object {
			moved++
		}
	}
	for ref := range before {
		if _, kept := after[ref]; strings.HasPrefix(ref, "refs/remotes/origin/") && !kept {
			moved++
		}
	}
	return moved
}

// issuePodDefaults reads GitHub's HEAD and keeps the admission-time default when GitHub still
// advertises it. This lets an unpushed issue start at the branch it was admitted against. It
// answers their branch names, HEAD's first.
func issuePodDefaults(ctx context.Context, run Runner, request FetchRequest, gitDir, recordedDefault string, held *fetchCredential, deadline time.Time) ([]string, error) {
	head, branches, err := remoteRefs(ctx, run, request, gitDir, held, deadline, "refs/heads/*")
	if err != nil {
		return nil, err
	}
	if head != "" && branches[head] == "" {
		return nil, fmt.Errorf("GitHub's HEAD of %s names %q, not a branch", request.Repo, head)
	}
	defaults := []string{}
	if head != "" {
		defaults = append(defaults, strings.TrimPrefix(head, "refs/heads/"))
	}
	recorded := "refs/heads/" + recordedDefault
	if recordedDefault != "" && branches[recorded] != "" && recorded != head {
		defaults = append(defaults, recordedDefault)
	}
	return defaults, nil
}

// issuePodRefspecs is the persistent fetch configuration of an issue clone: GitHub's current
// default branch, the issue's wildcard, and the ten exclusions that stop LEGION-10 matching
// LEGION-1. The recorded default is deliberately fetched only by provision, because an exact
// configured refspec for a deleted branch makes a later plain git fetch fail.
func issuePodRefspecs(defaultBranch, bookmark string) []string {
	branch := "refs/heads/" + bookmark
	refspecs := []string{
		"+refs/heads/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch,
		"+" + branch + "*:refs/remotes/origin/" + bookmark + "*",
	}
	for digit := '0'; digit <= '9'; digit++ {
		refspecs = append(refspecs, "^"+branch+string(digit)+"*")
	}
	return refspecs
}

func configureIssueOrigin(ctx context.Context, run Runner, cloneDir string, repository ghrepo.Repository, refspecs []string) error {
	gitDir := filepath.Join(cloneDir, ".git")
	commands := [][]string{
		{"git", "--git-dir=" + gitDir, "config", "--replace-all", "remote.origin.url", GitHubURL(repository)},
		{"git", "--git-dir=" + gitDir, "config", "--replace-all", "remote.origin.fetch", refspecs[0]},
		{"git", "--git-dir=" + gitDir, "config", "--replace-all", "remote.origin.tagOpt", "--no-tags"},
	}
	for _, refspec := range refspecs[1:] {
		commands = append(commands, []string{"git", "--git-dir=" + gitDir, "config", "--add", "remote.origin.fetch", refspec})
	}
	for _, argv := range commands {
		if _, err := RunChecked(ctx, run, argv, nil, cloneDir); err != nil {
			return err
		}
	}
	return nil
}

// setIssueHeads makes a local branch of each of defaults at origin's, and points HEAD at the
// first.
func setIssueHeads(ctx context.Context, run Runner, gitDir string, defaults []string) error {
	for _, branch := range defaults {
		if _, err := CacheGit(ctx, run, gitDir, "update-ref", "refs/heads/"+branch, "refs/remotes/origin/"+branch); err != nil {
			return err
		}
	}
	_, err := CacheGit(ctx, run, gitDir, "symbolic-ref", "HEAD", "refs/heads/"+defaults[0])
	return err
}

// keepIssueRefs deletes every ref of a fresh clone but origin's refs of defaults and of the
// issue's branches (issueBranch), and the local branch HEAD names, the first of defaults.
func keepIssueRefs(ctx context.Context, run Runner, gitDir string, defaults []string, bookmark string) error {
	refs, err := repositoryRefs(ctx, run, gitDir)
	if err != nil {
		return err
	}
	for ref := range refs {
		keep := false
		if name, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok {
			keep = slices.Contains(defaults, name) || issueBranch(name, bookmark)
		} else {
			keep = ref == "refs/heads/"+defaults[0]
		}
		if !keep {
			if _, err := CacheGit(ctx, run, gitDir, "update-ref", "--no-deref", "-d", ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// issueBranch is whether name is one the issue's configured refspecs bring (issuePodRefspecs):
// bookmark itself or bookmark followed by anything but a digit.
func issueBranch(name, bookmark string) bool {
	rest, ok := strings.CutPrefix(name, bookmark)
	return ok && (rest == "" || rest[0] < '0' || rest[0] > '9')
}

// groupedBytes is n with its digits grouped in threes by commas: 6,612,345,678.
func groupedBytes(n int64) string {
	digits := strconv.FormatInt(n, 10)
	grouped := make([]byte, 0, len(digits)+len(digits)/3)
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 && digits[i-1] != '-' {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, digits[i])
	}
	return string(grouped)
}

// refCount is how many refs a transfer changed, as a count of refs.
func refCount(n int) string {
	if n == 1 {
		return "1 ref"
	}
	return strconv.Itoa(n) + " refs"
}

// cloneSteps is the one CloneTimeout the shared clone's size-following steps share, ending at
// deadline by clock: ProvisionBound budgets one CloneTimeout for workspace-init's clone. bound is
// that bound as the clone read it (CloneBound), which a step with nothing left names. dir is
// where a step runs that opens no repository (`jj git clone`, `jj git init --colocate`): the
// directory the shared clone is made beside, since a jj that opens no workspace would otherwise
// search upwards from whatever directory the daemon or a test happens to run in and write the
// repository it finds there. A step that does open a repository names its own directory instead,
// the directory its own -R names (runIn). credential is the clone's remote, whose token every
// step's stderr is scrubbed of before the step's error is made from it.
type cloneSteps struct {
	clock      Clock
	deadline   time.Time
	bound      time.Duration
	dir        string
	credential remote
}

// run runs one size-following step of the shared clone's clone in s.dir, the directory for a step
// that opens no repository.
func (s cloneSteps) run(ctx context.Context, run Runner, argv, env []string) error {
	return s.runIn(ctx, run, s.dir, argv, env)
}

// runIn runs one size-following step of the shared clone's clone in dir, under what remains of the
// shared CloneTimeout, checked as RunChecked checks its commands; a step with nothing left fails,
// naming the bound, without running.
func (s cloneSteps) runIn(ctx context.Context, run Runner, dir string, argv, env []string) error {
	left, err := remaining(s.clock, s.deadline, "shared clone's", s.bound, strings.Join(argv, " "))
	if err != nil {
		return err
	}
	result, err := runCommandTimeout(ctx, run, argv, env, dir, left)
	result.Stderr = s.credential.scrub(result.Stderr)
	_, err = checkedResult(argv, result, err)
	return err
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func cloneCollision(err error) bool {
	return errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) || strings.Contains(err.Error(), "file exists")
}
