package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// kindsGuarded is list, a requesters tag's text ("" for none), with the kinds' rollback guard after
// it, as the requesters tag a secret carrying a proxy or a proxy-narrowed tag holds.
func kindsGuarded(list string) string { return policy.WithKindsGuard(list) }

// TestTheCreateCommandToPipeAValueNamesEveryFlag: the command line a create prints for piping its
// value in names every tag flag the create named, --proxy and --proxy-narrowed among them, with its
// value, so the printed command creates the secret the first one meant: one without --proxy would
// create a secret the broker releases as its value.
func TestTheCreateCommandToPipeAValueNamesEveryFlag(t *testing.T) {
	got := createCommand("API_KEY", "me", "human", tagFlags{
		Type: policy.TypeGoogleServiceAccount, GitHubOwners: "acme", Requesters: "ada@example.com", Scopes: "cloud-platform",
		Admin: policy.AdminTrue, Project: "PRIV", ProjectChannels: policy.ProjectChannelsSlack, MaxGrant: "1h",
		DefaultGrant: policy.DefaultGrantSession, Judge: "300", Guidance: "grant for the release job",
		Proxy: "gcp", ProxyNarrowed: policy.ProxyNarrowedTrue,
	})
	want := []string{"secret", "create", "API_KEY", "--owner", "me", "--tier", "human",
		"--type", policy.TypeGoogleServiceAccount, "--github-owners", "acme", "--requesters", "ada@example.com", "--scopes", "cloud-platform",
		"--admin", policy.AdminTrue, "--project", "PRIV", "--project-channels", policy.ProjectChannelsSlack, "--max-grant", "1h",
		"--default-grant", policy.DefaultGrantSession, "--judge", "300", "--guidance", "grant for the release job",
		"--proxy", "gcp", "--proxy-narrowed", policy.ProxyNarrowedTrue}
	if !slices.Equal(got, want) {
		t.Fatalf("createCommand = %q\nwant %q", got, want)
	}
	if bare := createCommand("API_KEY", "me", "agent", tagFlags{}); !slices.Equal(bare, []string{"secret", "create", "API_KEY", "--owner", "me", "--tier", "agent"}) {
		t.Fatalf("createCommand with no tag flag = %q; want the name, owner and tier alone", bare)
	}
}

// TestCreateWritesTheKindsGuardBesideAProxyTag: a create naming --proxy or --proxy-narrowed writes
// the kinds' rollback guard into the secret's requesters tag, after the list it names and after the
// project tags' guard where a project tag brings that one too, so a broker from before the kinds
// refuses the secret rather than release it as its value. A create naming neither writes no kinds
// guard. The cross-tag rules weigh the list without it: a google-service-account secret at agent
// tier names its one owner, and the guard, which has no @, would otherwise read as a service listed
// below human tier.
func TestCreateWritesTheKindsGuardBesideAProxyTag(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		for _, c := range []struct {
			name  string
			args  []string
			value string
			want  string
		}{
			{"PROXIED_KEY", []string{"--tier", "agent", "--proxy", "github"}, "v", kindsGuarded("")},
			{"NARROWED_KEY", []string{"--tier", "human", "--proxy-narrowed", "true"}, "v", kindsGuarded("")},
			{"LISTED_KEY", []string{"--tier", "human", "--proxy", "github", "--requesters", "ada@example.com"}, "v", kindsGuarded("ada@example.com")},
			{"BOTH_KEY", []string{"--tier", "agent", "--project", "PRIV", "--proxy", "slack"}, "v", policy.RequestersRollbackGuard + " " + policy.KindsRollbackGuard},
			{"ACCOUNT_KEY", []string{"--tier", "agent", "--proxy", "gcp", "--type", policy.TypeGoogleServiceAccount, "--requesters", "ada@example.com", "--max-grant", "1h"}, policytest.TypeValue(policy.TypeGoogleServiceAccount), kindsGuarded("ada@example.com")},
			{"PLAIN_LISTED", []string{"--tier", "human", "--requesters", "ada@example.com"}, "v", "ada@example.com"},
		} {
			useStdin(t, c.value)
			args := append([]string{"create", c.name, "--owner", "me"}, c.args...)
			if _, stderr, code := runSecret(args...); code != 0 {
				t.Fatalf("%v: exit %d, stderr %q", args, code, stderr)
			}
			if got := store.tags(t, c.name)[policy.TagRequesters]; got != c.want {
				t.Errorf("%v wrote requesters %q, want %q", args, got, c.want)
			}
		}
	})
}

// TestRetagKeepsTheKindsGuardWhileAProxyTagRemains: retag adds the kinds' guard to the requesters
// tag of a secret it leaves with a proxy or a proxy-narrowed tag, after the list the secret holds
// or the command names and after the project tags' guard, and never in place of either; keeps it
// while either proxy tag remains, whatever happens to the project guard; and takes it out with the
// last of them. --requesters none on a secret that keeps a proxy tag removes the list and keeps the
// guard, and beside another removal it is refused before any write, as for the project tags.
func TestRetagKeepsTheKindsGuardWhileAProxyTagRemains(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagRequesters: "ada@example.com", policy.TagAdmin: policy.AdminTrue, policy.TagMaxGrant: "1h"}, "v1")
		step := func(want string, args ...string) {
			t.Helper()
			if _, stderr, code := runSecret(append([]string{"retag", "API_KEY"}, args...)...); code != 0 {
				t.Fatalf("retag %v: exit %d, stderr %q", args, code, stderr)
			}
			got, present := store.tags(t, "API_KEY")[policy.TagRequesters]
			switch {
			case want == "" && present:
				t.Fatalf("after retag %v the requesters tag is %q, want none", args, got)
			case want != "" && got != want:
				t.Fatalf("after retag %v the requesters tag is %q, want %q", args, got, want)
			}
		}
		both := func(list string) string { return kindsGuarded(guarded(list)) }
		step(kindsGuarded("ada@example.com"), "--proxy", "github")
		step(kindsGuarded("ada@example.com"), "--proxy-narrowed", "true")
		step(both("ada@example.com"), "--project", "PRIV")
		step(both("ada@example.com bob@example.com"), "--requesters", "bob@example.com ada@example.com")
		_, stderr, code := runSecret("retag", "API_KEY", "--requesters", "none", "--admin", "none")
		if code != exitUsageError || !strings.Contains(stderr, policy.KindsRollbackGuard) {
			t.Fatalf("retag --requesters none --admin none on a proxied secret: exit %d stderr %q; want a usage error naming the guard", code, stderr)
		}
		if got := store.tags(t, "API_KEY"); got[policy.TagRequesters] != both("ada@example.com bob@example.com") || got[policy.TagAdmin] != policy.AdminTrue {
			t.Fatalf("a refused retag changed the tags to %v", got)
		}
		step(both("ada@example.com bob@example.com"), "--admin", "none")
		step(both(""), "--requesters", "none")
		step(kindsGuarded(""), "--project", "none")
		step(kindsGuarded(""), "--proxy", "none")
		step("", "--proxy-narrowed", "none")
		if tags := store.tags(t, "API_KEY"); tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierHuman || tags[policy.TagMaxGrant] != "1h" {
			t.Fatalf("tags at the end = %v, want owner, tier and max-grant kept", tags)
		}
	})
}

// TestRetagTakesTheKindsGuardOutOfAListWithTheLastProxyTag: removing the last proxy tag from a
// secret whose requesters tag holds a list beside the kinds' guard leaves the list alone. On AWS
// that is two writes, the removal and then the list's rewrite, in that order, so a refused removal
// leaves the proxy tag and the guard as they were.
func TestRetagTakesTheKindsGuardOutOfAListWithTheLastProxyTag(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "LISTED_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagProxy: "github", policy.TagRequesters: kindsGuarded("ada@example.com")}, "v1")
		if stdout, stderr, code := runSecret("retag", "LISTED_KEY", "--proxy", "none"); code != 0 {
			t.Fatalf("retag --proxy none: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		tags := store.tags(t, "LISTED_KEY")
		if _, ok := tags[policy.TagProxy]; ok || tags[policy.TagRequesters] != "ada@example.com" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierHuman {
			t.Fatalf("tags after --proxy none = %v, want no proxy and the requesters list without the guard", tags)
		}
	})
	t.Run("aws write order", func(t *testing.T) {
		listed := policytest.Secret("LISTED_KEY", "ada@example.com", policy.TierHuman, "v1")
		listed.Tags[policy.TagProxy] = "github"
		listed.Tags[policy.TagRequesters] = kindsGuarded("ada@example.com")
		local := secrets.NewLocal(listed)
		startSecretBroker(t, servedBy(local))
		writes := &countingTagWrites{Local: local}
		useAWS(t, writes, testAccount, adaSignIn)
		if _, stderr, code := runSecret("retag", "LISTED_KEY", "--proxy", "none"); code != 0 {
			t.Fatalf("retag --proxy none: exit %d, stderr %q", code, stderr)
		}
		if writes.untags != 1 || writes.tags != 1 {
			t.Fatalf("retag --proxy none made %d UntagResource and %d TagResource calls, want 1 and 1", writes.untags, writes.tags)
		}
		writes.refuseUntag = true
		listed.Tags[policy.TagProxy] = "github"
		listed.Tags[policy.TagRequesters] = kindsGuarded("ada@example.com")
		local.Put(listed)
		if _, _, code := runSecret("retag", "LISTED_KEY", "--proxy", "none"); code != 1 {
			t.Fatalf("retag --proxy none with UntagResource refused: exit %d, want 1", code)
		}
		if tags := secrets.TagMap(describe(t, local, "LISTED_KEY").Tags); tags[policy.TagProxy] != "github" || tags[policy.TagRequesters] != kindsGuarded("ada@example.com") {
			t.Fatalf("a refused removal left tags %v, want the proxy tag and the guard as they were", tags)
		}
	})
}

// TestTheKindsGuardIsNoRequesterACommandCanName: --requesters naming the kinds' guard is refused, as
// any entry that is neither an email nor a service name is, so the guard reaches a requesters tag
// only beside a proxy tag, where this CLI writes it itself.
func TestTheKindsGuardIsNoRequesterACommandCanName(t *testing.T) {
	if _, err := checkRequesters(policy.KindsRollbackGuard, true); err == nil {
		t.Fatalf("checkRequesters(%q) passed; want it refused", policy.KindsRollbackGuard)
	}
}

// TestTheKindsGuardHoldsToAWSsTagLimit: the kinds' guard is written into the requesters tag, and an
// AWS tag value holds 256 characters at most, so a list the guard would carry past 256 is refused
// on AWS before anything is written, naming the requesters tag and the store's limit, and written
// on GCP, whose annotations take it. The same list alone fits on both.
func TestTheKindsGuardHoldsToAWSsTagLimit(t *testing.T) {
	people := make([]string, 12)
	for i := range people {
		people[i] = fmt.Sprintf("person%02d@example.com", i)
	}
	list := strings.Join(people, " ")
	if len(list) > 256 || len(kindsGuarded(list)) <= 256 {
		t.Fatalf("the list is %d characters and %d with the guard; want at most 256 alone and more with it", len(list), len(kindsGuarded(list)))
	}
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		aws := strings.HasSuffix(t.Name(), "/aws")
		store.put(t, "LONG_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagRequesters: list}, "v1")
		_, stderr, code := runSecret("retag", "LONG_KEY", "--proxy", "github")
		tags := store.tags(t, "LONG_KEY")
		if aws {
			want := fmt.Sprintf("the %s tag is %d characters, and AWS Secrets Manager holds at most 256", policy.TagRequesters, len(kindsGuarded(list)))
			if code != exitUsageError || !strings.Contains(stderr, want) {
				t.Fatalf("AWS, --proxy beside a %d-character list: exit %d stderr %q; want %d saying %q", len(list), code, stderr, exitUsageError, want)
			}
			if _, written := tags[policy.TagProxy]; written || tags[policy.TagRequesters] != list {
				t.Fatalf("AWS: the store holds %v after the refusal; want nothing written", tags)
			}
			return
		}
		if code != 0 || tags[policy.TagProxy] != "github" || tags[policy.TagRequesters] != kindsGuarded(list) {
			t.Fatalf("GCP, --proxy beside a %d-character list: exit %d stderr %q, tags %v; want the proxy tag and the guard written", len(list), code, stderr, tags)
		}
	})
}

// TestShowPrintsTheKindsGuardApart: show, list --json and show --json print the requesters list
// without either guard, say whether the secret carries the kinds' guard on a kinds_rollback_guard
// line and field of its own, beside the project tags' rollback_guard, so a census of the store
// finds every secret behind the proxy that carries no guard: a proxy tag written without this CLI,
// which a broker from before the kinds would release as its value.
func TestShowPrintsTheKindsGuardApart(t *testing.T) {
	both := policytest.Secret("BOTH_KEY", "ada@example.com", policy.TierHuman, "v1")
	both.Tags[policy.TagProject] = "PRIV"
	both.Tags[policy.TagProxy] = "github"
	both.Tags[policy.TagRequesters] = kindsGuarded(guarded("ada@example.com"))
	proxied := policytest.Secret("PROXIED_KEY", "ada@example.com", policy.TierHuman, "v1")
	proxied.Tags[policy.TagProxyNarrowed] = policy.ProxyNarrowedTrue
	proxied.Tags[policy.TagRequesters] = kindsGuarded("")
	bare := policytest.Secret("BARE_KEY", "ada@example.com", policy.TierAgent, "v1")
	bare.Tags[policy.TagProxy] = "github"
	local := secrets.NewLocal(both, proxied, bare)
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	for _, c := range []struct {
		name, requesters, project, kinds string
		projectGuard, kindsGuard         bool
	}{
		{"BOTH_KEY", "ada@example.com", "yes", "yes", true, true},
		{"PROXIED_KEY", "-", "-", "yes", false, true},
		{"BARE_KEY", "-", "-", "-", false, false},
	} {
		stdout, stderr, code := runSecret("show", c.name)
		if code != 0 {
			t.Fatalf("show %s: exit %d, stderr %q", c.name, code, stderr)
		}
		for _, want := range []string{"\nrequesters: " + c.requesters + "\n", "\nrollback_guard: " + c.project + "\n", "\nkinds_rollback_guard: " + c.kinds + "\n"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("show %s stdout %q must contain %q", c.name, stdout, want)
			}
		}
		jsonOut, stderr, code := runSecret("show", c.name, "--json")
		if code != 0 {
			t.Fatalf("show %s --json: exit %d, stderr %q", c.name, code, stderr)
		}
		var view struct {
			Requesters         string `json:"requesters"`
			RollbackGuard      *bool  `json:"rollback_guard"`
			KindsRollbackGuard *bool  `json:"kinds_rollback_guard"`
		}
		if err := json.Unmarshal([]byte(jsonOut), &view); err != nil {
			t.Fatalf("show %s --json: %v: %s", c.name, err, jsonOut)
		}
		if want := strings.TrimPrefix(c.requesters, "-"); view.Requesters != want || view.RollbackGuard == nil || *view.RollbackGuard != c.projectGuard ||
			view.KindsRollbackGuard == nil || *view.KindsRollbackGuard != c.kindsGuard {
			t.Errorf("show %s --json = %s; want requesters %q, rollback_guard %v and kinds_rollback_guard %v", c.name, jsonOut, want, c.projectGuard, c.kindsGuard)
		}
	}
	stdout, stderr, code := runSecret("list", "--json")
	if code != 0 {
		t.Fatalf("list --json: exit %d, stderr %q", code, stderr)
	}
	var views []struct {
		Name               string `json:"name"`
		Proxy              string `json:"proxy"`
		ProxyNarrowed      string `json:"proxy_narrowed"`
		KindsRollbackGuard bool   `json:"kinds_rollback_guard"`
	}
	if err := json.Unmarshal([]byte(stdout), &views); err != nil {
		t.Fatalf("list --json: %v: %s", err, stdout)
	}
	var unguarded []string
	for _, v := range views {
		if (v.Proxy != "" || v.ProxyNarrowed != "") && !v.KindsRollbackGuard {
			unguarded = append(unguarded, v.Name)
		}
	}
	if len(views) != 3 || len(unguarded) != 1 || unguarded[0] != "BARE_KEY" {
		t.Fatalf("list --json = %s; want three rows and BARE_KEY alone behind the proxy without the guard", stdout)
	}
}
