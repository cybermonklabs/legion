package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// publicNamePlants are control (24)'s two entries: one no contract declares, one a credential's.
var publicNamePlants = []struct{ what, name, source, want string }{
	{"an entry with no contract source", "a_new_unsourced_name", "", "names no contract"},
	{"a credential's name", "refresh_token", "RFC 6749 §6", "credential"},
}

// publicNameGuardControl plants each of publicNamePlants into the public-name table of a worktree
// of HEAD, in Go's generated table and in the TypeScript it is generated from, and requires each
// half's guard test to fail on it with its message. It answers "" when all four failed so.
func publicNameGuardControl(repo string, _ inventory) (string, string) {
	var failed, out []string
	for _, plant := range publicNamePlants {
		verdict, output := publicNameGuardOnce(repo, plant.name, plant.source, plant.want)
		out = append(out, fmt.Sprintf("%s: %s", plant.what, orAccepted(verdict)), output)
		if verdict != "" {
			failed = append(failed, plant.what+": "+verdict)
		}
	}
	return strings.Join(failed, "; "), strings.Join(out, "\n")
}

func publicNameGuardOnce(repo, name, source, want string) (string, string) {
	work, err := os.MkdirTemp(os.Getenv("TMPDIR"), "redact-public-names.")
	if err != nil {
		return err.Error(), ""
	}
	defer os.RemoveAll(work)
	wt := filepath.Join(work, "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		return fmt.Sprintf("the worktree: %v: %s", err, out), ""
	}
	defer exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run()
	c := &controlRun{wt: wt}
	if err := c.edit("packages/envoy/internal/redact/public_names_generated.go", "var publicRequestNames = []publicRequestName{\n",
		fmt.Sprintf("var publicRequestNames = []publicRequestName{\n\t{Place: \"field\", Name: %q, Source: %q},\n", name, source)); err != nil {
		return err.Error(), ""
	}
	if err := c.edit("packages/contracts/src/public-request-names.ts", "export const PUBLIC_REQUEST_NAMES: readonly PublicRequestName[] = [\n",
		fmt.Sprintf("export const PUBLIC_REQUEST_NAMES: readonly PublicRequestName[] = [\n  { place: \"field\", name: %q, source: %q },\n", name, source)); err != nil {
		return err.Error(), ""
	}
	goTest := exec.Command("go", "test", "-count=1", "-run", "^TestThePublicNameTableHoldsOnlyContractNamesNoCredentialTravelsUnder$", "./internal/redact/")
	goTest.Dir = filepath.Join(wt, "packages/envoy")
	goOut, goErr := goTest.CombinedOutput()
	bunTest := exec.Command("bun", "test", "src/public-request-names.test.ts")
	bunTest.Dir = filepath.Join(wt, "packages/contracts")
	bunOut, bunErr := bunTest.CombinedOutput()
	output := "go: " + lastLines(string(goOut), 6) + "\nbun: " + lastLines(string(bunOut), 12)
	var wrong []string
	if goErr == nil || !strings.Contains(string(goOut), want) {
		wrong = append(wrong, "Go's guard did not fail on it naming "+want)
	}
	if bunErr == nil || !strings.Contains(string(bunOut), want) {
		wrong = append(wrong, "the TypeScript guard did not fail on it naming "+want)
	}
	return strings.Join(wrong, "; "), output
}
