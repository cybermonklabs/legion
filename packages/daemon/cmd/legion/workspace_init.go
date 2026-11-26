package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceProvisionUsage = "legion workspace-init provision --issue <KEY> --repo <owner>/<repo> --default-branch <name> [--root /legion] --credential-helper <git helper>"

// provisionTokenFileEnv points `workspace-init provision` at the mounted provisioning token.
const provisionTokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"

// cacheURLEnv names the ready repository cache that provision streams before fetching its issue
// delta from GitHub.
const cacheURLEnv = "LEGION_REPOSITORY_CACHE_URL"

// newProvisionRunner is the runner `provision`'s commands run under, workspace.NewRunner given
// commandBudget: the slow-command budget of every command carrying no bound of its own, the
// commands that carry one being the exceptions workspace.CommandTimeout names, the issue's
// checkout among them (workspace.Provision, Request.CheckoutBound). This package's tests wrap it to
// read the bound each command was given.
var newProvisionRunner = workspace.NewRunner

// commandBudget is the slow-command budget of the runners workspace-init's provision and cache
// run their commands under: workspace.CommandTimeout, 5 min. It is a variable so that this
// package's tests can give it their own deadline (programbound): a stand-in jj that a loaded machine
// made slower than the budget would be killed, and the row would read the kill.
var commandBudget = workspace.CommandTimeout

func init() {
	programbound.Register(&commandBudget)
}

// secretFileFromEnv is the secret file the environment variable name points at, once it reads
// (config.ReadSecretPointer); an error when name is not set or its file does not read.
func secretFileFromEnv(name string) (string, error) {
	file, set := os.LookupEnv(name)
	if !set {
		return "", errors.New(name + " is not set")
	}
	if _, err := config.ReadSecretPointer(name, file); err != nil {
		return "", err
	}
	return file, nil
}

// newWorkspaceInitCommand is `legion workspace-init`, the Kubernetes runtime's init container and
// repository cache process. An issue pod runs one `provision` init container, which fills its clone
// and makes its Jujutsu workspace while it holds the provisioning Secret. A repository cache pod
// runs `cache` as its one container, on its own volume, until it is stopped. Each one's log lines
// go to stdout and its refusals and failures to stderr. A usage error exits 2
// (workspace.InitExitUsage); `provision` exits 3 (workspace.InitExitOutsideVolume) for a failure
// whose cause is outside the issue volume, and 1 for every other failure. `cache` exits 1 for every
// failure (workspaceInitExit).
func newWorkspaceInitCommand(stdout, stderr io.Writer) *cobra.Command {
	stdout = redact.Writer(stdout)
	return group(&cobra.Command{
		Use:    "workspace-init",
		Short:  "an issue pod's init container and a repository cache pod's process",
		Hidden: true,
	},
		newWorkspaceProvisionCommand(stdout, stderr),
		newWorkspaceCacheCommand(stdout, stderr),
	)
}

func newWorkspaceProvisionCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: strings.TrimPrefix(workspaceProvisionUsage, "legion workspace-init "), Short: "fill the issue clone and provision its jj workspace on the pod's disk", Hidden: true, Args: noArguments}
	issue := cmd.Flags().String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
	repo := cmd.Flags().String("repo", "", "repository as <owner>/<name> (required)")
	defaultBranch := cmd.Flags().String("default-branch", "", "the repository's default branch as GitHub names it, where an issue with no branch starts (required)")
	root := cmd.Flags().String("root", "/legion", "issue volume root directory")
	credentialHelper := cmd.Flags().String("credential-helper", "", "git credential helper written into the clone's config (required)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		defer podLogs(stderr)()
		return issuePodInitExit(cmd, workspaceInit(ctx, *issue, *repo, *defaultBranch, *root, *credentialHelper, stdout), stderr)
	})
	return cmd
}

// workspaceInitExit is a subcommand's exit status once it has run: 0, or its failure on stderr
// with 1.
func workspaceInitExit(cmd *cobra.Command, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "%s: %v\n", cmd.CommandPath(), err)
	return 1
}

// issuePodInitExit is the exit status of an issue pod's init container, `provision`,
// once it has run: workspaceInitExit's, except workspace.InitExitOutsideVolume for a failure whose
// cause is outside the issue's volume (workspace.ErrOutsideVolume). A new volume does not cure such
// a cause, so the runtime keeps a kept volume for this exit.
func issuePodInitExit(cmd *cobra.Command, err error, stderr io.Writer) int {
	code := workspaceInitExit(cmd, err, stderr)
	if code != 0 && errors.Is(err, workspace.ErrOutsideVolume) {
		return workspace.InitExitOutsideVolume
	}
	return code
}

// workspaceInit validates everything before it touches the disk (provisionInputs), then removes
// stale locks and fills the issue clone and workspace. The one init container has the provisioning
// Secret mounted and creates its one-shot credential under TMPDIR, which it removes before the
// launcher starts. A kept volume reuses its clone and workspace; every provision updates the clone
// from GitHub under its narrow refspecs.
func workspaceInit(ctx context.Context, issue, repo, defaultBranch, root, credentialHelper string, stdout io.Writer) error {
	inputs, err := provisionInputs(issue, repo, defaultBranch, root, credentialHelper)
	if err != nil {
		// Every check before the first write to the volume is provisionInputs', so this one mark
		// covers each of them, and a check added there later too (issuePodInitExit).
		return workspace.OutsideVolume(err)
	}

	log := func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) }
	if err := workspace.RemoveStaleGitLocks(inputs.located, time.Now(), log); err != nil {
		return err
	}
	run := newProvisionRunner(programbound.Of(&commandBudget), inputs.tools)
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: inputs.repository, Issue: issue, DefaultBranch: defaultBranch, CredentialHelper: credentialHelper,
		Source: workspace.ForIssuePod(inputs.token, os.TempDir(), inputs.cacheURL, inputs.cacheToken, clock.Real(), log), Log: log,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init: %s on %s from %s\n", provisioned.Dir, provisioned.Bookmark, provisioned.DefaultBranch)
	return nil
}

// provisionInput is what `provision` reads before it touches the volume: the repository, the
// credentials, the tools it runs and the issue's workspace on the volume.
type provisionInput struct {
	repository ghrepo.Repository
	tools      map[string]string
	located    workspace.Workspace
	token      func() (string, error)
	cacheURL   string
	cacheToken string
}

// provisionInputs is every check `provision` makes before it touches the volume, --repo first,
// then the cache inputs, the readable provisioning token and the provisioner's tools. It writes
// nothing, so no refusal of it has a cause on the volume.
func provisionInputs(issue, repo, defaultBranch, root, credentialHelper string) (provisionInput, error) {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return provisionInput{}, err
	}
	if !legionclaim.IsIssueKey(issue) {
		return provisionInput{}, fmt.Errorf("--issue must be a Dispatch issue key like LEGION-1 (got %q)", issue)
	}
	if !filepath.IsAbs(root) {
		return provisionInput{}, fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if credentialHelper == "" {
		return provisionInput{}, fmt.Errorf("--credential-helper is required: %s", workspaceProvisionUsage)
	}
	if defaultBranch == "" {
		return provisionInput{}, fmt.Errorf("--default-branch is required: %s", workspaceProvisionUsage)
	}
	inputs := provisionInput{repository: repository, cacheURL: os.Getenv(cacheURLEnv)}
	if inputs.cacheURL != "" {
		if parsed, err := url.Parse(inputs.cacheURL); err != nil || parsed.Scheme != "http" || parsed.Host == "" {
			return provisionInput{}, fmt.Errorf("%s must be an http:// URL (got %q)", cacheURLEnv, inputs.cacheURL)
		}
		tokenFile, set := os.LookupEnv(repocache.CacheTokenFileEnv)
		if !set {
			return provisionInput{}, errors.New(repocache.CacheTokenFileEnv + " is not set: a repository cache needs its token")
		}
		if inputs.cacheToken, err = config.ReadSecretPointer(repocache.CacheTokenFileEnv, tokenFile); err != nil {
			return provisionInput{}, err
		}
	}
	tokenFile, err := secretFileFromEnv(provisionTokenFileEnv)
	if err != nil {
		return provisionInput{}, err
	}
	inputs.token = func() (string, error) { return config.ReadSecretPointer(provisionTokenFileEnv, tokenFile) }
	if inputs.tools, err = provisioningTools("git", "jj"); err != nil {
		return provisionInput{}, err
	}
	located, err := workspace.Location(root, repository, issue)
	if err != nil {
		return provisionInput{}, err
	}
	inputs.located = located
	return inputs, nil
}

// provisioningTools resolves the named tools a provisioning step runs from PATH — in an init
// container the image's, with no operator rc ahead of them — where the
// TypeScript runner found them (processEnvRunner, workspace-init.ts).
func provisioningTools(names ...string) (map[string]string, error) {
	tools := map[string]string{}
	for _, tool := range names {
		path, err := exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("%s is not on PATH: %w", tool, err)
		}
		tools[tool] = path
	}
	return tools, nil
}
