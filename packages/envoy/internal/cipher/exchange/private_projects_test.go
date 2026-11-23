package exchange_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// TestAPrivateProjectOnlyInItsPodsParsesTheSetting pins the reading of BROKER_PRIVATE_PROJECTS:
// whitespace-separated project keys, answered sorted; a key that is no project key, and a key named
// twice, refused naming the key, so a list that is not one is never read as some part of it; an
// empty or blank setting read as no private project; and Keys answering a copy, so a caller that
// changes it leaves the set as it was parsed.
func TestAPrivateProjectOnlyInItsPodsParsesTheSetting(t *testing.T) {
	t.Run("a list of keys", func(t *testing.T) {
		private, err := exchange.ParsePrivateProjects(" PRIV\n\tACME  CORE2 ")
		if err != nil {
			t.Fatalf("ParsePrivateProjects of three keys: %v", err)
		}
		if got, want := private.Keys(), []string{"ACME", "CORE2", "PRIV"}; !slices.Equal(got, want) {
			t.Fatalf("Keys() = %q, want %q: each key once, sorted", got, want)
		}
		for _, key := range []string{"ACME", "CORE2", "PRIV"} {
			if !private.Has(key) {
				t.Errorf("Has(%q) = false, want true: the setting names it", key)
			}
		}
		for _, key := range []string{"OTHER", "priv", "PRI", ""} {
			if private.Has(key) {
				t.Errorf("Has(%q) = true, want false: the setting names no such key", key)
			}
		}
	})
	t.Run("a key that is no project key", func(t *testing.T) {
		for _, bad := range []string{"priv", "Acme", "A", "TOOLONGPROJ", "AC-ME", "_", "PRIV,ACME"} {
			private, err := exchange.ParsePrivateProjects("ACME " + bad + " CORE")
			if err == nil {
				t.Errorf("ParsePrivateProjects naming %q = %q, want a refusal: it is no project key", bad, private.Keys())
				continue
			}
			if !strings.Contains(err.Error(), `"`+bad+`"`) {
				t.Errorf("ParsePrivateProjects refused %q with %q, which does not name the key", bad, err)
			}
		}
	})
	t.Run("a key named twice", func(t *testing.T) {
		// ACME, and not PRIV: the config test wraps this error in BROKER_PRIVATE_PROJECTS, which
		// holds PRIV, so only a key no setting name holds proves the refusal names the key.
		private, err := exchange.ParsePrivateProjects("ACME PRIV CORE ACME")
		if err == nil {
			t.Fatalf("ParsePrivateProjects naming ACME twice = %q, want a refusal: each private project is named once", private.Keys())
		}
		if !strings.Contains(err.Error(), "ACME") || strings.Contains(err.Error(), "PRIV") || strings.Contains(err.Error(), "CORE") {
			t.Errorf("ParsePrivateProjects refused ACME named twice with %q, want it to name ACME and no other key", err)
		}
	})
	t.Run("an empty or blank setting", func(t *testing.T) {
		for _, setting := range []string{"", " ", "\n\t "} {
			private, err := exchange.ParsePrivateProjects(setting)
			if err != nil {
				t.Errorf("ParsePrivateProjects(%q): %v, want no private project and no refusal", setting, err)
				continue
			}
			if keys := private.Keys(); keys != nil {
				t.Errorf("ParsePrivateProjects(%q).Keys() = %q, want nil: no project is private", setting, keys)
			}
			if private.Has("PRIV") {
				t.Errorf("ParsePrivateProjects(%q).Has(PRIV) = true, want false", setting)
			}
		}
		var unset exchange.PrivateProjects
		if keys := unset.Keys(); keys != nil || unset.Has("PRIV") {
			t.Errorf("the zero PrivateProjects answers Keys() = %q and Has(PRIV) = %v, want nil and false: a Config with no setting has no private project", keys, unset.Has("PRIV"))
		}
	})
	t.Run("Keys answers the caller's own copy", func(t *testing.T) {
		private, err := exchange.ParsePrivateProjects("ACME PRIV")
		if err != nil {
			t.Fatal(err)
		}
		keys := private.Keys()
		if len(keys) != 2 {
			t.Fatalf("Keys() = %q, want [ACME PRIV]", keys)
		}
		keys[0], keys[1] = "CHANGED", "ZZZ"
		if again := private.Keys(); !slices.Equal(again, []string{"ACME", "PRIV"}) {
			t.Fatalf("Keys() after the caller changed its copy = %q, want [ACME PRIV]: the set stays as it was parsed", again)
		}
		if private.Has("CHANGED") || !private.Has("ACME") || !private.Has("PRIV") {
			t.Fatalf("Has after the caller changed its copy of Keys: CHANGED %v, ACME %v, PRIV %v, want false, true, true", private.Has("CHANGED"), private.Has("ACME"), private.Has("PRIV"))
		}
	})
}
