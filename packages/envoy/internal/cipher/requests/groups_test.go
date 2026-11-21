// groups_test.go holds every decision site of the request machine for a secret a Google group
// owns: a member decides, lists and revokes, a non-member and an outsider do none of them, and a
// grant of the group's secret is re-checked at each release and reuse, so a stale group ends it
// and a decider the group no longer lists is refused with one log line.
package requests

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The group and people of these tests: example.com names alone, since the repository is public.
const (
	approversGroup = "approvers@example.com"
	approversForm  = record.GroupPrefix + approversGroup
	groupMember    = "member@example.com"
	secondMember   = "second@example.com"
	groupOutsider  = "outsider@example.com"
	// groupAsker is a person outside the group whose session asks for the group's secrets.
	groupAsker = "ada@example.com"
)

// groupSecrets are the group's two secrets: TEAM_KEY, human tier, which a member approves, and
// TEAM_TOKEN, agent tier with no requesters tag, which a member's session gets at once.
func groupSecrets() []secrets.LocalSecret {
	return []secrets.LocalSecret{
		policytest.Secret("TEAM_KEY", approversForm, policy.TierHuman, "team-v1"),
		policytest.Secret("TEAM_TOKEN", approversForm, policy.TierAgent, "token-v1"),
	}
}

// groupFixture is a Machine over groupSecrets whose policy, decisions, lists and chain check all
// read one stand-in group reader listing groupMember and secondMember in approversGroup, with a
// session of groupMember's and one of groupAsker's.
type groupFixture struct {
	m                   *Machine
	directory           *groupstest.Directory
	member, asker       string
	memberKey, askerKey *ecdsa.PrivateKey
}

func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(groupSecrets()...)
	directory := groupstest.New()
	directory.Set(approversGroup, groupMember, secondMember)
	loader := policytest.Loader(local)
	loader.Groups = directory
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	m := &Machine{
		Store:      st,
		Policy:     current,
		Secrets:    policytest.Store(local),
		PendingTTL: 12 * time.Hour,
		Audience:   testAudience,
		Skew:       time.Minute,
		Replay:     replayer(st),
		Groups:     directory,
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute, m.Airlock)
	m.Chain.Groups = directory
	f := &groupFixture{m: m, directory: directory}
	f.member, f.memberKey = newEnrollment(t, st, "session", "box-member-"+t.Name(), new(groupMember), nil)
	f.asker, f.askerKey = newEnrollment(t, st, "session", "box-asker-"+t.Name(), new(groupAsker), nil)
	return f
}

// ask is Create of names on enr, signed with key.
func (f *groupFixture) ask(t *testing.T, enr string, key *ecdsa.PrivateKey, names ...string) Request {
	t.Helper()
	req, err := f.m.Create(context.Background(), enr, signRequest(t, f.m, key, "team work", names...), "", nil)
	if err != nil {
		t.Fatalf("Create(%v): %v", names, err)
	}
	return req
}

// approvedTeamKey is the asker's grant of TEAM_KEY, approved by groupMember.
func (f *groupFixture) approvedTeamKey(t *testing.T) (recordID, grantID string) {
	t.Helper()
	req := f.ask(t, f.asker, f.askerKey, "TEAM_KEY")
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("the asker's TEAM_KEY request = %+v; want pending with a record", req)
	}
	dec, err := f.m.ApplyDecision(context.Background(), *req.RecordID, true, groupMember)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("ApplyDecision(TEAM_KEY) by a member = %+v, %v; want a grant", dec, err)
	}
	return *req.RecordID, dec.GrantID
}

// TestStillAllowedAdmitsAGroupMemberAndRefusesANonMember: a grant a member approved stays live
// under the group's live members; one decided by a non-member is refused as a decider the fresh
// group does not list, and one decided by a member of a stale group as a lapsed grant.
func TestStillAllowedAdmitsAGroupMemberAndRefusesANonMember(t *testing.T) {
	local := secrets.NewLocal(groupSecrets()...)
	directory := groupstest.New()
	directory.Set(approversGroup, groupMember)
	loader := policytest.Loader(local)
	loader.Groups = directory
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	set := current.Get()
	source := set.Secrets["TEAM_KEY"].Source
	asker := policy.Requester{Operator: groupAsker}
	if err := stillAllowed(set, proxyrules.Rules{}, "TEAM_KEY", source, policy.Approval, groupMember, nil, asker); err != nil {
		t.Fatalf("stillAllowed(approved by a member) = %v, want nil", err)
	}
	for _, c := range []struct {
		name, decider string
		stale         bool
	}{
		{"a non-member", groupOutsider, false},
		{"a member of a stale group", groupMember, true},
	} {
		if c.stale {
			directory.MarkStale(approversGroup)
		}
		err := stillAllowed(set, proxyrules.Rules{}, "TEAM_KEY", source, policy.Approval, c.decider, nil, asker)
		var lapsed record.ApproverNotAdmittedError
		if !errors.Is(err, ErrGrantNotLive) || !errors.As(err, &lapsed) ||
			lapsed != (record.ApproverNotAdmittedError{Group: approversGroup, Login: c.decider, Fresh: !c.stale}) {
			t.Fatalf("%s: stillAllowed = %v (typed %+v); want ErrGrantNotLive carrying the group's refusal, fresh %v", c.name, err, lapsed, !c.stale)
		}
	}
}

// TestMaySettleAdmitsAGroupMember: a person settles a name the policy routes to a group while the
// group lists them, and not when it does not, nor once it is stale.
func TestMaySettleAdmitsAGroupMember(t *testing.T) {
	directory := groupstest.New()
	directory.Set(approversGroup, groupMember)
	member, outsider := byPerson(groupMember), byPerson(groupOutsider)
	member.groups, outsider.groups = directory, directory
	if !member.maySettle(approversForm, approversForm) || outsider.maySettle(approversForm, approversForm) {
		t.Fatalf("maySettle: member %v, outsider %v; want true, false", member.maySettle(approversForm, approversForm), outsider.maySettle(approversForm, approversForm))
	}
	directory.MarkStale(approversGroup)
	if member.maySettle(approversForm, approversForm) {
		t.Fatal("a member of a stale group settles its name")
	}
}

// TestPendingForApproverListsAGroupRequestForEachMember: a request waiting on the group is listed
// for each of its two members, and for neither a person outside it nor the asker.
func TestPendingForApproverListsAGroupRequestForEachMember(t *testing.T) {
	f := newGroupFixture(t)
	req := f.ask(t, f.asker, f.askerKey, "TEAM_KEY")
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("TEAM_KEY = %+v; want pending", req)
	}
	var approver string
	if err := f.m.Store.Pool.QueryRow(context.Background(), `select approver from credential_requests where id=$1`, *req.RecordID).Scan(&approver); err != nil || approver != approversForm {
		t.Fatalf("the record's approver is %q, %v; want %s", approver, err, approversForm)
	}
	for _, login := range []string{groupMember, " Second@Example.com ", groupOutsider, groupAsker} {
		pending, err := f.m.PendingForApprover(context.Background(), login, PendingStates{})
		if err != nil {
			t.Fatalf("PendingForApprover(%s): %v", login, err)
		}
		listed := slices.ContainsFunc(pending, func(p PendingSummary) bool { return p.RecordID == *req.RecordID })
		if want := login == groupMember || login == " Second@Example.com "; listed != want {
			t.Errorf("PendingForApprover(%q) lists the group's request: %v, want %v", login, listed, want)
		}
		byState, err := f.m.PendingForApprover(context.Background(), login, PendingStates{Pending: true, Expired: true})
		if err != nil {
			t.Fatalf("PendingForApprover(%s, by state): %v", login, err)
		}
		if got := slices.ContainsFunc(byState, func(p PendingSummary) bool { return p.RecordID == *req.RecordID }); got != listed {
			t.Errorf("PendingForApprover(%q, by state) lists it: %v, the list with no state %v", login, got, listed)
		}
	}
}

// TestAMemberListsAndRevokesAGroupOwnedGrant: a grant one member approved is on another member's
// list, and that member's revoke ends it; the grant and the request name the deciding member.
func TestAMemberListsAndRevokesAGroupOwnedGrant(t *testing.T) {
	f := newGroupFixture(t)
	_, grantID := f.approvedTeamKey(t)
	grants, err := f.m.GrantsForApprover(context.Background(), secondMember)
	if err != nil {
		t.Fatalf("GrantsForApprover(second member): %v", err)
	}
	i := slices.IndexFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID })
	if i < 0 {
		t.Fatalf("GrantsForApprover(second member) = %+v; want the group's grant %s", grants, grantID)
	}
	if grants[i].Approver != approversForm || grants[i].DecidedBy != groupMember {
		t.Fatalf("the listed grant names approver %q, decided by %q; want %s and %s", grants[i].Approver, grants[i].DecidedBy, approversForm, groupMember)
	}
	if err := f.m.RevokeByApprover(context.Background(), grantID, secondMember, record.HumanActor(secondMember)); err != nil {
		t.Fatalf("RevokeByApprover(second member) = %v, want nil", err)
	}
	if _, err := f.m.Values(context.Background(), grantID, f.asker, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values after a member's revoke = %v, want ErrGrantNotLive", err)
	}
}

// TestAnOutsiderNeitherListsNorRevokesAGroupOwnedGrant: a person outside the group finds the
// group's grant on no list of their own, and their revoke is ErrNotApprover and ends nothing.
func TestAnOutsiderNeitherListsNorRevokesAGroupOwnedGrant(t *testing.T) {
	f := newGroupFixture(t)
	_, grantID := f.approvedTeamKey(t)
	grants, err := f.m.GrantsForApprover(context.Background(), groupOutsider)
	if err != nil {
		t.Fatalf("GrantsForApprover(outsider): %v", err)
	}
	if slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == grantID }) {
		t.Fatalf("GrantsForApprover(outsider) lists the group's grant: %+v", grants)
	}
	if err := f.m.RevokeByApprover(context.Background(), grantID, groupOutsider, record.HumanActor(groupOutsider)); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeByApprover(outsider) = %v, want ErrNotApprover", err)
	}
	if _, err := f.m.Values(context.Background(), grantID, f.asker, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values after the outsider's refused revoke = %v, want the grant still live", err)
	}
}

// automaticTeamToken is the member's automatic grant of TEAM_TOKEN, released once.
func (f *groupFixture) automaticTeamToken(t *testing.T) string {
	t.Helper()
	req := f.ask(t, f.member, f.memberKey, "TEAM_TOKEN")
	if req.State != "granted" || req.GrantID == nil || req.RecordID != nil {
		t.Fatalf("the member's TEAM_TOKEN = %+v; want granted at once, with no record", req)
	}
	if _, err := f.m.Values(context.Background(), *req.GrantID, f.member, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values of the member's automatic grant = %v, want its value", err)
	}
	return *req.GrantID
}

// staleUnderTheSamePolicy marks the group stale and fails t unless the live policy is the very
// set it was, at the same version, so no re-check below is the version's doing.
func (f *groupFixture) staleUnderTheSamePolicy(t *testing.T) {
	t.Helper()
	before := f.m.Policy.Get()
	f.directory.MarkStale(approversGroup)
	if after := f.m.Policy.Get(); after != before || after.Version != before.Version {
		t.Fatal("the policy moved while the group went stale")
	}
}

// TestAnAutomaticGrantOfAStaleGroupIsRefusedAtItsNextReleaseUnderAnUnchangedPolicy: the member's
// automatic grant of the group's agent-tier secret releases while the group is fresh, and is
// refused at its next release once the group is stale, the policy's version unchanged.
func TestAnAutomaticGrantOfAStaleGroupIsRefusedAtItsNextReleaseUnderAnUnchangedPolicy(t *testing.T) {
	f := newGroupFixture(t)
	grantID := f.automaticTeamToken(t)
	f.staleUnderTheSamePolicy(t)
	if release, err := f.m.Values(context.Background(), grantID, f.member, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values of the automatic grant, group stale = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
}

// TestAnAutomaticGrantOfAStaleGroupIsNotReusedUnderAnUnchangedPolicy: in the same state, the
// member's ask for the same name gets no grant back, and waits for the group's approval.
func TestAnAutomaticGrantOfAStaleGroupIsNotReusedUnderAnUnchangedPolicy(t *testing.T) {
	f := newGroupFixture(t)
	grantID := f.automaticTeamToken(t)
	f.staleUnderTheSamePolicy(t)
	req := f.ask(t, f.member, f.memberKey, "TEAM_TOKEN")
	if req.State != "pending" || req.RecordID == nil || (req.GrantID != nil && *req.GrantID == grantID) {
		t.Fatalf("the member's ask, group stale = %+v; want a new pending request, not the grant %s", req, grantID)
	}
	var approver string
	if err := f.m.Store.Pool.QueryRow(context.Background(), `select approver from credential_requests where id=$1`, *req.RecordID).Scan(&approver); err != nil || approver != approversForm {
		t.Fatalf("the new request waits on %q, %v; want %s", approver, err, approversForm)
	}
}

// TestAnApprovedGrantOfAStaleGroupIsRefusedWithNoChainAlarm: a grant a member approved is a
// lapsed grant once the group is stale: refused as no longer live, the group's refusal beneath,
// and no broken-chain line written.
func TestAnApprovedGrantOfAStaleGroupIsRefusedWithNoChainAlarm(t *testing.T) {
	capture := captureRecords(t)
	f := newGroupFixture(t)
	_, grantID := f.approvedTeamKey(t)
	if _, err := f.m.Values(context.Background(), grantID, f.asker, nil, ReleaseCall{}); err != nil {
		t.Fatalf("Values while the group is fresh = %v, want the value", err)
	}
	f.staleUnderTheSamePolicy(t)
	_, err := f.m.Values(context.Background(), grantID, f.asker, nil, ReleaseCall{})
	if !errors.Is(err, ErrGrantNotLive) || !errors.Is(err, record.ErrApproverNotAdmittedNow) || errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values, group stale = %v; want ErrGrantNotLive wrapping ErrApproverNotAdmittedNow, and no ErrGrantChainInvalid", err)
	}
	if lines := loggedLines(t, capture, GrantChainInvalidMessage); len(lines) != 0 {
		t.Fatalf("a lapsed grant wrote %d %q lines: %v", len(lines), GrantChainInvalidMessage, lines)
	}
}

// lapsedLine is one GrantApproverLapsedMessage line as it should be written.
type lapsedLine struct{ grant, record, decider, at string }

// requireLines fails t unless capture holds exactly want lapsed lines and chains chain lines
// since the mark-th lapsed line and the chainMark-th chain line.
func requireLines(t *testing.T, capture *logtest.Recorder, mark, chainMark int, chains int, want ...lapsedLine) {
	t.Helper()
	lapsed := loggedLines(t, capture, GrantApproverLapsedMessage)[mark:]
	if len(lapsed) != len(want) {
		t.Fatalf("the %q lines are %v; want %d", GrantApproverLapsedMessage, lapsed, len(want))
	}
	for i, w := range want {
		if lapsed[i].level != slog.LevelWarn {
			t.Errorf("%q was logged at %s, want WARN", GrantApproverLapsedMessage, lapsed[i].level)
		}
		attrs := map[string]any{"grant": w.grant, "record_id": w.record, "secret": "TEAM_KEY", "decider": w.decider,
			"group": approversGroup, "reason": "decider is not a member now", "at": w.at}
		if !reflect.DeepEqual(lapsed[i].attrs, attrs) {
			t.Fatalf("lapsed line %d is %v; want %v", i, lapsed[i].attrs, attrs)
		}
	}
	if got := len(loggedLines(t, capture, GrantChainInvalidMessage)) - chainMark; got != chains {
		t.Fatalf("%d %q lines, want %d", got, GrantChainInvalidMessage, chains)
	}
}

// TestALapsedGroupApprovalWritesOneLogLine: each refusal of a grant a member of the group
// decided, because the group does not admit the decider now, writes exactly one lapsed line
// naming the grant, its record, its secret, the decider, the group and where; a fresh group's
// refusal writes exactly one broken-chain line beside it, and a stale group's none.
func TestALapsedGroupApprovalWritesOneLogLine(t *testing.T) {
	capture := captureRecords(t)
	f := newGroupFixture(t)
	ctx := context.Background()
	recordID, grantID := f.approvedTeamKey(t)
	lapsedSoFar := func() int { return len(loggedLines(t, capture, GrantApproverLapsedMessage)) }
	chainsSoFar := func() int { return len(loggedLines(t, capture, GrantChainInvalidMessage)) }

	// The group stale: one line at the release, one at the reuse, no chain line.
	f.directory.MarkStale(approversGroup)
	mark, chainMark := lapsedSoFar(), chainsSoFar()
	if _, err := f.m.Values(ctx, grantID, f.asker, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values, group stale = %v, want ErrGrantNotLive", err)
	}
	requireLines(t, capture, mark, chainMark, 0, lapsedLine{grantID, recordID, groupMember, "values"})
	mark = lapsedSoFar()
	if req := f.ask(t, f.asker, f.askerKey, "TEAM_KEY"); req.State != "pending" {
		t.Fatalf("the asker's ask, group stale = %+v; want a new pending request", req)
	}
	requireLines(t, capture, mark, chainMark, 0, lapsedLine{grantID, recordID, groupMember, "reuse"})

	// The group fresh again, and the decision row rewritten to name a person it never held.
	f.directory.Set(approversGroup, groupMember, secondMember)
	if _, err := f.m.Store.Pool.Exec(ctx, `update credential_request_events set login=$2 where record_id=$1 and event='approved'`, recordID, groupOutsider); err != nil {
		t.Fatalf("rewrite the decision row: %v", err)
	}
	mark, chainMark = lapsedSoFar(), chainsSoFar()
	if _, err := f.m.Values(ctx, grantID, f.asker, nil, ReleaseCall{}); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values of a decision row naming an outsider = %v, want ErrGrantChainInvalid", err)
	}
	requireLines(t, capture, mark, chainMark, 1, lapsedLine{grantID, recordID, groupOutsider, "values"})
	if _, err := f.m.Store.Pool.Exec(ctx, `update credential_request_events set login=$2 where record_id=$1 and event='approved'`, recordID, groupMember); err != nil {
		t.Fatalf("restore the decision row: %v", err)
	}

	// The member dropped from a group that stays fresh: one of each at the release and the reuse.
	f.directory.Drop(approversGroup, groupMember)
	mark, chainMark = lapsedSoFar(), chainsSoFar()
	if _, err := f.m.Values(ctx, grantID, f.asker, nil, ReleaseCall{}); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values, the decider dropped = %v, want ErrGrantChainInvalid", err)
	}
	requireLines(t, capture, mark, chainMark, 1, lapsedLine{grantID, recordID, groupMember, "values"})
	mark, chainMark = lapsedSoFar(), chainsSoFar()
	f.ask(t, f.asker, f.askerKey, "TEAM_KEY")
	requireLines(t, capture, mark, chainMark, 1, lapsedLine{grantID, recordID, groupMember, "reuse"})
}
