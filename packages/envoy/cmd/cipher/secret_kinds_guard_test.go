package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// kindsGuarded is list, a requesters tag's text ("" for none), with the kinds' rollback guard after
// it and no project guard, as the requesters tag a secret carrying a proxy or a proxy-narrowed tag
// and no project tag holds.
func kindsGuarded(list string) string { return policy.WithGuards(list, false, true) }

// bothGuarded is list with both rollback guards after it, the project tags' first, as the
// requesters tag of a secret carrying a project tag and a proxy tag holds.
func bothGuarded(list string) string { return policy.WithGuards(list, true, true) }

// TestTheTagFlagTableHoldsEveryTagFlag: tagFlagTable is the one list of the tag flags, and each
// field of tagFlags is in it once. For each field, set alone, the table's flag is what the create's
// printed command names (createCommand), its tag is what optionalTags writes, and what a retag's
// none removes (noneRemoves), and the flag is one create and retag register and their synopses
// name. Create and retag register each flag straight into its field of tagFlags, so a field whose
// flag one of them does not register fails the -h check here. The fields are read from tagFlags
// itself, so a field the table leaves out fails here rather than drop out of a printed command or
// a retag's removal.
func TestTheTagFlagTableHoldsEveryTagFlag(t *testing.T) {
	fields := reflect.TypeOf(tagFlags{})
	if len(tagFlagTable) != fields.NumField() {
		t.Fatalf("tagFlagTable holds %d flags and tagFlags %d fields", len(tagFlagTable), fields.NumField())
	}
	help := map[string]string{}
	for _, form := range []string{"create", "retag"} {
		_, stderr, code := runSecret(form, "-h")
		if code != 0 {
			t.Fatalf("secret %s -h: exit %d, stderr %q", form, code, stderr)
		}
		help["secret "+form] = stderr
	}
	for i := range fields.NumField() {
		field := fields.Field(i).Name
		var f tagFlags
		value := fmt.Sprintf("value%02d", i)
		reflect.ValueOf(&f).Elem().Field(i).SetString(value)
		var rows []tagFlag
		for _, row := range tagFlagTable {
			if *row.field(&f) == value {
				rows = append(rows, row)
			}
		}
		if len(rows) != 1 {
			t.Fatalf("tagFlags.%s is the field of %d rows of tagFlagTable; want one", field, len(rows))
		}
		row := rows[0]
		if got, want := createCommand("API_KEY", "me", "agent", f), []string{"secret", "create", "API_KEY", "--owner", "me", "--tier", "agent", row.flag, value}; !slices.Equal(got, want) {
			t.Errorf("tagFlags.%s alone: createCommand = %q, want %q", field, got, want)
		}
		want := map[string]string{row.tag: value}
		if row.tag == policy.TagType {
			want[policy.TagRollbackGuard] = policy.RollbackGuardValue
		}
		if got := optionalTags(f); !maps.Equal(got, want) {
			t.Errorf("tagFlags.%s alone: optionalTags = %v, want %v", field, got, want)
		}
		reflect.ValueOf(&f).Elem().Field(i).SetString(tagNone)
		if removed := noneRemoves(f); len(removed) == 0 || removed[0] != row.tag {
			t.Errorf("tagFlags.%s set to none: noneRemoves = %q, want %s first", field, removed, row.tag)
		}
		if got := optionalTags(f); len(got) != 0 {
			t.Errorf("tagFlags.%s set to none: optionalTags = %v, want nothing set", field, got)
		}
		for form, text := range help {
			if !strings.Contains(lookupCommand(form).synopsis, "["+row.flag+" ") {
				t.Errorf("%s's synopsis names no %s", form, row.flag)
			}
			if !strings.Contains(text, "  -"+strings.TrimPrefix(row.flag, "--")+" string") {
				t.Errorf("%s -h registers no %s", form, row.flag)
			}
		}
	}
	if removed := noneRemoves(tagFlags{Type: tagNone}); !slices.Equal(removed, []string{policy.TagType, policy.LegacyTypeTag, policy.TagRollbackGuard}) {
		t.Errorf("--type none removes %q; want the type, the old kind tag and the type's rollback guard", removed)
	}
}

// TestTheRollbackGuardsAreInSplitGuardsOrder: rollbackGuards is read at the index policy.SplitGuards
// answers each guard at and policy.WithGuards takes it at, so each row's guard is the one that
// index answers, and the table's tags name each tag that brings a guard once.
func TestTheRollbackGuardsAreInSplitGuardsOrder(t *testing.T) {
	for i, g := range rollbackGuards {
		_, project, kinds := policy.SplitGuards(g.guard)
		if got, want := [2]bool{project, kinds}, [2]bool{i == 0, i == 1}; got != want {
			t.Errorf("rollbackGuards[%d] is %s, which SplitGuards answers as %v; want %v", i, g.guard, got, want)
		}
		if got := policy.WithGuards("", i == 0, i == 1); got != g.guard {
			t.Errorf("WithGuards for index %d writes %q; want rollbackGuards[%d], %s", i, got, i, g.guard)
		}
	}
	if got, want := guardTags(), "dispatch-project, project-channels, proxy or proxy-narrowed"; got != want {
		t.Errorf("guardTags = %q, want %q", got, want)
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
		both := bothGuarded
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
	both.Tags[policy.TagRequesters] = bothGuarded("ada@example.com")
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
