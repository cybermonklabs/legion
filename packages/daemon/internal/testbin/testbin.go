// Package testbin names the pinned binaries the real-binary tests run (OMP, UV), readies the
// fresh HOME a test runs Oh My Pi under (OMPHome) with the profile and environment it runs with
// (WriteOMPProfile, OMPEnviron), reads back what that run said - the frame shape (OMPFrame), the
// model one answer names (StateModel) and the session files it left (SessionFiles) - answers a
// test's own provider as Anthropic does (AnthropicAnswer), and probes the pinned jj for the
// working-copy filters provisioning is proven against (RequireJJFilters).
package testbin

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// OMP is the pinned Oh My Pi binary LEGION_TEST_OMP names (.github/actions/install-omp).
func OMP(t *testing.T) string {
	t.Helper()
	return binary(t, "LEGION_TEST_OMP", "Oh My Pi")
}

// UV is the uv the worker image ships, which LEGION_TEST_UV names (.github/actions/install-uv,
// from worker.Dockerfile's ARG UV_VERSION and ARG UV_SHA256).
func UV(t *testing.T) string {
	t.Helper()
	return binary(t, "LEGION_TEST_UV", "uv")
}

// RequireJJFilters fails unless the jj under test, reading no configuration of the user's, has
// working-copy filters: provisioning is proven against the jj the worker image ships. The probe is
// a wait whose answer may never come, so it is bounded by testwait.Remaining, a share of what is
// left of the test binary's deadline: a probe that hangs fails its test by name and leaves the
// tests queued behind it their own time to run and report, rather than holding the binary to its
// alarm.
func RequireJJFilters(t *testing.T) {
	t.Helper()
	bound := testwait.Remaining(t)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	out, _ := probeCommand(ctx, t.TempDir()).Output()
	if ctx.Err() != nil {
		t.Fatalf("the jj filter probe had not answered within %s, its share of what was left of the test binary's deadline", bound.Round(time.Millisecond))
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Fatal("this jj has no working-copy filters: provisioning is proven against the jj the worker image ships (packages/daemon/docker/worker.Dockerfile, ARG JJ_TOOL), which CI installs")
	}
}

// probeCommand is the jj RequireJJFilters runs, killed when ctx ends. It runs in the empty home it
// reads its configuration from: jj searches upward from the directory it runs in for the closest
// .jj, so a probe with no Dir would open the workspace the test process stands in, which in a
// Legion issue pod is a real one. Its environment is fixtureenv.Isolated's, which holds nothing
// that could lead jj to another workspace, repository, remote or credential — the test process
// holds the pod's own — and which supplies HOME, the XDG_CONFIG_HOME under it and a JJ_CONFIG
// naming a file that does not exist, so the probe answers for the pinned jj's own defaults. The
// command is built apart from the run so that TestTheJJProbeTakesNoVariableOfTheProcess reads what
// RequireJJFilters starts, and starts no jj of its own.
func probeCommand(ctx context.Context, home string) *exec.Cmd {
	probe := exec.CommandContext(ctx, "jj", "config", "list", "--include-defaults", "git.filter.enabled")
	probe.Dir = home
	probe.Env = fixtureenv.Isolated(home)
	return probe
}

// binary is the path variable names. A run without one skips, except on GitHub Actions, where
// ci.yaml's daemon-go job installs every pin and a skip would hide the only check of the code a
// real binary exercises.
func binary(t *testing.T, variable, name string) string {
	t.Helper()
	path := os.Getenv(variable)
	switch {
	case path != "":
		return path
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatalf("%s is unset on GitHub Actions: name the pinned %s binary (ci.yaml's daemon-go job installs it)", variable, name)
	}
	t.Skipf("%s names no %s binary", variable, name)
	return ""
}
