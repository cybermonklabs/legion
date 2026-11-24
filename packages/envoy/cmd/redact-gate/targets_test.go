package main

import (
	"bytes"
	"strings"
	"testing"
)

// boundaryAPI is an API line of the boundary census as addBoundary reads one: the library
// package, what the census enumerated of it, and the targets whose builds import it.
func boundaryAPI(targets ...string) finding {
	return finding{
		File: "packages/envoy",
		What: "golang.org/x/oauth2: 12 functions, methods and function variables with an error result: for " + strings.Join(targets, ", "),
		Kind: "api",
		OK:   true,
		Why:  "Go module packages/envoy",
	}
}

// TestTheCensusStageFailsABuildTargetNoBoundaryAPILineNames: the boundary census loads each module
// for every target CI builds (ciTargets), and each of its API lines names the targets whose builds
// import that library package. A target no API line names is one no build of any module loaded,
// and a census with no API line read nothing: the census stage fails either, in the gate itself.
func TestTheCensusStageFailsABuildTargetNoBoundaryAPILineNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		boundary []finding
		want     []string
		absent   []string
	}{
		{
			name:     "a target no API line names",
			boundary: []finding{boundaryAPI("linux/amd64", "darwin/arm64", "linux/arm64")},
			want:     []string{"stage 2 (boundary census) no BOUNDARY api line names the build target darwin/amd64"},
			absent:   []string{"the build target linux/amd64", "the build target darwin/arm64", "the build target linux/arm64", "printed no BOUNDARY api line"},
		},
		{
			name:     "no API line at all",
			boundary: []finding{{File: "packages/envoy/oauth2call/call.go", Line: 10, What: "golang.org/x/oauth2.Config.Exchange", Kind: "call", OK: true, Why: "the sole argument of redact.OAuth2"}},
			want:     []string{"stage 2 (boundary census) UNKNOWN: it printed no BOUNDARY api line, so it names no build target"},
			absent:   []string{"names the build target"},
		},
		{
			// The control: every target is named, each by one of two API lines, as a target CI
			// builds every module for and a library only some module imports are named.
			name:     "every target named, over two API lines",
			boundary: []finding{boundaryAPI("linux/amd64", "darwin/arm64"), boundaryAPI("linux/arm64", "darwin/amd64")},
			absent:   []string{"names the build target", "printed no BOUNDARY api line"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			g := &gate{out: &out}
			g.judgeStructural(census{Boundary: tc.boundary})
			judged := out.String()
			for _, want := range tc.want {
				if !strings.Contains(judged, want) {
					t.Errorf("the census stage does not fail with %q; what it printed:\n%s", want, judged)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(judged, absent) {
					t.Errorf("the census stage prints %q, which these findings do not warrant; what it printed:\n%s", absent, judged)
				}
			}
		})
	}
}
