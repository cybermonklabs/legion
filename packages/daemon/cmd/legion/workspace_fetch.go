package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// cacheURLEnv is where the runtime names the repository's cache to the fetch container, beside its
// token's file (LEGION_REPOSITORY_CACHE_TOKEN_FILE); --cache-url defaults to it.
const cacheURLEnv = "LEGION_REPOSITORY_CACHE_URL"

// defaultBranchEnv is where the runtime names the default branch it recorded for the issue, which
// a fetch from the cache keeps current beside the one GitHub names now (workspace.FetchRequest's
// RecordedDefault); unset or empty, there is none.
const defaultBranchEnv = "LEGION_DEFAULT_BRANCH"

// workspaceFetch fills the feed, the container's own directory on the issue's volume: from the
// repository cache when one is named, presenting the token LEGION_REPOSITORY_CACHE_TOKEN_FILE
// holds, then from GitHub the default branch, the issue's recorded default and the Legion issue
// branches; or from GitHub alone. The provisioning token is read from LEGION_PROVISION_TOKEN_FILE
// again before every transfer from GitHub, as the file stands then (the daemon keeps the Secret it
// is projected from fresh for the pod's whole life, whatever this fetch has done), so a long wait
// on the cache never leaves the delta an expired token. It makes every check first (fetchInputs)
// and refuses before it ever touches the feed or its own TMPDIR, where the one-shot credential
// goes, so a refusal here leaves neither behind. Each refusal has a cause outside the issue's
// volume (workspace.ErrOutsideVolume), and so do the failures workspace.Fetch reads as such. A
// transfer that fails transiently is run again (workspace.Fetch), each retried failure one line
// on stdout, and the last line says where the feed came from and the bytes each source brought.
// What it reads of its process — each variable, the PATH, TMPDIR, the clock and the bound — it
// reads from ws.
func workspaceFetch(ctx context.Context, ws workspaceProcess, repo, feed, cacheURL string, stdout io.Writer) error {
	inputs, err := fetchInputs(ws, repo, feed, cacheURL)
	if err != nil {
		// Every check before the first write to the volume is fetchInputs', so this one mark
		// covers each of them, and a check added there later too (issuePodInitExit).
		return workspace.OutsideVolume(err)
	}
	if err := os.MkdirAll(feed, 0o700); err != nil {
		return fmt.Errorf("create the feed directory %s: %w", feed, err)
	}
	report, err := workspace.Fetch(ctx, ws.runner(programbound.Of(&commandBudget), inputs.tools), workspace.FetchRequest{
		Repo: inputs.repository, Token: inputs.token, CredentialDir: ws.tempDir(), Feed: feed,
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init fetch: "+line) }, Clock: ws.clock, Bound: ws.fetchBound, CacheURL: cacheURL, CacheToken: inputs.cacheToken,
		RecordedDefault: ws.getenv(defaultBranchEnv),
	})
	if err != nil {
		return err
	}
	switch {
	case report.CacheBytes > 0:
		fmt.Fprintf(stdout, "workspace-init fetch: %s bytes from the repository cache %s in %s, its history verified in %s; %s bytes from GitHub for the delta (%s)\n",
			groupedBytes(report.CacheBytes), cacheURL, report.CacheDuration.Round(time.Millisecond), report.VerifyDuration.Round(time.Millisecond), groupedBytes(report.GitHubBytes), refCount(report.Refs))
	case report.Cloned:
		fmt.Fprintf(stdout, "workspace-init fetch: no repository cache named: cloned %s from GitHub (%s bytes)\n", inputs.repository, groupedBytes(report.GitHubBytes))
	default:
		fmt.Fprintf(stdout, "workspace-init fetch: the feed holds %s already: %s bytes from GitHub for the delta (%s)\n", inputs.repository, groupedBytes(report.GitHubBytes), refCount(report.Refs))
	}
	return nil
}

// fetchInput is what `fetch` reads before it touches the volume: the repository, the cache's
// token ("" with no cache named), the reader of the provisioning token and the tools it runs.
type fetchInput struct {
	repository ghrepo.Repository
	cacheToken string
	token      func() (string, error)
	tools      map[string]string
}

// fetchInputs is every check `fetch` makes before it touches the volume, and what those checks
// read: --repo before anything else, then --feed, --cache-url and its token, a readable
// provisioning token, and git alone on PATH. It writes nothing, so no refusal of it has a cause
// on the volume.
func fetchInputs(ws workspaceProcess, repo, feed, cacheURL string) (fetchInput, error) {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return fetchInput{}, err
	}
	if !filepath.IsAbs(feed) {
		return fetchInput{}, fmt.Errorf("--feed must be an absolute path (got %q)", feed)
	}
	inputs := fetchInput{repository: repository}
	if cacheURL != "" {
		if parsed, err := url.Parse(cacheURL); err != nil || parsed.Scheme != "http" || parsed.Host == "" {
			return fetchInput{}, fmt.Errorf("--cache-url must be an http:// URL (got %q)", cacheURL)
		}
		tokenFile, set := ws.lookup(repocache.CacheTokenFileEnv)
		if !set {
			return fetchInput{}, errors.New(repocache.CacheTokenFileEnv + " is not set: a repository cache needs its token")
		}
		if inputs.cacheToken, err = config.ReadSecretPointer(repocache.CacheTokenFileEnv, tokenFile); err != nil {
			return fetchInput{}, err
		}
	}
	tokenFile, err := ws.secretFile(provisionTokenFileEnv)
	if err != nil {
		return fetchInput{}, err
	}
	inputs.token = func() (string, error) { return config.ReadSecretPointer(provisionTokenFileEnv, tokenFile) }
	if inputs.tools, err = ws.tools("git"); err != nil {
		return fetchInput{}, err
	}
	return inputs, nil
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
