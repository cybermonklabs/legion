package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// runDispatchCommand runs one subcommand of a build against databaseURL, in a home of its own and
// with nothing of the caller's environment, and answers its exit code and its whole output.
func runDispatchCommand(t *testing.T, binary, databaseURL string, args ...string) (int, string) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(binary, args...)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_NATS_DISABLED=1",
	}
	sort.Strings(cmd.Env)
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s %s: %v\n%s", filepath.Base(binary), strings.Join(args, " "), err, out)
	}
	return code, strings.TrimRight(string(out), "\n")
}
