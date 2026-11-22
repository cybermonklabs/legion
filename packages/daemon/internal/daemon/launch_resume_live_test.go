//go:build e2e

// Each runtime a live proof launches workers through holds a resumed Oh My Pi to its session as
// every production runtime does (runtime.SessionExclusiveVariable): rigRuntime's launch is a copy
// of localRuntime's, and on 2026-11-20 w3's step 4 ran a resume without the variable, so the
// resumed Oh My Pi moved to a new session id while the older process still held the session, and
// its claim boot failed. This needs no rig: the launched `legion` is a stub that records the
// environment and arguments it was started with, and exits.
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// proofLauncher is the half of a runtime this test drives.
type proofLauncher interface {
	Spawn(context.Context, runtime.SpawnSpec) (runtime.Locator, error)
	Resume(context.Context, *runtime.Locator, runtime.SpawnSpec) (runtime.Locator, error)
}

func TestEachProofRuntimeHoldsAResumedOhMyPiToItsSession(t *testing.T) {
	exclusive := runtime.SessionExclusiveVariable + "=" + runtime.SessionExclusiveValue
	runtimes := map[string]func(local *localRuntime) proofLauncher{
		"localRuntime": func(local *localRuntime) proofLauncher { return local },
		"rigRuntime": func(local *localRuntime) proofLauncher {
			return &rigRuntime{localRuntime: local, relay: &rigRelay{address: "unix:///nowhere/relay.sock"}}
		},
	}
	for name, build := range runtimes {
		t.Run(name, func(t *testing.T) {
			for _, resume := range []bool{true, false} {
				dir := t.TempDir()
				stub := filepath.Join(dir, "legion")
				// cmd.Dir is the claim's working directory, so the records land there.
				script := "#!/bin/sh\n/usr/bin/env > launch.env\nprintf '%s\\n' \"$@\" > launch.args\n"
				if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				role := filepath.Join(dir, "implementer.md")
				if err := os.WriteFile(role, []byte("You are the implementer.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				local := newLocalRuntime(t, filepath.Join(dir, "state"), stub, "/nowhere/omp", filepath.Join(dir, "home"), "proof")
				rt := build(local)
				spec := runtime.SpawnSpec{
					Claim: "legion-proof-proof-2-implementer", Project: "proof", Tree: "PROOF-1", Issue: "PROOF-2",
					Role: claim.RoleImplementer, Generation: 3, BootToken: "boot-secret",
					Prompt: runtime.PromptParts{RolePromptPaths: []string{role}},
				}
				var loc runtime.Locator
				var err error
				if resume {
					spec.ResumeSessionFile = filepath.Join(dir, "2026-11-20T09-23-54-744Z_01a1f845-02f8-7436-8ffe-de997eaed4d0.jsonl")
					if err := os.WriteFile(spec.ResumeSessionFile, []byte("{}\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					loc, err = rt.Resume(context.Background(), nil, spec)
				} else {
					loc, err = rt.Spawn(context.Background(), spec)
				}
				if err != nil {
					t.Fatalf("launch (resume %v): %v", resume, err)
				}
				pane, ok := local.pane(loc)
				if !ok {
					t.Fatalf("launch (resume %v): no pane for %s", resume, loc.Label())
				}
				select {
				case <-pane.done:
				case <-time.After(30 * time.Second):
					t.Fatalf("launch (resume %v): the stub did not exit within 30 s", resume)
				}
				workDir := local.workDir(spec.Issue)
				envText, err := os.ReadFile(filepath.Join(workDir, "launch.env"))
				if err != nil {
					t.Fatalf("launch (resume %v): the stub recorded no environment: %v", resume, err)
				}
				argsText, err := os.ReadFile(filepath.Join(workDir, "launch.args"))
				if err != nil {
					t.Fatalf("launch (resume %v): the stub recorded no arguments: %v", resume, err)
				}
				env := strings.Split(strings.TrimSpace(string(envText)), "\n")
				args := strings.Split(strings.TrimSpace(string(argsText)), "\n")
				held := slices.Contains(env, exclusive)
				resumed := slices.Contains(args, "--resume="+spec.ResumeSessionFile)
				if resume && (!held || !resumed) {
					t.Errorf("a resume ran with %s %v and --resume=%s %v; want both (args %q)",
						exclusive, held, spec.ResumeSessionFile, resumed, args)
				}
				if !resume && slices.ContainsFunc(env, func(v string) bool {
					return strings.HasPrefix(v, runtime.SessionExclusiveVariable+"=")
				}) {
					t.Errorf("a fresh spawn ran with %s set; only a resume carries it", runtime.SessionExclusiveVariable)
				}
			}
		})
	}
}
