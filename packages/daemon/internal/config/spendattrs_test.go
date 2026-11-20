package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// liveSpendAttributes is what the live deployment writes into the operator's
// OTEL_RESOURCE_ATTRIBUTES, one value per lane: legion/Pulumi.prod.yaml of example-org/deployment
// at main on 2026-11-11, one entry per lane under each project's pod env. No line is cited,
// because that file moves under every edit of it; search it for the variable's name. The five
// values differ in the project as well as the lane, so each is written out as the deployment
// writes it, with a placeholder in place of the person's address.
var liveSpendAttributes = []struct{ lane, value string }{
	{"legion-smoke", "acme.project=legion,acme.lane=legion-smoke,acme.credential=gateway,acme.person=sami@example.com"},
	{"legion-linux", "acme.project=verified-linux,acme.lane=legion-linux,acme.credential=gateway,acme.person=sami@example.com"},
	{"legion-bet", "acme.project=task-making,acme.lane=legion-bet,acme.credential=gateway,acme.person=sami@example.com"},
	{"legion-infra", "acme.project=legion,acme.lane=legion-infra,acme.credential=gateway,acme.person=sami@example.com"},
	{"legion-ossv", "acme.project=oss-verify,acme.lane=legion-ossv,acme.credential=gateway,acme.person=sami@example.com"},
}

// spendAttributesReaders are the two readers of an operator's own OTEL_RESOURCE_ATTRIBUTES:
// `runtime.kubernetes.pod.env` (readPodEnv) and `runtime.tmux.pane_env` (readPaneEnv). Both read
// the same file at boot and under `legion config check`, so one table drives both and
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

// Every value of the live deployment's shape loads under both readers, as written: a reader that
// refused one of them would stop the deployment it was written to protect, and the runtime
// appends the launch's own pairs to exactly this text.
func TestBothReadersAcceptTheLiveSpendAttributes(t *testing.T) {
	for _, reader := range spendAttributesReaders {
		for _, live := range liveSpendAttributes {
			t.Run(reader.key+" "+live.lane, func(t *testing.T) {
				value := live.value
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
// the daemon's log and out of `legion config check`'s output.
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
			value:  "acme.project=legion,acme.lane=legion=infra,acme.credential=gateway",
			reason: `its member "acme.lane=legion=infra" does not split on = into exactly one key and one value`,
			absent: []string{"acme.project", "acme.credential", "gateway"},
		},
		{
			name:   "a member with a bare %",
			value:  "acme.project=legion,acme.note=100%done,acme.credential=gateway",
			reason: `its member "acme.note=100%done" has a value with a malformed percent escape`,
			absent: []string{"acme.project", "acme.credential", "gateway"},
		},
		{
			name:   "a value over 255 characters once decoded",
			value:  "acme.project=legion,acme.note=" + long + ",acme.credential=gateway",
			reason: `its member "acme.note=` + long + `" has a value of 256 characters once percent-decoded, over the detector's 255`,
			absent: []string{"acme.project", "acme.credential", "gateway"},
		},
		{
			name:   "a member with an empty key",
			value:  "acme.project=legion,=gateway,acme.lane=legion-infra",
			reason: `its member "=gateway" has an empty key`,
			absent: []string{"acme.project", "acme.lane", "legion-infra"},
		},
		{
			name:   "a member naming the issue the runtime writes itself",
			value:  "acme.project=legion,legion.issue=SHOP-1,acme.credential=gateway",
			reason: `its member "legion.issue=SHOP-1" names legion.issue, which the runtime writes into every launch itself`,
			absent: []string{"acme.project", "acme.credential", "gateway"},
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
