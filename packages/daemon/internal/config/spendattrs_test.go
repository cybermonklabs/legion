package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// operatorSpendAttributesFor is the value the live deployment writes into the operator's
// OTEL_RESOURCE_ATTRIBUTES for one lane: legion/Pulumi.prod.yaml of theorem-labs/agent-infra
// writes one per project, differing in the lane alone.
func operatorSpendAttributesFor(lane string) string {
	return "theorem.project=legion,theorem.lane=" + lane + ",theorem.credential=gateway,theorem.person=sami@theorem.dev"
}

// liveSpendLanes are the five lanes that deployment runs, each with a value of its own.
var liveSpendLanes = []string{"legion-smoke", "legion-linux", "legion-bet", "legion-infra", "legion-ossv"}

// spendAttributesReaders are the two readers of an operator's own OTEL_RESOURCE_ATTRIBUTES:
// `runtime.kubernetes.pod.env` (readPodEnv) and `runtime.tmux.pane_env` (readPaneEnv). Both read
// the same file at boot and under `legion start --check-config`, so one table drives both and
// holds them to one verdict: a value either runtime would hand the agent's OpenTelemetry detector
// is refused by whichever reader carries it, in that key's own sentence.
var spendAttributesReaders = []struct {
	// key is the configuration key whose sentence the refusal reads as.
	key string
	// file is the whole configuration file setting the variable to value.
	file func(value string) string
	// env is the variables the loader settled for that reader.
	env func(cfg Config) map[string]string
	// refusal is the whole error the reader answers, built around one SpendAttributesRefusal
	// reason, written out here rather than taken from the reader's own format string.
	refusal func(reason string) string
}{
	{
		key: podKey + ".env",
		file: func(value string) string {
			return kubernetesFile + "    pod:\n      env:\n        " + runtime.SpendAttributesVariable + ": " + strconv.Quote(value) + "\n"
		},
		env: func(cfg Config) map[string]string { return cfg.Runtime.Kubernetes.Pod.Env },
		refusal: func(reason string) string {
			return "runtime.kubernetes.pod.env sets OTEL_RESOURCE_ATTRIBUTES to a value the agent's OpenTelemetry detector would throw on," +
				" which costs the launch every attribute of the variable, the operator's own among them: " + reason
		},
	},
	{
		key: "runtime.tmux.pane_env",
		file: func(value string) string {
			return minimalFile + "runtime:\n  tmux:\n    pane_env:\n      " + runtime.SpendAttributesVariable + ": " + strconv.Quote(value) + "\n"
		},
		env: func(cfg Config) map[string]string { return cfg.Runtime.Tmux.PaneEnv },
		refusal: func(reason string) string {
			return "runtime.tmux.pane_env names OTEL_RESOURCE_ATTRIBUTES with a value the agent's OpenTelemetry detector would throw on," +
				" which costs the launch every attribute of the variable, the operator's own among them: " + reason
		},
	},
}

// Every value the live deployment writes loads under both readers, as written: a reader that
// refused one of them would stop the deployment it was written to protect, and the runtime
// appends the launch's own pairs to exactly this text.
func TestBothReadersAcceptTheLiveSpendAttributes(t *testing.T) {
	for _, reader := range spendAttributesReaders {
		for _, lane := range liveSpendLanes {
			t.Run(reader.key+" "+lane, func(t *testing.T) {
				value := operatorSpendAttributesFor(lane)
				cfg, err := LoadForValidation(writeConfigFile(t, reader.file(value)), noEnv)
				if err != nil {
					t.Fatalf("LoadForValidation: %v", err)
				}
				if got := reader.env(cfg)[runtime.SpendAttributesVariable]; got != value {
					t.Errorf("%s %s = %q, want the operator's value as written, %q", reader.key, runtime.SpendAttributesVariable, got, value)
				}
			})
		}
	}
}

// A value the detector would throw on is refused at load by whichever reader carries it, in that
// configuration key's own sentence: the key, the variable, and the one member that is wrong with
// what is wrong with it. The rest of the value is nobody's business and is never printed — an
// operator reading the refusal sees the member to fix, and the attributes beside it stay out of
// the daemon's log and out of `legion start --check-config`'s output.
func TestBothReadersRefuseWhatTheDetectorWouldThrowOn(t *testing.T) {
	long := strings.Repeat("a", 256)
	for _, tc := range []struct {
		name, value, reason string
		// absent is what the refusal may not hold: the members of the value beside the one
		// refused.
		absent []string
	}{
		{
			name:   "a member with two =",
			value:  "theorem.project=legion,theorem.lane=legion=infra,theorem.credential=gateway",
			reason: `its member "theorem.lane=legion=infra" does not split on = into exactly one key and one value`,
			absent: []string{"theorem.project", "theorem.credential", "gateway"},
		},
		{
			name:   "a member with a bare %",
			value:  "theorem.project=legion,theorem.note=100%done,theorem.credential=gateway",
			reason: `its member "theorem.note=100%done" has a value with a malformed percent escape`,
			absent: []string{"theorem.project", "theorem.credential", "gateway"},
		},
		{
			name:   "a value over 255 characters once decoded",
			value:  "theorem.project=legion,theorem.note=" + long + ",theorem.credential=gateway",
			reason: `its member "theorem.note=` + long + `" has a value of 256 characters once percent-decoded, over the detector's 255`,
			absent: []string{"theorem.project", "theorem.credential", "gateway"},
		},
		{
			name:   "a member with an empty key",
			value:  "theorem.project=legion,=gateway,theorem.lane=legion-infra",
			reason: `its member "=gateway" has an empty key`,
			absent: []string{"theorem.project", "theorem.lane", "legion-infra"},
		},
		{
			name:   "a member naming the issue the runtime writes itself",
			value:  "theorem.project=legion,legion.issue=INFRA-1,theorem.credential=gateway",
			reason: `its member "legion.issue=INFRA-1" names legion.issue, which the runtime writes into every launch itself`,
			absent: []string{"theorem.project", "theorem.credential", "gateway"},
		},
	} {
		for _, reader := range spendAttributesReaders {
			t.Run(reader.key+" "+tc.name, func(t *testing.T) {
				want := reader.refusal(tc.reason)
				_, err := LoadForValidation(writeConfigFile(t, reader.file(tc.value)), noEnv)
				if err == nil || err.Error() != want {
					t.Fatalf("LoadForValidation = %v, want\n%q", err, want)
				}
				for _, member := range tc.absent {
					if strings.Contains(err.Error(), member) {
						t.Errorf("the refusal %q holds %q, a member of the value beside the one it refuses", err, member)
					}
				}
			})
		}
	}
}
