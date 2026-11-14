package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// memoryGuardBlock is a runtime.kubernetes.memory_guard block holding issue_pods' members as one
// flow mapping, appended to kubernetesFile as its last member.
func memoryGuardBlock(members string) string {
	return "    memory_guard: {issue_pods: {" + members + "}}\n"
}

// A Kubernetes deployment that says nothing about the memory guard gets it anyway, on for issue
// pods at the wire's own defaults: the defaults are the shimwire constants themselves, which a
// launcher's guard runs by before any acknowledgement reaches it, so the two never differ.
func TestMemoryGuardIsOnAtTheDefaultsWhenTheFileSetsNoBlock(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	want := MemoryGuardConfig{IssuePods: MemoryGuardPodConfig{
		Enabled: true, ActPercent: shimwire.MemoryGuardActDefault, FloorPercent: shimwire.MemoryGuardFloorDefault,
		WaitSeconds: shimwire.MemoryGuardWaitDefault, IntervalMilliseconds: shimwire.MemoryGuardIntervalDefault,
	}}
	if got := cfg.Runtime.Kubernetes.MemoryGuard; got != want {
		t.Fatalf("memory_guard = %+v, want on at the wire's defaults %+v", got, want)
	}
	if DefaultMemoryGuard() != want {
		t.Fatalf("DefaultMemoryGuard() = %+v, want %+v", DefaultMemoryGuard(), want)
	}
	// An empty block, and an empty issue_pods, are the defaults as well.
	for _, block := range []string{"    memory_guard: {}\n", memoryGuardBlock("")} {
		cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+block), noEnv)
		if err != nil {
			t.Fatalf("LoadForValidation of %q: %v", block, err)
		}
		if got := cfg.Runtime.Kubernetes.MemoryGuard; got != want {
			t.Fatalf("memory_guard of %q = %+v, want the defaults %+v", block, got, want)
		}
	}
}

// Each key takes its value from the file and leaves the others at their defaults, and a file that
// sets every key gets every one.
func TestMemoryGuardTakesEachKeyFromTheFile(t *testing.T) {
	defaults := DefaultMemoryGuard().IssuePods
	with := func(edit func(*MemoryGuardPodConfig)) MemoryGuardPodConfig {
		settled := defaults
		edit(&settled)
		return settled
	}
	for _, tc := range []struct {
		name, members string
		want          MemoryGuardPodConfig
	}{
		{"enabled", "enabled: true", defaults},
		{"act_percent", "act_percent: 70", with(func(c *MemoryGuardPodConfig) { c.ActPercent = 70 })},
		{"floor_percent", "floor_percent: 25", with(func(c *MemoryGuardPodConfig) { c.FloorPercent = 25 })},
		{"wait_seconds", "wait_seconds: 12", with(func(c *MemoryGuardPodConfig) { c.WaitSeconds = 12 })},
		{"interval_milliseconds", "interval_milliseconds: 400", with(func(c *MemoryGuardPodConfig) { c.IntervalMilliseconds = 400 })},
		{
			"every key", "enabled: true, act_percent: 90, floor_percent: 5, wait_seconds: 30, interval_milliseconds: 1000",
			MemoryGuardPodConfig{Enabled: true, ActPercent: 90, FloorPercent: 5, WaitSeconds: 30, IntervalMilliseconds: 1000},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+memoryGuardBlock(tc.members)), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if got := cfg.Runtime.Kubernetes.MemoryGuard.IssuePods; got != tc.want {
				t.Fatalf("memory_guard.issue_pods = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// `enabled: false` turns the guard off, and keeps the numbers the file and the defaults settle on,
// so the state shows what turning it back on would run by.
func TestMemoryGuardEnabledFalseTurnsTheGuardOff(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+memoryGuardBlock("enabled: false, act_percent: 75")), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	want := DefaultMemoryGuard().IssuePods
	want.Enabled, want.ActPercent = false, 75
	if got := cfg.Runtime.Kubernetes.MemoryGuard.IssuePods; got != want {
		t.Fatalf("memory_guard.issue_pods = %+v, want the guard off with the file's act share %+v", got, want)
	}
}

// Each number is held to the wire's range: both ends load, one past either end is refused naming
// the whole key and the range, so `legion start --check-config` exits 1 saying where to look.
func TestMemoryGuardRefusesEachNumberOutsideItsRangeNamingTheKey(t *testing.T) {
	for _, number := range []struct {
		field    string
		min, max int
		get      func(MemoryGuardPodConfig) int
	}{
		{"act_percent", shimwire.MemoryGuardActMin, shimwire.MemoryGuardActMax, func(c MemoryGuardPodConfig) int { return c.ActPercent }},
		{"floor_percent", shimwire.MemoryGuardFloorMin, shimwire.MemoryGuardFloorMax, func(c MemoryGuardPodConfig) int { return c.FloorPercent }},
		{"wait_seconds", shimwire.MemoryGuardWaitMin, shimwire.MemoryGuardWaitMax, func(c MemoryGuardPodConfig) int { return c.WaitSeconds }},
		{"interval_milliseconds", shimwire.MemoryGuardIntervalMin, shimwire.MemoryGuardIntervalMax, func(c MemoryGuardPodConfig) int { return c.IntervalMilliseconds }},
	} {
		refusal := fmt.Sprintf("runtime.kubernetes.memory_guard.issue_pods.%s must be between %d and %d", number.field, number.min, number.max)
		for _, value := range []int{number.min, number.max} {
			t.Run(fmt.Sprintf("%s at %d", number.field, value), func(t *testing.T) {
				cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+memoryGuardBlock(fmt.Sprintf("%s: %d", number.field, value))), noEnv)
				if err != nil {
					t.Fatalf("LoadForValidation refused %s at the end of its range: %v", number.field, err)
				}
				if got := number.get(cfg.Runtime.Kubernetes.MemoryGuard.IssuePods); got != value {
					t.Fatalf("%s = %d, want %d", number.field, got, value)
				}
			})
		}
		for _, value := range []int{number.min - 1, number.max + 1} {
			t.Run(fmt.Sprintf("%s at %d", number.field, value), func(t *testing.T) {
				_, err := LoadForValidation(writeConfigFile(t, kubernetesFile+memoryGuardBlock(fmt.Sprintf("%s: %d", number.field, value))), noEnv)
				if err == nil || !strings.Contains(err.Error(), refusal) {
					t.Fatalf("LoadForValidation = %v, want the refusal %q", err, refusal)
				}
			})
		}
	}
}

// What else the loader refuses, each refusal naming the key an operator has to edit: a member
// neither block knows (controller_pod among them, until the controller's pod has a guard of its
// own to configure), a block that is no mapping, an `enabled` that is no YAML boolean, and a number
// that is no integer.
func TestMemoryGuardRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"an unknown member of issue_pods", memoryGuardBlock("act_share: 80"), "unknown key runtime.kubernetes.memory_guard.issue_pods.act_share"},
		{"an unknown pod kind", "    memory_guard: {controller_pod: {enabled: true}}\n", "unknown key runtime.kubernetes.memory_guard.controller_pod"},
		{"memory_guard not a mapping", "    memory_guard: true\n", "runtime.kubernetes.memory_guard must be a mapping"},
		{"issue_pods not a mapping", "    memory_guard: {issue_pods: [enabled]}\n", "runtime.kubernetes.memory_guard.issue_pods must be a mapping"},
		{"enabled as yes", memoryGuardBlock("enabled: yes"), "runtime.kubernetes.memory_guard.issue_pods.enabled must be true or false"},
		{"enabled as off", memoryGuardBlock("enabled: off"), "runtime.kubernetes.memory_guard.issue_pods.enabled must be true or false"},
		{"enabled as a number", memoryGuardBlock("enabled: 1"), "runtime.kubernetes.memory_guard.issue_pods.enabled must be true or false"},
		{"a share that is no integer", memoryGuardBlock("act_percent: 85.5"), "runtime.kubernetes.memory_guard.issue_pods.act_percent must be an integer"},
		{"a wait as a duration", memoryGuardBlock("wait_seconds: 5s"), "runtime.kubernetes.memory_guard.issue_pods.wait_seconds must be an integer"},
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
