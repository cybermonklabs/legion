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
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
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

// groupOwner is the person who owns OWNER_KEY, whose requesters name the group.
const groupOwner = "owner@example.com"

// groupSecrets are the group's secrets: TEAM_KEY, human tier, which a member approves;
// TEAM_TOKEN, agent tier with no requesters tag, which a member's session gets at once; and two
// secrets whose requesters name the group, LISTED_TOKEN, shared at agent tier, which a member's
// session gets at once, and OWNER_KEY, groupOwner's at human tier, which groupOwner approves.
func groupSecrets() []secrets.LocalSecret {
	listed := policytest.Secret("LISTED_TOKEN", policy.OwnerShared, policy.TierAgent, "listed-v1")
	listed.Tags[policy.TagRequesters] = approversForm
	owned := policytest.Secret("OWNER_KEY", groupOwner, policy.TierHuman, "owner-v1")
	owned.Tags[policy.TagRequesters] = approversForm
	return []secrets.LocalSecret{
		policytest.Secret("TEAM_KEY", approversForm, policy.TierHuman, "team-v1"),
		policytest.Secret("TEAM_TOKEN", approversForm, policy.TierAgent, "token-v1"),
		listed,
		owned,
	}
}

// groupFixture is a Machine over groupSecrets whose policy, decisions, lists and chain check all
// read one stand-in group reader listing groupMember and secondMember in approversGroup, with a
// session of groupMember's and one of groupAsker's, and the store its policy is read from.
type groupFixture struct {
	m                   *Machine
	local               *secrets.Local
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
	m.Chain = NewChainVerifier(st, testAudience, time.Minute, m.Policy)
	m.Chain.Groups = directory
	f := &groupFixture{m: m, local: local, directory: directory}
	f.member, f.memberKey = newEnrollment(t, st, "session", "box-member-"+t.Name(), new(groupMember), nil)
	f.asker, f.askerKey = newEnrollment(t, st, "session", "box-asker-"+t.Name(), new(groupAsker), nil)
	return f
}

// ask is Create of names on enr, signed with key.
func (f *groupFixture) ask(t *testing.T, enr string, key *ecdsa.PrivateKey, names ...string) Request {
	t.Helper()
	req, err := f.m.Create(context.Background(), enr, signRequest(t, f.m, key, "team work", names...), "")
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
// under the group's live members; one decided by a non-member, and one decided by a member of a
// stale group, are refused as not live, with the plain refusal: the typed lapse of a
// group-approved record is the chain check's to answer and log, never this one's.
func TestStillAllowedAdmitsAGroupMemberAndRefusesANonMember(t *testing.T) {
	t.Parallel()
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
	if err := stillAllowed(set, "TEAM_KEY", source, policy.Approval, groupMember, nil, asker); err != nil {
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
		err := stillAllowed(set, "TEAM_KEY", source, policy.Approval, c.decider, nil, asker)
		var lapsed record.ApproverNotAdmittedError
		if !errors.Is(err, ErrGrantNotLive) || errors.As(err, &lapsed) {
			t.Fatalf("%s: stillAllowed = %v; want ErrGrantNotLive and no lapse of the record's group", c.name, err)
		}
	}
}

// TestMaySettleAdmitsAGroupMember: a person settles a name the policy routes to a group while the
// set's group reader lists them, and not when it does not, nor once the group is stale.
func TestMaySettleAdmitsAGroupMember(t *testing.T) {
	t.Parallel()
	directory := groupstest.New()
	directory.Set(approversGroup, groupMember)
	member, outsider := bySession(store.BrowserSessionRef{Login: groupMember}), bySession(store.BrowserSessionRef{Login: groupOutsider})
	if !member.maySettle(approversForm, approversForm, directory) || outsider.maySettle(approversForm, approversForm, directory) {
		t.Fatalf("maySettle: member %v, outsider %v; want true, false", member.maySettle(approversForm, approversForm, directory), outsider.maySettle(approversForm, approversForm, directory))
	}
	directory.MarkStale(approversGroup)
	if member.maySettle(approversForm, approversForm, directory) {
		t.Fatal("a member of a stale group settles its name")
	}
}

// TestPendingForApproverListsAGroupRequestForEachMember: a request waiting on the group is listed
// for each of its two members, and for neither a person outside it nor the asker.
func TestPendingForApproverListsAGroupRequestForEachMember(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	if _, err := f.m.Values(context.Background(), grantID, f.asker, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values after a member's revoke = %v, want ErrGrantNotLive", err)
	}
}

// TestAnOutsiderNeitherListsNorRevokesAGroupOwnedGrant: a person outside the group finds the
// group's grant on no list of their own, and their revoke is ErrNotApprover and ends nothing.
func TestAnOutsiderNeitherListsNorRevokesAGroupOwnedGrant(t *testing.T) {
	t.Parallel()
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
	if _, err := f.m.Values(context.Background(), grantID, f.asker, ReleaseCall{}); err != nil {
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
	if _, err := f.m.Values(context.Background(), *req.GrantID, f.member, ReleaseCall{}); err != nil {
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
	t.Parallel()
	f := newGroupFixture(t)
	grantID := f.automaticTeamToken(t)
	f.staleUnderTheSamePolicy(t)
	if release, err := f.m.Values(context.Background(), grantID, f.member, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values of the automatic grant, group stale = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
}

// TestAnAutomaticGrantOfAStaleGroupIsNotReusedUnderAnUnchangedPolicy: in the same state, the
// member's ask for the same name gets no grant back, and waits for the group's approval.
func TestAnAutomaticGrantOfAStaleGroupIsNotReusedUnderAnUnchangedPolicy(t *testing.T) {
	t.Parallel()
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
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.captureRecords, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture := captureRecords(t)
	f := newGroupFixture(t)
	_, grantID := f.approvedTeamKey(t)
	if _, err := f.m.Values(context.Background(), grantID, f.asker, ReleaseCall{}); err != nil {
		t.Fatalf("Values while the group is fresh = %v, want the value", err)
	}
	f.staleUnderTheSamePolicy(t)
	_, err := f.m.Values(context.Background(), grantID, f.asker, ReleaseCall{})
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
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.captureRecords, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture := captureRecords(t)
	f := newGroupFixture(t)
	ctx := context.Background()
	recordID, grantID := f.approvedTeamKey(t)
	lapsedSoFar := func() int { return len(loggedLines(t, capture, GrantApproverLapsedMessage)) }
	chainsSoFar := func() int { return len(loggedLines(t, capture, GrantChainInvalidMessage)) }

	// The group stale: one line at the release, one at the reuse, no chain line.
	f.directory.MarkStale(approversGroup)
	mark, chainMark := lapsedSoFar(), chainsSoFar()
	if _, err := f.m.Values(ctx, grantID, f.asker, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
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
	if _, err := f.m.Values(ctx, grantID, f.asker, ReleaseCall{}); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values of a decision row naming an outsider = %v, want ErrGrantChainInvalid", err)
	}
	requireLines(t, capture, mark, chainMark, 1, lapsedLine{grantID, recordID, groupOutsider, "values"})
	if _, err := f.m.Store.Pool.Exec(ctx, `update credential_request_events set login=$2 where record_id=$1 and event='approved'`, recordID, groupMember); err != nil {
		t.Fatalf("restore the decision row: %v", err)
	}

	// The member dropped from a group that stays fresh: one of each at the release and the reuse.
	f.directory.Drop(approversGroup, groupMember)
	mark, chainMark = lapsedSoFar(), chainsSoFar()
	if _, err := f.m.Values(ctx, grantID, f.asker, ReleaseCall{}); !errors.Is(err, ErrGrantChainInvalid) {
		t.Fatalf("Values, the decider dropped = %v, want ErrGrantChainInvalid", err)
	}
	requireLines(t, capture, mark, chainMark, 1, lapsedLine{grantID, recordID, groupMember, "values"})
	mark, chainMark = lapsedSoFar(), chainsSoFar()
	f.ask(t, f.asker, f.askerKey, "TEAM_KEY")
	requireLines(t, capture, mark, chainMark, 1, lapsedLine{grantID, recordID, groupMember, "reuse"})
}

// grantRevoked reports whether grantID's row records a revoke.
func (f *groupFixture) grantRevoked(t *testing.T, grantID string) bool {
	t.Helper()
	var revoked bool
	if err := f.m.Store.Pool.QueryRow(context.Background(), `select revoked_at is not null from grants where id=$1`, grantID).Scan(&revoked); err != nil {
		t.Fatalf("read grant %s: %v", grantID, err)
	}
	return revoked
}

// TestAStaleRequestersGroupPausesItsGrants: a grant of a secret whose requesters name the group
// is refused while the group is stale, GRANT_NOT_LIVE, and not revoked, by its release and by an
// ask that would reuse it; a new ask is denied as the group unread; and the grant releases again
// once a read lists the person.
func TestAStaleRequestersGroupPausesItsGrants(t *testing.T) {
	t.Parallel()
	f := newGroupFixture(t)
	ctx := context.Background()
	req := f.ask(t, f.member, f.memberKey, "LISTED_TOKEN")
	if req.State != "granted" || req.GrantID == nil {
		t.Fatalf("the member's LISTED_TOKEN = %+v; want granted at once", req)
	}
	grantID := *req.GrantID
	if _, err := f.m.Values(ctx, grantID, f.member, ReleaseCall{}); err != nil {
		t.Fatalf("Values while the group is fresh = %v, want the value", err)
	}
	f.directory.MarkStale(approversGroup)
	if _, err := f.m.Values(ctx, grantID, f.member, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values while the requesters group is stale = %v, want ErrGrantNotLive", err)
	}
	again := f.ask(t, f.member, f.memberKey, "LISTED_TOKEN")
	if again.State != "denied" || again.Detail == nil || !strings.Contains(*again.Detail, policy.ReasonRequesterGroupUnread) {
		t.Fatalf("the member's ask while the group is stale = %+v; want denied %s", again, policy.ReasonRequesterGroupUnread)
	}
	if f.grantRevoked(t, grantID) {
		t.Fatal("a stale requesters group revoked the grant; want it paused")
	}
	f.directory.Set(approversGroup, groupMember, secondMember)
	if _, err := f.m.Values(ctx, grantID, f.member, ReleaseCall{}); err != nil {
		t.Fatalf("Values once the group is read again = %v, want the value", err)
	}
}

// TestAnApprovalWaitsWhileItsRequestersGroupIsUnread: the owner's approval of a request for a
// secret whose requesters name the group is refused while the group is stale, and the request
// stays pending, not denied; once the group is read the same approval grants it.
func TestAnApprovalWaitsWhileItsRequestersGroupIsUnread(t *testing.T) {
	t.Parallel()
	f := newGroupFixture(t)
	ctx := context.Background()
	req := f.ask(t, f.member, f.memberKey, "OWNER_KEY")
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("the member's OWNER_KEY = %+v; want pending on its owner", req)
	}
	f.directory.MarkStale(approversGroup)
	if _, err := f.m.ApplyDecision(ctx, *req.RecordID, true, groupOwner); !errors.Is(err, record.ErrNotApprover) || !strings.Contains(err.Error(), "stays pending") {
		t.Fatalf("the owner's approval while the group is stale = %v; want ErrNotApprover saying it stays pending", err)
	}
	if got, err := f.m.Get(ctx, req.ID); err != nil || got.State != "pending" {
		t.Fatalf("the request after the refused approval = %+v, %v; want it pending", got, err)
	}
	f.directory.Set(approversGroup, groupMember, secondMember)
	if dec, err := f.m.ApplyDecision(ctx, *req.RecordID, true, groupOwner); err != nil || dec.GrantID == "" {
		t.Fatalf("the owner's approval once the group is read = %+v, %v; want a grant", dec, err)
	}
}

// TestAMemberRevokesTheGroupsGrantWhileItIsStale: while the group is stale, a person its last read
// listed may still revoke a grant the group approved, since a revoke only takes power away; a
// person outside it, and a login spelled as the group itself, may not.
func TestAMemberRevokesTheGroupsGrantWhileItIsStale(t *testing.T) {
	t.Parallel()
	f := newGroupFixture(t)
	ctx := context.Background()
	_, grantID := f.approvedTeamKey(t)
	f.directory.MarkStale(approversGroup)
	for _, login := range []string{groupOutsider, approversForm} {
		if err := f.m.RevokeByApprover(ctx, grantID, login, record.HumanActor(login)); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("RevokeByApprover(%s) while the group is stale = %v, want ErrNotApprover", login, err)
		}
	}
	if f.grantRevoked(t, grantID) {
		t.Fatal("a refused revoke ended the grant")
	}
	if err := f.m.RevokeByApprover(ctx, grantID, secondMember, record.HumanActor(secondMember)); err != nil {
		t.Fatalf("RevokeByApprover(a member the last read listed) while the group is stale = %v, want nil", err)
	}
	if !f.grantRevoked(t, grantID) {
		t.Fatal("the member's revoke did not end the grant")
	}
}

// TestAMemberDismissesAnExpiredGroupRequest: an expired request waiting on the group is on each
// member's Inbox list by state, and a member dismisses it: it leaves the second member's list too.
// A person outside the group, and a login spelled as the group, are refused.
func TestAMemberDismissesAnExpiredGroupRequest(t *testing.T) {
	t.Parallel()
	f := newGroupFixture(t)
	ctx := context.Background()
	req := f.ask(t, f.asker, f.askerKey, "TEAM_KEY")
	if req.State != "pending" || req.RecordID == nil {
		t.Fatalf("the asker's TEAM_KEY = %+v; want pending on the group", req)
	}
	pastDeadline(t, f.m, req.ID)
	expired := *req.RecordID + "=expired"
	if got := listedStates(t, f.m, secondMember, bothStates); !slices.Contains(got, expired) {
		t.Fatalf("the second member's list = %v; want the expired group request %s", got, expired)
	}
	for _, login := range []string{groupOutsider, approversForm} {
		if d, err := f.m.Dismiss(ctx, *req.RecordID, login); !errors.Is(err, record.ErrNotApprover) {
			t.Fatalf("Dismiss by %s = %+v, %v; want record.ErrNotApprover", login, d, err)
		}
	}
	f.directory.MarkStale(approversGroup)
	if d, err := f.m.Dismiss(ctx, *req.RecordID, groupMember); !errors.Is(err, record.ErrNotApprover) {
		t.Fatalf("Dismiss by a member while the group is stale = %+v, %v; want record.ErrNotApprover, as the Inbox lists it to nobody", d, err)
	}
	f.directory.Set(approversGroup, groupMember, secondMember)
	if d, err := f.m.Dismiss(ctx, *req.RecordID, groupMember); err != nil || !d.Wrote || d.DismissedBy != groupMember {
		t.Fatalf("Dismiss by a member = %+v, %v; want it written by %s", d, err, groupMember)
	}
	if got := listedStates(t, f.m, secondMember, bothStates); slices.Contains(got, expired) {
		t.Fatalf("the second member's list after a member's dismissal = %v; want it gone", got)
	}
}

// TestAJudgedGroupGrantStaysOnAMembersListWhileTheGroupIsStale: a grant the model judge approved
// for the group's secret names the group as its approver and releases with no group read, so it
// stays on the list of every member the group's last good read listed while the group is stale,
// as those members may revoke it; a person outside the group does not list it, and once a fresh
// read drops a member the grant leaves that member's list.
func TestAJudgedGroupGrantStaysOnAMembersListWhileTheGroupIsStale(t *testing.T) {
	t.Parallel()
	f := newGroupFixture(t)
	ctx := context.Background()
	policytest.JudgeTags(t, f.m.Secrets.(secrets.AWS).Client.(*secrets.Local), f.m.Policy, "TEAM_KEY", 900, "")
	f.m.Chain = NewChainVerifier(f.m.Store, testAudience, time.Minute, f.m.Policy)
	f.m.Chain.Groups = f.directory
	f.m.Judge = func(PendingRequest) {}
	req := pendingJudgeable(t, f.m, f.asker, f.askerKey, "ACME-12 needs the team key", 600, "TEAM_KEY")
	dec, err := f.m.ApplyJudgeDecision(ctx, *req.RecordID, judgeModel, map[string]any{"verdict": "grant"})
	if err != nil || dec.GrantID == "" {
		t.Fatalf("the judge's approval of TEAM_KEY = %+v, %v; want a grant", dec, err)
	}
	listed := func(login string) bool {
		t.Helper()
		grants, err := f.m.GrantsForApprover(ctx, login)
		if err != nil {
			t.Fatalf("GrantsForApprover(%s): %v", login, err)
		}
		return slices.ContainsFunc(grants, func(g ApproverGrant) bool { return g.GrantID == dec.GrantID })
	}
	f.directory.MarkStale(approversGroup)
	if _, err := f.m.Values(ctx, dec.GrantID, f.asker, ReleaseCall{}); err != nil {
		t.Fatalf("Values of the judged grant while the group is stale = %v; want it released, as no group read decides it", err)
	}
	for login, want := range map[string]bool{groupMember: true, secondMember: true, groupOutsider: false} {
		if got := listed(login); got != want {
			t.Errorf("GrantsForApprover(%s) lists the judged group grant while the group is stale: %v, want %v", login, got, want)
		}
	}
	f.directory.Set(approversGroup, groupMember)
	if listed(secondMember) {
		t.Error("a member a fresh read dropped still lists the group's grant")
	}
}

// retag stores secret, the store's form of the policy's secret name, and rereads name, as an
// owner's retag and the reread after it do.
func (f *groupFixture) retag(t *testing.T, name string, secret secrets.LocalSecret) {
	t.Helper()
	f.local.Put(secret)
	if _, err := f.m.Policy.RefreshOne(context.Background(), name); err != nil {
		t.Fatalf("RefreshOne(%s): %v", name, err)
	}
}

// TestTheLapseLineFollowsTheRecordsGroup: a grant a member approved is refused with one lapsed
// line at its release and at the reuse an ask makes, after its secret was handed to a person and
// while its group is stale: the chain check reads the record's group, whoever owns the secret now.
// A grant a person approved writes no lapsed line once its secret is handed to a group the person
// is not in, at its release nor at the reuse an ask makes: no group approved it. The reuse half is
// the one that can fail, since the reuse path is where a lapsed line was once written from the
// policy's approver rather than the record's.
func TestTheLapseLineFollowsTheRecordsGroup(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (requests.captureRecords, logtest.SetDefault, logtest.Swap, slog.SetDefault).
	capture := captureRecords(t)
	f := newGroupFixture(t)
	ctx := context.Background()
	recordID, grantID := f.approvedTeamKey(t)
	f.retag(t, "TEAM_KEY", policytest.Secret("TEAM_KEY", groupOwner, policy.TierHuman, "team-v1"))
	f.directory.MarkStale(approversGroup)
	mark, chainMark := len(loggedLines(t, capture, GrantApproverLapsedMessage)), len(loggedLines(t, capture, GrantChainInvalidMessage))
	if _, err := f.m.Values(ctx, grantID, f.asker, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values after the retag, group stale = %v, want ErrGrantNotLive", err)
	}
	requireLines(t, capture, mark, chainMark, 0, lapsedLine{grantID, recordID, groupMember, "values"})
	mark = len(loggedLines(t, capture, GrantApproverLapsedMessage))
	f.ask(t, f.asker, f.askerKey, "TEAM_KEY")
	requireLines(t, capture, mark, chainMark, 0, lapsedLine{grantID, recordID, groupMember, "reuse"})

	f.directory.Set(approversGroup, groupMember, secondMember)
	owned := f.ask(t, f.member, f.memberKey, "OWNER_KEY")
	dec, err := f.m.ApplyDecision(ctx, *owned.RecordID, true, groupOwner)
	if err != nil || dec.GrantID == "" {
		t.Fatalf("the owner's approval of OWNER_KEY = %+v, %v; want a grant", dec, err)
	}
	handed := policytest.Secret("OWNER_KEY", approversForm, policy.TierHuman, "owner-v1")
	handed.Tags[policy.TagRequesters] = approversForm
	f.retag(t, "OWNER_KEY", handed)
	mark = len(loggedLines(t, capture, GrantApproverLapsedMessage))
	if _, err := f.m.Values(ctx, dec.GrantID, f.member, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) {
		t.Fatalf("Values of the person's grant after its secret went to the group = %v, want ErrGrantNotLive", err)
	}
	f.ask(t, f.member, f.memberKey, "OWNER_KEY")
	if lines := loggedLines(t, capture, GrantApproverLapsedMessage)[mark:]; len(lines) != 0 {
		t.Fatalf("a person's approval wrote %q lines at its release or its reuse: %v", GrantApproverLapsedMessage, lines)
	}
}
