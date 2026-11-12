package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

const gitCredentialUsage = "usage: legion git-credential get|store|erase  (git's credential helper for a shared clone; git runs it, never a person)"

// gitCredentialOperationUsage is each operation's own usage, which its --help prints.
var gitCredentialOperationUsage = map[string]string{
	"get":   "usage: legion git-credential get  (reads git's request on stdin; answers the token of the owner its path names, or nothing, exit 1)",
	"store": "usage: legion git-credential store  (reads git's request on stdin and writes nothing: the daemon is the one writer of the gh files)",
	"erase": "usage: legion git-credential erase  (reads git's request on stdin and erases nothing: the daemon is the one writer of the gh files)",
}

// gitCredentialStdin is where `legion git-credential` reads git's credential request: the
// process's stdin, which a test replaces.
var gitCredentialStdin io.Reader = os.Stdin

// runGitCredential is every shared clone's credential helper (workspace.GitHubCredentialHelper),
// which git runs with its operation and the request on stdin. `get` answers a request for
// https://github.com with the installation-token username and the token of the repository the
// request's path names (credential.useHttpPath, which every shared clone sets): its first segment
// is the owner and its second the repository, and the token comes from the hosts.yml in that
// repository's own directory of LEGION_GH_OWNERS_DIR, `<owner>/<repo>`, where the bundle holds
// one, from the owner's directory otherwise (repositoryGHConfigDir), and from nothing else. The
// bundle a daemon builds now holds one directory per owner the role's App is installed on and
// no repository directory at all, so the owner's directory is what answers every repository of
// that owner; a pod launched before that change projects the repository directories of its own
// launch, which is what the first step still looks for. Any other host or protocol, a request
// with no path, a path whose owner
// is no GitHub login or one the bundle holds no directory for — an owner the role's App is not
// installed on — an unset LEGION_GH_OWNERS_DIR,
// and a hosts.yml with no token answer nothing, exit 1, naming why on stderr, and git, which every
// shared clone tells credential.interactive=false, then fails rather than prompts. `store` and
// `erase` change nothing: the daemon is the one writer of the gh files.
func runGitCredential(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 0
	}
	if len(args) == 2 && helpRequested(args[1]) {
		if usage, ok := gitCredentialOperationUsage[args[0]]; ok {
			fmt.Fprintln(stderr, usage)
			return 0
		}
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 2
	}
	switch args[0] {
	case "store", "erase":
		_, _ = io.Copy(io.Discard, gitCredentialStdin)
		return 0
	case "get":
	default:
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 2
	}
	request := readCredentialRequest(gitCredentialStdin)
	if request["protocol"] != "https" {
		fmt.Fprintf(stderr, "legion git-credential: protocol %q is not https; answering nothing\n", request["protocol"])
		return 1
	}
	if request["host"] != "github.com" {
		fmt.Fprintf(stderr, "legion git-credential: host %q is not github.com; answering nothing\n", request["host"])
		return 1
	}
	ownersDir := os.Getenv("LEGION_GH_OWNERS_DIR")
	if ownersDir == "" {
		fmt.Fprintln(stderr, "legion git-credential: LEGION_GH_OWNERS_DIR is unset; answering nothing")
		return 1
	}
	if request["path"] == "" {
		fmt.Fprintln(stderr, "legion git-credential: git sent no path (credential.useHttpPath is off), so the request names no owner; answering nothing")
		return 1
	}
	path := strings.TrimPrefix(request["path"], "/")
	segment, rest, _ := strings.Cut(path, "/")
	owner := strings.ToLower(segment)
	if !ghconfig.IsOwner(owner) {
		fmt.Fprintf(stderr, "legion git-credential: the path %s names the owner %q, which is no GitHub login; answering nothing\n", request["path"], segment)
		return 1
	}
	repo, _, _ := strings.Cut(rest, "/")
	dir, err := repositoryGHConfigDir(ownersDir, owner, repo)
	if err != nil {
		fmt.Fprintf(stderr, "legion git-credential: %v; answering nothing\n", err)
		return 1
	}
	hosts, err := os.ReadFile(filepath.Join(dir, ghconfig.HostsFile))
	if err != nil {
		fmt.Fprintf(stderr, "legion git-credential: %v; answering nothing\n", err)
		return 1
	}
	token, err := ghconfig.TokenFromHosts(hosts)
	if err != nil {
		fmt.Fprintf(stderr, "legion git-credential: %s: %v; answering nothing\n", dir, err)
		return 1
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", ghconfig.GitUser, token)
	return 0
}

// readCredentialRequest reads git's credential protocol: `key=value` lines up to a blank line or
// the end of input, the last value of a key winning.
func readCredentialRequest(r io.Reader) map[string]string {
	request := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			request[key] = value
		}
	}
	return request
}

// repositoryGHConfigDir is the gh directory the bundle under ownersDir answers for owner's
// repository repo (ghconfig.RepositoryDir): the repository's own, `<owner>/<repo>`, where the
// bundle holds it, and the owner's own otherwise. The bundle this daemon builds holds no
// repository directory at all — a token reaches every repository its installation holds, so an
// owner's directory answers for every repository of that owner (appauth.GitHubCredential) — and
// the two-step pick stays all the same: a pod launched by a daemon before this change projects
// the directories of its launch, which hold repository directories, and the step costs one stat.
//
// That helper normalizes the name for a directory (ghconfig.RepositoryName), so a caller — the
// gh wrapper's target (gh_selectors.go) as much as a git credential request's path — may hand
// it as it was given it; the gh wrapper keys a repository by that same name as it reads a
// command, so two spellings of one repository are one repository, and normalizing it again
// there changes nothing for any name GitHub holds.
// A repo no bundle directory could be named for (ghconfig.IsDirName) is one the bundle holds
// none for, so the owner's is answered rather than a refusal. An owner the bundle holds no
// directory for at all is refused here, which is this caller's own answer to the helper's miss:
// the role's App has no installation on owner, so no token of this bundle reaches anything of
// it, and an answer of nothing would have git prompt.
func repositoryGHConfigDir(ownersDir, owner, repo string) (string, error) {
	if dir := ghconfig.RepositoryDir(ownersDir, owner, repo); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("the role's GitHub App is not installed on %q: %s holds no gh directory for it", owner, ownersDir)
}
