package gcp_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The advertiser's health gate goes with this change (dispatch://INFRA-2739/artifact/open-items-md,
// decision 17). After the split no component sets tailnetServiceHost.health: the front door never
// did, and the clustered hub was the last, so the gate's script variables, its healthy() function
// and the two constants they read are dead code that only a test rig still reaches. The rendered
// script is where a reader sees it, so that is where this test reads it. Its lines are counted,
// so an empty script fails here rather than passing every absence.
func TestClusteredNatsHubAdvertiserScriptHoldsNoDeadHealthGate(t *testing.T) {
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredTailnetName)
	command := podContainer(t, template, "advertiser")["command"].([]any)
	require.Len(t, command, 3, "the advertiser runs /bin/sh -c with one script")
	script := command[2].(string)

	lines := strings.Count(script, "\n")
	require.NotZero(t, lines, "the advertiser's script is empty, so its absences prove nothing")
	for _, dead := range []string{"\nhealth=", "\nhealth_timeout=", "\nrise=", "\nfall=", "healthy()"} {
		require.NotContains(t, script, dead, "the advertiser's script still carries the dead health gate (%q)", dead)
	}
	t.Logf("%d line(s) examined in the advertiser's script", lines)
}
