package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
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

// detectorOnWith resolves the writing check's settings under a bundle that ships the detector
// on, which no running server is given (writingcheck.DetectorEnabled): it is how the model
// group's settings are read and refused once a bundle turns the detector on.
func detectorOnWith(t *testing.T, overrides map[string]string) writingCheckConfig {
	t.Helper()
	cfg, err := resolveWritingCheckUnder(envGetter(overrides), false, true)
	if err != nil {
		t.Fatalf("resolveWritingCheckUnder(%v): %v", overrides, err)
	}
	return cfg
}

func detectorOnRefused(t *testing.T, overrides map[string]string, naming string) {
	t.Helper()
	if _, err := resolveWritingCheckUnder(envGetter(overrides), false, true); err == nil || !strings.Contains(err.Error(), naming) {
		t.Errorf("resolveWritingCheckUnder(%v): err = %v, want a refusal naming %s", overrides, err, naming)
	}
}

// The bundle ships the detector off, so the boot reads no model whatever the settings say:
// DISPATCH_WRITING_CHECK_DETECTOR=on and a whole model group still build a check of the
// structure rules alone, and a model group set in part boots, accepted and ignored.
func TestWritingCheckSettingsCannotTurnTheBundlesDetectorOn(t *testing.T) {
	if writingcheck.DetectorEnabled {
		t.Skip("the bundle ships the detector on")
	}
	boot := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_DETECTOR": "on"}))
	if !boot.WritingCheck.Enabled || boot.WritingCheck.Detector || boot.WritingCheck.Model {
		t.Errorf("WritingCheck = %+v, want on with the detector off and no model", boot.WritingCheck)
	}
	if partial := resolveWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": ""})); partial.WritingCheck.Model {
		t.Errorf("a partial model group: WritingCheck = %+v, want it ignored", partial.WritingCheck)
	}
	check, err := newWritingCheck(boot.WritingCheck)
	if err != nil || check == nil {
		t.Fatalf("newWritingCheck: %v, %v", check, err)
	}
}

// Under a bundle that ships the detector on, the settings build the check they describe, with
// its own reader model: the answerer and the embedder stay as they were.
func TestWritingCheckSettingsBuildTheCheck(t *testing.T) {
	cfg := detectorOnWith(t, writingCheckEnvironment(nil))
	if !cfg.Enabled || !cfg.Detector || !cfg.Model || cfg.Scope != embed.DefaultEntraScope || cfg.TokensPerMinute != 60_000 {
		t.Fatalf("WritingCheck = %+v", cfg)
	}
	boot := resolveWith(t, writingCheckEnvironment(nil))
	if boot.Answerer.Place != "" || boot.Embedder.Place == embed.PlaceAzure {
		t.Errorf("the writing check's model configured the answerer or the embedder: %+v, %+v", boot.Answerer, boot.Embedder)
	}
	check, err := newWritingCheck(cfg)
	if err != nil || check == nil {
		t.Fatalf("newWritingCheck: %v, %v", check, err)
	}
	// With no model group the check runs its structure rules alone, and boots.
	if plain := detectorOnWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on"}); !plain.Enabled || plain.Model {
		t.Errorf("no model group: WritingCheck = %+v, want on with no model", plain)
	}
	// Off, the group is accepted and ignored.
	if off := detectorOnWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK": "off"})); off.Enabled || off.Model {
		t.Errorf("check off: WritingCheck = %+v, want off with no model", off)
	}
	detectorOff := detectorOnWith(t, writingCheckEnvironment(map[string]string{
		"DISPATCH_WRITING_CHECK_DETECTOR": "off", "DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": "",
	}))
	if detectorOff.Detector || detectorOff.Model {
		t.Errorf("detector off: WritingCheck = %+v, want no model read", detectorOff)
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
			boot := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK_RULES_OFF": " phrasal-verb , vacuous-verb-phrase,,phrasal-verb"})
			if got := boot.WritingCheck.RulesOff; len(got) != 2 || got[0] != "phrasal-verb" || got[1] != "vacuous-verb-phrase" {
				t.Errorf("RulesOff = %v, want phrasal-verb and vacuous-verb-phrase", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK_RULES_OFF": "passive-voice"}, `DISPATCH_WRITING_CHECK_RULES_OFF names "passive-voice"`)
			// The detector's rule off means no model is read, so a model group set beside it is
			// accepted and ignored.
			if cfg := detectorOnWith(t, writingCheckEnvironment(map[string]string{
				"DISPATCH_WRITING_CHECK_RULES_OFF": "vacuous-verb-phrase", "DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": "",
			})); cfg.Model {
				t.Error("detector rule off: the model is read")
			}
		},
		"DISPATCH_WRITING_CHECK_DETECTOR": func(t *testing.T) {
			if cfg := detectorOnWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on"}); !cfg.Detector {
				t.Error("unset under a bundle that ships it on: the detector is off")
			}
			if cfg := detectorOnWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on", "DISPATCH_WRITING_CHECK_DETECTOR": "off"}); cfg.Detector {
				t.Error("off: the detector is on")
			}
			if boot := resolveWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on", "DISPATCH_WRITING_CHECK_DETECTOR": "on"}); boot.WritingCheck.Detector != writingcheck.DetectorEnabled {
				t.Errorf("on: Detector = %t, want the bundle's %t", boot.WritingCheck.Detector, writingcheck.DetectorEnabled)
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK_DETECTOR": "no"}, `DISPATCH_WRITING_CHECK_DETECTOR="no" (expected on or off)`)
		},
		"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": func(t *testing.T) {
			if cfg := detectorOnWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": " https://Check.Example/v1 "})); cfg.Endpoint != "https://Check.Example/v1" {
				t.Errorf("Endpoint = %q, want the trimmed URL", cfg.Endpoint)
			}
			detectorOnRefused(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": ""}), "DISPATCH_WRITING_CHECK_AZURE_ENDPOINT missing")
			detectorOnRefused(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_ENDPOINT": "http://check.example/v1"}), "DISPATCH_WRITING_CHECK_AZURE_ENDPOINT=")
		},
		"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": func(t *testing.T) {
			if cfg := detectorOnWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": " small-model "})); cfg.Deployment != "small-model" {
				t.Errorf("Deployment = %q, want the trimmed name", cfg.Deployment)
			}
			detectorOnRefused(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT": ""}), "DISPATCH_WRITING_CHECK_AZURE_DEPLOYMENT missing")
		},
		"DISPATCH_WRITING_CHECK_AZURE_API_VERSION": func(t *testing.T) {
			if cfg := detectorOnWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_API_VERSION": " 2024-05-01-preview "})); cfg.APIVersion != "2024-05-01-preview" {
				t.Errorf("APIVersion = %q", cfg.APIVersion)
			}
			if cfg := detectorOnWith(t, writingCheckEnvironment(nil)); cfg.APIVersion != "" {
				t.Errorf("unset: APIVersion = %q, want none sent", cfg.APIVersion)
			}
		},
		"DISPATCH_WRITING_CHECK_AZURE_SCOPE": func(t *testing.T) {
			if cfg := detectorOnWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_SCOPE": " https://ai.azure.com/.default "})); cfg.Scope != "https://ai.azure.com/.default" {
				t.Errorf("Scope = %q", cfg.Scope)
			}
			if cfg := detectorOnWith(t, writingCheckEnvironment(nil)); cfg.Scope != embed.DefaultEntraScope {
				t.Errorf("unset: Scope = %q, want %q", cfg.Scope, embed.DefaultEntraScope)
			}
		},
		"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": func(t *testing.T) {
			if cfg := detectorOnWith(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": " 90000 "})); cfg.TokensPerMinute != 90_000 {
				t.Errorf("TokensPerMinute = %d", cfg.TokensPerMinute)
			}
			detectorOnRefused(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": ""}), "DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE missing")
			detectorOnRefused(t, writingCheckEnvironment(map[string]string{"DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE": "0"}), `DISPATCH_WRITING_CHECK_AZURE_TOKENS_PER_MINUTE="0"`)
			detectorOnRefused(t, writingCheckEnvironment(map[string]string{"DISPATCH_AZURE_TENANT_ID": ""}), "DISPATCH_AZURE_TENANT_ID missing")
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
