package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// `plan_guidance` is a path of the deployment's, resolved against the directory of the file that
// names it as `instructions` is, and never against the daemon's working directory. Load reads no
// file at the path: boot reads it, so the path loads with nothing on disk.
func TestPlanGuidanceIsAPathKeyReadAtBoot(t *testing.T) {
	t.Run("a relative path resolves against the file's directory", func(t *testing.T) {
		path := writeConfigFile(t, minimalFile+"plan_guidance: rules/plan-guidance.md\n")

		cfg, err := Load(path, noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := filepath.Join(filepath.Dir(path), "rules/plan-guidance.md"); cfg.PlanGuidancePath != want {
			t.Errorf("PlanGuidancePath = %q, want %q", cfg.PlanGuidancePath, want)
		}
	})

	t.Run("a file that names none loads no guidance", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.PlanGuidancePath != "" {
			t.Errorf("PlanGuidancePath = %q, want none", cfg.PlanGuidancePath)
		}
	})

	t.Run("an empty value is refused", func(t *testing.T) {
		_, err := Load(writeConfigFile(t, minimalFile+"plan_guidance: \"\"\n"), noEnv)
		if want := "plan_guidance must not be empty"; err == nil || err.Error() != want {
			t.Fatalf("Load error = %v, want %q", err, want)
		}
	})
}

// The copy an owner's prompt reads starts with a heading that names the deployment and a blank
// line, and ends with a blank line and the operator's file verbatim; what stands between is
// Legion's own prompt text, whose words no test holds. A configured file with nothing in it is a
// misconfiguration, never an empty guidance, and a file that cannot be read is refused naming the
// operator's path, as the deployment's instructions are.
func TestPlanGuidanceCopyCarriesItsHeaderAndRefusesAnUnusableFile(t *testing.T) {
	t.Run("the copy", func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		const guidance = "Review on another model family.\n"

		if err := WritePlanGuidance([]byte(guidance), stateDir, "demo"); err != nil {
			t.Fatalf("WritePlanGuidance: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(stateDir, "plan-guidance.md"))
		if err != nil {
			t.Fatal(err)
		}
		header, end := "# Plan guidance of this deployment (demo)\n\n", "\n\n"+guidance
		if !strings.HasPrefix(string(got), header) || !strings.HasSuffix(string(got), end) {
			t.Errorf("contents = %q, want them to start %q and end %q, the operator's file after a blank line", got, header, end)
		}
	})

	dir := t.TempDir()
	blank := filepath.Join(dir, "blank.md")
	if err := os.WriteFile(blank, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent.md")
	// The largest file the bound takes, and one byte more: every owner reads the whole guidance at
	// every launch, and under Kubernetes it travels in the launcher's start frame.
	largest := filepath.Join(dir, "largest.md")
	if err := os.WriteFile(largest, []byte(strings.Repeat("g", PlanGuidanceMaxBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	over := filepath.Join(dir, "over.md")
	if err := os.WriteFile(over, []byte(strings.Repeat("g", PlanGuidanceMaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// A FIFO is read forever by a plain read with no writer: boot would never finish it.
	fifo := filepath.Join(dir, "fifo.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "blank", path: blank, want: "plan guidance file " + blank + " is empty"},
		{name: "absent", path: absent, want: "plan guidance file " + absent + " could not be read: "},
		{name: "a directory", path: dir, want: "plan_guidance names " + dir + ", which is not a regular file"},
		{name: "a FIFO", path: fifo, want: "plan_guidance names " + fifo + ", which is not a regular file"},
		{name: "one byte over the bound", path: over, want: fmt.Sprintf("plan_guidance names %s, which holds more than %d bytes", over, PlanGuidanceMaxBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadPlanGuidance(tc.path); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to start %q", err, tc.want)
			}
		})
	}
	t.Run("the largest file the bound takes", func(t *testing.T) {
		if contents, err := ReadPlanGuidance(largest); err != nil || len(contents) != PlanGuidanceMaxBytes {
			t.Fatalf("ReadPlanGuidance = %d bytes, %v; want all %d", len(contents), err, PlanGuidanceMaxBytes)
		}
	})
}
