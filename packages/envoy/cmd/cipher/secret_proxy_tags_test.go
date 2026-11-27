package main

import (
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
)

// TestCreateAndRetagWriteTheProxyTags: create --proxy KIND and --proxy-narrowed true write the
// secret's proxy and proxy-narrowed tags beside its owner and tier and the kinds' rollback guard,
// which a requesters tag of the guard alone carries, and nothing else, on each store. Retag writes
// each beside every tag the secret already holds, a type, its GitHub owners and rollback guard,
// requesters, an admin mark, a max grant, a default grant, a judge and a guidance among them, and
// changes none of those but the requesters tag, which holds the kinds' guard after its list while
// either proxy tag remains; --proxy none and --proxy-narrowed none each remove their own tag and
// leave the rest, the last of them the guard too, and a removal beside another change is refused
// before anything is written, since it is a retag of its own.
func TestCreateAndRetagWriteTheProxyTags(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		useStdin(t, "probe-value")
		if _, stderr, code := runSecret("create", "PROXIED_KEY", "--owner", "me", "--tier", "agent", "--proxy", "github"); code != 0 {
			t.Fatalf("create --proxy github: exit %d, stderr %q", code, stderr)
		}
		want := map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent, policy.TagProxy: "github", policy.TagRequesters: policy.KindsRollbackGuard}
		if got := store.tags(t, "PROXIED_KEY"); !maps.Equal(got, want) {
			t.Fatalf("PROXIED_KEY's tags = %v, want exactly %v", got, want)
		}

		useStdin(t, "probe-value")
		if _, stderr, code := runSecret("create", "NARROWED_KEY", "--owner", "me", "--tier", "human", "--proxy", "anthropic", "--proxy-narrowed", "true"); code != 0 {
			t.Fatalf("create --proxy anthropic --proxy-narrowed true: exit %d, stderr %q", code, stderr)
		}
		want = map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagProxy: "anthropic", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue, policy.TagRequesters: policy.KindsRollbackGuard}
		if got := store.tags(t, "NARROWED_KEY"); !maps.Equal(got, want) {
			t.Fatalf("NARROWED_KEY's tags = %v, want exactly %v", got, want)
		}

		// API_KEY holds a tag of nearly every kind a retag can write, and a set the broker loads.
		held := policytest.TypeSecret("API_KEY", "ada@example.com", policy.TierHuman, policy.TypeGitHubApp, "").Tags
		held[policy.TagRollbackGuard] = policy.RollbackGuardValue
		held[policy.TagRequesters] = "ada@example.com"
		held[policy.TagAdmin] = policy.AdminTrue
		held[policy.TagMaxGrant] = "1h"
		held[policy.TagDefaultGrant] = policy.DefaultGrantSession
		held[policy.TagJudge] = "600"
		held[policy.TagGuidance] = "grant for the release job"
		store.put(t, "API_KEY", held, policytest.TypeValue(policy.TypeGitHubApp))
		// with is the tags held, with tags written over them and the kinds' guard after the
		// requesters list, as a secret behind the proxy holds it.
		with := func(tags map[string]string) map[string]string {
			out := maps.Clone(held)
			maps.Copy(out, tags)
			out[policy.TagRequesters] = policy.WithGuards(held[policy.TagRequesters], false, true)
			return out
		}
		for _, step := range []struct {
			args []string
			want map[string]string
		}{
			{[]string{"--proxy", "github"}, with(map[string]string{policy.TagProxy: "github"})},
			{[]string{"--proxy", "anthropic"}, with(map[string]string{policy.TagProxy: "anthropic"})},
			{[]string{"--proxy-narrowed", "true"}, with(map[string]string{policy.TagProxy: "anthropic", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue})},
			{[]string{"--proxy-narrowed", "none"}, with(map[string]string{policy.TagProxy: "anthropic"})},
			{[]string{"--proxy-narrowed", "true"}, with(map[string]string{policy.TagProxy: "anthropic", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue})},
			{[]string{"--proxy", "none"}, with(map[string]string{policy.TagProxyNarrowed: policy.ProxyNarrowedTrue})},
			{[]string{"--proxy", "github"}, with(map[string]string{policy.TagProxy: "github", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue})},
		} {
			args := append([]string{"retag", "API_KEY"}, step.args...)
			if _, stderr, code := runSecret(args...); code != 0 {
				t.Fatalf("%q: exit %d, stderr %q", args, code, stderr)
			}
			if got := store.tags(t, "API_KEY"); !maps.Equal(got, step.want) {
				t.Fatalf("tags after %q = %v, want exactly %v", args, got, step.want)
			}
		}

		before := store.tags(t, "API_KEY")
		for _, args := range [][]string{
			{"retag", "API_KEY", "--proxy", "none", "--tier", "agent"},
			{"retag", "API_KEY", "--proxy-narrowed", "none", "--judge", "300"},
			{"retag", "API_KEY", "--proxy", "none", "--proxy-narrowed", "true"},
			{"retag", "API_KEY", "--proxy-narrowed", "none", "--proxy", "anthropic"},
		} {
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "a retag of its own") {
				t.Fatalf("%q: exit %d stderr %q; want a usage error saying a removal is a retag of its own", args, code, stderr)
			}
			if got := store.tags(t, "API_KEY"); !maps.Equal(got, before) {
				t.Fatalf("a refused %q changed the tags to %v, from %v", args, got, before)
			}
		}

		for _, args := range [][]string{{"retag", "API_KEY", "--proxy-narrowed", "none"}, {"retag", "API_KEY", "--proxy", "none"}} {
			if _, stderr, code := runSecret(args...); code != 0 {
				t.Fatalf("%q: exit %d, stderr %q", args, code, stderr)
			}
		}
		if got := store.tags(t, "API_KEY"); !maps.Equal(got, held) {
			t.Fatalf("tags after --proxy-narrowed none and --proxy none = %v, want exactly the tags held before, %v", got, held)
		}
	})
}

// TestTheProxyFlagsRefuseEveryOtherForm: --proxy is one lowercase word, the form every kind's
// name has, so an upper-case letter, a digit, a hyphen, an underscore and a space are each refused
// as that flag, and so is none on create, which removes the tag only in a retag. --proxy-narrowed
// takes true alone, and none only on retag, so yes, TRUE and 1 are refused on both forms; an empty
// --proxy-narrowed= alone on retag names no change, and is refused as a retag naming none. Each is
// a usage error before the sign-in is read, the store is called or the broker is asked anything,
// and none of them prints the value it refused. The usage line of a retag that names no flag names
// both flags.
func TestTheProxyFlagsRefuseEveryOtherForm(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, broker *secretBroker) {
		store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, "v1")
		before := store.calls()
		create := func(flags ...string) []string {
			return append([]string{"create", "NEW_KEY", "--owner", "me", "--tier", "agent"}, flags...)
		}
		const (
			proxyForm      = "--proxy is the name of a kind of the broker's profiles file"
			narrowedCreate = "--proxy-narrowed is " + policy.ProxyNarrowedTrue + " " + valueNotPrinted
			narrowedRetag  = "--proxy-narrowed is " + policy.ProxyNarrowedTrue + " or " + tagNone + " " + valueNotPrinted
		)
		for _, c := range []struct {
			args []string
			want string
			// value is the refused value, which the refusal must not print, or "" where the
			// refusal is of no value of the user's.
			value string
		}{
			{create("--proxy", "GitHub"), proxyForm, "GitHub"},
			{create("--proxy", "github2"), proxyForm, "github2"},
			{create("--proxy", "git-hub"), proxyForm, "git-hub"},
			{create("--proxy", "git_hub"), proxyForm, "git_hub"},
			{create("--proxy", "git hub"), proxyForm, "git hub"},
			{create("--proxy", "none"), "--proxy none removes the proxy tag, in a retag", ""},
			{create("--proxy-narrowed", "none"), narrowedCreate, ""},
			{create("--proxy-narrowed", "yes"), narrowedCreate, "yes"},
			{create("--proxy-narrowed", "TRUE"), narrowedCreate, "TRUE"},
			{create("--proxy-narrowed", "1"), narrowedCreate, ""},
			{[]string{"retag", "API_KEY", "--proxy", "GitHub"}, proxyForm, "GitHub"},
			{[]string{"retag", "API_KEY", "--proxy", "github2"}, proxyForm, "github2"},
			{[]string{"retag", "API_KEY", "--proxy", "git-hub"}, proxyForm, "git-hub"},
			{[]string{"retag", "API_KEY", "--proxy", "git_hub"}, proxyForm, "git_hub"},
			{[]string{"retag", "API_KEY", "--proxy", "git hub"}, proxyForm, "git hub"},
			{[]string{"retag", "API_KEY", "--proxy-narrowed", "yes"}, narrowedRetag, "yes"},
			{[]string{"retag", "API_KEY", "--proxy-narrowed", "TRUE"}, narrowedRetag, "TRUE"},
			{[]string{"retag", "API_KEY", "--proxy-narrowed", "1"}, narrowedRetag, ""},
			{[]string{"retag", "API_KEY", "--proxy-narrowed="}, "--proxy, --proxy-narrowed or a combination is required", ""},
			{[]string{"retag", "API_KEY"}, "--proxy, --proxy-narrowed or a combination is required", ""},
		} {
			useStdin(t, "probe-value")
			_, stderr, code := runSecret(c.args...)
			if code != exitUsageError || !strings.Contains(stderr, c.want) {
				t.Errorf("%q: exit %d stderr %q; want %d and %q", c.args, code, stderr, exitUsageError, c.want)
			}
			if c.value != "" && strings.Contains(stderr, strconv.Quote(c.value)) {
				t.Errorf("%q: stderr %q prints the value it refused", c.args, stderr)
			}
		}
		if calls := store.calls() - before; calls != 0 {
			t.Fatalf("refused commands made %d store calls; want none", calls)
		}
		if rereads, _ := broker.recorded(); len(rereads) != 0 {
			t.Fatalf("refused commands asked the broker to reread %q; want nothing asked", rereads)
		}
		if store.has(t, "NEW_KEY") {
			t.Fatal("a refused create made NEW_KEY")
		}
		if got, want := store.tags(t, "API_KEY"), map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}; !maps.Equal(got, want) {
			t.Fatalf("refused retags changed API_KEY's tags to %v", got)
		}
	})
}

// TestSecretShowAndJSONPrintTheProxyTags: show prints a proxy line holding the kind the proxy tag
// names and a proxy_narrowed line holding the proxy-narrowed tag, each "-" for a secret without
// it, on each store; show --json and list --json carry the two as proxy and proxy_narrowed, "" for
// a secret without them, so a reader of either sees which secrets the broker serves behind its
// proxy.
func TestSecretShowAndJSONPrintTheProxyTags(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "PROXIED_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagProxy: "anthropic", policy.TagProxyNarrowed: policy.ProxyNarrowedTrue}, "v1")
		store.put(t, "PLAIN_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, "v1")
		wantFields := map[string][2]string{"PROXIED_KEY": {"anthropic", policy.ProxyNarrowedTrue}, "PLAIN_KEY": {"", ""}}

		for name, printed := range map[string]string{
			"PROXIED_KEY": "\nproxy: anthropic\nproxy_narrowed: true\n",
			"PLAIN_KEY":   "\nproxy: -\nproxy_narrowed: -\n",
		} {
			stdout, stderr, code := runSecret("show", name)
			if code != 0 || !strings.Contains(stdout, printed) {
				t.Fatalf("show %s = exit %d stdout %q stderr %q; want it to print %q", name, code, stdout, stderr, printed)
			}
			jsonOut, stderr, code := runSecret("show", name, "--json")
			if code != 0 {
				t.Fatalf("show %s --json: exit %d, stderr %q", name, code, stderr)
			}
			var view map[string]any
			if err := json.Unmarshal([]byte(jsonOut), &view); err != nil {
				t.Fatalf("show %s --json is not a secret: %v: %s", name, err, jsonOut)
			}
			checkProxyFields(t, "show "+name+" --json", view, wantFields[name])
		}

		stdout, stderr, code := runSecret("list", "--json")
		if code != 0 {
			t.Fatalf("list --json: exit %d, stderr %q", code, stderr)
		}
		var views []map[string]any
		if err := json.Unmarshal([]byte(stdout), &views); err != nil {
			t.Fatalf("list --json is not a JSON array of secrets: %v: %s", err, stdout)
		}
		if len(views) != len(wantFields) {
			t.Fatalf("list --json = %s, want %d rows", stdout, len(wantFields))
		}
		for _, v := range views {
			name, _ := v["name"].(string)
			want, known := wantFields[name]
			if !known {
				t.Fatalf("list --json row %v names no secret this test stored", v)
			}
			checkProxyFields(t, "list --json row "+name, v, want)
		}
	})
}

// checkProxyFields fails t unless view carries the proxy and proxy_narrowed fields, each the
// string want names, "" included: a missing field and a null are not the empty string.
func checkProxyFields(t *testing.T, what string, view map[string]any, want [2]string) {
	t.Helper()
	for i, field := range []string{"proxy", "proxy_narrowed"} {
		got, present := view[field]
		if !present {
			t.Fatalf("%s carries no %s field: %v", what, field, view)
		}
		if got != want[i] {
			t.Fatalf("%s %s = %#v, want %q", what, field, got, want[i])
		}
	}
}

// TestTheCreateAndRetagSynopsesNameTheProxyFlags: the usage line of create names --proxy KIND and
// --proxy-narrowed true, and that of retag names each with none, which removes its tag there, so
// `agent-secrets --help` and each form's -h show how to put a secret behind the broker's proxy.
func TestTheCreateAndRetagSynopsesNameTheProxyFlags(t *testing.T) {
	for name, wants := range map[string][]string{
		"secret create": {"[--proxy KIND]", "[--proxy-narrowed true]"},
		"secret retag":  {"[--proxy KIND|none]", "[--proxy-narrowed true|none]"},
	} {
		synopsis := lookupCommand(name).synopsis
		for _, want := range wants {
			if !strings.Contains(synopsis, want) {
				t.Errorf("%s's synopsis %q names no %s", name, synopsis, want)
			}
		}
	}
}
