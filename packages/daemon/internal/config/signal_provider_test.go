package config

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// signalProviderFile is minimalFile with members, a YAML flow mapping's members, as DEMO's entry.
func signalProviderFile(members string) string {
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {"+members+"}\n", 1)
}

// The two keys of the model provider's headroom are optional members of a project: a project that
// names none has no token limit and counts every model's tokens, and a project that names them is
// read as written. The model selector is a Go regular expression: the loader refuses, naming the
// key and why, an empty pattern and a pattern Go's regexp refuses. A refused file is refused whole,
// so no key of it takes effect, the valid ones beside the refused one included.
func TestSignalProviderConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Projects["DEMO"].Provider; got != (SignalProvider{}) {
			t.Fatalf("a project that names no provider key reads %+v, want no limit and every model", got)
		}
	})
	t.Run("a valid set", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, signalProviderFile(`signal_token_limit_per_minute: 5500000, `+
			`signal_token_model_pattern: '^claude-(opus|sonnet)-'`)), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := SignalProvider{TokenLimit: 5500000, TokenModels: "^claude-(opus|sonnet)-"}
		if got := cfg.Projects["DEMO"].Provider; got != want {
			t.Fatalf("the project's provider = %+v, want %+v", got, want)
		}
	})
	t.Run("refused", func(t *testing.T) {
		const selector = "projects.DEMO.signal_token_model_pattern"
		// goRefusal is the refusal of pattern, which Go's regexp refuses, naming the key and Go's reason.
		goRefusal := func(pattern string) string {
			_, err := regexp.Compile(pattern)
			if err == nil {
				t.Fatalf("Go's regexp takes %q, which the row says it refuses", pattern)
			}
			return selector + " " + strconv.Quote(pattern) + " is not a Go regular expression: " + err.Error()
		}
		for _, tc := range []struct {
			name, members, refusal string
		}{
			{"a limit of zero", `signal_token_limit_per_minute: 0`,
				"projects.DEMO.signal_token_limit_per_minute must be a positive integer"},
			{"a limit that is no number", `signal_token_limit_per_minute: lots`,
				"projects.DEMO.signal_token_limit_per_minute must be an integer"},
			{"an empty pattern", `signal_token_model_pattern: ''`,
				selector + " must not be empty: an empty pattern matches every model, as leaving the key out does"},
			{"a group left open", `signal_token_model_pattern: '(claude-'`, goRefusal("(claude-")},
			{"a group never opened", `signal_token_model_pattern: 'claude-)'`, goRefusal("claude-)")},
			{"a lookahead", `signal_token_model_pattern: 'claude(?=-opus)'`, goRefusal("claude(?=-opus)")},
			{"a back reference", `signal_token_model_pattern: '(a)\1'`, goRefusal(`(a)\1`)},
			{"a repetition of nothing", `signal_token_model_pattern: '*claude'`, goRefusal("*claude")},
			{"a bound above Go's", `signal_token_model_pattern: 'a{1001}'`, goRefusal("a{1001}")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg, err := Load(writeConfigFile(t, signalProviderFile(tc.members)), noEnv)
				if err == nil || err.Error() != tc.refusal {
					t.Fatalf("Load error = %v, want %q", err, tc.refusal)
				}
				if !reflect.DeepEqual(cfg, Config{}) {
					t.Fatalf("a refused file loads %+v, want nothing", cfg)
				}
			})
		}
	})
	// Each pattern below is one Go's regexp compiles, so the loader takes it as written.
	t.Run("taken", func(t *testing.T) {
		for _, pattern := range []string{`[\w-]+ claude`, `(?:opus|sonnet)* claude`, `claude\s(a+)+`, `[[:digit:]]+\pL`} {
			body := signalProviderFile(`signal_token_model_pattern: ` + yamlSingleQuoted(pattern))
			cfg, err := Load(writeConfigFile(t, body), noEnv)
			if err != nil {
				t.Fatalf("Load of %q: %v", pattern, err)
			}
			if got := cfg.Projects["DEMO"].Provider.TokenModels; got != pattern {
				t.Errorf("the model pattern = %q, want %q", got, pattern)
			}
		}
	})
	// A file whose every other provider key is valid is refused whole for the one that is not, so
	// a daemon started on it keeps none of them, the limit included.
	t.Run("a refused file changes nothing", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, signalProviderFile(`signal_token_limit_per_minute: 5500000, `+
			`signal_token_model_pattern: '(claude-'`)), noEnv)
		if err == nil || !strings.HasPrefix(err.Error(), "projects.DEMO.signal_token_model_pattern ") {
			t.Fatalf("Load error = %v, want the model pattern's refusal", err)
		}
		if !reflect.DeepEqual(cfg, Config{}) {
			t.Fatalf("a refused file loads %+v, want nothing of it, its valid keys included", cfg)
		}
	})
}

// yamlSingleQuoted is s as a single-quoted YAML scalar: each quote doubled.
func yamlSingleQuoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
