package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// writingCheckEnvironment is a boot environment with the writing check on and its reader's model
// configured, signed in with the shared Azure sign-in, with overrides applied.
func writingCheckEnvironment(overrides map[string]string) map[string]string {
	values := withOverrides(map[string]string{
		"DISPATCH_WRITING_CHECK":                         "on",
		"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT":          "https://check.example/openai/v1",
		"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT":        "check-deployment",
		"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": "60000",
	}, azureFederated)
	values = withOverrides(values, map[string]string{"DISPATCH_AZURE_EMBED_KEY": ""})
	return signInEnvironment(withOverrides(values, overrides))
}

// The writing check's settings build the check they describe, with its own reader model: the
// state route names the model's host, deployment and quota, and no credential.
func TestWritingCheckSettingsBuildTheCheck(t *testing.T) {
	boot := resolveWith(t, writingCheckEnvironment(nil))
	if !boot.WritingCheck.Enabled || !boot.WritingCheck.Detector || !boot.WritingCheck.Model ||
		boot.WritingCheck.Scope != embed.DefaultEntraScope || boot.WritingCheck.TokensPerMinute != 60_000 {
		t.Fatalf("WritingCheck = %+v", boot.WritingCheck)
	}
	if boot.Answerer.Place != "" || boot.Embedder.Place == embed.PlaceAzure {
		t.Errorf("the writing check's model configured the answerer or the embedder: %+v, %+v", boot.Answerer, boot.Embedder)
	}
	check, err := newWritingCheck(boot.WritingCheck)
	if err != nil {
		t.Fatalf("newWritingCheck: %v", err)
	}
	if check == nil {
		t.Fatal("newWritingCheck built no check")
	}
	// With no model group the check runs its structure rules alone, and boots.
	plain := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on"})
	if !plain.WritingCheck.Enabled || plain.WritingCheck.Model {
		t.Errorf("no model group: WritingCheck = %+v, want on with no model", plain.WritingCheck)
	}
	// Off, the group and the sign-in beside it are accepted and ignored.
	off := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK": "off"}))
	if off.WritingCheck.Enabled || off.WritingCheck.Model {
		t.Errorf("check off: WritingCheck = %+v, want off with no model", off.WritingCheck)
	}
	detectorOff := resolveWith(t, writingCheckEnvironment(map[string]string{
		"DISPATCH_WRITING_CHECK_DETECTOR": "off", "DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": "",
	}))
	if detectorOff.WritingCheck.Detector || detectorOff.WritingCheck.Model {
		t.Errorf("detector off: WritingCheck = %+v, want no model read", detectorOff.WritingCheck)
	}
}

// writingCheckSettingCases are the cases TestEverySettingReachesItsReader runs for the writing
// check's rows.
func writingCheckSettingCases() map[string]func(t *testing.T) {
	return map[string]func(t *testing.T){
		"DISPATCH_WRITING_CHECK": func(t *testing.T) {
			if boot := resolveWith(t, nil); boot.WritingCheck.Enabled {
				t.Error("unset: the writing check is on, want off")
			}
			if boot := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK": " on "}); !boot.WritingCheck.Enabled {
				t.Error("on: the writing check is off")
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "true"}, `DISPATCH_WRITING_CHECK="true" (expected on or off)`)
		},
		"DISPATCH_WRITING_CHECK_RULES_OFF": func(t *testing.T) {
			boot := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK_RULES_OFF": " semicolon , vacuous-verb-phrase,,semicolon"})
			if got := boot.WritingCheck.RulesOff; len(got) != 2 || got[0] != "semicolon" || got[1] != "vacuous-verb-phrase" {
				t.Errorf("RulesOff = %v, want semicolon and vacuous-verb-phrase", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK_RULES_OFF": "passive-voice"}, `DISPATCH_WRITING_CHECK_RULES_OFF names "passive-voice"`)
			// The detector's rule off means no model is read, so a model group set beside it is
			// accepted and ignored.
			if boot := resolveWith(t, writingCheckEnvironment(map[string]string{
				"DISPATCH_WRITING_CHECK_RULES_OFF": "vacuous-verb-phrase", "DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": "",
			})); boot.WritingCheck.Model {
				t.Error("detector rule off: the model is read")
			}
		},
		"DISPATCH_WRITING_CHECK_DETECTOR": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on"}); !boot.WritingCheck.Detector {
				t.Error("unset: the detector is off, want on once the check is on")
			}
			if boot := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on", "DISPATCH_WRITING_CHECK_DETECTOR": "off"}); boot.WritingCheck.Detector {
				t.Error("off: the detector is on")
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK_DETECTOR": "no"}, `DISPATCH_WRITING_CHECK_DETECTOR="no" (expected on or off)`)
		},
		"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": func(t *testing.T) {
			if boot := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": " https://Check.Example/v1 "})); boot.WritingCheck.Endpoint != "https://Check.Example/v1" {
				t.Errorf("Endpoint = %q, want the trimmed URL", boot.WritingCheck.Endpoint)
			}
			refusedWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": ""}), "DISPATCH_WRITING_CHECK_AZURE_ENDPOINT missing")
			refusedWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": "http://check.example/v1"}), "DISPATCH_WRITING_CHECK_AZURE_ENDPOINT=")
		},
		"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": func(t *testing.T) {
			if boot := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": " small-model "})); boot.WritingCheck.Deployment != "small-model" {
				t.Errorf("Deployment = %q, want the trimmed name", boot.WritingCheck.Deployment)
			}
			refusedWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": ""}), "DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT missing")
		},
		"DISPATCH_WRITING_CHECK_AZURE_API_VERSION": func(t *testing.T) {
			if boot := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_API_VERSION": " 2024-05-01-preview "})); boot.WritingCheck.APIVersion != "2024-05-01-preview" {
				t.Errorf("APIVersion = %q", boot.WritingCheck.APIVersion)
			}
			if boot := resolveWith(t, writingCheckEnvironment(nil)); boot.WritingCheck.APIVersion != "" {
				t.Errorf("unset: APIVersion = %q, want none sent", boot.WritingCheck.APIVersion)
			}
		},
		"DISPATCH_WRITING_CHECK_AZURE_SCOPE": func(t *testing.T) {
			if boot := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_SCOPE": " https://ai.azure.com/.default "})); boot.WritingCheck.Scope != "https://ai.azure.com/.default" {
				t.Errorf("Scope = %q", boot.WritingCheck.Scope)
			}
			if boot := resolveWith(t, writingCheckEnvironment(nil)); boot.WritingCheck.Scope != embed.DefaultEntraScope {
				t.Errorf("unset: Scope = %q, want %q", boot.WritingCheck.Scope, embed.DefaultEntraScope)
			}
		},
		"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": func(t *testing.T) {
			if boot := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": " 90000 "})); boot.WritingCheck.TokensPerMinute != 90_000 {
				t.Errorf("TokensPerMinute = %d", boot.WritingCheck.TokensPerMinute)
			}
			refusedWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": ""}), "DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE missing")
			refusedWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": "0"}), `DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE="0"`)
			refusedWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_AZURE_TENANT_ID": ""}), "DISPATCH_AZURE_TENANT_ID missing")
		},
		"DISPATCH_TEST_WRITING_CHECK_READER_TIMEOUT": func(t *testing.T) {
			if boot := resolveWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_WRITING_CHECK_READER_TIMEOUT": "5s"}); boot.WritingCheck.ReaderTimeout != 5*time.Second {
				t.Errorf("ReaderTimeout = %v, want 5s", boot.WritingCheck.ReaderTimeout)
			}
			refusedWith(t, map[string]string{"DISPATCH_TEST_WRITING_CHECK_READER_TIMEOUT": "5s"}, "DISPATCH_TEST_HOOKS=1 required")
			refusedWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_WRITING_CHECK_READER_TIMEOUT": "soon"}, "expected a positive Go duration")
		},
	}
}
