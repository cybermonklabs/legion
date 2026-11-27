package config

import (
	"fmt"
	"reflect"
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
// read as written. The model selector is one of the subset of regular expressions that Go's regexp
// and JavaScript's RegExp both accept and read alike: the loader refuses, naming the key and why, a
// pattern either engine refuses and a pattern the two read differently. A refused file is refused
// whole, so no key of it takes effect, the valid ones beside the refused one included.
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
		subset := func(pattern, why string) string {
			return selector + " " + quoted(pattern) + " is outside the subset of patterns Go and JavaScript both read alike: " + why
		}
		for _, tc := range []struct {
			name, members, refusal string
		}{
			{"a limit of zero", `signal_token_limit_per_minute: 0`,
				"projects.DEMO.signal_token_limit_per_minute must be a positive integer"},
			{"a limit that is no number", `signal_token_limit_per_minute: lots`,
				"projects.DEMO.signal_token_limit_per_minute must be an integer"},
			{"an empty pattern", `signal_token_model_pattern: ''`,
				subset("", "it is empty, and an empty pattern matches every text")},
			{"a group left open", `signal_token_model_pattern: '(claude-'`,
				subset("(claude-", "a group opened at 0 is never closed")},
			{"a group never opened", `signal_token_model_pattern: 'claude-)'`,
				subset("claude-)", "the ) at 7 closes no group")},
			{"a lookahead, which Go refuses", `signal_token_model_pattern: 'claude(?=-opus)'`,
				subset("claude(?=-opus)", "(? at 6 opens a lookaround, a named group or a flag; only (?: is in the subset")},
			{"a back reference, which Go refuses", `signal_token_model_pattern: '(a)\1'`,
				subset(`(a)\1`, `\1 at 3 is not in the subset`)},
			{"a repetition of nothing", `signal_token_model_pattern: '*claude'`,
				subset("*claude", "the * at 0 repeats nothing")},
			{"a repetition of a repetition", `signal_token_model_pattern: 'claude*+'`,
				subset("claude*+", "the + at 7 repeats nothing")},
			{"a bound above Go's", `signal_token_model_pattern: 'a{1001}'`,
				subset("a{1001}", "the repetition at 1 is outside 0 to 1000, the bound Go's regexp takes")},
			{"an empty class", `signal_token_model_pattern: 'a[]b'`,
				subset("a[]b", "the class at 1 opens with ], which Go reads as a member and JavaScript as the class's end")},
			{"a repetition of a group that holds one", `signal_token_model_pattern: '(a+)+'`,
				subset("(a+)+", nestedRepetition(4))},
			{"a repetition of a group whose group holds one", `signal_token_model_pattern: '((.*))*'`,
				subset("((.*))*", nestedRepetition(6))},
			{"a pattern with a property class", `signal_token_model_pattern: 'claude-\pL'`,
				subset(`claude-\pL`, `\p at 7 is not in the subset`)},
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
	// Each pattern below is one the two engines both take and read differently, so a host of each
	// would select one model two ways: the loader refuses each, naming what differs.
	t.Run("read differently", func(t *testing.T) {
		for _, tc := range []struct {
			name, pattern, why string
		}{
			{"a space class", `claude\sopus`, `\s at 6 matches the spaces of Unicode in JavaScript and only ASCII's in Go; write the spaces as a class, such as [ \t]`},
			{"a POSIX class", `[[:digit:]]+ claude`, "the [ at 1 inside a class: Go reads [:name:] as a POSIX class, and JavaScript as members; write \\[ for the character"},
			{"a start of text", `\Aclaude`, `\A at 0 is not in the subset`},
			{"a quoted run", `\Qa.b\E`, `\Q at 0 is not in the subset`},
			{"a brace alone", `claude{`, `the { at 6 begins no repetition; write \{ for the character`},
			{"a bracket alone", `claude]`, `the ] at 6 closes no class; write \] for the character`},
			{"a class of every character", `[^]`, "the class at 0 opens with ], which Go reads as a member and JavaScript as the class's end"},
			{"a dash after a digit class", `[\d-z]`, `the - at 3 follows a class escape, which begins no range: Go reads it as the character and JavaScript refuses it; write \- for the character`},
			{"a dash after a word class", `[\w-+]`, `the - at 3 follows a class escape, which begins no range: Go reads it as the character and JavaScript refuses it; write \- for the character`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := signalProviderFile(`signal_token_model_pattern: ` + yamlSingleQuoted(tc.pattern))
				cfg, err := Load(writeConfigFile(t, body), noEnv)
				want := "projects.DEMO.signal_token_model_pattern " + quoted(tc.pattern) +
					" is outside the subset of patterns Go and JavaScript both read alike: " + tc.why
				if err == nil || err.Error() != want {
					t.Fatalf("Load error = %v, want %q", err, want)
				}
				if !reflect.DeepEqual(cfg, Config{}) {
					t.Fatalf("a refused file loads %+v, want nothing", cfg)
				}
			})
		}
	})
	// Each pattern below is in the subset: a dash that ends a class, a repetition of a group that
	// holds none, and repetitions side by side.
	t.Run("taken", func(t *testing.T) {
		for _, pattern := range []string{`[\w-]+ claude`, `[\d\-z]`, `(ab)+`, `(a+)b+`, `(?:opus|sonnet)* claude`} {
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
			`signal_token_model_pattern: 'claude\sopus'`)), noEnv)
		if err == nil || !strings.HasPrefix(err.Error(), "projects.DEMO.signal_token_model_pattern ") {
			t.Fatalf("Load error = %v, want the model pattern's refusal", err)
		}
		if !reflect.DeepEqual(cfg, Config{}) {
			t.Fatalf("a refused file loads %+v, want nothing of it, its valid keys included", cfg)
		}
	})
	// The two keys of a refusal class this key table once held are no key of a project or of the
	// operator's controller file, so each is refused as any other key the loader does not know,
	// and nothing of the file loads.
	t.Run("a removed refusal key is no key", func(t *testing.T) {
		for _, key := range []string{"signal_own_refusal_pattern", "signal_provider_refusal_pattern"} {
			cfg, err := Load(writeConfigFile(t, signalProviderFile(key+`: 'overloaded'`)), noEnv)
			if want := fmt.Sprintf(`Unknown key "projects.DEMO.%s"`, key); err == nil || err.Error() != want {
				t.Fatalf("Load of %s = %v, want %q", key, err, want)
			}
			if !reflect.DeepEqual(cfg, Config{}) {
				t.Fatalf("a refused file loads %+v, want nothing", cfg)
			}
			refused, err := LoadController(controllerFile(t, requiredControllerKeys+key+": 'overloaded'\n"), "")
			if want := fmt.Sprintf(`unknown key %q in the controller configuration`, key); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("LoadController of %s = %v, want %q", key, err, want)
			}
			if !reflect.DeepEqual(refused, ControllerConfig{}) {
				t.Fatalf("a refused controller file loads %+v, want nothing", refused)
			}
		}
	})
}

// quoted is pattern as a refusal names it, Go-quoted.
func quoted(pattern string) string { return strconv.Quote(pattern) }

// nestedRepetition is the reason a repetition at at of a group that holds one is refused.
func nestedRepetition(at int) string {
	return fmt.Sprintf("the repetition at %d repeats a group that holds a repetition, which JavaScript backtracks through "+
		"for a time that grows exponentially with the text; repeat the group or what is inside it, not both", at)
}

// yamlSingleQuoted is s as a single-quoted YAML scalar: each quote doubled.
func yamlSingleQuoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
