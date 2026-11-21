package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// boundary is the boundary census: it runs the two programs of wholeUseDir that take their closed
// set from the libraries' own exported API - boundary/main.go, from x/oauth2's export data, over
// each Go module the call census reads, and boundary.ts, from openid-client's declarations, over
// the TypeScript workspace - and answers each library reference they list as a call finding, OK
// where it is the sole argument of the boundary function, and each library they enumerated (an
// API line) as an api finding. A program that did not build or load, read nothing, or printed no
// summary is UNKNOWN, and so is a TypeScript census that enumerated no function of openid-client.
func (g *gate) boundary(dir string) census {
	var c census
	bin := filepath.Join(g.work, "boundary")
	build := exec.Command("go", "build", "-o", bin, "./boundary")
	build.Dir = filepath.Join(dir, wholeUseDir)
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: the Go census does not build: %v: %s", err, lastLine(out)))
		return c
	}
	mods, err := goModules(dir)
	if err != nil || len(mods) == 0 {
		c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: the Go modules cannot be listed: %v", err))
		return c
	}
	roots, _ := workRoots(dir)
	for _, mod := range mods {
		tagSets, err := ciTagSets(dir, mod)
		if err != nil {
			c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: the build tags of %s cannot be read: %v", mod, err))
			continue
		}
		list := exec.Command(bin, "-dir", ".", "-targets", strings.Join(ciTargets, ","), "-tags", strings.Join(tagSets, ";"), "./...")
		list.Dir = filepath.Join(dir, mod)
		// Each module loads as its own build loads it (moduleWork).
		list.Env = append(os.Environ(), "GOWORK="+moduleWork(dir, mod, roots))
		out, _ := list.CombinedOutput()
		c.addBoundary(out, mod, "Go module "+mod, false)
	}
	ts := exec.Command("bun", filepath.Join(wholeUseDir, "boundary.ts"))
	ts.Dir = dir
	out, _ := ts.CombinedOutput()
	c.addBoundary(out, "", "the TypeScript workspace", true)
	return c
}

// ciTargets is each GOOS/GOARCH CI builds the modules for, each of which the Go and boundary
// censuses load a module for: linux/amd64, the host, which every job builds and tests on;
// darwin/arm64, which the Go checks vet every module for (ci-job-envoy-go-checks.yaml,
// ci-job-typecheck.yaml, ci-job-envoy-infra-go.yaml); and linux/arm64 and darwin/amd64, which
// the listener's archives, the legion and agent-secrets releases and their notices build
// (.github/go-release-targets.json, envoy-infra-targets.json, agent-secrets-client-targets.json,
// .github/scripts/go-release-notices.sh). The host's comes first, so its build is the one each
// other's file sets are compared with.
var ciTargets = []string{"linux/amd64", "darwin/arm64", "linux/arm64", "darwin/amd64"}

// ciTagSets is every build tag list the CI's vet passes for mod on any of ciTargets, as
// .github/scripts/go_build_tags.py reads them off the module, and the empty list, which is a build
// with no tags.
func ciTagSets(dir, mod string) ([]string, error) {
	sets := []string{""}
	seen := map[string]bool{"": true}
	for _, target := range ciTargets {
		goos, goarch, _ := strings.Cut(target, "/")
		cmd := exec.Command("python3", filepath.Join(dir, ".github/scripts/go_build_tags.py"), ".", goos, goarch)
		cmd.Dir = filepath.Join(dir, mod)
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			tags := strings.TrimPrefix(strings.TrimSpace(line), "-tags=")
			if !seen[tags] {
				seen[tags] = true
				sets = append(sets, tags)
			}
		}
	}
	return sets, nil
}

// addBoundary reads one boundary census's output. A Go module that imports no x/oauth2 package
// references and enumerates nothing, which is no UNKNOWN; the TypeScript workspace depends on
// openid-client, so its census must enumerate it (needAPI).
func (c *census) addBoundary(out []byte, prefix, what string, needAPI bool) {
	summary := ""
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		switch fields[0] {
		case "SUMMARY":
			summary = scanner.Text()
		case "API":
			c.Boundary = append(c.Boundary, finding{File: prefix, What: strings.Join(fields[1:], ": "), Kind: "api", OK: true, Why: what})
		case "TARGET":
			c.Targets = append(c.Targets, fmt.Sprintf("boundary census, %s, for %s: %s", what, strings.Join(fields[1:2], ""), strings.Join(fields[2:], ", ")))
		case "ok", "FAIL":
			if len(fields) < 4 {
				c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: %s: a line it cannot read: %q", what, scanner.Text()))
				continue
			}
			file, line := position(prefix, fields[1])
			c.Boundary = append(c.Boundary, finding{File: file, Line: line, What: fields[2], Kind: "call", OK: fields[0] == "ok", Why: fields[3]})
		default:
			if strings.HasPrefix(fields[0], "UNKNOWN") {
				c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: %s: %s", what, scanner.Text()))
			}
		}
	}
	switch {
	case summary == "":
		c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: %s: the census printed no summary: %s", what, lastLine(out)))
	case needAPI && strings.Contains(summary, "library API=0\t"):
		c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: %s: it enumerated no function of its library", what))
	case strings.Contains(summary, "files=0\t") || strings.HasSuffix(summary, "files=0"):
		c.Unknown = append(c.Unknown, fmt.Sprintf("boundary census: %s: the census read no production file", what))
	}
}
