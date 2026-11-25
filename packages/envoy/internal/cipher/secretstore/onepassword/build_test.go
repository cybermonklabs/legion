package onepassword_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// clientTargets is every GOOS/GOARCH an agent-secrets or cipherd build is made for with
// CGO_ENABLED=0: the release's (.github/go-release-targets.json and
// .github/agent-secrets-client-targets.json) and the ones agent-secrets builds for without its
// value prompt (cmd/cipher's TestTheAgentSecretsBinaryBuildsWithoutThePrompt).
var clientTargets = []string{"linux/amd64", "linux/arm64", "linux/386", "darwin/amd64", "darwin/arm64", "windows/amd64", "freebsd/amd64"}

// TestTheOnePasswordPackageBuildsForEachClientTarget builds this package with CGO_ENABLED=0 for
// each of clientTargets. The 1Password SDK compiles that way for darwin, linux and windows alone
// (its internal.GetSharedLibCore has no file for other systems without cgo), so sdk.go is built
// for those three and sdk_other.go everywhere else; a build line that lets the SDK into another
// system's build fails here on freebsd/amd64.
func TestTheOnePasswordPackageBuildsForEachClientTarget(t *testing.T) {
	for _, target := range clientTargets {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			goos, goarch, _ := strings.Cut(target, "/")
			build := exec.Command("go", "build", ".")
			build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
			if out, err := build.CombinedOutput(); err != nil {
				t.Errorf("CGO_ENABLED=0 GOOS=%s GOARCH=%s go build .: %v\n%s", goos, goarch, err, out)
			}
		})
	}
}
