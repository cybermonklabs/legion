package policy_test

// This file is the merge-base half of the kinds' rollback guard check: secrets that carry a proxy
// tag, and a proxy-narrowed one, as `agent-secrets secret` writes them, with the kinds guard entry
// in their requesters tag, a bare twin of each that carries the same tags without it, and what the
// broker before the kinds answers for each. That broker is the merge base of the kinds, commit
// 233828e4aeb: it reads neither proxy tag, so it serves the bare
// twin, a secret meant for Cipher's proxy alone, as its value. The file uses only API that exists at
// that commit, so it is the one file the golden is generated from.
// testdata/kinds-guard-merge-base.json was written once, at the merge base, by:
//
//	git archive 233828e4aeb packages/envoy | tar -x -C <tmp>
//	cp packages/envoy/internal/cipher/policy/kindsguard_base_test.go <tmp>/packages/envoy/internal/cipher/policy/
//	go test -C <tmp>/packages/envoy ./internal/cipher/policy -run TestTheKindsGuardBaseAnswersAreRecorded -count=1 -v -args -write-kinds-guard-golden
//	cp <tmp>/packages/envoy/internal/cipher/policy/testdata/kinds-guard-merge-base.json packages/envoy/internal/cipher/policy/testdata/
//
// Never regenerate it at a later commit: a later broker reads the guard as no requester, so a
// golden written there records what this broker answers and not what a rollback would. The head
// half, which reads the golden and checks this broker against it, is kinds_guard_test.go. The
// answer type and the two people are the project tags' check's (rollbackguard_base_test.go),
// which the merge base holds too.

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

var writeKindsGuardGolden = flag.Bool("write-kinds-guard-golden", false, "write testdata/kinds-guard-merge-base.json; run only at the merge base, as kindsguard_base_test.go's head comment says")

const (
	// kindsGuardGolden is what the merge base answers for each of kindsGuardCases.
	kindsGuardGolden = "testdata/kinds-guard-merge-base.json"
	// The tag keys and the guard entry as this broker and `agent-secrets` write them, spelled out
	// because the merge base has no names for them. kinds_guard_test.go holds each to this
	// broker's own name, so this file records the strings the CLI writes.
	baseTagProxy         = "proxy"
	baseTagProxyNarrowed = "proxy-narrowed"
	baseKindsGuard       = "rollback-guard:kinds"
)

// kindsGuardCase is one secret of the check: its name, the secret as the store holds it, whether
// it carries the guard, and list, the requesters list it carries beside the guard ("" for none).
type kindsGuardCase struct {
	name    string
	secret  secrets.LocalSecret
	guarded bool
	list    string
}

// kindsGuardCases is the check's secrets: each shape of secret behind a kind, once as
// KGUARDED_<shape>, with the guard entry in its requesters tag, and once as KBARE_<shape>, the same
// tags without it. The bare twin is the control: the merge base serves it, so the guard, and no
// other tag of the shape, is what refuses its guarded twin. The shapes are a shared stored token
// behind slack, a person's human-tier token with its own requesters list, an administrator's (a
// list and an hour's cap are required) behind github, a stored token narrowed by a profile
// (proxy-narrowed=true beside its proxy tag), a github-app secret, and an anthropic-federation one
// behind the anthropic kind (its owner's list, an hour's cap and human tier are required).
func kindsGuardCases() []kindsGuardCase {
	shapes := []struct {
		shape string
		make  func(name string) secrets.LocalSecret
		list  string
		tags  map[string]string
	}{
		{"STORED", func(name string) secrets.LocalSecret {
			return policytest.Secret(name, policy.OwnerShared, policy.TierAgent, "v")
		}, "", map[string]string{baseTagProxy: "slack"}},
		{"LISTED", func(name string) secrets.LocalSecret {
			return policytest.Secret(name, guardOwner, policy.TierHuman, "v")
		}, guardOwner, map[string]string{baseTagProxy: "github"}},
		{"ADMIN", func(name string) secrets.LocalSecret {
			return policytest.Secret(name, guardOwner, policy.TierHuman, "v")
		}, guardOwner, map[string]string{baseTagProxy: "github", policy.TagAdmin: policy.AdminTrue, policy.TagMaxGrant: "1h"}},
		{"NARROWED", func(name string) secrets.LocalSecret {
			return policytest.Secret(name, policy.OwnerShared, policy.TierAgent, "v")
		}, "", map[string]string{baseTagProxy: "github", baseTagProxyNarrowed: "true"}},
		{"APP", func(name string) secrets.LocalSecret {
			return policytest.TypeSecret(name, policy.OwnerShared, policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp))
		}, "", map[string]string{baseTagProxy: "github"}},
		{"FEDERATION", func(name string) secrets.LocalSecret {
			return policytest.TypeSecret(name, guardOwner, policy.TierHuman, policy.TypeAnthropicFederation, policytest.TypeValue(policy.TypeAnthropicFederation))
		}, guardOwner, map[string]string{baseTagProxy: "anthropic", policy.TagAdmin: policy.AdminTrue, policy.TagMaxGrant: "1h"}},
	}
	var cases []kindsGuardCase
	for _, shape := range shapes {
		for _, guarded := range []bool{true, false} {
			name := "KBARE_" + shape.shape
			if guarded {
				name = "KGUARDED_" + shape.shape
			}
			s := shape.make(name)
			for key, value := range shape.tags {
				s.Tags[key] = value
			}
			delete(s.Tags, policy.TagRequesters)
			switch {
			case guarded && shape.list != "":
				s.Tags[policy.TagRequesters] = shape.list + " " + baseKindsGuard
			case guarded:
				s.Tags[policy.TagRequesters] = baseKindsGuard
			case shape.list != "":
				s.Tags[policy.TagRequesters] = shape.list
			}
			cases = append(cases, kindsGuardCase{name: name, secret: s, guarded: guarded, list: shape.list})
		}
	}
	return cases
}

// kindsGuardStore is a store holding every case.
func kindsGuardStore() *secrets.Local {
	store := secrets.NewLocal()
	for _, c := range kindsGuardCases() {
		store.Put(c.secret)
	}
	return store
}

// kindsGuardAnswers is what loader's Load answers for each case: served or refused, with the
// reason, and for a served one what Evaluate answers guardOutsider.
func kindsGuardAnswers(t *testing.T, loader policy.Loader) map[string]rollbackAnswer {
	t.Helper()
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	answers := map[string]rollbackAnswer{}
	for _, c := range kindsGuardCases() {
		if _, served := set.Secrets[c.name]; !served {
			answers[c.name] = rollbackAnswer{Reason: set.Refused[policytest.ID(c.name)].Reason}
			continue
		}
		answer := rollbackAnswer{Served: true}
		if d, err := set.Evaluate(c.name, policy.Requester{Operator: guardOutsider}); err != nil {
			answer.Outcome = "error: " + err.Error()
		} else {
			answer.Outcome = d.Outcome
		}
		answers[c.name] = answer
	}
	return answers
}

// readKindsGuardGolden is the golden's one reader, for this file's check and for
// kinds_guard_test.go's. It fails t unless the golden records an answer for exactly the cases
// kindsGuardCases builds, by name, and at least one, so a case added to or taken out of this file
// without the golden written again at the merge base fails rather than going unrecorded.
func readKindsGuardGolden(t *testing.T) map[string]rollbackAnswer {
	t.Helper()
	raw, err := os.ReadFile(kindsGuardGolden)
	if err != nil {
		t.Fatalf("read %s: %v", kindsGuardGolden, err)
	}
	var golden map[string]rollbackAnswer
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode %s: %v", kindsGuardGolden, err)
	}
	var cases, recorded []string
	for _, c := range kindsGuardCases() {
		cases = append(cases, c.name)
	}
	for name := range golden {
		recorded = append(recorded, name)
	}
	slices.Sort(cases)
	slices.Sort(recorded)
	if len(cases) == 0 || !slices.Equal(cases, recorded) {
		t.Fatalf("this file builds the cases %v and %s records %v; want the same names, and above zero: write the golden again at the merge base, as the head comment says", cases, kindsGuardGolden, recorded)
	}
	return golden
}

// TestTheKindsGuardBaseAnswersAreRecorded writes the golden, with -write-kinds-guard-golden, at
// the merge base alone. Without the flag it checks that the golden records exactly the cases this
// file builds (readKindsGuardGolden); what this broker answers for each case is
// kinds_guard_test.go's check. It never skips: the broker packages' run counts every skip as a
// database suite that did not run (database-skips.sh).
func TestTheKindsGuardBaseAnswersAreRecorded(t *testing.T) {
	if !*writeKindsGuardGolden {
		readKindsGuardGolden(t)
		return
	}
	answers := kindsGuardAnswers(t, policytest.Loader(kindsGuardStore()))
	raw, err := json.MarshalIndent(answers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(kindsGuardGolden), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kindsGuardGolden, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d answers to %s", len(answers), kindsGuardGolden)
}
