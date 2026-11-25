package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// memoryGuardKinds are the two pod kinds runtime.kubernetes.memory_guard is keyed by, each with
// the settled block of its own (block), so a test sets one kind's block of an expected
// configuration and holds the other kind's at what it was.
var memoryGuardKinds = []struct {
	name  string
	block func(*MemoryGuardConfig) *MemoryGuardPodConfig
}{
	{"issue_pods", func(c *MemoryGuardConfig) *MemoryGuardPodConfig { return &c.IssuePods }},
	{"controller_pod", func(c *MemoryGuardConfig) *MemoryGuardPodConfig { return &c.ControllerPod }},
}

// memoryGuardBlock is a runtime.kubernetes.memory_guard block holding one pod kind's members as
// one flow mapping, appended to kubernetesFile as its last member.
func memoryGuardBlock(kind, members string) string {
	return "    memory_guard: {" + kind + ": {" + members + "}}\n"
}

// loadMemoryGuard is the settled memory_guard of kubernetesFile with block appended.
func loadMemoryGuard(t *testing.T, block string) MemoryGuardConfig {
	t.Helper()
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+block), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation of %q: %v", block, err)
	}
	return cfg.Runtime.Kubernetes.MemoryGuard
}

// A Kubernetes deployment that says nothing about the memory guard gets it anyway, on for issue
// pods and for the controller's pod alike, each at the wire's own defaults: the defaults are the
// shimwire constants themselves, which a launcher's guard runs by before any acknowledgement
// reaches it, so the two never differ. The controller's pod waits 1 second after a kill, its own
// default (shimwire.MemoryGuardControllerWaitDefault), and issue pods the wire's 5.
func TestMemoryGuardIsOnAtTheDefaultsWhenTheFileSetsNoBlock(t *testing.T) {
	issue := MemoryGuardPodConfig{
		Enabled: true, ActPercent: shimwire.MemoryGuardActDefault, FloorPercent: shimwire.MemoryGuardFloorDefault,
		WaitSeconds: shimwire.MemoryGuardWaitDefault, IntervalMilliseconds: shimwire.MemoryGuardIntervalDefault,
	}
	controller := issue
	controller.WaitSeconds = shimwire.MemoryGuardControllerWaitDefault
	if issue != (MemoryGuardPodConfig{Enabled: true, ActPercent: 85, FloorPercent: 10, WaitSeconds: 5, IntervalMilliseconds: 100}) {
		t.Fatalf("the wire's issue pod defaults are %+v, want on at 85/10/5/100", issue)
	}
	if controller != (MemoryGuardPodConfig{Enabled: true, ActPercent: 85, FloorPercent: 10, WaitSeconds: 1, IntervalMilliseconds: 100}) {
		t.Fatalf("the wire's controller pod defaults are %+v, want on at 85/10/1/100", controller)
	}
	want := MemoryGuardConfig{IssuePods: issue, ControllerPod: controller}
	if got := loadMemoryGuard(t, ""); got != want {
		t.Fatalf("memory_guard = %+v, want both kinds on at the wire's defaults %+v", got, want)
	}
	if DefaultMemoryGuard() != want {
		t.Fatalf("DefaultMemoryGuard() = %+v, want %+v", DefaultMemoryGuard(), want)
	}
	// An empty block, and an empty block of either kind or of both, are the defaults as well.
	for _, block := range []string{
		"    memory_guard: {}\n",
		memoryGuardBlock("issue_pods", ""),
		memoryGuardBlock("controller_pod", ""),
		"    memory_guard: {issue_pods: {}, controller_pod: {}}\n",
	} {
		if got := loadMemoryGuard(t, block); got != want {
			t.Fatalf("memory_guard of %q = %+v, want the defaults %+v", block, got, want)
		}
	}
}

// A null block of either pod kind is unset, as every sibling block's null is, and loads as the
// defaults: in flow style and in block style, written with no value or as null.
func TestMemoryGuardANullPodKindBlockIsTheDefaults(t *testing.T) {
	for _, kind := range memoryGuardKinds {
		for _, block := range []string{
			"    memory_guard: {" + kind.name + ": }\n",
			"    memory_guard: {" + kind.name + ": null}\n",
			"    memory_guard:\n      " + kind.name + ":\n",
			"    memory_guard:\n      " + kind.name + ": ~\n",
		} {
			if got := loadMemoryGuard(t, block); got != DefaultMemoryGuard() {
				t.Fatalf("memory_guard of %q = %+v, want the defaults %+v", block, got, DefaultMemoryGuard())
			}
		}
	}
}

// Each key of either pod kind takes its value from the file and leaves the kind's other keys at
// their defaults, a file that sets every key gets every one, and the other kind stays at its
// defaults throughout.
func TestMemoryGuardTakesEachKeyFromTheFile(t *testing.T) {
	for _, kind := range memoryGuardKinds {
		for _, tc := range []struct {
			name, members string
			edit          func(*MemoryGuardPodConfig)
		}{
			{"enabled", "enabled: true", func(*MemoryGuardPodConfig) {}},
			{"act_percent", "act_percent: 70", func(c *MemoryGuardPodConfig) { c.ActPercent = 70 }},
			{"floor_percent", "floor_percent: 25", func(c *MemoryGuardPodConfig) { c.FloorPercent = 25 }},
			{"wait_seconds", "wait_seconds: 12", func(c *MemoryGuardPodConfig) { c.WaitSeconds = 12 }},
			{"interval_milliseconds", "interval_milliseconds: 400", func(c *MemoryGuardPodConfig) { c.IntervalMilliseconds = 400 }},
			{
				"every key", "enabled: true, act_percent: 90, floor_percent: 5, wait_seconds: 30, interval_milliseconds: 1000",
				func(c *MemoryGuardPodConfig) {
					*c = MemoryGuardPodConfig{Enabled: true, ActPercent: 90, FloorPercent: 5, WaitSeconds: 30, IntervalMilliseconds: 1000}
				},
			},
		} {
			t.Run(kind.name+" "+tc.name, func(t *testing.T) {
				want := DefaultMemoryGuard()
				tc.edit(kind.block(&want))
				if got := loadMemoryGuard(t, memoryGuardBlock(kind.name, tc.members)); got != want {
					t.Fatalf("memory_guard = %+v, want %+v", got, want)
				}
			})
		}
	}
}

// Whatever controller_pod holds, issue_pods settles on what its own block and its own defaults
// say, written before controller_pod or after it, and controller_pod takes every value its block
// sets: the two kinds share a reader and five key names but never a value.
func TestMemoryGuardIssuePodsIsUnchangedByAnyControllerPodValue(t *testing.T) {
	issue := MemoryGuardPodConfig{Enabled: true, ActPercent: 70, FloorPercent: 20, WaitSeconds: 9, IntervalMilliseconds: 250}
	const issueMembers = "enabled: true, act_percent: 70, floor_percent: 20, wait_seconds: 9, interval_milliseconds: 250"
	for _, tc := range []struct {
		name, members string
		want          MemoryGuardPodConfig
	}{
		{"off", "enabled: false", MemoryGuardPodConfig{Enabled: false, ActPercent: 85, FloorPercent: 10, WaitSeconds: 1, IntervalMilliseconds: 100}},
		{"act_percent", "act_percent: 90", MemoryGuardPodConfig{Enabled: true, ActPercent: 90, FloorPercent: 10, WaitSeconds: 1, IntervalMilliseconds: 100}},
		{"floor_percent", "floor_percent: 50", MemoryGuardPodConfig{Enabled: true, ActPercent: 85, FloorPercent: 50, WaitSeconds: 1, IntervalMilliseconds: 100}},
		{"wait_seconds", "wait_seconds: 60", MemoryGuardPodConfig{Enabled: true, ActPercent: 85, FloorPercent: 10, WaitSeconds: 60, IntervalMilliseconds: 100}},
		{"interval_milliseconds", "interval_milliseconds: 5000", MemoryGuardPodConfig{Enabled: true, ActPercent: 85, FloorPercent: 10, WaitSeconds: 1, IntervalMilliseconds: 5000}},
		{
			"every key", "enabled: false, act_percent: 50, floor_percent: 1, wait_seconds: 1, interval_milliseconds: 50",
			MemoryGuardPodConfig{Enabled: false, ActPercent: 50, FloorPercent: 1, WaitSeconds: 1, IntervalMilliseconds: 50},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, layout := range []struct {
				block string
				issue MemoryGuardPodConfig
			}{
				{memoryGuardBlock("controller_pod", tc.members), DefaultMemoryGuard().IssuePods},
				{"    memory_guard: {issue_pods: {" + issueMembers + "}, controller_pod: {" + tc.members + "}}\n", issue},
				{"    memory_guard: {controller_pod: {" + tc.members + "}, issue_pods: {" + issueMembers + "}}\n", issue},
			} {
				want := MemoryGuardConfig{IssuePods: layout.issue, ControllerPod: tc.want}
				if got := loadMemoryGuard(t, layout.block); got != want {
					t.Fatalf("memory_guard of %q = %+v, want %+v", layout.block, got, want)
				}
			}
		})
	}
}

// `enabled: false` turns one kind's guard off, keeps the numbers the file and the defaults settle
// on for it, so the state shows what turning it back on would run by, and leaves the other kind on.
func TestMemoryGuardEnabledFalseTurnsTheGuardOff(t *testing.T) {
	for _, kind := range memoryGuardKinds {
		t.Run(kind.name, func(t *testing.T) {
			want := DefaultMemoryGuard()
			off := kind.block(&want)
			off.Enabled, off.ActPercent = false, 75
			if got := loadMemoryGuard(t, memoryGuardBlock(kind.name, "enabled: false, act_percent: 75")); got != want {
				t.Fatalf("memory_guard = %+v, want %s off with the file's act share and the other kind on at the defaults: %+v", got, kind.name, want)
			}
		})
	}
}

// Each number of either pod kind is held to the wire's range, and the act share of both kinds to
// 90, below the wire's 95, so that the urgent share stays at most 95% of the limit (INFRA-3531 R4
// of spec version 43): both ends load, one past either end is refused naming the whole key and the
// range, so `legion config check` exits 1 saying where to look. The act share's 90 is written out
// rather than read from shimwire.MemoryGuardConfigActMax, so a bound moved there fails here.
func TestMemoryGuardRefusesEachNumberOutsideItsRangeNamingTheKey(t *testing.T) {
	for _, kind := range memoryGuardKinds {
		for _, number := range []struct {
			field    string
			min, max int
			get      func(MemoryGuardPodConfig) int
		}{
			{"act_percent", shimwire.MemoryGuardActMin, 90, func(c MemoryGuardPodConfig) int { return c.ActPercent }},
			{"floor_percent", shimwire.MemoryGuardFloorMin, shimwire.MemoryGuardFloorMax, func(c MemoryGuardPodConfig) int { return c.FloorPercent }},
			{"wait_seconds", shimwire.MemoryGuardWaitMin, shimwire.MemoryGuardWaitMax, func(c MemoryGuardPodConfig) int { return c.WaitSeconds }},
			{"interval_milliseconds", shimwire.MemoryGuardIntervalMin, shimwire.MemoryGuardIntervalMax, func(c MemoryGuardPodConfig) int { return c.IntervalMilliseconds }},
		} {
			refusal := fmt.Sprintf("runtime.kubernetes.memory_guard.%s.%s must be between %d and %d", kind.name, number.field, number.min, number.max)
			for _, value := range []int{number.min, number.max} {
				t.Run(fmt.Sprintf("%s %s at %d", kind.name, number.field, value), func(t *testing.T) {
					got := loadMemoryGuard(t, memoryGuardBlock(kind.name, fmt.Sprintf("%s: %d", number.field, value)))
					if read := number.get(*kind.block(&got)); read != value {
						t.Fatalf("%s.%s = %d, want %d", kind.name, number.field, read, value)
					}
				})
			}
			for _, value := range []int{number.min - 1, number.max + 1} {
				t.Run(fmt.Sprintf("%s %s at %d", kind.name, number.field, value), func(t *testing.T) {
					_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+memoryGuardBlock(kind.name, fmt.Sprintf("%s: %d", number.field, value))), noEnv)
					if err == nil || !strings.Contains(err.Error(), refusal) {
						t.Fatalf("LoadForValidation = %v, want the refusal %q", err, refusal)
					}
				})
			}
		}
	}
}

// What else the loader refuses, each refusal naming the key an operator has to edit: a member
// neither pod kind's block knows, a pod kind the mapping does not know, a block that is no
// mapping, an `enabled` that is no YAML boolean, and a number that is no integer.
func TestMemoryGuardRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"an unknown member of issue_pods", memoryGuardBlock("issue_pods", "act_share: 80"), "unknown key runtime.kubernetes.memory_guard.issue_pods.act_share"},
		{"an unknown member of controller_pod", memoryGuardBlock("controller_pod", "act_share: 80"), "unknown key runtime.kubernetes.memory_guard.controller_pod.act_share"},
		{"an unknown pod kind", "    memory_guard: {controller_pods: {enabled: true}}\n", "unknown key runtime.kubernetes.memory_guard.controller_pods"},
		{"memory_guard not a mapping", "    memory_guard: true\n", "runtime.kubernetes.memory_guard must be a mapping"},
		{"issue_pods not a mapping", "    memory_guard: {issue_pods: [enabled]}\n", "runtime.kubernetes.memory_guard.issue_pods must be a mapping"},
		{"controller_pod not a mapping", "    memory_guard: {controller_pod: [enabled]}\n", "runtime.kubernetes.memory_guard.controller_pod must be a mapping"},
		{"enabled as yes", memoryGuardBlock("issue_pods", "enabled: yes"), "runtime.kubernetes.memory_guard.issue_pods.enabled must be true or false"},
		{"enabled as off", memoryGuardBlock("issue_pods", "enabled: off"), "runtime.kubernetes.memory_guard.issue_pods.enabled must be true or false"},
		{"enabled as a number", memoryGuardBlock("issue_pods", "enabled: 1"), "runtime.kubernetes.memory_guard.issue_pods.enabled must be true or false"},
		{"the controller pod's enabled as off", memoryGuardBlock("controller_pod", "enabled: off"), "runtime.kubernetes.memory_guard.controller_pod.enabled must be true or false"},
		{"a share that is no integer", memoryGuardBlock("issue_pods", "act_percent: 85.5"), "runtime.kubernetes.memory_guard.issue_pods.act_percent must be an integer"},
		{"a wait as a duration", memoryGuardBlock("issue_pods", "wait_seconds: 5s"), "runtime.kubernetes.memory_guard.issue_pods.wait_seconds must be an integer"},
		{"the controller pod's wait as a duration", memoryGuardBlock("controller_pod", "wait_seconds: 5s"), "runtime.kubernetes.memory_guard.controller_pod.wait_seconds must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err == nil {
				t.Fatalf("LoadForValidation accepted %q", tc.block)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadForValidation = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
