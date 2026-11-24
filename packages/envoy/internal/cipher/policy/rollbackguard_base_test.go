package policy_test

// This file is the merge-base half of the project tags' rollback guard check: secrets that carry
// dispatch-project or project-channels as `agent-secrets secret` writes them, with the guard entry
// in their requesters tag, a bare twin of each that carries the same project tags without it, and
// what legion main 545b2116, the release before the project tags, answers for each. It uses only
// API that exists at that commit, so it is the one file the golden is generated from. At that
// commit the package is internal/broker/policy (#4634 moved it to internal/cipher/policy later), so
// the recipe puts the file there with its imports pointed back at internal/broker.
// testdata/rollback-guard-merge-base.json was written once, at the merge base, by:
//
//	git worktree add --detach <tmp>/guard-base 545b2116
//	sed 's#envoy/internal/cipher/#envoy/internal/broker/#' packages/envoy/internal/cipher/policy/rollbackguard_base_test.go > <tmp>/guard-base/packages/envoy/internal/broker/policy/rollbackguard_base_test.go
//	go test -C <tmp>/guard-base/packages/envoy ./internal/broker/policy -run TestTheRollbackGuardBaseAnswersAreRecorded -count=1 -v -args -write-rollback-golden
//	cp <tmp>/guard-base/packages/envoy/internal/broker/policy/testdata/rollback-guard-merge-base.json packages/envoy/internal/cipher/policy/testdata/
//
// Never regenerate it at a later commit: a later broker reads the guard as no requester, so a
// golden written there records what this broker answers and not what a rollback would. The head
// half, which reads the golden and checks this broker against it, is rollback_guard_test.go.

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
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

var writeRollbackGolden = flag.Bool("write-rollback-golden", false, "write testdata/rollback-guard-merge-base.json; run only at the merge base, as rollbackguard_base_test.go's head comment says")

const (
	// rollbackGolden is what the merge base answers for each of rollbackGuardCases.
	rollbackGolden = "testdata/rollback-guard-merge-base.json"
	// The tag keys and the guard entry as a later broker and `agent-secrets` write them, spelled
	// out because the merge base has no names for them. rollback_guard_test.go holds each to this
	// broker's own name, so this file records the strings the CLI writes.
	baseTagProject         = "dispatch-project"
	baseTagProjectChannels = "project-channels"
	baseRollbackGuard      = "rollback-guard:project-tags"
	// The classifier and shape tags as the CLI wrote them when the golden was recorded, spelled
	// out because this broker has no names for them since #4144: it reads "kind" as its legacy
	// type tag (policy.LegacyTypeTag) and no "shape" tag at all. Writing the same strings keeps
	// the cases the ones the golden answers, and keeps this file one that builds at the merge
	// base. Today's CLI writes "type" instead of "kind", which a broker as old as the merge base
	// does not read; that rollback floor is #4144's, and the guard entry this check is about is
	// read from the requesters tag whichever classifier tag a secret carries.
	baseTagKind     = "kind"
	baseTagShape    = "shape"
	baseShapeToken  = "token"
	baseKindApp     = "github-app"
	baseKindAccount = "google-service-account"
	// baseKindGuard is the classifier's own rollback guard value as the CLI wrote it beside kind
	// (policy.RollbackGuardValue at the merge base); #4144 made it type-secret.
	baseKindGuard = "kind-secret"
	// guardOwner owns the person-owned cases and is the person their requesters lists name.
	guardOwner = "carol@example.com"
	// guardOutsider is a person who owns nothing and is on no list: the session a broker that
	// cannot read the project tags would hand a project's secret to.
	guardOutsider = "mallory@example.com"
)

// rollbackCase is one secret of the check: its name, the secret as the store holds it, whether it
// carries the guard, and list, the requesters list it carries beside the guard ("" for none).
type rollbackCase struct {
	name    string
	secret  awstest.LocalSecret
	guarded bool
	list    string
}

// rollbackAnswer is what a broker's Load answers for one secret, and, for one it serves, what
// Evaluate answers guardOutsider's session that names no project.
type rollbackAnswer struct {
	Served  bool   `json:"served"`
	Reason  string `json:"reason,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// rollbackGuardCases is the check's secrets: each shape of secret a project can hold, once as
// GUARDED_<shape>, with the guard entry in its requesters tag, and once as BARE_<shape>, the same
// tags without it. The bare twin is the control: the merge base serves it, so the guard, and no
// other tag of the shape, is what refuses its guarded twin. The shapes are a stored shared secret,
// a person's human-tier one with its own requesters list, an admin one (a list and an hour's cap
// are required), a github-app one (with the classifier's own rollback guard), a
// google-service-account one (its owner's list is required), a project-channels one with no
// project, and the Slack token as a project holds it (session default, token shape, both tags).
func rollbackGuardCases() []rollbackCase {
	shapes := []struct {
		shape string
		make  func(name string) awstest.LocalSecret
		list  string
		tags  map[string]string
	}{
		{"STORED", func(name string) awstest.LocalSecret {
			return policytest.Secret(name, policy.OwnerShared, policy.TierAgent, "v")
		}, "", map[string]string{baseTagProject: "PRIV"}},
		{"LISTED", func(name string) awstest.LocalSecret {
			return policytest.Secret(name, guardOwner, policy.TierHuman, "v")
		}, guardOwner, map[string]string{baseTagProject: "PRIV"}},
		{"ADMIN", func(name string) awstest.LocalSecret {
			return policytest.Secret(name, guardOwner, policy.TierHuman, "v")
		}, guardOwner, map[string]string{baseTagProject: "PRIV", policy.TagAdmin: policy.AdminTrue, policy.TagMaxGrant: "1h"}},
		{"APP", func(name string) awstest.LocalSecret {
			return baseKindSecret(name, policy.OwnerShared, policy.TierAgent, baseKindApp)
		}, "", map[string]string{baseTagProject: "PRIV", policy.TagRollbackGuard: baseKindGuard}},
		{"ACCOUNT", func(name string) awstest.LocalSecret {
			return baseKindSecret(name, guardOwner, policy.TierHuman, baseKindAccount)
		}, guardOwner, map[string]string{baseTagProject: "PRIV", policy.TagRollbackGuard: baseKindGuard}},
		{"CHANNELS", func(name string) awstest.LocalSecret {
			return policytest.Secret(name, policy.OwnerShared, policy.TierAgent, "v")
		}, "", map[string]string{baseTagProjectChannels: "slack"}},
		{"SLACK", func(name string) awstest.LocalSecret {
			return policytest.Secret(name, policy.OwnerShared, policy.TierAgent, "v")
		}, "", map[string]string{baseTagProject: "PRIV", baseTagProjectChannels: "slack", policy.TagDefaultGrant: policy.DefaultGrantSession, baseTagShape: baseShapeToken}},
	}
	var cases []rollbackCase
	for _, shape := range shapes {
		for _, guarded := range []bool{true, false} {
			name := "BARE_" + shape.shape
			if guarded {
				name = "GUARDED_" + shape.shape
			}
			s := shape.make(name)
			for key, value := range shape.tags {
				s.Tags[key] = value
			}
			delete(s.Tags, policy.TagRequesters)
			switch {
			case guarded && shape.list != "":
				s.Tags[policy.TagRequesters] = shape.list + " " + baseRollbackGuard
			case guarded:
				s.Tags[policy.TagRequesters] = baseRollbackGuard
			case shape.list != "":
				s.Tags[policy.TagRequesters] = shape.list
			}
			cases = append(cases, rollbackCase{name: name, secret: s, guarded: guarded, list: shape.list})
		}
	}
	return cases
}

// baseKindSecret is the merge base's policytest.KindSecret, spelled with this file's literals: a
// secret tagged kind, with the github-owners a github-app one needs and the requesters list and
// hour's cap a google-service-account one needs.
func baseKindSecret(name, owner, tier, kind string) awstest.LocalSecret {
	secret := policytest.Secret(name, owner, tier, "v")
	secret.Tags[baseTagKind] = kind
	switch kind {
	case baseKindApp:
		secret.Tags[policy.TagGitHubOwners] = policytest.FixtureGitHubOwners
	case baseKindAccount:
		secret.Tags[policy.TagRequesters] = owner
		secret.Tags[policy.TagMaxGrant] = policytest.FixtureMaxGrant
	}
	return secret
}

// rollbackGuardStore is a store holding every case.
func rollbackGuardStore() *awstest.Local {
	store := awstest.NewLocal()
	for _, c := range rollbackGuardCases() {
		store.Put(c.secret)
	}
	return store
}

// rollbackAnswers is what loader answers for each case: served or refused, with the reason, and
// for a served one what Evaluate answers guardOutsider.
func rollbackAnswers(t *testing.T, loader policy.Loader) map[string]rollbackAnswer {
	t.Helper()
	set, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	answers := map[string]rollbackAnswer{}
	for _, c := range rollbackGuardCases() {
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

// readRollbackGolden is the golden's one reader, for this file's check and for
// rollback_guard_test.go's. It fails t unless the golden records an answer for exactly the cases
// rollbackGuardCases builds, by name, and at least one, so a case added to or taken out of this
// file without the golden written again at the merge base fails rather than going unrecorded.
func readRollbackGolden(t *testing.T) map[string]rollbackAnswer {
	t.Helper()
	raw, err := os.ReadFile(rollbackGolden)
	if err != nil {
		t.Fatalf("read %s: %v", rollbackGolden, err)
	}
	var golden map[string]rollbackAnswer
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode %s: %v", rollbackGolden, err)
	}
	var cases, recorded []string
	for _, c := range rollbackGuardCases() {
		cases = append(cases, c.name)
	}
	for name := range golden {
		recorded = append(recorded, name)
	}
	slices.Sort(cases)
	slices.Sort(recorded)
	if len(cases) == 0 || !slices.Equal(cases, recorded) {
		t.Fatalf("this file builds the cases %v and %s records %v; want the same names, and above zero: write the golden again at the merge base, as the head comment says", cases, rollbackGolden, recorded)
	}
	return golden
}

// TestTheRollbackGuardBaseAnswersAreRecorded writes the golden, with -write-rollback-golden, at the
// merge base alone. Without the flag it checks that the golden records exactly the cases this file
// builds (readRollbackGolden); what this broker answers for each case is rollback_guard_test.go's
// check. It never skips: the broker packages' run counts every skip as a database suite that did
// not run (database-skips.sh).
func TestTheRollbackGuardBaseAnswersAreRecorded(t *testing.T) {
	if !*writeRollbackGolden {
		readRollbackGolden(t)
		return
	}
	answers := rollbackAnswers(t, policytest.Loader(rollbackGuardStore()))
	raw, err := json.MarshalIndent(answers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rollbackGolden), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackGolden, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d answers to %s", len(answers), rollbackGolden)
}
