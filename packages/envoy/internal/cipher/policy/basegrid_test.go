package policy_test

// This file is the merge-base half of c-private-secrets' R9 check: a grid of secrets and
// requesters, and each cell's Set.Evaluate answer as legion main 545b2116 gave it, before the
// dispatch-project tag existed. testdata/evaluate-merge-base.json was written once, at the merge
// base, by this file as it was then (it used only API that existed at that commit), in the
// package's directory at that commit, internal/broker/policy (#4634 moved it to
// internal/cipher/policy later):
//
//	git worktree add <tmp>/d-golden 545b2116
//	sed 's#envoy/internal/cipher/#envoy/internal/broker/#' packages/envoy/internal/cipher/policy/basegrid_test.go > <tmp>/d-golden/packages/envoy/internal/broker/policy/basegrid_test.go
//	go test -C <tmp>/d-golden/packages/envoy ./internal/broker/policy -run TestAPrivateProjectSecretBaseGridIsRecorded -args -write-golden
//	cp <tmp>/d-golden/packages/envoy/internal/broker/policy/testdata/evaluate-merge-base.json packages/envoy/internal/cipher/policy/testdata/
//	git worktree remove <tmp>/d-golden
//
// Since #4144 renamed a secret's kind tag to type, the builder writes the type tag where it wrote
// the kind tag, with the same values; the cells and their names are unchanged. Never regenerate
// the golden at a later commit: a golden written by the code it checks compares that code with
// itself and proves nothing. A change that means to move an answer of this grid is a change to
// R9, which the golden exists to catch.

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

var writeGolden = flag.Bool("write-golden", false, "write testdata/evaluate-merge-base.json; run only at the merge base, as basegrid_test.go's head comment says")

const (
	// mergeBaseGolden is the grid's answers as the merge base gave them.
	mergeBaseGolden = "testdata/evaluate-merge-base.json"
	// gridPerson owns the grid's person-owned secrets and is the person a requesters list names.
	gridPerson = "ada@example.com"
	// gridOtherPerson is a person who owns nothing and is on no list.
	gridOtherPerson = "ben@example.com"
	// gridService owns the grid's service-owned secrets and is the service a requesters list
	// names; it is the one service the grid's loader registers.
	gridService = "grid-service"
	// gridOtherService is a service no secret names.
	gridOtherService = "grid-other-service"
)

// gridAnswer is one cell's Evaluate answer: the Decision's fields, or the error's text.
type gridAnswer struct {
	Outcome  string `json:"outcome,omitempty"`
	Approver string `json:"approver,omitempty"`
	Source   string `json:"source,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Error    string `json:"error,omitempty"`
}

// gridSecrets is the grid's secrets and their names: owner shared, person or service × tier
// agent or human × requesters none, a person or a service × type stored or github-app. Each name
// spells its cell. A combination the loader refuses — a service owning a human-tier secret —
// stays in the grid, answered ErrUnknownSecret, since a refusal is an answer too. edit, when not
// nil, changes each secret before it is stored.
func gridSecrets(edit func(*memory.Secret)) (*memory.Store, []string) {
	store := memory.New()
	var names []string
	for _, owner := range []struct{ name, tag string }{{"SHARED", policy.OwnerShared}, {"PERSON", gridPerson}, {"SERVICE", gridService}} {
		for _, tier := range []string{policy.TierAgent, policy.TierHuman} {
			for _, list := range []struct{ name, tag string }{{"NOLIST", ""}, {"LISTPERSON", gridPerson}, {"LISTSERVICE", gridService}} {
				for _, secretType := range []struct{ name, tag string }{{"STORED", ""}, {"APP", policy.TypeGitHubApp}} {
					name := "GRID_" + owner.name + "_" + map[string]string{policy.TierAgent: "AGENT", policy.TierHuman: "HUMAN"}[tier] + "_" + list.name + "_" + secretType.name
					s := policytest.Secret(name, owner.tag, tier, "v")
					if secretType.tag != "" {
						s = policytest.TypeSecret(name, owner.tag, tier, secretType.tag, "v")
					}
					if list.tag != "" {
						s.Tags[policy.TagRequesters] = list.tag
					}
					if edit != nil {
						edit(&s)
					}
					store.Put(s)
					names = append(names, name)
				}
			}
		}
	}
	return store, names
}

// gridRequesters is the grid's requesters: operator the owning person, another person or none ×
// service none, the listed one or another.
func gridRequesters() map[string]policy.Requester {
	requesters := map[string]policy.Requester{}
	for _, operator := range []struct{ name, login string }{{"owner", gridPerson}, {"other", gridOtherPerson}, {"none", ""}} {
		for _, service := range []struct{ name, service string }{{"none", ""}, {"listed", gridService}, {"other", gridOtherService}} {
			requesters["operator="+operator.name+" service="+service.name] = policy.Requester{Operator: operator.login, Service: service.service}
		}
	}
	return requesters
}

// gridAnswers is every cell's answer under set: each name of names, asked by each requester of
// gridRequesters, once as it is and once with that name withheld from it. adapt, when not nil,
// changes each requester before it asks.
func gridAnswers(set *policy.Set, names []string, adapt func(policy.Requester) policy.Requester) map[string]gridAnswer {
	answers := map[string]gridAnswer{}
	for _, name := range names {
		for who, r := range gridRequesters() {
			for _, withheld := range []bool{false, true} {
				asking := r
				if withheld {
					asking.Withheld = []string{name}
				}
				if adapt != nil {
					asking = adapt(asking)
				}
				key := name + " " + who + " withheld=" + map[bool]string{false: "no", true: "yes"}[withheld]
				d, err := set.Evaluate(name, asking)
				if err != nil {
					answers[key] = gridAnswer{Error: err.Error()}
					continue
				}
				answers[key] = gridAnswer{Outcome: d.Outcome, Approver: d.Approver, Source: d.Source, Reason: d.Reason}
			}
		}
	}
	return answers
}

// compareWithMergeBase fails t for each cell of answers the golden does not hold as the merge
// base answered it, naming the cell, and for a grid whose size is not the golden's or is zero.
func compareWithMergeBase(t *testing.T, answers map[string]gridAnswer) {
	t.Helper()
	raw, err := os.ReadFile(mergeBaseGolden)
	if err != nil {
		t.Fatalf("read %s: %v", mergeBaseGolden, err)
	}
	var golden map[string]gridAnswer
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode %s: %v", mergeBaseGolden, err)
	}
	if len(answers) == 0 || len(golden) == 0 {
		t.Fatalf("the grid holds %d cells and the golden %d entries; want both above zero, or the check compares nothing", len(answers), len(golden))
	}
	if len(answers) != len(golden) {
		t.Errorf("the grid holds %d cells and the golden %d entries; want them equal", len(answers), len(golden))
	}
	for cell, got := range answers {
		want, ok := golden[cell]
		if !ok {
			t.Errorf("cell %q is not in the golden", cell)
			continue
		}
		if got != want {
			t.Errorf("cell %q: Evaluate answers %+v, the merge base answered %+v", cell, got, want)
		}
	}
}

// TestAPrivateProjectSecretBaseGridIsRecorded is the grid with no project tag anywhere: every cell
// must answer as the merge base answered it, so a deployment with no project private reads each
// secret as it does today (R9). With -write-golden it writes the golden instead, which the head
// comment allows at the merge base alone.
func TestAPrivateProjectSecretBaseGridIsRecorded(t *testing.T) {
	store, names := gridSecrets(nil)
	set, err := policytest.Loader(store, gridService).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	answers := gridAnswers(set, names, nil)
	if *writeGolden {
		raw, err := json.MarshalIndent(answers, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(mergeBaseGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mergeBaseGolden, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d cells to %s", len(answers), mergeBaseGolden)
		return
	}
	compareWithMergeBase(t, answers)
}
