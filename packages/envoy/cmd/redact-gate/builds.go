package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ciTarget is one GOOS/GOARCH CI builds the modules for.
type ciTarget struct {
	GOOS   string
	GOARCH string
}

// String is the target as go writes a GOOS/GOARCH pair, "goos/goarch": the form the two listing
// programs take (-targets) and the boundary census's API lines name.
func (t ciTarget) String() string { return t.GOOS + "/" + t.GOARCH }

// env is the environment of a build for the target: cgo off where it is not the host's own, since
// cgo does not cross-compile here.
func (t ciTarget) env() []string {
	env := []string{"GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH}
	if t.GOOS != runtime.GOOS || t.GOARCH != runtime.GOARCH {
		env = append(env, "CGO_ENABLED=0")
	}
	return env
}

// hostBuild is the build of the host's own target with no tags, which the tests' listings read.
func hostBuild() ciBuild {
	return ciBuild{ciTarget: ciTarget{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
}

// ciTargets is each GOOS/GOARCH CI builds the modules for, each of which the Go and boundary
// censuses load a module for: linux/amd64, the host, which every job builds and tests on;
// darwin/arm64, which the Go checks vet every module for (ci-job-envoy-go-checks.yaml,
// ci-job-typecheck.yaml, ci-job-envoy-infra-go.yaml); and linux/arm64 and darwin/amd64, which
// the listener's archives, the legion and agent-secrets releases and their notices build
// (.github/go-release-targets.json, envoy-infra-targets.json, agent-secrets-client-targets.json,
// .github/scripts/go-release-notices.sh). The host's comes first, so its build is the one each
// other's file sets are compared with.
var ciTargets = []ciTarget{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "darwin", GOARCH: "arm64"}, {GOOS: "linux", GOARCH: "arm64"}, {GOOS: "darwin", GOARCH: "amd64"}}

// ciBuild is one build CI makes of a Go module: one of ciTargets with one of the build tag lists
// the CI's vet passes for the module, an empty list being a build with no tags.
type ciBuild struct {
	ciTarget
	Tags string
}

// ciBuilds is every build CI makes of one Go module: each of Targets with each of TagSets.
type ciBuilds struct {
	Targets []ciTarget
	TagSets []string
}

// all is every build of the module, target by target, the order the three Go censuses read it in.
func (b ciBuilds) all() []ciBuild {
	builds := make([]ciBuild, 0, len(b.Targets)*len(b.TagSets))
	for _, target := range b.Targets {
		for _, tags := range b.TagSets {
			builds = append(builds, ciBuild{ciTarget: target, Tags: tags})
		}
	}
	return builds
}

// flags are the -targets and -tags the two listing programs of wholeUseDir take: each target,
// comma separated, and each tag list, semicolon separated, which they cross themselves as all
// does.
func (b ciBuilds) flags() (targets, tags string) {
	names := make([]string, 0, len(b.Targets))
	for _, target := range b.Targets {
		names = append(names, target.String())
	}
	return strings.Join(names, ","), strings.Join(b.TagSets, ";")
}

// ciBuilds answers every build CI makes of the module mod of the snapshot at dir, read once for
// the module and kept (gate.builds), since the call, boundary and whole-value censuses all read
// the same builds: .github/scripts/go_build_tags.py runs once for each target of a module, not
// once for each census. The tag lists are the ones that script reads off the module for any of
// ciTargets, and the empty list, a build with no tags.
func (g *gate) ciBuilds(dir, mod string) (ciBuilds, error) {
	key := dir + "\x00" + mod
	if builds, read := g.builds[key]; read {
		return builds, nil
	}
	sets := []string{""}
	seen := map[string]bool{"": true}
	for _, target := range ciTargets {
		cmd := exec.Command("python3", filepath.Join(dir, ".github/scripts/go_build_tags.py"), ".", target.GOOS, target.GOARCH)
		cmd.Dir = filepath.Join(dir, mod)
		out, err := cmd.Output()
		if err != nil {
			return ciBuilds{}, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			tags := strings.TrimPrefix(strings.TrimSpace(line), "-tags=")
			if !seen[tags] {
				seen[tags] = true
				sets = append(sets, tags)
			}
		}
	}
	builds := ciBuilds{Targets: ciTargets, TagSets: sets}
	if g.builds == nil {
		g.builds = map[string]ciBuilds{}
	}
	g.builds[key] = builds
	return builds, nil
}
