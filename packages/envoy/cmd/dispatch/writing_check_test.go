package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
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

// standInEnvironment is a test process's boot environment with the writing check on, its
// detector turned on by the test hook and its reader pointed at a stand-in at readerURL, with
// overrides applied.
func standInEnvironment(readerURL string, overrides map[string]string) map[string]string {
	return withOverrides(map[string]string{
		"DISPATCH_TEST_HOOKS":                    "1",
		"DISPATCH_WRITING_CHECK":                 "on",
		"DISPATCH_TEST_WRITING_CHECK_DETECTOR":   "on",
		"DISPATCH_TEST_WRITING_CHECK_READER_URL": readerURL,
	}, overrides)
}

// The stand-in reader a test process's settings build calls the stand-in's /chat/completions over
// plain HTTP with no token and no api-version, in the protocol the Azure deployment speaks, and
// reads its reply as the reader's verdicts.
func TestWritingCheckStandInReaderSpeaksTheReadersProtocol(t *testing.T) {
	type request struct{ path, query, auth, model string }
	var mu sync.Mutex
	var got []request
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, request{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), body.Model})
		mu.Unlock()
		var sent struct {
			Candidates []struct{ ID, Text string } `json:"candidates"`
		}
		for _, message := range body.Messages {
			if text, ok := message.Content.(string); ok && message.Role == "user" {
				_ = json.Unmarshal([]byte(text), &sent)
			}
		}
		reply := reader.Reply{Vacuous: []reader.Named{}}
		for _, c := range sent.Candidates {
			if strings.Contains(c.Text, "names the problem") {
				reply.Vacuous = append(reply.Vacuous, reader.Named{ID: c.ID, Rewrite: "Say what the problem is."})
			}
		}
		text, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []map[string]any{
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": string(text)}, "finish_reason": nil}}},
			{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
		} {
			chunk["id"], chunk["object"], chunk["created"], chunk["model"] = "chatcmpl-stand-in", "chat.completion.chunk", 1, "stand-in"
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(stand.Close)

	boot := resolveWith(t, standInEnvironment(stand.URL, nil))
	if cfg := boot.WritingCheck; !cfg.Enabled || !cfg.Detector || !cfg.DetectorTestHook || !cfg.Model || cfg.ReaderURL != stand.URL || cfg.Endpoint != "" || cfg.TenantID != "" {
		t.Fatalf("WritingCheck = %+v, want the detector on with the stand-in as its model and no Azure field", cfg)
	}
	if check, err := newWritingCheck(boot.WritingCheck); err != nil || check == nil {
		t.Fatalf("newWritingCheck: %v, %v", check, err)
	}
	standIn, shown, err := standInReader(boot.WritingCheck)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimPrefix(stand.URL, "http://"); shown.EndpointHost != want || shown.Deployment != standInReaderModel || shown.TokensPerMinute != 0 {
		t.Errorf("state model = %+v, want host %s", shown, want)
	}
	verdicts, outcome := standIn.Read(context.Background(), []candidates.Candidate{
		{ID: "c1", Sentence: prose.Sentence{Field: "body", Text: "This names the problem."}},
		{ID: "c2", Sentence: prose.Sentence{Field: "body", Text: "The cache was cold."}},
	})
	if outcome != reader.OutcomeRead || len(verdicts) != 2 || !verdicts[0].Vacuous || verdicts[0].Rewrite == "" || verdicts[1].Vacuous {
		t.Errorf("Read = %+v, %q, want c1 vacuous with its rewrite and c2 not", verdicts, outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].path != "/chat/completions" || got[0].query != "" || strings.TrimSpace(strings.TrimPrefix(got[0].auth, "Bearer")) != "" || got[0].model != standInReaderModel {
		t.Errorf("the stand-in got %+v, want one call to /chat/completions with no query and no token", got)
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
		"DISPATCH_TEST_WRITING_CHECK_DETECTOR": func(t *testing.T) {
			on := map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_WRITING_CHECK": "on", "DISPATCH_TEST_WRITING_CHECK_DETECTOR": "on"}
			if cfg := resolveWith(t, on).WritingCheck; !cfg.Detector || !cfg.DetectorTestHook {
				t.Errorf("on under the hooks: WritingCheck = %+v, want the detector on although the bundle ships it %t", cfg, writingcheck.DetectorEnabled)
			}
			if cfg := resolveWith(t, withOverrides(on, map[string]string{"DISPATCH_WRITING_CHECK_DETECTOR": "off"})).WritingCheck; cfg.Detector {
				t.Errorf("DISPATCH_WRITING_CHECK_DETECTOR=off beside it: WritingCheck = %+v, want the detector off", cfg)
			}
			// Without the hooks it is refused, as every test-only setting is, so no deployment
			// turns the detector on.
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on", "DISPATCH_TEST_WRITING_CHECK_DETECTOR": "on"},
				"DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_WRITING_CHECK_DETECTOR")
			refusedWith(t, withOverrides(on, map[string]string{"DISPATCH_TEST_HOOKS": "true"}), "DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_WRITING_CHECK_DETECTOR")
			refusedWith(t, withOverrides(on, map[string]string{"DISPATCH_TEST_WRITING_CHECK_DETECTOR": "1"}), `DISPATCH_TEST_WRITING_CHECK_DETECTOR="1" (expected on)`)
			if cfg := resolveWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_WRITING_CHECK": "on"}).WritingCheck; cfg.Detector != writingcheck.DetectorEnabled || cfg.DetectorTestHook {
				t.Errorf("unset: WritingCheck = %+v, want the bundle's detector", cfg)
			}
		},
		"DISPATCH_TEST_WRITING_CHECK_READER_URL": func(t *testing.T) {
			const standIn = "http://127.0.0.1:9/v1"
			if cfg := resolveWith(t, standInEnvironment(" "+standIn+" ", nil)).WritingCheck; !cfg.Model || cfg.ReaderURL != standIn {
				t.Errorf("WritingCheck = %+v, want the trimmed stand-in as the reader's model", cfg)
			}
			// Without the hooks it is refused, as every test-only setting is.
			refusedWith(t, map[string]string{"DISPATCH_WRITING_CHECK": "on", "DISPATCH_TEST_WRITING_CHECK_READER_URL": standIn},
				"DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_WRITING_CHECK_READER_URL")
			for _, invalid := range []string{"127.0.0.1:9", "ftp://127.0.0.1/v1", "http:///v1", "http://127.0.0.1/v1?x=1", "http://127.0.0.1/v1#a"} {
				refusedWith(t, standInEnvironment(invalid, nil), fmt.Sprintf("DISPATCH_TEST_WRITING_CHECK_READER_URL=%q (expected an absolute http or https URL", invalid))
			}
			// Beside the model group, while the detector runs, it is refused: one or the other.
			refusedWith(t, withOverrides(writingCheckEnvironment(nil), standInEnvironment(standIn, nil)),
				"DISPATCH_TEST_WRITING_CHECK_READER_URL set beside DISPATCH_WRITING_CHECK_AZURE_")
			// With the detector off, as the model group is, it is accepted and ignored.
			off := map[string]map[string]string{
				"the check off":    {"DISPATCH_WRITING_CHECK": "off"},
				"the detector off": {"DISPATCH_WRITING_CHECK_DETECTOR": "off"},
			}
			if !writingcheck.DetectorEnabled {
				off["the bundle's detector, with no test hook"] = map[string]string{"DISPATCH_TEST_WRITING_CHECK_DETECTOR": ""}
			}
			for name, overrides := range off {
				if cfg := resolveWith(t, standInEnvironment(standIn, overrides)).WritingCheck; cfg.Model || cfg.ReaderURL != "" {
					t.Errorf("%s: WritingCheck = %+v, want the stand-in ignored", name, cfg)
				}
			}
		},
	}
}
