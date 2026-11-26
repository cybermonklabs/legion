package config

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// signalProviderFile is minimalFile with members, a YAML flow mapping's members, as DEMO's entry.
func signalProviderFile(members string) string {
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {"+members+"}\n", 1)
}

// The four keys of the model provider's signals are optional members of a project: a project
// that names none has no token limit, counts every model's tokens and gives its hosts no refusal
// pattern, and a project that names them is read as written. A pattern is one of the subset of
// regular expressions that Go's regexp and JavaScript's RegExp both accept and read alike, since
// the Claude Code bridge (Go) and the Oh My Pi plugin (JavaScript) class a refusal by the same
// pattern: the loader refuses, naming the key and why, a pattern either engine refuses and a
// pattern the two read differently. A refused file is refused whole, so no key of it takes
// effect, the valid ones beside the refused one included. The operator's controller file takes
// the two refusal patterns under the same names and by the same rule, since the controller's host
// classes its refusals as a role's does.
func TestSignalProviderConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Projects["DEMO"].Provider; got != (SignalProvider{}) {
			t.Fatalf("a project that names no provider key reads %+v, want no limit, every model and no pattern", got)
		}
	})
	t.Run("a valid set", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, signalProviderFile(`signal_token_limit_per_minute: 5500000, `+
			`signal_token_model_pattern: '^claude-(opus|sonnet)-', `+
			`signal_own_refusal_pattern: 'project (token|request) limit', `+
			`signal_provider_refusal_pattern: '[Oo]verloaded|Too many requests'`)), noEnv)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := SignalProvider{TokenLimit: 5500000, TokenModels: "^claude-(opus|sonnet)-",
			OwnRefusal: "project (token|request) limit", ProviderRefusal: "[Oo]verloaded|Too many requests"}
		if got := cfg.Projects["DEMO"].Provider; got != want {
			t.Fatalf("the project's provider = %+v, want %+v", got, want)
		}
	})
	t.Run("refused", func(t *testing.T) {
		const own = "projects.DEMO.signal_own_refusal_pattern"
		subset := func(pattern, why string) string {
			return own + " " + quoted(pattern) + " is outside the subset of patterns Go and JavaScript both read alike: " + why
		}
		for _, tc := range []struct {
			name, members, refusal string
		}{
			{"a limit of zero", `signal_token_limit_per_minute: 0`,
				"projects.DEMO.signal_token_limit_per_minute must be a positive integer"},
			{"a limit that is no number", `signal_token_limit_per_minute: lots`,
				"projects.DEMO.signal_token_limit_per_minute must be an integer"},
			{"an empty pattern", `signal_own_refusal_pattern: ''`,
				subset("", "it is empty, and an empty pattern matches every text")},
			{"a group left open", `signal_own_refusal_pattern: '(rate limit'`,
				subset("(rate limit", "a group opened at 0 is never closed")},
			{"a group never opened", `signal_own_refusal_pattern: 'rate) limit'`,
				subset("rate) limit", "the ) at 4 closes no group")},
			{"a lookahead, which Go refuses", `signal_own_refusal_pattern: 'rate(?= limit)'`,
				subset("rate(?= limit)", "(? at 4 opens a lookaround, a named group or a flag; only (?: is in the subset")},
			{"a back reference, which Go refuses", `signal_own_refusal_pattern: '(a)\1'`,
				subset(`(a)\1`, `\1 at 3 is not in the subset`)},
			{"a repetition of nothing", `signal_own_refusal_pattern: '*limit'`,
				subset("*limit", "the * at 0 repeats nothing")},
			{"a repetition of a repetition", `signal_own_refusal_pattern: 'limits*+'`,
				subset("limits*+", "the + at 7 repeats nothing")},
			{"a bound above Go's", `signal_own_refusal_pattern: 'a{1001}'`,
				subset("a{1001}", "the repetition at 1 is outside 0 to 1000, the bound Go's regexp takes")},
			{"an empty class", `signal_own_refusal_pattern: 'a[]b'`,
				subset("a[]b", "the class at 1 opens with ], which Go reads as a member and JavaScript as the class's end")},
			{"the provider's key, refused by the same rule", `signal_provider_refusal_pattern: '(?i)overloaded'`,
				"projects.DEMO.signal_provider_refusal_pattern " + quoted("(?i)overloaded") +
					" is outside the subset of patterns Go and JavaScript both read alike: (? at 0 opens a lookaround, a named group or a flag; only (?: is in the subset"},
			{"the model selector, refused by the same rule", `signal_token_model_pattern: 'claude-\pL'`,
				"projects.DEMO.signal_token_model_pattern " + quoted(`claude-\pL`) +
					` is outside the subset of patterns Go and JavaScript both read alike: \p at 7 is not in the subset`},
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
	// would class one refusal two ways: the loader refuses each, naming what differs.
	t.Run("read differently", func(t *testing.T) {
		for _, tc := range []struct {
			name, pattern, why string
		}{
			{"a space class", `rate\slimit`, `\s at 4 matches the spaces of Unicode in JavaScript and only ASCII's in Go; write the spaces as a class, such as [ \t]`},
			{"a POSIX class", `[[:digit:]]+ tokens`, "the [ at 1 inside a class: Go reads [:name:] as a POSIX class, and JavaScript as members; write \\[ for the character"},
			{"a start of text", `\Arate limit`, `\A at 0 is not in the subset`},
			{"a quoted run", `\Qa.b\E`, `\Q at 0 is not in the subset`},
			{"a brace alone", `limit{`, `the { at 5 begins no repetition; write \{ for the character`},
			{"a bracket alone", `limit]`, `the ] at 5 closes no class; write \] for the character`},
			{"a class of every character", `[^]`, "the class at 0 opens with ], which Go reads as a member and JavaScript as the class's end"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := signalProviderFile(`signal_own_refusal_pattern: ` + yamlSingleQuoted(tc.pattern))
				cfg, err := Load(writeConfigFile(t, body), noEnv)
				want := "projects.DEMO.signal_own_refusal_pattern " + quoted(tc.pattern) +
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
	// A file whose every other provider key is valid is refused whole for the one that is not, so
	// a daemon started on it keeps none of them, the limit and the other patterns included.
	t.Run("a refused file changes nothing", func(t *testing.T) {
		cfg, err := Load(writeConfigFile(t, signalProviderFile(`signal_token_limit_per_minute: 5500000, `+
			`signal_token_model_pattern: '^claude-', signal_provider_refusal_pattern: 'overloaded', `+
			`signal_own_refusal_pattern: 'rate\slimit'`)), noEnv)
		if err == nil || !strings.HasPrefix(err.Error(), "projects.DEMO.signal_own_refusal_pattern ") {
			t.Fatalf("Load error = %v, want the own pattern's refusal", err)
		}
		if !reflect.DeepEqual(cfg, Config{}) {
			t.Fatalf("a refused file loads %+v, want nothing of it, its valid keys included", cfg)
		}
	})
	t.Run("the operator's controller file", func(t *testing.T) {
		cfg, err := LoadController(controllerFile(t, requiredControllerKeys+
			"signal_own_refusal_pattern: 'project (token|request) limit'\nsignal_provider_refusal_pattern: '[Oo]verloaded'\n"), "")
		if err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		if cfg.OwnRefusalPattern != "project (token|request) limit" || cfg.ProviderRefusalPattern != "[Oo]verloaded" {
			t.Fatalf("the controller's patterns = %q and %q, want the file's", cfg.OwnRefusalPattern, cfg.ProviderRefusalPattern)
		}
		unset, err := LoadController(controllerFile(t, requiredControllerKeys), "")
		if err != nil {
			t.Fatalf("LoadController: %v", err)
		}
		if unset.OwnRefusalPattern != "" || unset.ProviderRefusalPattern != "" {
			t.Fatalf("a controller file that names no pattern reads %q and %q, want none", unset.OwnRefusalPattern, unset.ProviderRefusalPattern)
		}
		refused, err := LoadController(controllerFile(t, requiredControllerKeys+"signal_provider_refusal_pattern: 'rate\\slimit'\n"), "")
		want := "signal_provider_refusal_pattern " + quoted(`rate\slimit`) + " is outside the subset of patterns Go and JavaScript both read alike: " +
			`\s at 4 matches the spaces of Unicode in JavaScript and only ASCII's in Go; write the spaces as a class, such as [ \t]`
		if err == nil || err.Error() != want {
			t.Fatalf("LoadController error = %v, want %q", err, want)
		}
		if !reflect.DeepEqual(refused, ControllerConfig{}) {
			t.Fatalf("a refused controller file loads %+v, want nothing", refused)
		}
	})
}

// quoted is pattern as a refusal names it, Go-quoted.
func quoted(pattern string) string { return strconv.Quote(pattern) }

// yamlSingleQuoted is s as a single-quoted YAML scalar: each quote doubled.
func yamlSingleQuoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
