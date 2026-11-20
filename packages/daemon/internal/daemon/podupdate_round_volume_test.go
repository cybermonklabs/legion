package daemon

// Where the work a round's own replacement took is, as the issue is told it (dispatch://INFRA-4109
// R1, with the kept volume of cybermonklabs/legion#4550). The message says that work is on a kept
// volume only where the issue's Sandbox holds a kept volume made before the round opened, which
// the issue's next pod mounts (roundKeptVolume). Over any other disk the work went with the pod,
// and the line and the message say so: a message that calls lost work safe tells a role not to do
// that work again.

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestAnUpdateRoundSaysTheWorkIsLostWhereNoKeptVolumeHoldsIt(t *testing.T) {
	for _, disk := range []struct {
		name   string
		script func(*podUpdateRig)
	}{
		{name: "an ephemeral disk", script: func(*podUpdateRig) {}},
		// A Sandbox made after the round opened holds a new volume, which holds nothing of the
		// round's pod.
		{name: "a kept volume made after the round opened", script: func(rig *podUpdateRig) {
			rig.rt.ScriptKeptVolume(podUpdateIssue, podUpdateStart.Add(time.Minute))
		}},
	} {
		t.Run(disk.name, func(t *testing.T) {
			rig, line := replacedOverPassedOverClaims(t, func(rig *podUpdateRig) {
				rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
				disk.script(rig)
			})
			if kept, named := line["keptVolume"]; named {
				t.Errorf("the replacement's line says keptVolume=%v, want no kept volume: the disk went with the pod", kept)
			}
			messages := rig.messages()
			if len(messages) != 1 {
				t.Fatalf("the issue was written %d messages, want one: %q", len(messages), messages)
			}
			for _, want := range []string{"The pod and its disk are gone.", "Work that is not on a branch at origin is lost.", passedOverCommit} {
				if !strings.Contains(messages[0], want) {
					t.Errorf("the message does not say %q:\n%s", want, messages[0])
				}
			}
			if strings.Contains(messages[0], "kept volume") {
				t.Errorf("the message names a kept volume over a disk that went with the pod:\n%s", messages[0])
			}
		})
	}
}
