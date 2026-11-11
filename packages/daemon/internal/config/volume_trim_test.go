package config

import (
	"strings"
	"testing"
)

// A Kubernetes deployment that says nothing about the cache trimmer gets it anyway, on the
// daemon's own marks: every issue volume is a fixed size whose caches grow with the issue's work,
// so the trimmer is the default rather than a key an operator must find, as idle release is.
func TestVolumeTrimIsOnByDefaultForEveryKubernetesDeployment(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	got := cfg.Runtime.Kubernetes.VolumeTrim
	want := VolumeTrimConfig{Enabled: true, HighPercent: 85, LowPercent: 70, MinAgeMinutes: 15, IntervalSeconds: 60}
	if got != want {
		t.Fatalf("volume_trim = %#v, want %#v", got, want)
	}
	if got != DefaultVolumeTrim() {
		t.Fatalf("volume_trim = %#v, want DefaultVolumeTrim() %#v", got, DefaultVolumeTrim())
	}
}

// Each member settles on its own: the file's where it sets one, the default for the rest.
func TestVolumeTrimSettlesEachMemberTheFileLeavesOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
		want  VolumeTrimConfig
	}{
		{"high alone", "    volume_trim: {high_percent: 95}\n", VolumeTrimConfig{Enabled: true, HighPercent: 95, LowPercent: 70, MinAgeMinutes: 15, IntervalSeconds: 60}},
		{"low alone", "    volume_trim: {low_percent: 50}\n", VolumeTrimConfig{Enabled: true, HighPercent: 85, LowPercent: 50, MinAgeMinutes: 15, IntervalSeconds: 60}},
		{"age alone", "    volume_trim: {min_age_minutes: 120}\n", VolumeTrimConfig{Enabled: true, HighPercent: 85, LowPercent: 70, MinAgeMinutes: 120, IntervalSeconds: 60}},
		{"interval alone", "    volume_trim: {interval_seconds: 3600}\n", VolumeTrimConfig{Enabled: true, HighPercent: 85, LowPercent: 70, MinAgeMinutes: 15, IntervalSeconds: 3600}},
		{
			"every member", "    volume_trim: {enabled: true, high_percent: 90, low_percent: 60, min_age_minutes: 1, interval_seconds: 30}\n",
			VolumeTrimConfig{Enabled: true, HighPercent: 90, LowPercent: 60, MinAgeMinutes: 1, IntervalSeconds: 30},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+tc.block), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if got := cfg.Runtime.Kubernetes.VolumeTrim; got != tc.want {
				t.Fatalf("volume_trim = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// `enabled: false` turns the trimmer off, and leaves every mark the file or the default set: the
// daemon hands such a deployment's launcher no settings at all, which is what it reads as "run no
// trimmer" (sandbox.Options.VolumeTrim).
func TestVolumeTrimEnabledFalseTurnsTheTrimmerOff(t *testing.T) {
	cfg, err := LoadForValidation(writeConfigFile(t, kubernetesFile+"    volume_trim: {enabled: false}\n"), noEnv)
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	got := cfg.Runtime.Kubernetes.VolumeTrim
	if got.Enabled {
		t.Fatalf("volume_trim = %#v, want the trimmer off", got)
	}
	if want := (VolumeTrimConfig{HighPercent: 85, LowPercent: 70, MinAgeMinutes: 15, IntervalSeconds: 60}); got != want {
		t.Fatalf("volume_trim = %#v, want %#v: the marks keep their defaults", got, want)
	}
}

// What the loader refuses, each refusal naming the key an operator has to edit, so
// `legion start --check-config` exits 1 saying where to look.
func TestVolumeTrimRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		// `off` is no YAML boolean to this loader's reader: it reaches the reader as the string,
		// as `production_check`'s does (readProject), and the key takes true or false alone.
		{
			"enabled off", "    volume_trim: {enabled: off}\n",
			"runtime.kubernetes.volume_trim.enabled must be true or false",
		},
		{
			"enabled a word", "    volume_trim: {enabled: sometimes}\n",
			"runtime.kubernetes.volume_trim.enabled must be true or false",
		},
		{
			"a zero high mark", "    volume_trim: {high_percent: 0}\n",
			"runtime.kubernetes.volume_trim.high_percent must be a positive integer",
		},
		{
			"a high mark at a full volume", "    volume_trim: {high_percent: 100}\n",
			"runtime.kubernetes.volume_trim.high_percent must be at most 99",
		},
		{
			"a zero low mark", "    volume_trim: {low_percent: 0}\n",
			"runtime.kubernetes.volume_trim.low_percent must be a positive integer",
		},
		{
			"a low mark at a full volume", "    volume_trim: {low_percent: 100}\n",
			"runtime.kubernetes.volume_trim.low_percent must be at most 99",
		},
		{
			"marks that meet", "    volume_trim: {high_percent: 80, low_percent: 80}\n",
			"runtime.kubernetes.volume_trim.low_percent (80) is not below runtime.kubernetes.volume_trim.high_percent (80)",
		},
		{
			"a low mark above the high one", "    volume_trim: {high_percent: 60, low_percent: 70}\n",
			"runtime.kubernetes.volume_trim.low_percent (70) is not below runtime.kubernetes.volume_trim.high_percent (60)",
		},
		{
			"a low mark above the default high one", "    volume_trim: {low_percent: 90}\n",
			"runtime.kubernetes.volume_trim.low_percent (90) is not below runtime.kubernetes.volume_trim.high_percent (85)",
		},
		{
			"a zero age", "    volume_trim: {min_age_minutes: 0}\n",
			"runtime.kubernetes.volume_trim.min_age_minutes must be a positive integer",
		},
		// A zero period is no nicety: the trimmer arms a time.Ticker with it, which panics on a
		// non-positive period, in the process that is PID 1 of the pod's roles container.
		{
			"a zero interval", "    volume_trim: {interval_seconds: 0}\n",
			"runtime.kubernetes.volume_trim.interval_seconds must be a positive integer",
		},
		{
			"an interval past an hour", "    volume_trim: {interval_seconds: 3601}\n",
			"runtime.kubernetes.volume_trim.interval_seconds must be at most 3600",
		},
		{
			"an unknown member", "    volume_trim: {high_pct: 85}\n",
			"unknown key runtime.kubernetes.volume_trim.high_pct",
		},
		{
			"not a mapping", "    volume_trim: 85\n",
			"runtime.kubernetes.volume_trim must be a mapping",
		},
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
