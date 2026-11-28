package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities/capabilitiestest"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// The image probe's Claude lane holds the claude-envoy root the image carries to the gate's
// contract by equality, as the pod lane holds pi-legion (verifyClaude's readDeclaredContract): a
// root whose manifest declares the contract below the gate's, or the one above it, is refused
// naming the manifest, the package version and both numbers, with the remedy of a rebuilt image,
// since claude-envoy ships in the image alone. The root of the gate's own contract is the control,
// and passes with the line that says so. Each run is the whole probe `legion probe-image
// --claude-root --claude-code-pin` makes, over an image whose Oh My Pi lanes pass, so a refusal is
// the Claude lane's.
func TestTheGateRefusesAClaudeEnvoyRootOfAnotherContract(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, declared string
		refused        bool
	}{
		{name: "the gate's own contract", declared: "3"},
		{name: "the contract below the gate's", declared: "2", refused: true},
		{name: "the contract above the gate's", declared: "4", refused: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
			home := imageHome(t, contractCurrent)
			root := capabilitiestest.ClaudeRoot(t, testCase.declared)
			var logged bytes.Buffer

			err := ProbeImage(context.Background(), ImageProbe{
				Omp: f.invocation, Contract: 3, Env: imageEnv(home), WorkDir: t.TempDir(),
				PluginRoot: legionRootOf(home), EnvoyPluginRoot: envoyRootOf(home),
				ClaudeRoot: root, ClaudeCodePin: capabilitiestest.ClaudeCodePin(t),
				RoleReferences: promptrefs.New(), Log: slog.New(slog.NewTextHandler(&logged, nil)),
			})

			passed := strings.Contains(logged.String(), "boot gate: the claude-envoy root speaks this daemon's contract")
			if !testCase.refused {
				if err != nil || !passed {
					t.Fatalf("ProbeImage over a claude-envoy root of the gate's contract 3 = %v, want a pass and the line that says so:\n%s", err, logged.String())
				}
				return
			}
			if err == nil {
				t.Fatalf("ProbeImage passed a claude-envoy root of contract %s, where the gate requires 3", testCase.declared)
			}
			manifest := filepath.Join(root, "package.json")
			for _, want := range []string{
				"claude-envoy at " + manifest + " (package 1.0.0) speaks daemon API contract " + testCase.declared + "; this daemon requires 3",
				"Build the worker image from this daemon's commit.",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if passed {
				t.Errorf("the gate logged a claude-envoy root of contract %s as speaking its contract:\n%s", testCase.declared, logged.String())
			}
		})
	}
}
