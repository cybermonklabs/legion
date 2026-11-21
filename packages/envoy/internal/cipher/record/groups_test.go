package record

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
)

// The group and people of the decision tests: example.com names alone.
const (
	testGroup     = "approvers@example.com"
	testGroupForm = GroupPrefix + testGroup
	testMember    = "member@example.com"
	testOutsider  = "outsider@example.com"
)

// memberDirectory is a group reader listing testMember in testGroup, fresh.
func memberDirectory() *groupstest.Directory {
	d := groupstest.New()
	d.Set(testGroup, testMember)
	return d
}

// TestMayDecideAdmitsAMemberOfAGroupApprover: an agent_secret record whose approver is a group is
// decided by a member of the group in any casing, and by no non-member, nobody with no group
// reader, nobody once the group is stale, and no login spelled as the group itself; a person
// approver still admits that person alone, and a machine login naming a group admits nobody.
func TestMayDecideAdmitsAMemberOfAGroupApprover(t *testing.T) {
	fresh, stale := memberDirectory(), memberDirectory()
	stale.MarkStale(testGroup)
	for _, c := range []struct {
		name                  string
		kind, approver, login string
		groups                Groups
		want                  bool
	}{
		{"a member", KindAgentSecret, testGroupForm, testMember, fresh, true},
		{"a member in another casing", KindAgentSecret, testGroupForm, " Member@Example.com ", fresh, true},
		{"a non-member", KindAgentSecret, testGroupForm, testOutsider, fresh, false},
		{"a member with no group reader", KindAgentSecret, testGroupForm, testMember, nil, false},
		{"a member of a stale group", KindAgentSecret, testGroupForm, testMember, stale, false},
		{"the group's own form as a login", KindAgentSecret, testGroupForm, testGroupForm, fresh, false},
		{"the group's address as a login", KindAgentSecret, testGroupForm, testGroup, fresh, false},
		{"a person approver, that person", KindAgentSecret, "ada@example.com", "ada@example.com", fresh, true},
		{"a person approver, a member", KindAgentSecret, "ada@example.com", testMember, fresh, false},
		{"a machine login naming the group, a member", KindLauncherCredential, testGroupForm, testMember, fresh, false},
	} {
		if got := MayDecide(c.kind, c.approver, "", c.login, c.groups); got != c.want {
			t.Errorf("%s: MayDecide(%s, %q, %q) = %v, want %v", c.name, c.kind, c.approver, c.login, got, c.want)
		}
	}
	key, _ := proof.NewKey()
	compact, err := Sign(key, "https://secrets.test", secretDetail(), "team", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := podBody(compact)
	body.Approver = testGroupForm
	if got, err := body.ApproverLogin(KindAgentSecret, "", " MEMBER@example.com ", fresh); err != nil || got != testMember {
		t.Fatalf("ApproverLogin(member) = %q, %v; want %q", got, err, testMember)
	}
	if _, err := body.ApproverLogin(KindAgentSecret, "", testOutsider, fresh); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("ApproverLogin(outsider) = %v, want ErrNotApprover", err)
	}
}

// TestChainVerifierAdmitsAGroupMembersApproval: a group-approved record decided by a member
// verifies; once the group is stale it is a lapsed grant (ErrApproverNotAdmittedNow, no broken
// chain); with the member dropped from a group that stays fresh it is a broken chain that carries
// the typed error, as is a decision row naming an outsider; and a body whose approver was changed
// after it was hashed is a broken chain whatever the group says.
func TestChainVerifierAdmitsAGroupMembersApproval(t *testing.T) {
	key, _ := proof.NewKey()
	now := time.Now()
	compact, err := Sign(key, "https://secrets.test", secretDetail(), "team", "", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	body := podBody(compact)
	body.Approver = testGroupForm
	verifier := func(groups Groups, stored, decider string) *ChainVerifier {
		return &ChainVerifier{
			Kind: KindAgentSecret, Audience: "https://secrets.test", Skew: time.Minute, Groups: groups,
			FetchRecord: func(context.Context, string) (string, time.Time, bool, error) {
				return stored, now, true, nil
			},
			FetchDecisions: func(context.Context, string) ([]TerminalEvent, error) {
				return []TerminalEvent{{Event: "approved", Login: decider, Actor: HumanActor(decider)}}, nil
			},
		}
	}
	directory := memberDirectory()
	if _, err := verifier(directory, body.Canonical(), testMember).Verify(context.Background(), body.ID()); err != nil {
		t.Fatalf("Verify(a member's approval) = %v, want it verified", err)
	}

	directory.MarkStale(testGroup)
	_, err = verifier(directory, body.Canonical(), testMember).Verify(context.Background(), body.ID())
	if !errors.Is(err, ErrApproverNotAdmittedNow) || errors.Is(err, ErrChainBroken) {
		t.Fatalf("Verify(a member's approval, group stale) = %v; want ErrApproverNotAdmittedNow and not ErrChainBroken", err)
	}

	directory.Set(testGroup)
	for _, decider := range []string{testMember, testOutsider} {
		_, err = verifier(directory, body.Canonical(), decider).Verify(context.Background(), body.ID())
		var typed ApproverNotAdmittedError
		if !errors.Is(err, ErrChainBroken) || errors.Is(err, ErrApproverNotAdmittedNow) || !errors.As(err, &typed) ||
			typed != (ApproverNotAdmittedError{Group: testGroup, Login: decider, Fresh: true}) {
			t.Fatalf("Verify(approved by %s, not in the fresh group) = %v (typed %+v); want ErrChainBroken carrying the fresh typed error", decider, err, typed)
		}
	}

	directory.Set(testGroup, testMember)
	changed := body
	changed.Approver = GroupPrefix + "other@example.com"
	if _, err := verifier(directory, changed.Canonical(), testMember).Verify(context.Background(), body.ID()); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("Verify(a body whose approver changed after it was hashed) = %v, want ErrChainBroken", err)
	}
}

// TestGroupOfReadsTheGroupForm: an approver of the group form answers its bare address, and an
// approver that is a person, a sentinel, or the prefix with no address answers none.
func TestGroupOfReadsTheGroupForm(t *testing.T) {
	if address, ok := GroupOf("group:approvers@example.com"); !ok || address != "approvers@example.com" {
		t.Fatalf(`GroupOf("group:approvers@example.com") = %q, %v; want "approvers@example.com", true`, address, ok)
	}
	for _, approver := range []string{"approvers@example.com", "group:", "shared", AnyoneApprover, ""} {
		if address, ok := GroupOf(approver); ok || address != "" {
			t.Errorf("GroupOf(%q) = %q, %v; want \"\", false", approver, address, ok)
		}
	}
}

// TestApproverNotAdmittedIsLapsedOnlyWhenStale: a decider a stale group cannot vouch for is a
// lapsed grant (ErrApproverNotAdmittedNow), a decider a fresh group does not list is not, and the
// typed error is still found inside the broken-chain error that wraps it.
func TestApproverNotAdmittedIsLapsedOnlyWhenStale(t *testing.T) {
	stale := ApproverNotAdmittedError{Group: "approvers@example.com", Login: "member@example.com", Fresh: false}
	if !errors.Is(stale, ErrApproverNotAdmittedNow) {
		t.Fatalf("errors.Is(%v, ErrApproverNotAdmittedNow) = false for a stale group, want true", stale)
	}
	fresh := ApproverNotAdmittedError{Group: "approvers@example.com", Login: "member@example.com", Fresh: true}
	if errors.Is(fresh, ErrApproverNotAdmittedNow) {
		t.Fatalf("errors.Is(%v, ErrApproverNotAdmittedNow) = true for a fresh group, want false", fresh)
	}
	wrapped := fmt.Errorf("%w: %w", ErrChainBroken, fresh)
	var typed ApproverNotAdmittedError
	if !errors.As(wrapped, &typed) || typed != fresh {
		t.Fatalf("errors.As(%v) = %+v; want %+v", wrapped, typed, fresh)
	}
	if !errors.Is(wrapped, ErrChainBroken) || errors.Is(wrapped, ErrApproverNotAdmittedNow) {
		t.Fatalf("%v: want ErrChainBroken and not ErrApproverNotAdmittedNow", wrapped)
	}
}
