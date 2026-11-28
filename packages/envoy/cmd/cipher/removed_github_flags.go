// packages/envoy/cmd/cipher/removed_github_flags.go
//
// The exec form's four GitHub flags of an earlier release. A GitHub App token is a profile now,
// which the deployment's profiles file declares and --target narrows, so each flag exits 2 before
// anything is sent and names what replaces it.
package main

import "strings"

// removedGitHubFlags is each removed flag, by its name without dashes, and the line that refuses
// it.
var removedGitHubFlags = map[string]string{
	"github-owner":     "--github-owner was removed: narrow the profile with --target ACCOUNT",
	"github-repo":      "--github-repo was removed: narrow the profile with --target ACCOUNT/REPOSITORY",
	"github-read-only": "--github-read-only was removed: name a read profile, such as GITHUB_READ, and narrow it with --target ACCOUNT",
	"github-token":     `--github-token was removed: a profile with "value": "token" puts one token in the variable`,
}

// removedGitHubFlag is the refusal of the first removed flag the exec form's args give before
// their --, in each spelling the flag parser takes (-name, --name, either with =value), and false
// when they give none. The words after -- are the command's own, and none of them is read.
func removedGitHubFlag(args []string) (string, bool) {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if message, removed := removedGitHubFlags[name]; removed {
			return message, true
		}
	}
	return "", false
}
