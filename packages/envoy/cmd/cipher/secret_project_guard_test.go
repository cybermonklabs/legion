package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// guarded is list, a requesters list ("" for none), with the project tags' rollback guard after it,
// as the requesters tag a secret carrying a project tag holds.
func guarded(list string) string {
	if list == "" {
		return policy.RequestersRollbackGuard
	}
	return list + " " + policy.RequestersRollbackGuard
}

// TestCreateWritesTheRollbackGuardBesideAProjectTag: a create naming --project or
// --project-channels writes the rollback guard into the secret's requesters tag, after the list it
// names, so a broker from before the project tags refuses the secret rather than serve it by
// owner and tier. A create naming neither writes no guard. The cross-tag rules weigh the list
// without the guard: a google-service-account secret at agent tier names its one owner, and the
// guard, which has no @, would otherwise read as a service listed below human tier.
func TestCreateWritesTheRollbackGuardBesideAProjectTag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStdin, secretStdin set at secret_test.go:385).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		for _, c := range []struct {
			name  string
			args  []string
			value string
			want  string
		}{
			{"PRIV_KEY", []string{"--tier", "agent", "--project", "PRIV"}, "v", guarded("")},
			{"SLACK_TOKEN", []string{"--tier", "agent", "--project-channels", "slack"}, "v", guarded("")},
			{"LISTED_KEY", []string{"--tier", "human", "--project", "PRIV", "--requesters", "ada@example.com"}, "v", guarded("ada@example.com")},
			{"ACCOUNT_KEY", []string{"--tier", "agent", "--project", "PRIV", "--type", policy.TypeGoogleServiceAccount, "--requesters", "ada@example.com", "--max-grant", "1h"}, policytest.TypeValue(policy.TypeGoogleServiceAccount), guarded("ada@example.com")},
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
		useStdin(t, "v")
		if _, stderr, code := runSecret("create", "PLAIN_KEY", "--owner", "me", "--tier", "agent"); code != 0 {
			t.Fatalf("create with no project tag: exit %d, stderr %q", code, stderr)
		}
		if tags := store.tags(t, "PLAIN_KEY"); len(tags) != 2 {
			t.Errorf("PLAIN_KEY's tags = %v, want its owner and tier alone", tags)
		}
	})
}

// TestRetagKeepsTheRollbackGuardWhileAProjectTagRemains: retag adds the guard to the requesters
// tag of a secret it leaves with a project tag, merged after the list the secret holds or the
// command names and never in place of it, keeps it while either project tag remains, and takes
// it out with the last one. --requesters none on a secret that keeps a project tag removes the list
// and keeps the guard, so the tag is written as the guard alone; beside another removal it is
// refused before any write, since the guard's write is no removal and the two would be two calls.
func TestRetagKeepsTheRollbackGuardWhileAProjectTagRemains(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.onEachStore, main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.onEachStore, main.useAWS, awsClients set at secret_test.go:375).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "API_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagRequesters: "ada@example.com", policy.TagAdmin: policy.AdminTrue, policy.TagMaxGrant: "1h"}, "v1")
		step := func(want string, args ...string) {
			t.Helper()
			if _, stderr, code := runSecret(append([]string{"retag", "API_KEY"}, args...)...); code != 0 {
				t.Fatalf("retag %v: exit %d, stderr %q", args, code, stderr)
			}
			tags := store.tags(t, "API_KEY")
			got, present := tags[policy.TagRequesters]
			switch {
			case want == "" && present:
				t.Fatalf("after retag %v the requesters tag is %q, want none", args, got)
			case want != "" && got != want:
				t.Fatalf("after retag %v the requesters tag is %q, want %q", args, got, want)
			}
		}
		step(guarded("ada@example.com"), "--project", "PRIV")
		step(guarded("ada@example.com"), "--project-channels", "slack")
		step(guarded("ada@example.com bob@example.com"), "--requesters", "bob@example.com ada@example.com")
		_, stderr, code := runSecret("retag", "API_KEY", "--requesters", "none", "--admin", "none")
		if code != exitUsageError || !strings.Contains(stderr, policy.RequestersRollbackGuard) {
			t.Fatalf("retag --requesters none --admin none on a project's secret: exit %d stderr %q; want a usage error naming the guard", code, stderr)
		}
		if got := store.tags(t, "API_KEY"); got[policy.TagRequesters] != guarded("ada@example.com bob@example.com") || got[policy.TagAdmin] != policy.AdminTrue {
			t.Fatalf("a refused retag changed the tags to %v", got)
		}
		step(guarded("ada@example.com bob@example.com"), "--admin", "none")
		step(guarded(""), "--requesters", "none")
		step(guarded(""), "--project", "none")
		step("", "--project-channels", "none")
		if tags := store.tags(t, "API_KEY"); tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierHuman || tags[policy.TagMaxGrant] != "1h" {
			t.Fatalf("tags at the end = %v, want owner, tier and max-grant kept", tags)
		}
	})
}

// TestRetagTakesTheGuardOutOfAListWithTheLastProjectTag: removing the last project tag from a
// secret whose requesters tag holds a list beside the guard leaves the list alone. On AWS that is
// two writes, the removal and then the list's rewrite, in that order, so a failed second write
// leaves the guard on a secret with no project tag, which no broker serves any differently.
func TestRetagTakesTheGuardOutOfAListWithTheLastProjectTag(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it signals the test process or changes its signal handling (main.onEachStore, main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		store.put(t, "LISTED_KEY", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierHuman, policy.TagProject: "PRIV", policy.TagRequesters: guarded("ada@example.com")}, "v1")
		stdout, stderr, code := runSecret("retag", "LISTED_KEY", "--project", "none")
		if code != 0 {
			t.Fatalf("retag --project none: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		tags := store.tags(t, "LISTED_KEY")
		if _, ok := tags[policy.TagProject]; ok || tags[policy.TagRequesters] != "ada@example.com" || tags[policy.TagOwner] != "ada@example.com" || tags[policy.TagTier] != policy.TierHuman {
			t.Fatalf("tags after --project none = %v, want no project and the requesters list without the guard", tags)
		}
	})
	t.Run("aws write order", func(t *testing.T) {
		listed := policytest.Secret("LISTED_KEY", "ada@example.com", policy.TierHuman, "v1")
		listed.Tags[policy.TagProject] = "PRIV"
		listed.Tags[policy.TagRequesters] = guarded("ada@example.com")
		local := secrets.NewLocal(listed)
		startSecretBroker(t, servedBy(local))
		writes := &countingTagWrites{Local: local}
		useAWS(t, writes, testAccount, adaSignIn)
		if _, stderr, code := runSecret("retag", "LISTED_KEY", "--project", "none"); code != 0 {
			t.Fatalf("retag --project none: exit %d, stderr %q", code, stderr)
		}
		if writes.untags != 1 || writes.tags != 1 {
			t.Fatalf("retag --project none made %d UntagResource and %d TagResource calls, want 1 and 1", writes.untags, writes.tags)
		}
		writes.refuseUntag = true
		listed.Tags[policy.TagProject] = "PRIV"
		listed.Tags[policy.TagRequesters] = guarded("ada@example.com")
		local.Put(listed)
		if _, _, code := runSecret("retag", "LISTED_KEY", "--project", "none"); code != 1 {
			t.Fatalf("retag --project none with UntagResource refused: exit %d, want 1", code)
		}
		if tags := secrets.TagMap(describe(t, local, "LISTED_KEY").Tags); tags[policy.TagProject] != "PRIV" || tags[policy.TagRequesters] != guarded("ada@example.com") {
			t.Fatalf("a refused removal left tags %v, want the project and the guard as they were", tags)
		}
	})
}

// TestTheGuardIsNoRequesterACommandCanName: --requesters naming the guard's text is refused, as any
// entry that is neither an email nor a service name is, so the guard reaches a requesters tag only
// beside a project tag, where this CLI writes it itself.
func TestTheGuardIsNoRequesterACommandCanName(t *testing.T) {
	t.Parallel()
	if _, err := checkRequesters(policy.RequestersRollbackGuard, true); err == nil {
		t.Fatalf("checkRequesters(%q) passed; want it refused", policy.RequestersRollbackGuard)
	}
}

// TestShowPrintsRequestersWithoutTheGuard: show, list --json and show --json print the requesters
// list without the guard, and say whether the secret carries the guard on a rollback_guard line
// and field of its own, so a census of the store finds every secret carrying a project tag
// without it.
func TestShowPrintsRequestersWithoutTheGuard(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	guardedKey := policytest.Secret("GUARDED_KEY", "ada@example.com", policy.TierHuman, "v1")
	guardedKey.Tags[policy.TagProject] = "PRIV"
	guardedKey.Tags[policy.TagRequesters] = guarded("ada@example.com")
	bare := policytest.Secret("BARE_KEY", "ada@example.com", policy.TierAgent, "v1")
	bare.Tags[policy.TagProject] = "PRIV"
	local := secrets.NewLocal(guardedKey, bare)
	startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	for _, c := range []struct {
		name, requesters, line string
		guard                  bool
	}{
		{"GUARDED_KEY", "ada@example.com", "yes", true},
		{"BARE_KEY", "-", "-", false},
	} {
		stdout, stderr, code := runSecret("show", c.name)
		if code != 0 {
			t.Fatalf("show %s: exit %d, stderr %q", c.name, code, stderr)
		}
		for _, want := range []string{"\nrequesters: " + c.requesters + "\n", "\nrollback_guard: " + c.line + "\n"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("show %s stdout %q must contain %q", c.name, stdout, want)
			}
		}
		jsonOut, stderr, code := runSecret("show", c.name, "--json")
		if code != 0 {
			t.Fatalf("show %s --json: exit %d, stderr %q", c.name, code, stderr)
		}
		var view struct {
			Requesters    string `json:"requesters"`
			RollbackGuard *bool  `json:"rollback_guard"`
		}
		if err := json.Unmarshal([]byte(jsonOut), &view); err != nil {
			t.Fatalf("show %s --json: %v: %s", c.name, err, jsonOut)
		}
		if want := strings.TrimPrefix(c.requesters, "-"); view.Requesters != want || view.RollbackGuard == nil || *view.RollbackGuard != c.guard {
			t.Errorf("show %s --json = %s; want requesters %q and rollback_guard %v", c.name, jsonOut, want, c.guard)
		}
	}
	stdout, stderr, code := runSecret("list", "--json")
	if code != 0 {
		t.Fatalf("list --json: exit %d, stderr %q", code, stderr)
	}
	var views []struct {
		Name          string  `json:"name"`
		Project       *string `json:"project"`
		RollbackGuard bool    `json:"rollback_guard"`
	}
	if err := json.Unmarshal([]byte(stdout), &views); err != nil {
		t.Fatalf("list --json: %v: %s", err, stdout)
	}
	unguarded := 0
	for _, v := range views {
		if v.Project != nil && !v.RollbackGuard {
			unguarded++
		}
	}
	if len(views) != 2 || unguarded != 1 {
		t.Fatalf("list --json = %s; want two rows and one project secret without the guard", stdout)
	}
}
