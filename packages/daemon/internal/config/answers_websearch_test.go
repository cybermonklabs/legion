package config

import (
	"reflect"
	"testing"
)

// The answers.web_search pieces a table row assembles: a block for the priced Anthropic model, and
// the two reasons the loop offers no web_search tool, which the boot's `answers: tools` line and
// `legion config check` name (Answers.WebSearchOff).
const (
	opusSearch             = "  web_search: {model: anthropic/claude-opus-5-5, price_per_search: 0.01}\n"
	webSearchOffNoBlock    = "no answers.web_search block (set its model and price_per_search)"
	webSearchOffNoProvider = "answers.providers names no anthropic, the provider every search runs on"
)

// A web_search block whose model is an Anthropic model answers.prices prices settles as written:
// its model, as answers.prices names it, and its price per search, a plain number of USD, which
// may be 0. The loop offers the tool, and the loader warns of nothing.
func TestAnswersWebSearchSettlesItsModelAndPricePerSearch(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		want        AnswerWebSearch
	}{
		{"a cent a search", opusSearch, AnswerWebSearch{Model: "anthropic/claude-opus-5-5", PricePerSearch: 0.01}},
		{"free searches", "  web_search: {model: anthropic/claude-opus-5-5, price_per_search: 0}\n", AnswerWebSearch{Model: "anthropic/claude-opus-5-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+answersBlock(anthropicByFile+openaiByCommand, opusPrice+gptPrice, tc.block)), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Answers == nil || cfg.Answers.WebSearch == nil || *cfg.Answers.WebSearch != tc.want {
				t.Fatalf("Answers = %+v, want web_search %+v", cfg.Answers, tc.want)
			}
			if off := cfg.Answers.WebSearchOff(); off != "" {
				t.Errorf("WebSearchOff = %q, want the tool offered", off)
			}
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %#v, want none", cfg.Warnings)
			}
		})
	}
}

// A web_search block missing either key, naming a model of another provider or no model id at
// all, or naming an Anthropic model answers.prices does not price (a search call's tokens are
// priced there) is refused at load with the key and the reason, as every other malformed answers
// key is; so is a price per search that is negative or no number, a block that is no mapping, and
// a key it does not model. A block whose answers names no anthropic provider, so the loop offers
// no tool, is still held to its shape: only the price check needs the provider, since without it
// answers.prices can price no Anthropic model.
func TestAnswersWebSearchIsRefusedForEachMissingKeyAndEachModelItCannotRunOn(t *testing.T) {
	both := anthropicByFile + openaiByCommand
	for _, tc := range []struct{ name, body, want string }{
		{"no model", answersBlock(anthropicByFile, opusPrice, "  web_search: {price_per_search: 0.01}\n"),
			"answers.web_search.model is required: the Anthropic model each search runs on, anthropic/<model> as answers.prices names it"},
		{"no price per search", answersBlock(anthropicByFile, opusPrice, "  web_search: {model: anthropic/claude-opus-5-5}\n"),
			"answers.web_search.price_per_search is required: USD per search, a plain number and not per million tokens"},
		{"an OpenAI model", answersBlock(both, opusPrice+gptPrice, "  web_search: {model: openai/gpt-6.1, price_per_search: 0.01}\n"),
			`answers.web_search.model is "openai/gpt-6.1", which is no Anthropic model: every search runs on Anthropic's Messages API with its web search tool, so name an anthropic/<model>`},
		{"no model id", answersBlock(anthropicByFile, opusPrice, "  web_search: {model: claude-opus-5-5, price_per_search: 0.01}\n"),
			`answers.web_search.model is "claude-opus-5-5", which is no Anthropic model: every search runs on Anthropic's Messages API with its web search tool, so name an anthropic/<model>`},
		{"a model with no price", answersBlock(anthropicByFile, opusPrice, "  web_search: {model: anthropic/claude-sonnet-5-5, price_per_search: 0.01}\n"),
			"answers.web_search.model anthropic/claude-sonnet-5-5 has no price in answers.prices, which prices each search's tokens; price it there"},
		{"a negative price per search", answersBlock(anthropicByFile, opusPrice, "  web_search: {model: anthropic/claude-opus-5-5, price_per_search: -0.01}\n"),
			"answers.web_search.price_per_search must be a non-negative number"},
		{"a price per search that is no number", answersBlock(anthropicByFile, opusPrice, "  web_search: {model: anthropic/claude-opus-5-5, price_per_search: a cent}\n"),
			"answers.web_search.price_per_search must be a number"},
		{"not a mapping", answersBlock(anthropicByFile, opusPrice, "  web_search: on\n"),
			"answers.web_search must be a mapping of model and price_per_search"},
		{"an unknown key", answersBlock(anthropicByFile, opusPrice, "  web_search: {model: anthropic/claude-opus-5-5, price_per_search: 0.01, max_uses: 2}\n"),
			"unknown key answers.web_search.max_uses"},
		{"no anthropic provider, an OpenAI model", answersBlock(openaiByCommand, gptPrice, "  web_search: {model: openai/gpt-6.1, price_per_search: 0.01}\n"),
			`answers.web_search.model is "openai/gpt-6.1", which is no Anthropic model: every search runs on Anthropic's Messages API with its web search tool, so name an anthropic/<model>`},
		{"no provider at all, no model", "answers:\n  web_search: {price_per_search: 0.01}\n",
			"answers.web_search.model is required: the Anthropic model each search runs on, anthropic/<model> as answers.prices names it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadForValidation(writeConfigFile(t, minimalFile+tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("LoadForValidation = %v, want %q", err, tc.want)
			}
		})
	}
}

// No web_search block (absent or null), or a block where answers.providers names no anthropic, is
// no tool: the loop offers none, WebSearchOff says why in the words the boot's `answers: tools`
// line and `legion config check` print, and nothing else of the block changes, no warning among
// it. A block that names both reasons is told the first.
func TestAnswersWithNoWebSearchBlockOrNoAnthropicProviderOfferNoTool(t *testing.T) {
	for _, tc := range []struct {
		name, providers, prices, block string
		want                           *AnswerWebSearch
		off                            string
	}{
		{"no block", anthropicByFile, opusPrice, "", nil, webSearchOffNoBlock},
		{"a null block", anthropicByFile, opusPrice, "  web_search: null\n", nil, webSearchOffNoBlock},
		{"no anthropic provider", openaiByCommand, gptPrice, opusSearch,
			&AnswerWebSearch{Model: "anthropic/claude-opus-5-5", PricePerSearch: 0.01}, webSearchOffNoProvider},
		{"no anthropic provider and no block", openaiByCommand, gptPrice, "", nil, webSearchOffNoBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadForValidation(writeConfigFile(t, minimalFile+answersBlock(tc.providers, tc.prices, tc.block)), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation: %v", err)
			}
			if cfg.Answers == nil {
				t.Fatal("Answers = nil, want the loop on with no web_search tool")
			}
			if !reflect.DeepEqual(cfg.Answers.WebSearch, tc.want) {
				t.Errorf("WebSearch = %+v, want %+v", cfg.Answers.WebSearch, tc.want)
			}
			if off := cfg.Answers.WebSearchOff(); off != tc.off {
				t.Errorf("WebSearchOff = %q, want %q", off, tc.off)
			}
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %#v, want none: a loop with no web_search tool warns of nothing", cfg.Warnings)
			}
			without, err := LoadForValidation(writeConfigFile(t, minimalFile+answersBlock(tc.providers, tc.prices, "")), noEnv)
			if err != nil {
				t.Fatalf("LoadForValidation without the block: %v", err)
			}
			// Each file is written to a directory of its own, so each api_key_file resolves apart:
			// the providers are compared by their base URLs, and the rest of the block whole.
			got, want := *cfg.Answers, *without.Answers
			for provider, endpoint := range want.Providers {
				if got.Providers[provider].BaseURL != endpoint.BaseURL {
					t.Errorf("providers.%s = %+v, want %+v as without the block", provider, got.Providers[provider], endpoint)
				}
			}
			if len(got.Providers) != len(want.Providers) {
				t.Errorf("providers = %+v, want %+v as without the block", got.Providers, want.Providers)
			}
			got.WebSearch, got.Providers, want.Providers = nil, nil, nil
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Answers but web_search and providers = %+v, want the block without it %+v", got, want)
			}
		})
	}
}
