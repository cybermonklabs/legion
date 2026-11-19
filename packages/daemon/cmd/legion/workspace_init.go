package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceProvisionUsage = "legion workspace-init provision --issue <KEY> --repo <owner>/<repo> --default-branch <name> [--root /legion] --credential-helper <git helper> --feed <dir>"

// provisionTokenFileEnv points `workspace-init fetch` at the mounted provisioning token.
const provisionTokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"

// provisionBudget is the runner's slow-command budget for `provision`, workspace.CommandTimeout:
// what the runner gives a command carrying no bound of its own, the commands that carry one being
// the exceptions workspace.CommandTimeout names. A test shortens it to prove the issue's checkout
// takes no part of it, holding workspace.CheckoutTimeout of its own instead (workspace.Provision,
// Request.CheckoutBound).
var provisionBudget = workspace.CommandTimeout

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

// newWorkspaceInitCommand is `legion workspace-init`, the Kubernetes runtime's init containers,
// which prepare a pod's disk before the main container's worker-shim starts, and the repository
// cache's process. An issue pod runs two of them, which prepare the issue's clone and jj workspace
// on the pod's own disk: `fetch` is the first, the one process of the pod that holds the
// provisioning token, in a container that mounts nothing an agent of the issue can write;
// `provision` is the last, all the disk's work, in a container the provisioning Secret is not
// mounted in. The controller's pod (`controller: daemon`) runs none. A repository cache's pod runs
// `cache` as its one container, on its own volume, until it is stopped. Each one's log lines go to
// stdout and its refusals and failures to stderr — together the init log the runtime quotes. A
// usage error exits 2 (workspace.InitExitUsage), as for every legion command. `fetch` and
// `provision` exit 3 (workspace.InitExitOutsideVolume) for a failure whose cause is outside the
// issue's volume, and 1 for every other failure (issuePodInitExit). `cache` exits 1 for every
// failure (workspaceInitExit).
func newWorkspaceInitCommand(stdout, stderr io.Writer) *cobra.Command {
	return group(&cobra.Command{
		Use:    "workspace-init",
		Short:  "an issue pod's init containers: fetch the repository, provision the issue's workspace; and a repository cache pod's process",
		Hidden: true,
	},
		newWorkspaceFetchCommand(stdout, stderr),
		newWorkspaceProvisionCommand(stdout, stderr),
		newWorkspaceCacheCommand(stdout, stderr),
	)
}

func newWorkspaceFetchCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "fetch --repo <owner>/<repo> --feed <dir> [--cache-url http://<cache>:8418]", Short: "fill the pod's feed directory with the repository, from its cache or from GitHub", Hidden: true, Args: noArguments}
	repo := cmd.Flags().String("repo", "", "repository as <owner>/<name> (required)")
	feed := cmd.Flags().String("feed", "", "the pod's feed directory, the container's own (required)")
	cacheURL := cmd.Flags().String("cache-url", os.Getenv(cacheURLEnv), "the repository cache to fill the feed from before GitHub (default $"+cacheURLEnv+", set by the runtime while the cache is ready)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return issuePodInitExit(cmd, workspaceFetch(ctx, *repo, *feed, *cacheURL, stdout), stderr)
	})
	return cmd
}

func newWorkspaceProvisionCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: strings.TrimPrefix(workspaceProvisionUsage, "legion workspace-init "), Short: "provision the issue's clone and jj workspace on the pod's disk from the feed", Hidden: true, Args: noArguments}
	issue := cmd.Flags().String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
	repo := cmd.Flags().String("repo", "", "repository as <owner>/<name> (required)")
	defaultBranch := cmd.Flags().String("default-branch", "", "the repository's default branch as GitHub names it, where an issue with no branch starts (required)")
	root := cmd.Flags().String("root", "/legion", "issue volume root directory")
	credentialHelper := cmd.Flags().String("credential-helper", "", "git credential helper written into the clone's config (required)")
	feed := cmd.Flags().String("feed", "", "the pod's feed directory, which workspace-init fetch filled (required)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return issuePodInitExit(cmd, workspaceInit(ctx, *issue, *repo, *defaultBranch, *root, *credentialHelper, *feed, stdout), stderr)
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

// issuePodInitExit is the exit status of an issue pod's init container, `fetch` or `provision`,
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

// workspaceInit validates everything before it touches the disk, --repo first, and refuses to
// run where the provisioning token is pointed at: this is the process that runs git and jj against
// what every agent of the issue can write. Each refusal has a cause outside the issue's volume
// (workspace.ErrOutsideVolume). Then it removes the git locks an interrupted git or jj left and
// provisions from the feed, an issue with no branch at defaultBranch. It makes no other
// directory. An ephemeral disk is the pod's own and starts empty, so the workspace this
// provisions is the only thing on it a role reads. A kept volume (keep_volume) holds the clone and
// the workspace that the issue's earlier pods left, and the provision reuses them: it keeps the
// clone and brings the workspace it finds up to date (workspace.Provision). No lock guards
// the clone: it is the issue's own, on the issue's own disk, and no other pod mounts that disk
// while this one runs, since the issue's Sandbox runs one pod at a time. For the same reason, and
// because the pod's launcher container has not started, no process holds a git lock on the disk
// while this runs, so every one there is stale and would fail each later write of what it locks
// (LEGION-731). The kubelet restarts a failed init container on the same pod, and so on the same
// disk, so a first attempt killed mid-fetch leaves exactly those locks for the retry to clear. An
// earlier pod that was stopped on a kept volume leaves its locks for the next pod in the same way.
func workspaceInit(ctx context.Context, issue, repo, defaultBranch, root, credentialHelper, feed string, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return workspace.OutsideVolume(err)
	}
	if !legionclaim.IsIssueKey(issue) {
		return workspace.OutsideVolume(fmt.Errorf("--issue must be a Dispatch issue key like LEGION-1 (got %q)", issue))
	}
	if !filepath.IsAbs(root) {
		return workspace.OutsideVolume(fmt.Errorf("--root must be an absolute path (got %q)", root))
	}
	if credentialHelper == "" {
		return workspace.OutsideVolume(fmt.Errorf("--credential-helper is required: %s", workspaceProvisionUsage))
	}
	if !filepath.IsAbs(feed) {
		return workspace.OutsideVolume(fmt.Errorf("--feed must be an absolute path (got %q)", feed))
	}
	if defaultBranch == "" {
		return workspace.OutsideVolume(fmt.Errorf("--default-branch is required: %s", workspaceProvisionUsage))
	}
	if _, set := os.LookupEnv(provisionTokenFileEnv); set {
		return workspace.OutsideVolume(errors.New(provisionTokenFileEnv + " is set: provisioning runs without the provisioning token, which `workspace-init fetch` alone holds"))
	}
	tools, err := provisioningTools("git", "jj")
	if err != nil {
		return workspace.OutsideVolume(err)
	}
	located, err := workspace.Location(root, repository, issue)
	if err != nil {
		return workspace.OutsideVolume(err)
	}

	log := func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) }
	if err := workspace.RemoveStaleGitLocks(located, time.Now(), log); err != nil {
		return err
	}
	run := workspace.NewRunner(provisionBudget, tools)
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: repository, Issue: issue, DefaultBranch: defaultBranch, CredentialHelper: credentialHelper,
		Source: workspace.FromFeed(feed), Log: log,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init: %s on %s from %s\n", provisioned.Dir, provisioned.Bookmark, provisioned.DefaultBranch)
	return nil
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
