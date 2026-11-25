package capabilities

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// OverlayEntries leaves out the empty entries Oh My Pi leaves out.
func TestOverlayEntriesSkipsEmptyEntries(t *testing.T) {
	for value, want := range map[string][]string{
		"":                  nil,
		"/a.yml":            {"/a.yml"},
		"/a.yml:":           {"/a.yml"},
		":/a.yml":           {"/a.yml"},
		"/a.yml::/b.yml":    {"/a.yml", "/b.yml"},
		"/a.yml:/b.yml:::/": {"/a.yml", "/b.yml", "/"},
	} {
		if got := OverlayEntries(value); !slices.Equal(got, want) {
			t.Errorf("OverlayEntries(%q) = %q, want %q", value, got, want)
		}
	}
}

// CheckOverlays refuses only an overlay a run of Oh My Pi exits non-zero on, or one it cannot read,
// naming the file and Oh My Pi's own error line however long the output before it; an answer it
// cannot read as JSON, on exit 0, is no refusal, since the overlays started.
func TestCheckOverlaysRefusesOnlyANonZeroExit(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	script := "#!/bin/sh\nIFS=:\nfor f in $PI_CONFIG_FILES; do case \"$f\" in *bad*)\n" +
		"  echo '" + strings.Repeat("y", 500) + "' >&2; echo \"error: Failed to parse config overlay $f: SyntaxError\" >&2; exit 1;; esac; done\n" +
		"echo not-json\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	good, bad, missing := filepath.Join(dir, "good.yml"), filepath.Join(dir, "bad.yml"), filepath.Join(dir, "missing.yml")
	for _, file := range []string{good, bad} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH"), "PI_CONFIG_FILES=/elsewhere.yml"}, WorkDir: dir}
	for _, tc := range []struct {
		name          string
		overlays, own []string
		want          string
	}{
		{"a file that starts", []string{good}, []string{good}, ""},
		{"a missing file", []string{good, missing}, []string{missing}, missing + " cannot be read: stat " + missing + ": no such file or directory"},
		{"a file Oh My Pi refuses", []string{good, bad}, []string{bad}, "Oh My Pi refuses " + bad + ": error: Failed to parse config overlay " + bad + ": SyntaxError"},
		{"the list Oh My Pi refuses", []string{bad, good}, []string{good}, "Oh My Pi refuses PI_CONFIG_FILES " + bad + ":" + good + ": error: Failed to parse config overlay " + bad + ": SyntaxError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckOverlays(context.Background(), img, tc.overlays, tc.own)
			if err != nil || got != tc.want {
				t.Fatalf("CheckOverlays = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A run that never reaches an exit of its own (here a timeout) does not stop the check: it is
// returned as err, and a role file Oh My Pi refuses after it is still refused. The slow file's run
// waits until the test's cleanup releases it, so only its timeout ends it; the refused file's run
// answers at once. Both run under commandTimeout, a clock counted from before each run starts,
// which a loaded machine can spend before the refused file's run has answered: such a call saw no
// refusal to keep, and shows nothing of this, so the row calls again until a call in which the
// refused file's run reached its own exit, within a minute. A check that stops at the first
// timeout never runs the refused file at all, which writes a mark as its first act, and fails the
// row at once.
func TestCheckOverlaysKeepsCheckingAfterATimeout(t *testing.T) {
	was := commandTimeout
	commandTimeout = time.Second
	t.Cleanup(func() { commandTimeout = was })
	dir := t.TempDir()
	release, badRan := filepath.Join(dir, "release"), filepath.Join(dir, "bad.ran")
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release the slow file's runs: %v", err)
		}
	})
	omp := filepath.Join(dir, "omp")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$PI_CONFIG_FILES\" in\n"+
		"*slow.yml) while [ ! -e %s ] && kill -0 %d 2>/dev/null; do sleep 0.05; done ;;\n"+
		"*bad.yml) : >>%s; echo \"error: Failed to parse config overlay $PI_CONFIG_FILES\" >&2; exit 1 ;;\n"+
		"esac\necho '{\"value\": false}'\n", shellprefix.Literal(release), os.Getpid(), shellprefix.Literal(badRan))
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	slow, bad := filepath.Join(dir, "slow.yml"), filepath.Join(dir, "bad.yml")
	for _, file := range []string{slow, bad} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: dir}
	within := testwait.Bounded(t, time.Minute, 2*commandTimeout+procgroup.StopGrace+procgroup.WaitDelay)
	until := time.Now().Add(within)
	for calls := 1; ; calls++ {
		if err := os.Remove(badRan); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		got, err := CheckOverlays(context.Background(), img, []string{slow, bad}, []string{slow, bad})
		if err != nil && strings.Contains(err.Error(), "under PI_CONFIG_FILES "+bad+": timed out") {
			if time.Now().Before(until) {
				continue
			}
			t.Fatalf("no call of %d over %s in which the refused file's run answered before its %s timeout: %v", calls, within, commandTimeout, err)
		}
		if _, statErr := os.Stat(badRan); statErr != nil {
			t.Fatalf("call %d: the refused file's run never started (%v): the check stopped at the slow file's timeout (CheckOverlays = %q, %v)", calls, statErr, got, err)
		}
		if want := "Oh My Pi refuses " + bad + ": error: Failed to parse config overlay " + bad; got != want {
			t.Errorf("call %d: CheckOverlays refused %q, want %q", calls, got, want)
		}
		if err == nil || !strings.Contains(err.Error(), "under PI_CONFIG_FILES "+slow+": timed out") {
			t.Errorf("call %d: CheckOverlays err = %v, want the slow file's timeout", calls, err)
		}
		return
	}
}
